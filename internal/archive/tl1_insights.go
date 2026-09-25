package archive

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Detector severities, most urgent first.
var tl1SeverityRank = map[string]int{"high": 0, "medium": 1, "low": 2, "info": 3}

func tl1Severity(count int, high, medium int) string {
	switch {
	case count >= high:
		return "high"
	case count >= medium:
		return "medium"
	default:
		return "low"
	}
}

// tl1Detectors turns the analysis into a ranked list of concerns. Each carries
// the evidence behind it and a prompt an agent can act on.
func tl1Detectors(data *tl1Data, cells map[string]*tl1Cell, clusters []*tl1Cluster, candidates, human map[string]any, contracts, flavors []map[string]any) []map[string]any {
	detectors := []map[string]any{}
	add := func(detector map[string]any) { detectors = append(detectors, detector) }

	for index, cluster := range clusters {
		if index >= 10 || len(cluster.Tasks) < 3 {
			break
		}
		titles := map[string]string{
			tl1AttributionConfiguration:  "Executor failure: ",
			tl1AttributionInfrastructure: "Infrastructure failure: ",
			tl1AttributionContract:       "Output contract failure: ",
			tl1AttributionScript:         "Procedural script failure: ",
			tl1AttributionProvider:       "Provider limit: ",
		}
		prefix, ok := titles[cluster.Attribution]
		if !ok {
			prefix = "Runtime error: "
		}
		title := prefix + cluster.Signature
		severity := tl1Severity(len(cluster.Tasks), 20, 5)
		if cluster.HumanFollowups >= 10 {
			severity = "high"
		}
		flavors := []string{}
		for _, item := range cluster.Flavors.top(3) {
			flavors = append(flavors, fmt.Sprintf("%s (%d)", item["key"], item["count"]))
		}
		summary := fmt.Sprintf("%d tasks failed this way in %s, from %s to %s.", len(cluster.Tasks), strings.Join(flavors, ", "), tl1Day(cluster.First), tl1Day(cluster.Last))
		if cluster.HumanFollowups > 0 {
			summary += fmt.Sprintf(" %d of them created human follow-up tasks.", cluster.HumanFollowups)
		}
		if cluster.Cost > 0 {
			summary += fmt.Sprintf(" %s was spent on the failed runs.", tl1Dollars(cluster.Cost))
		}
		row := cluster.row()
		add(map[string]any{"id": "error:" + cluster.Class + ":" + stableID("tl1-signature", cluster.Signature), "kind": "error_cluster", "severity": severity,
			"title": title, "summary": summary, "attribution": cluster.Attribution, "flavor": cluster.Flavors.top(1)[0]["key"],
			"impact":   map[string]any{"tasks": len(cluster.Tasks), "cost_usd": cluster.Cost, "human_followups": cluster.HumanFollowups},
			"evidence": row, "filter": map[string]any{"error_signature": cluster.Signature}, "prompt": tl1ClusterPrompt(data, cluster)})
	}

	for _, cell := range cells {
		rate, ok := cell.agentErrorRate()
		if !ok || cell.Attempts < 5 || rate < 0.25 {
			continue
		}
		var peer *tl1Cell
		for _, other := range cells {
			otherRate, otherOK := other.agentErrorRate()
			if other.Flavor != cell.Flavor || other == cell || !otherOK || other.Attempts < 5 || otherRate > rate-0.15 {
				continue
			}
			if peer == nil || other.Attempts > peer.Attempts {
				peer = other
			}
		}
		summary := fmt.Sprintf("%d of %d %s runs on %s ended in an error attributable to the run (%s), excluding infrastructure failures.", cell.AgentErrors, cell.Attempts-cell.Infra, cell.Flavor, cell.Configuration, tl1Percent(rate))
		if peer != nil {
			peerRate, _ := peer.agentErrorRate()
			summary += fmt.Sprintf(" %s fails %s of the same flavor's runs.", peer.Configuration, tl1Percent(peerRate))
		}
		if top := cell.ErrorSignatures.top(1); len(top) > 0 {
			summary += fmt.Sprintf(" Most common: %s.", strings.TrimRight(firstString(top[0]["key"]), "."))
		}
		add(map[string]any{"id": "reliability:" + cell.Flavor + ":" + cell.Configuration, "kind": "configuration_reliability", "severity": tl1Severity(cell.AgentErrors, 20, 5),
			"title": fmt.Sprintf("%s fails %s of %s runs", cell.Configuration, tl1Percent(rate), cell.Flavor), "summary": summary,
			"flavor": cell.Flavor, "configuration": cell.Configuration, "impact": map[string]any{"tasks": cell.AgentErrors, "cost_usd": tl1CellErrorCost(data, cell)},
			"evidence": cell.row(), "filter": map[string]any{"flavor": cell.Flavor, "configuration": cell.Configuration, "disposition": tl1Errored},
			"prompt": tl1ReliabilityPrompt(data, cell, peer)})
	}

	for _, row := range contracts {
		started, failed := integer(row["repairs_started"]), integer(row["repairs_failed"])
		if started < 5 || float64(failed) < 0.5*float64(started) {
			continue
		}
		flavor := firstString(row["flavor"])
		add(map[string]any{"id": "contract:" + flavor, "kind": "output_contract", "severity": tl1Severity(int(failed), 20, 5),
			"title":   fmt.Sprintf("TL1 could not repair %s's handoff %d of %d times", flavor, failed, started),
			"summary": fmt.Sprintf("%s produced %d invalid handoffs. TL1's contract repair recovered %d and failed %d, so those tasks ended in runtime_error. Tighten the output instructions or the outputs schema.", flavor, integer(row["invalid_handoffs"]), integer(row["repairs_recovered"]), failed),
			"flavor":  flavor, "impact": map[string]any{"tasks": failed}, "evidence": row, "filter": map[string]any{"flavor": flavor, "error_class": "output_contract"},
			"prompt": tl1ContractPrompt(data, flavor, row)})
	}

	if outliers := tl1CostOutliers(data, cells); len(outliers) > 0 {
		excess := 0.0
		for _, row := range outliers {
			excess += row["excess_usd"].(float64)
		}
		severity := "low"
		if excess >= 50 {
			severity = "medium"
		}
		add(map[string]any{"id": "cost-outliers", "kind": "cost_outlier", "severity": severity,
			"title":   fmt.Sprintf("%d runs cost at least 3× their flavor's median", len(outliers)),
			"summary": fmt.Sprintf("These runs spent %s more than a median run of the same flavor and configuration. Long runs usually mean the agent looped, re-read large context, or fought the environment.", tl1Dollars(excess)),
			"impact":  map[string]any{"tasks": len(outliers), "cost_usd": excess}, "evidence": map[string]any{"attempts": outliers}, "prompt": tl1OutlierPrompt(data, outliers)})
	}

	if near := candidates["near_step_limit"].([]map[string]any); len(near) > 0 {
		add(map[string]any{"id": "step-budget", "kind": "budget_pressure", "severity": tl1Severity(len(near), 10, 3),
			"title":   fmt.Sprintf("%d open candidates have used 80%% or more of their workflow step budget", len(near)),
			"summary": "Candidates that exhaust workflow_steps stop without finishing. Check whether they are looping between flavors.",
			"impact":  map[string]any{"candidates": len(near)}, "evidence": map[string]any{"candidates": near}, "prompt": tl1LoopPrompt(data, "", near)})
	}

	// Revisits of human flavors are driven by upstream failures, which the
	// error detectors already cover; report the worst automated loops only.
	loops := []map[string]any{}
	for _, flavor := range flavors {
		rate, ok := flavor["revisit_rate"].(float64)
		if ok && rate >= 0.35 && flavor["tasks"].(int) >= 10 && flavor["execution_class"] == "llm" {
			loops = append(loops, flavor)
		}
	}
	sort.Slice(loops, func(i, j int) bool { return loops[i]["revisit_rate"].(float64) > loops[j]["revisit_rate"].(float64) })
	for index, flavor := range loops {
		if index >= 3 {
			break
		}
		rate := flavor["revisit_rate"].(float64)
		name := firstString(flavor["name"])
		revisits := int(rate*float64(flavor["tasks"].(int)) + 0.5)
		add(map[string]any{"id": "rework:" + name, "kind": "rework_loop", "severity": "medium",
			"title":   fmt.Sprintf("%s reruns on the same candidate %s of the time", name, tl1Percent(rate)),
			"summary": fmt.Sprintf("%d %s tasks ran on a candidate that had already been through %s. Repeated visits usually mean an upstream step hands over incomplete work, review criteria are unclear, or earlier runs failed.", revisits, name, name),
			"flavor":  name, "impact": map[string]any{"tasks": revisits}, "evidence": flavor, "filter": map[string]any{"flavor": name},
			"prompt": tl1LoopPrompt(data, name, nil)})
	}

	if share, ok := human["error_caused_share"].(float64); ok && share >= 0.3 && integer(human["error_caused"]) >= 10 {
		add(map[string]any{"id": "human-errors", "kind": "human_attention", "severity": "high",
			"title":   fmt.Sprintf("%s of human tasks exist because an automated step errored", tl1Percent(share)),
			"summary": fmt.Sprintf("%d human tasks were created to recover from runtime errors rather than to make product decisions. Fixing the top error clusters returns that time.", integer(human["error_caused"])),
			"impact":  map[string]any{"tasks": integer(human["error_caused"])}, "evidence": map[string]any{"causes": human["causes"]}, "prompt": tl1HumanPrompt(data, human)})
	}
	if pending := integer(human["pending"]); pending >= 10 {
		add(map[string]any{"id": "human-queue", "kind": "human_queue", "severity": tl1Severity(int(pending), 50, 10),
			"title":   fmt.Sprintf("%d tasks are waiting on a human", pending),
			"summary": fmt.Sprintf("The oldest has waited since %s. Candidates behind these tasks cannot progress.", tl1Day(firstString(human["oldest_pending_at"]))),
			"impact":  map[string]any{"tasks": pending}, "evidence": map[string]any{"pending_by_flavor": human["pending_by_flavor"]}, "prompt": tl1HumanPrompt(data, human)})
	}

	for _, row := range tl1Coverage(data)["by_executor"].([]map[string]any) {
		attempts, transcripts := row["attempts"].(int), row["with_transcript"].(int)
		if attempts < 5 || float64(transcripts) >= 0.8*float64(attempts) {
			continue
		}
		add(map[string]any{"id": "coverage:" + firstString(row["executor"]), "kind": "coverage_gap", "severity": "low",
			"title":   fmt.Sprintf("No transcript for %d of %d %s attempts", attempts-transcripts, attempts, row["executor"]),
			"summary": "Cost and token comparisons exclude these runs. Transcripts may have been deleted, or the executor wrote them elsewhere.",
			"impact":  map[string]any{"tasks": attempts - transcripts}, "evidence": row})
	}

	for _, flavor := range flavors {
		versions := flavor["shape_versions"].([]map[string]any)
		if len(versions) < 2 {
			continue
		}
		current, previous := versions[len(versions)-1], versions[len(versions)-2]
		if current["tasks"].(int) < 10 || previous["tasks"].(int) < 10 {
			continue
		}
		now, _ := current["error_rate"].(float64)
		before, _ := previous["error_rate"].(float64)
		name := firstString(flavor["name"])
		if now-before >= 0.15 {
			add(map[string]any{"id": "regression:" + name, "kind": "shape_regression", "severity": "high",
				"title":   fmt.Sprintf("%s errors rose from %s to %s after its latest definition change", name, tl1Percent(before), tl1Percent(now)),
				"summary": fmt.Sprintf("Shape version %s (since %s) errs more than %s did.", short(firstString(current["shape_version"])), tl1Day(firstString(current["first_seen"])), short(firstString(previous["shape_version"]))),
				"flavor":  name, "evidence": map[string]any{"versions": versions}, "prompt": tl1FlavorPrompt(data, name, "Its latest definition change made it fail more often. Compare the current and previous versions and propose a fix or rollback.")})
		} else if before-now >= 0.15 {
			add(map[string]any{"id": "improvement:" + name, "kind": "shape_improvement", "severity": "info",
				"title":   fmt.Sprintf("%s errors fell from %s to %s after its latest definition change", name, tl1Percent(before), tl1Percent(now)),
				"summary": "The change is working. Consider applying the same fix to similar flavors.", "flavor": name, "evidence": map[string]any{"versions": versions}})
		}
	}

	for _, row := range data.Health {
		flavor := firstString(row["flavor_id"])
		for _, candidate := range data.Flavors {
			if candidate.ID == flavor {
				flavor = candidate.Name
			}
		}
		add(map[string]any{"id": "health:" + flavor + ":" + firstString(row["configuration_name"]), "kind": "configuration_health", "severity": "medium",
			"title":   fmt.Sprintf("TL1 marked %s unhealthy for %s", row["configuration_name"], flavor),
			"summary": fmt.Sprintf("%s Next probe: %s.", firstString(row["reason"]), tl1Day(firstString(row["next_probe_at"]))), "flavor": flavor, "evidence": row})
	}

	for _, cell := range cells {
		share := float64(cell.CacheRead) / float64(max(cell.Input, 1))
		if cell.Input < 20_000_000 || share >= 0.6 || cell.Attempts < 5 {
			continue
		}
		add(map[string]any{"id": "cache:" + cell.Flavor + ":" + cell.Configuration, "kind": "cache_efficiency", "severity": "low",
			"title":   fmt.Sprintf("%s on %s reuses only %s of its input from cache", cell.Flavor, cell.Configuration, tl1Percent(share)),
			"summary": "Low cache reuse usually means the prompt prefix changes between turns (timestamps, reordered context) or sessions are restarted instead of resumed.",
			"flavor":  cell.Flavor, "configuration": cell.Configuration, "evidence": cell.row(), "prompt": tl1FlavorPrompt(data, cell.Flavor, fmt.Sprintf("On %s it reuses only %s of input tokens from the prompt cache. Find what makes the prompt prefix unstable and how to keep it stable.", cell.Configuration, tl1Percent(share)))})
	}

	sort.SliceStable(detectors, func(i, j int) bool {
		left, right := tl1SeverityRank[firstString(detectors[i]["severity"])], tl1SeverityRank[firstString(detectors[j]["severity"])]
		if left != right {
			return left < right
		}
		return integer(mapValueDefault(detectors[i]["impact"])["tasks"]) > integer(mapValueDefault(detectors[j]["impact"])["tasks"])
	})
	return detectors
}

