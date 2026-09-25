package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain keeps every test, and every process a test starts, out of this
// Mac's real support directory: serving a library writes its MCP launcher,
// library.json and host.json there. Helper processes share their parent's.
func TestMain(m *testing.M) {
	support := os.Getenv("PHAROS_TEST_SUPPORT_DIR")
	owned := support == ""
	if owned {
		dir, err := os.MkdirTemp("", "pharos-test-support-")
		if err != nil {
			panic(err)
		}
		support = dir
		os.Setenv("PHAROS_TEST_SUPPORT_DIR", support)
	}
	os.Setenv("PHAROS_SUPPORT_DIR", support)
	code := m.Run()
	if owned {
		os.RemoveAll(support)
	}
	os.Exit(code)
}

func testCatalog(t *testing.T) (*Catalog, Config) {
	t.Helper()
	root := t.TempDir()
	config := defaultConfig(filepath.Join(root, "archive.toml"))
	config.CatalogPath = filepath.Join(root, "catalog.sqlite3")
	config.ArchiveRoot = filepath.Join(root, "archive")
	config.StagingRoot = filepath.Join(root, "staging")
	config.APIToken = "test-token"
	if err := os.MkdirAll(config.ArchiveRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { catalog.Close() })
	return catalog, config
}

func TestCatalogPoolEnforcesForeignKeys(t *testing.T) {
	catalog, _ := testCatalog(t)
	connections := make([]*sql.Conn, 4)
	for index := range connections {
		connection, err := catalog.DB.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		connections[index] = connection
		defer connection.Close()
		var enabled int
		if err := connection.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&enabled); err != nil {
			t.Fatal(err)
		}
		if enabled != 1 {
			t.Fatalf("catalog connection %d has foreign_keys=%d", index, enabled)
		}
	}
}

