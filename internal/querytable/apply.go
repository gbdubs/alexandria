package querytable

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Result struct {
	Rows  []map[string]any `json:"rows"`
	Total int              `json:"total"`
}

func Apply(rows []map[string]any, query Query, schema Schema) (Result, error) {
	if err := schema.ValidateQuery(query); err != nil {
		return Result{}, err
	}
	filtered, err := filterRows(rows, query.Where, schema)
	if err != nil {
		return Result{}, err
	}
	total := len(filtered)
	if len(query.OrderBy) > 0 {
		filtered = sortRows(filtered, query.OrderBy, schema)
	}
	start := min(query.Offset, len(filtered))
	end := min(start+query.Limit, len(filtered))
	return Result{Rows: filtered[start:end], Total: total}, nil
}

// sortRows orders rows stably. Each row's sort values are extracted and
// parsed once up front; comparisons then only read the parsed keys.
func sortRows(rows []map[string]any, orderBy []OrderBy, schema Schema) []map[string]any {
	type keyed struct {
		row  map[string]any
		keys []sortKey
	}
	items := make([]keyed, len(rows))
	for index, row := range rows {
		keys := make([]sortKey, len(orderBy))
		for position, order := range orderBy {
			keys[position] = newSortKey(extractValue(row[order.Field], order.Extract), schema.Fields[order.Field].Kind)
		}
		items[index] = keyed{row, keys}
	}
	sort.SliceStable(items, func(i, j int) bool {
		for position, order := range orderBy {
			left, right := items[i].keys[position], items[j].keys[position]
			if left.null || right.null {
				if left.null && right.null {
					continue
				}
				nullsLast := order.Nulls != "first"
				if left.null {
					return !nullsLast
				}
				return nullsLast
			}
			comparison := left.compare(right, schema.Fields[order.Field].Kind)
			if comparison != 0 {
				if order.Dir == "desc" {
					return comparison > 0
				}
				return comparison < 0
			}
		}
		return false
	})
	sorted := make([]map[string]any, len(items))
	for index, item := range items {
		sorted[index] = item.row
	}
	return sorted
}

// sortKey is a value pre-parsed for compare: parsed values of the field's kind
// compare as numbers (booleans as 0/1) or times, anything else as text.
type sortKey struct {
	null, parsed bool
	number       float64
	time         time.Time
	text         string
}

func newSortKey(value any, kind FieldKind) sortKey {
	if nullish(value) {
		return sortKey{null: true}
	}
	key := sortKey{}
	if text, ok := value.(string); ok {
		key.text = text
	} else {
		key.text = fmt.Sprint(value)
	}
	switch kind {
	case Number:
		key.number, key.parsed = number(value)
	case Bool:
		var truth bool
		truth, key.parsed = boolean(value)
		if truth {
			key.number = 1
		}
	case Datetime:
		key.time, key.parsed = timeValue(value)
	}
	return key
}

func (left sortKey) compare(right sortKey, kind FieldKind) int {
	if kind == Datetime && left.parsed != right.parsed {
		return parsedFirst(left.parsed)
	}
	if !left.parsed || !right.parsed {
		return strings.Compare(left.text, right.text)
	}
	if kind == Datetime {
		return left.time.Compare(right.time)
	}
	if left.number < right.number {
		return -1
	}
	if left.number > right.number {
		return 1
	}
	return 0
}

func filterRows(rows []map[string]any, where []WhereTerm, schema Schema) ([]map[string]any, error) {
	filtered := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		matches := true
		for _, term := range where {
			termMatch := false
			for _, clause := range term.Predicates() {
				match, err := matchesClause(row, clause, schema.Fields[clause.Field])
				if err != nil {
					return nil, err
				}
				if match {
					termMatch = true
				}
			}
			if !termMatch {
				matches = false
				break
			}
		}
		if matches {
			filtered = append(filtered, row)
		}
	}
	return filtered, nil
}

func matchesClause(row map[string]any, clause WhereClause, field Field) (bool, error) {
	value := row[clause.Field]
	// A typed comparison has no usable value while its input is blank.
	if clause.Value == "" && clause.Op != "is_null" && clause.Op != "is_not_null" &&
		(clause.Op != "=" && clause.Op != "!=" || field.Kind == Number || field.Kind == Datetime || field.Kind == Bool) {
		return true, nil
	}
	base, err := matchesBase(value, clause, field)
	if err != nil {
		return false, fmt.Errorf("field %q: %w", clause.Field, err)
	}
	if !clause.Negated {
		return base, nil
	}
	return !nullish(value) && !base, nil
}

