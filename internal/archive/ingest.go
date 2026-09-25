package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

type IngestResult struct {
	Source string `json:"source"`
	// Host is the Mac whose copy of the source was ingested.
	Host             string `json:"host,omitempty"`
	Workspaces       int    `json:"workspaces"`
	Conversations    int    `json:"conversations"`
	Messages         int    `json:"messages"`
	SkippedUnchanged bool   `json:"skipped_unchanged"`
	Yielded          bool   `json:"yielded"`
	ExistingOnly     bool   `json:"existing_only,omitempty"`
	SkippedCurrent   int    `json:"skipped_current,omitempty"`
	// Parsed and Unchanged count the parts (files, or database sessions) an
	// incremental adapter parsed and passed over; see ingest_parts.go.
	Parsed    int `json:"parsed,omitempty"`
	Unchanged int `json:"unchanged,omitempty"`
	// Older counts a capture's records passed over because their host had
	// already written them from a newer read (see guardCapture).
	Older int `json:"older,omitempty"`
	Error any `json:"error"`
}

type ProgressFunc func(phase string, workspaces, conversations, messages, skippedCurrent int)

func (c *Catalog) Ingest(adapter Adapter, progress ProgressFunc) IngestResult {
	return c.IngestContext(context.Background(), adapter, progress)
}

// sourceHost is the Mac whose data an adapter reads, to which everything the
// ingest writes is attributed: the capturing Mac for a capture, else this one.
func sourceHost(adapter Adapter) Host {
	if view := captureViewOf(adapter); view != nil {
		return view.host
	}
	return currentHost()
}

func captureViewOf(adapter Adapter) *captureView {
	if captured, ok := adapter.(interface{ capture() *captureView }); ok {
		return captured.capture()
	}
	return nil
}

// IngestContext stops between workspace records once ctx is cancelled. Each
// record commits on its own, so the source is left "indexing" and the next
// sync resumes it, as after a crash.
func (c *Catalog) IngestContext(ctx context.Context, adapter Adapter, progress ProgressFunc) IngestResult {
	config := adapter.Config()
	host, view := sourceHost(adapter), captureViewOf(adapter)
	result := IngestResult{Source: config.Name, Host: host.ID}
	if host.Fallback {
		result.Error = errHostUnknown.Error()
		return result
	}
	report := func(phase string) {
		if progress != nil {
			progress(phase, result.Workspaces, result.Conversations, result.Messages, result.SkippedCurrent)
		}
	}
	state := func(coverage, cursor, fingerprint, message string, pending int, success bool) {
		// An index records its host's source state only once it completes: a
		// capture failing to index, perhaps on another Mac, says nothing about
		// the source.
		if view != nil && coverage != "complete" {
			return
		}
		c.recordSourceState(host.ID, config.Name, config.Kind, adapter.Capability(), coverage, cursor, fingerprint, message, pending, success)
	}
	if config.Path == "" {
		message := "FileNotFoundError: configured source path is unavailable: " + config.Path
		state("failed", "", "", message, 0, false)
		result.Error = message
		return result
	}
	if _, err := os.Stat(config.Path); err != nil {
		message := "FileNotFoundError: configured source path is unavailable: " + config.Path
		state("failed", "", "", message, 0, false)
		result.Error = message
		return result
	}
	report("checking")
	seen := ""
	if view != nil {
		seen = view.seen()
	}
	_ = c.registerHost(host, seen)
	fingerprint, err := adapter.Fingerprint()
	if err != nil {
		message := fmt.Sprintf("%T: %v", err, err)
		state("failed", "", "", message, 0, false)
		result.Error = message
		return result
	}
	if view != nil && view.offHost() {
		// Indexed without the host's Git checkouts at hand: the host's own sync
		// must not take it as current (see partTracker).
		fingerprint = "offhost:" + fingerprint
	}
	var priorFingerprint, lastSuccess, priorError, coverage sql.NullString
	var pending int64
	err = c.DB.QueryRow("SELECT fingerprint,last_success_at,error,pending_count,coverage FROM source_states WHERE host_id=? AND source_name=?", host.ID, config.Name).Scan(&priorFingerprint, &lastSuccess, &priorError, &pending, &coverage)
	if err == nil && priorFingerprint.String == fingerprint && lastSuccess.Valid && !priorError.Valid && pending == 0 && coverage.String == "complete" {
		state("complete", fingerprint, fingerprint, "", 0, true)
		result.SkippedUnchanged = true
		report("complete")
		return result
	}
	// Live sources can change their file-level fingerprint while a long scan is
	// interrupted. Per-record digests take precedence; this extractor check is
	// a compatibility path for records ingested before digests were recorded.
	resume := err == nil && coverage.String == "indexing"
	state("indexing", "", fingerprint, "", -1, false)
	report("indexing")
	err = c.ingestPass(ctx, adapter, host.ID, resume, &result, report, func(sourceID string) {
		state("indexing", sourceID, fingerprint, "", -1, false)
	})
	if err != nil && ctx.Err() != nil {
		result.Error = "interrupted: Pharos stopped before this source finished; the next sync resumes it"
		report("interrupted")
		return result
	}
	if err != nil {
		message := fmt.Sprintf("%T: %v", err, err)
		state("failed", "", fingerprint, message, 0, false)
		result.Error = message
		report("failed")
		return result
	}
	if provider, ok := adapter.(tl1SnapshotProvider); ok {
		if err := c.storeTL1Snapshots(provider.tl1Snapshots(), host.ID, view != nil); err != nil {
			message := fmt.Sprintf("TL1 analysis snapshot: %v", err)
			state("failed", "", fingerprint, message, 0, false)
			result.Error = message
			report("failed")
			return result
		}
	}
	state("complete", fingerprint, fingerprint, "", 0, true)
	report("complete")
	return result
}

