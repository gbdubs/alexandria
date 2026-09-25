package archive

import (
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

const (
	probeID1 = "11111111-1111-4111-8111-111111111111"
	probeID2 = "22222222-2222-4222-8222-222222222222"
	probeID3 = "33333333-3333-4333-8333-333333333333"
	probeID4 = "44444444-4444-4444-8444-444444444444"
	probeID5 = "55555555-5555-4555-8555-555555555555"
	probeID6 = "66666666-6666-4666-8666-666666666666"
)

// probeHome is a synthetic home directory with no agent environment and no
// login shell, so a probe sees only what a test puts there.
func probeHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for _, name := range probeEnvNames {
		t.Setenv(name, "")
	}
	stubShellEnv(t, nil)
	return home
}

func stubShellEnv(t *testing.T, values map[string]string) {
	previous := probeShellEnv
	probeShellEnv = func([]string) (map[string]string, error) { return values, nil }
	t.Cleanup(func() { probeShellEnv = previous })
}

func probePut(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sqliteFixture(t *testing.T, path string, tables ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range tables {
		if _, err := db.Exec("CREATE TABLE " + table + "(id TEXT)"); err != nil {
			t.Fatal(err)
		}
	}
}

// TL1 before registry.json listed its projects in workspaces.json.
func TestProbeFindsTL1BeforeRegistry(t *testing.T) {
	home := probeHome(t)
	legacy := filepath.Join(home, "code", "legacy")
	probePut(t, filepath.Join(legacy, "tl1.json"), `{"project_name":"legacy"}`)
	probePut(t, filepath.Join(home, ".tl1", "legacy.db"), "db")
	// A project inside ~/Documents is counted, never opened.
	private := filepath.Join(home, "Documents", "private")
	probePut(t, filepath.Join(private, "tl1.json"), `{"project_name":"private"}`)
	workspaces, _ := json.Marshal(map[string]any{"workspaces": []map[string]any{
		{"project_name": "legacy", "config_path": filepath.Join(legacy, "tl1.json"), "code_repo": legacy},
		{"project_name": "private", "config_path": filepath.Join(private, "tl1.json"), "code_repo": private},
	}})
	probePut(t, filepath.Join(home, ".tl1", tl1LegacyRegistryFile), string(workspaces))
	if err := os.Chmod(filepath.Join(home, "Documents"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, "Documents"), 0o755) })

	report := probeSources(home, Config{}, nil)
	tl1 := candidateNamed(t, report, "tl1")
	if tl1.Status != "found" || tl1.Path != filepath.Join(home, ".tl1", tl1LegacyRegistryFile) || tl1.Count != 1 || !tl1.Recommended ||
		!strings.Contains(tl1.Detail, "Projects: legacy") || !strings.Contains(tl1.Detail, "1 installations in protected folders") {
		t.Fatalf("tl1 = %+v", tl1)
	}
	checked := map[string]string{}
	for _, location := range report.Checked {
		if location.Kind == "tl1" {
			checked[location.Path] = location.Status
		}
	}
	if checked["~/.tl1/registry.json"] != "missing" || checked["~/.tl1/workspaces.json"] != "found" {
		t.Fatalf("checked TL1 locations = %v", checked)
	}
}

func candidateNamed(t *testing.T, report ProbeReport, name string) ProbeCandidate {
	t.Helper()
	for _, candidate := range report.Candidates {
		if candidate.Name == name {
			return candidate
		}
	}
	t.Fatalf("no candidate %q in %s", name, jsonText(report.Candidates))
	return ProbeCandidate{}
}

