package archive

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type tokenCounts map[string]float64

type tokenReport struct {
	counts, checkpoint, reported tokenCounts
	usage                        map[string]any
	scope, stream                string
	start                        int
}

func tokenNumber(values ...any) (float64, bool) {
	for _, v := range values {
		if n, ok := number(v); ok && n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) {
			return n, true
		}
	}
	return 0, false
}

// Canonical input includes cache reads/writes. Claude's base input excludes
// both; OpenAI input already includes them. Detail fields are subsets.
func normalizeTokenCounts(u map[string]any) tokenCounts {
	if u == nil {
		return nil
	}
	out := tokenCounts{}
	put := func(key string, values ...any) {
		if n, ok := tokenNumber(values...); ok {
			out[key] = n
		}
	}
	inputDetails := mapValueDefault(firstNonNil(u["input_tokens_details"], u["prompt_tokens_details"]))
	outputDetails := mapValueDefault(firstNonNil(u["output_tokens_details"], u["completion_tokens_details"]))
	put("input_tokens", u["input_tokens"], u["prompt_tokens"], u["inputTokens"])
	put("output_tokens", u["output_tokens"], u["completion_tokens"], u["outputTokens"])
	put("cache_read_input_tokens", u["cache_read_input_tokens"], u["cacheReadInputTokens"], u["cached_input_tokens"], inputDetails["cached_tokens"])
	put("cache_creation_input_tokens", u["cache_creation_input_tokens"], u["cacheCreationInputTokens"], u["cache_write_input_tokens"], inputDetails["cache_write_tokens"])
	if cache := mapValue(u["cache_creation"]); cache != nil {
		put("cache_creation_5m_input_tokens", cache["ephemeral_5m_input_tokens"], cache["5m_input_tokens"])
		put("cache_creation_1h_input_tokens", cache["ephemeral_1h_input_tokens"], cache["1h_input_tokens"])
		if _, ok := out["cache_creation_input_tokens"]; !ok {
			for _, v := range cache {
				if n, valid := tokenNumber(v); valid {
					out["cache_creation_input_tokens"] += n
				}
			}
		}
	}
	put("reasoning_output_tokens", u["reasoning_output_tokens"], u["thinking_tokens"], outputDetails["reasoning_tokens"], outputDetails["thinking_tokens"])
	separate := u["cache_read_input_tokens"] != nil || u["cache_creation_input_tokens"] != nil || u["cacheReadInputTokens"] != nil || u["cacheCreationInputTokens"] != nil || u["cache_creation"] != nil
	if separate {
		if n, ok := out["input_tokens"]; ok {
			out["uncached_input_tokens"] = n
		}
		out["input_tokens"] += out["cache_read_input_tokens"] + out["cache_creation_input_tokens"]
	} else if input, ok := out["input_tokens"]; ok {
		if _, hasRead := out["cache_read_input_tokens"]; hasRead {
			out["uncached_input_tokens"] = math.Max(0, input-out["cache_read_input_tokens"]-out["cache_creation_input_tokens"])
		} else if _, hasWrite := out["cache_creation_input_tokens"]; hasWrite {
			out["uncached_input_tokens"] = math.Max(0, input-out["cache_creation_input_tokens"])
		}
	}
	put("total_tokens", u["total_tokens"], u["totalTokens"])
	if _, ok := out["total_tokens"]; !ok {
		_, input := out["input_tokens"]
		_, output := out["output_tokens"]
		if input || output {
			out["total_tokens"] = out["input_tokens"] + out["output_tokens"]
		}
	}
	if _, ok := out["total_tokens"]; !ok {
		return nil
	}
	return out
}
func tokenObject(m MessageRecord) (map[string]any, map[string]any) {
	var raw map[string]any
	decoder := json.NewDecoder(strings.NewReader(defaultString(m.RawText, m.Text)))
	decoder.UseNumber()
	if decoder.Decode(&raw) != nil {
		return nil, nil
	}
	value := raw
	for depth := 0; depth < 5; depth++ {
		var next map[string]any
		for _, key := range []string{"payload", "event", "msg", "data"} {
			if next = mapValue(value[key]); next != nil {
				break
			}
		}
		if next == nil {
			break
		}
		value = next
	}
	return raw, value
}
func addTokenCounts(to, from tokenCounts) {
	for k, v := range from {
		to[k] += v
	}
}
func tokenRemainder(total, attributed tokenCounts) tokenCounts {
	out := tokenCounts{}
	for k, v := range total {
		out[k] = math.Max(0, v-attributed[k])
	}
	return out
}

