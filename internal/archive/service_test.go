package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func serveTest(server *Server, method, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func TestReleaseStopsWorkClosesCatalogAndRefusesRequests(t *testing.T) {
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	workerStopped := make(chan struct{})
	server.spawn(func(ctx context.Context) {
		<-ctx.Done()
		close(workerStopped)
	})
	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/release", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated release: %d", unauthorized.Code)
	}
	if response := serveTest(server, http.MethodGet, "/api/health"); response.Code != http.StatusOK {
		t.Fatalf("health before release: %d", response.Code)
	}
	response := serveTest(server, http.MethodPost, "/api/release")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"released":true`) {
		t.Fatalf("release: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-workerStopped:
	default:
		t.Fatal("release answered before background work stopped")
	}
	if err := catalog.DB.Ping(); err == nil {
		t.Fatal("catalog is still open after release")
	}
	if response := serveTest(server, http.MethodGet, "/api/health"); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("request after release: %d %s", response.Code, response.Body.String())
	}
	// A repeated release (a second eject attempt) succeeds at once.
	if response := serveTest(server, http.MethodPost, "/api/release"); response.Code != http.StatusOK {
		t.Fatalf("second release: %d", response.Code)
	}
}

func TestStopWaitsForInFlightRequests(t *testing.T) {
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	if !server.admit() {
		t.Fatal("request refused before stopping")
	}
	if err := server.stop(50 * time.Millisecond); !errors.Is(err, errStopTimeout) {
		t.Fatalf("stop with a request in flight: %v", err)
	}
	if err := catalog.DB.Ping(); err != nil {
		t.Fatalf("catalog closed under an in-flight request: %v", err)
	}
	if server.admit() {
		t.Fatal("new request admitted while stopping")
	}
	server.life.requests.Done()
	if err := server.stop(5 * time.Second); err != nil {
		t.Fatalf("stop after the request finished: %v", err)
	}
	if err := catalog.DB.Ping(); err == nil {
		t.Fatal("catalog is still open")
	}
	if _, err := os.Stat(releasedMarker(catalog.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stop that was not a release marked the library released: %v", err)
	}
}

// gatedSource emits its records one at a time, waiting on gate before each
// record after the first.
type gatedSource struct {
	copyFixture
	emitted chan string
	gate    chan struct{}
}

func (g *gatedSource) Discover(emit func(WorkspaceRecord) error) error {
	for index, record := range g.records {
		if index > 0 {
			<-g.gate
		}
		if err := emit(record); err != nil {
			return err
		}
		g.emitted <- record.SourceID
	}
	return nil
}

func TestStopInterruptsSyncBetweenRecords(t *testing.T) {
	catalog, config := testCatalog(t)
	record := func(id string) WorkspaceRecord {
		return WorkspaceRecord{SourceID: id, SourceKind: "claude", Account: "local", Title: id, Conversations: []ConversationRecord{{
			NativeID: id + "-thread", Provider: "claude", Account: "local",
			Messages: []MessageRecord{{NativeID: id + "-one", Role: "user", Kind: "message", Text: "hello " + id, Selected: true}},
		}}}
	}
	source := &gatedSource{copyFixture: *newCopyFixture(t, record("first"), record("second")), emitted: make(chan string, 2), gate: make(chan struct{})}
	previous := syncAdapter
	syncAdapter = func(SourceConfig) (Adapter, error) { return source, nil }
	t.Cleanup(func() { syncAdapter = previous })
	config.Sources = []SourceConfig{source.config}
	server := NewServer(config, catalog)

	synced := make(chan *httptest.ResponseRecorder)
	go func() { synced <- serveTest(server, http.MethodPost, "/api/sources/sync") }()
	if id := <-source.emitted; id != "first" {
		t.Fatalf("first record %q", id)
	}
	released := make(chan error)
	go func() { released <- server.stop(5 * time.Second) }()
	for server.life.ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	close(source.gate)
	response := <-synced
	if err := <-released; err != nil {
		t.Fatalf("stop: %v", err)
	}
	var body struct {
		OK      bool           `json:"ok"`
		Results []IngestResult `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || len(body.Results) != 1 || body.Results[0].Workspaces != 1 || !strings.Contains(fmt.Sprint(body.Results[0].Error), "interrupted") {
		t.Fatalf("interrupted sync reported %s", response.Body.String())
	}
	if run := server.activity()["runs"].([]map[string]any)[0]; run["state"] != "interrupted" {
		t.Fatalf("run state %v", run["state"])
	}
	reopened, err := OpenCatalog(catalog.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var workspaces int
	var coverage string
	if err := reopened.DB.QueryRow("SELECT COUNT(*) FROM workspaces").Scan(&workspaces); err != nil {
		t.Fatal(err)
	}
	if err := reopened.DB.QueryRow("SELECT coverage FROM source_states WHERE source_name=?", source.config.Name).Scan(&coverage); err != nil {
		t.Fatal(err)
	}
	if workspaces != 1 || coverage != "indexing" {
		t.Fatalf("after interruption: %d workspaces, coverage %q; want the first record kept and the source resumable", workspaces, coverage)
	}
}

func TestDriveGoneNoticesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catalog")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gone := driveGone(ctx, dir, 5*time.Millisecond)
	select {
	case <-gone:
		t.Fatal("present directory reported gone")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("missing directory not noticed")
	}
}

func TestAuthCookieIsPerPort(t *testing.T) {
	catalog, config := testCatalog(t)
	config.Port = 8766
	server := NewServer(config, catalog)
	login := httptest.NewRecorder()
	server.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/?token=test-token", nil))
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "aiwa_token_8766" {
		t.Fatalf("login cookies %v", cookies)
	}
	check := func(server *Server, name string, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		request.AddCookie(&http.Cookie{Name: name, Value: "test-token"})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("cookie %s: %d, want %d", name, response.Code, want)
		}
	}
	check(server, "aiwa_token_8766", http.StatusOK)
	check(server, "aiwa_token_8765", http.StatusUnauthorized)
	check(server, "aiwa_token", http.StatusUnauthorized)
	config.Port = 8765
	check(NewServer(config, catalog), "aiwa_token", http.StatusOK)
}

func TestServeRecordsTheLibraryOnlyOnceItHasItsPort(t *testing.T) {
	support := t.TempDir()
	t.Setenv("PHAROS_SUPPORT_DIR", support)
	config, err := InitLibrary(t.TempDir(), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	setLibraryPort(t, config, taken.Addr().(*net.TCPAddr).Port)
	marker := releasedMarker(config.CatalogPath)
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"--config", config.Path, "serve"}); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("serve on a taken port: %v", err)
	}
	if _, err := os.Stat(filepath.Join(support, "library.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a service that never got its port recorded the library: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a service that never got its port ended the release: %v", err)
	}
}

// TestServeHelper is the service process for TestServeStopsCleanlyOnSIGTERM.
func TestServeHelper(t *testing.T) {
	config := os.Getenv("PHAROS_TEST_SERVE_CONFIG")
	if config == "" {
		t.Skip("helper process")
	}
	if support := os.Getenv("PHAROS_TEST_SERVE_SUPPORT"); support != "" {
		os.Setenv("PHAROS_SUPPORT_DIR", support)
	}
	if err := Run([]string{"--config", config, "serve"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestServeStopsCleanlyOnSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a service process")
	}
	dir := t.TempDir()
	config, err := InitLibrary(dir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	text, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	text = []byte(strings.Replace(string(text), fmt.Sprintf("port = %d", libraryPort), fmt.Sprintf("port = %d", port), 1))
	if err := os.WriteFile(config.Path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releasedMarker(config.CatalogPath), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Serving a library writes per-Mac files; they must land in the test's
	// support directory, never under HOME.
	support, home := t.TempDir(), t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestServeHelper$")
	command.Env = append(os.Environ(), "PHAROS_TEST_SERVE_CONFIG="+config.Path, "PHAROS_TEST_SERVE_SUPPORT="+support, "HOME="+home)
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	defer command.Process.Kill()
	health := fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
	ready := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && !ready; time.Sleep(50 * time.Millisecond) {
		request, _ := http.NewRequest(http.MethodGet, health, nil)
		request.Header.Set("Authorization", "Bearer "+config.APIToken)
		if response, err := http.DefaultClient.Do(request); err == nil {
			response.Body.Close()
			ready = response.StatusCode == http.StatusOK
		}
	}
	if !ready {
		t.Fatalf("service did not become ready: %s", stderr.String())
	}
	if _, err := os.Stat(config.CatalogPath + "-wal"); err != nil {
		t.Fatalf("expected an open WAL while serving: %v", err)
	}
	if _, err := os.Stat(mcpLauncherPath(support)); err != nil {
		t.Fatalf("serve did not install the MCP launcher in the support directory: %v", err)
	}
	if pointer, err := readLibraryPointer(filepath.Join(support, "library.json")); err != nil || pointer.LibraryDir != filepath.Dir(config.Path) {
		t.Fatalf("serve did not record the library for the MCP launcher: %+v %v", pointer, err)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("serve wrote into HOME: %v", entries)
	}
	if _, err := os.Stat(releasedMarker(config.CatalogPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serve kept an earlier release's marker: %v", err)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("service exited with %v: %s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("service did not exit after SIGTERM")
	}
	// SQLite removes the WAL when the last connection closes cleanly.
	if _, err := os.Stat(config.CatalogPath + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("catalog was not closed cleanly: WAL stat %v", err)
	}
}
