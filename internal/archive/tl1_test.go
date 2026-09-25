package archive

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyTL1Error(t *testing.T) {
	for _, test := range []struct{ text, class, signature, attribution string }{
		{"worker wren died", "worker_died", "worker died", tl1AttributionInfrastructure},
		{"execution interrupted by process restart", "process_restart", "execution interrupted by process restart", tl1AttributionInfrastructure},
		{"codex exited with code 1: Reading additional input from stdin...\nNot inside a trusted directory and --skip-git-repo-check was not specified.", "executor_exit", "codex exited with code 1: Not inside a trusted directory and --skip-git-repo-check was not specified.", tl1AttributionConfiguration},
		{"contract_repair_exhausted: missing_output_fields: missing output field(s): explo; marker_missing: replacement 3f2a9c81d0", "output_contract", "contract_repair_exhausted: missing_output_fields: marker_missing", tl1AttributionContract},
		{"executor completed without a TL1_HANDOFF marker", "output_contract", "executor completed without a TL1_HANDOFF marker", tl1AttributionContract},
		{"Script exited with code 2", "script_exit", "script exited with code 2", tl1AttributionScript},
		{"watchdog timeout: no transcript activity for 34 minutes", "timeout", "watchdog timeout: no transcript activity for <n> minutes", tl1AttributionAgent},
		{"runtime_error", "unexplained", "runtime_error without a recorded reason", tl1AttributionAgent},
		{"", "", "", ""},
	} {
		class, signature, attribution := classifyTL1Error(test.text)
		if class != test.class || signature != test.signature || attribution != test.attribution {
			t.Errorf("classifyTL1Error(%q) = %q, %q, %q; want %q, %q, %q", test.text, class, signature, attribution, test.class, test.signature, test.attribution)
		}
	}
	// Run-specific identifiers must not split one failure into many clusters.
	_, first, _ := classifyTL1Error("failed to open /tmp/run-1/a.json for 1b39841311af4993b8fa6b957e1ff187")
	_, second, _ := classifyTL1Error("failed to open /tmp/run-2/b.json for 9199899a05e04eed8640833fa7dd50c0")
	if first != second {
		t.Errorf("signatures differ by run detail: %q vs %q", first, second)
	}
}

