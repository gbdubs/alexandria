package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func claudeLine(session, uuid, text, cwd string, minute int) string {
	return fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":%q,"cwd":%q,"timestamp":"2026-09-01T00:%02d:00Z","message":{"role":"user","content":%q}}`+"\n",
		uuid, session, cwd, minute, text)
}

func codexLines(id string, messages int) string {
	text := fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-09-01T00:00:00Z","payload":{"id":%q,"cwd":"/proj"}}`+"\n", id)
	for index := range messages {
		text += fmt.Sprintf(`{"type":"event_msg","timestamp":"2026-09-01T00:%02d:00Z","payload":{"type":"user_message","message":"message %d"}}`+"\n", index+1, index)
	}
	return text
}

func appendSourceFile(t *testing.T, file, content string) {
	t.Helper()
	writeSourceFile(t, file, readText(t, file)+content)
}

func countParses(t *testing.T) *atomic.Int64 {
	var parses atomic.Int64
	parseHook = func(string) { parses.Add(1) }
	t.Cleanup(func() { parseHook = nil })
	return &parses
}

func countCoverChecks(t *testing.T) *atomic.Int64 {
	var checks atomic.Int64
	coverCheckHook = func(string) { checks.Add(1) }
	t.Cleanup(func() { coverCheckHook = nil })
	return &checks
}

func ingestSource(t *testing.T, catalog *Catalog, source SourceConfig) IngestResult {
	t.Helper()
	adapter, err := MakeAdapter(source)
	if err != nil {
		t.Fatal(err)
	}
	result := catalog.Ingest(adapter, nil)
	if result.Error != nil {
		t.Fatalf("ingest %s: %v", source.Name, result.Error)
	}
	return result
}

