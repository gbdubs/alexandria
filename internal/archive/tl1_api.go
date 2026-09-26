package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// getTL1 serves the TL1 tab:
//
//	GET /api/tl1                      installations and the projects they run (drives tab visibility)
//	GET /api/tl1/overview             analysis for one project
//	GET /api/tl1/flavor?name=         one flavor's definition, history, and prompt
//	GET /api/tl1/candidate?id=        one candidate's task timeline
//
// All accept project= (its installations on every Mac, merged), installation=
// (one Mac's), since= (ISO time, or "latest" for the latest large enqueue),
// until=, days=, and scope=all|current.
func (s *Server) getTL1(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	selection := tl1Selection{Project: query.Get("project"), Installation: query.Get("installation")}
	window := tl1WindowFromQuery(query)
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/api/tl1":
		installations, err := s.Catalog.tl1Installations()
		writeResult(w, map[string]any{"installations": installations, "projects": tl1Projects(installations)}, err)
	case "/api/tl1/overview":
		value, err := s.Catalog.TL1Overview(selection, window)
		writeResult(w, value, err)
	case "/api/tl1/flavor":
		value, err := s.Catalog.TL1Flavor(selection, query.Get("name"), window)
		writeResult(w, value, err)
	case "/api/tl1/candidate":
		value, err := s.Catalog.TL1Candidate(selection, query.Get("id"))
		writeResult(w, value, err)
	default:
		writeJSON(w, map[string]any{"error": "not found"}, http.StatusNotFound)
	}
}

// TL1Flavor returns a flavor's definition, per-configuration performance,
// definition history, recent failures, and a tuning prompt.
func (c *Catalog) TL1Flavor(selection tl1Selection, name string, window tl1Window) (map[string]any, error) {
	installations, err := c.selectTL1(selection)
	if err != nil {
		return nil, err
	}
	data, err := c.loadTL1(installations, window)
	if err != nil {
		return nil, err
	}
	flavor := data.Flavors[name]
	if flavor == nil {
		return nil, fmt.Errorf("unknown TL1 flavor %q", name)
	}
	matrix, _ := tl1Matrix(data)
	cells := []map[string]any{}
	for _, row := range matrix {
		if row["flavor"] == name {
			cells = append(cells, row)
		}
	}
	var summary map[string]any
	for _, row := range tl1FlavorSummaries(data) {
		if row["name"] == name {
			summary = row
		}
	}
	failures := []map[string]any{}
	incoming, outgoing := tl1Counter{}, tl1Counter{}
	for index := len(data.Tasks) - 1; index >= 0; index-- {
		task := data.Tasks[index]
		if task.Flavor == name && task.Parent != nil {
			incoming[task.Parent.Flavor+" —"+defaultString(task.Parent.Outcome, "(no outcome)")+"→"]++
		}
		if task.Parent != nil && task.Parent.Flavor == name {
			outgoing["—"+defaultString(task.Parent.Outcome, "(no outcome)")+"→ "+task.Flavor]++
		}
		if task.Flavor == name && task.ErrorClass != "" && len(failures) < 20 {
			failures = append(failures, tl1TaskRowJSON(data, task))
		}
	}
	definition := map[string]any{}
	for key, value := range flavor.Row {
		if strings.HasSuffix(key, "_json") {
			definition[strings.TrimSuffix(key, "_json")] = tl1JSONValue(value)
		} else if key != "installation_id" {
			definition[key] = value
		}
	}
	return map[string]any{"project": data.Project, "installations": data.Installations, "flavor": name, "definition": definition, "summary": summary, "configurations": cells,
		"incoming": incoming.top(15), "outgoing": outgoing.top(15), "recent_failures": failures,
		"prompt": tl1FlavorPrompt(data, name, "Find the changes most likely to raise its advance rate and lower its cost per useful result.")}, nil
}

func tl1JSONValue(value any) any {
	text := firstString(value)
	if text == "" {
		return nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return text
	}
	return decoded
}

func tl1TaskRowJSON(data *tl1Data, task *tl1Task) map[string]any {
	cost, priced, duration := 0.0, false, 0.0
	configurations := []string{}
	for _, attempt := range task.Attempts {
		if attempt.HasCost {
			cost += attempt.Cost
			priced = true
		}
		duration += attempt.Duration
		if attempt.Configuration != "" {
			configurations = append(configurations, attempt.Configuration)
		}
	}
	var final *tl1Attempt
	if count := len(task.Attempts); count > 0 {
		final = task.Attempts[count-1]
	}
	row := map[string]any{"task_id": task.ID, "workspace_id": task.WorkspaceID, "title": task.Title, "flavor": task.Flavor, "execution_class": task.ExecutionClass,
		"status": task.Status, "outcome": nilIfEmpty(task.Outcome), "disposition": task.disposition(), "error_class": nilIfEmpty(task.ErrorClass),
		"error_signature": nilIfEmpty(task.ErrorSignature), "error_text": nilIfEmpty(tl1Clip(task.ErrorText, 600)), "attempts": len(task.Attempts),
		"configurations": uniqueStrings(configurations), "cost_usd": tl1Optional(cost, priced), "duration_ms": tl1Optional(duration, duration > 0),
		"created_at": task.CreatedAt, "claimed_at": nilIfEmpty(task.ClaimedAt), "completed_at": nilIfEmpty(task.CompletedAt), "parent_task_id": nilIfEmpty(task.ParentID),
		"shape_version": nilIfEmpty(task.ShapeVersion), "revisit": task.Revisit, "installation_id": task.Installation["installation_id"], "mac": task.Installation["host_label"]}
	if final != nil {
		row["conversation_native_id"] = nilIfEmpty(final.Conversation)
		row["transcript_path"] = nilIfEmpty(tl1TranscriptPath(data, task, final))
		row["log_tail"] = nilIfEmpty(tl1Clip(final.LogTail, 1200))
		row["tokens"] = final.Tokens["total_tokens"]
	}
	return row
}

