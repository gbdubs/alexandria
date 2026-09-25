package archive

import (
	"fmt"
	"sort"
	"strings"
)

// Minimum runs before a flavor/configuration cell is compared with others.
const tl1MinSample = 10

type tl1Cell struct {
	Flavor, Configuration                        string
	Attempts, Advanced, Escalated, Errors, Infra int
	AgentErrors, Retried, Open, Priced           int
	Cost, LinesChanged                           float64
	Costs, Durations, Tokens                     []float64
	Input, CacheRead, CacheWrite, Output, Reason int64
	Outcomes                                     tl1Counter
	ErrorSignatures                              tl1Counter
}

func (cell *tl1Cell) add(attempt *tl1Attempt) {
	cell.Attempts++
	switch attempt.disposition() {
	case tl1Advanced:
		cell.Advanced++
	case tl1Escalated:
		cell.Escalated++
	case tl1Errored:
		cell.Errors++
		cell.ErrorSignatures[attempt.ErrorSignature]++
		if attempt.agentError() {
			cell.AgentErrors++
		} else {
			cell.Infra++
		}
	case tl1Retried:
		cell.Retried++
	default:
		cell.Open++
	}
	if attempt.Final && attempt.Task.Outcome != "" {
		cell.Outcomes[attempt.Task.Outcome]++
	}
	// Spend counts every run; the typical-run medians leave out runs killed
	// by infrastructure, which otherwise pull them toward zero.
	typical := attempt.ErrorClass == "" || attempt.agentError()
	if attempt.HasCost {
		cell.Priced++
		cell.Cost += attempt.Cost
		if typical {
			cell.Costs = append(cell.Costs, attempt.Cost)
		}
	}
	if attempt.HasDuration && typical {
		cell.Durations = append(cell.Durations, attempt.Duration)
	}
	if total := attempt.Tokens["total_tokens"]; total > 0 && typical {
		cell.Tokens = append(cell.Tokens, float64(total))
	}
	cell.Input += attempt.Tokens["input_tokens"]
	cell.CacheRead += attempt.Tokens["cache_read_input_tokens"]
	cell.CacheWrite += attempt.Tokens["cache_creation_input_tokens"]
	cell.Output += attempt.Tokens["output_tokens"]
	cell.Reason += attempt.Tokens["reasoning_output_tokens"]
	cell.LinesChanged += attempt.LinesChanged
}

// agentErrorRate excludes infrastructure failures from both sides: they say
// nothing about the configuration that happened to be running.
func (cell *tl1Cell) agentErrorRate() (float64, bool) {
	denominator := cell.Attempts - cell.Infra
	if denominator <= 0 {
		return 0, false
	}
	return float64(cell.AgentErrors) / float64(denominator), true
}

// costPerAdvance is total spend divided by the runs that moved the workflow
// forward on their own: the price of one useful result.
func (cell *tl1Cell) costPerAdvance() (float64, bool) {
	if cell.Advanced == 0 || cell.Priced == 0 {
		return 0, false
	}
	// Scale spend to all attempts when some are unpriced.
	return cell.Cost * float64(cell.Attempts) / float64(cell.Priced) / float64(cell.Advanced), true
}

