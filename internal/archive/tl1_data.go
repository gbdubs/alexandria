package archive

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// tl1Data is one project's TL1 workflow history, loaded from the tl1_* tables
// and joined with priced token usage from the attempts' transcripts. A
// project runs as a separate installation on each Mac that has TL1; its
// history is theirs combined.
type tl1Data struct {
	Project string
	// Installations are the project's installations, most recently active
	// first.
	Installations []map[string]any
	Flavors       map[string]*tl1Flavor
	Tasks         []*tl1Task
	TaskByID      map[string]*tl1Task
	Attempts      []*tl1Attempt
	Candidates    map[string]map[string]any
	Events        []map[string]any
	Findings      []map[string]any
	Touches       []map[string]any
	Health        []map[string]any
	// AllTasks is the full history, ignoring the window; Enqueues (newest
	// first) are computed from it.
	AllTasks     []*tl1Task
	Enqueues     []tl1Enqueue
	Window       tl1Window
	Since, Until string
	Scope        string
}

type tl1Flavor struct {
	ID, Name, ExecutionClass, DefaultConfiguration, ShapeVersion, TemplateHash, Template string
	Configurations                                                                       map[string]any
	Transitions, Budgets, Outcomes                                                       map[string]any
	Row                                                                                  map[string]any
}

type tl1Task struct {
	ID, WorkspaceID, Flavor, ExecutionClass, ShapeVersion, Title, Status, Outcome string
	ErrorClass, ErrorSignature, ErrorAttribution, ErrorText                       string
	CandidateID, ParentID, CreatedBy, CreatedAt, ClaimedAt, CompletedAt           string
	// Installation is the tl1_installations row the task was read from.
	Installation map[string]any
	Parent       *tl1Task
	Children     []*tl1Task
	Attempts     []*tl1Attempt
	Revisit      bool
}

type tl1Attempt struct {
	ID, TaskID, Configuration, Executor, Model, Effort, Status           string
	ErrorClass, ErrorSignature, ErrorAttribution, FailureReason, LogTail string
	StartedAt, Conversation                                              string
	Transcripts                                                          []string
	Duration                                                             float64
	HasDuration                                                          bool
	LinesChanged, FilesChanged, PeakRSS                                  float64
	Cost                                                                 float64
	HasCost                                                              bool
	CostSource, PriceStatus, PricedModel                                 string
	Tokens                                                               map[string]int64
	Task                                                                 *tl1Task
	Final                                                                bool
}

// Attempt dispositions. "advanced" moved the workflow on without error or a
// human; "escalated" handed off to a human task; "retried" was superseded by
// another attempt of the same task.
const (
	tl1Advanced  = "advanced"
	tl1Escalated = "escalated"
	tl1Errored   = "error"
	tl1Retried   = "retried"
	tl1Open      = "open"
)

func (task *tl1Task) disposition() string {
	if task.ErrorClass != "" {
		return tl1Errored
	}
	for _, child := range task.Children {
		if child.ExecutionClass == "human" {
			return tl1Escalated
		}
	}
	if task.Outcome != "" {
		return tl1Advanced
	}
	return tl1Open
}

func (attempt *tl1Attempt) disposition() string {
	if attempt.ErrorClass != "" {
		return tl1Errored
	}
	if !attempt.Final {
		return tl1Retried
	}
	return attempt.Task.disposition()
}

// agentError reports errors the running configuration is responsible for.
// Infrastructure and provider failures would have hit any configuration.
func (attempt *tl1Attempt) agentError() bool {
	return attempt.ErrorClass != "" && attempt.ErrorAttribution != tl1AttributionInfrastructure && attempt.ErrorAttribution != tl1AttributionProvider
}

// tl1Installations lists every indexed installation, most recently active
// first, with the Mac it runs on.
func (c *Catalog) tl1Installations() ([]map[string]any, error) {
	return queryMaps(c.DB, `SELECT i.installation_id,i.project,i.repository,i.config_path,i.database_path,i.transcripts_dir,i.synced_at,i.host_id,h.label host_label,
		(SELECT COUNT(*) FROM tl1_tasks t WHERE t.installation_id=i.installation_id) tasks,
		(SELECT COUNT(*) FROM tl1_attempts a WHERE a.installation_id=i.installation_id) attempts,
		(SELECT MAX(t.created_at) FROM tl1_tasks t WHERE t.installation_id=i.installation_id) last_task_at
		FROM tl1_installations i LEFT JOIN hosts h ON h.id=i.host_id ORDER BY last_task_at DESC, tasks DESC`)
}