// tl1Fixture builds a TL1 installation with the current TL1 schema subset
// Pharos reads: flavors with agent configurations and transitions, a
// candidate whose implementation ran on Codex (transcript only, no summary)
// and whose review failed on an executor error, plus events and findings.
func tl1Fixture(t *testing.T, extraFailures int) string {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	transcripts := filepath.Join(root, "transcripts")
	for _, directory := range []string{repository, filepath.Join(transcripts, "task-impl"), filepath.Join(transcripts, "task-review")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(repository, "tl1.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	codex := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Implementing the fix."}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"go test ./...","aggregated_output":"FAIL","exit_code":1,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"a.go","kind":"update"}],"status":"completed"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1000000,"cached_input_tokens":900000,"output_tokens":20000,"reasoning_output_tokens":5000}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(transcripts, "task-impl", "1-codex.jsonl"), []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcripts, "task-review", "1-script.stderr.log"), []byte("boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "tl1.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
CREATE TABLE task_flavors (id TEXT PRIMARY KEY, name TEXT NOT NULL, execution_class TEXT, template TEXT, agent_configurations TEXT, default_agent_configuration TEXT, transitions TEXT, budgets TEXT, outputs_schema TEXT, read_only BOOLEAN, shape_version TEXT);
CREATE TABLE candidates (id TEXT PRIMARY KEY, title TEXT, status TEXT, canonical_branch TEXT, base_sha TEXT, head_sha TEXT, workflow_steps INTEGER, workflow_step_limit INTEGER, created_at TEXT);
CREATE TABLE tasks (id TEXT PRIMARY KEY, flavor_id TEXT, title TEXT, status TEXT, outcome TEXT, terminal_kind TEXT, terminal_reason TEXT, handoff_message TEXT, outputs TEXT, candidate_id TEXT, parent_task_id TEXT, shape_version TEXT, inputs TEXT, created_at TEXT, claimed_at TEXT, completed_at TEXT, updated_at TEXT);
CREATE TABLE task_attempts (id TEXT PRIMARY KEY, task_id TEXT, attempt_number INTEGER, worktree_id TEXT, executor TEXT, model TEXT, effort TEXT, agent_configuration TEXT, harness_id TEXT, status TEXT, failure_reason TEXT, started_at TEXT, completed_at TEXT);
CREATE TABLE transcript_summaries (attempt_id TEXT, filename TEXT, cost_usd REAL, model TEXT);
CREATE TABLE task_events (id INTEGER PRIMARY KEY, task_id TEXT, event_type TEXT, agent TEXT, detail TEXT, created_at TEXT);
CREATE TABLE review_findings (id TEXT PRIMARY KEY, candidate_id TEXT, review_task_id TEXT, severity TEXT, file_path TEXT, explanation TEXT, created_at TEXT);
INSERT INTO task_flavors VALUES
 ('f-impl','implement','llm','Fix the bug.','{"codex-terra-high":{"executor":"codex","model":"gpt-5.6-terra","effort":"high"}}','codex-terra-high','{"ready":{"handoff":"review"}}','{"workflow_steps":60}','{"summary":"text"}',0,'v2'),
 ('f-review','review','llm','Review it.','{"codex-terra-high":{"executor":"codex","model":"gpt-5.6-terra","effort":"high"}}','codex-terra-high','{"approved":{"handoff":"human"}}','{}','{}',1,'v1'),
 ('f-human','human-escalation','human','','{}',NULL,'{}','{}','{}',0,'v1');
INSERT INTO candidates VALUES ('cand-1','Fix parser','open','candidate/1','base','head',3,60,'2026-09-20 10:00:00');
INSERT INTO tasks VALUES
 ('task-impl','f-impl','Implement parser fix','done','ready',NULL,NULL,'ready',NULL,'cand-1',NULL,'v2','{}','2026-09-20 10:00:00','2026-09-20 10:00:05','2026-09-20 10:20:00','2026-09-20 10:20:00'),
 ('task-review','f-review','Review parser fix','done','runtime_error',NULL,'codex exited with code 1: Reading additional input from stdin...
Not inside a trusted directory and --skip-git-repo-check was not specified.',NULL,NULL,'cand-1','task-impl','v1','{}','2026-09-20 10:21:00','2026-09-20 10:21:02','2026-09-20 10:21:04','2026-09-20 10:21:04'),
 ('task-human','f-human','Recover review','pending',NULL,NULL,NULL,NULL,NULL,'cand-1','task-review','v1','{}','2026-09-20 10:22:00',NULL,NULL,'2026-09-20 10:22:00');
INSERT INTO task_attempts VALUES
 ('att-impl','task-impl',1,'wt-1','codex','gpt-5.6-terra','high','codex-terra-high','codex','succeeded',NULL,'2026-09-20 10:00:05','2026-09-20 10:20:00'),
 ('att-review','task-review',1,'wt-1','codex','gpt-5.6-terra','high','codex-terra-high','codex','succeeded',NULL,'2026-09-20 10:21:02','2026-09-20 10:21:04');
INSERT INTO task_events VALUES (1,'task-review','escalated','coordinator','{"reason":"runtime_error"}','2026-09-20 10:21:05');
INSERT INTO review_findings VALUES ('finding-1','cand-1','task-review','major','a.go','Off by one','2026-09-20 10:21:03');
`)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < extraFailures; index++ {
		task, attempt := fmt.Sprintf("task-extra-%d", index), fmt.Sprintf("att-extra-%d", index)
		if _, err := db.Exec(`INSERT INTO tasks(id,flavor_id,title,status,outcome,terminal_reason,candidate_id,shape_version,created_at,updated_at) VALUES(?,'f-review','Review again','done','runtime_error','codex exited with code 1: Not inside a trusted directory and --skip-git-repo-check was not specified.','cand-1','v1','2026-09-20 11:00:00','2026-09-20 11:00:00')`, task); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO task_attempts(id,task_id,attempt_number,worktree_id,executor,model,effort,agent_configuration,status,started_at,completed_at) VALUES(?,?,1,'wt','codex','gpt-5.6-terra','high','codex-terra-high','succeeded','2026-09-20 11:00:00','2026-09-20 11:00:02')`, attempt, task); err != nil {
			t.Fatal(err)
		}
	}
	registry := filepath.Join(root, "registry.json")
	payload, _ := json.Marshal(map[string]any{"schema_version": 1, "installations": []any{map[string]any{"installation_id": "install-1", "project_name": "fixture", "config_path": configPath, "db_path": database, "code_repo": repository, "transcripts_dir": transcripts}}})
	if err := os.WriteFile(registry, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return registry
}

