package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
)

// The Library shows derived per-workspace columns (turn and tool counts, PR
// lists, changed files, ...). Computing them scans each workspace's messages,
// which is far too slow to repeat for every workspace on every request, so
// they are stored in workspace_library:
//
//   - Triggers on every table those columns read mark the affected workspace
//     in workspace_library_dirty. No write path has to remember to refresh.
//   - Reads combine stored rows for clean workspaces with a live computation
//     for dirty ones in a single statement, so results are never stale.
//   - RefreshLibrary recomputes dirty workspaces in small write batches.
//
// To add a derived column, add it to libraryColumns; if it reads a table not
// in libraryDependencies, add that table too. The stored table and triggers are
// rebuilt automatically when either list changes.

type libraryColumn struct{ name, expr string }

// libraryColumns are evaluated per workspace with the workspace aliased as w.
var libraryColumns = []libraryColumn{
	{"conversation_count", "(SELECT COUNT(*) FROM conversations cx WHERE cx.workspace_id=w.id)"},
	{"turn_count", "(SELECT COUNT(*) FROM conversations tx JOIN messages tm ON tm.conversation_id=tx.id WHERE tx.workspace_id=w.id AND tm.role='user' AND tm.kind='message')"},
	{"changed_file_count", "(SELECT COUNT(*) FROM change_sets csx JOIN change_files cfx ON cfx.change_set_id=csx.id WHERE csx.workspace_id=w.id)"},
	{"token_count", `COALESCE(
		(SELECT MAX(mx.value) FROM metrics mx WHERE mx.workspace_id=w.id AND mx.name='total_tokens'),
		(SELECT SUM(mx.value) FROM metrics mx WHERE mx.workspace_id=w.id AND mx.name IN ('input_tokens','output_tokens','cache_tokens')))`},
	{"tool_use_count", `MAX(
		(SELECT COUNT(*) FROM conversations tc JOIN messages tm ON tm.conversation_id=tc.id WHERE tc.workspace_id=w.id AND tm.kind IN ('tool_call','delegation')),
		COALESCE((SELECT SUM(mx.value) FROM metrics mx WHERE mx.workspace_id=w.id AND mx.name='tool_calls' AND mx.extractor_version<>'core-v1'),0))`},
	{"tool_error_count", `MAX(
		(SELECT COUNT(*) FROM conversations ec JOIN messages em ON em.conversation_id=ec.id WHERE ec.workspace_id=w.id AND em.kind IN ('tool_result','delegation_result') AND json_extract(CASE WHEN json_valid(em.text) THEN em.text ELSE '{}' END,'$.is_error')=1),
		COALESCE((SELECT SUM(mx.value) FROM metrics mx WHERE mx.workspace_id=w.id AND mx.name='tool_errors' AND mx.extractor_version<>'core-v1'),0))`},
	// Claude marks a compaction with a compact_boundary event. Codex replays
	// earlier compactions when a session resumes or forks, so only those the
	// adapter attributed to a turn (compaction_trigger) are counted.
	{"compaction_count", `(SELECT COUNT(*) FROM conversations kc JOIN messages km ON km.conversation_id=kc.id WHERE kc.workspace_id=w.id AND km.kind='metadata'
		AND (km.text LIKE '%"subtype":"compact_boundary"%' OR km.text LIKE '%"compaction_trigger":"%'))`},
	// A conversation's "main" session is its root, or for a child transcript
	// the sub-agent itself, which its parent's delegation session already
	// counts (in this workspace or the parent's). Depth is measured from the
	// workspace's shallowest session, so a sub-agent's own workspace starts at 0.
	{"subagent_count", "(SELECT COUNT(*) FROM agent_sessions ax WHERE ax.workspace_id=w.id AND ax.native_id<>'main')"},
	{"subagent_depth", `COALESCE(
		(SELECT MAX(ax.depth) FROM agent_sessions ax WHERE ax.workspace_id=w.id AND ax.native_id<>'main')
		-(SELECT MIN(ax.depth) FROM agent_sessions ax WHERE ax.workspace_id=w.id),0)`},
	{"pr_count", "(SELECT COUNT(DISTINCT lx.pr_id) FROM work_pr_links lx WHERE lx.workspace_id=w.id)"},
	{"pr_numbers", "(SELECT GROUP_CONCAT(prx.number) FROM work_pr_links lx JOIN pull_requests prx ON prx.id=lx.pr_id WHERE lx.workspace_id=w.id)"},
	{"pr_details", "(SELECT json_group_array(json_object('number',prx.number,'title',prx.title,'url',prx.url,'host',prx.host)) FROM work_pr_links lx JOIN pull_requests prx ON prx.id=lx.pr_id WHERE lx.workspace_id=w.id)"},
	{"providers", "(SELECT GROUP_CONCAT(DISTINCT cx.provider) FROM conversations cx WHERE cx.workspace_id=w.id)"},
	{"models", "(SELECT GROUP_CONCAT(DISTINCT cx.model) FROM conversations cx WHERE cx.workspace_id=w.id AND cx.model IS NOT NULL)"},
	{"changed_files", "(SELECT GROUP_CONCAT(DISTINCT cfx.path) FROM change_sets csx JOIN change_files cfx ON cfx.change_set_id=csx.id WHERE csx.workspace_id=w.id)"},
	{"error_types", "(SELECT GROUP_CONCAT(DISTINCT mlx.error_type) FROM metric_ledger mlx WHERE mlx.workspace_id=w.id AND mlx.error_type IS NOT NULL)"},
	{"summary_initiation", "(SELECT sx.initiation FROM summaries sx WHERE sx.workspace_id=w.id)"},
	{"summary_outcome", "(SELECT sx.outcome FROM summaries sx WHERE sx.workspace_id=w.id)"},
}

