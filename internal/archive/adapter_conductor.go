package archive

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type conductorAdapter struct {
	baseAdapter
	// selected, when set, limits Discover to these session IDs.
	selected map[string]bool
}

const conductorExtractor = "conductor-v9-workflow-delegation"

// conductorPartExtractor versions the parser for per-session part states
// (source_item_states). It changes separately from conductorExtractor, which
// every Conductor conversation also stores as its coverage, so a parser change
// that must reach sessions already indexed need not rewrite them all.
// +sender: user messages record who sent them.
const conductorPartExtractor = conductorExtractor + "+sender"

func (a *conductorAdapter) ExtractorVersion() string { return conductorExtractor }
func (a *conductorAdapter) partExtractor() string    { return conductorPartExtractor }

func (a *conductorAdapter) Fingerprint() (string, error) {
	fingerprint, err := pathFingerprint(a.config.Path, func(path string, entry os.DirEntry) bool {
		name := strings.ToLower(entry.Name())
		return strings.HasSuffix(name, ".db") || strings.Contains(name, ".sqlite") || strings.HasSuffix(name, "-wal")
	})
	if err != nil {
		return "", err
	}
	return hashBytes([]byte(conductorExtractor + ":" + fingerprint)), nil
}

func (a *conductorAdapter) Discover(emit func(WorkspaceRecord) error) error {
	return a.discover(a.selected, nil, func(record *WorkspaceRecord, _ []sourcePart) error { return emit(*record) })
}

func (a *conductorAdapter) DiscoverSelected(selected map[string]bool, emit func(WorkspaceRecord) error) error {
	if len(selected) == 0 {
		return nil
	}
	return a.discover(selected, nil, func(record *WorkspaceRecord, _ []sourcePart) error { return emit(*record) })
}

// discoverParts treats each session as a part. Whether one changed is decided
// from its rows and index-only message statistics (see conductorSignals), so
// an unchanged session's messages are never read.
func (a *conductorAdapter) discoverParts(unchanged func([]sourcePart) bool, emit func(*WorkspaceRecord, []sourcePart) error) error {
	return a.discover(a.selected, unchanged, emit)
}

func (a *conductorAdapter) discover(selected map[string]bool, unchanged func([]sourcePart) bool, emit func(*WorkspaceRecord, []sourcePart) error) error {
	info, err := os.Stat(a.config.Path)
	if err != nil {
		return err
	}
	candidates := []string{}
	if !info.IsDir() {
		candidates = append(candidates, a.config.Path)
	} else {
		_ = filepath.WalkDir(a.config.Path, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return nil
			}
			lower := strings.ToLower(entry.Name())
			if (strings.HasSuffix(lower, ".db") || strings.Contains(lower, ".sqlite")) && !strings.HasSuffix(lower, "-wal") && !strings.HasSuffix(lower, "-shm") {
				candidates = append(candidates, path)
			}
			return nil
		})
	}
	sort.Strings(candidates)
	for _, path := range candidates {
		for attempt := 0; ; attempt++ {
			err := a.database(path, selected, unchanged, emit)
			if err == nil {
				break
			}
			if attempt >= 5 || (!strings.Contains(err.Error(), "database is locked") && !strings.Contains(err.Error(), "database is busy")) {
				return fmt.Errorf("could not read Conductor database consistently: %s: %w", path, err)
			}
			time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
		}
	}
	return nil
}

