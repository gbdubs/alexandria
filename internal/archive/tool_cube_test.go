package archive

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/gbdubs/pharos/internal/querytable"
)

// seedToolCalls writes n tool calls with varied values, including nulls and
// empty strings, across a few workspaces and sessions.
func seedToolCalls(t *testing.T, catalog *Catalog, n int) {
	t.Helper()
	random := rand.New(rand.NewSource(7))
	pick := func(values ...any) any { return values[random.Intn(len(values))] }
	tx, err := catalog.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO repositories(id,display_name,created_at,updated_at) VALUES('repo-1','alpha','2026-09-01T00:00:00.000Z','2026-09-01T00:00:00.000Z')")
	for w := range 4 {
		repository := any("repo-1")
		if w == 3 {
			repository = nil
		}
		exec(`INSERT INTO workspaces(id,source_kind,source_account,source_id,repository_id,title,indexed_at) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprint("ws-", w), pick("claude", "codex"), "local", fmt.Sprint("source-", w), repository, fmt.Sprint("Work ", w), "2026-09-01T00:00:00.000Z")
		exec(`INSERT INTO conversations(id,workspace_id,provider,account,native_id) VALUES(?,?,?,?,?)`,
			fmt.Sprint("conv-", w), fmt.Sprint("ws-", w), "claude", "local", fmt.Sprint("native-", w))
		exec(`INSERT INTO agent_sessions(id,workspace_id,conversation_id,native_id,kind,provider,depth) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprint("session-", w), fmt.Sprint("ws-", w), fmt.Sprint("conv-", w), "main", pick("root", "subagent"), "claude", w%2)
	}
	start := time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)
	for index := range n {
		w := random.Intn(4)
		started := start.Add(time.Duration(random.Intn(5*24*60)) * time.Minute)
		program := pick("git", "go", "rg", nil, "")
		exec(`INSERT INTO tool_calls(id,workspace_id,conversation_id,agent_session_id,sequence,provider,model,kind,tool_name,tool_category,
				mcp_server,command,program,subcommand,command_category,has_pipe,file_path,host,hosts,url,search_query,started_at,ended_at,
				duration_ms,duration_source,status,error_type,exit_code,interrupted,truncated,input_bytes,result_bytes,result_tokens,
				result_tokens_source,output_tokens,carried_requests,carried_tokens,lines_added,lines_removed)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprint("call-", index), fmt.Sprint("ws-", w), fmt.Sprint("conv-", w), pick(fmt.Sprint("session-", w), nil), index,
			pick("claude", "codex"), pick("claude-opus-5-5", "opus-5-5[1m]", "gpt-5", nil, ""), pick("tool_call", "delegation"),
			pick("Bash", "Read", "Edit", "WebFetch"), pick("command", "read", "web"), pick(nil, "pharos"),
			pick("git status", "go test ./...", nil, ""), program, pick("status", "test", nil), pick("vcs", "test", nil),
			random.Intn(2), pick("/a/b.go", nil, ""), pick("github.com", "example.com", nil, ""), pick(`["github.com"]`, "[]", nil),
			pick("https://github.com/x", nil), pick("sqlite", nil, ""), formatTime(started), pick(formatTime(started.Add(time.Second)), nil),
			pick(12, 3400, 0, nil), pick("reported", "timestamps", nil), pick("ok", "ok", "error", "no_result"),
			pick(nil, "nonzero_exit", "user_rejected", "timeout"), pick(nil, 0, 1, 127), random.Intn(2), random.Intn(2),
			random.Intn(500), random.Intn(9000), random.Intn(3000), pick("measured", "estimated", nil), random.Float64()*400,
			random.Intn(5), random.Intn(20000), pick(3, nil), pick(1, nil))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// toolRollupFromCalls is how tool_usage_daily was grouped before
// tool_call_cube: directly from the calls.
const toolRollupFromCalls = `SELECT date(t.started_at,'localtime') day,r.display_name repository_name,w.source_kind,t.provider,
		COALESCE(t.model,'') model,COALESCE(a.kind,'root') session_kind,t.tool_name,t.tool_category,t.mcp_server,t.program,t.subcommand,t.command_category,
		COUNT(*) call_count,SUM(COALESCE(t.status,'')='error') error_count,SUM(COALESCE(t.status,'')='no_result') no_result_count,
		SUM(COALESCE(t.error_type,'')='user_rejected') rejected_count,SUM(t.interrupted) interrupted_count,SUM(COALESCE(t.error_type,'')='timeout') timeout_count,
		SUM(COALESCE(t.error_type,'')='nonzero_exit') nonzero_exit_count,SUM(COALESCE(t.error_type,'')='hook_blocked') hook_blocked_count,SUM(t.truncated) truncated_count,
		COUNT(t.duration_ms) timed_count,COALESCE(SUM(t.duration_ms),0) total_duration_ms,MAX(t.duration_ms) max_duration_ms,
		SUM(t.input_bytes) input_bytes,SUM(t.result_bytes) result_bytes,SUM(t.result_tokens) result_tokens,
		SUM(COALESCE(t.result_tokens_source,'')='measured') measured_count,SUM(t.carried_tokens) carried_tokens,SUM(t.output_tokens) output_tokens,
		COALESCE(SUM(t.lines_added),0) lines_added,COALESCE(SUM(t.lines_removed),0) lines_removed,COUNT(DISTINCT t.workspace_id) work_count
	FROM tool_calls t
	JOIN workspaces w ON w.id=t.workspace_id
	LEFT JOIN repositories r ON r.id=w.repository_id
	LEFT JOIN agent_sessions a ON a.id=t.agent_session_id
	WHERE t.workspace_id NOT IN (SELECT workspace_id FROM tool_mirror_workspaces)
	GROUP BY 1,2,3,4,5,6,7,8,9,10,11,12`

// same compares two results that should agree. Floats may differ in the
// last places: sums taken in another order, or scores from float32 vectors.
func same(left, right any) bool {
	a, b := reflect.ValueOf(left), reflect.ValueOf(right)
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	if x, ok := number(left); ok && a.Kind() == reflect.Float64 {
		y, ok := number(right)
		return ok && math.Abs(x-y) <= 1e-7*math.Max(1, math.Abs(x))
	}
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case reflect.Slice:
		if a.Len() != b.Len() {
			return false
		}
		for index := range a.Len() {
			if !same(a.Index(index).Interface(), b.Index(index).Interface()) {
				return false
			}
		}
		return true
	case reflect.Map:
		if a.Len() != b.Len() {
			return false
		}
		for _, key := range a.MapKeys() {
			if !same(a.MapIndex(key).Interface(), b.MapIndex(key).Interface()) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for index := range a.NumField() {
			if !same(a.Field(index).Interface(), b.Field(index).Interface()) {
				return false
			}
		}
		return true
	case reflect.Pointer:
		return a.IsNil() == b.IsNil() && (a.IsNil() || same(a.Elem().Interface(), b.Elem().Interface()))
	}
	return reflect.DeepEqual(left, right)
}

func TestToolCallCubeAnswersLikeTheCalls(t *testing.T) {
	catalog, _ := testCatalog(t)
	seedToolCalls(t, catalog, 3000)
	ctx := context.Background()
	if err := catalog.ensureToolRollup(ctx); err != nil {
		t.Fatal(err)
	}
	document, err := querySchemaDocument("tool_calls")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := querytable.LoadSchema(document)
	if err != nil {
		t.Fatal(err)
	}
	direct := toolCallDataset
	direct.cube, direct.countFrom, direct.buckets = nil, "", nil

	// The rollup summed from the cube is the one grouped from the calls.
	fromCube, err := queryMaps(catalog.DB, toolRollupFromCube)
	if err != nil {
		t.Fatal(err)
	}
	fromCalls, err := queryMaps(catalog.DB, toolRollupFromCalls)
	if err != nil {
		t.Fatal(err)
	}
	key := func(row map[string]any) string {
		return fmt.Sprint(row["day"], row["repository_name"], row["source_kind"], row["provider"], row["model"], row["session_kind"],
			row["tool_name"], row["tool_category"], row["mcp_server"], row["program"], row["subcommand"], row["command_category"])
	}
	byKey := map[string]map[string]any{}
	for _, row := range fromCalls {
		byKey[key(row)] = row
	}
	if len(fromCube) != len(fromCalls) || len(fromCube) < 100 {
		t.Fatalf("rollup has %d groups from the cube, %d from the calls", len(fromCube), len(fromCalls))
	}
	for _, row := range fromCube {
		if want := byKey[key(row)]; !same(row, want) {
			t.Fatalf("rollup group from the cube:\n%v\nfrom the calls:\n%v", row, want)
		}
	}

	wheres := [][]querytable.WhereTerm{
		nil,
		{{Field: "tool_name", Op: "=", Value: "Bash"}},
		{{Field: "status", Op: "=", Value: "error"}, {Field: "repository_name", Op: "is_not_null"}},
		{{Field: "error_type", Op: "=", Value: "user_rejected"}},
		{{Field: "exit_code", Op: "!=", Value: "0"}, {Field: "exit_code", Op: "is_not_null"}},
		{{Field: "host", Op: "is_not_null"}},
		{{Field: "search_query", Op: "is_not_null"}},
		{{Field: "hosts", Op: "is_null"}},
		{{Field: "command", Op: "is_null", Negated: true}},
		{{Any: []querytable.WhereClause{{Field: "command_category", Op: "=", Value: "test"}, {Field: "model_family", Op: "contains", Value: "opus"}}}},
		{{Field: "day", Op: "=", Value: "2026-09-01"}, {Field: "program", Op: "=", Value: "git", Negated: true}},
		{{Field: "week", Op: "=", Value: "2026-08-31"}, {Field: "session_kind", Op: "=", Value: "subagent"}},
		{{Field: "month", Op: "=", Value: "2026-09"}, {Field: "agent_depth", Op: ">", Value: "0"}},
		{{Field: "day", Op: "=", Value: "2026-09-02"}, {Field: "title", Op: "starts_with", Value: "work"}},
		{{Field: "command_name", Op: "=", Value: "git status"}},
		{{Field: "has_pipe", Op: "=", Value: "true"}, {Field: "model", Op: "is_null"}},
		// Fields the cube cannot filter on: answered from the calls.
		{{Field: "command", Op: "contains", Value: "test"}},
		{{Field: "duration_ms", Op: ">", Value: "100"}, {Field: "tool_name", Op: "=", Value: "Read"}},
		{{Field: "started_at", Op: ">=", Value: "2026-09-02T00:00:00Z"}},
	}
	// Every measure the cube routes, for a few filters; counts for the rest.
	full, counts := []querytable.Aggregation{{ID: "calls", Op: "count"}}, []querytable.Aggregation{{ID: "calls", Op: "count"}}
	for _, group := range [][]string{nil, {"day"}, {"week", "tool_category"}, {"tool_name"}, {"error_type"}, {"command_name", "exit_code"}, {"host", "tool_name"}, {"model_family"}, {"repository_name"}} {
		counts = append(counts, querytable.Aggregation{ID: fmt.Sprint("count", group), Op: "count", GroupBy: group},
			querytable.Aggregation{ID: fmt.Sprint("duration", group), Op: "sum", Field: "duration_ms", GroupBy: group})
		full = append(full, querytable.Aggregation{ID: fmt.Sprint("count", group), Op: "count", GroupBy: group})
		for _, field := range []string{"error_count", "call_count", "duration_ms", "output_tokens", "lines_added", "exit_code", "interrupted"} {
			for _, op := range []string{"sum", "avg", "min", "max", "count"} {
				full = append(full, querytable.Aggregation{ID: fmt.Sprint(op, field, group), Op: op, Field: field, GroupBy: group})
			}
		}
		full = append(full,
			querytable.Aggregation{ID: fmt.Sprint("distinct", group), Op: "count_distinct", Field: "tool_name", GroupBy: group},
			querytable.Aggregation{ID: fmt.Sprint("started", group), Op: "max", Field: "started_at", GroupBy: group},
			querytable.Aggregation{ID: fmt.Sprint("programs", group), Op: "count", Field: "program", GroupBy: group})
	}
	for index, where := range wheres {
		aggregations := counts
		if index%6 == 0 {
			aggregations = full
		}
		query := querytable.Query{Where: where, OrderBy: []querytable.OrderBy{{Field: "started_at", Dir: "desc"}}, Limit: 25}
		got, err := toolCallDataset.Rows(ctx, catalog.DB, query, schema)
		if err != nil {
			t.Fatalf("%v: %v", where, err)
		}
		want, err := direct.Rows(ctx, catalog.DB, query, schema)
		if err != nil {
			t.Fatal(err)
		}
		if got.Total == 0 || !same(got, want) {
			t.Fatalf("rows where %v: total %d, want %d", where, got.Total, want.Total)
		}
		request := querytable.AggregationRequest{Where: where, Aggregations: aggregations}
		gotMetrics, err := toolCallDataset.Aggregate(ctx, catalog.DB, request, schema)
		if err != nil {
			t.Fatalf("%v: %v", where, err)
		}
		wantMetrics, err := direct.Aggregate(ctx, catalog.DB, request, schema)
		if err != nil {
			t.Fatal(err)
		}
		for index := range wantMetrics.Metrics {
			if !same(gotMetrics.Metrics[index], wantMetrics.Metrics[index]) {
				t.Fatalf("where %v, metric %s:\ncube  %v\ncalls %v", where, wantMetrics.Metrics[index].ID, gotMetrics.Metrics[index].Buckets, wantMetrics.Metrics[index].Buckets)
			}
		}
	}
	for _, field := range []string{"tool_name", "day", "week", "command_name", "model_family", "exit_code", "host", "title", "error_type"} {
		for _, search := range []string{"", "o"} {
			got, err := toolCallDataset.Distinct(ctx, catalog.DB, field, search, 50, schema)
			if err != nil {
				t.Fatal(err)
			}
			want, err := direct.Distinct(ctx, catalog.DB, field, search, 50, schema)
			if err != nil {
				t.Fatal(err)
			}
			if !same(got, want) {
				t.Fatalf("distinct %s %q: cube %v, calls %v", field, search, got, want)
			}
		}
	}
	names := []string{}
	for name := range schema.Fields {
		names = append(names, name)
	}
	got, err := toolCallDataset.FieldStats(ctx, catalog.DB, names, schema)
	if err != nil {
		t.Fatal(err)
	}
	want, err := direct.FieldStats(ctx, catalog.DB, names, schema)
	if err != nil {
		t.Fatal(err)
	}
	for name, stat := range got {
		expected := want[name]
		if stat.Distinct == nil {
			expected.Distinct = nil
		}
		if !same(stat, expected) {
			t.Fatalf("field stats %s: cube %+v, calls %+v", name, stat, expected)
		}
	}
	for _, name := range []string{"tool_name", "day", "duration_ms", "started_at"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("field stats left out %s: %v", name, got)
		}
	}
}
