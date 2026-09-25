package archive

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// toolLedgerVersion names the derivation below. Conversations whose ledger
// was built by another version are rebuilt by BackfillToolLedger.
const toolLedgerVersion = "tools-v2"

// modelRequest is one model API request reconstructed from usage evidence.
// ContextGrowth is how much the prompt grew since the previous request in the
// same agent stream, net of that request's own output; it is what tool
// results (and any user turns) added to the context.
type modelRequest struct {
	Key, NativeID, Stream, Model, RequestedAt string
	FirstIndex, Sequence                      int
	Counts                                    tokenCounts
	ContextGrowth                             *int64
	CompactedBefore                           bool
	ToolCalls                                 int

	aliases []string // keys of events merged into this request
}

type toolCommand struct {
	Position                                         int
	Operator, Command, Program, Subcommand, Category string
	ExitCode, DurationMS                             *int64
}

// toolCall is one tool invocation joined to its result.
//
// Token fields describe what the call cost: OutputTokens is the emitting
// request's output split across the calls it made; ResultTokens is what the
// result added to the context ("measured" from the next request's prompt
// growth, or "estimated" from its size); CarriedTokens is ResultTokens times
// the number of later requests that re-read it before compaction.
type toolCall struct {
	Key, CallID, Stream, Kind, CallNativeID, ResultNativeID string
	Sequence                                                int
	ToolName, Category, MCPServer, Model                    string
	Command, Program, Subcommand, CommandCategory, FilePath string
	CommandCount                                            int
	HasPipe, HasRedirect, HasHeredoc, Backgrounded          bool
	StartedAt, EndedAt, DurationSource                      string
	DurationMS                                              *int64
	Status, ErrorType                                       string
	ExitCode                                                *int64
	Interrupted, Truncated                                  bool
	InputBytes, ResultBytes, ResultTokens                   int64
	ResultTokensSource                                      string
	RequestKey, NextRequestKey                              string
	ParallelCount                                           int
	OutputTokens                                            float64
	CarriedRequests, CarriedTokens                          int64
	LinesAdded, LinesRemoved                                *int64
	Commands                                                []toolCommand
	URL, Host, Hosts, SearchQuery                           string
	URLCount                                                int
	URLs                                                    []toolURL

	resultIndex    int
	estimateTokens int64
	commandTexts   []string
}

