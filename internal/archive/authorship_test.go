package archive

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var authorshipStart = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func authorAt(minutes int) time.Time {
	return authorshipStart.Add(time.Duration(minutes) * time.Minute)
}

// categoryText joins the text of every span in one category.
func categoryText(text string, spans []authorSpan, category string) string {
	parts := []string{}
	for _, span := range spans {
		if span.Category == category {
			parts = append(parts, strings.TrimSpace(text[span.Start:span.End]))
		}
	}
	return strings.Join(parts, "|")
}

func TestAuthorshipSeparatesHarnessAttachmentsAndSlashCommands(t *testing.T) {
	c := newClassifier(nil)
	text := "<system_instruction>\nYou are working inside Conductor.\n</system_instruction>\n\nPlease fix the login bug @⟦pasted_text_2026-09-01.txt⟧(.context%2Fattachments%2Fab%2Fpasted_text_2026-09-01.txt)"
	spans := c.classify(authorInput{ID: "m", ConversationID: "c", Text: text, SentAt: authorAt(0)})
	if got := categoryText(text, spans, spanHarness); !strings.HasPrefix(got, "<system_instruction>") || !strings.HasSuffix(got, "</system_instruction>") {
		t.Fatalf("harness = %q", got)
	}
	if got := categoryText(text, spans, spanTyped); got != "Please fix the login bug" {
		t.Fatalf("typed = %q", got)
	}
	if got := categoryText(text, spans, spanAttachment); !strings.HasPrefix(got, "@⟦pasted_text") {
		t.Fatalf("attachment = %q", got)
	}
	path := "Create a PR .context/attachments/6oe65N/PR instructions.md"
	if got := categoryText(path, c.classify(authorInput{ID: "p", ConversationID: "d", Text: path, SentAt: authorAt(1)}), spanAttachment); got != ".context/attachments/6oe65N/PR instructions.md" {
		t.Fatalf("attachment path = %q", got)
	}
	slash := "/loop 5m check the deploy"
	spans = c.classify(authorInput{ID: "s", ConversationID: "e", Text: slash, SentAt: authorAt(2)})
	if categoryText(slash, spans, spanTemplate) != "/loop" || categoryText(slash, spans, spanTyped) != "5m check the deploy" {
		t.Fatalf("slash spans = %#v", spans)
	}
}

func TestAuthorshipAttributesSenders(t *testing.T) {
	c := newClassifier(nil)
	for _, input := range []authorInput{
		{Sender: "automation:claude-sdk-cli"}, {Sender: "automation:codex-exec"}, {Sender: "agent:session"},
		{SourceKind: "tl1"}, {SubAgent: true},
	} {
		input.ID, input.ConversationID, input.Text, input.SentAt = "m", "c", "Review this candidate carefully", authorAt(0)
		spans := c.classify(input)
		if len(spans) != 1 || spans[0].Category != spanAutomated || spans[0].Reason == "" {
			t.Fatalf("%#v spans = %#v", input, spans)
		}
	}
	person := authorInput{ID: "p", ConversationID: "c", Sender: "account:me", Text: "Review this candidate carefully", SentAt: authorAt(1)}
	if spans := c.classify(person); len(spans) != 1 || spans[0].Category != spanTyped {
		t.Fatalf("person spans = %#v", spans)
	}
}

func TestAuthorshipDetectsTemplatesButNotOneDayBroadcasts(t *testing.T) {
	inputs := []authorInput{}
	for day := 0; day < 5; day++ {
		inputs = append(inputs, authorInput{ID: "t" + string(rune('a'+day)), ConversationID: "c" + string(rune('a'+day)),
			Text: "Please review the changes in this workspace .context/attachments/x" + string(rune('a'+day)) + "/Review request.md", SentAt: authorAt(day * 24 * 60)})
	}
	broadcast := "Draft a migration plan for the billing tables and list risks"
	for session := 0; session < 6; session++ {
		inputs = append(inputs, authorInput{ID: "b" + string(rune('a'+session)), ConversationID: "s" + string(rune('a'+session)), Text: broadcast, SentAt: authorAt(10 * 24 * 60)})
	}
	c := newClassifier(inputs)
	for _, input := range inputs[:5] {
		if spans := c.classify(input); categoryText(input.Text, spans, spanTyped) != "" || !strings.Contains(spans[0].Reason, "5 sessions on 5 days") {
			t.Fatalf("template spans = %#v", spans)
		}
	}
	if spans := c.classify(inputs[5]); categoryText(broadcast, spans, spanTyped) != broadcast {
		t.Fatalf("first broadcast copy = %#v", spans)
	}
}

