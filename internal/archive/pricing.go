package archive

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Pricing turns reconciled token categories into API-equivalent list-price
// cost. It is not a bill: subscriptions, batch/priority tiers, and long-context
// premiums are not modeled, so cost is a lower bound at standard rates.

type priceChange struct {
	Provider      string   `json:"provider"`
	Model         string   `json:"model"`
	EffectiveFrom string   `json:"effective_from"`
	Input         *float64 `json:"input"`
	CacheRead     *float64 `json:"cache_read"`
	CacheWrite5m  *float64 `json:"cache_write_5m"`
	CacheWrite1h  *float64 `json:"cache_write_1h"`
	Output        *float64 `json:"output"`
	Status        string   `json:"status"`
	SourceURL     string   `json:"source_url"`
	SourceTitle   string   `json:"source_title,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	RetrievedAt   string   `json:"retrieved_at,omitempty"`
	// Assumption marks a price that no provider published, such as a
	// Codex-only model priced like its API sibling. Notes must say why.
	Assumption bool `json:"assumption,omitempty"`
}

type modelAlias struct {
	Alias         string `json:"alias"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	EffectiveFrom string `json:"effective_from"`
	Status        string `json:"status"`
	SourceURL     string `json:"source_url"`
	SourceTitle   string `json:"source_title,omitempty"`
	Notes         string `json:"notes,omitempty"`
}

type pricingDocument struct {
	Version  int           `json:"version"`
	Currency string        `json:"currency"`
	Unit     string        `json:"unit"`
	Changes  []priceChange `json:"changes"`
	Aliases  []modelAlias  `json:"aliases"`
}

func (p priceChange) id() string {
	return p.Provider + "/" + modelFamily(p.Model) + "/" + p.EffectiveFrom + "/" + p.Status
}

func (a modelAlias) id() string {
	return a.Provider + "/" + modelFamily(a.Alias) + "/" + a.EffectiveFrom + "/" + a.Status
}

func pricingDocumentBytes() ([]byte, error) {
	if value, err := assets.ReadFile("assets/pricing/cost_changes.json"); err == nil {
		return value, nil
	}
	// `macos/build-app.sh` copies the canonical file into embedded assets; these
	// fallbacks keep `go test` and `go run` working from the source tree.
	for _, candidate := range []string{
		filepath.Join("pricing", "cost_changes.json"),
		filepath.Join("..", "..", "pricing", "cost_changes.json"),
	} {
		if value, err := os.ReadFile(candidate); err == nil {
			return value, nil
		}
	}
	return nil, fmt.Errorf("pricing file not found: pricing/cost_changes.json")
}

func parsePricing(data []byte) (pricingDocument, []string) {
	var doc pricingDocument
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return doc, []string{fmt.Sprintf("pricing json: %v", err)}
	}
	return doc, validatePricing(doc)
}

