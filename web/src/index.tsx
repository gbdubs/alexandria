import React, { FormEvent, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { decodeQuery, encodeQuery, loadSchema, localStorageAdapter, toAggregationQuery, type AggregationClause, type FieldSchema, type OrderByClause, type ServerQuery, type Transport, type WhereTerm } from "@pythia-software/query-table-core";
import { useQueryTable, type QueryTableApi } from "@pythia-software/query-table-react";
import { DataTable, FilterValueProvider, MetricsPanel, QueryBuilder, defaultRenderers, type CellContext, type FilterValuePresentation, type RenderRegistry } from "@pythia-software/query-table-ui";
import "@pythia-software/query-table-ui/theme.css";
import "./alexandria.css";
import libraryDocument from "../../schemas/library.schema.json";
import usageDocument from "../../schemas/usage.schema.json";
import writingDocument from "../../schemas/writing.schema.json";
import mcpCallsDocument from "../../schemas/mcp_calls.schema.json";
import toolsDocument from "../../schemas/tools.schema.json";
import toolCallsDocument from "../../schemas/tool_calls.schema.json";
import tl1AttemptsDocument from "../../schemas/tl1_attempts.schema.json";
import { TL1Page } from "./tl1";
import { Icon } from "./icons";

type Row = Record<string, any>;
type Dataset = "library" | "usage" | "writing" | "mcp_calls" | "tools" | "tool_calls" | "tl1_attempts";
type LibraryView = "table" | "conversation";
type TranscriptMessage = { role: string; text: unknown; raw_text?: unknown };
type TurnSummary = { human: TranscriptMessage | null; response: TranscriptMessage | null };
type ConversationTurn = { provider: string; conversation: number; turn: number; input: TranscriptMessage | null; response: TranscriptMessage | null };
type PullRequest = { number: number; title?: string | null; url?: string | null; host?: string | null };
// Messages render with the conversation view's own renderer (ui.py), so XML
// chips, Markdown, and Show more behave the same in both places.
function ResultMessage({ label, message, kind }: { label: string; message: TranscriptMessage; kind: "human" | "assistant" }) {
  const body = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const card = window.alexandriaMessageCard?.(message);
    if (card) body.current?.replaceChildren(card);
    else if (body.current) body.current.textContent = String(message.text ?? "");
  }, [message.role, message.text, message.raw_text]);
  return <div className={`result-message ${kind}`}>
    <div className="result-message-label">{label}</div>
    <div className="result-message-body" ref={body} />
  </div>;
}

function githubPullURL(remote: unknown, number: number): string {
  const match = String(remote ?? "").match(/github\.com(?::|\/)([^/\s]+\/[^/\s]+?)(?:\.git)?$/i);
  return match ? `https://github.com/${match[1]}/pull/${number}` : "";
}

function pullRequests(row: Row): PullRequest[] {
  try {
    const details = typeof row.pr_details === "string" ? JSON.parse(row.pr_details) : row.pr_details;
    return Array.isArray(details) ? details.filter(pr => Number.isInteger(pr?.number) && pr.number > 0) : [];
  } catch { return []; }
}

function pullRequestURL(pr: PullRequest, remote: unknown): string {
  const url = pr.url || (pr.host === "github.com" ? githubPullURL(remote, pr.number) : "");
  return /^https?:\/\//i.test(url ?? "") ? String(url) : "";
}

function mainIntegration(row: Row): React.ReactNode {
  if (!row.main_merge_commit) return "";
  const prefix = row.main_merge_method === "merge" ? "Merged" : "On main";
  const label = `${prefix}: ${row.main_merge_title || String(row.main_merge_commit).slice(0, 12)}`;
  const url = /^https:\/\/github\.com\//i.test(String(row.main_merge_url ?? "")) ? String(row.main_merge_url) : "";
  return url ? <a className="qt-pr-link" href={url} target="_blank" rel="noopener noreferrer" title={String(row.main_merge_commit)} onClick={event => event.stopPropagation()}>{label}</a>
    : <span title={String(row.main_merge_commit)}>{label}</span>;
}

function formatUSD(amount: number): string {
  if (!Number.isFinite(amount)) return "";
  if (Math.abs(amount) >= 1e6) return `$${compactNumber(amount)}`;
  const digits = Math.abs(amount) >= 100 ? 0 : Math.abs(amount) >= 1 ? 2 : 4;
  return amount.toLocaleString(undefined, { style: "currency", currency: "USD", minimumFractionDigits: Math.min(digits, 2), maximumFractionDigits: digits });
}

function usdCost(value: unknown, row: Row): React.ReactNode {
  if (value === null || value === undefined || value === "") return <span className="muted" title="No confirmed API price for this model and date">—</span>;
  const amount = Number(value);
  const text = formatUSD(amount);
  if (row.price_status === "partial") return <span title="Some tokens had no confirmed rate, so this is a lower bound">{text}+</span>;
  if (row.price_status === "assumed") return <span title="Priced by assumption: no published API price, so a sibling model's price is used (see pricing/cost_changes.json)">≈{text}</span>;
  return <span>{text}</span>;
}

const queryParameter = (dataset: Dataset) => `q_${dataset}`;

function updateURI(name: string, value: string) {
  const url = new URL(window.location.href);
  if (value) url.searchParams.set(name, value);
  else url.searchParams.delete(name);
  if (url.href !== window.location.href) {
    history.replaceState(null, "", url);
    window.dispatchEvent(new Event("alexandria:uri-changed"));
  }
}

declare global {
  interface Window {
    alexandriaOpenDetail?: (id: string) => void;
    alexandriaCopyText?: (text: string) => Promise<void>;
    alexandriaMessageCard?: (message: TranscriptMessage) => HTMLElement;
    alexandriaTurnSummaries?: (conversation: Row) => TurnSummary[];
    pharosCarbon?: { refresh: () => Promise<void> };
    alexandriaQueryTables?: {
      refresh: (dataset: Dataset) => void;
      filterRepository: (repository: string) => void;
      filterModel: (model: string) => void;
    };
  }
}

const schemas: Record<Dataset, FieldSchema<Row>> = {
  library: loadSchema<Row>(libraryDocument),
  usage: loadSchema<Row>(usageDocument),
  writing: loadSchema<Row>(writingDocument),
  mcp_calls: loadSchema<Row>(mcpCallsDocument),
  tools: loadSchema<Row>(toolsDocument),
  tool_calls: loadSchema<Row>(toolCallsDocument),
  tl1_attempts: loadSchema<Row>(tl1AttemptsDocument),
};

const tableApis = new Map<Dataset, QueryTableApi<Row>>();
let clearLibrarySearch: (() => void) | undefined;

async function responseJSON<T>(response: Response): Promise<T> {
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `Request failed with HTTP ${response.status}`);
  return body as T;
}

function makeTransport(dataset: Dataset): Transport<Row> {
  const endpoint = (suffix = "") => {
    return `/api/query/${dataset}${suffix}`;
  };
  return {
    async fetchRows(query: ServerQuery, signal?: AbortSignal) {
      return responseJSON(await fetch(endpoint(), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(query),
        signal,
      }));
    },
    async fetchDistinctValues(query, signal) {
      const url = new URL(endpoint("/distinct"), window.location.origin);
      url.searchParams.set("field", query.field);
      url.searchParams.set("q", query.search);
      if (query.limit) url.searchParams.set("limit", String(query.limit));
      return responseJSON(await fetch(url.pathname + url.search, { signal }));
    },
    async fetchFieldStats(fields, signal) {
      const url = new URL(endpoint("/field-stats"), window.location.origin);
      url.searchParams.set("fields", fields.join(","));
      return responseJSON(await fetch(url.pathname + url.search, { signal }));
    },
    async fetchAggregations(query, signal) {
      return responseJSON(await fetch(endpoint("/aggregations"), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(query),
        signal,
      }));
    },
  };
}

const renderers: RenderRegistry<Row> = {
  ...defaultRenderers,
  bool_check(context: CellContext<Row>) {
    return context.value ? <Icon name="check" /> : defaultRenderers.bool_check(context);
  },
  number(context: CellContext<Row>) {
    const value = Number(context.value);
    if (/(^token_count$|_tokens$)/.test(context.field.name) && Number.isFinite(value) && Math.abs(value) >= 1e6)
      return <span title={value.toLocaleString()}>{compactNumber(value)}</span>;
    return defaultRenderers.number(context);
  },
  work_title({ value, row }: CellContext<Row>) {
    return <button className="qt-work-link" title={String(value ?? "")} onClick={(event) => {
      event.stopPropagation();
      window.alexandriaOpenDetail?.(String(row.workspace_id ?? row.id));
    }}>{String(value ?? "Untitled work")}</button>;
  },
  pull_requests({ value, row }: CellContext<Row>) {
    const prs = pullRequests(row);
    if (!prs.length) return String(value ?? "");
    return <span className="qt-pr-list">{prs.map((pr, index) => {
      const label = pr.title?.trim() ? `${pr.title.trim()} · #${pr.number}` : `PR #${pr.number}`;
      const url = pullRequestURL(pr, row.canonical_remote);
      return url ? <a key={`${pr.host}:${pr.number}:${index}`} className="qt-pr-link" href={url} target="_blank" rel="noopener noreferrer" title={label} onClick={event => event.stopPropagation()}>{label}</a>
        : <span key={`${pr.host}:${pr.number}:${index}`} title={label}>{label}</span>;
    })}</span>;
  },
  main_integration({ row }: CellContext<Row>) { return mainIntegration(row); },
  run_state({ value }: CellContext<Row>) {
    const state = String(value ?? "unknown");
    return <span className={`qt-state-pill ${state}`}>{state}</span>;
  },
  usd({ value, row }: CellContext<Row>) { return usdCost(value, row); },
  sync_progress({ value }: CellContext<Row>) {
    const progress = Math.max(0, Math.min(100, Number(value) || 0));
    return <span className="qt-progress"><span className="qt-progress-track"><span className="qt-progress-fill" style={{ width: `${progress}%` }} /></span><span>{Math.round(progress)}%</span></span>;
  },
  mcp_status({ value }: CellContext<Row>) {
    const state = String(value ?? "unknown");
    return <span className={`mcp-call-state ${state}`}>{state}</span>;
  },
  tool_status({ value }: CellContext<Row>) {
    const state = String(value ?? "unknown");
    return <span className={`mcp-call-state ${state}`}>{state.replace("_", " ")}</span>;
  },
};

// Filter chips, pickers, and cell quick filters show each value's schema label
// and, for status fields, the badge its cells use. Queries and URLs keep the
// raw value.
function optionLabels(fields: FieldSchema<Row>["fields"]): Map<string, Map<string, string>> {
  const labels = new Map<string, Map<string, string>>();
  for (const field of fields) {
    const values = field.filter?.values;
    if (values?.source !== "static") continue;
    const options = new Map<string, string>();
    for (const option of values.options) if (typeof option !== "string") options.set(option.value, option.label);
    if (options.size) labels.set(field.name, options);
  }
  return labels;
}

const statusBadges: Record<string, (value: string, label: string) => React.ReactNode> = {
  run_state: (value, label) => <span className={`qt-state-pill ${value}`}>{label}</span>,
  mcp_status: (value, label) => <span className={`mcp-call-state ${value}`}>{label}</span>,
  tool_status: (value, label) => <span className={`mcp-call-state ${value}`}>{label}</span>,
};

const filterPresentations = Object.fromEntries(Object.entries(schemas).map(([dataset, schema]) => {
  const labels = optionLabels(schema.fields);
  const badges = new Map(schema.fields.flatMap(field => typeof field.render === "string" && statusBadges[field.render] ? [[field.name, statusBadges[field.render]] as const] : []));
  const label = (field: string, value: string) => labels.get(field)?.get(value) ?? value;
  const presentation: FilterValuePresentation = {
    label,
    render: (field, value) => value ? badges.get(field)?.(value, label(field, value)) : undefined,
  };
  return [dataset, presentation];
})) as Record<Dataset, FilterValuePresentation>;

// A failed query clears its rows; say so rather than claiming nothing matched.
const failedMessage = "No results: the query failed (see the error above).";

const emptyMessages: Record<Dataset, string> = {
  library: "No work matches this query.",
  usage: "No token usage matches this query.",
  writing: "No conversations with user messages match this query.",
  mcp_calls: "No MCP calls match this query.",
  tools: "No tool use matches this query.",
  tool_calls: "No tool calls match this query.",
  tl1_attempts: "No TL1 runs match this query.",
};

// query-table-ui 0.3.0 formats metric values with a field's renderer only for
// its built-in unit keys (duration_ms, byte_size). Route dollar measures
// through the byte_size key and back to the usd renderer, so metric panels show
// "$1,234" instead of "1234.5678". Real byte_size fields are unaffected.
const usdMetricFields = new WeakSet<object>();
const tokenMetricFields = new WeakSet<object>();
const metricFieldCache = new WeakMap<object, FieldSchema<Row>["fields"]>();
function metricFields(fields: FieldSchema<Row>["fields"]): FieldSchema<Row>["fields"] {
  let mapped = metricFieldCache.get(fields);
  if (!mapped) {
    mapped = fields.map((field) => {
      if (field.render !== "usd" && !(field.render === "number" && /(^token_count$|_tokens$)/.test(field.name))) return field;
      const alias = { ...field, render: "byte_size" };
      if (field.render === "usd") usdMetricFields.add(alias);
      else tokenMetricFields.add(alias);
      return alias;
    });
    metricFieldCache.set(fields, mapped);
  }
  return mapped;
}
const metricRenderers: RenderRegistry<Row> = {
  ...renderers,
  byte_size(context: CellContext<Row>) {
    if (usdMetricFields.has(context.field as object)) return formatUSD(Number(context.value));
    if (tokenMetricFields.has(context.field as object)) return compactNumber(Number(context.value));
    return defaultRenderers.byte_size(context);
  },
};

