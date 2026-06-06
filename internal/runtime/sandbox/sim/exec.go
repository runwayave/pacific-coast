package sim

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	simsql "github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim/sql"
)

// execEnv is the per-statement context the executor passes around. It
// pairs the catalog (schema we trust) with the engine's tables map,
// the placeholder args supplied by the caller, and the clock used for
// now(). proposedRow is non-nil while resolving an ON CONFLICT DO
// UPDATE assignment — it carries the would-be-inserted row so
// EXCLUDED.col references resolve against the new values rather than
// the existing ones. scanRow/scanDesc are set while evaluating a
// row-context expression (vector distance in projection or ORDER BY,
// JSONB extract in WHERE) so evalExpr can read the live row.
type execEnv struct {
	catalog      *Catalog
	tables       map[string]*Table
	args         []any
	clock        func() time.Time
	pool         *Pool      // non-nil when invoked via Pool.QueryRow/Query/Exec — used for hypertable warn-once
	proposedRow  Row        // non-nil only during ON CONFLICT DO UPDATE
	proposedDesc *TableDesc // descriptor for resolving column names in proposedRow
	scanRow      Row        // non-nil while evaluating per-row expressions
	scanDesc     *TableDesc // descriptor for the current scan row
}

// arg returns the value bound to $n. Returns nil for out-of-range
// references; the parser already validates that placeholders look like
// positive ints, so the only way to land here is the caller supplied
// too few args — we signal that as a typed error.
func (e *execEnv) arg(n int) (any, error) {
	if n < 1 || n >= len(e.args) {
		return nil, fmt.Errorf("sandbox exec: $%d not bound (got %d args)", n, len(e.args)-1)
	}
	return e.args[n], nil
}

// table resolves a parsed TableRef against the catalog + tables map.
// Lazy-allocates storage on first reference so boot doesn't pay for
// empty entities. Also fires the one-time hypertable warning the
// fidelity matrix promises (pool-level once-set; nil pool means
// "warning disabled" — used by ad-hoc executor callers without a
// pool, none today).
func (e *execEnv) table(ref simsql.TableRef) (*Table, error) {
	desc := e.catalog.Lookup(ref.Qualified())
	if desc == nil {
		return nil, fmt.Errorf("%w: %s", ErrTableNotFound, ref.Qualified())
	}
	t, ok := e.tables[ref.Qualified()]
	if !ok {
		t = NewTable(desc)
		e.tables[ref.Qualified()] = t
	}
	if e.pool != nil {
		e.pool.warnHypertableOnce(ref.Qualified())
	}
	return t, nil
}

// evalExpr collapses an Expr down to a Go value at execute time.
// Placeholders look up bound args; literals carry their own value;
// function calls dispatch by name; Coalesce returns the placeholder
// value if non-nil, else recurses on the default; ExcludedRef reads
// the proposed-row value during ON CONFLICT DO UPDATE resolution.
func (e *execEnv) evalExpr(x simsql.Expr) (any, error) {
	switch v := x.(type) {
	case simsql.Placeholder:
		return e.arg(v.N)
	case simsql.Literal:
		switch v.Kind {
		case simsql.LitString:
			return v.Str, nil
		case simsql.LitInt64:
			return v.Int64, nil
		}
		return nil, fmt.Errorf("sandbox exec: unknown literal kind %d", v.Kind)
	case simsql.Coalesce:
		val, err := e.arg(v.Value.N)
		if err != nil {
			return nil, err
		}
		if val != nil {
			return val, nil
		}
		return e.evalExpr(v.Default)
	case simsql.FuncCall:
		switch strings.ToLower(v.Name) {
		case "now":
			if len(v.Args) != 0 {
				return nil, fmt.Errorf("sandbox exec: now() takes no arguments")
			}
			return e.clock(), nil
		}
		return nil, fmt.Errorf("%w: function %s() not implemented",
			simsql.ErrUnsupported, v.Name)
	case simsql.ExcludedRef:
		if e.proposedRow == nil || e.proposedDesc == nil {
			return nil, fmt.Errorf("sandbox exec: EXCLUDED.%s used outside ON CONFLICT DO UPDATE", v.Column)
		}
		idx := e.proposedDesc.ColIndex(v.Column)
		if idx < 0 {
			return nil, fmt.Errorf("%w: EXCLUDED.%s on %s", ErrColumnNotFound, v.Column, e.proposedDesc.Qualified())
		}
		return e.proposedRow[idx], nil
	case simsql.ColumnRef:
		if e.scanRow == nil || e.scanDesc == nil {
			return nil, fmt.Errorf("sandbox exec: bare column %s referenced outside row context", v.Name)
		}
		idx := e.scanDesc.ColIndex(v.Name)
		if idx < 0 {
			return nil, fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Name, e.scanDesc.Qualified())
		}
		return e.scanRow[idx], nil
	case simsql.VectorDistance:
		if e.scanRow == nil || e.scanDesc == nil {
			return nil, fmt.Errorf("sandbox exec: vector distance on %s used outside row context", v.Column)
		}
		idx := e.scanDesc.ColIndex(v.Column)
		if idx < 0 {
			return nil, fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Column, e.scanDesc.Qualified())
		}
		rowVec, err := asFloat32Slice(e.scanRow[idx])
		if err != nil {
			return nil, fmt.Errorf("sandbox exec: %s.%s: %w", e.scanDesc.Qualified(), v.Column, err)
		}
		argVal, err := e.arg(v.Arg.N)
		if err != nil {
			return nil, err
		}
		argVec, err := asFloat32Slice(argVal)
		if err != nil {
			return nil, fmt.Errorf("sandbox exec: vector arg $%d: %w", v.Arg.N, err)
		}
		return vectorDistance(rowVec, argVec, v.Op), nil
	case simsql.JsonExtract:
		if e.scanRow == nil || e.scanDesc == nil {
			return nil, fmt.Errorf("sandbox exec: JSONB extract on %s outside row context", v.Column)
		}
		idx := e.scanDesc.ColIndex(v.Column)
		if idx < 0 {
			return nil, fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Column, e.scanDesc.Qualified())
		}
		return resolveJsonPath(e.scanRow[idx], v.Path, v.AsText)
	}
	return nil, fmt.Errorf("sandbox exec: unknown expression type %T", x)
}

