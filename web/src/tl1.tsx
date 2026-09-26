import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import "./tl1.css";
import { Icon, type IconName } from "./icons";

// The TL1 tab: a project's performance by flavor and agent configuration,
// combined across the Macs that run it, ranked concerns and opportunities with investigation prompts,
// and drill-downs into flavors, candidates, and individual runs.

type Row = Record<string, any>;
type Counted = { key: string; count: number };
type Overview = {
  project?: string; installations: Row[]; since?: string | null; window?: Row; enqueues?: Row[];
  totals?: Row; coverage?: Row; matrix?: Row[]; flavors?: Row[]; graph?: { nodes: Row[]; edges: Row[] };
  errors?: Row[]; candidates?: Row; human?: Row; contract_repairs?: Row[]; reviews?: Row;
  detectors?: Row[]; opportunities?: Row[]; review_prompt?: string;
};
type Where = Array<{ field: string; op: string; value: unknown }>;
type Props = { attempts: React.ReactNode; filterAttempts: (where: Where) => void; copy: (text: string) => Promise<void> };

// The analysis window. "latest" and "enqueue" follow TL1's enqueue
// segmentation: a large enqueue is 5+ root tasks created with no gap over ten
// minutes, so each one marks when a batch of work ran on the flavor
// definitions of the time. "segment" stops at the next enqueue.
type TimeWindow =
  | { mode: "latest" } | { mode: "all" } | { mode: "days"; days: number }
  | { mode: "enqueue"; since: string; segment: boolean }
  | { mode: "custom"; since: string; until: string };
const windowStorage = "tl1-window";
const dayPresets = [{ label: "24 hours", days: 1 }, { label: "7 days", days: 7 }, { label: "30 days", days: 30 }];

function readWindow(): TimeWindow {
  try {
    const value = JSON.parse(localStorage.getItem(windowStorage) || "");
    if (value && ["latest", "all", "days", "enqueue", "custom"].includes(value.mode)) return value as TimeWindow;
  } catch { /* Missing or malformed state falls back to the latest enqueue. */ }
  return { mode: "latest" };
}

/** Query parameters for a window; enqueues are newest first. */
function windowParams(window: TimeWindow, enqueues: Row[]): Record<string, string> {
  switch (window.mode) {
    case "latest": return { since: "latest" };
    case "days": return { days: String(window.days) };
    case "enqueue": {
      const index = enqueues.findIndex(item => item.started_at === window.since);
      const next = index > 0 ? enqueues[index - 1].started_at : "";
      return window.segment && next ? { since: window.since, until: next } : { since: window.since };
    }
    case "custom": return Object.fromEntries(Object.entries({ since: window.since, until: window.until }).filter(([, value]) => value));
    default: return {};
  }
}

// datetime-local inputs work in local time without a zone; the API takes ISO.
const toLocalInput = (value: unknown) => {
  const date = new Date(String(value ?? ""));
  if (!value || Number.isNaN(date.getTime())) return "";
  const pad = (part: number) => String(part).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
};
const fromLocalInput = (value: string) => value ? new Date(value).toISOString() : "";

async function getJSON<T>(url: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(url, { signal });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `Request failed with HTTP ${response.status}`);
  return body as T;
}

const usd = (value: unknown) => {
  const amount = Number(value);
  if (value === null || value === undefined || !Number.isFinite(amount)) return "—";
  if (Math.abs(amount) >= 1e6) return `$${amount.toLocaleString(undefined, { notation: "compact", maximumFractionDigits: 1 })}`;
  const digits = Math.abs(amount) >= 100 ? 0 : 2;
  return amount.toLocaleString(undefined, { style: "currency", currency: "USD", minimumFractionDigits: digits, maximumFractionDigits: digits });
};
const pct = (value: unknown) => value === null || value === undefined || !Number.isFinite(Number(value)) ? "—" : `${Math.round(Number(value) * 100)}%`;
const count = (value: unknown) => value === null || value === undefined ? "—" : Number(value).toLocaleString();
const compact = (value: unknown) => {
  const number = Number(value);
  if (value === null || value === undefined || !Number.isFinite(number)) return "—";
  return number.toLocaleString(undefined, { notation: "compact", maximumFractionDigits: 1 });
};
const duration = (value: unknown) => {
  const ms = Number(value);
  if (value === null || value === undefined || !Number.isFinite(ms)) return "—";
  if (ms < 60_000) return `${Math.round(ms / 1000)}s`;
  if (ms < 3_600_000) return `${(ms / 60_000).toFixed(1)} min`;
  return `${(ms / 3_600_000).toFixed(1)} h`;
};
const when = (value: unknown) => {
  const date = new Date(String(value ?? ""));
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
};
// When each Mac's TL1 was last read, which differs when the library was last
// on that Mac.
const synced = (installations: Row[] = []) => installations.length > 1
  ? `Synced ${installations.map(item => `${item.host_label || "an unknown Mac"} ${when(item.synced_at)}`).join(", ")}`
  : `Synced ${when(installations[0]?.synced_at)}`;
const openWork = (id: unknown) => { if (id) window.alexandriaOpenDetail?.(String(id)); };
const typing = (target: EventTarget | null) => target instanceof HTMLElement && (target.isContentEditable || ["SELECT", "TEXTAREA"].includes(target.tagName)
  || (target instanceof HTMLInputElement && !["checkbox", "radio", "button"].includes(target.type)));

const severityLabel: Record<string, string> = { high: "High", medium: "Medium", low: "Low", info: "Info" };
const severityIcon: Record<string, IconName> = { high: "severity-high", medium: "severity-medium", low: "severity-low", info: "info" };
const dispositionLabel: Record<string, string> = { advanced: "Advanced", escalated: "Escalated to human", error: "Error", retried: "Retried", open: "Open" };

