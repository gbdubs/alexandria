package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// The CO₂ calculator turns reconciled token categories into an estimate of
// inference energy and emissions. It is an estimate, not a measurement:
// providers do not publish per-token energy for their models, so each model
// is placed in a size tier whose energy per output token comes from published
// measurements of comparable models, and input, cache-write, and cache-read
// tokens are charged as fractions of that. carbon/co2_factors.json holds every
// factor with its source; the Settings card applies data-center overhead (PUE)
// and a grid's carbon intensity, which the reader can change.

// carbonCategories are the token categories the calculator prices, in the
// order the Settings card lists them. Output includes reasoning.
var carbonCategories = []string{"uncached_input", "cache_write", "cache_read", "output"}

// carbonScenarios pair the low, central, and high value of every factor.
var carbonScenarios = []string{"low", "central", "high"}

type carbonRange struct {
	Low     float64 `json:"low"`
	Central float64 `json:"central"`
	High    float64 `json:"high"`
}

func (r carbonRange) get(scenario string) float64 {
	switch scenario {
	case "low":
		return r.Low
	case "high":
		return r.High
	}
	return r.Central
}

type carbonTier struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Match []string `json:"match"`
	// OutputWhPerMTok is IT (server) energy per million output tokens,
	// before data-center overhead.
	OutputWhPerMTok carbonRange `json:"output_wh_per_mtok"`
	Basis           string      `json:"basis"`
	Sources         []string    `json:"sources"`
}

type carbonRatio struct {
	carbonRange
	Basis   string   `json:"basis"`
	Sources []string `json:"sources"`
}

type carbonGrid struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	GramsPerKWh float64  `json:"g_co2e_per_kwh"`
	Basis       string   `json:"basis"`
	Sources     []string `json:"sources"`
}

type carbonEquivalent struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	GramsPerUnit float64  `json:"g_co2e_per_unit"`
	Sources      []string `json:"sources"`
}

type carbonSource struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Published string `json:"published"`
	Finding   string `json:"finding"`
}

type carbonDocument struct {
	Version     int    `json:"version"`
	RetrievedAt string `json:"retrieved_at"`
	// Tiers are matched in order against the model name; the first tier
	// with a matching substring wins, and DefaultTier takes the rest.
	Tiers       []carbonTier `json:"tiers"`
	DefaultTier string       `json:"default_tier"`
	// CategoryRatios are energy per token relative to an output token, keyed
	// by every category in carbonCategories except output.
	CategoryRatios map[string]carbonRatio `json:"ratios"`
	PUE            carbonRatio            `json:"pue"`
	Grids          []carbonGrid           `json:"grids"`
	DefaultGrid    string                 `json:"default_grid"`
	Equivalents    []carbonEquivalent     `json:"equivalents"`
	Caveats        []string               `json:"caveats"`
	Sources        []carbonSource         `json:"sources"`
}

func carbonDocumentBytes() ([]byte, error) {
	if value, err := assets.ReadFile("assets/carbon/co2_factors.json"); err == nil {
		return value, nil
	}
	// `macos/build-app.sh` copies the canonical file into embedded assets; these
	// fallbacks keep `go test` and `go run` working from the source tree.
	for _, candidate := range []string{
		filepath.Join("carbon", "co2_factors.json"),
		filepath.Join("..", "..", "carbon", "co2_factors.json"),
	} {
		if value, err := os.ReadFile(candidate); err == nil {
			return value, nil
		}
	}
	return nil, fmt.Errorf("carbon factors file not found: carbon/co2_factors.json")
}

func parseCarbon(data []byte) (carbonDocument, []string) {
	var doc carbonDocument
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return doc, []string{fmt.Sprintf("carbon json: %v", err)}
	}
	return doc, validateCarbon(doc)
}

