# Tool analytics

The Tools tab reads a ledger derived from retained messages: one `model_requests` row per model API request and one `tool_calls` row per tool call, joined to its result. Shell calls are also split into `tool_commands`, one row per simple command. The ledger is rebuilt with a conversation when it is ingested. `pharos build-tools` (or **Build tool ledger** in the Tools tab) fills it from stored messages for conversations indexed before it existed. That works without source files, so reclaimed TL1 work is covered too.

The Tools table reads `tool_usage_daily`, one row per local day and summary dimension. The Tool calls table reads `tool_calls` for its rows, and answers counts, metrics, filter values, and column stats from `tool_call_cube`: the calls grouped by workspace, day, and every field with few values, a quarter as many rows. Both are rebuilt when the ledger changes or the date does. A rebuild reads every call, so pages keep serving the previous build meanwhile: after a sync they can lag it by up to half a minute. A filter the cube cannot apply, such as text inside a command or a duration range, is counted across every call, which takes seconds on a large library. Column stats leave out the distinct counts of per-call numbers and of long text for the same reason.

## Calls and outcomes

- **Tool category** groups tools across providers: command, read, search, edit, web, agent, plan, user, mcp, meta, other. Codex code-mode `exec` scripts that call one non-shell tool (`write_stdin`, `web__run`, ...) are recorded under that tool.
- **Program, subcommand, command category** come from a lexical reading of the command line. It splits on `&&`, `||`, `;`, `|`, `&`, and newlines, skips heredoc bodies, variable assignments, and wrappers such as `sudo`, `env`, and `timeout`, and reads subcommands for tools like git, go, npm, and cargo (`npm run build`, `python3 -m pytest`). The call's program is its first command that does real work, so `cd repo && git status | head` is `git status`. Variables, aliases, and functions are not expanded.
- **Status** is `ok`, `error`, or `no_result` (no result was retained). A call is an error when the provider flagged it, a command exited non-zero, it was interrupted or timed out, or an MCP call failed. Exit code 1 from grep, rg, and diff means "no match" and is not an error.
- **Error type** is harness-imposed (`user_rejected`, `hook_blocked`, `interrupted`, `timeout`), `nonzero_exit` for a failed command, or a tool-level kind (`file_not_read`, `edit_no_match`, `file_not_found`, `permission_denied`, `invalid_input`, `file_too_large`, `tool_error`).

Filter chips and value pickers show readable labels for these keys (`nonzero_exit` is "Non-zero exit", `vcs` is "Version control", `tl1-export` is "TL1 export"). The labels live in the schemas' static `options`. Queries, URLs, saved queries, and table cells keep the raw keys, and the picker matches either.

## Sites and web searches

Each call records the URLs it reached in `tool_urls`, and its first URL, site (host and port), all sites, and search query on the call:

- **Tool arguments:** URL-named inputs of any tool, such as WebFetch's `url`, browser navigation, and the pages Codex's hosted web search opens.
- **Shell commands:** URLs on a line that runs a network client (`curl`, `wget`, `git clone`/`fetch`/`push`, `urlopen`, `requests.get`, `fetch(`, and similar), including URLs assigned to variables such a line uses (`BASE=https://…; curl "$BASE/x"`). URLs in commit messages, PR bodies, `echo`, or XML namespaces are not counted.
- **Results:** pages a web tool reports opening ("Web search completed for: …").
- **Search results:** links a web search returned. They are listed in the call's details but are not counted as sites reached, since the agent may not have opened them.

Only `http`, `https`, `ws`, `wss`, and `ftp` URLs with a literal host are kept; `file://`, `chrome://`, and templated hosts (`https://$HOST/…`) are skipped. The **Sites** preset counts calls by site, and **Web searches** lists searches with their queries.

Codex's hosted web searches (`web_search_call`) are recorded as `web_search` calls. Sessions ingested before this was added need a re-sync of the Codex source; other calls only need the tool ledger rebuilt.

## Duration

`reported` durations come from the provider: Claude's `durationMs` where present, Codex's wall time, and Codex process durations. Otherwise the duration is the gap between the call's and the result's timestamps (`timestamps`). For Claude, that gap includes any time spent waiting for a person to approve the call.

## Tokens and cost

- **Call output tokens:** the emitting request's output tokens, split evenly across the calls it made.
- **Context added:** how much the result grew the context. It is measured as the growth in the next request's prompt over the previous request's prompt plus its output. That growth is split across the results (and any user turns) that arrived in between, in proportion to their size. Without usable request usage it is estimated at 4 bytes a token, plus 1,500 per image. `result_tokens_source` records which.
- **Context carried:** context added times the number of later requests in the same agent stream until the context is compacted. Each of those requests re-reads the result.
- **Cost:** API-equivalent cost at the day's list prices (see [token accounting](token-accounting.md)). Context cost prices the first send as a cache write for Claude (5-minute rate) or as uncached input for others, then the carried tokens as cache reads. Output cost prices the call's output share.

Conductor's Codex sessions report usage per turn, not per request, so their results are always estimated. Linked mirrors of the same work (a Conductor workspace and the native session it wraps) are counted once, as in Usage.
