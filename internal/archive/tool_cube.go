package archive

import (
	"strconv"
	"strings"
)

// tool_call_cube groups tool calls by workspace, local day, and every field
// with few values, keeping each group's call count and the sums and ranges of
// its numbers. It has a quarter of tool_calls' rows at a fraction of their
// width, so the Tool calls table answers counts, metrics, filter values, and
// column stats from it whenever a request reads only those fields (see
// sqlCube), and tool_usage_daily is summed from it rather than from the calls.
// Both are rebuilt together (see rebuildToolRollup) and leave out mirrored
// workspaces.

// toolCubeGroups are the cube's grouping columns and the values they hold,
// over toolCallDataset's joins. Title, repository, and source follow from the
// workspace, so they add no groups.
var toolCubeGroups = func() []struct{ column, expr string } {
	groups := []struct{ column, expr string }{
		{"workspace_id", "t.workspace_id"}, {"title", "w.title"}, {"repository_name", "r.display_name"},
		{"source_kind", "w.source_kind"}, {"day", "date(t.started_at,'localtime')"},
		{"session_kind", "COALESCE(a.kind,'root')"}, {"agent_depth", "COALESCE(a.depth,0)"},
	}
	for _, column := range []string{"provider", "model", "kind", "tool_name", "tool_category", "mcp_server", "program", "subcommand",
		"command_category", "status", "error_type", "exit_code", "interrupted", "truncated", "has_pipe", "has_redirect", "has_heredoc",
		"backgrounded", "host", "duration_source", "result_tokens_source"} {
		groups = append(groups, struct{ column, expr string }{column, "t." + column})
	}
	// Fields with too many values to group by are kept as present or absent.
	for _, field := range toolCubePresence {
		groups = append(groups, struct{ column, expr string }{"present_" + field, "(COALESCE(t." + field + ",'')<>'')"})
	}
	return groups
}()

// toolCubePresence lists the text fields the cube keeps only as present
// (non-empty) or absent.
var toolCubePresence = []string{"command", "file_path", "url", "search_query", "hosts"}

// toolCubeMeasures are the number and time fields whose per-group ranges the
// cube keeps, with sums for the numbers.
var toolCubeMeasures = []struct {
	field string
	sum   bool
}{
	{"duration_ms", true}, {"input_bytes", true}, {"result_bytes", true}, {"result_tokens", true}, {"output_tokens", true},
	{"parallel_count", true}, {"carried_requests", true}, {"carried_tokens", true}, {"lines_added", true}, {"lines_removed", true},
	{"command_count", true}, {"url_count", true}, {"started_at", false}, {"ended_at", false},
}

// toolCubeSelect groups the calls toolCallDataset serves into cube rows,
// leaving out the workspaces listed in the mirrors table.
func toolCubeSelect(mirrors string) string {
	columns, positions := []string{}, []string{}
	for index, group := range toolCubeGroups {
		columns = append(columns, group.expr+" "+group.column)
		positions = append(positions, strconv.Itoa(index+1))
	}
	columns = append(columns, "COUNT(*) call_count")
	for _, measure := range toolCubeMeasures {
		value := "NULLIF(t." + measure.field + ",'')"
		if measure.sum {
			columns = append(columns, "SUM(t."+measure.field+") "+measure.field+"_sum")
		}
		columns = append(columns, "MIN("+value+") "+measure.field+"_min", "MAX("+value+") "+measure.field+"_max", "COUNT("+value+") "+measure.field+"_n")
	}
	return "SELECT " + strings.Join(columns, ",") + " " + toolCallDataset.from + " WHERE t.workspace_id NOT IN (SELECT workspace_id FROM " + mirrors + ")" +
		" GROUP BY " + strings.Join(positions, ",")
}

// toolRollupFromCube sums the cube into tool_usage_daily's groups and counts.
const toolRollupFromCube = `SELECT day,repository_name,source_kind,provider,COALESCE(model,'') model,session_kind,
		tool_name,tool_category,mcp_server,program,subcommand,command_category,
		SUM(call_count) call_count,SUM(CASE WHEN status='error' THEN call_count ELSE 0 END) error_count,
		SUM(CASE WHEN status='no_result' THEN call_count ELSE 0 END) no_result_count,
		SUM(CASE WHEN error_type='user_rejected' THEN call_count ELSE 0 END) rejected_count,SUM(interrupted*call_count) interrupted_count,
		SUM(CASE WHEN error_type='timeout' THEN call_count ELSE 0 END) timeout_count,
		SUM(CASE WHEN error_type='nonzero_exit' THEN call_count ELSE 0 END) nonzero_exit_count,
		SUM(CASE WHEN error_type='hook_blocked' THEN call_count ELSE 0 END) hook_blocked_count,SUM(truncated*call_count) truncated_count,
		SUM(duration_ms_n) timed_count,COALESCE(SUM(duration_ms_sum),0) total_duration_ms,MAX(duration_ms_max) max_duration_ms,
		SUM(input_bytes_sum) input_bytes,SUM(result_bytes_sum) result_bytes,SUM(result_tokens_sum) result_tokens,
		SUM(CASE WHEN result_tokens_source='measured' THEN call_count ELSE 0 END) measured_count,
		SUM(carried_tokens_sum) carried_tokens,SUM(output_tokens_sum) output_tokens,
		COALESCE(SUM(lines_added_sum),0) lines_added,COALESCE(SUM(lines_removed_sum),0) lines_removed,COUNT(DISTINCT workspace_id) work_count
	FROM tool_call_cube GROUP BY 1,2,3,4,5,6,7,8,9,10,11,12`

// toolCallCube describes tool_call_cube to the Tool calls table.
var toolCallCube = func() *sqlCube {
	cube := &sqlCube{from: "FROM tool_call_cube", count: "call_count", dimensions: map[string]string{
		"week": "date(day,'-6 days','weekday 1')", "month": "strftime('%Y-%m',day)", "model_family": "model_family(model)",
		"command_name": "NULLIF(TRIM(COALESCE(program,'')||' '||COALESCE(subcommand,'')),'')",
		// Per-call values that follow from a group.
		"call_count": "1", "error_count": "(status='error')",
	}, present: map[string]string{}, measures: map[string]cubeMeasure{}}
	for _, group := range toolCubeGroups {
		if !strings.HasPrefix(group.column, "present_") {
			cube.dimensions[group.column] = group.column
		}
	}
	for _, field := range toolCubePresence {
		cube.present[field] = "present_" + field
	}
	for _, measure := range toolCubeMeasures {
		columns := cubeMeasure{min: measure.field + "_min", max: measure.field + "_max", count: measure.field + "_n"}
		if measure.sum {
			columns.sum = measure.field + "_sum"
		}
		cube.measures[measure.field] = columns
	}
	return cube
}()