// validateCarbon checks that every factor is ordered low ≤ central ≤ high,
// positive, and cites a listed https source.
func validateCarbon(doc carbonDocument) []string {
	problems := []string{}
	if doc.Version != 1 {
		problems = append(problems, "version must be 1")
	}
	if _, err := time.Parse("2006-01-02", doc.RetrievedAt); err != nil {
		problems = append(problems, "retrieved_at must be YYYY-MM-DD")
	}
	sources := map[string]bool{}
	for index, source := range doc.Sources {
		if source.ID == "" || source.Title == "" || !strings.HasPrefix(source.URL, "https://") || source.Finding == "" {
			problems = append(problems, fmt.Sprintf("sources[%d] %s: id, title, https url, and finding are required", index, source.ID))
		}
		if sources[source.ID] {
			problems = append(problems, fmt.Sprintf("sources[%d] %s: duplicate id", index, source.ID))
		}
		sources[source.ID] = true
	}
	cited := func(label string, ids []string) {
		if len(ids) == 0 {
			problems = append(problems, label+": cite at least one source")
		}
		for _, id := range ids {
			if !sources[id] {
				problems = append(problems, fmt.Sprintf("%s: unknown source %q", label, id))
			}
		}
	}
	ordered := func(label string, value carbonRange) {
		if !(value.Low > 0 && value.Low <= value.Central && value.Central <= value.High) {
			problems = append(problems, label+": need 0 < low ≤ central ≤ high")
		}
	}
	tiers := map[string]bool{}
	for index, tier := range doc.Tiers {
		label := fmt.Sprintf("tiers[%d] %s", index, tier.Key)
		if tier.Key == "" || tier.Label == "" || tier.Basis == "" {
			problems = append(problems, label+": key, label, and basis are required")
		}
		tiers[tier.Key] = true
		ordered(label, tier.OutputWhPerMTok)
		cited(label, tier.Sources)
	}
	if !tiers[doc.DefaultTier] {
		problems = append(problems, "default_tier must name a tier")
	}
	for _, category := range carbonCategories[:len(carbonCategories)-1] {
		ratio, ok := doc.CategoryRatios[category]
		if !ok {
			problems = append(problems, "ratios."+category+" is required")
			continue
		}
		ordered("ratios."+category, ratio.carbonRange)
		cited("ratios."+category, ratio.Sources)
	}
	for category := range doc.CategoryRatios {
		if category == "output" || !slices.Contains(carbonCategories, category) {
			problems = append(problems, "ratios."+category+": not a token category (output is 1 by definition)")
		}
	}
	ordered("pue", doc.PUE.carbonRange)
	if doc.PUE.Low < 1 {
		problems = append(problems, "pue: cannot be below 1")
	}
	cited("pue", doc.PUE.Sources)
	grids := map[string]bool{}
	for index, grid := range doc.Grids {
		label := fmt.Sprintf("grids[%d] %s", index, grid.Key)
		if grid.Key == "" || grid.Label == "" || grid.GramsPerKWh <= 0 {
			problems = append(problems, label+": key, label, and a positive g_co2e_per_kwh are required")
		}
		grids[grid.Key] = true
		cited(label, grid.Sources)
	}
	if !grids[doc.DefaultGrid] {
		problems = append(problems, "default_grid must name a grid")
	}
	for index, equivalent := range doc.Equivalents {
		label := fmt.Sprintf("equivalents[%d] %s", index, equivalent.Key)
		if equivalent.Key == "" || equivalent.Label == "" || equivalent.GramsPerUnit <= 0 {
			problems = append(problems, label+": key, label, and a positive g_co2e_per_unit are required")
		}
		cited(label, equivalent.Sources)
	}
	return problems
}

func loadCarbonDocument() (carbonDocument, error) {
	data, err := carbonDocumentBytes()
	if err != nil {
		return carbonDocument{}, err
	}
	doc, problems := parseCarbon(data)
	if len(problems) > 0 {
		return doc, fmt.Errorf("carbon/co2_factors.json: %s", strings.Join(problems, "; "))
	}
	return doc, nil
}

// tier places a model in a size tier by the first matching name fragment.
// Unrecognized and placeholder models take the default tier.
func (doc carbonDocument) tier(model string) (string, bool) {
	family := modelFamily(model)
	for _, tier := range doc.Tiers {
		for _, fragment := range tier.Match {
			if fragment != "" && strings.Contains(family, strings.ToLower(fragment)) {
				return tier.Key, true
			}
		}
	}
	return doc.DefaultTier, false
}

