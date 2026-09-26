package archive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"alexandria/internal/querytable"
)

// libraryFixture creates three workspaces touching every derived-column
// dependency; a and b are native-alias mirrors (b, the tl1 side, represents).
func libraryFixture(t *testing.T) (*Catalog, Config) {
	t.Helper()
	catalog, config := testCatalog(t)
	statements := []string{
		`INSERT INTO repositories(id,display_name,created_at,updated_at) VALUES('repo','alexandria','x','x')`,
		`INSERT INTO workspaces(id,source_kind,source_account,source_id,repository_id,title,branch,activity_at,indexed_at) VALUES
			('a','codex','local','a','repo','Alpha','main','2026-09-23T03:00:00Z','x'),
			('b','tl1','local','b','repo','Bravo',NULL,'2026-09-23T01:00:00Z','x'),
			('c','canonical','local','c',NULL,'Charlie','feature','2026-09-23T02:00:00Z','x')`,
		`INSERT INTO conversations(id,workspace_id,provider,model,account,native_id) VALUES
			('ca','a','codex','gpt','local','ca'),('cb','b','tl1',NULL,'local','cb'),('cc','c','claude','opus','local','cc')`,
		`INSERT INTO messages(id,conversation_id,native_id,role,kind,text,created_at,content_hash) VALUES
			('m1','cc','1','user','message','Please fix the parser','2026-09-23T02:00:00Z','h'),
			('m2','cc','2','assistant','tool_call','{}','2026-09-23T02:00:01Z','h'),
			('m3','cc','3','tool','tool_result','{"is_error":1}','2026-09-23T02:00:02Z','h'),
			('m4','cc','4','assistant','message','Fixed it','2026-09-23T02:00:03Z','h'),
			('m5','ca','1','user','message','Mirror prompt','2026-09-23T03:00:00Z','h'),
			('k1','cc','k1','system','metadata','{"subtype":"compact_boundary","type":"system"}','2026-09-23T02:00:04Z','h'),
			('k2','cc','k2','system','metadata','{"compaction_trigger":"auto","payload":{},"type":"compacted"}','2026-09-23T02:00:05Z','h'),
			('k3','cc','k3','system','metadata','{"payload":{},"type":"compacted"}','2026-09-23T02:00:06Z','h')`,
		`INSERT INTO metrics(workspace_id,name,value,unit,status,extractor_version,observed_at) VALUES
			('c','total_tokens',500,'tokens','observed','test','x'),('a','total_tokens',900,'tokens','observed','test','x')`,
		`INSERT INTO metric_ledger(workspace_id,sequence,event_kind,error_type) VALUES('c',1,'tool','timeout')`,
		`INSERT INTO change_sets(id,workspace_id,classification) VALUES('cs','c','attempted')`,
		`INSERT INTO change_files(change_set_id,path,status,tracked) VALUES('cs','src/parser.go','modified',1),('cs','README.md','modified',1)`,
		`INSERT INTO pull_requests(id,host,repository_id,number,title) VALUES('pr','github.com','repo',42,'Parser fix')`,
		`INSERT INTO work_pr_links(workspace_id,pr_id,relationship,confidence,evidence_json) VALUES('c','pr','associated',1,'{}')`,
		`INSERT INTO summaries(workspace_id,initiation,outcome,evidence_json,model,extractor_version,created_at) VALUES('b','Summarized purpose',NULL,'{}','m','v','x')`,
		`INSERT INTO conversation_identity_links(left_id,right_id,relationship,confidence,evidence_json) VALUES('ca','cb','native-alias',1,'{}')`,
		`INSERT INTO agent_sessions(id,workspace_id,conversation_id,native_id,parent_id,kind,provider,depth) VALUES
			('sc','c','cc','main',NULL,'root','claude',0),('sc1','c','cc','t1','sc','subagent','claude',1),
			('sc2','c','cc','t2','sc1','subagent','claude',2)`,
	}
	for _, statement := range statements {
		if _, err := catalog.DB.Exec(statement); err != nil {
			t.Fatalf("%v\n%s", err, statement)
		}
	}
	return catalog, config
}

