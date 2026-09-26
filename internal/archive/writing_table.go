package archive

import (
	"context"
	"math"
	"slices"
	"sort"
	"strings"

	"alexandria/internal/querytable"
)

// The Usage page's writing view lists one row per Library work with the
// classified user text of its conversations (see authorship.go). A work that
// only holds sub-agent conversations spawned from another work (Codex stores
// each spawned thread as its own session) is folded into that parent, so its
// automated prompts and its tokens count toward the conversation that caused
// them.

// writingGroups combine categories the way the page reports them.
var writingGroups = []struct {
	name       string
	categories []string
}{
	{"copied", []string{spanQuoted, spanResent}},
	{"prompt", []string{spanTemplate, spanAttachment}},
	{"machine", []string{spanHarness, spanAutomated}},
}

// writingDay is one work's classified user text on one local day.
type writingDay struct {
	day                     string
	messages, typedMessages int64
	longestTyped            int64
	firstSent, lastSent     string
	counts                  map[string]int64
}

func (day *writingDay) add(other *writingDay) {
	day.messages += other.messages
	day.typedMessages += other.typedMessages
	day.longestTyped = max(day.longestTyped, other.longestTyped)
	if day.firstSent == "" || other.firstSent != "" && other.firstSent < day.firstSent {
		day.firstSent = other.firstSent
	}
	day.lastSent = max(day.lastSent, other.lastSent)
	for column, value := range other.counts {
		day.counts[column] += value
	}
}

func newWritingDay(day string) *writingDay { return &writingDay{day: day, counts: map[string]int64{}} }

// writingTable is the writing rows and, per row ID, its daily text.
type writingTable struct {
	rows  []map[string]any
	daily map[string]map[string]*writingDay
}

// writingData returns the writing rows and, per row ID, its daily text. They
// are kept until the next commit or local day (costs at today's prices follow
// the date), and must not be modified.
func (c *Catalog) writingData(ctx context.Context) ([]map[string]any, map[string]map[string]*writingDay, error) {
	c.ensureAuthorship()
	table, err := cachedValue(ctx, c, "writing:"+c.clock().Format("2006-01-02"), c.computeWritingData)
	return table.rows, table.daily, err
}

func (c *Catalog) computeWritingData(ctx context.Context) (writingTable, error) {
	parents, err := c.subagentParents(ctx)
	if err != nil {
		return writingTable{}, err
	}
	sums := []string{"workspace_id", "day", "COUNT(*) messages", "SUM(typed_words>0) typed_messages", "MAX(typed_words) longest_typed", "MIN(sent_at) first_sent", "MAX(sent_at) last_sent"}
	for _, column := range authorshipColumns() {
		sums = append(sums, "SUM("+column+") "+column)
	}
	result, err := c.DB.QueryContext(ctx, "SELECT "+strings.Join(sums, ",")+" FROM message_authorship GROUP BY workspace_id,day")
	if err != nil {
		return writingTable{}, err
	}
	defer result.Close()
	columns := authorshipColumns()
	daily := map[string]map[string]*writingDay{}
	members := map[string][]string{}
	for result.Next() {
		var workspace string
		entry := newWritingDay("")
		values := make([]int64, len(columns))
		targets := []any{&workspace, &entry.day, &entry.messages, &entry.typedMessages, &entry.longestTyped, &entry.firstSent, &entry.lastSent}
		for index := range values {
			targets = append(targets, &values[index])
		}
		if err := result.Scan(targets...); err != nil {
			return writingTable{}, err
		}
		for index, column := range columns {
			entry.counts[column] = values[index]
		}
		root := rootWorkspace(workspace, parents)
		if daily[root] == nil {
			daily[root] = map[string]*writingDay{}
		}
		if !slices.Contains(members[root], workspace) {
			members[root] = append(members[root], workspace)
		}
		if daily[root][entry.day] == nil {
			daily[root][entry.day] = newWritingDay(entry.day)
		}
		daily[root][entry.day].add(entry)
	}
	if err := result.Err(); err != nil {
		return writingTable{}, err
	}
	works, err := queryMapsContext(ctx, c.DB, `SELECT w.id,w.title,w.source_kind,r.display_name repository_name,
			(SELECT GROUP_CONCAT(DISTINCT cx.provider) FROM conversations cx WHERE cx.workspace_id=w.id) providers
		FROM workspaces w LEFT JOIN repositories r ON r.id=w.repository_id`)
	if err != nil {
		return writingTable{}, err
	}
	usage, err := c.allWorkspaceUsage(ctx)
	if err != nil {
		return writingTable{}, err
	}
	// Folded sub-agent works that sent no user-role text still spent tokens.
	for child := range parents {
		if root := rootWorkspace(child, parents); daily[root] != nil && !slices.Contains(members[root], child) {
			members[root] = append(members[root], child)
		}
	}
	rows := []map[string]any{}
	for _, work := range works {
		id := firstString(work["id"])
		days := daily[id]
		if days == nil {
			continue
		}
		total := newWritingDay("")
		for _, day := range days {
			total.add(day)
		}
		spent := &workspaceUsageTotal{}
		for _, member := range members[id] {
			if value := usage[member]; value != nil {
				spent.add(value)
			}
		}
		rows = append(rows, writingRow(work, total, spent, len(members[id])-1))
	}
	sort.SliceStable(rows, func(i, j int) bool { return firstString(rows[i]["id"]) < firstString(rows[j]["id"]) })
	return writingTable{rows, daily}, nil
}