func (cell *tl1Cell) row() map[string]any {
	medianCost, hasCost := tl1Median(cell.Costs)
	p90Cost, _ := tl1Percentile(cell.Costs, 0.9)
	medianDuration, hasDuration := tl1Median(cell.Durations)
	p90Duration, _ := tl1Percentile(cell.Durations, 0.9)
	medianTokens, hasTokens := tl1Median(cell.Tokens)
	agentRate, hasRate := cell.agentErrorRate()
	perAdvance, hasPerAdvance := cell.costPerAdvance()
	return map[string]any{
		"flavor": cell.Flavor, "configuration": cell.Configuration, "attempts": cell.Attempts, "advanced": cell.Advanced,
		"escalated": cell.Escalated, "errors": cell.Errors, "agent_errors": cell.AgentErrors, "infrastructure_errors": cell.Infra,
		"retried": cell.Retried, "open": cell.Open, "advance_rate": tl1Ratio(float64(cell.Advanced), float64(cell.Attempts)),
		"escalation_rate": tl1Ratio(float64(cell.Escalated), float64(cell.Attempts)), "error_rate": tl1Ratio(float64(cell.Errors), float64(cell.Attempts)),
		"agent_error_rate": tl1Optional(agentRate, hasRate), "cost_usd": tl1Optional(cell.Cost, cell.Priced > 0),
		"priced_attempts": cell.Priced, "median_cost_usd": tl1Optional(medianCost, hasCost), "p90_cost_usd": tl1Optional(p90Cost, hasCost),
		"cost_per_advance_usd": tl1Optional(perAdvance, hasPerAdvance), "median_duration_ms": tl1Optional(medianDuration, hasDuration),
		"p90_duration_ms": tl1Optional(p90Duration, hasDuration), "median_tokens": tl1Optional(medianTokens, hasTokens),
		"cache_read_share": tl1Ratio(float64(cell.CacheRead), float64(cell.Input)), "cache_write_share": tl1Ratio(float64(cell.CacheWrite), float64(cell.Input)),
		"output_tokens": cell.Output, "reasoning_share": tl1Ratio(float64(cell.Reason), float64(cell.Output)),
		"lines_per_dollar": tl1Ratio(cell.LinesChanged, cell.Cost), "lines_changed": cell.LinesChanged,
		"outcomes": cell.Outcomes.top(0), "top_errors": cell.ErrorSignatures.top(3), "low_sample": cell.Attempts < tl1MinSample,
	}
}

func newTL1Cell(flavor, configuration string) *tl1Cell {
	return &tl1Cell{Flavor: flavor, Configuration: configuration, Outcomes: tl1Counter{}, ErrorSignatures: tl1Counter{}}
}

// TL1Overview is the TL1 tab: totals, the flavor × configuration matrix, the
// observed workflow graph, error clusters, detectors with investigation
// prompts, and ranked opportunities.
func (c *Catalog) TL1Overview(selection tl1Selection, window tl1Window) (map[string]any, error) {
	all, err := c.tl1Installations()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return map[string]any{"installations": all}, nil
	}
	installations, err := c.selectTL1(selection)
	if err != nil {
		return nil, err
	}
	data, err := c.loadTL1(installations, window)
	if err != nil {
		return nil, err
	}
	matrix, cells := tl1Matrix(data)
	flavors := tl1FlavorSummaries(data)
	clusters := tl1ErrorClusters(data)
	candidates := tl1CandidateSummary(data)
	human := tl1HumanSummary(data)
	contracts := tl1ContractRepairs(data)
	reviews := tl1ReviewSummary(data)
	detectors := tl1Detectors(data, cells, clusters, candidates, human, contracts, flavors)
	opportunities := tl1Opportunities(data, cells, clusters)
	errors := make([]map[string]any, 0, len(clusters))
	for _, cluster := range clusters {
		errors = append(errors, cluster.row())
	}
	result := map[string]any{
		"project": data.Project, "installations": data.Installations, "days": window.Days, "scope": data.Scope, "since": nilIfEmpty(data.Since),
		"window": tl1WindowRow(data), "enqueues": tl1EnqueueRows(data, tl1EnqueueListLimit),
		"totals": tl1Totals(data, candidates, human), "coverage": tl1Coverage(data), "matrix": matrix, "flavors": flavors,
		"graph": tl1Graph(data), "errors": errors, "candidates": candidates, "human": human, "contract_repairs": contracts,
		"reviews": reviews, "detectors": detectors, "opportunities": opportunities, "configuration_health": data.Health,
	}
	result["review_prompt"] = tl1ReviewPrompt(data, result)
	return result, nil
}

