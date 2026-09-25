PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS repositories (
  id TEXT PRIMARY KEY,
  canonical_remote TEXT,
  display_name TEXT NOT NULL,
  owner TEXT,
  aliases_json TEXT NOT NULL DEFAULT '[]',
  local_locations_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY,
  source_kind TEXT NOT NULL,
  source_account TEXT NOT NULL,
  source_id TEXT NOT NULL,
  repository_id TEXT REFERENCES repositories(id),
  title TEXT NOT NULL,
  purpose TEXT,
  outcome TEXT,
  location TEXT,
  location_history_json TEXT NOT NULL DEFAULT '[]',
  base_ref TEXT,
  head_ref TEXT,
  branch TEXT,
  owner TEXT,
  flavor TEXT,
  version TEXT,
  lifecycle TEXT NOT NULL DEFAULT 'active',
  activity_at TEXT,
  activity_source TEXT,
  terminal INTEGER NOT NULL DEFAULT 0,
  custody_version TEXT,
  reclamation_authority INTEGER NOT NULL DEFAULT 0,
  preservation_completeness TEXT NOT NULL DEFAULT 'source',
  indexed_at TEXT NOT NULL,
  UNIQUE(source_kind, source_account, source_id)
);
CREATE INDEX IF NOT EXISTS workspaces_repo_idx ON workspaces(repository_id);
CREATE INDEX IF NOT EXISTS workspaces_activity_idx ON workspaces(activity_at);

CREATE TABLE IF NOT EXISTS work_items (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  source_kind TEXT NOT NULL,
  source_account TEXT NOT NULL DEFAULT 'local',
  source_id TEXT NOT NULL,
  title TEXT,
  status TEXT,
  explicit INTEGER NOT NULL DEFAULT 1,
  relationship_confidence REAL,
  evidence_json TEXT NOT NULL DEFAULT '{}',
  UNIQUE(source_kind, source_account, source_id)
);

CREATE TABLE IF NOT EXISTS conversations (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  work_item_id TEXT REFERENCES work_items(id) ON DELETE SET NULL,
  provider TEXT NOT NULL,
  model TEXT,
  account TEXT NOT NULL,
  native_id TEXT NOT NULL,
  parent_id TEXT REFERENCES conversations(id),
  agent_depth INTEGER NOT NULL DEFAULT 0,
  agent_path TEXT,
  agent_nickname TEXT,
  origin TEXT,
  origin_host_id TEXT,
  coverage TEXT NOT NULL DEFAULT 'complete',
  started_at TEXT,
  ended_at TEXT,
  aliases_json TEXT NOT NULL DEFAULT '[]',
  UNIQUE(provider, account, native_id)
);
CREATE INDEX IF NOT EXISTS conversations_workspace_idx ON conversations(workspace_id);