// The same request can appear as stream deltas, a final message, an SDK run
// result and a thread checkpoint. Resolve identities first, then use aggregates
// only to fill gaps. Keep all originals in raw_text for independent inspection.
func tokenStreamAliases(messages []MessageRecord) map[string]string {
	aliases := map[string]string{"main": "main"}
	for _, message := range messages {
		if message.Kind != "delegation" {
			continue
		}
		child := defaultString(nilIfEmpty(message.CallID), message.NativeID)
		if child != "" {
			aliases[child] = child
			if message.NativeID != "" {
				aliases[message.NativeID] = child
			}
		}
	}
	return aliases
}

func tokenStreamParents(messages []MessageRecord) map[string]string {
	parents, aliases := map[string]string{}, tokenStreamAliases(messages)
	for _, message := range messages {
		if message.Kind != "delegation" {
			continue
		}
		child := defaultString(nilIfEmpty(message.CallID), message.NativeID)
		parent := aliases[message.ParentNativeID]
		if parent == "" {
			parent = defaultString(nilIfEmpty(message.ParentNativeID), "main")
		}
		if child != "" && child != parent {
			parents[child] = parent
		}
	}
	return parents
}

func streamDescendsFrom(stream, ancestor string, parents map[string]string) bool {
	if stream == ancestor {
		return true
	}
	seen := map[string]bool{}
	for stream != "" && stream != "main" && !seen[stream] {
		seen[stream] = true
		stream = parents[stream]
		if stream == ancestor {
			return true
		}
	}
	return false
}

func streamDepth(stream string, parents map[string]string) int {
	depth, seen := 0, map[string]bool{}
	for stream != "" && stream != "main" && !seen[stream] {
		seen[stream] = true
		stream = parents[stream]
		depth++
	}
	return depth
}

