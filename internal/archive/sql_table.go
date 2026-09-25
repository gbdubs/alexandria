package archive

import (
	"context"
	"database/sql/driver"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"alexandria/internal/querytable"
	"modernc.org/sqlite"
)

// sqlDataset runs query-table requests in SQLite for datasets too large to
// load into memory (the tool ledger holds a row per tool call). Every field is
// bound to an allowlisted SQL expression; request values are always bound as
// parameters. Filter semantics follow querytable.Apply.
type sqlDataset struct {
	from    string            // FROM and JOIN clauses
	base    string            // always-applied condition, or ""
	columns map[string]string // field name -> SQL expression
}

// maxSQLBuckets bounds a metric's groups; the UI shows the largest first.
const maxSQLBuckets = 2000

var registerRegexp sync.Once

// registerSQLiteRegexp provides REGEXP for matches_regex filters.
func registerSQLiteRegexp() {
	registerRegexp.Do(func() {
		var cache sync.Map
		_ = sqlite.RegisterDeterministicScalarFunction("regexp", 2, func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			pattern, _ := args[0].(string)
			if args[1] == nil {
				return int64(0), nil
			}
			var expression *regexp.Regexp
			if cached, ok := cache.Load(pattern); ok {
				expression, _ = cached.(*regexp.Regexp)
			} else {
				compiled, err := regexp.Compile(pattern)
				if err != nil {
					cache.Store(pattern, (*regexp.Regexp)(nil))
					return int64(0), nil
				}
				expression = compiled
				cache.Store(pattern, compiled)
			}
			if expression == nil {
				return int64(0), nil
			}
			var text string
			switch value := args[1].(type) {
			case string:
				text = value
			case []byte:
				text = string(value)
			default:
				text = fmt.Sprint(value)
			}
			if expression.MatchString(text) {
				return int64(1), nil
			}
			return int64(0), nil
		})
	})
}

func (d sqlDataset) expr(field string) (string, error) {
	expression, ok := d.columns[field]
	if !ok {
		return "", fmt.Errorf("field %q has no SQL binding", field)
	}
	return expression, nil
}

func likePattern(value, prefix, suffix string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(value))
	return prefix + escaped + suffix
}

func (d sqlDataset) clause(clause querytable.WhereClause, field querytable.Field) (string, []any, error) {
	expression, err := d.expr(clause.Field)
	if err != nil {
		return "", nil, err
	}
	// Match querytable's pending-filter behavior for blank typed comparisons.
	if clause.Value == "" && clause.Op != "is_null" && clause.Op != "is_not_null" &&
		(clause.Op != "=" && clause.Op != "!=" || field.Kind == querytable.Number || field.Kind == querytable.Datetime || field.Kind == querytable.Bool) {
		return "1", nil, nil
	}
	isNull := fmt.Sprintf("(%s IS NULL OR %s = '')", expression, expression)
	if field.Kind == querytable.TextArray {
		// Text arrays are stored as JSON arrays; an empty one is null.
		isNull = fmt.Sprintf("(%s IS NULL OR %s IN ('','[]'))", expression, expression)
	}
	var base string
	args := []any{}
	switch clause.Op {
	case "includes":
		// Membership in a JSON text array; a null, empty, or malformed array
		// contains nothing. Case-insensitive unless the field opts out.
		elements := fmt.Sprintf("json_each(CASE WHEN json_valid(%s) THEN %s END)", expression, expression)
		if field.ArrayCaseSensitive {
			base = fmt.Sprintf("EXISTS (SELECT 1 FROM %s WHERE CAST(value AS TEXT) = ?)", elements)
			args = append(args, clause.Value)
		} else {
			base = fmt.Sprintf("EXISTS (SELECT 1 FROM %s WHERE lower(CAST(value AS TEXT)) = ?)", elements)
			args = append(args, strings.ToLower(clause.Value))
		}
	case "is_null":
		base = isNull
	case "is_not_null":
		base = "NOT " + isNull
	case "matches_regex", "not_matches_regex":
		registerSQLiteRegexp()
		base = fmt.Sprintf("(%s IS NOT NULL AND %s REGEXP ?)", expression, expression)
		if clause.Op == "not_matches_regex" {
			base = fmt.Sprintf("(%s IS NOT NULL AND NOT (%s REGEXP ?))", expression, expression)
		}
		args = append(args, clause.Value)
	case "contains":
		base = fmt.Sprintf("lower(COALESCE(%s,'')) LIKE ? ESCAPE '\\'", expression)
		args = append(args, likePattern(clause.Value, "%", "%"))
	case "starts_with":
		base = fmt.Sprintf("lower(COALESCE(%s,'')) LIKE ? ESCAPE '\\'", expression)
		args = append(args, likePattern(clause.Value, "", "%"))
	case "ends_with":
		base = fmt.Sprintf("lower(COALESCE(%s,'')) LIKE ? ESCAPE '\\'", expression)
		args = append(args, likePattern(clause.Value, "%", ""))
	case "=", "!=", ">", ">=", "<", "<=":
		var value any = clause.Value
		switch field.Kind {
		case querytable.Number:
			var number float64
			if _, err := fmt.Sscan(clause.Value, &number); err != nil {
				return "", nil, fmt.Errorf("field %q: not a number: %q", clause.Field, clause.Value)
			}
			value = number
		case querytable.Bool:
			switch strings.ToLower(clause.Value) {
			case "true", "1", "t":
				value = 1
			case "false", "0", "f":
				value = 0
			default:
				return "", nil, fmt.Errorf("field %q: not a bool: %q", clause.Field, clause.Value)
			}
		case querytable.Datetime:
			parsed, ok := parseTime(clause.Value)
			if !ok {
				return "", nil, fmt.Errorf("field %q: not a datetime: %q", clause.Field, clause.Value)
			}
			value = formatTime(parsed)
		}
		if clause.Op == "!=" {
			base = fmt.Sprintf("(%s IS NULL OR %s <> ?)", expression, expression)
		} else {
			base = fmt.Sprintf("%s %s ?", expression, clause.Op)
		}
		args = append(args, value)
	default:
		return "", nil, fmt.Errorf("unknown op %q", clause.Op)
	}
	if clause.Negated {
		return fmt.Sprintf("(NOT %s AND NOT (%s))", isNull, base), args, nil
	}
	return base, args, nil
}

