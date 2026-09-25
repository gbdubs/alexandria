package archive

import "fmt"

var tl1MCPWindow = map[string]any{
	"installation": map[string]any{"type": "string", "description": "TL1 installation ID; defaults to the most recently active"},
	"since":        map[string]any{"type": "string", "description": "Only tasks created at or after this ISO 8601 time. \"latest\" starts at the latest large enqueue, which isolates the most recent flavor revisions; tl1_overview lists recent enqueues"},
	"until":        map[string]any{"type": "string", "description": "Only tasks created before this ISO 8601 time; with since, analyzes one enqueue period"},
	"days":         map[string]any{"type": "integer", "minimum": 0, "description": "Only tasks created in the last N days (0 = all); ignored when since is set"},
	"scope":        map[string]any{"type": "string", "enum": []string{"all", "current"}, "description": "current keeps only runs of each flavor's current definition"},
}

func tl1MCPProperties(extra map[string]any) map[string]any {
	properties := map[string]any{}
	for key, value := range tl1MCPWindow {
		properties[key] = value
	}
	for key, value := range extra {
		properties[key] = value
	}
	return properties
}

var tl1MCPTools = []map[string]any{
	{"name": "tl1_overview", "description": "Summarize a TL1 installation's performance: totals, coverage, ranked concerns, savings opportunities, and cost/error by flavor and agent configuration. Start a TL1 optimization here.", "inputSchema": map[string]any{"type": "object", "properties": tl1MCPProperties(nil)}},
	{"name": "tl1_detector", "description": "Get one concern or opportunity from tl1_overview by ID, with its evidence and a ready-to-use investigation prompt.", "inputSchema": map[string]any{"type": "object", "required": []string{"id"}, "properties": tl1MCPProperties(map[string]any{"id": map[string]any{"type": "string"}})}},
	{"name": "tl1_flavor", "description": "Get one TL1 flavor's definition (template, configurations, transitions, budgets), per-configuration performance, definition history, and recent failures.", "inputSchema": map[string]any{"type": "object", "required": []string{"name"}, "properties": tl1MCPProperties(map[string]any{"name": map[string]any{"type": "string"}})}},
	{"name": "tl1_errors", "description": "List TL1 error clusters (normalized failure signatures) with counts, attribution, affected flavors and configurations, and sample tasks.", "inputSchema": map[string]any{"type": "object", "properties": tl1MCPProperties(map[string]any{"flavor": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "maximum": 50}})}},
	{"name": "tl1_candidate", "description": "Trace one TL1 candidate: its tasks in order with outcome, configuration, cost, and errors, plus events, review findings, and human touches.", "inputSchema": map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{"installation": tl1MCPWindow["installation"], "id": map[string]any{"type": "string"}}}},
}

func init() { mcpTools = append(mcpTools, tl1MCPTools...) }

// callTL1MCP answers the tl1_* tools. ok is false for other tool names.
func callTL1MCP(catalog *Catalog, name string, args map[string]any) (value any, ok bool, err error) {
	installation, window := firstString(args["installation"]), tl1WindowFromArgs(args)
	switch name {
	case "tl1_overview":
		overview, err := catalog.TL1Overview(installation, window)
		if err != nil {
			return nil, true, err
		}
		return tl1CompactOverview(overview), true, nil
	case "tl1_detector":
		overview, err := catalog.TL1Overview(installation, window)
		if err != nil {
			return nil, true, err
		}
		id := firstString(args["id"])
		for _, key := range []string{"detectors", "opportunities"} {
			items, _ := overview[key].([]map[string]any)
			for _, item := range items {
				if item["id"] == id {
					return item, true, nil
				}
			}
		}
		return nil, true, fmt.Errorf("no TL1 detector or opportunity with id %q", id)
	case "tl1_flavor":
		detail, err := catalog.TL1Flavor(installation, firstString(args["name"]), window)
		if err == nil {
			if definition, found := detail["definition"].(map[string]any); found {
				definition["template"] = nilIfEmpty(tl1Clip(firstString(definition["template"]), 8000))
			}
		}
		return detail, true, err
	case "tl1_errors":
		overview, err := catalog.TL1Overview(installation, window)
		if err != nil {
			return nil, true, err
		}
		limit := int(integer(valueOr(args["limit"], 15)))
		flavor := firstString(args["flavor"])
		items := []map[string]any{}
		for _, cluster := range overview["errors"].([]map[string]any) {
			if flavor != "" && !tl1CountsInclude(cluster["flavors"], flavor) {
				continue
			}
			if len(items) >= min(limit, 50) {
				break
			}
			items = append(items, cluster)
		}
		return map[string]any{"items": items}, true, nil
	case "tl1_candidate":
		value, err := catalog.TL1Candidate(installation, firstString(args["id"]))
		return value, true, err
	}
	return nil, false, nil
}

func tl1CountsInclude(value any, key string) bool {
	rows, _ := value.([]map[string]any)
	for _, row := range rows {
		if row["key"] == key {
			return true
		}
	}
	return false
}

// tl1CompactOverview drops prompts and evidence so the overview fits an agent's
// context; tl1_detector returns them for one item.
func tl1CompactOverview(overview map[string]any) map[string]any {
	strip := func(key string, limit int) []map[string]any {
		items, _ := overview[key].([]map[string]any)
		compact := []map[string]any{}
		for _, item := range items {
			if len(compact) >= limit {
				break
			}
			row := map[string]any{}
			for field, value := range item {
				if field != "prompt" && field != "evidence" {
					row[field] = value
				}
			}
			compact = append(compact, row)
		}
		return compact
	}
	matrix := []map[string]any{}
	rows, _ := overview["matrix"].([]map[string]any)
	for _, row := range rows {
		compact := map[string]any{}
		for _, field := range []string{"flavor", "configuration", "attempts", "advance_rate", "escalation_rate", "agent_error_rate", "median_cost_usd", "cost_per_advance_usd", "median_duration_ms", "cache_read_share", "low_sample", "flag", "default"} {
			compact[field] = row[field]
		}
		matrix = append(matrix, compact)
	}
	enqueues, _ := overview["enqueues"].([]map[string]any)
	return map[string]any{"installation": overview["installation"], "window": overview["window"], "recent_enqueues": enqueues[:min(len(enqueues), 5)], "scope": overview["scope"], "totals": overview["totals"],
		"coverage": overview["coverage"], "detectors": strip("detectors", 20), "opportunities": strip("opportunities", 10), "matrix": matrix,
		"note": "Use tl1_detector with an id for evidence and an investigation prompt; tl1_flavor for a flavor's definition."}
}