function CopyButton({ text, label, copy, primary }: { text: string; label: string; copy: Props["copy"]; primary?: boolean }) {
  const [state, setState] = useState<"" | "copied" | "failed">("");
  return <button type="button" className={`tl1-button ${primary ? "primary" : ""}`} disabled={!text} onClick={async () => {
    try { await copy(text); setState("copied"); } catch { setState("failed"); }
    window.setTimeout(() => setState(""), 2200);
  }}>{state === "copied" ? "Copied" : state === "failed" ? "Copy failed" : label}</button>;
}

function Meter({ value, tone = "accent", title }: { value: unknown; tone?: "accent" | "bad" | "warn"; title?: string }) {
  const number = Number(value);
  if (value === null || value === undefined || !Number.isFinite(number)) return <span className="muted">—</span>;
  return <span className="tl1-meter" title={title}>
    <span className="tl1-meter-track"><span className={`tl1-meter-fill ${tone}`} style={{ width: `${Math.max(2, Math.min(100, number * 100))}%` }} /></span>
    <span className="tl1-meter-value">{pct(number)}</span>
  </span>;
}

function Tile({ label, value, detail, title }: { label: string; value: React.ReactNode; detail?: React.ReactNode; title?: string }) {
  return <div className="tl1-tile" title={title}><span>{label}</span><strong>{value}</strong>{detail ? <small>{detail}</small> : null}</div>;
}

function Section({ id, title, description, actions, children }: { id: string; title: string; description?: React.ReactNode; actions?: React.ReactNode; children: React.ReactNode }) {
  return <section className="tl1-section" id={`tl1-${id}`} aria-labelledby={`tl1-${id}-title`}>
    <div className="tl1-section-head"><div><h2 id={`tl1-${id}-title`}>{title}</h2>{description ? <p className="muted">{description}</p> : null}</div>{actions ? <div className="tl1-actions">{actions}</div> : null}</div>
    {children}
  </section>;
}

// A ranked horizontal bar list: one magnitude per row, one hue, value labels
// always visible, and a hover title with the full breakdown.
function BarList({ rows, max, render, segments, wide }: { rows: Row[]; wide?: boolean; max: number; render: (row: Row) => { label: React.ReactNode; value: string; title: string; onClick?: () => void }; segments?: (row: Row) => Array<{ share: number; tone: string }> }) {
  return <ol className={`tl1-bars ${wide ? "wide" : ""}`}>{rows.map((row, index) => {
    const item = render(row);
    const parts = segments ? segments(row) : [{ share: 1, tone: "accent" }];
    const width = max > 0 ? Math.max(1, (Number(row.__value) / max) * 100) : 0;
    const content = <>
      <span className="tl1-bar-label">{item.label}</span>
      <span className="tl1-bar-track"><span className="tl1-bar" style={{ width: `${width}%` }}>{parts.filter(part => part.share > 0).map((part, position) => <span key={position} className={`tl1-bar-part ${part.tone}`} style={{ flexGrow: part.share }} />)}</span></span>
      <span className="tl1-bar-value">{item.value}</span>
    </>;
    return <li key={index} title={item.title}>{item.onClick ? <button type="button" className="tl1-bar-row" onClick={item.onClick}>{content}</button> : <div className="tl1-bar-row">{content}</div>}</li>;
  })}</ol>;
}

function Dialog({ title, subtitle, onClose, children }: { title: string; subtitle?: React.ReactNode; onClose: () => void; children: React.ReactNode }) {
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => { if (event.key === "Escape") onClose(); };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);
  return <div className="tl1-dialog-backdrop" onClick={onClose}>
    <div className="tl1-dialog" role="dialog" aria-modal="true" aria-label={title} onClick={event => event.stopPropagation()}>
      <div className="tl1-section-head"><div><h2>{title}</h2>{subtitle ? <p className="muted">{subtitle}</p> : null}</div><button type="button" className="tl1-button" onClick={onClose}>Close</button></div>
      {children}
    </div>
  </div>;
}

function ConfigurationTable({ rows, onFlavor, showFlavor }: { rows: Row[]; onFlavor?: (name: string) => void; showFlavor: boolean }) {
  let previous = "";
  return <div className="tl1-table-wrap"><table className="tl1-table">
    <thead><tr>
      {showFlavor ? <th>Flavor</th> : null}<th>Agent configuration</th><th className="num">Runs</th><th>Advanced</th><th>Escalated</th>
      <th title="Errors the run is responsible for; infrastructure failures (dead workers, restarts) are excluded">Agent errors</th>
      <th className="num">Median cost</th><th className="num" title="Total spend divided by runs that advanced the workflow on their own">Cost per useful result</th>
      <th className="num">Median time</th><th className="num">Median tokens</th><th className="num">Cache reads</th>
    </tr></thead>
    <tbody>{rows.map((row, index) => {
      const first = row.flavor !== previous;
      previous = row.flavor;
      return <tr key={index} className={`${row.low_sample ? "low-sample" : ""} ${first && showFlavor ? "group-start" : ""}`}>
        {showFlavor ? <td>{first ? <button type="button" className="tl1-link" onClick={() => onFlavor?.(row.flavor)}>{row.flavor}</button> : null}</td> : null}
        <td>{row.configuration}{row.default ? <span className="tl1-chip" title="The flavor's default agent configuration">default</span> : null}{row.low_sample ? <span className="tl1-chip muted" title="Fewer than 10 runs; not compared">few runs</span> : null}</td>
        <td className="num">{count(row.attempts)}</td>
        <td><Meter value={row.advance_rate} title={`${row.advanced} of ${row.attempts} runs advanced the workflow`} /></td>
        <td><Meter value={row.escalation_rate} tone="warn" title={`${row.escalated} runs handed off to a human`} /></td>
        <td><Meter value={row.agent_error_rate} tone="bad" title={`${row.agent_errors} errors attributable to the run; ${row.infrastructure_errors} infrastructure errors excluded${row.top_errors?.length ? `. Most common: ${row.top_errors[0].key}` : ""}`} /></td>
        <td className="num">{usd(row.median_cost_usd)}</td>
        <td className="num">{usd(row.cost_per_advance_usd)}{row.flag === "best_value" ? <span className="tl1-chip good">best value</span> : row.flag === "worst_value" ? <span className="tl1-chip bad">worst value</span> : null}</td>
        <td className="num">{duration(row.median_duration_ms)}</td>
        <td className="num">{compact(row.median_tokens)}</td>
        <td className="num">{pct(row.cache_read_share)}</td>
      </tr>;
    })}</tbody>
  </table></div>;
}

