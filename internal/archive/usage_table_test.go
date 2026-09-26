package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gbdubs/pharos/internal/querytable"
)

func ingestUsageFixture(t *testing.T, catalog *Catalog) {
	t.Helper()
	root := t.TempDir()
	events := []string{
		`{"type":"session_meta","timestamp":"2026-09-20T23:10:00Z","payload":{"id":"usage-timeline"}}`,
		`{"type":"token_usage_record","timestamp":"2026-09-20T23:15:00Z","payload":{"response_id":"r1","usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":20,"total_tokens":1020},"thread_token_usage":{"total_tokens":1020}}}`,
		`{"type":"token_usage_record","timestamp":"2026-09-21T01:05:00Z","payload":{"response_id":"r2","usage":{"input_tokens":2000,"cached_input_tokens":1500,"output_tokens":30,"total_tokens":2030},"thread_token_usage":{"total_tokens":3050}}}`,
	}
	if err := os.WriteFile(filepath.Join(root, "rollout-timeline.jsonl"), []byte(strings.Join(events, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := MakeAdapter(SourceConfig{Name: "usage", Kind: "codex", Path: root, Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if result := catalog.Ingest(adapter, nil); result.Error != nil {
		t.Fatal(result.Error)
	}
}

func usageByDay(t *testing.T, catalog *Catalog) map[string]map[string]any {
	t.Helper()
	rows, err := catalog.usageRows(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	days := map[string]map[string]any{}
	for _, row := range rows {
		days[firstString(row["day"])] = row
	}
	return days
}

func TestUsageRowsSplitSessionsByRequestTime(t *testing.T) {
	catalog, _ := testCatalog(t)
	ingestUsageFixture(t, catalog)
	days := usageByDay(t, catalog)
	first, second := days["2026-09-20"], days["2026-09-21"]
	if len(days) != 2 || first == nil || second == nil {
		t.Fatalf("usage should split across the two request days: %#v", days)
	}
	if integer(first["total_tokens"]) != 1020 || integer(first["cache_read_input_tokens"]) != 800 || integer(first["uncached_input_tokens"]) != 200 {
		t.Fatalf("first day: %#v", first)
	}
	if integer(second["total_tokens"]) != 2030 || integer(second["uncached_input_tokens"]) != 500 || firstString(second["attribution"]) != "request" {
		t.Fatalf("second day: %#v", second)
	}
	if first["week"] != "2026-09-14" || second["week"] != "2026-09-21" || second["month"] != "2026-09" {
		t.Fatalf("calendar buckets: %v/%v %v", first["week"], second["week"], second["month"])
	}
	if first["provider"] != "codex" || first["session_kind"] != "root" || first["source_kind"] != "codex" {
		t.Fatalf("dimensions: %#v", first)
	}
	// The ledger must reconcile with the session totals the rest of the app reports.
	var sessionTotal, ledgerTotal int64
	if err := catalog.DB.QueryRow("SELECT SUM(total_tokens) FROM agent_sessions").Scan(&sessionTotal); err != nil {
		t.Fatal(err)
	}
	if err := catalog.DB.QueryRow("SELECT SUM(total_tokens) FROM agent_session_usage").Scan(&ledgerTotal); err != nil {
		t.Fatal(err)
	}
	if sessionTotal != 3050 || ledgerTotal != sessionTotal {
		t.Fatalf("ledger %d does not reconcile with sessions %d", ledgerTotal, sessionTotal)
	}
}

func TestUsageSeedAndRefineExistingCatalogs(t *testing.T) {
	catalog, _ := testCatalog(t)
	ingestUsageFixture(t, catalog)
	if _, err := catalog.DB.Exec("DELETE FROM agent_session_usage"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	days := usageByDay(t, catalog)
	if len(days) != 1 || integer(days["2026-09-20"]["total_tokens"]) != 3050 || days["2026-09-20"]["attribution"] != "session-start" {
		t.Fatalf("legacy sessions should be seeded at their start: %#v", days)
	}
	refined, err := catalog.RefineUsageTimeline(nil)
	if err != nil || refined != 1 {
		t.Fatalf("refined=%d err=%v", refined, err)
	}
	days = usageByDay(t, catalog)
	if len(days) != 2 || integer(days["2026-09-21"]["total_tokens"]) != 2030 || days["2026-09-21"]["attribution"] != "request" {
		t.Fatalf("refinement should restore request timing: %#v", days)
	}
	if refined, err = catalog.RefineUsageTimeline(nil); err != nil || refined != 0 {
		t.Fatalf("refinement should be idempotent: refined=%d err=%v", refined, err)
	}
}

func TestUsageRowsSuppressMirroredWorkspaces(t *testing.T) {
	catalog, _ := testCatalog(t)
	for _, item := range []struct{ id, source string }{{"native", "codex"}, {"wrapper", "conductor"}} {
		if _, err := catalog.DB.Exec(`INSERT INTO workspaces(id,source_kind,source_account,source_id,title,indexed_at) VALUES(?,?,?,?,?,?)`, item.id, item.source, "local", item.id, item.id, "2026-09-23"); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.DB.Exec(`INSERT INTO conversations(id,workspace_id,provider,account,native_id) VALUES(?,?,?,?,?)`, "c-"+item.id, item.id, "codex", "local", item.id); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.DB.Exec(`INSERT INTO agent_sessions(id,workspace_id,conversation_id,native_id,kind,provider,model,started_at,input_tokens,output_tokens,total_tokens)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "s-"+item.id, item.id, "c-"+item.id, "main", "root", "codex", "gpt-6-sol", "2026-09-22T10:00:00Z", 90, 10, 100); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := catalog.DB.Exec(`INSERT INTO conversation_identity_links(left_id,right_id,relationship,confidence,evidence_json) VALUES('c-native','c-wrapper','mirror',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	if err := catalog.seedSessionUsage(); err != nil {
		t.Fatal(err)
	}
	rows, err := catalog.usageRows(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["workspace_id"] != "native" || integer(rows[0]["total_tokens"]) != 100 {
		t.Fatalf("mirrored usage counted more than once: %#v", rows)
	}
}

func TestUsageMetricsReadAsTimeline(t *testing.T) {
	catalog, config := testCatalog(t)
	ingestUsageFixture(t, catalog)
	server := NewServer(config, catalog)
	schema, err := server.queryTableSchema("usage")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := catalog.usageRows(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	request := querytable.AggregationRequest{Aggregations: []querytable.Aggregation{
		{ID: "daily", Op: "sum", Field: "total_tokens", GroupBy: []string{"day"}},
		{ID: "models", Op: "sum", Field: "cache_read_input_tokens", GroupBy: []string{"model_family"}},
	}}
	result, err := querytable.Aggregate(rows, request, schema)
	if err != nil {
		t.Fatal(err)
	}
	sortUsageTimeBuckets(request, result)
	daily := result.Metrics[0].Buckets
	if len(daily) != 2 || daily[0].Keys[0] != "2026-09-21" || daily[1].Keys[0] != "2026-09-20" {
		t.Fatalf("daily buckets should be newest first, not largest first: %#v", daily)
	}
	if result.Metrics[1].Buckets[0].Value != float64(2300) {
		t.Fatalf("cache reads by model: %#v", result.Metrics[1])
	}
}

func TestModelFamilyFoldsVariants(t *testing.T) {
	for input, want := range map[string]string{
		"claude-opus-5-5": "opus-5-5", "opus-5-5-1m": "opus-5-5", "claude-opus-5-5[1m]": "opus-5-5",
		"claude-haiku-4-5-20251001": "haiku-4-5", "gpt-6-sol": "gpt-6-sol", "Unknown model": "unknown model",
	} {
		if got := modelFamily(input); got != want {
			t.Errorf("modelFamily(%q) = %q, want %q", input, got, want)
		}
	}
}