var (
	exitCodePattern     = regexp.MustCompile(`(?m)^(?:Exit code:? |Process exited with code |exit status )(-?\d+)`)
	wallTimePattern     = regexp.MustCompile(`Wall time:? ([0-9.]+) seconds`)
	jsCommandPattern    = regexp.MustCompile(`\bcmd"?\s*:\s*"((?:[^"\\]|\\.)*)"`)
	jsTemplatePattern   = regexp.MustCompile("\\bcmd\"?\\s*:\\s*`([^`]*)`")
	jsToolPattern       = regexp.MustCompile(`\btools\.([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	jsCommandsPattern   = regexp.MustCompile(`\bcmds\s*=\s*\[((?:[^\]"]|"(?:[^"\\]|\\.)*")*)\]`)
	jsStringPattern     = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	patchFilePattern    = regexp.MustCompile(`(?m)^\*\*\* (?:Update|Add|Delete) File: (.+)$`)
	blockSuffixPattern  = regexp.MustCompile(`(:block:\d+|:usage)$`)
	benignExitPrograms  = map[string]bool{"grep": true, "rg": true, "ag": true, "egrep": true, "fgrep": true, "diff": true, "cmp": true, "test": true, "[": true, "[[": true}
	shellToolNames      = map[string]bool{"bash": true, "shell": true, "exec_command": true, "exec": true, "local_shell": true, "run": true, "container.exec": true, "run_command": true, "execute_command": true}
	toolCategoryByName  = map[string]string{}
	toolCategoryEntries = map[string][]string{
		"command": {"bash", "shell", "exec_command", "exec", "local_shell", "run", "write_stdin", "bashoutput", "killshell", "killbash", "run_command", "execute_command", "container.exec"},
		"read":    {"read", "view_image", "notebookread", "ls", "view", "read_file", "open_file", "list_dir", "list_directory"},
		"search":  {"grep", "glob", "search", "find", "codebase_search", "file_search", "grep_search"},
		"edit":    {"edit", "write", "multiedit", "notebookedit", "apply_patch", "applypatch", "str_replace_editor", "create_file", "edit_file"},
		"web":     {"webfetch", "websearch", "web_search", "web__run", "fetch", "browser", "web.run", "js", "js_reset"},
		"agent":   {"task", "agent", "spawn_agent", "delegate", "workflow", "explore", "send_message", "wait", "wait_agent", "list_agents", "followup_task", "interrupt_agent", "close_agent", "sendmessage", "taskoutput"},
		"plan":    {"todowrite", "todoread", "taskcreate", "taskupdate", "tasklist", "taskget", "taskstop", "update_plan", "enterplanmode", "exitplanmode", "enterworktree", "exitworktree"},
		"user":    {"askuserquestion", "request_user_input", "request_user_input_async"},
		"meta":    {"toolsearch", "skill", "get_skill", "request_plugin_install", "slashcommand"},
	}
)

func init() {
	for category, names := range toolCategoryEntries {
		for _, name := range names {
			toolCategoryByName[name] = category
		}
	}
}

// toolCategory groups tools by what they do across providers.
func toolCategory(name, kind string) (category, server string) {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "mcp__") {
		parts := strings.SplitN(name, "__", 3)
		if len(parts) >= 2 {
			server = parts[1]
		}
		if strings.Contains(lower, "chrome") || strings.Contains(lower, "browser") || strings.Contains(lower, "playwright") {
			return "web", server
		}
		return "mcp", server
	}
	if kind == "delegation" || strings.HasPrefix(lower, "collab__") {
		return "agent", ""
	}
	if category, ok := toolCategoryByName[lower]; ok {
		return category, ""
	}
	return "other", ""
}

// buildToolLedger derives model requests and tool calls from one
// conversation's retained messages. It reads the message envelopes written by
// the adapters and is deterministic, so it can run at ingest or be replayed
// later from stored messages.
func buildToolLedger(messages []MessageRecord, conversationModel string) ([]modelRequest, []toolCall) {
	streams := make([]string, len(messages))
	for index := range streams {
		streams[index] = "main"
	}
	for _, summary := range agentSessionSummaries(messages) {
		for _, index := range summary.messageIndexes {
			if index >= 0 && index < len(streams) {
				streams[index] = summary.nativeID
			}
		}
	}
	requests := ledgerRequests(messages, streams, conversationModel)
	byKey := map[string]*modelRequest{}
	byBase := map[string]string{}
	perStream := map[string][]*modelRequest{}
	for index := range requests {
		request := &requests[index]
		byKey[request.Key] = request
		for _, alias := range request.aliases {
			byKey[alias] = request
		}
		perStream[request.Stream] = append(perStream[request.Stream], request)
	}
	// Map each usage-bearing event to its request so tool calls in the same
	// provider event resolve to the request that produced them.
	for index, message := range messages {
		if key, _ := requestKeyAt(messages, index, streams); key != "" && byKey[key] != nil {
			if base := blockSuffixPattern.ReplaceAllString(message.NativeID, ""); base != "" && byBase[base] == "" {
				byBase[base] = key
			}
		}
	}
	compactions := map[string][]int{}
	for index, message := range messages {
		if isCompactionMarker(message) {
			compactions[streams[index]] = append(compactions[streams[index]], index)
		}
	}

	results := map[string]int{}
	for index, message := range messages {
		if message.Kind != "tool_result" && message.Kind != "delegation_result" {
			continue
		}
		key := defaultString(nilIfEmpty(message.CallID), message.NativeID)
		if _, seen := results[key]; !seen {
			results[key] = index
		}
	}
	calls := []toolCall{}
	seen := map[string]bool{}
	for index, message := range messages {
		if message.Kind != "tool_call" && message.Kind != "delegation" {
			continue
		}
		key := defaultString(nilIfEmpty(message.CallID), message.NativeID)
		if seen[key] {
			continue
		}
		seen[key] = true
		call := toolCall{Key: key, CallID: message.CallID, Stream: streams[index], Kind: message.Kind, CallNativeID: message.NativeID, Sequence: len(calls), StartedAt: message.CreatedAt, Model: defaultString(nilIfEmpty(strings.TrimSpace(message.Model)), conversationModel), resultIndex: -1, ParallelCount: 1}
		var envelope map[string]any
		_ = json.Unmarshal([]byte(message.Text), &envelope)
		call.ToolName = defaultString(envelope["tool"], "tool")
		input := envelope["input"]
		if encoded, err := json.Marshal(input); err == nil && input != nil {
			call.InputBytes = int64(len(encoded))
		}
		if script, ok := input.(string); ok && strings.EqualFold(call.ToolName, "exec") {
			if inner := scriptTool(script); inner != "" && inner != "exec_command" {
				call.ToolName = inner
			}
		}
		call.Category, call.MCPServer = toolCategory(call.ToolName, message.Kind)
		call.FilePath = toolFilePath(call.ToolName, input)
		if resultIndex, ok := results[key]; ok {
			call.resultIndex = resultIndex
		}
		var details map[string]any
		content := ""
		images := 0
		isError := false
		if call.resultIndex >= 0 {
			result := messages[call.resultIndex]
			call.ResultNativeID = result.NativeID
			call.EndedAt = result.CreatedAt
			var payload map[string]any
			_ = json.Unmarshal([]byte(result.Text), &payload)
			if payload == nil {
				content = result.Text
			} else {
				content, images = toolResultText(payload["content"])
				isError = payload["is_error"] == true
				details = mapValue(payload["details"])
			}
			call.ResultBytes = int64(len(content))
			call.estimateTokens = int64(math.Ceil(float64(len(content))/4)) + int64(images)*1500
		}
		applyToolCommands(&call, input, details)
		classifyToolOutcome(&call, content, isError, details)
		applyToolDuration(&call, content, details)
		applyLineCounts(&call, input, details)
		applyToolURLs(&call, input, content)
		// Emitting request: the usage in the same provider event, otherwise the
		// first usage reported at or after the call in its stream.
		base := blockSuffixPattern.ReplaceAllString(message.NativeID, "")
		call.RequestKey = byBase[base]
		if call.RequestKey == "" {
			for _, request := range perStream[call.Stream] {
				if request.FirstIndex >= index {
					call.RequestKey = request.Key
					break
				}
			}
		}
		if request := byKey[call.RequestKey]; request != nil {
			call.RequestKey = request.Key
		}
		calls = append(calls, call)
	}
	emitted := map[string]int{}
	for _, call := range calls {
		if call.RequestKey != "" {
			emitted[call.RequestKey]++
		}
	}
	for index := range calls {
		call := &calls[index]
		if request := byKey[call.RequestKey]; request != nil {
			call.ParallelCount = max(emitted[call.RequestKey], 1)
			call.OutputTokens = request.Counts["output_tokens"] / float64(call.ParallelCount)
			if call.Model == "" {
				call.Model = request.Model
			}
		}
	}
	for key, count := range emitted {
		if request := byKey[key]; request != nil {
			request.ToolCalls = count
		}
	}
	attributeResultTokens(messages, streams, calls, byKey, perStream, compactions)
	return requests, calls
}

// requestKeyAt names the request whose usage message sits at index, if any:
// a stream-scoped key and the provider's request (message or response) ID.
func requestKeyAt(messages []MessageRecord, index int, streams []string) (string, string) {
	message := messages[index]
	if message.Kind != "metadata" && message.RawText == "" {
		return "", ""
	}
	raw, value := tokenObject(message)
	if value == nil || !hasUsageEvidence(raw) {
		return "", ""
	}
	msg := mapValueDefault(value["message"])
	response := mapValueDefault(value["response"])
	id := firstString(msg["id"], response["id"], value["response_id"], value["usage_message_id"])
	if id == "" {
		// Some envelopes (Conductor's Claude events) carry usage without a
		// request ID. Each event stands for itself until mergeAnonymousRequests
		// joins the events of one streamed response.
		if msg["usage"] == nil {
			return "", ""
		}
		return streams[index] + ":event:" + blockSuffixPattern.ReplaceAllString(message.NativeID, ""), ""
	}
	return streams[index] + ":" + id, id
}

// ledgerRequests lists requests in first-appearance order. Providers repeat a
// request's usage while streaming; conversationTokenReports has already kept
// the final numbers on one message per request identity.
func ledgerRequests(messages []MessageRecord, streams []string, conversationModel string) []modelRequest {
	reports := conversationTokenReports(messages)
	first := map[string]int{}
	final := map[string]int{}
	nativeIDs := map[string]string{}
	identified := map[string]bool{}
	for index := range messages {
		key, nativeID := requestKeyAt(messages, index, streams)
		if key == "" {
			continue
		}
		nativeIDs[key] = nativeID
		identified[streams[index]] = true
		if _, ok := first[key]; !ok {
			first[key] = index
		}
		if reports[index].counts != nil && reports[index].scope == "request" {
			final[key] = index
		}
	}
	requests := []modelRequest{}
	add := func(key, nativeID string, firstIndex, finalIndex int) {
		counts := reports[finalIndex].counts
		if counts["total_tokens"] <= 0 {
			return
		}
		model := strings.TrimSpace(messages[finalIndex].Model)
		requests = append(requests, modelRequest{Key: key, NativeID: nativeID, Stream: streams[firstIndex], Model: defaultString(nilIfEmpty(model), conversationModel), RequestedAt: messages[firstIndex].CreatedAt, FirstIndex: firstIndex, Counts: counts})
	}
	for key, finalIndex := range final {
		add(key, nativeIDs[key], first[key], finalIndex)
	}
	// Providers without request identities (Codex token_count deltas) report
	// one thread-scoped delta per request.
	for index, report := range reports {
		if report.scope == "thread" && report.counts != nil && !identified[streams[index]] {
			add(streams[index]+":line:"+strconv.Itoa(index), "", index, index)
		}
	}
	sort.SliceStable(requests, func(i, j int) bool { return requests[i].FirstIndex < requests[j].FirstIndex })
	requests = mergeAnonymousRequests(messages, streams, requests)
	compactions := map[string][]int{}
	for index, message := range messages {
		if isCompactionMarker(message) {
			compactions[streams[index]] = append(compactions[streams[index]], index)
		}
	}
	previous := map[string]*modelRequest{}
	for index := range requests {
		request := &requests[index]
		request.Sequence = index
		if prior := previous[request.Stream]; prior != nil {
			request.CompactedBefore = compactedBetween(compactions[request.Stream], prior.FirstIndex, request.FirstIndex)
			if !request.CompactedBefore {
				growth := int64(request.Counts["input_tokens"] - prior.Counts["input_tokens"] - prior.Counts["output_tokens"])
				request.ContextGrowth = &growth
			}
		}
		previous[request.Stream] = request
	}
	return requests
}

// mergeAnonymousRequests joins consecutive ID-less usage events that belong
// to one streamed response: same stream, the same prompt size, and no tool
// result or user turn between them. The merged request keeps the first
// event's position and the largest reported output.
func mergeAnonymousRequests(messages []MessageRecord, streams []string, requests []modelRequest) []modelRequest {
	merged := make([]modelRequest, 0, len(requests))
	last := map[string]int{}
	aliases := map[string]string{}
	for _, request := range requests {
		position, seen := last[request.Stream]
		if seen && request.NativeID == "" && merged[position].NativeID == "" && strings.Contains(request.Key, ":event:") &&
			merged[position].Counts["input_tokens"] == request.Counts["input_tokens"] && !turnBetween(messages, streams, request.Stream, merged[position].FirstIndex, request.FirstIndex) {
			target := &merged[position]
			if request.Counts["output_tokens"] > target.Counts["output_tokens"] {
				target.Counts = request.Counts
			}
			aliases[request.Key] = target.Key
			continue
		}
		last[request.Stream] = len(merged)
		merged = append(merged, request)
	}
	if len(aliases) > 0 {
		for index := range merged {
			merged[index].aliases = nil
		}
		for alias, key := range aliases {
			for index := range merged {
				if merged[index].Key == key {
					merged[index].aliases = append(merged[index].aliases, alias)
				}
			}
		}
	}
	return merged
}

func turnBetween(messages []MessageRecord, streams []string, stream string, from, to int) bool {
	for index := from + 1; index < to && index < len(messages); index++ {
		message := messages[index]
		if streams[index] == stream && (message.Kind == "tool_result" || message.Kind == "delegation_result" || promptInput(message)) {
			return true
		}
	}
	return false
}

func compactedBetween(markers []int, from, to int) bool {
	for _, marker := range markers {
		if marker > from && marker < to {
			return true
		}
	}
	return false
}

func isCompactionMarker(message MessageRecord) bool {
	if message.Kind != "metadata" {
		return false
	}
	return strings.Contains(message.Text, `"compact_boundary"`) || strings.Contains(message.Text, `"type":"compacted"`) ||
		strings.Contains(message.Text, `"type":"compaction"`) || strings.Contains(message.Text, `"ContextCompaction"`)
}

// attributeResultTokens splits each request's prompt growth across the tool
// results (and other turns) that arrived since the previous request, in
// proportion to their size. Without usable growth the size estimate stands.
func attributeResultTokens(messages []MessageRecord, streams []string, calls []toolCall, byKey map[string]*modelRequest, perStream map[string][]*modelRequest, compactions map[string][]int) {
	type share struct {
		call     *toolCall
		estimate int64
	}
	groups := map[string][]share{}
	next := func(stream string, after int, exclude string) *modelRequest {
		for _, request := range perStream[stream] {
			if request.FirstIndex > after && request.Key != exclude {
				return request
			}
		}
		return nil
	}
	for index := range calls {
		call := &calls[index]
		call.ResultTokens, call.ResultTokensSource = call.estimateTokens, "estimated"
		if call.resultIndex < 0 {
			call.ResultTokens, call.ResultTokensSource = 0, ""
			continue
		}
		stream := streams[call.resultIndex]
		if request := next(stream, call.resultIndex, call.RequestKey); request != nil {
			call.NextRequestKey = request.Key
			groups[request.Key] = append(groups[request.Key], share{call, call.estimateTokens})
		}
		// Every later request in the stream re-reads the result until the
		// context is compacted.
		limit := len(messages)
		for _, marker := range compactions[stream] {
			if marker > call.resultIndex {
				limit = marker
				break
			}
		}
		for _, request := range perStream[stream] {
			if request.FirstIndex > call.resultIndex && request.FirstIndex < limit && request.Key != call.RequestKey {
				call.CarriedRequests++
			}
		}
	}
	for key, members := range groups {
		request := byKey[key]
		if request == nil || request.ContextGrowth == nil || *request.ContextGrowth <= 0 {
			continue
		}
		// Other turns that arrived in the same window take their share too.
		previousIndex := -1
		for _, candidate := range perStream[request.Stream] {
			if candidate.FirstIndex < request.FirstIndex {
				previousIndex = candidate.FirstIndex
			}
		}
		total := int64(0)
		for _, member := range members {
			total += member.estimate
		}
		for index := previousIndex + 1; index < request.FirstIndex && index >= 0; index++ {
			message := messages[index]
			if promptInput(message) && streams[index] == request.Stream {
				total += int64(math.Ceil(float64(len(message.Text)) / 4))
			}
		}
		if total <= 0 {
			continue
		}
		growth := float64(*request.ContextGrowth)
		for _, member := range members {
			member.call.ResultTokens = int64(math.Round(growth * float64(member.estimate) / float64(total)))
			member.call.ResultTokensSource = "measured"
		}
	}
	for index := range calls {
		calls[index].CarriedTokens = calls[index].ResultTokens * calls[index].CarriedRequests
	}
}

// toolResultText returns the text a result put in the context and how many
// images it carried (their base64 payload is not text the model reads).
func toolResultText(content any) (string, int) {
	switch value := content.(type) {
	case string:
		return value, 0
	case []any:
		parts, images := []string{}, 0
		for _, item := range value {
			block := mapValue(item)
			if block == nil {
				if text, ok := item.(string); ok {
					parts = append(parts, text)
				}
				continue
			}
			switch firstString(block["type"]) {
			case "image", "input_image", "image_url":
				images++
			case "text", "input_text", "output_text":
				parts = append(parts, firstString(block["text"]))
			default:
				if text := firstString(block["text"]); text != "" {
					parts = append(parts, text)
				} else if nested, count := toolResultText(block["content"]); nested != "" || count > 0 {
					parts = append(parts, nested)
					images += count
				}
			}
		}
		return strings.Join(parts, "\n"), images
	case map[string]any:
		if text := firstString(value["text"]); text != "" {
			return text, 0
		}
		return toolResultText(value["content"])
	case nil:
		return "", 0
	}
	encoded, _ := json.Marshal(content)
	return string(encoded), 0
}

func toolFilePath(name string, input any) string {
	if fields := mapValue(input); fields != nil {
		if path := firstString(fields["file_path"], fields["notebook_path"], fields["path"], fields["filePath"]); path != "" {
			return clipText(path, 500)
		}
	}
	lower := strings.ToLower(name)
	if lower == "apply_patch" || lower == "applypatch" {
		patch := firstString(input)
		if fields := mapValue(input); fields != nil {
			patch = firstString(fields["patch"], fields["input"])
		}
		if match := patchFilePattern.FindStringSubmatch(patch); len(match) == 2 {
			return clipText(strings.TrimSpace(match[1]), 500)
		}
	}
	return ""
}

// shellCommandText reads the command line from the input shapes providers use.
func shellCommandText(input any) string {
	fields := mapValue(input)
	if fields == nil {
		return ""
	}
	switch command := firstNonNil(fields["command"], fields["cmd"]).(type) {
	case string:
		return command
	case []any:
		words := make([]string, 0, len(command))
		for _, word := range command {
			words = append(words, firstString(word))
		}
		if len(words) >= 3 && (words[1] == "-lc" || words[1] == "-c") && (strings.HasSuffix(words[0], "sh")) {
			return words[len(words)-1]
		}
		return strings.Join(words, " ")
	}
	return ""
}

// applyToolCommands fills the command fields. Codex's exec tool runs a script
// that may start several processes; their executions are attached as details
// by the adapter, or recovered from the script text for older transcripts.
func applyToolCommands(call *toolCall, input any, details map[string]any) {
	lines := []struct {
		text               string
		exitCode, duration *int64
	}{}
	if executions, ok := details["commands"].([]any); ok {
		for _, raw := range executions {
			execution := mapValue(raw)
			if execution == nil {
				continue
			}
			line := struct {
				text               string
				exitCode, duration *int64
			}{text: firstString(execution["command"])}
			if code, ok := number(execution["exit_code"]); ok {
				value := int64(code)
				line.exitCode = &value
			}
			if duration, ok := number(execution["duration_ms"]); ok {
				value := int64(duration)
				line.duration = &value
			}
			if line.text != "" {
				lines = append(lines, line)
			}
		}
	}
	lowerName := strings.ToLower(call.ToolName)
	if len(lines) == 0 && shellToolNames[lowerName] {
		if text := shellCommandText(input); text != "" {
			lines = append(lines, struct {
				text               string
				exitCode, duration *int64
			}{text: text})
		} else if script, ok := input.(string); ok {
			for _, text := range scriptCommands(script) {
				lines = append(lines, struct {
					text               string
					exitCode, duration *int64
				}{text: text})
			}
		}
	}
	if len(lines) == 0 {
		return
	}
	texts := make([]string, len(lines))
	primarySet := false
	for position, line := range lines {
		texts[position] = line.text
		parsed := parseShellCommand(line.text)
		call.HasPipe = call.HasPipe || parsed.HasPipe
		call.HasRedirect = call.HasRedirect || parsed.HasRedirect
		call.HasHeredoc = call.HasHeredoc || parsed.HasHeredoc
		call.Backgrounded = call.Backgrounded || parsed.IsBackgrounded
		if primary := parsed.primary(); !primarySet && primary.Program != "" && (!shellSetupPrograms[primary.Program] || position == len(lines)-1) {
			call.Program, call.Subcommand, call.CommandCategory = primary.Program, primary.Subcommand, primary.Category
			primarySet = true
		}
		for index, segment := range parsed.Segments {
			command := toolCommand{Position: len(call.Commands), Operator: segment.Operator, Command: clipText(segment.Text, 500), Program: segment.Program, Subcommand: segment.Subcommand, Category: segment.Category}
			if index == 0 && position > 0 {
				command.Operator = "script"
			}
			if len(parsed.Segments) == 1 {
				command.ExitCode, command.DurationMS = line.exitCode, line.duration
			}
			call.Commands = append(call.Commands, command)
		}
	}
	call.commandTexts = texts
	call.Command = clipText(strings.Join(texts, "\n"), 2000)
	call.CommandCount = len(call.Commands)
}

// scriptCommands recovers shell commands from a Codex exec script such as
// `await tools.exec_command({cmd:"go test ./..."})` or `const cmds=["a","b"]`.
func scriptCommands(script string) []string {
	commands := []string{}
	decode := func(literal string) string {
		var text string
		if json.Unmarshal([]byte(`"`+literal+`"`), &text) != nil {
			return literal
		}
		return text
	}
	for _, match := range jsCommandsPattern.FindAllStringSubmatch(script, -1) {
		for _, literal := range jsStringPattern.FindAllStringSubmatch(match[1], -1) {
			commands = append(commands, decode(literal[1]))
		}
	}
	for _, match := range jsCommandPattern.FindAllStringSubmatch(script, -1) {
		commands = append(commands, decode(match[1]))
	}
	for _, match := range jsTemplatePattern.FindAllStringSubmatch(script, -1) {
		commands = append(commands, match[1])
	}
	return commands
}

// scriptTool names the tool a Codex exec script calls when it calls only one
// (tools.write_stdin, tools.web__run, ...), so code-mode calls group with the
// direct calls of the same tool.
func scriptTool(script string) string {
	names := map[string]bool{}
	first := ""
	for _, match := range jsToolPattern.FindAllStringSubmatch(script, -1) {
		if !names[match[1]] {
			names[match[1]] = true
			if first == "" {
				first = match[1]
			}
		}
	}
	if len(names) == 1 {
		return first
	}
	return ""
}

// harnessErrorSignatures mark failures the agent harness imposed (a person
// declined, a hook blocked, a timeout). They win over a command's exit code.
// toolErrorSignatures classify the remaining failures. First match wins.
var harnessErrorSignatures = []struct{ kind, needle string }{
	{"user_rejected", "the user doesn't want to proceed"},
	{"user_rejected", "user rejected"},
	{"user_rejected", "was rejected by the user"},
	{"user_rejected", "user denied"},
	{"user_rejected", "permission to use"},
	{"hook_blocked", "pretooluse"},
	{"hook_blocked", "blocked by hook"},
	{"hook_blocked", "hook error"},
	{"hook_blocked", "hook returned"},
	{"interrupted", "interrupted by user"},
	{"interrupted", "[request interrupted"},
	{"timeout", "command timed out"},
	{"timeout", "timed out after"},
}

var toolErrorSignatures = []struct{ kind, needle string }{
	{"file_not_read", "file has not been read yet"},
	{"file_not_read", "read it first"},
	{"edit_no_match", "string to replace not found"},
	{"edit_no_match", "found multiple matches"},
	{"edit_no_match", "old_string"},
	{"file_too_large", "exceeds maximum allowed"},
	{"invalid_input", "inputvalidationerror"},
	{"invalid_input", "input validation"},
	{"permission_denied", "permission denied"},
	{"permission_denied", "operation not permitted"},
	{"permission_denied", "eacces"},
	{"file_not_found", "no such file or directory"},
	{"file_not_found", "file does not exist"},
	{"file_not_found", "enoent"},
	{"file_not_found", "file not found"},
	{"timeout", "timed out"},
}

func classifyToolOutcome(call *toolCall, content string, isError bool, details map[string]any) {
	if call.resultIndex < 0 {
		call.Status = "no_result"
		return
	}
	call.Status = "ok"
	if code, ok := number(firstNonNil(details["exitCode"], details["exit_code"])); ok {
		value := int64(code)
		call.ExitCode = &value
	} else if match := exitCodePattern.FindStringSubmatch(content); len(match) == 2 {
		if code, err := strconv.ParseInt(match[1], 10, 64); err == nil {
			call.ExitCode = &code
		}
	}
	failedCommand := false
	for _, command := range call.Commands {
		if command.ExitCode != nil && *command.ExitCode != 0 && !(*command.ExitCode == 1 && benignExitPrograms[command.Program]) {
			failedCommand = true
			if call.ExitCode == nil {
				value := *command.ExitCode
				call.ExitCode = &value
			}
		}
	}
	status := strings.ToLower(firstString(details["status"]))
	call.Interrupted = details["interrupted"] == true
	if _, ok := number(details["timedOutAfterMs"]); ok {
		call.ErrorType = "timeout"
	}
	persisted := firstString(details["persistedOutputPath"]) != ""
	call.Truncated = persisted || details["truncated"] == true || strings.Contains(content, "<persisted-output>") ||
		strings.Contains(content, "Output truncated") || strings.Contains(content, "tokens truncated") || strings.Contains(content, "[... truncated")
	benign := call.ExitCode != nil && *call.ExitCode == 1 && benignExitPrograms[call.Program] && len(call.Commands) <= 1
	failed := isError || failedCommand || call.Interrupted || call.ErrorType != "" || status == "failed" || status == "declined" || status == "error" ||
		(call.ExitCode != nil && *call.ExitCode != 0 && !benign)
	if benign && !failedCommand && call.ErrorType == "" && !call.Interrupted {
		failed = false
	}
	if !failed {
		return
	}
	call.Status = "error"
	if call.ErrorType != "" {
		return
	}
	if call.Interrupted {
		call.ErrorType = "interrupted"
		return
	}
	head := strings.ToLower(clipText(content, 2000))
	for _, signature := range harnessErrorSignatures {
		if strings.Contains(head, signature.needle) {
			call.ErrorType = signature.kind
			call.Interrupted = call.Interrupted || signature.kind == "interrupted"
			return
		}
	}
	// Shell tools report a failed command as an error without always
	// repeating its exit code.
	if (call.ExitCode != nil && *call.ExitCode != 0) || failedCommand || (isError && call.Category == "command" && call.Program != "") {
		call.ErrorType = "nonzero_exit"
		return
	}
	if status == "declined" {
		call.ErrorType = "user_rejected"
		return
	}
	for _, signature := range toolErrorSignatures {
		if strings.Contains(head, signature.needle) {
			call.ErrorType = signature.kind
			return
		}
	}
	call.ErrorType = "tool_error"
}

func applyToolDuration(call *toolCall, content string, details map[string]any) {
	for _, key := range []string{"durationMs", "duration_ms", "totalDurationMs"} {
		if value, ok := number(details[key]); ok && value >= 0 {
			duration := int64(value)
			call.DurationMS, call.DurationSource = &duration, "reported"
			return
		}
	}
	if match := wallTimePattern.FindStringSubmatch(content); len(match) == 2 {
		if seconds, err := strconv.ParseFloat(match[1], 64); err == nil {
			duration := int64(math.Round(seconds * 1000))
			call.DurationMS, call.DurationSource = &duration, "reported"
			return
		}
	}
	if call.StartedAt == "" || call.EndedAt == "" {
		return
	}
	start, startOK := parseTime(call.StartedAt)
	end, endOK := parseTime(call.EndedAt)
	if startOK && endOK && !end.Before(start) {
		duration := end.Sub(start).Milliseconds()
		call.DurationMS, call.DurationSource = &duration, "timestamps"
	}
}

// applyLineCounts counts changed lines from a structured patch (Claude edit
// results) or a patch input (apply_patch).
func applyLineCounts(call *toolCall, input any, details map[string]any) {
	added, removed, found := int64(0), int64(0), false
	if added64, ok := number(details["lines_added"]); ok {
		removed64, _ := number(details["lines_removed"])
		added, removed, found = int64(added64), int64(removed64), true
	} else if hunks, ok := details["structuredPatch"].([]any); ok {
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
		found = true
	} else if lower := strings.ToLower(call.ToolName); lower == "apply_patch" || lower == "applypatch" {
		patch := firstString(input)
		if fields := mapValue(input); fields != nil {
			patch = firstString(fields["patch"], fields["input"])
		}
		for _, line := range strings.Split(patch, "\n") {
			if strings.HasPrefix(line, "***") {
				continue
			}
			if strings.HasPrefix(line, "+") {
				added++
			} else if strings.HasPrefix(line, "-") {
				removed++
			}
		}
		found = patch != ""
	}
	if found {
		call.LinesAdded, call.LinesRemoved = &added, &removed
	}
}
