package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func captureTestConfig(t *testing.T, sources ...SourceConfig) Config {
	t.Helper()
	root := t.TempDir()
	config := defaultConfig(filepath.Join(root, "archive.toml"))
	config.CaptureRoot = filepath.Join(root, "captures")
	config.Sources = sources
	return config
}

var captureClock = time.Date(2026, 9, 1, 12, 0, 0, 123456789, time.UTC)

// writeSourceFile writes content with a distinct mtime, as a tool would.
func writeSourceFile(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	captureClock = captureClock.Add(time.Minute)
	if err := os.Chtimes(file, captureClock, captureClock); err != nil {
		t.Fatal(err)
	}
}

func runCapture(t *testing.T, config Config) CaptureSummary {
	t.Helper()
	summary, err := Capture(context.Background(), config, config.Sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func captureResult(t *testing.T, summary CaptureSummary, name string) CaptureSourceResult {
	t.Helper()
	for _, result := range summary.Sources {
		if result.Name == name {
			return result
		}
	}
	t.Fatalf("no result for %s in %#v", name, summary)
	return CaptureSourceResult{}
}

func readCaptureManifest(t *testing.T, config Config, source string) captureManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(config.CaptureRoot, currentHost().ID, source, captureManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest captureManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	return manifest
}

func manifestFile(t *testing.T, manifest captureManifest, captured string) *capturedFile {
	t.Helper()
	for _, file := range manifest.Files {
		if file.Captured == captured {
			return file
		}
	}
	t.Fatalf("manifest lacks %s", captured)
	return nil
}

func capturedPath(config Config, source, rel string) string {
	return filepath.Join(config.CaptureRoot, currentHost().ID, source, filepath.FromSlash(rel))
}

func readText(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCaptureCopiesWhatAdaptersReadIncrementally(t *testing.T) {
	useHost(t, "host-a")
	sources := t.TempDir()
	claude, codex := filepath.Join(sources, "claude"), filepath.Join(sources, "codex")
	writeSourceFile(t, filepath.Join(claude, "-proj", "s1.jsonl"), `{"type":"user"}`+"\n")
	writeSourceFile(t, filepath.Join(claude, "-proj", "s1", "subagents", "a1.jsonl"), `{"type":"assistant"}`+"\n")
	writeSourceFile(t, filepath.Join(claude, "-proj", "notes.txt"), "not a transcript")
	writeSourceFile(t, filepath.Join(codex, "sessions", "2026", "09", "r1.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(codex, "archived_sessions", "r2.jsonl"), "{}\n{}\n")
	writeSourceFile(t, filepath.Join(codex, "imports", "rollout-3.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(codex, "log", "other.jsonl"), "{}\n")
	config := captureTestConfig(t, SourceConfig{Name: "claude", Kind: "claude", Path: claude, Enabled: true}, SourceConfig{Name: "codex", Kind: "codex", Path: codex, Enabled: true})

	first := runCapture(t, config)
	if !first.OK || captureResult(t, first, "claude").FilesCopied != 2 || captureResult(t, first, "codex").FilesCopied != 3 {
		t.Fatalf("first capture: %#v", first)
	}
	for _, rel := range []string{"files/-proj/s1.jsonl", "files/-proj/s1/subagents/a1.jsonl"} {
		source := filepath.Join(claude, strings.TrimPrefix(filepath.FromSlash(rel), "files/"))
		captured := capturedPath(config, "claude", rel)
		if readText(t, captured) != readText(t, source) {
			t.Fatalf("%s differs from its source", rel)
		}
		sourceInfo, _ := os.Stat(source)
		capturedInfo, _ := os.Stat(captured)
		if !capturedInfo.ModTime().Equal(sourceInfo.ModTime()) {
			t.Fatalf("%s mtime %v, source %v", rel, capturedInfo.ModTime(), sourceInfo.ModTime())
		}
		sum := sha256.Sum256([]byte(readText(t, source)))
		if entry := manifestFile(t, readCaptureManifest(t, config, "claude"), rel); entry.SHA256 != hex.EncodeToString(sum[:]) || entry.Path != source {
			t.Fatalf("manifest entry %#v", entry)
		}
	}
	for _, ignored := range []string{"claude/files/-proj/notes.txt", "codex/files/log/other.jsonl"} {
		if _, err := os.Stat(filepath.Join(config.CaptureRoot, "host-a", filepath.FromSlash(ignored))); err == nil {
			t.Fatalf("%s is not read by its adapter and must not be captured", ignored)
		}
	}
	var host captureHostRecord
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(config.CaptureRoot, "host-a", captureHostName))), &host); err != nil || host.ID != "host-a" || host.FirstCaptureAt == "" || host.LastCaptureAt == "" {
		t.Fatalf("host.json: %#v %v", host, err)
	}
	firstManifest := readCaptureManifest(t, config, "codex")
	before := manifestFile(t, firstManifest, "files/archived_sessions/r2.jsonl").CapturedAt
	lastDataAt := firstManifest.LastDataAt
	if lastDataAt == "" {
		t.Fatal("first capture did not record when data arrived")
	}

	second := runCapture(t, config)
	if codexResult := captureResult(t, second, "codex"); !second.OK || codexResult.FilesCopied != 0 || codexResult.FilesUnchanged != 3 || captureResult(t, second, "claude").FilesUnchanged != 2 {
		t.Fatalf("no-op capture copied: %#v", second)
	}
	manifest := readCaptureManifest(t, config, "codex")
	if manifestFile(t, manifest, "files/archived_sessions/r2.jsonl").CapturedAt != before || manifest.LastDataAt != lastDataAt || manifest.LastRun == nil || manifest.LastRun.FinishedAt == "" {
		t.Fatalf("no-op capture changed the manifest: %#v", manifest)
	}

	// A capture damaged or lost on the drive is copied again.
	if err := os.Remove(capturedPath(config, "codex", "files/sessions/2026/09/r1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if third := runCapture(t, config); captureResult(t, third, "codex").FilesCopied != 1 {
		t.Fatalf("lost capture was not restored: %#v", third)
	}
}

func TestCaptureAppendsAndPreservesRewrittenFiles(t *testing.T) {
	useHost(t, "host-a")
	claude := filepath.Join(t.TempDir(), "claude")
	file := filepath.Join(claude, "p", "s.jsonl")
	writeSourceFile(t, file, "line one\n")
	config := captureTestConfig(t, SourceConfig{Name: "claude", Kind: "claude", Path: claude, Enabled: true})
	runCapture(t, config)

	writeSourceFile(t, file, "line one\nline two\n")
	grown := captureResult(t, runCapture(t, config), "claude")
	entry := manifestFile(t, readCaptureManifest(t, config, "claude"), "files/p/s.jsonl")
	if grown.FilesCopied != 1 || grown.VersionsPreserved != 0 || len(entry.Previous) != 0 || readText(t, capturedPath(config, "claude", entry.Captured)) != "line one\nline two\n" {
		t.Fatalf("grown file: %#v %#v", grown, entry)
	}

	writeSourceFile(t, file, "LINE ONE\nline two\n")
	rewritten := captureResult(t, runCapture(t, config), "claude")
	entry = manifestFile(t, readCaptureManifest(t, config, "claude"), "files/p/s.jsonl")
	if rewritten.VersionsPreserved != 1 || len(entry.Previous) != 1 || readText(t, capturedPath(config, "claude", entry.Previous[0].Captured)) != "line one\nline two\n" {
		t.Fatalf("rewritten file lost its earlier capture: %#v %#v", rewritten, entry)
	}
	if !strings.HasPrefix(entry.Previous[0].Captured, "history/") || readText(t, capturedPath(config, "claude", entry.Captured)) != "LINE ONE\nline two\n" {
		t.Fatalf("rewritten file: %#v", entry)
	}

	writeSourceFile(t, file, "short\n")
	runCapture(t, config)
	entry = manifestFile(t, readCaptureManifest(t, config, "claude"), "files/p/s.jsonl")
	if len(entry.Previous) != 2 || readText(t, capturedPath(config, "claude", entry.Previous[1].Captured)) != "LINE ONE\nline two\n" {
		t.Fatalf("truncated file lost its earlier capture: %#v", entry)
	}
}

func TestCaptureKeepsFilesDeletedAtSource(t *testing.T) {
	useHost(t, "host-a")
	claude := filepath.Join(t.TempDir(), "claude")
	writeSourceFile(t, filepath.Join(claude, "p", "old.jsonl"), "old\n")
	writeSourceFile(t, filepath.Join(claude, "p", "new.jsonl"), "new\n")
	config := captureTestConfig(t, SourceConfig{Name: "claude", Kind: "claude", Path: claude, Enabled: true})
	runCapture(t, config)

	if err := os.Remove(filepath.Join(claude, "p", "old.jsonl")); err != nil {
		t.Fatal(err)
	}
	result := captureResult(t, runCapture(t, config), "claude")
	entry := manifestFile(t, readCaptureManifest(t, config, "claude"), "files/p/old.jsonl")
	if result.FilesMissing != 1 || entry.MissingSince == "" || readText(t, capturedPath(config, "claude", "files/p/old.jsonl")) != "old\n" {
		t.Fatalf("deleted source file: %#v %#v", result, entry)
	}
	since := entry.MissingSince
	runCapture(t, config)
	if again := manifestFile(t, readCaptureManifest(t, config, "claude"), "files/p/old.jsonl"); again.MissingSince != since {
		t.Fatalf("missing_since moved from %s to %s", since, again.MissingSince)
	}

	// A wholly unavailable source is more likely unmounted than deleted.
	if err := os.RemoveAll(claude); err != nil {
		t.Fatal(err)
	}
	gone := runCapture(t, config)
	if gone.OK || captureResult(t, gone, "claude").State != "failed" {
		t.Fatalf("unavailable source reported success: %#v", gone)
	}
	if entry := manifestFile(t, readCaptureManifest(t, config, "claude"), "files/p/new.jsonl"); entry.MissingSince != "" {
		t.Fatalf("an unavailable source marked its files missing: %#v", entry)
	}
}

func TestCaptureResumesAfterInterruption(t *testing.T) {
	useHost(t, "host-a")
	claude := filepath.Join(t.TempDir(), "claude")
	for index := range 10 {
		writeSourceFile(t, filepath.Join(claude, "p", fmt.Sprintf("s%02d.jsonl", index)), strings.Repeat(fmt.Sprintf("line %d\n", index), 100))
	}
	config := captureTestConfig(t, SourceConfig{Name: "claude", Kind: "claude", Path: claude, Enabled: true})
	previousInterval := captureCheckpointInterval
	captureCheckpointInterval = 0
	t.Cleanup(func() { captureCheckpointInterval = previousInterval; captureFault = nil })

	// A kill mid-run: nothing after the fault runs, as if the process died.
	copies := 0
	captureFault = func(stage, rel string) error {
		if copies++; copies == 6 {
			panic("killed")
		}
		return nil
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the injected kill did not happen")
			}
		}()
		runCapture(t, config)
	}()
	manifest := readCaptureManifest(t, config, "claude")
	if len(manifest.Files) != 5 || manifest.LastRun == nil || manifest.LastRun.FinishedAt != "" {
		t.Fatalf("killed run left %d files, last run %#v", len(manifest.Files), manifest.LastRun)
	}
	// What a kill mid-copy leaves behind, for a file no later run rewrites.
	stale := capturedPath(config, "claude", "files/p/orphan.jsonl") + captureTempSuffix
	writeSourceFile(t, stale, "half written")

	// An unplug mid-run: the drive fails and the run reports it.
	copies = 0
	captureFault = func(stage, rel string) error {
		if copies++; copies == 3 {
			return errors.New("device not configured")
		}
		return nil
	}
	failed := runCapture(t, config)
	result := captureResult(t, failed, "claude")
	manifest = readCaptureManifest(t, config, "claude")
	if failed.OK || result.State != "failed" || result.FilesCopied != 2 || len(manifest.Files) != 7 || len(manifest.LastRun.Errors) == 0 {
		t.Fatalf("failed run: %#v, manifest has %d files", result, len(manifest.Files))
	}

	captureFault = nil
	resumed := runCapture(t, config)
	result = captureResult(t, resumed, "claude")
	if !resumed.OK || result.FilesCopied != 3 || result.FilesUnchanged != 7 {
		t.Fatalf("resumed run: %#v", result)
	}
	if len(readCaptureManifest(t, config, "claude").Files) != 10 {
		t.Fatal("resumed run did not complete the manifest")
	}
	_ = filepath.WalkDir(filepath.Join(config.CaptureRoot, "host-a"), func(file string, entry os.DirEntry, err error) error {
		if strings.HasSuffix(file, captureTempSuffix) {
			t.Errorf("temporary file left behind: %s", file)
		}
		return nil
	})
	for index := range 10 {
		rel := fmt.Sprintf("files/p/s%02d.jsonl", index)
		if readText(t, capturedPath(config, "claude", rel)) != strings.Repeat(fmt.Sprintf("line %d\n", index), 100) {
			t.Fatalf("%s is wrong after resuming", rel)
		}
	}
}

func openWritableSQLite(t *testing.T, file string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+file+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func execSQL(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestCaptureSnapshotsSQLiteConsistentlyWhileWritten(t *testing.T) {
	useHost(t, "host-a")
	conductor := filepath.Join(t.TempDir(), "com.conductor.app")
	if err := os.MkdirAll(conductor, 0o755); err != nil {
		t.Fatal(err)
	}
	db := openWritableSQLite(t, filepath.Join(conductor, "conductor.db"))
	execSQL(t, db, "CREATE TABLE sessions(id TEXT PRIMARY KEY, message_count INTEGER)", "CREATE TABLE messages(id INTEGER PRIMARY KEY, session_id TEXT, body TEXT)",
		"INSERT INTO sessions VALUES('s1',0)")
	body := strings.Repeat("x", 4000)
	commit := func() error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec("INSERT INTO messages(session_id,body) VALUES('s1',?)", body); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE sessions SET message_count=message_count+1 WHERE id='s1'"); err != nil {
			return err
		}
		return tx.Commit()
	}
	for range 5000 {
		if err := commit(); err != nil {
			t.Fatal(err)
		}
	}
	cache := openWritableSQLite(t, filepath.Join(conductor, "cache.db"))
	execSQL(t, cache, "CREATE TABLE kv(k TEXT, v TEXT)")

	var commits atomic.Int64
	stop, done := make(chan struct{}), make(chan error)
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := commit(); err != nil {
				done <- err
				return
			}
			commits.Add(1)
		}
	}()
	config := captureTestConfig(t, SourceConfig{Name: "conductor", Kind: "conductor", Path: conductor, Enabled: true})
	started := commits.Load()
	summary := runCapture(t, config)
	during := commits.Load() - started
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("writer failed during the snapshot: %v", err)
	}
	result := captureResult(t, summary, "conductor")
	if !summary.OK || result.SnapshotsTaken != 1 || result.DatabasesSeen != 1 {
		t.Fatalf("snapshot run: %#v", summary)
	}
	if during == 0 {
		t.Log("the writer did not commit during the capture; consistency was not exercised under load")
	}
	snapshotFile := capturedPath(config, "conductor", "files/conductor.db")
	if _, err := os.Stat(capturedPath(config, "conductor", "files/cache.db")); err == nil {
		t.Fatal("a database without sessions and messages was captured")
	}
	snapshot, err := openReadOnlySQLite(snapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var counted, messages int
	var mode, integrity string
	if err := snapshot.QueryRow("SELECT message_count,(SELECT COUNT(*) FROM messages) FROM sessions").Scan(&counted, &messages); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if counted != messages || messages < 5000 || mode != "delete" || integrity != "ok" {
		t.Fatalf("snapshot: message_count=%d messages=%d journal=%s integrity=%s (writer committed %d times during capture)", counted, messages, mode, integrity, during)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(snapshotFile + suffix); err == nil {
			t.Fatalf("snapshot has a %s file", suffix)
		}
	}
	manifest := readCaptureManifest(t, config, "conductor")
	if len(manifest.Snapshots) != 1 || manifest.Snapshots[0].Method != "sqlite-backup" || manifest.Snapshots[0].Source.Size == 0 || len(manifest.Files) != 0 {
		t.Fatalf("manifest: %#v", manifest)
	}
}

func TestCaptureRetainsSnapshotGenerations(t *testing.T) {
	useHost(t, "host-a")
	conductor := filepath.Join(t.TempDir(), "conductor")
	if err := os.MkdirAll(conductor, 0o755); err != nil {
		t.Fatal(err)
	}
	db := openWritableSQLite(t, filepath.Join(conductor, "conductor.db"))
	execSQL(t, db, "CREATE TABLE sessions(id TEXT PRIMARY KEY)", "CREATE TABLE messages(id INTEGER PRIMARY KEY, session_id TEXT)", "INSERT INTO sessions VALUES('generation-1')")
	config := captureTestConfig(t, SourceConfig{Name: "conductor", Kind: "conductor", Path: conductor, Enabled: true})
	sessionsIn := func(rel string) string {
		t.Helper()
		snapshot, err := openReadOnlySQLite(capturedPath(config, "conductor", rel))
		if err != nil {
			t.Fatal(err)
		}
		defer snapshot.Close()
		var ids string
		if err := snapshot.QueryRow("SELECT group_concat(id) FROM (SELECT id FROM sessions ORDER BY id)").Scan(&ids); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	runCapture(t, config)
	// A session deleted at the source survives in the previous generation.
	execSQL(t, db, "DELETE FROM sessions", "INSERT INTO sessions VALUES('generation-2')")
	runCapture(t, config)
	snapshot := readCaptureManifest(t, config, "conductor").Snapshots[0]
	if len(snapshot.Generations) != 1 || sessionsIn(snapshot.Generations[0].Captured) != "generation-1" || sessionsIn(snapshot.Captured) != "generation-2" {
		t.Fatalf("second snapshot: %#v", snapshot)
	}
	first := snapshot.Generations[0].Captured

	execSQL(t, db, "INSERT INTO sessions VALUES('generation-3')")
	runCapture(t, config)
	snapshot = readCaptureManifest(t, config, "conductor").Snapshots[0]
	if len(snapshot.Generations) != 1 || sessionsIn(snapshot.Generations[0].Captured) != "generation-2" {
		t.Fatalf("third snapshot: %#v", snapshot)
	}
	if _, err := os.Stat(capturedPath(config, "conductor", first)); err == nil {
		t.Fatalf("generation beyond capture_snapshot_generations was kept: %s", first)
	}

	unchanged := captureResult(t, runCapture(t, config), "conductor")
	if unchanged.SnapshotsTaken != 0 || unchanged.SnapshotsUnchanged != 1 {
		t.Fatalf("unchanged database was snapshotted again: %#v", unchanged)
	}

	config.CaptureGenerations = 0
	execSQL(t, db, "INSERT INTO sessions VALUES('generation-4')")
	runCapture(t, config)
	if snapshot := readCaptureManifest(t, config, "conductor").Snapshots[0]; len(snapshot.Generations) != 0 || sessionsIn(snapshot.Captured) != "generation-2,generation-3,generation-4" {
		t.Fatalf("with no generations: %#v", snapshot)
	}
	if entries, err := os.ReadDir(capturedPath(config, "conductor", captureHistoryDir)); err == nil && len(entries) > 0 {
		t.Fatalf("history kept %d entries with no generations configured", len(entries))
	}
}

func TestCaptureSeparatesHosts(t *testing.T) {
	root := t.TempDir()
	config := captureTestConfig(t)
	for _, host := range []string{"host-a", "host-b"} {
		useHost(t, host)
		claude := filepath.Join(root, host, "claude")
		writeSourceFile(t, filepath.Join(claude, "p", host+".jsonl"), host+"\n")
		config.Sources = []SourceConfig{{Name: "claude", Kind: "claude", Path: claude, Enabled: true}}
		if summary := runCapture(t, config); !summary.OK || summary.Root != filepath.Join(config.CaptureRoot, host) {
			t.Fatalf("%s: %#v", host, summary)
		}
	}
	for _, host := range []string{"host-a", "host-b"} {
		dir := filepath.Join(config.CaptureRoot, host)
		if readText(t, filepath.Join(dir, "claude", "files", "p", host+".jsonl")) != host+"\n" {
			t.Fatalf("%s capture missing", host)
		}
		entries, _ := os.ReadDir(filepath.Join(dir, "claude", "files", "p"))
		var manifest captureManifest
		_ = json.Unmarshal([]byte(readText(t, filepath.Join(dir, "claude", captureManifestName))), &manifest)
		if len(entries) != 1 || manifest.Host.ID != host || len(manifest.Files) != 1 || !strings.Contains(readText(t, filepath.Join(dir, captureHostName)), `"id": "`+host+`"`) {
			t.Fatalf("%s captures mix hosts: %v %#v", host, entries, manifest)
		}
	}
}

func TestCaptureTL1MapsRegistryPaths(t *testing.T) {
	useHost(t, "host-a")
	root := t.TempDir()
	database := filepath.Join(root, "tl1", "project.db")
	transcripts := filepath.Join(root, "tl1", "project", "transcripts")
	repository := filepath.Join(root, "repo")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSourceFile(t, filepath.Join(repository, "tl1.json"), "{}")
	writeSourceFile(t, filepath.Join(transcripts, "task-1", "attempt.jsonl"), "{}\n")
	db := openWritableSQLite(t, database)
	execSQL(t, db, "CREATE TABLE tasks(id TEXT)", "CREATE TABLE task_attempts(id TEXT, task_id TEXT, attempt_number INTEGER)")
	registry := filepath.Join(root, "tl1", "registry.json")
	rows := []map[string]string{
		{"installation_id": "abc123", "db_path": database, "config_path": filepath.Join(repository, "tl1.json"), "code_repo": repository, "transcripts_dir": transcripts, "project_name": "project"},
		{"installation_id": "gone", "db_path": filepath.Join(root, "missing.db"), "config_path": filepath.Join(repository, "tl1.json"), "code_repo": repository},
	}
	data, _ := json.Marshal(map[string]any{"installations": rows})
	writeSourceFile(t, registry, string(data))
	config := captureTestConfig(t, SourceConfig{Name: "tl1", Kind: "tl1", Path: registry, Enabled: true})

	summary := runCapture(t, config)
	result := captureResult(t, summary, "tl1")
	if !summary.OK || result.FilesCopied != 2 || result.SnapshotsTaken != 1 {
		t.Fatalf("tl1 capture: %#v", summary)
	}
	manifest := readCaptureManifest(t, config, "tl1")
	if len(manifest.Installations) != 1 {
		t.Fatalf("installations: %#v", manifest.Installations)
	}
	installation := manifest.Installations[0]
	if installation.CapturedDB != "files/installations/abc123/project.db" || installation.CapturedTranscripts != "files/installations/abc123/transcripts" || installation.DBPath != database {
		t.Fatalf("installation mapping: %#v", installation)
	}
	mapped := map[string]string{}
	for _, mapping := range manifest.PathMap {
		mapped[mapping.Original] = mapping.Captured
	}
	if mapped[registry] != "files/registry.json" || mapped[database] != installation.CapturedDB || mapped[transcripts] != installation.CapturedTranscripts {
		t.Fatalf("path map: %#v", manifest.PathMap)
	}
	for _, rel := range []string{"files/registry.json", "files/installations/abc123/transcripts/task-1/attempt.jsonl", installation.CapturedDB} {
		if _, err := os.Stat(capturedPath(config, "tl1", rel)); err != nil {
			t.Fatalf("%s not captured: %v", rel, err)
		}
	}

	// An installation leaving the registry keeps its captures and mapping.
	data, _ = json.Marshal(map[string]any{"installations": rows[1:]})
	writeSourceFile(t, registry, string(data))
	runCapture(t, config)
	manifest = readCaptureManifest(t, config, "tl1")
	if len(manifest.Installations) != 1 || manifest.Installations[0].MissingSince == "" || len(manifest.PathMap) != 3 || manifest.Snapshots[0].MissingSince == "" {
		t.Fatalf("unregistered installation: %#v %#v", manifest.Installations, manifest.PathMap)
	}
	if _, err := os.Stat(capturedPath(config, "tl1", installation.CapturedDB)); err != nil {
		t.Fatalf("unregistered installation's snapshot was removed: %v", err)
	}
}

func TestCaptureRootDefaultsAndGuards(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) Config {
		t.Helper()
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		config, err := LoadConfig(file)
		if err != nil {
			t.Fatal(err)
		}
		return config
	}
	if config := write("library.toml", "library = true\ncatalog_path = \"catalog/catalog.sqlite3\"\n"); config.CaptureRoot != filepath.Join(dir, "captures") || config.CaptureGenerations != 1 {
		t.Fatalf("library capture root: %s", config.CaptureRoot)
	}
	if config := write("archive.toml", "data_dir = \"data\"\n"); config.CaptureRoot != filepath.Join(dir, "data", "captures") {
		t.Fatalf("legacy capture root: %s", config.CaptureRoot)
	}
	if config := write("custom.toml", "capture_root = \"raw\"\ncapture_snapshot_generations = 3\n"); config.CaptureRoot != filepath.Join(dir, "raw") || config.CaptureGenerations != 3 {
		t.Fatalf("configured capture root: %s %d", config.CaptureRoot, config.CaptureGenerations)
	}

	useHost(t, "host-a")
	config := captureTestConfig(t)
	config.CaptureRoot = filepath.Join(dir, "unmounted-volume", "captures")
	if _, err := beginCapture(config); err == nil {
		t.Fatal("a capture root whose drive is absent was created")
	}
	if _, err := os.Stat(filepath.Join(dir, "unmounted-volume")); err == nil {
		t.Fatal("the missing mount point was recreated")
	}
	config = captureTestConfig(t)
	session, err := beginCapture(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beginCapture(config); !errors.Is(err, errCaptureBusy) {
		t.Fatalf("second concurrent capture: %v", err)
	}
	session.Close()
	if session, err := beginCapture(config); err != nil {
		t.Fatalf("capture lock was not released: %v", err)
	} else {
		session.Close()
	}
	if _, err := captureSources(config, []string{"nope"}); err == nil {
		t.Fatal("unknown source accepted")
	}
}

func TestCaptureAPIRunsInBackgroundAndReportsSafeToUnplug(t *testing.T) {
	useHost(t, "host-a")
	catalog, config := testCatalog(t)
	claude := filepath.Join(t.TempDir(), "claude")
	writeSourceFile(t, filepath.Join(claude, "p", "s.jsonl"), "{}\n")
	config.CaptureRoot = filepath.Join(t.TempDir(), "captures")
	config.Sources = []SourceConfig{{Name: "claude", Kind: "claude", Path: claude, Enabled: true}}
	server := NewServer(config, catalog)
	release := make(chan struct{})
	captureFault = func(stage, rel string) error { <-release; return nil }
	t.Cleanup(func() { captureFault = nil })
	call := func(method, body string) (int, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(method, "/api/capture", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		value := map[string]any{}
		_ = json.Unmarshal(response.Body.Bytes(), &value)
		return response.Code, value
	}
	if code, _ := call(http.MethodPost, `{"sources":["nope"]}`); code != http.StatusBadRequest {
		t.Fatalf("unknown source: %d", code)
	}
	if code, body := call(http.MethodPost, `{}`); code != http.StatusAccepted || body["run"] == nil {
		t.Fatalf("start: %d %#v", code, body)
	}
	if code, body := call(http.MethodGet, ""); code != http.StatusOK || body["active"] != true || body["safe_to_unplug"] != false {
		t.Fatalf("status while running: %d %#v", code, body)
	}
	if code, _ := call(http.MethodPost, `{}`); code != http.StatusConflict {
		t.Fatalf("second capture: %d", code)
	}
	if _, err := Capture(context.Background(), config, config.Sources, nil); !errors.Is(err, errCaptureBusy) {
		t.Fatalf("CLI capture during an API capture: %v", err)
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body := call(http.MethodGet, "")
		run, _ := body["run"].(map[string]any)
		if run["state"] == "complete" && body["safe_to_unplug"] == true {
			if run["files_copied"] != float64(1) || run["completed_sources"] != float64(1) {
				t.Fatalf("finished run: %#v", run)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture did not finish: %#v", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCaptureAPIReleasesItsLockWhenAStopBeginsFirst(t *testing.T) {
	useHost(t, "host-a")
	catalog, config := testCatalog(t)
	claude := filepath.Join(t.TempDir(), "claude")
	writeSourceFile(t, filepath.Join(claude, "p", "s.jsonl"), "{}\n")
	config.CaptureRoot = filepath.Join(t.TempDir(), "captures")
	config.Sources = []SourceConfig{{Name: "claude", Kind: "claude", Path: claude, Enabled: true}}
	server := NewServer(config, catalog)
	// The request was admitted; then a release begins before its worker starts.
	server.beginStop()
	response := httptest.NewRecorder()
	server.startCapture(response, map[string]any{})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("capture after a stop began: %d %s", response.Code, response.Body.String())
	}
	if runs := server.captures.list(); len(runs) != 1 || runs[0].State != "failed" || runs[0].CompletedAt == nil {
		t.Fatalf("runs %#v", runs)
	}
	if server.captureActive() {
		t.Fatal("capture still reported active")
	}
	session, err := beginCapture(config)
	if err != nil {
		t.Fatalf("capture lock left held: %v", err)
	}
	session.Close()
}

func TestCaptureExports(t *testing.T) {
	useHost(t, "host-a")
	root := t.TempDir()
	chatgpt, canonical, single := filepath.Join(root, "chatgpt"), filepath.Join(root, "canonical"), filepath.Join(root, "release.json")
	writeSourceFile(t, filepath.Join(chatgpt, "conversations.json"), "[]")
	writeSourceFile(t, filepath.Join(chatgpt, "user.json"), "{}")
	writeSourceFile(t, filepath.Join(canonical, "a", "one.json"), "{}")
	writeSourceFile(t, filepath.Join(canonical, "notes.md"), "")
	writeSourceFile(t, single, "{}")
	config := captureTestConfig(t, SourceConfig{Name: "chatgpt", Kind: "chatgpt-export", Path: chatgpt, Enabled: true},
		SourceConfig{Name: "canonical", Kind: "canonical", Path: canonical, Enabled: true}, SourceConfig{Name: "release", Kind: "tl1-export", Path: single, Enabled: true})
	summary := runCapture(t, config)
	if !summary.OK {
		t.Fatalf("export capture: %#v", summary)
	}
	for source, want := range map[string]string{"chatgpt": "files/conversations.json", "canonical": "files/a/one.json", "release": "files/release.json"} {
		manifest := readCaptureManifest(t, config, source)
		if len(manifest.Files) != 1 || manifest.Files[0].Captured != want {
			t.Fatalf("%s captured %#v", source, manifest.Files)
		}
	}
}
