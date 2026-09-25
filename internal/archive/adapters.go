package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type MessageRecord struct {
	NativeID, Role, Kind, Text, Model, CreatedAt, ParentNativeID, PreviousNativeID, CallID, EvidenceLocator string
	RawText                                                                                                 string
	SourceOrder                                                                                             int
	Selected                                                                                                bool
	// Sender names who sent user-role input when the source records it:
	// "account:<id>" for a person, "agent:<session>" for another agent, or
	// "automation:<name>" for a script. Empty means the source is silent.
	Sender string `json:"Sender,omitempty"`
}

type ConversationRecord struct {
	NativeID, Provider, Model, Account, ParentNativeID, Origin, Coverage, StartedAt, EndedAt string
	AgentDepth                                                                               int    `json:"AgentDepth,omitempty"`
	AgentPath                                                                                string `json:"AgentPath,omitempty"`
	AgentNickname                                                                            string `json:"AgentNickname,omitempty"`
	Aliases                                                                                  []string
	Messages                                                                                 []MessageRecord
	// Observed is the version of the source this copy was read from (see
	// sourceVersion); not part of the record's digest.
	Observed int64 `json:"-"`
}

type WorkspaceRecord struct {
	SourceID, SourceKind, Title, Account, Purpose, Outcome, ActivityAt, Location string
	Repository                                                                   map[string]any
	Metadata                                                                     map[string]any
	Conversations                                                                []ConversationRecord
	WorkItems, Attempts, Handoffs, Metrics, Changes, PRs                         []map[string]any
	Observed                                                                     int64 `json:"-"`
}

// Source versions order what a host's copies of a record were read from, so an
// index never writes a capture over what that host already wrote from a newer
// read. A version is on the source host's clock, in nanoseconds: a file's
// mtime (captured copies keep it), or a database's last commit, from its and
// its WAL's mtimes. Live, a database's is read after its read transaction
// began, so it is no older than what was read; a capture's comes from the
// manifest, taken before its snapshot, so it is no newer. 0 is unknown.

// observe stamps a record and its conversations with the version read.
func observe(record *WorkspaceRecord, version int64) {
	record.Observed = version
	for index := range record.Conversations {
		record.Conversations[index].Observed = version
	}
}

// databaseVersion is the version of the database at path.
func (a baseAdapter) databaseVersion(path string) int64 {
	if a.view != nil {
		return a.view.databaseVersion(path)
	}
	version := int64(0)
	for _, file := range []string{path, path + "-wal"} {
		if info, err := os.Stat(file); err == nil {
			version = max(version, info.ModTime().UnixNano())
		}
	}
	return version
}

type Adapter interface {
	Config() SourceConfig
	Capability() string
	Fingerprint() (string, error)
	Discover(func(WorkspaceRecord) error) error
}

// SelectiveAdapter can repair an explicitly bounded set of source-native IDs
// without discovering or inserting any other records from the source.
type SelectiveAdapter interface {
	Adapter
	DiscoverSelected(map[string]bool, func(WorkspaceRecord) error) error
}

type VersionedAdapter interface {
	Adapter
	ExtractorVersion() string
}

type baseAdapter struct {
	config     SourceConfig
	capability string
	// view, when set, has the adapter read a capture of the source (see
	// capture_index.go); config.Path then names the captured copy.
	view *captureView
}

func (a baseAdapter) Config() SourceConfig { return a.config }
func (a baseAdapter) Capability() string   { return a.capability }

func (a baseAdapter) capture() *captureView { return a.view }

// bindCapture makes the adapter read the capture view names.
func (a *baseAdapter) bindCapture(view *captureView) { a.view = view }

// original is the source path a file the adapter read stands for. Origins and
// evidence locators always name it, so a capture and the live source agree.
func (a baseAdapter) original(path string) string {
	if a.view == nil {
		return path
	}
	return a.view.original(path)
}

// localLookups reports whether paths recorded in the source refer to this
// Mac, so Git may be asked about them. Another Mac's paths may not exist here,
// or may be an unrelated checkout.
func (a baseAdapter) localLookups() bool {
	return a.view == nil || !a.view.offHost()
}

func (a baseAdapter) repository(location, remote string) map[string]any {
	if !a.localLookups() {
		return repositoryFromName(location, remote)
	}
	return repositoryFromLocation(location, remote)
}

func MakeAdapter(config SourceConfig) (Adapter, error) {
	switch strings.ToLower(config.Kind) {
	case "canonical", "tl1-export":
		capability := "retrieval-only"
		if strings.EqualFold(config.Kind, "tl1-export") {
			capability = "tl1-release"
		}
		return &canonicalAdapter{baseAdapter{config: config, capability: capability}}, nil
	case "codex":
		return &jsonlAdapter{baseAdapter: baseAdapter{config: config, capability: "retrieval-only"}, provider: "codex"}, nil
	case "claude":
		return &jsonlAdapter{baseAdapter: baseAdapter{config: config, capability: "retrieval-only"}, provider: "claude"}, nil
	case "chatgpt", "chatgpt-export":
		return &chatGPTAdapter{baseAdapter{config: config, capability: "retrieval-only"}}, nil
	case "conductor":
		return &conductorAdapter{baseAdapter: baseAdapter{config: config, capability: "retrieval-only"}}, nil
	case "tl1":
		return &tl1Adapter{baseAdapter: baseAdapter{config: config, capability: "retrieval-only"}}, nil
	default:
		return nil, fmt.Errorf("unsupported source kind: %s", config.Kind)
	}
}

