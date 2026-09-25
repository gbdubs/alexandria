package archive

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"strings"

	"alexandria/internal/querytable"
	"modernc.org/sqlite"
)

// SQLite functions must exist before the catalog opens its connections.
func init() {
	registerSQLiteRegexp()
	_ = sqlite.RegisterDeterministicScalarFunction("model_family", 1, func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		model, _ := args[0].(string)
		if strings.TrimSpace(model) == "" {
			return nil, nil
		}
		return modelFamily(model), nil
	})
}

// toolRollupDataset serves the Tools summary: one row per local day and
// summary dimension (tool, shell program, model, repository, agent kind).
var toolRollupDataset = func() sqlDataset {
	columns := map[string]string{}
	for _, name := range []string{"id", "day", "week", "month", "repository_name", "source_kind", "provider", "model", "model_family", "session_kind",
		"tool_name", "tool_category", "mcp_server", "program", "subcommand", "command_name", "command_category", "call_count", "error_count", "error_rate",
		"no_result_count", "rejected_count", "interrupted_count", "timeout_count", "nonzero_exit_count", "hook_blocked_count", "truncated_count",
		"timed_count", "total_duration_ms", "avg_duration_ms", "max_duration_ms", "input_bytes", "result_bytes", "result_tokens", "measured_count",
		"avg_result_tokens", "carried_tokens", "output_tokens", "lines_added", "lines_removed", "work_count", "context_cost_usd", "output_cost_usd",
		"tool_cost_usd", "price_status"} {
		columns[name] = "d." + name
	}
	return sqlDataset{from: "FROM tool_usage_daily d", columns: columns}
}()

// toolCallDataset serves one row per tool call, excluding mirrored work.
var toolCallDataset = sqlDataset{
	from: `FROM tool_calls t JOIN workspaces w ON w.id=t.workspace_id LEFT JOIN repositories r ON r.id=w.repository_id
		LEFT JOIN agent_sessions a ON a.id=t.agent_session_id`,
	base: "t.workspace_id NOT IN (SELECT workspace_id FROM tool_mirror_workspaces)",
	columns: map[string]string{
		"id": "t.id", "call_id": "t.call_id", "sequence": "t.sequence",
		"started_at": "t.started_at", "ended_at": "t.ended_at",
		"day":             "date(t.started_at,'localtime')",
		"week":            "date(t.started_at,'localtime','-6 days','weekday 1')",
		"month":           "strftime('%Y-%m',t.started_at,'localtime')",
		"repository_name": "r.display_name", "title": "w.title", "workspace_id": "t.workspace_id", "conversation_id": "t.conversation_id",
		"source_kind": "w.source_kind", "provider": "t.provider", "model": "t.model", "model_family": "model_family(t.model)",
		"session_kind": "COALESCE(a.kind,'root')", "agent_depth": "COALESCE(a.depth,0)",
		"kind": "t.kind", "tool_name": "t.tool_name", "tool_category": "t.tool_category", "mcp_server": "t.mcp_server",
		"command": "t.command", "program": "t.program", "subcommand": "t.subcommand", "command_category": "t.command_category",
		"command_name":  "NULLIF(TRIM(COALESCE(t.program,'')||' '||COALESCE(t.subcommand,'')),'')",
		"command_count": "t.command_count", "has_pipe": "t.has_pipe", "has_redirect": "t.has_redirect", "has_heredoc": "t.has_heredoc",
		"backgrounded": "t.backgrounded", "file_path": "t.file_path",
		"status": "t.status", "error_type": "t.error_type", "exit_code": "t.exit_code", "interrupted": "t.interrupted", "truncated": "t.truncated",
		"call_count": "1", "error_count": "(t.status='error')",
		"duration_ms": "t.duration_ms", "duration_source": "t.duration_source",
		"input_bytes": "t.input_bytes", "result_bytes": "t.result_bytes", "result_tokens": "t.result_tokens",
		"result_tokens_source": "t.result_tokens_source", "output_tokens": "t.output_tokens", "parallel_count": "t.parallel_count",
		"carried_requests": "t.carried_requests", "carried_tokens": "t.carried_tokens",
		"lines_added": "t.lines_added", "lines_removed": "t.lines_removed",
		"url": "t.url", "host": "t.host", "hosts": "t.hosts", "url_count": "t.url_count", "search_query": "t.search_query",
	},
}

func sqlDatasetFor(dataset string) (sqlDataset, bool) {
	switch dataset {
	case "tools":
		return toolRollupDataset, true
	case "tool_calls":
		return toolCallDataset, true
	}
	return sqlDataset{}, false
}