// carbonTokens splits a ledger row into the calculator's categories.
// Unclassified tokens are charged as uncached input, the costlier input kind.
func carbonTokens(row map[string]any) map[string]int64 {
	return map[string]int64{
		"uncached_input": integer(row["uncached_input_tokens"]) + integer(row["unclassified_tokens"]),
		"cache_write":    integer(row["cache_creation_input_tokens"]),
		"cache_read":     integer(row["cache_read_input_tokens"]),
		"output":         integer(row["output_tokens"]),
	}
}

type carbonWindow struct {
	Key string `json:"key"`
	// Tokens by tier, then category.
	Tokens map[string]map[string]int64 `json:"tokens"`
	// EnergyWh is IT energy before PUE, by scenario, then category, plus
	// "total".
	EnergyWh map[string]map[string]float64 `json:"energy_wh"`
	// CO2eGrams applies each scenario's PUE and the default grid.
	CO2eGrams map[string]float64 `json:"co2e_g"`
}

// energy fills the window's IT energy from its token counts.
func (doc carbonDocument) energy(window *carbonWindow) {
	perTier := map[string]carbonRange{}
	for _, tier := range doc.Tiers {
		perTier[tier.Key] = tier.OutputWhPerMTok
	}
	grid := 0.0
	for _, candidate := range doc.Grids {
		if candidate.Key == doc.DefaultGrid {
			grid = candidate.GramsPerKWh
		}
	}
	window.EnergyWh, window.CO2eGrams = map[string]map[string]float64{}, map[string]float64{}
	for _, scenario := range carbonScenarios {
		energy := map[string]float64{"total": 0}
		for _, category := range carbonCategories {
			ratio := 1.0
			if category != "output" {
				ratio = doc.CategoryRatios[category].get(scenario)
			}
			sum := 0.0
			for tier, tokens := range window.Tokens {
				sum += float64(tokens[category]) / 1e6 * perTier[tier].get(scenario) * ratio
			}
			energy[category] = sum
			energy["total"] += sum
		}
		window.EnergyWh[scenario] = energy
		window.CO2eGrams[scenario] = energy["total"] * doc.PUE.get(scenario) * grid / 1000
	}
}

// carbonHealth answers GET /api/health/carbon: token totals by tier and
// category for all time and the last 30 days, their estimated energy and
// emissions, the factors behind them, and which models took the default tier.
func (c *Catalog) carbonHealth() (map[string]any, error) {
	doc, err := loadCarbonDocument()
	if err != nil {
		return nil, err
	}
	rows, err := c.localUsageRows(context.Background())
	if err != nil {
		return nil, err
	}
	since := c.clock().AddDate(0, 0, -29).Format("2006-01-02")
	all := &carbonWindow{Key: "all", Tokens: map[string]map[string]int64{}}
	recent := &carbonWindow{Key: "30d", Tokens: map[string]map[string]int64{}}
	unmatched := map[string]int64{}
	for _, row := range rows {
		model := firstString(row["model"])
		tier, matched := doc.tier(model)
		tokens := carbonTokens(row)
		windows := []*carbonWindow{all}
		if firstString(row["day"]) >= since {
			windows = append(windows, recent)
		}
		for _, window := range windows {
			if window.Tokens[tier] == nil {
				window.Tokens[tier] = map[string]int64{}
			}
			for category, count := range tokens {
				window.Tokens[tier][category] += count
			}
		}
		if !matched && !placeholderModel(model) {
			unmatched[model] += integer(row["total_tokens"])
		}
	}
	doc.energy(all)
	doc.energy(recent)
	models := []map[string]any{}
	for model, tokens := range unmatched {
		models = append(models, map[string]any{"model": model, "tokens": tokens})
	}
	sort.Slice(models, func(i, j int) bool { return integer(models[i]["tokens"]) > integer(models[j]["tokens"]) })
	return map[string]any{"factors": doc, "windows": []*carbonWindow{all, recent}, "default_tier_models": models,
		"generated_at": c.clock().UTC().Format(time.RFC3339)}, nil
}

// CarbonHealth is the cached Settings section for the CO₂ calculator.
func (c *Catalog) CarbonHealth(ctx context.Context) (map[string]any, error) {
	return cachedValue(ctx, c, "health:carbon", func(context.Context) (map[string]any, error) {
		return c.carbonHealth()
	})
}