func matchesBase(value any, clause WhereClause, field Field) (bool, error) {
	kind := field.Kind
	switch clause.Op {
	case "is_null":
		return nullish(value), nil
	case "is_not_null":
		return !nullish(value), nil
	case "includes":
		// Array membership: a null or missing array contains nothing.
		arrayText := strings.ToLower
		if field.ArrayCaseSensitive {
			arrayText = func(text string) string { return text }
		}
		needle := arrayText(clause.Value)
		for _, item := range stringSlice(value) {
			if arrayText(item) == needle {
				return true, nil
			}
		}
		return false, nil
	case "matches_regex", "not_matches_regex":
		if nullish(value) {
			return false, nil
		}
		expression, err := regexp.Compile(clause.Value)
		if err != nil {
			return false, nil
		}
		matched := expression.MatchString(fmt.Sprint(value))
		if clause.Op == "not_matches_regex" {
			matched = !matched
		}
		return matched, nil
	}
	coerced, err := coerce(kind, clause.Value)
	if err != nil {
		return false, err
	}
	comparison := compare(value, coerced, kind)
	switch clause.Op {
	case "=":
		return comparison == 0, nil
	case "!=":
		return comparison != 0, nil
	case ">":
		return comparison > 0, nil
	case ">=":
		return comparison >= 0, nil
	case "<":
		return comparison < 0, nil
	case "<=":
		return comparison <= 0, nil
	case "contains":
		return strings.Contains(strings.ToLower(fmt.Sprint(value)), strings.ToLower(clause.Value)), nil
	case "starts_with":
		return strings.HasPrefix(strings.ToLower(fmt.Sprint(value)), strings.ToLower(clause.Value)), nil
	case "ends_with":
		return strings.HasSuffix(strings.ToLower(fmt.Sprint(value)), strings.ToLower(clause.Value)), nil
	}
	return false, fmt.Errorf("unknown op %q", clause.Op)
}

func coerce(kind FieldKind, raw string) (any, error) {
	switch kind {
	case Number:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("not a number: %q", raw)
		}
		return value, nil
	case Bool:
		switch strings.ToLower(raw) {
		case "true", "1", "t":
			return true, nil
		case "false", "0", "f":
			return false, nil
		default:
			return nil, fmt.Errorf("not a bool: %q", raw)
		}
	case Datetime:
		if value, ok := parseTime(raw); ok {
			return value, nil
		}
		return nil, fmt.Errorf("not a datetime: %q", raw)
	}
	return raw, nil
}

func compare(left, right any, kind FieldKind) int {
	if kind == Number {
		l, lok := number(left)
		r, rok := number(right)
		if lok && rok {
			if l < r {
				return -1
			}
			if l > r {
				return 1
			}
			return 0
		}
	}
	if kind == Bool {
		l, lok := boolean(left)
		r, rok := boolean(right)
		if lok && rok {
			if l == r {
				return 0
			}
			if !l {
				return -1
			}
			return 1
		}
	}
	if kind == Datetime {
		l, lok := timeValue(left)
		r, rok := timeValue(right)
		if lok && rok {
			if l.Before(r) {
				return -1
			}
			if l.After(r) {
				return 1
			}
			return 0
		}
		// Mixing time and text comparison is not a consistent ordering, so a
		// sort would depend on input order. Rank parsed times before the rest.
		if lok != rok {
			return parsedFirst(lok)
		}
	}
	return strings.Compare(fmt.Sprint(left), fmt.Sprint(right))
}

func number(value any) (float64, bool) {
	switch item := value.(type) {
	case int:
		return float64(item), true
	case int64:
		return float64(item), true
	case float64:
		return item, !math.IsNaN(item)
	case jsonNumber:
		result, err := strconv.ParseFloat(string(item), 64)
		return result, err == nil
	case string:
		result, err := strconv.ParseFloat(item, 64)
		return result, err == nil
	}
	return 0, false
}

type jsonNumber string

func boolean(value any) (bool, bool) {
	switch item := value.(type) {
	case bool:
		return item, true
	case int64:
		return item != 0, true
	case float64:
		return item != 0, true
	case string:
		switch strings.ToLower(item) {
		case "true", "1", "t":
			return true, true
		case "false", "0", "f":
			return false, true
		}
	}
	return false, false
}

func nullish(value any) bool {
	if value == nil {
		return true
	}
	switch item := value.(type) {
	case string:
		return item == ""
	case []string:
		return len(item) == 0
	case []any:
		return len(item) == 0
	}
	return false
}

func stringSlice(value any) []string {
	switch items := value.(type) {
	case []string:
		return items
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, fmt.Sprint(item))
		}
		return out
	}
	return nil
}

func parsedFirst(leftParsed bool) int {
	if leftParsed {
		return -1
	}
	return 1
}

