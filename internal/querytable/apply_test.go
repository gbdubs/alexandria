package querytable

import (
	"reflect"
	"testing"
)

func testSchema() Schema {
	return Schema{Name: "items", IDField: "id", Fields: map[string]Field{
		"id":     {Name: "id", Kind: Text, Filterable: true, Sortable: true},
		"name":   {Name: "name", Kind: Text, Filterable: true, Sortable: true},
		"status": {Name: "status", Kind: Enum, Filterable: true, Sortable: true},
		"count":  {Name: "count", Kind: Number, Filterable: true, Sortable: true},
		"tags":   {Name: "tags", Kind: TextArray, Filterable: true, Sortable: true},
	}}
}

func TestApplyFiltersSortsAndPaginates(t *testing.T) {
	rows := []map[string]any{
		{"id": "a", "name": "Alpha", "status": "done", "count": 2.0, "tags": []string{"go"}},
		{"id": "b", "name": "Beta", "status": "open", "count": 7.0, "tags": []string{"ui", "go"}},
		{"id": "c", "name": "Gamma", "status": "open", "count": 5.0, "tags": []string{"ui"}},
	}
	query := Query{
		Where: []WhereTerm{
			{Any: []WhereClause{{Field: "name", Op: "contains", Value: "bet"}, {Field: "name", Op: "contains", Value: "gam"}}},
			{Field: "tags", Op: "includes", Value: "ui"},
		},
		OrderBy: []OrderBy{{Field: "count", Dir: "desc"}}, Limit: 1, Offset: 1,
	}
	result, err := Apply(rows, query, testSchema())
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || len(result.Rows) != 1 || result.Rows[0]["id"] != "c" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestApplyRejectsUnknownFields(t *testing.T) {
	_, err := Apply(nil, Query{Where: []WhereTerm{{Field: "secret", Op: "=", Value: "x"}}, Limit: 10}, testSchema())
	if err == nil {
		t.Fatal("expected unknown field to be rejected")
	}
}

func TestApplyPendingNumberFilter(t *testing.T) {
	rows := []map[string]any{{"id": "a", "count": 0.0}, {"id": "b", "count": 2.0}}
	for _, op := range []string{"=", "!=", ">"} {
		result, err := Apply(rows, Query{Where: []WhereTerm{{Field: "count", Op: op, Value: ""}}, Limit: 10}, testSchema())
		if err != nil || result.Total != 2 {
			t.Fatalf("pending %s filter: result=%#v err=%v", op, result, err)
		}
	}
	result, err := Apply(rows, Query{Where: []WhereTerm{{Field: "count", Op: "=", Value: "0"}}, Limit: 10}, testSchema())
	if err != nil || result.Total != 1 || result.Rows[0]["id"] != "a" {
		t.Fatalf("zero filter: result=%#v err=%v", result, err)
	}
}

func TestDistinctAndAggregate(t *testing.T) {
	rows := []map[string]any{
		{"id": "a", "status": "done", "count": 2.0},
		{"id": "b", "status": "open", "count": 4.0},
		{"id": "c", "status": "open", "count": 6.0},
	}
	distinct, err := Distinct(rows, "status", "o", 10, testSchema())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(distinct.Values, []string{"done", "open"}) {
		t.Fatalf("distinct: %#v", distinct)
	}
	aggregated, err := Aggregate(rows, AggregationRequest{Aggregations: []Aggregation{{ID: "avg", Op: "avg", Field: "count", GroupBy: []string{"status"}}}}, testSchema())
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregated.Metrics) != 1 || len(aggregated.Metrics[0].Buckets) != 2 {
		t.Fatalf("aggregated: %#v", aggregated)
	}
}

func TestCountMeasureDoesNotCountNulls(t *testing.T) {
	rows := []map[string]any{{"id": "a", "count": nil}, {"id": "b", "count": nil}}
	result, err := Aggregate(rows, AggregationRequest{Aggregations: []Aggregation{{ID: "count", Op: "count", Field: "count"}}}, testSchema())
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Metrics[0].Buckets[0].Value; got != 0 {
		t.Fatalf("COUNT(field) = %#v, want 0", got)
	}
}