func tl1Matrix(data *tl1Data) ([]map[string]any, map[string]*tl1Cell) {
	cells := map[string]*tl1Cell{}
	for _, attempt := range data.Attempts {
		if attempt.Task.ExecutionClass != "llm" {
			continue
		}
		key := attempt.Task.Flavor + "\x1f" + attempt.Configuration
		if cells[key] == nil {
			cells[key] = newTL1Cell(attempt.Task.Flavor, attempt.Configuration)
		}
		cells[key].add(attempt)
	}
	rows := make([]map[string]any, 0, len(cells))
	byFlavor := map[string][]map[string]any{}
	for _, cell := range cells {
		row := cell.row()
		row["default"] = data.Flavors[cell.Flavor] != nil && data.Flavors[cell.Flavor].DefaultConfiguration == cell.Configuration
		rows = append(rows, row)
		byFlavor[cell.Flavor] = append(byFlavor[cell.Flavor], row)
	}
	// Mark the cheapest and most expensive comparable cell per flavor by cost
	// per useful result.
	for _, group := range byFlavor {
		comparable := []map[string]any{}
		for _, row := range group {
			if row["low_sample"] == false && row["cost_per_advance_usd"] != nil {
				comparable = append(comparable, row)
			}
		}
		if len(comparable) < 2 {
			continue
		}
		sort.Slice(comparable, func(i, j int) bool {
			return comparable[i]["cost_per_advance_usd"].(float64) < comparable[j]["cost_per_advance_usd"].(float64)
		})
		comparable[0]["flag"] = "best_value"
		comparable[len(comparable)-1]["flag"] = "worst_value"
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i]["flavor"] == rows[j]["flavor"] {
			return rows[i]["attempts"].(int) > rows[j]["attempts"].(int)
		}
		return firstString(rows[i]["flavor"]) < firstString(rows[j]["flavor"])
	})
	return rows, cells
}

