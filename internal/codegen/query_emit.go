package codegen

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Proto emission for the per-entity query surface: Filter, OrderField/OrderBy,
// Include enum, plus QueryX RPC + Request/Response messages. Called from
// emitProtoEntity.

// emitProtoQuerySurface appends the QueryX surface (filter / order /
// include / RPC / request / response) to b. `inbound` is the pre-computed
// list of references pointing AT this entity, used to drive Include enum
// variants.
func emitProtoQuerySurface(b *strings.Builder, e *dsl.Entity, inbound []inboundRef) {
	// XFilter — one optional <Type>Predicate per filterable field, plus
	// the and/or/not composite arms at high field numbers (100/101/102)
	// so any future field additions don't collide.
	fmt.Fprintf(b, "message %sFilter {\n", e.Name)
	fields := slices.Clone(e.Fields)
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].ProtoNumber < fields[j].ProtoNumber
	})
	for _, f := range fields {
		pred, ok := predicateMessageForField(f.Type)
		if !ok {
			// Unsupported types (vector, interval, arrays) silently skip.
			// Callers can't filter on them via QueryX; vector search has
			// its own RPC, interval and array filters aren't supported through the typed predicate surface yet.
			continue
		}
		fmt.Fprintf(b, "  optional atlantis.common.v1.%s %s = %d;\n", pred, f.Name, f.ProtoNumber)
	}
	fmt.Fprintf(b, "  repeated %sFilter and = 100;\n", e.Name)
	fmt.Fprintf(b, "  repeated %sFilter or = 101;\n", e.Name)
	fmt.Fprintf(b, "  optional %sFilter not = 102;\n", e.Name)
	if len(e.RetiredProtoNumbers) > 0 {
		reserved := make([]string, len(e.RetiredProtoNumbers))
		for i, n := range e.RetiredProtoNumbers {
			reserved[i] = fmt.Sprintf("%d", n)
		}
		fmt.Fprintf(b, "  reserved %s;\n", strings.Join(reserved, ", "))
	}
	b.WriteString("}\n\n")

	// XOrderField enum — one variant per orderable field. Vector / interval
	// / array fields can't sit in an ORDER BY meaningfully; skip them.
	enumName := fmt.Sprintf("%sOrderField", e.Name)
	fmt.Fprintf(b, "enum %s {\n", enumName)
	fmt.Fprintf(b, "  %s_UNSPECIFIED = 0;\n", screamingSnake(enumName))
	for _, f := range fields {
		if !orderableType(f.Type) {
			continue
		}
		fmt.Fprintf(b, "  %s_%s = %d;\n", screamingSnake(enumName), strings.ToUpper(f.Name), f.ProtoNumber)
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(b, "message %sOrderBy {\n", e.Name)
	fmt.Fprintf(b, "  %s field = 1;\n", enumName)
	fmt.Fprintf(b, "  bool desc = 2;\n")
	b.WriteString("}\n\n")

	// XInclude enum — one variant per inbound FK reference. `inbound` was
	// computed by the caller against the full IR.
	incName := fmt.Sprintf("%sInclude", e.Name)
	fmt.Fprintf(b, "enum %s {\n", incName)
	fmt.Fprintf(b, "  %s_UNSPECIFIED = 0;\n", screamingSnake(incName))
	for i, ref := range inbound {
		// Enum value name: <INCLUDE_NAME>_<NAMESPACE>_<ENTITY>_BY_<FK_COL>.
		// The FK column is appended unconditionally to disambiguate cases
		// like consumer.CartItem.variant_id vs vendorpkg.CartItem.variant_id
		// both pointing at ProductVariant — and also where one source
		// entity has multiple FKs to the same target (e.g. an entity with
		// `created_by_user_id` AND `modified_by_user_id` both → User).
		// Variant numbers start at 1 (UNSPECIFIED holds 0).
		ns, ent, ok := strings.Cut(ref.FromEntityID, ".")
		if !ok {
			ns, ent = "", ref.FromEntityID
		}
		fmt.Fprintf(b, "  %s_%s_%s_BY_%s = %d;\n",
			screamingSnake(incName),
			screamingSnake(goNamespace(ns)),
			screamingSnake(ent),
			strings.ToUpper(ref.FromField),
			i+1)
	}
	b.WriteString("}\n\n")

	// QueryX request message. Field-mask + page_token carry keyset
	// pagination and includes; the translator + handler already understand
	// the shape.
	fmt.Fprintf(b, "message Query%sRequest {\n", e.Name)
	fmt.Fprintf(b, "  %sFilter filter = 1;\n", e.Name)
	fmt.Fprintf(b, "  repeated %sOrderBy order = 2;\n", e.Name)
	fmt.Fprintf(b, "  int32 limit = 3;\n")
	fmt.Fprintf(b, "  string page_token = 4;\n")
	fmt.Fprintf(b, "  google.protobuf.FieldMask fields = 5;\n")
	fmt.Fprintf(b, "  repeated %s includes = 6;\n", incName)
	fmt.Fprintf(b, "  bool cache_skip = 7;\n")
	b.WriteString("}\n\n")

	fmt.Fprintf(b, "message Query%sResponse {\n", e.Name)
	fmt.Fprintf(b, "  repeated %s entities = 1;\n", e.Name)
	fmt.Fprintf(b, "  string next_page_token = 2;\n")
	fmt.Fprintf(b, "  optional int64 total_estimate = 3;\n")
	b.WriteString("}\n")
}

