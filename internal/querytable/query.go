// Package querytable implements the query-table wire contract for Alexandria's
// map-backed datasets. Its types and limits mirror Pythia Software query-table
// v0.3.0. The upstream SQL compiler targets PostgreSQL; this executor keeps the
// same allowlist boundary while operating on SQLite-derived and in-memory rows.
package querytable

import (
	"encoding/json"
	"fmt"
)

const (
	MaxLimit        = 10_000
	MaxOffset       = 1_000_000
	MaxSelect       = 200
	MaxWhere        = 100
	MaxOrderBy      = 20
	MaxAggregations = 20
)

type WhereClause struct {
	Field   string `json:"field"`
	Op      string `json:"op"`
	Value   string `json:"value"`
	Negated bool   `json:"negated,omitempty"`
}

type WhereTerm struct {
	Field   string        `json:"field,omitempty"`
	Op      string        `json:"op,omitempty"`
	Value   string        `json:"value,omitempty"`
	Negated bool          `json:"negated,omitempty"`
	Any     []WhereClause `json:"any,omitempty"`
}

func (t WhereTerm) IsGroup() bool { return t.Any != nil }
func (t WhereTerm) Predicates() []WhereClause {
	if t.IsGroup() {
		return t.Any
	}
	return []WhereClause{{Field: t.Field, Op: t.Op, Value: t.Value, Negated: t.Negated}}
}

type OrderBy struct {
	Field   string        `json:"field"`
	Dir     string        `json:"dir"`
	Nulls   string        `json:"nulls,omitempty"`
	Extract *RegexExtract `json:"extract,omitempty"`
}

type RegexExtract struct {
	Regex string `json:"regex"`
}

type Aggregation struct {
	ID      string   `json:"id"`
	Op      string   `json:"op"`
	Field   string   `json:"field,omitempty"`
	GroupBy []string `json:"groupBy,omitempty"`
}

type Query struct {
	Select       []string      `json:"select"`
	Where        []WhereTerm   `json:"where"`
	OrderBy      []OrderBy     `json:"orderBy"`
	Limit        int           `json:"limit"`
	Offset       int           `json:"offset"`
	Aggregations []Aggregation `json:"aggregations,omitempty"`
}

func Decode(data []byte) (Query, error) {
	var query Query
	if err := json.Unmarshal(data, &query); err != nil {
		return query, fmt.Errorf("query json: %w", err)
	}
	if query.Limit == 0 {
		query.Limit = 100
	}
	if err := query.Validate(); err != nil {
		return Query{}, err
	}
	return query, nil
}

func (q Query) Validate() error {
	if q.Limit < 1 || q.Limit > MaxLimit {
		return fmt.Errorf("limit must be between 1 and %d", MaxLimit)
	}
	if q.Offset < 0 || q.Offset > MaxOffset {
		return fmt.Errorf("offset must be between 0 and %d", MaxOffset)
	}
	if len(q.Select) > MaxSelect || len(q.Where) > MaxWhere || len(q.OrderBy) > MaxOrderBy || len(q.Aggregations) > MaxAggregations {
		return fmt.Errorf("query exceeds structural limits")
	}
	literals := 0
	for _, term := range q.Where {
		predicates := term.Predicates()
		if len(predicates) == 0 {
			return fmt.Errorf("where group must contain a predicate")
		}
		for _, clause := range predicates {
			literals++
			if clause.Field == "" || len(clause.Field) > 256 || len(clause.Value) > 10_000 {
				return fmt.Errorf("invalid filter clause")
			}
		}
	}
	if literals > MaxWhere {
		return fmt.Errorf("where has %d clauses; maximum is %d", literals, MaxWhere)
	}
	return nil
}