// header renders between the filters and the metric panels, so a chart there
// can follow the table's filters.
function QuerySurface({ dataset, libraryView = "table", trailing, header }: { dataset: Dataset; libraryView?: LibraryView; trailing?: (row: Row, api: QueryTableApi<Row>) => React.ReactNode; header?: (api: QueryTableApi<Row>) => React.ReactNode }) {
  const schema = schemas[dataset];
  const transport = useMemo(() => makeTransport(dataset), [dataset]);
  const storage = useMemo(() => localStorageAdapter(), []);
  const api = useQueryTable<Row>({ schema, transport, storage, debounceMs: 100 });
  const apiRef = useRef(api);
  apiRef.current = api;
  // query-table 0.3.0's nullability effect depends on the controller object,
  // while useQueryTable returns a fresh facade as data arrives. A stable proxy
  // prevents an effect/request loop and still resolves every property live.
  const stableApi = useMemo(() => new Proxy({} as QueryTableApi<Row>, {
    get(_target, property) { return apiRef.current[property as keyof QueryTableApi<Row>]; },
  }), []);
  tableApis.set(dataset, api);
  useEffect(() => {
    return () => { tableApis.delete(dataset); };
  }, [dataset]);
  const pendingRouteQuery = useRef<string | null>(null);
  const firstRouteQuery = useRef(true);
  useEffect(() => {
    const restore = () => {
      const token = new URLSearchParams(window.location.search).get(queryParameter(dataset));
      if (token !== null) {
        const restored = decodeQuery(token);
        const canonicalToken = encodeQuery(restored);
        pendingRouteQuery.current = encodeQuery(apiRef.current.query) === canonicalToken ? null : canonicalToken;
        apiRef.current.setQuery(restored);
      } else if (!firstRouteQuery.current) {
        const defaultsToken = encodeQuery(apiRef.current.defaults);
        pendingRouteQuery.current = encodeQuery(apiRef.current.query) === defaultsToken ? null : defaultsToken;
        apiRef.current.setQuery(apiRef.current.defaults);
      }
      firstRouteQuery.current = false;
    };
    restore();
    window.addEventListener("alexandria:route", restore);
    return () => window.removeEventListener("alexandria:route", restore);
  }, [dataset]);
  const initialQuery = useRef(true);
  useEffect(() => {
    if (initialQuery.current) { initialQuery.current = false; if (pendingRouteQuery.current !== null) return; }
    const token = encodeQuery(api.query);
    if (pendingRouteQuery.current !== null && token !== pendingRouteQuery.current) return;
    pendingRouteQuery.current = null;
    updateURI(queryParameter(dataset), token);
  }, [dataset, api.query]);

  return <FilterValueProvider value={filterPresentations[dataset]}>
    {header?.(api)}
    <QueryBuilder api={stableApi} fields={schema.fields} total={api.total} running={api.loading} />
    <MetricsPanel aggregations={api.aggregations} fields={metricFields(schema.fields)} renderers={metricRenderers} />
    {api.error ? <div className="query-table-error">{api.error.message}</div> : null}
    {dataset === "library" && libraryView === "conversation" ? <ConversationResults api={api} /> : <DataTable
      maxHeight={100000}
      fields={api.visibleFields}
      rows={api.rows}
      query={api.query}
      onQueryChange={api.setQuery}
      renderers={renderers}
      rowId={api.rowId}
      columnDrag={api.columnDrag}
      total={api.total}
      loading={api.loading}
      emptyMessage={api.error ? failedMessage : emptyMessages[dataset]}
      {...(trailing ? { trailing: (row: Row) => trailing(row, api), trailingLabel: dataset === "mcp_calls" || dataset === "tool_calls" ? "View" : dataset === "tools" ? "Drill in" : "Actions" } : {})}
    />}
  </FilterValueProvider>;
}

function conversationTurns(work: Row): ConversationTurn[] {
  return (work.conversations ?? []).flatMap((conversation: Row, index: number) => {
    const provider = String(conversation.provider ?? "Agent");
    return (window.alexandriaTurnSummaries?.(conversation) ?? [])
      .filter(summary => summary.human)
      .map((summary, turn) => ({ provider, conversation: index + 1, turn: turn + 1, input: summary.human, response: summary.response }));
  });
}

function ResultConversationBrowser({ workspaceID }: { workspaceID: string }) {
  const [state, setState] = useState<{ loading: boolean; error: string; turns: ConversationTurn[] }>({ loading: true, error: "", turns: [] });
  const [active, setActive] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    void (async () => {
      try {
        const work = await responseJSON<Row>(await fetch(`/api/work/${encodeURIComponent(workspaceID)}`, { signal: controller.signal }));
        if (!controller.signal.aborted) setState({ loading: false, error: "", turns: conversationTurns(work) });
      } catch (error) {
        if (!controller.signal.aborted) setState({ loading: false, error: error instanceof Error ? error.message : "Conversation unavailable", turns: [] });
      }
    })();
    return () => controller.abort();
  }, [workspaceID]);
  if (state.loading) return <div className="conversation-result-status">Loading turns…</div>;
  if (state.error) return <div className="conversation-result-status badtext">{state.error}</div>;
  if (!state.turns.length) return <div className="conversation-result-status">No retained turns are available.</div>;
  const index = Math.min(active, state.turns.length - 1);
  const turn = state.turns[index];
  return <div className="result-turn-browser">
    <div className="result-turn-nav">
      <button type="button" disabled={index === 0} onClick={() => setActive(index - 1)} aria-label="Previous turn"><Icon name="arrow-left" /></button>
      <span>{turn.provider}{state.turns.some((item) => item.conversation !== turn.conversation) ? ` conversation ${turn.conversation}` : ""} · turn {turn.turn} of {state.turns.filter((item) => item.conversation === turn.conversation).length}</span>
      <button type="button" disabled={index === state.turns.length - 1} onClick={() => setActive(index + 1)} aria-label="Next turn"><Icon name="arrow-right" /></button>
    </div>
    <div className="result-exchange">
      <ResultMessage label="You" message={turn.input ?? { role: "user", text: "Input unavailable" }} kind="human" />
      <ResultMessage label={turn.provider} message={turn.response ?? { role: "assistant", text: "No retained response for this turn." }} kind="assistant" />
    </div>
  </div>;
}

function ConversationResultCard({ row }: { row: Row }) {
  const [browse, setBrowse] = useState(false);
  const turnCount = Math.max(0, Number(row.turn_count) || 0);
  const firstInput = row.first_input ? { role: "user", text: row.first_input } : row.purpose ? { role: "user", text: row.purpose } : null;
  const latestResponse = row.last_response ? { role: "assistant", text: row.last_response } : row.outcome ? { role: "assistant", text: row.outcome } : null;
  const toolCount = Math.max(0, Number(row.tool_use_count) || 0);
  const fileCount = Math.max(0, Number(row.changed_file_count) || 0);
  const models = String(row.models ?? "").split(",").map(model => model.trim()).filter(Boolean);
  const prs = pullRequests(row);
  return <article className="conversation-result-card">
    <div className="conversation-result-head">
      <div className="conversation-result-summary">
        <button className="conversation-result-title" type="button" onClick={() => window.alexandriaOpenDetail?.(String(row.id))}>{String(row.title ?? "Untitled work")}</button>
        <div className="conversation-result-meta">{[row.repository_name, row.source_kind, models.join(", "), row.activity_at ? new Date(row.activity_at).toLocaleDateString() : ""].filter(Boolean).join(" · ")}</div>
        <div className="conversation-result-facts">
          <span className="conversation-result-count">{turnCount} turn{turnCount === 1 ? "" : "s"}</span>
          <span className="conversation-result-count">{toolCount} tool use{toolCount === 1 ? "" : "s"}</span>
          <span className="conversation-result-count">{fileCount} file{fileCount === 1 ? "" : "s"} edited</span>
          <span className="conversation-result-count conversation-result-cost" title="API list-price equivalent cost">{usdCost(row.cost_usd, row)} API cost</span>
          {prs.map((pr, index) => {
            const url = pullRequestURL(pr, row.canonical_remote);
            const label = pr.title?.trim() ? `${pr.title.trim()} · #${pr.number}` : `PR #${pr.number}`;
            return url ? <a key={`${pr.host}:${pr.number}:${index}`} className="conversation-result-count conversation-result-pr" href={url} target="_blank" rel="noopener noreferrer" title={pr.title || label}>{label}</a>
              : <span key={`${pr.host}:${pr.number}:${index}`} className="conversation-result-count" title={pr.title || label}>{label}</span>;
          })}
          {row.main_merge_commit ? <span className="conversation-result-count conversation-result-pr">{mainIntegration(row)}</span> : null}
        </div>
      </div>
      <div className="conversation-result-actions">
        <button type="button" onClick={() => window.alexandriaOpenDetail?.(String(row.id))}>Open full conversation</button>
        <button type="button" onClick={() => setBrowse((value) => !value)} disabled={!turnCount} aria-expanded={browse}>{browse ? "Close turn browser" : "Browse turns"}</button>
      </div>
    </div>
    {firstInput || latestResponse ? <div className="conversation-result-preview">
      {firstInput ? <ResultMessage label="First ask" message={firstInput} kind="human" /> : null}
      {latestResponse ? <ResultMessage label="Latest response" message={latestResponse} kind="assistant" /> : null}
    </div> : <p className="muted conversation-result-empty">No retained message preview.</p>}
    {browse ? <ResultConversationBrowser workspaceID={String(row.id)} /> : null}
  </article>;
}

function ConversationResults({ api }: { api: QueryTableApi<Row> }) {
  if (!api.loading && !api.rows.length) return <div className="conversation-results-empty no-results">{api.error ? failedMessage : "No work matches this query."}</div>;
  return <div className={`conversation-results ${api.loading ? "loading" : ""}`} aria-busy={api.loading}>
    {api.loading && !api.rows.length ? <div className="conversation-results-empty">Loading conversations…</div> : null}
    {api.rows.map((row) => <ConversationResultCard key={String(api.rowId(row) ?? row.id)} row={row} />)}
  </div>;
}

type FindKind = "text" | "file" | "url";
type FindHit = { workspace_id: string; conversation_id?: string; title: string; provider?: string; started_at?: string; message_id?: string; role?: string; message_kind?: string; snippet?: string; path?: string; old_path?: string; url?: string; url_source?: string; tool_name?: string; tool_call_id?: string; attribution?: string; count: number };
type FindResult = { items: FindHit[]; total: number; limit: number; offset: number; limited?: boolean };

function openFindHit(hit: FindHit) {
  const params = new URLSearchParams();
  if (hit.conversation_id) params.set("conversation", hit.conversation_id);
  if (hit.message_id) params.set("message", hit.message_id);
  history.pushState(null, "", `/work/${encodeURIComponent(hit.workspace_id)}${params.size ? `?${params}` : ""}`);
  (window as unknown as { routeFromLocation?: () => void }).routeFromLocation?.();
}

function LibraryFindResults({ query, kind, fuzzy, caseSensitive, separators }: { query: string; kind: FindKind; fuzzy: boolean; caseSensitive: boolean; separators: boolean }) {
  const [offset, setOffset] = useState(0);
  const [result, setResult] = useState<FindResult | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  useEffect(() => { setOffset(0); }, [query, kind, fuzzy, caseSensitive, separators]);
  useEffect(() => {
    const controller = new AbortController();
    const params = new URLSearchParams({ q: query, kind, fuzzy: fuzzy ? "1" : "0", case: caseSensitive ? "1" : "0", separators: separators ? "1" : "0", offset: String(offset), limit: "50" });
    setLoading(true); setError("");
    void fetch(`/api/library/find?${params}`, { signal: controller.signal }).then(response => responseJSON<FindResult>(response)).then(value => {
      if (!controller.signal.aborted) setResult(value);
    }).catch(reason => { if (!controller.signal.aborted) setError(reason instanceof Error ? reason.message : "Search failed"); }).finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [query, kind, fuzzy, caseSensitive, separators, offset]);
  if (error) return <p className="query-table-error">{error}</p>;
  if (loading && !result) return <p className="muted">Searching conversations…</p>;
  return <section className="library-find-results" aria-busy={loading}>
    <p className="muted">{result?.limited ? "At least " : ""}{result?.total.toLocaleString() ?? 0} {kind === "text" ? "conversations" : kind === "file" ? "file changes" : "URL records"} found{loading ? " · updating…" : ""}{result?.limited ? " · Search reached its 5,000-record limit; narrow the query for complete results." : ""}</p>
    {!loading && !result?.items.length ? <p>No matches found.</p> : null}
    {result?.items.map((hit, index) => <article className="library-find-hit" key={`${hit.conversation_id}:${hit.message_id}:${hit.path}:${hit.url}:${index}`}>
      <div className="library-find-head"><button type="button" onClick={() => openFindHit(hit)}>{hit.title}</button><span>{[hit.provider, hit.started_at ? new Date(hit.started_at).toLocaleDateString() : ""].filter(Boolean).join(" · ")}</span></div>
      {kind === "text" ? <><p className="library-find-snippet">{hit.snippet}</p><button type="button" className="library-find-open" onClick={() => openFindHit(hit)}>Open {hit.role === "user" ? "user message" : hit.message_kind === "message" ? "agent response" : "agent thought"} · {hit.count} matching message{hit.count === 1 ? "" : "s"}</button></> : null}
      {kind === "file" ? <><p className="library-find-snippet">{hit.old_path ? `${hit.old_path} → ` : ""}{hit.path}{hit.count > 1 ? ` · ${hit.count} changes` : ""}</p><button type="button" className="library-find-open" onClick={() => openFindHit(hit)}>Open {hit.attribution === "workspace" ? "work · conversation unknown" : "conversation"}</button></> : null}
      {kind === "url" ? <><p className="library-find-snippet"><a href={hit.url} target="_blank" rel="noopener noreferrer">{hit.url}</a></p><p className="muted">{hit.tool_name} · {({ input: "Tool argument", command: "Shell command", result: "Opened page", search_result: "Search result link (may not have been opened)" } as Record<string,string>)[hit.url_source ?? ""] ?? hit.url_source}</p><button type="button" className="library-find-open" onClick={() => openFindHit(hit)}>Open tool call in conversation</button></> : null}
    </article>)}
    {result && result.total > result.limit ? <nav className="library-find-pages"><button type="button" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - result.limit))}>Previous</button><span>{offset + 1}–{Math.min(offset + result.limit, result.total)} of {result.total}</span><button type="button" disabled={offset + result.limit >= result.total} onClick={() => setOffset(offset + result.limit)}>Next</button></nav> : null}
  </section>;
}

