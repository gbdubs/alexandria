package archive

import (
	"context"
	"os"
)

// The Settings health cards load as independent sections so a slow one never
// holds back the rest: counts (COUNT(*) over millions of messages reads a
// large index when the page cache is cold), pricing (reprices the whole usage
// ledger), storage (filesystem and diskutil calls), and freshness. The two
// catalog-derived sections are kept until the next commit (see derivedCache).

// HealthCounts reports indexed workspace, conversation, and message totals.
func (c *Catalog) HealthCounts(ctx context.Context) (map[string]any, error) {
	return cachedValue(ctx, c, "health:counts", func(ctx context.Context) (map[string]any, error) {
		result := map[string]any{}
		for table, count := range map[string]string{
			"workspaces":     "SELECT COUNT(*) FROM workspaces",
			"conversations":  "SELECT COUNT(*) FROM conversations",
			"messages":       "SELECT COALESCE((SELECT value FROM catalog_counts WHERE name='messages'),(SELECT COUNT(*) FROM messages))",
			"human_messages": "SELECT COUNT(*) FROM messages WHERE role='user' AND kind='message'",
		} {
			var value int64
			if err := c.DB.QueryRowContext(ctx, count).Scan(&value); err != nil {
				return nil, err
			}
			result[table] = value
		}
		return result, nil
	})
}

// ensureMessageCount keeps the number of messages in catalog_counts, since
// counting them reads an index of millions of entries: seconds on a cold
// drive. Triggers keep it current, whichever build writes.
func (c *Catalog) ensureMessageCount() error {
	var present int
	if err := c.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='catalog_counts_messages_delete'").Scan(&present); err != nil || present == 1 {
		return err
	}
	tx, err := c.beginWrite(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The count and the triggers land in one transaction, so no write falls between them.
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS catalog_counts (name TEXT PRIMARY KEY, value INTEGER NOT NULL)",
		"CREATE TRIGGER IF NOT EXISTS catalog_counts_messages_insert AFTER INSERT ON messages BEGIN UPDATE catalog_counts SET value=value+1 WHERE name='messages'; END",
		"CREATE TRIGGER IF NOT EXISTS catalog_counts_messages_delete AFTER DELETE ON messages BEGIN UPDATE catalog_counts SET value=value-1 WHERE name='messages'; END",
		"INSERT OR REPLACE INTO catalog_counts(name,value) SELECT 'messages',COUNT(*) FROM messages",
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PricingHealth reports how much usage has a known price.
func (c *Catalog) PricingHealth(ctx context.Context) (map[string]any, error) {
	return cachedValue(ctx, c, "health:pricing", func(context.Context) (map[string]any, error) {
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
	usage, _ := cachedValue(ctx, c, "health:token-usage", func(context.Context) ([]map[string]any, error) { return c.tokenUsageByModel(), nil })
	health := map[string]any{"freshness": c.Freshness(), "workspaces": int64(0), "messages": int64(0),
		"index_bytes": c.indexBytes(), "token_usage": usage}
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
