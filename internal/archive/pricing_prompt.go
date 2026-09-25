package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// pricingStatus backs the Usage page's "Copy refresh prices prompt" button:
// coverage, the models that need research, and a ready-to-paste prompt.
func (c *Catalog) pricingStatus() map[string]any {
	health := c.pricingHealth()
	health["prompt"] = c.pricingRefreshPrompt(health)
	return health
}

// pricingRepository finds the source checkout that owns pricing/cost_changes.json
// by walking up from the running executable (dist/*.app/Contents/MacOS/...) and
// the working directory. It returns "" when Pharos runs outside a checkout.
func pricingRepository() string {
	starts := []string{}
	if executable, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(executable))
	}
	if directory, err := os.Getwd(); err == nil {
		starts = append(starts, directory)
	}
	for _, start := range starts {
		for directory := start; ; directory = filepath.Dir(directory) {
			if _, err := os.Stat(filepath.Join(directory, "pricing", "cost_changes.json")); err == nil {
				if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
					return directory
				}
			}
			if parent := filepath.Dir(directory); parent == directory {
				break
			}
		}
	}
	return ""
}

func (c *Catalog) pricingRefreshPrompt(health map[string]any) string {
	today := time.Now().Format("2006-01-02")
	latest, _ := queryMaps(c.DB, `SELECT provider,MAX(effective_from) effective_from,MAX(retrieved_at) retrieved_at,COUNT(*) rows
		FROM cost_changes WHERE status<>'rejected' GROUP BY provider ORDER BY provider`)
	repository := pricingRepository()
	location := "the Pharos repository (https://github.com/gbdubs/alexandria)"
	if repository != "" {
		location = "the Pharos repository at `" + repository + "`"
	}
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Refresh the API price history that Pharos uses to compute API-equivalent cost. Today is %s.\n\n", today)
	fmt.Fprintf(&prompt, "Work in %s. Edit only `pricing/cost_changes.json`. Read `pricing/README.md` first: it defines the file contract, and the rules below restate the parts that matter most.\n\n", location)

	prompt.WriteString("## What to research\n\n")
	unpriced, _ := health["unpriced_models"].([]map[string]any)
	if len(unpriced) == 0 {
		prompt.WriteString("1. No reported model is currently unpriced.\n")
	} else {
		prompt.WriteString("1. **Unpriced models.** These model strings from my usage have no confirmed price on the days they were used. For each one, find the official API price in effect on those days. If the string is a bare alias (for example `opus`) or a Claude Code or Codex name, find which API model it meant on those dates and add an alias row instead of a price row.\n\n")
		prompt.WriteString("   | Reported model | Client | Tokens | First used | Last used |\n   | --- | --- | --- | --- | --- |\n")
		for _, model := range unpriced {
			fmt.Fprintf(&prompt, "   | `%s` | %s | %s | %s | %s |\n", model["model"], model["provider"], compactTokens(integer(model["tokens"])),
				defaultString(model["first_day"], "?"), defaultString(model["last_day"], "?"))
		}
		prompt.WriteString("\n")
	}
	prompt.WriteString("2. **Price changes and new models since the last refresh.** Check each provider's official pricing page, model launch announcements, and the Claude Code changelog (for alias changes). Record every change after the newest `effective_from` below.\n\n")
	if len(latest) > 0 {
		prompt.WriteString("   | Provider | Newest effective_from | Last retrieved | Rows |\n   | --- | --- | --- | --- |\n")
		for _, row := range latest {
			fmt.Fprintf(&prompt, "   | %s | %s | %s | %d |\n", row["provider"], defaultString(row["effective_from"], "?"), defaultString(row["retrieved_at"], "?"), integer(row["rows"]))
		}
		prompt.WriteString("\n")
	}
	if assumed, _ := health["assumed_models"].([]map[string]any); len(assumed) > 0 {
		names := make([]string, 0, len(assumed))
		for _, model := range assumed {
			names = append(names, "`"+firstString(model["model"])+"`")
		}
		sort.Strings(names)
		verb := "are"
		if len(names) == 1 {
			verb = "is"
		}
		fmt.Fprintf(&prompt, "3. **Assumed prices.** %s %s priced by assumption (`\"assumption\": true` rows). Check whether an official price now exists. If one does, add a real row for the date it took effect; leave the assumption row in place for earlier dates.\n\n", strings.Join(names, ", "), verb)
	}

	prompt.WriteString(`## Rules for editing pricing/cost_changes.json

- Rates are USD per million tokens: ` + "`input`" + ` (uncached), ` + "`cache_read`, `cache_write_5m`, `cache_write_1h`, `output`" + `. Use ` + "`null`" + ` when the provider has no such rate. OpenAI's single cache-write rate goes in ` + "`cache_write_5m`" + `. Input and output are required.
- Record only standard-tier rates. Mention long-context, batch, flex, or priority prices in ` + "`notes`" + `; they are not used.
- ` + "`effective_from`" + ` (YYYY-MM-DD) is the day the price took effect, usually the launch or API-availability date. A price applies until the next row for the same model; an alias applies until the next row for the same alias.
- Use canonical API model IDs without date suffixes (` + "`claude-opus-5-5`, `gpt-6-sol`" + `). Matching ignores a ` + "`claude-`" + ` prefix, ` + "`-YYYYMMDD`" + ` dates, and ` + "`-1m`/`[1m]`" + ` suffixes, so don't add rows that differ only by those.
- Every row needs an ` + "`https://`" + ` ` + "`source_url`" + ` that you actually fetched and that states the value, plus ` + "`source_title`" + ` and ` + "`retrieved_at`" + ` (today). Prefer official pages; for past prices use web.archive.org snapshots. Never fill in a value from memory. If you can't cite it, leave it out and tell me.
- Add every new row with ` + "`\"status\": \"proposed\"`" + `. I review proposed rows and confirm them myself. Never edit or delete ` + "`confirmed`" + ` or ` + "`rejected`" + ` rows.
- Only when a model genuinely has no published API price (for example, a Codex-only or pre-launch model), you may propose a row with ` + "`\"assumption\": true`" + ` priced from its closest published sibling. Its ` + "`notes`" + ` must name the sibling and explain why it's a reasonable basis.
- Ignore placeholder model strings such as ` + "`<synthetic>`" + `; they aren't API calls.

## Check your work

`)
	if repository != "" {
		fmt.Fprintf(&prompt, "Run `cd %s && go test ./internal/archive -run 'Pricing|Price|Cost'` and fix any problems it reports. ", repository)
	} else {
		prompt.WriteString("Run `go test ./internal/archive -run 'Pricing|Price|Cost'` from the repository root and fix any problems it reports. ")
	}
	prompt.WriteString("`go run ./cmd/alexandria pricing check` also validates the file and lists models that are still unpriced.\n\n")
	prompt.WriteString("Finish with a table of every row you added (model or alias, effective_from, rates, source URL), plus anything you couldn't source and why. Don't commit. I'll review, confirm rows, and rebuild with `./launch.sh` so the app picks up the new prices.\n")
	return prompt.String()
}

func compactTokens(tokens int64) string {
	switch {
	case tokens >= 1_000_000_000_000:
		return fmt.Sprintf("%.1fT", float64(tokens)/1e12)
	case tokens >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(tokens)/1e9)
	case tokens >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(tokens)/1e6)
	case tokens >= 1_000:
		return fmt.Sprintf("%.1fK", float64(tokens)/1e3)
	}
	return fmt.Sprint(tokens)
}