CREATE TABLE IF NOT EXISTS mcp_calls (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  called_at TEXT NOT NULL,
  tool_name TEXT NOT NULL,
  arguments_json TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL,
  error_text TEXT,
  duration_ms INTEGER NOT NULL,
  response_bytes INTEGER NOT NULL,
  estimated_output_tokens INTEGER NOT NULL,
  result_count INTEGER,
  truncated INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS mcp_calls_called_at_idx ON mcp_calls(called_at DESC);

CREATE TABLE IF NOT EXISTS conversation_documents (
  conversation_id TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
  initiation TEXT,
  initiation_message_id TEXT REFERENCES messages(id) ON DELETE SET NULL,
  outcome TEXT,
  outcome_message_id TEXT REFERENCES messages(id) ON DELETE SET NULL,
  vector_json TEXT NOT NULL,
  model TEXT NOT NULL,
  indexed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS conversation_documents_initiation_message_idx ON conversation_documents(initiation_message_id);
CREATE INDEX IF NOT EXISTS conversation_documents_outcome_message_idx ON conversation_documents(outcome_message_id);

CREATE TABLE IF NOT EXISTS conversation_identity_links (
  left_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  right_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  relationship TEXT NOT NULL,
  confidence REAL NOT NULL,
  evidence_json TEXT NOT NULL,
  PRIMARY KEY(left_id,right_id,relationship)
);

CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  native_id TEXT NOT NULL,
  role TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'message',
  model TEXT,
  text TEXT NOT NULL,
  raw_text TEXT,
  source_order INTEGER,
  created_at TEXT,
  parent_native_id TEXT,
  previous_native_id TEXT,
  call_id TEXT,
  evidence_locator TEXT,
  content_hash TEXT NOT NULL,
  selected INTEGER NOT NULL DEFAULT 1,
  sender TEXT,
  UNIQUE(conversation_id, native_id)
);
CREATE INDEX IF NOT EXISTS messages_conversation_idx ON messages(conversation_id, created_at);
-- Serves the human-authorship rebuild, which reads prose turns by role in send order.
CREATE INDEX IF NOT EXISTS messages_prose_idx ON messages(role, created_at) WHERE kind='message';
-- Derived per user message by the authorship rebuild; see docs/human-authorship.md.
CREATE TABLE IF NOT EXISTS message_authorship (
  message_id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  sent_at TEXT NOT NULL,
  day TEXT NOT NULL,
  total_chars INTEGER NOT NULL,
  typed_chars INTEGER NOT NULL DEFAULT 0,
  typed_words INTEGER NOT NULL DEFAULT 0,
  harness_chars INTEGER NOT NULL DEFAULT 0,
  harness_words INTEGER NOT NULL DEFAULT 0,
  automated_chars INTEGER NOT NULL DEFAULT 0,
  automated_words INTEGER NOT NULL DEFAULT 0,
  template_chars INTEGER NOT NULL DEFAULT 0,
  template_words INTEGER NOT NULL DEFAULT 0,
  attachment_chars INTEGER NOT NULL DEFAULT 0,
  attachment_words INTEGER NOT NULL DEFAULT 0,
  quoted_chars INTEGER NOT NULL DEFAULT 0,
  quoted_words INTEGER NOT NULL DEFAULT 0,
  resent_chars INTEGER NOT NULL DEFAULT 0,
  resent_words INTEGER NOT NULL DEFAULT 0,
  pasted_chars INTEGER NOT NULL DEFAULT 0,
  pasted_words INTEGER NOT NULL DEFAULT 0,
  spans_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS message_authorship_conversation_idx ON message_authorship(conversation_id);
CREATE INDEX IF NOT EXISTS message_authorship_day_idx ON message_authorship(day);
-- Prose messages by role and time, for the human-authorship rebuild.
CREATE INDEX IF NOT EXISTS messages_prose_idx ON messages(role, created_at) WHERE kind='message';
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(message_id UNINDEXED, text, tokenize='unicode61');
CREATE TABLE IF NOT EXISTS message_fts_rows (
  message_id TEXT PRIMARY KEY,
  fts_rowid INTEGER NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS agent_sessions (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  native_id TEXT NOT NULL,
  parent_id TEXT REFERENCES agent_sessions(id) ON DELETE CASCADE,
  delegation_message_id TEXT REFERENCES messages(id) ON DELETE SET NULL,
  kind TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT,
  depth INTEGER NOT NULL DEFAULT 0,
  started_at TEXT,
  ended_at TEXT,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  uncached_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_5m_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_1h_input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_output_tokens INTEGER NOT NULL DEFAULT 0,
  unclassified_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  usage_status TEXT NOT NULL DEFAULT 'unavailable',
  UNIQUE(conversation_id, native_id)
);
CREATE INDEX IF NOT EXISTS agent_sessions_conversation_idx ON agent_sessions(conversation_id, depth);
CREATE INDEX IF NOT EXISTS agent_sessions_delegation_message_idx ON agent_sessions(delegation_message_id);
-- The Library's subagent columns count sessions per workspace inside its
-- maintenance write transactions; without this they scan the whole table.
CREATE INDEX IF NOT EXISTS agent_sessions_workspace_idx ON agent_sessions(workspace_id, native_id);

-- Hourly usage ledger beneath agent_sessions: each session's reconciled usage
-- split by the UTC hour and model of the accounting events that reported it.
-- 'session-start' rows had no event timestamp and sit at the session start.
CREATE TABLE IF NOT EXISTS agent_session_usage (
  agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
  usage_hour TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  attribution TEXT NOT NULL,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  uncached_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_5m_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_1h_input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_output_tokens INTEGER NOT NULL DEFAULT 0,
  unclassified_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(agent_session_id, usage_hour, model, attribution)
);
CREATE INDEX IF NOT EXISTS agent_session_usage_hour_idx ON agent_session_usage(usage_hour);

-- API price history. Rows are imported from the checked-in pricing file
-- (pricing/cost_changes.json); only 'confirmed' rows are used for cost.
-- assumption=1 marks a price no provider published (see the row's notes).
-- Rates are USD per million tokens; NULL means the provider has no such rate.
CREATE TABLE IF NOT EXISTS cost_changes (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  model_key TEXT NOT NULL,
  effective_from TEXT NOT NULL,
  input_per_mtok REAL,
  cache_read_per_mtok REAL,
  cache_write_5m_per_mtok REAL,
  cache_write_1h_per_mtok REAL,
  output_per_mtok REAL,
  status TEXT NOT NULL,
  assumption INTEGER NOT NULL DEFAULT 0,
  source_url TEXT NOT NULL,
  source_title TEXT,
  notes TEXT,
  retrieved_at TEXT
);
CREATE TABLE IF NOT EXISTS model_aliases (
  id TEXT PRIMARY KEY,
  alias TEXT NOT NULL,
  alias_key TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  model_key TEXT NOT NULL,
  effective_from TEXT NOT NULL,
  status TEXT NOT NULL,
  source_url TEXT NOT NULL,
  source_title TEXT,
  notes TEXT
);
-- Cost on Date: each confirmed price with the half-open interval
-- [effective_from, effective_to) it applies to. Join usage on
-- model_key = ? AND day >= effective_from AND (effective_to IS NULL OR day < effective_to).
CREATE VIEW IF NOT EXISTS cost_on_date AS
SELECT model_key, provider, model, effective_from,
  LEAD(effective_from) OVER (PARTITION BY model_key ORDER BY effective_from) effective_to,
  input_per_mtok, cache_read_per_mtok, cache_write_5m_per_mtok, cache_write_1h_per_mtok, output_per_mtok,
  assumption, id cost_change_id, source_url
FROM cost_changes WHERE status='confirmed';
CREATE VIEW IF NOT EXISTS model_alias_on_date AS
SELECT alias_key, alias, provider, model, model_key, effective_from,
  LEAD(effective_from) OVER (PARTITION BY alias_key ORDER BY effective_from) effective_to,
  id model_alias_id, source_url
FROM model_aliases WHERE status='confirmed';

-- Tool ledger: one row per model request and per tool call, derived from
-- retained messages (see internal/archive/tool_ledger.go). Rebuilt with the
-- conversation on ingest; tool_ledger_state records which derivation built it.
CREATE TABLE IF NOT EXISTS model_requests (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  agent_session_id TEXT,
  native_id TEXT,
  sequence INTEGER NOT NULL,
  requested_at TEXT,
  model TEXT,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  uncached_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  context_growth_tokens INTEGER,
  compacted_before INTEGER NOT NULL DEFAULT 0,
  tool_call_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS model_requests_conversation_idx ON model_requests(conversation_id, sequence);

CREATE TABLE IF NOT EXISTS tool_calls (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  agent_session_id TEXT,
  call_id TEXT,
  call_message_id TEXT,
  result_message_id TEXT,
  sequence INTEGER NOT NULL,
  provider TEXT NOT NULL,
  model TEXT,
  kind TEXT NOT NULL,
  tool_name TEXT NOT NULL,
  tool_category TEXT NOT NULL,
  mcp_server TEXT,
  command TEXT,
  program TEXT,
  subcommand TEXT,
  command_category TEXT,
  command_count INTEGER NOT NULL DEFAULT 0,
  has_pipe INTEGER NOT NULL DEFAULT 0,
  has_redirect INTEGER NOT NULL DEFAULT 0,
  has_heredoc INTEGER NOT NULL DEFAULT 0,
  backgrounded INTEGER NOT NULL DEFAULT 0,
  file_path TEXT,
  started_at TEXT,
  ended_at TEXT,
  duration_ms INTEGER,
  duration_source TEXT,
  status TEXT NOT NULL,
  error_type TEXT,
  exit_code INTEGER,
  interrupted INTEGER NOT NULL DEFAULT 0,
  truncated INTEGER NOT NULL DEFAULT 0,
  input_bytes INTEGER NOT NULL DEFAULT 0,
  result_bytes INTEGER NOT NULL DEFAULT 0,
  result_tokens INTEGER NOT NULL DEFAULT 0,
  result_tokens_source TEXT,
  request_id TEXT,
  next_request_id TEXT,
  parallel_count INTEGER NOT NULL DEFAULT 1,
  output_tokens REAL NOT NULL DEFAULT 0,
  carried_requests INTEGER NOT NULL DEFAULT 0,
  carried_tokens INTEGER NOT NULL DEFAULT 0,
  lines_added INTEGER,
  lines_removed INTEGER
);
CREATE INDEX IF NOT EXISTS tool_calls_conversation_idx ON tool_calls(conversation_id, sequence);
CREATE INDEX IF NOT EXISTS tool_calls_workspace_idx ON tool_calls(workspace_id);
CREATE INDEX IF NOT EXISTS tool_calls_started_idx ON tool_calls(started_at);

-- The simple commands of a shell tool call, in order. Operator is the shell
-- control operator joining a command to the previous one ("script" when a
-- Codex exec script started it as a separate process).
CREATE TABLE IF NOT EXISTS tool_commands (
  tool_call_id TEXT NOT NULL REFERENCES tool_calls(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  operator TEXT,
  command TEXT NOT NULL,
  program TEXT,
  subcommand TEXT,
  category TEXT,
  exit_code INTEGER,
  duration_ms INTEGER,
  PRIMARY KEY(tool_call_id, position)
);

-- Every URL a tool call reached, in order: URL arguments, network commands in
-- shell calls, pages a web tool opened, and links a web search returned
-- (source). tool_calls keeps the first URL and the hosts for filtering.
CREATE TABLE IF NOT EXISTS tool_urls (
  tool_call_id TEXT NOT NULL REFERENCES tool_calls(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  url TEXT NOT NULL,
  host TEXT,
  source TEXT NOT NULL,
  PRIMARY KEY(tool_call_id, position)
);
CREATE INDEX IF NOT EXISTS tool_urls_host_idx ON tool_urls(host);

CREATE TABLE IF NOT EXISTS tool_ledger_state (
  conversation_id TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
  version TEXT NOT NULL,
  tool_calls INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);

-- Daily tool rollup served by the Tools table, rebuilt from tool_calls when
-- the ledger changes. Mirrored workspaces (see suppressMirrors) are excluded
-- here and listed in tool_mirror_workspaces for per-call queries.
CREATE TABLE IF NOT EXISTS tool_usage_daily (
  id TEXT PRIMARY KEY,
  day TEXT,
  week TEXT,
  month TEXT,
  repository_name TEXT,
  source_kind TEXT,
  provider TEXT,
  model TEXT,
  model_family TEXT,
  session_kind TEXT,
  tool_name TEXT,
  tool_category TEXT,
  mcp_server TEXT,
  program TEXT,
  subcommand TEXT,
  command_name TEXT,
  command_category TEXT,
  call_count INTEGER NOT NULL DEFAULT 0,
  error_count INTEGER NOT NULL DEFAULT 0,
  error_rate REAL,
  no_result_count INTEGER NOT NULL DEFAULT 0,
  rejected_count INTEGER NOT NULL DEFAULT 0,
  interrupted_count INTEGER NOT NULL DEFAULT 0,
  timeout_count INTEGER NOT NULL DEFAULT 0,
  nonzero_exit_count INTEGER NOT NULL DEFAULT 0,
  hook_blocked_count INTEGER NOT NULL DEFAULT 0,
  truncated_count INTEGER NOT NULL DEFAULT 0,
  timed_count INTEGER NOT NULL DEFAULT 0,
  total_duration_ms INTEGER NOT NULL DEFAULT 0,
  avg_duration_ms REAL,
  max_duration_ms INTEGER,
  input_bytes INTEGER NOT NULL DEFAULT 0,
  result_bytes INTEGER NOT NULL DEFAULT 0,
  result_tokens INTEGER NOT NULL DEFAULT 0,
  measured_count INTEGER NOT NULL DEFAULT 0,
  avg_result_tokens REAL,
  carried_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens REAL NOT NULL DEFAULT 0,
  lines_added INTEGER NOT NULL DEFAULT 0,
  lines_removed INTEGER NOT NULL DEFAULT 0,
  work_count INTEGER NOT NULL DEFAULT 0,
  context_cost_usd REAL,
  output_cost_usd REAL,
  tool_cost_usd REAL,
  price_status TEXT
);
CREATE TABLE IF NOT EXISTS tool_mirror_workspaces (
  workspace_id TEXT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS agent_session_messages (
  agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  source_order INTEGER NOT NULL,
  PRIMARY KEY(agent_session_id, message_id)
);
CREATE INDEX IF NOT EXISTS agent_session_messages_message_idx ON agent_session_messages(message_id);

CREATE TABLE IF NOT EXISTS summaries (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  initiation TEXT,
  approaches TEXT,
  outcome TEXT,
  failures TEXT,
  unresolved TEXT,
  evidence_json TEXT NOT NULL,
  model TEXT NOT NULL,
  extractor_version TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS semantic_documents (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  text_hash TEXT NOT NULL,
  vector_json TEXT NOT NULL,
  dimensions INTEGER NOT NULL,
  model TEXT NOT NULL,
  indexed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_attempts (
  id TEXT PRIMARY KEY,
  work_item_id TEXT NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
  source_id TEXT NOT NULL,
  source_ref TEXT,
  attempt_no INTEGER,
  parent_id TEXT,
  configuration_json TEXT NOT NULL DEFAULT '{}',
  instructions TEXT,
  result TEXT,
  failure TEXT,
  validation TEXT,
  started_at TEXT,
  ended_at TEXT,
  UNIQUE(work_item_id, source_id)
);

CREATE TABLE IF NOT EXISTS handoffs (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  candidate_id TEXT,
  predecessor_id TEXT,
  successor_id TEXT,
  custody_holder TEXT,
  custody_version TEXT,
  checkpoint_json TEXT NOT NULL DEFAULT '{}',
  integration_evidence_json TEXT NOT NULL DEFAULT '{}',
  closure_evidence_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT
);

CREATE TABLE IF NOT EXISTS change_sets (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  attempt_id TEXT REFERENCES task_attempts(id) ON DELETE SET NULL,
  base_id TEXT,
  head_id TEXT,
  classification TEXT NOT NULL,
  patch_locator TEXT,
  complete INTEGER NOT NULL DEFAULT 1,
  created_at TEXT
);
CREATE INDEX IF NOT EXISTS change_sets_workspace_idx ON change_sets(workspace_id);

CREATE TABLE IF NOT EXISTS change_files (
  change_set_id TEXT NOT NULL REFERENCES change_sets(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  old_path TEXT,
  status TEXT NOT NULL,
  tracked INTEGER NOT NULL,
  bytes INTEGER,
  evidence_locator TEXT,
  PRIMARY KEY(change_set_id, path)
);
CREATE INDEX IF NOT EXISTS change_files_path_idx ON change_files(path);

CREATE TABLE IF NOT EXISTS pull_requests (
  id TEXT PRIMARY KEY,
  host TEXT NOT NULL,
  repository_id TEXT REFERENCES repositories(id),
  number INTEGER NOT NULL,
  url TEXT,
  title TEXT,
  state TEXT,
  base_ref TEXT,
  head_ref TEXT,
  commit_refs_json TEXT NOT NULL DEFAULT '[]',
  observed_at TEXT,
  UNIQUE(host, repository_id, number)
);

CREATE TABLE IF NOT EXISTS work_pr_links (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  pr_id TEXT NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
  relationship TEXT NOT NULL,
  confidence REAL NOT NULL,
  evidence_json TEXT NOT NULL,
  PRIMARY KEY(workspace_id, pr_id, relationship)
);

CREATE TABLE IF NOT EXISTS metrics (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  conversation_id TEXT REFERENCES conversations(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  value REAL,
  unit TEXT NOT NULL,
  status TEXT NOT NULL,
  extractor_version TEXT NOT NULL,
  coverage REAL,
  definition TEXT,
  observed_at TEXT,
  UNIQUE(workspace_id, conversation_id, name, extractor_version)
);
CREATE TABLE IF NOT EXISTS workspace_query_stats (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  token_count REAL
);
CREATE INDEX IF NOT EXISTS workspace_query_stats_tokens_idx
  ON workspace_query_stats(token_count DESC,workspace_id);
CREATE TRIGGER IF NOT EXISTS workspace_query_stats_workspace_insert
AFTER INSERT ON workspaces BEGIN
  INSERT OR IGNORE INTO workspace_query_stats(workspace_id) VALUES(NEW.id);
END;
CREATE TRIGGER IF NOT EXISTS workspace_query_stats_metrics_insert
AFTER INSERT ON metrics BEGIN
  INSERT INTO workspace_query_stats(workspace_id,token_count)
  VALUES(NEW.workspace_id,COALESCE(
    (SELECT MAX(value) FROM metrics WHERE workspace_id=NEW.workspace_id AND name='total_tokens'),
    (SELECT SUM(value) FROM metrics WHERE workspace_id=NEW.workspace_id AND name IN ('input_tokens','output_tokens','cache_tokens'))))
  ON CONFLICT(workspace_id) DO UPDATE SET token_count=excluded.token_count;
END;
CREATE TRIGGER IF NOT EXISTS workspace_query_stats_metrics_update
AFTER UPDATE ON metrics BEGIN
  UPDATE workspace_query_stats SET token_count=COALESCE(
    (SELECT MAX(value) FROM metrics WHERE workspace_id=OLD.workspace_id AND name='total_tokens'),
    (SELECT SUM(value) FROM metrics WHERE workspace_id=OLD.workspace_id AND name IN ('input_tokens','output_tokens','cache_tokens')))
  WHERE workspace_id=OLD.workspace_id;
  INSERT INTO workspace_query_stats(workspace_id,token_count)
  VALUES(NEW.workspace_id,COALESCE(
    (SELECT MAX(value) FROM metrics WHERE workspace_id=NEW.workspace_id AND name='total_tokens'),
    (SELECT SUM(value) FROM metrics WHERE workspace_id=NEW.workspace_id AND name IN ('input_tokens','output_tokens','cache_tokens'))))
  ON CONFLICT(workspace_id) DO UPDATE SET token_count=excluded.token_count;
END;
CREATE TRIGGER IF NOT EXISTS workspace_query_stats_metrics_delete
AFTER DELETE ON metrics BEGIN
  UPDATE workspace_query_stats SET token_count=COALESCE(
    (SELECT MAX(value) FROM metrics WHERE workspace_id=OLD.workspace_id AND name='total_tokens'),
    (SELECT SUM(value) FROM metrics WHERE workspace_id=OLD.workspace_id AND name IN ('input_tokens','output_tokens','cache_tokens')))
  WHERE workspace_id=OLD.workspace_id;
END;
CREATE TABLE IF NOT EXISTS metric_ledger (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  sequence INTEGER NOT NULL,
  event_kind TEXT NOT NULL,
  duration_ms INTEGER,
  input_tokens INTEGER,
  output_tokens INTEGER,
  cache_tokens INTEGER,
  error_type TEXT,
  evidence_locator TEXT,
  UNIQUE(workspace_id, sequence, event_kind)
);

-- One library can travel between Macs. A host is one Mac and OS user; source
-- paths, sync state, and sightings are only meaningful on the host that saw
-- them. Identities (workspaces, conversations, messages) stay host-independent.
-- conversations.origin_host_id with conversations.origin names the copy that
-- last wrote the whole conversation (NULL: meta legacy_host_id).
CREATE TABLE IF NOT EXISTS hosts (
  id TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  user_name TEXT,
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS source_states (
  host_id TEXT NOT NULL,
  source_name TEXT NOT NULL,
  kind TEXT NOT NULL,
  capability TEXT NOT NULL,
  cursor TEXT,
  fingerprint TEXT,
  coverage TEXT NOT NULL,
  last_attempt_at TEXT,
  last_success_at TEXT,
  error TEXT,
  pending_count INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(host_id,source_name)
);

CREATE TABLE IF NOT EXISTS source_record_states (
  host_id TEXT NOT NULL,
  source_name TEXT NOT NULL,
  source_kind TEXT NOT NULL,
  source_account TEXT NOT NULL,
  source_id TEXT NOT NULL,
  digest TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(host_id,source_name,source_kind,source_account,source_id)
);
CREATE INDEX IF NOT EXISTS source_record_states_record_idx ON source_record_states(source_kind,source_account,source_id);

-- What an ingest last parsed of each part of a source, per host, so parts that
-- have not changed are not parsed again: a transcript file (item is its
-- original path; size in bytes, version its mtime_ns) or a database session
-- (item sqlite:<original path>:sessions:<id>; size its message count, version
-- its highest message rowid, signal a digest of its other inputs).
CREATE TABLE IF NOT EXISTS source_item_states (
  host_id TEXT NOT NULL,
  source_name TEXT NOT NULL,
  item TEXT NOT NULL,
  extractor TEXT NOT NULL,
  size INTEGER NOT NULL,
  version INTEGER NOT NULL,
  signal TEXT,
  indexed_at TEXT NOT NULL,
  PRIMARY KEY(host_id,source_name,item)
);

-- The source version (see observe in adapters.go) each host last wrote a
-- workspace's own fields (conversation_id '') and each conversation copy
-- (conversation_id, origin) from, on that host's clock. A capture is never
-- written over a newer one.
CREATE TABLE IF NOT EXISTS copy_versions (
  host_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  conversation_id TEXT NOT NULL DEFAULT '',
  origin TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL,
  written_at TEXT NOT NULL,
  PRIMARY KEY(host_id,workspace_id,conversation_id,origin)
);

-- Where each host saw a workspace or conversation, for provenance and for
-- running Git only against paths that exist on this host.
CREATE TABLE IF NOT EXISTS workspace_sightings (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  host_id TEXT NOT NULL,
  source_name TEXT NOT NULL,
  location TEXT,
  repository_locations_json TEXT NOT NULL DEFAULT '[]',
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id,host_id,source_name)
);
CREATE INDEX IF NOT EXISTS workspace_sightings_host_idx ON workspace_sightings(host_id,source_name);

CREATE TABLE IF NOT EXISTS conversation_sightings (
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  host_id TEXT NOT NULL,
  origin TEXT NOT NULL,
  source_name TEXT NOT NULL,
  messages INTEGER,
  ended_at TEXT,
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  PRIMARY KEY(conversation_id,host_id,origin)
);

CREATE TABLE IF NOT EXISTS protections (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope_type TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  until_at TEXT,
  reason TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(scope_type, scope_id, mode)
);

CREATE TABLE IF NOT EXISTS activity_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  source TEXT NOT NULL,
  kind TEXT NOT NULL,
  meaningful INTEGER NOT NULL,
  occurred_at TEXT NOT NULL,
  evidence_json TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS receipts (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  operation_id TEXT NOT NULL UNIQUE,
  source_revision TEXT NOT NULL,
  package_path TEXT NOT NULL,
  package_hash TEXT NOT NULL,
  package_bytes INTEGER NOT NULL,
  retained_json TEXT NOT NULL,
  omitted_json TEXT NOT NULL,
  policy_json TEXT NOT NULL,
  activity_json TEXT NOT NULL,
  extraction_json TEXT NOT NULL,
  deletion_json TEXT NOT NULL,
  outcome TEXT NOT NULL,
  before_bytes INTEGER,
  after_bytes INTEGER,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS reclamation (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),
  state TEXT NOT NULL,
  scheduled_at TEXT,
  operation_id TEXT UNIQUE,
  expected_revision TEXT,
  expected_custody_version TEXT,
  exact_resources_json TEXT NOT NULL DEFAULT '[]',
  estimated_reclaimable_bytes INTEGER,
  estimated_retained_bytes INTEGER,
  blocker TEXT,
  warning TEXT,
  intent_at TEXT,
  result_json TEXT,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  key TEXT NOT NULL UNIQUE,
  priority INTEGER NOT NULL,
  heavy INTEGER NOT NULL,
  state TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  not_before TEXT,
  claimed_at TEXT,
  error TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_ready_idx ON jobs(state, heavy, priority, not_before);

-- One row per user message of the work each mirror group is represented by,
-- derived from messages by RebuildAuthorship. Character and word counts are
-- per span category; spans_json holds the labelled byte ranges and reasons.
CREATE TABLE IF NOT EXISTS message_authorship (
  message_id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  sent_at TEXT NOT NULL,
  day TEXT NOT NULL,
  total_chars INTEGER NOT NULL,
  typed_chars INTEGER NOT NULL,
  typed_words INTEGER NOT NULL,
  harness_chars INTEGER NOT NULL,
  automated_chars INTEGER NOT NULL,
  template_chars INTEGER NOT NULL,
  attachment_chars INTEGER NOT NULL,
  quoted_chars INTEGER NOT NULL,
  resent_chars INTEGER NOT NULL,
  pasted_chars INTEGER NOT NULL,
  pasted_words INTEGER NOT NULL,
  spans_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS message_authorship_conversation_idx ON message_authorship(conversation_id);
CREATE INDEX IF NOT EXISTS message_authorship_day_idx ON message_authorship(day);
