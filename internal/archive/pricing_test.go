package archive

import (
	"math"
	"strings"
	"testing"
	"time"
)

func rate(value float64) *float64 { return &value }

func testPricing() pricingDocument {
	return pricingDocument{Version: 1, Currency: "USD", Unit: "per_million_tokens",
		Changes: []priceChange{
			{Provider: "anthropic", Model: "claude-opus-4-8", EffectiveFrom: "2026-01-01", Input: rate(10), CacheRead: rate(1), CacheWrite5m: rate(12.5), CacheWrite1h: rate(20), Output: rate(50), Status: "confirmed", SourceURL: "https://example.com/a"},
			{Provider: "anthropic", Model: "claude-opus-4-8", EffectiveFrom: "2026-06-01", Input: rate(5), CacheRead: rate(0.5), CacheWrite5m: rate(6.25), CacheWrite1h: rate(10), Output: rate(25), Status: "confirmed", SourceURL: "https://example.com/b"},
			{Provider: "anthropic", Model: "claude-opus-5", EffectiveFrom: "2026-08-01", Input: rate(99), Output: rate(99), Status: "proposed", SourceURL: "https://example.com/c"},
			{Provider: "openai", Model: "gpt-5.5", EffectiveFrom: "2026-05-01", Input: rate(2), CacheRead: rate(0.2), Output: rate(16), Status: "confirmed", SourceURL: "https://example.com/d"},
		},
		Aliases: []modelAlias{
			{Alias: "opus", Provider: "anthropic", Model: "claude-opus-4-8", EffectiveFrom: "2026-01-01", Status: "confirmed", SourceURL: "https://example.com/e"},
			{Alias: "opus", Provider: "anthropic", Model: "claude-opus-5", EffectiveFrom: "2026-08-01", Status: "confirmed", SourceURL: "https://example.com/f"},
		},
	}
}

func near(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-9 }

func TestPriceBookUsesPriceOnDateAliasesAndTodaysPrice(t *testing.T) {
	catalog, _ := testCatalog(t)
	if err := catalog.replacePricing(testPricing(), "test"); err != nil {
		t.Fatal(err)
	}
	interval, err := queryMaps(catalog.DB, `SELECT effective_from,effective_to FROM cost_on_date WHERE model_key='opus-4-8' ORDER BY effective_from`)
	if err != nil || len(interval) != 2 || interval[0]["effective_to"] != "2026-06-01" || interval[1]["effective_to"] != nil {
		t.Fatalf("cost_on_date intervals: %#v %v", interval, err)
	}
	book, err := catalog.loadPriceBook()
	if err != nil {
		t.Fatal(err)
	}
	book.today = "2026-09-24"
	tokens := map[string]int64{"uncached_input_tokens": 1_000_000, "cache_read_input_tokens": 2_000_000,
		"cache_creation_input_tokens": 3_000_000, "cache_creation_1h_input_tokens": 1_000_000, "output_tokens": 100_000}
	// 1M uncached x10 + 2M reads x1 + 2M 5m-writes x12.5 + 1M 1h-writes x20 + 0.1M output x50.
	march := book.cost("opus-4-8-1m", "2026-03-10", tokens)
	if march.status != "priced" || !near(march.cost, 10+2+25+20+5) || !near(march.costToday, 5+1+12.5+10+2.5) || march.pricedModel != "claude-opus-4-8" {
		t.Fatalf("price before the cut: %+v cost=%v today=%v", march, deref(march.cost), deref(march.costToday))
	}
	if july := book.cost("opus", "2026-07-10", tokens); !near(july.cost, 5+1+12.5+10+2.5) {
		t.Fatalf("bare alias should resolve to the model current on that day: %+v", july)
	}
	// In August the alias points to a model whose only price is still proposed.
	if august := book.cost("opus", "2026-08-10", tokens); august.status != "unpriced" || august.cost != nil {
		t.Fatalf("proposed prices must not be used: %+v", august)
	}
	if early := book.cost("claude-opus-4-8", "2025-12-01", tokens); early.status != "unpriced" || early.cost != nil || !near(early.costToday, 5+1+12.5+10+2.5) {
		t.Fatalf("usage before the first known price: %+v", early)
	}
	// OpenAI has no cache-write rate, so a stray cache write makes cost a lower bound.
	openai := book.cost("gpt-5.5", "2026-06-01", map[string]int64{"uncached_input_tokens": 1_000_000, "cache_read_input_tokens": 1_000_000, "output_tokens": 1_000_000, "cache_creation_input_tokens": 10})
	if openai.status != "partial" || !near(openai.cost, 2+0.2+16) {
		t.Fatalf("missing rate should be partial: %+v", openai)
	}
	if unknown := book.cost("<synthetic>", "2026-06-01", tokens); unknown.status != "unpriced" {
		t.Fatalf("unknown model: %+v", unknown)
	}
}