func tl1FlavorSummaries(data *tl1Data) []map[string]any {
	type version struct {
		tasks, errors, advanced int
		costs                   []float64
		first, last             string
	}
	type summary struct {
		tasks, attempts, errors, escalated, advanced, revisits, open int
		cost                                                         float64
		costs, durations, waits                                      []float64
		outcomes, configurations                                     tl1Counter
		versions                                                     map[string]*version
	}
	summaries := map[string]*summary{}
	for _, task := range data.Tasks {
		value := summaries[task.Flavor]
		if value == nil {
			value = &summary{outcomes: tl1Counter{}, configurations: tl1Counter{}, versions: map[string]*version{}}
			summaries[task.Flavor] = value
		}
		value.tasks++
		switch task.disposition() {
		case tl1Errored:
			value.errors++
		case tl1Escalated:
			value.escalated++
		case tl1Advanced:
			value.advanced++
		default:
			value.open++
		}
		if task.Revisit {
			value.revisits++
		}
		if task.Outcome != "" {
			value.outcomes[task.Outcome]++
		}
		if wait, ok := tl1Hours(task.CreatedAt, task.ClaimedAt); ok {
			value.waits = append(value.waits, wait*3_600_000)
		}
		taskCost, priced := 0.0, false
		for _, attempt := range task.Attempts {
			value.attempts++
			if attempt.Configuration != "" {
				value.configurations[attempt.Configuration]++
			}
			if attempt.HasCost {
				taskCost += attempt.Cost
				priced = true
			}
			if attempt.HasDuration {
				value.durations = append(value.durations, attempt.Duration)
			}
		}
		if priced {
			value.cost += taskCost
			value.costs = append(value.costs, taskCost)
		}
		shape := defaultString(task.ShapeVersion, "unversioned")
		item := value.versions[shape]
		if item == nil {
			item = &version{first: task.CreatedAt}
			value.versions[shape] = item
		}
		item.tasks++
		item.last = task.CreatedAt
		if task.disposition() == tl1Errored {
			item.errors++
		}
		if task.disposition() == tl1Advanced {
			item.advanced++
		}
		if priced {
			item.costs = append(item.costs, taskCost)
		}
	}
	rows := []map[string]any{}
	for name, value := range summaries {
		flavor := data.Flavors[name]
		versions := []map[string]any{}
		for shape, item := range value.versions {
			median, ok := tl1Median(item.costs)
			versions = append(versions, map[string]any{"shape_version": shape, "current": flavor != nil && flavor.ShapeVersion == shape, "tasks": item.tasks,
				"error_rate": tl1Ratio(float64(item.errors), float64(item.tasks)), "advance_rate": tl1Ratio(float64(item.advanced), float64(item.tasks)),
				"median_cost_usd": tl1Optional(median, ok), "first_seen": item.first, "last_seen": item.last})
		}
		sort.Slice(versions, func(i, j int) bool {
			return firstString(versions[i]["first_seen"]) < firstString(versions[j]["first_seen"])
		})
		medianCost, hasCost := tl1Median(value.costs)
		medianDuration, hasDuration := tl1Median(value.durations)
		medianWait, hasWait := tl1Median(value.waits)
		row := map[string]any{"name": name, "tasks": value.tasks, "attempts": value.attempts, "errors": value.errors, "escalated": value.escalated,
			"advanced": value.advanced, "open": value.open, "error_rate": tl1Ratio(float64(value.errors), float64(value.tasks)),
			"escalation_rate": tl1Ratio(float64(value.escalated), float64(value.tasks)), "revisit_rate": tl1Ratio(float64(value.revisits), float64(value.tasks)),
			"cost_usd": value.cost, "median_task_cost_usd": tl1Optional(medianCost, hasCost), "median_duration_ms": tl1Optional(medianDuration, hasDuration),
			"median_queue_wait_ms": tl1Optional(medianWait, hasWait), "outcomes": value.outcomes.top(0), "configurations": value.configurations.top(0), "shape_versions": versions}
		if flavor != nil {
			row["execution_class"] = flavor.ExecutionClass
			row["default_configuration"] = nilIfEmpty(flavor.DefaultConfiguration)
			allowed := []string{}
			for configuration := range flavor.Configurations {
				allowed = append(allowed, configuration)
			}
			sort.Strings(allowed)
			row["allowed_configurations"] = allowed
			row["current_shape_version"] = nilIfEmpty(flavor.ShapeVersion)
			row["budgets"] = flavor.Budgets
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["cost_usd"].(float64) > rows[j]["cost_usd"].(float64) || (rows[i]["cost_usd"] == rows[j]["cost_usd"] && rows[i]["tasks"].(int) > rows[j]["tasks"].(int))
	})
	return rows
}

// tl1Graph returns observed transitions: predecessor flavor and outcome to the
// successor flavor TL1 created, with volume and the share caused by errors.
func tl1Graph(data *tl1Data) map[string]any {
	type edge struct {
		count, errors int
	}
	nodes := map[string]map[string]any{}
	edges := map[[3]string]*edge{}
	for _, task := range data.Tasks {
		node := nodes[task.Flavor]
		if node == nil {
			node = map[string]any{"flavor": task.Flavor, "execution_class": task.ExecutionClass, "tasks": 0, "cost_usd": 0.0, "errors": 0}
			nodes[task.Flavor] = node
		}
		node["tasks"] = node["tasks"].(int) + 1
		for _, attempt := range task.Attempts {
			if attempt.HasCost {
				node["cost_usd"] = node["cost_usd"].(float64) + attempt.Cost
			}
		}
		if task.ErrorClass != "" {
			node["errors"] = node["errors"].(int) + 1
		}
		if task.Parent == nil {
			continue
		}
		key := [3]string{task.Parent.Flavor, defaultString(task.Parent.Outcome, "(no outcome)"), task.Flavor}
		if edges[key] == nil {
			edges[key] = &edge{}
		}
		edges[key].count++
		if task.Parent.ErrorClass != "" {
			edges[key].errors++
		}
	}
	nodeRows := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		nodeRows = append(nodeRows, node)
	}
	sort.Slice(nodeRows, func(i, j int) bool { return firstString(nodeRows[i]["flavor"]) < firstString(nodeRows[j]["flavor"]) })
	edgeRows := make([]map[string]any, 0, len(edges))
	for key, value := range edges {
		edgeRows = append(edgeRows, map[string]any{"from": key[0], "outcome": key[1], "to": key[2], "count": value.count, "error_count": value.errors})
	}
	sort.Slice(edgeRows, func(i, j int) bool { return edgeRows[i]["count"].(int) > edgeRows[j]["count"].(int) })
	return map[string]any{"nodes": nodeRows, "edges": edgeRows}
}