// TL1Candidate returns one candidate's tasks in order with their attempts,
// events, review findings, and human touches.
func (c *Catalog) TL1Candidate(selection tl1Selection, candidateID string) (map[string]any, error) {
	installations, err := c.selectTL1(selection)
	if err != nil {
		return nil, err
	}
	data, err := c.loadTL1(installations, tl1Window{})
	if err != nil {
		return nil, err
	}
	candidate := data.Candidates[candidateID]
	if candidate == nil {
		return nil, fmt.Errorf("unknown TL1 candidate %q", candidateID)
	}
	tasks := []map[string]any{}
	ids := map[string]bool{}
	total := 0.0
	for _, task := range data.Tasks {
		if task.CandidateID != candidateID {
			continue
		}
		ids[task.ID] = true
		row := tl1TaskRowJSON(data, task)
		if cost, ok := row["cost_usd"].(float64); ok {
			total += cost
		}
		tasks = append(tasks, row)
	}
	pick := func(rows []map[string]any) []map[string]any {
		kept := []map[string]any{}
		for _, row := range rows {
			if ids[firstString(row["task_id"], row["review_task_id"])] || firstString(row["candidate_id"]) == candidateID {
				kept = append(kept, row)
			}
		}
		sort.SliceStable(kept, func(i, j int) bool { return firstString(kept[i]["created_at"]) < firstString(kept[j]["created_at"]) })
		return kept
	}
	return map[string]any{"project": data.Project, "installations": data.Installations, "candidate": candidate, "cost_usd": total, "tasks": tasks,
		"events": pick(data.Events), "review_findings": pick(data.Findings), "human_touches": pick(data.Touches)}, nil
}

// cachedTL1AttemptRows returns tl1AttemptRows, kept until the next commit.
func (c *Catalog) cachedTL1AttemptRows(ctx context.Context) ([]map[string]any, error) {
	return cachedValue(ctx, c, "tl1-attempts:"+time.Local.String(), func(context.Context) ([]map[string]any, error) {
		return c.tl1AttemptRows()
	})
}

// tl1AttemptRows backs the tl1_attempts query table: one row per attempt in
// every project, on every Mac.
func (c *Catalog) tl1AttemptRows() ([]map[string]any, error) {
	installations, err := c.tl1Installations()
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, project := range tl1Projects(installations) {
		data, err := c.loadTL1(project["installations"].([]map[string]any), tl1Window{})
		if err != nil {
			return nil, err
		}
		for _, attempt := range data.Attempts {
			task := attempt.Task
			installation := task.Installation
			var tokens, cacheShare any
			if total := attempt.Tokens["total_tokens"]; total > 0 {
				tokens = total
				if input := attempt.Tokens["input_tokens"]; input > 0 {
					cacheShare = float64(attempt.Tokens["cache_read_input_tokens"]) / float64(input) * 100
				}
			}
			day := ""
			if started, ok := parseTime(attempt.StartedAt); ok {
				day = started.Local().Format("2006-01-02")
			}
			rows = append(rows, map[string]any{
				"id": firstString(installation["installation_id"]) + ":" + attempt.ID, "attempt_id": attempt.ID, "task_id": task.ID, "workspace_id": task.WorkspaceID,
				"project": installation["project"], "installation_id": installation["installation_id"], "mac": installation["host_label"], "title": task.Title, "flavor": task.Flavor, "execution_class": task.ExecutionClass,
				"shape_version": nilIfEmpty(short(task.ShapeVersion)), "configuration": nilIfEmpty(attempt.Configuration), "executor": nilIfEmpty(attempt.Executor),
				"model": nilIfEmpty(attempt.Model), "effort": nilIfEmpty(attempt.Effort), "outcome": nilIfEmpty(task.Outcome), "disposition": attempt.disposition(),
				"error_class": nilIfEmpty(attempt.ErrorClass), "error_attribution": nilIfEmpty(attempt.ErrorAttribution), "error_signature": nilIfEmpty(attempt.ErrorSignature),
				"candidate_id": nilIfEmpty(task.CandidateID), "started_at": nilIfEmpty(attempt.StartedAt), "task_created_at": nilIfEmpty(task.CreatedAt), "day": nilIfEmpty(day),
				"duration_ms": tl1Optional(attempt.Duration, attempt.HasDuration), "cost_usd": tl1Optional(attempt.Cost, attempt.HasCost),
				"cost_source": nilIfEmpty(attempt.CostSource), "total_tokens": tokens, "output_tokens": attempt.Tokens["output_tokens"],
				"reasoning_tokens": attempt.Tokens["reasoning_output_tokens"], "cache_read_share": cacheShare, "lines_changed": attempt.LinesChanged,
				"files_changed": attempt.FilesChanged, "peak_rss_mb": tl1Optional(attempt.PeakRSS, attempt.PeakRSS > 0), "revisit": task.Revisit,
				"has_transcript": attempt.Conversation != "", "attempt_count": 1, "error_count": boolInt(attempt.ErrorClass != ""),
				"advanced_count": boolInt(attempt.disposition() == tl1Advanced), "escalated_count": boolInt(attempt.disposition() == tl1Escalated),
			})
		}
	}
	return rows, nil
}
