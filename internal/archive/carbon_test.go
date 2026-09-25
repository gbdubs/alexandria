package archive

import (
	"math"
	"strings"
	"testing"
	"time"
)

// The shipped factors file must satisfy its own contract.
func TestCarbonFactorsFileIsValid(t *testing.T) {
	data, err := carbonDocumentBytes()
	if err != nil {
		t.Fatal(err)
	}
	doc, problems := parseCarbon(data)
	if len(problems) > 0 {
		t.Fatalf("carbon/co2_factors.json:\n%s", strings.Join(problems, "\n"))
	}
	for model, want := range map[string]string{
		"claude-haiku-4-5-20251001": "small", "haiku": "small", "gpt-5.3-codex-spark": "small", "gpt-6-luna": "small",
		"claude-sonnet-5": "medium", "gpt-5.3-codex": "medium", "gpt-6-sol": "medium",
		"claude-opus-5-5[1m]": "large", "opus": "large", "claude-fable-5-1": "large", "gpt-6-astra": "large", "gpt-5-pro": "large",
		"gpt-5-mini": "small", "gemini-2.5-pro": "large",
	} {
		if got, _ := doc.tier(model); got != want {
			t.Errorf("tier(%q) = %q, want %q", model, got, want)
		}
	}
	if tier, matched := doc.tier("mystery-model"); matched || tier != doc.DefaultTier {
		t.Errorf("unrecognized model: %q %v", tier, matched)
	}
}

func TestCarbonValidationRejectsUncitedAndDisorderedFactors(t *testing.T) {
	doc := testCarbon()
	doc.Tiers[0].OutputWhPerMTok = carbonRange{Low: 3, Central: 2, High: 4}
	doc.Tiers[0].Sources = []string{"missing"}
	doc.PUE.Low = 0.9
	problems := strings.Join(validateCarbon(doc), "\n")
	for _, want := range []string{"tiers[0] small: need 0 < low ≤ central ≤ high", `unknown source "missing"`, "pue: cannot be below 1"} {
		if !strings.Contains(problems, want) {
			t.Errorf("missing %q in:\n%s", want, problems)
		}
	}
}

// Energy is tokens ÷ 1M × the tier's Wh per 1M output tokens × the category
// ratio; emissions multiply by PUE and the default grid.
func TestCarbonEnergyArithmetic(t *testing.T) {
	doc := testCarbon()
	window := &carbonWindow{Tokens: map[string]map[string]int64{
		"small": {"uncached_input": 2_000_000, "cache_read": 10_000_000, "output": 1_000_000},
		"large": {"cache_write": 1_000_000, "output": 500_000},
	}}
	doc.energy(window)
	// small: 2×100×0.1 + 10×100×0.01 + 1×100 = 20 + 10 + 100 = 130 Wh
	// large: 1×1000×0.2 + 0.5×1000 = 200 + 500 = 700 Wh
	central := window.EnergyWh["central"]
	for key, want := range map[string]float64{"uncached_input": 20, "cache_read": 10, "cache_write": 200, "output": 600, "total": 830} {
		if math.Abs(central[key]-want) > 1e-9 {
			t.Errorf("central %s = %v, want %v", key, central[key], want)
		}
	}
	// 830 Wh × PUE 1.2 × 400 g/kWh = 398.4 g
	if got := window.CO2eGrams["central"]; math.Abs(got-398.4) > 1e-9 {
		t.Errorf("central CO2e = %v, want 398.4", got)
	}
	if !(window.CO2eGrams["low"] < window.CO2eGrams["central"] && window.CO2eGrams["central"] < window.CO2eGrams["high"]) {
		t.Errorf("scenarios out of order: %v", window.CO2eGrams)
	}
}

func TestCarbonHealthSplitsWindowsAndCategories(t *testing.T) {
	catalog, _ := testCatalog(t)
	// Codex usage: 3000 input of which 2300 cached, 50 output, no model.
	ingestUsageFixture(t, catalog)
	catalog.now = func() time.Time { return time.Date(2026, 9, 21, 2, 15, 0, 0, time.UTC) }
	health, err := catalog.carbonHealth()
	if err != nil {
		t.Fatal(err)
	}
	windows := health["windows"].([]*carbonWindow)
	if len(windows) != 2 || windows[0].Key != "all" || windows[1].Key != "30d" {
		t.Fatalf("windows: %#v", windows)
	}
	doc := health["factors"].(carbonDocument)
	tokens := windows[0].Tokens[doc.DefaultTier]
	if tokens["uncached_input"] != 700 || tokens["cache_read"] != 2300 || tokens["cache_write"] != 0 || tokens["output"] != 50 {
		t.Fatalf("tokens: %#v", windows[0].Tokens)
	}
	if windows[1].Tokens[doc.DefaultTier]["output"] != 50 {
		t.Fatalf("last 30 days: %#v", windows[1].Tokens)
	}
	if windows[0].CO2eGrams["central"] <= 0 {
		t.Fatalf("no emissions: %#v", windows[0])
	}
	// Usage with no model is a placeholder, not an unrecognized model.
	if models := health["default_tier_models"].([]map[string]any); len(models) != 0 {
		t.Fatalf("default-tier models: %#v", models)
	}
	catalog.now = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }
	health, err = catalog.carbonHealth()
	if err != nil {
		t.Fatal(err)
	}
	if recent := health["windows"].([]*carbonWindow)[1]; len(recent.Tokens) != 0 || recent.EnergyWh["central"]["total"] != 0 {
		t.Fatalf("usage older than 30 days counted as recent: %#v", recent)
	}
}

func testCarbon() carbonDocument {
	cite := []string{"s"}
	ratio := func(low, central, high float64) carbonRatio {
		return carbonRatio{carbonRange: carbonRange{low, central, high}, Basis: "test", Sources: cite}
	}
	return carbonDocument{
		Version: 1, RetrievedAt: "2026-09-25", DefaultTier: "medium",
		Tiers: []carbonTier{
			{Key: "small", Label: "Small", Match: []string{"haiku"}, OutputWhPerMTok: carbonRange{50, 100, 200}, Basis: "test", Sources: cite},
			{Key: "medium", Label: "Medium", Match: []string{"sonnet"}, OutputWhPerMTok: carbonRange{200, 400, 800}, Basis: "test", Sources: cite},
			{Key: "large", Label: "Large", Match: []string{"opus"}, OutputWhPerMTok: carbonRange{500, 1000, 2000}, Basis: "test", Sources: cite},
		},
		CategoryRatios: map[string]carbonRatio{
			"uncached_input": ratio(0.05, 0.1, 0.2), "cache_write": ratio(0.1, 0.2, 0.3), "cache_read": ratio(0.005, 0.01, 0.02),
		},
		PUE:         ratio(1.1, 1.2, 1.4),
		Grids:       []carbonGrid{{Key: "test", Label: "Test grid", GramsPerKWh: 400, Basis: "test", Sources: cite}},
		DefaultGrid: "test",
		Sources:     []carbonSource{{ID: "s", Title: "Source", URL: "https://example.com", Finding: "test"}},
	}
}