type tl1Cluster struct {
	Class, Signature, Attribution string
	Tasks                         []*tl1Task
	Flavors, Configurations       tl1Counter
	HumanFollowups                int
	Cost                          float64
	First, Last, Example, LogTail string
}

func tl1ErrorClusters(data *tl1Data) []*tl1Cluster {
	clusters := map[string]*tl1Cluster{}
	for _, task := range data.Tasks {
		if task.ErrorClass == "" {
			continue
		}
		key := task.ErrorClass + "\x1f" + task.ErrorSignature
		cluster := clusters[key]
		if cluster == nil {
			cluster = &tl1Cluster{Class: task.ErrorClass, Signature: task.ErrorSignature, Attribution: task.ErrorAttribution, Flavors: tl1Counter{}, Configurations: tl1Counter{}, First: task.CreatedAt, Example: task.ErrorText}
			clusters[key] = cluster
		}
		cluster.Tasks = append(cluster.Tasks, task)
		cluster.Flavors[task.Flavor]++
		if task.CreatedAt < cluster.First {
			cluster.First = task.CreatedAt
		}
		if task.CreatedAt > cluster.Last {
			cluster.Last = task.CreatedAt
		}
		for _, attempt := range task.Attempts {
			if attempt.HasCost {
				cluster.Cost += attempt.Cost
			}
			if attempt.Final && attempt.Configuration != "" {
				cluster.Configurations[attempt.Configuration]++
			}
			if attempt.Final && cluster.LogTail == "" {
				cluster.LogTail = attempt.LogTail
			}
		}
		for _, child := range task.Children {
			if child.ExecutionClass == "human" {
				cluster.HumanFollowups++
			}
		}
	}
	rows := make([]*tl1Cluster, 0, len(clusters))
	for _, cluster := range clusters {
		rows = append(rows, cluster)
	}
	sort.Slice(rows, func(i, j int) bool {
		if len(rows[i].Tasks) == len(rows[j].Tasks) {
			return rows[i].Signature < rows[j].Signature
		}
		return len(rows[i].Tasks) > len(rows[j].Tasks)
	})
	return rows
}

func (cluster *tl1Cluster) row() map[string]any {
	samples := []map[string]any{}
	for index := len(cluster.Tasks) - 1; index >= 0 && len(samples) < 5; index-- {
		task := cluster.Tasks[index]
		samples = append(samples, map[string]any{"task_id": task.ID, "workspace_id": task.WorkspaceID, "title": task.Title, "flavor": task.Flavor, "created_at": task.CreatedAt})
	}
	return map[string]any{"class": cluster.Class, "signature": cluster.Signature, "attribution": cluster.Attribution, "count": len(cluster.Tasks),
		"flavors": cluster.Flavors.top(0), "configurations": cluster.Configurations.top(0), "human_followups": cluster.HumanFollowups,
		"cost_usd": cluster.Cost, "first_seen": cluster.First, "last_seen": cluster.Last, "example": tl1Clip(cluster.Example, 1200),
		"log_tail": nilIfEmpty(tl1Clip(cluster.LogTail, 1200)), "samples": samples}
}

func tl1Clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "") + "…"
}