func pathFingerprint(root string, include func(string, os.DirEntry) bool) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	rows := []string{}
	if !info.IsDir() {
		rows = append(rows, fmt.Sprintf("%s:%d:%d", filepath.Base(root), info.Size(), info.ModTime().UnixNano()))
	} else {
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if entry.IsDir() || !include(path, entry) {
				return nil
			}
			stat, e := entry.Info()
			if e == nil {
				relative, _ := filepath.Rel(root, path)
				rows = append(rows, fmt.Sprintf("%s:%d:%d", relative, stat.Size(), stat.ModTime().UnixNano()))
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(rows)
	return hashBytes([]byte(strings.Join(rows, "\n"))), nil
}

func messageText(value any) string {
	switch item := value.(type) {
	case string:
		return item
	case []any:
		parts := []string{}
		for _, child := range item {
			switch v := child.(type) {
			case string:
				parts = append(parts, v)
			case map[string]any:
				if kind := firstString(v["type"]); kind == "text" || kind == "input_text" || kind == "output_text" {
					parts = append(parts, firstString(v["text"]))
				}
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		return messageText(firstNonNil(item["content"], item["text"]))
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// iso stores source timestamps in the canonical format. Values it cannot parse
// are kept verbatim rather than guessed at or dropped.
func iso(value any) string {
	if canonical := canonicalTime(value); canonical != "" {
		return canonical
	}
	return asString(value)
}

func readJSON(path string) (any, error) {
	value, _, err := readJSONVersion(path)
	return value, err
}

// readJSONVersion also reports the version (mtime) of the file it read.
func readJSONVersion(path string) (any, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	value, err := decodeJSONValue(file)
	return value, info.ModTime().UnixNano(), err
}

func decodeJSONValue(file io.Reader) (any, error) {
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	return value, nil
}
func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values")
}

type canonicalAdapter struct{ baseAdapter }

func (a *canonicalAdapter) Fingerprint() (string, error) {
	fingerprint, err := pathFingerprint(a.config.Path, func(path string, entry os.DirEntry) bool {
		return strings.HasSuffix(strings.ToLower(entry.Name()), ".json")
	})
	return "token-events-v2:" + fingerprint, err
}
func (a *canonicalAdapter) Discover(emit func(WorkspaceRecord) error) error {
	info, err := os.Stat(a.config.Path)
	if err != nil {
		return err
	}
	files := []string{a.config.Path}
	if info.IsDir() {
		files = nil
		_ = filepath.WalkDir(a.config.Path, func(path string, entry os.DirEntry, e error) error {
			if e == nil && !entry.IsDir() && strings.HasSuffix(strings.ToLower(path), ".json") {
				files = append(files, path)
			}
			return nil
		})
		sort.Strings(files)
	}
	for _, file := range files {
		raw, version, err := readJSONVersion(file)
		if err != nil {
			return fmt.Errorf("cannot read canonical export %s: %w", file, err)
		}
		document, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("canonical export %s must contain an object", file)
		}
		rows := []any{document}
		if workspaces, exists := document["workspaces"]; exists {
			var valid bool
			rows, valid = workspaces.([]any)
			if !valid {
				return fmt.Errorf("canonical export %s workspaces must be an array", file)
			}
		}
		for index, value := range rows {
			row, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("canonical export %s workspace %d must be an object", file, index)
			}
			record, err := a.workspace(row, file, index)
			if err != nil {
				return err
			}
			observe(&record, version)
			if err := emit(record); err != nil {
				return err
			}
		}
	}
	return nil
}
func (a *canonicalAdapter) workspace(row map[string]any, file string, index int) (WorkspaceRecord, error) {
	conversations := []ConversationRecord{}
	rawConversations, ok := sliceValue(row["conversations"])
	if !ok {
		return WorkspaceRecord{}, fmt.Errorf("canonical export %s workspace %d conversations must be an array", file, index)
	}
	for cindex, raw := range rawConversations {
		conversation, ok := raw.(map[string]any)
		if !ok {
			return WorkspaceRecord{}, fmt.Errorf("canonical export %s conversation %d is malformed", file, cindex)
		}
		rawMessages, ok := sliceValue(conversation["messages"])
		if !ok {
			return WorkspaceRecord{}, fmt.Errorf("canonical export %s conversation %d is malformed", file, cindex)
		}
		messages := []MessageRecord{}
		for mindex, rawMessage := range rawMessages {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				return WorkspaceRecord{}, fmt.Errorf("canonical export %s message %d:%d must be an object", file, cindex, mindex)
			}
			id := firstString(message["id"], message["native_id"])
			if id == "" {
				id = fmt.Sprintf("m%d", mindex)
			}
			messages = append(messages, MessageRecord{RawText: accountingRaw(message), NativeID: id, Role: defaultString(message["role"], "unknown"), Kind: defaultString(message["kind"], "message"), Text: messageText(firstNonNil(message["text"], message["content"])), CreatedAt: iso(firstNonNil(message["created_at"], message["timestamp"])), ParentNativeID: firstString(message["parent_id"]), CallID: firstString(message["call_id"]), Selected: defaultBool(message["selected"], true), EvidenceLocator: fmt.Sprintf("%s:%d:%d", a.original(file), cindex, mindex)})
		}
		nativeID := firstString(conversation["id"], conversation["native_id"])
		if nativeID == "" {
			nativeID = fmt.Sprintf("%s-%d", strings.TrimSuffix(filepath.Base(file), filepath.Ext(file)), cindex)
		}
		conversations = append(conversations, ConversationRecord{NativeID: nativeID, Provider: defaultString(conversation["provider"], a.config.Kind), Account: a.config.Account, Model: firstString(conversation["model"]), ParentNativeID: firstString(conversation["parent_id"]), Origin: firstString(conversation["origin"]), Coverage: defaultString(conversation["coverage"], "complete"), Messages: messages, StartedAt: iso(conversation["started_at"]), EndedAt: iso(conversation["ended_at"]), Aliases: stringSlice(conversation["aliases"])})
	}
	sid := firstString(row["id"], row["source_id"])
	if sid == "" {
		sid = fmt.Sprintf("%s-%d", strings.TrimSuffix(filepath.Base(file), filepath.Ext(file)), index)
	}
	kind := a.config.Kind
	if a.capability == "tl1-release" && firstString(row["source_kind"]) != "" {
		kind = firstString(row["source_kind"])
	}
	return WorkspaceRecord{SourceID: sid, SourceKind: kind, Title: defaultString(firstNonNil(row["title"], row["purpose"]), sid), Account: a.config.Account, Purpose: firstString(row["purpose"]), Outcome: firstString(row["outcome"]), ActivityAt: iso(row["activity_at"]), Location: firstString(row["location"]), Repository: mapValue(row["repository"]), Metadata: mapValueDefault(row["metadata"]), Conversations: conversations, WorkItems: mapSlice(row["work_items"]), Attempts: mapSlice(row["attempts"]), Handoffs: mapSlice(row["handoffs"]), Metrics: mapSlice(row["metrics"]), Changes: mapSlice(row["changes"]), PRs: mapSlice(row["prs"])}, nil
}

func sliceValue(value any) ([]any, bool) {
	if value == nil {
		return []any{}, true
	}
	items, ok := value.([]any)
	return items, ok
}
func mapValue(value any) map[string]any { result, _ := value.(map[string]any); return result }
func mapValueDefault(value any) map[string]any {
	result := mapValue(value)
	if result == nil {
		return map[string]any{}
	}
	return result
}
func mapSlice(value any) []map[string]any {
	if rows, ok := value.([]map[string]any); ok {
		return rows
	}
	items, _ := sliceValue(value)
	result := []map[string]any{}
	for _, item := range items {
		if row, ok := item.(map[string]any); ok {
			result = append(result, row)
		}
	}
	return result
}
func stringSlice(value any) []string {
	items, _ := sliceValue(value)
	result := []string{}
	for _, item := range items {
		if text := firstString(item); text != "" {
			result = append(result, text)
		}
	}
	return result
}
func defaultString(value any, fallback string) string {
	if text := firstString(value); text != "" {
		return text
	}
	return fallback
}
func defaultBool(value any, fallback bool) bool {
	if item, ok := value.(bool); ok {
		return item
	}
	return fallback
}

type jsonlAdapter struct {
	baseAdapter
	provider string
}

var codexForkFilename = regexp.MustCompile(`([0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})_([0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})\.jsonl$`)

func (a *jsonlAdapter) Fingerprint() (string, error) {
	fingerprint, err := pathFingerprint(a.config.Path, func(path string, entry os.DirEntry) bool {
		return strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") && a.accept(path)
	})
	return jsonlExtractor + ":" + fingerprint, err
}

// jsonlExtractor versions the Claude and Codex parsers; bump it when parsing
// changes so every file is parsed again. v8 retains tool result metadata (exit
// codes, durations, interruptions); v9 retains Codex hosted web searches.
const jsonlExtractor = "message-model-v9"

func (a *jsonlAdapter) accept(path string) bool {
	relative, _ := filepath.Rel(a.config.Path, path)
	relative = filepath.ToSlash(relative)
	if a.provider == "codex" {
		return strings.HasPrefix(relative, "sessions/") || strings.HasPrefix(relative, "archived_sessions/") || strings.HasPrefix(filepath.Base(relative), "rollout")
	}
	return true
}
func (a *jsonlAdapter) Discover(emit func(WorkspaceRecord) error) error {
	return a.discoverParts(nil, func(record *WorkspaceRecord, _ []sourcePart) error {
		if record == nil {
			return nil
		}
		return emit(*record)
	})
}

func (a *jsonlAdapter) partExtractor() string { return a.provider + ":" + jsonlExtractor }

// discoverParts parses each transcript, a Claude session together with its
// subagents' files, unless unchanged passes over the files as they are.
func (a *jsonlAdapter) discoverParts(unchanged func([]sourcePart) bool, emit func(*WorkspaceRecord, []sourcePart) error) error {
	groups, infos, roots := map[string][]string{}, map[string]fs.FileInfo{}, []string{}
	if err := filepath.WalkDir(a.config.Path, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".jsonl") || !a.accept(path) {
			return nil
		}
		root := path
		if a.provider == "claude" {
			root = claudeRootPath(path)
		}
		if _, seen := groups[root]; !seen {
			roots = append(roots, root)
		}
		groups[root] = append(groups[root], path)
		if info, err := entry.Info(); err == nil {
			infos[path] = info
		}
		return nil
	}); err != nil {
		return err
	}
	if a.provider == "claude" {
		sort.Strings(roots)
	}
	for _, root := range roots {
		paths := groups[root]
		sort.Slice(paths, func(i, j int) bool {
			if paths[i] == root || paths[j] == root {
				return paths[i] == root
			}
			return paths[i] < paths[j]
		})
		if unchanged != nil {
			parts := make([]sourcePart, 0, len(paths))
			for _, path := range paths {
				if info := infos[path]; info != nil {
					parts = append(parts, sourcePart{item: a.original(path), size: info.Size(), version: info.ModTime().UnixNano()})
				}
			}
			if len(parts) == len(paths) && unchanged(parts) {
				continue
			}
		}
		var combined *WorkspaceRecord
		read := []sourcePart{}
		for _, path := range paths {
			record, part, ok, err := a.parseFile(path)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			if part.item != "" {
				read = append(read, part)
			}
			if !ok || len(record.Conversations) == 0 || len(record.Conversations[0].Messages) == 0 {
				continue
			}
			if combined == nil {
				combined = &record
			} else {
				combined.Conversations = append(combined.Conversations, record.Conversations...)
				combined.Observed = max(combined.Observed, record.Observed)
				if record.ActivityAt > combined.ActivityAt {
					combined.ActivityAt = record.ActivityAt
				}
			}
		}
		if combined != nil && a.provider == "claude" {
			combined.Metrics = reconciledTokenMetrics(*combined)
		}
		if err := emit(combined, read); err != nil {
			return fmt.Errorf("ingest %s: %w", root, err)
		}
	}
	return nil
}

