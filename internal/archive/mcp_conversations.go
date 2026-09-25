package archive

import (
	"fmt"
	"sort"
	"strings"
)

func mcpBudget(args map[string]any, fallback int) int {
	if args["max_output_tokens"] == nil {
		return fallback
	}
	return clamp(int(integer(args["max_output_tokens"])), 200, 4000)
}

// A conservative size estimate leaves room for JSON and model-specific
// tokenization. Every tool also bounds the number and length of text fields.
func fitConversationItems(result map[string]any, budget int) {
	items, _ := result["items"].([]map[string]any)
	if len(jsonText(result)) > budget*3 {
		// Null fields carry nothing; shed them before shedding whole cards.
		for _, item := range items {
			for key, value := range item {
				if value == nil {
					delete(item, key)
				}
			}
		}
	}
	for len(items) > 0 && len(jsonText(result)) > budget*3 {
		items = items[:len(items)-1]
		result["items"] = items
		result["truncated"] = true
	}
}

func (c *Catalog) searchConversations(args map[string]any) (map[string]any, error) {
	query := strings.TrimSpace(firstString(args["query"]))
	limit := clamp(int(integer(valueOr(args["limit"], 8))), 1, 20)
	offset := max(0, int(integer(args["offset"])))
	budget := mcpBudget(args, 1200)
	lexical := map[string]float64{}
	matches := map[string]map[string]any{}
	if parsed := ftsQuery(query); parsed != "" {
		rows, err := queryMaps(c.DB, `SELECT m.conversation_id,m.id message_id,
			snippet(messages_fts,1,'','',' … ',18) snippet,bm25(messages_fts) rank
			FROM messages_fts JOIN messages m ON m.id=messages_fts.message_id
			WHERE messages_fts MATCH ? ORDER BY rank LIMIT 500`, parsed)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			id := firstString(row["conversation_id"])
			if _, exists := lexical[id]; !exists {
				lexical[id] = 1 / float64(len(lexical)+1)
				matches[id] = row
			}
		}
	}
	clauses := []string{"1=1"}
	values := []any{}
	if repository := firstString(args["repository"]); repository != "" {
		clauses = append(clauses, "(r.display_name LIKE ? OR r.canonical_remote LIKE ?)")
		values = append(values, "%"+repository+"%", "%"+repository+"%")
	}
	if source := firstString(args["source"]); source != "" {
		clauses = append(clauses, "w.source_kind=?")
		values = append(values, source)
	}
	if provider := firstString(args["provider"]); provider != "" {
		clauses = append(clauses, "c.provider=?")
		values = append(values, provider)
	}
	if file := firstString(args["file"]); file != "" {
		clauses = append(clauses, `EXISTS(SELECT 1 FROM change_sets cs JOIN change_files cf ON cf.change_set_id=cs.id WHERE cs.workspace_id=w.id AND cf.path LIKE ?)`)
		values = append(values, "%"+file+"%")
	}
	if args["pr"] != nil {
		clauses = append(clauses, `EXISTS(SELECT 1 FROM work_pr_links l JOIN pull_requests p ON p.id=l.pr_id WHERE l.workspace_id=w.id AND p.number=?)`)
		values = append(values, integer(args["pr"]))
	}
	if from := firstString(args["from"]); from != "" {
		clauses = append(clauses, "COALESCE(c.started_at,w.activity_at)>=?")
		values = append(values, timeBound(from, false))
	}
	if to := firstString(args["to"]); to != "" {
		clauses = append(clauses, "COALESCE(c.started_at,w.activity_at)<=?")
		values = append(values, timeBound(to, true))
	}
	rows, err := queryMaps(c.DB, `SELECT c.id conversation_id,c.workspace_id,c.provider,c.model,c.coverage,c.started_at,c.ended_at,
		substr(w.title,1,101) title,w.activity_at,substr(r.display_name,1,101) repository,
		d.initiation,d.initiation_message_id,d.outcome conversation_outcome,d.outcome_message_id,d.vector_json,d.indexed_at,
		(SELECT COUNT(*) FROM messages m WHERE m.conversation_id=c.id) message_count
		FROM conversations c JOIN workspaces w ON w.id=c.workspace_id
		LEFT JOIN repositories r ON r.id=w.repository_id
		LEFT JOIN conversation_documents d ON d.conversation_id=c.id WHERE `+strings.Join(clauses, " AND "), values...)
	if err != nil {
		return nil, err
	}
	queryVector := semanticEmbed(query)
	results := []map[string]any{}
	for _, row := range rows {
		id := firstString(row["conversation_id"])
		score := lexical[id] * .55
		reasons := []string{}
		if lexical[id] > 0 {
			reasons = append(reasons, "message text")
		}
		if query != "" {
			if vector, decodeErr := decodeVector(row["vector_json"]); decodeErr == nil {
				semantic := semanticCosine(queryVector, vector)
				if semantic > .1 {
					score += semantic * .35
					reasons = append(reasons, "conversation context")
				}
			}
			if strings.Contains(strings.ToLower(firstString(row["title"])), strings.ToLower(query)) {
				score += .3
				reasons = append(reasons, "title")
			}
			if score <= 0 {
				continue
			}
		}
		if file := firstString(args["file"]); file != "" {
			reasons = append(reasons, "workspace changed file")
		}
		if args["pr"] != nil {
			reasons = append(reasons, "workspace pull request")
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "structured filters or recent activity")
		}
		snippet := firstString(row["initiation"])
		messageID := firstString(row["initiation_message_id"])
		if match := matches[id]; match != nil {
			snippet = firstString(match["snippet"])
			messageID = firstString(match["message_id"])
		}
		results = append(results, map[string]any{
			"conversation_id": id, "workspace_id": row["workspace_id"], "title": clipText(firstString(row["title"]), 100),
			"repository": row["repository"], "provider": row["provider"], "model": row["model"],
			"started_at": row["started_at"], "ended_at": row["ended_at"], "activity_at": row["activity_at"], "message_count": row["message_count"],
			"coverage": row["coverage"], "indexed_at": row["indexed_at"],
			"relevance_reason": strings.Join(reasons, ", "), "snippet": clipText(snippet, 180),
			"message_id": messageID, "score": score,
		})
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i]["score"].(float64) != results[j]["score"].(float64) {
			return results[i]["score"].(float64) > results[j]["score"].(float64)
		}
		left := firstString(results[i]["started_at"], results[i]["activity_at"])
		right := firstString(results[j]["started_at"], results[j]["activity_at"])
		if left != right {
			return left > right
		}
		return firstString(results[i]["conversation_id"]) < firstString(results[j]["conversation_id"])
	})
	results = c.collapseConversationMirrors(results)
	total := len(results)
	if offset > total {
		offset = total
	}
	end := min(total, offset+limit)
	freshness := c.Freshness()
	result := map[string]any{"items": results[offset:end], "next_offset": nil, "total": total,
		"freshness": map[string]any{"status": freshness["status"], "stale_sources": freshness["stale_sources"]}}
	if end < total {
		result["next_offset"] = end
	}
	fitConversationItems(result, budget)
	if len(result["items"].([]map[string]any)) < end-offset {
		result["next_offset"] = offset + len(result["items"].([]map[string]any))
	}
	return result, nil
}

