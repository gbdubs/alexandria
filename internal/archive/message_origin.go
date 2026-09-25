package archive

import (
	"fmt"
	"strings"
)

// Provider transcripts record several kinds of input under the "user" role:
// what a person typed, text the harness injected into the prompt (image
// captions, skill bodies, environment context, notifications), and prompts a
// parent agent sent a sub-agent. Only the first is a human turn. Injected text
// is kept as a "context" event so it still counts toward prompt size, and
// sub-agent prompts use the "agent" role so they stay readable without
// inflating human turn counts.

const (
	contextKind = "context"
	agentRole   = "agent"
)

// injectedContext builds the event stored for harness-injected prompt text.
func injectedContext(base MessageRecord, source, text string) MessageRecord {
	base.Role = "system"
	base.Kind = contextKind
	base.Text = jsonText(map[string]any{"type": "injected_context", "source": source, "text": text})
	return base
}

// promptInput reports whether a message is input that grew the model's
// prompt without being a tool result.
func promptInput(message MessageRecord) bool {
	return message.Kind == contextKind || (message.Kind == "message" && (message.Role == "user" || message.Role == agentRole))
}

// claudeInjectedSource names the harness source of a Claude "user" event, or
// returns "" when a person (or, in sub-agent transcripts, the parent agent)
// wrote it. Claude Code flags most injected events; the tag checks cover
// events it writes without a flag.
func claudeInjectedSource(event map[string]any, text string) string {
	trimmed := strings.TrimSpace(text)
	switch firstString(mapValue(event["origin"])["kind"]) {
	case "task-notification":
		return "task_notification"
	case "peer":
		return "peer_message"
	}
	if event["isCompactSummary"] == true {
		return "compaction_summary"
	}
	if event["isMeta"] == true {
		switch {
		case strings.HasPrefix(trimmed, "[Image:"):
			return "image_note"
		case strings.HasPrefix(trimmed, "Base directory for this skill"):
			return "skill"
		case strings.HasPrefix(trimmed, "<local-command-caveat>"):
			return "command_caveat"
		}
		return "harness"
	}
	switch {
	case strings.HasPrefix(trimmed, "<task-notification>"):
		return "task_notification"
	case strings.HasPrefix(trimmed, "<local-command-stdout>"), strings.HasPrefix(trimmed, "<local-command-stderr>"):
		return "command_output"
	case strings.HasPrefix(trimmed, "[Request interrupted by user"):
		return "interruption"
	}
	return ""
}

// codexInjectedSource names the harness source of one Codex user content
// block. Recent Codex versions tag each block's kind; older ones only wrap
// injected text in known tags.
func codexInjectedSource(itemKind, text string) string {
	if itemKind != "" {
		if strings.HasPrefix(itemKind, "user.") {
			return ""
		}
		return itemKind
	}
	trimmed := strings.TrimSpace(text)
	for _, known := range [][2]string{
		{"<environment_context>", "environments.environment_context"},
		{"<recommended_plugins>", "plugins.recommendations"},
		{"<turn_aborted>", "turn_aborted"},
		{"<user_instructions>", "user_instructions"},
		{"# AGENTS.md instructions", "user_instructions"},
	} {
		if strings.HasPrefix(trimmed, known[0]) {
			return known[1]
		}
	}
	if strings.HasPrefix(trimmed, "Warning: apply_patch was requested via exec_command") {
		return "harness_warning"
	}
	return ""
}

// codexSender names the automation that sent a session's user-role input, or
// returns "" for interactive sessions. `codex exec` runs are scripted.
func codexSender(meta map[string]any) string {
	if firstString(meta["originator"]) == "codex_exec" || firstString(meta["source"]) == "exec" {
		return "automation:codex-exec"
	}
	return ""
}

// claudeSender names the automation that sent a Claude user event, or returns
// "" for interactive input. Headless `claude -p` runs report the sdk-cli
// entrypoint; Conductor and the desktop app use others.
func claudeSender(event map[string]any) string {
	if firstString(event["entrypoint"]) == "sdk-cli" {
		return "automation:claude-sdk-cli"
	}
	return ""
}

// codexSubagent reports whether session metadata describes a spawned agent,
// whose user-role input comes from its parent agent.
func codexSubagent(meta map[string]any) bool {
	spawn := mapValue(mapValue(mapValue(meta["source"])["subagent"])["thread_spawn"])
	return firstString(meta["parent_thread_id"], spawn["parent_thread_id"]) != ""
}

// codexUserMessages splits a Codex user message into injected context blocks
// and the text a person (or parent agent) wrote, keeping block order.
func codexUserMessages(base MessageRecord, payload map[string]any, fromAgent bool) []MessageRecord {
	blocks, ok := payload["content"].([]any)
	if !ok {
		blocks = []any{payload["content"]}
	}
	kinds, _ := mapValue(payload["internal_chat_message_metadata_passthrough"])["content_item_kinds"].([]any)
	output, human, humanAt := []MessageRecord{}, []string{}, -1
	for index, raw := range blocks {
		text := messageText([]any{raw})
		if text == "" {
			continue
		}
		itemKind := ""
		if len(kinds) == len(blocks) {
			itemKind = firstString(kinds[index])
		} else if len(kinds) == 1 {
			itemKind = firstString(kinds[0])
		}
		if source := codexInjectedSource(itemKind, text); source != "" {
			context := injectedContext(base, source, text)
			context.NativeID = fmt.Sprintf("%s:block:%d", base.NativeID, index)
			output = append(output, context)
			continue
		}
		if humanAt < 0 {
			humanAt = len(output)
		}
		human = append(human, text)
	}
	images := imageAttachments(payload["content"])
	if len(human) == 0 && len(images) > 0 {
		human, humanAt = []string{"(image attachment)"}, len(output)
	}
	role := "user"
	if fromAgent {
		role = agentRole
	}
	if len(human) > 0 {
		item := base
		item.Role, item.Kind, item.Text = role, "message", strings.Join(human, "\n")
		records := []MessageRecord{item}
		if len(images) > 0 {
			attachment := attachmentRecord(base, role, images)
			attachment.NativeID = base.NativeID + ":attachments"
			records = append(records, attachment)
		}
		output = append(output[:humanAt], append(records, output[humanAt:]...)...)
	}
	// Only one record may carry the raw event, or its usage would count twice.
	for index := range output {
		if index > 0 {
			output[index].RawText = ""
		}
	}
	return output
}

// imageAttachments counts image blocks placed directly in message content,
// which is how user-attached images arrive. Images captured by tools live
// inside tool results and are never counted here.
func imageAttachments(content any) []string {
	blocks, _ := content.([]any)
	types := []string{}
	for _, raw := range blocks {
		block := mapValue(raw)
		switch firstString(block["type"]) {
		case "image":
			types = append(types, defaultString(mapValue(block["source"])["media_type"], "image"))
		case "input_image", "image_url":
			types = append(types, "image")
		}
	}
	return types
}

// attachmentRecord describes user-attached images without their payload.
func attachmentRecord(base MessageRecord, role string, types []string) MessageRecord {
	base.Role = role
	base.Kind = "attachment"
	base.Text = jsonText(map[string]any{"type": "image_attachment", "count": len(types), "media_types": types})
	return base
}
