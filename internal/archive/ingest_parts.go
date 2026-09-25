package archive

import (
	"context"
	"database/sql"
)

// Incremental parsing. A source's whole-tree fingerprint changes whenever any
// file in it does, which for a live source is nearly always, and the record
// digest then saves only the catalog writes, not the parsing. So adapters that
// can split a source into parts (a transcript; a Claude session with its
// subagents' files; a database session) report the version of each part they
// read, the ingest records it per host and source in source_item_states, and
// the next ingest passes over parts that have not changed. The record digest
// stays the final guard.

// sourcePart is one part of a source in the version an adapter read.
type sourcePart struct {
	item string // original path, or sqlite:<original path>:sessions:<id>
	size int64  // bytes, or a session's message count
	// version grows with each new version: the file's mtime_ns, or a
	// session's highest message rowid.
	version int64
	// signal digests anything else the part's record is built from.
	signal string
}

// partialAdapter can parse only the parts of a source that changed.
type partialAdapter interface {
	Adapter
	// partExtractor names the parser; a new one parses every part again.
	partExtractor() string
	// discoverParts emits each group of parts unchanged does not pass over,
	// with its record, or nil when it yields none (so it is not parsed again).
	discoverParts(unchanged func([]sourcePart) bool, emit func(*WorkspaceRecord, []sourcePart) error) error
}

type partState struct {
	extractor     string
	size, version int64
	signal        string
}

// partTracker decides which parts need parsing and collects what was parsed.
type partTracker struct {
	host, source, extractor string
	// capture is set when the parts are a capture's copies, which may be
	// older than what this host already indexed from the live source.
	capture bool
	states  map[string]partState
	pending []sourcePart
}

func (c *Catalog) newPartTracker(host, source string, adapter partialAdapter, view *captureView) (*partTracker, error) {
	extractor := adapter.partExtractor()
	if view != nil && view.offHost() {
		// Parsed without the host's Git checkouts, so the host's own sync must
		// parse it again to fill in what only its checkouts know.
		extractor += "@offhost"
	}
	tracker := &partTracker{host: host, source: source, extractor: extractor, capture: view != nil, states: map[string]partState{}}
	rows, err := c.DB.Query("SELECT item,extractor,size,version,COALESCE(signal,'') FROM source_item_states WHERE host_id=? AND source_name=?", host, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item string
		var state partState
		if err := rows.Scan(&item, &state.extractor, &state.size, &state.version, &state.signal); err != nil {
			return nil, err
		}
		tracker.states[item] = state
	}
	return tracker, rows.Err()
}

// unchanged reports whether every part is as it was last indexed. A capture's
// copy older than what this host already indexed (from the live source or a
// later capture) is passed over too: parsing it would roll the catalog back,
// since the host and origin match and it would be the writer.
func (t *partTracker) unchanged(parts []sourcePart) bool {
	for _, part := range parts {
		state, ok := t.states[part.item]
		if !ok || state.extractor != t.extractor {
			return false
		}
		if state.size == part.size && state.version == part.version && state.signal == part.signal {
			continue
		}
		if t.capture && state.version > part.version && state.size >= part.size {
			continue
		}
		return false
	}
	return len(parts) > 0
}

// record writes parts' states in tx along with anything pending.
func (t *partTracker) record(tx *sql.Tx, parts []sourcePart) error {
	timestamp := now()
	for _, part := range append(t.pending, parts...) {
		if _, err := tx.Exec(`INSERT INTO source_item_states(host_id,source_name,item,extractor,size,version,signal,indexed_at) VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(host_id,source_name,item) DO UPDATE SET extractor=excluded.extractor,size=excluded.size,version=excluded.version,
			signal=excluded.signal,indexed_at=excluded.indexed_at`, t.host, t.source, part.item, t.extractor, part.size, part.version, nilIfEmpty(part.signal), timestamp); err != nil {
			return err
		}
	}
	t.pending = nil
	return nil
}

// hold keeps parts whose records needed no write, to be recorded with the
// next write or flush: a commit per unchanged record would cost a full device
// flush each. Losing them to a crash only means parsing them again.
func (t *partTracker) hold(c *Catalog, parts []sourcePart) error {
	if t == nil {
		return nil
	}
	t.pending = append(t.pending, parts...)
	if len(t.pending) < 500 {
		return nil
	}
	return t.flush(c)
}

func (t *partTracker) flush(c *Catalog) error {
	if t == nil || len(t.pending) == 0 {
		return nil
	}
	// Runs after a cancellation too: the parts were parsed and are current.
	tx, err := c.beginWrite(context.Background())
	if err != nil {
		return err
	}
	if err := t.record(tx, nil); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.boundWAL(walSizeLimit)
	return nil
}
