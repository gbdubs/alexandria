package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isCanonicalTime(value string) bool {
	parsed, err := time.Parse(timeLayout, value)
	return err == nil && formatTime(parsed) == value
}

func TestCanonicalTimeParsesKnownShapes(t *testing.T) {
	for _, test := range []struct {
		value any
		want  string
	}{
		{"2026-04-03 04:13:07", "2026-04-03T04:13:07.000Z"},
		{"2026-04-03 04:13:07.5", "2026-04-03T04:13:07.500Z"},
		{"2026-04-03 04:13:07.123456", "2026-04-03T04:13:07.123Z"},
		{"2026-04-03T04:13:07Z", "2026-04-03T04:13:07.000Z"},
		{"2026-04-03T04:13:07.1Z", "2026-04-03T04:13:07.100Z"},
		{"2026-04-03T04:13:07.603Z", "2026-04-03T04:13:07.603Z"},
		{"2026-04-03T04:13:07.123456789Z", "2026-04-03T04:13:07.123Z"},
		{"2026-04-03T04:13:07.123456+00:00", "2026-04-03T04:13:07.123Z"},
		{"2026-04-02T22:13:07-06:00", "2026-04-03T04:13:07.000Z"},
		{"2026-04-03T04:13:07", "2026-04-03T04:13:07.000Z"},
		{"2026-04-03", "2026-04-03T00:00:00.000Z"},
		{float64(1775189587.25), "2026-04-03T04:13:07.250Z"},
		{int64(1775189587), "2026-04-03T04:13:07.000Z"},
		{int64(1775189587250), "2026-04-03T04:13:07.250Z"},
		{json.Number("1775189587"), "2026-04-03T04:13:07.000Z"},
		{"1775189587250", "2026-04-03T04:13:07.250Z"},
		{time.Date(2026, 4, 2, 22, 13, 7, 0, time.FixedZone("MDT", -6*3600)), "2026-04-03T04:13:07.000Z"},
		{nil, ""}, {"", ""}, {"not a time", ""}, {"2026-13-40 99:00:00", ""},
	} {
		if got := canonicalTime(test.value); got != test.want {
			t.Errorf("canonicalTime(%#v) = %q; want %q", test.value, got, test.want)
		}
	}
	if got := iso("not a time"); got != "not a time" {
		t.Errorf("iso must keep unparseable values verbatim, got %q", got)
	}
	if !isCanonicalTime(now()) {
		t.Errorf("now() = %q is not canonical", now())
	}
}

// assertCanonicalTimestamps checks every covered column in the catalog.
func assertCanonicalTimestamps(t *testing.T, catalog *Catalog) int {
	t.Helper()
	checked := 0
	for _, target := range timestampColumns {
		rows, err := catalog.DB.Query(fmt.Sprintf("SELECT %s FROM %s WHERE %[1]s IS NOT NULL", target.column, target.table))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				t.Fatal(err)
			}
			if !isCanonicalTime(value) {
				t.Errorf("%s.%s stored %q", target.table, target.column, value)
			}
			checked++
		}
		rows.Close()
	}
	return checked
}