func deref(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func TestPricingValidationRejectsUncitedAndConflictingRows(t *testing.T) {
	doc := testPricing()
	doc.Changes = append(doc.Changes,
		priceChange{Provider: "anthropic", Model: "claude-opus-4-8", EffectiveFrom: "2026-06-01", Input: rate(1), Output: rate(1), Status: "confirmed", SourceURL: "https://example.com/dup"},
		priceChange{Provider: "openai", Model: "gpt-x", EffectiveFrom: "June 2026", Input: rate(-1), Output: rate(1), Status: "maybe", SourceURL: "http://insecure"},
	)
	doc.Aliases = append(doc.Aliases, modelAlias{Alias: "claude-opus-4-8", Provider: "anthropic", Model: "opus-4-8-1m", EffectiveFrom: "2026-01-01", Status: "confirmed", SourceURL: "https://example.com"})
	problems := strings.Join(validatePricing(doc), "\n")
	for _, want := range []string{"duplicate model/effective_from/status", "effective_from (YYYY-MM-DD)", "non-negative", "resolves to itself"} {
		if !strings.Contains(problems, want) {
			t.Errorf("missing %q in:\n%s", want, problems)
		}
	}
	if problems := validatePricing(testPricing()); len(problems) != 0 {
		t.Fatalf("valid fixture rejected: %v", problems)
	}
	if _, problems := parsePricing([]byte(`{"version":1,"currency":"USD","unit":"per_million_tokens","changes":[{"model":"x","price":1}]}`)); len(problems) == 0 {
		t.Fatal("unknown fields should be rejected")
	}
}

func TestUsageAndLibraryRowsCarryCost(t *testing.T) {
	catalog, _ := testCatalog(t)
	doc := testPricing()
	doc.Changes = append(doc.Changes, priceChange{Provider: "openai", Model: "gpt-6-sol", EffectiveFrom: "2026-01-01", Input: rate(10), CacheRead: rate(1), Output: rate(100), Status: "confirmed", SourceURL: "https://example.com/g"})
	if err := catalog.replacePricing(doc, "test"); err != nil {
		t.Fatal(err)
	}
	ingestUsageFixture(t, catalog) // Codex fixture with no reported model.
	if _, err := catalog.DB.Exec(`UPDATE agent_sessions SET model='gpt-6-sol'`); err != nil {
		t.Fatal(err)
	}
	rows, err := catalog.usageRows(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, row := range rows {
		if row["price_status"] != "priced" || row["priced_model"] != "gpt-6-sol" {
			t.Fatalf("usage row not priced: %#v", row)
		}
		total += row["cost_usd"].(float64)
	}
	// Uncached 700 x $10, cache reads 2,300 x $1, output 50 x $100, per million.
	if want := (700*10 + 2300*1 + 50*100) / 1e6; math.Abs(total-want) > 1e-12 {
		t.Fatalf("usage cost %v, want %v", total, want)
	}
	library, err := catalog.searchRows(SearchOptions{}, nil)
	if err != nil || len(library) != 1 {
		t.Fatalf("%v %v", library, err)
	}
	if cost, ok := library[0]["cost_usd"].(float64); !ok || math.Abs(cost-total) > 1e-12 || library[0]["price_status"] != "priced" {
		t.Fatalf("library cost %#v, want %v", library[0]["cost_usd"], total)
	}
	health := catalog.pricingHealth()
	if integer(health["priced_tokens"]) != 3050 || integer(health["unpriced_tokens"]) != 0 || integer(health["confirmed_changes"]) != 4 {
		t.Fatalf("pricing health: %#v", health)
	}
}

func TestPricingFileIsValid(t *testing.T) {
	data, err := pricingDocumentBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, problems := parsePricing(data); len(problems) != 0 {
		t.Fatalf("pricing/cost_changes.json:\n%s", strings.Join(problems, "\n"))
	}
}

func TestAssumedPricesAndRefreshPrompt(t *testing.T) {
	catalog, _ := testCatalog(t)
	doc := testPricing()
	doc.Changes = append(doc.Changes, priceChange{Provider: "openai", Model: "gpt-6-sol", EffectiveFrom: "2026-09-15", Input: rate(2), CacheRead: rate(0.2), Output: rate(10),
		Status: "confirmed", Assumption: true, Notes: "pre-launch usage at the launch price", SourceURL: "https://example.com/sol"})
	if err := catalog.replacePricing(doc, "test"); err != nil {
		t.Fatal(err)
	}
	ingestUsageFixture(t, catalog) // 2026-09-20 and 2026-09-21: only the 21st is covered by the assumption.
	if _, err := catalog.DB.Exec(`UPDATE agent_sessions SET model='gpt-6-sol'`); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec(`UPDATE agent_session_usage SET model='gpt-6-sol' WHERE usage_hour<'2026-09-21'`); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec(`UPDATE agent_session_usage SET model='gpt-7-preview' WHERE usage_hour>='2026-09-21'`); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec(`INSERT INTO agent_session_usage(agent_session_id,usage_hour,model,attribution,input_tokens,output_tokens,total_tokens)
		SELECT agent_session_id,usage_hour,'<synthetic>',attribution,10,0,10 FROM agent_session_usage LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	rows, err := catalog.usageRows(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, row := range rows {
		statuses[firstString(row["model"])] = firstString(row["price_status"])
	}
	if statuses["gpt-6-sol"] != "assumed" || statuses["gpt-7-preview"] != "unpriced" || statuses["<synthetic>"] != "unpriced" {
		t.Fatalf("statuses: %v", statuses)
	}
	book, _ := catalog.loadPriceBook()
	if assumed := book.cost("gpt-6-sol", "2026-09-16", map[string]int64{"output_tokens": 1_000_000}); assumed.status != "assumed" || !near(assumed.cost, 10) {
		t.Fatalf("assumption rows must price as assumed: %+v", assumed)
	}
	status := catalog.pricingStatus()
	for _, model := range status["unpriced_models"].([]map[string]any) {
		if placeholderModel(firstString(model["model"])) {
			t.Fatalf("placeholder model sent to the pricing agent: %v", model)
		}
	}
	prompt := status["prompt"].(string)
	for _, want := range []string{"| `gpt-7-preview` | codex |", "\"status\": \"proposed\"", "\"assumption\": true", "pricing/cost_changes.json", "Never fill in a value from memory"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("refresh prompt is missing %q:\n%s", want, prompt)
		}
	}
	doc.Changes[len(doc.Changes)-1].Notes = ""
	if problems := strings.Join(validatePricing(doc), "\n"); !strings.Contains(problems, "assumption rows must explain") {
		t.Fatalf("assumption without notes accepted: %s", problems)
	}
}

func TestPricingSchemaMigratesCatalogsWithoutAssumption(t *testing.T) {
	catalog, _ := testCatalog(t)
	for _, statement := range []string{
		"DROP VIEW cost_on_date", "DROP TABLE cost_changes",
		`CREATE TABLE cost_changes (id TEXT PRIMARY KEY, provider TEXT NOT NULL, model TEXT NOT NULL, model_key TEXT NOT NULL,
			effective_from TEXT NOT NULL, input_per_mtok REAL, cache_read_per_mtok REAL, cache_write_5m_per_mtok REAL,
			cache_write_1h_per_mtok REAL, output_per_mtok REAL, status TEXT NOT NULL, source_url TEXT NOT NULL,
			source_title TEXT, notes TEXT, retrieved_at TEXT)`,
	} {
		if _, err := catalog.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	if err := catalog.replacePricing(testPricing(), "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.loadPriceBook(); err != nil {
		t.Fatalf("migrated view unusable: %v", err)
	}
}
