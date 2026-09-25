package archive

import (
	"context"
	"database/sql/driver"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

func (c *sourceCapture) captureDatabases(items []captureItem, present map[string]bool) error {
	for _, item := range items {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		rel := path.Join(captureFilesDir, item.rel)
		state, err := sqliteState(item.path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				c.fail(item.path, err)
				present[rel] = true
			}
			continue
		}
		present[rel] = true
		c.result.DatabasesSeen++
		previous := c.snapshots[rel]
		if previous != nil {
			previous.MissingSince = ""
			if previous.Source == state && c.intact(previous.Captured, previous.Size, previous.CapturedMTimeNS) {
				c.result.SnapshotsUnchanged++
				continue
			}
		}
		snapshot, err := c.snapshot(item, rel, state, previous)
		if err != nil {
			var destination destinationError
			if errors.As(err, &destination) {
				return err
			}
			c.fail(item.path, err)
			continue
		}
		c.snapshots[rel] = snapshot
		c.result.SnapshotsTaken++
		c.result.SnapshotBytes += snapshot.Size
		c.report()
		if err := c.commit(false); err != nil {
			return err
		}
		c.removePruned(false)
	}
	return nil
}

func sqliteState(file string) (sqliteSourceState, error) {
	info, err := os.Stat(file)
	if err != nil {
		return sqliteSourceState{}, err
	}
	if !info.Mode().IsRegular() {
		return sqliteSourceState{}, fs.ErrNotExist
	}
	state := sqliteSourceState{Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}
	if wal, err := os.Stat(file + "-wal"); err == nil {
		state.WALSize, state.WALMTimeNS = wal.Size(), wal.ModTime().UnixNano()
	}
	return state, nil
}

// snapshot replaces a database's latest snapshot, keeping the previous one as
// a generation. Retention keeps the newest capture_snapshot_generations.
func (c *sourceCapture) snapshot(item captureItem, rel string, state sqliteSourceState, previous *capturedSnapshot) (*capturedSnapshot, error) {
	if captureFault != nil {
		if err := captureFault("snapshot", rel); err != nil {
			return nil, toDestination(err)
		}
	}
	if err := c.ensureSpace(state.Size + state.WALSize); err != nil {
		return nil, err
	}
	destination := filepath.Join(c.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return nil, toDestination(err)
	}
	temporary := destination + captureTempSuffix
	os.Remove(temporary)
	if err := snapshotSQLite(c.ctx, item.path, temporary); err != nil {
		os.Remove(temporary)
		return nil, err
	}
	info, err := syncPath(temporary)
	if err != nil {
		os.Remove(temporary)
		return nil, toDestination(err)
	}
	snapshot := &capturedSnapshot{Path: item.path, Captured: rel, Size: info.Size(), CapturedMTimeNS: info.ModTime().UnixNano(), CapturedAt: now(), Method: "sqlite-backup", Source: state}
	if previous != nil {
		snapshot.Generations = slices.Clone(previous.Generations)
		if c.session.generations > 0 {
			source := previous.Source
			version := capturedVersion{Captured: previous.Captured, Size: previous.Size, CapturedAt: previous.CapturedAt, Source: &source}
			intact := c.intact(previous.Captured, previous.Size, previous.CapturedMTimeNS)
			kept, err := c.preserve(version, intact)
			if err != nil && intact {
				os.Remove(temporary)
				return nil, err
			}
			// Otherwise an interrupted rotation may already have replaced the
			// latest, leaving its predecessor in history to adopt.
			if err == nil {
				snapshot.Generations = append([]capturedVersion{kept}, snapshot.Generations...)
			}
		}
	}
	if err := os.Rename(temporary, destination); err != nil {
		os.Remove(temporary)
		return nil, toDestination(err)
	}
	snapshot.Generations = c.retain(item.path, snapshot.Generations)
	return snapshot, nil
}

// retain keeps the newest capture_snapshot_generations superseded snapshots
// and queues the rest for pruning once the manifest no longer names them.
// Where captures are indexed (see capture_index.go), a snapshot the index has
// not yet taken what it needs from is kept too, up to as many again, so
// sessions deleted at the source survive until indexed; past that the oldest
// goes with a warning, so an index that never runs cannot fill the drive.
func (c *sourceCapture) retain(path string, generations []capturedVersion) []capturedVersion {
	limit := c.session.generations
	if len(generations) <= limit {
		return generations
	}
	kept, extra := slices.Clone(generations[:limit]), 0
	for _, old := range generations[limit:] {
		needed := c.indexed.exists && !c.indexed.has(path, old.CapturedAt)
		if needed && extra < limit {
			kept = append(kept, old)
			extra++
			continue
		}
		if needed {
			c.result.Warnings = append(c.result.Warnings, captureError{Path: old.Captured, Error: "pruned a database snapshot that was never indexed; sessions deleted at the source before it was superseded may be lost"})
		}
		c.prune = append(c.prune, old.Captured)
	}
	return kept
}

// snapshotSQLite writes a transactionally consistent copy of src to dst with
// SQLite's online backup API from a read-only, query-only connection. It was
// measured faster and lighter than VACUUM INTO, which also needs a connection
// that is not query-only.
func snapshotSQLite(ctx context.Context, src, dst string) error {
	for attempt := 0; ; attempt++ {
		err := backupSQLite(ctx, src, dst)
		if err == nil || attempt >= 3 || !strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "busy") {
			return err
		}
		os.Remove(dst)
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
}

// backupStepPages bounds how long a snapshot runs before it notices that
// its context has ended: 4096 pages is 16 MB at SQLite's default page size.
var backupStepPages int32 = 4096

func backupSQLite(ctx context.Context, src, dst string) error {
	db, err := openReadOnlySQLite(src)
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Every step copies pages from the read transaction held here, so the
	// snapshot is consistent while the owning app keeps committing (WAL does
	// not block it). Without it each step would take a new read transaction,
	// and any foreign commit between steps would restart the copy.
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	var tables int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema").Scan(&tables); err != nil {
		return err
	}
	// The snapshot is a temporary file until it is fsynced and renamed, so
	// SQLite need not journal or sync it.
	target := (&url.URL{Scheme: "file", Path: dst}).String() + "?_pragma=journal_mode(off)&_pragma=synchronous(off)"
	return conn.Raw(func(driverConn any) error {
		source, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("the SQLite driver cannot make online backups")
		}
		backup, err := source.NewBackup(target)
		if err != nil {
			return err
		}
		// Stepping in chunks lets a release (before an eject) stop a
		// multi-gigabyte snapshot promptly.
		for more := true; more; {
			if err := ctx.Err(); err != nil {
				backup.Finish()
				return err
			}
			if more, err = backup.Step(backupStepPages); err != nil {
				backup.Finish()
				return err
			}
		}
		snapshot, err := backup.Commit()
		if err != nil {
			return err
		}
		defer snapshot.Close()
		// The copy inherits the source's WAL flag; a snapshot is one
		// self-contained file that opens read-only without -wal/-shm files.
		exec, ok := snapshot.(driver.ExecerContext)
		if !ok {
			return errors.New("the SQLite driver cannot configure the snapshot")
		}
		_, err = exec.ExecContext(ctx, "PRAGMA journal_mode=DELETE", nil)
		return err
	})
}
