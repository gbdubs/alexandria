package archive

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Snapshots step through one read transaction: a writer committing between
// every one-page step neither restarts the copy forever nor makes it
// inconsistent, and cancelling stops it.
func TestSnapshotSQLiteStepsThroughOneReadTransaction(t *testing.T) {
	previous := backupStepPages
	backupStepPages = 1
	t.Cleanup(func() { backupStepPages = previous })
	dir := t.TempDir()
	file := filepath.Join(dir, "source.db")
	db := openWritableSQLite(t, file)
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
	for range 2000 {
		if err := commit(); err != nil {
			t.Fatal(err)
		}
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	target := filepath.Join(dir, "snapshot.db")
	err := snapshotSQLite(ctx, file, target)
	during := commits.Load()
	close(stop)
	if writeErr := <-done; writeErr != nil {
		t.Fatalf("writer: %v", writeErr)
	}
	if err != nil {
		t.Fatalf("snapshot with one-page steps under %d commits: %v", during, err)
	}
	if during == 0 {
		t.Fatal("the writer never committed during the snapshot")
	}
	snapshot, err := openReadOnlySQLite(target)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var counted, messages int
	var integrity string
	if err := snapshot.QueryRow("SELECT message_count,(SELECT COUNT(*) FROM messages) FROM sessions").Scan(&counted, &messages); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if counted != messages || messages < 2000 || integrity != "ok" {
		t.Fatalf("snapshot: message_count=%d messages=%d integrity=%s", counted, messages, integrity)
	}
	t.Logf("snapshot of %d messages while the writer committed %d times", messages, during)

	// Cancelled a few pages into a copy of thousands, it stops.
	cancelled := &cancelAfter{Context: context.Background()}
	cancelled.left.Store(5)
	if err := backupSQLite(cancelled, file, filepath.Join(dir, "cancelled.db")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot: %v", err)
	}
}

// cancelAfter reports itself cancelled once Err has been asked enough times.
type cancelAfter struct {
	context.Context
	left atomic.Int64
}

func (c *cancelAfter) Err() error {
	if c.left.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}