// validatePricing enforces the contract the research agent must satisfy: every
// row is cited, dated, non-negative, and unambiguous among confirmed rows.
func validatePricing(doc pricingDocument) []string {
	problems := []string{}
	if doc.Version != 1 || doc.Currency != "USD" || doc.Unit != "per_million_tokens" {
		problems = append(problems, `header must be version 1, currency "USD", unit "per_million_tokens"`)
	}
	date := func(value string) bool { _, err := time.Parse("2006-01-02", value); return err == nil }
	status := func(value string) bool { return value == "proposed" || value == "confirmed" || value == "rejected" }
	cited := func(value string) bool { return strings.HasPrefix(value, "https://") }
	seen := map[string]bool{}
	for index, change := range doc.Changes {
		label := fmt.Sprintf("changes[%d] %s %s", index, change.Model, change.EffectiveFrom)
		if change.Provider == "" || modelFamily(change.Model) == "" || !date(change.EffectiveFrom) || !status(change.Status) || !cited(change.SourceURL) {
			problems = append(problems, label+": provider, model, effective_from (YYYY-MM-DD), status, and https source_url are required")
		}
		rates := 0
		for _, rate := range []*float64{change.Input, change.CacheRead, change.CacheWrite5m, change.CacheWrite1h, change.Output} {
			if rate != nil {
				rates++
				if *rate < 0 {
					problems = append(problems, label+": rates must be non-negative")
				}
			}
		}
		if change.Input == nil || change.Output == nil || rates == 0 {
			problems = append(problems, label+": input and output rates are required")
		}
		if change.Assumption && strings.TrimSpace(change.Notes) == "" {
			problems = append(problems, label+": assumption rows must explain their basis in notes")
		}
		if seen[change.id()] {
			problems = append(problems, label+": duplicate model/effective_from/status")
		}
		seen[change.id()] = true
		if confirmed := "confirmed:" + modelFamily(change.Model) + "/" + change.EffectiveFrom; change.Status == "confirmed" {
			if seen[confirmed] {
				problems = append(problems, label+": two confirmed prices for the same model and date")
			}
			seen[confirmed] = true
		}
	}
	for index, alias := range doc.Aliases {
		label := fmt.Sprintf("aliases[%d] %s %s", index, alias.Alias, alias.EffectiveFrom)
		if alias.Provider == "" || modelFamily(alias.Alias) == "" || modelFamily(alias.Model) == "" || !date(alias.EffectiveFrom) || !status(alias.Status) || !cited(alias.SourceURL) {
			problems = append(problems, label+": alias, provider, model, effective_from (YYYY-MM-DD), status, and https source_url are required")
		}
		if modelFamily(alias.Alias) == modelFamily(alias.Model) {
			problems = append(problems, label+": alias resolves to itself")
		}
		if seen["alias:"+alias.id()] {
			problems = append(problems, label+": duplicate alias/effective_from/status")
		}
		seen["alias:"+alias.id()] = true
	}
	return problems
}

// syncPricing replaces the catalog's price tables with the embedded pricing
// file whenever the file changes. An invalid file leaves the previous prices.
func (c *Catalog) syncPricing() error {
	data, err := pricingDocumentBytes()
	if err != nil {
		return nil
	}
	digest := hashBytes(data)
	var current string
	if err := c.DB.QueryRow("SELECT value FROM meta WHERE key='pricing_digest'").Scan(&current); err == nil && current == digest {
		return nil
	} else if err != nil && err != sql.ErrNoRows {
		return err
	}
	doc, problems := parsePricing(data)
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "pricing file ignored: %s\n", strings.Join(problems, "; "))
		return nil
	}
	return c.replacePricing(doc, digest)
}

func (c *Catalog) replacePricing(doc pricingDocument, digest string) error {
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM cost_changes; DELETE FROM model_aliases"); err != nil {
		return err
	}
	for _, change := range doc.Changes {
		if _, err := tx.Exec(`INSERT INTO cost_changes(id,provider,model,model_key,effective_from,input_per_mtok,cache_read_per_mtok,
			cache_write_5m_per_mtok,cache_write_1h_per_mtok,output_per_mtok,status,assumption,source_url,source_title,notes,retrieved_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, change.id(), change.Provider, change.Model, modelFamily(change.Model), change.EffectiveFrom,
			change.Input, change.CacheRead, change.CacheWrite5m, change.CacheWrite1h, change.Output, change.Status, change.Assumption, change.SourceURL,
			nilIfEmpty(change.SourceTitle), nilIfEmpty(change.Notes), nilIfEmpty(change.RetrievedAt)); err != nil {
			return err
		}
	}
	for _, alias := range doc.Aliases {
		if _, err := tx.Exec(`INSERT INTO model_aliases(id,alias,alias_key,provider,model,model_key,effective_from,status,source_url,source_title,notes)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, alias.id(), alias.Alias, modelFamily(alias.Alias), alias.Provider, alias.Model, modelFamily(alias.Model),
			alias.EffectiveFrom, alias.Status, alias.SourceURL, nilIfEmpty(alias.SourceTitle), nilIfEmpty(alias.Notes)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES('pricing_digest',?)", digest); err != nil {
		return err
	}
	return tx.Commit()
}

type priceInterval struct {
	from, to, model string
	rates           [5]*float64 // input, cache read, cache write 5m, cache write 1h, output
	assumption      bool
}

type aliasInterval struct{ from, to, modelKey string }

// priceBook is the in-memory form of the cost_on_date and model_alias_on_date
// views, loaded once per request.
type priceBook struct {
	prices  map[string][]priceInterval
	aliases map[string][]aliasInterval
	today   string
}

