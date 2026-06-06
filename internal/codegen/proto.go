package codegen

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/codegen/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// EmitProto renders one .proto file per entity in newIR. The previous IR
// (oldIR) supplies stable field numbers so the wire contract is preserved
// across regenerations.
//
// File naming: atlantis/<namespace>/v1/<snake_entity>.proto.
// One service per entity, named <Entity>Service.
//
// Package + path layout follow buf-standard: `atlantis.<ns>.v1` as the
// proto package with the directory mirroring it
// (PACKAGE_DIRECTORY_MATCH + PACKAGE_VERSION_SUFFIX). The buf-generated Go
// for this layout lands at `gen/go/pb/<ns>/v1/`.
//
// Wire stability rules enforced here:
//   - Field numbers are assigned in IR.Fields and never reused.
//   - Removed fields leave their numbers in Entity.RetiredProtoNumbers so
//     future generations can't reclaim them.
//   - New fields get the smallest free positive number (skipping retired
//     numbers).
//
// AssignProtoNumbers is the entrypoint for the codegen pipeline
// (tide generate runs it before any rendering); EmitProto assumes numbers are set.

// ProtoFile is one emitted .proto source file.
type ProtoFile struct {
	Path    string // e.g. "atlantis/consumer/v1/saved_outfit.proto"
	Content string
}

// AssignProtoNumbers walks newIR and sets ProtoNumber on every field, copying
// from oldIR where possible. Newly-added fields get fresh numbers higher than
// any existing or retired number in the entity. Removed fields' numbers are
// added to Entity.RetiredProtoNumbers on newIR. Mutates newIR in place.
func AssignProtoNumbers(oldIR, newIR *dsl.IR) {
	if newIR == nil {
		return
	}
	oldByID := indexByID(oldIR)

	for i := range newIR.Entities {
		e := &newIR.Entities[i]
		oldE, hasOld := oldByID[e.ID()]

		// Carry over retired numbers from the previous checkpoint.
		if hasOld {
			e.RetiredProtoNumbers = slices.Clone(oldE.RetiredProtoNumbers)
		}

		// Reserve = retired + numbers already in use on this entity.
		reserved := map[int]bool{}
		for _, n := range e.RetiredProtoNumbers {
			reserved[n] = true
		}

		// Pass A: preserve existing numbers from oldE for fields that still exist.
		if hasOld {
			oldByName := map[string]*dsl.Field{}
			for j := range oldE.Fields {
				oldByName[oldE.Fields[j].Name] = &oldE.Fields[j]
			}
			for j := range e.Fields {
				f := &e.Fields[j]
				if of, ok := oldByName[f.Name]; ok && of.ProtoNumber != 0 {
					f.ProtoNumber = of.ProtoNumber
					reserved[f.ProtoNumber] = true
				}
			}
			// Any old field NOT present in newIR is now retired.
			newByName := map[string]bool{}
			for j := range e.Fields {
				newByName[e.Fields[j].Name] = true
			}
			for j := range oldE.Fields {
				of := &oldE.Fields[j]
				if !newByName[of.Name] && of.ProtoNumber != 0 {
					if !reserved[of.ProtoNumber] {
						e.RetiredProtoNumbers = append(e.RetiredProtoNumbers, of.ProtoNumber)
						reserved[of.ProtoNumber] = true
					}
				}
			}
		}

		// Pass B: assign fresh numbers to anyone still at 0.
		next := 1
		for j := range e.Fields {
			f := &e.Fields[j]
			if f.ProtoNumber != 0 {
				continue
			}
			for reserved[next] {
				next++
			}
			f.ProtoNumber = next
			reserved[next] = true
		}

		sort.Ints(e.RetiredProtoNumbers)
	}
}