// asFloat32Slice coerces a Go value into the canonical []float32
// representation. Accepts []float32 (the codegen-emitted bind shape)
// plus []any of floats so test code can construct vectors inline.
func asFloat32Slice(v any) ([]float32, error) {
	switch x := v.(type) {
	case []float32:
		return x, nil
	case []float64:
		out := make([]float32, len(x))
		for i, f := range x {
			out[i] = float32(f)
		}
		return out, nil
	case []any:
		out := make([]float32, len(x))
		for i, e := range x {
			switch f := e.(type) {
			case float32:
				out[i] = f
			case float64:
				out[i] = float32(f)
			default:
				return nil, fmt.Errorf("vector element %d is %T, want float", i, e)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected vector ([]float32), got %T", v)
}

// vectorDistance computes the distance between two vectors under the
// requested pgvector operator. NaN return signals one side was nil
// (PG would return NULL, which the caller treats as "no match" in
// ORDER BY contexts and "no value" in projection).
//
// Semantics mirror pgvector exactly:
//   - cosine: 1 − cos(a, b)  ∈ [0, 2]
//   - L2: Euclidean distance
//   - IP: negative inner product (smaller = more similar)
func vectorDistance(a, b []float32, op simsql.VectorOp) float64 {
	if len(a) != len(b) {
		return 0
	}
	switch op {
	case simsql.VecCosine:
		var dot, na, nb float64
		for i := range a {
			fa, fb := float64(a[i]), float64(b[i])
			dot += fa * fb
			na += fa * fa
			nb += fb * fb
		}
		if na == 0 || nb == 0 {
			return 1
		}
		cos := dot / (math.Sqrt(na) * math.Sqrt(nb))
		return 1 - cos
	case simsql.VecL2:
		var sum float64
		for i := range a {
			d := float64(a[i]) - float64(b[i])
			sum += d * d
		}
		return math.Sqrt(sum)
	case simsql.VecIP:
		var dot float64
		for i := range a {
			dot += float64(a[i]) * float64(b[i])
		}
		return -dot
	}
	return 0
}

// resolveJsonPath walks the JSONB value down the supplied path. The
// stored value can be []byte (parsed lazily) or an already-parsed
// any tree. AsText controls whether the terminal access returns a
// raw string (PG's ->> semantics) or the structured subtree (->).
//
// PG-faithful behaviors:
//   - Missing keys → nil (treated as SQL NULL).
//   - ->> on a non-string scalar returns the JSON-text form
//     (`123` → "123", `true` → "true", etc.).
func resolveJsonPath(v any, path []string, asText bool) (any, error) {
	var node any
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		if err := json.Unmarshal(x, &node); err != nil {
			return nil, fmt.Errorf("sandbox exec: JSONB parse: %w", err)
		}
	case string:
		// Accept JSON in a string too — handy for tests that bind a
		// literal rather than constructing a []byte.
		if err := json.Unmarshal([]byte(x), &node); err != nil {
			return nil, fmt.Errorf("sandbox exec: JSONB parse: %w", err)
		}
	default:
		node = v
	}
	for _, k := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, nil
		}
		node, ok = m[k]
		if !ok {
			return nil, nil
		}
	}
	if !asText {
		return node, nil
	}
	if node == nil {
		return nil, nil
	}
	switch s := node.(type) {
	case string:
		return s, nil
	case float64:
		return fmt.Sprintf("%g", s), nil
	case bool:
		if s {
			return "true", nil
		}
		return "false", nil
	}
	// Composites: stringify via Marshal so the agent sees JSON text.
	b, err := json.Marshal(node)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// pkKeyFromValues mirrors Table.PKKey's encoding for caller-supplied
// PK values (rather than reading them out of a row).
func pkKeyFromValues(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprintf("%v", v)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out = out + "\x00" + p
	}
	return out
}

// matchesPK returns the PK column values if `preds` contains an
// equality predicate per PK column. Returns (nil, false) when no
// clean PK match is possible — the caller falls back to a scan.
//
// Group/IS-NULL/ANY/non-equality predicates do not count toward PK
// matching; the executor handles those during the post-PK filter pass.
func matchesPK(desc *TableDesc, preds []simsql.Pred, env *execEnv) ([]any, bool, error) {
	if len(desc.PKCols) == 0 {
		return nil, false, nil
	}
	pkVals := make([]any, len(desc.PKCols))
	matched := make([]bool, len(desc.PKCols))
	for _, p := range preds {
		eq, ok := p.(simsql.CmpPred)
		if !ok || eq.Op != simsql.OpEq {
			continue
		}
		for i, pk := range desc.PKCols {
			if eq.Column == pk {
				v, err := env.evalExpr(eq.Value)
				if err != nil {
					return nil, false, err
				}
				pkVals[i] = v
				matched[i] = true
			}
		}
	}
	for _, m := range matched {
		if !m {
			return nil, false, nil
		}
	}
	return pkVals, true, nil
}