func (c *Catalog) loadPriceBook() (priceBook, error) {
	book := priceBook{prices: map[string][]priceInterval{}, aliases: map[string][]aliasInterval{}, today: c.clock().Format("2006-01-02")}
	prices, err := queryMaps(c.DB, `SELECT * FROM cost_on_date`)
	if err != nil {
		return book, err
	}
	rate := func(value any) *float64 {
		if n, ok := number(value); ok {
			return &n
		}
		return nil
	}
	for _, row := range prices {
		key := firstString(row["model_key"])
		book.prices[key] = append(book.prices[key], priceInterval{
			from: firstString(row["effective_from"]), to: firstString(row["effective_to"]), model: firstString(row["model"]),
			rates: [5]*float64{rate(row["input_per_mtok"]), rate(row["cache_read_per_mtok"]), rate(row["cache_write_5m_per_mtok"]),
				rate(row["cache_write_1h_per_mtok"]), rate(row["output_per_mtok"])},
			assumption: integer(row["assumption"]) != 0,
		})
	}
	aliases, err := queryMaps(c.DB, `SELECT * FROM model_alias_on_date`)
	if err != nil {
		return book, err
	}
	for _, row := range aliases {
		key := firstString(row["alias_key"])
		book.aliases[key] = append(book.aliases[key], aliasInterval{from: firstString(row["effective_from"]), to: firstString(row["effective_to"]), modelKey: firstString(row["model_key"])})
	}
	return book, nil
}

func (b priceBook) resolve(model, day string) string {
	key := modelFamily(model)
	for hops := 0; hops < 4; hops++ {
		next := ""
		for _, alias := range b.aliases[key] {
			if day >= alias.from && (alias.to == "" || day < alias.to) {
				next = alias.modelKey
			}
		}
		if next == "" || next == key {
			break
		}
		key = next
	}
	return key
}

func (b priceBook) price(key, day string) *priceInterval {
	for index := range b.prices[key] {
		interval := &b.prices[key][index]
		if day >= interval.from && (interval.to == "" || day < interval.to) {
			return interval
		}
	}
	return nil
}

type usageCost struct {
	cost, costToday *float64
	status          string // priced, assumed, partial, or unpriced
	pricedModel     string
}

// cost prices one ledger entry. tokens holds the ledger's token categories.
// Reasoning is already part of output. A cache write without a TTL split is
// charged at the 5-minute rate. Categories whose rate is missing, and
// unclassified tokens, make the result "partial" rather than guessed.
func (b priceBook) cost(model, day string, tokens map[string]int64) usageCost {
	result := usageCost{status: "unpriced"}
	if day == "" {
		return result
	}
	key := b.resolve(model, day)
	oneHour := tokens["cache_creation_1h_input_tokens"]
	quantities := [5]int64{
		tokens["uncached_input_tokens"], tokens["cache_read_input_tokens"],
		max(tokens["cache_creation_input_tokens"]-oneHour, 0), oneHour, tokens["output_tokens"],
	}
	price := func(interval *priceInterval) (*float64, bool) {
		total, complete := 0.0, tokens["unclassified_tokens"] == 0
		for index, quantity := range quantities {
			if quantity == 0 {
				continue
			}
			if interval.rates[index] == nil {
				complete = false
				continue
			}
			total += float64(quantity) * *interval.rates[index] / 1_000_000
		}
		return &total, complete
	}
	if today := b.price(key, b.today); today != nil {
		result.costToday, _ = price(today)
	}
	at := b.price(key, day)
	if at == nil {
		return result
	}
	result.pricedModel = at.model
	cost, complete := price(at)
	result.cost = cost
	result.status = "priced"
	if at.assumption {
		result.status = "assumed"
	}
	if !complete {
		result.status = "partial"
	}
	return result
}

