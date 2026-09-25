package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Indexing parses a host's captures (see capture.go) into the catalog, on any
// Mac, without the capturing Mac. Everything it writes (source and record
// state, parsed parts, sightings, each conversation's writer) is attributed to
// the capturing host, and origins and evidence locators name the original
// source paths, never the capture's. So indexing a capture and ingesting the
// live source on the same Mac agree on each conversation's writer (host and
// origin) and share per-part state: neither re-parses nor takes over what the
// other wrote.
//
// A locator's captured bytes are found from where its conversation was seen
// (conversation_sightings: host_id, source_name, origin) through that source's
// manifest; see capturedPathFor.

// captureView lets an adapter read a capture as if it were the source.
type captureView struct {
	host     Host
	dir      string // <capture_root>/<host-id>/<source>
	manifest *captureManifest
	// mappings pair original paths with absolute captured paths.
	mappings []capturePathMapping
}

func newCaptureView(host Host, dir string, manifest *captureManifest) *captureView {
	view := &captureView{host: host, dir: dir, manifest: manifest}
	for _, mapping := range manifest.PathMap {
		view.mappings = append(view.mappings, capturePathMapping{Original: mapping.Original, Captured: filepath.Join(dir, filepath.FromSlash(mapping.Captured))})
	}
	return view
}

// original maps a captured path back to the source path it copies.
func (v *captureView) original(captured string) string {
	if mapped, ok := v.translate(captured, false); ok {
		return mapped
	}
	return captured
}

// captured maps a source path to its capture.
func (v *captureView) captured(original string) (string, bool) {
	return v.translate(original, true)
}

// translate maps path through the mapping with the longest matching prefix.
func (v *captureView) translate(path string, forward bool) (string, bool) {
	best, length := "", -1
	for _, mapping := range v.mappings {
		from, to := mapping.Captured, mapping.Original
		if forward {
			from, to = to, from
		}
		if rest, ok := pathUnder(path, from); ok && len(from) > length {
			best, length = filepath.Join(to, rest), len(from)
		}
	}
	return best, length >= 0
}

func pathUnder(path, root string) (string, bool) {
	if path == root {
		return "", true
	}
	if root != "" && strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/") {
		return path[len(strings.TrimSuffix(root, "/"))+1:], true
	}
	return "", false
}

// databaseVersion is a captured database's version: the source state the
// manifest recorded before snapshotting it; 0 (unknown) for a superseded
// snapshot from before generations recorded theirs.
func (v *captureView) databaseVersion(path string) int64 {
	rel, err := filepath.Rel(v.dir, path)
	if err != nil {
		return 0
	}
	rel = filepath.ToSlash(rel)
	for _, snapshot := range v.manifest.Snapshots {
		if snapshot.Captured == rel {
			return snapshot.Source.version()
		}
		for _, generation := range snapshot.Generations {
			if generation.Captured == rel && generation.Source != nil {
				return generation.Source.version()
			}
		}
	}
	return 0
}

func (s sqliteSourceState) version() int64 { return max(s.MTimeNS, s.WALMTimeNS) }

// offHost reports a capture of another Mac, whose paths this Mac's Git
// checkouts say nothing about.
func (v *captureView) offHost() bool { return v.host.ID != currentHost().ID }

// seen is when the host's data was last captured, for its hosts row.
func (v *captureView) seen() string {
	if v.host.ID == currentHost().ID {
		return ""
	}
	return v.manifest.UpdatedAt
}

// captureTarget is one host's captured source.
type captureTarget struct {
	Host   Host   `json:"host"`
	Source string `json:"source"`
	Dir    string `json:"dir"`
}

func (t captureTarget) label() string { return t.Host.ID + "/" + t.Source }

