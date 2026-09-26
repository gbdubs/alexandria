package archive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// referenceSearchConversations is search_conversations as it was before it
// ranked inside FTS5, read vectors from conversation_vectors, and completed
// only the page: it read every conversation's document and message count.
func (c *Catalog) referenceSearchConversations(args map[string]any) (map[string]any, error) {
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

func TestMCPConversationSearchMatchesItsFullScan(t *testing.T) {
	catalog, _ := testCatalog(t)
	message := func(id, role, text string) map[string]any {
		return map[string]any{"id": id, "role": role, "text": text}
	}
	conversation := func(id string, messages ...map[string]any) map[string]any {
		return map[string]any{"id": id, "provider": "codex", "messages": messages}
	}
	document := map[string]any{"workspaces": []any{
		map[string]any{"id": "work-1", "title": "Parser maintenance", "activity_at": "2026-09-20T12:00:00Z",
			"repository": map[string]any{"canonical_remote": "github.com/acme/parser", "display_name": "parser"},
			"conversations": []any{
				conversation("parser-session", message("p1", "user", "Fix the tokenizer parser bug"), message("p2", "assistant", "Fixed the tokenizer; tests pass")),
				conversation("lexer-session", message("l1", "user", "Speed up the lexer"), message("l2", "assistant", "The parser now streams tokens")),
			}},
		map[string]any{"id": "work-2", "title": "Dashboard colors", "activity_at": "2026-09-21T12:00:00Z",
			"repository": map[string]any{"canonical_remote": "github.com/acme/web", "display_name": "web"},
			"conversations": []any{
				conversation("css-session", message("c1", "user", "Improve the dashboard colors"), message("c2", "assistant", "Updated the palette")),
				conversation("chart-session", message("h1", "user", "The chart parser fails on dates"), message("h2", "assistant", "Parsed dates in UTC")),
				conversation("empty-session"),
			}},
	}}
	payload, _ := json.Marshal(document)
	export := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(export, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := MakeAdapter(SourceConfig{Name: "fixture", Kind: "canonical", Path: export, Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if result := catalog.Ingest(adapter, nil); result.Error != nil {
		t.Fatal(result.Error)
	}
	decode := func(value any) any {
		var decoded any
		if err := json.Unmarshal([]byte(jsonText(value)), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	for _, args := range []map[string]any{
		{"query": "parser"}, {"query": "tokenizer bug"}, {"query": "dashboard"}, {"query": "palette colors"},
		{"query": "Parser maintenance"}, {"query": ""}, {"query": "", "repository": "web"}, {"query": "parser", "repository": "parser"},
		{"query": "parser", "limit": 1, "offset": 1}, {"query": "parser", "max_output_tokens": 200},
	} {
		got, err := catalog.searchConversations(args)
		if err != nil {
			t.Fatal(err)
		}
		want, err := catalog.referenceSearchConversations(args)
		if err != nil {
			t.Fatal(err)
		}
		if !same(decode(got), decode(want)) {
			t.Fatalf("%v:\ngot  %s\nwant %s", args, jsonText(got), jsonText(want))
		}
	}
	// Passages come from the chosen conversation only, best first.
	for _, query := range []string{"parser", "tokenizer", "dates"} {
		for _, id := range []string{"parser-session", "chart-session"} {
			conversations, _ := queryMaps(catalog.DB, "SELECT id FROM conversations WHERE native_id=?", id)
			got, err := catalog.conversationPassages(map[string]any{"conversation_id": conversations[0]["id"], "query": query})
			if err != nil {
				t.Fatal(err)
			}
			want, err := queryMaps(catalog.DB, `SELECT m.id message_id FROM messages_fts JOIN messages m ON m.id=messages_fts.message_id
				WHERE m.conversation_id=? AND messages_fts MATCH ? ORDER BY bm25(messages_fts) LIMIT 5`, conversations[0]["id"], ftsQuery(query))
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for _, item := range got["items"].([]map[string]any) {
				ids = append(ids, firstString(item["message_id"]))
			}
			expected := []string{}
			for _, row := range want {
				expected = append(expected, firstString(row["message_id"]))
			}
			if strings.Join(ids, ",") != strings.Join(expected, ",") {
				t.Fatalf("passages %q in %s = %v, want %v", query, id, ids, expected)
			}
		}
	}
}

func TestQueryMetricsMatchesItsFullScan(t *testing.T) {
	catalog, _ := libraryFixture(t)
	for _, statement := range []string{
		`INSERT INTO workspaces(id,source_kind,source_account,source_id,title,indexed_at) VALUES
			('d','codex','local','d','Delta','x'),('e','codex','local','e','Echo','x'),('f','codex','local','f','Foxtrot','x')`,
		`INSERT INTO conversations(id,workspace_id,provider,account,native_id) VALUES('cf','f','codex','local','cf')`,
		`INSERT INTO metrics(workspace_id,conversation_id,name,value,unit,status,extractor_version) VALUES
			('d',NULL,'score',5,'points','observed','v1'),('e',NULL,'score',NULL,'points','unknown','v1'),
			('f',NULL,'score',40,'points','observed','v1'),('f','cf','score',2,'points','observed','v1')`,
	} {
		if _, err := catalog.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	key := func(row map[string]any) string {
		return fmt.Sprint(row["workspace_id"], "|", row["value"], "|", row["status"])
	}
	for _, args := range []map[string]any{
		{"name": "score"}, {"name": "score", "limit": 2}, {"name": "score", "minimum": 3}, {"name": "score", "maximum": 10},
		{"name": "score", "minimum": 3, "maximum": 10, "limit": 4}, {"name": "absent"}, {"name": "score", "limit": 1},
	} {
		result, err := callMCP(catalog, "query_metrics", args)
		if err != nil {
			t.Fatal(err)
		}
		got := result.(map[string]any)["items"].([]map[string]any)
		clauses, values := "m.name=?", []any{args["name"]}
		if args["minimum"] != nil {
			clauses += " AND (m.value>=? OR m.value IS NULL)"
			values = append(values, args["minimum"])
		}
		if args["maximum"] != nil {
			clauses += " AND (m.value<=? OR m.value IS NULL)"
			values = append(values, args["maximum"])
		}
		limit := int(integer(valueOr(args["limit"], 50)))
		want, err := queryMaps(catalog.DB, `SELECT w.id workspace_id,w.title,w.source_kind,m.value,m.unit,m.status,m.coverage,m.definition
			FROM workspaces w LEFT JOIN metrics m ON m.workspace_id=w.id AND `+clauses+` ORDER BY m.value IS NULL,m.value DESC LIMIT ?`, append(values, limit)...)
		if err != nil {
			t.Fatal(err)
		}
		want = catalog.suppressMirrors(want)
		if len(got) != len(want) {
			t.Fatalf("%v: %d rows, want %d", args, len(got), len(want))
		}
		// Rows without a value have no order; ranked ones must match in order.
		unranked := map[string]int{}
		for index := range want {
			if want[index]["value"] != nil {
				if key(got[index]) != key(want[index]) {
					t.Fatalf("%v row %d: %s, want %s", args, index, key(got[index]), key(want[index]))
				}
				continue
			}
			unranked[key(want[index])]++
			unranked[key(got[index])]--
		}
		for row, count := range unranked {
			if count != 0 && !(args["limit"] != nil && limit < 7) {
				t.Fatalf("%v: unranked rows differ at %s", args, row)
			}
		}
	}
}