// indexCaptures indexes every capture of the given host, as the current host.
func indexCaptures(t *testing.T, catalog *Catalog, root, host string) []IngestResult {
	t.Helper()
	targets, err := captureTargets(root, []string{host}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	results := []IngestResult{}
	for _, target := range targets {
		result := catalog.IndexCapture(context.Background(), target, nil)
		if result.Error != nil {
			t.Fatalf("index %s: %v", target.label(), result.Error)
		}
		results = append(results, result)
	}
	return results
}

func countRows(t *testing.T, catalog *Catalog, query string, args ...any) int {
	t.Helper()
	var count int
	if err := catalog.DB.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}

func conversationMessages(t *testing.T, catalog *Catalog, nativeID string) int {
	return countRows(t, catalog, "SELECT COUNT(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.native_id=?", nativeID)
}

func TestIndexAttributesCapturesToTheCapturingHost(t *testing.T) {
	root := t.TempDir()
	catalog, config := testCatalog(t)
	config.CaptureRoot = filepath.Join(root, "captures")
	originals := map[string]string{}
	for _, host := range []string{"host-a", "host-b"} {
		useHost(t, host)
		claude := filepath.Join(root, host, "claude")
		originals[host] = filepath.Join(claude, "-proj", host+"-session.jsonl")
		writeSourceFile(t, originals[host], claudeLine(host+"-session", "u1", "hello from "+host, "/Users/"+host+"/proj", 1))
		config.Sources = []SourceConfig{{Name: "claude", Kind: "claude", Path: claude, Account: "local", Enabled: true}}
		runCapture(t, config)
		// The capturing Mac leaves with its files.
		if err := os.RemoveAll(filepath.Join(root, host)); err != nil {
			t.Fatal(err)
		}
	}
	useHost(t, "host-c")
	targets, err := captureTargets(config.CaptureRoot, nil, true, nil)
	if err != nil || len(targets) != 2 {
		t.Fatalf("targets %#v: %v", targets, err)
	}
	for _, target := range targets {
		if result := catalog.IndexCapture(context.Background(), target, nil); result.Error != nil || result.Workspaces != 1 || result.Host != target.Host.ID {
			t.Fatalf("index %s: %#v", target.label(), result)
		}
	}
	hosts, err := catalog.capturedHosts(config.CaptureRoot)
	if err != nil || len(hosts) != 2 {
		t.Fatalf("captured hosts: %#v, %v", hosts, err)
	}
	for _, host := range hosts {
		id := firstString(host["id"])
		sources := host["sources"].([]map[string]any)
		if len(sources) != 1 {
			t.Fatalf("%s captured sources: %#v", id, sources)
		}
		source := sources[0]
		if source["path"] != filepath.Join(root, id, "claude") || source["account"] != "local" ||
			source["coverage"] != "complete" || firstString(source["last_attempt_at"]) == "" {
			t.Fatalf("%s source card details: %#v", id, source)
		}
	}
	for host, original := range originals {
		var origin, writer, locator, label string
		if err := catalog.DB.QueryRow(`SELECT c.origin,c.origin_host_id,m.evidence_locator FROM conversations c JOIN messages m ON m.conversation_id=c.id
			WHERE c.native_id=?`, host+"-session").Scan(&origin, &writer, &locator); err != nil {
			t.Fatal(err)
		}
		if origin != original || writer != host || locator != original+":1" {
			t.Fatalf("%s: origin %s writer %s locator %s", host, origin, writer, locator)
		}
		for _, query := range []string{
			"SELECT COUNT(*) FROM source_states WHERE host_id=? AND source_name='claude' AND coverage='complete'",
			"SELECT COUNT(*) FROM source_record_states WHERE host_id=? AND source_name='claude'",
			"SELECT COUNT(*) FROM workspace_sightings WHERE host_id=? AND source_name='claude'",
			"SELECT COUNT(*) FROM conversation_sightings WHERE host_id=? AND origin='" + original + "'",
			"SELECT COUNT(*) FROM source_item_states WHERE host_id=? AND item='" + original + "'",
		} {
			if count := countRows(t, catalog, query, host); count != 1 {
				t.Fatalf("%s: %s = %d", host, query, count)
			}
		}
		if err := catalog.DB.QueryRow("SELECT label FROM hosts WHERE id=?", host).Scan(&label); err != nil || label != "Mac "+host {
			t.Fatalf("%s host row: %q %v", host, label, err)
		}
		captured, err := capturedPathFor(config.CaptureRoot, host, "claude", original)
		if err != nil || readText(t, captured) != claudeLine(host+"-session", "u1", "hello from "+host, "/Users/"+host+"/proj", 1) {
			t.Fatalf("captured bytes for %s: %s %v", original, captured, err)
		}
	}
	for _, table := range []string{"source_states", "source_record_states", "source_item_states", "workspace_sightings", "conversation_sightings"} {
		if count := countRows(t, catalog, "SELECT COUNT(*) FROM "+table+" WHERE host_id='host-c'"); count != 0 {
			t.Fatalf("%s attributes %d rows to the indexing Mac", table, count)
		}
	}
}

func TestIndexAsksGitOnlyAboutThisMacsPaths(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:example/checkout.git"}} {
		if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, output)
		}
	}
	useHost(t, "host-a")
	config := captureTestConfig(t)
	claude := filepath.Join(root, "claude")
	writeSourceFile(t, filepath.Join(claude, "-p", "s.jsonl"), claudeLine("s", "u1", "hi", repo, 1))
	config.Sources = []SourceConfig{{Name: "claude", Kind: "claude", Path: claude, Account: "local", Enabled: true}}
	runCapture(t, config)
	remote := func(indexer string) string {
		t.Helper()
		catalog, _ := testCatalog(t)
		useHost(t, indexer)
		indexCaptures(t, catalog, config.CaptureRoot, "host-a")
		var value string
		_ = catalog.DB.QueryRow("SELECT COALESCE(r.canonical_remote,'') FROM workspaces w JOIN repositories r ON r.id=w.repository_id").Scan(&value)
		return value
	}
	// The same path on another Mac may be an unrelated checkout.
	if got := remote("host-b"); got != "" {
		t.Fatalf("another Mac's capture was matched to this Mac's checkout: %q", got)
	}
	if got := remote("host-a"); got != "git@github.com:example/checkout.git" {
		t.Fatalf("this Mac's capture lost its repository: %q", got)
	}
}