// captureTargets lists captured sources to index: those of hosts (default:
// this Mac) or of every captured host, limited to sources if any are named.
func captureTargets(root string, hosts []string, allHosts bool, sources []string) ([]captureTarget, error) {
	if root == "" {
		return nil, errors.New("capture_root is not configured")
	}
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("capture root %s is unavailable; is its drive mounted? %w", root, err)
	}
	if allHosts {
		hosts = nil
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() && validCaptureName(entry.Name()) {
				hosts = append(hosts, entry.Name())
			}
		}
	} else if len(hosts) == 0 {
		hosts = []string{currentHost().ID}
	}
	wanted := map[string]bool{}
	for _, source := range sources {
		wanted[source] = true
	}
	targets := []captureTarget{}
	for _, id := range hosts {
		if !validCaptureName(id) {
			return nil, fmt.Errorf("invalid host id %q", id)
		}
		dir := filepath.Join(root, id)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) && !allHosts {
			return nil, fmt.Errorf("no captures for host %s in %s", id, root)
		} else if err != nil {
			return nil, err
		}
		host := capturedHost(dir, id)
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || !validCaptureName(name) || (len(wanted) > 0 && !wanted[name]) {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, name, captureManifestName)); err == nil {
				targets = append(targets, captureTarget{Host: host, Source: name, Dir: filepath.Join(dir, name)})
			}
		}
	}
	for source := range wanted {
		found := false
		for _, target := range targets {
			found = found || target.Source == source
		}
		if !found {
			return nil, fmt.Errorf("no capture of source %s for the selected hosts", source)
		}
	}
	return targets, nil
}

// capturedHost reads a host directory's host.json; the directory name is the
// host ID either way.
func capturedHost(dir, id string) Host {
	host := Host{ID: id, Label: id}
	var record captureHostRecord
	if data, err := os.ReadFile(filepath.Join(dir, captureHostName)); err == nil && json.Unmarshal(data, &record) == nil {
		host.Label, host.User = defaultString(record.Label, id), record.User
	}
	return host
}