func tl1CellErrorCost(data *tl1Data, cell *tl1Cell) float64 {
	total := 0.0
	for _, attempt := range data.Attempts {
		if attempt.Task.Flavor == cell.Flavor && attempt.Configuration == cell.Configuration && attempt.agentError() && attempt.HasCost {
			total += attempt.Cost
		}
	}
	return total
}

func tl1CostOutliers(data *tl1Data, cells map[string]*tl1Cell) []map[string]any {
	outliers := []map[string]any{}
	for _, attempt := range data.Attempts {
		cell := cells[attempt.Task.Flavor+"\x1f"+attempt.Configuration]
		if cell == nil || !attempt.HasCost || len(cell.Costs) < 5 {
			continue
		}
		median, _ := tl1Median(cell.Costs)
		if attempt.Cost < 3*median || attempt.Cost-median < 2 {
			continue
		}
		outliers = append(outliers, map[string]any{"attempt_id": attempt.ID, "task_id": attempt.TaskID, "workspace_id": attempt.Task.WorkspaceID,
			"title": attempt.Task.Title, "flavor": attempt.Task.Flavor, "configuration": attempt.Configuration, "cost_usd": attempt.Cost,
			"median_cost_usd": median, "excess_usd": attempt.Cost - median, "ratio": attempt.Cost / median, "duration_ms": tl1Optional(attempt.Duration, attempt.HasDuration),
			"tokens": attempt.Tokens["total_tokens"], "disposition": attempt.disposition(), "conversation_native_id": attempt.Conversation})
	}
	sort.Slice(outliers, func(i, j int) bool { return outliers[i]["excess_usd"].(float64) > outliers[j]["excess_usd"].(float64) })
	if len(outliers) > 12 {
		outliers = outliers[:12]
	}
	return outliers
}