func openReadOnlySQLite(path string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (a *conductorAdapter) database(path string, selected map[string]bool, unchanged func([]sourcePart) bool, emit func(*WorkspaceRecord, []sourcePart) error) error {
	db, err := openReadOnlySQLite(path)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tables, err := tableNames(tx)
	if err != nil {
		return err
	}
	// After the first read, which fixes what the transaction sees.
	version := a.databaseVersion(path)
	sessionTable := firstPresent(tables, "sessions", "workspaces", "threads")
	messageTable := firstPresent(tables, "messages", "session_messages")
	if sessionTable == "" || messageTable == "" {
		return nil
	}
	scols, err := tableColumns(tx, sessionTable)
	if err != nil {
		return err
	}
	mcols, err := tableColumns(tx, messageTable)
	if err != nil {
		return err
	}
	sidColumn := firstPresent(scols, "id", "session_id", "uuid")
	fkColumn := firstPresent(mcols, "session_id", "workspace_id", "thread_id")
	if sidColumn == "" || fkColumn == "" {
		return nil
	}
	statement := "SELECT * FROM " + sessionTable
	arguments := []any{}
	if selected != nil {
		placeholders := make([]string, 0, len(selected))
		for id := range selected {
			placeholders = append(placeholders, "?")
			arguments = append(arguments, id)
		}
		statement += " WHERE " + sidColumn + " IN (" + strings.Join(placeholders, ",") + ")"
	}
	sessions, err := queryMaps(tx, statement, arguments...)
	if err != nil {
		return err
	}
	origin := a.original(path)
	var signals map[string]sourcePart
	if unchanged != nil {
		if signals, err = conductorSignals(tx, sessionTable, messageTable, sidColumn, fkColumn, sessions, scols, mcols, tables); err != nil {
			return err
		}
	}
	for _, session := range sessions {
		sid := firstString(session[sidColumn])
		if selected != nil && !selected[sid] {
			continue
		}
		var parts []sourcePart
		if signals != nil {
			part := signals[sid]
			part.item = "sqlite:" + origin + ":sessions:" + sid
			if parts = []sourcePart{part}; unchanged(parts) {
				continue
			}
		}
		record, err := a.session(path, tx, sessionTable, messageTable, session, sidColumn, fkColumn, scols, mcols, tables)
		if err != nil {
			return err
		}
		observe(&record, version)
		if err := emit(&record, parts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// conductorSignals describes each session's inputs without reading message
// text: its session, workspace, and repository rows, and its messages' count,
// highest rowid, and latest send and cancellation times, which Conductor's
// (session_id, sent_at) and (session_id, cancelled_at) indexes answer: 7 s for
// a cold 8.7 GB snapshot, where parsing every session takes 7 minutes (15 with
// its Git lookups). A message edited in place with none of
// these changing goes unnoticed until the session next changes; Conductor
// updates the session row as its status changes. Git lookups made while
// building a record are not covered either (main-branch merges are refreshed
// separately after each sync).
func conductorSignals(tx *sql.Tx, sessionTable, messageTable, sidColumn, fkColumn string, sessions []map[string]any, scols, mcols, tables map[string]bool) (map[string]sourcePart, error) {
	stats := map[string]sourcePart{}
	aggregates := "COUNT(*) messages,MAX(rowid) last"
	if mcols["sent_at"] {
		aggregates += ",MAX(sent_at) sent"
	}
	rows, err := queryMaps(tx, "SELECT "+fkColumn+" sid,"+aggregates+" FROM "+messageTable+" GROUP BY "+fkColumn)
	if err != nil {
		// A table without rowids (or anything else unexpected) is parsed whole.
		return nil, nil
	}
	latest := map[string][]any{}
	for _, row := range rows {
		sid := firstString(row["sid"])
		stats[sid] = sourcePart{size: integer(row["messages"]), version: integer(row["last"])}
		latest[sid] = []any{row["sent"]}
	}
	if mcols["cancelled_at"] {
		rows, err := queryMaps(tx, "SELECT "+fkColumn+" sid,MAX(cancelled_at) cancelled FROM "+messageTable+" WHERE cancelled_at IS NOT NULL GROUP BY "+fkColumn)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			sid := firstString(row["sid"])
			latest[sid] = append(latest[sid], row["cancelled"])
		}
	}
	lookup := func(table, column string) (map[string]map[string]any, error) {
		result := map[string]map[string]any{}
		if !tables[table] {
			return result, nil
		}
		columns, err := tableColumns(tx, table)
		if err != nil || !columns[column] {
			return result, err
		}
		rows, err := queryMaps(tx, "SELECT * FROM "+table)
		for _, row := range rows {
			result[firstString(row[column])] = row
		}
		return result, err
	}
	workspaces, err := lookup("workspaces", "id")
	if err != nil {
		return nil, err
	}
	repositories, err := lookup("repos", "id")
	if err != nil {
		return nil, err
	}
	signals := map[string]sourcePart{}
	for _, session := range sessions {
		sid := firstString(session[sidColumn])
		part := stats[sid]
		var workspace, repository map[string]any
		if scols["workspace_id"] && sessionTable != "workspaces" {
			workspace = workspaces[firstString(session["workspace_id"])]
			repository = repositories[firstString(workspace["repository_id"])]
		}
		part.signal = hashBytes([]byte(jsonText([]any{session, workspace, repository, latest[sid]})))
		signals[sid] = part
	}
	return signals, nil
}

func tableNames(q queryer) (map[string]bool, error) {
	rows, err := queryMaps(q, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return nil, err
	}
	result := map[string]bool{}
	for _, row := range rows {
		result[firstString(row["name"])] = true
	}
	return result, nil
}
func tableColumns(q queryer, table string) (map[string]bool, error) {
	rows, err := queryMaps(q, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	result := map[string]bool{}
	for _, row := range rows {
		result[firstString(row["name"])] = true
	}
	return result, nil
}
func firstPresent(values map[string]bool, choices ...string) string {
	for _, choice := range choices {
		if values[choice] {
			return choice
		}
	}
	return ""
}

func (a *conductorAdapter) session(path string, tx *sql.Tx, sessionTable, messageTable string, session map[string]any,
	sidColumn, fkColumn string, scols, mcols, tables map[string]bool) (WorkspaceRecord, error) {
	path = a.original(path)
	sid := firstString(session[sidColumn])
	order := ""
	if mcols["created_at"] && mcols["id"] {
		order = " ORDER BY created_at,id"
	}
	rows, err := queryMaps(tx, "SELECT * FROM "+messageTable+" WHERE "+fkColumn+"=?"+order, session[sidColumn])
	if err != nil {
		return WorkspaceRecord{}, err
	}
	messages := []MessageRecord{}
	delegated := map[string]bool{}
	currentModel := firstString(session["model"])
	for index, message := range rows {
		field, text := "none", ""
		for _, candidate := range []string{"content", "full_message", "text"} {
			if mcols[candidate] {
				value := messageText(message[candidate])
				if len(value) > len(text) {
					field, text = candidate, value
				}
			}
		}
		role := "unknown"
		if mcols["role"] {
			role = defaultString(message["role"], "unknown")
		} else if mcols["type"] {
			role = defaultString(message["type"], "unknown")
		}
		baseID := strconv.Itoa(index)
		if mcols["id"] {
			baseID = firstString(message["id"])
		}
		createdAt := ""
		if mcols["created_at"] {
			createdAt = iso(message["created_at"])
		}
		locator := fmt.Sprintf("sqlite:%s:%s:%s:%s:%d", path, messageTable, sid, field, index)
		if reported := firstString(message["model"]); reported != "" {
			currentModel = reported
		} else {
			var envelope map[string]any
			if json.Unmarshal([]byte(text), &envelope) == nil {
				if embedded := firstString(mapValue(envelope["message"])["model"]); embedded != "" {
					currentModel = embedded
				}
			}
		}
		parsed := conductorEvents(baseID, role, text, createdAt, locator, delegated)
		sender := conductorSender(message)
		for item := range parsed {
			parsed[item].Model = currentModel
			if parsed[item].Role == "user" {
				parsed[item].Sender = sender
			}
		}
		// The longest display column is not necessarily the richest accounting
		// column. Preserve structured evidence from shorter alternate columns too.
		for _, candidate := range []string{"content", "full_message", "text"} {
			alternative := messageText(message[candidate])
			var envelope map[string]any
			if alternative != text && json.Unmarshal([]byte(alternative), &envelope) == nil && hasUsageEvidence(envelope) {
				parsed = append(parsed, MessageRecord{NativeID: baseID + ":accounting:" + candidate, Role: "system", Kind: "metadata", Text: alternative, Model: currentModel, CreatedAt: createdAt, EvidenceLocator: locator + ":" + candidate, Selected: true})
			}
		}
		messages = append(messages, parsed...)
	}
	title := "Conductor " + short(sid)
	if scols["title"] && firstString(session["title"]) != "" {
		title = firstString(session["title"])
	}
	var workspace map[string]any
	if scols["workspace_id"] && session["workspace_id"] != nil && tables["workspaces"] {
		found, e := queryMaps(tx, "SELECT * FROM workspaces WHERE id=?", session["workspace_id"])
		if e != nil {
			return WorkspaceRecord{}, e
		}
		if len(found) > 0 {
			workspace = found[0]
		}
	}
	wcols := keySet(workspace)
	location := ""
	for _, key := range []string{"worktree_path", "workspace_path", "path", "cwd"} {
		if wcols[key] && firstString(workspace[key]) != "" {
			location = firstString(workspace[key])
			break
		}
	}
	if location == "" {
		for _, key := range []string{"worktree_path", "workspace_path", "path", "cwd"} {
			if scols[key] && firstString(session[key]) != "" {
				location = firstString(session[key])
				break
			}
		}
	}
	var nativeRepository map[string]any
	if workspace != nil && wcols["repository_id"] && workspace["repository_id"] != nil && tables["repos"] {
		found, e := queryMaps(tx, "SELECT * FROM repos WHERE id=?", workspace["repository_id"])
		if e != nil {
			return WorkspaceRecord{}, e
		}
		if len(found) > 0 {
			nativeRepository = found[0]
		}
	}
	rcols := keySet(nativeRepository)
	repositoryRoot, remote, repositoryName := "", "", ""
	for _, key := range []string{"root_path", "path"} {
		if rcols[key] && firstString(nativeRepository[key]) != "" {
			repositoryRoot = firstString(nativeRepository[key])
			break
		}
	}
	for _, key := range []string{"remote_url", "url"} {
		if rcols[key] && firstString(nativeRepository[key]) != "" {
			remote = firstString(nativeRepository[key])
			break
		}
	}
	for _, key := range []string{"name", "display_name"} {
		if rcols[key] && firstString(nativeRepository[key]) != "" {
			repositoryName = firstString(nativeRepository[key])
			break
		}
	}
	if remote == "" && a.localLookups() {
		remote = gitRemote(location, repositoryRoot)
	}
	updated := ""
	for _, key := range []string{"last_user_message_at", "updated_at", "created_at"} {
		if scols[key] && session[key] != nil {
			updated = iso(session[key])
			break
		}
	}
	aliases := []string{}
	for _, key := range []string{"claude_session_id", "codex_session_id", "rollout_id"} {
		if scols[key] && firstString(session[key]) != "" {
			aliases = append(aliases, firstString(session[key]))
		}
	}
	repository := a.repository(firstString(repositoryRoot, location), remote)
	if repository == nil && repositoryName != "" {
		repository = map[string]any{"display_name": repositoryName, "canonical_remote": nilIfEmpty(remote)}
	}
	if repository != nil {
		if repositoryName != "" {
			repository["display_name"] = repositoryName
		}
		repository["local_locations"] = uniqueStrings([]string{repositoryRoot, location})
	}
	prs := conductorPRs(messages)
	prTitle := ""
	if workspace != nil && wcols["pr_title"] {
		prTitle = firstString(workspace["pr_title"])
	}
	if slug := githubSlug(remote); slug != "" {
		for _, pr := range prs {
			pr["url"] = "https://github.com/" + slug + "/pull/" + fmt.Sprint(pr["number"])
			if firstString(pr["title"]) == "" && prTitle != "" {
				pr["title"] = prTitle
			}
		}
	}
	branch, lifecycle := "", "active"
	if workspace != nil {
		for _, key := range []string{"branch", "head_ref"} {
			if wcols[key] && firstString(workspace[key]) != "" {
				branch = firstString(workspace[key])
				break
			}
		}
		if wcols["state"] && firstString(workspace["state"]) != "" {
			lifecycle = firstString(workspace["state"])
		}
	}
	archiveCommit := ""
	if workspace != nil && wcols["archive_commit"] {
		archiveCommit = firstString(workspace["archive_commit"])
	}
	metadata := map[string]any{"branch": nilIfEmpty(branch), "head_ref": nilIfEmpty(archiveCommit), "lifecycle": lifecycle, "terminal": lifecycle == "archived" || lifecycle == "closed"}
	if scols["status"] {
		metadata["status"] = session["status"]
	}
	model, started := "", ""
	if scols["model"] {
		model = firstString(session["model"])
	}
	if scols["created_at"] {
		started = iso(session["created_at"])
	}
	provider := "conductor"
	if scols["agent_type"] {
		provider = defaultString(session["agent_type"], provider)
	}
	purpose, outcome := conductorPurposeOutcome(messages)
	changes := conductorToolChanges(messages, location, repositoryRoot)
	if archiveCommit != "" && repositoryRoot != "" && a.localLookups() {
		if change := conductorCommitChange(repositoryRoot, archiveCommit); change != nil {
			changes = append(changes, change)
			metadata["base_ref"] = change["base_id"]
		}
	}
	if archiveCommit != "" && a.localLookups() {
		for _, path := range uniqueStrings([]string{repositoryRoot, location}) {
			if integration := mainIntegration(path, archiveCommit, remote); integration != nil {
				for key, value := range integration {
					metadata[key] = value
				}
				break
			}
		}
	}
	metrics := reconciledTokenMetrics(WorkspaceRecord{Conversations: []ConversationRecord{{Messages: messages}}})
	if session["context_token_count"] != nil || session["context_used_percent"] != nil {
		messages = append(messages, MessageRecord{NativeID: sid + ":context-snapshot", Role: "system", Kind: "metadata", Text: jsonText(map[string]any{"type": "context_snapshot", "context_token_count": session["context_token_count"], "context_used_percent": session["context_used_percent"]}), Model: currentModel, CreatedAt: updated, EvidenceLocator: "sqlite:" + path + ":sessions:" + sid, Selected: true})
	}
	aliases = uniqueStrings(append(aliases, sid))
	return WorkspaceRecord{SourceID: sid, SourceKind: "conductor", Title: title, Account: a.config.Account, Purpose: purpose, Outcome: outcome, ActivityAt: updated, Location: location, Repository: repository, Metadata: metadata, PRs: prs, Metrics: metrics, Changes: changes, Conversations: []ConversationRecord{{NativeID: "conductor:" + sid, Provider: provider, Account: a.config.Account, Aliases: aliases, Origin: "sqlite:" + path, Coverage: conductorExtractor, Messages: messages, Model: model, StartedAt: started, EndedAt: updated}}}, nil
}

// conductorSender names who sent a Conductor message row. Conductor records
// the sending session for agent-to-agent messages, an API key name for
// scripted ones, and otherwise the signed-in account.
func conductorSender(row map[string]any) string {
	switch {
	case firstString(row["sender_session_id"]) != "":
		return "agent:" + firstString(row["sender_session_id"])
	case firstString(row["sender_api_key_name"]) != "":
		return "automation:" + firstString(row["sender_api_key_name"])
	case firstString(row["sender_id"]) != "":
		return "account:" + firstString(row["sender_id"])
	}
	return ""
}

func conductorPurposeOutcome(messages []MessageRecord) (string, string) {
	purpose, outcome := "", ""
	for _, message := range messages {
		if purpose == "" && message.Role == "user" && message.Kind == "message" {
			purpose = message.Text
		}
		if message.Role == "assistant" && message.Kind == "message" {
			outcome = message.Text
		}
	}
	if len(purpose) > 2000 {
		purpose = purpose[:2000]
	}
	if len(outcome) > 2000 {
		outcome = outcome[:2000]
	}
	return purpose, outcome
}

func conductorToolChanges(messages []MessageRecord, workspaceRoot, repositoryRoot string) []map[string]any {
	files := map[string]map[string]any{}
	for _, message := range messages {
		if message.Kind != "tool_call" || !json.Valid([]byte(message.Text)) {
			continue
		}
		var call map[string]any
		if json.Unmarshal([]byte(message.Text), &call) != nil {
			continue
		}
		tool := strings.ToLower(firstString(call["tool"]))
		input := mapValue(call["input"])
		paths := []struct{ path, status string }{}
		if input != nil && (tool == "edit" || tool == "write" || tool == "multiedit") {
			status := "modified"
			if tool == "write" {
				status = "written"
			}
			paths = append(paths, struct{ path, status string }{firstString(input["file_path"], input["path"]), status})
		}
		if tool == "apply_patch" || tool == "applypatch" {
			patch := firstString(call["input"])
			if input != nil {
				patch = firstString(input["patch"], input["input"])
			}
			pattern := regexp.MustCompile(`(?m)^\*\*\* (Update|Add|Delete) File: (.+)$`)
			for _, match := range pattern.FindAllStringSubmatch(patch, -1) {
				status := map[string]string{"Update": "modified", "Add": "added", "Delete": "deleted"}[match[1]]
				paths = append(paths, struct{ path, status string }{strings.TrimSpace(match[2]), status})
			}
		}
		for _, candidate := range paths {
			path := conductorRelativePath(candidate.path, workspaceRoot, repositoryRoot)
			if path == "" {
				continue
			}
			files[path] = map[string]any{"path": path, "status": candidate.status, "tracked": true, "evidence_locator": message.EvidenceLocator}
		}
	}
	if len(files) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(files))
	for _, file := range files {
		rows = append(rows, file)
	}
	sort.Slice(rows, func(i, j int) bool { return firstString(rows[i]["path"]) < firstString(rows[j]["path"]) })
	return []map[string]any{{"id": stableID("changes", "conductor-tools", jsonText(rows)), "classification": "attempted", "complete": false, "files": rows}}
}

func conductorRelativePath(value, workspaceRoot, repositoryRoot string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		for _, root := range []string{workspaceRoot, repositoryRoot} {
			if root == "" {
				continue
			}
			if relative, err := filepath.Rel(root, value); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return filepath.ToSlash(relative)
			}
		}
		return ""
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(clean)
}

func conductorCommitChange(repositoryRoot, commit string) map[string]any {
	if exec.Command("git", "-C", repositoryRoot, "cat-file", "-e", commit+"^{commit}").Run() != nil {
		return nil
	}
	baseBytes, _ := exec.Command("git", "-C", repositoryRoot, "rev-parse", commit+"^").Output()
	base := strings.TrimSpace(string(baseBytes))
	output, err := exec.Command("git", "-C", repositoryRoot, "show", "--format=", "--name-status", "--find-renames", commit).Output()
	if err != nil {
		return nil
	}
	files := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		status, path := parts[0], parts[len(parts)-1]
		if status == "" || path == "" {
			continue
		}
		file := map[string]any{"path": filepath.ToSlash(path), "status": "modified", "tracked": true, "evidence_locator": "git:" + repositoryRoot + ":" + commit}
		switch status[0] {
		case 'A':
			file["status"] = "added"
		case 'D':
			file["status"] = "deleted"
		case 'R':
			file["status"] = "renamed"
			if len(parts) > 2 {
				file["old_path"] = filepath.ToSlash(parts[1])
			}
		}
		files = append(files, file)
	}
	return map[string]any{"id": stableID("changes", "conductor-commit", commit), "base_id": nilIfEmpty(base), "head_id": commit, "classification": "checkpointed", "complete": true, "patch_locator": "git:" + repositoryRoot + ":" + commit, "files": files}
}

func keySet(value map[string]any) map[string]bool {
	result := map[string]bool{}
	for key := range value {
		result[key] = true
	}
	return result
}
func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
func gitRemote(locations ...string) string {
	for _, location := range locations {
		if location == "" {
			continue
		}
		if _, err := os.Stat(location); err != nil {
			continue
		}
		command := exec.Command("git", "-C", location, "config", "--get", "remote.origin.url")
		output, err := command.Output()
		if err == nil && strings.TrimSpace(string(output)) != "" {
			return strings.TrimSpace(string(output))
		}
	}
	return ""
}

var githubRemotePattern = regexp.MustCompile(`github\.com(?::|/)([^/\s]+/[^/\s]+?)(?:\.git)?$`)

func githubSlug(remote string) string {
	match := githubRemotePattern.FindStringSubmatch(remote)
	if len(match) > 1 {
		return match[1]
	}
	return ""
}

func conductorEvents(nativeID, fallbackRole, text, createdAt, locator string, delegated map[string]bool) []MessageRecord {
	base := MessageRecord{CreatedAt: createdAt, EvidenceLocator: locator, Selected: true}
	var event any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if decoder.Decode(&event) != nil {
		return []MessageRecord{{NativeID: nativeID, Role: fallbackRole, Kind: "message", Text: text, CreatedAt: createdAt, EvidenceLocator: locator, Selected: true}}
	}
	object, ok := event.(map[string]any)
	if !ok {
		return []MessageRecord{{NativeID: nativeID, Role: fallbackRole, Kind: "metadata", Text: jsonText(event), CreatedAt: createdAt, EvidenceLocator: locator, Selected: true}}
	}
	eventType := defaultString(object["type"], "metadata")
	message := mapValueDefault(object["message"])
	role := defaultString(firstNonNil(message["role"], object["role"]), fallbackRole)
	parent := firstString(object["parent_tool_use_id"])
	content := firstNonNil(message["content"], object["content"])
	blocks, _ := content.([]any)
	output := []MessageRecord{}
	for index, raw := range blocks {
		block := mapValue(raw)
		if block == nil {
			continue
		}
		blockType := firstString(block["type"])
		item := base
		item.NativeID = fmt.Sprintf("%s:block:%d", nativeID, index)
		item.ParentNativeID = parent
		switch blockType {
		case "text":
			if firstString(block["text"]) != "" {
				item.Role = role
				item.Kind = "message"
				item.Text = firstString(block["text"])
				output = append(output, item)
			}
		case "tool_use":
			name := defaultString(block["name"], "Tool")
			callID := firstString(block["id"])
			kind := "tool_call"
			if matchesFold(name, "task", "agent", "spawn_agent", "delegate", "workflow") {
				kind = "delegation"
				delegated[callID] = true
			}
			item.Role = "assistant"
			item.Kind = kind
			item.CallID = callID
			item.Text = jsonText(map[string]any{"tool": name, "input": mapValueDefault(block["input"])})
			output = append(output, item)
		case "tool_result":
			callID := firstString(block["tool_use_id"])
			kind := "tool_result"
			if delegated[callID] {
				kind = "delegation_result"
			}
			payload := map[string]any{"content": block["content"], "is_error": defaultBool(block["is_error"], false)}
			if object["tool_use_result"] != nil {
				payload["details"] = object["tool_use_result"]
			}
			item.Role = "tool"
			item.Kind = kind
			item.CallID = callID
			item.Text = jsonText(payload)
			output = append(output, item)
		}
	}
	if len(output) > 0 {
		if hasUsageEvidence(object) {
			output[0].RawText = text
		}
		return output
	}
	kind := "metadata"
	if eventType == "result" {
		kind = "result"
	} else if eventType == "error" {
		kind = "error"
	}
	base.NativeID = nativeID
	base.Role = role
	if role == "assistant" {
		base.Role = "assistant"
	}
	base.Kind = kind
	base.ParentNativeID = parent
	base.Text = jsonText(object)
	return []MessageRecord{base}
}

var prURLPattern = regexp.MustCompile(`https?://([^/\s]+)/([^/\s]+)/([^/\s]+)/pull/(\d+)`)

func conductorPRs(messages []MessageRecord) []map[string]any {
	found := map[string]map[string]any{}
	for _, message := range messages {
		for _, match := range prURLPattern.FindAllStringSubmatch(message.Text, -1) {
			number, _ := strconv.Atoi(match[4])
			key := fmt.Sprintf("%s/%s/%d", match[1], match[3], number)
			found[key] = map[string]any{"host": match[1], "number": number, "url": fmt.Sprintf("https://%s/%s/%s/pull/%d", match[1], match[2], match[3], number), "relationship": "created", "confidence": 1.0, "observed_at": nilIfEmpty(message.CreatedAt), "evidence": map[string]any{"locator": message.EvidenceLocator, "source": "native-tool-output"}}
		}
	}
	result := []map[string]any{}
	for _, value := range found {
		number := fmt.Sprint(value["number"])
		titlePattern := regexp.MustCompile(`(?i)PR\s*#` + regexp.QuoteMeta(number) + `\s*[:—-]\s*([^\n*]+)`)
		for _, message := range messages {
			if match := titlePattern.FindStringSubmatch(message.Text); len(match) > 1 {
				value["title"] = strings.TrimSpace(match[1])
				break
			}
		}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return fmt.Sprint(result[i]["url"]) < fmt.Sprint(result[j]["url"])
	})
	return result
}
