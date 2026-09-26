package archive

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const testVolume = "uuid:TEST-VOLUME"

func testIdentity(string) string { return testVolume }

// closedPort is a loopback port nothing listens on, so a library using it
// never forwards to a real service (8765/8766 may be the user's).
func closedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func setLibraryPort(t *testing.T, config Config, port int) {
	t.Helper()
	data, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	updated := regexp.MustCompile(`(?m)^port = \d+$`).ReplaceAllString(string(data), "port = "+strconv.Itoa(port))
	if updated == string(data) && !strings.Contains(updated, "port = "+strconv.Itoa(port)) {
		t.Fatal("library.toml has no port line")
	}
	if err := os.WriteFile(config.Path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

// testLibrary creates a library in dir holding one indexed workspace.
func testLibrary(t *testing.T, dir string) Config {
	t.Helper()
	config, err := InitLibrary(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	setLibraryPort(t, config, closedPort(t))
	if config, err = LoadConfig(config.Path); err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	ingestMCPFixture(t, catalog)
	return config
}

func ingestMCPFixture(t *testing.T, catalog *Catalog) {
	t.Helper()
	export := filepath.Join(t.TempDir(), "conversations.json")
	payload, _ := json.Marshal(map[string]any{"workspaces": []any{map[string]any{
		"id": "work-1", "title": "Parser maintenance", "activity_at": "2026-09-20T12:00:00Z",
		"repository": map[string]any{"canonical_remote": "github.com/acme/parser", "display_name": "parser"},
		"conversations": []any{map[string]any{"id": "parser-session", "provider": "codex", "messages": []any{
			map[string]any{"id": "parser-question", "role": "user", "text": "Fix the tokenizer parser bug"},
			map[string]any{"id": "parser-answer", "role": "assistant", "text": "Resolved the parser issue <with markup> & more."},
		}}},
	}}})
	if err := os.WriteFile(export, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := MakeAdapter(SourceConfig{Name: "fixture", Kind: "canonical", Path: export, Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if result := catalog.Ingest(adapter, nil); result.Error != nil {
		t.Fatal(result.Error)
	}
	if err := catalog.refreshAllLibrary(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// startMCP runs server over stdio pipes, as one long-lived MCP process.
func startMCP(t *testing.T, server *mcpServer) func(method string, params map[string]any) map[string]any {
	t.Helper()
	input, requests := io.Pipe()
	responses, output := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveMCP(input, output, server.handle, server.afterAnswer); output.Close() }()
	t.Cleanup(func() {
		requests.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	reader := bufio.NewReader(responses)
	id := 0
	return func(method string, params map[string]any) map[string]any {
		t.Helper()
		id++
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := requests.Write(append(payload, '\n')); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		if response["id"] != float64(id) {
			t.Fatalf("response for another request: %s", line)
		}
		return response
	}
}

func toolText(t *testing.T, response map[string]any) (string, bool) {
	t.Helper()
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("not a tool result: %#v", response)
	}
	content := result["content"].([]any)
	return content[0].(map[string]any)["text"].(string), result["isError"] == true
}

func fixtureWorkspace(t *testing.T, config Config) string {
	t.Helper()
	db, err := sql.Open("sqlite", catalogDSN(config.CatalogPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id string
	if err := db.QueryRow("SELECT id FROM workspaces WHERE title='Parser maintenance'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func mcpCallCount(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", catalogDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM mcp_calls").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestMCPServerOutlivesAnUnavailableLibrary(t *testing.T) {
	root := t.TempDir()
	pointer := filepath.Join(root, "support", "library.json")
	drive := filepath.Join(root, "drive")
	library := filepath.Join(drive, "Pharos")
	server := newMCPServer("", pointer)
	server.identity = testIdentity
	call := startMCP(t, server)
	search := map[string]any{"name": "search_work", "arguments": map[string]any{"query": "parser"}}

	if response := call("initialize", map[string]any{"protocolVersion": "2025-06-18"}); response["result"].(map[string]any)["serverInfo"] == nil {
		t.Fatalf("initialize without a library: %#v", response)
	}
	if tools := call("tools/list", nil)["result"].(map[string]any)["tools"].([]any); len(tools) != len(mcpTools) {
		t.Fatalf("tools/list without a library listed %d tools", len(tools))
	}
	if text, isError := toolText(t, call("tools/call", search)); !isError || !strings.Contains(text, "Pharos library is not connected: no library has been opened on this Mac") {
		t.Fatalf("call without library.json: %v %s", isError, text)
	}

	if err := os.MkdirAll(filepath.Dir(pointer), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pointer, []byte(`{"library_dir":`+strconv.Quote(library)+`,"volume_uuid":"TEST-VOLUME","updated_at":"2026-09-24T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if text, isError := toolText(t, call("tools/call", search)); !isError || !strings.Contains(text, "Pharos library is not connected: "+filepath.Join(library, "library.toml")+" is missing (volume TEST-VOLUME)") {
		t.Fatalf("call with the drive absent: %v %s", isError, text)
	}

	config := testLibrary(t, library)
	text, isError := toolText(t, call("tools/call", search))
	if isError || !strings.Contains(text, "Parser maintenance") {
		t.Fatalf("call after the library appeared: %v %s", isError, text)
	}
	if count := mcpCallCount(t, config.CatalogPath); count != 1 {
		t.Fatalf("recorded %d calls, want 1", count)
	}

	// Unplugged, then back: the same process recovers without a restart.
	if err := os.Rename(drive, drive+"-away"); err != nil {
		t.Fatal(err)
	}
	if text, isError := toolText(t, call("tools/call", search)); !isError || !strings.Contains(text, "not connected") {
		t.Fatalf("call after the drive left: %v %s", isError, text)
	}
	if tools := call("tools/list", nil)["result"].(map[string]any)["tools"].([]any); len(tools) != len(mcpTools) {
		t.Fatalf("tools/list after the drive left listed %d tools", len(tools))
	}
	if err := os.Rename(drive+"-away", drive); err != nil {
		t.Fatal(err)
	}
	if text, isError := toolText(t, call("tools/call", search)); isError || !strings.Contains(text, "Parser maintenance") {
		t.Fatalf("call after the drive returned: %v %s", isError, text)
	}

	// The library's switch still applies whenever the catalog is reachable.
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.SetMCPEnabled(false); err != nil {
		t.Fatal(err)
	}
	catalog.Close()
	if tools := call("tools/list", nil)["result"].(map[string]any)["tools"].([]any); len(tools) != 0 {
		t.Fatalf("disabled MCP listed %d tools", len(tools))
	}
	if text, isError := toolText(t, call("tools/call", search)); !isError || !strings.Contains(text, "turned off") {
		t.Fatalf("disabled MCP call: %v %s", isError, text)
	}
}

func TestMCPLibraryGuardsStillApply(t *testing.T) {
	config := testLibrary(t, filepath.Join(t.TempDir(), "Pharos"))
	server := newMCPServer(config.Path, "")
	checks := 0
	server.identity = func(string) string { checks++; return testVolume }
	call := startMCP(t, server)
	search := map[string]any{"name": "search_work", "arguments": map[string]any{"query": "parser"}}
	for range 3 {
		if text, isError := toolText(t, call("tools/call", search)); isError {
			t.Fatal(text)
		}
	}
	if checks != 1 {
		t.Fatalf("checked the volume %d times for one mounted library, want 1", checks)
	}
	server.identity = func(string) string { return "uuid:A-CLONE" }
	if err := SetRootString(config.Path, "volume_id", "uuid:TEST-VOLUME-2"); err != nil {
		t.Fatal(err)
	}
	if text, isError := toolText(t, call("tools/call", search)); !isError || !strings.Contains(text, "refusing to open") {
		t.Fatalf("a changed pin was not rechecked: %v %s", isError, text)
	}
	if err := os.Rename(config.CatalogPath, config.CatalogPath+".moved"); err != nil {
		t.Fatal(err)
	}
	server.identity = func(string) string { return "uuid:TEST-VOLUME-2" }
	if text, isError := toolText(t, call("tools/call", search)); !isError || !strings.Contains(text, "catalog not found") {
		t.Fatalf("a missing catalog was not refused: %v %s", isError, text)
	}
	if _, err := os.Stat(config.CatalogPath); !os.IsNotExist(err) {
		t.Fatalf("MCP created a library catalog: %v", err)
	}
}

func TestMCPListsToolsWithoutWaitingForTheHost(t *testing.T) {
	config := testLibrary(t, filepath.Join(t.TempDir(), "Pharos"))
	previous := currentHost
	currentHost = func() Host { time.Sleep(5 * time.Second); return previous() } // ioreg hanging
	t.Cleanup(func() { currentHost = previous })
	for _, server := range []*mcpServer{newMCPServer(config.Path, ""), newMCPServer("", writePointer(t, config))} {
		server.identity = testIdentity
		started := time.Now()
		if _, err := server.config(); err != nil {
			t.Fatal(err)
		}
		call := startMCP(t, server)
		call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
		if tools := call("tools/list", nil)["result"].(map[string]any)["tools"].([]any); len(tools) != len(mcpTools) {
			t.Fatalf("tools/list listed %d tools", len(tools))
		}
		if took := time.Since(started); took > 2*time.Second {
			t.Fatalf("starting, initialize and tools/list took %v while the host was being detected", took)
		}
	}
}

// writePointer writes a library.json naming config's library.
func writePointer(t *testing.T, config Config) string {
	t.Helper()
	pointer := filepath.Join(t.TempDir(), "library.json")
	if err := os.WriteFile(pointer, []byte(`{"library_dir":`+strconv.Quote(filepath.Dir(config.Path))+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return pointer
}

func TestMCPLeavesOlderCatalogsToTheApp(t *testing.T) {
	config := testLibrary(t, filepath.Join(t.TempDir(), "Pharos"))
	db, err := sql.Open("sqlite", catalogDSN(config.CatalogPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	older := strconv.Itoa(catalogSchemaVersion - 1)
	if _, err := db.Exec("UPDATE meta SET value=? WHERE key='schema_version'", older); err != nil {
		t.Fatal(err)
	}
	server := newMCPServer(config.Path, "")
	server.identity = testIdentity
	call := startMCP(t, server)
	text, isError := toolText(t, call("tools/call", map[string]any{"name": "search_work", "arguments": map[string]any{"query": "parser"}}))
	if !isError || !strings.Contains(text, "needs to be opened by the Pharos app once to upgrade it") {
		t.Fatalf("call on an older catalog: %v %s", isError, text)
	}
	var stored string
	if err := db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&stored); err != nil || stored != older {
		t.Fatalf("the MCP migrated the catalog: schema_version=%q %v", stored, err)
	}
}

// touchVanishedMapping reads a mapped page whose file has been truncated away,
// which raises SIGBUS as SQLite's mapped index on a yanked drive does.
func touchVanishedMapping(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "mapped"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(4096); err != nil {
		t.Fatal(err)
	}
	data, err := syscall.Mmap(int(file.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Munmap(data) })
	if err := file.Truncate(0); err != nil {
		t.Fatal(err)
	}
	faultSink = data[100]
}

var faultSink byte

// faultingAnswer faults in calls whose arguments mention "fault-now".
func faultingAnswer(t *testing.T) func(*Catalog, map[string]any) map[string]any {
	return func(catalog *Catalog, request map[string]any) map[string]any {
		if strings.Contains(jsonText(request), "fault-now") {
			touchVanishedMapping(t)
		}
		return handleMCP(catalog, request)
	}
}

func TestMCPAnswersAFaultedCallAndRestarts(t *testing.T) {
	config := testLibrary(t, filepath.Join(t.TempDir(), "Pharos"))
	server := newMCPServer(config.Path, "")
	server.identity = testIdentity
	server.answer = faultingAnswer(t)
	restarts := 0
	server.restart = func([]byte) error { restarts++; return errors.New("no restart in this test") }
	call := startMCP(t, server)
	text, isError := toolText(t, call("tools/call", map[string]any{"name": "search_work", "arguments": map[string]any{"query": "fault-now"}}))
	if !isError || !strings.Contains(text, "Pharos library disconnected") {
		t.Fatalf("faulted call: %v %s", isError, text)
	}
	// Were the restart to fail, the process would still answer.
	if text, isError := toolText(t, call("tools/call", map[string]any{"name": "search_work", "arguments": map[string]any{"query": "parser"}})); isError || !strings.Contains(text, "Parser maintenance") || restarts != 1 {
		t.Fatalf("call after the fault: %v %s, %d restarts", isError, text, restarts)
	}
}

// TestMCPHelper is the MCP process for TestMCPRestartKeepsTheConnection.
func TestMCPHelper(t *testing.T) {
	config := os.Getenv("PHAROS_TEST_MCP_CONFIG")
	if config == "" {
		t.Skip("helper process")
	}
	server := newMCPServer(config, "")
	server.identity = testIdentity
	server.answer = faultingAnswer(t)
	if err := server.run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestMCPRestartKeepsTheConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an MCP process")
	}
	config := testLibrary(t, filepath.Join(t.TempDir(), "Pharos"))
	// Started, as by the launcher, through a link that a newer build repoints.
	current := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(os.Args[0], current); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(current, "-test.run=^TestMCPHelper$")
	command.Env = append(os.Environ(), "PHAROS_TEST_MCP_CONFIG="+config.Path)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	request := func(id int, query string) string {
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": "search_work", "arguments": map[string]any{"query": query}}})
		return string(payload) + "\n"
	}
	responses := bufio.NewReader(output)
	next := func() map[string]any {
		t.Helper()
		line, err := responses.ReadBytes('\n')
		if err != nil {
			t.Fatalf("%v; stderr: %s", err, stderr.String())
		}
		var response map[string]any
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if _, err := io.WriteString(input, request(0, "parser")); err != nil {
		t.Fatal(err)
	}
	next()
	newer := filepath.Join(t.TempDir(), "newer")
	if err := os.WriteFile(newer, []byte("#!/bin/sh\necho another build ran >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(current); err != nil || os.Symlink(newer, current) != nil {
		t.Fatalf("repointing the link: %v", err)
	}
	// One write, so the second request is read ahead of the fault.
	if _, err := io.WriteString(input, request(1, "fault-now")+request(2, "parser")); err != nil {
		t.Fatal(err)
	}
	for index, want := range []string{"Pharos library disconnected", "Parser maintenance"} {
		response := next()
		if text, _ := toolText(t, response); response["id"] != float64(index+1) || !strings.Contains(text, want) {
			t.Fatalf("response %v: %s; want id %d with %q", response["id"], text, index+1, want)
		}
	}
	if _, err := io.WriteString(input, request(3, "parser")); err != nil {
		t.Fatal(err)
	}
	if text, isError := toolText(t, next()); isError || !strings.Contains(text, "Parser maintenance") {
		t.Fatalf("call after the restart: %s", text)
	}
	input.Close()
	if err := command.Wait(); err != nil {
		t.Fatalf("MCP process: %v; stderr: %s", err, stderr.String())
	}
	if log := stderr.String(); !strings.Contains(log, "restarting after a memory fault") || strings.Contains(log, "could not restart") || strings.Contains(log, "another build") {
		t.Fatalf("stderr: %s", log)
	}
}

func TestMCPDoesNotReopenAReleasedLibrary(t *testing.T) {
	config := testLibrary(t, filepath.Join(t.TempDir(), "Pharos"))
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	release := httptest.NewRequest(http.MethodPost, "/api/release", nil)
	release.Header.Set("Authorization", "Bearer "+config.APIToken)
	released := httptest.NewRecorder()
	if NewServer(config, catalog).ServeHTTP(released, release); released.Code != http.StatusOK {
		t.Fatalf("release: %d %s", released.Code, released.Body.String())
	}
	marker := releasedMarker(config.CatalogPath)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("release left no marker: %v", err)
	}
	// The service has exited; the drive is about to unmount.
	server := newMCPServer(config.Path, "")
	server.identity = testIdentity
	call := startMCP(t, server)
	search := map[string]any{"name": "search_work", "arguments": map[string]any{"query": "parser"}}
	if text, isError := toolText(t, call("tools/call", search)); !isError || !strings.Contains(text, "released it so that its drive can be ejected") {
		t.Fatalf("call right after a release: %v %s", isError, text)
	}
	if holdsCatalog(t, config.CatalogPath) || mcpCallCount(t, config.CatalogPath) != 0 {
		t.Fatal("a released library was reopened")
	}
	// An eject that never happened stops mattering after a while.
	old := time.Now().Add(-releaseQuiet - time.Second)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	if text, isError := toolText(t, call("tools/call", search)); isError || !strings.Contains(text, "Parser maintenance") {
		t.Fatalf("call long after a release: %v %s", isError, text)
	}
}

// normalizedToolResult drops the one time-dependent field (freshness lag).
func normalizedToolResult(t *testing.T, response map[string]any) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(jsonText(response)), &value); err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)["result"].(map[string]any)
	if content, ok := result["content"].([]any); ok {
		var inner any
		if err := json.Unmarshal([]byte(content[0].(map[string]any)["text"].(string)), &inner); err != nil {
			t.Fatal(err)
		}
		var strip func(any)
		strip = func(node any) {
			switch typed := node.(type) {
			case map[string]any:
				delete(typed, "lag_seconds")
				for _, child := range typed {
					strip(child)
				}
			case []any:
				for _, child := range typed {
					strip(child)
				}
			}
		}
		strip(inner)
		content[0].(map[string]any)["text"] = inner
	}
	return result
}

func TestMCPForwardingMatchesDirect(t *testing.T) {
	config := testLibrary(t, filepath.Join(t.TempDir(), "Pharos"))
	server := newMCPServer(config.Path, "")
	server.identity = testIdentity
	call := startMCP(t, server)
	requests := []map[string]any{
		{"name": "search_conversations", "arguments": map[string]any{"query": "tokenizer parser", "limit": 5}},
		{"name": "search_work", "arguments": map[string]any{"query": "parser"}},
		{"name": "search_work", "arguments": map[string]any{}},
		{"name": "get_work_detail", "arguments": map[string]any{"workspace_id": fixtureWorkspace(t, config)}},
		{"name": "trace", "arguments": map[string]any{"file": "missing.go"}},
		{"name": "query_metrics", "arguments": map[string]any{"name": "total_tokens"}},
		{"name": "get_receipt", "arguments": map[string]any{"id": "nothing"}},
	}
	run := func() []any {
		results := []any{normalizedToolResult(t, call("tools/list", nil))}
		conversation := ""
		for _, request := range requests {
			response := call("tools/call", request)
			results = append(results, normalizedToolResult(t, response))
			if text, _ := toolText(t, response); conversation == "" && request["name"] == "search_conversations" {
				conversation = regexp.MustCompile(`"conversation_id":"([^"]+)"`).FindStringSubmatch(text)[1]
			}
		}
		for _, name := range []string{"get_conversation_overview", "get_conversation_messages"} {
			results = append(results, normalizedToolResult(t, call("tools/call", map[string]any{"name": name, "arguments": map[string]any{"conversation_id": conversation}})))
		}
		return results
	}
	calls := len(requests) + 2
	direct := run()
	if count := mcpCallCount(t, config.CatalogPath); count != calls {
		t.Fatalf("direct calls recorded %d, want %d", count, calls)
	}

	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	var forwarded atomic.Int32
	service := NewServer(config, catalog)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/mcp/rpc" {
			forwarded.Add(1)
		}
		service.ServeHTTP(w, r)
	}))
	defer listener.Close()
	setLibraryPort(t, config, listener.Listener.Addr().(*net.TCPAddr).Port)
	through := run()
	if int(forwarded.Load()) != calls+1 {
		t.Fatalf("forwarded %d requests, want %d", forwarded.Load(), calls+1)
	}
	if count := mcpCallCount(t, config.CatalogPath); count != 2*calls {
		t.Fatalf("forwarded calls recorded %d in total, want %d", count, 2*calls)
	}
	for index := range direct {
		if !reflect.DeepEqual(direct[index], through[index]) {
			t.Fatalf("request %d differs:\ndirect:    %s\nforwarded: %s", index, jsonText(direct[index]), jsonText(through[index]))
		}
	}
}

func TestMCPForwardingDeclinesOtherServices(t *testing.T) {
	config := testLibrary(t, filepath.Join(t.TempDir(), "Pharos"))
	server := newMCPServer(config.Path, "")
	server.identity = testIdentity
	call := startMCP(t, server)
	search := map[string]any{"name": "search_work", "arguments": map[string]any{"query": "parser"}}
	serve := func(handler http.Handler) {
		t.Helper()
		listener := httptest.NewServer(handler)
		t.Cleanup(listener.Close)
		setLibraryPort(t, config, listener.Listener.Addr().(*net.TCPAddr).Port)
	}

	// Another catalog behind the same token and port: it declines, and the
	// call is answered from this library directly.
	other, otherConfig := testCatalog(t)
	otherConfig.APIToken = config.APIToken
	serve(NewServer(otherConfig, other))
	if text, isError := toolText(t, call("tools/call", search)); isError || !strings.Contains(text, "Parser maintenance") {
		t.Fatalf("call past another catalog's service: %v %s", isError, text)
	}
	if count := mcpCallCount(t, other.Path); count != 0 {
		t.Fatalf("another catalog recorded %d calls", count)
	}
	if count := mcpCallCount(t, config.CatalogPath); count != 1 {
		t.Fatalf("library recorded %d calls, want 1", count)
	}

	// A service that has released the library must not see it reopened here.
	serve(NewServer(config, nil))
	if text, isError := toolText(t, call("tools/call", search)); !isError || !strings.Contains(text, "not connected") {
		t.Fatalf("call to a released service: %v %s", isError, text)
	}
	if count := mcpCallCount(t, config.CatalogPath); count != 1 {
		t.Fatalf("a released library was reopened: %d calls recorded", count)
	}
}

func TestOpenCatalogForQuerySkipsInitialize(t *testing.T) {
	catalog, _ := testCatalog(t)
	ingestMCPFixture(t, catalog)
	path := catalog.Path
	catalog.Close()
	meta := func(key string) string {
		t.Helper()
		db, err := sql.Open("sqlite", catalogDSN(path))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var value string
		if err := db.QueryRow("SELECT COALESCE((SELECT value FROM meta WHERE key=?),'')", key).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	setMeta := func(statement string) {
		t.Helper()
		db, err := sql.Open("sqlite", catalogDSN(path))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if meta(initializedBuildKey) != buildDigest() || buildDigest() == "" {
		t.Fatalf("Initialize did not record this build: %q", meta(initializedBuildKey))
	}

	// Another process holds the write lock: a full open would wait for it and
	// fail after busy_timeout, while a query open never writes.
	writer, err := sql.Open("sqlite", catalogDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	ctx := context.Background()
	held, err := writer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	light, err := OpenCatalogForQuery(path)
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Since(started)
	if value, err := callMCP(light, "search_conversations", map[string]any{"query": "tokenizer parser"}); err != nil || len(value.(map[string]any)["items"].([]map[string]any)) != 1 {
		t.Fatalf("query on the light path: %v %#v", err, value)
	}
	if opened > time.Second {
		t.Fatalf("query open took %v while another connection held the write lock", opened)
	}
	if _, err := held.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	held.Close()
	light.closeQuery()

	for _, test := range []struct{ name, statement, key string }{
		{"catalog from a build before this check", "DELETE FROM meta WHERE key='" + initializedBuildKey + "'", initializedBuildKey},
		{"another build", "UPDATE meta SET value='other' WHERE key='" + initializedBuildKey + "'", initializedBuildKey},
		{"stale Library projection", "UPDATE meta SET value='stale' WHERE key='workspace_library_version'", "workspace_library_version"},
	} {
		want := meta(test.key)
		setMeta(test.statement)
		opened, err := OpenCatalogForQuery(path)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		opened.closeQuery()
		if got := meta(test.key); got != want {
			t.Fatalf("%s: %s=%q after a query open, want %q (migrated)", test.name, test.key, got, want)
		}
	}

	setMeta("UPDATE meta SET value='" + strconv.Itoa(catalogSchemaVersion+1) + "' WHERE key='schema_version'")
	if _, err := OpenCatalogForQuery(path); err == nil || !strings.Contains(err.Error(), "schema version "+strconv.Itoa(catalogSchemaVersion+1)) {
		t.Fatalf("a newer catalog was not refused: %v", err)
	}
	if got := meta("schema_version"); got != strconv.Itoa(catalogSchemaVersion+1) {
		t.Fatalf("a refused catalog was modified: schema_version=%q", got)
	}
	// Migrating an older catalog is left to the app.
	for _, statement := range []string{
		"INSERT OR REPLACE INTO meta(key,value) VALUES('schema_version','" + strconv.Itoa(catalogSchemaVersion-1) + "')",
		"DELETE FROM meta WHERE key='schema_version'",
	} {
		setMeta(statement)
		want := meta("schema_version")
		if _, err := OpenCatalogForQuery(path); !errors.Is(err, errCatalogNeedsUpgrade) {
			t.Fatalf("%s: query open of an older catalog: %v", statement, err)
		}
		if got := meta("schema_version"); got != want {
			t.Fatalf("%s: an older catalog was migrated by a query open: schema_version=%q", statement, got)
		}
	}
	missing := filepath.Join(t.TempDir(), "catalog.sqlite3")
	if _, err := OpenCatalogForQuery(missing); !os.IsNotExist(err) {
		t.Fatalf("query open of a missing catalog: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("a query open created a catalog")
	}
}

func TestInitializeRunsOneAtATime(t *testing.T) {
	catalog, _ := testCatalog(t)
	path := catalog.Path
	catalog.Close()
	setBuild := func(value string) {
		t.Helper()
		db, err := sql.Open("sqlite", catalogDSN(path))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("UPDATE meta SET value=? WHERE key=?", value, initializedBuildKey); err != nil {
			t.Fatal(err)
		}
	}
	build := func() (value string) {
		t.Helper()
		db, err := sql.Open("sqlite", catalogDSN(path))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.QueryRow("SELECT value FROM meta WHERE key=?", initializedBuildKey).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	// Another process (the service, mid-upgrade) holds the initialize lock.
	holder := exec.Command("/usr/bin/python3", "-c", `import fcntl, sys, time
f = open(sys.argv[1], "a"); fcntl.flock(f, fcntl.LOCK_EX); print("locked", flush=True); sys.stdin.read()`, filepath.Join(filepath.Dir(path), initializeLockName))
	release, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Skipf("python3: %v", err)
	}
	defer holder.Process.Kill()
	if line, err := bufio.NewReader(locked).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("lock holder: %q %v", line, err)
	}
	setBuild("another build")
	started := time.Now()
	if _, err := OpenCatalogForQuery(path); !errors.Is(err, errCatalogInitializing) || time.Since(started) > 5*time.Second {
		t.Fatalf("query open while another process initializes: %v after %v", err, time.Since(started))
	}
	if build() != "another build" {
		t.Fatal("a query open initialized the catalog alongside another process")
	}
	opened := make(chan error, 1)
	go func() {
		full, err := OpenCatalog(path)
		if err == nil {
			full.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		t.Fatalf("a full open did not wait for the other process: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	release.Close()
	if err := <-opened; err != nil || build() != buildDigest() {
		t.Fatalf("full open once the lock was free: %v, build %q", err, build())
	}
	setBuild("another build")
	light, err := OpenCatalogForQuery(path)
	if err != nil || build() != buildDigest() {
		t.Fatalf("query open with the lock free: %v, build %q", err, build())
	}
	light.closeQuery()
}

// openFiles lists the files this process has open.
func openFiles(t *testing.T) []string {
	t.Helper()
	output, err := exec.Command("lsof", "-n", "-P", "-p", strconv.Itoa(os.Getpid()), "-Fn").Output()
	if err != nil {
		t.Skipf("lsof: %v", err)
	}
	files := []string{}
	for _, line := range strings.Split(string(output), "\n") {
		if name, ok := strings.CutPrefix(line, "n"); ok {
			files = append(files, name)
		}
	}
	return files
}

func holdsCatalog(t *testing.T, path string) bool {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range openFiles(t) {
		if strings.HasPrefix(file, filepath.Join(resolved, filepath.Base(path))) || strings.HasPrefix(file, path) {
			return true
		}
	}
	return false
}

func TestMCPHoldsNoCatalogHandleBetweenCalls(t *testing.T) {
	config := testLibrary(t, filepath.Join(t.TempDir(), "Pharos"))
	open, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !holdsCatalog(t, config.CatalogPath) {
		t.Fatal("an open catalog is not visible to the check")
	}
	open.Close()
	server := newMCPServer(config.Path, "")
	server.identity = testIdentity
	call := startMCP(t, server)
	for _, request := range []map[string]any{
		{"name": "search_work", "arguments": map[string]any{}}, // warms the Library cache and its monitor connection
		{"name": "search_conversations", "arguments": map[string]any{"query": "parser"}},
		{"name": "get_work_detail", "arguments": map[string]any{"workspace_id": fixtureWorkspace(t, config)}},
	} {
		if text, isError := toolText(t, call("tools/call", request)); isError {
			t.Fatal(text)
		}
		if holdsCatalog(t, config.CatalogPath) {
			t.Fatalf("%s left the catalog open", request["name"])
		}
	}
	call("tools/list", nil)
	if holdsCatalog(t, config.CatalogPath) {
		t.Fatal("tools/list left the catalog open")
	}
}

func TestMCPStatusPresentsLauncherForLibraries(t *testing.T) {
	support := t.TempDir()
	t.Setenv("PHAROS_SUPPORT_DIR", support)
	catalog, config := testCatalog(t)
	config.Library = true
	status, err := catalog.MCPStatus(config)
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(support, "bin", "pharos-mcp")
	if status["command"] != launcher || len(status["args"].([]string)) != 0 || status["launcher_installed"] != false ||
		!strings.Contains(status["note"].(string), "install-mcp") {
		t.Fatalf("library MCP settings: %#v", status)
	}
	if _, err := InstallMCPLauncher(pharosSupportDir()); err != nil {
		t.Fatal(err)
	}
	if status, _ = catalog.MCPStatus(config); status["launcher_installed"] != true || strings.Contains(status["note"].(string), "install-mcp") {
		t.Fatalf("installed launcher: %#v", status)
	}
}

func TestRecordLibraryWritesTheAppsLibraryJSON(t *testing.T) {
	support := filepath.Join(t.TempDir(), "Pharos")
	config := Config{Path: "/Volumes/euclid 1/Pharos/library.toml", Library: true}
	volume := func(dir string) (string, string) {
		if dir != "/Volumes/euclid 1/Pharos" {
			t.Fatalf("volume looked up for %s", dir)
		}
		return "642c2c39-5926-4831-abe1-34642f37b103", "euclid"
	}
	read := func() map[string]string {
		t.Helper()
		var record map[string]string
		data, err := os.ReadFile(filepath.Join(support, "library.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	if err := recordLibrary(support, config, volume); err != nil {
		t.Fatal(err)
	}
	record := read()
	// The app decodes updated_at as ISO 8601 without fractional seconds.
	if at, err := time.Parse(time.RFC3339, record["updated_at"]); err != nil || at.Nanosecond() != 0 || !strings.HasSuffix(record["updated_at"], "Z") {
		t.Fatalf("updated_at %q: %v", record["updated_at"], err)
	}
	delete(record, "updated_at")
	if want := map[string]string{"library_dir": "/Volumes/euclid 1/Pharos", "volume_uuid": "642C2C39-5926-4831-ABE1-34642F37B103", "volume_name": "euclid"}; !reflect.DeepEqual(record, want) {
		t.Fatalf("library.json %v, want %v", record, want)
	}
	if pointer, err := readLibraryPointer(filepath.Join(support, "library.json")); err != nil || pointer.LibraryDir != "/Volumes/euclid 1/Pharos" {
		t.Fatalf("the MCP reads %+v, %v", pointer, err)
	}
	// library.toml's pin names the volume, as in the app.
	config.VolumeID = "uuid:abcd-1234"
	if err := recordLibrary(support, config, volume); err != nil || read()["volume_uuid"] != "ABCD-1234" {
		t.Fatalf("pinned volume_uuid %q, %v", read()["volume_uuid"], err)
	}
	if entries, _ := os.ReadDir(support); len(entries) != 1 {
		t.Fatalf("support directory holds %v", entries)
	}
}

func TestMCPLauncherRunsFromLocalDisk(t *testing.T) {
	if _, err := os.Stat("/usr/bin/plutil"); err != nil {
		t.Skip("needs macOS plutil")
	}
	root := t.TempDir()
	support := filepath.Join(root, "Application Support", "Pharos's")
	path, err := InstallMCPLauncher(support)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("launcher mode: %v %v", info, err)
	}
	if again, err := InstallMCPLauncher(support); err != nil || again != path {
		t.Fatalf("reinstall: %q %v", again, err)
	}
	launch := func() (string, string, error) {
		command := exec.Command(path)
		command.Env = []string{"PATH=/usr/bin:/bin"}
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		return stdout.String(), stderr.String(), err
	}
	fakeApp := func(app, name string) {
		t.Helper()
		binary := filepath.Join(app, "Contents", "MacOS", "pharos")
		if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(binary, []byte("#!/bin/sh\necho \""+name+" $* ($PHAROS_SUPPORT_DIR)\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pointer := filepath.Join(support, "library.json")

	if _, stderr, err := launch(); err == nil || !strings.Contains(stderr, "cannot start") {
		t.Fatalf("launcher with nothing installed: %v %s", err, stderr)
	}

	library := filepath.Join(root, "drive with space", "Pharos")
	fakeApp(filepath.Join(library, "Pharos.app"), "library")
	if err := os.WriteFile(pointer, []byte(`{"library_dir":`+strconv.Quote(library)+`,"volume_uuid":"X"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := launch()
	if err != nil || stdout != "library mcp --library-json "+pointer+" ("+support+")\n" || !strings.Contains(stderr, "warning") {
		t.Fatalf("launcher falling back to the library's app: %v %q %q", err, stdout, stderr)
	}

	fakeApp(filepath.Join(support, "runtime", "build-1", "Pharos.app"), "runtime")
	if err := os.Symlink(filepath.Join("build-1", "Pharos.app"), filepath.Join(support, "runtime", "current")); err != nil {
		t.Fatal(err)
	}
	if stdout, stderr, err := launch(); err != nil || stdout != "runtime mcp --library-json "+pointer+" ("+support+")\n" || stderr != "" {
		t.Fatalf("launcher with a local runtime: %v %q %q", err, stdout, stderr)
	}
}
