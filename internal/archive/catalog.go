package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Catalog struct {
	Path    string
	DB      *sql.DB
	library libraryCache
	derived derivedCache
	// quietVersion and warmVersion are the catalog versions warmCaches last
	// saw and warmed; only the Library maintenance loop uses them.
	quietVersion, warmVersion int64
	tools                     toolLedgerState
	// authorship tracks the human-authorship rebuild.
	authorship authorshipState
	wal        walBound
	now        func() time.Time
	// background runs the catalog's own background work (the authorship
	// rebuild). The service sets it to its spawn, so a release cancels the
	// work and waits for it before closing the catalog; nil runs a goroutine.
	background func(work func(context.Context)) bool
}

func OpenCatalog(path string) (*Catalog, error) {
	if err := os.MkdirAll(filepathDir(path), 0o755); err != nil {
		return nil, err
	}
	// SQLite PRAGMAs are connection-local. The DSN applies them to every pooled
	// connection, so deleting a session always cascades its message links and
	// every commit is durable (see catalogDSN).
	db, err := sql.Open("sqlite", catalogDSN(path))
	if err != nil {
		return nil, err
	}
	// SQLite serializes writes. A small pool still allows UI reads while a source
	// snapshot is being parsed without creating unbounded lock contention.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	catalog := &Catalog{Path: path, DB: db}
	if err := catalog.initializeLocked(true); err != nil {
		db.Close()
		return nil, err
	}
	return catalog, nil
}

func filepathDir(path string) string {
	at := strings.LastIndex(path, string(os.PathSeparator))
	if at < 0 {
		return "."
	}
	if at == 0 {
		return string(os.PathSeparator)
	}
	return path[:at]
}

func (c *Catalog) Close() error {
	c.library.mu.Lock()
	c.library.close()
	c.library.mu.Unlock()
	c.derived.mu.Lock()
	c.derived.close()
	c.derived.mu.Unlock()
	// The last connection to close deletes the WAL, but only when no other
	// process (an MCP server, the app) has the catalog open. Empty it either
	// way so the next opener, possibly on another Mac, has no log to replay.
	_ = c.Checkpoint()
	return c.DB.Close()
}

// catalogSchemaVersion is the newest catalog schema this build understands.
// Bump it with any migration an older build must not write around: a library
// drive moves between Macs that may run different builds.
//
// 5: per-host source_states/source_record_states primary keys; a version-4
// build would fail its ON CONFLICT upserts against them.
// 6: source_item_states. A version-5 build ingests without updating it, and
// part states left behind could let an index of an older capture pass for
// newer than what that build wrote, and roll conversations back.
// 7: the tool ledger (tool_calls, model_requests, tool_ledger_state),
// messages.sender, message_authorship, the tl1_* analysis tables, and
// timestamps kept in timeLayout. A version-6 build re-ingests conversations
// without rebuilding their ledger, whose version stamps would then pass the
// stale rows as current; it records a TL1 source as synced without its
// analysis tables; and it writes timestamps that no longer sort as times.
// 8: source index versions make parser-only updates visible to capture status.
const catalogSchemaVersion = 8

// checkSchemaVersion refuses, before any migration runs, a catalog written by
// a newer build.
func (c *Catalog) checkSchemaVersion() error {
	var tables int
	if err := c.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='meta'").Scan(&tables); err != nil || tables == 0 {
		return err
	}
	var stored string
	err := c.DB.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&stored)
	if err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	version, err := strconv.Atoi(strings.TrimSpace(stored))
	if err != nil || version > catalogSchemaVersion {
		return fmt.Errorf("catalog %s has schema version %s, but this build of Pharos supports up to %d; open it with a newer build (the catalog was not modified)", c.Path, stored, catalogSchemaVersion)
	}
	return nil
}