func TestLiveIngestAndCaptureIndexShareWriterAndParts(t *testing.T) {
	useHost(t, "host-a")
	catalog, config := testCatalog(t)
	config.CaptureRoot = filepath.Join(t.TempDir(), "captures")
	claude := filepath.Join(t.TempDir(), "claude")
	file := filepath.Join(claude, "-proj", "s1.jsonl")
	writeSourceFile(t, file, claudeLine("s1", "u1", "one", "/proj", 1))
	source := SourceConfig{Name: "claude", Kind: "claude", Path: claude, Account: "local", Enabled: true}
	config.Sources = []SourceConfig{source}
	parses, covers := countParses(t), countCoverChecks(t)
	check := func(step string, wantParses, wantMessages int) {
		t.Helper()
		var origin, writer string
		if err := catalog.DB.QueryRow("SELECT origin,origin_host_id FROM conversations WHERE native_id='s1'").Scan(&origin, &writer); err != nil {
			t.Fatal(err)
		}
		if int(parses.Load()) != wantParses || covers.Load() != 0 || conversationMessages(t, catalog, "s1") != wantMessages || origin != file || writer != "host-a" {
			t.Fatalf("%s: %d parses, %d cover checks, %d messages, writer %s %s", step, parses.Load(), covers.Load(), conversationMessages(t, catalog, "s1"), writer, origin)
		}
		parses.Store(0)
	}
	ingestSource(t, catalog, source)
	check("live", 1, 1)
	runCapture(t, config)
	if result := indexCaptures(t, catalog, config.CaptureRoot, "host-a")[0]; !result.SkippedUnchanged {
		t.Fatalf("a capture of what was ingested live was indexed again: %#v", result)
	}
	check("capture of the same data", 0, 1)

	appendSourceFile(t, file, claudeLine("s1", "u2", "two", "/proj", 2))
	runCapture(t, config)
	indexCaptures(t, catalog, config.CaptureRoot, "host-a")
	check("capture first", 1, 2)
	if result := ingestSource(t, catalog, source); !result.SkippedUnchanged {
		t.Fatalf("live data already indexed from its capture was ingested again: %#v", result)
	}
	check("then live", 0, 2)

	// The live source moves on; its capture, now older, must not roll it back.
	appendSourceFile(t, file, claudeLine("s1", "u3", "three", "/proj", 3))
	ingestSource(t, catalog, source)
	check("live ahead", 1, 3)
	if result := indexCaptures(t, catalog, config.CaptureRoot, "host-a")[0]; result.Unchanged != 1 || result.Workspaces != 0 {
		t.Fatalf("older capture: %#v", result)
	}
	check("older capture", 0, 3)
}

func TestIngestParsesOnlyChangedFiles(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	codex := filepath.Join(t.TempDir(), "codex")
	file := func(index int) string {
		return filepath.Join(codex, "sessions", fmt.Sprintf("rollout-%d.jsonl", index))
	}
	for index := range 5 {
		writeSourceFile(t, file(index), codexLines(fmt.Sprintf("session-%d", index), 1))
	}
	source := SourceConfig{Name: "codex", Kind: "codex", Path: codex, Account: "local", Enabled: true}
	parses := countParses(t)
	step := func(name string, wantParses, wantWritten int) IngestResult {
		t.Helper()
		parses.Store(0)
		result := ingestSource(t, catalog, source)
		if int(parses.Load()) != wantParses || result.Parsed != wantParses || result.Workspaces != wantWritten {
			t.Fatalf("%s: %d parses, %#v", name, parses.Load(), result)
		}
		return result
	}
	step("first", 5, 5)
	if result := step("unchanged", 0, 0); !result.SkippedUnchanged {
		t.Fatalf("unchanged source was scanned: %#v", result)
	}
	appendSourceFile(t, file(2), `{"type":"event_msg","timestamp":"2026-09-01T01:00:00Z","payload":{"type":"agent_message","message":"reply"}}`+"\n")
	if result := step("one file grew", 1, 1); result.Unchanged != 4 {
		t.Fatalf("unchanged files: %#v", result)
	}
	if conversationMessages(t, catalog, "session-2") != 2 {
		t.Fatal("the grown file's new message is missing")
	}
	// Touched but identical: parsed, but the record digest skips the write.
	writeSourceFile(t, file(3), readText(t, file(3)))
	if result := step("one file touched", 1, 0); result.SkippedCurrent != 1 {
		t.Fatalf("touched file: %#v", result)
	}
	writeSourceFile(t, file(5), codexLines("session-5", 1))
	step("a new file", 1, 1)
}