function LibraryPage() {
  const read = () => new URLSearchParams(location.search);
  const [draft, setDraft] = useState(() => read().get("search") ?? "");
  const [search, setSearch] = useState(() => read().get("search") ?? "");
  const [kind, setKind] = useState<FindKind>(() => (read().get("kind") as FindKind) || "text");
  const [fuzzy, setFuzzy] = useState(() => read().get("fuzzy") !== "0");
  const [caseSensitive, setCaseSensitive] = useState(() => read().get("case") === "1");
  const [separators, setSeparators] = useState(() => read().get("separators") === "1");
  const [view, setView] = useState<LibraryView>(() => read().get("view") === "conversation" ? "conversation" : "table");
  useEffect(() => { clearLibrarySearch = () => { setDraft(""); setSearch(""); updateURI("search", ""); }; return () => { clearLibrarySearch = undefined; }; }, []);
  useEffect(() => { const restore = () => { const params = read(); const q = params.get("search") ?? ""; setDraft(q); setSearch(q); setKind((params.get("kind") as FindKind) || "text"); setFuzzy(params.get("fuzzy") !== "0"); setCaseSensitive(params.get("case") === "1"); setSeparators(params.get("separators") === "1"); setView(params.get("view") === "conversation" ? "conversation" : "table"); }; window.addEventListener("alexandria:route", restore); return () => window.removeEventListener("alexandria:route", restore); }, []);
  function submit(event: FormEvent) { event.preventDefault(); const next = draft.trim(); setSearch(next); updateURI("search", next); }
  function chooseView(next: LibraryView) { setView(next); updateURI("view", next === "conversation" ? next : ""); }
  return <div className="alexandria-query-page">
    <div className="view-heading library-heading"><div><h1>Find past work</h1></div>{!search ? <div className="library-view-toggle" role="group" aria-label="Library result view"><button type="button" className={view === "table" ? "active" : ""} aria-pressed={view === "table"} onClick={() => chooseView("table")}>Table</button><button type="button" className={view === "conversation" ? "active" : ""} aria-pressed={view === "conversation"} onClick={() => chooseView("conversation")}>Conversations</button></div> : null}</div>
    <form className="semantic-search" onSubmit={submit}><select aria-label="Search type" value={kind} onChange={event => { const next = event.target.value as FindKind; setKind(next); updateURI("kind", next === "text" ? "" : next); }}><option value="text">Conversation text</option><option value="file">Modified file</option><option value="url">Tool URL</option></select><input type="search" value={draft} onChange={event => setDraft(event.target.value)} placeholder={kind === "text" ? "Words or an exact phrase in quotes…" : kind === "file" ? "File path or name…" : "URL or host…"} aria-label="Search library" /><button type="submit">Search</button></form>
    {kind === "text" ? <div className="search-options" role="group" aria-label="Text search options"><label><input type="checkbox" checked={fuzzy} onChange={event => { setFuzzy(event.target.checked); updateURI("fuzzy", event.target.checked ? "" : "0"); }} /> Fuzzy words</label><label><input type="checkbox" checked={caseSensitive} onChange={event => { setCaseSensitive(event.target.checked); updateURI("case", event.target.checked ? "1" : ""); }} /> Case sensitive</label><label title="For quoted phrases, require punctuation and spaces exactly as typed"><input type="checkbox" checked={separators} onChange={event => { setSeparators(event.target.checked); updateURI("separators", event.target.checked ? "1" : ""); }} /> Match separators</label></div> : null}
    {search ? <LibraryFindResults query={search} kind={kind} fuzzy={fuzzy} caseSensitive={caseSensitive} separators={separators} /> : <QuerySurface dataset="library" libraryView={view} />}
  </div>;
}

type ChartPeriod = "day" | "week" | "month";
type ChartRange = "30d" | "90d" | "6m" | "1y" | "all";
type TokenSplit = "type" | "provider" | "model_family" | "repository_name" | "session_kind";
type TokenChartView = { metric: "tokens" | "cost"; split: TokenSplit; period: ChartPeriod; range: ChartRange };
// A preset either fills the metric panels or sets the chart above them.
type UsagePreset = { label: string; title: string; aggregations?: AggregationClause[]; chart?: Partial<TokenChartView>; orderBy?: OrderByClause[] };
const sum = (field: string, groupBy: string[], label: string): AggregationClause => ({ id: `${field}:${groupBy.join(",")}`, op: "sum", field, groupBy, label });
const costPresets: UsagePreset[] = [
  { label: "Total cost", title: "Total API-equivalent cost, per month, and by provider", aggregations: [
    sum("cost_usd", [], "Total cost"),
    sum("cost_usd", ["month"], "Cost per month"),
    sum("cost_usd", ["provider"], "Cost by provider"),
  ] },
  { label: "Daily cost", title: "Chart API-equivalent cost per day, split by provider", chart: { metric: "cost", split: "provider", period: "day" } },
  { label: "Weekly cost", title: "API-equivalent cost per week, by model and by repository", aggregations: [
    sum("cost_usd", ["week", "model_family"], "Cost per week by model"),
    sum("cost_usd", ["repository_name"], "Cost by repository"),
  ] },
  { label: "Cost by model", title: "Cost by model, with the cost of the same tokens at today's prices", aggregations: [
    sum("cost_usd", ["model_family"], "Cost by model"),
    sum("cost_today_usd", ["model_family"], "Same tokens at today's prices"),
  ] },
  { label: "Most expensive work", title: "Workspaces ranked by API-equivalent cost", aggregations: [sum("cost_usd", ["title"], "Cost by work")] },
  { label: "Price changes", title: "Cost as billed on the day versus the same usage at today's prices, per month", aggregations: [
    sum("cost_usd", ["month"], "Cost at the day's prices"),
    sum("cost_today_usd", ["month"], "Cost at today's prices"),
  ] },
  { label: "Subagent cost", title: "Chart root agent versus subagent cost per week", chart: { metric: "cost", split: "session_kind", period: "week" } },
  { label: "Price coverage", title: "Cost and tokens by price status: priced, assumed, partial, or unpriced", aggregations: [
    sum("cost_usd", ["price_status"], "Cost by price status"),
    sum("total_tokens", ["price_status"], "Tokens by price status"),
  ] },
];

const usagePresets: UsagePreset[] = [
  { label: "Daily by provider", title: "Chart total tokens per day, split by provider", chart: { metric: "tokens", split: "provider", period: "day" } },
  { label: "Weekly cache mix", title: "Chart uncached input, cache reads, cache writes, and output per week", chart: { metric: "tokens", split: "type", period: "week" } },
  { label: "Models", title: "Total tokens and cache reads by model and provider", aggregations: [
    sum("total_tokens", ["model_family", "provider"], "Tokens by model"),
    sum("cache_read_input_tokens", ["model_family"], "Cached input by model"),
    sum("uncached_input_tokens", ["model_family"], "Uncached input by model"),
  ] },
  { label: "Repositories", title: "Total tokens by repository, overall and per week", aggregations: [
    sum("total_tokens", ["repository_name"], "Tokens by repository"),
    sum("total_tokens", ["week", "repository_name"], "Tokens per week by repository"),
  ] },
  { label: "Subagents", title: "Chart root agent versus subagent tokens per week", chart: { metric: "tokens", split: "session_kind", period: "week" } },
];

const byDesc = (field: string): OrderByClause[] => [{ field, dir: "desc" }];
const writingPresets: UsagePreset[] = [
  { label: "Deepest conversations", title: "Conversations with the most words you typed", orderBy: byDesc("typed_words") },
  { label: "Most hands-on", title: "Conversations with the most messages you typed into", orderBy: byDesc("typed_turns") },
  { label: "Longest messages", title: "Conversations holding your longest single typed message", orderBy: byDesc("longest_typed_words") },
  { label: "Most pasted-in", title: "Conversations with the most text that looks pasted", orderBy: byDesc("pasted_words") },
  { label: "Most copied from agents", title: "Conversations with the most agent output or earlier messages copied in", orderBy: byDesc("copied_words") },
];
const writingMetricPresets: UsagePreset[] = [
  { label: "By repository", title: "Typed and pasted words by repository", aggregations: [sum("typed_words", ["repository_name"], "Typed words by repository"), sum("pasted_words", ["repository_name"], "Likely pasted words by repository")] },
  { label: "By provider", title: "Typed words and tokens by provider", aggregations: [sum("typed_words", ["providers"], "Typed words by provider"), sum("total_tokens", ["providers"], "Tokens by provider")] },
  { label: "By source", title: "Typed words by source app", aggregations: [sum("typed_words", ["source_kind"], "Typed words by source")] },
];

type PricingModel = { model: string; provider: string; tokens: number; first_day: string; last_day: string };
type PricingStatus = { prompt: string; unpriced_models: PricingModel[]; unpriced_tokens: number; confirmed_changes: number; proposed_changes: number };
const acknowledgedPricingKey = "pharos-pricing-acknowledged-models";

function acknowledgedModels(): Set<string> {
  try { return new Set(JSON.parse(localStorage.getItem(acknowledgedPricingKey) ?? "[]")); } catch { return new Set(); }
}

async function copyText(text: string) {
  try { await navigator.clipboard.writeText(text); return; } catch { /* WKWebView may deny the async clipboard; fall back below. */ }
  const area = document.createElement("textarea");
  area.value = text;
  area.style.position = "fixed";
  area.style.opacity = "0";
  document.body.append(area);
  area.select();
  const copied = document.execCommand("copy");
  area.remove();
  if (!copied) throw new Error("Clipboard unavailable");
}

// Highlights when usage includes a model with no confirmed price that the user
// hasn't handed to an agent yet. Copying the prompt acknowledges those models.
function RefreshPricesButton() {
  const [status, setStatus] = useState<PricingStatus | null>(null);
  const [acknowledged, setAcknowledged] = useState(acknowledgedModels);
  const [copied, setCopied] = useState<"" | "copied" | "failed">("");
  useEffect(() => {
    const controller = new AbortController();
    const load = () => void fetch("/api/pricing", { signal: controller.signal }).then(responseJSON<PricingStatus>).then(setStatus).catch(() => {});
    load();
    window.addEventListener("alexandria:usage-refresh", load);
    return () => { controller.abort(); window.removeEventListener("alexandria:usage-refresh", load); };
  }, []);
  const unpriced = status?.unpriced_models ?? [];
  const fresh = unpriced.filter((model) => !acknowledged.has(model.model));
  async function copy() {
    if (!status) return;
    try {
      await copyText(status.prompt);
      const next = new Set([...acknowledged, ...unpriced.map((model) => model.model)]);
      localStorage.setItem(acknowledgedPricingKey, JSON.stringify([...next]));
      setAcknowledged(next);
      setCopied("copied");
    } catch { setCopied("failed"); }
    window.setTimeout(() => setCopied(""), 2500);
  }
  if (!fresh.length) return null;
  return <div className="refresh-prices-notice" role="status">
    <button type="button" className="refresh-prices attention" title={`New unpriced models: ${fresh.map((model) => model.model).join(", ")}`} onClick={() => void copy()}>
      {copied === "copied" ? "Prompt copied" : copied === "failed" ? "Copy failed" : "Copy refresh prices prompt"}
      {!copied ? <span className="refresh-prices-count" aria-label={`${fresh.length} new unpriced models`}>{fresh.length}</span> : null}
    </button>
    <p>{fresh.length === 1 ? "A new model has" : `${fresh.length} new models have`} no price definition. Cost totals may be incomplete until pricing is updated.</p>
  </div>;
}