// evalPreds returns true if the row satisfies every predicate. The
// executor scans every row and calls this; it is the conjunction
// evaluator the WHERE clause depends on.
func evalPreds(desc *TableDesc, row Row, preds []simsql.Pred, env *execEnv) (bool, error) {
	for _, p := range preds {
		ok, err := evalPred(desc, row, p, env)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func evalPred(desc *TableDesc, row Row, p simsql.Pred, env *execEnv) (bool, error) {
	switch v := p.(type) {
	case simsql.CmpPred:
		idx := desc.ColIndex(v.Column)
		if idx < 0 {
			return false, fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Column, desc.Qualified())
		}
		want, err := env.evalExpr(v.Value)
		if err != nil {
			return false, err
		}
		return compareWithOp(row[idx], v.Op, want), nil
	case simsql.GroupPred:
		// Recursive evaluator. ConnAnd: all must hold; ConnOr: at least
		// one must. Either way, a column-not-found inside a sub-pred
		// short-circuits with the underlying error.
		switch v.Connective {
		case simsql.ConnAnd:
			for _, sub := range v.Preds {
				ok, err := evalPred(desc, row, sub, env)
				if err != nil {
					return false, err
				}
				if !ok {
					return false, nil
				}
			}
			return true, nil
		case simsql.ConnOr:
			for _, sub := range v.Preds {
				ok, err := evalPred(desc, row, sub, env)
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
			return false, nil
		}
		return false, fmt.Errorf("sandbox exec: unknown group connective %d", v.Connective)
	case simsql.IsNullPred:
		idx := desc.ColIndex(v.Column)
		if idx < 0 {
			return false, fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Column, desc.Qualified())
		}
		isNull := row[idx] == nil
		if v.Not {
			return !isNull, nil
		}
		return isNull, nil
	case simsql.AnyPred:
		idx := desc.ColIndex(v.Column)
		if idx < 0 {
			return false, fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Column, desc.Qualified())
		}
		arr, err := env.arg(v.Arg.N)
		if err != nil {
			return false, err
		}
		return anyContains(arr, row[idx])
	case simsql.JsonExtractPred:
		idx := desc.ColIndex(v.Column)
		if idx < 0 {
			return false, fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Column, desc.Qualified())
		}
		lhs, err := resolveJsonPath(row[idx], v.Path, v.AsText)
		if err != nil {
			return false, err
		}
		want, err := env.evalExpr(v.Value)
		if err != nil {
			return false, err
		}
		return compareWithOp(lhs, v.Op, want), nil
	}
	return false, fmt.Errorf("sandbox exec: unknown predicate type %T", p)
}

// equalValues compares two Go values for SQL equality. Nil on either
// side never equals — matches PG's NULL-as-unknown semantics. Otherwise
// we rely on Go's == via interface comparison, which works for the
// scalar types stored in the sim (int64, string, bool, time.Time).
func equalValues(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	// time.Time needs .Equal — interface == compares pointers, not values.
	if ta, ok := a.(time.Time); ok {
		if tb, ok := b.(time.Time); ok {
			return ta.Equal(tb)
		}
		return false
	}
	return a == b
}

// compareWithOp evaluates `a <op> b` under SQL semantics. NULL on
// either side always yields FALSE — matching PG's three-valued logic
// where comparisons with NULL produce UNKNOWN, which the surrounding
// boolean context treats as "not true." Codegen-emitted predicates
// always pair non-NULL bind values, so this rule never silently hides
// data on the keyset path.
func compareWithOp(a any, op simsql.CmpOp, b any) bool {
	if a == nil || b == nil {
		return false
	}
	c := compareValues(a, b)
	switch op {
	case simsql.OpEq:
		return c == 0
	case simsql.OpNE:
		return c != 0
	case simsql.OpLT:
		return c < 0
	case simsql.OpLE:
		return c <= 0
	case simsql.OpGT:
		return c > 0
	case simsql.OpGE:
		return c >= 0
	}
	return false
}