// parseTime accepts RFC 3339 and SQLite's zone-less "2006-01-02 15:04:05"
// (as UTC). Go accepts a fractional second after the seconds field even when
// the layout omits it.
func parseTime(raw string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05Z07:00", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if result, err := time.Parse(layout, raw); err == nil {
			return result, true
		}
	}
	return time.Time{}, false
}

func timeValue(value any) (time.Time, bool) {
	if item, ok := value.(time.Time); ok {
		return item, true
	}
	return parseTime(fmt.Sprint(value))
}

func extractValue(value any, extraction *RegexExtract) any {
	if extraction == nil {
		return value
	}
	if nullish(value) {
		return nil
	}
	expression, err := regexp.Compile(extraction.Regex)
	if err != nil {
		return nil
	}
	match := expression.FindStringSubmatch(fmt.Sprint(value))
	if len(match) == 0 {
		return nil
	}
	if len(match) > 1 {
		return match[1]
	}
	return match[0]
}

type DistinctResult struct {
	Values  []string `json:"values"`
	HasMore bool     `json:"hasMore"`
	HasNull bool     `json:"hasNull"`
}

func Distinct(rows []map[string]any, fieldName, search string, limit int, schema Schema) (DistinctResult, error) {
	field, ok := schema.Fields[fieldName]
	if !ok || !field.Filterable {
		return DistinctResult{}, fmt.Errorf("unknown filter field %q", fieldName)
	}
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	values, seen, hasNull := []string{}, map[string]bool{}, false
	needle := strings.ToLower(search)
	for _, row := range rows {
		value := row[fieldName]
		if nullish(value) {
			hasNull = true
			continue
		}
		candidates := []string{fmt.Sprint(value)}
		if field.Kind == TextArray {
			candidates = stringSlice(value)
		}
		for _, candidate := range candidates {
			if needle != "" && !strings.Contains(strings.ToLower(candidate), needle) {
				continue
			}
			if !seen[candidate] {
				seen[candidate] = true
				values = append(values, candidate)
			}
		}
	}
	sort.Strings(values)
	hasMore := len(values) > limit
	if hasMore {
		values = values[:limit]
	}
	return DistinctResult{Values: values, HasMore: hasMore, HasNull: hasNull}, nil
}

// MaxFieldStats bounds the fields one field-stats request may name.
const MaxFieldStats = 100

// FieldStat is one field's dataset-wide statistics for the column picker.
// Min and Max are reported for number and datetime fields.
type FieldStat struct {
	Distinct int `json:"distinct"`
	Min      any `json:"min,omitempty"`
	Max      any `json:"max,omitempty"`
}

// StatFields validates a field-stats request and returns the backend fields
// to compute. Unknown fields are rejected; known fields without a server
// binding are skipped, since the server has no values for them.
func StatFields(names []string, schema Schema) ([]string, error) {
	if len(names) > MaxFieldStats {
		return nil, fmt.Errorf("at most %d fields per field-stats request", MaxFieldStats)
	}
	fields, seen := []string{}, map[string]bool{}
	for _, name := range names {
		field, ok := schema.Fields[name]
		if !ok {
			return nil, fmt.Errorf("unknown field %q", name)
		}
		if field.Backend && !seen[name] {
			seen[name] = true
			fields = append(fields, name)
		}
	}
	return fields, nil
}

// FieldStats counts each field's distinct non-null values across rows, and
// finds the range of number and datetime fields.
func FieldStats(rows []map[string]any, names []string, schema Schema) (map[string]FieldStat, error) {
	fields, err := StatFields(names, schema)
	if err != nil {
		return nil, err
	}
	result := make(map[string]FieldStat, len(fields))
	for _, name := range fields {
		kind := schema.Fields[name].Kind
		seen := map[string]bool{}
		var low, high any
		for _, row := range rows {
			value := row[name]
			if nullish(value) {
				continue
			}
			seen[fmt.Sprint(value)] = true
			if kind != Number && kind != Datetime {
				continue
			}
			if kind == Number {
				if _, ok := number(value); !ok {
					continue
				}
			} else if _, ok := timeValue(value); !ok {
				continue
			}
			if low == nil || compare(value, low, kind) < 0 {
				low = value
			}
			if high == nil || compare(value, high, kind) > 0 {
				high = value
			}
		}
		result[name] = FieldStat{Distinct: len(seen), Min: low, Max: high}
	}
	return result, nil
}

type AggregationRequest struct {
	Where        []WhereTerm   `json:"where"`
	Aggregations []Aggregation `json:"aggregations"`
}

type Bucket struct {
	Keys  []any `json:"keys"`
	Value any   `json:"value"`
	Count int   `json:"count"`
}
type Metric struct {
	ID      string   `json:"id"`
	Buckets []Bucket `json:"buckets"`
}
type AggregationResult struct {
	Metrics []Metric `json:"metrics"`
}

