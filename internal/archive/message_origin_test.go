package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseJSONL(t *testing.T, provider, path string, lines ...string) []MessageRecord {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, ok, err := (&jsonlAdapter{provider: provider}).parse(path)
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	return record.Conversations[0].Messages
}

func byNativeID(messages []MessageRecord) map[string]MessageRecord {
	result := map[string]MessageRecord{}
	for _, message := range messages {
		result[message.NativeID] = message
	}
	return result
}

func humanTurns(messages []MessageRecord) []string {
	turns := []string{}
	for _, message := range messages {
		if message.Role == "user" && message.Kind == "message" {
			turns = append(turns, message.Text)
		}
	}
	return turns
}

func TestClaudeSeparatesInjectedInputFromHumanTurns(t *testing.T) {
	messages := parseJSONL(t, "claude", filepath.Join(t.TempDir(), "session.jsonl"),
		`{"sessionId":"s","uuid":"prompt","type":"user","message":{"role":"user","content":"Show me the header"}}`,
		`{"sessionId":"s","uuid":"read","parentUuid":"prompt","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_read","name":"Read","input":{"file_path":"/tmp/header.png"}}]}}`,
		`{"sessionId":"s","uuid":"result","parentUuid":"read","type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_read","type":"tool_result","content":[{"type":"image","source":{"type":"base64","data":"AAAA","media_type":"image/png"}}]}]},"sourceToolAssistantUUID":"read"}`,
		`{"sessionId":"s","uuid":"caption","parentUuid":"result","type":"user","message":{"role":"user","content":"[Image: original 3414x1894, displayed at 2000x1110. Multiply coordinates by 1.71 to map to original image.]"},"isMeta":true,"turnCompanion":true}`,
		`{"sessionId":"s","uuid":"skill","type":"user","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /skills/run"}]},"isMeta":true}`,
		`{"sessionId":"s","uuid":"notice","type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>1</task-id></task-notification>"},"origin":{"kind":"task-notification"}}`,
		`{"sessionId":"s","uuid":"summary","type":"user","message":{"role":"user","content":"This session is being continued from a previous conversation."},"isCompactSummary":true,"isVisibleInTranscriptOnly":true}`,
		`{"sessionId":"s","uuid":"stdout","type":"user","message":{"role":"user","content":"<local-command-stdout>Compacted</local-command-stdout>"}}`,
		`{"sessionId":"s","uuid":"pasted","type":"user","message":{"role":"user","content":[{"type":"text","text":"What about this one?"},{"type":"image","source":{"type":"base64","data":"AAAA","media_type":"image/jpeg"}}]}}`,
		`{"sessionId":"s","uuid":"only-image","type":"user","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"AAAA","media_type":"image/png"}}]}}`,
	)
	if turns := humanTurns(messages); strings.Join(turns, "|") != "Show me the header|What about this one?|(image attachment)" {
		t.Fatalf("human turns = %q", turns)
	}
	found := byNativeID(messages)
	for id, source := range map[string]string{"caption": "image_note", "skill": "skill", "notice": "task_notification", "summary": "compaction_summary", "stdout": "command_output"} {
		message := found[id]
		if message.Role != "system" || message.Kind != contextKind || !strings.Contains(message.Text, `"source":"`+source+`"`) {
			t.Fatalf("%s = %#v, want %s context", id, message, source)
		}
	}
	if found["caption"].CallID != "toolu_read" {
		t.Fatalf("image caption is not linked to its tool call: %#v", found["caption"])
	}
	if attachment := found["pasted:attachments"]; attachment.Role != "user" || attachment.Kind != "attachment" || !strings.Contains(attachment.Text, "image/jpeg") || strings.Contains(attachment.Text, "AAAA") {
		t.Fatalf("user attachment = %#v", attachment)
	}
	if purpose, _ := agentPurposeOutcome(messages); purpose != "Show me the header" {
		t.Fatalf("purpose = %q", purpose)
	}
}

func TestClaudeSubagentPromptComesFromAgent(t *testing.T) {
	root := t.TempDir()
	messages := parseJSONL(t, "claude", filepath.Join(root, "root-session", "subagents", "agent-1.jsonl"),
		`{"sessionId":"root-session","uuid":"brief","type":"user","message":{"role":"user","content":"Audit the parser"}}`,
		`{"sessionId":"root-session","uuid":"answer","type":"assistant","message":{"role":"assistant","content":"Audited"}}`,
	)
	if brief := byNativeID(messages)["brief"]; brief.Role != agentRole || brief.Kind != "message" {
		t.Fatalf("sub-agent prompt = %#v", brief)
	}
	if purpose, _ := agentPurposeOutcome(messages); purpose != "Audit the parser" {
		t.Fatalf("sub-agent purpose = %q", purpose)
	}
}

func TestCodexSeparatesInjectedContextFromHumanTurns(t *testing.T) {
	messages := parseJSONL(t, "codex", filepath.Join(t.TempDir(), "rollout.jsonl"),
		`{"type":"session_meta","payload":{"id":"root"}}`,
		`{"type":"response_item","payload":{"type":"message","id":"tagged","role":"user","content":[{"type":"input_text","text":"<recommended_plugins>x</recommended_plugins>"},{"type":"input_text","text":"<environment_context><cwd>/w</cwd></environment_context>"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["plugins.recommendations","environments.environment_context"]}}}`,
		`{"type":"response_item","payload":{"type":"message","id":"legacy","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/w</cwd>\n</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","id":"prompt","role":"user","content":[{"type":"input_text","text":"Fix the tests"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text","user.image"]}}}`,
		`{"type":"response_item","payload":{"type":"message","id":"aborted","role":"user","content":[{"type":"input_text","text":"<turn_aborted>\nThe user interrupted.\n</turn_aborted>"}]}}`,
	)
	if turns := humanTurns(messages); strings.Join(turns, "|") != "Fix the tests" {
		t.Fatalf("human turns = %q", turns)
	}
	found := byNativeID(messages)
	for id, source := range map[string]string{"tagged:block:0": "plugins.recommendations", "tagged:block:1": "environments.environment_context", "legacy:block:0": "environments.environment_context", "aborted:block:0": "turn_aborted"} {
		if message := found[id]; message.Kind != contextKind || !strings.Contains(message.Text, `"source":"`+source+`"`) {
			t.Fatalf("%s = %#v, want %s context", id, message, source)
		}
	}
	if attachment := found["prompt:attachments"]; attachment.Kind != "attachment" || strings.Contains(attachment.Text, "AAAA") {
		t.Fatalf("user attachment = %#v", attachment)
	}
}

func TestCodexSubagentPromptComesFromAgent(t *testing.T) {
	messages := parseJSONL(t, "codex", filepath.Join(t.TempDir(), "rollout-child.jsonl"),
		`{"type":"session_meta","payload":{"id":"child","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}}}`,
		`{"type":"response_item","payload":{"type":"message","id":"brief","role":"user","content":[{"type":"input_text","text":"Review the diff"}]}}`,
	)
	if brief := byNativeID(messages)["brief"]; brief.Role != agentRole {
		t.Fatalf("sub-agent prompt = %#v", brief)
	}
}