// anyContains checks whether `arr` (an interface wrapping a slice) holds
// `target`. Supports []int64, []string, []bool — the shapes
// shapes codegen-emitted BatchGet uses.
func anyContains(arr, target any) (bool, error) {
	switch s := arr.(type) {
	case []int64:
		t, ok := target.(int64)
		if !ok {
			return false, nil
		}
		for _, v := range s {
			if v == t {
				return true, nil
			}
		}
		return false, nil
	case []string:
		t, ok := target.(string)
		if !ok {
			return false, nil
		}
		for _, v := range s {
			if v == t {
				return true, nil
			}
		}
		return false, nil
	case []bool:
		t, ok := target.(bool)
		if !ok {
			return false, nil
		}
		for _, v := range s {
			if v == t {
				return true, nil
			}
		}
		return false, nil
	case []any:
		for _, v := range s {
			if equalValues(v, target) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("%w: ANY argument type %T not supported",
		simsql.ErrUnsupported, arr)
}

// execInsert handles INSERT INTO ... VALUES (...) [ON CONFLICT ...]
// [RETURNING ...]. Returns the rows-affected count, the row to
// project for RETURNING (the existing row under DO NOTHING; the
// updated row under DO UPDATE; the inserted row otherwise), and the
// descriptor for scan dispatch.
func execInsert(ins *simsql.Insert, env *execEnv) (int64, Row, *TableDesc, error) {
	tbl, err := env.table(ins.Table)
	if err != nil {
		return 0, nil, nil, err
	}
	desc := tbl.Desc

	row := make(Row, len(desc.Cols))
	for i, col := range ins.Cols {
		idx := desc.ColIndex(col)
		if idx < 0 {
			return 0, nil, nil, fmt.Errorf("%w: %s on %s", ErrColumnNotFound, col, desc.Qualified())
		}
		v, err := env.evalExpr(ins.Args[i])
		if err != nil {
			return 0, nil, nil, err
		}
		row[idx] = v
	}

	// Without an ON CONFLICT clause, the legacy path: insert and let
	// the PK-conflict error bubble up.
	if ins.OnConflict == simsql.ConflictNone {
		if _, err := tbl.Insert(row); err != nil {
			return 0, nil, nil, err
		}
		return 1, row, desc, nil
	}

	// Conflict target must match the table's PK. Future
	// secondary-unique support extends this to declared UniqueSpecs.
	if !sameColumnSet(ins.ConflictTarget, desc.PKCols) {
		return 0, nil, nil, fmt.Errorf("%w: ON CONFLICT target must match PK columns (got %v, PK %v)",
			simsql.ErrUnsupported, ins.ConflictTarget, desc.PKCols)
	}

	// Compute the would-be PK key from the proposed row and look up
	// any existing collision.
	key, err := tbl.PKKey(row)
	if err != nil {
		return 0, nil, nil, err
	}
	existing := tbl.Get(key)
	if existing == nil {
		// No conflict — proceed as a regular insert.
		if _, err := tbl.Insert(row); err != nil {
			return 0, nil, nil, err
		}
		return 1, row, desc, nil
	}

	switch ins.OnConflict {
	case simsql.ConflictNothing:
		// Insertion is silently skipped; return the existing row for
		// RETURNING so the caller sees the durable row. PG's actual
		// RETURNING semantics on DO NOTHING return zero rows; we
		// chose the existing-row form because every codegen-emitted
		// upsert pattern uses RETURNING to retrieve the canonical PK
		// regardless of whether the row was new or pre-existing.
		return 0, existing, desc, nil
	case simsql.ConflictUpdate:
		// Resolve SET assignments against an env that sees the
		// proposed row through EXCLUDED.col.
		updEnv := *env
		updEnv.proposedRow = row
		updEnv.proposedDesc = desc
		patch := make(map[string]any, len(ins.ConflictUpdates))
		for _, a := range ins.ConflictUpdates {
			if desc.ColIndex(a.Column) < 0 {
				return 0, nil, nil, fmt.Errorf("%w: ON CONFLICT update column %s on %s",
					ErrColumnNotFound, a.Column, desc.Qualified())
			}
			if desc.IsPKCol(a.Column) {
				return 0, nil, nil, fmt.Errorf("sandbox exec: ON CONFLICT DO UPDATE cannot reassign PK column %q", a.Column)
			}
			v, err := updEnv.evalExpr(a.Value)
			if err != nil {
				return 0, nil, nil, err
			}
			patch[a.Column] = v
		}
		affected := tbl.Update(key, patch)
		// Read the now-updated row for RETURNING.
		updated := tbl.Get(key)
		return affected, updated, desc, nil
	}
	return 0, nil, nil, fmt.Errorf("sandbox exec: unknown conflict action %d", ins.OnConflict)
}

// sameColumnSet reports whether two column lists name the same set
// of columns, regardless of order. Used by ON CONFLICT to verify the
// target matches the PK declaration.
func sameColumnSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
		if seen[x] < 0 {
			return false
		}
	}
	return true
}

// evalIntExpr evaluates an Expr expected to produce an int64 value
// (LIMIT and OFFSET both want one). Accepts placeholders that bind to
// integer args and inline integer literals. Returns a contextual error
// when the value isn't an integer (e.g. user wrote LIMIT 'abc').
func evalIntExpr(env *execEnv, e simsql.Expr, ctx string) (int64, error) {
	v, err := env.evalExpr(e)
	if err != nil {
		return 0, err
	}
	n, ok := toInt64(v)
	if !ok {
		return 0, fmt.Errorf("sandbox exec: %s must be int, got %T", ctx, v)
	}
	return n, nil
}

// expandStarProjections walks the projection list and rewrites any
// Star projection into one bare-column Projection per declared catalog
// column. Returns the input slice unchanged when no Star is present,
// so the caller can avoid allocating on the common non-star path.
//
// Column order mirrors the catalog's declared order, which itself
// mirrors codegen's emission order — keeping output deterministic.
func expandStarProjections(in []simsql.Projection, desc *TableDesc) []simsql.Projection {
	hasStar := false
	for _, p := range in {
		if p.IsStar() {
			hasStar = true
			break
		}
	}
	if !hasStar {
		return in
	}
	out := make([]simsql.Projection, 0, len(in)+len(desc.Cols))
	for _, p := range in {
		if !p.IsStar() {
			out = append(out, p)
			continue
		}
		for _, c := range desc.Cols {
			out = append(out, simsql.Projection{Column: c.Name})
		}
	}
	return out
}