func dirtyCount(t *testing.T, catalog *Catalog) int {
	t.Helper()
	var count int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM workspace_library_dirty").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func libraryRowsByID(t *testing.T, catalog *Catalog) map[string]map[string]any {
	t.Helper()
	rows, err := catalog.searchRows(SearchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[firstString(row["id"])] = row
	}
	return byID
}

func TestLibraryStoredRowsMatchLiveComputation(t *testing.T) {
	catalog, _ := libraryFixture(t)
	if dirtyCount(t, catalog) != 3 {
		t.Fatalf("new workspaces were not marked dirty: %d", dirtyCount(t, catalog))
	}
	live := libraryRowsByID(t, catalog)
	if err := catalog.refreshAllLibrary(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dirtyCount(t, catalog) != 0 {
		t.Fatal("refresh left dirty workspaces")
	}
	stored := libraryRowsByID(t, catalog)
	if !reflect.DeepEqual(live, stored) {
		t.Fatalf("stored rows differ from live rows:\nlive   %#v\nstored %#v", live, stored)
	}
	c := stored["c"]
	for field, want := range map[string]any{
		"conversation_count": int64(1), "turn_count": int64(1), "tool_use_count": int64(1), "tool_error_count": int64(1),
		"changed_file_count": int64(2), "file_edit_count": int64(2), "token_count": float64(500), "pr_count": int64(1),
		"pr_numbers": "42", "providers": "claude", "models": "opus", "error_types": "timeout", "branch_count": int64(1),
		"repository_name": nil, "subagent_count": int64(2), "subagent_depth": int64(2), "compaction_count": int64(2),
	} {
		if !reflect.DeepEqual(c[field], want) {
			t.Errorf("c.%s = %#v, want %#v", field, c[field], want)
		}
	}
	if len(stored) != 2 || stored["a"] != nil || firstString(stored["b"]["purpose"]) != "Summarized purpose" {
		t.Fatalf("mirror or summary handling changed: %#v", stored)
	}
}

func TestLibraryReflectsEveryDependencyBeforeAndAfterRefresh(t *testing.T) {
	catalog, _ := libraryFixture(t)
	if err := catalog.refreshAllLibrary(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name, statement string
		check           func(map[string]map[string]any) bool
	}{
		{"message insert", `INSERT INTO messages(id,conversation_id,native_id,role,kind,text,content_hash) VALUES('m6','cc','6','user','message','Again','h')`,
			func(rows map[string]map[string]any) bool { return integer(rows["c"]["turn_count"]) == 2 }},
		{"agent session insert", `INSERT INTO agent_sessions(id,workspace_id,conversation_id,native_id,parent_id,kind,provider,depth) VALUES('sc3','c','cc','t3','sc','subagent','claude',1)`,
			func(rows map[string]map[string]any) bool { return integer(rows["c"]["subagent_count"]) == 3 }},
		{"message update", `UPDATE messages SET text='{"is_error":0}' WHERE id='m3'`,
			func(rows map[string]map[string]any) bool { return integer(rows["c"]["tool_error_count"]) == 0 }},
		{"change file delete", `DELETE FROM change_files WHERE path='README.md'`,
			func(rows map[string]map[string]any) bool { return integer(rows["c"]["changed_file_count"]) == 1 }},
		{"metric update", `UPDATE metrics SET value=700 WHERE workspace_id='c'`,
			func(rows map[string]map[string]any) bool { return integer(rows["c"]["token_count"]) == 700 }},
		{"pull request retitle", `UPDATE pull_requests SET title='Renamed' WHERE id='pr'`,
			func(rows map[string]map[string]any) bool {
				return strings.Contains(firstString(rows["c"]["pr_details"]), "Renamed")
			}},
		{"summary insert", `INSERT INTO summaries(workspace_id,outcome,evidence_json,model,extractor_version,created_at) VALUES('c','Shipped','{}','m','v','x')`,
			func(rows map[string]map[string]any) bool { return firstString(rows["c"]["outcome"]) == "Shipped" }},
		{"ledger insert", `INSERT INTO metric_ledger(workspace_id,sequence,event_kind,error_type) VALUES('c',2,'tool','crash')`,
			func(rows map[string]map[string]any) bool {
				return strings.Contains(firstString(rows["c"]["error_types"]), "crash")
			}},
		{"conversation insert", `INSERT INTO conversations(id,workspace_id,provider,account,native_id) VALUES('cc2','c','codex','local','cc2')`,
			func(rows map[string]map[string]any) bool { return integer(rows["c"]["conversation_count"]) == 2 }},
		{"workspace insert", `INSERT INTO workspaces(id,source_kind,source_account,source_id,title,indexed_at) VALUES('d','canonical','local','d','Delta','x')`,
			func(rows map[string]map[string]any) bool {
				return rows["d"] != nil && integer(rows["d"]["conversation_count"]) == 0
			}},
		{"workspace delete", `DELETE FROM workspaces WHERE id='d'`,
			func(rows map[string]map[string]any) bool { return rows["d"] == nil }},
		{"workspace column update", `UPDATE workspaces SET title='Charlie 2' WHERE id='c'`,
			func(rows map[string]map[string]any) bool { return rows["c"]["title"] == "Charlie 2" }},
	} {
		if _, err := catalog.DB.Exec(change.statement); err != nil {
			t.Fatalf("%s: %v", change.name, err)
		}
		before := libraryRowsByID(t, catalog)
		if !change.check(before) {
			t.Fatalf("%s: not visible before refresh: %#v", change.name, before)
		}
		if err := catalog.refreshAllLibrary(context.Background()); err != nil {
			t.Fatal(err)
		}
		after := libraryRowsByID(t, catalog)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: refreshed rows differ:\nbefore %#v\nafter  %#v", change.name, before, after)
		}
	}
}

