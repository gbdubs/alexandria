package archive

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// toolRollupVersion names the tool_usage_daily layout and derivation.
const toolRollupVersion = "rollup-v1"

// toolLedgerState tracks the in-process ledger backfill and rollup rebuilds.
type toolLedgerState struct {
	backfill    sync.Mutex
	mu          sync.Mutex
	running     bool
	done, total int
	lastError   string
	rollup      sync.Mutex
	rolledAt    time.Time
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func nullableInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// bumpToolLedgerGeneration marks the rollup stale. It runs in the writer's
// transaction, so another process's reads see the change with the data.
func bumpToolLedgerGeneration(tx execer) error {
	_, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('tool_ledger_generation','1')
		ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT)`)
	return err
}

// replaceToolLedger rebuilds one conversation's model requests and tool calls.
func replaceToolLedger(tx *sql.Tx, workspaceID, conversationID string, conversation ConversationRecord) error {
	if _, err := tx.Exec("DELETE FROM tool_calls WHERE conversation_id=?", conversationID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM model_requests WHERE conversation_id=?", conversationID); err != nil {
		return err
	}
	requests, calls := buildToolLedger(conversation.Messages, strings.TrimSpace(conversation.Model))
	sessionID := func(stream string) any {
		if stream == "" {
			stream = "main"
		}
		return stableID("agent-session", conversationID, stream)
	}
	requestID := func(key string) any {
		if key == "" {
			return nil
		}
		return stableID("model-request", conversationID, key)
	}
	messageID := func(nativeID string) any {
		if nativeID == "" {
			return nil
		}
		return stableID("message", conversationID, nativeID)
	}
	if len(requests) > 0 {
		statement, err := tx.Prepare(`INSERT INTO model_requests(id,conversation_id,agent_session_id,native_id,sequence,requested_at,model,
			input_tokens,uncached_input_tokens,cache_read_input_tokens,cache_creation_input_tokens,output_tokens,reasoning_output_tokens,total_tokens,
			context_growth_tokens,compacted_before,tool_call_count) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		for _, request := range requests {
			counts := request.Counts
			uncached := max(counts["input_tokens"]-counts["cache_read_input_tokens"]-counts["cache_creation_input_tokens"], 0)
			if _, err := statement.Exec(requestID(request.Key), conversationID, sessionID(request.Stream), nilIfEmpty(request.NativeID), request.Sequence,
				nilIfEmpty(request.RequestedAt), nilIfEmpty(request.Model), integer(counts["input_tokens"]), integer(uncached),
				integer(counts["cache_read_input_tokens"]), integer(counts["cache_creation_input_tokens"]), integer(counts["output_tokens"]),
				integer(counts["reasoning_output_tokens"]), integer(counts["total_tokens"]), nullableInt(request.ContextGrowth),
				boolInt(request.CompactedBefore), request.ToolCalls); err != nil {
				statement.Close()
				return err
			}
		}
		statement.Close()
	}
	if len(calls) > 0 {
		callStatement, err := tx.Prepare(`INSERT INTO tool_calls(id,workspace_id,conversation_id,agent_session_id,call_id,call_message_id,result_message_id,
			sequence,provider,model,kind,tool_name,tool_category,mcp_server,command,program,subcommand,command_category,command_count,
			has_pipe,has_redirect,has_heredoc,backgrounded,file_path,started_at,ended_at,duration_ms,duration_source,status,error_type,exit_code,
			interrupted,truncated,input_bytes,result_bytes,result_tokens,result_tokens_source,request_id,next_request_id,parallel_count,
			output_tokens,carried_requests,carried_tokens,lines_added,lines_removed,url,host,hosts,url_count,search_query)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer callStatement.Close()
		commandStatement, err := tx.Prepare(`INSERT INTO tool_commands(tool_call_id,position,operator,command,program,subcommand,category,exit_code,duration_ms)
			VALUES(?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer commandStatement.Close()
		urlStatement, err := tx.Prepare(`INSERT INTO tool_urls(tool_call_id,position,url,host,source) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer urlStatement.Close()
		for _, call := range calls {
			id := stableID("tool-call", conversationID, call.Key)
			if _, err := callStatement.Exec(id, workspaceID, conversationID, sessionID(call.Stream), nilIfEmpty(call.CallID),
				messageID(call.CallNativeID), messageID(call.ResultNativeID), call.Sequence, conversation.Provider, nilIfEmpty(call.Model),
				call.Kind, call.ToolName, call.Category, nilIfEmpty(call.MCPServer), nilIfEmpty(call.Command), nilIfEmpty(call.Program),
				nilIfEmpty(call.Subcommand), nilIfEmpty(call.CommandCategory), call.CommandCount, boolInt(call.HasPipe), boolInt(call.HasRedirect),
				boolInt(call.HasHeredoc), boolInt(call.Backgrounded), nilIfEmpty(call.FilePath), nilIfEmpty(call.StartedAt), nilIfEmpty(call.EndedAt),
				nullableInt(call.DurationMS), nilIfEmpty(call.DurationSource), call.Status, nilIfEmpty(call.ErrorType), nullableInt(call.ExitCode),
				boolInt(call.Interrupted), boolInt(call.Truncated), call.InputBytes, call.ResultBytes, call.ResultTokens,
				nilIfEmpty(call.ResultTokensSource), requestID(call.RequestKey), requestID(call.NextRequestKey), call.ParallelCount,
				call.OutputTokens, call.CarriedRequests, call.CarriedTokens, nullableInt(call.LinesAdded), nullableInt(call.LinesRemoved),
				nilIfEmpty(call.URL), nilIfEmpty(call.Host), nilIfEmpty(call.Hosts), call.URLCount, nilIfEmpty(call.SearchQuery)); err != nil {
				return err
			}
			for _, command := range call.Commands {
				if _, err := commandStatement.Exec(id, command.Position, nilIfEmpty(command.Operator), command.Command, nilIfEmpty(command.Program),
					nilIfEmpty(command.Subcommand), nilIfEmpty(command.Category), nullableInt(command.ExitCode), nullableInt(command.DurationMS)); err != nil {
					return err
				}
			}
			for _, address := range call.URLs {
				if _, err := urlStatement.Exec(id, address.Position, address.URL, address.Host, address.Source); err != nil {
					return err
				}
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO tool_ledger_state(conversation_id,version,tool_calls,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(conversation_id) DO UPDATE SET version=excluded.version,tool_calls=excluded.tool_calls,updated_at=excluded.updated_at`,
		conversationID, toolLedgerVersion, len(calls), now()); err != nil {
		return err
	}
	return bumpToolLedgerGeneration(tx)
}

// storedMessages reads a conversation's retained messages in source order.
func storedMessages(ctx context.Context, q queryer, conversationID any) ([]MessageRecord, error) {
	messages, err := queryMapsContext(ctx, q, "SELECT * FROM messages WHERE conversation_id=? ORDER BY source_order IS NULL,source_order,created_at,id", conversationID)
	if err != nil {
		return nil, err
	}
	records := make([]MessageRecord, 0, len(messages))
	for _, message := range messages {
		records = append(records, MessageRecord{
			NativeID: firstString(message["native_id"]), Role: firstString(message["role"]), Kind: firstString(message["kind"]),
			Text: firstString(message["text"]), RawText: firstString(message["raw_text"]), CreatedAt: firstString(message["created_at"]),
			Model:          firstString(message["model"]),
			ParentNativeID: firstString(message["parent_native_id"]), CallID: firstString(message["call_id"]),
			PreviousNativeID: firstString(message["previous_native_id"]),
			EvidenceLocator:  firstString(message["evidence_locator"]), SourceOrder: int(integer(message["source_order"])), Selected: integer(message["selected"]) != 0,
			Sender: firstString(message["sender"]),
		})
	}
	return records, nil
}

// BackfillToolLedger builds the tool ledger from stored messages for every
// conversation whose ledger is missing or was built by an older derivation.
// It needs no source files, so reclaimed TL1 work is covered too. Each
// conversation commits separately; an interrupted run resumes where it left off.
func (c *Catalog) BackfillToolLedger(ctx context.Context, progress func(done, total int)) (int, error) {
	state := &c.tools
	if !state.backfill.TryLock() {
		return 0, fmt.Errorf("a tool ledger build is already running")
	}
	defer state.backfill.Unlock()
	pending, err := queryMapsContext(ctx, c.DB, `SELECT c.id,c.workspace_id,c.provider,c.model FROM conversations c
		LEFT JOIN tool_ledger_state s ON s.conversation_id=c.id
		WHERE s.conversation_id IS NULL OR s.version<>? ORDER BY c.started_at DESC`, toolLedgerVersion)
	if err != nil {
		return 0, err
	}
	state.mu.Lock()
	state.running, state.done, state.total, state.lastError = true, 0, len(pending), ""
	state.mu.Unlock()
	finish := func(done int, err error) (int, error) {
		state.mu.Lock()
		state.running, state.done = false, done
		if err != nil {
			state.lastError = err.Error()
		}
		state.mu.Unlock()
		return done, err
	}
	for index, row := range pending {
		if err := ctx.Err(); err != nil {
			return finish(index, err)
		}
		messages, err := storedMessages(ctx, c.DB, row["id"])
		if err != nil {
			return finish(index, err)
		}
		tx, err := c.DB.BeginTx(ctx, nil)
		if err != nil {
			return finish(index, err)
		}
		conversation := ConversationRecord{Provider: firstString(row["provider"]), Model: firstString(row["model"]), Messages: messages}
		if err := replaceToolLedger(tx, firstString(row["workspace_id"]), firstString(row["id"]), conversation); err != nil {
			tx.Rollback()
			return finish(index, err)
		}
		if err := tx.Commit(); err != nil {
			return finish(index, err)
		}
		state.mu.Lock()
		state.done = index + 1
		state.mu.Unlock()
		if progress != nil {
			progress(index+1, len(pending))
		}
	}
	return finish(len(pending), nil)
}

type toolLedgerBuild struct {
	running     bool
	done, total int
}

// toolLedgerProgress reports an in-process backfill, for the library status.
func (c *Catalog) toolLedgerProgress() toolLedgerBuild {
	state := &c.tools
	state.mu.Lock()
	defer state.mu.Unlock()
	return toolLedgerBuild{state.running, state.done, state.total}
}

// ToolLedgerStatus reports ledger coverage and any in-process backfill.
func (c *Catalog) ToolLedgerStatus(ctx context.Context) (map[string]any, error) {
	var conversations, current, calls int64
	if err := c.DB.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM conversations),
		(SELECT COUNT(*) FROM tool_ledger_state WHERE version=?),
		(SELECT COALESCE(SUM(tool_calls),0) FROM tool_ledger_state WHERE version=?)`, toolLedgerVersion, toolLedgerVersion).Scan(&conversations, &current, &calls); err != nil {
		return nil, err
	}
	state := &c.tools
	state.mu.Lock()
	defer state.mu.Unlock()
	return map[string]any{
		"version": toolLedgerVersion, "conversations": conversations, "current_conversations": current,
		"pending_conversations": max(conversations-current, 0), "tool_calls": calls,
		"backfill": map[string]any{"running": state.running, "done": state.done, "total": state.total, "error": nilIfEmpty(state.lastError)},
	}, nil
}

func (c *Catalog) metaValue(ctx context.Context, key string) (string, error) {
	var value string
	err := c.DB.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// ensureToolRollup rebuilds tool_usage_daily when the ledger or identity
// links changed since the last build. While a backfill is writing, rebuilds
// are spaced out so each query does not redo the whole rollup.
func (c *Catalog) ensureToolRollup(ctx context.Context) error {
	state := &c.tools
	state.rollup.Lock()
	defer state.rollup.Unlock()
	generation, err := c.metaValue(ctx, "tool_ledger_generation")
	if err != nil {
		return err
	}
	// A new rollup layout rebuilds even when the ledger is unchanged.
	generation += "/" + toolRollupVersion
	built, err := c.metaValue(ctx, "tool_rollup_generation")
	if err != nil {
		return err
	}
	day := c.clock().Format("2006-01-02")
	builtDay, err := c.metaValue(ctx, "tool_rollup_day")
	if err != nil {
		return err
	}
	if generation == built && day == builtDay {
		return nil
	}
	state.mu.Lock()
	running := state.running
	state.mu.Unlock()
	if running && built != "" && time.Since(state.rolledAt) < 30*time.Second {
		return nil
	}
	if err := c.rebuildToolRollup(ctx, generation, day); err != nil {
		return err
	}
	state.rolledAt = time.Now()
	return nil
}

// rebuildToolRollup groups tool calls by local day and every summary
// dimension. Mirrors of the same work count once, as in Usage.
func (c *Catalog) rebuildToolRollup(ctx context.Context, generation, day string) error {
	workspaces, err := queryMapsContext(ctx, c.DB, `SELECT w.id,w.source_kind,w.activity_at FROM workspaces w
		WHERE EXISTS(SELECT 1 FROM tool_calls t WHERE t.workspace_id=w.id)`)
	if err != nil {
		return err
	}
	suppressed := []string{}
	for _, row := range c.suppressMirrors(workspaces) {
		if mirrors, ok := row["mirrored_workspace_ids"].([]string); ok {
			suppressed = append(suppressed, mirrors...)
		}
	}
	book, err := c.loadPriceBook()
	if err != nil {
		return err
	}
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM tool_mirror_workspaces"); err != nil {
		return err
	}
	for _, id := range suppressed {
		if _, err := tx.Exec("INSERT OR IGNORE INTO tool_mirror_workspaces(workspace_id) VALUES(?)", id); err != nil {
			return err
		}
	}
	rows, err := queryMapsContext(ctx, tx, `SELECT date(t.started_at,'localtime') day,r.display_name repository_name,w.source_kind,t.provider,
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
		GROUP BY 1,2,3,4,5,6,7,8,9,10,11,12`)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM tool_usage_daily"); err != nil {
		return err
	}
	statement, err := tx.Prepare(`INSERT INTO tool_usage_daily(id,day,week,month,repository_name,source_kind,provider,model,model_family,session_kind,
		tool_name,tool_category,mcp_server,program,subcommand,command_name,command_category,call_count,error_count,error_rate,no_result_count,rejected_count,
		interrupted_count,timeout_count,nonzero_exit_count,hook_blocked_count,truncated_count,timed_count,total_duration_ms,avg_duration_ms,
		max_duration_ms,input_bytes,result_bytes,result_tokens,measured_count,avg_result_tokens,carried_tokens,output_tokens,lines_added,
		lines_removed,work_count,context_cost_usd,output_cost_usd,tool_cost_usd,price_status)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, row := range rows {
		dayValue := firstString(row["day"])
		var week, month any
		if parsed, err := time.Parse("2006-01-02", dayValue); err == nil {
			week = parsed.AddDate(0, 0, -((int(parsed.Weekday()) + 6) % 7)).Format("2006-01-02")
			month = parsed.Format("2006-01")
		}
		model := strings.TrimSpace(firstString(row["model"]))
		calls := integer(row["call_count"])
		var errorRate, avgDuration, avgResult any
		if calls > 0 {
			errorRate = float64(integer(row["error_count"])) / float64(calls) * 100
			avgResult = float64(integer(row["result_tokens"])) / float64(calls)
		}
		if timed := integer(row["timed_count"]); timed > 0 {
			avgDuration = float64(integer(row["total_duration_ms"])) / float64(timed)
		}
		// A result is first sent as new input (a cache write for Claude) and
		// then re-read from cache by every later request until compaction.
		context := map[string]int64{"cache_read_input_tokens": integer(row["carried_tokens"])}
		if firstString(row["provider"]) == "claude" {
			context["cache_creation_input_tokens"] = integer(row["result_tokens"])
		} else {
			context["uncached_input_tokens"] = integer(row["result_tokens"])
		}
		outputTokens, _ := number(row["output_tokens"])
		contextCost := book.cost(defaultString(nilIfEmpty(model), "Unknown model"), dayValue, context)
		outputCost := book.cost(defaultString(nilIfEmpty(model), "Unknown model"), dayValue, map[string]int64{"output_tokens": int64(outputTokens + 0.5)})
		total := addCost(addCost(nil, contextCost.cost), outputCost.cost)
		status := worsePriceStatus(contextCost.status, outputCost.status)
		id := stableID("tool-usage", dayValue, row["repository_name"], row["source_kind"], row["provider"], model, row["session_kind"], row["tool_name"],
			row["tool_category"], row["mcp_server"], row["program"], row["subcommand"], row["command_category"])
		if _, err := statement.Exec(id, nilIfEmpty(dayValue), week, month, row["repository_name"], row["source_kind"], row["provider"], nilIfEmpty(model),
			nilIfEmpty(modelFamily(model)), row["session_kind"], row["tool_name"], row["tool_category"], row["mcp_server"], row["program"], row["subcommand"],
			nilIfEmpty(strings.TrimSpace(firstString(row["program"])+" "+firstString(row["subcommand"]))), row["command_category"], calls, row["error_count"], errorRate, row["no_result_count"], row["rejected_count"], row["interrupted_count"],
			row["timeout_count"], row["nonzero_exit_count"], row["hook_blocked_count"], row["truncated_count"], row["timed_count"], row["total_duration_ms"],
			avgDuration, row["max_duration_ms"], row["input_bytes"], row["result_bytes"], row["result_tokens"], row["measured_count"], avgResult,
			row["carried_tokens"], outputTokens, row["lines_added"], row["lines_removed"], row["work_count"], costValue(contextCost.cost),
			costValue(outputCost.cost), costValue(total), status); err != nil {
			return err
		}
	}
	for key, value := range map[string]string{"tool_rollup_generation": generation, "tool_rollup_day": day} {
		if _, err := tx.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func costValue(value any) any {
	if cost, ok := value.(*float64); ok {
		if cost == nil {
			return nil
		}
		return *cost
	}
	return value
}
