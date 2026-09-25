package querytable

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

// TestSortRowsMatchesPairwiseCompare checks the pre-parsed sort against the
// direct comparator over values that parse, fail to parse, and are null.
func TestSortRowsMatchesPairwiseCompare(t *testing.T) {
	schema := Schema{Fields: map[string]Field{
		"n": {Name: "n", Kind: Number}, "d": {Name: "d", Kind: Datetime},
		"b": {Name: "b", Kind: Bool}, "s": {Name: "s", Kind: Text},
	}}
	pools := map[string][]any{
		"n": {nil, "", 1, int64(2), 2.5, "3", "x", "10", -1.0, []any{}},
		"d": {nil, "", "2026-09-24", "2026-09-24T01:00:00Z", "2026-09-23T23:00:00-05:00", "not a date", "2026-09-24T01:00:00.5Z"},
		"b": {nil, true, false, "true", "0", 1, "maybe"},
		"s": {nil, "", "a", "B", 3, []string{}, "ab"},
	}
	random := rand.New(rand.NewSource(1))
	for trial := range 200 {
		rows := make([]map[string]any, 60)
		for index := range rows {
			rows[index] = map[string]any{"id": index}
			for field, pool := range pools {
				rows[index][field] = pool[random.Intn(len(pool))]
			}
		}
		fields := []string{"n", "d", "b", "s"}
		random.Shuffle(len(fields), func(i, j int) { fields[i], fields[j] = fields[j], fields[i] })
		orderBy := []OrderBy{}
		for _, field := range fields[:1+random.Intn(3)] {
			order := OrderBy{Field: field, Dir: []string{"asc", "desc"}[random.Intn(2)], Nulls: []string{"", "first", "last"}[random.Intn(3)]}
			orderBy = append(orderBy, order)
		}
		want := append([]map[string]any(nil), rows...)
		sort.SliceStable(want, func(i, j int) bool {
			for _, order := range orderBy {
				left, right := want[i][order.Field], want[j][order.Field]
				if nullish(left) || nullish(right) {
					if nullish(left) && nullish(right) {
						continue
					}
					return nullish(left) == (order.Nulls == "first")
				}
				if comparison := compare(left, right, schema.Fields[order.Field].Kind); comparison != 0 {
					return (comparison > 0) == (order.Dir == "desc")
				}
			}
			return false
		})
		if got := sortRows(rows, orderBy, schema); !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d order %+v:\ngot  %v\nwant %v", trial, orderBy, got, want)
		}
	}
}

// TestDatetimeSortIgnoresInputOrder sorts SQLite space-format, ISO and
// unparseable datetimes; every permutation must produce the same order.
func TestDatetimeSortIgnoresInputOrder(t *testing.T) {
	schema := Schema{Fields: map[string]Field{"activity_at": {Name: "activity_at", Kind: Datetime}}}
	values := []any{
		"2026-04-03 04:13:07", "2026-04-03T04:13:07.603Z", "2026-04-03T04:13:06Z",
		"2026-04-03 04:13:07.9", "2026-04-02T23:00:00-06:00", "2026-04-03 23:59:59",
		"2026-04-03T10:00:00.000Z", "2026-04-03", "unknown", "zzz", "2026-04-04 00:00:00",
	}
	want := []any{
		"2026-04-03", "2026-04-03T04:13:06Z", "2026-04-03 04:13:07", "2026-04-03T04:13:07.603Z",
		"2026-04-03 04:13:07.9", "2026-04-02T23:00:00-06:00", "2026-04-03T10:00:00.000Z",
		"2026-04-03 23:59:59", "2026-04-04 00:00:00", "unknown", "zzz",
	}
	random := rand.New(rand.NewSource(1))
	for trial := range 200 {
		rows := make([]map[string]any, len(values))
		for index, position := range random.Perm(len(values)) {
			rows[index] = map[string]any{"activity_at": values[position]}
		}
		for _, dir := range []string{"asc", "desc"} {
			sorted := sortRows(rows, []OrderBy{{Field: "activity_at", Dir: dir}}, schema)
			got := make([]any, len(sorted))
			for index, row := range sorted {
				got[index] = row["activity_at"]
			}
			expected := append([]any(nil), want...)
			if dir == "desc" {
				for i, j := 0, len(expected)-1; i < j; i, j = i+1, j-1 {
					expected[i], expected[j] = expected[j], expected[i]
				}
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("trial %d %s:\ngot  %v\nwant %v", trial, dir, got, expected)
			}
		}
	}
}