func (c *Catalog) Initialize() error {
	if err := c.checkSchemaVersion(); err != nil {
		return err
	}
	if err := c.migratePricingSchema(); err != nil {
		return err
	}
	if _, err := c.DB.Exec(schemaSQL()); err != nil {
		return fmt.Errorf("initialize catalog: %w", err)
	}
	if err := c.ensureTL1Schema(); err != nil {
		return fmt.Errorf("initialize TL1 tables: %w", err)
	}
	// Keep large bulk imports from spending most of their time merging FTS
	// segments inside individual message inserts. These settings persist in FTS5.
	if _, err := c.DB.Exec("INSERT INTO messages_fts(messages_fts,rank) VALUES('automerge',8)"); err != nil {
		return err
	}
	if _, err := c.DB.Exec("INSERT INTO messages_fts(messages_fts,rank) VALUES('crisismerge',32)"); err != nil {
		return err
	}
	for _, migration := range []struct{ table, column, statement string }{
		{"source_states", "index_version", "ALTER TABLE source_states ADD COLUMN index_version TEXT"},
		{"messages", "raw_text", "ALTER TABLE messages ADD COLUMN raw_text TEXT"},
		{"messages", "source_order", "ALTER TABLE messages ADD COLUMN source_order INTEGER"},
		{"messages", "model", "ALTER TABLE messages ADD COLUMN model TEXT"},
		{"messages", "previous_native_id", "ALTER TABLE messages ADD COLUMN previous_native_id TEXT"},
		{"messages", "sender", "ALTER TABLE messages ADD COLUMN sender TEXT"},
		{"message_authorship", "harness_words", "ALTER TABLE message_authorship ADD COLUMN harness_words INTEGER NOT NULL DEFAULT 0"},
		{"message_authorship", "automated_words", "ALTER TABLE message_authorship ADD COLUMN automated_words INTEGER NOT NULL DEFAULT 0"},
		{"message_authorship", "template_words", "ALTER TABLE message_authorship ADD COLUMN template_words INTEGER NOT NULL DEFAULT 0"},
		{"message_authorship", "attachment_words", "ALTER TABLE message_authorship ADD COLUMN attachment_words INTEGER NOT NULL DEFAULT 0"},
		{"message_authorship", "quoted_words", "ALTER TABLE message_authorship ADD COLUMN quoted_words INTEGER NOT NULL DEFAULT 0"},
		{"message_authorship", "resent_words", "ALTER TABLE message_authorship ADD COLUMN resent_words INTEGER NOT NULL DEFAULT 0"},
		{"conversations", "model", "ALTER TABLE conversations ADD COLUMN model TEXT"},
		{"conversations", "agent_depth", "ALTER TABLE conversations ADD COLUMN agent_depth INTEGER NOT NULL DEFAULT 0"},
		{"conversations", "agent_path", "ALTER TABLE conversations ADD COLUMN agent_path TEXT"},
		{"conversations", "agent_nickname", "ALTER TABLE conversations ADD COLUMN agent_nickname TEXT"},
		{"workspaces", "reclamation_authority", "ALTER TABLE workspaces ADD COLUMN reclamation_authority INTEGER NOT NULL DEFAULT 0"},
		{"workspaces", "main_merge_commit", "ALTER TABLE workspaces ADD COLUMN main_merge_commit TEXT"},
		{"workspaces", "main_merge_title", "ALTER TABLE workspaces ADD COLUMN main_merge_title TEXT"},
		{"workspaces", "main_merge_url", "ALTER TABLE workspaces ADD COLUMN main_merge_url TEXT"},
		{"workspaces", "main_merge_method", "ALTER TABLE workspaces ADD COLUMN main_merge_method TEXT"},
		{"agent_sessions", "unclassified_tokens", "ALTER TABLE agent_sessions ADD COLUMN unclassified_tokens INTEGER NOT NULL DEFAULT 0"},
		{"agent_sessions", "cache_creation_5m_input_tokens", "ALTER TABLE agent_sessions ADD COLUMN cache_creation_5m_input_tokens INTEGER NOT NULL DEFAULT 0"},
		{"agent_sessions", "cache_creation_1h_input_tokens", "ALTER TABLE agent_sessions ADD COLUMN cache_creation_1h_input_tokens INTEGER NOT NULL DEFAULT 0"},
		{"tool_usage_daily", "command_name", "ALTER TABLE tool_usage_daily ADD COLUMN command_name TEXT"},
		{"tool_calls", "url", "ALTER TABLE tool_calls ADD COLUMN url TEXT"},
		{"tool_calls", "host", "ALTER TABLE tool_calls ADD COLUMN host TEXT"},
		{"tool_calls", "hosts", "ALTER TABLE tool_calls ADD COLUMN hosts TEXT"},
		{"tool_calls", "url_count", "ALTER TABLE tool_calls ADD COLUMN url_count INTEGER NOT NULL DEFAULT 0"},
		{"tool_calls", "search_query", "ALTER TABLE tool_calls ADD COLUMN search_query TEXT"},
	} {
		has, err := c.hasColumn(migration.table, migration.column)
		if err != nil {
			return err
		}
		if !has {
			if _, err := c.DB.Exec(migration.statement); err != nil {
				return err
			}
		}
	}
	// Holds every column the Library's rows read (libraryWorkspaceFields), so
	// building them reads this, not the workspace rows, whose purpose and
	// outcome text spills onto overflow pages ahead of the later columns.
	// Created here, once the migrations above have added those columns.
	if _, err := c.DB.Exec(`CREATE INDEX IF NOT EXISTS workspaces_library_idx ON workspaces(id,title,source_kind,activity_at,
		branch,owner,flavor,version,lifecycle,preservation_completeness,main_merge_title,location,repository_id)`); err != nil {
		return err
	}
	if err := c.migrateHosts(); err != nil {
		return err
	}
	// Existing Python catalogs may contain duplicates created before the partial
	// unique index was introduced. Preserve the newest row exactly as before.
	_, _ = c.DB.Exec(`DELETE FROM metrics WHERE conversation_id IS NULL AND id NOT IN (
		SELECT MAX(id) FROM metrics WHERE conversation_id IS NULL GROUP BY workspace_id,name,extractor_version)`)
	_, err := c.DB.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS metrics_workspace_unique_idx
		ON metrics(workspace_id,name,extractor_version) WHERE conversation_id IS NULL`)
	if err != nil {
		return err
	}
	if err := c.ensureLibrary(); err != nil {
		return err
	}
	// Never lower the recorded version, even if a newer build raced this open.
	_, err = c.DB.Exec(`INSERT INTO meta(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE
		SET value=excluded.value WHERE CAST(meta.value AS INTEGER)<CAST(excluded.value AS INTEGER)`, strconv.Itoa(catalogSchemaVersion))
	if err != nil {
		return err
	}
	// Native agent session IDs can equal Conductor's own session IDs. Keep the
	// two source records distinct while preserving existing conversation IDs and
	// their dependent message/agent-session rows.
	if _, err := c.DB.Exec(`UPDATE conversations SET native_id='conductor:'||native_id
		WHERE native_id NOT LIKE 'conductor:%' AND workspace_id IN
		(SELECT id FROM workspaces WHERE source_kind='conductor')`); err != nil {
		return err
	}
	// TL1 transcripts were once stored under provider 'tl1' instead of the
	// agent that wrote them. Re-read TL1 sources once so ingest can correct
	// the provider in place, even when the registry itself is unchanged.
	var tl1ProviderVersion string
	if err := c.DB.QueryRow("SELECT value FROM meta WHERE key='tl1_native_provider_version'").Scan(&tl1ProviderVersion); err == sql.ErrNoRows {
		if _, err = c.DB.Exec("UPDATE source_states SET fingerprint=NULL WHERE kind='tl1'"); err != nil {
			return err
		}
		if _, err = c.DB.Exec("INSERT INTO meta(key,value) VALUES('tl1_native_provider_version','1')"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := c.normalizeTimestamps(); err != nil {
		return err
	}
	var ftsRowsVersion string
	if err := c.DB.QueryRow("SELECT value FROM meta WHERE key='message_fts_rows_version'").Scan(&ftsRowsVersion); err == sql.ErrNoRows {
		// Existing FTS rows need a one-time rowid lookup for bounded updates.
		if _, err = c.DB.Exec(`INSERT OR REPLACE INTO message_fts_rows(message_id,fts_rowid)
			SELECT message_id,rowid FROM messages_fts`); err != nil {
			return err
		}
		if _, err = c.DB.Exec("INSERT INTO meta(key,value) VALUES('message_fts_rows_version','1')"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// A catalog with no conversations yet has nothing for the substring
	// index to catch up on; ingest indexes each conversation it adds.
	if _, err := c.DB.Exec(`INSERT OR IGNORE INTO meta(key,value) SELECT 'message_trigram_version','1'
		WHERE NOT EXISTS(SELECT 1 FROM conversations)`); err != nil {
		return err
	}
	if err := c.backfillAgentSessions(); err != nil {
		return err
	}
	if _, err := c.DB.Exec(`UPDATE conversations SET agent_depth=1 WHERE provider='claude'
		AND parent_id IS NOT NULL AND agent_depth=0 AND origin LIKE '%/subagents/%'`); err != nil {
		return err
	}
	if err := c.backfillChildSessionKinds(); err != nil {
		return err
	}
	if err := c.backfillConversationDocuments(); err != nil {
		return err
	}
	for _, store := range []vectorStore{workspaceVectors, conversationVectors} {
		if err := c.backfillVectors(store); err != nil {
			return err
		}
	}
	if err := c.ensureMessageCount(); err != nil {
		return err
	}
	if err := c.syncPricing(); err != nil {
		return err
	}
	// Last, so a query open skips Initialize only after all of it succeeded.
	return c.recordInitializedBuild()
}

// timestampColumns are the stored instants kept in timeLayout. usage_hour
// (hour buckets) and date-only columns such as cost_changes.retrieved_at keep
// their own fixed-width formats.
var timestampColumns = []struct{ table, column string }{
	{"repositories", "created_at"}, {"repositories", "updated_at"},
	{"workspaces", "activity_at"}, {"workspaces", "indexed_at"},
	{"conversations", "started_at"}, {"conversations", "ended_at"},
	{"mcp_calls", "called_at"},
	{"conversation_documents", "indexed_at"},
	{"messages", "created_at"},
	{"agent_sessions", "started_at"}, {"agent_sessions", "ended_at"},
	{"summaries", "created_at"},
	{"semantic_documents", "indexed_at"},
	{"task_attempts", "started_at"}, {"task_attempts", "ended_at"},
	{"handoffs", "created_at"},
	{"change_sets", "created_at"},
	{"pull_requests", "observed_at"},
	{"metrics", "observed_at"},
	{"source_states", "last_attempt_at"}, {"source_states", "last_success_at"}, {"source_states", "updated_at"},
	{"source_record_states", "updated_at"},
	{"protections", "until_at"}, {"protections", "created_at"},
	{"activity_events", "occurred_at"},
	{"receipts", "created_at"}, {"receipts", "updated_at"},
	{"reclamation", "scheduled_at"}, {"reclamation", "intent_at"}, {"reclamation", "updated_at"},
	{"jobs", "claimed_at"}, {"jobs", "created_at"}, {"jobs", "updated_at"},
}

const timestampFormatVersion = "1"

// normalizeTimestamps rewrites stored timestamps written before every writer
// used timeLayout. Re-ingest cannot be relied on for this: unchanged source
// records are skipped by digest. It returns the number of values rewritten.
func (c *Catalog) normalizeTimestamps() (int, error) {
	var version string
	if err := c.DB.QueryRow("SELECT value FROM meta WHERE key='timestamp_format_version'").Scan(&version); err == nil && version == timestampFormatVersion {
		return 0, nil
	} else if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	total := 0
	for _, target := range timestampColumns {
		updated, err := c.normalizeTimestampColumn(target.table, target.column)
		if err != nil {
			return total, fmt.Errorf("normalize %s.%s: %w", target.table, target.column, err)
		}
		total += updated
	}
	_, err := c.DB.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES('timestamp_format_version',?)", timestampFormatVersion)
	return total, err
}

func (c *Catalog) normalizeTimestampColumn(table, column string) (int, error) {
	// Candidates are collected first so the scan can use a covering index;
	// updates then commit in small batches to keep the writer lock short.
	rows, err := c.DB.Query(fmt.Sprintf(`SELECT rowid,%[2]s FROM %[1]s WHERE %[2]s IS NOT NULL
		AND (length(%[2]s)<>24 OR substr(%[2]s,11,1)<>'T' OR substr(%[2]s,24,1)<>'Z')`, table, column))
	if err != nil {
		return 0, err
	}
	type update struct {
		rowID int64
		value string
	}
	updates := []update{}
	unparsed := 0
	for rows.Next() {
		var rowID int64
		var raw any
		if err := rows.Scan(&rowID, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		if bytes, ok := raw.([]byte); ok {
			raw = string(bytes)
		}
		canonical := canonicalTime(raw)
		if canonical == "" {
			unparsed++
			continue
		}
		if canonical != raw {
			updates = append(updates, update{rowID, canonical})
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if unparsed > 0 {
		fmt.Fprintf(os.Stderr, "Timestamp normalization: kept %d unparseable %s.%s values\n", unparsed, table, column)
	}
	const batchSize = 2000
	for start := 0; start < len(updates); start += batchSize {
		tx, err := c.DB.Begin()
		if err != nil {
			return 0, err
		}
		statement, err := tx.Prepare(fmt.Sprintf("UPDATE %s SET %s=? WHERE rowid=?", table, column))
		if err != nil {
			tx.Rollback()
			return 0, err
		}
		for _, item := range updates[start:min(start+batchSize, len(updates))] {
			if _, err := statement.Exec(item.value, item.rowID); err != nil {
				statement.Close()
				tx.Rollback()
				return 0, err
			}
		}
		statement.Close()
		if err := tx.Commit(); err != nil {
			return 0, err
		}
	}
	return len(updates), nil
}

func (c *Catalog) backfillChildSessionKinds() error {
	children, err := queryMaps(c.DB, `SELECT c.id,c.agent_depth FROM conversations c JOIN agent_sessions a ON a.conversation_id=c.id
		WHERE c.agent_depth>0 AND a.native_id='main' AND a.kind='root'`)
	if err != nil {
		return err
	}
	for _, child := range children {
		if _, err := c.DB.Exec(`UPDATE agent_sessions SET depth=depth+?,
			kind=CASE WHEN native_id='main' THEN 'subagent' ELSE kind END WHERE conversation_id=?`, child["agent_depth"], child["id"]); err != nil {
			return err
		}
	}
	// Codex filename forks are conversation continuations. Their parent link
	// alone does not establish an agent spawn.
	_, err = c.DB.Exec(`UPDATE agent_sessions SET depth=MAX(depth-1,0),
		kind=CASE WHEN native_id='main' THEN 'root' ELSE kind END
		WHERE conversation_id IN (SELECT c.id FROM conversations c JOIN agent_sessions a
			ON a.conversation_id=c.id WHERE c.provider='codex' AND c.agent_depth=0 AND c.parent_id IS NOT NULL
			AND a.native_id='main' AND a.kind='subagent')`)
	if err != nil {
		return err
	}
	return nil
}

func (c *Catalog) backfillAgentSessions() error {
	conversations, err := queryMaps(c.DB, `SELECT c.id,c.workspace_id,c.provider,c.model,c.started_at,c.agent_depth FROM conversations c
		WHERE NOT EXISTS(SELECT 1 FROM agent_sessions a WHERE a.conversation_id=c.id)`)
	if err != nil {
		return err
	}
	for _, row := range conversations {
		if err := c.rebuildAgentSessions(row); err != nil {
			return err
		}
	}
	return c.seedSessionUsage()
}

// seedSessionUsage gives sessions written before the hourly ledger (or by the
// Python importer) one ledger row at their start hour. This is instant; the
// exact per-event timeline needs RefineUsageTimeline, which re-reads messages.
func (c *Catalog) seedSessionUsage() error {
	_, err := c.DB.Exec(`INSERT OR IGNORE INTO agent_session_usage(
		agent_session_id,usage_hour,model,attribution,input_tokens,uncached_input_tokens,cache_read_input_tokens,
		cache_creation_input_tokens,cache_creation_5m_input_tokens,cache_creation_1h_input_tokens,
		output_tokens,reasoning_output_tokens,unclassified_tokens,total_tokens)
		SELECT a.id,COALESCE(strftime('%Y-%m-%dT%H:00:00Z',COALESCE(a.started_at,a.ended_at,c.started_at)),''),
			COALESCE(TRIM(a.model),''),'session-start',a.input_tokens,
			MAX(a.input_tokens-a.cache_read_input_tokens-a.cache_creation_input_tokens,0),a.cache_read_input_tokens,
			a.cache_creation_input_tokens,a.cache_creation_5m_input_tokens,a.cache_creation_1h_input_tokens,
			a.output_tokens,a.reasoning_output_tokens,a.unclassified_tokens,a.total_tokens
		FROM agent_sessions a JOIN conversations c ON c.id=a.conversation_id
		WHERE a.total_tokens>0 AND NOT EXISTS(SELECT 1 FROM agent_session_usage u WHERE u.agent_session_id=a.id)`)
	return err
}

// RefineUsageTimeline re-derives the hourly ledger from retained messages for
// conversations whose usage is only placed at session start. It is bounded to
// those conversations and commits each one separately.
func (c *Catalog) RefineUsageTimeline(progress func(done, total int)) (int, error) {
	conversations, err := queryMaps(c.DB, `SELECT c.id,c.workspace_id,c.provider,c.model,c.started_at,c.agent_depth FROM conversations c
		WHERE c.id IN (SELECT a.conversation_id FROM agent_session_usage u JOIN agent_sessions a ON a.id=u.agent_session_id
			WHERE u.attribution='session-start')`)
	if err != nil {
		return 0, err
	}
	for index, row := range conversations {
		if err := c.rebuildAgentSessions(row); err != nil {
			return index, err
		}
		c.boundWAL(walSizeLimit)
		if progress != nil {
			progress(index+1, len(conversations))
		}
	}
	return len(conversations), nil
}

func (c *Catalog) rebuildAgentSessions(row map[string]any) error {
	records, err := storedMessages(context.Background(), c.DB, row["id"])
	if err != nil {
		return err
	}
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	conversation := ConversationRecord{Provider: firstString(row["provider"]), Model: firstString(row["model"]), StartedAt: firstString(row["started_at"]), AgentDepth: int(integer(row["agent_depth"])), Messages: records}
	if err := replaceAgentSessions(tx, firstString(row["workspace_id"]), firstString(row["id"]), conversation); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (c *Catalog) hasColumn(table, column string) (bool, error) {
	rows, err := c.DB.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func queryMaps(q queryer, statement string, args ...any) ([]map[string]any, error) {
	return queryMapsContext(context.Background(), q, statement, args...)
}

func queryMapsContext(ctx context.Context, q queryer, statement string, args ...any) ([]map[string]any, error) {
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	output := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for index, column := range columns {
			value := values[index]
			if data, ok := value.([]byte); ok {
				value = string(data)
			}
			row[column] = value
		}
		output = append(output, row)
	}
	return output, rows.Err()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Freshness reports this host's sources; other hosts' sources are listed
// separately and never make this host stale, since they sync only when the
// library is attached to them.
func (c *Catalog) Freshness() map[string]any {
	host := currentHost()
	rows, err := queryMaps(c.DB, `SELECT s.*,COALESCE(h.label,s.host_id) host_label FROM source_states s
		LEFT JOIN hosts h ON h.id=s.host_id ORDER BY s.source_name,s.host_id`)
	if err != nil {
		return map[string]any{"sources": []any{}, "stale_sources": 0, "status": "stale", "error": err.Error(), "host": host}
	}
	stale := 0
	sources, others := []map[string]any{}, []map[string]any{}
	for _, row := range rows {
		last := firstString(row["last_success_at"])
		lag := any(nil)
		isStale := true
		if parsed, ok := parseTime(last); ok {
			seconds := int64(time.Since(parsed).Seconds())
			if seconds < 0 {
				seconds = 0
			}
			lag = seconds
			isStale = seconds > 86400
		}
		if firstString(row["error"]) != "" || integer(row["pending_count"]) != 0 || firstString(row["coverage"]) != "complete" {
			isStale = true
		}
		row["lag_seconds"] = lag
		row["stale"] = isStale
		if firstString(row["host_id"]) != host.ID {
			others = append(others, row)
			continue
		}
		sources = append(sources, row)
		if isStale {
			stale++
		}
	}
	status := "stale"
	if len(sources) > 0 && stale == 0 {
		status = "current"
	}
	return map[string]any{"sources": sources, "stale_sources": stale, "status": status, "host": host, "other_host_sources": others}
}

func integer(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		result, _ := v.Int64()
		return result
	case string:
		result, _ := strconv.ParseInt(v, 10, 64)
		return result
	}
	return 0
}

type SearchOptions struct {
	Query, Repository, Source, File, Owner, Provider, Model, From, To, Flavor, Version, Outcome, Error, Metric string
	PR                                                                                                         *int
	Minimum, Maximum                                                                                           *float64
	ChangedOnly                                                                                                bool
	// Substring adds matches inside words, from the substring index.
	Substring     bool
	Limit, Offset int
	// evidence, when set, receives what matched each workspace the query
	// found, for explaining a page of results.
	evidence *map[string]*searchEvidence
	// ctx lets an abandoned HTTP request interrupt the library query.
	ctx context.Context
}

// unfiltered reports whether options select every Library row, which is the
// set cachedLibraryRows keeps between requests.
func (o SearchOptions) unfiltered() bool {
	o.ctx, o.Limit, o.Offset, o.evidence = nil, 0, 0, nil
	// Substring changes only what a query matches.
	o.Substring = o.Substring && o.Query != ""
	return o == SearchOptions{}
}

func (o SearchOptions) context() context.Context {
	if o.ctx == nil {
		return context.Background()
	}
	return o.ctx
}

func (c *Catalog) Search(options SearchOptions) (map[string]any, error) {
	rows, err := c.searchRows(options, libraryCompactFields)
	if err != nil {
		return nil, err
	}
	limit := clamp(options.Limit, 1, 200)
	offset := max(options.Offset, 0)
	end := min(offset+limit, len(rows))
	if offset > len(rows) {
		offset = len(rows)
	}
	items, err := c.libraryPage(options.context(), rows[offset:end])
	if err != nil {
		return nil, err
	}
	return map[string]any{"items": items, "limit": limit, "offset": options.Offset, "freshness": c.Freshness()}, nil
}

// searchRows performs the existing semantic/structured library search without
// pagination. Query-table applies its validated filters, multi-sort, and page
// window after this domain-specific relevance stage. The returned row maps may
// be shared with other requests and must not be modified. Rows carry at least
// fields (nil: every column); see libraryFields.
func (c *Catalog) searchRows(options SearchOptions, fields libraryFields) ([]map[string]any, error) {
	if options.unfiltered() {
		c.library.mu.Lock()
		c.library.usedAt = time.Now()
		c.library.mu.Unlock()
		return c.cachedLibraryRows(options.context(), fields)
	}
	return c.computeSearchRows(options, fields)
}

func (c *Catalog) computeSearchRows(options SearchOptions, fields libraryFields) ([]map[string]any, error) {
	ctx := options.context()
	where := []string{}
	args := []any{}
	var evidence map[string]*searchEvidence
	if strings.TrimSpace(options.Query) != "" && ftsQuery(options.Query) != "" {
		var err error
		if evidence, err = c.searchEvidence(ctx, options); err != nil {
			return nil, err
		}
		if options.evidence != nil {
			*options.evidence = evidence
		}
		if len(evidence) == 0 {
			return []map[string]any{}, nil
		}
		where = append(where, "w.id IN ("+placeholders(len(evidence))+")")
		for id := range evidence {
			args = append(args, id)
		}
	}
	if options.Repository != "" {
		where = append(where, "(r.display_name LIKE ? OR r.canonical_remote LIKE ?)")
		args = append(args, "%"+options.Repository+"%", "%"+options.Repository+"%")
	}
	if options.Source != "" {
		where = append(where, "w.source_kind=?")
		args = append(args, options.Source)
	}
	if options.Owner != "" {
		where = append(where, "(w.owner LIKE ? OR r.owner LIKE ?)")
		args = append(args, "%"+options.Owner+"%", "%"+options.Owner+"%")
	}
	// A filter on another table selects its workspaces once, rather than
	// probing that table for each of tens of thousands of workspaces.
	if options.Provider != "" {
		where = append(where, "w.id IN (SELECT cp.workspace_id FROM conversations cp WHERE cp.provider=?)")
		args = append(args, options.Provider)
	}
	if options.Model != "" {
		where = append(where, "w.id IN (SELECT cm.workspace_id FROM conversations cm WHERE cm.model LIKE ?)")
		args = append(args, "%"+options.Model+"%")
	}
	if options.From != "" {
		where = append(where, "w.activity_at>=?")
		args = append(args, timeBound(options.From, false))
	}
	if options.To != "" {
		where = append(where, "w.activity_at<=?")
		args = append(args, timeBound(options.To, true))
	}
	if options.Flavor != "" {
		where = append(where, "w.flavor=?")
		args = append(args, options.Flavor)
	}
	if options.Version != "" {
		where = append(where, "w.version LIKE ?")
		args = append(args, "%"+options.Version+"%")
	}
	if options.Outcome != "" {
		where = append(where, "w.outcome LIKE ?")
		args = append(args, "%"+options.Outcome+"%")
	}
	if options.Error != "" {
		where = append(where, "w.id IN (SELECT ml.workspace_id FROM metric_ledger ml WHERE ml.error_type LIKE ?)")
		args = append(args, "%"+options.Error+"%")
	}
	if options.Metric != "" {
		clause := []string{"mx.name=?"}
		args = append(args, options.Metric)
		if options.Minimum != nil {
			clause = append(clause, "mx.value>=?")
			args = append(args, *options.Minimum)
		}
		if options.Maximum != nil {
			clause = append(clause, "mx.value<=?")
			args = append(args, *options.Maximum)
		}
		where = append(where, "w.id IN (SELECT mx.workspace_id FROM metrics mx WHERE "+strings.Join(clause, " AND ")+")")
	}
	if options.File != "" {
		pattern := options.File
		if !strings.Contains(pattern, "%") {
			pattern = "%" + pattern + "%"
		}
		where = append(where, "w.id IN (SELECT cs.workspace_id FROM change_files cf JOIN change_sets cs ON cs.id=cf.change_set_id WHERE cf.path LIKE ?)")
		args = append(args, pattern)
	}
	if options.ChangedOnly {
		where = append(where, "w.id IN (SELECT x.workspace_id FROM change_files xf JOIN change_sets x ON x.id=xf.change_set_id)")
	}
	if options.PR != nil {
		where = append(where, "w.id IN (SELECT wpl.workspace_id FROM work_pr_links wpl JOIN pull_requests pr ON pr.id=wpl.pr_id WHERE pr.number=?)")
		args = append(args, *options.PR)
	}
	rows, err := c.libraryRows(ctx, strings.Join(where, " AND "), args, fields)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if found := evidence[firstString(row["id"])]; found != nil {
			row["score"] = found.score()
			row["match_types"] = found.matchTypes()
		} else {
			row["score"] = 0.0
			row["match_types"] = []string{"structured"}
		}
	}
	if len(evidence) > 0 {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i]["score"].(float64) > rows[j]["score"].(float64) })
	}
	rows = c.suppressMirrors(rows)
	// Costs take a ledger scan; a page without them gets them in libraryPage.
	if fields.hasAny(libraryCostFields) {
		if err := c.attachWorkspaceCosts(ctx, rows); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func (c *Catalog) WorkDetail(id string) (map[string]any, error) {
	return c.workDetail(id, true)
}

func (c *Catalog) workDetail(id string, includeMessages bool) (map[string]any, error) {
	rows, err := queryMaps(c.DB, `SELECT w.*,r.display_name repository_name,r.canonical_remote,r.local_locations_json repository_locations_json FROM workspaces w
		LEFT JOIN repositories r ON r.id=w.repository_id WHERE w.id=?`, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	result := rows[0]
	conversations, err := queryMaps(c.DB, "SELECT * FROM conversations WHERE workspace_id=? ORDER BY started_at", id)
	if err != nil {
		return nil, err
	}
	for _, conversation := range conversations {
		if !includeMessages {
			var count int
			if err := c.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE conversation_id=?", conversation["id"]).Scan(&count); err != nil {
				return nil, err
			}
			conversation["message_count"] = count
			conversation["message_access"] = map[string]any{"tool": "get_conversation_messages", "conversation_id": conversation["id"]}
			continue
		}
		messages, e := queryMaps(c.DB, "SELECT * FROM messages WHERE conversation_id=? ORDER BY source_order IS NULL,source_order,created_at,id", conversation["id"])
		if e != nil {
			return nil, e
		}
		if err := c.attachAuthorship(conversation["id"], messages); err != nil {
			return nil, err
		}
		conversation["messages"] = messages
		records := make([]MessageRecord, 0, len(messages))
		for _, m := range messages {
			records = append(records, MessageRecord{
				NativeID: firstString(m["native_id"]), Role: firstString(m["role"]), Kind: firstString(m["kind"]),
				Text: firstString(m["text"]), RawText: firstString(m["raw_text"]), CreatedAt: firstString(m["created_at"]),
				ParentNativeID: firstString(m["parent_native_id"]), CallID: firstString(m["call_id"]),
			})
		}
		conversation["token_usage"] = conversationTokenCounts(records)
	}
	result["conversations"] = conversations
	queries := []struct{ key, sql string }{
		{"agent_sessions", `SELECT a.*,(SELECT COUNT(*) FROM agent_session_messages am WHERE am.agent_session_id=a.id) message_count
			FROM agent_sessions a WHERE a.workspace_id=? ORDER BY a.depth,a.started_at,a.id`},
		{"changes", `SELECT cs.id,cs.classification,cs.base_id,cs.head_id,cs.complete,cf.path,cf.old_path,cf.status,cf.tracked,cf.bytes,cf.evidence_locator FROM change_sets cs LEFT JOIN change_files cf ON cf.change_set_id=cs.id WHERE cs.workspace_id=? ORDER BY cf.path`},
		{"metrics", "SELECT * FROM metrics WHERE workspace_id=? ORDER BY name"},
		{"metric_ledger", "SELECT * FROM metric_ledger WHERE workspace_id=? ORDER BY sequence LIMIT 10000"},
		{"attempts", `SELECT a.* FROM task_attempts a JOIN work_items i ON i.id=a.work_item_id WHERE i.workspace_id=? ORDER BY a.attempt_no,a.started_at`},
		{"handoffs", "SELECT * FROM handoffs WHERE workspace_id=? ORDER BY created_at"},
		{"prs", `SELECT pr.*,l.relationship,l.confidence,l.evidence_json FROM work_pr_links l JOIN pull_requests pr ON pr.id=l.pr_id WHERE l.workspace_id=?`},
		{"sightings", `SELECT s.*,COALESCE(h.label,s.host_id) host_label FROM workspace_sightings s LEFT JOIN hosts h ON h.id=s.host_id
			WHERE s.workspace_id=? ORDER BY s.last_seen_at DESC,s.host_id,s.source_name`},
	}
	for _, query := range queries {
		values, e := queryMaps(c.DB, query.sql, id)
		if e != nil {
			return nil, e
		}
		result[query.key] = values
	}
	summary, _ := queryMaps(c.DB, "SELECT * FROM summaries WHERE workspace_id=?", id)
	if len(summary) > 0 {
		result["summary"] = summary[0]
	} else {
		result["summary"] = nil
	}
	receipt, _ := queryMaps(c.DB, "SELECT * FROM receipts WHERE workspace_id=? ORDER BY created_at DESC LIMIT 1", id)
	if len(receipt) > 0 {
		result["receipt"] = receipt[0]
	} else {
		result["receipt"] = nil
	}
	links, _ := queryMaps(c.DB, `SELECT l.relationship,l.confidence,l.evidence_json,
		CASE WHEN a.workspace_id=? THEN b.workspace_id ELSE a.workspace_id END related_workspace_id,
		CASE WHEN a.workspace_id=? THEN b.provider ELSE a.provider END related_provider
		FROM conversation_identity_links l JOIN conversations a ON a.id=l.left_id JOIN conversations b ON b.id=l.right_id
		WHERE l.left_id IN (SELECT id FROM conversations WHERE workspace_id=?) OR l.right_id IN (SELECT id FROM conversations WHERE workspace_id=?)`, id, id, id, id)
	result["identity_links"] = links
	return result, nil
}

func (c *Catalog) suppressMirrors(rows []map[string]any) []map[string]any {
	byID := map[string]map[string]any{}
	parent := map[string]string{}
	for _, row := range rows {
		id := firstString(row["id"], row["workspace_id"])
		byID[id] = row
		parent[id] = id
	}
	var find func(string) string
	find = func(id string) string {
		if parent[id] != id {
			parent[id] = find(parent[id])
		}
		return parent[id]
	}
	links, _ := cachedValue(context.Background(), c, "identity-links", func(context.Context) ([]map[string]any, error) {
		return queryMaps(c.DB, `SELECT a.workspace_id left_workspace,b.workspace_id right_workspace FROM conversation_identity_links l
			JOIN conversations a ON a.id=l.left_id JOIN conversations b ON b.id=l.right_id`)
	})
	for _, link := range links {
		left, right := firstString(link["left_workspace"]), firstString(link["right_workspace"])
		if parent[left] != "" && parent[right] != "" {
			l, r := find(left), find(right)
			if l != r {
				parent[r] = l
			}
		}
	}
	// Visit groups in ID order and break priority ties on the latest activity,
	// then ID, so the representative never depends on map iteration order.
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	groups := map[string][]map[string]any{}
	roots := []string{}
	for _, id := range ids {
		root := find(id)
		if len(groups[root]) == 0 {
			roots = append(roots, root)
		}
		groups[root] = append(groups[root], byID[id])
	}
	priority := map[string]int{"tl1": 0, "tl1-export": 0, "codex": 1, "claude": 1, "chatgpt": 1, "conductor": 9}
	output := []map[string]any{}
	for _, root := range roots {
		group := groups[root]
		chosen := group[0]
		for _, candidate := range group[1:] {
			cp, ok := priority[firstString(candidate["source_kind"])]
			if !ok {
				cp = 5
			}
			bp, ok := priority[firstString(chosen["source_kind"])]
			if !ok {
				bp = 5
			}
			if cp < bp || cp == bp && firstString(candidate["activity_at"]) > firstString(chosen["activity_at"]) {
				chosen = candidate
			}
		}
		aliases := map[string]bool{}
		mirrors := []string{}
		for _, row := range group {
			aliases[firstString(row["source_kind"])] = true
			if row != nil && firstString(row["id"], row["workspace_id"]) != firstString(chosen["id"], chosen["workspace_id"]) {
				mirrors = append(mirrors, firstString(row["id"], row["workspace_id"]))
			}
		}
		names := []string{}
		for name := range aliases {
			if name != "" {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		chosen["source_aliases"] = names
		chosen["mirrored_workspace_ids"] = mirrors
		output = append(output, chosen)
	}
	sort.SliceStable(output, func(i, j int) bool {
		leftScore, _ := number(output[i]["score"])
		rightScore, _ := number(output[j]["score"])
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		leftActivity, rightActivity := firstString(output[i]["activity_at"]), firstString(output[j]["activity_at"])
		if leftActivity != rightActivity {
			return leftActivity > rightActivity
		}
		return firstString(output[i]["id"], output[i]["workspace_id"]) < firstString(output[j]["id"], output[j]["workspace_id"])
	})
	return output
}

func placeholders(count int) string {
	values := make([]string, count)
	for index := range values {
		values[index] = "?"
	}
	return strings.Join(values, ",")
}
func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