func TestProbeFindsKnownLocationsOnly(t *testing.T) {
	home := probeHome(t)
	claude := filepath.Join(home, ".claude", "projects", "-Users-me-repo")
	probePut(t, filepath.Join(claude, probeID1+".jsonl"), "{}\n")
	probePut(t, filepath.Join(claude, probeID2+".jsonl"), "{}\n")
	probePut(t, filepath.Join(claude, probeID1, "subagents", "agent-a.jsonl"), "{}\n")
	probePut(t, filepath.Join(home, ".claude", "settings.json"), `{"cleanupPeriodDays": 90}`)
	probePut(t, filepath.Join(home, ".claude-work", "projects", "-repo", probeID3+".jsonl"), "{}\n")
	if err := os.MkdirAll(filepath.Join(home, ".claude-empty", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	probePut(t, filepath.Join(home, ".claude.json"), "{}")
	probePut(t, filepath.Join(home, ".codex", "sessions", "2026", "09", "01", "rollout-2026-09-01T00-00-00-"+probeID4+".jsonl"), "{}\n")
	probePut(t, filepath.Join(home, ".codex", "history.jsonl"), "{}\n")
	probePut(t, filepath.Join(home, "alt-claude", "projects", "-x", probeID5+".jsonl"), "{}\n")
	probePut(t, filepath.Join(home, "work-codex", "archived_sessions", "rollout-2026-09-02T00-00-00-"+probeID6+".jsonl"), "{}\n")
	conductor := filepath.Join(home, "Library", "Application Support", "com.conductor.app")
	sqliteFixture(t, filepath.Join(conductor, "conductor.db"), "sessions", "session_messages")
	sqliteFixture(t, filepath.Join(conductor, "cache.db"), "session_messages")
	// TL1 installations: one real, one from a test run in a temporary
	// directory, one whose database is gone.
	temporary := filepath.Join(home, "tmp")
	previous := probeTempDirs
	probeTempDirs = func() []string { return []string{temporary} }
	t.Cleanup(func() { probeTempDirs = previous })
	probePut(t, filepath.Join(home, ".tl1", "real.db"), "db")
	probePut(t, filepath.Join(home, ".tl1", "real.toml"), "")
	probePut(t, filepath.Join(temporary, "run", "tl1.db"), "db")
	probePut(t, filepath.Join(temporary, "run", "tl1.toml"), "")
	for _, dir := range []string{filepath.Join(home, "code", "real"), filepath.Join(temporary, "run", "repo")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	registry, _ := json.Marshal(map[string]any{"installations": []map[string]any{
		{"installation_id": "a", "project_name": "real", "db_path": "~/.tl1/real.db", "config_path": "~/.tl1/real.toml", "code_repo": "~/code/real"},
		{"installation_id": "b", "project_name": "pytest", "db_path": filepath.Join(temporary, "run", "tl1.db"), "config_path": filepath.Join(temporary, "run", "tl1.toml"), "code_repo": filepath.Join(temporary, "run", "repo")},
		{"installation_id": "c", "project_name": "gone", "db_path": "~/.tl1/gone.db", "config_path": "~/.tl1/real.toml", "code_repo": "~/code/real"},
	}})
	probePut(t, filepath.Join(home, ".tl1", "registry.json"), string(registry))
	// A profile inside ~/Documents, reached both through a symbolic link and
	// through the login shell's CLAUDE_CONFIG_DIR, must never be entered.
	documents := filepath.Join(home, "Documents")
	probePut(t, filepath.Join(documents, "claude", "projects", "-x", "secret.jsonl"), "{}\n")
	if err := os.Symlink(filepath.Join(documents, "claude"), filepath.Join(home, ".claude-docs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(documents, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(documents, 0o755) })
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "alt-claude"))
	stubShellEnv(t, map[string]string{"CODEX_HOME": "~/work-codex", "CLAUDE_CONFIG_DIR": filepath.Join(documents, "claude")})

	report := probeSources(home, Config{}, nil)
	names := []string{}
	for _, candidate := range report.Candidates {
		names = append(names, candidate.Name)
	}
	if got, want := strings.Join(names, ","), "claude,claude-docs,claude-work,codex,conductor,tl1,claude-alt-claude,codex-work-codex,chatgpt"; got != want {
		t.Fatalf("candidates = %s, want %s", got, want)
	}
	main := candidateNamed(t, report, "claude")
	if main.Status != "found" || main.Count != 2 || main.Files != 3 || main.Unit != "session" || !main.Recommended || main.Oldest == "" {
		t.Fatalf("claude = %+v", main)
	}
	if main.RetentionDays != 90 || !strings.Contains(main.Retention, "90 days") || !strings.Contains(main.Retention, "~/.claude/settings.json") {
		t.Fatalf("claude retention = %d %q", main.RetentionDays, main.Retention)
	}
	if work := candidateNamed(t, report, "claude-work"); work.Count != 1 || work.RetentionDays != 30 || !strings.Contains(work.Retention, "default") || work.FoundBy != "~/.claude* directory" {
		t.Fatalf("claude-work = %+v", work)
	}
	protected := candidateNamed(t, report, "claude-docs")
	if protected.Status != "protected" || protected.Count != 0 || protected.Recommended || !protected.Acceptable || !strings.Contains(protected.FoundBy, "CLAUDE_CONFIG_DIR (login shell)") {
		t.Fatalf("protected profile = %+v", protected)
	}
	if alt := candidateNamed(t, report, "claude-alt-claude"); alt.Count != 1 || alt.FoundBy != "CLAUDE_CONFIG_DIR (service environment)" {
		t.Fatalf("CLAUDE_CONFIG_DIR profile = %+v", alt)
	}
	if codex := candidateNamed(t, report, "codex"); codex.Count != 1 || codex.Files != 1 || codex.Path != filepath.Join(home, ".codex") {
		t.Fatalf("codex = %+v", codex)
	}
	if codex := candidateNamed(t, report, "codex-work-codex"); codex.Count != 1 || codex.FoundBy != "CODEX_HOME (login shell)" {
		t.Fatalf("CODEX_HOME = %+v", codex)
	}
	if conductor := candidateNamed(t, report, "conductor"); conductor.Count != 1 || !strings.Contains(conductor.Detail, "conductor.db (sessions, session_messages)") || strings.Contains(conductor.Detail, "cache.db") {
		t.Fatalf("conductor = %+v", conductor)
	}
	if tl1 := candidateNamed(t, report, "tl1"); tl1.Count != 1 || !strings.Contains(tl1.Detail, "Projects: real") || !strings.Contains(tl1.Detail, "2 temporary or missing") {
		t.Fatalf("tl1 = %+v", tl1)
	}
	if chatgpt := candidateNamed(t, report, "chatgpt"); chatgpt.Status != "manual" || chatgpt.Acceptable || !strings.Contains(chatgpt.Detail, "conversations.json") {
		t.Fatalf("chatgpt = %+v", chatgpt)
	}
	empty := false
	for _, location := range report.Checked {
		empty = empty || (location.Path == "~/.claude-empty/projects" && location.Status == "empty")
	}
	if !empty || len(report.Environment) != 3 || report.ShellEnv != "read" {
		t.Fatalf("checked = %+v environment = %+v shell = %q", report.Checked, report.Environment, report.ShellEnv)
	}
}

func TestProtectedLocations(t *testing.T) {
	home := "/Users/me"
	for path, want := range map[string]bool{
		"/Users/me/Documents":                                     true,
		"/Users/me/documents/x":                                   true,
		"/Users/me/Desktop/a/b":                                   true,
		"/Users/me/Library/Containers/com.x/Data":                 true,
		"/Users/me/Library/Mobile Documents/com~apple~CloudDocs":  true,
		"/Users/me/Library/Application Support/com.conductor.app": false,
		"/Users/me/.claude/projects":                              false,
		"/Users/me/DocumentsArchive":                              false,
		"/Volumes/Other/claude":                                   true,
		"/opt/claude":                                             false,
	} {
		if got := protectedLocation(home, path); got != want {
			t.Errorf("protectedLocation(%s) = %t, want %t", path, got, want)
		}
	}
}

func TestLoginShellEnvToleratesNoiseAndHangs(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/profiles/claude work")
	t.Setenv("CODEX_HOME", "")
	// Arguments are -l -i -c SCRIPT.
	noisy := script("noisy", "echo 'Welcome!'\nprintf 'prompt junk'\neval \"$4\"\necho 'logout junk'\nexit 3\n")
	values, err := loginShellEnv(noisy, probeEnvNames, 2*time.Second)
	if err != nil || values["CLAUDE_CONFIG_DIR"] != "/profiles/claude work" || len(values) != 1 {
		t.Fatalf("noisy shell = %v, %v", values, err)
	}
	started := time.Now()
	if _, err := loginShellEnv(script("hangs", "sleep 30 &\nsleep 30\n"), probeEnvNames, 300*time.Millisecond); err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("hanging shell returned %v after %s", err, time.Since(started))
	}
	// A background job holding stdout open must not hold up the probe.
	started = time.Now()
	values, err = loginShellEnv(script("holder", "eval \"$4\"\nsleep 5 &\n"), probeEnvNames, 2*time.Second)
	if err != nil || values["CLAUDE_CONFIG_DIR"] == "" || time.Since(started) > 1500*time.Millisecond {
		t.Fatalf("background job: %v, %v after %s", values, err, time.Since(started))
	}
	if _, err := loginShellEnv(script("silent", "exit 0\n"), probeEnvNames, time.Second); err == nil {
		t.Fatal("a shell that printed nothing was accepted")
	}
}

// writeLibrary writes library.toml with extra appended and returns its path.
func probeLibrary(t *testing.T, extra string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, libraryConfigName)
	probePut(t, path, "library = true\ncatalog_path = \"catalog/catalog.sqlite3\"\napi_token = \"library-token\"\n"+extra)
	return path
}

func TestLibraryMergesCurrentHostSources(t *testing.T) {
	home := probeHome(t)
	path := probeLibrary(t, `
[[sources]]
name = "claude"
kind = "claude"
path = "/shared/claude"

[[sources]]
name = "export"
kind = "chatgpt"
path = "imports/chatgpt"
`)
	dir := filepath.Dir(path)
	probePut(t, filepath.Join(dir, "hosts", "host-a.toml"), `# Pharos sources for Mac host-a.
[[sources]]
name = "claude"
kind = "claude"
path = "~/.claude/projects"

[[sources]]
name = "codex"
kind = "codex"
path = "~/.codex"
enabled = false
`)
	useHost(t, "host-a")
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(dir, "hosts", "host-a.toml")
	want := []SourceConfig{
		{Name: "claude", Kind: "claude", Path: filepath.Join(home, ".claude", "projects"), Enabled: true, File: host},
		{Name: "codex", Kind: "codex", Path: filepath.Join(home, ".codex"), Enabled: false, File: host},
		{Name: "export", Kind: "chatgpt", Path: filepath.Join(dir, "imports", "chatgpt"), Enabled: true, File: path},
	}
	if len(config.Sources) != len(want) {
		t.Fatalf("sources = %+v", config.Sources)
	}
	for index, source := range config.Sources {
		if source.Name != want[index].Name || source.Kind != want[index].Kind || source.Path != want[index].Path || source.Enabled != want[index].Enabled || source.File != want[index].File {
			t.Errorf("source %d = %+v, want %+v", index, source, want[index])
		}
	}
	// Another Mac sees only what library.toml shares.
	useHost(t, "host-b")
	config, err = LoadConfig(path)
	if err != nil || len(config.Sources) != 2 || config.Sources[0].Path != "/shared/claude" || config.Sources[1].Name != "export" {
		t.Fatalf("host-b sources = %+v, %v", config.Sources, err)
	}
	// A per-user configuration never reads host files.
	useHost(t, "host-a")
	legacy := filepath.Join(dir, "archive.toml")
	probePut(t, legacy, "[[sources]]\nname = \"claude\"\nkind = \"claude\"\npath = \"/legacy\"\n")
	probePut(t, filepath.Join(dir, "hosts", "host-a.toml"), "[[sources]]\nname = \"codex\"\nkind = \"codex\"\npath = \"~/.codex\"\n")
	config, err = LoadConfig(legacy)
	if err != nil || len(config.Sources) != 1 || config.Sources[0].File != legacy {
		t.Fatalf("legacy sources = %+v, %v", config.Sources, err)
	}
}

func probeServer(t *testing.T, path string) *Server {
	t.Helper()
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { catalog.Close() })
	return NewServer(config, catalog)
}

