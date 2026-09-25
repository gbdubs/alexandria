# Pharos

Pharos is a local-first macOS library for finding past work across TL1, Conductor, Codex, Claude, and supported desktop exports. A second capability, preserving and reclaiming **TL1-owned** workspaces through a custody-aware owner hook, is mothballed; see [`docs/reclamation/README.md`](docs/reclamation/README.md) for what remains and how to resume it.

The primary implementation is Go 1.26 with SQLite FTS, a local concept index, an authenticated loopback API, a read-only MCP server, and a responsive desktop UI. The Go service and UI are embedded in the macOS `.app`; the earlier Python implementation remains in `src/` as a compatibility reference while the destructive TL1 workflow is deliberately held back.

Pharos was previously named Alexandria (and before that, AI Work Archive). Only the visible name changed: the CLI executable is still `alexandria`, configuration still lives in `AI Work Archive`, and internal identifiers keep their earlier names so existing configs, saved views, and preferences carry over.

## Safety defaults

- Every configured source is read-only. No home-directory scan occurs: `alexandria probe` and the new-Mac panel check only a fixed list of known agent locations, and nothing is indexed until you opt in.
- No source is reclaimed. TL1 reclamation is mothballed and excluded from the default build.
- The archive never calls `rm -rf`, `git worktree remove`, `git worktree prune`, or `git gc`. TL1 owns the worker handshake, lock/lease, atomic recheck, and exact-resource removal.
- Preservation must fit the 100,000,000-byte logical cap, reside on the pinned volume, and pass a complete hash/reopen check before acknowledgement.
- Heavy work is deferred unless CPU, memory, and authoritative TL1/agent activity signals all say the machine is truly idle. Unknown owner activity means “not idle.”

## Quick start

```sh
./launch.sh
```

This one command builds the Go service and Swift wrapper, initializes
`~/Library/Application Support/AI Work Archive/archive.toml` if it is missing,
builds the macOS app, and launches it. Existing configuration is preserved.

Edit that configuration to set `archive_root`, paste the value reported by
`dist/Pharos.app/Contents/MacOS/alexandria volume-id /Volumes/euclid`, and set `enabled = true`
only on source paths you want indexed. Then ingest from the workspace when you
want to refresh the library:

```sh
dist/Pharos.app/Contents/MacOS/alexandria --config "$HOME/Library/Application Support/AI Work Archive/archive.toml" ingest
```