// Stacked column charts by day, week (Monday), or month over a chosen range.
// Keys match the usage dataset's day, week, and month fields.
const chartPeriods: Record<ChartPeriod, { unit: string }> = { day: { unit: "day" }, week: { unit: "week" }, month: { unit: "month" } };
const chartRanges: Array<[ChartRange, string]> = [["30d", "30 days"], ["90d", "90 days"], ["6m", "6 months"], ["1y", "1 year"], ["all", "All time"]];
const periodOptions: Array<[ChartPeriod, string]> = [["day", "Day"], ["week", "Week"], ["month", "Month"]];
type ChartSeries = { key: string; label: string; color: string };
// parts lists what a folded series (Other) holds in this bucket.
type ChartBucket = { key: string; date: Date; values: Record<string, number>; parts?: Record<string, Array<[string, number]>>; detail?: string };
const pad2 = (value: number) => String(value).padStart(2, "0");
const localDay = (date: Date) => `${date.getFullYear()}-${pad2(date.getMonth() + 1)}-${pad2(date.getDate())}`;

function periodStart(date: Date, period: ChartPeriod): Date {
  const start = new Date(date);
  start.setHours(12, 0, 0, 0);
  if (period === "week") start.setDate(start.getDate() - ((start.getDay() + 6) % 7));
  if (period === "month") start.setDate(1);
  return start;
}

function periodKey(date: Date, period: ChartPeriod): string {
  const day = localDay(periodStart(date, period));
  return period === "month" ? day.slice(0, 7) : day;
}

// parseDay reads a day key, or a month key as its first day.
function parseDay(day: string): Date {
  const [year, month, date] = day.split("-").map(Number);
  return new Date(year, (month || 1) - 1, date || 1, 12);
}

// earliestKey returns the first day, week, or month key among values, which
// "All time" starts from.
function earliestKey(values: unknown[]): Date | undefined {
  const keys = values.map(String).filter(value => /^\d{4}-\d{2}/.test(value)).sort();
  return keys.length ? parseDay(keys[0]) : undefined;
}

function rangeStart(range: ChartRange, earliest?: Date): Date {
  const start = new Date();
  start.setHours(12, 0, 0, 0);
  if (range === "30d") start.setDate(start.getDate() - 29);
  else if (range === "90d") start.setDate(start.getDate() - 89);
  else if (range === "6m") { start.setMonth(start.getMonth() - 6); start.setDate(start.getDate() + 1); }
  else if (range === "1y") { start.setFullYear(start.getFullYear() - 1); start.setDate(start.getDate() + 1); }
  else return earliest && earliest < start ? earliest : start;
  return start;
}

function periodBuckets(period: ChartPeriod, range: ChartRange, earliest?: Date): ChartBucket[] {
  const end = periodStart(new Date(), period), buckets: ChartBucket[] = [];
  for (let date = periodStart(rangeStart(range, earliest), period); date <= end && buckets.length < 5000;) {
    buckets.push({ key: periodKey(date, period), date: new Date(date), values: {} });
    if (period === "day") date.setDate(date.getDate() + 1);
    else if (period === "week") date.setDate(date.getDate() + 7);
    else date.setMonth(date.getMonth() + 1);
  }
  return buckets;
}

// niceTicks places two or three gridlines at round values up to peak.
function niceTicks(peak: number): number[] {
  if (!(peak > 0)) return [];
  const raw = peak / 2.5, magnitude = 10 ** Math.floor(Math.log10(raw)), normal = raw / magnitude;
  const step = (normal <= 1 ? 1 : normal <= 2 ? 2 : normal <= 5 ? 5 : 10) * magnitude, ticks: number[] = [];
  for (let index = 1; index * step <= peak * 1.0001 && index < 10; index++) ticks.push(index * step);
  return ticks;
}

function compactNumber(value: number): string {
  const n = Number(value) || 0, abs = Math.abs(n);
  if (abs >= 1e6) {
    const units = [[1e6, "M"], [1e9, "B"], [1e12, "T"]] as const;
    let index = abs >= 1e12 ? 2 : abs >= 1e9 ? 1 : 0;
    let scaled = n / units[index][0], digits = Math.abs(scaled) < 10 ? 1 : 0;
    if (index < 2 && Math.abs(Number(scaled.toFixed(digits))) >= 1000) {
      index++;
      scaled = n / units[index][0];
      digits = 1;
    }
    return `${scaled.toFixed(digits)}${units[index][1]}`;
  }
  if (abs >= 1e4) return `${Math.round(n / 1e3)}k`;
  return Math.round(n).toLocaleString();
}

function readStored<T extends object>(key: string, fallback: T, valid: (value: T) => boolean): T {
  try {
    const stored = { ...fallback, ...JSON.parse(localStorage.getItem(key) ?? "{}") };
    return valid(stored) ? stored : fallback;
  } catch { return fallback; }
}

function store(key: string, value: unknown) {
  try { localStorage.setItem(key, typeof value === "string" ? value : JSON.stringify(value)); } catch { /* storage may be unavailable */ }
}

function Segmented<T extends string>({ label, value, options, onChange }: { label: string; value: T; options: Array<[T, string, string?]>; onChange: (next: T) => void }) {
  return <div className="usage-presets chart-toggle" role="group" aria-label={label}>
    <span className="usage-preset-label">{label}</span>
    {options.map(([option, text, disabledReason]) => <button key={option} type="button" aria-pressed={value === option} disabled={Boolean(disabledReason)} title={disabledReason} onClick={() => onChange(option)}>{text}</button>)}
  </div>;
}

// A single series needs no legend unless the title does not name it (a split
// that happens to hold one value). Hovering a column shows a key of the series
// it holds, largest first, with what a folded series contains.
function StackedColumns({ title, controls, series, buckets, period, format, tickFormat = format, noun, loading, error, legend = false }: {
  title: string; controls: React.ReactNode; series: ChartSeries[]; buckets: ChartBucket[]; period: ChartPeriod;
  format: (value: number) => string; tickFormat?: (value: number) => string; noun: string; loading: boolean; error: string; legend?: boolean;
}) {
  const root = useRef<HTMLDivElement>(null), tipRef = useRef<HTMLDivElement>(null);
  const [tip, setTip] = useState<{ bucket: ChartBucket; x: number; width: number; top: number } | null>(null);
  const total = (bucket: ChartBucket) => series.reduce((sum, item) => sum + (bucket.values[item.key] ?? 0), 0);
  const peak = Math.max(0, ...buckets.map(total)), scale = peak || 1, unit = chartPeriods[period].unit, stacked = series.length > 1;
  const ticks = niceTicks(peak);
  const label = (date: Date) => period === "month" ? date.toLocaleDateString(undefined, { month: "short", year: "numeric" }) : date.toLocaleDateString(undefined, { month: "short", day: "numeric", ...(period === "day" && buckets.length > 120 ? { year: "2-digit" } : {}) });
  const heading = (bucket: ChartBucket) => `${period === "week" ? "Week of " : ""}${label(bucket.date)}`;
  const suffix = noun ? ` ${noun}` : "";
  const present = (bucket: ChartBucket) => series.filter(item => (bucket.values[item.key] ?? 0) > 0).sort((left, right) => bucket.values[right.key] - bucket.values[left.key]);
  const describe = (bucket: ChartBucket) => `${heading(bucket)}: `
    + (present(bucket).map(item => `${item.label} ${format(bucket.values[item.key])}`).join(", ") || "none")
    + (stacked ? `; ${format(total(bucket))}${suffix} in all` : suffix) + (bucket.detail ? `; ${bucket.detail}` : "");
  useLayoutEffect(() => {
    const element = tipRef.current, box = root.current;
    if (!tip || !element || !box) return;
    const width = element.offsetWidth, right = tip.x + tip.width / 2 + 10;
    element.style.left = `${right + width <= box.offsetWidth - 4 ? right : Math.max(0, tip.x - tip.width / 2 - 10 - width)}px`;
    element.style.top = `${Math.max(0, Math.min(tip.top, box.offsetHeight - element.offsetHeight - 4))}px`;
  }, [tip]);
  function show(event: React.SyntheticEvent<HTMLElement>, bucket: ChartBucket) {
    const box = root.current?.getBoundingClientRect(), at = event.currentTarget.getBoundingClientRect(), plot = event.currentTarget.parentElement?.getBoundingClientRect();
    if (box && plot) setTip({ bucket, x: at.left - box.left + at.width / 2, width: at.width, top: plot.top - box.top });
  }
  const density = buckets.length > 300 ? "denser" : buckets.length > 120 ? "dense" : "";
  const middle = buckets[Math.floor(buckets.length / 2)];
  return <div className="usage-chart" ref={root}>
    <div className="usage-chart-head"><h3>{title}</h3><div className="usage-chart-controls">{controls}</div></div>
    {stacked || legend ? <div className="usage-chart-legend">{series.map(item => <span key={item.key}><span className="usage-swatch" style={{ background: item.color }} />{item.label}</span>)}</div> : null}
    {error ? <p className="query-table-error">{error}</p> : null}
    <div className="usage-chart-plot">
      {ticks.map(tick => <div key={tick} className="usage-chart-grid" style={{ bottom: `${100 * tick / scale}%` }} aria-hidden="true"><span>{tickFormat(tick)}</span></div>)}
      <div className={`usage-chart-bars ${density} ${loading ? "loading" : ""}`} role="list" aria-busy={loading} aria-label={title}>
        {buckets.map(bucket => {
          const parts = series.filter(item => (bucket.values[item.key] ?? 0) > 0 && (!stacked || 100 * bucket.values[item.key] / scale >= 0.5));
          return <span key={bucket.key} role="listitem" tabIndex={0} aria-label={describe(bucket)} className={tip?.bucket.key === bucket.key ? "active" : ""} onMouseEnter={event => show(event, bucket)} onFocus={event => show(event, bucket)} onMouseLeave={() => setTip(null)} onBlur={() => setTip(null)}>
            {parts.map((item, index) => {
              const share = 100 * bucket.values[item.key] / scale;
              return <i key={item.key} className={index === parts.length - 1 ? "top" : ""} style={{ height: `${stacked ? share : Math.max(1.5, share)}%`, background: item.color }} />;
            })}
          </span>;
        })}
      </div>
    </div>
    <div className="usage-chart-axis"><span>{buckets.length ? label(buckets[0].date) : ""}</span><span>{middle && buckets.length > 4 ? label(middle.date) : ""}</span><span>{period === "day" ? "today" : `this ${unit}`}</span></div>
    {tip ? <div className="usage-chart-tip" ref={tipRef} role="status">
      <div className="usage-chart-tip-head">{heading(tip.bucket)}</div>
      {present(tip.bucket).length ? <ul>{present(tip.bucket).map(item => {
        const parts = [...(tip.bucket.parts?.[item.key] ?? [])].sort((left, right) => right[1] - left[1]);
        return <li key={item.key}>
          <span className="usage-chart-tip-row"><span className="usage-swatch" style={{ background: item.color }} /><span className="usage-chart-tip-label">{item.label}</span><span className="usage-chart-tip-value">{format(tip.bucket.values[item.key])}</span></span>
          {parts.length ? <ul className="usage-chart-tip-parts">
            {parts.slice(0, 4).map(([name, value]) => <li key={name} className="usage-chart-tip-row"><span className="usage-chart-tip-label">{name}</span><span className="usage-chart-tip-value">{format(value)}</span></li>)}
            {parts.length > 4 ? <li className="usage-chart-tip-more">+{parts.length - 4} more</li> : null}
          </ul> : null}
        </li>;
      })}</ul> : <div className="usage-chart-tip-more">Nothing in this {unit}</div>}
      {stacked && present(tip.bucket).length > 1 ? <div className="usage-chart-tip-row usage-chart-tip-total"><span className="usage-chart-tip-label">Total</span><span className="usage-chart-tip-value">{format(total(tip.bucket))}{suffix}</span></div> : null}
      {tip.bucket.detail ? <div className="usage-chart-tip-more">{tip.bucket.detail}</div> : null}
    </div> : null}
  </div>;
}

type AggregationMetric = { id: string; buckets: Array<{ keys: unknown[]; value: unknown }> };

// Fetches aggregations for the given filters, refetching when either changes
// or Usage is refreshed. Results are tagged so a stale response never renders
// under new chart settings.
function useAggregations(dataset: Dataset, where: WhereTerm[], aggregations: AggregationClause[]) {
  const key = JSON.stringify({ where, aggregations });
  const [state, setState] = useState<{ key: string; metrics: AggregationMetric[]; error: string; loading: boolean }>({ key: "", metrics: [], error: "", loading: true });
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    const bump = () => setRefresh(value => value + 1);
    window.addEventListener("alexandria:usage-refresh", bump);
    return () => window.removeEventListener("alexandria:usage-refresh", bump);
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    setState(previous => ({ ...previous, loading: true }));
    fetch(`/api/query/${dataset}/aggregations`, { method: "POST", headers: { "Content-Type": "application/json" }, body: key, signal: controller.signal })
      .then(responseJSON<{ metrics: AggregationMetric[] }>)
      .then(result => setState({ key, metrics: result.metrics ?? [], error: "", loading: false }))
      .catch(failure => { if (!controller.signal.aborted) setState({ key, metrics: [], error: failure instanceof Error ? failure.message : String(failure), loading: false }); });
    return () => controller.abort();
  }, [dataset, key, refresh]);
  return { ...state, metrics: state.key === key ? state.metrics : [] };
}

