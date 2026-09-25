package archive

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"
)

// The catalog may live on an external drive that is unplugged mid-write, so
// every pooled connection runs with:
//
//   - synchronous=FULL with fullfsync: each commit ends in F_FULLFSYNC, which
//     makes the drive flush its write cache (a plain fsync on macOS does not),
//     so a commit that has returned survives the drive losing power. This costs
//     ~0.4 ms per commit on a Thunderbolt SSD and ~4.5 ms on the internal one.
//   - checkpoint_fullfsync: checkpoints flush the drive before the WAL is
//     reused. Without it a yank can corrupt the database rather than only lose
//     recent commits; modernc's build leaves it off.
//   - journal_size_limit: SQLite never shrinks the -wal file by itself. Once
//     the log restarts, the next commit truncates it to this size, which is far
//     above the ~4 MB between automatic checkpoints, so routine restarts never
//     truncate and regrow the file.
const (
	walSizeLimit     = 64 << 20
	busyTimeoutMS    = 5000
	checkpointWaitMS = 1000
	walRetryDelay    = 30 * time.Second
)

// commitFullFsync is off only in tests, whose many tiny commits would each
// wait ~4.5 ms for the internal SSD to flush.
var commitFullFsync = true

func catalogDSN(path string) string {
	fullfsync := 0
	if commitFullFsync {
		fullfsync = 1
	}
	return (&url.URL{Scheme: "file", Path: path}).String() + fmt.Sprintf("?_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)"+
		"&_pragma=synchronous(FULL)&_pragma=fullfsync(%d)&_pragma=checkpoint_fullfsync(1)&_pragma=journal_size_limit(%d)",
		busyTimeoutMS, fullfsync, walSizeLimit)
}

var errCheckpointBusy = errors.New("catalog checkpoint incomplete: the WAL is still in use")

// Checkpoint copies the WAL into the database and truncates it to zero bytes.
// It is best effort: rather than hold up writers, it gives up after about a
// second if a reader of an older snapshot or another writer is still active.
// Readers are never blocked.
func (c *Catalog) Checkpoint() error {
	// Don't wait indefinitely for a pooled connection either, e.g. in Close
	// while long queries still hold every connection.
	acquire, cancel := context.WithTimeout(context.Background(), time.Duration(checkpointWaitMS)*time.Millisecond)
	conn, err := c.DB.Conn(acquire)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx := context.Background()
	// A passive pass copies what it can without the write lock, so the
	// truncating pass below holds writers off only briefly.
	if _, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", checkpointWaitMS)); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS))
	var busy, logFrames, checkpointed int
	if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errCheckpointBusy
	}
	return nil
}

// walBound lets one goroutine at a time checkpoint an oversized WAL and backs
// off after readers kept a checkpoint from finishing.
type walBound struct {
	mu      sync.Mutex
	retryAt time.Time
}

// boundWAL checkpoints once the WAL outgrows limit. An automatic checkpoint
// cannot restart the log while any reader still uses it, and the Library's
// overlapping reads during a sync rarely leave such a gap, so without this the
// log grows by everything a sync writes: reads slow down searching it, and an
// unclean exit leaves all of it to replay on the next open. Syncs call this
// between transactions, where the writer does not contend for its own lock.
func (c *Catalog) boundWAL(limit int64) {
	if info, err := os.Stat(c.Path + "-wal"); err != nil || info.Size() <= limit {
		return
	}
	if !c.wal.mu.TryLock() {
		return
	}
	defer c.wal.mu.Unlock()
	if time.Now().Before(c.wal.retryAt) {
		return
	}
	if err := c.Checkpoint(); err != nil {
		c.wal.retryAt = time.Now().Add(walRetryDelay)
	}
}

// keepWALSmall bounds the WAL between syncs and while other processes write.
func (c *Catalog) keepWALSmall(ctx context.Context, every time.Duration, limit int64) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.boundWAL(limit)
		}
	}
}
