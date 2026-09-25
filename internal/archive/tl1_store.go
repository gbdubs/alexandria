package archive

import (
	"context"
	"database/sql"
	"sort"
	"strings"
)

// The tl1_* tables mirror TL1's workflow state per installation for analysis:
// flavors (the workflow definition), tasks and attempts (what ran, with which
// agent configuration, and how it ended), candidates, events, review findings,
// and human touches. Each installation's rows are replaced together after a
// successful TL1 sync, or index of a TL1 capture. Tasks join Library
// workspaces by workspace_id; attempts join their transcript by
// conversation_native_id. tl1_installations records the Mac the installation
// is on (host_id) with its original paths there, and the database version the
// rows were read from (source_version), so an older capture never replaces
// them.
const tl1SchemaSQL = `
CREATE TABLE IF NOT EXISTS tl1_installations (
  installation_id TEXT PRIMARY KEY,
  source_account TEXT NOT NULL,
  project TEXT,
  repository TEXT,
  database_path TEXT,
  config_path TEXT,
  transcripts_dir TEXT,
  synced_at TEXT NOT NULL,
  host_id TEXT,
  source_version INTEGER
);
CREATE TABLE IF NOT EXISTS tl1_flavors (
  installation_id TEXT NOT NULL,
  flavor_id TEXT NOT NULL,
  name TEXT NOT NULL,
  execution_class TEXT,
  executor TEXT,
  model TEXT,
  effort TEXT,
  candidate_behavior TEXT,
  read_only INTEGER NOT NULL DEFAULT 0,
  default_agent_configuration TEXT,
  agent_configurations_json TEXT,
  agent_preferences_json TEXT,
  transitions_json TEXT,
  outcomes_json TEXT,
  budgets_json TEXT,
  retry_config_json TEXT,
  inputs_schema_json TEXT,
  outputs_schema_json TEXT,
  escalation_shape TEXT,
  max_parallelism INTEGER,
  template TEXT,
  template_hash TEXT,
  shape_version TEXT,
  updated_at TEXT,
  PRIMARY KEY(installation_id, flavor_id)
);
CREATE TABLE IF NOT EXISTS tl1_tasks (
  installation_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  workspace_id TEXT,
  flavor TEXT,
  flavor_id TEXT,
  shape_version TEXT,
  execution_class TEXT,
  title TEXT,
  status TEXT,
  outcome TEXT,
  terminal_kind TEXT,
  terminal_reason TEXT,
  error_text TEXT,
  error_class TEXT,
  error_signature TEXT,
  error_attribution TEXT,
  candidate_id TEXT,
  parent_task_id TEXT,
  root_task_id TEXT,
  created_by_task TEXT,
  created_by_agent TEXT,
  assigned_configuration TEXT,
  suspension_count INTEGER,
  outputs_json TEXT,
  handoff_message TEXT,
  tags_json TEXT,
  created_at TEXT,
  claimed_at TEXT,
  completed_at TEXT,
  updated_at TEXT,
  PRIMARY KEY(installation_id, task_id)
);
CREATE INDEX IF NOT EXISTS tl1_tasks_candidate_idx ON tl1_tasks(installation_id, candidate_id);
CREATE INDEX IF NOT EXISTS tl1_tasks_parent_idx ON tl1_tasks(installation_id, parent_task_id);
CREATE TABLE IF NOT EXISTS tl1_attempts (
  installation_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  attempt_number INTEGER,
  configuration TEXT,
  executor TEXT,
  model TEXT,
  effort TEXT,
  harness_id TEXT,
  agent TEXT,
  selection_basis_json TEXT,
  status TEXT,
  failure_reason TEXT,
  error_class TEXT,
  error_signature TEXT,
  error_attribution TEXT,
  started_at TEXT,
  completed_at TEXT,
  duration_ms INTEGER,
  files_changed INTEGER,
  lines_added INTEGER,
  lines_removed INTEGER,
  peak_rss_mb REAL,
  avg_rss_mb REAL,
  peak_cpu_pct REAL,
  avg_cpu_pct REAL,
  reported_cost_usd REAL,
  reported_tool_errors INTEGER,
  transcripts_json TEXT,
  conversation_native_id TEXT,
  log_tail TEXT,
  PRIMARY KEY(installation_id, attempt_id)
);
CREATE INDEX IF NOT EXISTS tl1_attempts_task_idx ON tl1_attempts(installation_id, task_id);
CREATE TABLE IF NOT EXISTS tl1_candidates (
  installation_id TEXT NOT NULL,
  candidate_id TEXT NOT NULL,
  title TEXT,
  status TEXT,
  branch TEXT,
  base_sha TEXT,
  head_sha TEXT,
  pr_number INTEGER,
  pr_url TEXT,
  pr_state TEXT,
  integrated_sha TEXT,
  root_task_id TEXT,
  workflow_steps INTEGER,
  workflow_step_limit INTEGER,
  outcome_visits_json TEXT,
  tags_json TEXT,
  created_at TEXT,
  closed_at TEXT,
  PRIMARY KEY(installation_id, candidate_id)
);
CREATE TABLE IF NOT EXISTS tl1_events (
  installation_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  task_id TEXT,
  event_type TEXT NOT NULL,
  agent TEXT,
  detail_json TEXT,
  created_at TEXT,
  PRIMARY KEY(installation_id, event_id)
);
CREATE INDEX IF NOT EXISTS tl1_events_task_idx ON tl1_events(installation_id, task_id);
CREATE TABLE IF NOT EXISTS tl1_review_findings (
  installation_id TEXT NOT NULL,
  finding_id TEXT NOT NULL,
  candidate_id TEXT,
  review_task_id TEXT,
  attempt_id TEXT,
  severity TEXT,
  file_path TEXT,
  line INTEGER,
  explanation TEXT,
  recommendation TEXT,
  created_at TEXT,
  PRIMARY KEY(installation_id, finding_id)
);
CREATE TABLE IF NOT EXISTS tl1_human_touches (
  installation_id TEXT NOT NULL,
  touch_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  task_id TEXT,
  candidate_id TEXT,
  action TEXT,
  body TEXT,
  created_at TEXT,
  PRIMARY KEY(installation_id, touch_id)
);
CREATE TABLE IF NOT EXISTS tl1_configuration_health (
  installation_id TEXT NOT NULL,
  flavor_id TEXT NOT NULL,
  configuration_name TEXT NOT NULL,
  reason TEXT,
  next_probe_at TEXT,
  updated_at TEXT,
  PRIMARY KEY(installation_id, flavor_id, configuration_name)
);
`