func ingestTL1Fixture(t *testing.T, catalog *Catalog, extraFailures int) {
	t.Helper()
	adapter, err := MakeAdapter(SourceConfig{Name: "tl1", Kind: "tl1", Path: tl1Fixture(t, extraFailures), Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if result := catalog.Ingest(adapter, nil); result.Error != nil {
		t.Fatalf("ingest failed: %v", result.Error)
	}
}

func TestTL1IngestCapturesWorkflowAndCodexTranscripts(t *testing.T) {
	catalog, _ := testCatalog(t)
	ingestTL1Fixture(t, catalog, 0)

	var tasks, attempts, events, findings int
	for table, target := range map[string]*int{"tl1_tasks": &tasks, "tl1_attempts": &attempts, "tl1_events": &events, "tl1_review_findings": &findings} {
		if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE installation_id='install-1'").Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if tasks != 3 || attempts != 2 || events != 1 || findings != 1 {
		t.Fatalf("snapshot counts: tasks=%d attempts=%d events=%d findings=%d", tasks, attempts, events, findings)
	}
	// TL1 writes no transcript summary for Codex; the transcript must still be
	// found on disk and its exec-format usage accounted for.
	var conversation string
	if err := catalog.DB.QueryRow("SELECT conversation_native_id FROM tl1_attempts WHERE attempt_id='att-impl'").Scan(&conversation); err != nil {
		t.Fatal(err)
	}
	if conversation != "install-1:att-impl:1-codex.jsonl" {
		t.Fatalf("Codex attempt not linked to its transcript: %q", conversation)
	}
	// TL1 orchestrates; the provider is the agent that wrote the transcript.
	var conversationProvider, sessionProvider string
	if err := catalog.DB.QueryRow(`SELECT c.provider,s.provider FROM conversations c JOIN agent_sessions s ON s.conversation_id=c.id
		WHERE c.native_id=?`, conversation).Scan(&conversationProvider, &sessionProvider); err != nil {
		t.Fatal(err)
	}
	if conversationProvider != "codex" || sessionProvider != "codex" {
		t.Fatalf("TL1 Codex transcript provider: conversation=%q session=%q", conversationProvider, sessionProvider)
	}
	var input, cached, output, reasoning int64
	if err := catalog.DB.QueryRow(`SELECT s.input_tokens,s.cache_read_input_tokens,s.output_tokens,s.reasoning_output_tokens
		FROM agent_sessions s JOIN conversations c ON c.id=s.conversation_id WHERE c.native_id=?`, conversation).Scan(&input, &cached, &output, &reasoning); err != nil {
		t.Fatal(err)
	}
	if input != 1_000_000 || cached != 900_000 || output != 20_000 || reasoning != 5_000 {
		t.Fatalf("Codex exec usage: input=%d cached=%d output=%d reasoning=%d", input, cached, output, reasoning)
	}
	var class, attribution, workspace string
	if err := catalog.DB.QueryRow("SELECT error_class,error_attribution,workspace_id FROM tl1_tasks WHERE task_id='task-review'").Scan(&class, &attribution, &workspace); err != nil {
		t.Fatal(err)
	}
	if class != "executor_exit" || attribution != tl1AttributionConfiguration {
		t.Fatalf("review error classified as %s/%s", class, attribution)
	}
	var workspaces int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM workspaces WHERE id=? AND source_kind='tl1'", workspace).Scan(&workspaces); err != nil || workspaces != 1 {
		t.Fatalf("TL1 task does not join its Library workspace %s: %d %v", workspace, workspaces, err)
	}
}

// TL1 before registry.json listed projects in workspaces.json, and TL1 goes
// on listing each one from there until it is next used and registered.
func TestTL1RegistryRowsIncludeUnregisteredLegacyProjects(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, value any) string {
		path := filepath.Join(dir, name)
		payload, _ := json.Marshal(value)
		probePut(t, path, string(payload))
		return path
	}
	project := func(name, config string) map[string]any {
		return map[string]any{"project_name": name, "config_path": config, "code_repo": filepath.Dir(config), "last_used": "2026-04-29T08:12:49"}
	}
	// alpha keeps TL1's default locations; beta's tl1.json moves them.
	alpha := write("alpha/tl1.json", map[string]any{"project_name": "alpha"})
	beta := write("beta/tl1.json", map[string]any{"project_name": "beta", "db_path": "state/beta.db", "transcripts_dir": "/elsewhere/beta"})
	gamma := write("gamma/tl1.json", map[string]any{"project_name": "gamma"})
	delta := write("delta/tl1.json", map[string]any{"project_name": "delta"})
	legacy := write(tl1LegacyRegistryFile, map[string]any{"workspaces": []any{project("alpha", alpha), project("beta", beta), project("gamma", gamma), project("delta", delta)}})
	rows, err := tl1RegistryRows(legacy, os.ReadFile)
	if err != nil || len(rows) != 4 {
		t.Fatalf("legacy rows = %v %v", rows, err)
	}
	if rows[0]["db_path"] != filepath.Join(dir, "alpha.db") || rows[0]["transcripts_dir"] != filepath.Join(dir, "alpha", "transcripts") || rows[0]["installation_id"] != nil {
		t.Fatalf("alpha = %v", rows[0])
	}
	if rows[1]["db_path"] != filepath.Join(dir, "beta", "state", "beta.db") || rows[1]["transcripts_dir"] != "/elsewhere/beta" {
		t.Fatalf("beta = %v", rows[1])
	}
	// Once TL1 registers gamma, and another installation claims delta's
	// database, only alpha and beta remain listed from workspaces.json,
	// whichever registry file the source names.
	registry := write(tl1RegistryFile, map[string]any{"installations": []any{
		map[string]any{"installation_id": "g", "project_name": "gamma", "config_path": gamma, "code_repo": filepath.Dir(gamma), "db_path": filepath.Join(dir, "gamma.db")},
		map[string]any{"installation_id": "d", "project_name": "delta-v2", "config_path": filepath.Join(dir, "delta-v2", "tl1.json"), "code_repo": dir, "db_path": filepath.Join(dir, "delta.db")},
	}})
	for _, path := range []string{registry, legacy} {
		rows, err := tl1RegistryRows(path, os.ReadFile)
		names := []string{}
		for _, row := range rows {
			names = append(names, firstString(row["project_name"]))
		}
		if err != nil || strings.Join(names, ",") != "gamma,delta-v2,alpha,beta" {
			t.Fatalf("rows from %s = %v %v", filepath.Base(path), names, err)
		}
	}
	other := write("other.json", map[string]any{"projects": []any{}})
	if _, err := tl1RegistryRows(other, os.ReadFile); err == nil || !strings.Contains(err.Error(), "installations or workspaces") {
		t.Fatalf("unrecognized registry error = %v", err)
	}
}

