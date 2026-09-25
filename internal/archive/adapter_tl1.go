package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// tl1Adapter indexes each TL1 task as a Library workspace and, alongside that,
// captures a per-installation snapshot of TL1's native workflow tables for the
// TL1 analysis layer (see tl1_store.go). Both come from one read transaction.
type tl1Adapter struct {
	baseAdapter
	snapshots []tl1Snapshot
}

type tl1Installation struct{ ID, Database, Repository, Transcripts, Project, ConfigPath string }

// tl1Snapshot is TL1's workflow state for one installation, normalized to the
// tl1_* catalog tables. Rows are keyed by those tables' column names.
type tl1Snapshot struct {
	// Installation names the original paths, also for a capture.
	Installation tl1Installation
	Account      string
	// Version is the database version the snapshot was read from (see
	// databaseVersion), so an older capture never replaces a newer snapshot.
	Version                                           int64
	Flavors, Tasks, Attempts, Candidates, Events      []map[string]any
	ReviewFindings, HumanTouches, ConfigurationHealth []map[string]any
}

// tl1Snapshots returns the snapshots captured by the most recent Discover.
func (a *tl1Adapter) tl1Snapshots() []tl1Snapshot { return a.snapshots }

func (a *tl1Adapter) installations() ([]tl1Installation, error) {
	if a.view != nil {
		return a.capturedInstallations(), nil
	}
	raw, err := readJSON(a.config.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot read TL1 registry %s: %w", a.config.Path, err)
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("TL1 registry %s must contain an installations array", a.config.Path)
	}
	rows, ok := object["installations"].([]any)
	if !ok {
		return nil, fmt.Errorf("TL1 registry %s must contain an installations array", a.config.Path)
	}
	result := []tl1Installation{}
	seen := map[string]bool{}
	for _, rawRow := range rows {
		row := mapValue(rawRow)
		if row == nil {
			continue
		}
		database, configPath, repository := expandPath(firstString(row["db_path"])), expandPath(firstString(row["config_path"])), expandPath(firstString(row["code_repo"]))
		if database == "" || configPath == "" || repository == "" {
			continue
		}
		dbInfo, dbErr := os.Stat(database)
		configInfo, configErr := os.Stat(configPath)
		repoInfo, repoErr := os.Stat(repository)
		if ephemeralTL1(a.config.Path, repository) || dbErr != nil || configErr != nil || repoErr != nil || !dbInfo.Mode().IsRegular() || !configInfo.Mode().IsRegular() || !repoInfo.IsDir() {
			continue
		}
		id := defaultString(row["installation_id"], database)
		key := id + "\x1f" + database
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, tl1Installation{ID: id, Database: database, Repository: repository, Transcripts: expandPath(firstString(row["transcripts_dir"])), Project: firstString(row["project_name"]), ConfigPath: configPath})
	}
	return result, nil
}

// capturedInstallations reads the installations a capture holds. Its manifest
// records each one's original paths, as the capturing Mac resolved them from
// the registry, with where their database snapshot and transcripts were
// captured, including installations since removed from the registry. The TL1
// config and code repository are not captured and need not exist here.
func (a *tl1Adapter) capturedInstallations() []tl1Installation {
	result := []tl1Installation{}
	seen := map[string]bool{}
	for _, installation := range a.view.manifest.Installations {
		database := filepath.Join(a.view.dir, filepath.FromSlash(installation.CapturedDB))
		key := installation.ID + "\x1f" + installation.DBPath
		if seen[key] || installation.CapturedDB == "" || ephemeralTL1(a.view.manifest.Source.Path, installation.CodeRepo) {
			continue
		}
		if info, err := os.Stat(database); err != nil || !info.Mode().IsRegular() {
			continue
		}
		seen[key] = true
		transcripts := ""
		if installation.CapturedTranscripts != "" {
			transcripts = filepath.Join(a.view.dir, filepath.FromSlash(installation.CapturedTranscripts))
		}
		result = append(result, tl1Installation{ID: installation.ID, Database: database, Repository: installation.CodeRepo, Transcripts: transcripts, Project: installation.Project, ConfigPath: installation.ConfigPath})
	}
	return result
}

// recorded is installation as the catalog records it: with the original
// paths of a capture's database and transcripts, which name them on the Mac
// that has them (the analysis prompts hand them to agents there).
func (a *tl1Adapter) recorded(installation tl1Installation) tl1Installation {
	installation.Database = a.original(installation.Database)
	if installation.Transcripts != "" {
		installation.Transcripts = a.original(installation.Transcripts)
	}
	return installation
}

// ephemeralTL1 reports an installation whose repository is in a temporary
// directory (a TL1 test run), unless the registry itself is temporary.
func ephemeralTL1(registry, repository string) bool {
	return !temporaryPath(registry) && temporaryPath(repository)
}

// macTemporary matches macOS per-user temporary directories on any Mac, for
// paths recorded elsewhere.
var macTemporary = regexp.MustCompile(`^(/private)?/var/folders/[^/]+/[^/]+/T/`)

