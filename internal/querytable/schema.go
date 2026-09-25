package querytable

import (
	"encoding/json"
	"fmt"
	"slices"
)

type FieldKind string

const (
	Text      FieldKind = "text"
	Number    FieldKind = "number"
	Enum      FieldKind = "enum"
	Datetime  FieldKind = "datetime"
	Bool      FieldKind = "bool"
	TextArray FieldKind = "textarray"
)

type Field struct {
	Name       string
	Kind       FieldKind
	FilterOps  []string
	Filterable bool
	Sortable   bool
	// Backend fields have a server binding; derived fields are client-only.
	Backend bool
	// ArrayCaseSensitive makes textarray element matching exact instead of
	// case-insensitive (schema filter.arrayCaseSensitive).
	ArrayCaseSensitive bool
}

type Schema struct {
	Name    string
	IDField string
	Fields  map[string]Field
}

type schemaDocument struct {
	Name    string `json:"name"`
	IDField string `json:"idField"`
	Fields  []struct {
		Name   string    `json:"name"`
		Type   FieldKind `json:"type"`
		Source any       `json:"source"`
		Filter *struct {
			Enabled            *bool    `json:"enabled"`
			Ops                []string `json:"ops"`
			ArrayCaseSensitive bool     `json:"arrayCaseSensitive"`
		} `json:"filter"`
		Sort *struct {
			Enabled *bool `json:"enabled"`
		} `json:"sort"`
		Bindings map[string]any `json:"bindings"`
	} `json:"fields"`
}

func LoadSchema(data []byte) (Schema, error) {
	var doc schemaDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return Schema{}, fmt.Errorf("schema json: %w", err)
	}
	if doc.Name == "" || doc.IDField == "" || len(doc.Fields) == 0 {
		return Schema{}, fmt.Errorf("schema requires name, idField, and fields")
	}
	schema := Schema{Name: doc.Name, IDField: doc.IDField, Fields: map[string]Field{}}
	for _, raw := range doc.Fields {
		if raw.Name == "" || !validKind(raw.Type) {
			return Schema{}, fmt.Errorf("schema field %q has invalid type %q", raw.Name, raw.Type)
		}
		backend := raw.Source != "derived" && len(raw.Bindings) > 0
		filterable, sortable := true, true
		if raw.Filter != nil && raw.Filter.Enabled != nil {
			filterable = *raw.Filter.Enabled
		}
		if raw.Sort != nil && raw.Sort.Enabled != nil {
			sortable = *raw.Sort.Enabled
		}
		if !backend {
			filterable, sortable = false, false
		}
		var ops []string
		if raw.Filter != nil && raw.Filter.Ops != nil {
			ops = append([]string{}, raw.Filter.Ops...)
		}
		schema.Fields[raw.Name] = Field{Name: raw.Name, Kind: raw.Type, FilterOps: ops, Filterable: filterable, Sortable: sortable, Backend: backend,
			ArrayCaseSensitive: raw.Filter != nil && raw.Filter.ArrayCaseSensitive}
	}
	if _, ok := schema.Fields[schema.IDField]; !ok {
		return Schema{}, fmt.Errorf("schema idField %q is not defined", schema.IDField)
	}
	return schema, nil
}

func validKind(kind FieldKind) bool {
	switch kind {
	case Text, Number, Enum, Datetime, Bool, TextArray:
		return true
	}
	return false
}

func (s Schema) ValidateQuery(query Query) error {
	if err := query.Validate(); err != nil {
		return err
	}
	for _, name := range query.Select {
		if _, ok := s.Fields[name]; !ok {
			return fmt.Errorf("unknown select field %q", name)
		}
	}
	for _, term := range query.Where {
		for _, clause := range term.Predicates() {
			field, ok := s.Fields[clause.Field]
			if !ok {
				return fmt.Errorf("unknown filter field %q", clause.Field)
			}
			if !field.Filterable {
				return fmt.Errorf("field %q is not filterable", clause.Field)
			}
			if !opAllowed(field, clause.Op) {
				return fmt.Errorf("field %q: op %q is not enabled", clause.Field, clause.Op)
			}
		}
	}
	for _, order := range query.OrderBy {
		field, ok := s.Fields[order.Field]
		if !ok {
			return fmt.Errorf("unknown sort field %q", order.Field)
		}
		if !field.Sortable {
			return fmt.Errorf("field %q is not sortable", order.Field)
		}
		if order.Dir != "asc" && order.Dir != "desc" {
			return fmt.Errorf("unknown sort direction %q", order.Dir)
		}
		if order.Nulls != "" && order.Nulls != "first" && order.Nulls != "last" {
			return fmt.Errorf("unknown null position %q", order.Nulls)
		}
	}
	return nil
}

// opAllowed reports whether op is enabled on field. An explicit filter.ops
// list narrows the type's default matrix but cannot widen it, as in upstream's
// Go backend: matching only implements the matrix's ops for each kind.
func opAllowed(field Field, op string) bool {
	if field.FilterOps != nil && !slices.Contains(field.FilterOps, op) {
		return false
	}
	if op == "is_null" || op == "is_not_null" {
		return true
	}
	switch field.Kind {
	case Text:
		return op == "=" || op == "!=" || op == "contains" || op == "starts_with" || op == "ends_with" || op == "matches_regex" || op == "not_matches_regex"
	case Enum:
		return op == "=" || op == "!="
	case Number, Datetime:
		return op == "=" || op == "!=" || op == ">" || op == ">=" || op == "<" || op == "<="
	case Bool:
		return op == "=" || op == "!="
	case TextArray:
		return op == "includes"
	}
	return false
}
