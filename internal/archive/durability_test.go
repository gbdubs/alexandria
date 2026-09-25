package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() { commitFullFsync = false }

func walBytes(t *testing.T, catalog *Catalog) int64 {
	t.Helper()
	info, err := os.Stat(catalog.Path + "-wal")
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// writeRows commits a few pages, well below the automatic checkpoint.
func writeRows(t *testing.T, db *sql.DB, prefix string) {
	t.Helper()
	for index := 0; index < 20; index++ {
		if _, err := db.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)", prefix+string(rune('a'+index)), strings.Repeat("x", 2000)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatalogPoolAppliesDurabilityPragmas(t *testing.T) {
	commitFullFsync = true
	t.Cleanup(func() { commitFullFsync = false })
	catalog, _ := testCatalog(t)
	if err := catalog.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"busy_timeout": busyTimeoutMS, "foreign_keys": 1, "synchronous": 2, "fullfsync": 1,
		"checkpoint_fullfsync": 1, "journal_size_limit": walSizeLimit}
	ctx := context.Background()
	// Holding every pooled connection at once checks each one, including the
	// one Checkpoint borrowed and returned.
	for index := 0; index < 4; index++ {
		connection, err := catalog.DB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		for pragma, value := range want {
			var got int64
			if err := connection.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != value {
				t.Fatalf("catalog connection %d has %s=%d, want %d", index, pragma, got, value)
			}
		}
		var mode string
		if err := connection.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
			t.Fatalf("catalog connection %d journal_mode=%q (%v)", index, mode, err)
		}
	}
}