func TestClaudeSessionIsParsedWithItsSubagents(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	claude := filepath.Join(t.TempDir(), "claude")
	root, other := filepath.Join(claude, "-p", "s1.jsonl"), filepath.Join(claude, "-p", "s2.jsonl")
	agent := filepath.Join(claude, "-p", "s1", "subagents", "agent-a.jsonl")
	writeSourceFile(t, root, claudeLine("s1", "u1", "root", "/p", 1))
	writeSourceFile(t, agent, claudeLine("s1", "a1", "agent", "/p", 2))
	writeSourceFile(t, other, claudeLine("s2", "u1", "other", "/p", 3))
	source := SourceConfig{Name: "claude", Kind: "claude", Path: claude, Account: "local", Enabled: true}
	var parsed []string
	parseHook = func(path string) { parsed = append(parsed, filepath.Base(path)) }
	t.Cleanup(func() { parseHook = nil })
	step := func(name string, want ...string) {
		t.Helper()
		parsed = nil
		ingestSource(t, catalog, source)
		if strings.Join(parsed, ",") != strings.Join(want, ",") {
			t.Fatalf("%s parsed %v, want %v", name, parsed, want)
		}
	}
	step("first", "s1.jsonl", "agent-a.jsonl", "s2.jsonl")
	appendSourceFile(t, agent, claudeLine("s1", "a2", "agent again", "/p", 4))
	step("subagent grew", "s1.jsonl", "agent-a.jsonl")
	if conversationMessages(t, catalog, "s1:subagent:agent-a") != 2 || conversationMessages(t, catalog, "s1") != 1 {
		t.Fatal("the session's subagent was not updated")
	}
	writeSourceFile(t, filepath.Join(claude, "-p", "s1", "subagents", "agent-b.jsonl"), claudeLine("s1", "b1", "second agent", "/p", 5))
	step("new subagent", "s1.jsonl", "agent-a.jsonl", "agent-b.jsonl")
	if conversationMessages(t, catalog, "s1:subagent:agent-b") != 1 {
		t.Fatal("the new subagent is missing")
	}
	step("unchanged")
}

func conductorFixture(t *testing.T, dir string) func(...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db := openWritableSQLite(t, filepath.Join(dir, "conductor.db"))
	execSQL(t, db, "CREATE TABLE sessions(id TEXT PRIMARY KEY, title TEXT, created_at TEXT, updated_at TEXT)",
		"CREATE TABLE session_messages(id TEXT PRIMARY KEY, session_id TEXT, role TEXT, content TEXT, created_at TEXT, sent_at TEXT, cancelled_at TEXT)",
		"CREATE INDEX messages_sent ON session_messages(session_id, sent_at)")
	return func(statements ...string) { execSQL(t, db, statements...) }
}

func addConductorSession(id string, messages int) []string {
	statements := []string{fmt.Sprintf("INSERT INTO sessions VALUES('%s','Session %s','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')", id, id)}
	for index := range messages {
		statements = append(statements, fmt.Sprintf("INSERT INTO session_messages VALUES('%s-m%d','%s','user','message %d of %s','2026-09-01T00:%02d:00Z','2026-09-01T00:%02d:00Z',NULL)", id, index, id, index, id, index, index))
	}
	return statements
}

func TestConductorParsesOnlyChangedSessions(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	dir := filepath.Join(t.TempDir(), "conductor")
	exec := conductorFixture(t, dir)
	for _, id := range []string{"s1", "s2", "s3"} {
		exec(addConductorSession(id, 2)...)
	}
	source := SourceConfig{Name: "conductor", Kind: "conductor", Path: dir, Account: "local", Enabled: true}
	step := func(name string, wantParsed, wantWritten int) {
		t.Helper()
		result := ingestSource(t, catalog, source)
		if result.Parsed != wantParsed || result.Workspaces != wantWritten || result.Parsed+result.Unchanged != 3 && !result.SkippedUnchanged {
			t.Fatalf("%s: %#v", name, result)
		}
	}
	step("first", 3, 3)
	exec("INSERT INTO session_messages VALUES('s1-m9','s1','assistant','reply','2026-09-01T01:00:00Z','2026-09-01T01:00:00Z',NULL)")
	step("a message added", 1, 1)
	exec("UPDATE sessions SET title='Renamed' WHERE id='s2'")
	step("a session renamed", 1, 1)
	exec("UPDATE session_messages SET cancelled_at='2026-09-02T00:00:00Z' WHERE id='s3-m1'")
	step("a message cancelled", 1, 0)
	if conversationMessages(t, catalog, "conductor:s1") != 3 {
		t.Fatal("the added message is missing")
	}
	var title string
	if err := catalog.DB.QueryRow("SELECT title FROM workspaces WHERE source_id='s2'").Scan(&title); err != nil || title != "Renamed" {
		t.Fatalf("renamed session: %q %v", title, err)
	}
}