func conversationTokenReports(messages []MessageRecord) []tokenReport {
	reports := make([]tokenReport, len(messages))
	streamAliases := tokenStreamAliases(messages)
	seen := map[string]int{}
	active := map[string]string{}
	runs := map[string]int{}
	cumulative := map[string]tokenCounts{}
	modelCounters := map[string]tokenCounts{}
	resultIDs := map[string]bool{}
	recordStreams := map[string]bool{}
	for _, m := range messages {
		raw, v := tokenObject(m)
		if firstString(raw["type"], v["type"]) == "token_usage_record" {
			stream := defaultString(firstNonNil(v["agent_id"], raw["parent_tool_use_id"], nilIfEmpty(m.ParentNativeID)), "main")
			stream = defaultString(streamAliases[stream], stream)
			recordStreams[stream] = true
		}
	}
	for i, m := range messages {
		raw, v := tokenObject(m)
		if v == nil {
			continue
		}
		typ := firstString(v["type"], raw["type"])
		stream := defaultString(firstNonNil(v["agent_id"], raw["parent_tool_use_id"], nilIfEmpty(m.ParentNativeID)), "main")
		stream = defaultString(streamAliases[stream], stream)
		hasRecords := recordStreams[stream]
		msg := mapValueDefault(v["message"])
		response := mapValueDefault(v["response"])
		metadata := mapValueDefault(v["metadata"])
		u := mapValue(firstNonNil(msg["usage"], response["usage"], v["usage"], metadata["usage"]))
		counts := normalizeTokenCounts(u)
		id := firstString(msg["id"], response["id"], v["response_id"], v["usage_message_id"])
		if id == "" && (matchesFold(typ, "message", "response") || firstString(v["object"]) == "response") {
			id = firstString(v["id"])
		}
		if typ == "message_start" {
			active[stream] = id
		}
		if typ == "message_delta" && id == "" {
			id = active[stream]
			if id == "" {
				id = fmt.Sprintf("unidentified-stream-%d", i)
			}
			active[stream] = id
		}
		if typ == "message_stop" {
			delete(active, stream)
		}
		r := tokenReport{counts: counts, usage: u, scope: "request", stream: stream}
		if matchesFold(typ, "result", "turn.completed", "cost-state") {
			if resultID := firstString(v["uuid"], v["id"], raw["uuid"]); resultID != "" {
				key := stream + ":" + typ + ":" + resultID
				if resultIDs[key] {
					continue
				}
				resultIDs[key] = true
			}
		}
		info := mapValueDefault(v["info"])
		if snapshot := mapValue(v["latest_token_usage_record"]); snapshot != nil && !hasRecords {
			info = map[string]any{"total_token_usage": snapshot["thread_token_usage"], "last_token_usage": snapshot["usage"]}
		}
		if running := normalizeTokenCounts(mapValue(info["total_token_usage"])); running != nil {
			r.scope = "thread"
			r.counts = nil
			r.reported = running
			if !hasRecords {
				if previous := cumulative[stream]; previous != nil && running["total_tokens"] < previous["total_tokens"] {
					r.counts = normalizeTokenCounts(mapValue(info["last_token_usage"]))
				} else {
					r.counts = tokenRemainder(running, cumulative[stream])
				}
				cumulative[stream] = running
			}
		} else if typ == "token_count" {
			if !hasRecords {
				r.counts = normalizeTokenCounts(mapValue(info["last_token_usage"]))
			}
			r.scope = "thread"
		} else if matchesFold(firstString(v["subtype"]), "task_progress", "task_notification") {
			r.scope = "task"
			r.stream = defaultString(firstNonNil(v["tool_use_id"], v["task_id"], raw["parent_tool_use_id"]), stream)
			r.stream = defaultString(streamAliases[r.stream], r.stream)
			id = "task:" + r.stream
		} else if matchesFold(typ, "result", "turn.completed", "cost-state") {
			r.scope = "run"
			r.start = runs[stream]
			runs[stream] = i + 1
			if typ == "cost-state" {
				r.scope = "session"
			}
			models := tokenCounts{}
			for model, usage := range mapValueDefault(v["modelUsage"]) {
				key := stream + ":" + firstString(raw["session_id"], v["session_id"]) + ":" + model
				current := normalizeTokenCounts(mapValue(usage))
				previous := modelCounters[key]
				if current["total_tokens"] < previous["total_tokens"] {
					addTokenCounts(models, current)
				} else {
					addTokenCounts(models, tokenRemainder(current, previous))
				}
				modelCounters[key] = current
			}
			if models["total_tokens"] > r.counts["total_tokens"] {
				r.counts = models
			}
		} else {
			r.checkpoint = normalizeTokenCounts(mapValue(v["thread_token_usage"]))
		}
		if id != "" && (r.scope == "request" || r.scope == "task") {
			key := stream + ":" + id
			if previous, ok := seen[key]; ok {
				merged := tokenCounts{}
				for k, n := range reports[previous].counts {
					merged[k] = n
				}
				for k, n := range r.counts {
					merged[k] = n
				}
				if r.scope == "request" {
					usage := map[string]any{}
					for k, v := range reports[previous].usage {
						usage[k] = v
					}
					for k, v := range r.usage {
						usage[k] = v
					}
					r.usage = usage
					merged = normalizeTokenCounts(usage)
				}
				r.counts = merged
				reports[previous].counts = nil
				reports[previous].checkpoint = nil
			}
			seen[key] = i
		}
		reports[i] = r
	}
	requests := map[string]tokenCounts{}
	for i, r := range reports {
		if r.scope != "request" || r.counts == nil {
			continue
		}
		if requests[r.stream] == nil {
			requests[r.stream] = tokenCounts{}
		}
		addTokenCounts(requests[r.stream], r.counts)
		gap := tokenRemainder(r.checkpoint, requests[r.stream])
		addTokenCounts(r.counts, gap)
		addTokenCounts(requests[r.stream], gap)
		reports[i] = r
	}
	if len(recordStreams) > 0 {
		latest := map[string]int{}
		for i, r := range reports {
			if r.scope == "thread" && r.reported != nil && recordStreams[r.stream] {
				latest[r.stream] = i
			}
		}
		for stream, i := range latest {
			reports[i].counts = tokenRemainder(reports[i].reported, requests[stream])
		}
	}
	parents := tokenStreamParents(messages)
	tasks := []int{}
	for i, r := range reports {
		if r.scope == "task" && r.counts != nil {
			tasks = append(tasks, i)
		}
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		return streamDepth(reports[tasks[i]].stream, parents) > streamDepth(reports[tasks[j]].stream, parents)
	})
	for _, index := range tasks {
		r := reports[index]
		attributed := tokenCounts{}
		for childIndex, child := range reports {
			if childIndex == index || child.counts == nil || (child.scope != "request" && child.scope != "task") {
				continue
			}
			if streamDescendsFrom(child.stream, r.stream, parents) {
				addTokenCounts(attributed, child.counts)
			}
		}
		reports[index].counts = tokenRemainder(r.counts, attributed)
	}
	for i, r := range reports {
		if r.counts == nil {
			continue
		}
		if r.scope == "run" || r.scope == "session" {
			attributed := tokenCounts{}
			for _, prior := range reports[r.start:i] {
				if r.stream == "main" || prior.stream == r.stream {
					addTokenCounts(attributed, prior.counts)
				}
			}
			r.counts = tokenRemainder(r.counts, attributed)
		}
		reports[i] = r
	}
	return reports
}