// libraryDependency maps a changed row (referenced as NEW or OLD) to the
// workspaces whose derived columns it can affect, selected as a column "id".
type libraryDependency struct {
	table      string
	workspaces func(row string) string
	insertOnly bool
}

func workspaceColumn(row string) string { return "SELECT " + row + ".workspace_id AS id" }

var libraryDependencies = []libraryDependency{
	// Workspace columns are read live; a new workspace only needs a stored row.
	{table: "workspaces", workspaces: func(row string) string { return "SELECT " + row + ".id AS id" }, insertOnly: true},
	{table: "conversations", workspaces: workspaceColumn},
	{table: "messages", workspaces: func(row string) string {
		return "SELECT workspace_id AS id FROM conversations WHERE id=" + row + ".conversation_id"
	}},
	{table: "change_sets", workspaces: workspaceColumn},
	{table: "change_files", workspaces: func(row string) string {
		return "SELECT workspace_id AS id FROM change_sets WHERE id=" + row + ".change_set_id"
	}},
	{table: "agent_sessions", workspaces: workspaceColumn},
	{table: "metrics", workspaces: workspaceColumn},
	{table: "metric_ledger", workspaces: workspaceColumn},
	{table: "work_pr_links", workspaces: workspaceColumn},
	{table: "pull_requests", workspaces: func(row string) string {
		return "SELECT workspace_id AS id FROM work_pr_links WHERE pr_id=" + row + ".id"
	}},
	{table: "summaries", workspaces: workspaceColumn},
}

func libraryTableDDL() string {
	columns := []string{"workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE"}
	for _, column := range libraryColumns {
		columns = append(columns, column.name)
	}
	return "CREATE TABLE workspace_library (\n  " + strings.Join(columns, ",\n  ") + "\n)"
}

func libraryTriggerDDL() []string {
	statements := []string{}
	for _, dependency := range libraryDependencies {
		// An outer INSERT OR REPLACE/ABORT would override a trigger's OR IGNORE,
		// so insert only unmarked IDs and never raise a conflict at all.
		mark := func(row string) string {
			return "INSERT INTO workspace_library_dirty(workspace_id) SELECT DISTINCT id FROM (" +
				dependency.workspaces(row) + ") WHERE id IS NOT NULL AND id NOT IN (SELECT workspace_id FROM workspace_library_dirty);"
		}
		name := "workspace_library_" + dependency.table
		statements = append(statements, fmt.Sprintf("CREATE TRIGGER %s_insert AFTER INSERT ON %s BEGIN %s END", name, dependency.table, mark("NEW")))
		if dependency.insertOnly {
			continue
		}
		statements = append(statements,
			fmt.Sprintf("CREATE TRIGGER %s_update AFTER UPDATE ON %s BEGIN %s %s END", name, dependency.table, mark("OLD"), mark("NEW")),
			fmt.Sprintf("CREATE TRIGGER %s_delete AFTER DELETE ON %s BEGIN %s END", name, dependency.table, mark("OLD")))
	}
	return statements
}

