package archive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

var mcpTools = []map[string]any{
	{"name": "search_conversations", "description": "Find relevant past conversations. Returns compact ranked cards and message handles, never transcripts. Start here; then inspect an overview or passages.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "repository": map[string]any{"type": "string"}, "source": map[string]any{"type": "string"}, "provider": map[string]any{"type": "string"}, "file": map[string]any{"type": "string"}, "pr": map[string]any{"type": "integer"}, "from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}, "offset": map[string]any{"type": "integer", "minimum": 0}, "max_output_tokens": map[string]any{"type": "integer", "minimum": 200, "maximum": 4000}}}},
	{"name": "get_conversation_overview", "description": "Get an extractive, cited overview of one conversation before reading messages.", "inputSchema": map[string]any{"type": "object", "required": []string{"conversation_id"}, "properties": map[string]any{"conversation_id": map[string]any{"type": "string"}, "max_output_tokens": map[string]any{"type": "integer", "minimum": 200, "maximum": 4000}}}},
	{"name": "search_conversation_passages", "description": "Find short matching passages within a chosen conversation; returns message IDs for focused reading.", "inputSchema": map[string]any{"type": "object", "required": []string{"conversation_id", "query"}, "properties": map[string]any{"conversation_id": map[string]any{"type": "string"}, "query": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 10}, "max_output_tokens": map[string]any{"type": "integer", "minimum": 200, "maximum": 4000}}}},
	{"name": "get_conversation_messages", "description": "Read a small bounded message window, optionally centered on a message ID. For a long message, request message_id with next_text_offset to continue its text.", "inputSchema": map[string]any{"type": "object", "required": []string{"conversation_id"}, "properties": map[string]any{"conversation_id": map[string]any{"type": "string"}, "around_message_id": map[string]any{"type": "string"}, "message_id": map[string]any{"type": "string"}, "text_offset": map[string]any{"type": "integer", "minimum": 0}, "offset": map[string]any{"type": "integer", "minimum": 0}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 12}, "max_output_tokens": map[string]any{"type": "integer", "minimum": 200, "maximum": 4000}}}},
	{"name": "search_work", "description": "Search archived AI work with structured repository, source, file, and PR filters.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "repository": map[string]any{"type": "string"}, "source": map[string]any{"type": "string"}, "file": map[string]any{"type": "string"}, "pr": map[string]any{"type": "integer"}, "limit": map[string]any{"type": "integer", "maximum": 100}}, "additionalProperties": true}},
	{"name": "get_work_detail", "description": "Get bounded evidence-linked work metadata, changes, metrics, PRs, and receipt.", "inputSchema": map[string]any{"type": "object", "required": []string{"workspace_id"}, "properties": map[string]any{"workspace_id": map[string]any{"type": "string"}}}},
	{"name": "get_conversation_excerpt", "description": "Compatibility alias for bounded get_conversation_messages.", "inputSchema": map[string]any{"type": "object", "required": []string{"conversation_id"}, "properties": map[string]any{"conversation_id": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "maximum": 12}, "offset": map[string]any{"type": "integer"}, "max_output_tokens": map[string]any{"type": "integer", "maximum": 4000}}}},
	{"name": "get_change_set", "description": "Get a change inventory and preserved patch locator.", "inputSchema": map[string]any{"type": "object", "required": []string{"change_set_id"}, "properties": map[string]any{"change_set_id": map[string]any{"type": "string"}}}},
	{"name": "trace", "description": "Trace work by changed file, PR number, or TL1 task/source ID.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"file": map[string]any{"type": "string"}, "pr": map[string]any{"type": "integer"}, "task": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "maximum": 100}}}},
	{"name": "query_metrics", "description": "Rank work by a metric while retaining unknown/missing coverage.", "inputSchema": map[string]any{"type": "object", "required": []string{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "minimum": map[string]any{"type": "number"}, "maximum": map[string]any{"type": "number"}, "limit": map[string]any{"type": "integer", "maximum": 100}}}},
	{"name": "get_receipt", "description": "Retrieve a preservation/reclamation receipt by receipt or operation ID.", "inputSchema": map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{"id": map[string]any{"type": "string"}}}},
}

