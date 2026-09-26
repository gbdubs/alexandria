package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestSemanticSearchReadsVectorsAnOlderBuildRewrote(t *testing.T) {
	catalog, _ := testCatalog(t)
	workspace := func(id, title, text string) map[string]any {
		return map[string]any{"id": id, "title": title, "activity_at": "2026-09-20T12:00:00Z",
			"conversations": []any{map[string]any{"id": id + "-conversation", "provider": "codex", "messages": []any{
				map[string]any{"id": id + "-message", "role": "user", "text": text},
			}}}}
	}
	payload, _ := json.Marshal(map[string]any{"workspaces": []any{
		workspace("work-1", "Fix parser", "Please inspect the parser"),
		workspace("work-2", "Tune cache", "Speed up the cache"),
	}})
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
	var first, second string
	if err := catalog.DB.QueryRow("SELECT id FROM workspaces WHERE source_id='work-1'").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := catalog.DB.QueryRow("SELECT id FROM workspaces WHERE source_id='work-2'").Scan(&second); err != nil {
		t.Fatal(err)
	}
	// stored reports the vector semantic_vectors holds for workspace, if any.
	stored := func(workspace string) []float64 {
		t.Helper()
		var blob []byte
		err := catalog.DB.QueryRow("SELECT vector FROM semantic_vectors WHERE workspace_id=?", workspace).Scan(&blob)
		if err == sql.ErrNoRows {
			return nil
		} else if err != nil {
			t.Fatal(err)
		}
		return appendVectorBlob(nil, blob)
	}
	near := func(left, right []float64) bool {
		if len(left) != len(right) || len(left) != semanticDimensions {
			return false
		}
		for index := range left {
			if math.Abs(left[index]-right[index]) > 1e-6 {
				return false
			}
		}
		return true
	}
	for _, id := range []string{first, second} {
		var text string
		if err := catalog.DB.QueryRow("SELECT vector_json FROM semantic_documents WHERE workspace_id=?", id).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if vector, _ := decodeVector(text); !near(stored(id), vector) {
			t.Fatalf("ingest stored a vector for %s that differs from its vector_json", id)
		}
	}
	// A build without semantic_vectors rewrites only vector_json.
	query := "zebra giraffe migration"
	if _, err := catalog.DB.Exec("UPDATE semantic_documents SET vector_json=? WHERE workspace_id=?", jsonText(semanticEmbed(query)), first); err != nil {
		t.Fatal(err)
	}
	if stored(first) != nil {
		t.Fatal("the stale vector was kept")
	}
	found := func() {
		t.Helper()
		matches, err := catalog.semanticMatches(context.Background(), query, .05, 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 || matches[0].id != first || math.Abs(matches[0].score-1) > 1e-6 {
			t.Fatalf("semantic matches = %#v, want %s first", matches, first)
		}
		result, err := catalog.Search(SearchOptions{Query: query, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if items := result["items"].([]map[string]any); len(items) == 0 || items[0]["id"] != first {
			t.Fatalf("search items = %#v, want %s first", items, first)
		}
	}
	found()
	if err := catalog.backfillVectors(workspaceVectors); err != nil {
		t.Fatal(err)
	}
	if !near(stored(first), semanticEmbed(query)) {
		t.Fatal("the backfill did not store the rewritten vector")
	}
	found()
	if _, err := catalog.DB.Exec("DELETE FROM workspaces WHERE id=?", second); err != nil {
		t.Fatal(err)
	}
	var vectors int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM semantic_vectors").Scan(&vectors); err != nil || vectors != 1 {
		t.Fatalf("%d vectors after deleting a workspace (%v)", vectors, err)
	}
}