// tl1Opportunities estimates savings from switching a flavor to a cheaper
// configuration that is at least as reliable, and from removing error clusters.
func tl1Opportunities(data *tl1Data, cells map[string]*tl1Cell, clusters []*tl1Cluster) []map[string]any {
	opportunities := []map[string]any{}
	byFlavor := map[string][]*tl1Cell{}
	for _, cell := range cells {
		byFlavor[cell.Flavor] = append(byFlavor[cell.Flavor], cell)
	}
	for flavor, group := range byFlavor {
		var incumbent *tl1Cell
		for _, cell := range group {
			if incumbent == nil || cell.Attempts > incumbent.Attempts {
				incumbent = cell
			}
		}
		incumbentPrice, ok := incumbent.costPerAdvance()
		incumbentRate, rateOK := incumbent.agentErrorRate()
		if !ok || !rateOK || incumbent.Attempts < tl1MinSample {
			continue
		}
		incumbentEscalation := float64(incumbent.Escalated) / float64(incumbent.Attempts)
		for _, cell := range group {
			price, priceOK := cell.costPerAdvance()
			rate, cellRateOK := cell.agentErrorRate()
			if cell == incumbent || !priceOK || !cellRateOK || cell.Attempts < tl1MinSample || price >= incumbentPrice*0.8 {
				continue
			}
			if rate > incumbentRate+0.05 || float64(cell.Escalated)/float64(cell.Attempts) > incumbentEscalation+0.05 {
				continue
			}
			savings := (incumbentPrice - price) * float64(incumbent.Advanced)
			opportunities = append(opportunities, map[string]any{"id": "switch:" + flavor + ":" + cell.Configuration, "kind": "cheaper_configuration", "flavor": flavor,
				"from_configuration": incumbent.Configuration, "to_configuration": cell.Configuration, "estimated_savings_usd": savings,
				"title": fmt.Sprintf("Run more %s on %s", flavor, cell.Configuration),
				"summary": fmt.Sprintf("%s costs %s per useful result on %s vs %s on %s, with an error rate of %s vs %s over %d and %d runs. Routing %s's %d useful runs to %s would have saved about %s.",
					flavor, tl1Dollars(price), cell.Configuration, tl1Dollars(incumbentPrice), incumbent.Configuration, tl1Percent(rate), tl1Percent(incumbentRate), cell.Attempts, incumbent.Attempts,
					incumbent.Configuration, incumbent.Advanced, cell.Configuration, tl1Dollars(savings)),
				"evidence": map[string]any{"incumbent": incumbent.row(), "alternative": cell.row()}, "prompt": tl1ExperimentPrompt(data, incumbent, cell)})
		}
	}
	for index, cluster := range clusters {
		// Clusters without meaningful spend are already listed as concerns.
		if index >= 8 || cluster.Cost < 1 {
			continue
		}
		opportunities = append(opportunities, map[string]any{"id": "fix:" + stableID("tl1-signature", cluster.Signature), "kind": "fix_error_cluster",
			"flavor": cluster.Flavors.top(1)[0]["key"], "estimated_savings_usd": cluster.Cost, "human_tasks_avoided": cluster.HumanFollowups,
			"title":    "Eliminate: " + cluster.Signature,
			"summary":  fmt.Sprintf("%d failed tasks cost %s and created %d human follow-ups.", len(cluster.Tasks), tl1Dollars(cluster.Cost), cluster.HumanFollowups),
			"evidence": cluster.row(), "prompt": tl1ClusterPrompt(data, cluster)})
	}
	sort.SliceStable(opportunities, func(i, j int) bool {
		left, right := opportunities[i]["estimated_savings_usd"].(float64), opportunities[j]["estimated_savings_usd"].(float64)
		if left != right {
			return left > right
		}
		return integer(opportunities[i]["human_tasks_avoided"]) > integer(opportunities[j]["human_tasks_avoided"])
	})
	return opportunities
}