// IndexCapture parses one host's captured source into the catalog. It reads
// only the capture, never the original paths, and stops between records once
// ctx is cancelled; the next run resumes.
func (c *Catalog) IndexCapture(ctx context.Context, target captureTarget, progress ProgressFunc) IngestResult {
	result := IngestResult{Source: target.Source, Host: target.Host.ID}
	fail := func(err error) IngestResult {
		result.Error = err.Error()
		return result
	}
	manifest, err := loadCaptureManifest(target.Dir)
	if err != nil {
		return fail(err)
	}
	if manifest.Version == 0 {
		return fail(fmt.Errorf("%s holds no capture", target.Dir))
	}
	// The directory decides attribution; a manifest naming another host was
	// moved or copied and must not be indexed as this one.
	if manifest.Host.ID != target.Host.ID || manifest.Source.Name != target.Source {
		return fail(fmt.Errorf("capture %s belongs to %s/%s", target.Dir, manifest.Host.ID, manifest.Source.Name))
	}
	view := newCaptureView(target.Host, target.Dir, manifest)
	root, ok := view.captured(manifest.Source.Path)
	if !ok {
		return fail(fmt.Errorf("capture manifest %s does not map its source path", target.Dir))
	}
	config := SourceConfig{Name: target.Source, Kind: manifest.Source.Kind, Path: root, Account: defaultString(manifest.Source.Account, "local"), Enabled: true}
	adapter, err := MakeAdapter(config)
	if err != nil {
		return fail(err)
	}
	binder, ok := adapter.(interface{ bindCapture(*captureView) })
	if !ok {
		return fail(fmt.Errorf("captures of %s sources cannot be indexed", config.Kind))
	}
	binder.bindCapture(view)
	// Only one index of a capture at a time, by the service or the CLI.
	lock, err := os.OpenFile(filepath.Join(target.Dir, indexLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fail(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("%s is already being indexed: %w", target.label(), err))
	}
	// What is written is guarded record by record (see guardCapture), so a
	// capture older than what its host already indexed rolls nothing back.
	if result = c.IngestContext(ctx, adapter, progress); result.Error != nil || ctx.Err() != nil {
		return result
	}
	marker := loadIndexMarker(target.Dir)
	if err := c.indexGenerations(ctx, adapter, view, marker, &result); err != nil {
		if ctx.Err() == nil {
			result.Error = fmt.Sprintf("superseded snapshots: %v", err)
		}
		return result
	}
	for _, snapshot := range manifest.Snapshots {
		marker.add(snapshot.Path, snapshot.CapturedAt)
	}
	// A capture may have replaced a snapshot while it was indexed, and what
	// was read is then unknown: its snapshots stay unlisted for the next run.
	current, err := loadCaptureManifest(target.Dir)
	if err != nil {
		return result
	}
	marker.keepUnchanged(manifest, current)
	if marker.changed {
		if err := marker.save(target.Dir, current); err != nil {
			result.Error = fmt.Sprintf("record indexed snapshots: %v", err)
		}
	}
	return result
}

// indexGenerations takes from each superseded Conductor snapshot not yet
// indexed the sessions no newer snapshot has: sessions deleted at the source
// survive only there. Sessions still present are indexed from the newest
// snapshot holding them, so an older copy never rolls one back. Other kinds
// cannot index only what is missing, so their generations are merely recorded.
func (c *Catalog) indexGenerations(ctx context.Context, adapter Adapter, view *captureView, marker *indexMarker, result *IngestResult) error {
	conductor, ok := adapter.(*conductorAdapter)
	for _, snapshot := range view.manifest.Snapshots {
		pending := false
		for _, generation := range snapshot.Generations {
			pending = pending || !marker.has(snapshot.Path, generation.CapturedAt)
		}
		if !pending || !ok {
			for _, generation := range snapshot.Generations {
				marker.add(snapshot.Path, generation.CapturedAt)
			}
			continue
		}
		handled, err := conductorSessionIDs(filepath.Join(view.dir, filepath.FromSlash(snapshot.Captured)))
		if err != nil {
			return err
		}
		for _, generation := range snapshot.Generations { // newest first
			file := filepath.Join(view.dir, filepath.FromSlash(generation.Captured))
			if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
				marker.add(snapshot.Path, generation.CapturedAt) // gone: nothing left to take
				continue
			}
			ids, err := conductorSessionIDs(file)
			if err != nil {
				return err
			}
			missing := map[string]bool{}
			for id := range ids {
				if !handled[id] {
					missing[id] = true
					handled[id] = true
				}
			}
			if marker.has(snapshot.Path, generation.CapturedAt) || len(missing) == 0 {
				marker.add(snapshot.Path, generation.CapturedAt)
				continue
			}
			older := &captureView{host: view.host, dir: view.dir, manifest: view.manifest, mappings: []capturePathMapping{{Original: snapshot.Path, Captured: file}}}
			config := conductor.config
			config.Path = file
			reader := &conductorAdapter{baseAdapter: baseAdapter{config: config, capability: conductor.capability, view: older}, selected: missing}
			if err := c.ingestPass(ctx, reader, view.host.ID, false, result, func(string) {}, func(string) {}); err != nil {
				return err
			}
			marker.add(snapshot.Path, generation.CapturedAt)
		}
	}
	return nil
}

func conductorSessionIDs(file string) (map[string]bool, error) {
	db, err := openReadOnlySQLite(file)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tables, err := tableNames(db)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	table := firstPresent(tables, "sessions", "workspaces", "threads")
	if table == "" {
		return ids, nil
	}
	columns, err := tableColumns(db, table)
	if err != nil {
		return nil, err
	}
	column := firstPresent(columns, "id", "session_id", "uuid")
	if column == "" {
		return ids, nil
	}
	rows, err := queryMaps(db, "SELECT "+column+" id FROM "+table)
	for _, row := range rows {
		ids[firstString(row["id"])] = true
	}
	return ids, err
}

// indexLockName, in a captured source's directory, is held (flock) while the
// capture is indexed. A capture never takes it.
const indexLockName = ".index.lock"

// indexMarkerName, in a captured source's directory, lists the database
// snapshots (by original path and captured_at) the index no longer needs.
// Capture reads it to keep superseded snapshots until they are indexed; it
// never opens the catalog. Only the index writes it.
const indexMarkerName = "indexed.json"

type indexMarker struct {
	Version   int               `json:"version"`
	UpdatedAt string            `json:"updated_at"`
	Snapshots []indexedSnapshot `json:"snapshots"`
	// exists is false until an index has written the marker.
	exists  bool
	changed bool
	// saved holds the entries read from disk, which keepUnchanged keeps.
	saved map[string]bool
}

