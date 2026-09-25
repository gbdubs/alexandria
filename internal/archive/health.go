package archive

import (
	"context"
	"os"
	"sync"
)

// The Settings health cards load as independent sections so a slow one never
// holds back the rest: counts (COUNT(*) over millions of messages reads a
// large index when the page cache is cold), pricing (reprices the whole usage
// ledger), storage (filesystem and diskutil calls), and freshness.
// healthCache keeps the two catalog-derived sections until the next commit.
type healthCache struct {
	mu       sync.Mutex
	versions dataVersionMonitor
	// generation advances whenever the monitor reconnects, so a section
	// computed against the previous connection's versions is not stored.
	generation int64
	sections   map[string]healthSection
}

type healthSection struct {
	version int64
	value   map[string]any
}

func (cache *healthCache) close() {
	cache.versions.close()
	cache.sections = nil
	cache.generation++
}

// cachedHealthSection returns name's cached value while the catalog is
// unchanged, otherwise computes it outside the lock so sections load in
// parallel. The returned map may be shared and must not be modified.
func (c *Catalog) cachedHealthSection(ctx context.Context, name string, compute func(context.Context) (map[string]any, error)) (map[string]any, error) {
	cache := &c.health
	cache.mu.Lock()
	version, versionErr := cache.versions.read(ctx, c.Path)
	if versionErr != nil {
		cache.sections = nil
		cache.generation++
	} else if section, ok := cache.sections[name]; ok && section.version == version {
		cache.mu.Unlock()
		return section.value, nil
	}
	generation := cache.generation
	cache.mu.Unlock()
	value, err := compute(ctx)
	if err != nil || versionErr != nil {
		return value, err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.generation == generation {
		if cache.sections == nil {
			cache.sections = map[string]healthSection{}
		}
		cache.sections[name] = healthSection{version: version, value: value}
	}
	return value, nil
}

// HealthCounts reports the indexed workspace and message totals.
func (c *Catalog) HealthCounts(ctx context.Context) (map[string]any, error) {
	return c.cachedHealthSection(ctx, "counts", func(ctx context.Context) (map[string]any, error) {
		result := map[string]any{}
		for _, table := range []string{"workspaces", "messages"} {
			var value int64
			if err := c.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&value); err != nil {
				return nil, err
			}
			result[table] = value
		}
		return result, nil
	})
}

// PricingHealth reports how much usage has a known price.
func (c *Catalog) PricingHealth(ctx context.Context) (map[string]any, error) {
	return c.cachedHealthSection(ctx, "pricing", func(context.Context) (map[string]any, error) {
		return c.pricingHealth(), nil
	})
}

func (c *Catalog) indexBytes() int64 {
	if info, err := os.Stat(c.Path); err == nil {
		return info.Size()
	}
	return 0
}

// Health reports every section at once, plus per-model token totals, for the
// CLI and API clients that want a single document. The Settings page fetches
// the sections separately.
func (c *Catalog) Health() map[string]any {
	ctx := context.Background()
	health := map[string]any{"freshness": c.Freshness(), "workspaces": int64(0), "messages": int64(0),
		"index_bytes": c.indexBytes(), "token_usage": c.tokenUsageByModel()}
	if counts, err := c.HealthCounts(ctx); err == nil {
		health["workspaces"], health["messages"] = counts["workspaces"], counts["messages"]
	}
	health["pricing"], _ = c.PricingHealth(ctx)
	return health
}

func (c *Catalog) tokenUsageByModel() []map[string]any {
	rows, err := queryMaps(c.DB, `WITH metric_totals AS (
		SELECT workspace_id,conversation_id,
			COALESCE(
				MAX(CASE WHEN name='total_tokens' THEN value END),
				SUM(CASE WHEN name IN ('input_tokens','output_tokens','cache_tokens') THEN value ELSE 0 END)
			) tokens,
			COALESCE(MAX(CASE WHEN name='uncached_input_tokens' THEN value END),0) uncached_input_tokens,
			COALESCE(MAX(CASE WHEN name='cache_read_input_tokens' THEN value END),0) cache_read_input_tokens,
			COALESCE(MAX(CASE WHEN name='cache_creation_input_tokens' THEN value END),0) cache_creation_input_tokens,
			COALESCE(MAX(CASE WHEN name='cache_creation_5m_input_tokens' THEN value END),0) cache_creation_5m_input_tokens,
			COALESCE(MAX(CASE WHEN name='cache_creation_1h_input_tokens' THEN value END),0) cache_creation_1h_input_tokens,
			COALESCE(MAX(CASE WHEN name='output_tokens' THEN value END),0) output_tokens,
			COALESCE(MAX(CASE WHEN name='reasoning_output_tokens' THEN value END),0) reasoning_output_tokens,
			COALESCE(MAX(CASE WHEN name='unclassified_tokens' THEN value END),0) unclassified_tokens
		FROM metrics
		WHERE unit='tokens'
		GROUP BY workspace_id,conversation_id
	), workspace_identity AS (
		SELECT w.id workspace_id,
			CASE WHEN COUNT(DISTINCT cx.provider)=1 THEN MAX(cx.provider) ELSE w.source_kind END service,
			CASE
				WHEN COUNT(cx.id)=0 THEN 'Unknown model'
				WHEN COUNT(DISTINCT COALESCE(NULLIF(TRIM(cx.model),''),'Unknown model'))=1
					THEN MAX(COALESCE(NULLIF(TRIM(cx.model),''),'Unknown model'))
				ELSE 'Multiple models'
			END model
		FROM (SELECT DISTINCT workspace_id FROM metric_totals) used
		CROSS JOIN workspaces w ON w.id=used.workspace_id
		LEFT JOIN conversations cx ON cx.workspace_id=w.id
		GROUP BY w.id
	)
	SELECT COALESCE(cx.provider,wi.service) service,
		COALESCE(NULLIF(TRIM(cx.model),''),wi.model,'Unknown model') model,
		CAST(SUM(mt.tokens) AS INTEGER) tokens,
		CAST(SUM(mt.uncached_input_tokens) AS INTEGER) uncached_input_tokens,
		CAST(SUM(mt.cache_read_input_tokens) AS INTEGER) cache_read_input_tokens,
		CAST(SUM(mt.cache_creation_input_tokens) AS INTEGER) cache_creation_input_tokens,
		CAST(SUM(mt.cache_creation_5m_input_tokens) AS INTEGER) cache_creation_5m_input_tokens,
		CAST(SUM(mt.cache_creation_1h_input_tokens) AS INTEGER) cache_creation_1h_input_tokens,
		CAST(SUM(mt.output_tokens) AS INTEGER) output_tokens,
		CAST(SUM(mt.reasoning_output_tokens) AS INTEGER) reasoning_output_tokens,
		CAST(SUM(mt.unclassified_tokens) AS INTEGER) unclassified_tokens
	FROM metric_totals mt
	JOIN workspace_identity wi ON wi.workspace_id=mt.workspace_id
	LEFT JOIN conversations cx ON cx.id=mt.conversation_id
	WHERE mt.tokens>0
	GROUP BY COALESCE(cx.provider,wi.service),COALESCE(NULLIF(TRIM(cx.model),''),wi.model,'Unknown model')
	ORDER BY tokens DESC,service,model`)
	if err != nil {
		return []map[string]any{}
	}
	return rows
}
