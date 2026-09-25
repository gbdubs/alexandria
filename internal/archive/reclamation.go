//go:build reclamation

// Reclamation is mothballed. This file is excluded from default builds and is
// kept compiling under `-tags reclamation` so development can resume from it.
// See docs/reclamation/README.md for what was unwired and how to restore it.

package archive

import (
	"database/sql"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (c *Catalog) Protect(scopeType, scopeID, mode, until, reason string) error {
	if mode != "protect" && mode != "snooze" {
		return fmt.Errorf("mode must be protect or snooze")
	}
	_, err := c.DB.Exec(`INSERT INTO protections(scope_type,scope_id,mode,until_at,reason,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(scope_type,scope_id,mode) DO UPDATE SET until_at=excluded.until_at,reason=excluded.reason`, scopeType, scopeID, mode, nilIfEmpty(iso(until)), nilIfEmpty(reason), now())
	return err
}
func (c *Catalog) Unprotect(scopeType, scopeID string) (int64, error) {
	result, err := c.DB.Exec("DELETE FROM protections WHERE scope_type=? AND scope_id=?", scopeType, scopeID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
func (c *Catalog) protections(workspaceID string) ([]map[string]any, error) {
	var repo sql.NullString
	if err := c.DB.QueryRow("SELECT repository_id FROM workspaces WHERE id=?", workspaceID).Scan(&repo); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	return queryMaps(c.DB, `SELECT * FROM protections WHERE (scope_type='workspace' AND scope_id=?) OR (scope_type='repository' AND scope_id=?) OR (scope_type='work_item' AND scope_id IN (SELECT id FROM work_items WHERE workspace_id=?))`, workspaceID, repo.String, workspaceID)
}

func (c *Catalog) RefreshUpcoming(config Config) ([]map[string]any, error) {
	rows, err := queryMaps(c.DB, `SELECT * FROM workspaces WHERE source_kind IN ('tl1','tl1-export') AND reclamation_authority=1 ORDER BY activity_at`)
	if err != nil {
		return nil, err
	}
	current := time.Now().UTC()
	output := []map[string]any{}
	for _, row := range rows {
		id := firstString(row["id"])
		activity, hasActivity := parseTime(firstString(row["activity_at"]))
		protections, err := c.protections(id)
		if err != nil {
			return nil, err
		}
		var active map[string]any
		for _, protection := range protections {
			until, hasUntil := parseTime(firstString(protection["until_at"]))
			if firstString(protection["mode"]) == "protect" || (hasUntil && until.After(current)) {
				active = protection
				break
			}
		}
		state, blocker, scheduled := "", "", ""
		if active != nil {
			state = "protected"
			blocker = defaultString(active["reason"], firstString(active["mode"]))
		} else if !hasActivity || firstString(row["activity_source"]) == "" {
			state = "blocked"
			blocker = "meaningful owner activity is unknown"
		} else {
			scheduledTime := activity.AddDate(0, 0, config.EligibleDays)
			upcoming := activity.AddDate(0, 0, config.UpcomingDays)
			scheduled = formatTime(scheduledTime)
			if !current.Before(scheduledTime) || !current.Before(upcoming) {
				state = "upcoming"
			} else {
				state = "discovered"
			}
		}
		prior, _ := queryMaps(c.DB, "SELECT state,operation_id,scheduled_at FROM reclamation WHERE workspace_id=?", id)
		if active == nil && len(prior) > 0 {
			priorState := firstString(prior[0]["state"])
			if (priorState == "upcoming" || priorState == "preparing" || priorState == "verified") && scheduled != "" && firstString(prior[0]["scheduled_at"]) != "" {
				newDate, _ := parseTime(scheduled)
				oldDate, _ := parseTime(firstString(prior[0]["scheduled_at"]))
				if newDate.After(oldDate) {
					until := formatTime(current.AddDate(0, 0, config.SnoozeDays))
					if err := c.Protect("workspace", id, "snooze", until, "Automatically snoozed after reopened activity"); err != nil {
						return nil, err
					}
					state = "protected"
					blocker = "Automatically snoozed after reopened activity"
				}
			} else if priorState == "preparing" || priorState == "verified" || priorState == "reclaiming" || priorState == "reclaimed" || priorState == "partially_reclaimed" {
				state = priorState
			}
		}
		_, err = c.DB.Exec(`INSERT INTO reclamation(workspace_id,state,scheduled_at,blocker,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(workspace_id) DO UPDATE SET state=CASE WHEN reclamation.state='reclaimed' THEN reclamation.state ELSE excluded.state END,scheduled_at=excluded.scheduled_at,blocker=excluded.blocker,updated_at=excluded.updated_at`, id, state, nilIfEmpty(scheduled), nilIfEmpty(blocker), now())
		if err != nil {
			return nil, err
		}
		output = append(output, map[string]any{"workspace_id": id, "state": state, "scheduled_at": nilIfEmpty(scheduled), "blocker": nilIfEmpty(blocker)})
	}
	return output, nil
}
func (c *Catalog) Upcoming() (map[string]any, error) {
	items, err := queryMaps(c.DB, `SELECT r.*,w.title,w.source_id,w.activity_at,w.activity_source,w.location,EXISTS(SELECT 1 FROM protections p WHERE p.scope_type='workspace' AND p.scope_id=w.id) protected FROM reclamation r JOIN workspaces w ON w.id=r.workspace_id WHERE r.state IN ('upcoming','protected','blocked','preparing','verified','reclaiming') ORDER BY r.scheduled_at`)
	return map[string]any{"items": items}, err
}

// reclamationHealth summarized queue bytes for /api/health under the
// "reclamation" key (rendered as the "Eligible TL1" and "Measured reclaimed"
// Health cards).
func (c *Catalog) reclamationHealth() map[string]any {
	reclamation := map[string]any{"total": int64(0), "blocked_bytes": int64(0), "eligible_bytes": int64(0), "reclaimed_bytes": int64(0)}
	rows, err := queryMaps(c.DB, `SELECT COUNT(*) total,
		COALESCE(SUM(CASE WHEN state='blocked' THEN estimated_reclaimable_bytes ELSE 0 END),0) blocked_bytes,
		COALESCE(SUM(CASE WHEN state IN ('upcoming','verified') THEN estimated_reclaimable_bytes ELSE 0 END),0) eligible_bytes,
		COALESCE(SUM(CASE WHEN state='reclaimed' THEN MAX(before_bytes-after_bytes,0) ELSE 0 END),0) reclaimed_bytes
		FROM reclamation LEFT JOIN receipts USING(workspace_id)`)
	if err == nil && len(rows) == 1 {
		reclamation = rows[0]
	}
	return reclamation
}

// upcomingRows backed the "upcoming" query-table dataset (queryTableRows).
func (s *Server) upcomingRows() ([]map[string]any, error) {
	if _, err := s.Catalog.RefreshUpcoming(s.Config()); err != nil {
		return nil, err
	}
	payload, err := s.Catalog.Upcoming()
	if err != nil {
		return nil, err
	}
	rows, _ := payload["items"].([]map[string]any)
	return rows, nil
}

// serveUpcoming handled GET /api/upcoming.
func (s *Server) serveUpcoming(w http.ResponseWriter) {
	_, err := s.Catalog.RefreshUpcoming(s.Config())
	if err != nil {
		writeError(w, err, 500)
		return
	}
	value, err := s.Catalog.Upcoming()
	writeResult(w, value, err)
}

// serveProtect handled POST /api/protect/{workspace_id}.
func (s *Server) serveProtect(w http.ResponseWriter, path string, body map[string]any) {
	id := strings.TrimPrefix(path, "/api/protect/")
	mode := defaultString(body["mode"], "protect")
	if mode == "remove" {
		removed, err := s.Catalog.Unprotect("workspace", id)
		if err == nil {
			_, err = s.Catalog.RefreshUpcoming(s.Config())
		}
		writeResult(w, map[string]any{"ok": err == nil, "workspace_id": id, "removed": removed}, err)
		return
	}
	until := firstString(body["until_at"])
	if mode == "snooze" && until == "" {
		until = formatTime(time.Now().AddDate(0, 0, s.Config().SnoozeDays))
	}
	err := s.Catalog.Protect("workspace", id, mode, until, firstString(body["reason"]))
	if err == nil {
		_, err = s.Catalog.RefreshUpcoming(s.Config())
	}
	writeResult(w, map[string]any{"ok": err == nil, "workspace_id": id, "mode": mode, "until_at": nilIfEmpty(until)}, err)
}

// runUpcomingCLI handled `alexandria upcoming`.
func runUpcomingCLI(catalog *Catalog, config Config) error {
	if _, err := catalog.RefreshUpcoming(config); err != nil {
		return err
	}
	value, err := catalog.Upcoming()
	if err != nil {
		return err
	}
	return printJSON(value)
}

// runProtectCLI handled `alexandria protect`.
func runProtectCLI(catalog *Catalog, config Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: alexandria protect {repository,workspace,work_item} ID [--snooze-until DATE] [--reason TEXT] [--remove]")
	}
	scopeType, scopeID := args[0], args[1]
	if scopeType != "repository" && scopeType != "workspace" && scopeType != "work_item" {
		return fmt.Errorf("invalid scope type: %s", scopeType)
	}
	flags := flag.NewFlagSet("protect", flag.ContinueOnError)
	until := flags.String("snooze-until", "", "")
	reason := flags.String("reason", "", "")
	remove := flags.Bool("remove", false, "")
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if *remove {
		count, err := catalog.Unprotect(scopeType, scopeID)
		if err != nil {
			return err
		}
		_, _ = catalog.RefreshUpcoming(config)
		return printJSON(map[string]any{"ok": true, "removed": count})
	}
	mode := "protect"
	if *until != "" {
		mode = "snooze"
	}
	if err := catalog.Protect(scopeType, scopeID, mode, *until, *reason); err != nil {
		return err
	}
	_, _ = catalog.RefreshUpcoming(config)
	return printJSON(map[string]any{"ok": true, "mode": mode})
}