func claudeRootPath(path string) string {
	for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "subagents" {
			return filepath.Dir(dir) + ".jsonl"
		}
	}
	return path
}

// parseHook observes every transcript parsed; tests count them.
var parseHook func(path string)

func (a *jsonlAdapter) parse(path string) (WorkspaceRecord, bool, error) {
	record, _, ok, err := a.parseFile(path)
	return record, ok, err
}

// parseFile also reports the version of the file it read: exactly the bytes
// present when it was opened, so a log appended meanwhile, or a capture
// replaced meanwhile, is parsed again next time rather than taken as current.
func (a *jsonlAdapter) parseFile(path string) (WorkspaceRecord, sourcePart, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return WorkspaceRecord{}, sourcePart{}, false, nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return WorkspaceRecord{}, sourcePart{}, false, nil
	}
	part := sourcePart{item: a.original(path), size: info.Size(), version: info.ModTime().UnixNano()}
	if parseHook != nil {
		parseHook(path)
	}
	events := []map[string]any{}
	reader := bufio.NewReaderSize(io.LimitReader(file, part.size), 64*1024)
	line := 0
	for {
		encoded, readErr := reader.ReadBytes('\n')
		if len(encoded) == 0 && readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != io.EOF {
			return WorkspaceRecord{}, part, false, fmt.Errorf("read %s line %d: %w", path, line+1, readErr)
		}
		line++
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		var event map[string]any
		if decoder.Decode(&event) == nil {
			event["_line"] = int64(line)
			events = append(events, event)
		}
		if readErr == io.EOF {
			break
		}
	}
	var record WorkspaceRecord
	var ok bool
	if a.provider == "codex" {
		record, ok, err = a.codex(path, events)
	} else {
		record, ok, err = a.claude(path, events)
	}
	observe(&record, part.version)
	return record, part, ok, err
}