func writingRow(work map[string]any, total *writingDay, spent *workspaceUsageTotal, folded int) map[string]any {
	row := map[string]any{
		"id": work["id"], "workspace_id": work["id"], "title": work["title"], "repository_name": work["repository_name"],
		"source_kind": work["source_kind"], "providers": work["providers"],
		"first_message_at": nilIfEmpty(total.firstSent), "last_message_at": nilIfEmpty(total.lastSent),
		"user_turns": total.messages, "typed_turns": total.typedMessages, "longest_typed_words": total.longestTyped,
		"total_tokens": spent.tokens, "cost_usd": spent.cost, "cost_today_usd": spent.today, "price_status": nilIfEmpty(spent.status),
		"subagent_works": int64(folded),
	}
	words := int64(0)
	for _, category := range authorshipCategories {
		value := total.counts[category+"_words"]
		row[category+"_words"] = value
		words += value
	}
	for _, group := range writingGroups {
		value := int64(0)
		for _, category := range group.categories {
			value += total.counts[category+"_words"]
		}
		row[group.name+"_words"] = value
	}
	typed := total.counts[spanTyped+"_words"]
	row["user_words"] = words
	row["typed_share"], row["avg_typed_words"], row["typed_per_mtok"] = nil, nil, nil
	if words > 0 {
		row["typed_share"] = roundTenth(float64(typed) / float64(words) * 100)
	}
	if total.typedMessages > 0 {
		row["avg_typed_words"] = math.Round(float64(typed) / float64(total.typedMessages))
	}
	if spent.tokens > 0 {
		row["typed_per_mtok"] = roundTenth(float64(typed) / (float64(spent.tokens) / 1e6))
	}
	return row
}

func roundTenth(value float64) float64 { return math.Round(value*10) / 10 }

// subagentParents maps each work holding only sub-agent conversations whose
// parents live in another work to that work.
func (c *Catalog) subagentParents(ctx context.Context) (map[string]string, error) {
	rows, err := queryMapsContext(ctx, c.DB, `SELECT c.workspace_id child,MIN(p.workspace_id) parent
		FROM conversations c JOIN conversations p ON p.id=c.parent_id
		WHERE p.workspace_id<>c.workspace_id
		GROUP BY c.workspace_id
		HAVING COUNT(*)=(SELECT COUNT(*) FROM conversations x WHERE x.workspace_id=c.workspace_id)`)
	if err != nil {
		return nil, err
	}
	parents := map[string]string{}
	for _, row := range rows {
		parents[firstString(row["child"])] = firstString(row["parent"])
	}
	return parents, nil
}

// rootWorkspace follows parent links to the work a sub-agent chain started in.
func rootWorkspace(id string, parents map[string]string) string {
	for range 32 {
		parent, ok := parents[id]
		if !ok || parent == id {
			break
		}
		id = parent
	}
	return id
}

// writingSeries sums, per day, the classified text of the writing rows that
// match where, and totals it, so the page's chart and breakdown follow the
// table's filters.
func (c *Catalog) writingSeries(ctx context.Context, where []querytable.WhereTerm, schema querytable.Schema) (map[string]any, error) {
	rows, daily, err := c.writingData(ctx)
	if err != nil {
		return nil, err
	}
	matched, err := querytable.Apply(rows, querytable.Query{Where: where, Limit: len(rows) + 1}, schema)
	if err != nil {
		return nil, err
	}
	byDay := map[string]*writingDay{}
	total := newWritingDay("")
	for _, row := range matched.Rows {
		for day, entry := range daily[firstString(row["id"])] {
			if byDay[day] == nil {
				byDay[day] = newWritingDay(day)
			}
			byDay[day].add(entry)
			total.add(entry)
		}
	}
	series := make([]map[string]any, 0, len(byDay))
	for _, entry := range byDay {
		series = append(series, writingCounts(entry, map[string]any{"day": entry.day}))
	}
	sort.Slice(series, func(i, j int) bool { return firstString(series[i]["day"]) < firstString(series[j]["day"]) })
	first, last := "", ""
	if len(series) > 0 {
		first, last = firstString(series[0]["day"]), firstString(series[len(series)-1]["day"])
	}
	return map[string]any{
		"works": matched.Total, "daily": series,
		"totals": writingCounts(total, map[string]any{"first_day": nilIfEmpty(first), "last_day": nilIfEmpty(last)}),
	}, nil
}

func writingCounts(entry *writingDay, into map[string]any) map[string]any {
	into["messages"], into["typed_messages"] = entry.messages, entry.typedMessages
	for _, column := range authorshipColumns() {
		into[column] = entry.counts[column]
	}
	return into
}

// writingSeriesRequest is the body of POST /api/query/writing/series.
type writingSeriesRequest struct {
	Where []querytable.WhereTerm `json:"where"`
}
