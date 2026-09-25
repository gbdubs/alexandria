package archive

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// otherProcess opens the catalog file the way a separate CLI process would,
// with its own pool that the Library cache knows nothing about.
func otherProcess(t *testing.T, catalog *Catalog) *Catalog {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+catalog.Path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Catalog{Path: catalog.Path, DB: db}
}

func sameRow(left, right map[string]any) bool {
	return reflect.ValueOf(left).UnsafePointer() == reflect.ValueOf(right).UnsafePointer()
}

func TestLibraryCacheSeesCommitsFromAnotherConnection(t *testing.T) {
	catalog, _ := libraryFixture(t)
	if err := catalog.refreshAllLibrary(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := otherProcess(t, catalog)
	first, second := libraryRowsByID(t, catalog), libraryRowsByID(t, catalog)
	if !sameRow(first["c"], second["c"]) {
		t.Fatal("an unchanged catalog rebuilt the Library rows")
	}
	for _, change := range []struct {
		name, statement string
		check           func(map[string]map[string]any) bool
	}{
		{"message insert", `INSERT INTO messages(id,conversation_id,native_id,role,kind,text,content_hash) VALUES('m6','cc','6','user','message','Again','h')`,
			func(rows map[string]map[string]any) bool { return integer(rows["c"]["turn_count"]) == 2 }},
		{"metric insert", `INSERT INTO metrics(workspace_id,name,value,unit,status,extractor_version,observed_at) VALUES('b','total_tokens',77,'tokens','observed','test','x')`,
			func(rows map[string]map[string]any) bool { return integer(rows["b"]["token_count"]) == 77 }},
		{"workspace title update", `UPDATE workspaces SET title='Charlie 2' WHERE id='c'`,
			func(rows map[string]map[string]any) bool { return rows["c"]["title"] == "Charlie 2" }},
		{"repository rename", `UPDATE repositories SET display_name='renamed' WHERE id='repo'`,
			func(rows map[string]map[string]any) bool { return rows["b"]["repository_name"] == "renamed" }},
		// c becomes a mirror of the tl1 workspace b, which represents both.
		{"identity link", `INSERT INTO conversation_identity_links(left_id,right_id,relationship,confidence,evidence_json) VALUES('cc','cb','native-alias',1,'{}')`,
			func(rows map[string]map[string]any) bool {
				return rows["c"] == nil && strings.Contains(fmt.Sprint(rows["b"]["mirrored_workspace_ids"]), "c")
			}},
	} {
		if _, err := other.DB.Exec(change.statement); err != nil {
			t.Fatalf("%s: %v", change.name, err)
		}
		if rows := libraryRowsByID(t, catalog); !change.check(rows) {
			t.Fatalf("%s: cached rows are stale: %#v", change.name, rows)
		}
		// The refresher's commit changes the version but no values.
		before := libraryRowsByID(t, catalog)
		if err := other.refreshAllLibrary(context.Background()); err != nil {
			t.Fatal(err)
		}
		if after := libraryRowsByID(t, catalog); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: refreshed rows differ:\nbefore %#v\nafter  %#v", change.name, before, after)
		}
	}
}

func TestLibraryCacheRepricesOnPriceBookAndDateChanges(t *testing.T) {
	catalog, _ := testCatalog(t)
	today := time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)
	catalog.now = func() time.Time { return today }
	ingestUsageFixture(t, catalog)
	if _, err := catalog.DB.Exec(`UPDATE agent_sessions SET model='cache-probe-1'`); err != nil {
		t.Fatal(err)
	}
	cost := func() (any, any) {
		t.Helper()
		rows, err := catalog.searchRows(SearchOptions{}, nil)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%v %v", rows, err)
		}
		return rows[0]["cost_usd"], rows[0]["cost_today_usd"]
	}
	if total, _ := cost(); total != nil {
		t.Fatalf("unpriced model has cost %v", total)
	}
	doc := testPricing()
	doc.Changes = append(doc.Changes,
		priceChange{Provider: "openai", Model: "cache-probe-1", EffectiveFrom: "2026-01-01", Input: rate(10), CacheRead: rate(1), Output: rate(100), Status: "confirmed", SourceURL: "https://example.com/g"},
		priceChange{Provider: "openai", Model: "cache-probe-1", EffectiveFrom: "2026-10-01", Input: rate(20), CacheRead: rate(2), Output: rate(200), Status: "confirmed", SourceURL: "https://example.com/h"})
	if err := otherProcess(t, catalog).replacePricing(doc, "test"); err != nil {
		t.Fatal(err)
	}
	// Uncached 700 x $10, cache reads 2,300 x $1, output 50 x $100, per million.
	want := (700*10 + 2300*1 + 50*100) / 1e6
	total, beforeCut := cost()
	if !near(ptr(total), want) || !near(ptr(beforeCut), want) {
		t.Fatalf("after price-book change: cost %v today %v, want %v", total, beforeCut, want)
	}
	today = time.Date(2026, 10, 2, 12, 0, 0, 0, time.Local)
	if total, afterCut := cost(); !near(ptr(total), want) || !near(ptr(afterCut), 2*want) {
		t.Fatalf("after date change: cost %v today %v, want %v and %v", total, afterCut, want, 2*want)
	}
}

func ptr(value any) *float64 {
	if number, ok := value.(float64); ok {
		return &number
	}
	return nil
}

func TestLibraryCacheServesConcurrentRequestsWithoutSharingPages(t *testing.T) {
	catalog, config := libraryFixture(t)
	server := NewServer(config, catalog)
	other := otherProcess(t, catalog)
	request := func(method, path, body string) error {
		recorder := httptest.NewRecorder()
		incoming := httptest.NewRequest(method, path, strings.NewReader(body))
		incoming.Header.Set("Authorization", "Bearer test-token")
		server.ServeHTTP(recorder, incoming)
		if recorder.Code != http.StatusOK {
			return fmt.Errorf("%s %s: %d %s", method, path, recorder.Code, recorder.Body.String())
		}
		return nil
	}
	var group sync.WaitGroup
	errs := make(chan error, 200)
	for worker := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for iteration := range 10 {
				errs <- request(http.MethodPost, "/api/query/library", `{"select":["title"],"orderBy":[{"field":"title","dir":"asc"}],"limit":10,"offset":0}`)
				errs <- request(http.MethodGet, "/api/query/library/distinct?field=repository_name", "")
				if worker == 0 {
					_, err := other.DB.Exec(`UPDATE workspaces SET title=? WHERE id='c'`, fmt.Sprint("Charlie ", iteration))
					errs <- err
				}
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := catalog.searchRows(SearchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if _, ok := row["first_input"]; ok {
			t.Fatalf("page previews leaked into cached rows: %#v", row)
		}
	}
}