func (a *jsonlAdapter) codex(path string, events []map[string]any) (WorkspaceRecord, bool, error) {
	origin := a.original(path)
	meta := map[string]any{}
	model := ""
	messages := []MessageRecord{}
	usage := map[string]float64{}
	delegated := map[string]bool{}
	toolNames := map[string]string{}
	// Codex reports each process an exec call starts, and MCP call outcomes,
	// as completed items between the call and its output. They are attached
	// to the call's result as details once the transcript has been read.
	details := map[string]map[string]any{}
	resultIndexes := map[string]int{}
	resultContent := map[string]any{}
	openCall := ""
	// Codex does not record why it compacted. A compaction that opens a turn
	// before any input or model output is a user /compact; one inside a turn
	// is automatic. Compactions replayed at the start of a resumed or forked
	// session belong to neither and stay unlabeled.
	inTurn, turnFresh := false, false
	for _, event := range events {
		payload := mapValue(event["payload"])
		if payload == nil {
			payload = event
		}
		etype := firstString(event["type"])
		switch ptype := firstString(payload["type"]); {
		case etype == "event_msg" && ptype == "task_started":
			inTurn, turnFresh = true, true
		case etype == "event_msg" && (ptype == "task_complete" || ptype == "turn_aborted"):
			inTurn, turnFresh = false, false
		case etype == "compacted":
			if turnFresh {
				event["compaction_trigger"] = "manual"
			} else if inTurn {
				event["compaction_trigger"] = "auto"
			}
			turnFresh = false
		case etype == "response_item" || (etype == "event_msg" && (ptype == "user_message" || ptype == "agent_message")):
			turnFresh = false
		}
		if etype == "event_msg" && firstString(payload["type"]) == "item_completed" {
			codexItemDetails(mapValue(payload["item"]), openCall, details)
			continue
		}
		if etype == "session_meta" {
			if len(meta) == 0 {
				meta = payload
			}
			model = defaultString(payload["model"], model)
			continue
		}
		if etype == "turn_context" {
			model = defaultString(payload["model"], model)
			continue
		}
		role, text, kind, sender := "", "", "", ""
		if etype == "event_msg" && (firstString(payload["type"]) == "user_message" || firstString(payload["type"]) == "agent_message") {
			if firstString(payload["type"]) == "user_message" {
				role, sender = "user", codexSender(meta)
				if codexSubagent(meta) {
					role = agentRole
				}
			} else {
				role = "assistant"
			}
			text = messageText(firstNonNil(payload["message"], payload["text"]))
			kind = "message"
		} else if etype == "response_item" && firstString(payload["type"]) == "message" {
			role = defaultString(payload["role"], "unknown")
			if role == "user" {
				base := MessageRecord{RawText: accountingRaw(event), NativeID: firstString(payload["id"], event["id"]), Model: model, CreatedAt: iso(event["timestamp"]), EvidenceLocator: fmt.Sprintf("%s:%d", origin, integer(event["_line"])), Selected: true, Sender: codexSender(meta)}
				if base.NativeID == "" {
					base.NativeID = fmt.Sprintf("line-%d", integer(event["_line"]))
				}
				messages = append(messages, codexUserMessages(base, payload, codexSubagent(meta))...)
				continue
			}
			text = messageText(payload["content"])
			kind = "message"
		} else if etype == "response_item" && (firstString(payload["type"]) == "function_call" || firstString(payload["type"]) == "custom_tool_call") {
			role = "assistant"
			name := defaultString(payload["name"], "tool")
			callID := firstString(payload["call_id"])
			input := firstNonNil(payload["arguments"], payload["input"])
			if serialized, ok := input.(string); ok {
				var decoded any
				if json.Unmarshal([]byte(serialized), &decoded) == nil {
					input = decoded
				}
			}
			text = jsonText(map[string]any{"tool": name, "input": input})
			toolNames[callID] = name
			openCall = callID
			kind = "tool_call"
			if name == "spawn_agent" || name == "delegate" {
				kind = "delegation"
				delegated[callID] = true
			}
		} else if etype == "response_item" && (firstString(payload["type"]) == "function_call_output" || firstString(payload["type"]) == "custom_tool_call_output") {
			role = "tool"
			callID := firstString(payload["call_id"])
			text = jsonText(map[string]any{"tool": toolNames[callID], "content": payload["output"]})
			resultIndexes[callID] = len(messages)
			resultContent[callID] = payload["output"]
			if openCall == callID {
				openCall = ""
			}
			kind = "tool_result"
			if delegated[callID] {
				kind = "delegation_result"
			}
		} else if etype == "response_item" && firstString(payload["type"]) == "web_search_call" {
			// Hosted web searches carry their action (a search, or a page opened
			// or searched within) but no output item. Record them as a call and
			// a completion so the tool ledger sees what was searched and visited.
			id := defaultString(payload["id"], fmt.Sprintf("line-%d", integer(event["_line"])))
			callID := "web_search:" + id
			action := mapValue(payload["action"])
			if action == nil {
				action = map[string]any{}
			}
			status := defaultString(payload["status"], "completed")
			summary := "Web search " + status
			if target := firstString(action["url"]); target != "" {
				summary += " for: " + target
			} else if query := firstString(action["query"]); query != "" {
				summary += ": " + query
			}
			base := MessageRecord{Model: model, CallID: callID, CreatedAt: iso(event["timestamp"]), EvidenceLocator: fmt.Sprintf("%s:%d", origin, integer(event["_line"])), Selected: true}
			call, result := base, base
			call.NativeID, call.Role, call.Kind, call.RawText = id, "assistant", "tool_call", accountingRaw(event)
			call.Text = jsonText(map[string]any{"tool": "web_search", "input": action})
			result.NativeID, result.Role, result.Kind = id+":result", "tool", "tool_result"
			result.Text = jsonText(map[string]any{"tool": "web_search", "content": summary, "details": map[string]any{"status": status}})
			messages = append(messages, call, result)
			continue
		} else {
			if info := mapValue(payload["info"]); info != nil {
				if tokens := mapValue(info["total_token_usage"]); tokens != nil {
					for key, value := range tokens {
						if number, ok := number(value); ok {
							usage[key] = number
						}
					}
				}
			}
			if firstString(payload["type"]) != "token_count" && etype != "compacted" && firstString(payload["type"]) != "compaction" && !hasUsageEvidence(event) {
				continue
			}
			role, kind, text = "system", "metadata", jsonText(event)
		}
		if text != "" {
			id := firstString(payload["id"], event["id"])
			if id == "" {
				id = fmt.Sprintf("line-%d", integer(event["_line"]))
			}
			messages = append(messages, MessageRecord{RawText: accountingRaw(event), NativeID: id, Role: role, Kind: kind, Text: text, Model: model, CallID: firstString(payload["call_id"]), CreatedAt: iso(event["timestamp"]), EvidenceLocator: fmt.Sprintf("%s:%d", origin, integer(event["_line"])), Selected: true, Sender: sender})
		}
	}
	for callID, extra := range details {
		if index, ok := resultIndexes[callID]; ok && index < len(messages) && messages[index].CallID == callID {
			messages[index].Text = jsonText(map[string]any{"tool": toolNames[callID], "content": resultContent[callID], "details": extra})
		}
	}
	nativeID := defaultString(meta["id"], strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	spawn := mapValue(mapValue(mapValue(meta["source"])["subagent"])["thread_spawn"])
	parentNativeID := firstString(meta["parent_thread_id"], spawn["parent_thread_id"])
	agentDepth := int(integer(spawn["depth"]))
	if parentNativeID != "" && agentDepth < 1 {
		agentDepth = 1
	}
	if match := codexForkFilename.FindStringSubmatch(filepath.Base(path)); len(match) == 3 && nativeID == match[1] {
		parentNativeID, nativeID = nativeID, match[2]
	}
	cwd := firstString(meta["cwd"])
	git := mapValueDefault(meta["git"])
	repository := a.repository(cwd, firstString(git["repository_url"]))
	purpose, outcome := agentPurposeOutcome(messages)
	metrics := []map[string]any{}
	for key, value := range usage {
		metrics = append(metrics, map[string]any{"name": key, "value": value, "unit": "tokens", "status": "reported-cumulative", "extractor_version": "codex-v1", "coverage": 1.0})
	}
	started, ended := "", ""
	if len(events) > 0 {
		started = iso(events[0]["timestamp"])
		ended = iso(events[len(events)-1]["timestamp"])
	}
	record := WorkspaceRecord{SourceID: nativeID, SourceKind: "codex", Title: defaultString(meta["title"], "Codex "+short(nativeID)), Account: a.config.Account, Purpose: purpose, Outcome: outcome, ActivityAt: ended, Location: cwd, Repository: repository, Metadata: map[string]any{"originator": meta["originator"], "branch": git["branch"], "head_ref": git["commit_hash"]}, Conversations: []ConversationRecord{{NativeID: nativeID, Provider: "codex", Account: a.config.Account, Model: model, ParentNativeID: parentNativeID, AgentDepth: agentDepth, AgentPath: firstString(meta["agent_path"], spawn["agent_path"]), AgentNickname: firstString(meta["agent_nickname"], spawn["agent_nickname"]), Origin: origin, Coverage: "complete", Messages: messages, StartedAt: started, EndedAt: ended}}, Metrics: metrics}
	record.Metrics = reconciledTokenMetrics(record)
	return record, true, nil
}

func (a *jsonlAdapter) claude(path string, events []map[string]any) (WorkspaceRecord, bool, error) {
	origin := a.original(path)
	messages := []MessageRecord{}
	nativeID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	rootID := ""
	if root := claudeRootPath(path); root != path {
		rootID = strings.TrimSuffix(filepath.Base(root), filepath.Ext(root))
	}
	cwd, started, ended, model, branch, title := "", "", "", "", "", ""
	tokens := map[string]float64{"input_tokens": 0, "output_tokens": 0, "cache_tokens": 0}
	delegated := map[string]bool{}
	toolNames := map[string]string{}
	resultCalls := map[string]string{}
	for _, event := range events {
		if rootID == "" {
			nativeID = defaultString(firstNonNil(event["sessionId"], event["session_id"]), nativeID)
		}
		cwd = defaultString(event["cwd"], cwd)
		if firstString(event["gitBranch"]) != "" && firstString(event["gitBranch"]) != "HEAD" {
			branch = firstString(event["gitBranch"])
		}
		if firstString(event["type"]) == "ai-title" {
			title = defaultString(event["aiTitle"], title)
		}
		timestamp := iso(event["timestamp"])
		if started == "" {
			started = timestamp
		}
		if timestamp != "" {
			ended = timestamp
		}
		message := mapValue(event["message"])
		if message == nil {
			message = event
		}
		role := defaultString(event["type"], firstString(message["role"]))
		if firstString(event["subtype"]) == "compact_boundary" {
			messages = append(messages, MessageRecord{NativeID: fmt.Sprintf("compact-%d", integer(event["_line"])), Role: "system", Kind: "metadata", Text: jsonText(event), Model: model, CreatedAt: timestamp, EvidenceLocator: fmt.Sprintf("%s:%d", origin, integer(event["_line"])), Selected: true})
		}
		if role != "user" && role != "assistant" {
			if hasUsageEvidence(event) {
				messages = append(messages, MessageRecord{NativeID: fmt.Sprintf("usage-%d", integer(event["_line"])), Role: "system", Kind: "metadata", Text: jsonText(event), Model: model, CreatedAt: timestamp, EvidenceLocator: fmt.Sprintf("%s:%d", origin, integer(event["_line"])), Selected: true})
			}
			continue
		}
		model = defaultString(message["model"], model)
		parent := firstString(event["parentUuid"])
		content := firstNonNil(message["content"], event["content"])
		text := messageText(content)
		baseID := firstString(event["uuid"], event["id"])
		if baseID == "" {
			baseID = fmt.Sprintf("line-%d", integer(event["_line"]))
		}
		item := MessageRecord{NativeID: baseID, Role: role, Kind: "message", Text: text, Model: model, PreviousNativeID: parent, CreatedAt: timestamp, EvidenceLocator: fmt.Sprintf("%s:%d", origin, integer(event["_line"])), Selected: true}
		if role != "user" {
			if text != "" {
				messages = append(messages, item)
			}
		} else if source := claudeInjectedSource(event, text); source != "" {
			if text != "" {
				context := injectedContext(item, source, text)
				// Image captions follow the tool result that carried the image.
				context.CallID = resultCalls[parent]
				messages = append(messages, context)
			}
		} else {
			if rootID != "" {
				item.Role = agentRole
			}
			item.Sender = claudeSender(event)
			images := imageAttachments(content)
			if item.Text == "" && len(images) > 0 {
				item.Text = "(image attachment)"
			}
			if item.Text != "" {
				messages = append(messages, item)
			}
			if len(images) > 0 {
				attachment := attachmentRecord(item, item.Role, images)
				attachment.NativeID = baseID + ":attachments"
				messages = append(messages, attachment)
			}
		}
		if blocks, ok := content.([]any); ok {
			for index, raw := range blocks {
				block := mapValue(raw)
				if block == nil {
					continue
				}
				blockType := firstString(block["type"])
				callID := firstString(block["id"], block["tool_use_id"])
				kind, blockText, blockRole := "", "", role
				if blockType == "tool_use" {
					name := defaultString(block["name"], "tool")
					toolNames[callID] = name
					kind = "tool_call"
					if matchesFold(name, "task", "agent", "spawn_agent", "delegate", "workflow") {
						kind = "delegation"
						delegated[callID] = true
					}
					blockText = jsonText(map[string]any{"tool": name, "input": mapValueDefault(block["input"])})
				} else if blockType == "tool_result" {
					kind = "tool_result"
					if delegated[callID] {
						kind = "delegation_result"
					}
					blockRole = "tool"
					resultCalls[baseID] = callID
					payload := map[string]any{"tool": toolNames[callID], "content": block["content"], "is_error": block["is_error"]}
					if details := compactToolDetails(event["toolUseResult"]); len(details) > 0 {
						payload["details"] = details
					}
					blockText = jsonText(payload)
				} else {
					continue
				}
				messages = append(messages, MessageRecord{NativeID: fmt.Sprintf("%s:block:%d", baseID, index), Role: blockRole, Kind: kind, Text: blockText, Model: model, PreviousNativeID: parent, CallID: callID, CreatedAt: timestamp, EvidenceLocator: fmt.Sprintf("%s:%d:block:%d", origin, integer(event["_line"]), index), Selected: true})
			}
		}
		if usage := mapValue(message["usage"]); usage != nil {
			messages = append(messages, MessageRecord{RawText: jsonText(event), NativeID: baseID + ":usage", Role: "system", Kind: "metadata", Text: jsonText(map[string]any{"type": "token_usage", "usage": usage, "usage_message_id": firstString(message["id"])}), Model: model, PreviousNativeID: parent, CreatedAt: timestamp, EvidenceLocator: fmt.Sprintf("%s:%d", origin, integer(event["_line"])), Selected: true})
			for _, key := range []string{"input_tokens", "output_tokens"} {
				if value, ok := number(usage[key]); ok {
					tokens[key] += value
				}
			}
			for _, key := range []string{"cache_creation_input_tokens", "cache_read_input_tokens"} {
				if value, ok := number(usage[key]); ok {
					tokens["cache_tokens"] += value
				}
			}
		}
	}
	if rootID != "" {
		subagentsDir := filepath.Join(strings.TrimSuffix(claudeRootPath(path), ".jsonl"), "subagents")
		relative, _ := filepath.Rel(subagentsDir, path)
		nativeID = rootID + ":subagent:" + strings.TrimSuffix(filepath.ToSlash(relative), filepath.Ext(relative))
	}
	repository := a.repository(cwd, "")
	purpose, outcome := agentPurposeOutcome(messages)
	metrics := []map[string]any{}
	for key, value := range tokens {
		if value > 0 {
			metrics = append(metrics, map[string]any{"name": key, "value": value, "unit": "tokens", "status": "reported-incremental", "extractor_version": "claude-v1", "coverage": 1.0})
		}
	}
	sourceID := nativeID
	if rootID != "" {
		sourceID = rootID
	}
	depth := 0
	if rootID != "" {
		depth = 1
	}
	record := WorkspaceRecord{SourceID: sourceID, SourceKind: "claude", Title: defaultString(title, "Claude "+short(sourceID)), Account: a.config.Account, Purpose: purpose, Outcome: outcome, ActivityAt: ended, Location: cwd, Repository: repository, Metadata: map[string]any{"branch": branch}, Conversations: []ConversationRecord{{NativeID: nativeID, Provider: "claude", Account: a.config.Account, Model: model, ParentNativeID: rootID, AgentDepth: depth, Origin: origin, Coverage: "complete", Messages: messages, StartedAt: started, EndedAt: ended}}, Metrics: metrics}
	record.Metrics = reconciledTokenMetrics(record)
	return record, true, nil
}

func agentPurposeOutcome(messages []MessageRecord) (string, string) {
	purpose, outcome := "", ""
	for _, message := range messages {
		if message.Kind != "message" {
			continue
		}
		if purpose == "" && (message.Role == "user" || message.Role == agentRole) {
			purpose = message.Text
		}
		if message.Role == "assistant" {
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

func number(value any) (float64, bool) {
	switch item := value.(type) {
	case float64:
		return item, true
	case int64:
		return float64(item), true
	case json.Number:
		value, err := item.Float64()
		return value, err == nil
	case string:
		value, err := strconv.ParseFloat(item, 64)
		return value, err == nil
	}
	return 0, false
}
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func short(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}
func matchesFold(value string, choices ...string) bool {
	for _, choice := range choices {
		if strings.EqualFold(value, choice) {
			return true
		}
	}
	return false
}

// toolDetailSkip lists tool result metadata that duplicates the result text
// or file contents already retained elsewhere.
var toolDetailSkip = map[string]bool{"stdout": true, "stderr": true, "content": true, "originalFile": true, "oldString": true, "newString": true, "file": true, "filenames": true, "results": true, "matches": true, "listing": true, "prompt": true, "result": true, "code": true, "codeText": true}

// compactToolDetails keeps the scalar metadata of a tool result (exit status,
// duration, interruption, persisted-output markers) and line counts for
// structured patches, without the bulk output it describes.
func compactToolDetails(value any) map[string]any {
	source := mapValue(value)
	if source == nil {
		return nil
	}
	details := map[string]any{}
	for key, item := range source {
		if toolDetailSkip[key] {
			continue
		}
		switch typed := item.(type) {
		case bool, json.Number, float64, int64:
			details[key] = typed
		case string:
			if len(typed) <= 300 {
				details[key] = typed
			}
		}
	}
	if hunks, ok := source["structuredPatch"].([]any); ok {
		added, removed := 0, 0
		for _, raw := range hunks {
			lines, _ := mapValue(raw)["lines"].([]any)
			for _, line := range lines {
				text := firstString(line)
				if strings.HasPrefix(text, "+") {
					added++
				} else if strings.HasPrefix(text, "-") {
					removed++
				}
			}
		}
		details["lines_added"], details["lines_removed"] = added, removed
	}
	return details
}

// codexItemDetails records what a completed Codex item says about the call
// it belongs to: the processes an exec call ran, or an MCP call's outcome.
func codexItemDetails(item map[string]any, openCall string, details map[string]map[string]any) {
	if item == nil {
		return
	}
	durationMS := func(value any) any {
		duration := mapValue(value)
		if duration == nil {
			return nil
		}
		seconds, _ := number(duration["secs"])
		nanos, _ := number(duration["nanos"])
		return int64(seconds*1000 + nanos/1e6)
	}
	switch firstString(item["type"]) {
	case "CommandExecution":
		if openCall == "" {
			return
		}
		command := ""
		switch value := item["command"].(type) {
		case []any:
			words := make([]string, 0, len(value))
			for _, word := range value {
				words = append(words, firstString(word))
			}
			if len(words) >= 3 && (words[1] == "-lc" || words[1] == "-c") {
				command = words[len(words)-1]
			} else {
				command = strings.Join(words, " ")
			}
		case string:
			command = value
		}
		kinds := []string{}
		if parsed, ok := item["parsed_cmd"].([]any); ok {
			for _, raw := range parsed {
				if kind := firstString(mapValue(raw)["type"]); kind != "" {
					kinds = append(kinds, kind)
				}
			}
		}
		entry := map[string]any{"command": clipText(command, 2000), "exit_code": item["exit_code"], "status": item["status"], "duration_ms": durationMS(item["duration"])}
		if len(kinds) > 0 {
			entry["parsed"] = kinds
		}
		if details[openCall] == nil {
			details[openCall] = map[string]any{}
		}
		commands, _ := details[openCall]["commands"].([]any)
		details[openCall]["commands"] = append(commands, entry)
	case "McpToolCall":
		id := firstString(item["id"])
		if id == "" {
			return
		}
		if details[id] == nil {
			details[id] = map[string]any{}
		}
		details[id]["status"] = item["status"]
		details[id]["server"] = item["server"]
		details[id]["duration_ms"] = durationMS(item["duration"])
	}
}