func tl1Day(value string) string {
	if parsed, ok := parseTime(value); ok {
		return parsed.Local().Format("Jan 2 15:04")
	}
	return defaultString(value, "unknown")
}

// Prompt construction. Prompts are self-contained: an agent without Pharos can
// still act on them from the file paths; with Pharos MCP it can dig further.

func tl1PromptHeader(data *tl1Data, goal string) string {
	lines := []string{goal, "", "## TL1 installation"}
	for _, item := range [][2]string{{"Project", "project"}, {"Repository", "repository"}, {"TL1 config (flavor definitions)", "config_path"}, {"TL1 database (read-only; do not write)", "database_path"}, {"Transcripts", "transcripts_dir"}} {
		if value := firstString(data.Installation[item[1]]); value != "" {
			lines = append(lines, fmt.Sprintf("- %s: %s", item[0], value))
		}
	}
	if data.Since != "" || data.Until != "" {
		lines = append(lines, "- Analysis window: "+tl1WindowLabel(data)+". Pass since="+defaultString(nilIfEmpty(data.Since), "(none)")+tl1UntilArg(data)+" to Pharos TL1 tools to see the same window.")
	}
	lines = append(lines, "", "Pharos (local MCP server `pharos`) can help if connected: tl1_overview, tl1_flavor, tl1_errors, and tl1_candidate return this analysis; get_conversation_overview and search_conversation_passages open transcripts by conversation ID.")
	return strings.Join(lines, "\n")
}