type indexedSnapshot struct {
	Path       string `json:"path"`
	CapturedAt string `json:"captured_at"`
}

// loadIndexMarker reads a source's marker; an unreadable one counts as
// absent, so capture falls back to its plain retention.
func loadIndexMarker(dir string) *indexMarker {
	marker := &indexMarker{saved: map[string]bool{}}
	if data, err := os.ReadFile(filepath.Join(dir, indexMarkerName)); err == nil && json.Unmarshal(data, marker) == nil {
		marker.exists = true
	}
	for _, snapshot := range marker.Snapshots {
		marker.saved[snapshot.Path+"\x1f"+snapshot.CapturedAt] = true
	}
	return marker
}

func (m *indexMarker) has(path, capturedAt string) bool {
	for _, snapshot := range m.Snapshots {
		if snapshot.Path == path && snapshot.CapturedAt == capturedAt {
			return true
		}
	}
	return false
}

// keepUnchanged drops what this run added for a database whose snapshots
// changed between the manifest the index read (before) and the current one.
func (m *indexMarker) keepUnchanged(before, current *captureManifest) {
	versions := func(manifest *captureManifest) map[string]string {
		result := map[string]string{}
		for _, snapshot := range manifest.Snapshots {
			result[snapshot.Path] = snapshot.CapturedAt
			for _, generation := range snapshot.Generations {
				result[snapshot.Path] += "," + generation.CapturedAt
			}
		}
		return result
	}
	was, now := versions(before), versions(current)
	kept := m.Snapshots[:0]
	for _, snapshot := range m.Snapshots {
		if was[snapshot.Path] == now[snapshot.Path] || m.saved[snapshot.Path+"\x1f"+snapshot.CapturedAt] {
			kept = append(kept, snapshot)
		}
	}
	m.Snapshots = kept
}

func (m *indexMarker) add(path, capturedAt string) {
	if !m.has(path, capturedAt) {
		m.Snapshots = append(m.Snapshots, indexedSnapshot{Path: path, CapturedAt: capturedAt})
		m.changed = true
	}
}

// save keeps only snapshots the manifest still names. Its temporary name is
// not one a starting capture sweeps up.
func (m *indexMarker) save(dir string, manifest *captureManifest) error {
	named := map[string]bool{}
	for _, snapshot := range manifest.Snapshots {
		named[snapshot.Path+"\x1f"+snapshot.CapturedAt] = true
		for _, generation := range snapshot.Generations {
			named[snapshot.Path+"\x1f"+generation.CapturedAt] = true
		}
	}
	kept := []indexedSnapshot{}
	for _, snapshot := range m.Snapshots {
		if named[snapshot.Path+"\x1f"+snapshot.CapturedAt] {
			kept = append(kept, snapshot)
		}
	}
	sort.Slice(kept, func(i, j int) bool {
		return kept[i].Path+kept[i].CapturedAt < kept[j].Path+kept[j].CapturedAt
	})
	m.Version, m.UpdatedAt, m.Snapshots = 1, now(), kept
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, indexMarkerName), append(data, '\n'), 0o600)
}

// capturedPathFor finds the captured copy of an original source path that
// host captured from source: the manifest's file entry for a transcript, else
// its longest path_map prefix (a database snapshot, a TL1 transcript). An
// evidence locator is <original path>:<line>..., or sqlite:<original path>:...
// for a database, so a reader strips those to get the path.
func capturedPathFor(captureRoot, host, source, original string) (string, error) {
	if !validCaptureName(host) || !validCaptureName(source) {
		return "", fmt.Errorf("invalid host or source name")
	}
	dir := filepath.Join(captureRoot, host, source)
	manifest, err := loadCaptureManifest(dir)
	if err != nil {
		return "", err
	}
	for _, file := range manifest.Files {
		if file.Path == original {
			return filepath.Join(dir, filepath.FromSlash(file.Captured)), nil
		}
	}
	if captured, ok := newCaptureView(Host{ID: host}, dir, manifest).captured(original); ok {
		if _, err := os.Stat(captured); err == nil {
			return captured, nil
		}
	}
	return "", fmt.Errorf("%s was not captured from %s/%s", original, host, source)
}