func conversationTokenCounts(messages []MessageRecord) tokenCounts {
	totals := tokenCounts{}
	for _, report := range conversationTokenReports(messages) {
		addTokenCounts(totals, report.counts)
	}
	return totals
}

type agentSessionSummary struct {
	nativeID, parentNativeID string
	delegationMessageIndex   int
	depth                    int
	messageIndexes           []int
	counts                   tokenCounts
	usage                    map[usageBucket]tokenCounts
	startedAt, endedAt       string
}

// usageBucket places reconciled usage on the timeline at the UTC hour of the
// accounting event that reported it. Events without a timestamp are placed at
// the session start and marked, so trends never silently invent precision.
type usageBucket struct {
	hour, model, attribution string
}

func usageHour(timestamp string) string {
	parsed, ok := parseTime(timestamp)
	if !ok {
		return ""
	}
	return parsed.UTC().Truncate(time.Hour).Format(time.RFC3339)
}

func (s *agentSessionSummary) addUsage(message MessageRecord, counts tokenCounts) {
	bucket := usageBucket{hour: usageHour(message.CreatedAt), model: strings.TrimSpace(message.Model), attribution: "request"}
	if bucket.hour == "" {
		bucket.attribution = "session-start"
	}
	if s.usage[bucket] == nil {
		s.usage[bucket] = tokenCounts{}
	}
	addTokenCounts(s.usage[bucket], counts)
}