// Token types stack from the bottom in this order; adjacent colors pass the
// palette validator's normal-vision and color-blindness checks in both themes.
const tokenTypes: ChartSeries[] = [
  { key: "uncached_input_tokens", label: "Uncached input", color: "var(--heat-3)" },
  { key: "cache_read_input_tokens", label: "Cache reads", color: "var(--heat-1)" },
  { key: "cache_creation_input_tokens", label: "Cache writes", color: "var(--gold)" },
  { key: "output_tokens", label: "Output", color: "var(--heat-2)" },
  { key: "unclassified_tokens", label: "Unclassified", color: "var(--muted)" },
];
const splitColors = ["var(--heat-2)", "var(--gold)", "var(--heat-0)", "var(--heat-3)", "var(--heat-1)"];
const tokenSplits: Array<[TokenSplit, string]> = [["type", "Token type"], ["provider", "Provider"], ["model_family", "Model"], ["repository_name", "Repository"], ["session_kind", "Agent"]];
// Providers and agent kinds keep one color each whatever the filters; models
// and repositories show the five largest in view and fold the rest into Other.
const fixedSplitOrder: Partial<Record<TokenSplit, string[]>> = { provider: ["claude", "codex", "tl1", "chatgpt", "canonical"], session_kind: ["root", "subagent"] };
const sessionKindLabels: Record<string, string> = { root: "Root agent", subagent: "Sub-agent" };
const tokenChartKey = "pharos-usage-chart";
const defaultTokenChart: TokenChartView = { metric: "tokens", split: "type", period: "week", range: "6m" };
const costTick = (value: number) => Math.abs(value) >= 1e4 ? `$${compactNumber(value)}` : `$${value.toLocaleString(undefined, { maximumFractionDigits: 2 })}`;

function splitLabel(split: TokenSplit, value: unknown): string {
  if (value === null || value === undefined || value === "") return split === "repository_name" ? "No repository" : "Unknown";
  const text = String(value);
  if (split === "provider") return optionLabels(schemas.usage.fields).get("provider")?.get(text) ?? text;
  if (split === "session_kind") return sessionKindLabels[text] ?? text;
  return text;
}

function TokenChart({ where, view, onChange }: { where: WhereTerm[]; view: TokenChartView; onChange: (next: Partial<TokenChartView>) => void }) {
  const split: TokenSplit = view.metric === "cost" && view.split === "type" ? "provider" : view.split;
  const measure = view.metric === "cost" ? "cost_usd" : "total_tokens";
  const aggregations = useMemo(() => split === "type"
    ? tokenTypes.map(type => sum(type.key, [view.period], type.label))
    : [sum(measure, [view.period, split], "chart")], [split, measure, view.period]);
  const result = useAggregations("usage", where, aggregations);
  const { series, buckets } = useMemo(() => {
    const earliest = earliestKey(result.metrics.flatMap(metric => metric.buckets.map(bucket => bucket.keys[0])));
    const buckets = periodBuckets(view.period, view.range, earliest), byKey = new Map(buckets.map(bucket => [bucket.key, bucket]));
    if (split === "type") {
      result.metrics.forEach((metric, index) => {
        for (const bucket of metric.buckets) {
          const target = byKey.get(String(bucket.keys[0]));
          if (target) target.values[tokenTypes[index].key] = Number(bucket.value) || 0;
        }
      });
      return { series: tokenTypes.filter(type => type.key !== "unclassified_tokens" || buckets.some(bucket => bucket.values[type.key])), buckets };
    }
    const totals = new Map<string, number>(), labels = new Map<string, string>();
    const rows = (result.metrics[0]?.buckets ?? []).filter(bucket => byKey.has(String(bucket.keys[0])));
    for (const bucket of rows) {
      const name = String(bucket.keys[1] ?? "");
      totals.set(name, (totals.get(name) ?? 0) + (Number(bucket.value) || 0));
      labels.set(name, splitLabel(split, bucket.keys[1]));
    }
    const fixed = fixedSplitOrder[split];
    let shown: string[];
    if (fixed) shown = [...fixed.filter(name => totals.get(name)), ...[...totals.keys()].filter(name => !fixed.includes(name) && totals.get(name))];
    else shown = [...totals.entries()].filter(([, value]) => value > 0).sort((left, right) => right[1] - left[1]).map(([name]) => name);
    const top = shown.slice(0, 5), folded = shown.length > 5;
    const color = (name: string, index: number) => splitColors[(fixed ? fixed.indexOf(name) : index)] ?? splitColors[index % splitColors.length];
    const series: ChartSeries[] = top.map((name, index) => ({ key: `s:${name}`, label: labels.get(name) ?? name, color: color(name, index) }));
    // A lone folded value keeps its name; Other lists what it holds on hover.
    if (folded) series.push({ key: "other", label: shown.length === 6 ? labels.get(shown[5]) ?? shown[5] : `Other (${shown.length - 5})`, color: "var(--muted)" });
    for (const bucket of rows) {
      const name = String(bucket.keys[1] ?? ""), target = byKey.get(String(bucket.keys[0]))!, value = Number(bucket.value) || 0;
      const key = top.includes(name) ? `s:${name}` : "other";
      target.values[key] = (target.values[key] ?? 0) + value;
      if (key === "other" && value > 0 && shown.length > 6) ((target.parts ??= {}).other ??= []).push([labels.get(name) ?? name, value]);
    }
    return { series, buckets };
  }, [result.metrics, split, view.period, view.range]);
  const unit = chartPeriods[view.period].unit, splitName = tokenSplits.find(([value]) => value === split)?.[1].toLowerCase() ?? split;
  const title = `${view.metric === "cost" ? "API cost" : "Tokens"} per ${unit} by ${splitName}`;
  const controls = <>
    <Segmented label="Show" value={view.metric} options={[["tokens", "Tokens"], ["cost", "Cost"]]} onChange={metric => onChange({ metric })} />
    <Segmented label="Split" value={split} options={tokenSplits.map(([value, text]) => [value, text, value === "type" && view.metric === "cost" ? "Cost is priced per request, not per token type" : undefined])} onChange={next => onChange({ split: next })} />
    <Segmented label="By" value={view.period} options={periodOptions} onChange={period => onChange({ period })} />
    <Segmented label="Range" value={view.range} options={chartRanges} onChange={range => onChange({ range })} />
  </>;
  return <StackedColumns title={title} controls={controls} series={series.length ? series : [{ key: "none", label: title, color: "var(--heat-1)" }]} buckets={buckets} period={view.period}
    format={view.metric === "cost" ? formatUSD : compactNumber} tickFormat={view.metric === "cost" ? costTick : compactNumber} noun={view.metric === "cost" ? "" : "tokens"} loading={result.loading} error={result.error} legend={series.length > 0} />;
}

function PresetRow({ label, presets, onApply, clear }: { label: string; presets: UsagePreset[]; onApply: (preset: UsagePreset | null) => void; clear?: boolean }) {
  return <div className="usage-presets" role="group" aria-label={`${label} presets`}>
    <span className="usage-preset-label">{label}</span>
    {presets.map(preset => <button key={preset.label} type="button" title={preset.title} onClick={() => onApply(preset)}>{preset.label}</button>)}
    {clear ? <button type="button" className="usage-preset-clear" onClick={() => onApply(null)}>Clear metrics</button> : null}
  </div>;
}

function TokenUsage() {
  const [chart, setChart] = useState<TokenChartView>(() => readStored(tokenChartKey, defaultTokenChart, view =>
    ["tokens", "cost"].includes(view.metric) && tokenSplits.some(([value]) => value === view.split) && view.period in chartPeriods && chartRanges.some(([value]) => value === view.range)));
  const update = (next: Partial<TokenChartView>) => setChart(previous => { const merged = { ...previous, ...next }; store(tokenChartKey, merged); return merged; });
  function apply(preset: UsagePreset | null) {
    if (preset?.chart) {
      update(preset.chart);
      document.querySelector("#usage .usage-chart")?.scrollIntoView({ block: "nearest", behavior: "smooth" });
      return;
    }
    tableApis.get("usage")?.setQuery(previous => ({ ...previous, aggregations: preset?.aggregations ?? [], offset: 0 }));
  }
  return <QuerySurface dataset="usage" header={api => {
    const where = toAggregationQuery(api.query, schemas.usage).where;
    return <>
      <TokenSummary where={where} />
      <TokenChart where={where} view={chart} onChange={update} />
      <div className="usage-preset-rows">
        <PresetRow label="Cost" presets={costPresets} onApply={apply} />
        <PresetRow label="Tokens" presets={usagePresets} onApply={apply} clear />
      </div>
    </>;
  }} />;
}

const tokenSummaryAggregations: AggregationClause[] = [
  sum("total_tokens", [], "Total tokens"),
  sum("cost_usd", [], "API cost"),
  sum("uncached_input_tokens", [], "Uncached input"),
  sum("cache_read_input_tokens", [], "Cache reads"),
  sum("output_tokens", [], "Output tokens"),
  { id: "agent_sessions", op: "count_distinct", field: "agent_session_id", groupBy: [], label: "Agent sessions" },
];

function TokenSummary({ where }: { where: WhereTerm[] }) {
  const result = useAggregations("usage", where, tokenSummaryAggregations);
  const values = new Map(result.metrics.map(metric => [metric.id, Number(metric.buckets[0]?.value) || 0]));
  const value = (field: string) => values.get(`${field}:`) ?? 0;
  const tokens = value("total_tokens"), cost = value("cost_usd"), uncached = value("uncached_input_tokens");
  const reads = value("cache_read_input_tokens"), output = value("output_tokens"), sessions = values.get("agent_sessions") ?? 0;
  const cacheHit = uncached + reads > 0 ? `${Math.round(reads / (uncached + reads) * 100)}%` : "—";
  const card = (label: string, amount: string, detail: string) => <div className="mcp-metric"><span>{label}</span><strong>{amount}</strong><small>{detail}</small></div>;
  return <div className="mcp-metrics usage-cards token-summary" aria-label="Machine token summary" aria-busy={result.loading}>
    {card("Total tokens", result.loading ? "Loading…" : result.error ? "Unavailable" : compactNumber(tokens), "all reported input and output")}
    {card("API price equivalent", result.loading ? "Loading…" : result.error ? "Unavailable" : formatUSD(cost), "list-price estimate at the day’s rates")}
    {card("Cache hit rate", result.loading ? "Loading…" : result.error ? "Unavailable" : cacheHit, `${compactNumber(reads)} cached input tokens read`)}
    {card("Output tokens", result.loading ? "Loading…" : result.error ? "Unavailable" : compactNumber(output), "including reported reasoning output")}
    {card("Agent sessions", result.loading ? "Loading…" : result.error ? "Unavailable" : sessions.toLocaleString(), "distinct sessions in the current filters")}
  </div>;
}

// Writing categories as the page reports them. Each group lists the
// message_authorship categories it combines (see docs/human-authorship.md).
const writingGroups = [
  { key: "typed", label: "Typed", description: "Written or dictated by you", categories: ["typed"], color: "var(--sea)" },
  { key: "pasted", label: "Likely pasted", description: "Code, logs, tables, agent-style formatting, or sent faster than typing", categories: ["pasted"], color: "var(--heat-1)" },
  { key: "copied", label: "Copied or re-sent", description: "Matches agent output or your own messages from the previous 48 hours", categories: ["quoted", "resent"], color: "var(--gold)" },
  { key: "prompts", label: "Templates and attachments", description: "Repeated one-click prompts, slash commands, attachment references", categories: ["template", "attachment"], color: "var(--heat-2)" },
  { key: "machine", label: "Harness and automation", description: "Harness instructions and prompts sent by scripts or other agents", categories: ["harness", "automated"], color: "var(--muted)" },
];
type WritingCounts = Record<string, number | string | null>;
type WritingSeries = { works: number; daily: WritingCounts[]; totals: WritingCounts };
type AuthorshipStatus = { built_at: string | null; running: boolean; stale: boolean; error: string | null };
const writingViewKey = "pharos-writing-view";
const groupTotal = (counts: WritingCounts, group: typeof writingGroups[number], unit: "words" | "chars") => group.categories.reduce((sum, category) => sum + (Number(counts[`${category}_${unit}`]) || 0), 0);

// Tracks the background classification, polling while it runs, and reports
// each finished build so the page can reload.
function useAuthorshipStatus(onBuilt: () => void): AuthorshipStatus | null {
  const [status, setStatus] = useState<AuthorshipStatus | null>(null);
  const built = useRef<string | null | undefined>(undefined);
  useEffect(() => {
    let timer = 0, live = true;
    const load = async () => {
      try {
        const next = await responseJSON<AuthorshipStatus>(await fetch("/api/authorship"));
        if (!live) return;
        setStatus(next);
        if (built.current !== undefined && next.built_at !== built.current) onBuilt();
        built.current = next.built_at;
        if (next.running || next.stale) timer = window.setTimeout(load, 5000);
      } catch { /* the status line stays as it was */ }
    };
    void load();
    window.addEventListener("alexandria:usage-refresh", load);
    return () => { live = false; window.clearTimeout(timer); window.removeEventListener("alexandria:usage-refresh", load); };
  }, []);
  return status;
}