func (d sqlDataset) where(terms []querytable.WhereTerm, schema querytable.Schema) (string, []any, error) {
	parts := []string{}
	args := []any{}
	if d.base != "" {
		parts = append(parts, d.base)
	}
	for _, term := range terms {
		alternatives := []string{}
		for _, clause := range term.Predicates() {
			sqlText, clauseArgs, err := d.clause(clause, schema.Fields[clause.Field])
			if err != nil {
				return "", nil, err
			}
			alternatives = append(alternatives, sqlText)
			args = append(args, clauseArgs...)
		}
		parts = append(parts, "("+strings.Join(alternatives, " OR ")+")")
	}
	if len(parts) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(parts, " AND "), args, nil
}

func (d sqlDataset) selectList() string {
	names := make([]string, 0, len(d.columns))
	for name := range d.columns {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for index, name := range names {
		parts[index] = d.columns[name] + ` AS "` + name + `"`
	}
	return strings.Join(parts, ",")
}

func (d sqlDataset) Rows(ctx context.Context, q queryer, query querytable.Query, schema querytable.Schema) (querytable.Result, error) {
	if err := schema.ValidateQuery(query); err != nil {
		return querytable.Result{}, err
	}
	where, args, err := d.where(query.Where, schema)
	if err != nil {
		return querytable.Result{}, err
	}
	var total int
	countRows, err := queryMapsContext(ctx, q, "SELECT COUNT(*) n "+d.from+where, args...)
	if err != nil {
		return querytable.Result{}, err
	}
	if len(countRows) == 1 {
		total = int(integer(countRows[0]["n"]))
	}
	orders := []string{}
	for _, order := range query.OrderBy {
		if order.Extract != nil {
			return querytable.Result{}, fmt.Errorf("sorting by an extracted value is not supported for this table")
		}
		expression, err := d.expr(order.Field)
		if err != nil {
			return querytable.Result{}, err
		}
		nulls := "LAST"
		if order.Nulls == "first" {
			nulls = "FIRST"
		}
		direction := "ASC"
		if order.Dir == "desc" {
			direction = "DESC"
		}
		// Numbers and times are never '', so they sort on the bare column,
		// which lets SQLite walk an index for the default newest-first page.
		if kind := schema.Fields[order.Field].Kind; kind != querytable.Number && kind != querytable.Datetime && kind != querytable.Bool {
			expression = "NULLIF(" + expression + ",'')"
		}
		orders = append(orders, fmt.Sprintf("%s %s NULLS %s", expression, direction, nulls))
	}
	if id, ok := d.columns[schema.IDField]; ok {
		orders = append(orders, id+" ASC")
	}
	statement := "SELECT " + d.selectList() + " " + d.from + where + " ORDER BY " + strings.Join(orders, ",") + " LIMIT ? OFFSET ?"
	rows, err := queryMapsContext(ctx, q, statement, append(args, query.Limit, query.Offset)...)
	if err != nil {
		return querytable.Result{}, err
	}
	return querytable.Result{Rows: rows, Total: total}, nil
}

func (d sqlDataset) Distinct(ctx context.Context, q queryer, fieldName, search string, limit int, schema querytable.Schema) (querytable.DistinctResult, error) {
	field, ok := schema.Fields[fieldName]
	if !ok || !field.Filterable {
		return querytable.DistinctResult{}, fmt.Errorf("unknown filter field %q", fieldName)
	}
	expression, err := d.expr(fieldName)
	if err != nil {
		return querytable.DistinctResult{}, err
	}
	if limit < 1 {
		limit = 50
	}
	limit = min(limit, 200)
	where, args, err := d.where(nil, schema)
	if err != nil {
		return querytable.DistinctResult{}, err
	}
	condition := " WHERE "
	if where != "" {
		condition = where + " AND "
	}
	filter := ""
	filterArgs := append([]any{}, args...)
	if search != "" {
		filter = fmt.Sprintf(" AND lower(CAST(%s AS TEXT)) LIKE ? ESCAPE '\\'", expression)
		filterArgs = append(filterArgs, likePattern(search, "%", "%"))
	}
	values, err := queryMapsContext(ctx, q, fmt.Sprintf("SELECT DISTINCT CAST(%s AS TEXT) value %s%s%s IS NOT NULL AND %s <> ''%s ORDER BY 1 LIMIT ?",
		expression, d.from, condition, expression, expression, filter), append(filterArgs, limit+1)...)
	if err != nil {
		return querytable.DistinctResult{}, err
	}
	result := querytable.DistinctResult{Values: []string{}}
	for _, row := range values {
		result.Values = append(result.Values, firstString(row["value"]))
	}
	if len(result.Values) > limit {
		result.Values, result.HasMore = result.Values[:limit], true
	}
	nulls, err := queryMapsContext(ctx, q, fmt.Sprintf("SELECT EXISTS(SELECT 1 %s%s(%s IS NULL OR %s = '')) has_null", d.from, condition, expression, expression), args...)
	if err == nil && len(nulls) == 1 {
		result.HasNull = integer(nulls[0]["has_null"]) != 0
	}
	return result, nil
}

func (d sqlDataset) Aggregate(ctx context.Context, q queryer, request querytable.AggregationRequest, schema querytable.Schema) (querytable.AggregationResult, error) {
	if err := schema.ValidateQuery(querytable.Query{Where: request.Where, Limit: 1}); err != nil {
		return querytable.AggregationResult{}, err
	}
	where, args, err := d.where(request.Where, schema)
	if err != nil {
		return querytable.AggregationResult{}, err
	}
	result := querytable.AggregationResult{Metrics: []querytable.Metric{}}
	for _, aggregation := range request.Aggregations {
		if aggregation.ID == "" {
			return result, fmt.Errorf("aggregation id is required")
		}
		if len(aggregation.GroupBy) > 20 {
			return result, fmt.Errorf("aggregation %q has too many group fields", aggregation.ID)
		}
		measure := ""
		if aggregation.Field != "" {
			field, ok := schema.Fields[aggregation.Field]
			if !ok {
				return result, fmt.Errorf("unknown measure field %q", aggregation.Field)
			}
			expression, err := d.expr(aggregation.Field)
			if err != nil {
				return result, err
			}
			nonEmpty := fmt.Sprintf("NULLIF(%s,'')", expression)
			switch aggregation.Op {
			case "count":
				measure = "COUNT(" + nonEmpty + ")"
			case "count_distinct":
				measure = "COUNT(DISTINCT " + nonEmpty + ")"
			case "sum":
				measure = "SUM(" + expression + ")"
			case "avg":
				measure = "AVG(" + expression + ")"
			case "min":
				measure = "MIN(" + nonEmpty + ")"
			case "max":
				measure = "MAX(" + nonEmpty + ")"
			default:
				return result, fmt.Errorf("unknown aggregate op %q", aggregation.Op)
			}
			if field.Kind == querytable.TextArray && aggregation.Op != "count" {
				return result, fmt.Errorf("aggregate op %q not allowed on %s field", aggregation.Op, field.Kind)
			}
		} else if aggregation.Op == "count" {
			measure = "COUNT(*)"
		} else {
			return result, fmt.Errorf("aggregation %q requires a field", aggregation.ID)
		}
		keys := []string{}
		for index, field := range aggregation.GroupBy {
			if _, ok := schema.Fields[field]; !ok {
				return result, fmt.Errorf("unknown group field %q", field)
			}
			expression, err := d.expr(field)
			if err != nil {
				return result, err
			}
			keys = append(keys, fmt.Sprintf("%s AS k%d", expression, index))
		}
		selectKeys := ""
		groupBy := ""
		if len(keys) > 0 {
			selectKeys = strings.Join(keys, ",") + ","
			positions := make([]string, len(keys))
			for index := range keys {
				positions[index] = fmt.Sprint(index + 1)
			}
			groupBy = " GROUP BY " + strings.Join(positions, ",")
		}
		statement := fmt.Sprintf("SELECT %s%s AS value,COUNT(*) AS n %s%s%s ORDER BY value DESC LIMIT %d", selectKeys, measure, d.from, where, groupBy, maxSQLBuckets)
		rows, err := queryMapsContext(ctx, q, statement, args...)
		if err != nil {
			return result, err
		}
		buckets := make([]querytable.Bucket, 0, len(rows))
		for _, row := range rows {
			keyValues := make([]any, len(aggregation.GroupBy))
			for index := range keyValues {
				keyValues[index] = row[fmt.Sprintf("k%d", index)]
			}
			value := row["value"]
			if (aggregation.Op == "sum" || aggregation.Op == "avg") && value != nil {
				if number, ok := number(value); ok {
					value = number
				}
			}
			buckets = append(buckets, querytable.Bucket{Keys: keyValues, Value: value, Count: int(integer(row["n"]))})
		}
		if len(aggregation.GroupBy) > 0 {
			// Drop the empty group SQLite returns for a filter matching nothing.
			kept := buckets[:0]
			for _, bucket := range buckets {
				if bucket.Count > 0 {
					kept = append(kept, bucket)
				}
			}
			buckets = kept
		}
		sort.SliceStable(buckets, func(i, j int) bool {
			left, leftOK := number(buckets[i].Value)
			right, rightOK := number(buckets[j].Value)
			if leftOK && rightOK && left != right {
				return left > right
			}
			return fmt.Sprint(buckets[i].Keys) < fmt.Sprint(buckets[j].Keys)
		})
		result.Metrics = append(result.Metrics, querytable.Metric{ID: aggregation.ID, Buckets: buckets})
	}
	return result, nil
}

// FieldStats reports each field's distinct non-null values, and the range of
// number and datetime fields, over the whole dataset in one scan.
func (d sqlDataset) FieldStats(ctx context.Context, q queryer, names []string, schema querytable.Schema) (map[string]querytable.FieldStat, error) {
	requested, err := querytable.StatFields(names, schema)
	if err != nil {
		return nil, err
	}
	fields, measures := []string{}, []string{}
	for _, name := range requested {
		expression, ok := d.columns[name]
		if !ok {
			continue // priced or derived after paging; no SQL values
		}
		index := len(fields)
		fields = append(fields, name)
		measures = append(measures, fmt.Sprintf("COUNT(DISTINCT NULLIF(%s,'')) AS d%d", expression, index))
		if kind := schema.Fields[name].Kind; kind == querytable.Number || kind == querytable.Datetime {
			measures = append(measures, fmt.Sprintf("MIN(NULLIF(%s,'')) AS lo%d,MAX(NULLIF(%s,'')) AS hi%d", expression, index, expression, index))
		}
	}
	result := make(map[string]querytable.FieldStat, len(fields))
	if len(fields) == 0 {
		return result, nil
	}
	where, args, err := d.where(nil, schema)
	if err != nil {
		return nil, err
	}
	rows, err := queryMapsContext(ctx, q, "SELECT "+strings.Join(measures, ",")+" "+d.from+where, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return result, nil
	}
	for index, name := range fields {
		row := rows[0]
		result[name] = querytable.FieldStat{Distinct: int(integer(row[fmt.Sprintf("d%d", index)])),
			Min: row[fmt.Sprintf("lo%d", index)], Max: row[fmt.Sprintf("hi%d", index)]}
	}
	return result, nil
}
