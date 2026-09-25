# Transcript reader research and design

Research date: 2026-09-20.

This design compares Conductor 0.85, Codex Desktop 26.908.70816, and Claude Code Desktop 2.2553.1. The review used official documentation and product media, plus read-only inspection of the locally installed first-party application bundles. Live native UI automation was unavailable, so implementation details inferred from bundles are treated as supporting evidence rather than a stable public contract.

## Shared model

All three products make the conversation, not the event log, the primary reading experience. They converge on four visual layers:

1. **Intent:** the user request is visually distinct and anchors a turn.
2. **Narrative:** assistant prose is full-width, readable, and largely frameless.
3. **Activity:** commands, reads, searches, and tool calls are quiet semantic rows, collapsed by default.
4. **Artifacts:** diffs, plans, tasks, and other outputs receive richer review surfaces outside the routine activity trace.

| Surface | Conductor | Codex Desktop | Claude Code Desktop |
|---|---|---|---|
| Default activity | Icon-led one-line rows | Consecutive work becomes one grammatical disclosure | Semantic one-line rows |
| Expanded activity | Bordered group with nested chronological rows | Scroll-bounded activity units and specialized details | Local disclosure plus specialized tool renderers |
| Global density | Normal and “Garry” expanded mode | Local disclosures and grouped activity | Normal, Thinking, and Verbose modes |
| Commands | Intent label plus literal command | Terminal block, output, copy, exit state | Compact row; detail inline or in Terminal pane |
| File changes | Inline summary; full review in Changes | Turn outcome card; full review pane | Path and +/- row; inline diff; Changes pane |
| Sub-agents | Nested bordered run with child events | Lifecycle summary and navigable child conversation | Task row; assignment/result; Subagent pane |
| Reasoning | Collapsible and density-controlled | `Thinking` / `Thought for …`, bounded when open | Optional Thinking mode, associated with work groups |
| Blocking states | High-emphasis inline approval/question/plan cards | Separate awaiting-approval state | Decision-oriented permission cards |

## Product-specific lessons

### Conductor

Conductor is strongest at causal hierarchy. A sub-agent owns its child execution inside one disclosure, running/waiting state stays inline, and expanded state remains stable as work streams. Its checkpoint model reinforces the user turn as the unit of work. Full diffs and durable todos live in dedicated review surfaces rather than flooding the transcript.