func TestIngestStoresCanonicalTimestamps(t *testing.T) {
	catalog, _ := testCatalog(t)
	export := filepath.Join(t.TempDir(), "export.json")
	document := `{"workspaces": [{
		"id": "work-1", "title": "Timestamps", "activity_at": "2026-04-03 04:13:07",
		"repository": {"canonical_remote": "github.com/acme/parser", "display_name": "parser"},
		"conversations": [{
			"id": "conversation-1", "provider": "codex",
			"started_at": "2026-04-03T06:13:07+02:00", "ended_at": 1775190000.5,
			"messages": [
				{"id": "m1", "role": "user", "text": "one", "created_at": "2026-04-03 04:13:08.25"},
				{"id": "m2", "role": "assistant", "text": "two", "created_at": "2026-04-03T04:13:09.1Z"},
				{"id": "m3", "role": "user", "text": "three", "created_at": 1775189590123},
				{"id": "m4", "role": "assistant", "text": "four", "created_at": "2026-04-03T04:13:11.123456+00:00"}
			]
		}],
		"attempts": [{"source_id": "a1", "started_at": "2026-04-03 04:00:00", "ended_at": "2026-04-03 04:10:00"}],
		"handoffs": [{"id": "h1", "created_at": "2026-04-03 04:11:00"}],
		"metrics": [{"name": "custom", "value": 1, "observed_at": "2026-04-03T04:12:00.5Z"}],
		"changes": [{"classification": "attempted", "created_at": "2026-04-03 04:12:30", "files": []}],
		"prs": [{"number": 7, "observed_at": "2026-04-03 04:12:45"}]
	}]}`
	if err := os.WriteFile(export, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := MakeAdapter(SourceConfig{Name: "fixture", Kind: "canonical", Path: export, Account: "local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if result := catalog.Ingest(adapter, nil); result.Error != nil {
		t.Fatalf("ingest: %v", result.Error)
	}
	if checked := assertCanonicalTimestamps(t, catalog); checked < 20 {
		t.Fatalf("only %d timestamps were stored; fixture did not reach every writer", checked)
	}
	for query, want := range map[string]string{
		"SELECT activity_at FROM workspaces":                          "2026-04-03T04:13:07.000Z",
		"SELECT started_at FROM conversations":                        "2026-04-03T04:13:07.000Z",
		"SELECT ended_at FROM conversations":                          "2026-04-03T04:20:00.500Z",
		"SELECT created_at FROM messages WHERE native_id='m1'":        "2026-04-03T04:13:08.250Z",
		"SELECT created_at FROM messages WHERE native_id='m3'":        "2026-04-03T04:13:10.123Z",
		"SELECT created_at FROM messages WHERE native_id='m4'":        "2026-04-03T04:13:11.123Z",
		"SELECT started_at FROM task_attempts":                        "2026-04-03T04:00:00.000Z",
		"SELECT created_at FROM handoffs":                             "2026-04-03T04:11:00.000Z",
		"SELECT observed_at FROM metrics WHERE name='custom'":         "2026-04-03T04:12:00.500Z",
		"SELECT created_at FROM change_sets":                          "2026-04-03T04:12:30.000Z",
		"SELECT observed_at FROM pull_requests":                       "2026-04-03T04:12:45.000Z",
		"SELECT occurred_at FROM activity_events ORDER BY id LIMIT 1": "2026-04-03T04:13:07.000Z",
	} {
		var got string
		if err := catalog.DB.QueryRow(query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got != want {
			t.Errorf("%s = %q; want %q", query, got, want)
		}
	}
}

func TestTimestampMigrationNormalizesExistingRows(t *testing.T) {
	catalog, _ := testCatalog(t)
	// Seed one row per covered table with a different legacy shape in each
	// column; foreign keys are off only while seeding.
	legacy := []struct{ stored, want string }{
		{"2026-04-03 04:13:07", "2026-04-03T04:13:07.000Z"},
		{"2026-04-03T04:13:07.6Z", "2026-04-03T04:13:07.600Z"},
		{"2026-04-03T04:13:07.123456+00:00", "2026-04-03T04:13:07.123Z"},
		{"2026-04-02T22:13:07.5-06:00", "2026-04-03T04:13:07.500Z"},
		{"2026-04-03 04:13:07.25", "2026-04-03T04:13:07.250Z"},
		{"2026-04-03T04:13:07Z", "2026-04-03T04:13:07.000Z"},
	}
	ctx := context.Background()
	connection, err := catalog.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	tables := []string{}
	columns := map[string][]string{}
	for _, target := range timestampColumns {
		if len(columns[target.table]) == 0 {
			tables = append(tables, target.table)
		}
		columns[target.table] = append(columns[target.table], target.column)
	}
	shape := 0
	for _, table := range tables {
		info, err := queryMaps(catalog.DB, "SELECT name,type,\"notnull\",dflt_value,pk FROM pragma_table_info(?)", table)
		if err != nil {
			t.Fatal(err)
		}
		names, values := []string{}, []any{}
		for _, column := range info {
			name := firstString(column["name"])
			if integer(column["notnull"]) == 1 && column["dflt_value"] == nil || integer(column["pk"]) > 0 {
				names = append(names, name)
				if strings.Contains(strings.ToUpper(firstString(column["type"])), "INT") || strings.Contains(strings.ToUpper(firstString(column["type"])), "REAL") {
					values = append(values, 1)
				} else {
					// One shared ID makes every foreign key resolve.
					values = append(values, "x")
				}
			}
		}
		for _, column := range columns[table] {
			value := legacy[shape%len(legacy)]
			shape++
			want[table+"."+column] = value.want
			found := false
			for index, name := range names {
				if name == column {
					values[index], found = value.stored, true
				}
			}
			if !found {
				names, values = append(names, column), append(values, value.stored)
			}
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
		if _, err := connection.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s)", table, strings.Join(names, ","), placeholders), values...); err != nil {
			t.Fatalf("seed %s: %v", table, err)
		}
	}
	if _, err := connection.ExecContext(ctx, "DELETE FROM meta WHERE key='timestamp_format_version'"); err != nil {
		t.Fatal(err)
	}
	connection.Close()

	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	for _, target := range timestampColumns {
		var got string
		if err := catalog.DB.QueryRow(fmt.Sprintf("SELECT %s FROM %s", target.column, target.table)).Scan(&got); err != nil {
			t.Fatalf("%s.%s: %v", target.table, target.column, err)
		}
		if key := target.table + "." + target.column; got != want[key] {
			t.Errorf("%s = %q; want %q", key, got, want[key])
		}
	}
	assertCanonicalTimestamps(t, catalog)

	// Re-running changes nothing, whether guarded by the meta key or forced.
	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec("DELETE FROM meta WHERE key='timestamp_format_version'"); err != nil {
		t.Fatal(err)
	}
	if updated, err := catalog.normalizeTimestamps(); err != nil || updated != 0 {
		t.Fatalf("forced second normalization updated %d values: %v", updated, err)
	}
	for _, target := range timestampColumns {
		var got string
		catalog.DB.QueryRow(fmt.Sprintf("SELECT %s FROM %s", target.column, target.table)).Scan(&got)
		if key := target.table + "." + target.column; got != want[key] {
			t.Errorf("after second run %s = %q; want %q", key, got, want[key])
		}
	}
}

// TestSuppressMirrorsIgnoresInputOrder links two equal-priority mirrors and
// leaves standalone rows with tied activity; any input order gives one result.
func TestSuppressMirrorsIgnoresInputOrder(t *testing.T) {
	catalog, _ := testCatalog(t)
	for _, id := range []string{"codex-a", "claude-b"} {
		if _, err := catalog.DB.Exec(`INSERT INTO workspaces(id,source_kind,source_account,source_id,title,indexed_at) VALUES(?,?,?,?,?,?)`, id, strings.Split(id, "-")[0], "local", id, id, now()); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.DB.Exec(`INSERT INTO conversations(id,workspace_id,provider,account,native_id) VALUES(?,?,?,?,?)`, "c-"+id, id, "codex", "local", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := catalog.DB.Exec(`INSERT INTO conversation_identity_links(left_id,right_id,relationship,confidence,evidence_json) VALUES('c-codex-a','c-claude-b','mirror',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	rows := func() []map[string]any {
		return []map[string]any{
			{"id": "codex-a", "source_kind": "codex", "activity_at": "2026-04-03T04:13:07.000Z"},
			{"id": "claude-b", "source_kind": "claude", "activity_at": "2026-04-03T05:00:00.000Z"},
			{"id": "tie-2", "source_kind": "codex", "activity_at": "2026-04-03T04:13:07.000Z"},
			{"id": "tie-1", "source_kind": "codex", "activity_at": "2026-04-03T04:13:07.000Z"},
		}
	}
	var first string
	for trial := range 50 {
		input := rows()
		for i := len(input) - 1; i > 0; i-- {
			j := (trial*7 + i*3) % (i + 1)
			input[i], input[j] = input[j], input[i]
		}
		output := catalog.suppressMirrors(input)
		ids := []string{}
		for _, row := range output {
			ids = append(ids, fmt.Sprint(row["id"], row["mirrored_workspace_ids"]))
		}
		got := strings.Join(ids, ",")
		if first == "" {
			first = got
			if want := "claude-b[codex-a],tie-1[],tie-2[]"; got != want {
				t.Fatalf("suppressMirrors = %s; want %s", got, want)
			}
		} else if got != first {
			t.Fatalf("trial %d: %s; first %s", trial, got, first)
		}
	}
}
