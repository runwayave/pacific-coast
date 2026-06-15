// Package schema provides pure functions that compute SQL identifiers and
// column lists from the DSL IR. These are the shared building blocks for
// both the codegen emitters (which produce Go source as strings) and the
// runtime server (which executes SQL directly).
//
// This package must not import internal/codegen/ — the dependency flows
// the other direction.
package schema

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// QualifiedTable returns the schema-qualified, double-quoted table name
// for an entity, e.g. `"atlantis"."consumer_account"`.
//
// Honors the `table "schema.table"` modifier when set; otherwise falls
// back to the computed `atlantis.<namespace>_<snake>` form.
func QualifiedTable(e *dsl.Entity) string {
	return QuoteIdent(EntitySchema(e)) + "." + QuoteIdent(EntityPhysicalTable(e))
}

// EntitySchema returns the schema where this entity's table lives.
// `table "schema.table"` overrides the default; bare `table "name"`
// (no schema prefix) lives in `public`; no override at all lives in
// `atlantis`.
func EntitySchema(e *dsl.Entity) string {
	if e.TableName != "" {
		if i := strings.IndexByte(e.TableName, '.'); i >= 0 {
			return e.TableName[:i]
		}
		return "public"
	}
	return "atlantis"
}

// EntityPhysicalTable returns the bare table name (the part inside the
// quotes after the schema dot). Without an override it is the computed
// flat name; with one it is the table portion of the override.
func EntityPhysicalTable(e *dsl.Entity) string {
	if e.TableName != "" {
		if i := strings.IndexByte(e.TableName, '.'); i >= 0 {
			return e.TableName[i+1:]
		}
		return e.TableName
	}
	return TableName(e)
}

// TableName maps an entity to its computed physical table name:
// `<namespace>_<snake_case_name>`.
func TableName(e *dsl.Entity) string {
	return e.Namespace + "_" + SnakeCase(e.Name)
}

// QuoteIdent wraps a SQL identifier in double quotes, escaping any
// embedded double quotes by doubling them.
func QuoteIdent(s string) string {
	if !strings.ContainsAny(s, `"\n`) {
		return `"` + s + `"`
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// FieldColumns returns the names of every field on the entity in
// declaration order — the SELECT list for a full-entity read.
func FieldColumns(e *dsl.Entity) []string {
	out := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		out[i] = f.Name
	}
	return out
}

// InsertColumns lists every column the INSERT statement carries values
// for. Identity and Serial columns are excluded because Postgres
// generates their values.
func InsertColumns(e *dsl.Entity) []string {
	var out []string
	for _, f := range e.Fields {
		if f.Identity || f.Serial {
			continue
		}
		out = append(out, f.Name)
	}
	return out
}

// SQLType maps a DSL field type to its Postgres type string.
func SQLType(t dsl.FieldType) string {
	if t.Array {
		inner := "text"
		if t.Elem != nil {
			inner = SQLType(*t.Elem)
		}
		return inner + "[]"
	}
	switch t.Name {
	case "smallint":
		return "SMALLINT"
	case "int":
		return "INTEGER"
	case "bigint":
		return "BIGINT"
	case "text":
		return "TEXT"
	case "varchar":
		return fmt.Sprintf("VARCHAR(%d)", t.Len)
	case "citext":
		return "CITEXT"
	case "boolean":
		return "BOOLEAN"
	case "timestamptz":
		return "TIMESTAMPTZ"
	case "date":
		return "DATE"
	case "interval":
		return "INTERVAL"
	case "uuid":
		return "UUID"
	case "bytea":
		return "BYTEA"
	case "jsonb":
		return "JSONB"
	case "vector":
		return fmt.Sprintf("vector(%d)", t.VecDim)
	case "numeric":
		if t.HasNumP {
			return fmt.Sprintf("NUMERIC(%d, %d)", t.NumP, t.NumS)
		}
		return "NUMERIC"
	}
	return strings.ToUpper(t.Name)
}

// QuoteAll wraps every string in double-quoted identifiers.
func QuoteAll(ids []string) []string {
	out := make([]string, len(ids))
	for i, s := range ids {
		out[i] = QuoteIdent(s)
	}
	return out
}

// DefaultExpr renders a Default in SQL form (with appropriate quoting).
func DefaultExpr(d dsl.Default) string {
	switch d.Kind {
	case dsl.DefaultIRString:
		return "'" + strings.ReplaceAll(d.Str, "'", "''") + "'"
	case dsl.DefaultIRInt:
		return fmt.Sprintf("%d", d.Int)
	case dsl.DefaultIRFloat:
		return strconv.FormatFloat(d.Float, 'g', -1, 64)
	case dsl.DefaultIRBool:
		if d.Bool {
			return "TRUE"
		}
		return "FALSE"
	case dsl.DefaultIRNow:
		return "now()"
	case dsl.DefaultIRRaw:
		return d.Str
	}
	return "NULL"
}

// SnakeCase converts UpperCamelCase to snake_case.
func SnakeCase(s string) string {
	var out []rune
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := rune(s[i-1])
			next := rune(0)
			if i+1 < len(s) {
				next = rune(s[i+1])
			}
			if (prev >= 'a' && prev <= 'z') ||
				(next >= 'a' && next <= 'z' && prev >= 'A' && prev <= 'Z') {
				out = append(out, '_')
			}
		}
		if r >= 'A' && r <= 'Z' {
			r = r - 'A' + 'a'
		}
		out = append(out, r)
	}
	return string(out)
}

// PKColumns returns the fields that form the entity's primary key in
// DSL declaration order. Returns nil if no PK is declared (should not
// happen in a well-formed IR).
func PKColumns(e *dsl.Entity) []*dsl.Field {
	if len(e.CompositePK) > 0 {
		out := make([]*dsl.Field, 0, len(e.CompositePK))
		for _, name := range e.CompositePK {
			f := e.FindField(name)
			if f != nil {
				out = append(out, f)
			}
		}
		return out
	}
	if pk := e.PrimaryField(); pk != nil {
		return []*dsl.Field{pk}
	}
	return nil
}

// IsPKColumn reports whether the named column is part of the entity's
// primary key.
func IsPKColumn(e *dsl.Entity, name string) bool {
	for _, f := range PKColumns(e) {
		if f.Name == name {
			return true
		}
	}
	return false
}

// IsEffectivelyNullable reports whether a field should be treated as
// nullable on the proto wire. A field is nullable when it is not NOT
// NULL, or when it has a declared DEFAULT (so the caller can omit it
// and let the server-side COALESCE fire the default).
func IsEffectivelyNullable(f *dsl.Field) bool {
	if f.Default != nil {
		return true
	}
	return !f.NotNull
}

// PredicateKindForField maps a DSL field type to the query.PredicateKind
// constant. Returns ("", false) when the type is not filterable.
func PredicateKindForField(t dsl.FieldType) (string, bool) {
	if t.Array {
		return "", false
	}
	switch t.Name {
	case "text", "varchar", "citext", "uuid":
		return "PredicateString", true
	case "numeric":
		return "PredicateNumeric", true
	case "int", "smallint":
		return "PredicateInt32", true
	case "bigint":
		return "PredicateInt64", true
	case "boolean":
		return "PredicateBool", true
	case "timestamptz", "date":
		return "PredicateTimestamp", true
	case "jsonb", "bytea":
		return "PredicateBytes", true
	}
	return "", false
}