func (c *Catalog) collapseConversationMirrors(rows []map[string]any) []map[string]any {
	byID := map[string]map[string]any{}
	parent := map[string]string{}
	for _, row := range rows {
		id := firstString(row["conversation_id"])
		byID[id], parent[id] = row, id
	}
	var find func(string) string
	find = func(id string) string {
		if parent[id] != id {
			parent[id] = find(parent[id])
		}
		return parent[id]
	}
	links, _ := queryMaps(c.DB, "SELECT left_id,right_id FROM conversation_identity_links")
	for _, link := range links {
		left, right := firstString(link["left_id"]), firstString(link["right_id"])
		if parent[left] != "" && parent[right] != "" {
			parent[find(right)] = find(left)
		}
	}
	groups := map[string][]map[string]any{}
	for id, row := range byID {
		groups[find(id)] = append(groups[find(id)], row)
	}
	output := []map[string]any{}
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool { return group[i]["score"].(float64) > group[j]["score"].(float64) })
		chosen := group[0]
		if len(group) > 1 {
			aliases := []string{}
			for _, mirror := range group[1:] {
				aliases = append(aliases, firstString(mirror["conversation_id"]))
			}
			sort.Strings(aliases)
			chosen["linked_conversation_ids"] = aliases
		}
		output = append(output, chosen)
	}
	sort.SliceStable(output, func(i, j int) bool {
		if output[i]["score"].(float64) != output[j]["score"].(float64) {
			return output[i]["score"].(float64) > output[j]["score"].(float64)
		}
		left := firstString(output[i]["started_at"], output[i]["activity_at"])
		right := firstString(output[j]["started_at"], output[j]["activity_at"])
		if left != right {
			return left > right
		}
		return firstString(output[i]["conversation_id"]) < firstString(output[j]["conversation_id"])
	})
	return output
}