// temporaryPath compares resolved paths: registries record /private/var/...
// while os.TempDir() is /var/folders/..., a symlink to it.
func temporaryPath(path string) bool {
	if path == "" {
		return false
	}
	path = resolvedPath(path)
	temporary := resolvedPath(os.TempDir())
	return strings.HasPrefix(path, temporary+string(os.PathSeparator)) || macTemporary.MatchString(path)
}

// resolvedPath resolves symlinks in the longest existing prefix of path.
func resolvedPath(path string) string {
	path = filepath.Clean(path)
	for dir, rest := path, ""; ; {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// tl1CapturedFile reports the files under an installation's transcripts
// directory that the adapter reads, and so a capture copies: agent
// transcripts (<attempt>-<executor>.jsonl) and procedural attempts' script
// logs (see tl1LogTail).
func tl1CapturedFile(name string) bool {
	name = strings.ToLower(name)
	if strings.HasSuffix(name, ".jsonl") {
		return true
	}
	for _, suffix := range tl1ScriptLogs {
		if strings.HasSuffix(name, "-"+suffix) {
			return true
		}
	}
	return false
}

func (a *tl1Adapter) Fingerprint() (string, error) {
	installations, err := a.installations()
	if err != nil {
		return "", err
	}
	rows := []string{}
	collect := func(path string) {
		if info, e := os.Stat(path); e == nil {
			rows = append(rows, fmt.Sprintf("%s:%d:%d", path, info.Size(), info.ModTime().UnixNano()))
		}
	}
	collect(a.config.Path)
	for _, installation := range installations {
		collect(installation.Database)
		collect(installation.Database + "-wal")
		if installation.Transcripts != "" {
			_ = filepath.WalkDir(installation.Transcripts, func(path string, entry os.DirEntry, e error) error {
				if e == nil && !entry.IsDir() && strings.HasSuffix(strings.ToLower(path), ".jsonl") {
					collect(path)
				}
				return nil
			})
		}
	}
	sort.Strings(rows)
	return hashBytes([]byte("tl1-analysis-v1:" + strings.Join(rows, "\n"))), nil
}

func (a *tl1Adapter) Discover(emit func(WorkspaceRecord) error) error {
	a.snapshots = nil
	installations, err := a.installations()
	if err != nil {
		return err
	}
	for _, installation := range installations {
		snapshot, err := a.database(installation, emit)
		if err != nil {
			return fmt.Errorf("TL1 installation %s: %w", installation.Database, err)
		}
		a.snapshots = append(a.snapshots, snapshot)
	}
	return nil
}

// tl1Select reads the wanted columns that exist in table. TL1 schemas grow by
// migration, so older installations simply yield NULL for newer columns.
func tl1Select(q queryer, table string, wanted []string, suffix string) ([]map[string]any, error) {
	info, err := queryMaps(q, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, row := range info {
		present[firstString(row["name"])] = true
	}
	columns := make([]string, 0, len(wanted))
	for _, column := range wanted {
		if present[column] {
			columns = append(columns, `"`+column+`"`)
		} else {
			columns = append(columns, `NULL AS "`+column+`"`)
		}
	}
	return queryMaps(q, "SELECT "+strings.Join(columns, ",")+` FROM "`+table+`"`+suffix)
}

func (a *tl1Adapter) database(installation tl1Installation, emit func(WorkspaceRecord) error) (tl1Snapshot, error) {
	snapshot := tl1Snapshot{Installation: a.recorded(installation), Account: a.config.Account}
	db, err := openReadOnlySQLite(installation.Database)
	if err != nil {
		return snapshot, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback()
	tables, err := tableNames(tx)
	if err != nil {
		return snapshot, err
	}
	snapshot.Version = a.databaseVersion(installation.Database)
	if !tables["tasks"] || !tables["task_attempts"] {
		return snapshot, fmt.Errorf("TL1 database %s is missing task tables", installation.Database)
	}
	attempts, err := rowsBy(tx, "task_attempts", "task_id", " ORDER BY task_id,attempt_number")
	if err != nil {
		return snapshot, err
	}
	summaries := map[string][]map[string]any{}
	if tables["transcript_summaries"] {
		summaries, err = rowsBy(tx, "transcript_summaries", "attempt_id", " ORDER BY attempt_id,filename")
		if err != nil {
			return snapshot, err
		}
	}
	resources := map[string][]map[string]any{}
	if tables["resource_usage"] {
		resources, err = rowsBy(tx, "resource_usage", "attempt_id", "")
		if err != nil {
			return snapshot, err
		}
	}
	tools := map[string][]map[string]any{}
	if tables["tool_usage"] {
		tools, err = rowsBy(tx, "tool_usage", "attempt_id", "")
		if err != nil {
			return snapshot, err
		}
	}
	handoffs := map[string][]map[string]any{}
	if tables["handoffs"] {
		handoffs, err = rowsBy(tx, "handoffs", "from_task_id", "")
		if err != nil {
			return snapshot, err
		}
	}
	candidates := map[string]map[string]any{}
	if tables["candidates"] {
		rows, e := queryMaps(tx, "SELECT * FROM candidates")
		if e != nil {
			return snapshot, e
		}
		for _, row := range rows {
			candidates[firstString(row["id"])] = row
			snapshot.Candidates = append(snapshot.Candidates, tl1CandidateRow(installation.ID, row))
		}
	}
	flavors := map[string]map[string]any{}
	if tables["task_flavors"] {
		rows, e := queryMaps(tx, "SELECT * FROM task_flavors")
		if e != nil {
			return snapshot, e
		}
		for _, row := range rows {
			flavors[firstString(row["id"])] = row
			snapshot.Flavors = append(snapshot.Flavors, tl1FlavorRow(installation.ID, row))
		}
	}
	if err := a.readActivity(tx, tables, installation.ID, &snapshot); err != nil {
		return snapshot, err
	}
	tasks, err := queryMaps(tx, "SELECT * FROM tasks ORDER BY created_at,id")
	if err != nil {
		return snapshot, err
	}
	for _, task := range tasks {
		taskID := firstString(task["id"])
		files := a.transcriptFiles(installation, taskID)
		record, err := a.task(installation, task, attempts[taskID], summaries, resources, tools, handoffs[taskID], candidates, flavors, files)
		if err != nil {
			return snapshot, err
		}
		// Its conversations keep their transcripts' versions.
		record.Observed = snapshot.Version
		if err := emit(record); err != nil {
			return snapshot, fmt.Errorf("ingest task %s: %w", taskID, err)
		}
		taskRow := tl1TaskRow(installation.ID, a.config.Account, task, flavors[firstString(task["flavor_id"])])
		snapshot.Tasks = append(snapshot.Tasks, taskRow)
		native := attempts[taskID]
		for index, attempt := range native {
			final := index == len(native)-1
			snapshot.Attempts = append(snapshot.Attempts, a.attemptRow(installation, taskRow, attempt, summaries[firstString(attempt["id"])], resources[firstString(attempt["id"])], files, final))
		}
	}
	return snapshot, tx.Commit()
}

// readActivity copies TL1's event log, review findings, human responses, and
// comments. Missing tables (older TL1 versions) are skipped.
func (a *tl1Adapter) readActivity(tx queryer, tables map[string]bool, installationID string, snapshot *tl1Snapshot) error {
	if tables["task_events"] {
		rows, err := tl1Select(tx, "task_events", []string{"id", "task_id", "event_type", "agent", "detail", "created_at"}, " ORDER BY id")
		if err != nil {
			return err
		}
		for _, row := range rows {
			snapshot.Events = append(snapshot.Events, map[string]any{"installation_id": installationID, "event_id": firstString(row["id"]), "task_id": row["task_id"], "event_type": row["event_type"], "agent": row["agent"], "detail_json": nilIfEmpty(firstString(row["detail"])), "created_at": nilIfEmpty(iso(row["created_at"]))})
		}
	}
	if tables["review_findings"] {
		rows, err := tl1Select(tx, "review_findings", []string{"id", "candidate_id", "review_task_id", "attempt_id", "severity", "file_path", "line", "explanation", "recommendation", "created_at"}, "")
		if err != nil {
			return err
		}
		for _, row := range rows {
			row["installation_id"] = installationID
			row["finding_id"] = row["id"]
			delete(row, "id")
			row["created_at"] = nilIfEmpty(iso(row["created_at"]))
			snapshot.ReviewFindings = append(snapshot.ReviewFindings, row)
		}
	}
	if tables["human_responses"] {
		rows, err := tl1Select(tx, "human_responses", []string{"id", "task_id", "candidate_id", "authority_action", "message", "created_at"}, "")
		if err != nil {
			return err
		}
		for _, row := range rows {
			snapshot.HumanTouches = append(snapshot.HumanTouches, map[string]any{"installation_id": installationID, "touch_id": "response:" + firstString(row["id"]), "kind": "response", "task_id": row["task_id"], "candidate_id": row["candidate_id"], "action": row["authority_action"], "body": row["message"], "created_at": nilIfEmpty(iso(row["created_at"]))})
		}
	}
	if tables["comments"] {
		rows, err := tl1Select(tx, "comments", []string{"id", "task_id", "candidate_id", "author_human", "author_agent", "body", "file_path", "created_at"}, "")
		if err != nil {
			return err
		}
		for _, row := range rows {
			action := "agent_comment"
			if firstString(row["author_human"]) != "" {
				action = "human_comment"
			}
			snapshot.HumanTouches = append(snapshot.HumanTouches, map[string]any{"installation_id": installationID, "touch_id": "comment:" + firstString(row["id"]), "kind": "comment", "task_id": row["task_id"], "candidate_id": row["candidate_id"], "action": action, "body": row["body"], "created_at": nilIfEmpty(iso(row["created_at"]))})
		}
	}
	if tables["agent_configuration_health"] {
		rows, err := tl1Select(tx, "agent_configuration_health", []string{"flavor_id", "configuration_name", "reason", "next_probe_at", "updated_at"}, "")
		if err != nil {
			return err
		}
		for _, row := range rows {
			row["installation_id"] = installationID
			snapshot.ConfigurationHealth = append(snapshot.ConfigurationHealth, row)
		}
	}
	return nil
}

func rowsBy(q queryer, table, key, order string) (map[string][]map[string]any, error) {
	rows, err := queryMaps(q, "SELECT * FROM "+table+order)
	if err != nil {
		return nil, err
	}
	result := map[string][]map[string]any{}
	for _, row := range rows {
		if id := firstString(row[key]); id != "" {
			result[id] = append(result[id], row)
		}
	}
	return result, nil
}

func tl1Purpose(task map[string]any) string {
	parts := []string{}
	for _, key := range []string{"instruction_prepend", "inputs", "instruction_append"} {
		value := task[key]
		text := asString(value)
		if text == "" || text == "{}" || text == "[]" {
			continue
		}
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil && decoded != nil {
			if pretty, e := json.MarshalIndent(decoded, "", "  "); e == nil {
				text = string(pretty)
			}
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// tl1TaskError returns the text explaining why a task ended in error. TL1
// records runtime failures as an outcome while the task itself is "done".
func tl1TaskError(task map[string]any) string {
	if firstString(task["outcome"]) != "runtime_error" && firstString(task["terminal_kind"]) != "failed" && firstString(task["status"]) != "failed" {
		return ""
	}
	outputs := map[string]any{}
	_ = json.Unmarshal([]byte(firstString(task["outputs"])), &outputs)
	return defaultString(firstNonNil(nilIfEmpty(firstString(task["terminal_reason"])), nilIfEmpty(firstString(task["handoff_message"])), outputs["error"]), "runtime_error")
}

// tl1AttemptConfiguration names the agent configuration an attempt ran with.
// Attempts from before TL1 recorded configuration names use executor-model-effort.
func tl1AttemptConfiguration(attempt map[string]any) string {
	if name := firstString(attempt["agent_configuration"]); name != "" {
		return name
	}
	parts := []string{}
	for _, key := range []string{"executor", "model", "effort"} {
		if value := firstString(attempt[key]); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "-")
}

var tl1TranscriptFile = regexp.MustCompile(`^(\d+)-([A-Za-z0-9_.]+)\.jsonl$`)

// transcriptFiles lists transcripts/<task>/ once per task. Files are named
// <attempt_number>-<executor>.jsonl; script logs share the attempt prefix.
func (a *tl1Adapter) transcriptFiles(installation tl1Installation, taskID string) []string {
	if installation.Transcripts == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(installation.Transcripts, taskID))
	if err != nil {
		return nil
	}
	names := []string{}
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

// attemptTranscripts returns the attempt's agent transcripts: files TL1
// summarized plus any <n>-*.jsonl on disk. TL1 only summarizes Claude runs, so
// relying on summaries alone would drop every Codex transcript.
func attemptTranscripts(attempt map[string]any, summaries []map[string]any, files []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, summary := range summaries {
		if name := firstString(summary["filename"]); name != "" && !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	number := fmt.Sprint(integer(attempt["attempt_number"]))
	for _, name := range files {
		if match := tl1TranscriptFile.FindStringSubmatch(name); match != nil && match[1] == number && !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	return result
}

func tl1ConversationNativeID(installationID, attemptID, filename string) string {
	return fmt.Sprintf("%s:%s:%s", installationID, attemptID, filename)
}

func (a *tl1Adapter) task(installation tl1Installation, task map[string]any, nativeAttempts []map[string]any, summaries map[string][]map[string]any, resources map[string][]map[string]any, tools map[string][]map[string]any, handoffs []map[string]any, candidates map[string]map[string]any, flavors map[string]map[string]any, files []string) (WorkspaceRecord, error) {
	taskID := firstString(task["id"])
	candidate := candidates[firstString(task["candidate_id"])]
	attempts := []map[string]any{}
	conversations := []ConversationRecord{}
	type metric struct {
		value        float64
		unit, status string
	}
	metrics := map[string]metric{}
	taskError := tl1TaskError(task)
	for index, attempt := range nativeAttempts {
		attemptID := firstString(attempt["id"])
		configuration := map[string]any{}
		for _, key := range []string{"executor", "model", "effort", "agent", "harness_id", "agent_configuration"} {
			if attempt[key] != nil {
				configuration[key] = attempt[key]
			}
		}
		if basis := firstString(attempt["selection_basis"]); basis != "" {
			var decoded any
			if json.Unmarshal([]byte(basis), &decoded) == nil {
				configuration["selection_basis"] = decoded
			}
		}
		failure := attempt["failure_reason"]
		if failure == nil && taskError != "" && index == len(nativeAttempts)-1 {
			failure = taskError
		}
		attempts = append(attempts, map[string]any{"source_id": attemptID, "source_ref": attempt["worktree_id"], "attempt_no": attempt["attempt_number"], "instructions": tl1Purpose(task), "result": attempt["result_summary"], "failure": failure, "validation": attempt["status"], "started_at": iso(attempt["started_at"]), "ended_at": iso(attempt["completed_at"]), "configuration": configuration})
		for _, pair := range [][2]string{{"files_changed", "files_changed"}, {"lines_added", "lines_added"}, {"lines_removed", "lines_removed"}} {
			if value, ok := number(attempt[pair[0]]); ok {
				old := metrics[pair[1]]
				metrics[pair[1]] = metric{old.value + value, "count", "reported-incremental"}
			}
		}
		for _, filename := range attemptTranscripts(attempt, summaries[attemptID], files) {
			conversation, ok, err := a.transcript(installation, taskID, attempt, filename)
			if err != nil {
				return WorkspaceRecord{}, err
			}
			if ok {
				conversations = append(conversations, conversation)
			}
		}
		for _, summary := range summaries[attemptID] {
			for _, item := range []struct{ key, name, unit string }{{"num_events", "transcript_events", "count"}, {"num_turns", "transcript_turns", "count"}, {"duration_ms", "execution_duration", "milliseconds"}, {"cost_usd", "reported_cost", "USD"}, {"tool_errors", "tool_errors", "count"}} {
				if value, ok := number(summary[item.key]); ok {
					old := metrics[item.name]
					metrics[item.name] = metric{old.value + value, item.unit, "reported-incremental"}
				}
			}
		}
		for _, resource := range resources[attemptID] {
			for _, item := range []struct{ key, name, unit string }{{"peak_rss_mb", "peak_rss", "MB"}, {"peak_cpu_pct", "peak_cpu", "percent"}} {
				if value, ok := number(resource[item.key]); ok {
					old := metrics[item.name]
					if value < old.value {
						value = old.value
					}
					metrics[item.name] = metric{value, item.unit, "reported-peak"}
				}
			}
		}
		for _, tool := range tools[attemptID] {
			if value, ok := number(tool["count"]); ok {
				name := "tool:" + defaultString(tool["tool_name"], "unknown")
				old := metrics[name]
				metrics[name] = metric{old.value + value, "count", "reported-incremental"}
			}
		}
	}
	updated := iso(firstNonNil(task["updated_at"], task["completed_at"], task["created_at"]))
	status := defaultString(task["status"], "unknown")
	terminal := task["completed_at"] != nil || task["terminal_kind"] != nil || matchesFold(status, "completed", "failed", "cancelled", "canceled", "closed", "merged")
	prs := []map[string]any{}
	if candidate != nil && candidate["pr_number"] != nil {
		prs = append(prs, map[string]any{"number": candidate["pr_number"], "url": candidate["pr_url"], "state": candidate["pr_state"], "merged": firstString(candidate["pr_state"]) == "merged", "updated_at": updated})
	}
	metricRows := []map[string]any{}
	for name, value := range metrics {
		metricRows = append(metricRows, map[string]any{"name": name, "value": value.value, "unit": value.unit, "status": value.status, "extractor_version": "tl1-registry-v1", "coverage": 1.0})
	}
	sort.Slice(metricRows, func(i, j int) bool {
		return firstString(metricRows[i]["name"]) < firstString(metricRows[j]["name"])
	})
	handoffRows := []map[string]any{}
	for _, row := range handoffs {
		handoffRows = append(handoffRows, map[string]any{"id": row["op_id"], "candidate_id": row["candidate_id"], "predecessor_id": row["from_task_id"], "custody_version": row["custody_version_at_req"], "checkpoint": map[string]any{"status": row["checkpoint_status"], "sha": row["checkpoint_sha"]}, "closure_evidence": map[string]any{"result": row["result"], "finalized": row["finalized"]}, "created_at": iso(row["created_at"])})
	}
	sort.Slice(handoffRows, func(i, j int) bool { return firstString(handoffRows[i]["id"]) < firstString(handoffRows[j]["id"]) })
	project := installation.Project
	if project == "" {
		project = filepath.Base(installation.Repository)
	}
	flavorID := firstString(task["flavor_id"])
	metadata := map[string]any{"status": status, "terminal": terminal, "lifecycle": map[bool]string{true: "terminal", false: "active"}[terminal], "branch": candidate["canonical_branch"], "base_ref": candidate["base_sha"], "head_ref": candidate["head_sha"], "custody_version": candidate["custody_version"], "flavor": defaultString(flavors[flavorID]["name"], flavorID), "version": task["shape_version"]}
	return WorkspaceRecord{SourceID: installation.ID + ":" + taskID, SourceKind: "tl1", Title: defaultString(task["title"], taskID), Account: a.config.Account, Purpose: tl1Purpose(task), Outcome: firstString(task["outcome"]), ActivityAt: updated, Location: installation.Repository, Repository: map[string]any{"display_name": project, "local_locations": []string{installation.Repository}}, Metadata: metadata, Conversations: conversations, WorkItems: []map[string]any{{"source_id": taskID, "title": task["title"], "status": status, "explicit": true}}, Attempts: attempts, Handoffs: handoffRows, Metrics: metricRows, PRs: prs}, nil
}

func (a *tl1Adapter) transcript(installation tl1Installation, taskID string, attempt map[string]any, filename string) (ConversationRecord, bool, error) {
	if installation.Transcripts == "" || filename == "" {
		return ConversationRecord{}, false, nil
	}
	path := filepath.Join(installation.Transcripts, taskID, filename)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ConversationRecord{}, false, nil
	}
	provider := "claude"
	if strings.Contains(strings.ToLower(filename), "codex") || firstString(attempt["executor"]) == "codex" {
		provider = "codex"
	}
	model := firstString(attempt["model"])
	var source ConversationRecord
	if provider == "codex" && codexExecTranscript(path) {
		conversation, ok, err := parseCodexExec(path, a.original(path))
		if err != nil || !ok {
			return ConversationRecord{}, false, err
		}
		source = conversation
	} else {
		parser := jsonlAdapter{baseAdapter: baseAdapter{config: SourceConfig{Name: "tl1-transcript", Kind: provider, Path: filepath.Dir(path), Account: a.config.Account}, capability: "retrieval-only", view: a.view}, provider: provider}
		record, ok, err := parser.parse(path)
		if err != nil || !ok || len(record.Conversations) == 0 {
			return ConversationRecord{}, false, err
		}
		source = record.Conversations[0]
	}
	aliases := uniqueStrings(append([]string{source.NativeID}, source.Aliases...))
	source.NativeID = tl1ConversationNativeID(installation.ID, firstString(attempt["id"]), filename)
	// TL1 orchestrates the run; the model provider is the agent that wrote the
	// transcript. The workspace's source_kind records the TL1 origin.
	source.Provider = provider
	source.Account = a.config.Account
	source.Aliases = aliases
	if source.Model == "" {
		source.Model = model
	}
	// Codex exec transcripts carry no timestamps; the attempt bounds place
	// their usage on the right day.
	if source.StartedAt == "" {
		source.StartedAt = iso(attempt["started_at"])
	}
	if source.EndedAt == "" {
		source.EndedAt = iso(firstNonNil(attempt["completed_at"], attempt["started_at"]))
	}
	for index := range source.Messages {
		if source.Messages[index].Model == "" {
			source.Messages[index].Model = source.Model
		}
	}
	return source, true, nil
}

// codexExecTranscript reports whether path holds `codex exec --json` events
// (thread.started/item.completed/turn.completed) rather than a rollout file.
func codexExecTranscript(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	line, _ := bufio.NewReaderSize(file, 64*1024).ReadBytes('\n')
	var event map[string]any
	if json.Unmarshal(bytes.TrimSpace(line), &event) != nil {
		return false
	}
	kind := firstString(event["type"])
	return strings.HasPrefix(kind, "thread.") || strings.HasPrefix(kind, "turn.") || strings.HasPrefix(kind, "item.")
}

// parseCodexExec reads the `codex exec --json` event stream TL1 records for
// Codex attempts. Only completed items are kept; turn.completed usage feeds
// token accounting through its raw event. origin is the path the file stands
// for (see baseAdapter.original), which its origin and evidence locators name.
func parseCodexExec(path, origin string) (ConversationRecord, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return ConversationRecord{}, false, nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ConversationRecord{}, false, nil
	}
	// Exactly the bytes present when it was opened, as jsonlAdapter.parseFile.
	reader := bufio.NewReaderSize(io.LimitReader(file, info.Size()), 64*1024)
	messages := []MessageRecord{}
	threadID := ""
	line := 0
	turns := 0
	add := func(id, role, kind, text, callID string, raw string) {
		messages = append(messages, MessageRecord{NativeID: id, Role: role, Kind: kind, Text: text, CallID: callID, RawText: raw, EvidenceLocator: fmt.Sprintf("%s:%d", origin, line), Selected: true})
	}
	for {
		encoded, readErr := reader.ReadBytes('\n')
		if len(encoded) == 0 && readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != io.EOF {
			return ConversationRecord{}, false, fmt.Errorf("read %s line %d: %w", path, line+1, readErr)
		}
		line++
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		var event map[string]any
		if decoder.Decode(&event) != nil {
			if readErr == io.EOF {
				break
			}
			continue
		}
		switch firstString(event["type"]) {
		case "thread.started":
			threadID = defaultString(event["thread_id"], threadID)
		case "turn.started":
			turns++
		case "turn.completed":
			add(fmt.Sprintf("turn-%d:usage", turns), "system", "metadata", jsonText(map[string]any{"type": "token_usage", "usage": event["usage"]}), "", jsonText(event))
		case "turn.failed", "error":
			message := firstString(mapValue(event["error"])["message"], event["message"])
			add(fmt.Sprintf("line-%d", line), "system", "error", defaultString(message, jsonText(event)), "", "")
		case "item.completed":
			item := mapValue(event["item"])
			id := defaultString(item["id"], fmt.Sprintf("line-%d", line))
			switch firstString(item["type"]) {
			case "agent_message":
				add(id, "assistant", "message", firstString(item["text"]), "", "")
			case "command_execution":
				add(id+":call", "assistant", "tool_call", jsonText(map[string]any{"tool": "shell", "input": map[string]any{"command": item["command"]}}), id, "")
				exitCode := item["exit_code"]
				add(id+":result", "tool", "tool_result", jsonText(map[string]any{"tool": "shell", "content": item["aggregated_output"], "exit_code": exitCode, "is_error": exitCode != nil && integer(exitCode) != 0}), id, "")
			case "file_change":
				add(id+":call", "assistant", "tool_call", jsonText(map[string]any{"tool": "apply_patch", "input": map[string]any{"changes": item["changes"]}}), id, "")
				add(id+":result", "tool", "tool_result", jsonText(map[string]any{"tool": "apply_patch", "content": item["status"], "is_error": firstString(item["status"]) == "failed"}), id, "")
			case "mcp_tool_call":
				name := strings.Trim(firstString(item["server"])+"."+firstString(item["tool"]), ".")
				add(id+":call", "assistant", "tool_call", jsonText(map[string]any{"tool": defaultString(name, "mcp"), "input": item["arguments"]}), id, "")
				add(id+":result", "tool", "tool_result", jsonText(map[string]any{"tool": defaultString(name, "mcp"), "content": firstNonNil(item["result"], item["error"]), "is_error": item["error"] != nil || firstString(item["status"]) == "failed"}), id, "")
			case "web_search":
				add(id+":call", "assistant", "tool_call", jsonText(map[string]any{"tool": "web_search", "input": map[string]any{"query": item["query"]}}), id, "")
			case "todo_list":
				add(id+":call", "assistant", "tool_call", jsonText(map[string]any{"tool": "update_plan", "input": map[string]any{"items": item["items"]}}), id, "")
			case "error":
				add(id, "system", "error", firstString(item["message"]), "", "")
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	if len(messages) == 0 {
		return ConversationRecord{}, false, nil
	}
	nativeID := defaultString(threadID, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	return ConversationRecord{NativeID: nativeID, Provider: "codex", Origin: origin, Coverage: "complete", Messages: messages, Observed: info.ModTime().UnixNano()}, true, nil
}

func tl1FlavorRow(installationID string, row map[string]any) map[string]any {
	template := firstString(row["template"])
	templateHash := ""
	if template != "" {
		templateHash = hashBytes([]byte(template))
	}
	return map[string]any{
		"installation_id": installationID, "flavor_id": firstString(row["id"]), "name": firstString(row["name"]),
		"execution_class": tl1ExecutionClass(row),
		"executor":        row["executor"], "model": row["model"], "effort": row["effort"], "candidate_behavior": row["candidate_behavior"],
		"read_only": boolInt(row["read_only"]), "default_agent_configuration": row["default_agent_configuration"],
		"agent_configurations_json": tl1JSONColumn(row["agent_configurations"]), "agent_preferences_json": tl1JSONColumn(row["agent_configuration_preferences"]),
		"transitions_json": tl1JSONColumn(row["transitions"]), "outcomes_json": tl1JSONColumn(row["outcomes"]), "budgets_json": tl1JSONColumn(row["budgets"]),
		"retry_config_json": tl1JSONColumn(row["retry_config"]), "inputs_schema_json": tl1JSONColumn(row["inputs_schema"]), "outputs_schema_json": tl1JSONColumn(row["outputs_schema"]),
		"escalation_shape": row["escalation_shape"], "max_parallelism": row["max_parallelism"], "template": nilIfEmpty(template), "template_hash": nilIfEmpty(templateHash),
		"shape_version": row["shape_version"], "updated_at": nilIfEmpty(iso(row["updated_at"])),
	}
}

// tl1ExecutionClass is llm, procedural, or human. Older TL1 flavors only
// carry is_llm.
func tl1ExecutionClass(flavor map[string]any) string {
	if class := firstString(flavor["execution_class"]); class != "" {
		return class
	}
	if boolInt(valueOr(flavor["is_llm"], true)) == 1 {
		return "llm"
	}
	return "procedural"
}

func tl1JSONColumn(value any) any {
	text := firstString(value)
	if text == "" {
		return nil
	}
	return text
}

func tl1CandidateRow(installationID string, row map[string]any) map[string]any {
	return map[string]any{
		"installation_id": installationID, "candidate_id": firstString(row["id"]), "title": row["title"], "status": row["status"],
		"branch": row["canonical_branch"], "base_sha": row["base_sha"], "head_sha": row["head_sha"], "pr_number": row["pr_number"], "pr_url": row["pr_url"],
		"pr_state": row["pr_state"], "integrated_sha": row["integrated_sha"], "root_task_id": row["root_task_id"], "workflow_steps": row["workflow_steps"],
		"workflow_step_limit": row["workflow_step_limit"], "outcome_visits_json": tl1JSONColumn(row["outcome_visits"]), "tags_json": tl1JSONColumn(row["tags"]),
		"created_at": nilIfEmpty(iso(row["created_at"])), "closed_at": nilIfEmpty(iso(row["closed_at"])),
	}
}

func tl1TaskRow(installationID, account string, task, flavor map[string]any) map[string]any {
	taskID := firstString(task["id"])
	errorText := tl1TaskError(task)
	class, signature, attribution := classifyTL1Error(errorText)
	executionClass := ""
	if flavor != nil {
		executionClass = tl1ExecutionClass(flavor)
	}
	return map[string]any{
		"installation_id": installationID, "task_id": taskID, "workspace_id": stableID("workspace", "tl1", account, installationID+":"+taskID),
		"flavor": defaultString(flavor["name"], firstString(task["flavor_id"])), "flavor_id": task["flavor_id"], "shape_version": task["shape_version"],
		"execution_class": nilIfEmpty(executionClass), "title": task["title"], "status": task["status"], "outcome": task["outcome"],
		"terminal_kind": task["terminal_kind"], "terminal_reason": task["terminal_reason"], "error_text": nilIfEmpty(errorText),
		"error_class": nilIfEmpty(class), "error_signature": nilIfEmpty(signature), "error_attribution": nilIfEmpty(attribution),
		"candidate_id": task["candidate_id"], "parent_task_id": task["parent_task_id"], "root_task_id": task["root_task_id"],
		"created_by_task": task["created_by_task"], "created_by_agent": task["created_by_agent"], "assigned_configuration": task["agent_configuration"],
		"suspension_count": task["suspension_count"], "outputs_json": tl1JSONColumn(task["outputs"]), "handoff_message": task["handoff_message"],
		"tags_json": tl1JSONColumn(task["tags"]), "created_at": nilIfEmpty(iso(task["created_at"])), "claimed_at": nilIfEmpty(iso(task["claimed_at"])),
		"completed_at": nilIfEmpty(iso(task["completed_at"])), "updated_at": nilIfEmpty(iso(task["updated_at"])),
	}
}

func (a *tl1Adapter) attemptRow(installation tl1Installation, task, attempt map[string]any, summaries, resources []map[string]any, files []string, final bool) map[string]any {
	attemptID := firstString(attempt["id"])
	errorText := firstString(attempt["failure_reason"])
	if errorText == "" && final {
		errorText = firstString(task["error_text"])
	}
	class, signature, attribution := classifyTL1Error(errorText)
	started, startedOK := parseTime(iso(attempt["started_at"]))
	ended, endedOK := parseTime(iso(attempt["completed_at"]))
	var duration any
	if startedOK && endedOK && !ended.Before(started) {
		duration = ended.Sub(started).Milliseconds()
	}
	var reportedCost any
	var toolErrors any
	for _, summary := range summaries {
		if value, ok := number(summary["cost_usd"]); ok {
			reportedCost = addCost(reportedCost, &value)
		}
		if value, ok := number(summary["tool_errors"]); ok && value > 0 {
			toolErrors = integer(toolErrors) + int64(value)
		}
	}
	resource := map[string]any{}
	if len(resources) > 0 {
		resource = resources[0]
	}
	transcripts := attemptTranscripts(attempt, summaries, files)
	var conversation any
	for _, name := range transcripts {
		if _, err := os.Stat(filepath.Join(installation.Transcripts, firstString(task["task_id"]), name)); err == nil {
			conversation = tl1ConversationNativeID(installation.ID, attemptID, name)
			break
		}
	}
	var logTail any
	if errorText != "" {
		logTail = nilIfEmpty(tl1LogTail(installation, firstString(task["task_id"]), attempt, files))
	}
	return map[string]any{
		"installation_id": installation.ID, "attempt_id": attemptID, "task_id": task["task_id"], "attempt_number": attempt["attempt_number"],
		"configuration": nilIfEmpty(tl1AttemptConfiguration(attempt)), "executor": attempt["executor"], "model": attempt["model"], "effort": attempt["effort"],
		"harness_id": attempt["harness_id"], "agent": attempt["agent"], "selection_basis_json": tl1JSONColumn(attempt["selection_basis"]),
		"status": attempt["status"], "failure_reason": attempt["failure_reason"], "error_class": nilIfEmpty(class), "error_signature": nilIfEmpty(signature),
		"error_attribution": nilIfEmpty(attribution), "started_at": nilIfEmpty(iso(attempt["started_at"])), "completed_at": nilIfEmpty(iso(attempt["completed_at"])),
		"duration_ms": duration, "files_changed": attempt["files_changed"], "lines_added": attempt["lines_added"], "lines_removed": attempt["lines_removed"],
		"peak_rss_mb": resource["peak_rss_mb"], "avg_rss_mb": resource["avg_rss_mb"], "peak_cpu_pct": resource["peak_cpu_pct"], "avg_cpu_pct": resource["avg_cpu_pct"],
		"reported_cost_usd": reportedCost, "reported_tool_errors": toolErrors, "transcripts_json": jsonText(transcripts), "conversation_native_id": conversation,
		"log_tail": logTail,
	}
}

// tl1ScriptLogs are a procedural attempt's script logs, <attempt>-<name>, in
// the order tl1LogTail prefers them.
var tl1ScriptLogs = []string{"script.stderr.log", "script.log", "script.stdout.log"}

// tl1LogTail keeps the end of a failed procedural attempt's script output:
// stderr when it has content, otherwise the combined log.
func tl1LogTail(installation tl1Installation, taskID string, attempt map[string]any, files []string) string {
	if installation.Transcripts == "" {
		return ""
	}
	prefix := strconv.FormatInt(integer(attempt["attempt_number"]), 10) + "-"
	for _, suffix := range tl1ScriptLogs {
		for _, name := range files {
			if name != prefix+suffix {
				continue
			}
			if text := strings.TrimSpace(readTail(filepath.Join(installation.Transcripts, taskID, name), 2048)); text != "" {
				return text
			}
		}
	}
	return ""
}

func readTail(path string, limit int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	offset := max(info.Size()-limit, 0)
	buffer := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(buffer, offset); err != nil && err != io.EOF {
		return ""
	}
	return strings.ToValidUTF8(string(buffer), "")
}