function Finding({ item, copy, onFlavor, onRuns }: { item: Row; copy: Props["copy"]; onFlavor: (name: string) => void; onRuns: (filter: Row) => void }) {
  const impact = item.impact ?? {};
  const evidence = item.evidence ?? {};
  const facts: string[] = [];
  if (impact.tasks) facts.push(`${count(impact.tasks)} tasks`);
  if (impact.candidates) facts.push(`${count(impact.candidates)} candidates`);
  if (impact.human_followups) facts.push(`${count(impact.human_followups)} human follow-ups`);
  if (impact.cost_usd >= 0.01) facts.push(`${usd(impact.cost_usd)} spent`);
  if (item.estimated_savings_usd >= 0.01) facts.push(`≈${usd(item.estimated_savings_usd)} savings`);
  if (item.human_tasks_avoided) facts.push(`${count(item.human_tasks_avoided)} human tasks avoided`);
  const severity = String(item.severity ?? "opportunity");
  return <article className={`tl1-finding ${severity}`}>
    <div className="tl1-finding-head">
      {item.severity ? <span className={`tl1-severity ${severity}`}><Icon name={severityIcon[severity] ?? "severity-low"} />{severityLabel[severity] ?? severity}</span> : <span className="tl1-severity opportunity">Opportunity</span>}
      <h3>{item.title}</h3>
    </div>
    <p>{item.summary}</p>
    {facts.length ? <div className="tl1-facts">{facts.map(fact => <span key={fact} className="tl1-chip">{fact}</span>)}</div> : null}
    <div className="tl1-actions">
      {item.prompt ? <CopyButton text={item.prompt} label="Copy investigation prompt" copy={copy} primary /> : null}
      {item.filter ? <button type="button" className="tl1-button" onClick={() => onRuns(item.filter)}>Show runs</button> : null}
      {item.flavor ? <button type="button" className="tl1-button" onClick={() => onFlavor(String(item.flavor))}>Open {item.flavor}</button> : null}
    </div>
    {evidence.example || evidence.samples?.length || evidence.attempts?.length ? <details className="tl1-evidence"><summary>Evidence</summary>
      {evidence.example ? <pre>{evidence.example}</pre> : null}
      {evidence.log_tail ? <><h4>Script log tail</h4><pre>{evidence.log_tail}</pre></> : null}
      {evidence.samples?.length ? <ul>{evidence.samples.map((sample: Row) => <li key={sample.task_id}><button type="button" className="tl1-link" onClick={() => openWork(sample.workspace_id)}>{sample.title || sample.task_id}</button> <span className="muted">{sample.flavor} · {when(sample.created_at)}</span></li>)}</ul> : null}
      {evidence.attempts?.length ? <ul>{evidence.attempts.map((attempt: Row) => <li key={attempt.attempt_id}><button type="button" className="tl1-link" onClick={() => openWork(attempt.workspace_id)}>{attempt.title}</button> <span className="muted">{attempt.flavor} on {attempt.configuration}: {usd(attempt.cost_usd)} vs median {usd(attempt.median_cost_usd)} ({Number(attempt.ratio).toFixed(1)}×)</span></li>)}</ul> : null}
    </details> : null}
  </article>;
}

function FlavorDialog({ query, name, onClose, copy, onRuns }: { query: string; name: string; onClose: () => void; copy: Props["copy"]; onRuns: (filter: Row) => void }) {
  const [detail, setDetail] = useState<Row | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    getJSON<Row>(`/api/tl1/flavor?${query}&name=${encodeURIComponent(name)}`, controller.signal).then(setDetail).catch(failure => { if (!controller.signal.aborted) setError(failure.message); });
    return () => controller.abort();
  }, [query, name]);
  const summary = detail?.summary ?? {};
  const definition = detail?.definition ?? {};
  return <Dialog title={name} subtitle={definition.execution_class ? `${definition.execution_class} flavor${definition.default_agent_configuration ? ` · default ${definition.default_agent_configuration}` : ""}` : undefined} onClose={onClose}>
    {error ? <div className="query-table-error">{error}</div> : !detail ? <p className="muted">Loading…</p> : <>
      <div className="tl1-tiles">
        <Tile label="Tasks" value={count(summary.tasks)} detail={`${count(summary.attempts)} runs`} />
        <Tile label="Advanced" value={pct(summary.tasks ? summary.advanced / summary.tasks : null)} />
        <Tile label="Errors" value={pct(summary.error_rate)} detail={`${count(summary.errors)} tasks`} />
        <Tile label="Escalated" value={pct(summary.escalation_rate)} />
        <Tile label="Spend" value={usd(summary.cost_usd)} detail={`median ${usd(summary.median_task_cost_usd)} per task`} />
        <Tile label="Repeat visits" value={pct(summary.revisit_rate)} title="Share of tasks on a candidate that had already run this flavor" />
      </div>
      <div className="tl1-actions"><CopyButton text={detail.prompt} label="Copy tuning prompt" copy={copy} primary /><button type="button" className="tl1-button" onClick={() => { onRuns({ flavor: name }); onClose(); }}>Show runs</button></div>
      {detail.configurations?.length ? <><h3>By agent configuration</h3><ConfigurationTable rows={detail.configurations} showFlavor={false} /></> : null}
      {summary.shape_versions?.length ? <><h3>Definition history</h3><div className="tl1-table-wrap"><table className="tl1-table"><thead><tr><th>Shape version</th><th>First run</th><th>Last run</th><th className="num">Tasks</th><th>Advanced</th><th>Errors</th><th className="num">Median task cost</th></tr></thead>
        <tbody>{summary.shape_versions.map((version: Row) => <tr key={version.shape_version}><td><code>{String(version.shape_version).slice(0, 10)}</code>{version.current ? <span className="tl1-chip">current</span> : null}</td><td>{when(version.first_seen)}</td><td>{when(version.last_seen)}</td><td className="num">{count(version.tasks)}</td><td><Meter value={version.advance_rate} /></td><td><Meter value={version.error_rate} tone="bad" /></td><td className="num">{usd(version.median_cost_usd)}</td></tr>)}</tbody></table></div></> : null}
      <div className="tl1-two">
        <div><h3>Arrives from</h3><ul className="tl1-list">{(detail.incoming ?? []).map((item: Counted) => <li key={item.key}><span>{item.key}</span><span className="num">{count(item.count)}</span></li>)}</ul></div>
        <div><h3>Hands off to</h3><ul className="tl1-list">{(detail.outgoing ?? []).map((item: Counted) => <li key={item.key}><span>{item.key}</span><span className="num">{count(item.count)}</span></li>)}</ul></div>
      </div>
      {detail.recent_failures?.length ? <><h3>Recent failures</h3><ul className="tl1-failures">{detail.recent_failures.map((task: Row) => <li key={task.task_id}>
        <button type="button" className="tl1-link" onClick={() => openWork(task.workspace_id)}>{task.title}</button> <span className="muted">{when(task.created_at)} · {task.configurations?.join(", ") || "no agent"}</span>
        <div className="tl1-signature">{task.error_signature}</div>
      </li>)}</ul></> : null}
      <details className="tl1-evidence"><summary>Definition</summary>
        {definition.template ? <><h4>Prompt template</h4><pre>{definition.template}</pre></> : null}
        {(["agent_configurations", "transitions", "outcomes", "budgets", "retry_config", "outputs_schema", "inputs_schema"] as const).map(key => definition[key] ? <div key={key}><h4>{key.replace(/_/g, " ")}</h4><pre>{JSON.stringify(definition[key], null, 2)}</pre></div> : null)}
      </details>
    </>}
  </Dialog>;
}