func tl1TranscriptPath(data *tl1Data, task *tl1Task, attempt *tl1Attempt) string {
	directory := firstString(data.Installation["transcripts_dir"])
	if directory == "" || attempt == nil || len(attempt.Transcripts) == 0 {
		return ""
	}
	return filepath.Join(directory, task.ID, attempt.Transcripts[0])
}

func tl1TaskEvidence(data *tl1Data, tasks []*tl1Task, limit int) string {
	lines := []string{}
	for index := len(tasks) - 1; index >= 0 && len(lines) < limit; index-- {
		task := tasks[index]
		var final *tl1Attempt
		if count := len(task.Attempts); count > 0 {
			final = task.Attempts[count-1]
		}
		line := fmt.Sprintf("- task %s (%s, %s)", task.ID, task.Flavor, tl1Day(task.CreatedAt))
		if final != nil && final.Configuration != "" {
			line += " on " + final.Configuration
		}
		if path := tl1TranscriptPath(data, task, final); path != "" {
			line += "\n  transcript: " + path
		}
		if final != nil && final.Conversation != "" {
			line += "\n  Pharos conversation: " + final.Conversation
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func tl1FlavorDefinition(data *tl1Data, name string, templateLimit int) string {
	flavor := data.Flavors[name]
	if flavor == nil {
		return "(flavor definition not found in the TL1 snapshot)"
	}
	lines := []string{fmt.Sprintf("- name: %s (%s)", flavor.Name, flavor.ExecutionClass)}
	if flavor.DefaultConfiguration != "" {
		lines = append(lines, "- default agent configuration: "+flavor.DefaultConfiguration)
	}
	if len(flavor.Configurations) > 0 {
		lines = append(lines, "- allowed agent configurations: "+jsonText(flavor.Configurations))
	}
	if len(flavor.Transitions) > 0 {
		lines = append(lines, "- outcome transitions: "+jsonText(flavor.Transitions))
	}
	if len(flavor.Budgets) > 0 {
		lines = append(lines, "- budgets: "+jsonText(flavor.Budgets))
	}
	if flavor.ShapeVersion != "" {
		lines = append(lines, "- current shape version: "+flavor.ShapeVersion)
	}
	if templateLimit > 0 && flavor.Template != "" {
		lines = append(lines, "- prompt template:\n```\n"+tl1Clip(flavor.Template, templateLimit)+"\n```")
	}
	return strings.Join(lines, "\n")
}

func tl1ClusterPrompt(data *tl1Data, cluster *tl1Cluster) string {
	asks := map[string]string{
		tl1AttributionConfiguration:  "The executor CLI exited before doing work. Find the invocation or environment setting that causes it (flags, working directory trust, auth, sandbox) and give the exact TL1 configuration or harness change that fixes it.",
		tl1AttributionInfrastructure: "Workers died or were interrupted. Determine whether these share a cause (memory pressure, restarts, sleep, coordinator crashes) using the timing and resource data, and propose how TL1 should prevent or automatically retry them without a human.",
		tl1AttributionContract:       "The agent's handoff did not satisfy the flavor's output contract and TL1's repair failed. Compare the outputs schema with what the agent produced, then propose changes to the prompt template and/or schema so agents reliably produce valid handoffs.",
		tl1AttributionScript:         "A procedural step's script failed. Use the log tail to find the failing command and propose a fix to the script or its inputs, plus whether this failure should route to a human at all.",
		tl1AttributionProvider:       "The provider rejected or throttled requests. Propose parallelism, scheduling, or configuration-affinity changes that avoid the limit.",
	}
	ask := defaultString(asks[cluster.Attribution], "Diagnose the root cause from the transcripts and propose a concrete change to the TL1 configuration, prompt template, or environment that prevents it.")
	flavor := firstString(cluster.Flavors.top(1)[0]["key"])
	parts := []string{
		tl1PromptHeader(data, "Investigate a recurring TL1 failure and propose a fix."),
		"", "## Failure",
		fmt.Sprintf("- class: %s (%s)", cluster.Class, cluster.Attribution),
		"- signature: " + cluster.Signature,
		fmt.Sprintf("- occurrences: %d tasks, %s to %s", len(cluster.Tasks), tl1Day(cluster.First), tl1Day(cluster.Last)),
		"- flavors: " + tl1CounterText(cluster.Flavors),
		"- agent configurations: " + defaultString(tl1CounterText(cluster.Configurations), "none (procedural or human)"),
		fmt.Sprintf("- human follow-up tasks created: %d", cluster.HumanFollowups),
		fmt.Sprintf("- spend on failed runs: %s", tl1Dollars(cluster.Cost)),
		"", "Example error text:", "```", tl1Clip(cluster.Example, 1500), "```",
	}
	if cluster.LogTail != "" {
		parts = append(parts, "", "Script log tail:", "```", tl1Clip(cluster.LogTail, 1500), "```")
	}
	parts = append(parts, "", "## Recent examples", tl1TaskEvidence(data, cluster.Tasks, 5),
		"", "## Definition of the most affected flavor", tl1FlavorDefinition(data, flavor, 0),
		"", "## What to do", ask,
		"Read at least two example transcripts or logs before concluding. Report: root cause, evidence (file and line or transcript excerpt), the exact change, and how to verify it on the next TL1 run. Do not modify the TL1 database.")
	return strings.Join(parts, "\n")
}

func tl1CounterText(counter tl1Counter) string {
	parts := []string{}
	for _, item := range counter.top(5) {
		parts = append(parts, fmt.Sprintf("%s (%d)", item["key"], item["count"]))
	}
	return strings.Join(parts, ", ")
}

func tl1CellText(cell *tl1Cell) string {
	rate, _ := cell.agentErrorRate()
	median, _ := tl1Median(cell.Costs)
	duration, _ := tl1Median(cell.Durations)
	perAdvance, ok := cell.costPerAdvance()
	text := fmt.Sprintf("%s: %d runs, advanced %d, escalated %d, errors %d (agent error rate %s), median cost %s, median duration %.1f min",
		cell.Configuration, cell.Attempts, cell.Advanced, cell.Escalated, cell.Errors, tl1Percent(rate), tl1Dollars(median), duration/60000)
	if ok {
		text += ", cost per useful result " + tl1Dollars(perAdvance)
	}
	return text
}

func tl1CellTasks(data *tl1Data, cell *tl1Cell, disposition string) []*tl1Task {
	tasks := []*tl1Task{}
	for _, attempt := range data.Attempts {
		if attempt.Task.Flavor == cell.Flavor && attempt.Configuration == cell.Configuration && (disposition == "" || attempt.disposition() == disposition) {
			tasks = append(tasks, attempt.Task)
		}
	}
	return tasks
}

func tl1ReliabilityPrompt(data *tl1Data, cell, peer *tl1Cell) string {
	parts := []string{
		tl1PromptHeader(data, fmt.Sprintf("Find out why the %s flavor fails so often on the %s agent configuration.", cell.Flavor, cell.Configuration)),
		"", "## Performance", "- " + tl1CellText(cell), "- errors: " + tl1CounterText(cell.ErrorSignatures),
	}
	if peer != nil {
		parts = append(parts, "- comparison: "+tl1CellText(peer))
	}
	parts = append(parts, "", "## Failing runs", tl1TaskEvidence(data, tl1CellTasks(data, cell, tl1Errored), 5),
		"", "## Flavor definition", tl1FlavorDefinition(data, cell.Flavor, 4000),
		"", "## What to do",
		"Decide whether the failures come from the configuration (model, executor, effort, harness setup) or from the flavor itself, and recommend one of: fix the configuration, change the flavor's default or preferences, or change the prompt/outputs. Give the exact tl1.json edit and a way to verify it.")
	return strings.Join(parts, "\n")
}

func tl1ContractPrompt(data *tl1Data, flavor string, row map[string]any) string {
	tasks := []*tl1Task{}
	for _, task := range data.Tasks {
		if task.Flavor == flavor && task.ErrorClass == "output_contract" {
			tasks = append(tasks, task)
		}
	}
	outputs := ""
	if definition := data.Flavors[flavor]; definition != nil {
		outputs = firstString(definition.Row["outputs_schema_json"])
	}
	return strings.Join([]string{
		tl1PromptHeader(data, fmt.Sprintf("Make the %s flavor produce valid handoffs.", flavor)),
		"", "## Contract failures", fmt.Sprintf("- invalid handoffs: %d, repairs started: %d, recovered: %d, failed: %d", integer(row["invalid_handoffs"]), integer(row["repairs_started"]), integer(row["repairs_recovered"]), integer(row["repairs_failed"])),
		"- outputs schema: " + defaultString(outputs, "(not recorded)"),
		"", "## Failing runs", tl1TaskEvidence(data, tasks, 5),
		"", "## Flavor definition", tl1FlavorDefinition(data, flavor, 6000),
		"", "## What to do",
		"Find which fields or markers agents omit or mistype and why (ambiguous instructions, fields buried late in the template, schema too strict). Propose a revised output section for the template and any schema relaxation, and show it against two failing transcripts.",
	}, "\n")
}

func tl1OutlierPrompt(data *tl1Data, outliers []map[string]any) string {
	lines := []string{}
	for _, row := range outliers {
		task := data.TaskByID[firstString(row["task_id"])]
		line := fmt.Sprintf("- %s on %s: %s vs median %s (%.1f×), %s", row["flavor"], row["configuration"], tl1Dollars(row["cost_usd"].(float64)), tl1Dollars(row["median_cost_usd"].(float64)), row["ratio"], row["disposition"])
		if task != nil {
			for _, attempt := range task.Attempts {
				if attempt.ID == firstString(row["attempt_id"]) {
					if path := tl1TranscriptPath(data, task, attempt); path != "" {
						line += "\n  transcript: " + path
					}
				}
			}
		}
		lines = append(lines, line)
	}
	return strings.Join([]string{
		tl1PromptHeader(data, "Explain why these TL1 runs cost far more than typical runs of the same flavor and configuration."),
		"", "## Expensive runs", strings.Join(lines, "\n"),
		"", "## What to do",
		"For each of the top three, read the transcript and classify the excess: looping or retries, re-reading large files, long tool outputs in context, fighting the environment, or genuinely harder work. Then propose flavor-level changes (instructions, budgets, tool guidance, effort level) that would cut the excess without hurting outcomes.",
	}, "\n")
}

func tl1LoopPrompt(data *tl1Data, flavor string, near []map[string]any) string {
	edges := tl1Graph(data)["edges"].([]map[string]any)
	lines := []string{}
	for _, edge := range edges {
		if flavor != "" && edge["from"] != flavor && edge["to"] != flavor {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s —%s→ %s: %d", edge["from"], edge["outcome"], edge["to"], edge["count"]))
		if len(lines) >= 20 {
			break
		}
	}
	candidates := []string{}
	for _, row := range near {
		candidates = append(candidates, fmt.Sprintf("- %s: %s (%v/%v steps)", row["candidate_id"], row["title"], row["workflow_steps"], row["workflow_step_limit"]))
		if len(candidates) >= 10 {
			break
		}
	}
	goal := "Find why TL1 candidates loop through workflow steps without finishing."
	if flavor != "" {
		goal = fmt.Sprintf("Find why %s keeps running again on the same candidates.", flavor)
	}
	parts := []string{tl1PromptHeader(data, goal), "", "## Observed transitions", strings.Join(lines, "\n")}
	if len(candidates) > 0 {
		parts = append(parts, "", "## Candidates near their step budget", strings.Join(candidates, "\n"))
	}
	if flavor != "" {
		parts = append(parts, "", "## Flavor definition", tl1FlavorDefinition(data, flavor, 3000))
	}
	parts = append(parts, "", "## What to do", "Trace two looping candidates end to end (tl1_candidate in Pharos, or the TL1 task tree). Identify which handoff loses information or which review criterion keeps failing, and propose the change to templates, transitions, or visit caps that breaks the loop.")
	return strings.Join(parts, "\n")
}

func tl1HumanPrompt(data *tl1Data, human map[string]any) string {
	causes := []string{}
	for _, item := range human["causes"].([]map[string]any) {
		causes = append(causes, fmt.Sprintf("- %s: %d", item["key"], item["count"]))
	}
	return strings.Join([]string{
		tl1PromptHeader(data, "Reduce the human attention this TL1 workflow needs."),
		"", "## Human tasks", fmt.Sprintf("- total: %d, pending now: %d, created by an upstream error: %d", integer(human["tasks"]), integer(human["pending"]), integer(human["error_caused"])),
		"", "## What leads to a human task (predecessor flavor → outcome or error)", strings.Join(causes, "\n"),
		"", "## What to do",
		"Separate human tasks that make real product decisions from those that only restart failed automation. For the second kind, propose automatic retries or fixes for the underlying errors. For the first, propose clearer escalation summaries so each decision takes less time.",
	}, "\n")
}

func tl1FlavorPrompt(data *tl1Data, flavor, concern string) string {
	cells := []string{}
	matrix, _ := tl1Matrix(data)
	for _, row := range matrix {
		if row["flavor"] != flavor {
			continue
		}
		cells = append(cells, fmt.Sprintf("- %s: %v runs, advance rate %s, agent error rate %s, median cost %v, cost per useful result %v",
			row["configuration"], row["attempts"], tl1Percent(row["advance_rate"]), tl1Percent(row["agent_error_rate"]), tl1Money(row["median_cost_usd"]), tl1Money(row["cost_per_advance_usd"])))
	}
	return strings.Join([]string{
		tl1PromptHeader(data, fmt.Sprintf("Improve the %s TL1 flavor. %s", flavor, concern)),
		"", "## Performance by agent configuration", defaultString(strings.Join(cells, "\n"), "(no LLM runs in this window)"),
		"", "## Flavor definition", tl1FlavorDefinition(data, flavor, 8000),
		"", "## What to do", "Read three successful and three unsuccessful transcripts of this flavor. Propose specific template, schema, budget, or configuration changes, each with the evidence behind it and how to measure the effect on the next runs.",
	}, "\n")
}

func tl1Money(value any) string {
	if number, ok := value.(float64); ok {
		return tl1Dollars(number)
	}
	return "—"
}

func tl1ExperimentPrompt(data *tl1Data, incumbent, alternative *tl1Cell) string {
	return strings.Join([]string{
		tl1PromptHeader(data, fmt.Sprintf("Design a controlled trial of %s vs %s for the %s flavor.", alternative.Configuration, incumbent.Configuration, incumbent.Flavor)),
		"", "## Current evidence", "- incumbent " + tl1CellText(incumbent), "- alternative " + tl1CellText(alternative),
		"", "## Flavor definition", tl1FlavorDefinition(data, incumbent.Flavor, 0),
		"", "## What to do",
		"Check whether the two configurations saw comparable work (similar candidates and inputs, same shape version) or whether selection bias explains the difference. If the comparison holds, propose the tl1.json change (default configuration or preference weights) for a trial on a fixed share of new tasks, the number of runs needed to confirm the difference, and the metrics to watch (advance rate, escalations, downstream review findings, cost per useful result).",
	}, "\n")
}

// tl1ReviewPrompt is the starting point: the whole analysis condensed into a
// prompt asking for a prioritized optimization plan.
func tl1ReviewPrompt(data *tl1Data, overview map[string]any) string {
	totals := overview["totals"].(map[string]any)
	lines := []string{
		tl1PromptHeader(data, "Review this TL1 installation's performance and produce a prioritized optimization plan."),
		"", "## Totals",
		fmt.Sprintf("- tasks: %d; LLM attempts: %d; procedural attempts: %d; human tasks: %v (pending %v)", totals["tasks"], totals["llm_attempts"], totals["procedural_attempts"], totals["human_tasks"], totals["pending_human"]),
		fmt.Sprintf("- spend: %s; candidates: %v, finished: %v; cost per finished candidate: %s", tl1Dollars(totals["cost_usd"].(float64)), totals["candidates"], totals["finished_candidates"], tl1Money(totals["cost_per_finished_candidate_usd"])),
		fmt.Sprintf("- LLM error rate: %s (excluding infrastructure: %s)", tl1Percent(totals["llm_error_rate"]), tl1Percent(totals["agent_error_rate"])),
		"", "## Top concerns",
	}
	for index, detector := range overview["detectors"].([]map[string]any) {
		if index >= 8 {
			break
		}
		lines = append(lines, fmt.Sprintf("%d. [%s] %s — %s", index+1, detector["severity"], detector["title"], detector["summary"]))
	}
	lines = append(lines, "", "## Opportunities")
	for index, opportunity := range overview["opportunities"].([]map[string]any) {
		if index >= 5 {
			break
		}
		lines = append(lines, fmt.Sprintf("- %s — %s", opportunity["title"], opportunity["summary"]))
	}
	lines = append(lines, "", "## Flavor × configuration")
	for _, row := range overview["matrix"].([]map[string]any) {
		if row["attempts"].(int) < 5 {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s / %s: %v runs, advance %s, agent errors %s, median %s, per useful result %s", row["flavor"], row["configuration"], row["attempts"], tl1Percent(row["advance_rate"]), tl1Percent(row["agent_error_rate"]), tl1Money(row["median_cost_usd"]), tl1Money(row["cost_per_advance_usd"])))
	}
	lines = append(lines, "", "## What to do",
		"Verify the top three concerns against transcripts and the TL1 config before trusting them. Then produce a plan ordered by expected impact per unit of effort: for each item give the change (file and edit), the evidence, the expected effect on cost, errors, or human time, and how to measure it after the next runs. Flag anything where the data is too thin to act on.")
	return strings.Join(lines, "\n")
}

func tl1ClusterImpact(cluster *tl1Cluster) string {
	text := fmt.Sprintf("%d tasks failed this way", len(cluster.Tasks))
	if cluster.Cost >= 0.01 {
		text += fmt.Sprintf(", spending %s on the failed runs", tl1Dollars(cluster.Cost))
	}
	if cluster.HumanFollowups > 0 {
		text += fmt.Sprintf(", and %d needed a human to recover", cluster.HumanFollowups)
	}
	return text + "."
}