func TestTL1IngestsLegacyWorkspaces(t *testing.T) {
	root := filepath.Dir(tl1Fixture(t, 0))
	if err := os.Remove(filepath.Join(root, tl1RegistryFile)); err != nil {
		t.Fatal(err)
	}
	repository, database := filepath.Join(root, "repo"), filepath.Join(root, "tl1.db")
	config := filepath.Join(repository, "tl1.json")
	probePut(t, config, `{"project_name":"fixture","db_path":"../tl1.db","transcripts_dir":"../transcripts"}`)
	payload, _ := json.Marshal(map[string]any{"workspaces": []any{map[string]any{"project_name": "fixture", "config_path": config, "code_repo": repository, "last_used": "2026-04-29T08:12:49"}}})
	legacy := filepath.Join(root, tl1LegacyRegistryFile)
	probePut(t, legacy, string(payload))
	catalog, _ := testCatalog(t)
	adapter, err := MakeAdapter(SourceConfig{Name: "tl1", Kind: "tl1", Path: legacy, Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if result := catalog.Ingest(adapter, nil); result.Error != nil || result.Workspaces != 3 {
		t.Fatalf("ingest = %+v", result)
	}
	// A legacy project has no installation ID; its database identifies it.
	var tasks int
	var project, conversation string
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM tl1_tasks WHERE installation_id=?", database).Scan(&tasks); err != nil || tasks != 3 {
		t.Fatalf("tasks = %d %v", tasks, err)
	}
	if err := catalog.DB.QueryRow("SELECT project FROM tl1_installations WHERE installation_id=?", database).Scan(&project); err != nil || project != "fixture" {
		t.Fatalf("installation project = %q %v", project, err)
	}
	if err := catalog.DB.QueryRow("SELECT conversation_native_id FROM tl1_attempts WHERE attempt_id='att-impl'").Scan(&conversation); err != nil || conversation != database+":att-impl:1-codex.jsonl" {
		t.Fatalf("transcript = %q %v", conversation, err)
	}
}

func TestTL1ReingestCorrectsLegacyProviderInPlace(t *testing.T) {
	catalog, _ := testCatalog(t)
	ingestTL1Fixture(t, catalog, 0)
	native := "install-1:att-impl:1-codex.jsonl"
	var id string
	if err := catalog.DB.QueryRow("SELECT id FROM conversations WHERE native_id=?", native).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// Simulate a catalog written when TL1 transcripts were filed under 'tl1'.
	if _, err := catalog.DB.Exec("UPDATE conversations SET provider='tl1' WHERE native_id=?", native); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec("DELETE FROM meta WHERE key='tl1_native_provider_version'"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	var fingerprint sql.NullString
	if err := catalog.DB.QueryRow("SELECT fingerprint FROM source_states WHERE kind='tl1'").Scan(&fingerprint); err != nil || fingerprint.Valid {
		t.Fatalf("TL1 source not queued for re-read: %v %v", fingerprint, err)
	}
	ingestTL1Fixture(t, catalog, 0)
	var count int
	var provider, newID string
	if err := catalog.DB.QueryRow("SELECT COUNT(*),MAX(provider),MAX(id) FROM conversations WHERE native_id=?", native).Scan(&count, &provider, &newID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || provider != "codex" || newID != id {
		t.Fatalf("legacy TL1 conversation not corrected in place: count=%d provider=%q id=%q (was %q)", count, provider, newID, id)
	}
}

func TestTL1OverviewFindsConcernsWithPrompts(t *testing.T) {
	catalog, _ := testCatalog(t)
	ingestTL1Fixture(t, catalog, 0)
	overview, err := catalog.TL1Overview("", tl1Window{})
	if err != nil {
		t.Fatal(err)
	}
	totals := overview["totals"].(map[string]any)
	if totals["llm_attempts"] != 2 || totals["human_tasks"] != 1 || totals["pending_human"] != 1 {
		t.Fatalf("unexpected totals: %v", totals)
	}
	if cost, _ := totals["cost_usd"].(float64); cost <= 0 {
		t.Fatalf("Codex tokens were not priced: %v", totals["cost_usd"])
	}
	var implement map[string]any
	for _, row := range overview["matrix"].([]map[string]any) {
		if row["flavor"] == "implement" && row["configuration"] == "codex-terra-high" {
			implement = row
		}
	}
	if implement == nil || implement["advanced"] != 1 || implement["default"] != true {
		t.Fatalf("implement matrix row: %v", implement)
	}
	errors := overview["errors"].([]map[string]any)
	if len(errors) != 1 || errors[0]["human_followups"] != 1 || !strings.Contains(firstString(errors[0]["signature"]), "trusted directory") {
		t.Fatalf("error clusters: %v", errors)
	}
	human := overview["human"].(map[string]any)
	if human["error_caused"] != 1 {
		t.Fatalf("human task should be attributed to the upstream error: %v", human)
	}
	reviews := overview["reviews"].(map[string]any)
	if authors := reviews["by_implementation"].([]map[string]any); len(authors) != 1 || authors[0]["configuration"] != "codex-terra-high" || authors[0]["major_or_blocker"] != 1 {
		t.Fatalf("review finding attribution: %v", reviews["by_implementation"])
	}
	prompt := firstString(overview["review_prompt"])
	for _, want := range []string{"fixture", "tl1.json", "Top concerns"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("review prompt is missing %q:\n%s", want, prompt)
		}
	}
	candidate, err := catalog.TL1Candidate("install-1", "cand-1")
	if err != nil {
		t.Fatal(err)
	}
	if tasks := candidate["tasks"].([]map[string]any); len(tasks) != 3 || tasks[1]["disposition"] != tl1Errored {
		t.Fatalf("candidate timeline: %v", tasks)
	}
	flavor, err := catalog.TL1Flavor("install-1", "implement", tl1Window{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstString(flavor["prompt"]), "Fix the bug.") {
		t.Fatalf("flavor prompt should include the template: %s", flavor["prompt"])
	}
	// Scope "current" drops runs of superseded definitions.
	current, err := catalog.TL1Overview("install-1", tl1Window{Scope: "current"})
	if err != nil {
		t.Fatal(err)
	}
	if current["totals"].(map[string]any)["tasks"] != 3 {
		t.Fatalf("fixture tasks all use current shape versions: %v", current["totals"])
	}
}

func TestTL1APIAndMCP(t *testing.T) {
	catalog, config := testCatalog(t)
	ingestTL1Fixture(t, catalog, 4)
	server := NewServer(config, catalog)
	get := func(path string) map[string]any {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, response.Code, response.Body.String())
		}
		value := map[string]any{}
		_ = json.Unmarshal(response.Body.Bytes(), &value)
		return value
	}
	if installations := get("/api/tl1")["installations"].([]any); len(installations) != 1 {
		t.Fatalf("installations: %v", installations)
	}
	if detectors := get("/api/tl1/overview?days=0")["detectors"].([]any); len(detectors) == 0 {
		t.Fatal("overview returned no detectors")
	}
	get("/tl1")
	latest := get("/api/tl1/overview?since=latest")
	if window := latest["window"].(map[string]any); window["mode"] != "latest_enqueue" || latest["enqueues"] == nil {
		t.Fatalf("latest enqueue window: %v", window)
	}
	if bounded := get("/api/tl1/overview?since=2026-09-20T00:00:00Z&until=2026-09-20T10:00:00Z"); bounded["window"].(map[string]any)["tasks"] != float64(0) {
		t.Fatalf("no fixture task was created before 10:00: %v", bounded["window"])
	}

	request := httptest.NewRequest(http.MethodPost, "/api/query/tl1_attempts", strings.NewReader(`{"limit":10,"where":[{"field":"disposition","op":"=","value":"error"}]}`))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var rows struct{ Rows []map[string]any }
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil || response.Code != http.StatusOK || len(rows.Rows) != 5 || rows.Rows[0]["flavor"] != "review" {
		t.Fatalf("tl1_attempts query: %d %s", response.Code, response.Body.String())
	}

	if err := catalog.SetMCPEnabled(true); err != nil {
		t.Fatal(err)
	}
	listed := handleMCP(catalog, map[string]any{"id": 1, "method": "tools/list"})
	names := jsonText(listed)
	for _, tool := range []string{"tl1_overview", "tl1_detector", "tl1_flavor", "tl1_errors", "tl1_candidate"} {
		if !strings.Contains(names, `"`+tool+`"`) {
			t.Errorf("MCP tools/list is missing %s", tool)
		}
	}
	value, err := callMCP(catalog, "tl1_overview", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	compact := value.(map[string]any)
	if len(compact["detectors"].([]map[string]any)) == 0 {
		t.Fatal("five identical failures should produce detectors")
	}
	for _, detector := range compact["detectors"].([]map[string]any) {
		if detector["prompt"] != nil || detector["evidence"] != nil {
			t.Fatal("compact overview should omit prompts and evidence")
		}
		detail, err := callMCP(catalog, "tl1_detector", map[string]any{"id": detector["id"]})
		if err != nil {
			t.Fatal(err)
		}
		if detail.(map[string]any)["id"] != detector["id"] {
			t.Fatalf("tl1_detector returned %v", detail)
		}
	}
}

// TestTL1EnqueueSpikesMatchTL1 ports TL1's own dashboard test: child tasks do
// not count, a burst needs five root tasks within ten-minute gaps, and the
// latest burst starts the default window.
func TestTL1EnqueueSpikesMatchTL1(t *testing.T) {
	tasks := []*tl1Task{}
	add := func(prefix string, count int, created, createdBy, parent, shape string) {
		for index := 0; index < count; index++ {
			tasks = append(tasks, &tl1Task{ID: fmt.Sprintf("%s-%d", prefix, index), Flavor: "flavor", ShapeVersion: shape, CreatedAt: canonicalTime(created), CreatedBy: createdBy, ParentID: parent})
		}
	}
	add("old", 3, "2026-01-01 08:00:00", "", "", "v1")
	add("new", 6, "2026-01-02 12:00:00", "", "", "v1")
	add("child", 6, "2026-01-03 16:00:00", "new-0", "", "v1")
	add("retry", 6, "2026-01-03 16:00:00", "", "new-1", "v1")
	add("latest", 5, "2026-01-04 09:30:00", "", "", "v2")
	spikes := tl1EnqueueSpikes(tasks)
	if len(spikes) != 2 || spikes[0].StartedAt != "2026-01-04T09:30:00.000Z" || spikes[0].RootTasks != 5 || spikes[1].StartedAt != "2026-01-02T12:00:00.000Z" || spikes[1].RootTasks != 6 {
		t.Fatalf("spikes: %+v", spikes)
	}
	// The flavor's v2 definition first ran in the latest enqueue's period.
	if len(spikes[0].Changed) != 1 || spikes[0].Changed[0] != "flavor" || len(spikes[1].Changed) != 0 || len(spikes[0].ChangedDuring) != 0 {
		t.Fatalf("definition changes: %+v", spikes)
	}
	// A revision partway through a period is reported separately.
	add("late", 1, "2026-01-04 11:00:00", "", "", "v3")
	if spikes := tl1EnqueueSpikes(tasks); len(spikes[0].ChangedDuring) != 1 || len(spikes[0].Changed) != 1 {
		t.Fatalf("mid-period change: %+v", spikes[0])
	}
	// A gap longer than ten minutes splits a burst; four roots are not large.
	gapped := []*tl1Task{}
	for index, minute := range []int{0, 5, 10, 21, 25, 30, 35} {
		gapped = append(gapped, &tl1Task{ID: fmt.Sprint(index), CreatedAt: canonicalTime(fmt.Sprintf("2026-01-05 10:%02d:00", minute))})
	}
	if spikes := tl1EnqueueSpikes(gapped); len(spikes) != 0 {
		t.Fatalf("3 + 4 root tasks split by an 11-minute gap are not large enqueues: %+v", spikes)
	}

	since, until, err := tl1ResolveWindow(tl1Window{Since: tl1LatestEnqueue}, spikes, time.Now())
	if err != nil || since != "2026-01-04T09:30:00.000Z" || until != "" {
		t.Fatalf("latest enqueue window: %q %q %v", since, until, err)
	}
	since, until, err = tl1ResolveWindow(tl1Window{Since: "2026-01-02T12:00:00Z", Until: "2026-01-04T09:30:00Z", Days: 7}, spikes, time.Now())
	if err != nil || since != "2026-01-02T12:00:00.000Z" || until != "2026-01-04T09:30:00.000Z" {
		t.Fatalf("enqueue period window: %q %q %v", since, until, err)
	}
	if _, _, err := tl1ResolveWindow(tl1Window{Since: "yesterday-ish"}, spikes, time.Now()); err == nil {
		t.Fatal("an unparseable bound should be rejected")
	}
}