const dispositionTone: Record<string, string> = { advanced: "accent", escalated: "warn", error: "bad", retried: "muted", open: "muted" };

function CandidateDialog({ query, id, onClose }: { query: string; id: string; onClose: () => void }) {
  const [detail, setDetail] = useState<Row | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    getJSON<Row>(`/api/tl1/candidate?${query}&id=${encodeURIComponent(id)}`, controller.signal).then(setDetail).catch(failure => { if (!controller.signal.aborted) setError(failure.message); });
    return () => controller.abort();
  }, [query, id]);
  const tasks: Row[] = detail?.tasks ?? [];
  const times = tasks.flatMap(task => [Date.parse(task.created_at), Date.parse(task.completed_at ?? task.created_at)]).filter(Number.isFinite);
  const start = Math.min(...times), end = Math.max(...times), span = Math.max(end - start, 1);
  const candidate = detail?.candidate ?? {};
  return <Dialog title={String(candidate.title ?? id)} subtitle={detail ? `${candidate.status} · ${candidate.workflow_steps}/${candidate.workflow_step_limit} steps · ${usd(detail.cost_usd)} · ${tasks.length} tasks` : undefined} onClose={onClose}>
    {error ? <div className="query-table-error">{error}</div> : !detail ? <p className="muted">Loading…</p> : <>
      <div className="tl1-legend" aria-label="Legend">{Object.entries(dispositionTone).filter(([key]) => key !== "retried").map(([key, tone]) => <span key={key}><span className={`tl1-swatch ${tone}`} />{dispositionLabel[key]}</span>)}</div>
      <ol className="tl1-timeline">{tasks.map(task => {
        const left = ((Date.parse(task.created_at) - start) / span) * 100;
        const right = ((Date.parse(task.completed_at ?? task.created_at) - start) / span) * 100;
        const tone = dispositionTone[task.disposition] ?? "muted";
        return <li key={task.task_id} title={`${task.flavor} → ${task.outcome ?? task.disposition}\n${when(task.created_at)} – ${when(task.completed_at)}\n${usd(task.cost_usd)} · ${task.configurations?.join(", ") || "no agent"}${task.error_signature ? `\n${task.error_signature}` : ""}`}>
          <button type="button" className="tl1-timeline-label tl1-link" onClick={() => openWork(task.workspace_id)}>{task.flavor}</button>
          <span className="tl1-timeline-track"><span className={`tl1-timeline-bar ${tone}`} style={{ left: `${left}%`, width: `${Math.max(right - left, 0.6)}%` }} /></span>
          <span className="tl1-timeline-outcome">{task.outcome ?? dispositionLabel[task.disposition]}{task.cost_usd ? ` · ${usd(task.cost_usd)}` : ""}</span>
        </li>;
      })}</ol>
      {detail.review_findings?.length ? <><h3>Review findings</h3><ul className="tl1-failures">{detail.review_findings.map((finding: Row) => <li key={finding.finding_id}><span className={`tl1-chip ${finding.severity === "major" || finding.severity === "blocker" ? "bad" : ""}`}>{finding.severity}</span> {finding.file_path ? <code>{finding.file_path}{finding.line ? `:${finding.line}` : ""}</code> : null}<div>{finding.explanation}</div></li>)}</ul></> : null}
      {detail.human_touches?.length ? <><h3>Human touches</h3><ul className="tl1-failures">{detail.human_touches.map((touch: Row) => <li key={touch.touch_id}><span className="muted">{when(touch.created_at)} · {touch.action ?? touch.kind}</span>{touch.body ? <div>{touch.body}</div> : null}</li>)}</ul></> : null}
    </>}
  </Dialog>;
}