func agentSessionSummaries(messages []MessageRecord) []agentSessionSummary {
	root := &agentSessionSummary{nativeID: "main", delegationMessageIndex: -1, counts: tokenCounts{}, usage: map[usageBucket]tokenCounts{}}
	sessions := map[string]*agentSessionSummary{"main": root}
	aliases := tokenStreamAliases(messages)
	order := []string{"main"}
	messageSessions := make([]string, len(messages))
	// Materialize every delegation before assigning messages so nested children
	// remain queryable even when a provider emits their records out of order.
	for index, message := range messages {
		if message.Kind != "delegation" {
			continue
		}
		key := defaultString(nilIfEmpty(message.CallID), message.NativeID)
		if key == "" || sessions[key] != nil {
			continue
		}
		sessions[key] = &agentSessionSummary{nativeID: key, delegationMessageIndex: index, counts: tokenCounts{}, usage: map[usageBucket]tokenCounts{}}
		order = append(order, key)
	}
	for index, message := range messages {
		key := "main"
		if canonical := aliases[message.ParentNativeID]; sessions[canonical] != nil {
			key = canonical
		}
		messageSessions[index] = key
		session := sessions[key]
		session.messageIndexes = append(session.messageIndexes, index)
		if message.CreatedAt != "" {
			if session.startedAt == "" || message.CreatedAt < session.startedAt {
				session.startedAt = message.CreatedAt
			}
			if session.endedAt == "" || message.CreatedAt > session.endedAt {
				session.endedAt = message.CreatedAt
			}
		}
		if message.Kind == "delegation" {
			child := defaultString(nilIfEmpty(message.CallID), message.NativeID)
			if sessions[child] != nil {
				sessions[child].parentNativeID = key
			}
		}
	}
	for index, report := range conversationTokenReports(messages) {
		if report.counts == nil {
			continue
		}
		key := report.stream
		key = defaultString(aliases[key], key)
		if key == "" || key == "main" {
			key = messageSessions[index]
		}
		if sessions[key] == nil {
			parent := messageSessions[index]
			if parent == key || parent == "" {
				parent = "main"
			}
			sessions[key] = &agentSessionSummary{nativeID: key, parentNativeID: parent, delegationMessageIndex: -1, counts: tokenCounts{}, usage: map[usageBucket]tokenCounts{}}
			order = append(order, key)
		}
		if prior := messageSessions[index]; key != prior {
			kept := sessions[prior].messageIndexes[:0]
			for _, messageIndex := range sessions[prior].messageIndexes {
				if messageIndex != index {
					kept = append(kept, messageIndex)
				}
			}
			sessions[prior].messageIndexes = kept
			sessions[key].messageIndexes = append(sessions[key].messageIndexes, index)
			messageSessions[index] = key
			if timestamp := messages[index].CreatedAt; timestamp != "" {
				if sessions[key].startedAt == "" || timestamp < sessions[key].startedAt {
					sessions[key].startedAt = timestamp
				}
				if sessions[key].endedAt == "" || timestamp > sessions[key].endedAt {
					sessions[key].endedAt = timestamp
				}
			}
		}
		addTokenCounts(sessions[key].counts, report.counts)
		sessions[key].addUsage(messages[index], report.counts)
	}
	for _, session := range sessions {
		session.startedAt, session.endedAt = "", ""
		for _, index := range session.messageIndexes {
			timestamp := messages[index].CreatedAt
			if timestamp != "" && (session.startedAt == "" || timestamp < session.startedAt) {
				session.startedAt = timestamp
			}
			if timestamp != "" && (session.endedAt == "" || timestamp > session.endedAt) {
				session.endedAt = timestamp
			}
		}
		start := usageHour(defaultString(session.startedAt, session.endedAt))
		for bucket, counts := range session.usage {
			if bucket.hour != "" || start == "" {
				continue
			}
			delete(session.usage, bucket)
			bucket.hour = start
			if session.usage[bucket] == nil {
				session.usage[bucket] = tokenCounts{}
			}
			addTokenCounts(session.usage[bucket], counts)
		}
	}
	depth := func(key string) int {
		value, seen := 0, map[string]bool{}
		for key != "" && key != "main" && !seen[key] {
			seen[key] = true
			key = sessions[key].parentNativeID
			value++
		}
		return value
	}
	result := make([]agentSessionSummary, 0, len(order))
	for _, key := range order {
		sessions[key].depth = depth(key)
		result = append(result, *sessions[key])
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].depth < result[j].depth })
	return result
}

func reconciledTokenMetrics(record WorkspaceRecord) []map[string]any {
	totals := tokenCounts{}
	for _, c := range record.Conversations {
		addTokenCounts(totals, conversationTokenCounts(c.Messages))
	}
	if _, ok := totals["total_tokens"]; ok {
		if unclassified := math.Max(0, totals["total_tokens"]-totals["input_tokens"]-totals["output_tokens"]); unclassified > 0 {
			totals["unclassified_tokens"] = unclassified
		}
	}
	rows := []map[string]any{}
	for name, value := range totals {
		rows = append(rows, map[string]any{"name": name, "value": value, "unit": "tokens", "status": "reported-reconciled", "extractor_version": "tokens-v2", "definition": "Retained request usage plus unallocated aggregate remainder. Input includes cached input; cache and reasoning are subsets. Raw accounting evidence retained on messages."})
	}
	sort.Slice(rows, func(i, j int) bool { return firstString(rows[i]["name"]) < firstString(rows[j]["name"]) })
	return rows
}

func accountingRaw(event map[string]any) string {
	if raw := firstString(event["raw_text"]); raw != "" {
		return raw
	}
	if hasUsageEvidence(event) {
		return jsonText(event)
	}
	return ""
}

// Only inspect protocol envelopes, never tool arguments, output, or prose.
// Retain the entire original accounting object so new dimensions remain
// inspectable even when they do not contribute to the token total.
func hasUsageEvidence(event map[string]any) bool {
	for _, key := range []string{"usage", "modelUsage", "total_token_usage", "last_token_usage", "thread_token_usage", "turn_token_usage", "latest_token_usage_record", "estimated_tokens", "estimated_tokens_delta"} {
		if event[key] != nil {
			return true
		}
	}
	for _, key := range []string{"payload", "info", "event", "msg", "message", "response", "metadata"} {
		if value := mapValue(event[key]); value != nil && hasUsageEvidence(value) {
			return true
		}
	}
	return false
}
