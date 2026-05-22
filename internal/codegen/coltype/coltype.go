// Package coltype maps a DSL column type to its emitted Go, proto,
// scan, and bind representations. Functions are pure: same DSL inputs
// → same string outputs, which keeps codegen deterministic and makes
// table-driven tests an exhaustive safety net for every DSL type.
package coltype

import (
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// GoType maps a DSL type to its Go in-memory representation.
//
// Nullable scalars surface as `*T` so absent and zero-value stay
// distinguishable. Arrays and naturally nullable shapes (`[]byte`,
// `[]float32`) skip the pointer wrap — their nil zero already encodes
// absence, and double-pointing would force a nil check on the outer
// pointer too.
//
// notNull=true forces the unwrapped form regardless of the column's
// NotNull marker.
func GoType(t dsl.FieldType, notNull bool) string {
	if t.Array {
		if t.Elem == nil {
			return "[]any"
		}
		return "[]" + GoType(*t.Elem, true)
	}
	base := ""
	switch t.Name {
	case "smallint":
		base = "int32"
	case "int":
		base = "int32"
	case "bigint":
		base = "int64"
	case "text", "varchar", "citext":
		base = "string"
	case "boolean":
		base = "bool"
	case "timestamptz", "date":
		base = "time.Time"
	case "interval":
		base = "time.Duration"
	case "uuid":
		base = "string"
	case "bytea":
		base = "[]byte"
	case "jsonb":
		base = "[]byte"
	case "vector":
		base = "[]float32"
	case "numeric":
		base = "string"
	default:
		base = "any"
	}
	switch base {
	case "[]byte", "[]float32":
		return base
	}
	if !notNull {
		return "*" + base
	}
	return base
}

// ProtoType maps a DSL type to its protobuf field-type representation
// (without the field number / name).
//
// Returns an error for types unknown to this package. The IR lowering
// pass rejects unknown types upstream, so an error here surfaces a
// codegen-internal bug rather than a user-facing schema issue.
func ProtoType(t dsl.FieldType) (string, error) {
	if t.Array {
		inner, err := ProtoType(*t.Elem)
		if err != nil {
			return "", err
		}
		return "repeated " + inner, nil
	}
	switch t.Name {
	case "smallint":
		return "int32", nil
	case "int":
		return "int32", nil
	case "bigint":
		return "int64", nil
	case "text", "varchar", "citext":
		return "string", nil
	case "boolean":
		return "bool", nil
	case "timestamptz", "date":
		// proto3 well-known type covers wall-clock dates too at the wire
		// boundary; the server converts to Postgres `date` on read/write.
		return "google.protobuf.Timestamp", nil
	case "interval":
		return "google.protobuf.Duration", nil
	case "uuid":
		return "string", nil
	case "bytea":
		return "bytes", nil
	case "jsonb":
		return "bytes", nil
	case "numeric":
		// Exact decimals: convey as string and parse server-side rather
		// than collapsing precision through proto's double/int64 forms.
		return "string", nil
	case "vector":
		return "repeated float", nil
	}
	return "", fmt.Errorf("unsupported type %q for proto", t.Name)
}

// ScanFragments returns three independently-emittable pieces because
// the decl block, the rows.Scan call, and the post-scan assignment
// loop land in different parts of a generated handler. Caller picks
// the local name (no naming scheme imposed) and the protoField LHS.
//
//   - decl:   `var <local> <go-scan-type>` — the rows.Scan target.
//   - target: `&<local>` — passed into rows.Scan's variadic args.
//   - assign: copies <local> into <protoField>, folding in any
//     nullable→pointer or pgvector→[]float32 conversion.
//
// Mirror of BindExpr in the opposite direction.
func ScanFragments(t dsl.FieldType, notNull bool, local, protoField string) (decl, target, assign string) {
	target = "&" + local

	if t.Array {
		elem := "any"
		if t.Elem != nil {
			elem = GoType(*t.Elem, true)
		}
		decl = fmt.Sprintf("var %s []%s", local, elem)
		assign = fmt.Sprintf("%s = %s", protoField, local)
		return
	}

	switch t.Name {
	case "smallint", "int":
		if notNull {
			decl = fmt.Sprintf("var %s int32", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s sql.NullInt32", local)
			assign = fmt.Sprintf("%s = runtime.Int32PtrFromNull(%s)", protoField, local)
		}
	case "bigint":
		if notNull {
			decl = fmt.Sprintf("var %s int64", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s sql.NullInt64", local)
			assign = fmt.Sprintf("%s = runtime.Int64PtrFromNull(%s)", protoField, local)
		}
	case "text", "varchar", "citext", "uuid", "numeric":
		if notNull {
			decl = fmt.Sprintf("var %s string", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s sql.NullString", local)
			assign = fmt.Sprintf("%s = runtime.StringPtrFromNull(%s)", protoField, local)
		}
	case "boolean":
		if notNull {
			decl = fmt.Sprintf("var %s bool", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s sql.NullBool", local)
			assign = fmt.Sprintf("%s = runtime.BoolPtrFromNull(%s)", protoField, local)
		}
	case "timestamptz", "date":
		if notNull {
			decl = fmt.Sprintf("var %s time.Time", local)
			assign = fmt.Sprintf("%s = runtime.TimeToProto(%s)", protoField, local)
		} else {
			// Inline conversion rather than reaching for
			// runtime.TimePtrToProto — that helper takes *time.Time, not
			// sql.NullTime, so going through it would need a second
			// intermediary.
			decl = fmt.Sprintf("var %s sql.NullTime", local)
			assign = fmt.Sprintf(`if %s.Valid {
		%s = runtime.TimeToProto(%s.Time)
	}`, local, protoField, local)
		}
	case "interval":
		// Postgres INTERVAL has no native pgx Go type; rendered as text
		// and parsed in the caller.
		if notNull {
			decl = fmt.Sprintf("var %s string", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s sql.NullString", local)
			assign = fmt.Sprintf("%s = runtime.StringPtrFromNull(%s)", protoField, local)
		}
	case "bytea", "jsonb":
		decl = fmt.Sprintf("var %s []byte", local)
		assign = fmt.Sprintf("%s = %s", protoField, local)
	case "vector":
		decl = fmt.Sprintf("var %s pgvector.Vector", local)
		assign = fmt.Sprintf("%s = runtime.VectorToFloat32(%s.Slice())", protoField, local)
	default:
		// Defensive fallback so an IR lowering miss surfaces as "scan
		// target is any" rather than a panic at codegen time.
		decl = fmt.Sprintf("var %s any", local)
		assign = fmt.Sprintf("_ = %s // unknown type %s", local, t.Name)
	}
	return
}

// BindExpr returns the Go expression that turns a proto field into
// the value pgx needs at bind. Mirror of ScanFragments.
//
// protoFieldPtr is needed alongside protoGetter because the runtime
// helpers for nullable scalars take *T (proto3's nullable shape)
// rather than the dereferenced getter. Pass "" when the column is
// not-null — the nullable branches won't be reached.
//
// Vectors wrap through pgvector.NewVector so pgx sees the right
// binary format; arrays return the bare getter because pgx scans
// `[]T` directly via the element's scanner.
func BindExpr(t dsl.FieldType, notNull bool, protoGetter, protoFieldPtr string) string {
	if t.Array {
		return protoGetter
	}
	switch t.Name {
	case "smallint", "int":
		if notNull {
			return protoGetter
		}
		return "runtime.NullableInt32(" + protoFieldPtr + ")"
	case "bigint":
		if notNull {
			return protoGetter
		}
		return "runtime.NullableInt64(" + protoFieldPtr + ")"
	case "text", "varchar", "citext", "uuid", "numeric":
		if notNull {
			return protoGetter
		}
		return "runtime.NullableString(" + protoFieldPtr + ")"
	case "boolean":
		if notNull {
			return protoGetter
		}
		return "runtime.NullableBool(" + protoFieldPtr + ")"
	case "timestamptz", "date":
		if notNull {
			return "runtime.ProtoToTime(" + protoGetter + ")"
		}
		return "runtime.ProtoToTimePtr(" + protoFieldPtr + ")"
	case "interval":
		if notNull {
			return protoGetter
		}
		return "runtime.NullableString(" + protoFieldPtr + ")"
	case "bytea", "jsonb":
		return protoGetter
	case "vector":
		return "pgvector.NewVector(" + protoGetter + ")"
	}
	return protoGetter
}

// NeedsPgvector reports whether emitting code for the given type
// requires the github.com/pgvector/pgvector-go import.
//
// The array recursion is defensive — no current type nests vectors
// under an array — but mirrors the GoType/ProtoType recursion so
// import detection can't diverge from emission shape.
func NeedsPgvector(t dsl.FieldType) bool {
	if t.Array {
		if t.Elem == nil {
			return false
		}
		return NeedsPgvector(*t.Elem)
	}
	return t.Name == "vector"
}

// NeedsDatabaseSQL reports whether emitting code for the given type
// requires the database/sql import — driven by the sql.NullX scan
// locals on nullable scalar columns. Returns false for shapes whose
// nullable path scans into a non-sql.Null type (arrays, bytea, jsonb,
// vector).
func NeedsDatabaseSQL(t dsl.FieldType, notNull bool) bool {
	if notNull || t.Array {
		return false
	}
	switch t.Name {
	case "smallint", "int", "bigint",
		"text", "varchar", "citext", "uuid", "numeric",
		"boolean",
		"timestamptz", "date",
		"interval":
		return true
	}
	return false
}