function WritingPanel({ where, status, reload }: { where: WhereTerm[]; status: AuthorshipStatus | null; reload: number }) {
  const [view, setView] = useState(() => readStored(writingViewKey, { period: "week" as ChartPeriod, scope: "typed" as "typed" | "all", range: "6m" as ChartRange }, value => value.period in chartPeriods && ["typed", "all"].includes(value.scope) && chartRanges.some(([option]) => option === value.range)));
  const update = (next: Partial<typeof view>) => setView(previous => { const merged = { ...previous, ...next }; store(writingViewKey, merged); return merged; });
  const key = JSON.stringify({ where });
  const [data, setData] = useState<{ key: string; series: WritingSeries | null; error: string; loading: boolean }>({ key: "", series: null, error: "", loading: true });
  useEffect(() => {
    const controller = new AbortController();
    setData(previous => ({ ...previous, loading: true }));
    fetch("/api/query/writing/series", { method: "POST", headers: { "Content-Type": "application/json" }, body: key, signal: controller.signal })
      .then(responseJSON<WritingSeries>)
      .then(series => setData({ key, series, error: "", loading: false }))
      .catch(failure => { if (!controller.signal.aborted) setData({ key, series: null, error: failure instanceof Error ? failure.message : String(failure), loading: false }); });
    return () => controller.abort();
  }, [key, reload]);
  const series = data.series, totals = series?.totals ?? {};
  const groups = view.scope === "typed" ? writingGroups.slice(0, 1) : writingGroups;
  const buckets = useMemo(() => {
    const buckets = periodBuckets(view.period, view.range, earliestKey((series?.daily ?? []).map(day => day.day))), byKey = new Map(buckets.map(bucket => [bucket.key, bucket])), messages = new Map<string, number>();
    for (const day of series?.daily ?? []) {
      const bucket = byKey.get(periodKey(parseDay(String(day.day)), view.period));
      if (!bucket) continue;
      messages.set(bucket.key, (messages.get(bucket.key) ?? 0) + (Number(day.typed_messages) || 0));
      for (const group of writingGroups) bucket.values[group.key] = (bucket.values[group.key] ?? 0) + groupTotal(day, group, "words");
    }
    for (const bucket of buckets) bucket.detail = `${(messages.get(bucket.key) ?? 0).toLocaleString()} messages with typed text`;
    return buckets;
  }, [series, view.period, view.range]);
  const typed = Number(totals.typed_words) || 0, pasted = Number(totals.pasted_words) || 0, typedMessages = Number(totals.typed_messages) || 0;
  const since = localDay(new Date(Date.now() - 29 * 86_400_000));
  const recent = (series?.daily ?? []).filter(day => String(day.day) >= since);
  const recentTyped = recent.reduce((sum, day) => sum + (Number(day.typed_words) || 0), 0), recentMessages = recent.reduce((sum, day) => sum + (Number(day.typed_messages) || 0), 0);
  const building = status !== null && !status.built_at;
  const card = (label: string, value: string, detail: string) => <div className="mcp-metric"><span>{label}</span><strong>{value}</strong><small>{detail}</small></div>;
  const unit = chartPeriods[view.period].unit;
  const allChars = writingGroups.reduce((sum, group) => sum + groupTotal(totals, group, "chars"), 0);
  return <section className="writing-panel" aria-label="Human Words">
    {data.error ? <div className="query-table-error">{data.error}</div> : null}
    <div className="mcp-metrics usage-cards">
      {building ? card("Words you wrote", "Building…", "Classifying every retained user message") : <>
        {card("Words you wrote", compactNumber(typed), `Up to ${compactNumber(typed + pasted)} including text that only looks pasted${totals.first_day ? ` · since ${totals.first_day}` : ""}`)}
        {card("Last 30 days", compactNumber(recentTyped), `${recentMessages.toLocaleString()} messages with typed text`)}
        {card("Messages with typed text", typedMessages.toLocaleString(), `of ${(Number(totals.messages) || 0).toLocaleString()} user turns in ${(series?.works ?? 0).toLocaleString()} conversations`)}
        {card("Words per message", typedMessages ? Math.round(typed / typedMessages).toLocaleString() : "—", "average typed words, where any were typed")}
        {card("Novels", (typed / 90000).toFixed(1), "at 90,000 words each")}
      </>}
    </div>
    <StackedColumns title={view.scope === "typed" ? `Typed words per ${unit}` : `Words of user input per ${unit}`}
      controls={<>
        <Segmented label="Show" value={view.scope} options={[["typed", "Typed"], ["all", "All input"]]} onChange={scope => update({ scope })} />
        <Segmented label="By" value={view.period} options={periodOptions} onChange={period => update({ period })} />
        <Segmented label="Range" value={view.range} options={chartRanges} onChange={range => update({ range })} />
      </>}
      series={groups.map(group => ({ key: group.key, label: group.label, color: group.color }))} buckets={buckets} period={view.period}
      format={value => value.toLocaleString()} tickFormat={compactNumber} noun="words" loading={data.loading} error="" />
    <div className="writing-mix">
      <h3>Where user-turn text came from</h3>
      {!allChars ? <p className="muted">{data.loading ? "Loading…" : "No classified user messages match these filters."}</p> : <>
        <div className="writing-stack" role="img" aria-label={writingGroups.map(group => `${group.label} ${Math.round(100 * groupTotal(totals, group, "chars") / allChars)}%`).join(", ")}>
          {writingGroups.map(group => { const value = groupTotal(totals, group, "chars"); return value ? <i key={group.key} style={{ flex: value, background: group.color }} title={`${group.label}: ${Math.round(100 * value / allChars)}%`} /> : null; })}
        </div>
        <table className="writing-table">
          <thead><tr><th>Source</th><th className="num">Words</th><th className="num">Characters</th><th className="num">Share of text</th></tr></thead>
          <tbody>{writingGroups.map(group => { const chars = groupTotal(totals, group, "chars"); return <tr key={group.key}>
            <td><span className="usage-swatch" style={{ background: group.color }} /><strong>{group.label}</strong><div className="muted">{group.description}</div></td>
            <td className="num">{groupTotal(totals, group, "words").toLocaleString()}</td><td className="num">{chars.toLocaleString()}</td><td className="num">{(100 * chars / allChars).toFixed(1)}%</td>
          </tr>; })}</tbody>
        </table>
      </>}
    </div>
  </section>;
}

function WritingUsage({ onStatus }: { onStatus: (status: AuthorshipStatus | null) => void }) {
  const [reload, setReload] = useState(0);
  const status = useAuthorshipStatus(() => { setReload(value => value + 1); tableApis.get("writing")?.refresh(); });
  useEffect(() => onStatus(status), [status]);
  function apply(preset: UsagePreset | null) {
    tableApis.get("writing")?.setQuery(previous => ({ ...previous, ...(preset?.orderBy ? { orderBy: preset.orderBy } : { aggregations: preset?.aggregations ?? [] }), offset: 0 }));
  }
  return <QuerySurface dataset="writing" header={api => <>
    <WritingPanel where={toAggregationQuery(api.query, schemas.writing).where} status={status} reload={reload} />
    <div className="usage-preset-rows">
      <PresetRow label="Sort" presets={writingPresets} onApply={apply} />
      <PresetRow label="Metrics" presets={writingMetricPresets} onApply={apply} clear />
    </div>
  </>} />;
}

type UsageView = "tokens" | "writing" | "carbon";
const usageViewKey = "pharos-usage-view";

// ?usage= picks the view; otherwise the last choice is kept.
function initialUsageView(): UsageView {
  const requested = new URLSearchParams(location.search).get("usage");
  if (requested === "tokens" || requested === "writing" || requested === "carbon") { store(usageViewKey, requested); return requested; }
  try {
    const saved = localStorage.getItem(usageViewKey);
    return saved === "writing" || saved === "carbon" ? saved : "tokens";
  } catch { return "tokens"; }
}

function CarbonImpact() {
  useEffect(() => {
    const refresh = () => { void window.pharosCarbon?.refresh(); };
    refresh();
    window.addEventListener("alexandria:usage-refresh", refresh);
    return () => window.removeEventListener("alexandria:usage-refresh", refresh);
  }, []);
  return <section aria-label="Carbon Impact" data-feedback-label="Carbon Impact">
    <div id="carbonCard" className="carbon-card" aria-busy="true" />
  </section>;
}

function UsagePage() {
  const [view, setView] = useState<UsageView>(initialUsageView);
  const [status, setStatus] = useState<AuthorshipStatus | null>(null);
  useEffect(() => {
    const restore = () => { if (location.pathname === "/usage") setView(initialUsageView()); };
    window.addEventListener("alexandria:route", restore);
    return () => window.removeEventListener("alexandria:route", restore);
  }, []);
  function choose(next: UsageView) { setView(next); store(usageViewKey, next); updateURI("usage", next === "writing" ? next : ""); }
  const statusText = !status ? "" : status.error ? `Last update failed: ${status.error}` : status.running ? "Updating…" : status.built_at ? `Updated ${new Date(status.built_at).toLocaleString()}` : "Waiting to build";
  return <div className="alexandria-query-page usage-page">
    <div className="view-heading library-heading usage-heading">
      <div><h1>Usage</h1><p className="muted">{view === "tokens"
        ? "Reconciled tokens per agent session, day, and model. Input includes cached input; filter to any provider, model, or repository and the chart follows. Cost is the API list-price equivalent on the day of use, at standard rates. It ignores long-context premiums and subscriptions, so treat it as a lower bound. ≈ marks costs priced by assumption."
        : view === "writing"
          ? <>Text you typed or dictated into agent chats, per conversation. Harness instructions, one-click prompts, attachments, pastes, and copied agent output are counted separately; filter the table and the chart and breakdown follow. {statusText ? <span className="meta">{statusText}</span> : null}</>
          : "Estimated inference electricity and CO₂e for the tokens in this library, using published research and explicit assumptions."}</p></div>
      <div className="usage-heading-actions">
        <div className="library-view-toggle" role="group" aria-label="Usage view">
          <button type="button" className={view === "writing" ? "active" : ""} aria-pressed={view === "writing"} onClick={() => choose("writing")}>Human Words</button>
          <button type="button" className={view === "tokens" ? "active" : ""} aria-pressed={view === "tokens"} onClick={() => choose("tokens")}>Machine Tokens</button>
          <button type="button" className={view === "carbon" ? "active" : ""} aria-pressed={view === "carbon"} onClick={() => choose("carbon")}>Carbon Impact</button>
        </div>
        {view === "tokens" ? <RefreshPricesButton /> : null}
      </div>
    </div>
    {view === "tokens" ? <TokenUsage /> : view === "writing" ? <WritingUsage onStatus={setStatus} /> : <CarbonImpact />}
  </div>;
}

type MCPStatus = { enabled: boolean; transport: string; command: string; args: string[]; note: string };
type MCPCall = { id: number; called_at: string; tool_name: string; arguments_json: string; status: string; error_text: string | null; duration_ms: number; response_bytes: number; estimated_output_tokens: number; result_count: number | null; truncated: boolean };
type MCPHistory = { stats: { total_calls: number; failed_calls: number; average_output_tokens: number; largest_output_tokens: number; truncated_calls: number } };

const mcpMetrics = [
  { label: "By tool", aggregations: [sum("call_count", ["tool_name"], "Calls by tool"), sum("error_count", ["tool_name"], "Errors by tool"), sum("estimated_output_tokens", ["tool_name"], "Output tokens by tool")] },
  { label: "By day", aggregations: [sum("call_count", ["day"], "Calls per day"), sum("error_count", ["day"], "Errors per day"), sum("estimated_output_tokens", ["day"], "Output tokens per day")] },
  { label: "Failures", aggregations: [sum("error_count", ["tool_name", "status"], "Failures by tool and status")] },
  { label: "Token cost", aggregations: [sum("estimated_output_tokens", ["tool_name"], "Output tokens by tool"), sum("truncated_count", ["tool_name"], "Truncated calls by tool")] },
];

