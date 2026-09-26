package archive

import (
	"context"
	"sync"
	"time"
)

// Several pages show data derived from the whole catalog: the Settings health
// cards, the Usage table (every session's usage per day and model, priced),
// the Writing and TL1 tables, and the per-workspace costs behind the Library's
// cost columns. Each takes up to a couple of seconds to compute on a large
// catalog, and a page asks for it several times at once (rows, chart,
// metrics), so derivedCache keeps each value until the next commit (see
// dataVersionMonitor):
//
//   - A key names the value and whatever else it depends on, such as the local
//     date for costs at today's prices.
//   - Concurrent requests for a missing value wait for one computation. It
//     finishes even if the request that started it is abandoned, since the
//     others still want it and the next request can use it.
//   - After a commit, warmCaches recomputes the values pages used recently,
//     so the next visit does not wait.
//
// Cached values are shared between requests and must never be modified.
type derivedCache struct {
	mu       sync.Mutex
	versions dataVersionMonitor
	// generation advances whenever the monitor reconnects, so a value
	// computed against the previous connection's versions is not stored.
	generation int64
	entries    map[string]*derivedEntry
}

type derivedEntry struct {
	version int64
	ready   chan struct{} // closed once value and err are set
	value   any
	err     error
	// compute and usedAt let warmCaches recompute a value still in use.
	compute func(context.Context) (any, error)
	usedAt  time.Time
}

// recentUse is how long after its last request a value is kept warm.
const recentUse = 30 * time.Minute

func (cache *derivedCache) close() {
	cache.versions.close()
	cache.entries = nil
	cache.generation++
}

// cachedValue returns key's value while the catalog is unchanged, and
// otherwise computes it once for every caller that asks meanwhile.
func cachedValue[T any](ctx context.Context, c *Catalog, key string, compute func(context.Context) (T, error)) (T, error) {
	value, err := c.derivedValue(ctx, key, func(ctx context.Context) (any, error) { return compute(ctx) }, true)
	if err != nil {
		var zero T
		return zero, err
	}
	return value.(T), nil
}

// derivedValue is cachedValue without a type. A request marks the value as
// used, which keeps it warm.
func (c *Catalog) derivedValue(ctx context.Context, key string, compute func(context.Context) (any, error), request bool) (any, error) {
	cache := &c.derived
	cache.mu.Lock()
	version, err := cache.versions.read(ctx, c.Path)
	if err != nil {
		cache.entries = nil
		cache.generation++
		cache.mu.Unlock()
		return compute(ctx)
	}
	entry := cache.entries[key]
	if entry == nil || entry.version != version {
		usedAt := time.Time{}
		if entry != nil {
			usedAt = entry.usedAt
		}
		entry = &derivedEntry{version: version, ready: make(chan struct{}), compute: compute, usedAt: usedAt}
		if cache.entries == nil {
			cache.entries = map[string]*derivedEntry{}
		}
		cache.entries[key] = entry
		if request {
			entry.usedAt = time.Now()
		}
		generation := cache.generation
		cache.mu.Unlock()
		value, err := compute(context.WithoutCancel(ctx))
		cache.mu.Lock()
		entry.value, entry.err = value, err
		if (err != nil || cache.generation != generation) && cache.entries[key] == entry {
			delete(cache.entries, key)
		}
		close(entry.ready)
	} else if request {
		entry.usedAt = time.Now()
	}
	cache.mu.Unlock()
	select {
	case <-entry.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return entry.value, entry.err
}

// catalogVersion reads the catalog's data version on the cache's monitor.
func (c *Catalog) catalogVersion(ctx context.Context) (int64, error) {
	cache := &c.derived
	cache.mu.Lock()
	defer cache.mu.Unlock()
	version, err := cache.versions.read(ctx, c.Path)
	if err != nil {
		cache.entries = nil
		cache.generation++
	}
	return version, err
}

// warmCaches recomputes, once the catalog has been quiet for a maintenance
// cycle after a change, the Library rows and derived values requested
// recently, so the first page after a sync or an MCP call finds them ready
// rather than waiting a second or more on a cold drive. After startup it
// warms the Library, the page the app opens on, the usage behind it, and the
// Tools rollup.
func (c *Catalog) warmCaches(ctx context.Context) {
	version, err := c.catalogVersion(ctx)
	if err != nil || version != c.quietVersion {
		c.quietVersion = version
		return
	}
	if version == c.warmVersion {
		return
	}
	first := c.warmVersion == 0
	c.warmVersion = version
	// The rows keep the fields recent requests needed, such as long text.
	if usedAt, fields := c.libraryUse(); first || time.Since(usedAt) < recentUse {
		_, _ = c.cachedLibraryRows(ctx, libraryCompactFields.union(fields))
	}
	if first {
		// Usage, the Settings pricing card, and the Library's cost columns;
		// and the Tools rollup, which an upgrade must build before the page
		// can show it.
		_, _ = c.localUsageRows(ctx)
		_, _ = c.allWorkspaceUsage(ctx)
		_ = c.currentToolRollup(ctx)
	}
	cache := &c.derived
	cache.mu.Lock()
	stale := map[string]func(context.Context) (any, error){}
	for key, entry := range cache.entries {
		if entry.version != version && time.Since(entry.usedAt) < recentUse {
			stale[key] = entry.compute
		}
	}
	cache.mu.Unlock()
	for key, compute := range stale {
		if ctx.Err() != nil {
			return
		}
		_, _ = c.derivedValue(ctx, key, compute, false)
	}
}