// execSelectNoFrom handles SELECT with no FROM clause: a single
// synthetic row whose values come from evaluating each projection
// expression once. WHERE / ORDER BY / LIMIT / OFFSET aren't meaningful
// without a table — reject those explicitly so users get a clean error
// rather than a confused empty-table behaviour.
func execSelectNoFrom(sel *simsql.Select, env *execEnv) ([][]any, int64, *TableDesc, error) {
	if len(sel.Where) > 0 {
		return nil, 0, nil, fmt.Errorf("%w: WHERE without FROM", simsql.ErrUnsupported)
	}
	if len(sel.OrderBy) > 0 {
		return nil, 0, nil, fmt.Errorf("%w: ORDER BY without FROM", simsql.ErrUnsupported)
	}
	if sel.Limit != nil || sel.Offset != nil {
		return nil, 0, nil, fmt.Errorf("%w: LIMIT/OFFSET without FROM", simsql.ErrUnsupported)
	}
	row := make([]any, len(sel.Cols))
	for i, p := range sel.Cols {
		switch {
		case p.IsStar():
			return nil, 0, nil, fmt.Errorf("%w: SELECT * without FROM", simsql.ErrUnsupported)
		case p.IsWindowCount():
			return nil, 0, nil, fmt.Errorf("%w: window function without FROM", simsql.ErrUnsupported)
		case p.IsExpr():
			val, err := env.evalExpr(p.Expr)
			if err != nil {
				return nil, 0, nil, err
			}
			row[i] = val
		default:
			return nil, 0, nil, fmt.Errorf("%w: bare column %q without FROM", ErrColumnNotFound, p.Column)
		}
	}
	return [][]any{row}, 1, nil, nil
}

// execSelect handles all Phase-1 SELECT shapes. The executor picks the
// PK-fast-path when WHERE matches every PK column exactly; otherwise it
// scans the table, filters, sorts, and slices for LIMIT/OFFSET.
//
// Returns the projected rows already laid out as projection-ordered
// slices, the total-row count (post-WHERE, pre-LIMIT) for window-COUNT,
// and the descriptor for scan-target dispatch.
func execSelect(sel *simsql.Select, env *execEnv) ([][]any, int64, *TableDesc, error) {
	// SELECT without FROM (e.g. "SELECT 1", "SELECT now()") — evaluate
	// each projection once against an empty row environment and return
	// a single synthetic row. Matches PG's behaviour. We never produce
	// a TableDesc here because the catalog has no entry for "no table";
	// scanProjected ignores desc for projection-driven dispatch.
	if sel.Table.Schema == "" && sel.Table.Name == "" {
		return execSelectNoFrom(sel, env)
	}

	tbl, err := env.table(sel.Table)
	if err != nil {
		return nil, 0, nil, err
	}
	desc := tbl.Desc

	// Expand `SELECT *` into one bare-column Projection per declared
	// catalog column. We mutate sel.Cols in place because each Parse()
	// call produces a fresh Stmt; the caller (pool.Query/QueryRow)
	// reads sel.Cols AFTER this call to build the row cursor, so the
	// expansion needs to be visible there.
	sel.Cols = expandStarProjections(sel.Cols, desc)

	// Validate projections up front. Bare-column projections need a
	// real column; Expr and window-count projections defer validation
	// to evaluation (Expr may reference columns or use literals).
	for _, p := range sel.Cols {
		if p.IsWindowCount() || p.IsExpr() {
			continue
		}
		if desc.ColIndex(p.Column) < 0 {
			return nil, 0, nil, fmt.Errorf("%w: %s on %s", ErrColumnNotFound, p.Column, desc.Qualified())
		}
	}

	// Validate WHERE column references early. GroupPred recurses;
	// IS-NULL/ANY/Cmp all carry a column name we can hoist-check.
	if err := validateWhereCols(desc, sel.Where); err != nil {
		return nil, 0, nil, err
	}

	// Try the PK-fast-path.
	rows, err := selectRows(tbl, sel.Where, env)
	if err != nil {
		return nil, 0, nil, err
	}

	// ORDER BY: multi-column, per-column ASC/DESC + explicit NULLS
	// FIRST/LAST. PG defaults to NULLS LAST on ASC, NULLS FIRST on
	// DESC; the comparator below honors that when oc.Nulls is
	// NullsDefault. Sort is stable so identical-key rows keep insertion
	// order (matches what a btree-indexed scan would produce).
	//
	// Each OrderByCol may carry an Expr (vector distance or
	// JSONB extract) instead of a bare Column. We materialize the
	// per-row sort key once via evalExpr to avoid O(N log N) repeated
	// expression work, and sort an index permutation rather than rows
	// directly so the precomputed keys stay positionally aligned.
	if len(sel.OrderBy) > 0 {
		indices := make([]int, len(sel.OrderBy))
		for i, oc := range sel.OrderBy {
			if oc.Expr != nil {
				indices[i] = -1
				continue
			}
			idx := desc.ColIndex(oc.Column)
			if idx < 0 {
				return nil, 0, nil, fmt.Errorf("%w: ORDER BY %s on %s",
					ErrColumnNotFound, oc.Column, desc.Qualified())
			}
			indices[i] = idx
		}
		keys := make([][]any, len(rows))
		for ri, row := range rows {
			k := make([]any, len(sel.OrderBy))
			for j, oc := range sel.OrderBy {
				if oc.Expr == nil {
					k[j] = row[indices[j]]
					continue
				}
				rowEnv := *env
				rowEnv.scanRow = row
				rowEnv.scanDesc = desc
				val, err := rowEnv.evalExpr(oc.Expr)
				if err != nil {
					return nil, 0, nil, err
				}
				k[j] = val
			}
			keys[ri] = k
		}
		perm := make([]int, len(rows))
		for i := range perm {
			perm[i] = i
		}
		sort.SliceStable(perm, func(i, j int) bool {
			for k, oc := range sel.OrderBy {
				cmp := compareOrdered(keys[perm[i]][k], keys[perm[j]][k], oc)
				if cmp != 0 {
					return cmp < 0
				}
			}
			return false
		})
		sortedRows := make([]Row, len(rows))
		for i, p := range perm {
			sortedRows[i] = rows[p]
		}
		rows = sortedRows
	}

	total := int64(len(rows))

	// LIMIT / OFFSET applied AFTER sort and AFTER the total snapshot
	// (window-COUNT projects the pre-paging count). Both fields are
	// Expr — most commonly Placeholder ($N), but also integer literals
	// ("LIMIT 5"). evalExpr handles both uniformly.
	off := int64(0)
	if sel.Offset != nil {
		n, err := evalIntExpr(env, sel.Offset, "OFFSET")
		if err != nil {
			return nil, 0, nil, err
		}
		off = n
	}
	lim := int64(len(rows))
	if sel.Limit != nil {
		n, err := evalIntExpr(env, sel.Limit, "LIMIT")
		if err != nil {
			return nil, 0, nil, err
		}
		lim = n
	}
	if off > int64(len(rows)) {
		rows = nil
	} else {
		rows = rows[off:]
	}
	if int64(len(rows)) > lim {
		rows = rows[:lim]
	}

	// Project to caller column order. For window-count projections,
	// the same value (total) appears in every output row — that's
	// exactly what PG's COUNT(*) OVER () produces. Expression
	// projections (vector distance / JSONB extract) evaluate against
	// the live row.
	out := make([][]any, len(rows))
	for i, row := range rows {
		projected, err := projectRowWithExpr(desc, row, sel.Cols, total, env)
		if err != nil {
			return nil, 0, nil, err
		}
		out[i] = projected
	}
	return out, total, desc, nil
}