function WindowControls({ value, onChange, enqueues, resolved }: { value: TimeWindow; onChange: (next: TimeWindow) => void; enqueues: Row[]; resolved?: Row }) {
  const selectedSince = value.mode === "latest" ? enqueues[0]?.started_at : value.mode === "enqueue" ? value.since : undefined;
  const segment = value.mode === "enqueue" && value.segment;
  const pick = (item: Row, index: number) => onChange(index === 0 && !segment ? { mode: "latest" } : { mode: "enqueue", since: item.started_at, segment });
  const custom = value.mode === "custom" ? value : { since: resolved?.since ?? "", until: resolved?.until ?? "" };
  return <div className="tl1-window">
    <div className="tl1-controls" role="group" aria-label="Analysis window">
      <div className="tl1-segmented" role="group" aria-label="Time window">
        <button type="button" aria-pressed={value.mode === "latest"} disabled={!enqueues.length} onClick={() => onChange({ mode: "latest" })}
          title={enqueues.length ? `Tasks created since the latest large enqueue, ${when(enqueues[0].started_at)} (as in TL1)` : "No large enqueue (5+ root tasks within 10 minutes) was found"}>Latest enqueue</button>
        {dayPresets.map(item => <button key={item.days} type="button" aria-pressed={value.mode === "days" && value.days === item.days} onClick={() => onChange({ mode: "days", days: item.days })}>{item.label}</button>)}
        <button type="button" aria-pressed={value.mode === "all"} onClick={() => onChange({ mode: "all" })}>All time</button>
      </div>
      <label>From <input type="datetime-local" aria-label="Tasks created from" value={toLocalInput(custom.since)} onChange={event => onChange({ mode: "custom", since: fromLocalInput(event.target.value), until: custom.until ?? "" })} /></label>
      <label>to <input type="datetime-local" aria-label="Tasks created before" value={toLocalInput(custom.until)} onChange={event => onChange({ mode: "custom", since: custom.since ?? "", until: fromLocalInput(event.target.value) })} /></label>
    </div>
    {enqueues.length ? <div className="tl1-enqueues" role="group" aria-label="Recent large enqueues">
      <span className="tl1-enqueues-label muted" title="A large enqueue is 5 or more root tasks created with no gap over 10 minutes. Press [ and ] to step to an older or newer enqueue.">Enqueues</span>
      <ol className="tl1-enqueue-list">{enqueues.map((item, index) => {
        const changed: string[] = item.changed_flavors ?? [];
        const flavors = (item.flavors ?? []).map((flavor: Counted) => `${flavor.count} ${flavor.key}`).join(", ");
        return <li key={item.started_at}><button type="button" className="tl1-enqueue" aria-pressed={item.started_at === selectedSince} onClick={() => pick(item, index)}
          title={`${when(item.started_at)} – ${when(item.ended_at)}\n${count(item.root_task_count)} root tasks: ${flavors}\n${count(item.tasks_since)} tasks created since${changed.length ? `\nNew definitions: ${changed.join(", ")}` : ""}${item.changed_during?.length ? `\nRevised during this period: ${item.changed_during.join(", ")}` : ""}`}>
          <strong>{index === 0 ? "Latest · " : ""}{when(item.started_at)}</strong>
          <span>{count(item.root_task_count)} roots{changed.length ? <> · <span className="tl1-changed">{changed.length} new {changed.length === 1 ? "definition" : "definitions"}</span></> : null}</span>
        </button></li>;
      })}</ol>
      <label className="tl1-check" title="Stop at the start of the next enqueue, so the analysis covers only the work this enqueue started under its definitions">
        <input type="checkbox" checked={segment} disabled={!selectedSince || selectedSince === enqueues[0]?.started_at}
          onChange={event => onChange({ mode: "enqueue", since: selectedSince, segment: event.target.checked })} /> Until next enqueue
      </label>
    </div> : null}
  </div>;
}

function WindowSummary({ resolved, enqueues, onFlavor, onCurrent }: { resolved: Row; enqueues: Row[]; onFlavor: (name: string) => void; onCurrent?: () => void }) {
  const enqueue = enqueues.find(item => item.started_at === resolved.since);
  const changed: string[] = enqueue?.changed_flavors ?? [];
  const mixed: string[] = resolved.mixed_definitions ?? [];
  return <p className="tl1-window-summary">
    <strong>{count(resolved.tasks)}</strong> of {count(resolved.total_tasks)} tasks: {resolved.label ?? "all tasks"}.
    {resolved.note ? <span className="muted"> {resolved.note}</span> : null}
    {changed.length ? <span> New definitions took effect at this enqueue: {changed.map((name, index) => <React.Fragment key={name}>{index ? ", " : ""}<button type="button" className="tl1-link" onClick={() => onFlavor(name)}>{name}</button></React.Fragment>)}.</span> : null}
    {mixed.length && onCurrent ? <span className="tl1-mixed"> {mixed.length === 1 ? `${mixed[0]} ran` : `${mixed.length} flavors ran`} more than one definition in this window, so {mixed.length === 1 ? "its" : "their"} numbers blend revisions.{" "}
      <button type="button" className="tl1-link" title={mixed.join(", ")} onClick={onCurrent}>Show current definitions only</button></span> : null}
  </p>;
}