function MCPPage() {
  const [status, setStatus] = useState<MCPStatus | null>(null);
  const [history, setHistory] = useState<MCPHistory | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [copied, setCopied] = useState("");
  const [selectedCall, setSelectedCall] = useState<MCPCall | null>(null);
  async function refresh() {
    try {
      const [nextStatus, nextHistory] = await Promise.all([
        fetch("/api/mcp").then(responseJSON<MCPStatus>),
        fetch("/api/mcp/calls?limit=1").then(responseJSON<MCPHistory>),
      ]);
      setStatus(nextStatus); setHistory(nextHistory); setError("");
    } catch (failure) { setError(failure instanceof Error ? failure.message : "MCP status unavailable"); }
  }

  useEffect(() => {
    void refresh();
    const onRoute = () => { if (location.pathname === "/mcp") void refresh(); };
    const timer = window.setInterval(() => { if (document.querySelector("#mcp.active")) void refresh(); }, 5000);
    window.addEventListener("alexandria:route", onRoute);
    return () => { window.clearInterval(timer); window.removeEventListener("alexandria:route", onRoute); };
  }, []);

  async function toggle() {
    if (!status) return;
    setBusy(true);
    try {
      const next = await responseJSON<{ enabled: boolean }>(await fetch("/api/mcp/enabled", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ enabled: !status.enabled }),
      }));
      setStatus({ ...status, enabled: next.enabled }); setError("");
    } catch (failure) { setError(failure instanceof Error ? failure.message : "Could not update MCP"); }
    finally { setBusy(false); }
  }

  async function copy(label: string, value: string) {
    try { if (window.alexandriaCopyText) await window.alexandriaCopyText(value); else await navigator.clipboard.writeText(value); setCopied(label); window.setTimeout(() => setCopied(""), 2000); }
    catch { setError("Could not copy to clipboard. Select the text and copy it manually."); }
  }

  function applyMetricPreset(aggregations: AggregationClause[] | null) {
    tableApis.get("mcp_calls")?.setQuery(previous => ({ ...previous, aggregations: aggregations ?? [], offset: 0 }));
  }

  const connection = status ? JSON.stringify({ mcpServers: { pharos: { command: status.command, args: status.args } } }, null, 2) : "";
  const prompt = status ? `Add Pharos as a local stdio MCP server. Use search_conversations first, with a small limit and max_output_tokens budget. Open get_conversation_overview for promising results, search_conversation_passages for a specific topic, and get_conversation_messages only around cited message IDs. Treat previews as leads and inspect the cited evidence before relying on an outcome. Pharos is read-only. If its tools are unavailable, enable MCP in the Pharos app and reconnect this agent client.\n\nConnection definition:\n${connection}` : "";
  const stats = history?.stats;
  return <div className="mcp-page alexandria-query-page">
    <div className="view-heading"><div><h1>MCP</h1><p className="muted">Let local agents search past conversations in small steps. Review calls to spot oversized responses and failed queries.</p></div><button type="button" className="mcp-refresh" onClick={() => { void refresh(); tableApis.get("mcp_calls")?.refresh(); }}>Refresh</button></div>
    {error ? <div className="query-table-error" role="alert">{error}</div> : null}
    <section className="mcp-card mcp-status-card" aria-label="MCP availability">
      <div><span className={`mcp-status ${status?.enabled ? "enabled" : "disabled"}`}>{status ? status.enabled ? "Available" : "Off" : "Loading"}</span><h2>Agent access</h2><p className="muted">{status?.note ?? "Loading connection settings…"}</p></div>
      <button type="button" className={`toggle ${status?.enabled ? "on" : ""}`} role="switch" aria-checked={Boolean(status?.enabled)} aria-label="Enable MCP" disabled={!status || busy} onClick={() => void toggle()} />
    </section>
    <section className="mcp-history" aria-label="MCP call history">
      <div className="mcp-card-heading"><div><h2>Call history</h2><p className="muted">Newest first by default. Token counts are estimates from response size; no response text is stored. The latest 5,000 calls are retained. Summary cards cover all retained calls; table filters and metrics apply below.</p></div></div>
      <div className="mcp-metrics">
        <div className="mcp-metric"><span>Calls</span><strong>{stats?.total_calls?.toLocaleString() ?? "—"}</strong></div>
        <div className="mcp-metric"><span>Errors</span><strong>{stats?.failed_calls?.toLocaleString() ?? "—"}</strong></div>
        <div className="mcp-metric"><span>Average output</span><strong>{stats ? `${compactNumber(stats.average_output_tokens)} est. tokens` : "—"}</strong></div>
        <div className="mcp-metric"><span>Largest output</span><strong>{stats ? `${compactNumber(stats.largest_output_tokens)} est. tokens` : "—"}</strong></div>
        <div className="mcp-metric"><span>Truncated</span><strong>{stats?.truncated_calls?.toLocaleString() ?? "—"}</strong></div>
      </div>
      <div className="usage-presets" role="group" aria-label="MCP metric presets">
        {mcpMetrics.map(preset => <button key={preset.label} type="button" onClick={() => applyMetricPreset(preset.aggregations)}>{preset.label}</button>)}
        <button type="button" className="usage-preset-clear" onClick={() => applyMetricPreset(null)}>Clear metrics</button>
      </div>
      <QuerySurface dataset="mcp_calls" trailing={row => <button type="button" className="mcp-detail-button" onClick={() => setSelectedCall(row as MCPCall)}>Details</button>} />
    </section>
    <div className="mcp-setup-grid">
      <section className="mcp-card"><div className="mcp-card-heading"><div><h2>Connection</h2><p className="muted">Add this stdio server in an agent client’s MCP settings. The client launches Pharos locally.</p></div><button type="button" disabled={!status} onClick={() => void copy("connection", connection)}>{copied === "connection" ? "Copied" : "Copy JSON"}</button></div><pre className="mcp-code"><code>{connection || "Loading…"}</code></pre><p className="mcp-fineprint">Client settings formats vary. Use the command and args shown here if your client does not accept this JSON shape.</p></section>
      <section className="mcp-card"><div className="mcp-card-heading"><div><h2>Prompt for an agent</h2><p className="muted">Paste this when asking an agent to add Pharos and use it efficiently.</p></div><button type="button" disabled={!status} onClick={() => void copy("prompt", prompt)}>{copied === "prompt" ? "Copied" : "Copy prompt"}</button></div><pre className="mcp-prompt">{prompt || "Loading…"}</pre></section>
    </div>
    {selectedCall ? <div className="mcp-dialog-backdrop" onClick={() => setSelectedCall(null)}><div className="mcp-dialog" role="dialog" aria-modal="true" aria-label="MCP call details" onClick={event => event.stopPropagation()}>
      <div className="mcp-card-heading"><div><h2>{selectedCall.tool_name}</h2><p className="muted">{new Date(selectedCall.called_at).toLocaleString()} · {selectedCall.status}</p></div><button type="button" onClick={() => setSelectedCall(null)}>Close</button></div>
      <p className="muted">{compactNumber(selectedCall.estimated_output_tokens)} estimated output tokens · {selectedCall.response_bytes.toLocaleString()} bytes · {selectedCall.duration_ms.toLocaleString()} ms{selectedCall.result_count === null ? "" : ` · ${selectedCall.result_count} results`}{selectedCall.truncated ? " · truncated" : ""}</p>
      <h3>Sanitized arguments</h3><pre className="mcp-code">{selectedCall.arguments_json}</pre>
      {selectedCall.error_text ? <><h3>Error</h3><pre className="mcp-code badtext">{selectedCall.error_text}</pre></> : null}
    </div></div> : null}
  </div>;
}

type ToolPreset = { label: string; title: string; aggregations?: AggregationClause[]; where?: WhereTerm[]; orderBy?: OrderByClause[] };
type ToolLedgerStatus = { conversations: number; current_conversations: number; pending_conversations: number; tool_calls: number; backfill: { running: boolean; done: number; total: number; error: string | null } };
const count = (groupBy: string[], label: string): AggregationClause => ({ id: `count:${groupBy.join(",")}`, op: "count", groupBy, label });
const shellCalls: WhereTerm[] = [{ field: "tool_category", op: "=", value: "command" }];

// Summary presets group the daily rollup; call presets explore individual calls.
const toolSummaryPresets: Array<{ group: string; presets: ToolPreset[] }> = [
  { group: "Errors", presets: [
    { label: "Error rate by tool", title: "Errors and calls per tool; divide for the rate", aggregations: [sum("error_count", ["tool_name"], "Errors by tool"), sum("call_count", ["tool_name"], "Calls by tool")] },
    { label: "Failure kinds", title: "Why calls failed: rejected, interrupted, timed out, non-zero exit, blocked by hook, or no result", aggregations: [
      sum("rejected_count", [], "Rejected by user"), sum("interrupted_count", [], "Interrupted"), sum("timeout_count", [], "Timed out"),
      sum("nonzero_exit_count", [], "Non-zero exit"), sum("hook_blocked_count", [], "Blocked by hook"), sum("no_result_count", [], "No result"),
    ] },
    { label: "Failing commands", title: "Non-zero exits by shell program and subcommand", where: shellCalls, aggregations: [sum("nonzero_exit_count", ["command_name"], "Non-zero exits by command"), sum("call_count", ["command_name"], "Runs by command")] },
    { label: "Errors by model", title: "Errors and calls by model", aggregations: [sum("error_count", ["model_family"], "Errors by model"), sum("call_count", ["model_family"], "Calls by model")] },
  ] },
  { group: "Commands", presets: [
    { label: "Top programs", title: "Shell calls by program (git, cat, go, rg, …)", where: shellCalls, aggregations: [sum("call_count", ["program"], "Shell calls by program")] },
    { label: "Subcommands", title: "Shell calls by program and subcommand (git status, go test, npm run build, …)", where: shellCalls, aggregations: [sum("call_count", ["command_name"], "Shell calls by command"), sum("total_duration_ms", ["command_name"], "Time by command")] },
    { label: "Command categories", title: "Shell calls and time by kind of command", where: shellCalls, aggregations: [sum("call_count", ["command_category"], "Calls by command category"), sum("total_duration_ms", ["command_category"], "Time by command category")] },
    { label: "Shell instead of tools", title: "Shell calls that read, search, or list files, which dedicated tools could do", where: [...shellCalls, { any: ["read", "search", "list"].map(value => ({ field: "command_category", op: "=" as const, value })) }], aggregations: [sum("call_count", ["program"], "Read/search/list via shell")] },
  ] },
  { group: "Time", presets: [
    { label: "Slowest tools", title: "Total and longest duration per tool", aggregations: [sum("total_duration_ms", ["tool_name"], "Total time by tool"), { id: "max:max_duration_ms:tool_name", op: "max", field: "max_duration_ms", groupBy: ["tool_name"], label: "Longest call by tool" }] },
    { label: "Tests & builds", title: "Time spent in test and build commands per week", where: [{ any: ["test", "build"].map(value => ({ field: "command_category", op: "=" as const, value })) }], aggregations: [sum("total_duration_ms", ["week", "command_category"], "Test/build time per week"), sum("call_count", ["week", "command_category"], "Test/build runs per week")] },
    { label: "Calls per day", title: "Tool calls per day by category", aggregations: [sum("call_count", ["day", "tool_category"], "Calls per day by category")] },
  ] },
  { group: "Context", presets: [
    { label: "Context added", title: "Tokens tool results added to the context, by tool and by shell program", aggregations: [sum("result_tokens", ["tool_name"], "Context added by tool"), sum("result_tokens", ["program"], "Context added by program")] },
    { label: "Context carried", title: "Result tokens re-read by later requests until compaction", aggregations: [sum("carried_tokens", ["tool_name"], "Context carried by tool"), sum("carried_tokens", ["program"], "Context carried by program")] },
    { label: "Cost by tool", title: "API-equivalent cost of tool results and the output that requested them", aggregations: [sum("tool_cost_usd", ["tool_name"], "Cost by tool"), sum("context_cost_usd", ["tool_category"], "Context cost by category")] },
    { label: "MCP servers", title: "MCP calls, errors, and context by server", where: [{ field: "mcp_server", op: "is_not_null", value: "" }], aggregations: [sum("call_count", ["mcp_server"], "Calls by MCP server"), sum("error_count", ["mcp_server"], "Errors by MCP server"), sum("result_tokens", ["mcp_server"], "Context added by MCP server")] },
    { label: "Subagents", title: "Tool use by root agents versus subagents", aggregations: [sum("call_count", ["session_kind", "tool_category"], "Calls by agent kind")] },
  ] },
];

const toolCallPresets: ToolPreset[] = [
  { label: "Calls by day", title: "Tool call volume and errors over time", aggregations: [count(["day"], "Calls per day"), sum("error_count", ["day"], "Errors per day")] },
  { label: "Calls by week", title: "Weekly tool call volume by category", aggregations: [count(["week", "tool_category"], "Calls per week by category")] },
  { label: "Calls by tool", title: "Tool call volume by tool", aggregations: [count(["tool_name"], "Calls by tool")] },
  { label: "Slowest calls", title: "Single calls by duration", orderBy: [{ field: "duration_ms", dir: "desc" }] },
  { label: "Largest results", title: "Single calls by tokens their result added to the context", orderBy: [{ field: "result_tokens", dir: "desc" }] },
  { label: "Most carried", title: "Results re-read the most before compaction", orderBy: [{ field: "carried_tokens", dir: "desc" }] },
  { label: "Failures", title: "Failed calls, newest first, with a breakdown by error type", where: [{ field: "status", op: "=", value: "error" }], orderBy: [{ field: "started_at", dir: "desc" }], aggregations: [count(["error_type"], "Failures by type")] },
  { label: "Exit codes", title: "Non-zero exit codes by command", where: [{ field: "exit_code", op: "!=", value: "0" }, { field: "exit_code", op: "is_not_null", value: "" }], aggregations: [count(["command_name", "exit_code"], "Exit codes by command")] },
  { label: "Rejected", title: "Calls a person declined", where: [{ field: "error_type", op: "=", value: "user_rejected" }], orderBy: [{ field: "started_at", dir: "desc" }], aggregations: [count(["tool_name"], "Rejections by tool")] },
  { label: "Sites", title: "Calls that reached a URL (web fetches, browser navigation, curl and other network commands), by site and tool", where: [{ field: "host", op: "is_not_null", value: "" }], orderBy: [{ field: "started_at", dir: "desc" }], aggregations: [count(["host"], "Calls by site"), count(["host", "tool_name"], "Calls by site and tool")] },
  { label: "Web searches", title: "Web searches, newest first, by tool", where: [{ field: "search_query", op: "is_not_null", value: "" }], orderBy: [{ field: "started_at", dir: "desc" }], aggregations: [count(["tool_name"], "Searches by tool")] },
];