// selectRows applies WHERE to the table, using the PK-fast-path when
// possible and a scan otherwise. Returns the filtered rows in insertion
// order; downstream ORDER BY handles final ordering.
func selectRows(tbl *Table, where []simsql.Pred, env *execEnv) ([]Row, error) {
	desc := tbl.Desc

	// If WHERE has at least one PK-equality predicate per PK column AND
	// nothing else, we can look up the single row directly. With extra
	// predicates (the soft-delete IS NULL filter is the common case),
	// we still take the fast path but verify the row passes the
	// remaining predicates.
	if pkVals, hit, err := matchesPK(desc, where, env); err != nil {
		return nil, err
	} else if hit {
		row := tbl.Get(pkKeyFromValues(pkVals))
		if row == nil {
			return nil, nil
		}
		// Codegen routinely appends `AND soft_delete IS NULL`
		// to PK lookups for soft-delete entities. We re-run all preds
		// against the row to honor those; the cost is bounded by the
		// predicate count (small).
		ok, err := evalPreds(desc, row, where, env)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		return []Row{row}, nil
	}

	// Full scan path. tbl.Scan returns rows in insertion order.
	var out []Row
	err := tbl.Scan(func(row Row) error {
		ok, err := evalPreds(desc, row, where, env)
		if err != nil {
			return err
		}
		if ok {
			// Copy so callers can mutate output (e.g. via assign during
			// projection) without leaking back into table storage.
			cp := make(Row, len(row))
			copy(cp, row)
			out = append(out, cp)
		}
		return nil
	})
	return out, err
}

// projectRowWithExpr renders a row down to its projected outputs.
// Three projection shapes:
//
//   - WindowCount: same precomputed total goes into every row.
//   - Expr: the expression evaluates against the live scanRow.
//   - Bare Column: a direct positional read.
//
// Threads scan context into env so VectorDistance and JsonExtract can
// reach the current row.
func projectRowWithExpr(desc *TableDesc, row Row, projs []simsql.Projection, total int64, env *execEnv) ([]any, error) {
	out := make([]any, len(projs))
	for i, p := range projs {
		switch {
		case p.IsWindowCount():
			out[i] = total
		case p.IsExpr():
			rowEnv := *env
			rowEnv.scanRow = row
			rowEnv.scanDesc = desc
			val, err := rowEnv.evalExpr(p.Expr)
			if err != nil {
				return nil, err
			}
			out[i] = val
		default:
			out[i] = row[desc.ColIndex(p.Column)]
		}
	}
	return out, nil
}