func (c *Catalog) conversationOverview(args map[string]any) (map[string]any, error) {
	id := firstString(args["conversation_id"])
	rows, err := queryMaps(c.DB, `SELECT c.id conversation_id,c.workspace_id,c.provider,c.model,c.coverage,c.started_at,c.ended_at,
		substr(w.title,1,121) title,substr(w.outcome,1,201) outcome,w.activity_at,substr(r.display_name,1,101) repository,
		d.initiation,d.initiation_message_id,d.outcome conversation_outcome,d.outcome_message_id,d.indexed_at,
		(SELECT COUNT(*) FROM messages m WHERE m.conversation_id=c.id) message_count
		FROM conversations c JOIN workspaces w ON w.id=c.workspace_id
		LEFT JOIN repositories r ON r.id=w.repository_id
		LEFT JOIN conversation_documents d ON d.conversation_id=c.id WHERE c.id=?`, id)
	if err != nil || len(rows) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("conversation not found")
	}
	row := rows[0]
	budget := mcpBudget(args, 700)
	result := map[string]any{"conversation_id": id, "workspace_id": row["workspace_id"], "title": clipText(firstString(row["title"]), 120),
		"repository": row["repository"], "provider": row["provider"], "model": row["model"], "coverage": row["coverage"],
		"started_at": row["started_at"], "ended_at": row["ended_at"], "indexed_at": row["indexed_at"], "message_count": row["message_count"],
		"request":           map[string]any{"text": clipText(firstString(row["initiation"]), min(500, budget)), "message_id": row["initiation_message_id"]},
		"last_response":     map[string]any{"text": clipText(firstString(row["conversation_outcome"]), min(500, budget)), "message_id": row["outcome_message_id"]},
		"workspace_outcome": clipText(firstString(row["outcome"]), 200),
		"note":              "Extractive previews; inspect cited messages before treating an outcome as verified."}
	for len(jsonText(result)) > budget*3 {
		request := result["request"].(map[string]any)
		response := result["last_response"].(map[string]any)
		request["text"] = clipText(firstString(request["text"]), max(40, len([]rune(firstString(request["text"])))/2))
		response["text"] = clipText(firstString(response["text"]), max(40, len([]rune(firstString(response["text"])))/2))
		result["workspace_outcome"] = clipText(firstString(result["workspace_outcome"]), 80)
		if len([]rune(firstString(request["text"]))) <= 41 && len([]rune(firstString(response["text"]))) <= 41 {
			break
		}
	}
	return result, nil
}