// priceToolCalls adds each page row's context and output cost, priced like
// the rollup (see rebuildToolRollup).
func (c *Catalog) priceToolCalls(rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	book, err := c.loadPriceBook()
	if err != nil {
		return err
	}
	for _, row := range rows {
		day := firstString(row["day"])
		model := defaultString(nilIfEmpty(strings.TrimSpace(firstString(row["model"]))), "Unknown model")
		context := map[string]int64{"cache_read_input_tokens": integer(row["carried_tokens"])}
		if firstString(row["provider"]) == "claude" {
			context["cache_creation_input_tokens"] = integer(row["result_tokens"])
		} else {
			context["uncached_input_tokens"] = integer(row["result_tokens"])
		}
		output, _ := number(row["output_tokens"])
		contextCost := book.cost(model, day, context)
		outputCost := book.cost(model, day, map[string]int64{"output_tokens": int64(output + 0.5)})
		row["context_cost_usd"] = costValue(contextCost.cost)
		row["output_cost_usd"] = costValue(outputCost.cost)
		row["tool_cost_usd"] = costValue(addCost(addCost(nil, contextCost.cost), outputCost.cost))
		row["price_status"] = worsePriceStatus(contextCost.status, outputCost.status)
		for _, flag := range []string{"has_pipe", "has_redirect", "has_heredoc", "backgrounded", "interrupted", "truncated"} {
			row[flag] = integer(row[flag]) != 0
		}
	}
	return nil
}

// toolCallDetail returns one call with its input, a bounded excerpt of its
// result, its shell commands, and the requests around it.
func (c *Catalog) toolCallDetail(ctx context.Context, id string) (map[string]any, error) {
	result, err := toolCallDataset.Rows(ctx, c.DB, querytable.Query{Where: []querytable.WhereTerm{{Field: "id", Op: "=", Value: id}}, Limit: 1},
		querytable.Schema{IDField: "id", Fields: map[string]querytable.Field{"id": {Name: "id", Kind: querytable.Text, Filterable: true, Sortable: true}}})
	if err != nil {
		return nil, err
	}
	if len(result.Rows) == 0 {
		return nil, nil
	}
	call := result.Rows[0]
	if err := c.priceToolCalls(result.Rows); err != nil {
		return nil, err
	}
	links, err := queryMapsContext(ctx, c.DB, `SELECT t.call_message_id,t.result_message_id,t.request_id,t.next_request_id FROM tool_calls t WHERE t.id=?`, id)
	if err != nil || len(links) == 0 {
		return call, err
	}
	link := links[0]
	messageText := func(messageID any) string {
		var text string
		if messageID != nil {
			_ = c.DB.QueryRowContext(ctx, "SELECT text FROM messages WHERE id=?", messageID).Scan(&text)
		}
		return text
	}
	// Show the tool input and result content rather than the stored envelopes.
	var input, envelope map[string]any
	if text := messageText(link["call_message_id"]); text != "" {
		if json.Unmarshal([]byte(text), &input) != nil {
			call["input_text"] = clipText(text, 20000)
		} else if script, ok := input["input"].(string); ok {
			call["input_text"] = clipText(script, 20000)
		} else if pretty, err := json.MarshalIndent(input["input"], "", "  "); err == nil {
			call["input_text"] = clipText(string(pretty), 20000)
		}
	}
	if text := messageText(link["result_message_id"]); text != "" {
		content := text
		if json.Unmarshal([]byte(text), &envelope) == nil {
			content, _ = toolResultText(envelope["content"])
			if details := mapValue(envelope["details"]); len(details) > 0 {
				if pretty, err := json.MarshalIndent(details, "", "  "); err == nil {
					call["result_details"] = clipText(string(pretty), 5000)
				}
			}
		}
		call["result_text"], call["result_text_bytes"] = clipText(content, 20000), len(content)
	}
	commands, err := queryMapsContext(ctx, c.DB, `SELECT position,operator,command,program,subcommand,category,exit_code,duration_ms
		FROM tool_commands WHERE tool_call_id=? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	call["commands"] = commands
	urls, err := queryMapsContext(ctx, c.DB, `SELECT position,url,host,source FROM tool_urls WHERE tool_call_id=? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	call["urls"] = urls
	for key, requestID := range map[string]any{"request": link["request_id"], "next_request": link["next_request_id"]} {
		if requestID == nil {
			continue
		}
		requests, err := queryMapsContext(ctx, c.DB, `SELECT id,native_id,requested_at,model,input_tokens,cache_read_input_tokens,
			cache_creation_input_tokens,output_tokens,context_growth_tokens,compacted_before,tool_call_count FROM model_requests WHERE id=?`, requestID)
		if err != nil {
			return nil, err
		}
		if len(requests) == 1 {
			call[key] = requests[0]
		}
	}
	return call, nil
}