// Filters that select the calls behind one summary row.
function drillFilters(row: Row): WhereTerm[] {
  const where: WhereTerm[] = [];
  for (const field of ["day", "tool_name", "program", "subcommand", "provider", "model", "repository_name", "session_kind", "source_kind"]) {
    const value = row[field];
    where.push(value === null || value === undefined || value === "" ? { field, op: "is_null", value: "" } : { field, op: "=", value: String(value) });
  }
  return where;
}

function formatDurationMS(value: unknown): string {
  const ms = Number(value);
  if (value === null || value === undefined || !Number.isFinite(ms)) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  return `${Math.floor(ms / 60_000)}m ${Math.round((ms % 60_000) / 1000)}s`;
}

const urlSourceLabels: Record<string, string> = { input: "Tool argument", command: "Shell command", result: "Opened", search_result: "Search result" };

// Reached URLs first; the links a search returned follow, since the agent may not have opened them.
function ToolCallURLs({ urls }: { urls: Row[] }) {
  const reached = urls.filter(entry => entry.source !== "search_result"), results = urls.filter(entry => entry.source === "search_result");
  const table = (rows: Row[]) => <div className="mcp-tool-table-wrap"><table className="tool-commands tool-urls"><thead><tr><th>Site</th><th>Source</th><th>URL</th></tr></thead><tbody>
    {rows.map(entry => <tr key={entry.position}><td>{String(entry.host)}</td><td>{urlSourceLabels[String(entry.source)] ?? String(entry.source)}</td><td className="tool-command-text"><a href={String(entry.url)} target="_blank" rel="noopener noreferrer">{String(entry.url)}</a></td></tr>)}
  </tbody></table></div>;
  return <>
    {reached.length ? <><h3>Sites reached</h3>{table(reached)}</> : null}
    {results.length ? <><h3>Search results ({results.length.toLocaleString()})</h3>{table(results)}</> : null}
  </>;
}

function ToolCallDialog({ id, onClose }: { id: string; onClose: () => void }) {
  const [call, setCall] = useState<Row | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    fetch(`/api/tool-calls/${encodeURIComponent(id)}`, { signal: controller.signal }).then(responseJSON<Row>).then(setCall)
      .catch(failure => { if (!controller.signal.aborted) setError(failure instanceof Error ? failure.message : "Call unavailable"); });
    return () => controller.abort();
  }, [id]);
  const number = (value: unknown) => value === null || value === undefined ? "—" : Number(value).toLocaleString();
  return <div className="mcp-dialog-backdrop" onClick={onClose}><div className="mcp-dialog tool-dialog" role="dialog" aria-modal="true" aria-label="Tool call details" onClick={event => event.stopPropagation()}>
    <div className="mcp-card-heading"><div><h2>{call ? String(call.tool_name) : "Tool call"}</h2><p className="muted">{call ? `${new Date(String(call.started_at)).toLocaleString()} · ${call.status}${call.error_type ? ` · ${String(call.error_type).replace(/_/g, " ")}` : ""}` : error || "Loading…"}</p></div>
      <div className="tool-dialog-actions">{call?.workspace_id ? <button type="button" onClick={() => { onClose(); window.alexandriaOpenDetail?.(String(call.workspace_id)); }}>Open work</button> : null}<button type="button" onClick={onClose}>Close</button></div></div>
    {call ? <>
      <dl className="tool-facts">
        <div><dt>Duration</dt><dd>{formatDurationMS(call.duration_ms)}{call.duration_source === "timestamps" ? " (timestamps)" : ""}</dd></div>
        <div><dt>Context added</dt><dd>{call.result_tokens == null ? "—" : compactNumber(Number(call.result_tokens))} tokens{call.result_tokens_source ? ` · ${call.result_tokens_source}` : ""}</dd></div>
        <div><dt>Carried</dt><dd>{call.carried_tokens == null ? "—" : compactNumber(Number(call.carried_tokens))} tokens over {number(call.carried_requests)} requests</dd></div>
        <div><dt>Cost</dt><dd>{call.tool_cost_usd === null || call.tool_cost_usd === undefined ? "—" : formatUSD(Number(call.tool_cost_usd))}</dd></div>
        <div><dt>Exit code</dt><dd>{call.exit_code ?? "—"}</dd></div>
        <div><dt>Calls in request</dt><dd>{number(call.parallel_count)}</dd></div>
      </dl>
      {call.title ? <p className="muted">{String(call.title)}{call.repository_name ? ` · ${call.repository_name}` : ""} · {String(call.provider)} {call.model ? `· ${call.model}` : ""}</p> : null}
      {Array.isArray(call.commands) && call.commands.length ? <><h3>Commands</h3><div className="mcp-tool-table-wrap"><table className="tool-commands"><thead><tr><th>#</th><th>Program</th><th>Category</th><th>Exit</th><th>Duration</th><th>Command</th></tr></thead><tbody>
        {call.commands.map((command: Row) => <tr key={command.position}><td>{command.operator && command.operator !== "script" ? command.operator : Number(command.position) + 1}</td><td>{[command.program, command.subcommand].filter(Boolean).join(" ")}</td><td>{command.category ?? ""}</td><td>{command.exit_code ?? ""}</td><td>{command.duration_ms === null || command.duration_ms === undefined ? "" : formatDurationMS(command.duration_ms)}</td><td className="tool-command-text">{String(command.command)}</td></tr>)}
      </tbody></table></div></> : null}
      {call.search_query ? <><h3>Search query</h3><p className="tool-search-query">{String(call.search_query)}</p></> : null}
      {Array.isArray(call.urls) && call.urls.length ? <ToolCallURLs urls={call.urls} /> : null}
      <h3>Input</h3><pre className="mcp-code">{String(call.input_text ?? "")}</pre>
      <h3>Result{Number(call.result_text_bytes) > String(call.result_text ?? "").length ? ` (first ${String(call.result_text ?? "").length.toLocaleString()} of ${Number(call.result_text_bytes).toLocaleString()} characters)` : ""}</h3><pre className="mcp-code">{String(call.result_text ?? "") || "No result was recorded."}</pre>
      {call.result_details ? <><h3>Result metadata</h3><pre className="mcp-code">{String(call.result_details)}</pre></> : null}
    </> : null}
  </div></div>;
}

function ToolLedgerBanner() {
  const [status, setStatus] = useState<ToolLedgerStatus | null>(null);
  const [error, setError] = useState("");
  const load = useRef(async () => {});
  load.current = async () => {
    try { setStatus(await responseJSON<ToolLedgerStatus>(await fetch("/api/tools/status"))); setError(""); }
    catch (failure) { setError(failure instanceof Error ? failure.message : "Status unavailable"); }
  };
  useEffect(() => {
    void load.current();
    const timer = window.setInterval(() => { if (document.querySelector("#tools.active")) void load.current(); }, 3000);
    return () => window.clearInterval(timer);
  }, []);
  const running = status?.backfill.running ?? false;
  const wasRunning = useRef(false);
  useEffect(() => {
    if (wasRunning.current && !running) { tableApis.get("tools")?.refresh(); tableApis.get("tool_calls")?.refresh(); }
    wasRunning.current = running;
  }, [running]);
  async function build() {
    try { await fetch("/api/tools/backfill", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }); await load.current(); }
    catch (failure) { setError(failure instanceof Error ? failure.message : "Could not start"); }
  }
  if (error) return <div className="query-table-error">{error}</div>;
  if (!status || (!running && status.pending_conversations === 0 && !status.backfill.error)) return null;
  const percent = status.backfill.total ? Math.round(status.backfill.done / status.backfill.total * 100) : 0;
  return <div className="tool-ledger-banner" role="status">
    {running ? <span>Building the tool ledger from retained transcripts: {status.backfill.done.toLocaleString()} of {status.backfill.total.toLocaleString()} conversations ({percent}%). Tables refresh as it goes.</span>
      : <span>{status.pending_conversations.toLocaleString()} of {status.conversations.toLocaleString()} conversations have no tool ledger yet. Building it re-reads their retained messages; no source files are needed.{status.backfill.error ? ` Last attempt failed: ${status.backfill.error}` : ""}</span>}
    {running ? null : <button type="button" onClick={() => void build()}>Build tool ledger</button>}
  </div>;
}

function termKey(term: WhereTerm): string {
  const clause = (item: { field: string; op: string; value: string; negated?: boolean }) => `${item.field}|${item.op}|${item.value}|${item.negated ? 1 : 0}`;
  return "any" in term ? `any:${term.any.map(clause).join("&")}` : clause(term);
}

function ToolsPage() {
  const [view, setView] = useState<"summary" | "calls">(() => new URLSearchParams(location.search).get("tools_view") === "summary" ? "summary" : "calls");
  const [selected, setSelected] = useState<string | null>(null);
  // Filters the last preset added, so the next preset replaces them while
  // keeping filters the user added by hand.
  const presetWhere = useRef<Partial<Record<Dataset, string[]>>>({});
  function choose(next: "summary" | "calls") { setView(next); updateURI("tools_view", next === "summary" ? next : ""); }
  function apply(dataset: Dataset, preset: ToolPreset | null) {
    const added = presetWhere.current[dataset] ?? [];
    presetWhere.current[dataset] = (preset?.where ?? []).map(termKey);
    tableApis.get(dataset)?.setQuery(previous => ({
      ...previous,
      aggregations: preset?.aggregations ?? [],
      where: [...previous.where.filter(term => !added.includes(termKey(term))), ...(preset?.where ?? [])],
      orderBy: preset?.orderBy ?? previous.orderBy,
      offset: 0,
    }));
  }
  function drill(row: Row) {
    tableApis.get("tool_calls")?.setQuery(previous => ({ ...previous, where: drillFilters(row), aggregations: [], offset: 0 }));
    choose("calls");
  }
  return <div className="alexandria-query-page tools-page">
    <div className="view-heading library-heading"><div><h1>Tool use</h1><p className="muted">Every tool call from retained transcripts, joined to its result. Shell commands are parsed into program, subcommand, and category. <b>Context added</b> is what a result grew the next request’s prompt by (measured from provider usage, or estimated from size at 4 bytes a token); <b>context carried</b> counts it again for every later request until compaction. Claude durations come from timestamps and include any wait for approval. Linked mirrors of the same work are counted once.</p></div>
      <div className="library-view-toggle" role="group" aria-label="Tool view">
        <button type="button" className={view === "calls" ? "active" : ""} aria-pressed={view === "calls"} onClick={() => choose("calls")}>Calls</button>
        <button type="button" className={view === "summary" ? "active" : ""} aria-pressed={view === "summary"} onClick={() => choose("summary")}>Summary</button>
      </div></div>
    <ToolLedgerBanner />
    <div hidden={view !== "summary"}>
      {toolSummaryPresets.map(({ group, presets }) => <div key={group} className="usage-presets" role="group" aria-label={`${group} presets`}>
        <span className="usage-preset-label">{group}</span>
        {presets.map(preset => <button key={preset.label} type="button" title={preset.title} onClick={() => apply("tools", preset)}>{preset.label}</button>)}
        {group === "Context" ? <button type="button" className="usage-preset-clear" onClick={() => apply("tools", null)}>Clear metrics</button> : null}
      </div>)}
      <QuerySurface dataset="tools" trailing={row => <button type="button" className="mcp-detail-button" title="Show the calls behind this row" onClick={() => drill(row)}>Calls</button>} />
    </div>
    <div hidden={view !== "calls"}>
      <div className="usage-presets" role="group" aria-label="Call presets">
        <span className="usage-preset-label">Calls</span>
        {toolCallPresets.map(preset => <button key={preset.label} type="button" title={preset.title} onClick={() => apply("tool_calls", preset)}>{preset.label}</button>)}
        <button type="button" className="usage-preset-clear" onClick={() => apply("tool_calls", null)}>Clear metrics</button>
      </div>
      <QuerySurface dataset="tool_calls" trailing={row => <button type="button" className="mcp-detail-button" onClick={() => setSelected(String(row.id))}>Details</button>} />
    </div>
    {selected ? <ToolCallDialog id={selected} onClose={() => setSelected(null)} /> : null}
  </div>;
}

const mounts: Array<[string, React.ReactNode]> = [
  ["queryTableLibrary", <LibraryPage />],
  ["queryTableUsage", <UsagePage />],
  ["toolsPage", <ToolsPage />],
  ["mcpPage", <MCPPage />],
  ["tl1Page", <TL1Page attempts={<QuerySurface dataset="tl1_attempts" />} filterAttempts={(where) => tableApis.get("tl1_attempts")?.setQuery(previous => ({ ...previous, where: where as WhereTerm[], offset: 0 }))} copy={copyText} />],
];
for (const [id, component] of mounts) {
  const element = document.getElementById(id);
  if (element) createRoot(element).render(component);
}
window.alexandriaQueryTables = {
  refresh(dataset) {
    tableApis.get(dataset)?.refresh();
    if (dataset === "usage") {
      tableApis.get("writing")?.refresh();
      window.dispatchEvent(new Event("alexandria:usage-refresh"));
    }
  },
  filterRepository(repository) {
    clearLibrarySearch?.();
    tableApis.get("library")?.setQuery((previous) => ({
      ...previous,
      where: [{ field: "repository_name", op: "=", value: repository }],
      offset: 0,
    }));
  },
  filterModel(model) {
    clearLibrarySearch?.();
    tableApis.get("library")?.setQuery((previous) => ({
      ...previous,
      where: [{ field: "models", op: "contains", value: model }],
      offset: 0,
    }));
  },
};
