package archive

import (
	"context"
	"time"
)

// usageSummaryWindows are the header summary's trailing windows, newest
// first; a zero span is all time.
var usageSummaryWindows = []struct {
	key  string
	span time.Duration
}{
	{"1h", time.Hour}, {"6h", 6 * time.Hour}, {"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}, {"all", 0},
}

// usageSummaryWindow is one window's topline totals.
type usageSummaryWindow struct {
	Key          string                        `json:"key"`
	HumanWords   int64                         `json:"human_words"`
	Messages     int64                         `json:"messages"`
	Tokens       int64                         `json:"tokens"`
	CostUSD      any                           `json:"cost_usd"`
	CarbonTokens map[string]map[string]float64 `json:"carbon_tokens"`
}

// UsageSummary answers GET /api/usage/summary: words typed by a person, the
// messages carrying them, tokens, API-equivalent cost, and carbon inputs by
// model tier and token category over trailing windows. Human text comes from
// the authorship ledger, which already keeps only each mirror group's
// representative work; usage metrics come from the same de-mirrored ledger as
// the Usage page. That ledger is hourly, so a window's starting hour counts in
// proportion to its overlap.
func (c *Catalog) UsageSummary(ctx context.Context) (map[string]any, error) {
	now := c.clock()
	windows := make([]*usageSummaryWindow, len(usageSummaryWindows))
	cutoffs := make([]time.Time, len(usageSummaryWindows))
	for index, window := range usageSummaryWindows {
		windows[index] = &usageSummaryWindow{Key: window.key}
		if window.span > 0 {
			cutoffs[index] = now.Add(-window.span)
		}
	}

	c.ensureAuthorship()
	rows, err := c.DB.QueryContext(ctx, "SELECT sent_at,typed_words FROM message_authorship WHERE typed_words>0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sentAt string
		var words int64
		if err := rows.Scan(&sentAt, &words); err != nil {
			return nil, err
		}
		sent, ok := parseTime(sentAt)
		for index, window := range windows {
			if cutoffs[index].IsZero() || ok && !sent.Before(cutoffs[index]) {
				window.HumanWords += words
				window.Messages++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ledger, err := c.representativeLedger()
	if err != nil {
		return nil, err
	}
	book, err := c.loadPriceBook()
	if err != nil {
		return nil, err
	}
	carbon, err := loadCarbonDocument()
	if err != nil {
		return nil, err
	}
	for _, entry := range ledger {
		hour, ok := parseTime(firstString(entry["usage_hour"]))
		day := ""
		if ok {
			day = localMidnight(hour, time.Local).Format("2006-01-02")
		}
		tokens := ledgerTokens(entry)
		priced := book.cost(ledgerModel(entry), day, tokens)
		tier, _ := carbon.tier(ledgerModel(entry))
		carbonParts := carbonTokens(entry)
		for index, window := range windows {
			share := 1.0
			if !cutoffs[index].IsZero() {
				if !ok {
					continue
				}
				share = min(max(hour.Add(time.Hour).Sub(cutoffs[index]).Hours(), 0), 1)
			}
			if share == 0 {
				continue
			}
			window.Tokens += int64(float64(tokens["total_tokens"])*share + .5)
			if window.CarbonTokens == nil {
				window.CarbonTokens = map[string]map[string]float64{}
			}
			if window.CarbonTokens[tier] == nil {
				window.CarbonTokens[tier] = map[string]float64{}
			}
			for category, count := range carbonParts {
				window.CarbonTokens[tier][category] += float64(count) * share
			}
			if priced.cost != nil {
				cost := *priced.cost * share
				window.CostUSD = addCost(window.CostUSD, &cost)
			}
		}
	}
	return map[string]any{"generated_at": now.UTC().Format(time.RFC3339), "windows": windows, "carbon_factors": carbon}, nil
}