// execUpdate handles UPDATE ... SET ... [WHERE ...]. WHERE is
// required — unbounded UPDATEs are rejected so a misspelled key in a
// soft-delete path can't silently rewrite the whole table. rowsAffected matches what CommandTag.RowsAffected() will report.
func execUpdate(upd *simsql.Update, env *execEnv) (int64, error) {
	tbl, err := env.table(upd.Table)
	if err != nil {
		return 0, err
	}
	desc := tbl.Desc

	if len(upd.Where) == 0 {
		return 0, fmt.Errorf("%w: UPDATE without WHERE not supported", simsql.ErrUnsupported)
	}

	// Pre-evaluate SET values once — they don't depend on the row being
	// updated — SET values are pure functions of bind args, not of the row being updated.
	patch := make(map[string]any, len(upd.Set))
	for _, a := range upd.Set {
		if desc.ColIndex(a.Column) < 0 {
			return 0, fmt.Errorf("%w: %s on %s", ErrColumnNotFound, a.Column, desc.Qualified())
		}
		if desc.IsPKCol(a.Column) {
			return 0, fmt.Errorf("sandbox exec: UPDATE SET cannot include PK column %q", a.Column)
		}
		v, err := env.evalExpr(a.Value)
		if err != nil {
			return 0, err
		}
		patch[a.Column] = v
	}

	// PK-fast-path. With extra predicates (soft-delete) we still want
	// the fast path but re-check remaining preds against the row.
	if pkVals, hit, err := matchesPK(desc, upd.Where, env); err != nil {
		return 0, err
	} else if hit {
		key := pkKeyFromValues(pkVals)
		row := tbl.Get(key)
		if row == nil {
			return 0, nil
		}
		ok, err := evalPreds(desc, row, upd.Where, env)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, nil
		}
		return tbl.Update(key, patch), nil
	}

	// Scan path: visit every row, filter by predicates, update matches.
	var affected int64
	err = tbl.ScanKeyed(func(key string, row Row) error {
		ok, err := evalPreds(desc, row, upd.Where, env)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		affected += tbl.Update(key, patch)
		return nil
	})
	return affected, err
}

// execDelete handles DELETE FROM ... [WHERE ...]. Refuses unbounded
// DELETEs for the same reason as UPDATE; soft-delete is its own UPDATE
// path, not DELETE.
func execDelete(del *simsql.Delete, env *execEnv) (int64, error) {
	tbl, err := env.table(del.Table)
	if err != nil {
		return 0, err
	}
	desc := tbl.Desc

	if len(del.Where) == 0 {
		return 0, fmt.Errorf("%w: DELETE without WHERE not supported", simsql.ErrUnsupported)
	}

	if pkVals, hit, err := matchesPK(desc, del.Where, env); err != nil {
		return 0, err
	} else if hit {
		key := pkKeyFromValues(pkVals)
		row := tbl.Get(key)
		if row == nil {
			return 0, nil
		}
		ok, err := evalPreds(desc, row, del.Where, env)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, nil
		}
		return tbl.Delete(key), nil
	}

	// Scan path. We collect keys first so we don't mutate during scan.
	var keysToDelete []string
	err = tbl.ScanKeyed(func(key string, row Row) error {
		ok, err := evalPreds(desc, row, del.Where, env)
		if err != nil {
			return err
		}
		if ok {
			keysToDelete = append(keysToDelete, key)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	var affected int64
	for _, k := range keysToDelete {
		affected += tbl.Delete(k)
	}
	return affected, nil
}

// compareOrdered returns -1/0/+1 for "a comes before b under oc."
// Honors per-column direction (DESC inverts) and NULLS handling. The
// per-column return enables the multi-column tiebreak loop in
// execSelect to short-circuit on the first non-zero column.
//
// PG NULL ordering defaults: ASC = NULLS LAST, DESC = NULLS FIRST.
// Explicit NULLS FIRST/LAST overrides those defaults unconditionally.
func compareOrdered(a, b any, oc simsql.OrderByCol) int {
	aNil := a == nil
	bNil := b == nil
	if aNil && bNil {
		return 0
	}
	if aNil || bNil {
		// One side nil; decide which sorts first per NULLS policy.
		nullsFirst := false
		switch oc.Nulls {
		case simsql.NullsFirst:
			nullsFirst = true
		case simsql.NullsLast:
			nullsFirst = false
		default:
			nullsFirst = oc.Desc // PG default: NULLS FIRST under DESC
		}
		// If aNil and NULLS first → a < b → return -1.
		if aNil {
			if nullsFirst {
				return -1
			}
			return 1
		}
		// bNil
		if nullsFirst {
			return 1
		}
		return -1
	}
	c := compareValues(a, b)
	if oc.Desc {
		return -c
	}
	return c
}

// validateWhereCols hoists column-not-found errors out of evalPred so
// they surface before any scan work. Walks GroupPred recursively.
func validateWhereCols(desc *TableDesc, preds []simsql.Pred) error {
	for _, p := range preds {
		switch v := p.(type) {
		case simsql.CmpPred:
			if desc.ColIndex(v.Column) < 0 {
				return fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Column, desc.Qualified())
			}
		case simsql.IsNullPred:
			if desc.ColIndex(v.Column) < 0 {
				return fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Column, desc.Qualified())
			}
		case simsql.AnyPred:
			if desc.ColIndex(v.Column) < 0 {
				return fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Column, desc.Qualified())
			}
		case simsql.JsonExtractPred:
			if desc.ColIndex(v.Column) < 0 {
				return fmt.Errorf("%w: %s on %s", ErrColumnNotFound, v.Column, desc.Qualified())
			}
		case simsql.GroupPred:
			if err := validateWhereCols(desc, v.Preds); err != nil {
				return err
			}
		}
	}
	return nil
}