// ingestPass ingests the records an adapter discovers, or for an incremental
// adapter those of the parts that changed, as host's copy. cursor records
// progress every 25 workspaces written.
func (c *Catalog) ingestPass(ctx context.Context, adapter Adapter, host string, resume bool, result *IngestResult, report func(string), cursor func(string)) error {
	config := adapter.Config()
	var tracker *partTracker
	partial, incremental := adapter.(partialAdapter)
	if incremental {
		var err error
		if tracker, err = c.newPartTracker(host, config.Name, partial, captureViewOf(adapter)); err != nil {
			return err
		}
	}
	process := func(record *WorkspaceRecord, parts []sourcePart) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		result.Parsed += len(parts)
		if record == nil {
			return tracker.hold(c, parts)
		}
		digest, err := workspaceRecordDigest(*record)
		if err != nil {
			return err
		}
		var storedDigest string
		if err := c.DB.QueryRow(`SELECT digest FROM source_record_states WHERE host_id=? AND source_name=? AND source_kind=?
			AND source_account=? AND source_id=?`, host, config.Name, record.SourceKind, record.Account, record.SourceID).Scan(&storedDigest); err == nil && storedDigest == digest {
			result.SkippedCurrent++
			report("indexing")
			return tracker.hold(c, parts)
		} else if err != nil && err != sql.ErrNoRows {
			return err
		}
		// Parts supersede this: a record skipped here would have its part
		// recorded as current and never be parsed again.
		if resume && !incremental && c.recordUsesCurrentExtractor(adapter, *record, host) {
			result.SkippedCurrent++
			report("indexing")
			return tracker.hold(c, parts)
		}
		tx, err := c.beginWrite(context.Background())
		if err != nil {
			return err
		}
		copied, err := sightCopiedRecord(tx, *record, digest, host, config.Name)
		if err != nil {
			tx.Rollback()
			return err
		}
		conversations, messages := 0, 0
		if !copied {
			conversations, messages, err = ingestCopy(tx, *record, adapter.Capability() == "tl1-release", host, config.Name, captureViewOf(adapter) != nil)
			if errors.Is(err, errOlderCopy) {
				// Nothing is recorded, so the newer read's digest and parts stand.
				tx.Rollback()
				result.Older++
				report("indexing")
				return nil
			} else if err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO source_record_states(host_id,source_name,source_kind,source_account,source_id,digest,updated_at)
			VALUES(?,?,?,?,?,?,?) ON CONFLICT(host_id,source_name,source_kind,source_account,source_id) DO UPDATE SET
			digest=excluded.digest,updated_at=excluded.updated_at`, host, config.Name, record.SourceKind, record.Account, record.SourceID, digest, now()); err != nil {
			tx.Rollback()
			return err
		}
		if tracker != nil {
			if err := tracker.record(tx, parts); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		c.boundWAL(walSizeLimit)
		if copied {
			result.SkippedCurrent++
			report("indexing")
			return nil
		}
		result.Workspaces++
		result.Conversations += conversations
		result.Messages += messages
		if result.Workspaces == 1 || result.Workspaces%25 == 0 {
			cursor(record.SourceID)
		}
		report("indexing")
		return nil
	}
	if !incremental {
		return adapter.Discover(func(record WorkspaceRecord) error { return process(&record, nil) })
	}
	err := partial.discoverParts(func(parts []sourcePart) bool {
		if !tracker.unchanged(parts) {
			return false
		}
		result.Unchanged += len(parts)
		return true
	}, process)
	if flushErr := tracker.flush(c); err == nil {
		err = flushErr
	}
	return err
}

// beginWrite starts a transaction holding the write lock from its first
// statement. ingestCopy reads before it writes, and a deferred transaction
// whose read snapshot is overtaken by another commit (library maintenance
// commits constantly during a sync) fails its first write at once instead of
// waiting out busy_timeout.
func (c *Catalog) beginWrite(ctx context.Context) (*sql.Tx, error) {
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM meta WHERE 0"); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func workspaceRecordDigest(record WorkspaceRecord) (string, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return "go-v1:" + hashBytes(encoded), nil
}

func (c *Catalog) recordUsesCurrentExtractor(adapter Adapter, record WorkspaceRecord, host string) bool {
	versioned, ok := adapter.(VersionedAdapter)
	if !ok || versioned.ExtractorVersion() == "" {
		return false
	}
	var count int
	var indexedAt sql.NullString
	if err := c.DB.QueryRow(`SELECT COUNT(*),MAX(w.indexed_at) FROM conversations c JOIN workspaces w ON w.id=c.workspace_id
		WHERE w.source_kind=? AND w.source_account=? AND w.source_id=? AND c.coverage=?
		AND EXISTS(SELECT 1 FROM workspace_sightings s WHERE s.workspace_id=w.id AND s.host_id=?)`, record.SourceKind, record.Account, record.SourceID, versioned.ExtractorVersion(), host).Scan(&count, &indexedAt); err != nil {
		return false
	}
	if record.ActivityAt != "" {
		activity, activityOK := parseTime(record.ActivityAt)
		indexed, indexedOK := parseTime(indexedAt.String)
		if !activityOK || !indexedOK || activity.After(indexed) {
			return false
		}
	}
	return count >= len(record.Conversations) && len(record.Conversations) > 0
}

// IngestExisting repairs only source IDs already present in the catalog. It
// intentionally leaves source coverage partial so it can never masquerade as
// a complete source discovery.
func (c *Catalog) IngestExisting(adapter Adapter, progress ProgressFunc) IngestResult {
	config := adapter.Config()
	result := IngestResult{Source: config.Name, ExistingOnly: true}
	selective, ok := adapter.(SelectiveAdapter)
	if !ok {
		result.Error = "adapter does not support bounded existing-row repair"
		return result
	}
	// Only records this host saw from this source: another Mac's records are
	// not in this Mac's source, and a recycled path here may be unrelated.
	host := sourceHost(adapter)
	if host.Fallback {
		result.Error = errHostUnknown.Error()
		return result
	}
	_ = c.registerHost(host, "")
	rows, err := queryMaps(c.DB, `SELECT DISTINCT w.source_id FROM workspace_sightings s JOIN workspaces w ON w.id=s.workspace_id
		WHERE s.host_id=? AND (s.source_name=? OR (s.source_name='' AND w.source_kind=? AND w.source_account=?))`, host.ID, config.Name, config.Kind, config.Account)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	selected := map[string]bool{}
	for _, row := range rows {
		selected[firstString(row["source_id"])] = true
	}
	c.recordSourceState(host.ID, config.Name, config.Kind, adapter.Capability(), "repairing-existing", "", "", "", len(selected), false)
	err = selective.DiscoverSelected(selected, func(record WorkspaceRecord) error {
		if !selected[record.SourceID] {
			return fmt.Errorf("selective adapter emitted unrequested source id %q", record.SourceID)
		}
		tx, beginErr := c.beginWrite(context.Background())
		if beginErr != nil {
			return beginErr
		}
		conversations, messages, ingestErr := ingestCopy(tx, record, adapter.Capability() == "tl1-release", host.ID, config.Name, captureViewOf(adapter) != nil)
		if errors.Is(ingestErr, errOlderCopy) {
			tx.Rollback()
			delete(selected, record.SourceID)
			return nil
		}
		if ingestErr != nil {
			tx.Rollback()
			return ingestErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return commitErr
		}
		c.boundWAL(walSizeLimit)
		result.Workspaces++
		result.Conversations += conversations
		result.Messages += messages
		delete(selected, record.SourceID)
		c.recordSourceState(host.ID, config.Name, config.Kind, adapter.Capability(), "repairing-existing", record.SourceID, "", "", len(selected), false)
		if progress != nil {
			progress("repairing-existing", result.Workspaces, result.Conversations, result.Messages, result.SkippedCurrent)
		}
		return nil
	})
	if err != nil {
		message := fmt.Sprintf("%T: %v", err, err)
		c.recordSourceState(host.ID, config.Name, config.Kind, adapter.Capability(), "failed", "", "", message, len(selected), false)
		result.Error = message
		return result
	}
	if len(selected) != 0 {
		message := fmt.Sprintf("%d existing source IDs were not found in the configured source", len(selected))
		c.recordSourceState(host.ID, config.Name, config.Kind, adapter.Capability(), "partial-existing", "", "", message, len(selected), false)
		result.Error = message
		return result
	}
	c.recordSourceState(host.ID, config.Name, config.Kind, adapter.Capability(), "partial-existing", "", "", "", 0, true)
	return result
}

func (c *Catalog) RecordSourceState(name, kind, capability, coverage, cursor, fingerprint, errorText string, pending int, success bool) {
	c.recordSourceState(currentHost().ID, name, kind, capability, coverage, cursor, fingerprint, errorText, pending, success)
}

func (c *Catalog) recordSourceState(host, name, kind, capability, coverage, cursor, fingerprint, errorText string, pending int, success bool) {
	timestamp := now()
	var successAt any
	if success {
		successAt = timestamp
	}
	var errorValue any
	if errorText != "" {
		errorValue = errorText
	}
	_, _ = c.DB.Exec(`INSERT INTO source_states(host_id,source_name,kind,capability,cursor,fingerprint,coverage,last_attempt_at,last_success_at,error,pending_count,updated_at)
	VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(host_id,source_name) DO UPDATE SET kind=excluded.kind,capability=excluded.capability,
	cursor=COALESCE(excluded.cursor,source_states.cursor),fingerprint=COALESCE(excluded.fingerprint,source_states.fingerprint),coverage=excluded.coverage,
	last_attempt_at=excluded.last_attempt_at,last_success_at=COALESCE(excluded.last_success_at,source_states.last_success_at),error=excluded.error,
	pending_count=excluded.pending_count,updated_at=excluded.updated_at`, host, name, kind, capability, nilIfEmpty(cursor), nilIfEmpty(fingerprint), coverage, timestamp, successAt, errorValue, pending, timestamp)
}

func ingestWorkspace(tx *sql.Tx, record WorkspaceRecord, allowReclamation bool) (int, int, error) {
	return ingestCopy(tx, record, allowReclamation, currentHost().ID, "", false)
}

// ingestCopy applies one host's copy of a workspace from a source. See
// copies.go for when a copy may replace or prune what is stored, and
// guardCapture for a copy read from a capture.
func ingestCopy(tx *sql.Tx, record WorkspaceRecord, allowReclamation bool, host, source string, fromCapture bool) (int, int, error) {
	record = canonicalRecordTimes(record)
	workspaceID, err := storedWorkspaceID(tx, record)
	if err != nil {
		return 0, 0, err
	}
	if (record.SourceKind == "conductor" || record.SourceKind == "tl1") && workspaceID != "" {
		for _, conversation := range record.Conversations {
			if conversation.Provider != record.SourceKind {
				// Preserve the existing conversation/message identity while correcting
				// the old source-container provider to the native agent provider.
				if _, err := tx.Exec("UPDATE conversations SET provider=? WHERE workspace_id=? AND provider=? AND account=? AND native_id=?", conversation.Provider, workspaceID, record.SourceKind, conversation.Account, conversation.NativeID); err != nil {
					return 0, 0, err
				}
			}
		}
	}
	authority, err := resolveAuthority(tx, record, workspaceID, host)
	if err != nil {
		return 0, 0, err
	}
	if fromCapture {
		if err := guardCapture(tx, record, workspaceID, host, &authority); err != nil {
			return 0, 0, err
		}
	}
	repoID := ""
	if record.Repository != nil && !authority.older {
		repoID, err = upsertRepository(tx, record.Repository)
		if err != nil {
			return 0, 0, err
		}
	}
	metadata := record.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	if authority.workspace {
		workspaceID, err = upsertWorkspace(tx, record, repoID, metadata, allowReclamation)
	} else {
		err = mergeWorkspace(tx, workspaceID, record, repoID, metadata)
	}
	if err != nil {
		return 0, 0, err
	}
	workItemID := ""
	if authority.older {
		// Work items, attempts and handoffs are the workspace's own, which an
		// older copy leaves as they are.
		if err := tx.QueryRow("SELECT COALESCE(MAX(id),'') FROM work_items WHERE workspace_id=?", workspaceID).Scan(&workItemID); err != nil {
			return 0, 0, err
		}
	}
	for _, item := range record.WorkItems {
		if authority.older {
			break
		}
		if firstString(item["source_kind"]) == "" {
			item["source_kind"] = record.SourceKind
		}
		if firstString(item["source_account"]) == "" {
			item["source_account"] = record.Account
		}
		if firstString(item["source_id"]) == "" {
			item["source_id"] = firstString(item["id"])
		}
		workItemID, err = upsertWorkItem(tx, workspaceID, item)
		if err != nil {
			return 0, 0, err
		}
	}
	if workItemID == "" && !authority.older {
		workItemID, err = upsertWorkItem(tx, workspaceID, map[string]any{"source_kind": record.SourceKind, "source_account": record.Account, "source_id": record.SourceID, "title": record.Title, "status": metadata["status"], "explicit": true})
		if err != nil {
			return 0, 0, err
		}
	}
	if !authority.older {
		if err := upsertAttempts(tx, workItemID, record.Attempts); err != nil {
			return 0, 0, err
		}
		if err := upsertHandoffs(tx, workspaceID, record.Handoffs); err != nil {
			return 0, 0, err
		}
	}
	messageCount, conversationCount := 0, 0
	conversationIDs := make([]string, len(record.Conversations))
	merged := map[string]bool{}
	written := 0
	for index, conversation := range record.Conversations {
		if authority.conversations[index].older {
			continue
		}
		messageCount += len(conversation.Messages)
		conversationCount++
		if claim := authority.conversations[index]; !claim.writer {
			conversationIDs[index] = claim.id
			added, err := mergeConversation(tx, claim, conversation)
			if err != nil {
				return 0, 0, err
			}
			if added {
				merged[claim.id] = true
			}
			if err := linkAliases(tx, claim.id, conversation.Aliases); err != nil {
				return 0, 0, err
			}
			continue
		}
		written++
		conversationID, err := upsertConversation(tx, workspaceID, workItemID, conversation, host)
		if err != nil {
			return 0, 0, err
		}
		conversationIDs[index] = conversationID
		retained := map[string]bool{}
		textChanged := false
		for sourceOrder, message := range conversation.Messages {
			message.SourceOrder = sourceOrder
			id, changed, err := upsertMessage(tx, conversationID, message)
			if err != nil {
				return 0, 0, err
			}
			textChanged = textChanged || changed
			retained[id] = true
		}
		var stored int
		if err := tx.QueryRow("SELECT COUNT(*) FROM messages WHERE conversation_id=?", conversationID).Scan(&stored); err != nil {
			return 0, 0, err
		}
		staleMessages := stored > len(retained)
		if !staleMessages && textChanged {
			rows, err := queryMaps(tx, "SELECT id FROM messages WHERE conversation_id=?", conversationID)
			if err != nil {
				return 0, 0, err
			}
			for _, row := range rows {
				if !retained[firstString(row["id"])] {
					staleMessages = true
					break
				}
			}
		}
		if textChanged || staleMessages {
			if err := deleteConversationFTS(tx, conversationID); err != nil {
				return 0, 0, err
			}
		}
		if staleMessages {
			if err := pruneMessages(tx, conversationID, retained); err != nil {
				return 0, 0, err
			}
		}
		if textChanged || staleMessages {
			if err := insertConversationFTS(tx, conversationID); err != nil {
				return 0, 0, err
			}
		}
		if err := upsertConversationDocument(tx, conversationID); err != nil {
			return 0, 0, err
		}
		if err := linkAliases(tx, conversationID, conversation.Aliases); err != nil {
			return 0, 0, err
		}
		if err := replaceAgentSessions(tx, workspaceID, conversationID, conversation); err != nil {
			return 0, 0, err
		}
		if err := replaceToolLedger(tx, workspaceID, conversationID, conversation); err != nil {
			return 0, 0, err
		}
	}
	if authority.workspace {
		metrics := record.Metrics
		if tokens := reconciledTokenMetrics(record); len(tokens) > 0 {
			if _, err := tx.Exec("DELETE FROM metrics WHERE workspace_id=? AND unit='tokens'", workspaceID); err != nil {
				return 0, 0, err
			}
			metrics = append([]map[string]any{}, tokens...)
			for _, metric := range record.Metrics {
				if firstString(metric["unit"]) != "tokens" {
					metrics = append(metrics, metric)
				}
			}
		}
		if err := replaceMetrics(tx, workspaceID, metrics); err != nil {
			return 0, 0, err
		}
		if err := replaceMetrics(tx, workspaceID, derivedMetrics(record)); err != nil {
			return 0, 0, err
		}
		if err := metricLedger(tx, workspaceID, record); err != nil {
			return 0, 0, err
		}
	} else if len(merged) > 0 || written > 0 {
		if err := rederiveWorkspace(tx, workspaceID, merged); err != nil {
			return 0, 0, err
		}
	}
	if !authority.older {
		if err := upsertChanges(tx, workspaceID, record.Changes); err != nil {
			return 0, 0, err
		}
		if err := upsertPRs(tx, workspaceID, repoID, record.PRs); err != nil {
			return 0, 0, err
		}
	}
	if authority.workspace {
		if err := upsertSummary(tx, workspaceID, record); err != nil {
			return 0, 0, err
		}
	}
	if record.ActivityAt != "" && !authority.older {
		_, err = tx.Exec(`INSERT INTO activity_events(workspace_id,source,kind,meaningful,occurred_at,evidence_json) VALUES(?,?,?,?,?,?)`, workspaceID, record.SourceKind, "native", 1, record.ActivityAt, "{}")
		if err != nil {
			return 0, 0, err
		}
	}
	if err := recordSightings(tx, workspaceID, conversationIDs, record, host, source, !authority.older); err != nil {
		return 0, 0, err
	}
	if err := recordCopyVersions(tx, host, workspaceID, conversationIDs, record, !authority.older); err != nil {
		return 0, 0, err
	}
	return conversationCount, messageCount, nil
}

// errOlderCopy rejects a capture's record holding nothing newer than what its
// host already wrote.
var errOlderCopy = errors.New("the capture is older than what its host already indexed")

// guardCapture keeps a record read from a capture from deleting, overwriting
// or rolling back what its host wrote from a newer read of the source, however
// that was recorded (see observe). A conversation copy older than the one
// stored is left alone, and then so are the workspace's own fields: only a copy
// no older in any part may replace them, and prune, as a live sync does. A
// record with nothing newer is rejected with errOlderCopy. Equal versions are
// the same content, so they pass.
func guardCapture(tx *sql.Tx, record WorkspaceRecord, workspaceID, host string, authority *copyAuthority) error {
	if workspaceID == "" {
		return nil
	}
	older := 0
	for index, conversation := range record.Conversations {
		claim := &authority.conversations[index]
		if claim.id == "" {
			continue
		}
		stored, known, err := storedCopyVersion(tx, host, workspaceID, claim.id, conversation.Origin)
		if err != nil {
			return err
		}
		if known && conversation.Observed < stored {
			claim.older = true
			older++
		}
	}
	stored, known, err := storedCopyVersion(tx, host, workspaceID, "", "")
	if err != nil {
		return err
	}
	workspaceOlder := known && record.Observed < stored
	if older == len(record.Conversations) && (older > 0 || workspaceOlder) {
		return errOlderCopy
	}
	if older > 0 || workspaceOlder {
		authority.older, authority.workspace = true, false
	}
	return nil
}

// storedCopyVersion is the version host last wrote a workspace's own fields
// (conversation "") or a conversation copy from. Rows written before versions
// were recorded fall back to when their sighting was last written, which is
// no earlier than the read it came from.
func storedCopyVersion(tx *sql.Tx, host, workspaceID, conversationID, origin string) (int64, bool, error) {
	var version int64
	err := tx.QueryRow("SELECT version FROM copy_versions WHERE host_id=? AND workspace_id=? AND conversation_id=? AND origin=?",
		host, workspaceID, conversationID, origin).Scan(&version)
	if err != sql.ErrNoRows {
		return version, err == nil, err
	}
	var seen sql.NullString
	if conversationID == "" {
		err = tx.QueryRow("SELECT MAX(last_seen_at) FROM workspace_sightings WHERE workspace_id=? AND host_id=?", workspaceID, host).Scan(&seen)
	} else {
		err = tx.QueryRow("SELECT last_seen_at FROM conversation_sightings WHERE conversation_id=? AND host_id=? AND origin=?", conversationID, host, origin).Scan(&seen)
	}
	if err == sql.ErrNoRows {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	at, ok := parseTime(seen.String)
	return at.UnixNano(), ok, nil
}

// recordCopyVersions records the versions host's copy was written from: the
// conversations written, and the workspace's own fields unless an older copy
// left them alone.
func recordCopyVersions(tx *sql.Tx, host, workspaceID string, conversationIDs []string, record WorkspaceRecord, workspace bool) error {
	timestamp := now()
	write := func(conversationID, origin string, version int64) error {
		if version <= 0 {
			return nil
		}
		_, err := tx.Exec(`INSERT INTO copy_versions(host_id,workspace_id,conversation_id,origin,version,written_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(host_id,workspace_id,conversation_id,origin) DO UPDATE SET version=excluded.version,written_at=excluded.written_at`,
			host, workspaceID, conversationID, origin, version, timestamp)
		return err
	}
	if workspace {
		if err := write("", "", record.Observed); err != nil {
			return err
		}
	}
	for index, conversation := range record.Conversations {
		if conversationIDs[index] != "" {
			if err := write(conversationIDs[index], conversation.Origin, conversation.Observed); err != nil {
				return err
			}
		}
	}
	return nil
}

// canonicalRecordTimes returns record with every stored timestamp in
// timeLayout. Adapters already use iso(); this also covers map rows passed
// through from source files. Unparseable values are kept verbatim.
func canonicalRecordTimes(record WorkspaceRecord) WorkspaceRecord {
	record.ActivityAt = iso(record.ActivityAt)
	conversations := make([]ConversationRecord, len(record.Conversations))
	for index, conversation := range record.Conversations {
		conversation.StartedAt = iso(conversation.StartedAt)
		conversation.EndedAt = iso(conversation.EndedAt)
		messages := make([]MessageRecord, len(conversation.Messages))
		for position, message := range conversation.Messages {
			message.CreatedAt = iso(message.CreatedAt)
			messages[position] = message
		}
		conversation.Messages = messages
		conversations[index] = conversation
	}
	record.Conversations = conversations
	record.Attempts = canonicalRowTimes(record.Attempts, "started_at", "ended_at")
	record.Handoffs = canonicalRowTimes(record.Handoffs, "created_at")
	record.Metrics = canonicalRowTimes(record.Metrics, "observed_at")
	record.Changes = canonicalRowTimes(record.Changes, "created_at")
	record.PRs = canonicalRowTimes(record.PRs, "observed_at")
	return record
}

func canonicalRowTimes(rows []map[string]any, keys ...string) []map[string]any {
	if rows == nil {
		return nil
	}
	output := make([]map[string]any, len(rows))
	for index, row := range rows {
		copied := make(map[string]any, len(row))
		for key, value := range row {
			copied[key] = value
		}
		for _, key := range keys {
			if value, ok := copied[key]; ok && value != nil {
				copied[key] = nilIfEmpty(iso(value))
			}
		}
		output[index] = copied
	}
	return output
}

func upsertRepository(tx *sql.Tx, value map[string]any) (string, error) {
	canonical := firstString(value["canonical_remote"])
	id := firstString(value["id"])
	if id == "" {
		identity := canonical
		if identity == "" {
			identity = firstString(value["display_name"])
		}
		id = stableID("repo", identity, value["owner"])
	}
	timestamp := now()
	display := defaultString(value["display_name"], canonical)
	if display == "" {
		display = "Unknown repository"
	}
	_, err := tx.Exec(`INSERT INTO repositories(id,canonical_remote,display_name,owner,aliases_json,local_locations_json,created_at,updated_at)
	VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET canonical_remote=COALESCE(excluded.canonical_remote,repositories.canonical_remote),display_name=excluded.display_name,
	owner=COALESCE(excluded.owner,repositories.owner),aliases_json=excluded.aliases_json,local_locations_json=excluded.local_locations_json,updated_at=excluded.updated_at`, id, nilIfEmpty(canonical), display, nilIfEmpty(firstString(value["owner"])), jsonText(valueOr(value["aliases"], []any{})), jsonText(valueOr(value["local_locations"], []any{})), timestamp, timestamp)
	return id, err
}

func valueOr(value, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}
func boolInt(value any) int {
	if item, ok := value.(bool); ok && item {
		return 1
	}
	if integer(value) != 0 {
		return 1
	}
	return 0
}

func upsertWorkspace(tx *sql.Tx, record WorkspaceRecord, repoID string, metadata map[string]any, allow bool) (string, error) {
	id := stableID("workspace", record.SourceKind, record.Account, record.SourceID)
	_, err := tx.Exec(`INSERT INTO workspaces(id,source_kind,source_account,source_id,repository_id,title,purpose,outcome,location,location_history_json,base_ref,head_ref,branch,owner,flavor,version,lifecycle,activity_at,activity_source,terminal,custody_version,reclamation_authority,preservation_completeness,indexed_at)
	VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(source_kind,source_account,source_id) DO UPDATE SET
	repository_id=COALESCE(excluded.repository_id,workspaces.repository_id),title=excluded.title,purpose=COALESCE(excluded.purpose,workspaces.purpose),outcome=COALESCE(excluded.outcome,workspaces.outcome),location=COALESCE(excluded.location,workspaces.location),location_history_json=excluded.location_history_json,
	base_ref=COALESCE(excluded.base_ref,workspaces.base_ref),head_ref=COALESCE(excluded.head_ref,workspaces.head_ref),branch=COALESCE(excluded.branch,workspaces.branch),owner=COALESCE(excluded.owner,workspaces.owner),flavor=COALESCE(excluded.flavor,workspaces.flavor),version=COALESCE(excluded.version,workspaces.version),lifecycle=excluded.lifecycle,
	activity_at=CASE WHEN excluded.activity_at IS NULL THEN workspaces.activity_at WHEN workspaces.activity_at IS NULL OR excluded.activity_at>workspaces.activity_at THEN excluded.activity_at ELSE workspaces.activity_at END,
	activity_source=CASE WHEN excluded.activity_at IS NOT NULL AND (excluded.activity_at>=workspaces.activity_at OR workspaces.activity_at IS NULL) THEN excluded.activity_source ELSE workspaces.activity_source END,
	terminal=excluded.terminal,custody_version=COALESCE(excluded.custody_version,workspaces.custody_version),reclamation_authority=excluded.reclamation_authority,preservation_completeness=excluded.preservation_completeness,indexed_at=excluded.indexed_at`, id, record.SourceKind, record.Account, record.SourceID, nilIfEmpty(repoID), defaultString(record.Title, record.SourceID), nilIfEmpty(record.Purpose), nilIfEmpty(record.Outcome), nilIfEmpty(record.Location), "[]", nilIfEmpty(firstString(metadata["base_ref"])), nilIfEmpty(firstString(metadata["head_ref"])), nilIfEmpty(firstString(metadata["branch"])), nilIfEmpty(firstString(metadata["owner"])), nilIfEmpty(firstString(metadata["flavor"])), nilIfEmpty(firstString(metadata["version"])), defaultString(metadata["lifecycle"], "active"), nilIfEmpty(record.ActivityAt), record.SourceKind+":native", boolInt(metadata["terminal"]), nilIfEmpty(firstString(metadata["custody_version"])), boolInt(allow), "source", now())
	if err != nil {
		return "", err
	}
	err = tx.QueryRow("SELECT id FROM workspaces WHERE source_kind=? AND source_account=? AND source_id=?", record.SourceKind, record.Account, record.SourceID).Scan(&id)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`UPDATE workspaces SET main_merge_commit=COALESCE(?,main_merge_commit),main_merge_title=COALESCE(?,main_merge_title),main_merge_url=COALESCE(?,main_merge_url),main_merge_method=COALESCE(?,main_merge_method) WHERE id=?`,
		nilIfEmpty(firstString(metadata["main_merge_commit"])), nilIfEmpty(firstString(metadata["main_merge_title"])), nilIfEmpty(firstString(metadata["main_merge_url"])), nilIfEmpty(firstString(metadata["main_merge_method"])), id)
	return id, err
}

func upsertWorkItem(tx *sql.Tx, workspaceID string, value map[string]any) (string, error) {
	source := defaultString(value["source_kind"], "unknown")
	account := defaultString(value["source_account"], "local")
	sourceID := firstString(value["source_id"])
	id := stableID("work", source, account, sourceID)
	_, err := tx.Exec(`INSERT INTO work_items(id,workspace_id,source_kind,source_account,source_id,title,status,explicit,relationship_confidence,evidence_json) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(source_kind,source_account,source_id) DO UPDATE SET workspace_id=excluded.workspace_id,title=excluded.title,status=excluded.status,explicit=excluded.explicit,relationship_confidence=excluded.relationship_confidence,evidence_json=excluded.evidence_json`, id, workspaceID, source, account, sourceID, nilIfEmpty(firstString(value["title"])), nilIfEmpty(firstString(value["status"])), boolInt(valueOr(value["explicit"], true)), value["relationship_confidence"], jsonText(valueOr(value["evidence"], map[string]any{})))
	if err != nil {
		return "", err
	}
	err = tx.QueryRow("SELECT id FROM work_items WHERE source_kind=? AND source_account=? AND source_id=?", source, account, sourceID).Scan(&id)
	return id, err
}

func upsertConversation(tx *sql.Tx, workspaceID, workItemID string, value ConversationRecord, host string) (string, error) {
	workItem := nilIfEmpty(workItemID)
	id := stableID("conversation", value.Provider, value.Account, value.NativeID)
	var parent any
	if value.ParentNativeID != "" {
		var found string
		if tx.QueryRow("SELECT id FROM conversations WHERE provider=? AND account=? AND native_id=?", value.Provider, value.Account, value.ParentNativeID).Scan(&found) == nil {
			parent = found
		}
	}
	_, err := tx.Exec(`INSERT INTO conversations(id,workspace_id,work_item_id,provider,model,account,native_id,parent_id,agent_depth,agent_path,agent_nickname,origin,origin_host_id,coverage,started_at,ended_at,aliases_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(provider,account,native_id) DO UPDATE SET workspace_id=excluded.workspace_id,work_item_id=COALESCE(excluded.work_item_id,conversations.work_item_id),parent_id=COALESCE(excluded.parent_id,conversations.parent_id),agent_depth=excluded.agent_depth,agent_path=COALESCE(excluded.agent_path,conversations.agent_path),agent_nickname=COALESCE(excluded.agent_nickname,conversations.agent_nickname),origin=COALESCE(excluded.origin,conversations.origin),origin_host_id=excluded.origin_host_id,model=COALESCE(excluded.model,conversations.model),coverage=excluded.coverage,started_at=COALESCE(excluded.started_at,conversations.started_at),ended_at=MAX(excluded.ended_at,conversations.ended_at),aliases_json=excluded.aliases_json`, id, workspaceID, workItem, value.Provider, nilIfEmpty(value.Model), value.Account, value.NativeID, parent, value.AgentDepth, nilIfEmpty(value.AgentPath), nilIfEmpty(value.AgentNickname), nilIfEmpty(value.Origin), host, defaultString(value.Coverage, "complete"), nilIfEmpty(value.StartedAt), nilIfEmpty(value.EndedAt), jsonText(value.Aliases))
	if err != nil {
		return "", err
	}
	err = tx.QueryRow("SELECT id FROM conversations WHERE provider=? AND account=? AND native_id=?", value.Provider, value.Account, value.NativeID).Scan(&id)
	return id, err
}

func upsertMessage(tx *sql.Tx, conversationID string, value MessageRecord) (string, bool, error) {
	id := stableID("message", conversationID, value.NativeID)
	selected := 1
	if !value.Selected {
		selected = 0
	}
	// Preserve accounting evidence independently of the display text hash.
	contentHash := hashBytes([]byte(value.Text))
	var existingID, existingHash string
	lookupErr := tx.QueryRow("SELECT id,content_hash FROM messages WHERE conversation_id=? AND native_id=?", conversationID, value.NativeID).Scan(&existingID, &existingHash)
	if lookupErr != nil && lookupErr != sql.ErrNoRows {
		return "", false, lookupErr
	}
	if lookupErr == nil && existingHash == contentHash {
		_, updateErr := tx.Exec(`UPDATE messages SET role=?,kind=?,model=?,created_at=?,parent_native_id=?,previous_native_id=?,call_id=?,evidence_locator=?,selected=?,raw_text=?,source_order=?,sender=? WHERE id=?`, defaultString(value.Role, "unknown"), defaultString(value.Kind, "message"), nilIfEmpty(value.Model), nilIfEmpty(value.CreatedAt), nilIfEmpty(value.ParentNativeID), nilIfEmpty(value.PreviousNativeID), nilIfEmpty(value.CallID), nilIfEmpty(value.EvidenceLocator), selected, nilIfEmpty(value.RawText), value.SourceOrder, nilIfEmpty(value.Sender), existingID)
		return existingID, false, updateErr
	}
	if lookupErr == nil {
		id = existingID
	}
	_, err := tx.Exec(`INSERT INTO messages(id,conversation_id,native_id,role,kind,model,text,raw_text,source_order,created_at,parent_native_id,previous_native_id,call_id,evidence_locator,content_hash,selected,sender)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(conversation_id,native_id) DO UPDATE SET
		role=excluded.role,kind=excluded.kind,model=excluded.model,text=excluded.text,raw_text=excluded.raw_text,source_order=excluded.source_order,
		created_at=excluded.created_at,parent_native_id=excluded.parent_native_id,previous_native_id=excluded.previous_native_id,call_id=excluded.call_id,
		evidence_locator=excluded.evidence_locator,content_hash=excluded.content_hash,selected=excluded.selected,sender=excluded.sender`,
		id, conversationID, value.NativeID, defaultString(value.Role, "unknown"), defaultString(value.Kind, "message"), nilIfEmpty(value.Model), value.Text,
		nilIfEmpty(value.RawText), value.SourceOrder, nilIfEmpty(value.CreatedAt), nilIfEmpty(value.ParentNativeID), nilIfEmpty(value.PreviousNativeID),
		nilIfEmpty(value.CallID), nilIfEmpty(value.EvidenceLocator), contentHash, selected, nilIfEmpty(value.Sender))
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

func deleteConversationFTS(tx *sql.Tx, conversationID string) error {
	where := "message_id IN (SELECT id FROM messages WHERE conversation_id=?)"
	if _, err := tx.Exec("DELETE FROM messages_fts WHERE rowid IN (SELECT fts_rowid FROM message_fts_rows WHERE "+where+")", conversationID); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM message_fts_rows WHERE "+where, conversationID)
	return err
}

func insertConversationFTS(tx *sql.Tx, conversationID string) error {
	var lastRowID int64
	if err := tx.QueryRow("SELECT COALESCE(MAX(rowid),0) FROM messages_fts").Scan(&lastRowID); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO messages_fts(message_id,text) SELECT id,text FROM messages WHERE conversation_id=? AND text<>''", conversationID); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO message_fts_rows(message_id,fts_rowid) SELECT message_id,rowid FROM messages_fts WHERE rowid>?", lastRowID)
	return err
}

func pruneMessages(tx *sql.Tx, conversationID string, retained map[string]bool) error {
	rows, err := queryMaps(tx, "SELECT id FROM messages WHERE conversation_id=?", conversationID)
	if err != nil {
		return err
	}
	stale := []any{}
	for _, row := range rows {
		id := firstString(row["id"])
		if !retained[id] {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	for start := 0; start < len(stale); start += 500 {
		batch := stale[start:min(start+500, len(stale))]
		if _, err := tx.Exec("DELETE FROM messages WHERE id IN ("+placeholders(len(batch))+")", batch...); err != nil {
			return err
		}
	}
	return nil
}

func linkAliases(tx *sql.Tx, conversationID string, aliases []string) error {
	if len(aliases) == 0 {
		return nil
	}
	var provider, account string
	if err := tx.QueryRow("SELECT provider,account FROM conversations WHERE id=?", conversationID).Scan(&provider, &account); err != nil {
		return err
	}
	for _, alias := range aliases {
		matches, err := queryMaps(tx, "SELECT id,provider FROM conversations WHERE account=? AND native_id=? AND id<>?", account, alias, conversationID)
		if err != nil {
			return err
		}
		for _, match := range matches {
			left, right := conversationID, firstString(match["id"])
			if left > right {
				left, right = right, left
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO conversation_identity_links(left_id,right_id,relationship,confidence,evidence_json) VALUES(?,?,?,?,?)`, left, right, "native-alias", 1.0, jsonText(map[string]any{"alias": alias, "declared_by": provider})); err != nil {
				return err
			}
		}
	}
	return nil
}

