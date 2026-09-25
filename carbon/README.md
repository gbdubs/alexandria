# CO₂ factors

`co2_factors.json` holds the factors Pharos uses to estimate the electricity
and CO₂e behind token usage. The app embeds this file; Settings → Carbon
footprint and `GET /api/health/carbon` apply it to the same de-mirrored usage
ledger as the Usage page.

The result is **an estimate, not a measurement**. Anthropic publishes no
per-token or per-query energy, and OpenAI has published only one per-query
average, so every number here comes from comparable measured systems. Training,
embodied hardware emissions, water, and networking are excluded.

## Formula

For each model tier and token type:

```text
Wh  = tokens ÷ 1,000,000 × tier Wh per 1M output tokens × token-type ratio
CO₂e g = Wh × PUE × grid g CO₂e/kWh ÷ 1,000
```

- **Tiers** (`tiers`) give server energy per million output tokens before
  data-center overhead. A model takes the first tier, in file order, with a
  `match` fragment contained in its normalized name (`modelFamily`: no
  `claude-` prefix, snapshot date, or `[1m]`). Models that match nothing take
  `default_tier`; the card lists them.
- **Ratios** (`ratios`) give energy per token relative to an output token for
  `uncached_input`, `cache_write`, and `cache_read`. Output is 1. Reasoning
  tokens are part of output; unclassified tokens are charged as uncached input.
- **PUE** is facility energy ÷ server energy.
- **Grids** are the choices offered on the card; `default_grid` is used for
  the API's `co2e_g`.
- **Equivalents** turn grams into everyday comparisons.

Every factor has a `low`, `central`, and `high` value (grids and equivalents
have one). The card's estimate switch uses all lows, all centrals, or all
highs together, so the low–high range is deliberately wide: in agentic coding
most tokens are cache reads, and the cache-read ratio is the least certain
factor. The grid and PUE can be changed on the card and are remembered per
browser.

## File contract

- Every tier, ratio, PUE, grid, and equivalent cites one or more `sources` by
  `id`; each source needs an `https://` URL, a title, and the `finding` it
  supports.
- Ranges must satisfy `0 < low ≤ central ≤ high`; PUE cannot be below 1.
- `retrieved_at` is the date the sources were last checked.

`go test ./internal/archive -run TestCarbon` validates the file. After editing
it, rebuild with `./launch.sh` so the app embeds the new copy.

## Updating

Search for newer disclosures before changing a factor: provider sustainability
reports and per-query figures, the ML.ENERGY leaderboard, papers measuring
prefill, decode, and KV-cache energy, cloud PUE reports, EPA eGRID, and Ember's
Global Electricity Review. Hardware efficiency improves quickly, so revisit the
tier values at least yearly.