func TestLibraryQueryTableServesArbitrarySortsAndFilters(t *testing.T) {
	catalog, config := libraryFixture(t)
	if err := catalog.refreshAllLibrary(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config, catalog)
	post := func(path, body string) map[string]any {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		var decoded map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	sorted := post("/api/query/library", `{"select":["title"],"orderBy":[{"field":"title","dir":"asc"},{"field":"id","dir":"asc"}],"limit":10,"offset":0}`)
	rows := sorted["rows"].([]any)
	if sorted["total"] != float64(2) || rows[0].(map[string]any)["title"] != "Bravo" || rows[1].(map[string]any)["title"] != "Charlie" {
		t.Fatalf("unexpected sorted page: %#v", sorted)
	}
	if firstString(rows[1].(map[string]any)["first_input"]) != "Please fix the parser" || firstString(rows[1].(map[string]any)["last_response"]) != "Fixed it" {
		t.Fatalf("page rows lack previews: %#v", rows[1])
	}
	filtered := post("/api/query/library", `{"select":["title"],"where":[{"field":"tool_error_count","op":">=","value":"1"}],"orderBy":[{"field":"changed_file_count","dir":"desc"}],"limit":10,"offset":0}`)
	if filtered["total"] != float64(1) || filtered["rows"].([]any)[0].(map[string]any)["id"] != "c" {
		t.Fatalf("unexpected filtered page: %#v", filtered)
	}
	pending := post("/api/query/library", `{"select":["title"],"where":[{"field":"subagent_count","op":"=","value":""}],"limit":10,"offset":0}`)
	if pending["total"] != float64(2) {
		t.Fatalf("pending subagent count filter hid the library: %#v", pending)
	}
	metrics := post("/api/query/library/aggregations", `{"aggregations":[{"id":"n","op":"count","groupBy":["repository_name"]}]}`)
	var want querytable.AggregationResult
	encoded, _ := json.Marshal(metrics)
	if err := json.Unmarshal(encoded, &want); err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, bucket := range want.Metrics[0].Buckets {
		total += bucket.Value.(float64)
	}
	if total != 2 {
		t.Fatalf("mirrored workspaces were counted twice: %#v", metrics)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/query/library/distinct?field=changed_files&q=", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "src/parser.go") {
		t.Fatalf("distinct changed files: %d %s", response.Code, response.Body.String())
	}
	get := func(path string) (int, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		var decoded map[string]any
		_ = json.Unmarshal(response.Body.Bytes(), &decoded)
		return response.Code, decoded
	}
	code, stats := get("/api/query/library/field-stats?fields=title,changed_file_count,activity_at")
	if code != http.StatusOK {
		t.Fatalf("field stats: %d %#v", code, stats)
	}
	title, _ := stats["title"].(map[string]any)
	changed, _ := stats["changed_file_count"].(map[string]any)
	activity, _ := stats["activity_at"].(map[string]any)
	if title["distinct"] != float64(2) || title["min"] != nil || changed["min"] == nil || activity["max"] == nil {
		t.Fatalf("field stats: %#v", stats)
	}
	if code, body := get("/api/query/library/field-stats?fields=title,secret"); code != http.StatusBadRequest {
		t.Fatalf("unknown stats field: %d %#v", code, body)
	}
}

func TestLibraryProjectionRebuildsWhenDefinitionChanges(t *testing.T) {
	catalog, _ := libraryFixture(t)
	if err := catalog.refreshAllLibrary(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec("UPDATE meta SET value='stale' WHERE key='workspace_library_version'"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ensureLibrary(); err != nil {
		t.Fatal(err)
	}
	if dirtyCount(t, catalog) != 3 {
		t.Fatalf("rebuild did not mark every workspace dirty: %d", dirtyCount(t, catalog))
	}
	var stored int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM workspace_library").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("rebuild kept stale rows: %d %v", stored, err)
	}
	if len(libraryRowsByID(t, catalog)) != 2 {
		t.Fatal("rows unavailable while the rebuilt projection is dirty")
	}
	var legacy int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'workspace_query_stats%'").Scan(&legacy); err != nil || legacy != 0 {
		t.Fatalf("legacy query stats remain: %d %v", legacy, err)
	}
}

func TestReopenedCatalogHasNoLegacyQueryStats(t *testing.T) {
	catalog, _ := testCatalog(t)
	// A second open keeps the library version, so only schema.sql runs.
	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	var legacy int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'workspace_query_stats%'").Scan(&legacy); err != nil || legacy != 0 {
		t.Fatalf("schema.sql recreated workspace_query_stats (superseded by workspace_library): %d %v", legacy, err)
	}
}