func tl1CandidateSummary(data *tl1Data) map[string]any {
	type totals struct {
		cost                 float64
		tasks, human, errors int
	}
	perCandidate := map[string]*totals{}
	for _, task := range data.Tasks {
		if task.CandidateID == "" {
			continue
		}
		value := perCandidate[task.CandidateID]
		if value == nil {
			value = &totals{}
			perCandidate[task.CandidateID] = value
		}
		value.tasks++
		if task.ExecutionClass == "human" {
			value.human++
		}
		if task.ErrorClass != "" {
			value.errors++
		}
		for _, attempt := range task.Attempts {
			if attempt.HasCost {
				value.cost += attempt.Cost
			}
		}
	}
	statuses := tl1Counter{}
	steps, finishedCosts := []float64{}, []float64{}
	nearLimit := []map[string]any{}
	top := []map[string]any{}
	for id, candidate := range data.Candidates {
		status := defaultString(candidate["status"], "unknown")
		statuses[status]++
		stepCount, _ := number(candidate["workflow_steps"])
		limit, _ := number(candidate["workflow_step_limit"])
		steps = append(steps, stepCount)
		value := perCandidate[id]
		if value == nil {
			value = &totals{}
		}
		row := map[string]any{"candidate_id": id, "title": candidate["title"], "status": status, "workflow_steps": stepCount, "workflow_step_limit": limit,
			"cost_usd": value.cost, "tasks": value.tasks, "human_tasks": value.human, "errors": value.errors, "pr_url": candidate["pr_url"],
			"created_at": candidate["created_at"], "closed_at": candidate["closed_at"]}
		if status != "open" {
			finishedCosts = append(finishedCosts, value.cost)
		}
		if limit > 0 && stepCount >= 0.8*limit && status == "open" {
			nearLimit = append(nearLimit, row)
		}
		top = append(top, row)
	}
	sort.Slice(top, func(i, j int) bool { return top[i]["cost_usd"].(float64) > top[j]["cost_usd"].(float64) })
	if len(top) > 15 {
		top = top[:15]
	}
	medianSteps, hasSteps := tl1Median(steps)
	p90Steps, _ := tl1Percentile(steps, 0.9)
	medianFinished, hasFinished := tl1Median(finishedCosts)
	return map[string]any{"count": len(data.Candidates), "statuses": statuses.top(0), "median_steps": tl1Optional(medianSteps, hasSteps),
		"p90_steps": tl1Optional(p90Steps, hasSteps), "median_finished_cost_usd": tl1Optional(medianFinished, hasFinished), "finished": len(finishedCosts),
		"near_step_limit": nearLimit, "most_expensive": top}
}

func tl1HumanSummary(data *tl1Data) map[string]any {
	pending := tl1Counter{}
	byFlavor := tl1Counter{}
	causes := tl1Counter{}
	oldest := ""
	errorCaused, total := 0, 0
	for _, task := range data.Tasks {
		if task.ExecutionClass != "human" {
			continue
		}
		total++
		byFlavor[task.Flavor]++
		if task.Status == "pending" || task.Status == "claimed" {
			pending[task.Flavor]++
			if oldest == "" || task.CreatedAt < oldest {
				oldest = task.CreatedAt
			}
		}
		if task.Parent != nil {
			cause := task.Parent.Flavor + " → " + defaultString(task.Parent.Outcome, "(no outcome)")
			if task.Parent.ErrorClass != "" {
				errorCaused++
				cause = task.Parent.Flavor + " → " + task.Parent.ErrorClass
			}
			causes[cause]++
		}
	}
	responses := 0
	for _, touch := range data.Touches {
		if firstString(touch["kind"]) == "response" || firstString(touch["action"]) == "human_comment" {
			responses++
		}
	}
	pendingTotal := 0
	for _, count := range pending {
		pendingTotal += count
	}
	return map[string]any{"tasks": total, "pending": pendingTotal, "pending_by_flavor": pending.top(0), "by_flavor": byFlavor.top(0),
		"causes": causes.top(12), "error_caused": errorCaused, "error_caused_share": tl1Ratio(float64(errorCaused), float64(total)),
		"oldest_pending_at": nilIfEmpty(oldest), "responses": responses}
}

