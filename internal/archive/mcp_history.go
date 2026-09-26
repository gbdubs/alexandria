package archive

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// mcpQueryRows feeds the shared query-table engine. The log is capped at 5,000
// calls, and only the already-sanitized arguments (never response text) are read.
func (c *Catalog) mcpQueryRows(ctx context.Context) ([]map[string]any, error) {
	rows, err := queryMapsContext(ctx, c.DB, `SELECT id,called_at,tool_name,arguments_json,status,error_text,duration_ms,
		response_bytes,estimated_output_tokens,result_count,truncated FROM mcp_calls`)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		at, ok := parseTime(firstString(row["called_at"]))
		if ok {
			at = at.In(time.Local)
			row["day"] = at.Format("2006-01-02")
			row["week"] = at.AddDate(0, 0, -((int(at.Weekday()) + 6) % 7)).Format("2006-01-02")
			row["month"] = at.Format("2006-01")
		}
		row["call_count"] = 1
		if firstString(row["status"]) == "ok" {
			row["error_count"] = 0
		} else {
			row["error_count"] = 1
		}
		if integer(row["truncated"]) != 0 {
			row["truncated_count"] = 1
			row["truncated"] = true
		} else {
			row["truncated_count"] = 0
			row["truncated"] = false
		}
	}
	return rows, nil
}

// MCP is enabled by default to preserve existing local stdio integrations.
// The catalog setting is shared by the app and already-running MCP processes.
func (c *Catalog) MCPEnabled() (bool, error) {
	var value string
	err := c.DB.QueryRow("SELECT value FROM meta WHERE key='mcp_enabled'").Scan(&value)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return value != "false", err
}

func (c *Catalog) SetMCPEnabled(enabled bool) error {
	value := "false"
	if enabled {
		value = "true"
	}
	_, err := c.DB.Exec("INSERT INTO meta(key,value) VALUES('mcp_enabled',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", value)
	return err
}

var mcpLoggedArguments = []string{
	"query", "repository", "source", "provider", "file", "pr", "from", "to",
	"limit", "offset", "max_output_tokens", "conversation_id", "workspace_id",
	"message_id", "around_message_id", "text_offset", "change_set_id", "task", "name",
}

func mcpArgumentSummary(args map[string]any) string {
	summary := map[string]any{}
	for _, key := range mcpLoggedArguments {
		if value, ok := args[key]; ok && value != nil {
			if text, ok := value.(string); ok {
				summary[key] = clipText(text, 300)
			} else {
				summary[key] = value
			}
		}
	}
	return jsonText(summary)
}

func mcpResultCount(value any) *int {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	switch items := object["items"].(type) {
	case []map[string]any:
		count := len(items)
		return &count
	case []any:
		count := len(items)
		return &count
	}
	return nil
}

func mcpResultTruncated(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if object["truncated"] == true || object["next_text_offset"] != nil {
		return true
	}
	items, ok := object["items"].([]map[string]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if item["next_text_offset"] != nil {
			return true
		}
	}
	return false
}