// serveMCP answers requests, one JSON object per line, until input ends.
// After each answer it calls next, if set, with the input read but not yet
// answered.
func serveMCP(input io.Reader, output io.Writer, handle func(map[string]any) map[string]any, next func(pending []byte)) error {
	reader := bufio.NewReaderSize(input, 64*1024)
	encoder := json.NewEncoder(output)
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr == nil || len(line) > 0 {
			var request map[string]any
			if err := json.Unmarshal(line, &request); err != nil {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": err.Error()}})
			} else if response := handle(request); response != nil {
				if err := encoder.Encode(response); err != nil {
					return err
				}
			}
		}
		if readErr == io.EOF {
			return nil
		} else if readErr != nil {
			return readErr
		}
		if next != nil {
			pending, _ := reader.Peek(reader.Buffered())
			next(pending)
		}
	}
}
func mcpResult(request map[string]any, value any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": value}
}

// mcpToolError is a failed tools/call in the form handleMCP reports errors.
func mcpToolError(request map[string]any, message string) map[string]any {
	return mcpResult(request, map[string]any{"content": []map[string]any{{"type": "text", "text": jsonText(message)}}, "isError": true})
}

// handleMCP answers one request. Only tools/list and tools/call use catalog,
// which may be nil for the others.
func handleMCP(catalog *Catalog, request map[string]any) map[string]any {
	method := firstString(request["method"])
	if method == "notifications/initialized" {
		return nil
	}
	if method == "initialize" {
		params := mapValueDefault(request["params"])
		return mcpResult(request, map[string]any{"protocolVersion": defaultString(params["protocolVersion"], "2025-06-18"), "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "ai-work-archive", "version": "0.2.0-go"}})
	}
	if method == "tools/list" {
		enabled, err := catalog.MCPEnabled()
		if err != nil {
			return map[string]any{"jsonrpc": "2.0", "id": request["id"], "error": map[string]any{"code": -32603, "message": "MCP setting unavailable"}}
		}
		if !enabled {
			return mcpResult(request, map[string]any{"tools": []any{}})
		}
		return mcpResult(request, map[string]any{"tools": mcpTools})
	}
	if method == "tools/call" {
		started := time.Now()
		params := mapValueDefault(request["params"])
		name := firstString(params["name"])
		arguments := mapValueDefault(params["arguments"])
		enabled, err := catalog.MCPEnabled()
		var value any
		status := "ok"
		if err != nil {
			status = "error"
			value = "MCP setting unavailable"
		} else if !enabled {
			status = "disabled"
			value = "MCP is turned off in Pharos"
		} else {
			value, err = callMCP(catalog, name, arguments)
			if err != nil {
				status = "error"
				value = err.Error()
			}
		}
		isError := err != nil
		if status == "disabled" {
			isError = true
		}
		text := jsonText(value)
		truncated := false
		if len(text) > 900_000 {
			text = jsonText(map[string]any{"error": "response exceeds 900 KB; use pagination or a narrower query"})
			isError = true
			status = "oversized"
			truncated = true
		}
		if mcpResultTruncated(value) {
			truncated = true
		}
		errorText := ""
		if isError {
			errorText = text
		}
		if logErr := catalog.RecordMCPCall(name, arguments, status, errorText, time.Since(started), len(text), mcpResultCount(value), truncated); logErr != nil {
			fmt.Fprintf(os.Stderr, "MCP history: %v\n", logErr)
		}
		return mcpResult(request, map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError})
	}
	return map[string]any{"jsonrpc": "2.0", "id": request["id"], "error": map[string]any{"code": -32601, "message": "method not found"}}
}

func callMCP(catalog *Catalog, name string, args map[string]any) (any, error) {
	limit := int(integer(valueOr(args["limit"], 50)))
	if limit > 100 {
		limit = 100
	}
	switch name {
	case "search_conversations":
		return catalog.searchConversations(args)
	case "get_conversation_overview":
		return catalog.conversationOverview(args)
	case "search_conversation_passages":
		return catalog.conversationPassages(args)
	case "get_conversation_messages":
		return catalog.conversationMessages(args)
	case "search_work":
		options := SearchOptions{Query: firstString(args["query"]), Repository: firstString(args["repository"]), Source: firstString(args["source"]), File: firstString(args["file"]), Owner: firstString(args["owner"]), Provider: firstString(args["provider"]), Model: firstString(args["model"]), From: firstString(args["from"]), To: firstString(args["to"]), Flavor: firstString(args["flavor"]), Version: firstString(args["version"]), Outcome: firstString(args["outcome"]), Error: firstString(args["error"]), Metric: firstString(args["metric"]), Limit: limit}
		if args["pr"] != nil {
			value := int(integer(args["pr"]))
			options.PR = &value
		}
		if value, ok := number(args["minimum"]); ok {
			options.Minimum = &value
		}
		if value, ok := number(args["maximum"]); ok {
			options.Maximum = &value
		}
		return catalog.Search(options)
	case "get_work_detail":
		value, err := catalog.workDetail(firstString(args["workspace_id"]), false)
		if err != nil {
			return nil, err
		}
		if value == nil {
			return nil, fmt.Errorf("workspace not found")
		}
		return value, nil
	case "get_conversation_excerpt":
		return catalog.conversationMessages(args)
	case "get_change_set":
		id := firstString(args["change_set_id"])
		changes, err := queryMaps(catalog.DB, "SELECT * FROM change_sets WHERE id=?", id)
		if err != nil {
			return nil, err
		}
		if len(changes) == 0 {
			return nil, fmt.Errorf("change set not found")
		}
		files, err := queryMaps(catalog.DB, "SELECT * FROM change_files WHERE change_set_id=? ORDER BY path", id)
		return map[string]any{"change": changes[0], "files": files}, err
	case "trace":
		if firstString(args["task"]) != "" {
			rows, err := queryMaps(catalog.DB, "SELECT w.* FROM workspaces w LEFT JOIN work_items i ON i.workspace_id=w.id WHERE w.source_id=? OR i.source_id=? LIMIT ?", args["task"], args["task"], limit)
			return map[string]any{"items": rows, "freshness": catalog.Freshness()}, err
		}
		options := SearchOptions{File: firstString(args["file"]), Limit: limit}
		if args["pr"] != nil {
			value := int(integer(args["pr"]))
			options.PR = &value
		}
		return catalog.Search(options)
	case "query_metrics":
		clauses := "m.name=?"
		values := []any{firstString(args["name"])}
		if args["minimum"] != nil {
			clauses += " AND (m.value>=? OR m.value IS NULL)"
			values = append(values, args["minimum"])
		}
		if args["maximum"] != nil {
			clauses += " AND (m.value<=? OR m.value IS NULL)"
			values = append(values, args["maximum"])
		}
		values = append(values, limit)
		rows, err := queryMaps(catalog.DB, `SELECT w.id workspace_id,w.title,w.source_kind,m.value,m.unit,m.status,m.coverage,m.definition FROM workspaces w LEFT JOIN metrics m ON m.workspace_id=w.id AND `+clauses+` ORDER BY m.value IS NULL,m.value DESC LIMIT ?`, values...)
		return map[string]any{"items": catalog.suppressMirrors(rows), "note": "Missing values are retained and sort after observed/derived values; native-alias mirrors count once."}, err
	case "get_receipt":
		id := firstString(args["id"])
		rows, err := queryMaps(catalog.DB, "SELECT * FROM receipts WHERE id=? OR operation_id=?", id, id)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("receipt not found")
		}
		return rows[0], nil
	}
	if value, ok, err := callTL1MCP(catalog, name, args); ok {
		return value, err
	}
	return nil, fmt.Errorf("unknown tool: %s", name)
}