func TestAuthorshipFindsCopiedTextWithinWindow(t *testing.T) {
	c := newClassifier(nil)
	reply := "Here are the notable gaps in what this telemetry can answer with high confidence today and what it would take to close them"
	c.agentText.add(authorAt(0), textOrigin{"other-chat", "reply"}, shingles(wordTokens(reply)))
	text := "Consider these gaps and whether we could add telemetry.\n\n" + reply
	spans := c.classify(authorInput{ID: "q", ConversationID: "c", Text: text, SentAt: authorAt(30)})
	if got := categoryText(text, spans, spanTyped); got != "Consider these gaps and whether we could add telemetry." {
		t.Fatalf("typed = %q", got)
	}
	var quoted authorSpan
	for _, span := range spans {
		if span.Category == spanQuoted {
			quoted = span
		}
	}
	if quoted.Source != "other-chat" || !strings.Contains(quoted.Reason, "30m") {
		t.Fatalf("quoted = %#v", quoted)
	}
	late := authorInput{ID: "late", ConversationID: "c", Text: reply, SentAt: authorAt(49 * 60)}
	c.agentText.evict(late.SentAt.Add(-authorshipWindow))
	c.userText.evict(late.SentAt.Add(-authorshipWindow))
	if spans := c.classify(late); categoryText(reply, spans, spanTyped) != reply {
		t.Fatalf("text from outside the window = %#v", spans)
	}
	again := authorInput{ID: "again", ConversationID: "d", Text: reply, SentAt: authorAt(49*60 + 5)}
	if spans := c.classify(again); len(spans) != 1 || spans[0].Category != spanResent || spans[0].Source != "c" {
		t.Fatalf("resent = %#v", spans)
	}
}

func TestAuthorshipFlagsPastedStructure(t *testing.T) {
	c := newClassifier(nil)
	log := "I'm failing to set up firebase with this error - what permission am I missing?\n\n" +
		"[debug] [2026-04-16T13:26:00.356Z] > authorizing via signed-in user\n" +
		"[debug] [2026-04-16T13:26:00.357Z] > command requires scopes: [\"email\"]\n" +
		"[debug] [2026-04-16T13:26:01.002Z] <<< HTTP RESPONSE 403"
	spans := c.classify(authorInput{ID: "l", ConversationID: "c", Text: log, SentAt: authorAt(0)})
	if got := categoryText(log, spans, spanTyped); got != "I'm failing to set up firebase with this error - what permission am I missing?" {
		t.Fatalf("typed = %q (%#v)", got, spans)
	}
	plan := "Please critique this plan.\n\n# Plan: Run a task once\n## Context\nToday tasks only run in the pool.\n## Steps\n- **Queue** add a flag\n- **Worker** honor it"
	spans = c.classify(authorInput{ID: "p", ConversationID: "c", Text: plan, SentAt: authorAt(60)})
	if got := categoryText(plan, spans, spanTyped); got != "Please critique this plan." {
		t.Fatalf("plan typed = %q", got)
	}
	code := "Why does this fail?\n```go\nfunc main() { panic(1) }\n```"
	spans = c.classify(authorInput{ID: "k", ConversationID: "c", Text: code, SentAt: authorAt(120)})
	if categoryText(code, spans, spanTyped) != "Why does this fail?" || !strings.HasPrefix(categoryText(code, spans, spanPasted), "```go") {
		t.Fatalf("code spans = %#v", spans)
	}
}

func TestAuthorshipTypingSpeedOnlyFlagsImpossibleRates(t *testing.T) {
	c := newClassifier(nil)
	c.classify(authorInput{ID: "first", ConversationID: "c", Text: "Start the migration", SentAt: authorAt(0)})
	long := strings.Repeat("this sentence is plain prose that someone could type ", 20)
	fast := authorInput{ID: "fast", ConversationID: "c", Text: long, SentAt: authorAt(0).Add(10 * time.Second)}
	if spans := c.classify(fast); len(spans) != 1 || spans[0].Category != spanPasted || !strings.Contains(spans[0].Reason, "faster than typing") {
		t.Fatalf("fast spans = %#v", spans)
	}
	slow := authorInput{ID: "slow", ConversationID: "c", Text: strings.ReplaceAll(long, "plain", "calm"), SentAt: authorAt(20)}
	if spans := c.classify(slow); len(spans) != 1 || spans[0].Category != spanTyped {
		t.Fatalf("slow spans = %#v", spans)
	}
}