// worsePriceStatus keeps the least complete status when entries are combined.
func worsePriceStatus(left, right string) string {
	rank := map[string]int{"": 0, "priced": 1, "assumed": 2, "partial": 3, "unpriced": 4}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func addCost(total any, value *float64) any {
	if value == nil {
		return total
	}
	if current, ok := total.(float64); ok {
		return current + *value
	}
	return *value
}

// placeholderModel reports model strings that are not real models, such as
// Claude Code's "<synthetic>" messages or usage with no model recorded. They
// stay unpriced but are never sent to the pricing agent.
func placeholderModel(model string) bool {
	return model == "Unknown model" || (strings.HasPrefix(model, "<") && strings.HasSuffix(model, ">"))
}

// modelsWithStatus lists reported model strings whose usage has the given
// price status, largest first, with the dates they were used, so the pricing
// agent knows what to research and for which period.
func modelsWithStatus(rows []map[string]any, status string) []map[string]any {
	type usage struct {
		provider, first, last string
		tokens                int64
	}
	totals := map[string]*usage{}
	for _, row := range rows {
		model, day := firstString(row["model"]), firstString(row["day"])
		if firstString(row["price_status"]) != status || placeholderModel(model) {
			continue
		}
		if totals[model] == nil {
			totals[model] = &usage{provider: firstString(row["provider"])}
		}
		entry := totals[model]
		entry.tokens += integer(row["total_tokens"])
		if day != "" && (entry.first == "" || day < entry.first) {
			entry.first = day
		}
		if day > entry.last {
			entry.last = day
		}
	}
	result := []map[string]any{}
	for model, entry := range totals {
		result = append(result, map[string]any{"model": model, "provider": entry.provider, "tokens": entry.tokens, "first_day": entry.first, "last_day": entry.last})
	}
	sort.Slice(result, func(i, j int) bool { return integer(result[i]["tokens"]) > integer(result[j]["tokens"]) })
	return result
}

// pricingHealth reports how much usage has a confirmed price, so gaps in the
// price history are visible instead of silently lowering cost.
func (c *Catalog) pricingHealth() map[string]any {
	health := map[string]any{"priced_tokens": int64(0), "assumed_tokens": int64(0), "partial_tokens": int64(0), "unpriced_tokens": int64(0),
		"unpriced_models": []map[string]any{}, "assumed_models": []map[string]any{}}
	for _, status := range []string{"confirmed", "proposed"} {
		var changes int64
		_ = c.DB.QueryRow("SELECT COUNT(*) FROM cost_changes WHERE status=?", status).Scan(&changes)
		health[status+"_changes"] = changes
	}
	rows, err := c.usageRows(time.Local)
	if err != nil {
		health["error"] = err.Error()
		return health
	}
	// Totals for the Settings usage card, overall and for the last 30 days.
	since := c.clock().AddDate(0, 0, -29).Format("2006-01-02")
	var tokens, recentTokens int64
	var cost, recentCost any
	for _, row := range rows {
		key := firstString(row["price_status"]) + "_tokens"
		if _, ok := health[key]; ok {
			health[key] = integer(health[key]) + integer(row["total_tokens"])
		}
		value, priced := row["cost_usd"].(float64)
		tokens += integer(row["total_tokens"])
		if priced {
			cost = addCost(cost, &value)
		}
		if firstString(row["day"]) >= since {
			recentTokens += integer(row["total_tokens"])
			if priced {
				recentCost = addCost(recentCost, &value)
			}
		}
	}
	health["total_tokens"], health["cost_usd"], health["recent_tokens"], health["recent_cost_usd"] = tokens, cost, recentTokens, recentCost
	health["unpriced_models"] = modelsWithStatus(rows, "unpriced")
	health["assumed_models"] = modelsWithStatus(rows, "assumed")
	return health
}

// migratePricingSchema runs before schema.sql: it adds columns that the
// pricing views select, and drops the views so schema.sql recreates them.
func (c *Catalog) migratePricingSchema() error {
	var exists int
	if err := c.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cost_changes'").Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		has, err := c.hasColumn("cost_changes", "assumption")
		if err != nil {
			return err
		}
		if !has {
			if _, err := c.DB.Exec("ALTER TABLE cost_changes ADD COLUMN assumption INTEGER NOT NULL DEFAULT 0"); err != nil {
				return err
			}
		}
	}
	_, err := c.DB.Exec("DROP VIEW IF EXISTS cost_on_date; DROP VIEW IF EXISTS model_alias_on_date")
	return err
}