func Aggregate(rows []map[string]any, request AggregationRequest, schema Schema) (AggregationResult, error) {
	query := Query{Where: request.Where, Limit: 1}
	if err := schema.ValidateQuery(query); err != nil {
		return AggregationResult{}, err
	}
	filtered, err := filterRows(rows, request.Where, schema)
	if err != nil {
		return AggregationResult{}, err
	}
	result := AggregationResult{Metrics: []Metric{}}
	for _, aggregation := range request.Aggregations {
		if aggregation.ID == "" {
			return result, fmt.Errorf("aggregation id is required")
		}
		if len(aggregation.GroupBy) > 20 {
			return result, fmt.Errorf("aggregation %q has too many group fields", aggregation.ID)
		}
		if aggregation.Op != "count" && aggregation.Field == "" {
			return result, fmt.Errorf("aggregation %q requires a field", aggregation.ID)
		}
		if aggregation.Field != "" {
			measure, ok := schema.Fields[aggregation.Field]
			if !ok {
				return result, fmt.Errorf("unknown measure field %q", aggregation.Field)
			}
			if !aggregationAllowed(measure.Kind, aggregation.Op) {
				return result, fmt.Errorf("aggregate op %q not allowed on %s field", aggregation.Op, measure.Kind)
			}
		} else if aggregation.Op != "count" {
			return result, fmt.Errorf("unknown aggregate op %q", aggregation.Op)
		}
		for _, field := range aggregation.GroupBy {
			if _, ok := schema.Fields[field]; !ok {
				return result, fmt.Errorf("unknown group field %q", field)
			}
		}
		groups := map[string]*Bucket{}
		values := map[string][]any{}
		measureKind := Text
		if aggregation.Field != "" {
			measureKind = schema.Fields[aggregation.Field].Kind
		}
		for _, row := range filtered {
			keys := make([]any, len(aggregation.GroupBy))
			parts := make([]string, len(keys))
			for index, field := range aggregation.GroupBy {
				keys[index] = row[field]
				parts[index] = fmt.Sprint(row[field])
			}
			key := strings.Join(parts, "\x00")
			if groups[key] == nil {
				groups[key] = &Bucket{Keys: keys}
			}
			groups[key].Count++
			if aggregation.Field != "" && !nullish(row[aggregation.Field]) {
				values[key] = append(values[key], row[aggregation.Field])
			}
		}
		if len(filtered) == 0 && len(aggregation.GroupBy) == 0 {
			groups[""] = &Bucket{Keys: []any{}}
		}
		buckets := make([]Bucket, 0, len(groups))
		for key, bucket := range groups {
			bucket.Value = aggregateValue(aggregation.Op, values[key], bucket.Count, aggregation.Field != "", measureKind)
			buckets = append(buckets, *bucket)
		}
		sort.SliceStable(buckets, func(i, j int) bool {
			li, lok := number(buckets[i].Value)
			rj, rok := number(buckets[j].Value)
			if lok && rok && li != rj {
				return li > rj
			}
			return fmt.Sprint(buckets[i].Keys) < fmt.Sprint(buckets[j].Keys)
		})
		result.Metrics = append(result.Metrics, Metric{ID: aggregation.ID, Buckets: buckets})
	}
	return result, nil
}

func aggregateValue(op string, values []any, rowCount int, hasMeasure bool, kind FieldKind) any {
	switch op {
	case "count":
		if !hasMeasure {
			return rowCount
		}
		return len(values)
	case "count_distinct":
		seen := map[string]bool{}
		for _, value := range values {
			seen[fmt.Sprint(value)] = true
		}
		return len(seen)
	case "sum", "avg":
		total, count := 0.0, 0
		for _, value := range values {
			if number, ok := number(value); ok {
				total += number
				count++
			}
		}
		if count == 0 {
			return nil
		}
		if op == "avg" {
			return total / float64(count)
		}
		return total
	case "min", "max":
		if len(values) == 0 {
			return nil
		}
		best := values[0]
		for _, value := range values[1:] {
			comparison := compare(value, best, kind)
			if (op == "min" && comparison < 0) || (op == "max" && comparison > 0) {
				best = value
			}
		}
		return best
	}
	return nil
}

func aggregationAllowed(kind FieldKind, op string) bool {
	switch kind {
	case Number:
		return op == "count" || op == "count_distinct" || op == "sum" || op == "avg" || op == "min" || op == "max"
	case Datetime, Enum, Text:
		return op == "count" || op == "count_distinct" || op == "min" || op == "max"
	case Bool:
		return op == "count" || op == "count_distinct"
	case TextArray:
		return op == "count"
	}
	return false
}