// libraryDerivedSelect computes stored-column values for workspaces matching
// filter, in workspace_library's column order.
func libraryDerivedSelect(filter string) string {
	columns := []string{"w.id workspace_id"}
	for _, column := range libraryColumns {
		columns = append(columns, column.expr+" "+column.name)
	}
	return "SELECT " + strings.Join(columns, ",\n\t") + "\n\tFROM workspaces w WHERE " + filter
}

// libraryVersion identifies the projection's table, columns, and triggers.
func libraryVersion() string {
	digest := sha256.Sum256([]byte(libraryTableDDL() + "\n" + libraryDerivedSelect("1") + "\n" + strings.Join(libraryTriggerDDL(), "\n")))
	return hex.EncodeToString(digest[:])
}

// ensureLibrary creates or rebuilds the stored projection and its triggers.
// A rebuild marks every workspace dirty; reads stay correct meanwhile.
func (c *Catalog) ensureLibrary() error {
	if _, err := c.DB.Exec(`CREATE TABLE IF NOT EXISTS workspace_library_dirty (
		workspace_id TEXT PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return err
	}
	triggers := libraryTriggerDDL()
	version := libraryVersion()
	var current string
	err := c.DB.QueryRow("SELECT value FROM meta WHERE key='workspace_library_version'").Scan(&current)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if current == version {
		legacy, err := queryMaps(c.DB, "SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'workspace_query_stats_%'")
		if err != nil {
			return err
		}
		for _, row := range legacy {
			if _, err := c.DB.Exec(`DROP TRIGGER IF EXISTS "` + firstString(row["name"]) + `"`); err != nil {
				return err
			}
		}
		if _, err := c.DB.Exec("DROP TABLE IF EXISTS workspace_query_stats"); err != nil {
			return err
		}
		return nil
	}
	tx, err := c.beginWrite(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	names, err := queryMaps(tx, "SELECT name FROM sqlite_master WHERE type='trigger' AND (name LIKE 'workspace_library_%' OR name LIKE 'workspace_query_stats_%')")
	if err != nil {
		return err
	}
	statements := []string{}
	for _, row := range names {
		statements = append(statements, `DROP TRIGGER IF EXISTS "`+firstString(row["name"])+`"`)
	}
	// workspace_query_stats held token_count before workspace_library did.
	statements = append(statements, "DROP TABLE IF EXISTS workspace_query_stats",
		"DELETE FROM meta WHERE key='workspace_query_stats_version'",
		"DROP TABLE IF EXISTS workspace_library", libraryTableDDL())
	statements = append(statements, triggers...)
	statements = append(statements, "INSERT OR IGNORE INTO workspace_library_dirty(workspace_id) SELECT id FROM workspaces")
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("rebuild library projection: %w", err)
		}
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES('workspace_library_version',?)", version); err != nil {
		return err
	}
	return tx.Commit()
}

// RefreshLibrary recomputes up to batch dirty workspaces in one short write
// transaction and reports how many it cleared.
func (c *Catalog) RefreshLibrary(ctx context.Context, batch int) (int, error) {
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Write first so the transaction takes the write lock (honoring
	// busy_timeout) instead of failing to upgrade a stale read snapshot.
	rows, err := queryMapsContext(ctx, tx, `DELETE FROM workspace_library_dirty WHERE workspace_id IN
		(SELECT workspace_id FROM workspace_library_dirty LIMIT ?) RETURNING workspace_id`, batch)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	args := make([]any, len(rows))
	for index, row := range rows {
		args[index] = row["workspace_id"]
	}
	// Deleted workspaces produce no row; their stored rows cascaded away.
	statement := "INSERT OR REPLACE INTO workspace_library " + libraryDerivedSelect("w.id IN ("+placeholders(len(args))+")")
	if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
		return 0, err
	}
	return len(rows), tx.Commit()
}

// refreshAllLibrary clears every dirty workspace, for commands that ingest
// without a running service to maintain the projection.
func (c *Catalog) refreshAllLibrary(ctx context.Context) error {
	for {
		count, err := c.RefreshLibrary(ctx, 10)
		if err != nil || count == 0 {
			return err
		}
	}
}

// maintainLibrary keeps the stored projection current, including after
// ingests run by other processes against the same catalog.
func (c *Catalog) maintainLibrary(ctx context.Context) {
	for {
		count, err := c.RefreshLibrary(ctx, 10)
		wait := time.Duration(0)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(os.Stderr, "Library refresh: %v\n", err)
			wait = 5 * time.Second
		} else if count == 0 {
			wait = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// A Library request reads only the fields it filters, sorts, groups, or
// aggregates on: a full row carries tens of megabytes of purpose and outcome
// text across the catalog. libraryPage then fills in whole rows for the page
// the UI shows, which renders fields it never selected (pr_details,
// canonical_remote, main_merge_*, and purpose/outcome as preview fallbacks).

// libraryFields is a set of Library field names; nil means every column.
type libraryFields map[string]bool

var (
	// libraryWorkspaceFields are Library fields read straight from workspaces.
	libraryWorkspaceFields = []string{"id", "title", "source_kind", "activity_at", "branch", "owner", "flavor",
		"version", "lifecycle", "preservation_completeness", "main_merge_title", "location"}
	// libraryGoFields are added in Go after the read: relevance, mirror
	// suppression, and costs.
	libraryGoFields = []string{"score", "match_types", "source_aliases", "mirrored_workspace_ids",
		"cost_usd", "cost_today_usd", "price_status"}
	libraryCostFields = []string{"cost_usd", "cost_today_usd", "price_status"}
	// libraryLargeFields hold long text, so only requests that use them read them.
	libraryLargeFields = []string{"purpose", "outcome", "changed_files", "pr_details", "summary_initiation", "summary_outcome"}
	// libraryCompactFields is every field except long text and costs, which
	// take a ledger scan: enough for any request that doesn't filter, sort, or
	// group on those.
	libraryCompactFields = func() libraryFields {
		fields := libraryFields{}
		for name := range libraryKnownFields() {
			fields[name] = true
		}
		for _, name := range append(append([]string{}, libraryLargeFields...), libraryCostFields...) {
			delete(fields, name)
		}
		return fields
	}()
)

// libraryExtraColumns are row columns computed from derived and joined values.
func libraryExtraColumns(derived map[string]string) []libraryColumn {
	return []libraryColumn{
		{"purpose", "COALESCE(w.purpose," + derived["summary_initiation"] + ")"},
		{"outcome", "COALESCE(w.outcome," + derived["summary_outcome"] + ")"},
		{"repository_name", "r.display_name"},
		{"canonical_remote", "r.canonical_remote"},
		{"file_edit_count", derived["changed_file_count"]},
		{"branch_count", "CASE WHEN w.branch IS NULL OR TRIM(w.branch)='' THEN 0 ELSE 1 END"},
	}
}

func libraryKnownFields() libraryFields {
	known := libraryFields{}
	for _, name := range append(append([]string{}, libraryWorkspaceFields...), libraryGoFields...) {
		known[name] = true
	}
	for _, column := range append(append([]libraryColumn{}, libraryColumns...), libraryExtraColumns(nil)...) {
		known[column.name] = true
	}
	return known
}

// libraryFieldsFor returns the fields a request needs, plus those the search
// pipeline itself reads. An unknown name selects every column, so a field
// missing from these lists costs speed, never correctness.
func libraryFieldsFor(names ...string) libraryFields {
	known := libraryKnownFields()
	fields := libraryFields{"id": true, "source_kind": true, "activity_at": true, "score": true}
	for _, name := range names {
		if !known[name] {
			return nil
		}
		fields[name] = true
	}
	return fields
}

func (f libraryFields) has(name string) bool { return f == nil || f[name] }

func (f libraryFields) hasAny(names []string) bool {
	return slices.ContainsFunc(names, f.has)
}

// covers reports whether rows read with f carry every field in other.
func (f libraryFields) covers(other libraryFields) bool {
	if f == nil {
		return true
	}
	if other == nil {
		return false
	}
	for name := range other {
		if !f[name] {
			return false
		}
	}
	return true
}

func (f libraryFields) union(other libraryFields) libraryFields {
	if f == nil || other == nil {
		return nil
	}
	fields := maps.Clone(f)
	maps.Copy(fields, other)
	return fields
}

// libraryRowSelect lists a Library row's columns in fields (nil: all),
// reading each derived column through value (a stored column or its live
// expression). w.id is always read.
func libraryRowSelect(value func(libraryColumn) string, fields libraryFields) string {
	columns := []string{"w.*"}
	if fields != nil {
		columns = []string{"w.id"}
		for _, name := range libraryWorkspaceFields {
			if name != "id" && fields[name] {
				columns = append(columns, "w."+name)
			}
		}
	}
	derived := map[string]string{}
	for _, column := range libraryColumns {
		derived[column.name] = value(column)
		if fields.has(column.name) {
			columns = append(columns, derived[column.name]+" "+column.name)
		}
	}
	for _, column := range libraryExtraColumns(derived) {
		if fields.has(column.name) {
			columns = append(columns, column.expr+" "+column.name)
		}
	}
	return "SELECT " + strings.Join(columns, ",\n\t") + "\n\tFROM workspaces w LEFT JOIN repositories r ON r.id=w.repository_id"
}

// libraryRows reads Library rows with fields (nil: every column) for
// workspaces matching the optional predicate (over w and r). Stored values
// serve clean workspaces; the same expressions are evaluated live for dirty or
// missing ones. Both reads share one transaction, so a concurrent refresh
// cannot drop or duplicate a row.
func (c *Catalog) libraryRows(ctx context.Context, predicate string, args []any, fields libraryFields) ([]map[string]any, error) {
	if predicate != "" {
		predicate = " AND (" + predicate + ")"
	}
	stored := libraryRowSelect(func(column libraryColumn) string { return "l." + column.name }, fields) +
		" JOIN workspace_library l ON l.workspace_id=w.id" +
		" WHERE w.id NOT IN (SELECT workspace_id FROM workspace_library_dirty)" + predicate
	live := libraryRowSelect(func(column libraryColumn) string { return "(" + column.expr + ")" }, fields) +
		" WHERE (w.id IN (SELECT workspace_id FROM workspace_library_dirty)" +
		" OR w.id NOT IN (SELECT workspace_id FROM workspace_library))" + predicate
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := queryMapsContext(ctx, tx, stored, args...)
	if err != nil {
		return nil, err
	}
	dirty, err := queryMapsContext(ctx, tx, live, args...)
	if err != nil {
		return nil, err
	}
	return append(rows, dirty...), nil
}

// libraryPage returns copies of a page of Library rows completed with every
// column, costs, and the first request and latest response text. The input
// rows may be cached, so they are left unchanged.
func (c *Catalog) libraryPage(ctx context.Context, rows []map[string]any) ([]map[string]any, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	args := make([]any, len(rows))
	for index, row := range rows {
		args[index] = firstString(row["id"])
	}
	full, err := c.libraryRows(ctx, "w.id IN ("+placeholders(len(args))+")", args, nil)
	if err != nil {
		return nil, err
	}
	if err := c.attachWorkspaceCosts(full); err != nil {
		return nil, err
	}
	byID := make(map[string]map[string]any, len(full))
	for _, row := range full {
		byID[firstString(row["id"])] = row
	}
	page := make([]map[string]any, len(rows))
	for index, row := range rows {
		page[index] = maps.Clone(row)
		// Relevance and mirror fields come from the search, not the read.
		maps.Copy(page[index], byID[firstString(row["id"])])
	}
	return page, c.attachLibraryPreviews(ctx, page, args)
}

// attachLibraryPreviews adds the first request and latest response text to
// page rows, whose workspace IDs are ids. They are large, so only displayed
// rows carry them.
func (c *Catalog) attachLibraryPreviews(ctx context.Context, page []map[string]any, ids []any) error {
	previews, err := queryMapsContext(ctx, c.DB, `SELECT w.id,
		(SELECT fm.text FROM conversations fc JOIN messages fm ON fm.conversation_id=fc.id WHERE fc.workspace_id=w.id AND fm.role='user' AND fm.kind='message' ORDER BY fm.created_at,fm.id LIMIT 1) first_input,
		(SELECT lm.text FROM conversations lc JOIN messages lm ON lm.conversation_id=lc.id WHERE lc.workspace_id=w.id AND lm.role='assistant' AND lm.kind='message' ORDER BY lm.created_at DESC,lm.id DESC LIMIT 1) last_response
		FROM workspaces w WHERE w.id IN (`+placeholders(len(ids))+`)`, ids...)
	if err != nil {
		return err
	}
	byID := make(map[string]map[string]any, len(previews))
	for _, preview := range previews {
		byID[firstString(preview["id"])] = preview
	}
	for _, row := range page {
		preview := byID[firstString(row["id"])]
		row["first_input"], row["last_response"] = preview["first_input"], preview["last_response"]
	}
	return nil
}
