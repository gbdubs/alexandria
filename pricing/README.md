# API price history

`cost_changes.json` holds the API list prices Pharos uses to turn token
usage into API-equivalent cost. The app embeds this file and imports it into the
`cost_changes` and `model_aliases` tables whenever its contents change. The
`cost_on_date` and `model_alias_on_date` views give each row the half-open date
interval `[effective_from, effective_to)` it applies to.

Cost is **not a bill**. It prices each token category at the standard rate in
effect on the day of use. It ignores subscriptions, batch/flex/priority tiers,
and long-context premiums, so it is a lower bound.

## File contract

```json
{
  "version": 1,
  "currency": "USD",
  "unit": "per_million_tokens",
  "changes": [
    { "provider": "anthropic", "model": "claude-opus-4-8", "effective_from": "2026-05-28",
      "input": 5, "cache_read": 0.5, "cache_write_5m": 6.25, "cache_write_1h": 10, "output": 25,
      "status": "proposed", "source_url": "https://…", "source_title": "…", "notes": "…", "retrieved_at": "2026-09-24" }
  ],
  "aliases": [
    { "alias": "opus", "provider": "anthropic", "model": "claude-opus-4-8", "effective_from": "2026-05-28",
      "status": "proposed", "source_url": "https://…", "source_title": "…", "notes": "…" }
  ]
}
```

- **Rates** are USD per million tokens. Use `null` when the provider has no rate for a category (for example, OpenAI cache writes). Input and output are required.
- **`effective_from`** is the date the price took effect. A price applies until the next row for the same model; an alias applies until the next row for the same alias.
- **Model matching** ignores a `claude-` prefix, snapshot dates (`-20251001`), and `-1m`/`[1m]` context variants, so a single row covers `claude-opus-4-8`, `opus-4-8`, and `opus-4-8-1m`.
- **Aliases** map names such as Claude Code's bare `opus` to the concrete model they meant on a given date.
- **`status`**: `proposed` rows are imported but never used for cost. A person flips a row to `confirmed` after checking its source. `rejected` keeps a checked-and-wrong finding from being proposed again.
- Every row must cite an `https://` source that states the price or mapping.
- **`assumption: true`** marks a price no provider published, such as a Codex-only model priced like its closest API sibling, or pre-launch usage priced at the launch price. `notes` must explain the basis. These rows count toward cost, but that cost is shown as `≈$…` with status `assumed`, so you can filter it out.

Validate the file and see which of your models still lack a confirmed price:

```sh
pharos --config ~/Library/Application\ Support/AI\ Work\ Archive/archive.toml pricing check [FILE]
```

Cost cells show `—` for unpriced usage, `≈` for assumed prices, and a trailing `+` when part of the usage had no rate. Settings → Health shows the share of usage that is priced.

## Refreshing prices

Run a research agent by hand when prices are out of date. On the Usage page,
**Copy refresh prices prompt** copies a prompt built from your catalog: which
models are unpriced and when they were used, the newest price per provider,
which models are priced by assumption, and the rules above. Paste it into a new
agent session in this repository. The button is highlighted, with a count, when
usage includes an unpriced model that you haven't copied a prompt for yet.
Placeholders such as `<synthetic>` are ignored. The same prompt is available from
the command line:

```sh
pharos --config ~/Library/Application\ Support/AI\ Work\ Archive/archive.toml pricing prompt
```

The agent adds only `proposed` rows. Review each row's source, flip the correct
ones to `confirmed` (or `rejected`), and rebuild with `./launch.sh` so the app
embeds the updated file.