Sources: [workspaces and workflow](https://conductor.build/docs/concepts/workflow), [checkpoints](https://conductor.build/docs/reference/checkpoints), [todos](https://conductor.build/docs/reference/todos), [keyboard shortcuts](https://conductor.build/docs/reference/keyboard-shortcuts), and [Codex UI changelog](https://www.conductor.build/changelog/0.51.0-codex-is-more-beautiful).

### Codex Desktop

Codex is strongest at compression and outcome handoff. Completed low-level operations collapse into sentences such as “edited files, read files, and ran commands,” while expansion preserves chronology. Long reasoning, command output, and tool results have bounded viewports. Edits appear both as activity and as a turn-level diff artifact, creating a clear transition from agent execution to human review. Raw MCP protocol data is one level deeper than the human-readable result.

Source: [Codex App Server](https://learn.chatgpt.com/docs/app-server) and the locally installed renderer bundle.

### Claude Code Desktop

Claude provides the clearest two-axis disclosure model: a global Normal/Thinking/Verbose transcript setting plus local expansion and dedicated Changes, Terminal, Tasks, Subagent, and Plan panes. Its compact rows answer “what happened, to what, and how much,” while tool-specific detail renderers avoid falling back to raw JSON. Sub-agents are summarized by their contract—assignment, state, duration, result—rather than replaying an entire child transcript into the parent by default.

Sources: [Claude Code on desktop](https://code.claude.com/docs/en/desktop), [desktop redesign](https://claude.com/blog/claude-code-desktop-redesign), and [preview, review, and merge](https://claude.com/blog/preview-review-and-merge-with-claude-code).

## Pharos design

Pharos is an archive reader, not a live execution console. It should optimize for rapid reconstruction of past work while retaining the original evidence. The implemented reader therefore uses three depths:

### Scan

- A conversation header names the provider and summarizes turns, tool calls, sub-agents, and model.
- Each turn collapses to its prompt, latest outcome excerpt, and reply/activity counts.
- Assistant prose remains the narrative spine; user prompts are visually anchored without making every message a chat bubble.
- Consecutive operational events collapse into one grammatical activity group categorized as reads, searches, commands, edits, browser actions, or run events.

### Inspect

- Expanding an activity group reveals chronological rows with a semantic icon, human label, salient path/command/query, time, and explicit error styling.
- Sub-agent calls are causal containers. Their assignment, completion state, nested work, and returned result stay together.
- Search includes prompts, prose, tool payloads, and nested sub-agent evidence.
- “Expand all” and “Collapse all” provide a global audit-density control without changing the stored transcript.

### Audit

- Each activity row retains the complete archived payload behind another disclosure.
- Structured payloads remain lazily navigable as a JSON tree.
- Large textual output is bounded and scrollable instead of expanding the page indefinitely.
- Exact source coverage, native identity, and origin remain available in Source details.

### Refined event grammar

The reader follows Tufte's principle that presentation should support reasoning about evidence ([Analytical design and human factors](https://www.edwardtufte.com/notebook/analytical-design-and-human-factors/)). The primary unit is an aligned action and its distinguishing target, not a container describing the data format. Quiet typography and spacing replace nested activity cards.

- A Read shows the filename and its directory immediately; Run shows the command; Search shows its query and scope. Full paths remain available on expansion and hover. There is no generic activity bucket hiding these targets.
- Open turns show the prompt once in the conversation. Closed turns show a prompt/outcome preview. Opening all turns does not open every raw payload.
- Provider envelopes are normalized before classification, pairing, counts, and turn grouping. Mixed text/tool blocks preserve their order; wrapped user-role tool results remain outputs, not human turns. Multiple results remain available, and orphaned child events are preserved.
- One expansion reveals readable inputs, outputs, or changes. Original source JSON is the last disclosure. Unknown event types receive a short semantic description rather than a JSON preview.
- Missing evidence is explicit: no returned result is not success, a failed edit is not labeled completed, and a Write without previous contents shows unknown deltas and retained content rather than an invented diff. Replacement-all and content-free delete patches also avoid fabricated totals.

- Tool calls and their matching results are one disclosure keyed by `call_id`. The compact row describes the action once; expansion separates input from result and preserves the result's relative completion time.
- Write, edit, and patch calls use file chips containing the path and observed `+added/−removed` counts. Expanding the row opens a bounded, line-colored diff before the lower-level tool payload.
- Common wrapped message envelopes are unwrapped into readable conversation text. The original JSON remains available under **Raw event** rather than replacing the message.
- Human and assistant messages share the same compact/expanded behavior. Long messages truncate in place, while the enclosing user turn remains independently collapsible.
- All transcript timing is relative to the beginning of its user turn and occupies a narrow left gutter (`+10s`, `+2m03s`) instead of repeating labels and wall-clock timestamps.
- A fixed conversation minimap reflects the relative height of every turn, highlights user-authored turns and the current turn, shows the visible viewport, and supports click-to-navigate.

## Data normalization

Semantic rendering depends on retaining tool identity, call identity, inputs, and results. Native Codex and Claude ingestion now stores tool events in a common envelope:

```json
{"tool": "exec_command", "input": {"cmd": "go test ./..."}}
```

Results use the same identity and call id:

```json
{"tool": "exec_command", "content": "ok", "is_error": false}
```

Regular tool results are retained, not only delegation results. Claude's `parentUuid` message-chain pointer is not treated as a sub-agent parent; only explicit tool-call relationships should drive visual nesting. This prevents ordinary Claude replies from disappearing into an unrelated causal tree.

## Deliberate limits

- Pharos never synthesizes or exposes hidden reasoning. It only renders reasoning-like content if a source explicitly exports it as visible evidence.
- Inline transcript activity is not a replacement for the existing workspace-level Changes section. A future dedicated diff viewer can deepen that handoff without making routine turns heavier.
- Live approvals, timers, retry controls, and checkpoints are intentionally out of scope for an immutable archive. Historical failure and result states remain visible, but the reader does not pretend they are actionable.
- A future density selector could add a prose-only reading mode. The default keeps operation targets visible in compact rows.

## Browser verification

The workspace detail page is conversation-first: a compact title/repository/branch/date header, PR state links, collapsed Usage and Files controls, and a selector when multiple conversations exist. The Files index combines retained change evidence with transcript file targets and searches the relevant conversation; it does not claim those operations represent final working-tree state. Reported tool failures have a next-action navigation control, not a workspace failure verdict. Only explicit coverage/preservation limitations produce warnings. Attempts, handoffs, source identities, paths, and preservation receipts live in Archive details below the transcript. Duplicate initiation/outcome/failure prose is omitted, except as a collapsed fallback when no transcript was retained.

The dense layout uses near-zero inter-event margins, approximately 23px operation rows, and smaller prose/detail typography. Assistant responses render headings, emphasis, code, lists, quotations, tables, and links as Markdown using DOM construction; raw HTML is displayed as text and unsafe link schemes are not activated. Bash commands receive git, Python, typecheck, test, lint, or generic command icons.

Todo list summaries compare each snapshot to the previous one in the same agent scope. They describe added, removed, renamed, or status-changed tasks; the first available snapshot is labeled as a set of todos without inventing earlier history. Expanded previews retain the full list. Repository locations now flow through both detail APIs, alongside workspace roots and recorded session working directories, so paths inside either checkout default to collapsed.

Adjacent calls of the same operation now share one row with independent file/search chips; grouping stops at prose, delegation, or a different operation to preserve chronology. Each chip opens only its own output or file diff, while the row's Details disclosure retains complete inputs, results, and source data. File chips contain an expandable directory prefix, filename, and copy control. Known workspace paths expand relative to the workspace; external or unclassified absolute paths stay explicit.

Todo creation, updates, deletion, and lists have distinct labels and status previews, including TodoWrite, task management tools, and plan updates. Agent prose has a stronger visual treatment and a larger compact excerpt; raw-event access is an inline control with no additional collapsed line.

`tests/transcript-ui.test.cjs` exercises the real embedded HTML in headless Chrome, including provider envelopes, result pairing, errors, file changes, nested agents, keyboard disclosure, mobile filename visibility, and minimap navigation. Install its isolated dependency with `npm install --prefix .context/browser-tests playwright --no-audit --no-fund`, then run `node --test tests/transcript-ui.test.cjs`. Set `TRANSCRIPT_SCREENSHOTS=1` to capture fixtures under `.context/browser-tests/screenshots`.