// tl1Project names the project an installation runs. The same project on
// two Macs is two installations of one project.
func tl1Project(installation map[string]any) string {
	return defaultString(installation["project"], firstString(installation["installation_id"]))
}

// tl1Projects groups installations (most recently active first) by project,
// keeping that order.
func tl1Projects(installations []map[string]any) []map[string]any {
	projects := []map[string]any{}
	byName := map[string]map[string]any{}
	for _, installation := range installations {
		name := tl1Project(installation)
		project := byName[name]
		if project == nil {
			project = map[string]any{"project": name, "tasks": int64(0), "attempts": int64(0), "last_task_at": installation["last_task_at"], "installations": []map[string]any{}}
			byName[name] = project
			projects = append(projects, project)
		}
		project["tasks"] = project["tasks"].(int64) + integer(installation["tasks"])
		project["attempts"] = project["attempts"].(int64) + integer(installation["attempts"])
		project["installations"] = append(project["installations"].([]map[string]any), installation)
	}
	return projects
}

// tl1Selection names what to analyze: a project on every Mac that runs it,
// or, with Installation, on one Mac. Neither selects the most recently
// active project.
type tl1Selection struct{ Project, Installation string }

// selectTL1 returns the installations a selection covers, most recently
// active first.
func (c *Catalog) selectTL1(selection tl1Selection) ([]map[string]any, error) {
	installations, err := c.tl1Installations()
	if err != nil {
		return nil, err
	}
	if len(installations) == 0 {
		return nil, fmt.Errorf("no TL1 installations are indexed; enable and sync a tl1 source")
	}
	if selection.Installation != "" {
		for _, installation := range installations {
			if firstString(installation["installation_id"]) == selection.Installation {
				return []map[string]any{installation}, nil
			}
		}
		return nil, fmt.Errorf("unknown TL1 installation %q", selection.Installation)
	}
	project := defaultString(nilIfEmpty(selection.Project), tl1Project(installations[0]))
	selected := []map[string]any{}
	for _, installation := range installations {
		if tl1Project(installation) == project {
			selected = append(selected, installation)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("unknown TL1 project %q", project)
	}
	return selected, nil
}

// loadTL1 reads the history of installations of one project, keeping tasks
// created inside the window. Scope "current" keeps only tasks run with their
// flavor's current shape version (the live workflow definition), which is the
// definition on the most recently active installation that has the flavor.
// TL1 IDs are random, so an ID read from two installations is one task
// (a database carried to another Mac), counted once from the first.
func (c *Catalog) loadTL1(installations []map[string]any, window tl1Window) (*tl1Data, error) {
	scope := defaultString(window.Scope, "all")
	data := &tl1Data{Project: tl1Project(installations[0]), Installations: installations, Flavors: map[string]*tl1Flavor{}, TaskByID: map[string]*tl1Task{},
		Candidates: map[string]map[string]any{}, Window: window, Scope: scope}
	all := map[string]*tl1Task{}
	ordered := []*tl1Task{}
	flavorNames := map[string]map[string]string{}
	for _, installation := range installations {
		installationID := firstString(installation["installation_id"])
		flavors, err := queryMaps(c.DB, "SELECT * FROM tl1_flavors WHERE installation_id=?", installationID)
		if err != nil {
			return nil, err
		}
		flavorNames[installationID] = map[string]string{}
		for _, row := range flavors {
			flavor := &tl1Flavor{ID: firstString(row["flavor_id"]), Name: firstString(row["name"]), ExecutionClass: firstString(row["execution_class"]),
				DefaultConfiguration: firstString(row["default_agent_configuration"]), ShapeVersion: firstString(row["shape_version"]),
				TemplateHash: firstString(row["template_hash"]), Template: firstString(row["template"]), Configurations: tl1JSONMap(row["agent_configurations_json"]),
				Transitions: tl1JSONMap(row["transitions_json"]), Budgets: tl1JSONMap(row["budgets_json"]), Outcomes: tl1JSONMap(row["outcomes_json"]), Row: row}
			flavorNames[installationID][flavor.ID] = flavor.Name
			if data.Flavors[flavor.Name] == nil {
				data.Flavors[flavor.Name] = flavor
			}
		}
		tasks, err := queryMaps(c.DB, "SELECT * FROM tl1_tasks WHERE installation_id=? ORDER BY created_at,task_id", installationID)
		if err != nil {
			return nil, err
		}
		for _, row := range tasks {
			task := &tl1Task{ID: firstString(row["task_id"]), WorkspaceID: firstString(row["workspace_id"]), Flavor: firstString(row["flavor"]),
				ExecutionClass: firstString(row["execution_class"]), ShapeVersion: firstString(row["shape_version"]), Title: firstString(row["title"]),
				Status: firstString(row["status"]), Outcome: firstString(row["outcome"]), ErrorClass: firstString(row["error_class"]),
				ErrorSignature: firstString(row["error_signature"]), ErrorAttribution: firstString(row["error_attribution"]), ErrorText: firstString(row["error_text"]),
				CandidateID: firstString(row["candidate_id"]), ParentID: firstString(row["parent_task_id"]), CreatedBy: firstString(row["created_by_task"]), CreatedAt: firstString(row["created_at"]),
				ClaimedAt: firstString(row["claimed_at"]), CompletedAt: firstString(row["completed_at"]), Installation: installation}
			if all[task.ID] != nil {
				continue
			}
			// Classify from the stored text so classifier improvements apply
			// without a resync.
			task.ErrorClass, task.ErrorSignature, task.ErrorAttribution = classifyTL1Error(task.ErrorText)
			all[task.ID] = task
			ordered = append(ordered, task)
		}
	}
	if len(installations) > 1 {
		sort.SliceStable(ordered, func(i, j int) bool {
			if ordered[i].CreatedAt != ordered[j].CreatedAt {
				return ordered[i].CreatedAt < ordered[j].CreatedAt
			}
			return ordered[i].ID < ordered[j].ID
		})
	}
	// Parent links and revisits use the full history so a windowed view still
	// knows where each task came from.
	seenInCandidate := map[string]bool{}
	for _, task := range ordered {
		if parent := all[task.ParentID]; parent != nil {
			task.Parent = parent
			parent.Children = append(parent.Children, task)
		}
		if task.CandidateID != "" {
			key := task.CandidateID + "\x1f" + task.Flavor
			task.Revisit = seenInCandidate[key]
			seenInCandidate[key] = true
		}
	}
	data.AllTasks = ordered
	data.Enqueues = tl1EnqueueSpikes(ordered)
	var err error
	if data.Since, data.Until, err = tl1ResolveWindow(window, data.Enqueues, time.Now()); err != nil {
		return nil, err
	}
	// Each candidate belongs to the installation of its first task in the
	// window; rows about it from elsewhere are copies.
	candidateOwner := map[string]string{}
	for _, task := range ordered {
		if data.Since != "" && task.CreatedAt < data.Since || data.Until != "" && task.CreatedAt >= data.Until {
			continue
		}
		if scope == "current" {
			if flavor := data.Flavors[task.Flavor]; flavor != nil && flavor.ShapeVersion != "" && task.ShapeVersion != flavor.ShapeVersion {
				continue
			}
		}
		data.Tasks = append(data.Tasks, task)
		data.TaskByID[task.ID] = task
		if _, found := candidateOwner[task.CandidateID]; !found {
			candidateOwner[task.CandidateID] = firstString(task.Installation["installation_id"])
		}
	}
	for _, installation := range installations {
		installationID := firstString(installation["installation_id"])
		owns := func(taskID string) *tl1Task {
			if task := data.TaskByID[taskID]; task != nil && firstString(task.Installation["installation_id"]) == installationID {
				return task
			}
			return nil
		}
		attempts, err := queryMaps(c.DB, "SELECT * FROM tl1_attempts WHERE installation_id=? ORDER BY task_id,attempt_number", installationID)
		if err != nil {
			return nil, err
		}
		usage, err := c.tl1AttemptUsage(installationID)
		if err != nil {
			return nil, err
		}
		for _, row := range attempts {
			task := owns(firstString(row["task_id"]))
			if task == nil {
				continue
			}
			attempt := &tl1Attempt{ID: firstString(row["attempt_id"]), TaskID: task.ID, Configuration: firstString(row["configuration"]), Executor: firstString(row["executor"]),
				Model: firstString(row["model"]), Effort: firstString(row["effort"]), Status: firstString(row["status"]), ErrorClass: firstString(row["error_class"]),
				ErrorSignature: firstString(row["error_signature"]), ErrorAttribution: firstString(row["error_attribution"]), FailureReason: firstString(row["failure_reason"]),
				LogTail: firstString(row["log_tail"]), StartedAt: firstString(row["started_at"]), Conversation: firstString(row["conversation_native_id"]), Task: task}
			if attempt.FailureReason != "" {
				attempt.ErrorClass, attempt.ErrorSignature, attempt.ErrorAttribution = classifyTL1Error(attempt.FailureReason)
			} else if attempt.ErrorClass != "" {
				attempt.ErrorClass, attempt.ErrorSignature, attempt.ErrorAttribution = task.ErrorClass, task.ErrorSignature, task.ErrorAttribution
			}
			_ = json.Unmarshal([]byte(firstString(row["transcripts_json"])), &attempt.Transcripts)
			if value, ok := number(row["duration_ms"]); ok {
				attempt.Duration, attempt.HasDuration = value, true
			}
			added, _ := number(row["lines_added"])
			removed, _ := number(row["lines_removed"])
			attempt.LinesChanged = added + removed
			attempt.FilesChanged, _ = number(row["files_changed"])
			attempt.PeakRSS, _ = number(row["peak_rss_mb"])
			if spent := usage[attempt.ID]; spent != nil {
				attempt.Tokens = spent.tokens
				attempt.PriceStatus = spent.status
				attempt.PricedModel = spent.pricedModel
				if attempt.Model == "" {
					attempt.Model = spent.model
				}
				if spent.cost != nil {
					attempt.Cost, attempt.HasCost, attempt.CostSource = *spent.cost, true, "tokens"
				}
			}
			if !attempt.HasCost {
				if value, ok := number(row["reported_cost_usd"]); ok {
					attempt.Cost, attempt.HasCost, attempt.CostSource = value, true, "tl1-reported"
				}
			}
			if attempt.Configuration == "" && task.ExecutionClass == "llm" {
				attempt.Configuration = defaultString(nilIfEmpty(modelFamily(attempt.Model)), "unrecorded")
			}
			task.Attempts = append(task.Attempts, attempt)
			data.Attempts = append(data.Attempts, attempt)
		}
		candidates, err := queryMaps(c.DB, "SELECT * FROM tl1_candidates WHERE installation_id=?", installationID)
		if err != nil {
			return nil, err
		}
		for _, row := range candidates {
			if id := firstString(row["candidate_id"]); candidateOwner[id] == installationID {
				data.Candidates[id] = row
			}
		}
		inWindow := func(rows []map[string]any) []map[string]any {
			kept := []map[string]any{}
			for _, row := range rows {
				if task := firstString(row["task_id"], row["review_task_id"]); task != "" && owns(task) == nil {
					continue
				}
				if task := firstString(row["task_id"], row["review_task_id"]); task == "" && candidateOwner[firstString(row["candidate_id"])] != installationID {
					continue
				}
				kept = append(kept, row)
			}
			return kept
		}
		for _, target := range []struct {
			query string
			into  *[]map[string]any
		}{
			{"SELECT * FROM tl1_events WHERE installation_id=? ORDER BY created_at", &data.Events},
			{"SELECT * FROM tl1_review_findings WHERE installation_id=? ORDER BY created_at", &data.Findings},
			{"SELECT * FROM tl1_human_touches WHERE installation_id=? ORDER BY created_at", &data.Touches},
		} {
			rows, err := queryMaps(c.DB, target.query, installationID)
			if err != nil {
				return nil, err
			}
			*target.into = append(*target.into, inWindow(rows)...)
		}
		// Health is each Mac's own TL1 judging a configuration; flavor IDs are
		// its own too.
		health, err := queryMaps(c.DB, "SELECT * FROM tl1_configuration_health WHERE installation_id=?", installationID)
		if err != nil {
			return nil, err
		}
		for _, row := range health {
			row["flavor"] = defaultString(flavorNames[installationID][firstString(row["flavor_id"])], firstString(row["flavor_id"]))
			row["host_label"] = installation["host_label"]
			data.Health = append(data.Health, row)
		}
	}
	if len(installations) > 1 {
		for _, rows := range []*[]map[string]any{&data.Events, &data.Findings, &data.Touches} {
			sort.SliceStable(*rows, func(i, j int) bool {
				return firstString((*rows)[i]["created_at"]) < firstString((*rows)[j]["created_at"])
			})
		}
	}
	for _, task := range data.Tasks {
		if count := len(task.Attempts); count > 0 {
			task.Attempts[count-1].Final = true
		}
	}
	return data, nil
}

func tl1JSONMap(value any) map[string]any {
	result := map[string]any{}
	_ = json.Unmarshal([]byte(firstString(value)), &result)
	return result
}

type tl1Usage struct {
	tokens                     map[string]int64
	cost                       *float64
	status, pricedModel, model string
}

// tl1AttemptUsage prices every TL1 transcript of the installation and sums it
// per attempt. Transcript native IDs are <installation>:<attempt>:<file>.
func (c *Catalog) tl1AttemptUsage(installationID string) (map[string]*tl1Usage, error) {
	sums := make([]string, len(usageTokenFields))
	for index, field := range usageTokenFields {
		sums[index] = "u." + field
	}
	prefix := installationID + ":"
	rows, err := queryMaps(c.DB, `SELECT c.native_id,u.usage_hour,u.model usage_model,s.model session_model,`+strings.Join(sums, ",")+`
		FROM conversations c JOIN workspaces w ON w.id=c.workspace_id JOIN agent_sessions s ON s.conversation_id=c.id JOIN agent_session_usage u ON u.agent_session_id=s.id
		WHERE w.source_kind='tl1' AND c.native_id>=? AND c.native_id<? AND u.total_tokens>0`, prefix, prefix+"￿")
	if err != nil {
		return nil, err
	}
	book, err := c.loadPriceBook()
	if err != nil {
		return nil, err
	}
	result := map[string]*tl1Usage{}
	for _, row := range rows {
		parts := strings.SplitN(firstString(row["native_id"]), ":", 3)
		if len(parts) < 3 {
			continue
		}
		spent := result[parts[1]]
		if spent == nil {
			spent = &tl1Usage{tokens: map[string]int64{}}
			result[parts[1]] = spent
		}
		tokens := ledgerTokens(row)
		for key, value := range tokens {
			spent.tokens[key] += value
		}
		day := ""
		if hour, ok := parseTime(firstString(row["usage_hour"])); ok {
			day = localMidnight(hour, time.Local).Format("2006-01-02")
		}
		model := ledgerModel(row)
		if spent.model == "" && model != "Unknown model" {
			spent.model = model
		}
		priced := book.cost(model, day, tokens)
		spent.cost = tl1AddCost(spent.cost, priced.cost)
		spent.status = worsePriceStatus(spent.status, priced.status)
		if priced.pricedModel != "" {
			spent.pricedModel = priced.pricedModel
		}
	}
	return result, nil
}

func tl1AddCost(total, value *float64) *float64 {
	if value == nil {
		return total
	}
	sum := *value
	if total != nil {
		sum += *total
	}
	return &sum
}

// Summary statistics.

func tl1Median(values []float64) (float64, bool) { return tl1Percentile(values, 0.5) }

func tl1Percentile(values []float64, p float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	sorted := append([]float64{}, values...)
	sort.Float64s(sorted)
	position := p * float64(len(sorted)-1)
	low := int(math.Floor(position))
	high := int(math.Ceil(position))
	return sorted[low] + (sorted[high]-sorted[low])*(position-float64(low)), true
}

func tl1Ratio(numerator, denominator float64) any {
	if denominator == 0 {
		return nil
	}
	return numerator / denominator
}

func tl1Optional(value float64, ok bool) any {
	if !ok {
		return nil
	}
	return value
}

func tl1Round(value float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(value*scale) / scale
}

func tl1Hours(from, to string) (float64, bool) {
	start, ok := parseTime(from)
	end, ok2 := parseTime(to)
	if !ok || !ok2 || end.Before(start) {
		return 0, false
	}
	return end.Sub(start).Hours(), true
}

// tl1Counter counts string keys and returns them largest first.
type tl1Counter map[string]int

func (c tl1Counter) top(limit int) []map[string]any {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if c[keys[i]] == c[keys[j]] {
			return keys[i] < keys[j]
		}
		return c[keys[i]] > c[keys[j]]
	})
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	result := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		result = append(result, map[string]any{"key": key, "count": c[key]})
	}
	return result
}