func replaceMetrics(tx *sql.Tx, workspaceID string, values []map[string]any) error {
	for _, value := range values {
		extractor := defaultString(value["extractor_version"], "core-v1")
		if _, err := tx.Exec("DELETE FROM metrics WHERE workspace_id=? AND conversation_id IS NULL AND name=? AND extractor_version=?", workspaceID, firstString(value["name"]), extractor); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO metrics(workspace_id,conversation_id,name,value,unit,status,extractor_version,coverage,definition,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, workspaceID, nil, firstString(value["name"]), value["value"], defaultString(value["unit"], "count"), defaultString(value["status"], "observed"), extractor, value["coverage"], nilIfEmpty(firstString(value["definition"])), defaultString(value["observed_at"], now())); err != nil {
			return err
		}
	}
	return nil
}

func replaceAgentSessions(tx *sql.Tx, workspaceID, conversationID string, conversation ConversationRecord) error {
	if _, err := tx.Exec("DELETE FROM agent_sessions WHERE conversation_id=?", conversationID); err != nil {
		return err
	}
	summaries := agentSessionSummaries(conversation.Messages)
	ids := map[string]string{}
	for _, summary := range summaries {
		ids[summary.nativeID] = stableID("agent-session", conversationID, summary.nativeID)
	}
	for _, summary := range summaries {
		childAgent := conversation.AgentDepth > 0
		var parentID, delegationMessageID any
		if summary.parentNativeID != "" {
			parentID = ids[summary.parentNativeID]
		}
		if summary.delegationMessageIndex >= 0 && summary.delegationMessageIndex < len(conversation.Messages) {
			delegationMessageID = stableID("message", conversationID, conversation.Messages[summary.delegationMessageIndex].NativeID)
		}
		kind := "subagent"
		if summary.nativeID == "main" {
			kind = "root"
			if childAgent {
				kind = "subagent"
			}
		}
		depth := summary.depth
		if childAgent {
			depth += conversation.AgentDepth
		}
		status := "unavailable"
		unclassified := max(summary.counts["total_tokens"]-summary.counts["input_tokens"]-summary.counts["output_tokens"], 0)
		if unclassified > 0 {
			status = "reported-partial"
		} else if summary.counts["total_tokens"] > 0 {
			status = "reported-reconciled"
		}
		if _, err := tx.Exec(`INSERT INTO agent_sessions(
			id,workspace_id,conversation_id,native_id,parent_id,delegation_message_id,kind,provider,model,depth,
			started_at,ended_at,input_tokens,uncached_input_tokens,cache_read_input_tokens,
			cache_creation_input_tokens,cache_creation_5m_input_tokens,cache_creation_1h_input_tokens,
			output_tokens,reasoning_output_tokens,unclassified_tokens,total_tokens,usage_status)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			ids[summary.nativeID], workspaceID, conversationID, summary.nativeID, parentID, delegationMessageID,
			kind, conversation.Provider, nilIfEmpty(conversation.Model), depth, nilIfEmpty(summary.startedAt), nilIfEmpty(summary.endedAt),
			integer(summary.counts["input_tokens"]), integer(summary.counts["uncached_input_tokens"]),
			integer(summary.counts["cache_read_input_tokens"]), integer(summary.counts["cache_creation_input_tokens"]),
			integer(summary.counts["cache_creation_5m_input_tokens"]), integer(summary.counts["cache_creation_1h_input_tokens"]),
			integer(summary.counts["output_tokens"]), integer(summary.counts["reasoning_output_tokens"]),
			integer(unclassified), integer(summary.counts["total_tokens"]), status); err != nil {
			return err
		}
		for bucket, counts := range summary.usage {
			if counts["total_tokens"] <= 0 {
				continue
			}
			hour := defaultString(bucket.hour, usageHour(conversation.StartedAt))
			if _, err := tx.Exec(`INSERT INTO agent_session_usage(
				agent_session_id,usage_hour,model,attribution,input_tokens,uncached_input_tokens,cache_read_input_tokens,
				cache_creation_input_tokens,cache_creation_5m_input_tokens,cache_creation_1h_input_tokens,
				output_tokens,reasoning_output_tokens,unclassified_tokens,total_tokens)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(agent_session_id,usage_hour,model,attribution) DO UPDATE SET
					input_tokens=input_tokens+excluded.input_tokens,
					uncached_input_tokens=uncached_input_tokens+excluded.uncached_input_tokens,
					cache_read_input_tokens=cache_read_input_tokens+excluded.cache_read_input_tokens,
					cache_creation_input_tokens=cache_creation_input_tokens+excluded.cache_creation_input_tokens,
					cache_creation_5m_input_tokens=cache_creation_5m_input_tokens+excluded.cache_creation_5m_input_tokens,
					cache_creation_1h_input_tokens=cache_creation_1h_input_tokens+excluded.cache_creation_1h_input_tokens,
					output_tokens=output_tokens+excluded.output_tokens,
					reasoning_output_tokens=reasoning_output_tokens+excluded.reasoning_output_tokens,
					unclassified_tokens=unclassified_tokens+excluded.unclassified_tokens,
					total_tokens=total_tokens+excluded.total_tokens`,
				ids[summary.nativeID], hour, defaultString(bucket.model, strings.TrimSpace(conversation.Model)), bucket.attribution,
				integer(counts["input_tokens"]),
				integer(max(counts["input_tokens"]-counts["cache_read_input_tokens"]-counts["cache_creation_input_tokens"], 0)),
				integer(counts["cache_read_input_tokens"]), integer(counts["cache_creation_input_tokens"]),
				integer(counts["cache_creation_5m_input_tokens"]), integer(counts["cache_creation_1h_input_tokens"]),
				integer(counts["output_tokens"]), integer(counts["reasoning_output_tokens"]),
				integer(max(counts["total_tokens"]-counts["input_tokens"]-counts["output_tokens"], 0)),
				integer(counts["total_tokens"])); err != nil {
				return err
			}
		}
	}
	for _, summary := range summaries {
		for _, index := range summary.messageIndexes {
			if index < 0 || index >= len(conversation.Messages) {
				continue
			}
			messageID := stableID("message", conversationID, conversation.Messages[index].NativeID)
			if _, err := tx.Exec(`INSERT INTO agent_session_messages(agent_session_id,message_id,source_order) VALUES(?,?,?)
				ON CONFLICT(agent_session_id,message_id) DO UPDATE SET source_order=MIN(source_order,excluded.source_order)`, ids[summary.nativeID], messageID, index); err != nil {
				return err
			}
		}
	}
	return nil
}

func derivedMetrics(record WorkspaceRecord) []map[string]any {
	counts := map[string]float64{"user_turns": 0, "agent_turns": 0, "delegated_turns": 0, "tool_calls": 0, "visible_errors": 0}
	starts, ends := []string{}, []string{}
	for _, conversation := range record.Conversations {
		if conversation.StartedAt != "" {
			starts = append(starts, conversation.StartedAt)
		}
		if conversation.EndedAt != "" {
			ends = append(ends, conversation.EndedAt)
		}
		for _, message := range conversation.Messages {
			if message.Role == "user" && defaultString(message.Kind, "message") == "message" {
				counts["user_turns"]++
			}
			if message.Role == "assistant" && message.Kind != "delegation" {
				counts["agent_turns"]++
			}
			if message.Kind == "delegation" {
				counts["delegated_turns"]++
			}
			if message.Kind == "tool_call" {
				counts["tool_calls"]++
			}
			lower := strings.ToLower(message.Text)
			if strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "failure") {
				counts["visible_errors"]++
			}
		}
	}
	result := []map[string]any{}
	for name, value := range counts {
		result = append(result, map[string]any{"name": name, "value": value, "unit": "count", "status": "derived", "extractor_version": "core-v1", "coverage": 1.0, "definition": "Derived from retained, classified message events."})
	}
	if len(starts) > 0 && len(ends) > 0 {
		sort.Strings(starts)
		sort.Strings(ends)
		start, ok1 := parseTime(starts[0])
		end, ok2 := parseTime(ends[len(ends)-1])
		if ok1 && ok2 {
			seconds := end.Sub(start).Seconds()
			if seconds < 0 {
				seconds = 0
			}
			result = append(result, map[string]any{"name": "wall_clock_span", "value": seconds, "unit": "seconds", "status": "derived", "extractor_version": "core-v1", "coverage": 1.0, "definition": "Elapsed span from earliest available conversation start to latest available end; not execution duration."})
		}
	}
	return result
}

func metricLedger(tx *sql.Tx, workspaceID string, record WorkspaceRecord) error {
	sequence := 0
	for _, conversation := range record.Conversations {
		for _, message := range conversation.Messages {
			if sequence >= 10000 {
				return nil
			}
			kind := defaultString(message.Kind, "message")
			eventKind := kind
			if kind == "message" {
				eventKind = message.Role + "_turn"
			}
			lower := strings.ToLower(message.Text)
			var errorType any
			if strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "failure") {
				errorType = "visible-error"
			}
			if _, err := tx.Exec(`INSERT INTO metric_ledger(workspace_id,sequence,event_kind,error_type,evidence_locator) VALUES(?,?,?,?,?) ON CONFLICT(workspace_id,sequence,event_kind) DO UPDATE SET error_type=excluded.error_type,evidence_locator=excluded.evidence_locator`, workspaceID, sequence, eventKind, errorType, nilIfEmpty(message.EvidenceLocator)); err != nil {
				return err
			}
			sequence++
		}
	}
	return nil
}

func upsertSummary(tx *sql.Tx, workspaceID string, record WorkspaceRecord) error {
	var firstUser, lastAgent *MessageRecord
	failures := []MessageRecord{}
	for ci := range record.Conversations {
		for mi := range record.Conversations[ci].Messages {
			message := &record.Conversations[ci].Messages[mi]
			if message.Text == "" {
				continue
			}
			if firstUser == nil && message.Role == "user" && defaultString(message.Kind, "message") == "message" {
				firstUser = message
			}
			if message.Role == "assistant" && defaultString(message.Kind, "message") == "message" {
				lastAgent = message
			}
			lower := strings.ToLower(message.Text)
			if len(failures) < 3 && (strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "failure")) {
				failures = append(failures, *message)
			}
		}
	}
	bounded := func(message *MessageRecord) any {
		if message == nil {
			return nil
		}
		text := message.Text
		if len(text) > 2000 {
			text = text[:2000]
		}
		return text
	}
	failureTexts := []string{}
	failureLocators := []string{}
	for index := range failures {
		failureTexts = append(failureTexts, fmt.Sprint(bounded(&failures[index])))
		failureLocators = append(failureLocators, failures[index].EvidenceLocator)
	}
	outcome := record.Outcome
	if outcome == "" && lastAgent != nil {
		outcome = fmt.Sprint(bounded(lastAgent))
	}
	evidence := map[string]any{"initiation": nil, "outcome": nil, "failures": failureLocators}
	if firstUser != nil {
		evidence["initiation"] = firstUser.EvidenceLocator
	}
	if lastAgent != nil {
		evidence["outcome"] = lastAgent.EvidenceLocator
	}
	_, err := tx.Exec(`INSERT INTO summaries(workspace_id,initiation,approaches,outcome,failures,unresolved,evidence_json,model,extractor_version,created_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(workspace_id) DO UPDATE SET initiation=excluded.initiation,approaches=excluded.approaches,outcome=excluded.outcome,failures=excluded.failures,unresolved=excluded.unresolved,evidence_json=excluded.evidence_json,model=excluded.model,extractor_version=excluded.extractor_version,created_at=excluded.created_at`, workspaceID, bounded(firstUser), nil, nilIfEmpty(outcome), nilIfEmpty(strings.Join(failureTexts, "\n\n")), nil, jsonText(evidence), "local-extractive", "summary-v1", now())
	if err != nil {
		return err
	}
	semanticParts := []string{record.Title, record.Purpose, record.Outcome}
	if firstUser != nil {
		semanticParts = append(semanticParts, fmt.Sprint(bounded(firstUser)))
	}
	if lastAgent != nil {
		semanticParts = append(semanticParts, fmt.Sprint(bounded(lastAgent)))
	}
	semanticParts = append(semanticParts, failureTexts...)
	semanticText := strings.Join(semanticParts, "\n")
	vector := semanticEmbed(semanticText)
	_, err = tx.Exec(`INSERT INTO semantic_documents(workspace_id,text_hash,vector_json,dimensions,model,indexed_at) VALUES(?,?,?,?,?,?) ON CONFLICT(workspace_id) DO UPDATE SET text_hash=excluded.text_hash,vector_json=excluded.vector_json,dimensions=excluded.dimensions,model=excluded.model,indexed_at=excluded.indexed_at`, workspaceID, hashBytes([]byte(semanticText)), jsonText(vector), len(vector), "local-concept-hash-v1", now())
	return err
}

func upsertChanges(tx *sql.Tx, workspaceID string, changes []map[string]any) error {
	for index, change := range changes {
		id := firstString(change["id"])
		if id == "" {
			id = stableID("changes", workspaceID, change["base_id"], change["head_id"], index)
		}
		if _, err := tx.Exec(`INSERT INTO change_sets(id,workspace_id,base_id,head_id,classification,patch_locator,complete,created_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET patch_locator=excluded.patch_locator,complete=excluded.complete`, id, workspaceID, nilIfEmpty(firstString(change["base_id"])), nilIfEmpty(firstString(change["head_id"])), defaultString(change["classification"], "attempted"), nilIfEmpty(firstString(change["patch_locator"])), boolInt(valueOr(change["complete"], true)), defaultString(change["created_at"], now())); err != nil {
			return err
		}
		for _, file := range mapSlice(change["files"]) {
			if _, err := tx.Exec(`INSERT INTO change_files(change_set_id,path,old_path,status,tracked,bytes,evidence_locator) VALUES(?,?,?,?,?,?,?) ON CONFLICT(change_set_id,path) DO UPDATE SET old_path=excluded.old_path,status=excluded.status,tracked=excluded.tracked,bytes=excluded.bytes,evidence_locator=excluded.evidence_locator`, id, firstString(file["path"]), nilIfEmpty(firstString(file["old_path"])), defaultString(file["status"], "modified"), boolInt(valueOr(file["tracked"], true)), file["bytes"], nilIfEmpty(firstString(file["evidence_locator"]))); err != nil {
				return err
			}
		}
	}
	return nil
}

func upsertAttempts(tx *sql.Tx, workItemID string, attempts []map[string]any) error {
	for index, value := range attempts {
		sourceID := firstString(value["source_id"], value["id"])
		if sourceID == "" {
			sourceID = fmt.Sprint(index)
		}
		id := stableID("attempt", workItemID, sourceID)
		if _, err := tx.Exec(`INSERT INTO task_attempts(id,work_item_id,source_id,source_ref,attempt_no,parent_id,configuration_json,instructions,result,failure,validation,started_at,ended_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(work_item_id,source_id) DO UPDATE SET source_ref=excluded.source_ref,attempt_no=excluded.attempt_no,parent_id=excluded.parent_id,configuration_json=excluded.configuration_json,instructions=excluded.instructions,result=excluded.result,failure=excluded.failure,validation=excluded.validation,started_at=excluded.started_at,ended_at=excluded.ended_at`, id, workItemID, sourceID, value["source_ref"], valueOr(value["attempt_no"], index+1), value["parent_id"], jsonText(valueOr(value["configuration"], map[string]any{})), value["instructions"], value["result"], value["failure"], value["validation"], value["started_at"], value["ended_at"]); err != nil {
			return err
		}
	}
	return nil
}
func upsertHandoffs(tx *sql.Tx, workspaceID string, handoffs []map[string]any) error {
	for index, value := range handoffs {
		id := firstString(value["id"])
		if id == "" {
			id = stableID("handoff", workspaceID, index)
		}
		if _, err := tx.Exec(`INSERT INTO handoffs(id,workspace_id,candidate_id,predecessor_id,successor_id,custody_holder,custody_version,checkpoint_json,integration_evidence_json,closure_evidence_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET custody_holder=excluded.custody_holder,custody_version=excluded.custody_version,checkpoint_json=excluded.checkpoint_json,integration_evidence_json=excluded.integration_evidence_json,closure_evidence_json=excluded.closure_evidence_json`, id, workspaceID, value["candidate_id"], value["predecessor_id"], value["successor_id"], value["custody_holder"], value["custody_version"], jsonText(valueOr(value["checkpoint"], map[string]any{})), jsonText(valueOr(value["integration_evidence"], map[string]any{})), jsonText(valueOr(value["closure_evidence"], map[string]any{})), value["created_at"]); err != nil {
			return err
		}
	}
	return nil
}
func upsertPRs(tx *sql.Tx, workspaceID, repoID string, prs []map[string]any) error {
	for _, value := range prs {
		host := defaultString(value["host"], "github.com")
		number := integer(value["number"])
		var repositoryIdentity any = repoID
		if repoID == "" {
			repositoryIdentity = nil
		}
		id := stableID("pr", host, repositoryIdentity, number)
		if _, err := tx.Exec(`INSERT INTO pull_requests(id,host,repository_id,number,url,title,state,base_ref,head_ref,commit_refs_json,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET url=excluded.url,title=excluded.title,state=excluded.state,base_ref=excluded.base_ref,head_ref=excluded.head_ref,commit_refs_json=excluded.commit_refs_json,observed_at=excluded.observed_at`, id, host, nilIfEmpty(repoID), number, value["url"], value["title"], value["state"], value["base_ref"], value["head_ref"], jsonText(valueOr(value["commit_refs"], []any{})), defaultString(value["observed_at"], now())); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO work_pr_links(workspace_id,pr_id,relationship,confidence,evidence_json) VALUES(?,?,?,?,?) ON CONFLICT(workspace_id,pr_id,relationship) DO UPDATE SET confidence=excluded.confidence,evidence_json=excluded.evidence_json`, workspaceID, id, defaultString(value["relationship"], "associated"), valueOr(value["confidence"], 0.5), jsonText(valueOr(value["evidence"], map[string]any{}))); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) ReconcileIdentities() (int, error) {
	var before int
	if err := c.DB.QueryRow("SELECT COUNT(*) FROM conversation_identity_links").Scan(&before); err != nil {
		return 0, err
	}
	rows, err := queryMaps(c.DB, "SELECT id,aliases_json FROM conversations WHERE aliases_json<>'[]'")
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		var aliases []string
		if jsonErr := decodeJSONText(firstString(row["aliases_json"]), &aliases); jsonErr != nil {
			continue
		}
		tx, err := c.beginWrite(context.Background())
		if err != nil {
			return 0, err
		}
		if err := linkAliases(tx, firstString(row["id"]), aliases); err != nil {
			tx.Rollback()
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
	}
	links, err := queryMaps(c.DB, `SELECT wa.id left_id,wa.source_kind left_kind,wa.activity_at left_activity,
		wb.id right_id,wb.source_kind right_kind,wb.activity_at right_activity
		FROM conversation_identity_links l JOIN conversations ca ON ca.id=l.left_id
		JOIN conversations cb ON cb.id=l.right_id JOIN workspaces wa ON wa.id=ca.workspace_id
		JOIN workspaces wb ON wb.id=cb.workspace_id`)
	if err != nil {
		return 0, err
	}
	for _, link := range links {
		pairs := [][5]string{
			{firstString(link["left_id"]), firstString(link["left_kind"]), firstString(link["left_activity"]), firstString(link["right_kind"]), firstString(link["right_activity"])},
			{firstString(link["right_id"]), firstString(link["right_kind"]), firstString(link["right_activity"]), firstString(link["left_kind"]), firstString(link["left_activity"])},
		}
		for _, pair := range pairs {
			if pair[1] != "tl1" && pair[1] != "tl1-export" {
				continue
			}
			linked, ok := parseTime(pair[4])
			if !ok {
				continue
			}
			activity, hasActivity := parseTime(pair[2])
			if !hasActivity || linked.After(activity) {
				if _, err := c.DB.Exec("UPDATE workspaces SET activity_at=?,activity_source=? WHERE id=?", formatTime(linked), "linked:"+pair[3]+":native-id", pair[0]); err != nil {
					return 0, err
				}
			}
		}
	}
	var after int
	if err := c.DB.QueryRow("SELECT COUNT(*) FROM conversation_identity_links").Scan(&after); err != nil {
		return 0, err
	}
	if after != before {
		// New links can change which mirror represents the work in Tools.
		if err := bumpToolLedgerGeneration(c.DB); err != nil {
			return 0, err
		}
	}
	return after - before, nil
}
func decodeJSONText(text string, value any) error { return json.Unmarshal([]byte(text), value) }