// includeSlotName builds the field name for the include slot on an
// entity's proto message. Stable across runs: the slot name is derived
// from the source entity's name and FK column, so reordering inbound
// references doesn't rename the slot — only its proto field number.
func includeSlotName(sourceEntity, fkField string) string {
	return "included_" + snakeCase(sourceEntity) + "_by_" + fkField
}

// predicateMessageForField maps a DSL field type to the predicate proto
// message name. Returns ("", false) when the type isn't filterable via
// QueryX (vector, interval, arrays).
func predicateMessageForField(t dsl.FieldType) (string, bool) {
	if t.Array {
		return "", false
	}
	switch t.Name {
	case "text", "varchar", "citext", "uuid":
		return "StringPredicate", true
	case "numeric":
		return "NumericPredicate", true
	case "int", "smallint":
		return "Int32Predicate", true
	case "bigint":
		return "Int64Predicate", true
	case "boolean":
		return "BoolPredicate", true
	case "timestamptz", "date":
		return "TimestampPredicate", true
	case "jsonb", "bytea":
		return "BytesPredicate", true
	}
	return "", false
}

// orderableType is true when the field can sit in an ORDER BY clause. We
// allow every scalar; vectors and arrays don't sort meaningfully.
func orderableType(t dsl.FieldType) bool {
	if t.Array || t.Name == "vector" {
		return false
	}
	return true
}

// inboundRef captures a foreign key pointing AT some entity X — used to
// drive XInclude enum variant generation and the include slot fields on
// the target entity message.
type inboundRef struct {
	// FromEntityID is the entity that declares the FK (e.g. "consumer.Session").
	FromEntityID string
	// FromField is the column on FromEntityID holding the FK value.
	FromField string
	// FromEntity is a pointer back to the source entity so handlers can
	// reach its table name, PK type, and column list when emitting the
	// include attach helper. nil for cross-IR resolution failures.
	FromEntity *dsl.Entity
}

// includableSource reports whether an inbound FK can drive a real
// include slot on the target entity's message.
//
// Same-namespace sources qualify; cross-namespace sources do not, because
// emitting a slot for a foreign-namespace message would force every
// caller package to import every other namespace's proto types just to
// receive a Query<E> response. The XInclude enum still carries the
// cross-namespace variants so the proto contract is forward-compatible —
// the runtime handler returns Unimplemented when one is requested.
//
// PK arity is irrelevant here. Composite-PK source entities have full
// proto message bodies (the typed Filter / Include / Query surface) just
// like single-PK ones; the attach helper can reference them without
// special-casing.
func includableSource(ref inboundRef, target *dsl.Entity) bool {
	refNS, _, ok := strings.Cut(ref.FromEntityID, ".")
	if !ok {
		return false
	}
	return refNS == target.Namespace
}

// computeInboundRefs scans every entity for `references` fields pointing
// AT each other entity, and returns a map keyed by target entity ID.
//
// One call before EmitProto's main loop; cheap (O(entities × fields)).
func computeInboundRefs(ir *dsl.IR) map[string][]inboundRef {
	if ir == nil {
		return nil
	}
	out := map[string][]inboundRef{}
	for i := range ir.Entities {
		e := &ir.Entities[i]
		for _, f := range e.Fields {
			if f.Ref == nil || f.Ref.TargetID == "" {
				continue
			}
			out[f.Ref.TargetID] = append(out[f.Ref.TargetID], inboundRef{
				FromEntityID: e.ID(),
				FromField:    f.Name,
				FromEntity:   e,
			})
		}
	}
	// Stable order for deterministic enum value numbering.
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool {
			a, b := out[k][i], out[k][j]
			if a.FromEntityID != b.FromEntityID {
				return a.FromEntityID < b.FromEntityID
			}
			return a.FromField < b.FromField
		})
	}
	return out
}

// screamingSnake turns "AccountOrderField" into "ACCOUNT_ORDER_FIELD".
// Buf's ENUM_VALUE_PREFIX lint rule computes the expected enum prefix via
// the same heuristic — insert `_` before an uppercase character if (the
// previous char was lowercase) OR (the next char is lowercase). The
// downside is that `OAuthProvider` becomes `O_AUTH_PROVIDER`; we live
// with it because matching buf's heuristic is more valuable than a nicer
// enum variant for one entity.
func screamingSnake(camel string) string {
	rs := []rune(camel)
	var b strings.Builder
	for i, r := range rs {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prevLower := rs[i-1] >= 'a' && rs[i-1] <= 'z'
			nextLower := i+1 < len(rs) && rs[i+1] >= 'a' && rs[i+1] <= 'z'
			if prevLower || nextLower {
				b.WriteByte('_')
			}
		}
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r - 32)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