// compareValues returns -1, 0, 1 for a<b, a==b, a>b. Supports the
// scalar types stored in the sim, including float64/float32 which
// arrive via vector-distance expression results. Returns 0 on type
// mismatch so the sort is stable for heterogeneous values (which the
// executor never produces in practice).
func compareValues(a, b any) int {
	switch av := a.(type) {
	case int64:
		if bv, ok := b.(int64); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			}
			return 0
		}
	case float64:
		if bv, ok := b.(float64); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			}
			return 0
		}
	case float32:
		if bv, ok := b.(float32); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			}
			return 0
		}
	case string:
		if bv, ok := b.(string); ok {
			return strings.Compare(av, bv)
		}
	case bool:
		if bv, ok := b.(bool); ok {
			switch {
			case !av && bv:
				return -1
			case av && !bv:
				return 1
			}
			return 0
		}
	case time.Time:
		if bv, ok := b.(time.Time); ok {
			switch {
			case av.Before(bv):
				return -1
			case av.After(bv):
				return 1
			}
			return 0
		}
	}
	return 0
}

// toInt64 normalizes any int-like Go value (int, int32, int64, uint,
// uint32, uint64) to int64 for LIMIT/OFFSET arguments. Returns ok=false
// for non-int values.
func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case uint:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		return int64(x), true
	}
	return 0, false
}

// scanInto writes the named columns from a projected row into the
// caller-supplied scan destinations. labels[i] is the column / alias
// name; desc provides catalog-driven type info for column columns; for
// window-count aliases (which the catalog doesn't know about) the
// projector pre-assigned int64 values and we route those through the
// *int64 / *any branches of assign().
func scanProjected(desc *TableDesc, projs []simsql.Projection, row []any, dest []any) error {
	if len(projs) != len(dest) {
		return fmt.Errorf("sandbox scan: %d projections vs %d dest", len(projs), len(dest))
	}
	for i, p := range projs {
		v := row[i]
		if err := assign(dest[i], v); err != nil {
			label := p.Column
			if p.IsWindowCount() {
				label = p.WindowCountAlias
			}
			return fmt.Errorf("sandbox scan: col %q: %w", label, err)
		}
	}
	_ = desc // descriptor reserved for future per-column type-dispatch
	return nil
}

// scanInto writes the named columns from a row (laid out per the
// descriptor) into the caller-supplied scan destinations. Used by the
// INSERT RETURNING / DELETE RETURNING paths where the projection is
// just `[]string` of column names.
func scanInto(desc *TableDesc, row Row, cols []string, dest []any) error {
	if len(cols) != len(dest) {
		return fmt.Errorf("sandbox scan: %d cols vs %d dest", len(cols), len(dest))
	}
	for i, col := range cols {
		idx := desc.ColIndex(col)
		if idx < 0 {
			return fmt.Errorf("%w: %s on %s", ErrColumnNotFound, col, desc.Qualified())
		}
		v := row[idx]
		if err := assign(dest[i], v); err != nil {
			return fmt.Errorf("sandbox scan: col %q: %w", col, err)
		}
	}
	return nil
}

// assign writes v into the location dst points at. Supports the
// common Go scalar pointer types codegen-emitted scans use. NULL values
// (v == nil) yield the zero value of the target type.
func assign(dst any, v any) error {
	switch p := dst.(type) {
	case *any:
		*p = v
	case *int64:
		if v == nil {
			*p = 0
			return nil
		}
		switch x := v.(type) {
		case int64:
			*p = x
		case int:
			*p = int64(x)
		case int32:
			*p = int64(x)
		default:
			return fmt.Errorf("cannot assign %T to *int64", v)
		}
	case *string:
		if v == nil {
			*p = ""
			return nil
		}
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("cannot assign %T to *string", v)
		}
		*p = s
	case *bool:
		if v == nil {
			*p = false
			return nil
		}
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("cannot assign %T to *bool", v)
		}
		*p = b
	case *time.Time:
		if v == nil {
			*p = time.Time{}
			return nil
		}
		t, ok := v.(time.Time)
		if !ok {
			return fmt.Errorf("cannot assign %T to *time.Time", v)
		}
		*p = t
	case *[]byte:
		if v == nil {
			*p = nil
			return nil
		}
		b, ok := v.([]byte)
		if !ok {
			return fmt.Errorf("cannot assign %T to *[]byte", v)
		}
		*p = b
	case *[]string:
		if v == nil {
			*p = nil
			return nil
		}
		if s, ok := v.([]string); ok {
			*p = s
			return nil
		}
		// Accept []any holding strings — JSON args arrive that way
		if s, ok := v.([]any); ok {
			out := make([]string, len(s))
			for i, e := range s {
				str, ok := e.(string)
				if !ok {
					return fmt.Errorf("array element %d is %T, want string", i, e)
				}
				out[i] = str
			}
			*p = out
			return nil
		}
		return fmt.Errorf("cannot assign %T to *[]string", v)
	case *[]int64:
		if v == nil {
			*p = nil
			return nil
		}
		if s, ok := v.([]int64); ok {
			*p = s
			return nil
		}
		if s, ok := v.([]any); ok {
			out := make([]int64, len(s))
			for i, e := range s {
				n, ok := toInt64(e)
				if !ok {
					return fmt.Errorf("array element %d is %T, want int", i, e)
				}
				out[i] = n
			}
			*p = out
			return nil
		}
		return fmt.Errorf("cannot assign %T to *[]int64", v)
	default:
		return fmt.Errorf("unsupported scan target %T", dst)
	}
	return nil
}