func (c *Catalog) conversationPassages(args map[string]any) (map[string]any, error) {
	id := firstString(args["conversation_id"])
	query := strings.TrimSpace(firstString(args["query"]))
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit := clamp(int(integer(valueOr(args["limit"], 5))), 1, 10)
	rows, err := queryMaps(c.DB, `SELECT m.id message_id,m.role,m.kind,m.source_order,
		snippet(messages_fts,1,'','',' … ',28) snippet FROM messages_fts
		JOIN messages m ON m.id=messages_fts.message_id
		WHERE m.conversation_id=? AND messages_fts MATCH ? ORDER BY bm25(messages_fts) LIMIT ?`, id, ftsQuery(query), limit)
	if err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for _, row := range rows {
		items = append(items, map[string]any{"message_id": row["message_id"], "role": row["role"], "kind": row["kind"],
			"source_order": row["source_order"], "snippet": clipText(firstString(row["snippet"]), 260)})
	}
	result := map[string]any{"conversation_id": id, "items": items}
	fitConversationItems(result, mcpBudget(args, 900))
	return result, nil
}

func (c *Catalog) conversationMessages(args map[string]any) (map[string]any, error) {
	id := firstString(args["conversation_id"])
	limit := clamp(int(integer(valueOr(args["limit"], 4))), 1, 12)
	offset := max(0, int(integer(args["offset"])))
	messageID := firstString(args["message_id"])
	textOffset := max(0, int(integer(args["text_offset"])))
	if messageID != "" {
		length := max(100, mcpBudget(args, 1200)*3-300)
		rows, err := queryMaps(c.DB, `SELECT id,role,kind,source_order,created_at,
			substr(text,?,?) text,length(text) text_length,evidence_locator FROM messages
			WHERE id=? AND conversation_id=?`, textOffset+1, length, messageID, id)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("message not found in conversation")
		}
		row := rows[0]
		runes := []rune(firstString(row["text"]))
		total := int(integer(row["text_length"]))
		textOffset = min(textOffset, total)
		end := textOffset + len(runes)
		item := map[string]any{"message_id": messageID, "role": row["role"], "kind": row["kind"],
			"source_order": row["source_order"], "created_at": row["created_at"], "evidence_locator": row["evidence_locator"],
			"text": string(runes), "text_offset": textOffset, "next_text_offset": nil}
		if end < total {
			item["next_text_offset"] = end
		}
		for len(jsonText(item)) > mcpBudget(args, 1200)*3 && end > textOffset+20 {
			end -= max(1, (end-textOffset)/8)
			item["text"] = string(runes[:end-textOffset])
			item["next_text_offset"] = end
		}
		return item, nil
	}
	if anchor := firstString(args["around_message_id"]); anchor != "" {
		rows, err := queryMaps(c.DB, `SELECT position FROM (
			SELECT id,row_number() OVER (ORDER BY source_order IS NULL,source_order,created_at,id)-1 position
			FROM messages WHERE conversation_id=?) WHERE id=?`, id, anchor)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("message not found in conversation")
		}
		offset = max(0, int(integer(rows[0]["position"]))-limit/2)
	}
	budget := mcpBudget(args, 1200)
	maxText := max(100, (budget*3-300)/limit)
	rows, err := queryMaps(c.DB, `SELECT id,role,kind,source_order,created_at,substr(text,1,?) text,
		length(text) text_length,evidence_locator FROM messages
		WHERE conversation_id=? ORDER BY source_order IS NULL,source_order,created_at,id LIMIT ? OFFSET ?`, maxText, id, limit+1, offset)
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := []map[string]any{}
	for _, row := range rows {
		original := firstString(row["text"])
		item := map[string]any{"message_id": row["id"], "role": row["role"], "kind": row["kind"],
			"source_order": row["source_order"], "created_at": row["created_at"], "evidence_locator": row["evidence_locator"],
			"text": clipText(original, maxText), "next_text_offset": nil}
		if int(integer(row["text_length"])) > maxText {
			item["next_text_offset"] = maxText
		}
		items = append(items, item)
	}
	result := map[string]any{"conversation_id": id, "items": items, "offset": offset, "next_offset": nil}
	if hasMore {
		result["next_offset"] = offset + len(items)
	}
	fitConversationItems(result, budget)
	if len(result["items"].([]map[string]any)) < len(items) {
		result["next_offset"] = offset + len(result["items"].([]map[string]any))
	}
	return result, nil
}
