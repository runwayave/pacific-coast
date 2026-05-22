package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// EmitGoCacheKeys emits one `<entity>_cache_keys.go` per entity with the
// derived key functions referenced by the generated server handlers.
//
// What gets emitted per entity:
//
//   - bodyKey(id):            atl:v1:{ID}:{id}:{ver}      — versioned body
//   - pointerKey(id):         atl:v1:{ID}:{id}:ver        — version pointer
//   - tagKey(self):           the tag template expanded against an entity value
//   - indexKey<Index>(args):  atl:v1:{ID}:idx:{name}:{args-hash}
//
// `ID` is the "namespace.Entity" string. We deliberately use the
// canonical ID rather than the snake-cased table name so cache keys stay
// readable in memcachedctl / debug dumps even when entity names get long.
//
// Index keys are emitted for every btree / partial index. HNSW and GIN
// indexes are NOT cached — vector queries are not cached at all, and
// GIN's high cardinality has the same problem.
func EmitGoCacheKeys(newIR *dsl.IR) ([]GoFile, error) {
	if newIR == nil {
		return nil, fmt.Errorf("EmitGoCacheKeys: newIR is required")
	}
	var out []GoFile
	for i := range newIR.Entities {
		e := &newIR.Entities[i]
		f, err := emitGoCacheKeysEntity(e)
		if err != nil {
			return nil, fmt.Errorf("entity %s: %w", e.ID(), err)
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// pkParam describes one parameter of a PK-keyed function (BodyKey,
// PointerKey, Get, Delete). Single-PK entities produce one entry;
// composite-PK entities produce one per column in DSL declaration order.
type pkParam struct {
	Name   string // Go parameter name (lower-camel, reserved-word-safe)
	GoType string // Go type (string, int64, etc.)
}

// pkKeyParams returns the parameter list for a PK-keyed function.
// Order is significant: it must match the order used by runtime.CompositeID
// in the server emitter, otherwise the body/pointer keys produced here
// won't match the keys the cache and outbox actually hold.
func pkKeyParams(e *dsl.Entity) []pkParam {
	if len(e.CompositePK) > 0 {
		out := make([]pkParam, 0, len(e.CompositePK))
		for _, name := range e.CompositePK {
			f := e.FindField(name)
			if f == nil {
				continue
			}
			out = append(out, pkParam{Name: goParamName(f.Name), GoType: goFieldType(f.Type, true)})
		}
		return out
	}
	if pk := e.PrimaryField(); pk != nil {
		return []pkParam{{Name: "id", GoType: goFieldType(pk.Type, true)}}
	}
	return nil
}

func emitGoCacheKeysEntity(e *dsl.Entity) (GoFile, error) {
	ns := goNamespace(e.Namespace)
	pkParams := pkKeyParams(e)
	if len(pkParams) == 0 {
		return GoFile{}, fmt.Errorf("no primary field")
	}

	// Body is rendered into a scratch builder first so we can decide which
	// imports are needed before writing the header. crypto/sha256 and
	// encoding/hex are only referenced inside emitted IndexKey functions;
	// entities with no btree / partial indexes (every index is HNSW or GIN,
	// or the entity has no index at all) would trip "imported and not used"
	// if we hard-coded the imports.
	var body strings.Builder
	hasIndexKeyFn := false

	entityID := e.ID()
	sigParams := make([]string, len(pkParams))
	callArgs := make([]string, len(pkParams))
	for i, p := range pkParams {
		sigParams[i] = p.Name + " " + p.GoType
		callArgs[i] = p.Name
	}
	pkSig := strings.Join(sigParams, ", ")
	pkCall := strings.Join(callArgs, ", ")

	// Body & pointer keys. The single-pass-through-CompositeID encoding is
	// required: the server hot path (Update/Delete/Create) computes
	// `runtime.CompositeID(id)` (single PK) or `runtime.CompositeID(col1, col2)`
	// (composite) before calling Cache.CurrentVersion / Outbox.Enqueue, so
	// any external caller that builds a body or pointer key must use the
	// same length-prefixed encoding to land on the same memcached entry.
	// Earlier revisions used fmt.Sprint(id) here, which silently diverged
	// for single-PK rows; the keys appeared to work but never matched what
	// the cache held. The CompositeID helper handles both arities and is
	// the encoder.
	fmt.Fprintf(&body, `// %sBodyKey returns the cache key holding the encoded entity at the given version.
func %sBodyKey(%s, version int64) string {
	return runtime.CacheKey(%q, runtime.CompositeID(%s), version)
}

// %sPointerKey returns the current-version pointer key for the row.
func %sPointerKey(%s) string {
	return runtime.PointerKey(%q, runtime.CompositeID(%s))
}

`, e.Name, e.Name, pkSig, entityID, pkCall,
		e.Name, e.Name, pkSig, entityID, pkCall)

	// Tag key (optional).
	if e.Cache != nil && e.Cache.Tag != "" {
		// Build the tag expansion code. Each {placeholder} is replaced with
		// the corresponding field value rendered via fmt.Sprint.
		args, expansion := tagExpansion(e, e.Cache.Tag, e.Cache.TagFields)
		fmt.Fprintf(&body, `// %sTagKey returns the shared cache tag for one row.
func %sTagKey(%s) string {
	return %s
}

`, e.Name, e.Name, args, expansion)
	}

	// Index keys for every btree / partial index.
	for _, idx := range e.Indexes {
		if idx.Kind == dsl.IndexHNSW || idx.Kind == dsl.IndexGIN {
			continue
		}
		if emitIndexKeyFn(&body, e, idx) {
			hasIndexKeyFn = true
		}
	}

	var header strings.Builder
	header.WriteString("// Code generated by tidectl. DO NOT EDIT.\n\n")
	fmt.Fprintf(&header, "package %s\n\n", ns)
	header.WriteString("import (\n")
	if hasIndexKeyFn {
		header.WriteString("\t\"crypto/sha256\"\n")
		header.WriteString("\t\"encoding/hex\"\n")
	}
	header.WriteString("\t\"fmt\"\n")
	header.WriteString("\t\"strings\"\n")
	header.WriteString("\t\"time\"\n\n")
	header.WriteString("\t\"github.com/rachitkumar205/atlantis/internal/runtime\"\n")
	header.WriteString(")\n\n")
	header.WriteString("var _ = fmt.Sprint\n")
	header.WriteString("var _ = strings.Builder{}\n")
	header.WriteString("var _ = time.Time{}\n\n")

	path := fmt.Sprintf("gen/go/keys/%s/%s_keys.go", ns, snakeCase(e.Name))
	return GoFile{Path: path, Content: header.String() + body.String()}, nil
}

// emitIndexKeyFn emits one IndexKey<Name> function for a btree / partial
// index. The key shape is:
//
//	atl:v1:{entity}:idx:{index_name}:{sha256-of-args-hex-first-16}
//
// We hash the joined arg string because index args may include arbitrary
// user-supplied text; truncating the hex preserves a high collision margin
// while keeping memcached keys short. The hash hex is short for human
// readability in cache dumps.
func emitIndexKeyFn(b *strings.Builder, e *dsl.Entity, idx dsl.Index) bool {
	// Skip emitting a typed cache-key function for indexes that contain
	// any expression entries — there's no caller-supplied parameter to
	// hash. The SQL emitter still creates the index for query-planner use.
	for _, f := range idx.Fields {
		if f.IsExpr {
			return false
		}
	}

	// Generate the function name from the index fields. e.g.
	// btree over (consumer_id, created_at desc) → IndexByConsumerIdCreatedAt.
	//
	// Partial indexes encode the WHERE predicate into the suffix so two
	// partials on the same field set but different predicates produce
	// distinct function names. Without this, OutfitInteraction's two
	// partials on (consumer_id, variant_id, interacted_at) — one filtering
	// action='purchased', the other action='added_to_cart' — would emit
	// colliding key functions and fail to compile.
	suffix := "By"
	for _, f := range idx.Fields {
		suffix += snakeToCamel(f.Name)
	}
	if idx.Kind == dsl.IndexPartial {
		suffix = "Partial" + suffix + partialPredSuffix(idx.Where)
	}
	fnName := e.Name + suffix + "Key"

	// Build the parameter list mirroring the index fields. We don't include
	// `desc` in the signature — direction only affects the SQL query, not the
	// cache key shape.
	params := make([]string, 0, len(idx.Fields))
	args := make([]string, 0, len(idx.Fields))
	for _, f := range idx.Fields {
		field := e.FindField(f.Name)
		// If the index references a field that no longer exists (shouldn't
		// happen at this stage — validate.go catches it — but be defensive),
		// fall back to `any`.
		var t string
		if field != nil {
			t = goFieldType(field.Type, true)
		} else {
			t = "any"
		}
		params = append(params, fmt.Sprintf("%s %s", goParamName(f.Name), t))
		// Length-prefix each arg so values containing the ":" separator
		// can't collide. Without this, index_a({"x:y"}) and
		// index_a({"x", "y"}) would hash to the same key.
		// runtime.EncodeKeyArg is the encoder; the import is
		// already in scope at the top of every emitted cache-key file.
		args = append(args, fmt.Sprintf("runtime.EncodeKeyArg(%s)", goParamName(f.Name)))
	}

	// Render the index name token inside the cache key. indexKey() in diff.go
	// returns e.g. "btree:a,b desc" or "partial:a|b is null"; strip the
	// "kind:" prefix and sanitize so the token is a clean ASCII slug.
	rawKey := indexKey(idx)
	if colon := strings.IndexByte(rawKey, ':'); colon >= 0 {
		rawKey = rawKey[colon+1:]
	}
	indexNameToken := strings.ReplaceAll(rawKey, " ", "_")
	indexNameToken = strings.ReplaceAll(indexNameToken, ",", "_")
	indexNameToken = strings.ReplaceAll(indexNameToken, "|", "_w_")

	fmt.Fprintf(b, `// %s returns the cache key for an index lookup.
func %s(%s) string {
	parts := []string{%s}
	h := sha256.Sum256([]byte(strings.Join(parts, ":")))
	return "atl:v1:%s:idx:%s:" + hex.EncodeToString(h[:8])
}

`, fnName, fnName, strings.Join(params, ", "), strings.Join(args, ", "),
		e.ID(), indexNameToken)
	return true
}

// partialPredSuffix renders a partial-index WHERE predicate as a Go identifier
// fragment. The fragment must be deterministic and unique across the predicate
// forms the DSL accepts (IS NULL / IS NOT NULL / comparison-against-literal),
// otherwise two partials on the same field set collide at function-name level
// even though the SQL emitter handles them as distinct indexes.
func partialPredSuffix(p *dsl.PartialPred) string {
	if p == nil {
		return ""
	}
	field := snakeToCamel(p.Field)
	switch {
	case p.Op == "" && p.IsNull:
		return "Where" + field + "IsNull"
	case p.Op == "":
		return "Where" + field + "IsNotNull"
	}
	op := map[string]string{
		"=": "Eq", "!=": "Ne",
		"<": "Lt", "<=": "Le",
		">": "Gt", ">=": "Ge",
	}[p.Op]
	if op == "" {
		op = "Op"
	}
	lit := ""
	if p.Literal != nil {
		switch p.Literal.Kind {
		case dsl.DefaultIRString:
			lit = snakeToCamel(p.Literal.Str)
		case dsl.DefaultIRInt:
			lit = fmt.Sprintf("%d", p.Literal.Int)
		case dsl.DefaultIRBool:
			if p.Literal.Bool {
				lit = "True"
			} else {
				lit = "False"
			}
		}
	}
	return "Where" + field + op + lit
}

// tagExpansion builds the function arg list and the runtime expression that
// renders the tag template. Each {field} placeholder becomes one function
// parameter typed against the field's Go type — that keeps the keys package
// independent of the server / client row types (no circular imports) and
// makes the API self-documenting at call sites:
//
//	key := SavedOutfitTagKey(row.ConsumerId)
func tagExpansion(e *dsl.Entity, tag string, fields []string) (params, body string) {
	pieces := splitTagTemplate(tag)

	// Build the parameter list in template-encounter order, deduping
	// repeated placeholders (the same field may appear more than once in a
	// single tag template).
	seen := map[string]bool{}
	var paramList []string
	for _, p := range pieces {
		if !p.isField {
			continue
		}
		if seen[p.value] {
			continue
		}
		seen[p.value] = true
		field := e.FindField(p.value)
		var t string
		if field != nil {
			t = goFieldType(field.Type, true)
		} else {
			t = "any"
		}
		paramList = append(paramList, fmt.Sprintf("%s %s", goParamName(p.value), t))
	}
	params = strings.Join(paramList, ", ")

	// Build the concat expression piece by piece.
	var parts []string
	for _, p := range pieces {
		if p.isField {
			parts = append(parts, fmt.Sprintf("fmt.Sprint(%s)", goParamName(p.value)))
		} else {
			parts = append(parts, fmt.Sprintf("%q", p.value))
		}
	}
	body = strings.Join(parts, " + ")
	_ = fields // kept on signature for future use
	return params, body
}

// tagPiece is one chunk of a parsed tag template: a literal string or a field placeholder.
type tagPiece struct {
	value   string
	isField bool
}

// splitTagTemplate parses a tag template like "consumer:{consumer_id}" into
// alternating literal and field pieces. Unbalanced braces fall through as
// literal text (mirroring parseTagPlaceholders in ir.go).
func splitTagTemplate(tag string) []tagPiece {
	var out []tagPiece
	rem := tag
	for {
		open := strings.IndexByte(rem, '{')
		if open < 0 {
			if rem != "" {
				out = append(out, tagPiece{value: rem})
			}
			return out
		}
		close := strings.IndexByte(rem[open:], '}')
		if close < 0 {
			out = append(out, tagPiece{value: rem})
			return out
		}
		if open > 0 {
			out = append(out, tagPiece{value: rem[:open]})
		}
		out = append(out, tagPiece{value: rem[open+1 : open+close], isField: true})
		rem = rem[open+close+1:]
	}
}

// goParamName turns a snake_case field name into a Go-acceptable parameter
// name. We lower-case the first letter of the camel form so it doesn't shadow
// the exported field name. If the result collides with a Go reserved word
// (e.g. a DSL field named `type` or `range`), we append `Val` so the emitted
// code is still syntactically valid. The same input always produces the same
// output, so the regenerated tree stays diff-stable.
func goParamName(snake string) string {
	camel := snakeToCamel(snake)
	if camel == "" {
		return "arg"
	}
	name := strings.ToLower(camel[:1]) + camel[1:]
	if goReservedWords[name] {
		return name + "Val"
	}
	return name
}

// goReservedWords is the Go 1.x keyword set. Predeclared identifiers
// (any, nil, true, etc.) shadow but don't error, so we leave them be.
var goReservedWords = map[string]bool{
	"break":       true,
	"case":        true,
	"chan":        true,
	"const":       true,
	"continue":    true,
	"default":     true,
	"defer":       true,
	"else":        true,
	"fallthrough": true,
	"for":         true,
	"func":        true,
	"go":          true,
	"goto":        true,
	"if":          true,
	"import":      true,
	"interface":   true,
	"map":         true,
	"package":     true,
	"range":       true,
	"return":      true,
	"select":      true,
	"struct":      true,
	"switch":      true,
	"type":        true,
	"var":         true,
}