// tl1SnapshotTables lists the per-installation tables in replacement order.
var tl1SnapshotTables = []string{"tl1_flavors", "tl1_tasks", "tl1_attempts", "tl1_candidates", "tl1_events", "tl1_review_findings", "tl1_human_touches", "tl1_configuration_health"}

func (c *Catalog) ensureTL1Schema() error {
	if _, err := c.DB.Exec(tl1SchemaSQL); err != nil {
		return err
	}
	for _, column := range []string{"host_id TEXT", "source_version INTEGER"} {
		has, err := c.hasColumn("tl1_installations", strings.Fields(column)[0])
		if err != nil {
			return err
		}
		if !has {
			if _, err := c.DB.Exec("ALTER TABLE tl1_installations ADD COLUMN " + column); err != nil {
				return err
			}
		}
	}
	return nil
}

// tl1SnapshotProvider is implemented by adapters that capture TL1 workflow
// state during Discover.
type tl1SnapshotProvider interface {
	tl1Snapshots() []tl1Snapshot
}

// storeTL1Snapshots replaces each installation's TL1 analysis rows in one
// transaction, so readers never see a partially replaced installation. host is
// the Mac the snapshots were read from. A snapshot read from a capture is
// passed over when the stored one was read from a newer version of the
// database (see guardCapture): TL1 is indexed whole, so replacing it would
// roll the analysis back. A live sync is authoritative, as for its records.
func (c *Catalog) storeTL1Snapshots(snapshots []tl1Snapshot, host string, fromCapture bool) error {
	for _, snapshot := range snapshots {
		tx, err := c.beginWrite(context.Background())
		if err != nil {
			return err
		}
		if fromCapture {
			stored, known, err := storedTL1Version(tx, snapshot.Installation.ID)
			if err != nil {
				tx.Rollback()
				return err
			}
			if known && snapshot.Version < stored {
				tx.Rollback()
				continue
			}
		}
		if err := storeTL1Snapshot(tx, snapshot, host); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// storedTL1Version is the database version an installation's stored rows were
// read from. Rows stored before versions were recorded compare against when
// they were synced, which is no earlier than the read they came from.
func storedTL1Version(tx *sql.Tx, installationID string) (int64, bool, error) {
	var version sql.NullInt64
	var synced sql.NullString
	err := tx.QueryRow("SELECT source_version,synced_at FROM tl1_installations WHERE installation_id=?", installationID).Scan(&version, &synced)
	if err == sql.ErrNoRows {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	if version.Valid {
		return version.Int64, true, nil
	}
	at, ok := parseTime(synced.String)
	return at.UnixNano(), ok, nil
}

func storeTL1Snapshot(tx *sql.Tx, snapshot tl1Snapshot, host string) error {
	id := snapshot.Installation.ID
	for _, table := range tl1SnapshotTables {
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE installation_id=?", id); err != nil {
			return err
		}
	}
	var version any
	if snapshot.Version > 0 {
		version = snapshot.Version
	}
	if _, err := tx.Exec(`INSERT INTO tl1_installations(installation_id,source_account,project,repository,database_path,config_path,transcripts_dir,synced_at,host_id,source_version)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(installation_id) DO UPDATE SET source_account=excluded.source_account,project=excluded.project,
		repository=excluded.repository,database_path=excluded.database_path,config_path=excluded.config_path,transcripts_dir=excluded.transcripts_dir,
		synced_at=excluded.synced_at,host_id=excluded.host_id,source_version=excluded.source_version`,
		id, snapshot.Account, nilIfEmpty(snapshot.Installation.Project), nilIfEmpty(snapshot.Installation.Repository),
		nilIfEmpty(snapshot.Installation.Database), nilIfEmpty(snapshot.Installation.ConfigPath), nilIfEmpty(snapshot.Installation.Transcripts), now(),
		nilIfEmpty(host), version); err != nil {
		return err
	}
	for table, rows := range map[string][]map[string]any{
		"tl1_flavors": snapshot.Flavors, "tl1_tasks": snapshot.Tasks, "tl1_attempts": snapshot.Attempts, "tl1_candidates": snapshot.Candidates,
		"tl1_events": snapshot.Events, "tl1_review_findings": snapshot.ReviewFindings, "tl1_human_touches": snapshot.HumanTouches,
		"tl1_configuration_health": snapshot.ConfigurationHealth,
	} {
		if err := insertTL1Rows(tx, table, rows); err != nil {
			return err
		}
	}
	return nil
}

// insertTL1Rows inserts rows sharing one column set, preparing the statement
// once per table.
func insertTL1Rows(tx *sql.Tx, table string, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	columns := make([]string, 0, len(rows[0]))
	for column := range rows[0] {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	statement, err := tx.Prepare("INSERT OR REPLACE INTO " + table + "(" + strings.Join(columns, ",") + ") VALUES(" + placeholders(len(columns)) + ")")
	if err != nil {
		return err
	}
	defer statement.Close()
	values := make([]any, len(columns))
	for _, row := range rows {
		for index, column := range columns {
			values[index] = row[column]
		}
		if _, err := statement.Exec(values...); err != nil {
			return err
		}
	}
	return nil
}