export function TL1Page({ attempts, filterAttempts, copy }: Props) {
  // TL1 runs as one installation per Mac, so a project may have several.
  const [projects, setProjects] = useState<Row[]>([]);
  const [project, setProject] = useState(() => localStorage.getItem("tl1-project") ?? "");
  // One Mac's installation, or "" for every Mac.
  const [installation, setInstallation] = useState("");
  const [timeWindow, setTimeWindowState] = useState<TimeWindow>(readWindow);
  const [enqueues, setEnqueues] = useState<Row[]>([]);
  const setTimeWindow = useCallback((next: TimeWindow) => {
    setTimeWindowState(next);
    try { localStorage.setItem(windowStorage, JSON.stringify(next)); } catch { /* Storage may be disabled. */ }
  }, []);
  const [scope, setScope] = useState("all");
  const [overview, setOverview] = useState<Overview | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [flavor, setFlavor] = useState("");
  const [candidate, setCandidate] = useState("");
  const [showAll, setShowAll] = useState(false);
  const runsRef = useRef<HTMLDivElement>(null);

  const loadInstallations = useCallback(async () => {
    try {
      const body = await getJSON<{ projects?: Row[] }>("/api/tl1");
      const found = Array.isArray(body.projects) ? body.projects : [];
      setProjects(found);
      const tab = document.getElementById("tl1Tab");
      if (tab) tab.hidden = found.length === 0;
    } catch { /* The tab stays hidden when TL1 data is unavailable. */ }
  }, []);
  useEffect(() => {
    void loadInstallations();
    window.addEventListener("alexandria:route", loadInstallations);
    return () => window.removeEventListener("alexandria:route", loadInstallations);
  }, [loadInstallations]);

  const current = projects.find(item => item.project === project) ?? projects[0];
  const selected = String(current?.project ?? "");
  const macs: Row[] = current?.installations ?? [];
  const mac = macs.length > 1 && macs.some(item => item.installation_id === installation) ? installation : "";
  const query = useMemo(() => new URLSearchParams({ project: selected, ...(mac ? { installation: mac } : {}), ...windowParams(timeWindow, enqueues), scope }).toString(), [selected, mac, timeWindow, enqueues, scope]);

  const load = useCallback((signal?: AbortSignal) => {
    if (!selected) return;
    setLoading(true);
    getJSON<Overview>(`/api/tl1/overview?${query}`, signal)
      .then(body => {
        setOverview(body);
        setError("");
        // Enqueues are listed regardless of the window; keep the list stable
        // so a segment's end (the next enqueue) resolves without a refetch.
        const found = Array.isArray(body.enqueues) ? body.enqueues : [];
        setEnqueues(previous => JSON.stringify(previous) === JSON.stringify(found) ? previous : found);
      })
      .catch(failure => { if (!signal?.aborted) setError(failure.message); })
      .finally(() => { if (!signal?.aborted) setLoading(false); });
  }, [query, selected]);
  useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  // [ and ] step to an older or newer enqueue while the TL1 tab is open.
  useEffect(() => {
    const step = (event: KeyboardEvent) => {
      if ((event.key !== "[" && event.key !== "]") || event.metaKey || event.ctrlKey || event.altKey || typing(event.target)) return;
      // offsetParent is null while the TL1 view is hidden.
      if (!runsRef.current?.offsetParent || !enqueues.length) return;
      const since = timeWindow.mode === "latest" ? enqueues[0].started_at : timeWindow.mode === "enqueue" ? timeWindow.since : "";
      const current = enqueues.findIndex(item => item.started_at === since);
      const next = current < 0 ? 0 : Math.max(0, Math.min(enqueues.length - 1, current + (event.key === "[" ? 1 : -1)));
      const segment = timeWindow.mode === "enqueue" && timeWindow.segment;
      setTimeWindow(next === 0 && !segment ? { mode: "latest" } : { mode: "enqueue", since: enqueues[next].started_at, segment });
      event.preventDefault();
    };
    window.addEventListener("keydown", step);
    return () => window.removeEventListener("keydown", step);
  }, [enqueues, timeWindow, setTimeWindow]);

  const windowWhere = useCallback((): Where => {
    const resolved = overview?.window ?? {};
    const where: Where = [];
    if (resolved.since) where.push({ field: "task_created_at", op: ">=", value: resolved.since });
    if (resolved.until) where.push({ field: "task_created_at", op: "<", value: resolved.until });
    return where;
  }, [overview]);

  const projectWhere = useCallback((): Where => {
    if (mac) return [{ field: "installation_id", op: "=", value: mac }];
    return selected ? [{ field: "project", op: "=", value: selected }] : [];
  }, [selected, mac]);

  const showRuns = useCallback((filter: Row) => {
    filterAttempts([...Object.entries(filter).map(([field, value]) => ({ field, op: "=", value })), ...windowWhere(), ...projectWhere()]);
    runsRef.current?.scrollIntoView({ behavior: "smooth", block: "start" });
  }, [filterAttempts, windowWhere, projectWhere]);

  if (!projects.length) return <div className="tl1-page"><div className="view-heading"><div><h1>TL1</h1><p className="muted">No TL1 installations are indexed. Enable the tl1 source in Settings and sync it.</p></div></div></div>;

  const totals = overview?.totals ?? {};
  const coverage = overview?.coverage ?? {};
  const detectors = overview?.detectors ?? [];
  const visibleDetectors = showAll ? detectors : detectors.slice(0, 8);
  const flavors: Row[] = (overview?.flavors ?? []).filter(row => row.cost_usd > 0).slice(0, 14).map(row => ({ ...row, __value: row.cost_usd }));
  const edges: Row[] = (overview?.graph?.edges ?? []).slice(0, 20).map(row => ({ ...row, __value: row.count }));
  const human = overview?.human ?? {};
  const causes = (human.causes ?? []).map((row: Counted) => ({ ...row, __value: row.count }));
  const candidates = overview?.candidates ?? {};
  const reviews = overview?.reviews ?? {};

  return <div className="tl1-page">
    <div className="view-heading">
      <div><h1>TL1</h1><p className="muted">How each flavor performs on each agent configuration: cost, time, tokens, errors, and human attention. Concerns and opportunities come with prompts you can hand to an agent. Cost is the API list-price equivalent from transcripts.</p></div>
      <CopyButton text={overview?.review_prompt ?? ""} label="Copy optimization review prompt" copy={copy} primary />
    </div>
    <div className="tl1-controls" role="group" aria-label="TL1 filters">
      <label>Project <select value={selected} onChange={event => {
        setProject(event.target.value);
        setInstallation("");
        localStorage.setItem("tl1-project", event.target.value);
        // A chosen enqueue belongs to the previous project.
        setEnqueues([]);
        if (timeWindow.mode === "enqueue") setTimeWindow({ mode: "latest" });
      }}>
        {projects.map(item => <option key={item.project} value={item.project}>{item.project} · {count(item.tasks)} tasks{item.installations?.length > 1 ? ` · ${item.installations.length} Macs` : ""}</option>)}
      </select></label>
      {macs.length > 1 ? <label>Mac <select value={mac} onChange={event => {
        setInstallation(event.target.value);
        setEnqueues([]);
        if (timeWindow.mode === "enqueue") setTimeWindow({ mode: "latest" });
      }}>
        <option value="">All {macs.length} Macs</option>
        {macs.map(item => <option key={item.installation_id} value={item.installation_id}>{item.host_label || "Unknown Mac"} · {count(item.tasks)} tasks</option>)}
      </select></label> : null}
      <div className="tl1-segmented" role="group" aria-label="Flavor definitions" title="Current keeps only runs of each flavor's current definition (shape version)">
        <button type="button" aria-pressed={scope === "all"} onClick={() => setScope("all")}>All definitions</button>
        <button type="button" aria-pressed={scope === "current"} onClick={() => setScope("current")}>Current definitions</button>
      </div>
      <button type="button" className="tl1-button" onClick={() => { void loadInstallations(); load(); }}>{loading ? "Loading…" : "Refresh"}</button>
    </div>
    <WindowControls value={timeWindow} onChange={setTimeWindow} enqueues={enqueues} resolved={overview?.window} />
    {error ? <div className="query-table-error" role="alert">{error}</div> : null}
    {overview ? <>
      <WindowSummary resolved={overview.window ?? {}} enqueues={enqueues} onFlavor={setFlavor} onCurrent={scope === "all" ? () => setScope("current") : undefined} />
      <p className="tl1-coverage muted" title={(coverage.by_executor ?? []).map((row: Row) => `${row.executor}: ${row.with_transcript}/${row.attempts} with transcript, ${row.priced_from_tokens} priced`).join("\n")}>
        Transcripts found for {pct(coverage.transcript_share)} of LLM runs; cost known for {pct(coverage.cost_share)}. {synced(overview.installations)}.
      </p>
      <div className="tl1-tiles">
        <Tile label="Spend" value={usd(totals.cost_usd)} detail={`${count(totals.llm_attempts)} LLM runs · ${count(totals.procedural_attempts)} procedural`} />
        <Tile label="Per finished candidate" value={usd(totals.cost_per_finished_candidate_usd)} detail={`${count(totals.finished_candidates)} of ${count(totals.candidates)} finished`} />
        <Tile label="LLM runs ending in error" value={pct(totals.llm_error_rate)} detail={`${pct(totals.agent_error_rate)} excluding infrastructure`} />
        <Tile label="Human tasks" value={count(totals.human_tasks)} detail={`${count(totals.pending_human)} waiting · ${Number(totals.human_tasks_per_candidate ?? 0).toFixed(1)} per candidate`} />
        <Tile label="Median steps per candidate" value={count(totals.median_steps)} detail={`median LLM run ${duration(totals.median_llm_duration_ms)}`} />
        <Tile label="Input read from cache" value={pct(totals.cache_read_share)} detail={`${compact(totals.tokens?.total_tokens)} tokens`} />
      </div>

      <Section id="concerns" title="Concerns" description="Ranked by severity, then by the number of tasks affected. Each prompt includes the evidence and the flavor's definition.">
        {detectors.length ? <div className="tl1-findings">{visibleDetectors.map(item => <Finding key={item.id} item={item} copy={copy} onFlavor={setFlavor} onRuns={showRuns} />)}</div> : <p className="muted">No concerns in this window.</p>}
        {detectors.length > 8 ? <button type="button" className="tl1-button" onClick={() => setShowAll(value => !value)}>{showAll ? "Show fewer" : `Show all ${detectors.length}`}</button> : null}
      </Section>

      {overview.opportunities?.length ? <Section id="opportunities" title="Opportunities" description="Estimated from this window's runs. Configuration switches require at least 10 runs on each side and no worse error or escalation rate.">
        <div className="tl1-findings">{overview.opportunities.slice(0, 6).map(item => <Finding key={item.id} item={item} copy={copy} onFlavor={setFlavor} onRuns={showRuns} />)}</div>
      </Section> : null}

      <Section id="matrix" title="Flavor × agent configuration" description="Advanced: moved the workflow on without error or a human. Agent errors exclude infrastructure failures. Cost per useful result is total spend divided by advanced runs; rows with fewer than 10 runs are not compared.">
        {overview.matrix?.length ? <ConfigurationTable rows={overview.matrix} onFlavor={setFlavor} showFlavor /> : <p className="muted">No LLM runs in this window.</p>}
      </Section>

      <div className="tl1-two">
        <Section id="spend" title="Spend by flavor" description="Select a flavor for its definition, history, and tuning prompt.">
          <BarList rows={flavors} max={Math.max(...flavors.map(row => row.cost_usd), 0)} render={row => ({
            label: row.name, value: usd(row.cost_usd), onClick: () => setFlavor(row.name),
            title: `${row.name}: ${usd(row.cost_usd)} over ${row.tasks} tasks (median ${usd(row.median_task_cost_usd)})\nErrors ${pct(row.error_rate)} · escalated ${pct(row.escalation_rate)} · repeat visits ${pct(row.revisit_rate)}`,
          })} />
        </Section>
        <Section id="transitions" title="Workflow transitions" description="Most frequent handoffs: predecessor, its outcome, and the successor TL1 created.">
          <div className="tl1-legend"><span><span className="tl1-swatch accent" />Normal handoff</span><span><span className="tl1-swatch bad" />After an error</span></div>
          <BarList wide rows={edges} max={Math.max(...edges.map(row => row.count), 0)} segments={row => [{ share: row.count - row.error_count, tone: "accent" }, { share: row.error_count, tone: "bad" }]} render={row => ({
            label: <>{row.from} <span className="muted">—{row.outcome}→</span> {row.to}</>, value: count(row.count),
            title: `${row.from} —${row.outcome}→ ${row.to}: ${row.count} tasks${row.error_count ? `, ${row.error_count} after an error` : ""}`,
          })} />
        </Section>
      </div>

      <Section id="errors" title="Error clusters" description="Failures grouped by normalized signature. Attribution says who can fix it: configuration, output contract, script, infrastructure, provider, or the agent.">
        {overview.errors?.length ? <div className="tl1-table-wrap"><table className="tl1-table">
          <thead><tr><th className="num">Tasks</th><th>Signature</th><th>Attribution</th><th>Flavors</th><th>Configurations</th><th className="num">Human follow-ups</th><th className="num">Spend</th><th>Last seen</th><th /></tr></thead>
          <tbody>{overview.errors.slice(0, 20).map(cluster => <tr key={`${cluster.class}:${cluster.signature}`}>
            <td className="num">{count(cluster.count)}</td><td className="tl1-signature">{cluster.signature}</td><td>{String(cluster.attribution).replace("_", " ")}</td>
            <td>{cluster.flavors.map((item: Counted) => `${item.key} (${item.count})`).join(", ")}</td>
            <td>{cluster.configurations.map((item: Counted) => `${item.key} (${item.count})`).join(", ") || "—"}</td>
            <td className="num">{count(cluster.human_followups)}</td><td className="num">{cluster.cost_usd >= 0.01 ? usd(cluster.cost_usd) : "—"}</td><td>{when(cluster.last_seen)}</td>
            <td><button type="button" className="tl1-button small" onClick={() => showRuns({ error_signature: cluster.signature })}>Runs</button></td>
          </tr>)}</tbody>
        </table></div> : <p className="muted">No errors in this window.</p>}
      </Section>

      <div className="tl1-two">
        <Section id="human" title="Human attention" description={`${count(human.tasks)} human tasks, ${count(human.pending)} waiting${human.oldest_pending_at ? ` (oldest since ${when(human.oldest_pending_at)})` : ""}. ${pct(human.error_caused_share)} exist because an automated step errored.`}>
          <h3>What leads to a human task</h3>
          <BarList wide rows={causes} max={Math.max(...causes.map((row: Row) => row.count), 0)} render={row => ({ label: row.key, value: count(row.count), title: `${row.key}: ${row.count} human tasks` })} />
          {human.pending_by_flavor?.length ? <><h3>Waiting now</h3><ul className="tl1-list">{human.pending_by_flavor.map((item: Counted) => <li key={item.key}><span>{item.key}</span><span className="num">{count(item.count)}</span></li>)}</ul></> : null}
        </Section>
        <Section id="reviews" title="Review findings" description={`${count(reviews.total)} findings (${(reviews.severities ?? []).map((item: Counted) => `${item.count} ${item.key}`).join(", ") || "none"}), attributed to the configuration that last changed the candidate's code.`}>
          {reviews.by_implementation?.length ? <div className="tl1-table-wrap"><table className="tl1-table"><thead><tr><th>Implemented by</th><th className="num">Runs</th><th className="num">Findings per run</th><th className="num">Major per run</th></tr></thead>
            <tbody>{reviews.by_implementation.map((row: Row) => <tr key={`${row.flavor}:${row.configuration}`}><td>{row.flavor} · {row.configuration}</td><td className="num">{count(row.implementation_runs)}</td><td className="num">{Number(row.findings_per_run ?? 0).toFixed(2)}</td><td className="num">{Number(row.major_per_run ?? 0).toFixed(2)}</td></tr>)}</tbody></table></div> : <p className="muted">No findings could be attributed.</p>}
          {reviews.top_files?.length ? <><h3>Most-flagged files</h3><ul className="tl1-list">{reviews.top_files.map((item: Counted) => <li key={item.key}><code>{item.key}</code><span className="num">{count(item.count)}</span></li>)}</ul></> : null}
        </Section>
      </div>

      <Section id="candidates" title="Candidates" description={`${count(candidates.count)} candidates (${(candidates.statuses ?? []).map((item: Counted) => `${item.count} ${item.key}`).join(", ")}); median ${count(candidates.median_steps)} steps, 90th percentile ${count(candidates.p90_steps)}. Select one for its timeline.`}>
        <div className="tl1-table-wrap"><table className="tl1-table">
          <thead><tr><th>Most expensive</th><th>Status</th><th className="num">Spend</th><th className="num">Tasks</th><th className="num">Steps</th><th className="num">Human tasks</th><th className="num">Errors</th><th>Started</th></tr></thead>
          <tbody>{(candidates.most_expensive ?? []).map((row: Row) => <tr key={row.candidate_id}>
            <td><button type="button" className="tl1-link" onClick={() => setCandidate(row.candidate_id)}>{row.title || row.candidate_id}</button></td><td>{row.status}</td>
            <td className="num">{usd(row.cost_usd)}</td><td className="num">{count(row.tasks)}</td><td className="num">{count(row.workflow_steps)}/{count(row.workflow_step_limit)}</td>
            <td className="num">{count(row.human_tasks)}</td><td className="num">{count(row.errors)}</td><td>{when(row.created_at)}</td>
          </tr>)}</tbody>
        </table></div>
      </Section>
    </> : !error ? <p className="muted">Loading TL1 analysis…</p> : null}

    <div ref={runsRef}><Section id="runs" title="Runs" description="Every TL1 attempt with its flavor, configuration, result, cost, and error. Filter, group, and add metrics like any Pharos table." actions={<div className="tl1-actions">
      <button type="button" className="tl1-button" title="Filter to runs of tasks created in the analysis window" onClick={() => filterAttempts([...windowWhere(), ...projectWhere()])}>Show runs in window</button>
      <button type="button" className="tl1-button" onClick={() => filterAttempts([])}>Clear filters</button>
    </div>}>
      {attempts}
    </Section></div>

    {flavor ? <FlavorDialog query={query} name={flavor} onClose={() => setFlavor("")} copy={copy} onRuns={showRuns} /> : null}
    {candidate ? <CandidateDialog query={query} id={candidate} onClose={() => setCandidate("")} /> : null}
  </div>;
}
