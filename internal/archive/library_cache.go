package archive

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"sync"
	"time"
)

// Every Library table request (rows, distinct values, aggregations) starts
// from the full unfiltered row set, which takes most of a second to build on a
// large catalog. libraryCache keeps the last set until anything it depends on
// may have changed:
//
//   - Any commit to the catalog, by this process or another (ingest runs as a
//     separate CLI). PRAGMA data_version changes whenever a connection other
//     than the one reading it commits, so it is read on a dedicated read-only
//     connection that never writes; every commit then counts.
//   - The local date, since cost_today_usd prices usage at today's rates.
//
// Cached row maps are shared between requests and must never be modified.
// Code that adds fields to rows copies them first (see libraryPage). The rows
// hold only the fields requests have needed (see libraryFields), which keeps
// the tens of megabytes of purpose text out of memory and out of each rebuild.
// Costs are also left out until a request filters, sorts, or groups on them.
type libraryCache struct {
	mu       sync.Mutex
	versions dataVersionMonitor
	version  int64
	day      string
	fields   libraryFields
	rows     []map[string]any
}

// clock returns the current time; tests replace now to change the date.
func (c *Catalog) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// cachedLibraryRows returns the unfiltered Library rows with at least fields,
// rebuilding them only after a commit, a date change, or a request for fields
// they lack. Concurrent misses wait for one rebuild.
func (c *Catalog) cachedLibraryRows(ctx context.Context, fields libraryFields) ([]map[string]any, error) {
	cache := &c.library
	cache.mu.Lock()
	defer cache.mu.Unlock()
	// Read the version before building: a commit during the build leaves the
	// rows tagged older than they are, which only costs an extra rebuild.
	version, versionErr := cache.dataVersion(ctx, c.Path)
	day := c.clock().Format("2006-01-02")
	current := versionErr == nil && cache.rows != nil && version == cache.version && day == cache.day
	if current && cache.fields.covers(fields) {
		return slices.Clone(cache.rows), nil
	}
	// Rebuild with the compact fields most requests need, adding long text and
	// costs only once a request uses them; keep fields still-current rows had, so requests
	// alternating between two long-text fields don't rebuild every time.
	fields = libraryCompactFields.union(fields)
	if current {
		fields = fields.union(cache.fields)
	}
	cache.rows = nil
	rows, err := c.computeSearchRows(SearchOptions{ctx: ctx}, fields)
	if err != nil {
		return nil, err
	}
	// Without a version the rows are still correct, just not reusable.
	if versionErr == nil {
		cache.rows, cache.version, cache.day, cache.fields = rows, version, day, fields
	}
	return slices.Clone(rows), nil
}

func (cache *libraryCache) dataVersion(ctx context.Context, path string) (int64, error) {
	version, err := cache.versions.read(ctx, path)
	if err != nil {
		// A new monitor connection's versions cannot be compared with these rows'.
		cache.rows = nil
	}
	return version, err
}

// close drops the monitor connection and the rows tagged with its versions.
func (cache *libraryCache) close() {
	cache.versions.close()
	cache.rows = nil
}

// dataVersionMonitor reads PRAGMA data_version on a dedicated read-only
// connection that never writes, so every commit to the catalog, by any
// connection or process, changes the version it reports. Callers serialize
// access, and must discard values tagged with earlier versions after an
// error, since the reopened connection starts a new sequence.
type dataVersionMonitor struct {
	monitor *sql.DB
	conn    *sql.Conn
}

func (m *dataVersionMonitor) read(ctx context.Context, path string) (int64, error) {
	if m.conn == nil {
		if path == "" {
			return 0, errors.New("catalog has no file to monitor")
		}
		monitor, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			return 0, err
		}
		conn, err := monitor.Conn(context.Background())
		if err != nil {
			monitor.Close()
			return 0, err
		}
		m.monitor, m.conn = monitor, conn
	}
	var version int64
	if err := m.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		m.close()
		return 0, err
	}
	return version, nil
}

func (m *dataVersionMonitor) close() {
	if m.conn != nil {
		m.conn.Close()
		m.monitor.Close()
	}
	m.monitor, m.conn = nil, nil
}