func TestIndexKeepsDeletedConductorSessionsFromUnindexedSnapshots(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	dir := filepath.Join(t.TempDir(), "conductor")
	exec := conductorFixture(t, dir)
	config := captureTestConfig(t, SourceConfig{Name: "conductor", Kind: "conductor", Path: dir, Account: "local", Enabled: true})
	generations := func() []capturedVersion { return readCaptureManifest(t, config, "conductor").Snapshots[0].Generations }
	capture := func() CaptureSourceResult {
		t.Helper()
		return captureResult(t, runCapture(t, config), "conductor")
	}
	exec(addConductorSession("s1", 1)...)
	capture()
	indexCaptures(t, catalog, config.CaptureRoot, "host-a")
	exec(addConductorSession("s2", 1)...)
	capture()
	exec("DELETE FROM session_messages WHERE session_id='s2'", "DELETE FROM sessions WHERE id='s2'")
	capture()
	exec(addConductorSession("s3", 1)...)
	capture()
	// The snapshot holding s2 is past capture_snapshot_generations but kept,
	// since no index has seen it.
	if kept := generations(); len(kept) != 2 {
		t.Fatalf("generations: %#v", kept)
	}
	indexCaptures(t, catalog, config.CaptureRoot, "host-a")
	var origin, writer string
	if err := catalog.DB.QueryRow("SELECT origin,origin_host_id FROM conversations WHERE native_id='conductor:s2'").Scan(&origin, &writer); err != nil {
		t.Fatalf("session deleted at the source was not indexed from its snapshot: %v", err)
	}
	if origin != "sqlite:"+filepath.Join(dir, "conductor.db") || writer != "host-a" {
		t.Fatalf("s2 from an older snapshot: origin %s writer %s", origin, writer)
	}
	if conversationMessages(t, catalog, "conductor:s3") != 1 {
		t.Fatal("latest snapshot not indexed")
	}
	// Indexed snapshots are pruned as usual again.
	exec(addConductorSession("s4", 1)...)
	capture()
	if kept := generations(); len(kept) != 1 {
		t.Fatalf("indexed generations were kept: %#v", kept)
	}
	// An index that never runs cannot make capture keep snapshots forever.
	exec(addConductorSession("s5", 1)...)
	capture()
	exec(addConductorSession("s6", 1)...)
	capture()
	exec(addConductorSession("s7", 1)...)
	result := capture()
	if kept := generations(); len(kept) != 2 || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0].Error, "never indexed") {
		t.Fatalf("unindexed generations past the limit: %#v %#v", kept, result.Warnings)
	}
}