To keep the app and its whole library on an external drive that moves between
Macs instead, run `macos/install-library.sh /Volumes/euclid/Pharos`. The app
then uses the `library.toml` beside it, whose paths are relative to its own directory;
see [Portable library](docs/configuration.md#portable-library). On each Mac the
drive is plugged into, double-click **Add This Mac.command** beside the app: it
finds that Mac's Claude Code, Codex, Conductor, and TL1 history, captures it
onto the drive, and indexes it ([Adding a Mac](docs/configuration.md#adding-a-mac)).
To bring an existing per-user install along without re-indexing, add `--adopt`
with its `archive.toml`; see [Moving an existing install onto a drive](docs/configuration.md#moving-an-existing-install-onto-a-drive).

The UI has separate Library, Usage, Tools, MCP, and Settings areas. Library, Usage, Tools, Activity, and MCP call history
use the shared query-table pattern: schema-driven columns, field discovery,
filtering (including OR/NOT), multi-sort, paging, persistent saved views,
optional aggregate metrics, column reorder, and resize.

- **Library** searches content, repositories, source types, and actual changed-file inventory. **Sub-agents** (delegated sessions, including nested ones), **Sub-agent depth** (deepest nesting below the workspace's own session), and **Compactions** (context compactions across the workspace's sessions) are filterable, sortable, and groupable number fields. Work detail shows evidence-linked summaries, attempted/checkpointed/integrated changes, conversations, metrics, PR associations, and receipts.

Past Work also shows when an archived commit appears on the local `origin/main` history. It links the first mainline commit containing that work, whether it arrived through a merge commit or directly. This uses local Git objects only; squash merges and work without a retained commit ID cannot be attributed by ancestry. Refresh the local `origin/main` ref and sync a source to update these associations.
- **TL1** appears once a TL1 source is synced. It compares each flavor across agent configurations (advance, escalation, and agent error rates, cost per useful result, time, tokens, cache reuse), clusters errors by normalized signature and who can fix them, traces human attention and review findings to their causes, segments work by TL1's large enqueues (defaulting to the latest, so analysis follows the most recent flavor revisions), and ranks concerns and savings opportunities, each with a copyable investigation prompt. Flavor and candidate drill-downs and a per-run query table sit underneath. See [`docs/tl1-analysis.md`](docs/tl1-analysis.md).
- **Usage** has two views, switched at the top of the page, and remembers the last one. **Tokens** is the token-usage query table: one row per agent session, local day, and model, with uncached input, cache reads, cache writes, output, and reasoning kept separate. Provider, model, repository, source, agent kind, and day/week/month are all filter and group-by fields, so metrics such as "cache reads per week by model" are a saved view. A stacked chart above the table shows tokens or cost per day, week, or month over the last 30 days to all time, split by token type, provider, model, repository, or agent kind, and follows the table's filters. Preset buttons set up common breakdowns. Linked mirrors of the same work are counted once. **Your writing** is described below.
- **Tools** analyzes tool use from retained transcripts. A daily summary table groups calls by tool, shell program and subcommand (`git status`, `go test`, `sed`), model, repository, and agent kind, with error counts by kind, durations, the tokens each result added to the context, the tokens re-read by later requests until compaction, and their API-equivalent cost. A per-call table filters and sorts every call; a summary row drills into its calls, and each call opens with its parsed commands, input, and result. Each call also records the sites it reached: URLs from web fetches, browser navigation, Codex web searches, and network commands such as `curl`, plus any web search query and the links it returned. Presets cover error rates, failure kinds, top commands, shell calls a dedicated tool could make, slow tools, test and build time, context cost, sites reached, and web searches. Definitions are in [`docs/tool-analytics.md`](docs/tool-analytics.md).
- **MCP** controls local agent access, provides a copyable stdio connection definition and setup prompt, and offers a query-table of recent tool calls with filters, saved views, metrics, response-size estimates, duration, result counts, truncation, and errors.
- **Sources** lists every configured source, path availability, coverage, last attempted and successful sync, and errors. It provides persistent enable/pause controls, **Sync one** for safe trials even while paused, and **Sync all enabled** for global refreshes.
- **Library drive** (in the header) names the drive holding the library and what is holding or writing the library (sync, index, capture, backup, Git lookups, Library view refreshes), with progress. Its panel says how to disconnect the drive: always **Eject**, which in the app releases the library first and then ejects the drive, or says why not; in a plain browser it says how to eject in Finder. See [Library status and Eject](docs/configuration.md#library-status-and-eject).
- **Macs and captures** (in Settings → Sources) lists this Mac and every Mac whose captures are in the library, with when each source was captured and indexed and which need indexing, plus **Capture** and **Index** for one Mac or all. A newly added Mac is captured right after onboarding, then offered **Index now**.
- **Library drive** checks and **Backups** (in Settings → Health) show the drive's encryption, Spotlight, file system, free space, volume pin and backup age, each with what to do and a copyable command, and back the library up to a folder on another drive.
- **Activity** shows live source-sync progress, indexed workspace/conversation/message counts, failures, and per-source results from the current app session.
- **Your writing** (in Usage) estimates how much text you typed or dictated into agent chats. Each user message is split into typed text, harness instructions, one-click templates, attachments, agent output copied from the previous 48 hours, re-sent text, likely pastes, and prompts sent by scripts or other agents. A query table lists one row per conversation, sortable and filterable by each kind of input and by tokens and cost, so "deepest conversations" is one click; the chart and breakdown above it follow the filters. The transcript reader shows the split for each message, and Settings has cards that open each Usage view. Definitions are in [`docs/human-authorship.md`](docs/human-authorship.md).
- **Health** shows retrieval coverage/freshness, index size, and external-storage health. Retrieval-only is shown as an intentional capability.

Delegated work is retained as a recursive `agent_sessions` tree, including sub-agents of sub-agents and links back to each session's messages. Session and workspace usage preserve uncached input, cache reads, cache creation, output, reasoning, and any unclassified aggregate remainder separately so pricing can be applied without reconstructing provider envelopes.

Run diagnostics with `dist/Pharos.app/Contents/MacOS/alexandria --config "$HOME/Library/Application Support/AI Work Archive/archive.toml" doctor`. Configuration is documented in [`docs/configuration.md`](docs/configuration.md).

## Sources and identity

| Kind | Acquisition | Mutation capability |
| --- | --- | --- |
| `tl1` | Registered installations via consistent SQLite snapshots and native transcripts | None |
| `tl1-export` | Owner-produced canonical JSON | TL1 release contract only |
| `conductor` | Consistent read-only SQLite snapshot transaction and schema inspection | None |
| `codex` | Native rollout/session JSONL | None |
| `claude` | Native project session JSONL | None |
| `chatgpt-export` | User-provided `conversations.json` | None |
| `canonical` | Documented interchange JSON | None |

Deduplication uses account-scoped native IDs. Conductor aliases are retained as evidence-backed identity links after all adapters run; repeated identical prompts are never merged by content hash. The Conductor extractor inspects each row and selects the materially populated `content`, `full_message`, or `text` field, recording that decision in its evidence locator.

No claim is made that ChatGPT desktop’s local cache is complete. ChatGPT is indexed from a user-provided supported export, and its health row makes that acquisition boundary visible.

## Search, API, and MCP

Exact file-change searches use `change_files`, distinct from conversation mentions. Repository, source, PR, task, and metrics queries use structured data. Natural-language results combine FTS with a deterministic on-device concept/feature embedding; no archive content leaves the machine. Extractive summaries cite retained source locators. Inference can be replaced or extended later without becoming a reclamation prerequisite.

The HTTP service binds only to loopback and requires either a bearer token or its HttpOnly UI cookie. Core endpoints are:

```text
GET  /api/search?q=&repository=&source=&file=&pr=&limit=&offset=
GET  /api/work/{workspace_id}
GET  /api/conversation/{conversation_id}?limit=&offset=
GET  /api/change/{change_set_id}
GET  /api/trace?file=&pr=
GET  /api/receipt/{receipt_or_operation_id}
GET  /api/health
GET  /api/sources
GET  /api/activity
GET  /api/library/status
GET  /api/health/drive
GET  /api/mcp
GET  /api/mcp/calls?limit=&offset=&tool=&status=
POST /api/mcp/enabled
POST /api/sources/{name}/sync
POST /api/sources/{name}/enabled
POST /api/sources/sync
POST /api/query/{library|activity|usage|…}
GET  /api/query/{library|activity|usage|…}/distinct?field=&q=&limit=
GET  /api/query/{library|activity|usage|…}/field-stats?fields=a,b
POST /api/query/{library|activity|usage|…}/aggregations
```

The query endpoints implement the `@pythia-software/query-table-*` 0.4.2 wire
contract. Their allowlisted field definitions live in [`schemas`](schemas), and
the same documents drive the React frontend and Go executor. Pharos uses a
map-backed Go adapter because the upstream compiler currently emits PostgreSQL;
the catalog remains SQLite and query values never become SQL text.

Source controls only change ingestion participation or perform an explicit
read-only refresh. They never delete source data.

`alexandria mcp` exposes read-only conversation discovery in four layers:
`search_conversations` returns compact ranked cards; `get_conversation_overview`
returns an extractive preview with message references;
`search_conversation_passages` finds matching passages within a conversation;
and `get_conversation_messages` reads bounded windows or chunks of a long
message. The discovery tools accept `max_output_tokens` as an approximate
response budget (defaulting to 700–1,200 depending on the tool). Search cards
include coverage and index freshness; conversation documents are generated
locally and existing catalogs are backfilled on opening.

The existing `search_work`, `get_work_detail`, `get_conversation_excerpt`,
`get_change_set`, `trace`, `query_metrics`, and `get_receipt` tools remain
available. `get_conversation_excerpt` is a compatibility alias for bounded
message windows. MCP exposes no arbitrary deletion tool.

The MCP page controls a shared enabled flag in the local catalog. Turning it off
hides tools from new listings and rejects calls from already-connected clients;
it does not alter another client's configuration. Tool-call history retains the
latest 5,000 calls with an allowlisted, shortened argument summary and response
metrics, never response bodies. The page provides per-tool totals and filters
for tool and status. Output token counts are estimates from bytes.

## Preservation, TL1 release, and scheduling (mothballed)

Reclamation is mothballed. The Upcoming tab, protect/snooze controls, and the
`upcoming`/`protect` commands are excluded from the default build, and the Go
service refuses `preserve`, `reclaim`, `reconcile`, `tick`, and `worker`. The
preservation package format, release state machine, and idle scheduler survive
in the Python reference implementation and the owner contract
([`docs/tl1-contract.md`](docs/tl1-contract.md)).
[`docs/reclamation/README.md`](docs/reclamation/README.md) lists every parked
piece and the steps to resume. The launchd templates under
[`macos/launchd`](macos/launchd) belong to that parked scheduler.

## Native macOS wrapper

The quick-start launcher handles initialization, the absolute CLI path required
by Finder, building, and opening the app:

```sh
./launch.sh
```

Use `./launch.sh --no-open` to initialize and build without opening the app.

The SwiftUI wrapper starts the loopback-only Go service embedded beside it and presents its authenticated interface in WebKit. The catalog, configuration, staging data, and preserved archive remain outside the app bundle.

### Code signing

`macos/build-app.sh` (and therefore `launch.sh`) signs what it builds: first the
embedded service (`local.ai-work-archive.service`), then the bundle
(`local.ai-work-archive`), both with the hardened runtime. The build fails if
`codesign --verify --strict --deep` rejects the result.

| Variable | Effect |
| --- | --- |
| `PHAROS_CODESIGN_IDENTITY` | Keychain signing identity (name or SHA-1). Unset: ad-hoc signing. |
| `PHAROS_HARDENED_RUNTIME=0` | Sign without the hardened runtime, for example to attach a debugger. |
| `PHAROS_UNIVERSAL=1` | Build arm64 and x86_64 slices with `lipo` instead of the native architecture only. |

A stable identity matters when Pharos runs from an external drive. macOS asks
before an app reads files on a removable volume and records the answer against
the app's designated requirement. The service is started by the app, so its
file access counts as Pharos's. An ad-hoc signature's designated requirement is
its cdhash, which changes whenever the code does, so a rebuilt Pharos can be
asked again or silently lose access. With a keychain identity, the requirement
is the bundle identifier plus the certificate, which survives rebuilds.
Permissions are still stored per Mac, so expect one prompt on each Mac.

Without an Apple Development or Developer ID certificate, run
`macos/create-signing-identity.sh` once. It creates a self-signed identity named
"Pharos Local Code Signing" in your login keychain and trusts it for code
signing only (macOS asks for your password). Then build with
`PHAROS_CODESIGN_IDENTITY="Pharos Local Code Signing"`. A self-signed identity
is for your own Macs only: Gatekeeper rejects it for downloaded copies, the key
exists only on the Mac that created it, and replacing the certificate changes
the designated requirement. The script header lists the details. Builds are not
timestamped or notarized.

To inspect a signature:

```sh
codesign -dvvv dist/Pharos.app                           # Identifier, Signature=adhoc or Authority=…, flags=…(runtime)
codesign -d -r- dist/Pharos.app                          # designated requirement
codesign -dvvv dist/Pharos.app/Contents/MacOS/alexandria
codesign -d -r- dist/Pharos.app/Contents/MacOS/alexandria
codesign --verify --strict --deep --verbose=2 dist/Pharos.app
```

An ad-hoc build reports `designated => cdhash H"…"`. A build signed with the
self-signed identity should report
`designated => identifier "local.ai-work-archive" and certificate leaf = H"…"`.

### UI feedback

The app includes a dependency-free visual annotation tool. Choose **Annotate** in
the bottom-right corner (or press Option-A), hover to identify an element, click
it, and add a note. While hovering, press **Up Arrow** to select progressively
higher parent elements or **Down Arrow** to return toward the original child.
**Feedback** shows the saved annotations and copies a
structured Markdown report containing selectors, bounds, visible or selected
text, computed styles, view context, and a source-file hint suitable for pasting
into a coding-agent conversation. Saved annotations and the note currently being
composed survive app refreshes. They remain local in WebKit storage and are never
uploaded by the archive service.

## Development and verification

For new interface icons, follow the [icon drawing and usage guide](docs/iconography.md).

```sh
go test ./...
go vet ./...
(cd web && npm ci && npx tsc --noEmit && npm run build)
(cd web && npm run check-bundle)
./launch.sh --no-open

# Compatibility/reference suite during the transition
PYTHONPATH=src python3 -m unittest discover -s tests -v
python3 -m compileall -q src
```

The Go tests cover stable cross-language IDs, source configuration, canonical
ingestion, API compatibility, the query-table allowlist/filter/sort/pagination/
aggregation contract, semantic search vectors, and native Conductor
repository/PR/tool/sub-agent extraction. `macos/build-app.sh` reinstalls
`web/` from its lockfile and rebuilds the frontend when npm is available, and
otherwise embeds the checked-in bundle with a warning. `npm run check-bundle`
rebuilds into a scratch directory and fails unless the checked-in
`internal/archive/assets/query-tables.{js,css}` match `web/src` byte for byte;
run it before committing frontend changes.
The Python compatibility suite continues to cover the mothballed preservation and
reclamation reference; `go vet -tags reclamation ./...` keeps the parked Go
queue code compiling. The TL1 hook must maintain
its own disposable integration tests because its repository concurrency
protocol is owner-specific.

`go.mod` pins the Go toolchain (`toolchain go1.26.8`); an older local `go`
downloads it automatically through the Go module proxy.