func (c *Catalog) RecordMCPCall(name string, args map[string]any, status, errorText string, duration time.Duration, responseBytes int, resultCount *int, truncated bool) error {
	if len(errorText) > 500 {
		errorText = errorText[:500]
	}
	var count any
	if resultCount != nil {
		count = *resultCount
	}
	_, err := c.DB.Exec(`INSERT INTO mcp_calls(called_at,tool_name,arguments_json,status,error_text,duration_ms,response_bytes,estimated_output_tokens,result_count,truncated)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, now(), clipText(name, 100), mcpArgumentSummary(args), status,
		nilIfEmpty(errorText), duration.Milliseconds(), responseBytes, (responseBytes+2)/3, count, boolInt(truncated))
	if err != nil {
		return err
	}
	_, err = c.DB.Exec(`DELETE FROM mcp_calls WHERE id <= COALESCE((SELECT id FROM mcp_calls ORDER BY id DESC LIMIT 1 OFFSET 5000),0)`)
	return err
}

func (c *Catalog) MCPCallHistory(limit, offset int, tool, status string) (map[string]any, error) {
	limit = clamp(limit, 1, 100)
	offset = max(offset, 0)
	where := []string{}
	args := []any{}
	if tool != "" {
		where = append(where, "tool_name=?")
		args = append(args, tool)
	}
	if status != "" {
		where = append(where, "status=?")
		args = append(args, status)
	}
	predicate := ""
	if len(where) > 0 {
		predicate = " WHERE " + strings.Join(where, " AND ")
	}
	filtered, err := queryMaps(c.DB, "SELECT COUNT(*) total FROM mcp_calls"+predicate, args...)
	if err != nil {
		return nil, err
	}
	args = append(args, limit, offset)
	rows, err := queryMaps(c.DB, `SELECT id,called_at,tool_name,arguments_json,status,error_text,duration_ms,
		response_bytes,estimated_output_tokens,result_count,truncated FROM mcp_calls`+predicate+` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	stats, err := queryMaps(c.DB, `SELECT COUNT(*) total_calls,
		COALESCE(SUM(CASE WHEN status<>'ok' THEN 1 ELSE 0 END),0) failed_calls,
		COALESCE(ROUND(AVG(estimated_output_tokens)),0) average_output_tokens,
		COALESCE(MAX(estimated_output_tokens),0) largest_output_tokens,
		COALESCE(SUM(CASE WHEN truncated<>0 THEN 1 ELSE 0 END),0) truncated_calls
		FROM mcp_calls`)
	if err != nil {
		return nil, err
	}
	tools, err := queryMaps(c.DB, `SELECT tool_name,COUNT(*) calls,
		SUM(CASE WHEN status<>'ok' THEN 1 ELSE 0 END) errors,
		ROUND(AVG(estimated_output_tokens)) average_output_tokens,
		MAX(estimated_output_tokens) largest_output_tokens,
		SUM(CASE WHEN truncated<>0 THEN 1 ELSE 0 END) truncated_calls,
		ROUND(AVG(duration_ms)) average_duration_ms
		FROM mcp_calls GROUP BY tool_name ORDER BY calls DESC,tool_name`)
	if err != nil {
		return nil, err
	}
	return map[string]any{"items": rows, "stats": stats[0], "tool_stats": tools,
		"filtered_total": filtered[0]["total"], "limit": limit, "offset": offset}, nil
}

func (c *Catalog) MCPStatus(config Config) (map[string]any, error) {
	enabled, err := c.MCPEnabled()
	if err != nil {
		return nil, err
	}
	if config.Library {
		// A command on the library drive would pin it and die with it.
		launcher := mcpLauncherPath(pharosSupportDir())
		_, missing := os.Stat(launcher)
		note := "Agent clients start this launcher on this Mac. It runs Pharos from this Mac's disk and opens the library only while answering, so its drive can be ejected while agents stay connected; tool calls report the library as not connected until the drive is back. Disabling MCP rejects tool calls, including from already-connected clients."
		if missing != nil {
			note += " The launcher is not installed yet; run `pharos install-mcp`."
		}
		return map[string]any{"enabled": enabled, "transport": "stdio", "command": launcher, "args": []string{},
			"launcher_installed": missing == nil, "note": note}, nil
	}
	executable := config.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate Pharos executable: %w", err)
		}
	}
	executable, _ = filepath.Abs(executable)
	configPath, _ := filepath.Abs(config.Path)
	return map[string]any{"enabled": enabled, "transport": "stdio", "command": executable,
		"args": []string{"--config", configPath, "mcp"},
		"note": strings.TrimSpace("Agent clients launch this local command when they connect. Disabling MCP rejects tool calls, including from already-connected clients.")}, nil
}
