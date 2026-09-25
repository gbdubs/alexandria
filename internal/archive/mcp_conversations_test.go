package archive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPConversationDiscoveryAndBoundedReading(t *testing.T) {
	catalog, _ := testCatalog(t)
	export := filepath.Join(t.TempDir(), "conversations.json")
	longText := "Resolved the parser issue. " + strings.Repeat("Detailed implementation evidence. ", 300)
	document := map[string]any{"workspaces": []any{map[string]any{
		"id": "work-1", "title": "Parser maintenance", "activity_at": "2026-09-20T12:00:00Z",
		"repository": map[string]any{"canonical_remote": "github.com/acme/parser", "display_name": "parser"},
		"conversations": []any{
			map[string]any{"id": "parser-session", "provider": "codex", "messages": []any{
				map[string]any{"id": "parser-question", "role": "user", "text": "Fix the tokenizer parser bug"},
				map[string]any{"id": "parser-answer", "role": "assistant", "text": longText},
			}},
			map[string]any{"id": "css-session", "provider": "codex", "messages": []any{
				map[string]any{"id": "css-question", "role": "user", "text": "Improve the dashboard colors"},
				map[string]any{"id": "css-answer", "role": "assistant", "text": "Updated the palette"},
			}},
		},
	}}}
	payload, _ := json.Marshal(document)
	if err := os.WriteFile(export, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := MakeAdapter(SourceConfig{Name: "fixture", Kind: "canonical", Path: export, Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if result := catalog.Ingest(adapter, nil); result.Error != nil {
		t.Fatalf("ingest: %v", result.Error)
	}
	search, err := callMCP(catalog, "search_conversations", map[string]any{"query": "tokenizer parser bug", "limit": 5, "max_output_tokens": 600})
	if err != nil {
		t.Fatal(err)
	}
	searchResult := search.(map[string]any)
	items := searchResult["items"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("expected one conversation, got %#v", items)
	}
	conversationID := firstString(items[0]["conversation_id"])
	if strings.Contains(jsonText(searchResult), "Detailed implementation evidence") || len(jsonText(searchResult)) > 1800 {
		t.Fatalf("search returned transcript or exceeded budget: %s", jsonText(searchResult))
	}
	if firstString(items[0]["message_id"]) == "" {
		t.Fatalf("missing search evidence: %#v", items[0])
	}
	minimumBudgetSearch, err := callMCP(catalog, "search_conversations", map[string]any{
		"query": "tokenizer parser bug", "limit": 1, "max_output_tokens": 200,
	})
	if err != nil || len(minimumBudgetSearch.(map[string]any)["items"].([]map[string]any)) != 1 {
		t.Fatalf("minimum budget lost the search hit: %v %#v", err, minimumBudgetSearch)
	}
	if len(jsonText(minimumBudgetSearch)) > 600 {
		t.Fatalf("minimum budget search exceeded response budget")
	}
	overview, err := callMCP(catalog, "get_conversation_overview", map[string]any{"conversation_id": conversationID})
	if err != nil || firstString(overview.(map[string]any)["title"]) != "Parser maintenance" {
		t.Fatalf("overview: %v %#v", err, overview)
	}
	passages, err := callMCP(catalog, "search_conversation_passages", map[string]any{"conversation_id": conversationID, "query": "tokenizer"})
	if err != nil || len(passages.(map[string]any)["items"].([]map[string]any)) != 1 {
		t.Fatalf("passages: %v %#v", err, passages)
	}
	window, err := callMCP(catalog, "get_conversation_messages", map[string]any{"conversation_id": conversationID, "limit": 2, "max_output_tokens": 500})
	if err != nil {
		t.Fatal(err)
	}
	messageItems := window.(map[string]any)["items"].([]map[string]any)
	if len(messageItems) != 2 || messageItems[1]["next_text_offset"] == nil || len(jsonText(window)) > 1500 {
		t.Fatalf("message window was not bounded: %#v", window)
	}
	chunk, err := callMCP(catalog, "get_conversation_messages", map[string]any{
		"conversation_id": conversationID, "message_id": messageItems[1]["message_id"],
		"text_offset": messageItems[1]["next_text_offset"], "max_output_tokens": 500,
	})
	if err != nil || !strings.Contains(firstString(chunk.(map[string]any)["text"]), "implementation evidence") {
		t.Fatalf("message continuation: %v %#v", err, chunk)
	}
	if len(jsonText(chunk)) > 1500 {
		t.Fatalf("message continuation exceeded budget")
	}
	if _, err := catalog.DB.Exec("DELETE FROM conversation_documents WHERE conversation_id=?", conversationID); err != nil {
		t.Fatal(err)
	}
	if err := catalog.backfillConversationDocuments(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM conversation_documents WHERE conversation_id=?", conversationID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("backfill failed: count=%d error=%v", count, err)
	}
}