func TestReingestReplacesSessionMessageLinks(t *testing.T) {
	catalog, _ := testCatalog(t)
	record := WorkspaceRecord{
		SourceID: "tl1-task", SourceKind: "tl1", Account: "local", Title: "Task",
		Conversations: []ConversationRecord{{
			NativeID: "tl1-transcript", Provider: "claude", Account: "local",
			Messages: []MessageRecord{{NativeID: "event-1", Role: "user", Kind: "message", Text: "First event", Selected: true}},
		}},
	}
	for run := 0; run < 2; run++ {
		tx, err := catalog.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ingestWorkspace(tx, record, false); err != nil {
			tx.Rollback()
			t.Fatalf("ingest run %d: %v", run+1, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	var links int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM agent_session_messages").Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Fatalf("reingest left %d links; want 1", links)
	}
}

func TestReingestFTSUsesRowidsAndSkipsUnchangedText(t *testing.T) {
	catalog, _ := testCatalog(t)
	record := WorkspaceRecord{
		SourceID: "session", SourceKind: "conductor", Account: "local", Title: "Session",
		Conversations: []ConversationRecord{{NativeID: "thread", Provider: "codex", Account: "local",
			Messages: []MessageRecord{{NativeID: "one", Role: "user", Kind: "message", Text: "alpha", Selected: true}}}},
	}
	ingest := func() {
		t.Helper()
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
	ftsRow := func() (int64, string) {
		t.Helper()
		var rowid int64
		var text string
		if err := catalog.DB.QueryRow(`SELECT f.rowid,f.text FROM messages_fts f
			JOIN message_fts_rows r ON r.fts_rowid=f.rowid`).Scan(&rowid, &text); err != nil {
			t.Fatal(err)
		}
		return rowid, text
	}
	ingest()
	first, value := ftsRow()
	if value != "alpha" {
		t.Fatalf("FTS text = %q", value)
	}
	ingest()
	second, _ := ftsRow()
	if second != first {
		t.Fatalf("unchanged text rebuilt FTS row %d as %d", first, second)
	}
	record.Conversations[0].Messages[0].Text = "beta"
	ingest()
	_, value = ftsRow()
	if value != "beta" {
		t.Fatalf("updated FTS text = %q", value)
	}
	record.Conversations[0].Messages = nil
	ingest()
	var count int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("pruned conversation has %d FTS rows", count)
	}
}

func TestFTSRowidMigrationIndexesExistingRows(t *testing.T) {
	catalog, _ := testCatalog(t)
	if _, err := catalog.DB.Exec("INSERT INTO messages_fts(message_id,text) VALUES('legacy','old text')"); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec("DELETE FROM meta WHERE key='message_fts_rows_version'"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	var text string
	if err := catalog.DB.QueryRow(`SELECT f.text FROM messages_fts f JOIN message_fts_rows r
		ON r.fts_rowid=f.rowid WHERE r.message_id='legacy'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "old text" {
		t.Fatalf("migrated FTS text = %q", text)
	}
}

func TestIngestSkipsUnchangedRecordsWhenSourceFingerprintChanges(t *testing.T) {
	catalog, _ := testCatalog(t)
	path := filepath.Join(t.TempDir(), "export.json")
	adapter, err := MakeAdapter(SourceConfig{Name: "fixture", Kind: "canonical", Path: path, Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	write := func(title, padding string) {
		t.Helper()
		payload := `{"workspaces":[{"id":"work","title":"` + title + `","conversations":[{"id":"thread","provider":"codex","messages":[{"id":"one","role":"user","text":"hello"}]}]}]}` + padding
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("First", "")
	first := catalog.Ingest(adapter, nil)
	if first.Error != nil || first.Workspaces != 1 {
		t.Fatalf("initial ingest: %+v", first)
	}
	write("First", " ")
	second := catalog.Ingest(adapter, nil)
	if second.Error != nil || second.Workspaces != 0 || second.SkippedCurrent != 1 {
		t.Fatalf("unchanged record was reingested: %+v", second)
	}
	write("Updated", "  ")
	third := catalog.Ingest(adapter, nil)
	if third.Error != nil || third.Workspaces != 1 {
		t.Fatalf("changed record was skipped: %+v", third)
	}
	var title string
	if err := catalog.DB.QueryRow("SELECT title FROM workspaces WHERE source_kind='canonical'").Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Updated" {
		t.Fatalf("catalog title = %q", title)
	}
}

func TestLegacyConductorResumeChecksActivityTime(t *testing.T) {
	catalog, _ := testCatalog(t)
	adapter, err := MakeAdapter(SourceConfig{Name: "conductor", Kind: "conductor", Path: t.TempDir(), Account: "local"})
	if err != nil {
		t.Fatal(err)
	}
	record := WorkspaceRecord{SourceID: "session", SourceKind: "conductor", Account: "local", Title: "Session",
		ActivityAt: "2026-09-20T00:00:00Z", Conversations: []ConversationRecord{{
			NativeID: "thread", Provider: "codex", Account: "local", Coverage: conductorExtractor,
			Messages: []MessageRecord{{NativeID: "one", Role: "user", Kind: "message", Text: "hello", Selected: true}},
		}},
	}
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
	if !catalog.recordUsesCurrentExtractor(adapter, record, currentHost().ID) {
		t.Fatal("recently indexed record was not resumable")
	}
	record.ActivityAt = "2099-01-01T00:00:00Z"
	if catalog.recordUsesCurrentExtractor(adapter, record, currentHost().ID) {
		t.Fatal("new source activity was skipped")
	}
}

func TestCodexJSONLReadsLargeEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-large.jsonl")
	message := strings.Repeat("x", 17*1024*1024)
	data := `{"type":"session_meta","payload":{"id":"large-session"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","message":"` + message + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := jsonlAdapter{baseAdapter: baseAdapter{config: SourceConfig{Path: path, Account: "local"}}, provider: "codex"}
	record, ok, err := adapter.parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(record.Conversations) != 1 || len(record.Conversations[0].Messages) != 1 || len(record.Conversations[0].Messages[0].Text) != len(message) {
		t.Fatalf("large JSONL event was not retained: ok=%v conversations=%d", ok, len(record.Conversations))
	}
}

func TestCodexModelFollowsTurnContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-models.jsonl")
	data := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"model-session"}}`,
		`{"type":"turn_context","payload":{"model":"gpt-5.6-terra"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}}`,
		`{"type":"turn_context","payload":{"model":"gpt-5.6-sol"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := jsonlAdapter{baseAdapter: baseAdapter{config: SourceConfig{Path: path, Account: "local"}}, provider: "codex"}
	record, ok, err := adapter.parse(path)
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	conversation := record.Conversations[0]
	if conversation.Model != "gpt-5.6-sol" || len(conversation.Messages) != 2 || conversation.Messages[0].Model != "gpt-5.6-terra" || conversation.Messages[1].Model != "gpt-5.6-sol" {
		t.Fatalf("model transitions lost: %#v", conversation)
	}
	catalog, _ := testCatalog(t)
	tx, err := catalog.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ingestWorkspace(tx, record, false); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var model string
	if err := catalog.DB.QueryRow("SELECT model FROM messages WHERE text='first'").Scan(&model); err != nil || model != "gpt-5.6-terra" {
		t.Fatalf("stored first model = %q, err=%v", model, err)
	}
}

func TestCodexCompactionTrigger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-compaction.jsonl")
	data := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"compaction-session"}}`,
		`{"type":"compacted","payload":{"message":"replayed"}}`,
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"work"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{}"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10}}}}`,
		`{"type":"compacted","payload":{"message":"automatic"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10}}}}`,
		`{"type":"compacted","payload":{"message":"requested"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := jsonlAdapter{baseAdapter: baseAdapter{config: SourceConfig{Path: path, Account: "local"}}, provider: "codex"}
	record, ok, err := adapter.parse(path)
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	triggers := []string{}
	for _, message := range record.Conversations[0].Messages {
		if strings.Contains(message.Text, `"type":"compacted"`) {
			var event map[string]any
			if err := json.Unmarshal([]byte(message.Text), &event); err != nil {
				t.Fatal(err)
			}
			triggers = append(triggers, firstString(event["compaction_trigger"]))
		}
	}
	if strings.Join(triggers, ",") != ",auto,manual" {
		t.Fatalf("compaction triggers = %q", triggers)
	}
}

func TestCodexMessagesToExistingAgentDoNotCreateSubagents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-agents.jsonl")
	data := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"agent-session"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"spawn","arguments":"{\"task_name\":\"review\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"spawn","output":"{\"task_name\":\"/root/review\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"send_message","call_id":"message","arguments":"{\"target\":\"review\",\"message\":\"Update\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"message","output":""}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := jsonlAdapter{provider: "codex"}
	record, ok, err := adapter.parse(path)
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	messages := record.Conversations[0].Messages
	if len(messages) != 4 || messages[0].Kind != "delegation" || messages[1].Kind != "delegation_result" || messages[2].Kind != "tool_call" || messages[3].Kind != "tool_result" {
		t.Fatalf("agent event kinds: %#v", messages)
	}
	if got := len(agentSessionSummaries(messages)); got != 2 {
		t.Fatalf("agent sessions = %d, want root and spawned child", got)
	}
}

func TestWorkflowToolIsDelegation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow-root.jsonl")
	data := strings.Join([]string{
		`{"sessionId":"workflow-root","uuid":"call","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Workflow","id":"workflow-1","input":{"script":"agents run here"}}]}}`,
		`{"sessionId":"workflow-root","uuid":"result","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"workflow-1","content":"done"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := jsonlAdapter{provider: "claude"}
	record, ok, err := adapter.parse(path)
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	messages := record.Conversations[0].Messages
	if len(messages) != 2 || messages[0].Kind != "delegation" || messages[1].Kind != "delegation_result" {
		t.Fatalf("workflow event kinds: %#v", messages)
	}
}

func TestCodexParserUpgradeDoesNotSkipUnchangedFile(t *testing.T) {
	catalog, _ := testCatalog(t)
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := MakeAdapter(SourceConfig{Name: "codex", Kind: "codex", Path: root, Account: "local"})
	if err != nil {
		t.Fatal(err)
	}
	record := WorkspaceRecord{SourceID: "session", SourceKind: "codex", Account: "local", Conversations: []ConversationRecord{{Origin: path}}}
	if _, err := catalog.DB.Exec(`INSERT INTO source_record_states(host_id,source_name,source_kind,source_account,source_id,digest,updated_at)
		VALUES(?,'codex','codex','local','session','old-digest',?)`, currentHost().ID, now()); err != nil {
		t.Fatal(err)
	}
	if catalog.recordUsesCurrentExtractor(adapter, record, currentHost().ID) {
		t.Fatal("old Codex record suppressed a parser repair")
	}
	if err := os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if catalog.recordUsesCurrentExtractor(adapter, record, currentHost().ID) {
		t.Fatal("modified Codex file was skipped")
	}
}

func TestRepeatedNativeMessageIDDoesNotDuplicateSessionLink(t *testing.T) {
	catalog, _ := testCatalog(t)
	record := WorkspaceRecord{SourceID: "claude", SourceKind: "claude", Account: "local", Title: "Claude",
		Conversations: []ConversationRecord{{NativeID: "thread", Provider: "claude", Account: "local",
			Messages: []MessageRecord{
				{NativeID: "same", Role: "assistant", Kind: "message", Text: "draft", Selected: true},
				{NativeID: "same", Role: "assistant", Kind: "message", Text: "final", Selected: true},
			},
		}},
	}
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
	var links int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM agent_session_messages").Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Fatalf("got %d duplicate session links", links)
	}
	var text string
	if err := catalog.DB.QueryRow("SELECT text FROM messages").Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "final" {
		t.Fatalf("stored message = %q", text)
	}
}

func TestSourceInventoryReportsConversationDocumentCoverage(t *testing.T) {
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	check := func(wantDocs, wantConversations int) {
		t.Helper()
		inventory, err := server.sourceInventory()
		if err != nil {
			t.Fatal(err)
		}
		if inventory["ingested_documents"] != wantDocs || inventory["total_conversations"] != wantConversations {
			t.Fatalf("unexpected coverage: %#v", inventory)
		}
	}
	check(0, 0)
	if _, err := catalog.DB.Exec(`INSERT INTO workspaces(id,source_kind,source_account,source_id,title,indexed_at)
		VALUES('workspace-1','canonical','local','source-1','Test workspace','2026-09-23T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"conversation-1", "conversation-2"} {
		if _, err := catalog.DB.Exec(`INSERT INTO conversations(id,workspace_id,provider,account,native_id)
			VALUES(?,'workspace-1','canonical','local',?)`, id, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := catalog.DB.Exec(`INSERT INTO conversation_documents(conversation_id,vector_json,model,indexed_at)
		VALUES('conversation-1','[]','test','2026-09-23T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	check(1, 2)
}

func TestStableIDsMatchPythonCatalog(t *testing.T) {
	got := stableID("workspace", "conductor", "local", "e9d9806d-fa82-4c6e-97e9-0de3453e32f8")
	want := "workspace_9129eb3d0f4c5356b997a6a284a04745"
	if got != want {
		t.Fatalf("stable ID mismatch: got %s want %s", got, want)
	}
	repository := stableID("repo", "git@github.com:Pythia-Software/explo.git", nil)
	if repository != "repo_aee60273f1995774ae5ab72f826055ee" {
		t.Fatalf("repository ID mismatch: %s", repository)
	}
}

func TestSemanticVectorMatchesPythonImplementation(t *testing.T) {
	vector := semanticEmbed("fix parser bug")
	want := map[int]float64{15: -0.087616400599, 47: -0.486757781107, 48: 0.087616400599, 111: -0.267716779609, 126: 0.486757781107}
	for index, expected := range want {
		difference := vector[index] - expected
		if difference < 0 {
			difference = -difference
		}
		if difference > 1e-11 {
			t.Fatalf("dimension %d got %.12f want %.12f", index, vector[index], expected)
		}
	}
}

func TestCodexRolloutKeepsFirstSessionIdentity(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"child-one", "child-two"} {
		lines := []string{
			`{"type":"session_meta","timestamp":"2026-09-23T00:00:00Z","payload":{"id":"` + id + `","cwd":"/child","git":{"branch":"feature/child","commit_hash":"abc123"}}}`,
			`{"type":"turn_context","timestamp":"2026-09-23T00:00:01Z","payload":{"model":"gpt-test"}}`,
			`{"type":"response_item","timestamp":"2026-09-23T00:00:02Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}`,
			`{"type":"session_meta","timestamp":"2026-09-23T00:00:03Z","payload":{"id":"parent","cwd":"/parent"}}`,
		}
		if err := os.WriteFile(filepath.Join(root, "rollout-"+id+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	adapter, err := MakeAdapter(SourceConfig{Name: "codex", Kind: "codex", Path: root, Account: "local"})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	if err := adapter.Discover(func(record WorkspaceRecord) error {
		seen[record.SourceID] = true
		if record.Location != "/child" || record.Conversations[0].NativeID != record.SourceID ||
			firstString(record.Metadata["branch"]) != "feature/child" || firstString(record.Metadata["head_ref"]) != "abc123" {
			t.Fatalf("rollout identity changed mid-file: %#v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !seen["child-one"] || !seen["child-two"] || len(seen) != 2 {
		t.Fatalf("rollouts merged: %#v", seen)
	}
}

func TestCodexForkWithParentSessionMetaUsesFilenameIdentity(t *testing.T) {
	root := t.TempDir()
	parent := "01a09256-2e53-7b13-bd03-02c212671bce"
	child := "01a0925e-e9fc-73f1-a57f-230e71ea742b"
	path := filepath.Join(root, "rollout-2026-09-11T15-28-08-"+parent+"_"+child+".jsonl")
	lines := []string{
		`{"type":"session_meta","timestamp":"2026-09-11T21:28:09Z","payload":{"id":"` + parent + `","cwd":"/fork"}}`,
		`{"type":"turn_context","timestamp":"2026-09-11T21:28:10Z","payload":{"model":"gpt-test"}}`,
		`{"type":"response_item","timestamp":"2026-09-11T21:28:11Z","payload":{"type":"message","role":"assistant","content":"fork response"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := &jsonlAdapter{provider: "codex"}
	record, ok, err := adapter.parse(path)
	if err != nil || !ok || record.SourceID != child || record.Conversations[0].NativeID != child || record.Conversations[0].ParentNativeID != parent || len(record.Conversations[0].Messages) != 1 {
		t.Fatalf("fork identity and parent: %#v, %v, %v", record, ok, err)
	}
	catalog, _ := testCatalog(t)
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
	var kind string
	if err := catalog.DB.QueryRow("SELECT kind FROM agent_sessions WHERE native_id='main'").Scan(&kind); err != nil || kind != "root" {
		t.Fatalf("Codex continuation session kind = %q, err=%v", kind, err)
	}
}

func TestCodexSubagentMetadataCreatesChildSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-child.jsonl")
	data := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"child","parent_thread_id":"parent","agent_path":"/root/review/details","agent_nickname":"Ada","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","depth":2}}}}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":"reviewed"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := jsonlAdapter{provider: "codex"}
	record, ok, err := adapter.parse(path)
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	child := record.Conversations[0]
	if child.ParentNativeID != "parent" || child.AgentDepth != 2 || child.AgentPath != "/root/review/details" || child.AgentNickname != "Ada" {
		t.Fatalf("subagent metadata: %#v", child)
	}
	catalog, _ := testCatalog(t)
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
	var kind string
	var depth int
	if err := catalog.DB.QueryRow("SELECT kind,depth FROM agent_sessions WHERE native_id='main'").Scan(&kind, &depth); err != nil || kind != "subagent" || depth != 2 {
		t.Fatalf("subagent session = %q depth %d, err=%v", kind, depth, err)
	}
	row, err := queryMaps(catalog.DB, "SELECT id,workspace_id,provider,model,started_at,agent_depth FROM conversations")
	if err != nil || len(row) != 1 {
		t.Fatalf("stored child conversation: %#v, %v", row, err)
	}
	if err := catalog.rebuildAgentSessions(row[0]); err != nil {
		t.Fatal(err)
	}
	if err := catalog.DB.QueryRow("SELECT kind,depth FROM agent_sessions WHERE native_id='main'").Scan(&kind, &depth); err != nil || kind != "subagent" || depth != 2 {
		t.Fatalf("rebuilt subagent session = %q depth %d, err=%v", kind, depth, err)
	}
}

func TestClaudeSubagentHasOwnConversation(t *testing.T) {
	catalog, _ := testCatalog(t)
	root := t.TempDir()
	childDir := filepath.Join(root, "root-session", "subagents")
	nestedDir := filepath.Join(childDir, "workflows", "wf-1")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(root, "root-session.jsonl"): `{"sessionId":"root-session","uuid":"root-message","type":"assistant","gitBranch":"feature/root","message":{"model":"claude-test","content":"root answer"}}` + "\n" + `{"sessionId":"root-session","type":"ai-title","aiTitle":"A useful session title"}`,
		filepath.Join(childDir, "agent-1.jsonl"):  `{"sessionId":"root-session","uuid":"child-message","type":"assistant","message":{"model":"claude-test","content":"child answer"}}`,
		filepath.Join(nestedDir, "agent-2.jsonl"): `{"sessionId":"root-session","uuid":"nested-message","type":"assistant","message":{"model":"claude-test","content":"nested answer"}}`,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	adapter, err := MakeAdapter(SourceConfig{Name: "claude", Kind: "claude", Path: root, Account: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if result := catalog.Ingest(adapter, nil); result.Error != nil || result.Workspaces != 1 || result.Conversations != 3 {
		t.Fatalf("subagent ingest: %#v", result)
	}
	var title, branch string
	if err := catalog.DB.QueryRow("SELECT title,branch FROM workspaces WHERE source_kind='claude'").Scan(&title, &branch); err != nil || title != "A useful session title" || branch != "feature/root" {
		t.Fatalf("Claude title or branch missing: %q %q, %v", title, branch, err)
	}
	rows, err := queryMaps(catalog.DB, `SELECT c.native_id,p.native_id parent_native_id,COUNT(m.id) messages
		FROM conversations c LEFT JOIN conversations p ON p.id=c.parent_id
		JOIN messages m ON m.conversation_id=c.id GROUP BY c.id ORDER BY c.native_id`)
	if err != nil || len(rows) != 3 || firstString(rows[0]["native_id"]) != "root-session" ||
		firstString(rows[1]["native_id"]) != "root-session:subagent:agent-1" ||
		firstString(rows[1]["parent_native_id"]) != "root-session" ||
		firstString(rows[2]["native_id"]) != "root-session:subagent:workflows/wf-1/agent-2" ||
		firstString(rows[2]["parent_native_id"]) != "root-session" ||
		integer(rows[0]["messages"]) != 1 || integer(rows[1]["messages"]) != 1 || integer(rows[2]["messages"]) != 1 {
		t.Fatalf("subagent relationship or messages missing: %#v, %v", rows, err)
	}
	kinds, err := queryMaps(catalog.DB, `SELECT c.native_id,a.kind,a.depth FROM conversations c
		JOIN agent_sessions a ON a.conversation_id=c.id WHERE a.native_id='main' ORDER BY c.native_id`)
	if err != nil || len(kinds) != 3 || firstString(kinds[0]["kind"]) != "root" ||
		firstString(kinds[1]["kind"]) != "subagent" || firstString(kinds[2]["kind"]) != "subagent" ||
		integer(kinds[1]["depth"]) != 1 || integer(kinds[2]["depth"]) != 1 {
		t.Fatalf("child session usage kind or depth missing: %#v, %v", kinds, err)
	}
}

func TestNativeToolEventsRetainIdentityResultsAndConversationFlow(t *testing.T) {
	root := t.TempDir()
	codexSessions := filepath.Join(root, "codex", "sessions")
	if err := os.MkdirAll(codexSessions, 0o755); err != nil {
		t.Fatal(err)
	}
	codexLog := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"codex-session"}}`,
		`{"type":"response_item","timestamp":"2026-09-20T12:00:00Z","payload":{"type":"function_call","id":"call-item","call_id":"call-1","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}`,
		`{"type":"response_item","timestamp":"2026-09-20T12:00:01Z","payload":{"type":"function_call_output","id":"result-item","call_id":"call-1","output":"ok"}}`,
		`{"type":"event_msg","timestamp":"2026-09-20T12:00:02Z","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":12000}}}}`,
		`{"type":"compacted","timestamp":"2026-09-20T12:00:03Z","payload":{"summary":"Reduced context"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(codexSessions, "rollout-test.jsonl"), []byte(codexLog), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := MakeAdapter(SourceConfig{Name: "codex", Kind: "codex", Path: filepath.Join(root, "codex"), Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var codex WorkspaceRecord
	if err := adapter.Discover(func(record WorkspaceRecord) error { codex = record; return nil }); err != nil {
		t.Fatal(err)
	}
	messages := codex.Conversations[0].Messages
	if len(messages) != 4 || messages[0].Kind != "tool_call" || messages[1].Kind != "tool_result" {
		t.Fatalf("unexpected Codex tool messages: %#v", messages)
	}
	if !strings.Contains(messages[0].Text, `"tool":"exec_command"`) || messages[0].CallID != "call-1" || messages[1].CallID != "call-1" {
		t.Fatalf("Codex tool identity was not retained: %#v", messages)
	}
	if !strings.Contains(messages[2].Text, `"total_tokens":12000`) || !strings.Contains(messages[3].Text, `"type":"compacted"`) {
		t.Fatalf("Codex usage and compaction missing: %#v", messages)
	}

	claudeRoot := filepath.Join(root, "claude")
	if err := os.MkdirAll(claudeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	claudeLog := strings.Join([]string{
		`{"sessionId":"claude-session","uuid":"user-1","type":"user","message":{"content":"Please inspect"}}`,
		`{"sessionId":"claude-session","uuid":"assistant-1","parentUuid":"user-1","type":"assistant","message":{"model":"claude-sonnet-test","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"main.go"}}]}}`,
		`{"sessionId":"claude-session","uuid":"result-1","parentUuid":"assistant-1","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"package main"}]}}`,
		`{"sessionId":"claude-session","uuid":"answer-1","type":"assistant","message":{"id":"request-1","content":"Done","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":1000}}}`,
		`{"sessionId":"claude-session","type":"system","subtype":"compact_boundary"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(claudeRoot, "session.jsonl"), []byte(claudeLog), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err = MakeAdapter(SourceConfig{Name: "claude", Kind: "claude", Path: claudeRoot, Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var claude WorkspaceRecord
	if err := adapter.Discover(func(record WorkspaceRecord) error { claude = record; return nil }); err != nil {
		t.Fatal(err)
	}
	messages = claude.Conversations[0].Messages
	foundResult := false
	for _, message := range messages {
		if message.ParentNativeID != "" {
			t.Fatalf("ordinary Claude message-chain parent leaked into visual nesting: %#v", message)
		}
		if message.Kind == "tool_result" && strings.Contains(message.Text, `"tool":"Read"`) {
			foundResult = true
		}
		if message.NativeID == "assistant-1:block:0" && (message.Model != "claude-sonnet-test" || message.PreviousNativeID != "user-1") {
			t.Fatalf("Claude model or event chain missing: %#v", message)
		}
	}
	if !foundResult {
		t.Fatalf("Claude regular tool result or identity missing: %#v", messages)
	}
	foundUsage, foundCompaction := false, false
	for _, message := range messages {
		foundUsage = foundUsage || strings.Contains(message.Text, `"cache_read_input_tokens":1000`)
		foundCompaction = foundCompaction || strings.Contains(message.Text, `"subtype":"compact_boundary"`)
	}
	if !foundUsage || !foundCompaction {
		t.Fatalf("Claude usage or compaction missing: %#v", messages)
	}
}

func TestCanonicalIngestAndAPIContract(t *testing.T) {
	catalog, config := testCatalog(t)
	export := filepath.Join(t.TempDir(), "export.json")
	document := map[string]any{"workspaces": []any{
		map[string]any{
			"id": "work-1", "title": "Fix parser", "activity_at": "2026-09-20T12:00:00Z",
			"repository": map[string]any{"canonical_remote": "github.com/acme/parser", "display_name": "parser"},
			"conversations": []any{map[string]any{
				"id": "conversation-1", "provider": "codex", "messages": []any{
					map[string]any{"id": "message-1", "role": "user", "text": "Please inspect src/parser.go"},
					map[string]any{"id": "message-2", "role": "assistant", "text": "Fixed the parser"},
				},
			}},
			"changes": []any{map[string]any{
				"classification": "attempted", "files": []any{map[string]any{"path": "src/parser.go", "status": "modified"}},
			}},
		},
	}}
	payload, _ := json.Marshal(document)
	if err := os.WriteFile(export, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	source := SourceConfig{Name: "fixture", Kind: "canonical", Path: export, Account: "local", Enabled: true}
	adapter, err := MakeAdapter(source)
	if err != nil {
		t.Fatal(err)
	}
	result := catalog.Ingest(adapter, nil)
	if result.Error != nil {
		t.Fatalf("ingest failed: %v", result.Error)
	}
	if result.Workspaces != 1 || result.Messages != 2 {
		t.Fatalf("unexpected counts: %+v", result)
	}
	found, err := catalog.Search(SearchOptions{Query: "parser", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(found["items"].([]map[string]any)) != 1 {
		t.Fatalf("expected search hit: %#v", found)
	}
	server := NewServer(config, catalog)
	request := httptest.NewRequest(http.MethodGet, "/api/search?q=parser", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body["items"].([]any)) != 1 {
		t.Fatalf("unexpected API body: %s", response.Body.String())
	}
	queryRequest := httptest.NewRequest(http.MethodPost, "/api/query/library", strings.NewReader(`{"select":["title","source_kind","file_edit_count"],"where":[{"field":"source_kind","op":"=","value":"canonical"}],"orderBy":[{"field":"title","dir":"asc"}],"limit":25,"offset":0}`))
	queryRequest.Header.Set("Authorization", "Bearer test-token")
	queryResponse := httptest.NewRecorder()
	server.ServeHTTP(queryResponse, queryRequest)
	if queryResponse.Code != http.StatusOK {
		t.Fatalf("query-table status %d: %s", queryResponse.Code, queryResponse.Body.String())
	}
	var queryBody map[string]any
	if err := json.Unmarshal(queryResponse.Body.Bytes(), &queryBody); err != nil {
		t.Fatal(err)
	}
	if queryBody["total"] != float64(1) || len(queryBody["rows"].([]any)) != 1 {
		t.Fatalf("unexpected query-table body: %s", queryResponse.Body.String())
	}
	row := queryBody["rows"].([]any)[0].(map[string]any)
	if integer(row["file_edit_count"]) != 1 {
		t.Fatalf("file edit count was not exposed in library query: %#v", row)
	}
	if integer(row["turn_count"]) != 1 || firstString(row["first_input"]) != "Please inspect src/parser.go" || firstString(row["last_response"]) != "Fixed the parser" {
		t.Fatalf("conversation preview was not exposed in library query: %#v", row)
	}
}

func TestHealthReportsActiveSync(t *testing.T) {
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	check := func(want bool) {
		t.Helper()
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("health status %d: %s", response.Code, response.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["sync_active"] != want {
			t.Fatalf("sync_active=%v, want %v", body["sync_active"], want)
		}
	}
	check(false)
	run := server.startRun([]SourceConfig{{Name: "test"}})
	check(true)
	server.updateRun(run.ID, func(run *SyncRun) { run.State = "complete" })
	check(false)
}

func TestAppCSPAllowsOnlyLocalBlobFormulaWorkers(t *testing.T) {
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "worker-src 'self' blob:") {
		t.Fatalf("formula workers are blocked by CSP: %q", policy)
	}
	if strings.Contains(policy, "http:") || strings.Contains(policy, "https:") {
		t.Fatalf("CSP unexpectedly allows remote worker sources: %q", policy)
	}
}

func TestConductorSemanticEventsRepositoryAndPR(t *testing.T) {
	catalog, _ := testCatalog(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	worktree := filepath.Join(repository, ".conductor", "kiev")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "conductor.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	schema := `CREATE TABLE sessions (id TEXT PRIMARY KEY,status TEXT,claude_session_id TEXT,created_at TEXT,updated_at TEXT,last_user_message_at TEXT,workspace_id TEXT,title TEXT,model TEXT,agent_type TEXT);
CREATE TABLE workspaces (id TEXT PRIMARY KEY,repository_id TEXT,branch TEXT,state TEXT,workspace_path TEXT,archive_commit TEXT,pr_title TEXT);
CREATE TABLE repos (id TEXT PRIMARY KEY,remote_url TEXT,name TEXT,root_path TEXT);
CREATE TABLE session_messages (id TEXT PRIMARY KEY,session_id TEXT,role TEXT,content TEXT,created_at TEXT,model TEXT,turn_id TEXT,sender_session_id TEXT);`
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO sessions VALUES (?,?,?,?,?,?,?,?,?,?)", "session-1", "idle", "claude-native", "2026-09-20T10:00:00Z", "2026-09-20T10:02:00Z", "2026-09-20T10:00:00Z", "workspace-1", "Contextual time", "opus", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO workspaces VALUES (?,?,?,?,?,?,?)", "workspace-1", "repo-1", "feature/time", "archived", worktree, nil, "Contextual timestamps"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO repos VALUES (?,?,?,?)", "repo-1", "https://github.com/acme/library.git", "library", repository); err != nil {
		t.Fatal(err)
	}
	events := []struct{ id, role, content, at string }{{"m1", "user", "Please update timestamps", "2026-09-20T10:00:00Z"}, {"m2", "assistant", jsonText(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "task-call", "name": "Task", "input": map[string]any{"description": "Find timestamp use", "prompt": "Inspect the UI"}}}}}), "2026-09-20T10:00:10Z"}, {"m2-edit", "assistant", jsonText(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "edit-call", "name": "Edit", "input": map[string]any{"file_path": filepath.Join(worktree, "web", "time.ts"), "old_string": "old", "new_string": "new"}}}}}), "2026-09-20T10:00:15Z"}, {"m3", "assistant", jsonText(map[string]any{"type": "assistant", "parent_tool_use_id": "task-call", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Found the formatter"}}}}), "2026-09-20T10:00:20Z"}, {"m4", "assistant", jsonText(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "task-call", "is_error": true, "content": "https://github.com/acme/library/pull/64"}}}}), "2026-09-20T10:00:30Z"}, {"m5", "assistant", jsonText(map[string]any{"type": "result", "subtype": "success", "usage": map[string]any{"input_tokens": 1200, "output_tokens": 350, "cache_creation_input_tokens": 800, "cache_read_input_tokens": 4200, "output_tokens_details": map[string]any{"thinking_tokens": 125}}}), "2026-09-20T10:00:40Z"}}
	for _, event := range events {
		if _, err = db.Exec("INSERT INTO session_messages VALUES (?,?,?,?,?,NULL,NULL,NULL)", event.id, "session-1", event.role, event.content, event.at); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	source := SourceConfig{Name: "conductor", Kind: "conductor", Path: root, Account: "local", Enabled: true}
	adapter, err := MakeAdapter(source)
	if err != nil {
		t.Fatal(err)
	}
	result := catalog.Ingest(adapter, nil)
	if result.Error != nil {
		t.Fatalf("ingest failed: %v", result.Error)
	}
	id := stableID("workspace", "conductor", "local", "session-1")
	detail, err := catalog.WorkDetail(id)
	if err != nil {
		t.Fatal(err)
	}
	if detail == nil || detail["repository_name"] != "library" {
		t.Fatalf("repository not linked: %#v", detail)
	}
	if detail["purpose"] != "Please update timestamps" || detail["outcome"] != "Found the formatter" {
		t.Fatalf("purpose/outcome not derived: %#v", detail)
	}
	messages := detail["conversations"].([]map[string]any)[0]["messages"].([]map[string]any)
	kinds := []string{}
	for _, message := range messages {
		kinds = append(kinds, firstString(message["kind"]))
	}
	want := []string{"message", "delegation", "tool_call", "message", "delegation_result", "result"}
	if jsonText(kinds) != jsonText(want) {
		t.Fatalf("message kinds got %v want %v", kinds, want)
	}
	for _, message := range messages {
		if firstString(message["model"]) != "opus" {
			t.Fatalf("Conductor session model missing from message: %#v", message)
		}
	}
	prs := detail["prs"].([]map[string]any)
	if len(prs) != 1 || integer(prs[0]["number"]) != 64 || firstString(prs[0]["title"]) != "Contextual timestamps" {
		t.Fatalf("PR not linked: %#v", prs)
	}
	conversations := detail["conversations"].([]map[string]any)
	if firstString(conversations[0]["provider"]) != "claude" || firstString(conversations[0]["model"]) != "opus" {
		t.Fatalf("native provider/model not retained: %#v", conversations[0])
	}
	metrics := detail["metrics"].([]map[string]any)
	foundTokens := map[string]int64{}
	for _, metric := range metrics {
		if firstString(metric["unit"]) == "tokens" {
			foundTokens[firstString(metric["name"])] = integer(metric["value"])
		}
	}
	if foundTokens["input_tokens"] != 6200 || foundTokens["uncached_input_tokens"] != 1200 || foundTokens["output_tokens"] != 350 || foundTokens["cache_creation_input_tokens"] != 800 || foundTokens["cache_read_input_tokens"] != 4200 || foundTokens["reasoning_output_tokens"] != 125 {
		t.Fatalf("token metrics not extracted: %#v", foundTokens)
	}
	libraryRows, err := catalog.searchRows(SearchOptions{}, nil)
	if err != nil || len(libraryRows) != 1 {
		t.Fatalf("library rows: %#v, %v", libraryRows, err)
	}
	row := libraryRows[0]
	var prDetails []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal([]byte(firstString(row["pr_details"])), &prDetails); err != nil || len(prDetails) != 1 || prDetails[0].Number != 64 || prDetails[0].Title != "Contextual timestamps" || prDetails[0].URL == "" {
		t.Fatalf("library PR details: %#v, %v", prDetails, err)
	}
	for key, want := range map[string]int64{"token_count": 6550, "tool_use_count": 2, "tool_error_count": 1, "pr_count": 1, "branch_count": 1, "file_edit_count": 1} {
		if got := integer(row[key]); got != want {
			t.Errorf("%s = %d, want %d (row %#v)", key, got, want, row)
		}
	}
	tokenUsage := catalog.Health()["token_usage"].([]map[string]any)
	if len(tokenUsage) != 1 || firstString(tokenUsage[0]["service"]) != "claude" ||
		firstString(tokenUsage[0]["model"]) != "opus" || integer(tokenUsage[0]["tokens"]) != 6550 {
		t.Fatalf("health token usage not grouped by service and model: %#v", tokenUsage)
	}
	claudeRoot := filepath.Join(root, "claude")
	if err := os.MkdirAll(claudeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(claudeRoot, "session-1.jsonl")
	if err := os.WriteFile(claudePath, []byte("{\"sessionId\":\"session-1\",\"uuid\":\"native-message\",\"type\":\"assistant\",\"message\":{\"model\":\"claude-test\",\"content\":\"Native answer\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	native, err := MakeAdapter(SourceConfig{Name: "claude", Kind: "claude", Path: claudeRoot, Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if result := catalog.Ingest(native, nil); result.Error != nil {
		t.Fatalf("native Claude ingest: %v", result.Error)
	}
	rows, err := queryMaps(catalog.DB, `SELECT w.source_kind,c.native_id,COUNT(m.id) message_count
		FROM conversations c JOIN workspaces w ON w.id=c.workspace_id
		JOIN messages m ON m.conversation_id=c.id WHERE c.provider='claude'
		GROUP BY c.id ORDER BY w.source_kind`)
	if err != nil || len(rows) != 2 || firstString(rows[0]["source_kind"]) != "claude" ||
		firstString(rows[0]["native_id"]) != "session-1" || integer(rows[0]["message_count"]) != 1 ||
		firstString(rows[1]["source_kind"]) != "conductor" ||
		firstString(rows[1]["native_id"]) != "conductor:session-1" || integer(rows[1]["message_count"]) != 6 {
		t.Fatalf("source conversation identity collision: %#v, %v", rows, err)
	}
}

func TestSourceTogglePreservesOtherConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.toml")
	source := "api_token = \"secret\" # keep\n\n[[sources]]\nname = \"one\"\nkind = \"canonical\"\npath = \"/tmp/one\"\nenabled = true\n\n[[sources]]\nname = \"two\"\nkind = \"canonical\"\npath = \"/tmp/two\"\nenabled = true\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetSourceEnabled(path, "two", false); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Sources[0].Enabled || config.Sources[1].Enabled {
		t.Fatalf("wrong source toggle: %#v", config.Sources)
	}
	if config.APIToken != "secret" {
		t.Fatal("root configuration was changed")
	}
}

func TestTL1RegistryIsReadOnlyAndIndexesNativeTask(t *testing.T) {
	catalog, _ := testCatalog(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	transcripts := filepath.Join(root, "transcripts")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(transcripts, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(repository, "tl1.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "tl1.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE tasks (id TEXT PRIMARY KEY,title TEXT,status TEXT,inputs TEXT,outcome TEXT,candidate_id TEXT,flavor_id TEXT,shape_version TEXT,created_at TEXT,completed_at TEXT,updated_at TEXT,instruction_prepend TEXT,instruction_append TEXT,terminal_kind TEXT);
CREATE TABLE task_flavors (id TEXT PRIMARY KEY,name TEXT NOT NULL);
CREATE TABLE task_attempts (id TEXT PRIMARY KEY,task_id TEXT,attempt_number INTEGER,worktree_id TEXT,executor TEXT,session_id TEXT,model TEXT,effort TEXT,agent TEXT,status TEXT,result_summary TEXT,failure_reason TEXT,files_changed INTEGER,lines_added INTEGER,lines_removed INTEGER,started_at TEXT,completed_at TEXT,harness_id TEXT);
INSERT INTO task_flavors VALUES ('flavor-1','candidate-review');
INSERT INTO tasks VALUES ('task-1','Native TL1 task','completed','{"request":"index"}','Indexed',NULL,'flavor-1','v1','2026-09-20T11:00:00Z','2026-09-20T12:01:00Z','2026-09-20T12:01:00Z','','','completed');
INSERT INTO task_attempts VALUES ('attempt-1','task-1',1,'worktree-1','claude','session-1','sonnet','high','worker','completed','Done',NULL,2,10,1,'2026-09-20T11:00:00Z','2026-09-20T12:01:00Z',NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	registry := filepath.Join(root, "registry.json")
	registryValue := map[string]any{"schema_version": 1, "installations": []any{map[string]any{"installation_id": "install-1", "project_name": "fixture", "config_path": configPath, "db_path": database, "code_repo": repository, "transcripts_dir": transcripts}}}
	payload, _ := json.Marshal(registryValue)
	if err := os.WriteFile(registry, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := MakeAdapter(SourceConfig{Name: "tl1", Kind: "tl1", Path: registry, Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Capability() != "retrieval-only" {
		t.Fatalf("native TL1 must remain read-only")
	}
	result := catalog.Ingest(adapter, nil)
	if result.Error != nil {
		t.Fatalf("ingest failed: %v", result.Error)
	}
	if result.Workspaces != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	var sourceID, flavor string
	var authority int
	if err := catalog.DB.QueryRow("SELECT source_id,flavor,reclamation_authority FROM workspaces WHERE source_kind='tl1'").Scan(&sourceID, &flavor, &authority); err != nil {
		t.Fatal(err)
	}
	if sourceID != "install-1:task-1" || flavor != "candidate-review" || authority != 0 {
		t.Fatalf("unexpected TL1 workspace: %s flavor=%s authority=%d", sourceID, flavor, authority)
	}
}

func TestEmbeddedUIIsServedWithoutPython(t *testing.T) {
	html := appHTML()
	if !strings.HasPrefix(html, "<!doctype html>") {
		t.Fatalf("unexpected embedded UI prefix: %.40q", html)
	}
	if !strings.Contains(html, "Pharos") {
		t.Fatal("embedded UI is missing brand")
	}
}

func TestAppRoutesServeUIOnDirectNavigation(t *testing.T) {
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	for _, path := range []string{"/library?search=debug", "/sources", "/activity", "/health", "/work/workspace_123"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "id=\"navigator\"") {
			t.Errorf("GET %s: status %d, missing app UI", path, response.Code)
		}
	}
}