// EmitProto renders the .proto file set for newIR. Assumes AssignProtoNumbers
// has already run. Caller writes the files at the indicated paths.
func EmitProto(newIR *dsl.IR) ([]ProtoFile, error) {
	if newIR == nil {
		return nil, fmt.Errorf("EmitProto: newIR is required")
	}
	// Inbound FK map drives the <Entity>Include enum emission.
	// Computed once per pipeline run.
	inboundByEntity := computeInboundRefs(newIR)

	var out []ProtoFile
	for i := range newIR.Entities {
		e := &newIR.Entities[i]
		f, err := emitProtoEntity(e, inboundByEntity[e.ID()])
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// entityHasTimestampField reports whether any column on the entity
// has a DSL type whose protobuf surface is google.protobuf.Timestamp.
// Used to skip the timestamp.proto import on entities without such
// fields — buf's UNUSED_IMPORT lint rule rejects the import otherwise.
func entityHasTimestampField(e *dsl.Entity) bool {
	for _, f := range e.Fields {
		if f.Type.Name == "timestamptz" || f.Type.Name == "date" {
			return true
		}
	}
	return false
}

// pkColumn captures one column participating in an entity's primary
// key for proto emission. Single-PK entities supply one entry;
// composite-PK entities supply len(CompositePK) entries in DSL order.
type pkColumn struct {
	Name      string // snake_case column name
	ProtoType string // protobuf scalar (`string`, `int64`, ...)
}

// resolvePKColumns turns an entity's PK declaration into the ordered
// list of (name, proto type) pairs the request-shell emitter needs.
// Returns nil for entities that have neither a single primary field
// nor a composite list — those slip through with the historical
// (id int64) fallback in the request-shell emitter.
func resolvePKColumns(e *dsl.Entity) []pkColumn {
	if len(e.CompositePK) > 0 {
		out := make([]pkColumn, 0, len(e.CompositePK))
		for _, name := range e.CompositePK {
			f := e.FindField(name)
			if f == nil {
				continue
			}
			pt, err := protoFieldType(f.Type)
			if err != nil {
				pt = "string"
			}
			out = append(out, pkColumn{Name: f.Name, ProtoType: pt})
		}
		return out
	}
	if pk := e.PrimaryField(); pk != nil {
		pt, err := protoFieldType(pk.Type)
		if err != nil {
			pt = "int64"
		}
		return []pkColumn{{Name: pk.Name, ProtoType: pt}}
	}
	return nil
}

func emitProtoEntity(e *dsl.Entity, inbound []inboundRef) (ProtoFile, error) {
	var b strings.Builder

	b.WriteString("// Code generated by tide. DO NOT EDIT.\n\n")
	b.WriteString("syntax = \"proto3\";\n\n")
	b.WriteString("package atlantis." + goNamespace(e.Namespace) + ".v1;\n\n")
	if entityHasTimestampField(e) {
		b.WriteString("import \"google/protobuf/timestamp.proto\";\n")
	}
	b.WriteString("import \"google/protobuf/field_mask.proto\";\n")
	b.WriteString("import \"atlantis/common/v1/predicates.proto\";\n")

	// Each same-namespace inbound FK whose source entity has a real
	// proto message (i.e., is not a composite-PK header-only stub) gets
	// an import so its include slot can reference the message type.
	// Deduplicated because two FKs from the same source entity emit two
	// slots but only one import.
	imported := map[string]bool{}
	for _, ref := range inbound {
		if !includableSource(ref, e) {
			continue
		}
		refNS, refEnt, _ := strings.Cut(ref.FromEntityID, ".")
		importPath := fmt.Sprintf("atlantis/%s/v1/%s.proto",
			goNamespace(refNS), snakeCase(refEnt))
		if imported[importPath] {
			continue
		}
		imported[importPath] = true
		fmt.Fprintf(&b, "import %q;\n", importPath)
	}
	b.WriteString("\n")

	// Message.
	b.WriteString("message " + e.Name + " {\n")
	// Sort fields by proto number for deterministic output. Codegen has
	// already assigned numbers; we render in numerical order, which also
	// matches what a hand-written .proto looks like.
	fields := slices.Clone(e.Fields)
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].ProtoNumber < fields[j].ProtoNumber
	})
	for _, f := range fields {
		ptype, err := protoFieldType(f.Type)
		if err != nil {
			return ProtoFile{}, fmt.Errorf("%s.%s: %w", e.ID(), f.Name, err)
		}
		// `optional` for nullable scalars matches proto3 presence rules.
		// Columns with a declared SQL DEFAULT are also marked optional even
		// when NOT NULL: this gives callers a way to omit the field and let
		// the server's INSERT COALESCE the column to the declared default.
		// Without optional, proto3 scalars collapse "unset" and "zero value"
		// into the same wire shape, and the server can't distinguish "use
		// the default" from "explicitly bind 0/empty/false".
		opt := ""
		if (!f.NotNull || f.Default != nil) && !isRepeated(f.Type) {
			opt = "optional "
		}
		fmt.Fprintf(&b, "  %s%s %s = %d;\n", opt, ptype, f.Name, f.ProtoNumber)
	}
	// Include slots for inbound FKs in the same namespace whose source
	// entity has a real proto message. Field numbers start at 1000 to
	// leave room for column additions; the index within the inbound list
	// (1-based) gives the offset, mirroring the XInclude enum so the slot
	// number and enum variant move in lockstep. Cross-namespace and
	// composite-PK source entities are visible in the XInclude enum but
	// have no slot here — the handler errors Unimplemented for them.
	for i, ref := range inbound {
		if !includableSource(ref, e) {
			continue
		}
		_, refEnt, _ := strings.Cut(ref.FromEntityID, ".")
		slot := includeSlotName(refEnt, ref.FromField)
		fmt.Fprintf(&b, "  repeated %s %s = %d;\n", refEnt, slot, 1000+i+1)
	}
	// Reserved field numbers — protobuf's own mechanism for preventing reuse.
	if len(e.RetiredProtoNumbers) > 0 {
		reserved := make([]string, len(e.RetiredProtoNumbers))
		for i, n := range e.RetiredProtoNumbers {
			reserved[i] = fmt.Sprintf("%d", n)
		}
		fmt.Fprintf(&b, "  reserved %s;\n", strings.Join(reserved, ", "))
	}
	b.WriteString("}\n\n")

	pkCols := resolvePKColumns(e)
	if len(pkCols) == 0 {
		pkCols = []pkColumn{{Name: "id", ProtoType: "int64"}}
	}
	composite := len(pkCols) > 1

	// Every entity, regardless of PK arity, exposes the same seven RPCs.
	// The typed predicate surface (Filter, OrderField, Include) is
	// PK-arity-agnostic: composite-PK rows expose each PK column as an
	// independently filterable / orderable field, indistinguishable in the
	// wire shape from any other typed column. PK arity reappears only in
	// the request shells (Get / Delete / BatchGet take one field vs. a PK
	// wrapper) and in the cache-key encoding (runtime.CompositeID).
	b.WriteString("service " + e.Name + "Service {\n")
	fmt.Fprintf(&b, "  rpc Get%s(Get%sRequest) returns (Get%sResponse);\n", e.Name, e.Name, e.Name)
	fmt.Fprintf(&b, "  rpc List%s(List%sRequest) returns (List%sResponse);\n", e.Name, e.Name, e.Name)
	fmt.Fprintf(&b, "  rpc Create%s(Create%sRequest) returns (Create%sResponse);\n", e.Name, e.Name, e.Name)
	fmt.Fprintf(&b, "  rpc Update%s(Update%sRequest) returns (Update%sResponse);\n", e.Name, e.Name, e.Name)
	fmt.Fprintf(&b, "  rpc Delete%s(Delete%sRequest) returns (Delete%sResponse);\n", e.Name, e.Name, e.Name)
	fmt.Fprintf(&b, "  rpc BatchGet%s(BatchGet%sRequest) returns (BatchGet%sResponse);\n", e.Name, e.Name, e.Name)
	fmt.Fprintf(&b, "  rpc Query%s(Query%sRequest) returns (Query%sResponse);\n", e.Name, e.Name, e.Name)
	for _, idx := range e.Indexes {
		if idx.Kind == dsl.IndexHNSW {
			fmt.Fprintf(&b, "  rpc Search%sBy%s(Search%sBy%sRequest) returns (Search%sBy%sResponse);\n",
				e.Name, snakeToCamel(idx.Field), e.Name, snakeToCamel(idx.Field), e.Name, snakeToCamel(idx.Field))
		}
	}
	b.WriteString("}\n\n")

	if composite {
		// Emit the PK wrapper message first so BatchGet can reference it.
		fmt.Fprintf(&b, "message %sPK {\n", e.Name)
		for i, c := range pkCols {
			fmt.Fprintf(&b, "  %s %s = %d;\n", c.ProtoType, c.Name, i+1)
		}
		b.WriteString("}\n\n")
	}

	fmt.Fprintf(&b, "message Get%sRequest {\n", e.Name)
	for i, c := range pkCols {
		fmt.Fprintf(&b, "  %s %s = %d;\n", c.ProtoType, c.Name, i+1)
	}
	b.WriteString("}\n")
	fmt.Fprintf(&b, "message Get%sResponse { %s entity = 1; }\n\n", e.Name, e.Name)

	// List's request shape keeps `offset` for wire compatibility but the
	// generated server rejects any non-zero value. Callers that page
	// past the first chunk read `next_page_token` and pass it to
	// QueryX directly; the shim does not accept a page token in the
	// List request, since adding a token field would force the offset
	// field to share oneof semantics with it (proto3 doesn't support
	// adding a field to an existing message in a way that toggles
	// presence of another field). New callers should call QueryX.
	fmt.Fprintf(&b, "message List%sRequest { int32 limit = 1; int32 offset = 2; }\n", e.Name)
	fmt.Fprintf(&b, "message List%sResponse { repeated %s entities = 1; int64 total = 2; string next_page_token = 3; }\n\n", e.Name, e.Name)

	fmt.Fprintf(&b, "message Create%sRequest { %s entity = 1; }\n", e.Name, e.Name)
	fmt.Fprintf(&b, "message Create%sResponse { %s entity = 1; }\n\n", e.Name, e.Name)

	fmt.Fprintf(&b, "message Update%sRequest { %s entity = 1; }\n", e.Name, e.Name)
	fmt.Fprintf(&b, "message Update%sResponse { %s entity = 1; }\n\n", e.Name, e.Name)

	fmt.Fprintf(&b, "message Delete%sRequest {\n", e.Name)
	for i, c := range pkCols {
		fmt.Fprintf(&b, "  %s %s = %d;\n", c.ProtoType, c.Name, i+1)
	}
	b.WriteString("}\n")
	fmt.Fprintf(&b, "message Delete%sResponse {}\n\n", e.Name)

	if composite {
		fmt.Fprintf(&b, "message BatchGet%sRequest { repeated %sPK ids = 1; }\n", e.Name, e.Name)
	} else {
		fmt.Fprintf(&b, "message BatchGet%sRequest { repeated %s %ss = 1; }\n", e.Name, pkCols[0].ProtoType, pkCols[0].Name)
	}
	fmt.Fprintf(&b, "message BatchGet%sResponse { repeated %s entities = 1; }\n", e.Name, e.Name)

	for _, idx := range e.Indexes {
		if idx.Kind == dsl.IndexHNSW {
			rpc := fmt.Sprintf("Search%sBy%s", e.Name, snakeToCamel(idx.Field))
			b.WriteString("\n")
			fmt.Fprintf(&b, "message %sRequest {\n", rpc)
			fmt.Fprintf(&b, "  repeated float query_vector = 1;\n")
			fmt.Fprintf(&b, "  int32 limit = 2;\n")
			fmt.Fprintf(&b, "}\n")
			fmt.Fprintf(&b, "message %sResponse {\n", rpc)
			fmt.Fprintf(&b, "  repeated %s entities = 1;\n", e.Name)
			fmt.Fprintf(&b, "  repeated float distances = 2;\n")
			fmt.Fprintf(&b, "}\n")
		}
	}

	// The typed query surface (Filter, OrderField, OrderBy, Include,
	// QueryRequest, QueryResponse) is emitted from the same template for
	// every entity. PK arity has no influence here — see the comment on
	// the service block above for the reasoning.
	b.WriteString("\n")
	emitProtoQuerySurface(&b, e, inbound)
	_ = composite // intentionally unused; the per-entity surface no longer branches on arity

	path := fmt.Sprintf("atlantis/%s/v1/%s.proto", goNamespace(e.Namespace), snakeCase(e.Name))
	return ProtoFile{Path: path, Content: b.String()}, nil
}

// protoFieldType maps a DSL type to its protobuf scalar / well-known
// type. Thin wrapper routing through coltype so proto emission and Go
// scan/bind emission can never disagree on type semantics.
func protoFieldType(t dsl.FieldType) (string, error) {
	return coltype.ProtoType(t)
}

func isRepeated(t dsl.FieldType) bool {
	if t.Array {
		return true
	}
	return t.Name == "vector"
}

// goNamespace maps a DSL namespace to its Go-/proto-safe form. Go's
// toolchain reserves directories literally named `vendor` (they're treated
// as a module's vendor directory), so any DSL namespace that would collide
// with that reservation is remapped here. SQL keeps the original DSL
// namespace so on-disk table prefixes stay unchanged.
func goNamespace(ns string) string {
	if ns == "vendor" {
		return "vendorpkg"
	}
	return ns
}

// snakeToCamel converts snake_case to UpperCamelCase for proto symbol names.
// Used to build RPC names like `SearchProductVariantByItemVec` from the
// field name `item_vec`.
func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}
