package archive

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"alexandria/internal/querytable"
)

var usageTokenFields = []string{
	"total_tokens", "input_tokens", "uncached_input_tokens", "cache_read_input_tokens",
	"cache_creation_input_tokens", "cache_creation_5m_input_tokens", "cache_creation_1h_input_tokens",
	"output_tokens", "reasoning_output_tokens", "unclassified_tokens",
}

// usageTimeFields are the calendar buckets derived from the ledger hour.
var usageTimeFields = map[string]bool{"day": true, "week": true, "month": true}

// usageRows returns one row per agent session, local calendar day, and model.
// Linked mirrors of the same work (for example a Conductor workspace and the
// native Claude session it wraps) keep only the representative workspace, as
// Library does, so the same requests are not counted twice.
func (c *Catalog) usageRows(location *time.Location) ([]map[string]any, error) {
	ledger, err := c.usageLedger(nil)
	if err != nil {
		return nil, err
	}
	workspaces := []map[string]any{}
	seen := map[string]bool{}
	for _, row := range ledger {
		id := firstString(row["workspace_id"])
		if !seen[id] {
			seen[id] = true
			workspaces = append(workspaces, map[string]any{"id": id, "source_kind": row["source_kind"]})
		}
	}
	book, err := c.loadPriceBook()
	if err != nil {
		return nil, err
	}
	representative := map[string]bool{}
	for _, row := range c.suppressMirrors(workspaces) {
		representative[firstString(row["id"])] = true
	}
	grouped := map[string]map[string]any{}
	order := []string{}
	for _, entry := range ledger {
		if !representative[firstString(entry["workspace_id"])] {
			continue
		}
		model := ledgerModel(entry)
		var day, week, month, at any
		if hour, ok := parseTime(firstString(entry["usage_hour"])); ok {
			midnight := localMidnight(hour, location)
			day = midnight.Format("2006-01-02")
			week = midnight.AddDate(0, 0, -((int(midnight.Weekday()) + 6) % 7)).Format("2006-01-02")
			month = midnight.Format("2006-01")
			at = hour.UTC().Format(time.RFC3339)
		}
		key := firstString(entry["agent_session_id"]) + "|" + firstString(day) + "|" + model
		row := grouped[key]
		if row == nil {
			row = map[string]any{
				"id": key, "agent_session_id": entry["agent_session_id"], "conversation_id": entry["conversation_id"],
				"workspace_id": entry["workspace_id"], "title": entry["title"], "repository_name": entry["repository_name"],
				"source_kind": entry["source_kind"], "provider": entry["provider"], "model": model, "model_family": modelFamily(model),
				"session_kind": entry["session_kind"], "depth": integer(entry["depth"]), "attribution": entry["attribution"],
				"day": day, "week": week, "month": month, "first_usage_at": at, "last_usage_at": at,
				"cost_usd": nil, "cost_today_usd": nil, "price_status": "", "priced_model": nil,
			}
			for _, field := range usageTokenFields {
				row[field] = int64(0)
			}
			grouped[key] = row
			order = append(order, key)
		}
		tokens := ledgerTokens(entry)
		for _, field := range usageTokenFields {
			row[field] = integer(row[field]) + tokens[field]
		}
		priced := book.cost(model, firstString(day), tokens)
		row["cost_usd"] = addCost(row["cost_usd"], priced.cost)
		row["cost_today_usd"] = addCost(row["cost_today_usd"], priced.costToday)
		row["price_status"] = worsePriceStatus(firstString(row["price_status"]), priced.status)
		if priced.pricedModel != "" {
			row["priced_model"] = priced.pricedModel
		}
		if at != nil {
			if row["first_usage_at"] == nil || firstString(at) < firstString(row["first_usage_at"]) {
				row["first_usage_at"] = at
			}
			if firstString(at) > firstString(row["last_usage_at"]) {
				row["last_usage_at"] = at
			}
		}
		// A day that mixes exact and session-start placement is only as precise
		// as its least precise contribution.
		if firstString(entry["attribution"]) != "request" {
			row["attribution"] = entry["attribution"]
		}
	}
	rows := make([]map[string]any, 0, len(order))
	for _, key := range order {
		row := grouped[key]
		if input := integer(row["input_tokens"]); input > 0 {
			row["cache_read_share"] = float64(integer(row["cache_read_input_tokens"])) / float64(input) * 100
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// localUsageRows returns usageRows in local time, kept until the next commit
// or local day, since costs at today's prices follow the date.
func (c *Catalog) localUsageRows(ctx context.Context) ([]map[string]any, error) {
	key := "usage:" + time.Local.String() + ":" + c.clock().Format("2006-01-02")
	return cachedValue(ctx, c, key, func(context.Context) ([]map[string]any, error) { return c.usageRows(time.Local) })
}

// usageLedger reads priced ledger entries, optionally for specific workspaces.
func (c *Catalog) usageLedger(workspaceIDs []string) ([]map[string]any, error) {
	sums := make([]string, len(usageTokenFields))
	for index, field := range usageTokenFields {
		sums[index] = "u." + field
	}
	filter, args := "", []any{}
	if len(workspaceIDs) > 0 {
		filter = " AND a.workspace_id IN (" + placeholders(len(workspaceIDs)) + ")"
		for _, id := range workspaceIDs {
			args = append(args, id)
		}
	}
	return queryMaps(c.DB, `SELECT u.agent_session_id,u.usage_hour,u.model usage_model,u.attribution,`+strings.Join(sums, ",")+`,
			a.kind session_kind,a.depth,a.provider,a.model session_model,a.conversation_id,
			w.id workspace_id,w.title,w.source_kind,r.display_name repository_name
		FROM agent_session_usage u
		JOIN agent_sessions a ON a.id=u.agent_session_id
		JOIN workspaces w ON w.id=a.workspace_id
		LEFT JOIN repositories r ON r.id=w.repository_id
		WHERE u.total_tokens>0`+filter, args...)
}

func ledgerModel(entry map[string]any) string {
	model := defaultString(nilIfEmpty(strings.TrimSpace(firstString(entry["usage_model"]))), strings.TrimSpace(firstString(entry["session_model"])))
	return defaultString(model, "Unknown model")
}

func ledgerTokens(entry map[string]any) map[string]int64 {
	tokens := make(map[string]int64, len(usageTokenFields))
	for _, field := range usageTokenFields {
		tokens[field] = integer(entry[field])
	}
	return tokens
}

func localMidnight(instant time.Time, location *time.Location) time.Time {
	local := instant.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
}

// attachWorkspaceCosts adds each Library row's API-equivalent cost. Large
// result sets read the whole ledger rather than an oversized IN list.
func (c *Catalog) attachWorkspaceCosts(ctx context.Context, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	var totals map[string]*workspaceUsageTotal
	var err error
	if len(rows) <= 500 {
		ids := []string{}
		for _, row := range rows {
			ids = append(ids, firstString(row["id"]))
		}
		totals, err = c.workspaceUsage(ids)
	} else {
		totals, err = c.allWorkspaceUsage(ctx)
	}
	if err != nil {
		return err
	}
	for _, row := range rows {
		row["cost_usd"], row["cost_today_usd"], row["price_status"] = nil, nil, nil
		if value := totals[firstString(row["id"])]; value != nil {
			row["cost_usd"], row["cost_today_usd"], row["price_status"] = value.cost, value.today, value.status
		}
	}
	return nil
}

// workspaceUsageTotal is one workspace's reconciled tokens and priced cost.
type workspaceUsageTotal struct {
	tokens      int64
	cost, today any
	status      string
}

// add folds another workspace's usage into this one.
func (total *workspaceUsageTotal) add(other *workspaceUsageTotal) {
	total.tokens += other.tokens
	if cost, ok := other.cost.(float64); ok {
		total.cost = addCost(total.cost, &cost)
	}
	if today, ok := other.today.(float64); ok {
		total.today = addCost(total.today, &today)
	}
	total.status = worsePriceStatus(total.status, other.status)
}

// allWorkspaceUsage returns workspaceUsage for every workspace, kept until the
// next commit or local day. The totals are shared and must not be modified.
func (c *Catalog) allWorkspaceUsage(ctx context.Context) (map[string]*workspaceUsageTotal, error) {
	return cachedValue(ctx, c, "workspace-usage:"+c.clock().Format("2006-01-02"), func(context.Context) (map[string]*workspaceUsageTotal, error) {
		return c.workspaceUsage(nil)
	})
}

// workspaceUsage totals the priced ledger per workspace, for the given
// workspaces or, with none, all of them.
func (c *Catalog) workspaceUsage(workspaceIDs []string) (map[string]*workspaceUsageTotal, error) {
	ledger, err := c.usageLedger(workspaceIDs)
	if err != nil {
		return nil, err
	}
	book, err := c.loadPriceBook()
	if err != nil {
		return nil, err
	}
	totals := map[string]*workspaceUsageTotal{}
	for _, entry := range ledger {
		day := ""
		if hour, ok := parseTime(firstString(entry["usage_hour"])); ok {
			day = localMidnight(hour, time.Local).Format("2006-01-02")
		}
		tokens := ledgerTokens(entry)
		priced := book.cost(ledgerModel(entry), day, tokens)
		id := firstString(entry["workspace_id"])
		if totals[id] == nil {
			totals[id] = &workspaceUsageTotal{}
		}
		total := totals[id]
		total.tokens += tokens["total_tokens"]
		total.cost = addCost(total.cost, priced.cost)
		total.today = addCost(total.today, priced.costToday)
		total.status = worsePriceStatus(total.status, priced.status)
	}
	return totals, nil
}

var (
	modelDateSuffix    = regexp.MustCompile(`-\d{8}$`)
	modelContextSuffix = regexp.MustCompile(`(-1m|\[1m\])$`)
)

// modelFamily folds provider prefixes, snapshot dates, and context-window
// variants so "claude-opus-5-5", "opus-5-5-1m", and "claude-opus-5-5[1m]"
// group together. The exact reported model remains in the model field.
func modelFamily(model string) string {
	family := strings.ToLower(strings.TrimSpace(model))
	family = strings.TrimPrefix(family, "claude-")
	family = modelContextSuffix.ReplaceAllString(family, "")
	family = modelDateSuffix.ReplaceAllString(family, "")
	return family
}

// sortUsageTimeBuckets orders metrics grouped by a calendar field newest first,
// so breakdowns read as a timeline instead of by magnitude.
func sortUsageTimeBuckets(request querytable.AggregationRequest, result querytable.AggregationResult) {
	for index, aggregation := range request.Aggregations {
		if index >= len(result.Metrics) {
			break
		}
		position := -1
		for groupIndex, field := range aggregation.GroupBy {
			if usageTimeFields[field] {
				position = groupIndex
				break
			}
		}
		if position < 0 {
			continue
		}
		buckets := result.Metrics[index].Buckets
		sort.SliceStable(buckets, func(i, j int) bool {
			return firstString(buckets[i].Keys[position]) > firstString(buckets[j].Keys[position])
		})
	}
}