// Every request shape must answer exactly as it would from full rows, whether
// the projection is stored or computed live.
func TestLibraryRequestsReadingFewerFieldsMatchFullRows(t *testing.T) {
	for _, refreshed := range []bool{false, true} {
		catalog, config := libraryFixture(t)
		if _, err := catalog.DB.Exec(`UPDATE workspaces SET purpose='Rewrite the parser',outcome='Parser merged' WHERE id='c'`); err != nil {
			t.Fatal(err)
		}
		// A priced workspace, since costs are attached only where needed.
		doc := testPricing()
		doc.Changes = append(doc.Changes, priceChange{Provider: "openai", Model: "gpt-6-sol", EffectiveFrom: "2026-01-01", Input: rate(10), CacheRead: rate(1), Output: rate(100), Status: "confirmed", SourceURL: "https://example.com/g"})
		if err := catalog.replacePricing(doc, "test"); err != nil {
			t.Fatal(err)
		}
		ingestUsageFixture(t, catalog)
		if _, err := catalog.DB.Exec(`UPDATE agent_sessions SET model='gpt-6-sol'`); err != nil {
			t.Fatal(err)
		}
		if refreshed {
			if err := catalog.refreshAllLibrary(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		server := NewServer(config, catalog)
		schema, err := server.queryTableSchema("library")
		if err != nil {
			t.Fatal(err)
		}
		call := func(method, path, body string) any {
			t.Helper()
			request := httptest.NewRequest(method, path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer test-token")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("%s %s: %d %s", method, path, response.Code, response.Body.String())
			}
			return decodeJSON(t, response.Body.Bytes())
		}
		fullRows := func(search string) []map[string]any {
			t.Helper()
			rows, err := catalog.computeSearchRows(SearchOptions{Query: search}, nil)
			if err != nil {
				t.Fatal(err)
			}
			return rows
		}
		for _, body := range []string{
			`{"select":["title","repository_name","source_kind","activity_at","token_count","cost_usd","tool_use_count","tool_error_count","pr_count","pr_numbers","main_merge_title","branch_count","file_edit_count","outcome"],"orderBy":[{"field":"activity_at","dir":"desc","nulls":"last"},{"field":"id","dir":"asc"}],"limit":50,"offset":0}`,
			`{"select":["title"],"where":[{"field":"purpose","op":"contains","value":"summarized"}],"limit":10,"offset":0}`,
			`{"select":["title"],"where":[{"field":"outcome","op":"is_not_null","value":""}],"orderBy":[{"field":"cost_usd","dir":"desc"}],"limit":10,"offset":0}`,
			`{"select":["title"],"where":[{"any":[{"field":"changed_files","op":"contains","value":"parser"},{"field":"title","op":"=","value":"Bravo"}]}],"orderBy":[{"field":"purpose","dir":"asc"}],"limit":10,"offset":0}`,
			`{"select":["cost_usd"],"where":[{"field":"price_status","op":"=","value":"priced"}],"orderBy":[{"field":"cost_today_usd","dir":"asc"}],"limit":5,"offset":0}`,
			`{"select":["pr_numbers"],"orderBy":[{"field":"pr_numbers","dir":"desc","extract":{"regex":"(\\d+)"}}],"limit":1,"offset":1}`,
		} {
			var query querytable.Query
			if err := json.Unmarshal([]byte(body), &query); err != nil {
				t.Fatal(err)
			}
			for _, search := range []string{"", "parser"} {
				want, err := querytable.Apply(fullRows(search), query, schema)
				if err != nil {
					t.Fatal(err)
				}
				if err := catalog.attachLibraryPreviews(context.Background(), want.Rows); err != nil {
					t.Fatal(err)
				}
				got := call(http.MethodPost, "/api/query/library?search="+search, body)
				if expected := decodeJSON(t, mustJSON(t, want)); !reflect.DeepEqual(got, expected) {
					t.Fatalf("refreshed=%v search=%q %s:\ngot  %v\nwant %v", refreshed, search, body, got, expected)
				}
			}
		}
		for _, field := range []string{"changed_files", "purpose", "outcome", "source_kind", "repository_name", "pr_numbers", "price_status"} {
			want, err := querytable.Distinct(fullRows(""), field, "", 50, schema)
			if err != nil {
				t.Fatal(err)
			}
			if got := call(http.MethodGet, "/api/query/library/distinct?field="+field, ""); !reflect.DeepEqual(got, decodeJSON(t, mustJSON(t, want))) {
				t.Fatalf("refreshed=%v distinct %s: got %v, want %v", refreshed, field, got, want)
			}
		}
		aggregations := `{"where":[{"field":"purpose","op":"is_not_null","value":""}],"aggregations":[{"id":"n","op":"count","groupBy":["repository_name"]},{"id":"tokens","op":"sum","field":"token_count","groupBy":["source_kind"]},{"id":"outcomes","op":"count_distinct","field":"outcome"},{"id":"cost","op":"max","field":"cost_usd"}]}`
		var request querytable.AggregationRequest
		if err := json.Unmarshal([]byte(aggregations), &request); err != nil {
			t.Fatal(err)
		}
		want, err := querytable.Aggregate(fullRows(""), request, schema)
		if err != nil {
			t.Fatal(err)
		}
		if got := call(http.MethodPost, "/api/query/library/aggregations", aggregations); !reflect.DeepEqual(got, decodeJSON(t, mustJSON(t, want))) {
			t.Fatalf("refreshed=%v aggregations: got %v, want %v", refreshed, got, want)
		}
	}
}

// A default Library page must not read long text for rows it doesn't show:
// the cached row set carries none, and only the page is completed.
func TestLibraryDefaultPageReadsNoLongTextForOtherRows(t *testing.T) {
	catalog, config := libraryFixture(t)
	if err := catalog.refreshAllLibrary(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config, catalog)
	request := httptest.NewRequest(http.MethodPost, "/api/query/library", strings.NewReader(
		`{"select":["title","outcome","pr_numbers"],"orderBy":[{"field":"activity_at","dir":"desc","nulls":"last"},{"field":"id","dir":"asc"}],"limit":1,"offset":0}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	page := decodeJSON(t, response.Body.Bytes()).(map[string]any)["rows"].([]any)[0].(map[string]any)
	for _, field := range []string{"purpose", "outcome", "pr_details", "canonical_remote", "first_input", "cost_usd"} {
		if _, ok := page[field]; !ok {
			t.Errorf("page row lacks %s, which the UI renders: %v", field, page)
		}
	}
	if catalog.library.fields == nil {
		t.Fatal("the cache holds every column")
	}
	for _, row := range catalog.library.rows {
		for _, field := range append(append([]string{}, libraryLargeFields...), libraryCostFields...) {
			if _, ok := row[field]; ok {
				t.Fatalf("cached row carries %s: %v", field, row)
			}
		}
	}
	for _, value := range []func(libraryColumn) string{
		func(column libraryColumn) string { return "l." + column.name },
		func(column libraryColumn) string { return "(" + column.expr + ")" },
	} {
		statement := libraryRowSelect(value, catalog.library.fields)
		for _, text := range []string{"w.*", "purpose", "outcome", "summary_", "changed_files", "pr_details"} {
			if strings.Contains(statement, text) {
				t.Fatalf("default request reads %s for every row:\n%s", text, statement)
			}
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestLibraryPreviewsFollowTheirMessages(t *testing.T) {
	catalog, _ := libraryFixture(t)
	ctx := context.Background()
	preview := func() (any, any) {
		t.Helper()
		page := []map[string]any{{"id": "c"}}
		if err := catalog.attachLibraryPreviews(ctx, page); err != nil {
			t.Fatal(err)
		}
		return page[0]["first_input"], page[0]["last_response"]
	}
	stored := func() int {
		t.Helper()
		var count int
		if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM workspace_previews WHERE workspace_id='c'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if err := catalog.refreshAllLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	if first, last := preview(); first != "Please fix the parser" || last != "Fixed it" || stored() != 1 {
		t.Fatalf("preview %q/%q, stored %d", first, last, stored())
	}
	// A new response marks the workspace dirty, which drops the stored
	// preview until the refresh; meanwhile the page finds it itself.
	if _, err := catalog.DB.Exec(`INSERT INTO messages(id,conversation_id,native_id,role,kind,text,created_at,content_hash)
		VALUES('m9','cc','9','assistant','message','Shipped it','2026-09-23T03:00:00Z','h')`); err != nil {
		t.Fatal(err)
	}
	if _, last := preview(); last != "Shipped it" || stored() != 0 {
		t.Fatalf("latest response %q, stored %d", last, stored())
	}
	// A build without previews refreshes the projection only: no stale row.
	if _, err := catalog.DB.Exec("DELETE FROM workspace_library_dirty"); err != nil {
		t.Fatal(err)
	}
	if _, last := preview(); last != "Shipped it" || stored() != 0 {
		t.Fatalf("after an older build's refresh: %q, stored %d", last, stored())
	}
	// Large workspaces are filled in; small ones are quick to preview without.
	if filled, err := catalog.refreshPreviews(ctx, 25); err != nil || filled != 0 {
		t.Fatalf("filled %d small workspaces (%v)", filled, err)
	}
	if _, err := catalog.DB.Exec("UPDATE workspace_library SET turn_count=1000 WHERE workspace_id='c'"); err != nil {
		t.Fatal(err)
	}
	if filled, err := catalog.refreshPreviews(ctx, 25); err != nil || filled != 1 || stored() != 1 {
		t.Fatalf("filled %d, stored %d (%v)", filled, stored(), err)
	}
	if _, last := preview(); last != "Shipped it" {
		t.Fatalf("filled preview %q", last)
	}
}

func TestLibraryRowsReadTheWorkspaceIndex(t *testing.T) {
	catalog, _ := libraryFixture(t)
	stored := libraryRowSelect(func(column libraryColumn) string { return "l." + column.name }, libraryCompactFields) +
		" JOIN workspace_library l ON l.workspace_id=w.id WHERE w.id NOT IN (SELECT workspace_id FROM workspace_library_dirty)"
	plan, err := queryMaps(catalog.DB, "EXPLAIN QUERY PLAN "+stored)
	if err != nil {
		t.Fatal(err)
	}
	if text := jsonText(plan); !strings.Contains(text, "COVERING INDEX workspaces_library_idx") {
		t.Fatalf("Library rows read the workspace table, so workspaces_library_idx lacks a field they use: %s", text)
	}
}