// tl1ContractRepairs counts TL1's output-contract repair loop per flavor:
// invalid handoffs detected, repairs started, recovered, and failed.
func tl1ContractRepairs(data *tl1Data) []map[string]any {
	type counts struct{ invalid, started, recovered, failed int }
	perFlavor := map[string]*counts{}
	for _, event := range data.Events {
		task := data.TaskByID[firstString(event["task_id"])]
		if task == nil {
			continue
		}
		value := perFlavor[task.Flavor]
		if value == nil {
			value = &counts{}
			perFlavor[task.Flavor] = value
		}
		switch firstString(event["event_type"]) {
		case "invalid_handoff_detected":
			value.invalid++
		case "contract_repair_started":
			value.started++
		case "contract_repair_recovered":
			value.recovered++
		case "contract_repair_failed":
			value.failed++
		}
	}
	rows := []map[string]any{}
	for flavor, value := range perFlavor {
		if value.invalid+value.started == 0 {
			continue
		}
		rows = append(rows, map[string]any{"flavor": flavor, "invalid_handoffs": value.invalid, "repairs_started": value.started,
			"repairs_recovered": value.recovered, "repairs_failed": value.failed, "repair_failure_rate": tl1Ratio(float64(value.failed), float64(value.started))})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["invalid_handoffs"].(int) > rows[j]["invalid_handoffs"].(int) })
	return rows
}

// tl1ReviewSummary attributes each review finding to the configuration of the
// last code-writing run on its candidate before the finding, so review quality
// can be compared across implementing configurations.
func tl1ReviewSummary(data *tl1Data) map[string]any {
	type change struct {
		at, flavor, configuration string
	}
	changes := map[string][]change{}
	implementations := tl1Counter{}
	// TL1 does not always record lines changed, so any LLM run of a flavor
	// that may write code counts as an implementation.
	for _, attempt := range data.Attempts {
		flavor := data.Flavors[attempt.Task.Flavor]
		if attempt.Task.CandidateID == "" || attempt.Task.ExecutionClass != "llm" || flavor == nil || boolInt(flavor.Row["read_only"]) == 1 || attempt.ErrorClass != "" {
			continue
		}
		key := attempt.Task.Flavor + " · " + attempt.Configuration
		implementations[key]++
		changes[attempt.Task.CandidateID] = append(changes[attempt.Task.CandidateID], change{attempt.StartedAt, attempt.Task.Flavor, attempt.Configuration})
	}
	for _, list := range changes {
		sort.Slice(list, func(i, j int) bool { return list[i].at < list[j].at })
	}
	severities := tl1Counter{}
	files := tl1Counter{}
	type attributed struct{ total, major int }
	byAuthor := map[string]*attributed{}
	for _, finding := range data.Findings {
		severity := defaultString(finding["severity"], "unspecified")
		severities[severity]++
		if path := firstString(finding["file_path"]); path != "" {
			files[path]++
		}
		at := firstString(finding["created_at"])
		var author *change
		for index := range changes[firstString(finding["candidate_id"])] {
			item := changes[firstString(finding["candidate_id"])][index]
			if item.at <= at {
				author = &item
			}
		}
		if author == nil {
			continue
		}
		key := author.flavor + " · " + author.configuration
		if byAuthor[key] == nil {
			byAuthor[key] = &attributed{}
		}
		byAuthor[key].total++
		if severity == "major" || severity == "blocker" {
			byAuthor[key].major++
		}
	}
	authors := []map[string]any{}
	for key, value := range byAuthor {
		runs := implementations[key]
		parts := strings.SplitN(key, " · ", 2)
		authors = append(authors, map[string]any{"flavor": parts[0], "configuration": parts[1], "implementation_runs": runs, "findings": value.total,
			"major_or_blocker": value.major, "findings_per_run": tl1Ratio(float64(value.total), float64(runs)), "major_per_run": tl1Ratio(float64(value.major), float64(runs))})
	}
	sort.Slice(authors, func(i, j int) bool { return authors[i]["findings"].(int) > authors[j]["findings"].(int) })
	return map[string]any{"total": len(data.Findings), "severities": severities.top(0), "top_files": files.top(10), "by_implementation": authors}
}

