package archive

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Initialize migrates, backfills, and scans whole tables on every open, taking
// the write lock. A query open (one per MCP tool call) skips it when this very
// build has already fully initialized the catalog: every migration and
// backfill it would run has then run.

// initializedBuildKey names the meta row recording the last build whose
// Initialize completed. Builds that predate it leave it unchanged, so their
// visible effects (schema_version, the Library projection) are checked too.
const initializedBuildKey = "initialized_build"

// buildDigest identifies this executable's bytes, so any code change, including
// a migration added without a schema version bump, forces one full Initialize.
// "" (unreadable) never matches.
var buildDigest = sync.OnceValue(func() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
})

func (c *Catalog) recordInitializedBuild() error {
	digest := buildDigest()
	if digest == "" {
		return nil
	}
	_, err := c.DB.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE
		SET value=excluded.value WHERE meta.value<>excluded.value`, initializedBuildKey, digest)
	return err
}

// errCatalogNeedsUpgrade refuses an older catalog on the MCP's direct path.
// Upgrading it (and, from before hosts, claiming its rows for this Mac) is the
// app's job; an MCP call racing the starting service must not do it too.
var errCatalogNeedsUpgrade = errors.New("This library needs to be opened by the Pharos app once to upgrade it")

// initializedByThisBuild reports whether Initialize can be skipped, and
// refuses a catalog that would need a migration. It only reads, so it never
// waits for a writer.
func (c *Catalog) initializedByThisBuild() (bool, error) {
	rows, err := queryMaps(c.DB, "SELECT key,value FROM meta WHERE key IN ('schema_version','workspace_library_version',?)", initializedBuildKey)
	if err != nil && !strings.Contains(err.Error(), "no such table") {
		return false, err
	}
	stored := map[string]string{}
	for _, row := range rows {
		stored[firstString(row["key"])] = firstString(row["value"])
	}
	if version, _ := strconv.Atoi(strings.TrimSpace(stored["schema_version"])); version < catalogSchemaVersion {
		return false, fmt.Errorf("%w (%s has catalog schema version %d; this build uses %d)", errCatalogNeedsUpgrade, c.Path, version, catalogSchemaVersion)
	}
	digest := buildDigest()
	return digest != "" && stored["schema_version"] == strconv.Itoa(catalogSchemaVersion) && stored[initializedBuildKey] == digest &&
		stored["workspace_library_version"] == libraryVersion(), nil
}

// OpenCatalogForQuery opens an existing catalog for MCP tool calls, which
// write only their call history. Connections get the same pragmas as
// OpenCatalog. Initialize runs only when this build has not initialized a
// catalog of its own schema version, which is idempotent: an older catalog is
// refused (see errCatalogNeedsUpgrade), and one from a newer build is refused
// by Initialize's version check before anything is written.
func OpenCatalogForQuery(path string) (*Catalog, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", catalogDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	catalog := &Catalog{Path: path, DB: db}
	current, err := catalog.initializedByThisBuild()
	if err == nil && !current {
		err = catalog.initializeLocked(false)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return catalog, nil
}

// Initialize writes statement by statement, so another process's Initialize
// could run in its gaps: during the service's upgrade of a large catalog, an
// MCP call would redo the backfills alongside it and stall on the write lock.
// An advisory lock beside the catalog runs one Initialize at a time.
// flock is released when its process exits, so it never goes stale.
const initializeLockName = ".initialize.lock"

var errCatalogInitializing = errors.New("Pharos is upgrading this library's catalog; try again in a moment")

// initializeLocked runs Initialize under the initialize lock. Unless wait, it
// gives up after a moment with errCatalogInitializing, and skips Initialize
// when whoever held the lock initialized the catalog for this build.
func (c *Catalog) initializeLocked(wait bool) error {
	file, err := os.OpenFile(filepath.Join(filepath.Dir(c.Path), initializeLockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	for deadline := time.Now().Add(2 * time.Second); ; {
		how := syscall.LOCK_EX
		if !wait {
			how |= syscall.LOCK_NB
		}
		err = syscall.Flock(int(file.Fd()), how)
		if err == nil {
			break
		} else if err == syscall.EWOULDBLOCK && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		} else if err == syscall.EWOULDBLOCK {
			return errCatalogInitializing
		} else if err != syscall.EINTR {
			return err
		}
	}
	if !wait {
		if current, err := c.initializedByThisBuild(); err != nil || current {
			return err
		}
	}
	return c.Initialize()
}

// closeQuery closes without Close's truncating checkpoint, which takes the
// write lock, and so would after every tool call. Only an oversized WAL is
// checkpointed. SQLite still checkpoints when the last connection of any
// process closes, which leaves nothing to replay if the drive is then pulled.
func (c *Catalog) closeQuery() error {
	c.boundWAL(walSizeLimit)
	c.library.mu.Lock()
	c.library.close()
	c.library.mu.Unlock()
	c.derived.mu.Lock()
	c.derived.close()
	c.derived.mu.Unlock()
	return c.DB.Close()
}