func probeCall(t *testing.T, server *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+server.Config().APIToken)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	decoded := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("%s %s: %v: %s", method, path, err, response.Body.String())
	}
	return response.Code, decoded
}

func probeReadText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSourceToggleWritesTheFileThatDefinesTheSource(t *testing.T) {
	probeHome(t)
	useHost(t, "host-a")
	path := probeLibrary(t, "\n[[sources]]\nname = \"export\"\nkind = \"chatgpt\"\npath = \"imports\"\n\n[[sources]]\nname = \"claude\"\nkind = \"claude\"\npath = \"/shared\"\n")
	host := filepath.Join(filepath.Dir(path), "hosts", "host-a.toml")
	probePut(t, host, "[[sources]]\nname = \"claude\"\nkind = \"claude\"\npath = \"~/.claude/projects\"\nenabled = true\n")
	server := probeServer(t, path)
	library := probeReadText(t, path)
	if code, body := probeCall(t, server, http.MethodPost, "/api/sources/claude/enabled", `{"enabled":false}`); code != 200 {
		t.Fatalf("toggle claude: %d %v", code, body)
	}
	if !strings.Contains(probeReadText(t, host), "enabled = false") || probeReadText(t, path) != library {
		t.Fatalf("host file = %s\nlibrary.toml = %s", probeReadText(t, host), probeReadText(t, path))
	}
	if code, body := probeCall(t, server, http.MethodPost, "/api/sources/export/enabled", `{"enabled":false}`); code != 200 {
		t.Fatalf("toggle export: %d %v", code, body)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range reloaded.Sources {
		if source.Enabled {
			t.Fatalf("%s is still enabled on disk: %+v", source.Name, reloaded.Sources)
		}
	}
	for _, source := range server.Config().Sources {
		if source.Enabled {
			t.Fatalf("%s still enabled in the running config", source.Name)
		}
	}
}

func TestProbeAcceptWritesHostFileAndReloadsSources(t *testing.T) {
	home := probeHome(t)
	useHost(t, "host-a")
	probePut(t, filepath.Join(home, ".claude", "projects", "-r", probeID1+".jsonl"), "{}\n")
	probePut(t, filepath.Join(home, ".claude-work", "projects", "-r", probeID3+".jsonl"), "{}\n")
	probePut(t, filepath.Join(home, ".codex", "sessions", "rollout-2026-09-01T00-00-00-"+probeID4+".jsonl"), "{}\n")
	path := probeLibrary(t, "")
	host := filepath.Join(filepath.Dir(path), "hosts", "host-a.toml")
	server := probeServer(t, path)
	if code, status := probeCall(t, server, http.MethodGet, "/api/probe/status", ""); code != 200 || status["needs_onboarding"] != true || status["host_file_display"] != "hosts/host-a.toml" {
		t.Fatalf("status = %d %v", code, status)
	}
	for body, problem := range map[string]string{
		`{"accept":["nope"]}`:    "no candidate",
		`{"accept":["chatgpt"]}`: "cannot be added",
		`{"accept":["claude-work"],"names":{"claude-work":"a b"}}`:             "must start with",
		`{"accept":["claude","claude-work"],"names":{"claude-work":"claude"}}`: "already exists",
	} {
		if code, result := probeCall(t, server, http.MethodPost, "/api/probe/accept", body); code != 400 || !strings.Contains(firstString(result["error"]), problem) {
			t.Fatalf("%s: %d %v", body, code, result)
		}
	}
	if _, err := os.Stat(host); !os.IsNotExist(err) {
		t.Fatalf("a rejected request wrote the host file: %v", err)
	}
	code, result := probeCall(t, server, http.MethodPost, "/api/probe/accept", `{"accept":["claude"],"decline":["codex","claude-work"],"names":{"claude":"claude-mbp"}}`)
	if code != 200 || jsonText(result["added"]) != `["claude-mbp"]` || jsonText(result["declined"]) != `["codex","claude-work"]` {
		t.Fatalf("accept = %d %v", code, result)
	}
	text := probeReadText(t, host)
	for _, fragment := range []string{"# Pharos sources for Mac host-a (user tester).", "name = \"claude-mbp\"\nkind = \"claude\"\npath = \"~/.claude/projects\"\nenabled = true", "name = \"codex\"\nkind = \"codex\"\npath = \"~/.codex\"\nenabled = false"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("host file lacks %q:\n%s", fragment, text)
		}
	}
	sources := map[string]SourceConfig{}
	for _, source := range server.Config().Sources {
		sources[source.Name] = source
	}
	if !sources["claude-mbp"].Enabled || sources["codex"].Enabled || sources["claude-mbp"].File != host || sources["claude-mbp"].Path != filepath.Join(home, ".claude", "projects") {
		t.Fatalf("running sources = %+v", sources)
	}
	if _, status := probeCall(t, server, http.MethodGet, "/api/probe/status", ""); status["needs_onboarding"] != false || status["host_sources"] != float64(3) {
		t.Fatalf("status after accept = %v", status)
	}
	report := ProbeSources(server.Config(), server.Catalog)
	if claude := candidateNamed(t, report, "claude-mbp"); claude.Configured == nil || claude.Configured.Scope != "host" || claude.Recommended {
		t.Fatalf("accepted candidate = %+v", claude)
	}
	// Accepting a remembered, paused source switches it on in place.
	code, result = probeCall(t, server, http.MethodPost, "/api/probe/accept", `{"accept":["codex"]}`)
	if code != 200 || jsonText(result["enabled"]) != `["codex"]` || strings.Count(probeReadText(t, host), "\n[[sources]]\n") != 3 {
		t.Fatalf("enable codex = %d %v\n%s", code, result, probeReadText(t, host))
	}
	if source, _ := server.configuredSource("codex"); !source.Enabled {
		t.Fatal("codex was not enabled in the running config")
	}
}