func TestAggregateIncludesAllFilteredRows(t *testing.T) {
	rows := make([]map[string]any, MaxLimit+7)
	for index := range rows {
		rows[index] = map[string]any{"id": index, "status": "open", "count": 1.0}
	}
	result, err := Aggregate(rows, AggregationRequest{Aggregations: []Aggregation{{ID: "total", Op: "count"}}}, testSchema())
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Metrics[0].Buckets[0].Value; got != MaxLimit+7 {
		t.Fatalf("COUNT(*) = %#v, want %d", got, MaxLimit+7)
	}
}

func TestArrayIncludesCaseSensitivity(t *testing.T) {
	rows := []map[string]any{
		{"id": "a", "tags": []string{"Bug", "ui"}},
		{"id": "b", "tags": []any{"bug"}},
		{"id": "c", "tags": nil},
		{"id": "d"},
		{"id": "e", "tags": []string{}},
	}
	ids := func(schema Schema, clause WhereClause) []string {
		t.Helper()
		result, err := Apply(rows, Query{Where: []WhereTerm{{Field: clause.Field, Op: clause.Op, Value: clause.Value, Negated: clause.Negated}}, Limit: 10}, schema)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, row := range result.Rows {
			out = append(out, row["id"].(string))
		}
		return out
	}
	insensitive := testSchema()
	if got := ids(insensitive, WhereClause{Field: "tags", Op: "includes", Value: "BUG"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("default includes = %v", got)
	}
	sensitive := testSchema()
	tags := sensitive.Fields["tags"]
	tags.ArrayCaseSensitive = true
	sensitive.Fields = map[string]Field{"id": sensitive.Fields["id"], "tags": tags}
	if got := ids(sensitive, WhereClause{Field: "tags", Op: "includes", Value: "Bug"}); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("case-sensitive includes = %v", got)
	}
	if got := ids(sensitive, WhereClause{Field: "tags", Op: "includes", Value: "BUG"}); len(got) != 0 {
		t.Fatalf("case-sensitive includes matched another case: %v", got)
	}
	// Null and missing arrays neither include a value nor satisfy its negation.
	if got := ids(sensitive, WhereClause{Field: "tags", Op: "includes", Value: "Bug", Negated: true}); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("negated includes = %v", got)
	}
	if got := ids(sensitive, WhereClause{Field: "tags", Op: "is_null"}); !reflect.DeepEqual(got, []string{"c", "d", "e"}) {
		t.Fatalf("is_null = %v", got)
	}
}

func TestLoadSchemaReadsArrayCaseSensitive(t *testing.T) {
	schema, err := LoadSchema([]byte(`{"name":"items","idField":"id","fields":[
		{"name":"id","type":"text","bindings":{"sql":{}}},
		{"name":"labels","type":"textarray","filter":{"arrayCaseSensitive":true},"bindings":{"sql":{}}},
		{"name":"tags","type":"textarray","bindings":{"sql":{}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !schema.Fields["labels"].ArrayCaseSensitive || schema.Fields["tags"].ArrayCaseSensitive {
		t.Fatalf("fields: %#v", schema.Fields)
	}
}

func TestExplicitOpsCannotWidenTypeMatrix(t *testing.T) {
	field := Field{Name: "tags", Kind: TextArray, Filterable: true, FilterOps: []string{"includes", "contains"}}
	if !opAllowed(field, "includes") || opAllowed(field, "contains") || opAllowed(field, "is_null") {
		t.Fatal("explicit ops must narrow, not widen, the textarray matrix")
	}
}

func TestFieldStats(t *testing.T) {
	schema := testSchema()
	for name, field := range schema.Fields {
		field.Backend = name != "status"
		schema.Fields[name] = field
	}
	rows := []map[string]any{
		{"id": "a", "name": "Alpha", "count": 2.0, "tags": []string{"go"}},
		{"id": "b", "name": "Alpha", "count": 10.0, "tags": []string{}},
		{"id": "c", "name": "", "count": nil},
	}
	stats, err := FieldStats(rows, []string{"name", "count", "tags", "status"}, schema)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]FieldStat{"name": {Distinct: Count(1)}, "count": {Distinct: Count(2), Min: 2.0, Max: 10.0}, "tags": {Distinct: Count(1)}}
	if !reflect.DeepEqual(stats, want) {
		t.Fatalf("stats = %#v", stats)
	}
	if _, err := FieldStats(rows, []string{"secret"}, schema); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := FieldStats(rows, make([]string, MaxFieldStats+1), schema); err == nil {
		t.Fatal("oversized request accepted")
	}
}