func TestIndexResumesAfterInterruption(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	codex := filepath.Join(t.TempDir(), "codex")
	for index := range 5 {
		writeSourceFile(t, filepath.Join(codex, "sessions", fmt.Sprintf("rollout-%d.jsonl", index)), codexLines(fmt.Sprintf("session-%d", index), 1))
	}
	config := captureTestConfig(t, SourceConfig{Name: "codex", Kind: "codex", Path: codex, Account: "local", Enabled: true})
	runCapture(t, config)
	targets, err := captureTargets(config.CaptureRoot, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	parses := countParses(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := catalog.IndexCapture(ctx, targets[0], func(phase string, workspaces, _, _, _ int) {
		if workspaces == 2 {
			cancel()
		}
	})
	var coverage string
	_ = catalog.DB.QueryRow("SELECT coverage FROM source_states WHERE host_id='host-a' AND source_name='codex'").Scan(&coverage)
	// An index records its host's source state only once complete.
	if result.Workspaces != 2 || !strings.Contains(fmt.Sprint(result.Error), "interrupted") || coverage != "" {
		t.Fatalf("interrupted index: %#v, coverage %s", result, coverage)
	}
	parses.Store(0)
	result = catalog.IndexCapture(context.Background(), targets[0], nil)
	_ = catalog.DB.QueryRow("SELECT coverage FROM source_states WHERE host_id='host-a' AND source_name='codex'").Scan(&coverage)
	if result.Error != nil || result.Workspaces != 3 || result.Unchanged != 2 || parses.Load() != 3 || coverage != "complete" {
		t.Fatalf("resumed index: %#v, %d parses, coverage %s", result, parses.Load(), coverage)
	}
}

func TestIndexReadsOneVersionWhileACaptureReplacesIt(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	claude := filepath.Join(t.TempDir(), "claude")
	file := filepath.Join(claude, "-p", "s1.jsonl")
	writeSourceFile(t, file, claudeLine("s1", "u1", "one", "/p", 1))
	config := captureTestConfig(t, SourceConfig{Name: "claude", Kind: "claude", Path: claude, Account: "local", Enabled: true})
	runCapture(t, config)
	appendSourceFile(t, file, claudeLine("s1", "u2", "two", "/p", 2))
	// The index holds no capture lock: a capture replacing the very file being
	// parsed proceeds, and the index reads the version it opened whole.
	var captured atomic.Bool
	parseHook = func(string) {
		if captured.CompareAndSwap(false, true) {
			if summary := runCapture(t, config); !summary.OK || captureResult(t, summary, "claude").FilesCopied != 1 {
				t.Errorf("capture during an index: %#v", summary)
			}
		}
	}
	t.Cleanup(func() { parseHook = nil })
	indexCaptures(t, catalog, config.CaptureRoot, "host-a")
	if !captured.Load() || conversationMessages(t, catalog, "s1") != 1 {
		t.Fatalf("index during a capture: %d messages", conversationMessages(t, catalog, "s1"))
	}
	// It recorded the version it read, so the replacement is parsed next.
	if result := indexCaptures(t, catalog, config.CaptureRoot, "host-a")[0]; result.Parsed != 1 || conversationMessages(t, catalog, "s1") != 2 {
		t.Fatalf("after the capture: %#v, %d messages", result, conversationMessages(t, catalog, "s1"))
	}
}

// A TL1 before registry.json listed the project in workspaces.json, with its
// database and transcripts where they are here by default.
func TestIndexTL1CaptureWithoutTheOriginalPaths(t *testing.T) {
	for _, file := range []string{tl1RegistryFile, tl1LegacyRegistryFile} {
		t.Run(file, func(t *testing.T) { testIndexTL1CaptureWithoutTheOriginalPaths(t, file) })
	}
}

func testIndexTL1CaptureWithoutTheOriginalPaths(t *testing.T, file string) {
	useHost(t, "host-a")
	root := t.TempDir()
	database := filepath.Join(root, "tl1", "project.db")
	transcripts := filepath.Join(root, "tl1", "project", "transcripts")
	repository := filepath.Join(root, "repo")
	writeSourceFile(t, filepath.Join(repository, "tl1.json"), `{"project_name":"project"}`)
	transcript := filepath.Join(transcripts, "task-1", "attempt.jsonl")
	writeSourceFile(t, transcript, claudeLine("tl1-session", "u1", "do the task", repository, 1))
	db := openWritableSQLite(t, database)
	execSQL(t, db, "CREATE TABLE tasks(id TEXT, title TEXT, status TEXT, created_at TEXT)",
		"CREATE TABLE task_attempts(id TEXT, task_id TEXT, attempt_number INTEGER, model TEXT)",
		"CREATE TABLE transcript_summaries(attempt_id TEXT, filename TEXT)",
		"INSERT INTO tasks VALUES('task-1','The task','completed','2026-09-01T00:00:00Z')",
		"INSERT INTO task_attempts VALUES('attempt-1','task-1',1,'claude-x')",
		"INSERT INTO transcript_summaries VALUES('attempt-1','attempt.jsonl')")
	db.Close()
	registry := filepath.Join(root, "tl1", file)
	data, _ := json.Marshal(map[string]any{"installations": []map[string]string{{"installation_id": "abc123", "db_path": database,
		"config_path": filepath.Join(repository, "tl1.json"), "code_repo": repository, "transcripts_dir": transcripts, "project_name": "project"}}})
	installation := "abc123"
	if file == tl1LegacyRegistryFile {
		data, _ = json.Marshal(map[string]any{"workspaces": []map[string]string{{"project_name": "project",
			"config_path": filepath.Join(repository, "tl1.json"), "code_repo": repository, "last_used": "2026-04-29T08:12:49"}}})
		installation = database
	}
	writeSourceFile(t, registry, string(data))
	config := captureTestConfig(t, SourceConfig{Name: "tl1", Kind: "tl1", Path: registry, Account: "local", Enabled: true})
	runCapture(t, config)
	// Index on another Mac, where none of the registry's paths exist.
	for _, path := range []string{filepath.Join(root, "tl1"), repository} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	useHost(t, "host-b")
	catalog, _ := testCatalog(t)
	if result := indexCaptures(t, catalog, config.CaptureRoot, "host-a")[0]; result.Workspaces != 1 || result.Conversations != 1 {
		t.Fatalf("TL1 capture: %#v", result)
	}
	var location, origin, writer, locator string
	if err := catalog.DB.QueryRow(`SELECT w.location,c.origin,c.origin_host_id,m.evidence_locator FROM workspaces w JOIN conversations c ON c.workspace_id=w.id
		JOIN messages m ON m.conversation_id=c.id WHERE w.source_id=?`, installation+":task-1").Scan(&location, &origin, &writer, &locator); err != nil {
		t.Fatal(err)
	}
	if location != repository || origin != transcript || writer != "host-a" || locator != transcript+":1" {
		t.Fatalf("TL1 paths: location %s origin %s writer %s locator %s", location, origin, writer, locator)
	}
}

// TL1's analysis tables (tl1_store.go) come from a capture too: attributed to
// the capturing Mac with its original paths, script logs read from the
// capture, and never rolled back by an older capture.
func TestIndexTL1CaptureStoresAnalysisFromTheCapture(t *testing.T) {
	useHost(t, "host-a")
	root := t.TempDir()
	database := filepath.Join(root, "tl1", "project.db")
	transcripts := filepath.Join(root, "tl1", "project", "transcripts")
	repository := filepath.Join(root, "repo")
	configPath := filepath.Join(repository, "tl1.json")
	writeSourceFile(t, configPath, "{}")
	writeSourceFile(t, filepath.Join(transcripts, "task-1", "1-script.stderr.log"), "setup\nboom: exit status 2\n")
	db := openWritableSQLite(t, database)
	execSQL(t, db, "CREATE TABLE tasks(id TEXT, title TEXT, status TEXT, outcome TEXT, created_at TEXT)",
		"CREATE TABLE task_attempts(id TEXT, task_id TEXT, attempt_number INTEGER, executor TEXT, failure_reason TEXT)",
		"INSERT INTO tasks VALUES('task-1','The task','done','runtime_error','2026-09-01T00:00:00Z')",
		"INSERT INTO task_attempts VALUES('attempt-1','task-1',1,'script','exit status 2')")
	db.Close()
	registry := filepath.Join(root, "tl1", "registry.json")
	data, _ := json.Marshal(map[string]any{"installations": []map[string]string{{"installation_id": "abc123", "db_path": database,
		"config_path": configPath, "code_repo": repository, "transcripts_dir": transcripts, "project_name": "project"}}})
	writeSourceFile(t, registry, string(data))
	config := captureTestConfig(t, SourceConfig{Name: "tl1", Kind: "tl1", Path: registry, Account: "local", Enabled: true})
	runCapture(t, config)
	for _, path := range []string{filepath.Join(root, "tl1"), repository} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	useHost(t, "host-b")
	catalog, _ := testCatalog(t)
	if result := indexCaptures(t, catalog, config.CaptureRoot, "host-a")[0]; result.Error != nil || result.Workspaces != 1 {
		t.Fatalf("TL1 capture: %#v", result)
	}
	var host, databasePath, transcriptsDir, recordedConfig, logTail string
	var version int64
	if err := catalog.DB.QueryRow(`SELECT i.host_id,i.database_path,i.transcripts_dir,i.config_path,i.source_version,a.log_tail
		FROM tl1_installations i JOIN tl1_attempts a ON a.installation_id=i.installation_id WHERE i.installation_id='abc123'`).
		Scan(&host, &databasePath, &transcriptsDir, &recordedConfig, &version, &logTail); err != nil {
		t.Fatal(err)
	}
	if host != "host-a" || databasePath != database || transcriptsDir != transcripts || recordedConfig != configPath || version <= 0 || !strings.Contains(logTail, "boom") {
		t.Fatalf("TL1 analysis: host %s database %s transcripts %s config %s version %d log %q", host, databasePath, transcriptsDir, recordedConfig, version, logTail)
	}
	reindex := func(storedVersion int64) string {
		t.Helper()
		execSQL(t, catalog.DB, "UPDATE tl1_tasks SET title='as stored' WHERE installation_id='abc123'",
			fmt.Sprintf("UPDATE tl1_installations SET source_version=%d WHERE installation_id='abc123'", storedVersion),
			"UPDATE source_states SET fingerprint=NULL WHERE host_id='host-a' AND source_name='tl1'")
		if result := indexCaptures(t, catalog, config.CaptureRoot, "host-a")[0]; result.Error != nil {
			t.Fatalf("TL1 capture again: %#v", result)
		}
		var title string
		if err := catalog.DB.QueryRow("SELECT title FROM tl1_tasks WHERE installation_id='abc123' AND task_id='task-1'").Scan(&title); err != nil {
			t.Fatal(err)
		}
		return title
	}
	if title := reindex(version + 1); title != "as stored" {
		t.Fatalf("an older capture replaced TL1's analysis: %q", title)
	}
	if title := reindex(version); title != "The task" {
		t.Fatalf("the capture did not replace TL1's analysis read from the same version: %q", title)
	}
}

func TestTL1EphemeralInstallationsResolveTemporaryPaths(t *testing.T) {
	t.Setenv("TMPDIR", "/var/folders/zz/pharos-test/T/")
	registry := "/Users/someone/.tl1/registry.json"
	for _, repository := range []string{
		"/private/var/folders/zz/pharos-test/T/tl1-run/repo", // as registries record it
		"/var/folders/zz/pharos-test/T/tl1-run/repo",
		"/private/var/folders/qq/another-mac/T/repo", // recorded on another Mac
	} {
		if !ephemeralTL1(registry, repository) {
			t.Errorf("%s is not treated as ephemeral", repository)
		}
	}
	if ephemeralTL1(registry, "/Users/someone/code/repo") {
		t.Error("a real repository is treated as ephemeral")
	}
	if ephemeralTL1("/private/var/folders/zz/pharos-test/T/registry.json", "/private/var/folders/zz/pharos-test/T/repo") {
		t.Error("a temporary registry's installations are filtered")
	}
}

func TestIndexAPIRunsInBackground(t *testing.T) {
	useHost(t, "host-a")
	catalog, config := testCatalog(t)
	claude := filepath.Join(t.TempDir(), "claude")
	writeSourceFile(t, filepath.Join(claude, "p", "s.jsonl"), claudeLine("s", "u1", "hi", "/p", 1))
	config.CaptureRoot = filepath.Join(t.TempDir(), "captures")
	config.Sources = []SourceConfig{{Name: "claude", Kind: "claude", Path: claude, Account: "local", Enabled: true}}
	runCapture(t, config)
	lastDataAt := readCaptureManifest(t, config, "claude").LastDataAt
	// Existing libraries predate the manifest summary; the index API still
	// derives the last data time from their captured files.
	manifestPath := filepath.Join(config.CaptureRoot, "host-a", "claude", captureManifestName)
	var old map[string]any
	if data, err := os.ReadFile(manifestPath); err != nil || json.Unmarshal(data, &old) != nil {
		t.Fatalf("read capture manifest: %v", err)
	}
	delete(old, "last_data_at")
	if data, err := json.Marshal(old); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config, catalog)
	call := func(method, body string) (int, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(method, "/api/index", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		value := map[string]any{}
		_ = json.Unmarshal(response.Body.Bytes(), &value)
		return response.Code, value
	}
	if code, _ := call(http.MethodPost, `{"host":"nobody"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown host: %d", code)
	}
	server.ingestMu.Lock()
	if code, _ := call(http.MethodPost, `{}`); code != http.StatusConflict {
		t.Fatalf("index during a sync: %d", code)
	}
	server.ingestMu.Unlock()
	if code, body := call(http.MethodPost, `{"sources":["claude"]}`); code != http.StatusAccepted || body["run"] == nil {
		t.Fatalf("start: %d %#v", code, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body := call(http.MethodGet, "")
		run, _ := body["run"].(map[string]any)
		if run["state"] == "complete" {
			hosts, _ := body["hosts"].([]any)
			host, _ := hosts[0].(map[string]any)
			sources, _ := host["sources"].([]any)
			source, _ := sources[0].(map[string]any)
			if run["workspaces"] != float64(1) || run["kind"] != indexRunKind || host["id"] != "host-a" || host["current"] != true || source["needs_index"] != false || source["last_data_at"] != lastDataAt {
				t.Fatalf("finished index: %#v", body)
			}
			break
		}
		if time.Now().After(deadline) || run["state"] == "failed" {
			t.Fatalf("index did not finish: %#v", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !server.ingestMu.TryLock() {
		t.Fatal("the index kept the sync lock")
	}
	server.ingestMu.Unlock()
}

func TestIndexPassesOverWholeSourceCapturesOlderThanTheLastSync(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	export := filepath.Join(t.TempDir(), "export.json")
	write := func(title string) {
		writeSourceFile(t, export, `{"workspaces":[{"id":"work","title":"`+title+`","conversations":[{"id":"thread","messages":[{"id":"one","role":"user","text":"hello"}]}]}]}`)
	}
	title := func() string {
		var value string
		_ = catalog.DB.QueryRow("SELECT title FROM workspaces WHERE source_id='work'").Scan(&value)
		return value
	}
	source := SourceConfig{Name: "export", Kind: "canonical", Path: export, Account: "local", Enabled: true}
	config := captureTestConfig(t, source)
	write("Captured")
	runCapture(t, config)
	if indexCaptures(t, catalog, config.CaptureRoot, "host-a"); title() != "Captured" {
		t.Fatalf("capture not indexed: %q", title())
	}
	write("Synced later")
	ingestSource(t, catalog, source)
	if result := indexCaptures(t, catalog, config.CaptureRoot, "host-a")[0]; result.Older != 1 || title() != "Synced later" {
		t.Fatalf("older capture rolled the source back: %#v %q", result, title())
	}
	runCapture(t, config)
	write("Newest")
	runCapture(t, config)
	if indexCaptures(t, catalog, config.CaptureRoot, "host-a"); title() != "Newest" {
		t.Fatalf("newer capture not indexed: %q", title())
	}
}