func TestSyncTruncatesWALWhileLibraryMonitorIsOpen(t *testing.T) {
	catalog, config := testCatalog(t)
	export := filepath.Join(t.TempDir(), "export.json")
	payload, _ := json.Marshal(map[string]any{"workspaces": []any{map[string]any{
		"id": "work-1", "title": "Fix parser", "activity_at": "2026-09-20T12:00:00Z",
		"conversations": []any{map[string]any{"id": "conversation-1", "provider": "codex", "messages": []any{
			map[string]any{"id": "message-1", "role": "user", "text": "Please inspect src/parser.go"},
			map[string]any{"id": "message-2", "role": "assistant", "text": "Fixed the parser"},
		}}},
	}}})
	if err := os.WriteFile(export, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	config.Sources = []SourceConfig{{Name: "fixture", Kind: "canonical", Path: export, Account: "local", Enabled: true}}
	// The Library's data_version monitor stays open between requests.
	if rows, err := catalog.searchRows(SearchOptions{}, libraryCompactFields); err != nil || len(rows) != 0 {
		t.Fatalf("library before sync: %v %v", rows, err)
	}
	if catalog.library.versions.conn == nil {
		t.Fatal("library monitor connection was not opened")
	}
	server := NewServer(config, catalog)
	request := httptest.NewRequest(http.MethodPost, "/api/sources/sync", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok":true`) {
		t.Fatalf("sync status %d: %s", response.Code, response.Body.String())
	}
	if size := walBytes(t, catalog); size != 0 {
		t.Fatalf("WAL holds %d bytes after the sync", size)
	}
	rows, err := catalog.searchRows(SearchOptions{}, libraryCompactFields)
	if err != nil || len(rows) != 1 {
		t.Fatalf("library after sync: %v %v", rows, err)
	}
}

func TestCloseEmptiesWALWhileAnotherProcessHasCatalogOpen(t *testing.T) {
	catalog, _ := testCatalog(t)
	other := otherProcess(t, catalog)
	var count int
	if err := other.DB.QueryRow("SELECT COUNT(*) FROM meta").Scan(&count); err != nil {
		t.Fatal(err)
	}
	writeRows(t, catalog.DB, "close-")
	if walBytes(t, catalog) == 0 {
		t.Fatal("writes left nothing in the WAL")
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	// The other connection keeps SQLite from deleting the log on close.
	if _, err := os.Stat(catalog.Path + "-wal"); err != nil {
		t.Fatal(err)
	}
	if size := walBytes(t, catalog); size != 0 {
		t.Fatalf("WAL holds %d bytes after Close", size)
	}
	if err := other.DB.QueryRow("SELECT COUNT(*) FROM meta WHERE key LIKE 'close-%'").Scan(&count); err != nil || count != 20 {
		t.Fatalf("checkpointed rows: %d %v", count, err)
	}
}

func TestCheckpointGivesUpOnAnOldReaderWithoutHoldingUpWriters(t *testing.T) {
	catalog, _ := testCatalog(t)
	other := otherProcess(t, catalog)
	writeRows(t, catalog.DB, "before-")
	reader, err := other.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var count int
	if err := reader.QueryRow("SELECT COUNT(*) FROM meta").Scan(&count); err != nil {
		t.Fatal(err)
	}
	writeRows(t, catalog.DB, "after-")
	started := time.Now()
	if err := catalog.Checkpoint(); !errors.Is(err, errCheckpointBusy) {
		t.Fatalf("checkpoint with an old reader: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("checkpoint waited %s for the reader", elapsed)
	}
	if walBytes(t, catalog) == 0 {
		t.Fatal("WAL was truncated under a reader")
	}
	writeRows(t, catalog.DB, "later-")
	ctx := context.Background()
	connections := []*sql.Conn{}
	for index := 0; index < 4; index++ {
		connection, err := catalog.DB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
		var timeout int
		if err := connection.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != busyTimeoutMS {
			t.Fatalf("connection %d busy_timeout=%d (%v) after checkpoint", index, timeout, err)
		}
	}
	// With every connection taken, Checkpoint gives up instead of waiting.
	if err := catalog.Checkpoint(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("checkpoint without a free connection: %v", err)
	}
	for _, connection := range connections {
		connection.Close()
	}
	reader.Rollback()
	if err := catalog.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if size := walBytes(t, catalog); size != 0 {
		t.Fatalf("WAL holds %d bytes after the reader finished", size)
	}
}

// The service's periodic checkpoint must empty the WAL while Library reads,
// including the data_version monitor, keep overlapping.
func TestKeepWALSmallTruncatesUnderContinuousReads(t *testing.T) {
	catalog, _ := libraryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	defer func() { cancel(); readers.Wait() }()
	for index := 0; index < 3; index++ {
		readers.Add(1)
		go func(index int) {
			defer readers.Done()
			for ctx.Err() == nil {
				if index == 0 {
					_, _ = catalog.searchRows(SearchOptions{ctx: ctx}, libraryCompactFields)
				} else {
					_, _ = queryMapsContext(ctx, catalog.DB, "SELECT w.id,COUNT(m.id) FROM workspaces w LEFT JOIN conversations c ON c.workspace_id=w.id LEFT JOIN messages m ON m.conversation_id=c.id GROUP BY w.id")
				}
			}
		}(index)
	}
	writeRows(t, catalog.DB, "keep-")
	if walBytes(t, catalog) == 0 {
		t.Fatal("writes left nothing in the WAL")
	}
	go catalog.keepWALSmall(ctx, 20*time.Millisecond, 0)
	deadline := time.Now().Add(10 * time.Second)
	for walBytes(t, catalog) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("WAL still holds %d bytes under continuous reads", walBytes(t, catalog))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A deferred transaction that reads first cannot take the write lock once
// another connection has committed; ingest must hold the lock from the start.
func TestBeginWriteHoldsTheWriteLockFromItsFirstStatement(t *testing.T) {
	catalog, _ := testCatalog(t)
	other, err := sql.Open("sqlite", "file:"+catalog.Path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := catalog.beginWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec("INSERT INTO meta(key,value) VALUES('probe','1')"); err == nil {
		t.Fatal("another connection committed while beginWrite's transaction was open")
	}
	var workspaces int
	if err := tx.QueryRow("SELECT COUNT(*) FROM workspaces").Scan(&workspaces); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO meta(key,value) VALUES('after_read','1')"); err != nil {
		t.Fatalf("write after a read failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec("INSERT INTO meta(key,value) VALUES('probe','1')"); err != nil {
		t.Fatalf("write lock not released on commit: %v", err)
	}
}
