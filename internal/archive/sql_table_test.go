package archive

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"alexandria/internal/querytable"
)

func TestSQLArrayIncludesCaseSensitivity(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE items(id TEXT, tags TEXT);
		INSERT INTO items VALUES ('a','["Bug","ui"]'),('b','["bug"]'),('c',NULL),('d',''),('e','[]'),('f','not json')`); err != nil {
		t.Fatal(err)
	}
	table := sqlDataset{from: "FROM items", columns: map[string]string{"id": "id", "tags": "tags"}}
	schema := querytable.Schema{Name: "items", IDField: "id", Fields: map[string]querytable.Field{
		"id":   {Name: "id", Kind: querytable.Text, Filterable: true, Sortable: true},
		"tags": {Name: "tags", Kind: querytable.TextArray, Filterable: true},
	}}
	ids := func(schema querytable.Schema, term querytable.WhereTerm) []string {
		t.Helper()
		result, err := table.Rows(context.Background(), db, querytable.Query{Where: []querytable.WhereTerm{term}, OrderBy: []querytable.OrderBy{{Field: "id", Dir: "asc"}}, Limit: 10}, schema)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, row := range result.Rows {
			out = append(out, firstString(row["id"]))
		}
		return out
	}
	if got := ids(schema, querytable.WhereTerm{Field: "tags", Op: "includes", Value: "BUG"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("default includes = %v", got)
	}
	tags := schema.Fields["tags"]
	tags.ArrayCaseSensitive = true
	schema.Fields["tags"] = tags
	if got := ids(schema, querytable.WhereTerm{Field: "tags", Op: "includes", Value: "Bug"}); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("case-sensitive includes = %v", got)
	}
	if got := ids(schema, querytable.WhereTerm{Field: "tags", Op: "includes", Value: "Bug", Negated: true}); !reflect.DeepEqual(got, []string{"b", "f"}) {
		t.Fatalf("negated includes = %v", got)
	}
	if got := ids(schema, querytable.WhereTerm{Field: "tags", Op: "is_null"}); !reflect.DeepEqual(got, []string{"c", "d", "e"}) {
		t.Fatalf("is_null = %v", got)
	}
}

func TestSQLPendingNumberFilter(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE items(id TEXT, subagent_count INTEGER);
		INSERT INTO items VALUES ('a',0),('b',2)`); err != nil {
		t.Fatal(err)
	}
	table := sqlDataset{from: "FROM items", columns: map[string]string{"id": "id", "subagent_count": "subagent_count"}}
	schema := querytable.Schema{Name: "items", IDField: "id", Fields: map[string]querytable.Field{
		"id":             {Name: "id", Kind: querytable.Text, Filterable: true, Sortable: true},
		"subagent_count": {Name: "subagent_count", Kind: querytable.Number, Filterable: true},
	}}
	for _, op := range []string{"=", "!=", ">"} {
		result, err := table.Rows(context.Background(), db, querytable.Query{Where: []querytable.WhereTerm{{Field: "subagent_count", Op: op, Value: ""}}, Limit: 10}, schema)
		if err != nil || result.Total != 2 {
			t.Fatalf("pending %s filter: result=%#v err=%v", op, result, err)
		}
	}
	result, err := table.Rows(context.Background(), db, querytable.Query{Where: []querytable.WhereTerm{{Field: "subagent_count", Op: "=", Value: "0"}}, Limit: 10}, schema)
	if err != nil || result.Total != 1 || result.Rows[0]["id"] != "a" {
		t.Fatalf("zero filter: result=%#v err=%v", result, err)
	}
}