func tl1Totals(data *tl1Data, candidates, human map[string]any) map[string]any {
	cost, reported := 0.0, 0.0
	llm, procedural, llmErrors, agentErrors, infra := 0, 0, 0, 0, 0
	tokens := map[string]int64{}
	durations := []float64{}
	for _, attempt := range data.Attempts {
		if attempt.HasCost {
			cost += attempt.Cost
			if attempt.CostSource == "tl1-reported" {
				reported += attempt.Cost
			}
		}
		for key, value := range attempt.Tokens {
			tokens[key] += value
		}
		if attempt.Task.ExecutionClass == "llm" {
			llm++
			if attempt.ErrorClass != "" {
				llmErrors++
				if attempt.agentError() {
					agentErrors++
				} else {
					infra++
				}
			}
			if attempt.HasDuration {
				durations = append(durations, attempt.Duration)
			}
		} else {
			procedural++
		}
	}
	finished := integer(candidates["finished"])
	medianDuration, ok := tl1Median(durations)
	return map[string]any{"cost_usd": cost, "reported_only_cost_usd": reported, "tasks": len(data.Tasks), "llm_attempts": llm, "procedural_attempts": procedural,
		"human_tasks": human["tasks"], "pending_human": human["pending"], "candidates": candidates["count"], "finished_candidates": finished,
		"cost_per_finished_candidate_usd": tl1Ratio(cost, float64(finished)), "median_steps": candidates["median_steps"],
		"llm_error_rate": tl1Ratio(float64(llmErrors), float64(llm)), "agent_error_rate": tl1Ratio(float64(agentErrors), float64(llm-infra)),
		"infrastructure_errors": infra, "human_tasks_per_candidate": tl1Ratio(float64(integer(human["tasks"])), float64(integer(candidates["count"]))),
		"median_llm_duration_ms": tl1Optional(medianDuration, ok), "tokens": tokens,
		"cache_read_share": tl1Ratio(float64(tokens["cache_read_input_tokens"]), float64(tokens["input_tokens"]))}
}

// tl1Coverage reports how much of the LLM work Pharos can account for, so a
// cheap-looking configuration is never an artifact of missing transcripts.
func tl1Coverage(data *tl1Data) map[string]any {
	type coverage struct{ attempts, transcripts, tokens, priced, reported int }
	perExecutor := map[string]*coverage{}
	for _, attempt := range data.Attempts {
		if attempt.Task.ExecutionClass != "llm" {
			continue
		}
		executor := defaultString(attempt.Executor, "unrecorded")
		value := perExecutor[executor]
		if value == nil {
			value = &coverage{}
			perExecutor[executor] = value
		}
		value.attempts++
		if attempt.Conversation != "" {
			value.transcripts++
		}
		if attempt.Tokens["total_tokens"] > 0 {
			value.tokens++
		}
		if attempt.HasCost && attempt.CostSource == "tokens" {
			value.priced++
		}
		if attempt.HasCost && attempt.CostSource == "tl1-reported" {
			value.reported++
		}
	}
	rows := []map[string]any{}
	total := &coverage{}
	for executor, value := range perExecutor {
		rows = append(rows, map[string]any{"executor": executor, "attempts": value.attempts, "with_transcript": value.transcripts, "with_tokens": value.tokens,
			"priced_from_tokens": value.priced, "tl1_reported_cost_only": value.reported})
		total.attempts += value.attempts
		total.transcripts += value.transcripts
		total.tokens += value.tokens
		total.priced += value.priced + value.reported
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["attempts"].(int) > rows[j]["attempts"].(int) })
	return map[string]any{"by_executor": rows, "transcript_share": tl1Ratio(float64(total.transcripts), float64(total.attempts)),
		"cost_share": tl1Ratio(float64(total.priced), float64(total.attempts))}
}

func tl1Percent(value any) string {
	number, ok := value.(float64)
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", number*100)
}

func tl1Dollars(value float64) string {
	if value >= 100 {
		return fmt.Sprintf("$%.0f", value)
	}
	return fmt.Sprintf("$%.2f", value)
}
