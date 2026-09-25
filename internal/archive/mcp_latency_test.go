package archive

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMCPLatencyReport measures MCP request latency on a synthetic catalog,
// forwarded to a running service and opened directly per request. It is slow
// and needs disk space, so it runs only with PHAROS_MCP_BENCH set to a
// directory, which keeps the generated library between runs:
//
//	PHAROS_MCP_BENCH=/tmp/pharos-mcp-bench go test ./internal/archive -run MCPLatency -v -timeout 60m
//
// PHAROS_MCP_BENCH_WORKSPACES sizes it (default 2000 workspaces of 4
// conversations of 60 messages).
func TestMCPLatencyReport(t *testing.T) {
	root := os.Getenv("PHAROS_MCP_BENCH")
	if root == "" {
		t.Skip("set PHAROS_MCP_BENCH to a directory to run")
	}
	commitFullFsync = true
	t.Cleanup(func() { commitFullFsync = false })
	workspaces := 2000
	if value := os.Getenv("PHAROS_MCP_BENCH_WORKSPACES"); value != "" {
		fmt.Sscan(value, &workspaces)
	}
	dir := filepath.Join(root, fmt.Sprintf("library-%d", workspaces))
	config, err := LoadConfig(filepath.Join(dir, libraryConfigName))
	if err != nil {
		config = generateBenchLibrary(t, dir, workspaces)
	}
	info, _ := os.Stat(config.CatalogPath)
	t.Logf("catalog: %s, %.0f MB", config.CatalogPath, float64(info.Size())/1e6)
	setLibraryPort(t, config, closedPort(t))

	timed := func(runs int, run func()) time.Duration {
		samples := []time.Duration{}
		for range runs {
			started := time.Now()
			run()
			samples = append(samples, time.Since(started))
		}
		slices.Sort(samples)
		return samples[len(samples)/2]
	}
	full := timed(5, func() {
		catalog, err := OpenCatalog(config.CatalogPath)
		if err != nil {
			t.Fatal(err)
		}
		catalog.Close()
	})
	var light *Catalog
	open := timed(20, func() {
		if light, err = OpenCatalogForQuery(config.CatalogPath); err != nil {
			t.Fatal(err)
		}
		light.closeQuery()
	})
	t.Logf("open: full OpenCatalog+Close %v, OpenCatalogForQuery+closeQuery %v (medians)", full, open)
	started := time.Now()
	volumeIdentity(dir)
	t.Logf("volume check (diskutil, uncached): %v", time.Since(started))

	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	var workspace, conversation string
	if err := catalog.DB.QueryRow("SELECT workspace_id,id FROM conversations ORDER BY id LIMIT 1 OFFSET (SELECT COUNT(*)/2 FROM conversations)").Scan(&workspace, &conversation); err != nil {
		t.Fatal(err)
	}
	requests := []map[string]any{
		{"name": "search_conversations", "arguments": map[string]any{"query": "tokenizer migration regression", "limit": 5}},
		{"name": "search_work", "arguments": map[string]any{"query": "parser cache", "limit": 10}},
		{"name": "search_work", "arguments": map[string]any{"limit": 10}},
		{"name": "get_conversation_overview", "arguments": map[string]any{"conversation_id": conversation}},
		{"name": "get_conversation_messages", "arguments": map[string]any{"conversation_id": conversation, "limit": 8}},
		{"name": "get_work_detail", "arguments": map[string]any{"workspace_id": workspace}},
	}
	label := func(request map[string]any) string {
		arguments := request["arguments"].(map[string]any)
		if request["name"] == "search_work" && arguments["query"] == nil {
			return "search_work (unfiltered)"
		}
		return request["name"].(string)
	}
	server := newMCPServer(config.Path, "")
	call := func(request map[string]any) {
		response := server.handle(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": request})
		if text, isError := toolTextOf(response); isError {
			t.Fatalf("%s: %s", request["name"], text)
		}
	}
	service := httptest.NewServer(NewServer(config, catalog))
	defer service.Close()
	lines := []string{"| request | in service (handleMCP) | forwarded (MCP process → service) | direct (open, call, close) |", "| --- | --- | --- | --- |"}
	for _, request := range requests {
		inService := timed(9, func() {
			handleMCP(catalog, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": request})
		})
		setLibraryPort(t, config, service.Listener.Addr().(*net.TCPAddr).Port)
		forwarded := timed(9, func() { call(request) })
		setLibraryPort(t, config, closedPort(t))
		direct := timed(9, func() { call(request) })
		lines = append(lines, fmt.Sprintf("| %s | %v | %v | %v |", label(request), inService.Round(time.Microsecond*100), forwarded.Round(time.Microsecond*100), direct.Round(time.Microsecond*100)))
	}
	t.Log("median of 9 runs; the first direct call also checks the volume\n" + strings.Join(lines, "\n"))
}

func toolTextOf(response map[string]any) (string, bool) {
	result, _ := response["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	if len(content) == 0 {
		if list, ok := result["content"].([]any); ok && len(list) > 0 {
			item, _ := list[0].(map[string]any)
			return firstString(item["text"]), result["isError"] == true
		}
		return "", true
	}
	return firstString(content[0]["text"]), result["isError"] == true
}

var benchWords = func() []string {
	random := rand.New(rand.NewSource(1))
	words := strings.Fields("parser tokenizer cache migration regression schema index query build deploy test failure retry timeout " +
		"library catalog session workspace branch commit review lint format refactor rename module package interface handler " +
		"request response latency memory leak profile benchmark flaky race lock mutex channel goroutine sqlite wal checkpoint")
	letters := "abcdefghijklmnopqrstuvwxyz"
	for len(words) < 8000 {
		word := make([]byte, 3+random.Intn(8))
		for index := range word {
			word[index] = letters[random.Intn(len(letters))]
		}
		words = append(words, string(word))
	}
	return words
}()

func benchText(random *rand.Rand, words int) string {
	var text strings.Builder
	for index := range words {
		if index > 0 {
			text.WriteByte(' ')
		}
		// Zipf-like: common words dominate, as in real transcripts.
		text.WriteString(benchWords[int(float64(len(benchWords))*random.Float64()*random.Float64()*random.Float64())])
	}
	return text.String()
}

func generateBenchLibrary(t *testing.T, dir string, workspaces int) Config {
	t.Helper()
	config, err := InitLibrary(dir, volumeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	random := rand.New(rand.NewSource(2))
	started := time.Now()
	const batch = 100
	for first := 0; first < workspaces; first += batch {
		items := []any{}
		for index := first; index < min(first+batch, workspaces); index++ {
			conversations := []any{}
			for c := range 4 {
				messages := []any{}
				for m := range 60 {
					role := "user"
					if m%2 == 1 {
						role = "assistant"
					}
					messages = append(messages, map[string]any{"id": fmt.Sprintf("m-%d-%d-%d", index, c, m), "role": role,
						"text": benchText(random, 20+random.Intn(300)), "created_at": time.Date(2026, 1, 1, 0, 0, index*60+c*10+m, 0, time.UTC).Format(time.RFC3339)})
				}
				conversations = append(conversations, map[string]any{"id": fmt.Sprintf("c-%d-%d", index, c), "provider": []string{"codex", "claude"}[c%2], "messages": messages})
			}
			items = append(items, map[string]any{"id": fmt.Sprintf("w-%d", index), "title": benchText(random, 6),
				"activity_at":   time.Date(2026, 1, 1, 0, 0, index*60, 0, time.UTC).Format(time.RFC3339),
				"repository":    map[string]any{"canonical_remote": fmt.Sprintf("github.com/acme/repo-%d", index%40), "display_name": fmt.Sprintf("repo-%d", index%40)},
				"conversations": conversations})
		}
		export := filepath.Join(dir, "export.json")
		payload, _ := json.Marshal(map[string]any{"workspaces": items})
		if err := os.WriteFile(export, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		adapter, err := MakeAdapter(SourceConfig{Name: fmt.Sprintf("bench-%d", first), Kind: "canonical", Path: export, Account: "local", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if result := catalog.Ingest(adapter, nil); result.Error != nil {
			t.Fatal(result.Error)
		}
		os.Remove(export)
		t.Logf("ingested %d/%d workspaces in %v", min(first+batch, workspaces), workspaces, time.Since(started).Round(time.Second))
	}
	if err := catalog.refreshAllLibrary(t.Context()); err != nil {
		t.Fatal(err)
	}
	return config
}