func TestProbeAcceptRefusesPerUserConfig(t *testing.T) {
	probeHome(t)
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	if code, status := probeCall(t, server, http.MethodGet, "/api/probe/status", ""); code != 200 || status["library"] != false || status["needs_onboarding"] != false {
		t.Fatalf("status = %d %v", code, status)
	}
	if code, _ := probeCall(t, server, http.MethodGet, "/api/probe", ""); code != 200 {
		t.Fatalf("probe = %d", code)
	}
	if code, result := probeCall(t, server, http.MethodPost, "/api/probe/accept", `{"accept":["claude"]}`); code != 400 || !strings.Contains(firstString(result["error"]), "not a portable library") {
		t.Fatalf("accept = %d %v", code, result)
	}
	if err := runProbeCLI(config, catalog, []string{"--accept", "claude"}); err == nil || !strings.Contains(err.Error(), "not a portable library") {
		t.Fatalf("CLI accept = %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(config.Path), hostsDirName)); !os.IsNotExist(err) {
		t.Fatalf("hosts directory created for a per-user config: %v", err)
	}
}

func TestProbeOverlapNamesTheMacsThatIndexedSessions(t *testing.T) {
	home := probeHome(t)
	catalog, config := testCatalog(t)
	at := "2026-09-01T00:00:00Z"
	indexed := WorkspaceRecord{SourceID: probeID1, SourceKind: "claude", Account: "local", Title: "Indexed", ActivityAt: at,
		Conversations: []ConversationRecord{{NativeID: probeID1, Provider: "claude", Account: "local", Origin: "/Users/other/.claude/projects/-r/" + probeID1 + ".jsonl",
			Coverage: "complete", StartedAt: at, EndedAt: at, Messages: []MessageRecord{{NativeID: "m1", Role: "user", Kind: "message", Text: "hello", CreatedAt: at, Selected: true}}}}}
	ingestAs(t, catalog, "host-b", newCopyFixture(t, indexed))
	useHost(t, "host-a")
	probePut(t, filepath.Join(home, ".claude", "projects", "-r", probeID1+".jsonl"), "{}\n")
	probePut(t, filepath.Join(home, ".claude", "projects", "-r", probeID2+".jsonl"), "{}\n")
	overlap := candidateNamed(t, probeSources(home, config, catalog), "claude").Overlap
	if overlap == nil || overlap.Sampled != 2 || overlap.InLibrary != 1 || overlap.Estimated != 1 || len(overlap.Hosts) != 1 || overlap.Hosts[0].Label != "Mac host-b" || overlap.Hosts[0].Current {
		t.Fatalf("overlap = %+v", overlap)
	}
	if want := "1 of its 2 sessions is already in the library, indexed from Mac host-b."; overlap.Summary != want {
		t.Fatalf("summary = %q, want %q", overlap.Summary, want)
	}
	sampled := overlapSummary(ProbeOverlap{Sampled: 50, Total: 1639, InLibrary: 45, Hosts: []ProbeOverlapHost{{Label: "Mac Studio"}, {Label: "x", Current: true}}})
	if sampled != "45 of 50 sampled sessions are already in the library, indexed from Mac Studio and this Mac." {
		t.Fatalf("sampled summary = %q", sampled)
	}
	if ids := sampleSessions([]probeSession{{"a", time.Unix(3, 0)}, {"b", time.Unix(1, 0)}, {"c", time.Unix(2, 0)}, {"d", time.Unix(4, 0)}}, 2); jsonText(ids) != `["b","a"]` {
		t.Fatalf("sample = %v", ids)
	}
}

func TestProbeReportsAnEmptyProfileNamedByTheEnvironment(t *testing.T) {
	home := probeHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude-new", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubShellEnv(t, map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude-new"})
	report := probeSources(home, Config{}, nil)
	if fresh := candidateNamed(t, report, "claude-new"); fresh.Status != "empty" || fresh.Recommended || !fresh.Acceptable || fresh.FoundBy != "CLAUDE_CONFIG_DIR (login shell)" {
		t.Fatalf("empty profile = %+v", fresh)
	}
}