func TestAuthorshipRebuildCountsRepresentativeWorkOnce(t *testing.T) {
	catalog, _ := testCatalog(t)
	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	reply := "The parser now accepts nested tables and every fixture passes with the new grammar rules enabled"
	for _, record := range []WorkspaceRecord{
		{SourceID: "claude-session", SourceKind: "claude", Account: "local", Title: "Parser", Conversations: []ConversationRecord{{
			NativeID: "session", Provider: "claude", Account: "local", Coverage: "complete", Messages: []MessageRecord{
				{NativeID: "u1", Role: "user", Kind: "message", Text: "<system_instruction>\nConductor\n</system_instruction>\n\nFix the parser for nested tables", CreatedAt: "2026-09-01T12:00:00.000Z", Selected: true},
				{NativeID: "a1", Role: "assistant", Kind: "message", Text: reply, CreatedAt: "2026-09-01T12:05:00.000Z", Selected: true},
			}}}},
		{SourceID: "other", SourceKind: "codex", Account: "local", Title: "Review", Conversations: []ConversationRecord{{
			NativeID: "other", Provider: "codex", Account: "local", Coverage: "complete", Messages: []MessageRecord{
				{NativeID: "u2", Role: "user", Kind: "message", Text: "Check this claim: " + reply, CreatedAt: "2026-09-01T13:00:00.000Z", Selected: true},
				{NativeID: "u3", Role: "user", Kind: "message", Text: "Audit the grammar", CreatedAt: "2026-09-01T13:30:00.000Z", Selected: true, Sender: "automation:codex-exec"},
			}}}},
	} {
		tx, err := catalog.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ingestWorkspace(tx, record, false); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.RebuildAuthorship(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	stats, err := catalog.AuthorshipStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	totals := stats["totals"].(map[string]any)
	if integer(totals["messages"]) != 3 || integer(totals["typed_words"]) != 9 || integer(totals["automated_chars"]) != int64(len("Audit the grammar")) ||
		integer(totals["quoted_chars"]) != int64(len(reply)) || integer(totals["harness_chars"]) == 0 {
		encoded, _ := json.Marshal(stats)
		t.Fatalf("stats = %s", encoded)
	}
	daily, _ := stats["daily"].([]map[string]any)
	if len(daily) != 1 || daily[0]["day"] != "2026-09-01" && daily[0]["day"] != "2026-09-02" || integer(daily[0]["quoted_words"]) != 16 ||
		integer(daily[0]["automated_words"]) != 3 || integer(totals["quoted_words"]) != 16 {
		t.Fatalf("daily = %#v, totals = %#v", daily, totals)
	}
	rows, err := queryMaps(catalog.DB, "SELECT w.id FROM workspaces w WHERE w.source_id='other'")
	if err != nil || len(rows) != 1 {
		t.Fatalf("workspace rows = %v %v", rows, err)
	}
	detail, err := catalog.WorkDetail(firstString(rows[0]["id"]))
	if err != nil {
		t.Fatal(err)
	}
	messages := detail["conversations"].([]map[string]any)[0]["messages"].([]map[string]any)
	authorship, _ := messages[0]["authorship"].(map[string]any)
	spans, _ := authorship["spans"].([]map[string]any)
	if len(spans) != 2 || spans[1]["category"] != spanQuoted || !strings.HasPrefix(firstString(spans[1]["excerpt"]), "The parser now accepts") {
		t.Fatalf("detail authorship = %#v", authorship)
	}
	// A copied span links to the message it was copied from.
	source, _ := spans[1]["source"].(map[string]any)
	claude, err := queryMaps(catalog.DB, "SELECT m.id message_id,c.id conversation_id,c.workspace_id FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.native_id='a1'")
	if err != nil || len(claude) != 1 {
		t.Fatalf("source rows = %v %v", claude, err)
	}
	if source["workspace_id"] != claude[0]["workspace_id"] || source["conversation_id"] != claude[0]["conversation_id"] || source["message_id"] != claude[0]["message_id"] || source["title"] != "Parser" {
		t.Fatalf("source = %#v, want %#v", source, claude[0])
	}
}

func TestProseIndexServesAuthorshipQueries(t *testing.T) {
	catalog, _ := testCatalog(t)
	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SELECT m.id FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.kind='message' AND m.role='user' AND m.created_at IS NOT NULL",
		"SELECT conversation_id FROM messages WHERE kind='message' AND role='assistant' AND created_at>=? ORDER BY created_at",
	} {
		rows, err := queryMaps(catalog.DB, "EXPLAIN QUERY PLAN "+query, "2026-01-01")
		if err != nil {
			t.Fatal(err)
		}
		plan, _ := json.Marshal(rows)
		if !strings.Contains(string(plan), "messages_prose_idx") || strings.Contains(string(plan), "TEMP B-TREE") {
			t.Fatalf("plan for %q = %s", query, plan)
		}
	}
}

func TestClaudeKeepsHarnessTextAndMarksHeadlessRuns(t *testing.T) {
	prompt := "<system_instruction>\nYou are working inside Conductor.\n</system_instruction>\n\nShip the fix"
	messages := parseJSONL(t, "claude", filepath.Join(t.TempDir(), "session.jsonl"),
		`{"sessionId":"s","uuid":"first","type":"user","entrypoint":"sdk-ts","message":{"role":"user","content":"<system_instruction>\nYou are working inside Conductor.\n</system_instruction>\n\nShip the fix"}}`,
		`{"sessionId":"s","uuid":"headless","type":"user","entrypoint":"sdk-cli","message":{"role":"user","content":"Review the candidate"}}`,
	)
	// The prompt keeps harness text: it is part of what the model read.
	if turns := humanTurns(messages); strings.Join(turns, "|") != prompt+"|Review the candidate" {
		t.Fatalf("human turns = %q", turns)
	}
	found := byNativeID(messages)
	if found["first"].Sender != "" || found["headless"].Sender != "automation:claude-sdk-cli" {
		t.Fatalf("senders = %q %q", found["first"].Sender, found["headless"].Sender)
	}
}

func TestCodexKeepsHarnessTextAndMarksExecRuns(t *testing.T) {
	messages := parseJSONL(t, "codex", filepath.Join(t.TempDir(), "rollout.jsonl"),
		`{"type":"session_meta","payload":{"id":"root","originator":"codex_exec","source":"exec"}}`,
		`{"type":"response_item","payload":{"type":"message","id":"prompt","role":"user","content":[{"type":"input_text","text":"<system_instruction>\nConductor\n</system_instruction>\n\nPlease review the changes"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}}`,
	)
	if turns := humanTurns(messages); strings.Join(turns, "|") != "<system_instruction>\nConductor\n</system_instruction>\n\nPlease review the changes" {
		t.Fatalf("human turns = %q", turns)
	}
	if sender := byNativeID(messages)["prompt"].Sender; sender != "automation:codex-exec" {
		t.Fatalf("sender = %q", sender)
	}
}

func TestConductorSender(t *testing.T) {
	for want, row := range map[string]map[string]any{
		"agent:s1":      {"sender_session_id": "s1", "sender_id": "u"},
		"automation:ci": {"sender_api_key_name": "ci", "sender_id": "u"},
		"account:u":     {"sender_id": "u"},
		"":              {},
	} {
		if got := conductorSender(row); got != want {
			t.Fatalf("conductorSender(%v) = %q, want %q", row, got, want)
		}
	}
}

func TestAuthorshipUpgradeAddsWordColumns(t *testing.T) {
	catalog, _ := testCatalog(t)
	if _, err := catalog.DB.Exec("DROP TABLE IF EXISTS message_authorship"); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec(`CREATE TABLE message_authorship (message_id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, workspace_id TEXT NOT NULL,
		sent_at TEXT NOT NULL, day TEXT NOT NULL, total_chars INTEGER NOT NULL, typed_chars INTEGER NOT NULL, typed_words INTEGER NOT NULL,
		harness_chars INTEGER NOT NULL, automated_chars INTEGER NOT NULL, template_chars INTEGER NOT NULL, attachment_chars INTEGER NOT NULL,
		quoted_chars INTEGER NOT NULL, resent_chars INTEGER NOT NULL, pasted_chars INTEGER NOT NULL, pasted_words INTEGER NOT NULL, spans_json TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	if err := catalog.RebuildAuthorship(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AuthorshipStats(context.Background()); err != nil {
		t.Fatal(err)
	}
}
