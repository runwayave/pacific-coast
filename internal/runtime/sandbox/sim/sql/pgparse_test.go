// pgparse_test covers the translator from end to end. The structure is
// three layers of tables:
//
//   1. parseOK — input → AST shape we expect (verified via reflect.DeepEqual)
//   2. parseErr — input → must wrap ErrUnsupported (or carry a specific message)
//   3. integration — full Parse → Stmt happy-path smoke tests for each kind
//
// Every edge case that surfaced during the migration plan's adversarial
// review is pinned here so a future regression in pg_query_go (or in
// the translator) trips a focused test rather than a UI-visible failure.

package sql

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// ─────────────────────────── helpers ───────────────────────────

// parseStmt is a thin wrapper that fails the test on any error.
// Use this when the test asserts on AST shape; the err path has its
// own table.
func parseStmt(t *testing.T, src string) Stmt {
	t.Helper()
	got, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", src, err)
	}
	return got
}

// assertUnsupported confirms err wraps ErrUnsupported AND that its
// message contains the expected substring. The substring check is
// load-bearing — operators read these messages directly.
func assertUnsupported(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected ErrUnsupported, got nil")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v; want errors.Is ErrUnsupported", err)
	}
	if wantSubstr != "" && !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("err = %q; want substring %q", err.Error(), wantSubstr)
	}
}

// ─────────────────────────── entry-point edge cases ───────────────────────────

func TestParse_EmptyInput(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"\n\n",
		"-- just a comment",
		"-- comment one\n-- comment two\n",
		"/* block comment */",
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			_, err := Parse(src)
			assertUnsupported(t, err, "empty SQL")
		})
	}
}

func TestParse_MultiStatementRejected(t *testing.T) {
	cases := []string{
		"SELECT 1; SELECT 2;",
		"SELECT 1; SELECT 2",
		"INSERT INTO \"s\".\"t\" (a) VALUES ($1); SELECT 1;",
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			_, err := Parse(src)
			assertUnsupported(t, err, "multiple statements")
		})
	}
}

func TestParse_TrailingSemicolonOK(t *testing.T) {
	// pg_query treats trailing `;` as a statement terminator; the result
	// is still len(Stmts)==1. Important because every codegen-emitted
	// query and most user inputs end this way.
	if _, err := Parse("SELECT 1;"); err != nil {
		t.Fatalf("SELECT 1; should parse, got %v", err)
	}
}

func TestParse_LeadingCommentsOK(t *testing.T) {
	// Comments before the active SELECT must not confuse the translator.
	src := `-- intro line
-- another line
SELECT 1`
	if _, err := Parse(src); err != nil {
		t.Fatalf("comments + SELECT should parse, got %v", err)
	}
}

func TestParse_SyntaxErrorWrapped(t *testing.T) {
	// pg_query's own syntax-error message gets wrapped with ErrUnsupported
	// so callers can branch on errors.Is.
	_, err := Parse("SELECT FROM WHERE")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("syntax error should wrap ErrUnsupported, got %v", err)
	}
}

// ─────────────────────────── SELECT ───────────────────────────

func TestSelect_NoFrom(t *testing.T) {
	stmt := parseStmt(t, "SELECT 1")
	sel, ok := stmt.(*Select)
	if !ok {
		t.Fatalf("got %T, want *Select", stmt)
	}
	if sel.Table.Schema != "" || sel.Table.Name != "" {
		t.Fatalf("no-FROM SELECT should have zero TableRef, got %+v", sel.Table)
	}
	if len(sel.Cols) != 1 {
		t.Fatalf("want 1 projection, got %d", len(sel.Cols))
	}
	if !sel.Cols[0].IsExpr() {
		t.Fatalf("SELECT 1 projection should be Expr, got %+v", sel.Cols[0])
	}
	lit, ok := sel.Cols[0].Expr.(Literal)
	if !ok || lit.Kind != LitInt64 || lit.Int64 != 1 {
		t.Fatalf("projection expr = %+v; want Literal{Int64:1}", sel.Cols[0].Expr)
	}
}

func TestSelect_NoFromMultipleExpressions(t *testing.T) {
	stmt := parseStmt(t, "SELECT 1, 'two', $1")
	sel := stmt.(*Select)
	if len(sel.Cols) != 3 {
		t.Fatalf("want 3 projections, got %d", len(sel.Cols))
	}
	want := []any{
		Literal{Kind: LitInt64, Int64: 1},
		Literal{Kind: LitString, Str: "two"},
		Placeholder{N: 1},
	}
	for i, w := range want {
		if !reflect.DeepEqual(sel.Cols[i].Expr, w) {
			t.Errorf("col %d = %+v; want %+v", i, sel.Cols[i].Expr, w)
		}
	}
}

func TestSelect_BareTableQualified(t *testing.T) {
	stmt := parseStmt(t, `SELECT "id" FROM "consumer"."accounts"`)
	sel := stmt.(*Select)
	if sel.Table.Schema != "consumer" || sel.Table.Name != "accounts" {
		t.Fatalf("table = %+v; want consumer.accounts", sel.Table)
	}
	if len(sel.Cols) != 1 || sel.Cols[0].Column != "id" {
		t.Fatalf("cols = %+v; want one bare column 'id'", sel.Cols)
	}
}

func TestSelect_WithoutSchemaNamePreserved(t *testing.T) {
	// pg_query lowercases unquoted identifiers but preserves quoted
	// ones. Our catalog is case-sensitive — make sure both shapes
	// reach the AST unaltered.
	cases := []struct {
		src        string
		wantSchema string
		wantName   string
	}{
		{`SELECT "x" FROM "Foo"."Bar"`, "Foo", "Bar"}, // quoted, original case
		{`SELECT "x" FROM "foo"."bar"`, "foo", "bar"}, // quoted, lowercase
		{`SELECT "x" FROM foo.bar`, "foo", "bar"},     // unquoted → lowercase
		{`SELECT "x" FROM Foo.Bar`, "foo", "bar"},     // unquoted → lowercase
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			sel := parseStmt(t, tc.src).(*Select)
			if sel.Table.Schema != tc.wantSchema || sel.Table.Name != tc.wantName {
				t.Errorf("got %s.%s; want %s.%s",
					sel.Table.Schema, sel.Table.Name, tc.wantSchema, tc.wantName)
			}
		})
	}
}

func TestSelect_StarParsed(t *testing.T) {
	// SELECT * now translates to a Star-marked Projection rather than
	// being rejected outright — the executor expands it against the
	// catalog at run time. Verify the AST carries the Star flag.
	sel := parseStmt(t, `SELECT * FROM "s"."t"`).(*Select)
	if len(sel.Cols) != 1 {
		t.Fatalf("want 1 projection, got %d (%+v)", len(sel.Cols), sel.Cols)
	}
	if !sel.Cols[0].IsStar() {
		t.Errorf("projection should be Star, got %+v", sel.Cols[0])
	}
}

func TestSelect_StarMixedWithExplicitColumns(t *testing.T) {
	// PG accepts `SELECT *, "extra" FROM t` — the executor expands the
	// * and appends the explicit columns. Verify both projections land.
	sel := parseStmt(t, `SELECT *, "extra" FROM "s"."t"`).(*Select)
	if len(sel.Cols) != 2 {
		t.Fatalf("want 2 projections, got %d", len(sel.Cols))
	}
	if !sel.Cols[0].IsStar() {
		t.Errorf("col[0] should be Star, got %+v", sel.Cols[0])
	}
	if sel.Cols[1].Column != "extra" {
		t.Errorf("col[1] = %+v; want bare column 'extra'", sel.Cols[1])
	}
}

func TestSelect_QualifiedStar(t *testing.T) {
	// `t.*` — qualified star. The translator collapses to a Star
	// projection; expansion happens against the single FROM table.
	sel := parseStmt(t, `SELECT "t".* FROM "s"."t"`).(*Select)
	if len(sel.Cols) != 1 || !sel.Cols[0].IsStar() {
		t.Errorf("qualified star should map to Star projection, got %+v", sel.Cols)
	}
}

func TestSelect_AsteriskInExpressionRejected(t *testing.T) {
	// Defense in depth — `*` outside a projection slot is a syntax
	// error pg_query catches; the translator just surfaces it.
	_, err := Parse(`SELECT 1 FROM "s"."t" WHERE "a" = *`)
	if err == nil {
		t.Fatalf("expected error for bare * in WHERE, got nil")
	}
}

func TestSelect_LimitOffsetPlaceholders(t *testing.T) {
	sel := parseStmt(t, `SELECT "a" FROM "s"."t" LIMIT $1 OFFSET $2`).(*Select)
	lim, ok := sel.Limit.(Placeholder)
	if !ok || lim.N != 1 {
		t.Fatalf("Limit = %+v; want Placeholder $1", sel.Limit)
	}
	off, ok := sel.Offset.(Placeholder)
	if !ok || off.N != 2 {
		t.Fatalf("Offset = %+v; want Placeholder $2", sel.Offset)
	}
}

func TestSelect_LimitOffsetIntegerLiterals(t *testing.T) {
	// PG accepts inline integer literals for LIMIT/OFFSET. Translator
	// now passes them through; executor evaluates the Expr at run time.
	sel := parseStmt(t, `SELECT "a" FROM "s"."t" LIMIT 10 OFFSET 5`).(*Select)
	lim, ok := sel.Limit.(Literal)
	if !ok || lim.Kind != LitInt64 || lim.Int64 != 10 {
		t.Fatalf("Limit = %+v; want Literal{Int64:10}", sel.Limit)
	}
	off, ok := sel.Offset.(Literal)
	if !ok || off.Kind != LitInt64 || off.Int64 != 5 {
		t.Fatalf("Offset = %+v; want Literal{Int64:5}", sel.Offset)
	}
}

func TestSelect_LimitOffsetMixed(t *testing.T) {
	// One placeholder + one literal — common when users mix typed
	// pagination ($limit) with a hardcoded offset for debugging.
	sel := parseStmt(t, `SELECT "a" FROM "s"."t" LIMIT $1 OFFSET 10`).(*Select)
	if _, ok := sel.Limit.(Placeholder); !ok {
		t.Errorf("Limit = %T; want Placeholder", sel.Limit)
	}
	if _, ok := sel.Offset.(Literal); !ok {
		t.Errorf("Offset = %T; want Literal", sel.Offset)
	}
}

func TestSelect_LimitOffsetNonNumericRejected(t *testing.T) {
	// String-literal LIMIT is grammar-valid in PG but the executor
	// can't make sense of it. Translator rejects up-front with a clear
	// error rather than passing through to a runtime "got string, want
	// int" surprise.
	cases := []string{
		`SELECT "a" FROM "s"."t" LIMIT 'ten'`,
		`SELECT "a" FROM "s"."t" OFFSET 'five'`,
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			_, err := Parse(src)
			assertUnsupported(t, err, "")
		})
	}
}

func TestSelect_OrderByVariations(t *testing.T) {
	cases := []struct {
		src      string
		wantCols []OrderByCol
	}{
		{
			`SELECT "a" FROM "s"."t" ORDER BY "a"`,
			[]OrderByCol{{Column: "a"}},
		},
		{
			`SELECT "a" FROM "s"."t" ORDER BY "a" DESC`,
			[]OrderByCol{{Column: "a", Desc: true}},
		},
		{
			`SELECT "a" FROM "s"."t" ORDER BY "a" NULLS FIRST, "b" DESC NULLS LAST`,
			[]OrderByCol{
				{Column: "a", Nulls: NullsFirst},
				{Column: "b", Desc: true, Nulls: NullsLast},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			sel := parseStmt(t, tc.src).(*Select)
			if !reflect.DeepEqual(sel.OrderBy, tc.wantCols) {
				t.Errorf("OrderBy = %+v; want %+v", sel.OrderBy, tc.wantCols)
			}
		})
	}
}

func TestSelect_WindowCount(t *testing.T) {
	sel := parseStmt(t, `SELECT "a", COUNT(*) OVER () AS total FROM "s"."t"`).(*Select)
	if len(sel.Cols) != 2 {
		t.Fatalf("want 2 projections, got %d", len(sel.Cols))
	}
	if !sel.Cols[1].IsWindowCount() || sel.Cols[1].WindowCountAlias != "total" {
		t.Fatalf("second projection should be WindowCount(total), got %+v", sel.Cols[1])
	}
}

func TestSelect_WindowCount_RequiresAlias(t *testing.T) {
	_, err := Parse(`SELECT "a", COUNT(*) OVER () FROM "s"."t"`)
	assertUnsupported(t, err, "alias")
}

func TestSelect_WindowCount_PartitionRejected(t *testing.T) {
	_, err := Parse(`SELECT COUNT(*) OVER (PARTITION BY "a") AS x FROM "s"."t"`)
	assertUnsupported(t, err, "PARTITION")
}

func TestSelect_WhereChain(t *testing.T) {
	sel := parseStmt(t, `SELECT "a" FROM "s"."t" WHERE "a" = $1 AND "b" IS NULL AND "c" = ANY($2)`).(*Select)
	if len(sel.Where) != 3 {
		t.Fatalf("want 3 AND-flattened preds, got %d (%+v)", len(sel.Where), sel.Where)
	}
	if eq, ok := sel.Where[0].(CmpPred); !ok || eq.Column != "a" || eq.Op != OpEq {
		t.Errorf("pred[0] = %+v; want CmpPred{a, =, $1}", sel.Where[0])
	}
	if isn, ok := sel.Where[1].(IsNullPred); !ok || isn.Column != "b" || isn.Not {
		t.Errorf("pred[1] = %+v; want IsNullPred{b, false}", sel.Where[1])
	}
	if any, ok := sel.Where[2].(AnyPred); !ok || any.Column != "c" || any.Arg.N != 2 {
		t.Errorf("pred[2] = %+v; want AnyPred{c, $2}", sel.Where[2])
	}
}

func TestSelect_WhereOrGroup(t *testing.T) {
	sel := parseStmt(t, `SELECT "a" FROM "s"."t" WHERE "a" = $1 OR "b" = $2`).(*Select)
	if len(sel.Where) != 1 {
		t.Fatalf("OR should yield single GroupPred wrapper, got %d preds", len(sel.Where))
	}
	gp, ok := sel.Where[0].(GroupPred)
	if !ok || gp.Connective != ConnOr {
		t.Fatalf("pred[0] = %+v; want GroupPred{OR, ...}", sel.Where[0])
	}
	if len(gp.Preds) != 2 {
		t.Fatalf("OR group should have 2 children, got %d", len(gp.Preds))
	}
}

func TestSelect_IsNotNull(t *testing.T) {
	sel := parseStmt(t, `SELECT "a" FROM "s"."t" WHERE "b" IS NOT NULL`).(*Select)
	isn := sel.Where[0].(IsNullPred)
	if !isn.Not {
		t.Errorf("IS NOT NULL should have Not=true")
	}
}

func TestSelect_CmpOps(t *testing.T) {
	// Every comparator the executor models — verify the operator string
	// maps to the right CmpOp.
	cases := []struct {
		src string
		op  CmpOp
	}{
		{`SELECT "a" FROM "s"."t" WHERE "a" = $1`, OpEq},
		{`SELECT "a" FROM "s"."t" WHERE "a" <> $1`, OpNE},
		{`SELECT "a" FROM "s"."t" WHERE "a" != $1`, OpNE},
		{`SELECT "a" FROM "s"."t" WHERE "a" < $1`, OpLT},
		{`SELECT "a" FROM "s"."t" WHERE "a" <= $1`, OpLE},
		{`SELECT "a" FROM "s"."t" WHERE "a" > $1`, OpGT},
		{`SELECT "a" FROM "s"."t" WHERE "a" >= $1`, OpGE},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			sel := parseStmt(t, tc.src).(*Select)
			got := sel.Where[0].(CmpPred).Op
			if got != tc.op {
				t.Errorf("op = %v; want %v", got, tc.op)
			}
		})
	}
}

func TestSelect_AnyWithNonEqualRejected(t *testing.T) {
	_, err := Parse(`SELECT "a" FROM "s"."t" WHERE "a" < ANY($1)`)
	assertUnsupported(t, err, "ANY")
}

// ─────────────────────────── JSON extract ───────────────────────────

func TestSelect_JsonExtractPred_SingleArrow(t *testing.T) {
	sel := parseStmt(t, `SELECT "id" FROM "s"."t" WHERE "data"->>'type' = $1`).(*Select)
	jep, ok := sel.Where[0].(JsonExtractPred)
	if !ok {
		t.Fatalf("pred[0] = %T; want JsonExtractPred", sel.Where[0])
	}
	if jep.Column != "data" {
		t.Errorf("column = %q; want data", jep.Column)
	}
	if !reflect.DeepEqual(jep.Path, []string{"type"}) {
		t.Errorf("path = %+v; want [type]", jep.Path)
	}
	if !jep.AsText {
		t.Errorf("AsText should be true for ->>")
	}
}

func TestSelect_JsonExtractPred_ChainedSubtreeThenText(t *testing.T) {
	// data -> 'a' -> 'b' ->> 'c' — chained subtree access then text-cast final.
	sel := parseStmt(t, `SELECT "id" FROM "s"."t" WHERE "data"->'a'->'b'->>'c' = $1`).(*Select)
	jep := sel.Where[0].(JsonExtractPred)
	if jep.Column != "data" {
		t.Errorf("column = %q; want data", jep.Column)
	}
	if !reflect.DeepEqual(jep.Path, []string{"a", "b", "c"}) {
		t.Errorf("path = %+v; want [a b c]", jep.Path)
	}
	if !jep.AsText {
		t.Errorf("AsText should be true for trailing ->>")
	}
}

func TestSelect_JsonExtractPred_AllSubtree(t *testing.T) {
	// data -> 'a' -> 'b' — no text-cast.
	sel := parseStmt(t, `SELECT "id" FROM "s"."t" WHERE "data"->'a'->'b' = $1`).(*Select)
	jep := sel.Where[0].(JsonExtractPred)
	if jep.AsText {
		t.Errorf("AsText should be false for all-arrow chain")
	}
	if !reflect.DeepEqual(jep.Path, []string{"a", "b"}) {
		t.Errorf("path = %+v; want [a b]", jep.Path)
	}
}

func TestSelect_JsonExtractInProjection(t *testing.T) {
	// `data->>'k' AS k_text` — JSON extract in expression position.
	sel := parseStmt(t, `SELECT "data"->>'k' AS k_text FROM "s"."t"`).(*Select)
	if !sel.Cols[0].IsExpr() {
		t.Fatalf("projection should be Expr, got %+v", sel.Cols[0])
	}
	je, ok := sel.Cols[0].Expr.(JsonExtract)
	if !ok {
		t.Fatalf("expr = %T; want JsonExtract", sel.Cols[0].Expr)
	}
	if je.Column != "data" || !je.AsText {
		t.Errorf("JsonExtract = %+v; want data + AsText", je)
	}
}

// ─────────────────────────── vector distance ───────────────────────────

func TestSelect_VectorDistanceProjectionAndOrderBy(t *testing.T) {
	cases := []struct {
		op  string
		vec VectorOp
	}{
		{"<=>", VecCosine},
		{"<->", VecL2},
		{"<#>", VecIP},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			src := `SELECT "id", "embedding" ` + tc.op + ` $1::vector AS dist FROM "s"."t" ORDER BY "embedding" ` + tc.op + ` $1::vector LIMIT $2`
			sel := parseStmt(t, src).(*Select)
			// Projection.
			if !sel.Cols[1].IsExpr() {
				t.Fatalf("col[1] should be Expr, got %+v", sel.Cols[1])
			}
			vd, ok := sel.Cols[1].Expr.(VectorDistance)
			if !ok {
				t.Fatalf("col[1].Expr = %T; want VectorDistance", sel.Cols[1].Expr)
			}
			if vd.Op != tc.vec || vd.Column != "embedding" || vd.Arg.N != 1 {
				t.Errorf("VectorDistance = %+v; want {embedding, %v, $1}", vd, tc.vec)
			}
			// ORDER BY same shape.
			if len(sel.OrderBy) != 1 || sel.OrderBy[0].Expr == nil {
				t.Fatalf("ORDER BY should carry an Expr, got %+v", sel.OrderBy)
			}
			ob, ok := sel.OrderBy[0].Expr.(VectorDistance)
			if !ok || ob.Op != tc.vec {
				t.Errorf("ORDER BY Expr = %+v; want VectorDistance with %v", sel.OrderBy[0].Expr, tc.vec)
			}
		})
	}
}

func TestSelect_VectorDistance_PG17HammingRejected(t *testing.T) {
	// PG17's pgvector adds `<+>` hamming distance. The translator
	// shouldn't silently emit a bogus VectorOp — it should reject.
	_, err := Parse(`SELECT "embedding" <+> $1::vector AS d FROM "s"."t"`)
	assertUnsupported(t, err, "")
}

// ─────────────────────────── INSERT ───────────────────────────

func TestInsert_Basic(t *testing.T) {
	stmt := parseStmt(t, `INSERT INTO "s"."t" ("a", "b") VALUES ($1, $2)`)
	ins, ok := stmt.(*Insert)
	if !ok {
		t.Fatalf("got %T, want *Insert", stmt)
	}
	if ins.Table.Schema != "s" || ins.Table.Name != "t" {
		t.Errorf("table = %+v", ins.Table)
	}
	if !reflect.DeepEqual(ins.Cols, []string{"a", "b"}) {
		t.Errorf("cols = %+v", ins.Cols)
	}
	if len(ins.Args) != 2 {
		t.Fatalf("args = %+v", ins.Args)
	}
	if ph, ok := ins.Args[0].(Placeholder); !ok || ph.N != 1 {
		t.Errorf("args[0] = %+v; want Placeholder{1}", ins.Args[0])
	}
}

func TestInsert_Returning(t *testing.T) {
	ins := parseStmt(t, `INSERT INTO "s"."t" ("a") VALUES ($1) RETURNING "a", "id"`).(*Insert)
	if !reflect.DeepEqual(ins.Returning, []string{"a", "id"}) {
		t.Errorf("returning = %+v; want [a id]", ins.Returning)
	}
}

func TestInsert_OnConflictNothing(t *testing.T) {
	ins := parseStmt(t, `INSERT INTO "s"."t" ("a") VALUES ($1) ON CONFLICT ("a") DO NOTHING`).(*Insert)
	if ins.OnConflict != ConflictNothing {
		t.Errorf("OnConflict = %v; want ConflictNothing", ins.OnConflict)
	}
	if !reflect.DeepEqual(ins.ConflictTarget, []string{"a"}) {
		t.Errorf("ConflictTarget = %+v; want [a]", ins.ConflictTarget)
	}
}

func TestInsert_OnConflictUpdateWithExcluded(t *testing.T) {
	ins := parseStmt(t,
		`INSERT INTO "s"."t" ("a", "b") VALUES ($1, $2) `+
			`ON CONFLICT ("a") DO UPDATE SET "b" = EXCLUDED."b"`,
	).(*Insert)
	if ins.OnConflict != ConflictUpdate {
		t.Errorf("OnConflict = %v; want ConflictUpdate", ins.OnConflict)
	}
	if len(ins.ConflictUpdates) != 1 {
		t.Fatalf("ConflictUpdates = %+v; want 1 entry", ins.ConflictUpdates)
	}
	upd := ins.ConflictUpdates[0]
	if upd.Column != "b" {
		t.Errorf("update col = %q; want b", upd.Column)
	}
	exc, ok := upd.Value.(ExcludedRef)
	if !ok {
		t.Fatalf("update value = %T; want ExcludedRef", upd.Value)
	}
	if exc.Column != "b" {
		t.Errorf("ExcludedRef.Column = %q; want b", exc.Column)
	}
}

func TestInsert_ExcludedDetectionLowercase(t *testing.T) {
	// pg_query lowercases unquoted identifiers. EXCLUDED must be detected
	// case-insensitively because users write it both ways.
	cases := []string{
		`INSERT INTO "s"."t" ("a") VALUES ($1) ON CONFLICT ("a") DO UPDATE SET "a" = excluded."a"`,
		`INSERT INTO "s"."t" ("a") VALUES ($1) ON CONFLICT ("a") DO UPDATE SET "a" = EXCLUDED."a"`,
		`INSERT INTO "s"."t" ("a") VALUES ($1) ON CONFLICT ("a") DO UPDATE SET "a" = Excluded."a"`,
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			ins := parseStmt(t, src).(*Insert)
			if _, ok := ins.ConflictUpdates[0].Value.(ExcludedRef); !ok {
				t.Errorf("excluded ref not detected: %+v", ins.ConflictUpdates[0].Value)
			}
		})
	}
}

func TestInsert_OnConflictConstraintRejected(t *testing.T) {
	_, err := Parse(`INSERT INTO "s"."t" ("a") VALUES ($1) ON CONFLICT ON CONSTRAINT "uq" DO NOTHING`)
	assertUnsupported(t, err, "CONSTRAINT")
}

func TestInsert_MultiRowRejected(t *testing.T) {
	_, err := Parse(`INSERT INTO "s"."t" ("a") VALUES ($1), ($2)`)
	assertUnsupported(t, err, "multi-row")
}

func TestInsert_CoalesceDefault(t *testing.T) {
	// Codegen-emitted pattern: COALESCE($1::timestamptz, now()) for
	// columns with a SQL DEFAULT.
	ins := parseStmt(t, `INSERT INTO "s"."t" ("created_at") VALUES (COALESCE($1::timestamptz, now()))`).(*Insert)
	c, ok := ins.Args[0].(Coalesce)
	if !ok {
		t.Fatalf("args[0] = %T; want Coalesce", ins.Args[0])
	}
	if c.Value.N != 1 {
		t.Errorf("Coalesce.Value = %+v; want $1", c.Value)
	}
	fn, ok := c.Default.(FuncCall)
	if !ok || fn.Name != "NOW" {
		t.Errorf("Coalesce.Default = %+v; want FuncCall{NOW}", c.Default)
	}
}

func TestInsert_CoalesceMoreThanTwoArgsRejected(t *testing.T) {
	_, err := Parse(`INSERT INTO "s"."t" ("a") VALUES (COALESCE($1, $2, $3))`)
	assertUnsupported(t, err, "COALESCE")
}

// ─────────────────────────── UPDATE / DELETE ───────────────────────────

func TestUpdate_Basic(t *testing.T) {
	upd := parseStmt(t, `UPDATE "s"."t" SET "a" = $1, "b" = now() WHERE "id" = $2`).(*Update)
	if upd.Table.Name != "t" {
		t.Errorf("table = %+v", upd.Table)
	}
	if len(upd.Set) != 2 {
		t.Fatalf("set = %+v", upd.Set)
	}
	if upd.Set[0].Column != "a" || upd.Set[1].Column != "b" {
		t.Errorf("set columns = %+v %+v", upd.Set[0], upd.Set[1])
	}
	if fn, ok := upd.Set[1].Value.(FuncCall); !ok || fn.Name != "NOW" {
		t.Errorf("set[1].value = %+v; want NOW()", upd.Set[1].Value)
	}
	if len(upd.Where) != 1 {
		t.Fatalf("where = %+v", upd.Where)
	}
}

func TestUpdate_ReturningRejected(t *testing.T) {
	_, err := Parse(`UPDATE "s"."t" SET "a" = $1 RETURNING "id"`)
	assertUnsupported(t, err, "RETURNING")
}

func TestUpdate_FromClauseRejected(t *testing.T) {
	_, err := Parse(`UPDATE "s"."t" SET "a" = "u"."b" FROM "s"."u" WHERE "s"."t"."id" = "u"."id"`)
	assertUnsupported(t, err, "FROM")
}

func TestDelete_Basic(t *testing.T) {
	del := parseStmt(t, `DELETE FROM "s"."t" WHERE "id" = $1`).(*Delete)
	if del.Table.Name != "t" {
		t.Errorf("table = %+v", del.Table)
	}
	if len(del.Where) != 1 {
		t.Errorf("where = %+v", del.Where)
	}
}

func TestDelete_UsingRejected(t *testing.T) {
	_, err := Parse(`DELETE FROM "s"."t" USING "s"."u" WHERE "s"."t"."id" = "u"."id"`)
	assertUnsupported(t, err, "USING")
}

// ─────────────────────────── unsupported PG features ───────────────────────────

func TestSelect_JoinRejected(t *testing.T) {
	_, err := Parse(`SELECT "a"."id" FROM "s"."a" JOIN "s"."b" ON "a"."id" = "b"."a_id"`)
	assertUnsupported(t, err, "JOIN")
}

func TestSelect_MultiTableFromRejected(t *testing.T) {
	_, err := Parse(`SELECT "a"."id" FROM "s"."a", "s"."b"`)
	assertUnsupported(t, err, "multi-table FROM")
}

func TestSelect_SubqueryInFromRejected(t *testing.T) {
	_, err := Parse(`SELECT "x" FROM (SELECT "id" FROM "s"."t") sub`)
	assertUnsupported(t, err, "")
}

func TestSelect_CTE_Rejected(t *testing.T) {
	_, err := Parse(`WITH x AS (SELECT "id" FROM "s"."t") SELECT * FROM x`)
	assertUnsupported(t, err, "WITH/CTE")
}

func TestSelect_RecursiveCTE_Rejected(t *testing.T) {
	// Conformance suite scenarios.go:506 specifically scopes WITH
	// RECURSIVE to embedded-only — sim must reject so the differential
	// test keeps producing the same outcome.
	src := `WITH RECURSIVE r AS (SELECT 1 UNION ALL SELECT 1) SELECT * FROM r`
	_, err := Parse(src)
	assertUnsupported(t, err, "")
}

func TestSelect_GroupByRejected(t *testing.T) {
	_, err := Parse(`SELECT "a" FROM "s"."t" GROUP BY "a"`)
	assertUnsupported(t, err, "GROUP BY")
}

func TestSelect_HavingRejected(t *testing.T) {
	_, err := Parse(`SELECT "a" FROM "s"."t" GROUP BY "a" HAVING COUNT(*) > 0`)
	// GROUP BY rejection wins (alphabetical in source); either error is fine.
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

func TestSelect_UnionRejected(t *testing.T) {
	_, err := Parse(`SELECT 1 UNION SELECT 2`)
	assertUnsupported(t, err, "UNION")
}

func TestSelect_LockingRejected(t *testing.T) {
	_, err := Parse(`SELECT "a" FROM "s"."t" FOR UPDATE`)
	assertUnsupported(t, err, "locking")
}

func TestSelect_NotPredicateRejected(t *testing.T) {
	_, err := Parse(`SELECT "a" FROM "s"."t" WHERE NOT ("a" = $1)`)
	assertUnsupported(t, err, "NOT")
}

func TestSelect_TableAliasRejected(t *testing.T) {
	// `FROM "s"."t" AS x` — the executor doesn't model aliases.
	_, err := Parse(`SELECT "x"."a" FROM "s"."t" AS "x"`)
	assertUnsupported(t, err, "alias")
}

// ─────────────────────────── literals + expressions ───────────────────────────

func TestExpr_NullLiteralRejected(t *testing.T) {
	_, err := Parse(`INSERT INTO "s"."t" ("a") VALUES (NULL)`)
	assertUnsupported(t, err, "NULL")
}

func TestExpr_BooleanLiteralRejected(t *testing.T) {
	_, err := Parse(`INSERT INTO "s"."t" ("a") VALUES (TRUE)`)
	assertUnsupported(t, err, "boolean")
}

func TestExpr_TypeCastTransparent(t *testing.T) {
	// $1::vector should reach the Args slot as a bare Placeholder —
	// the cast is consumed by pg_query, executor never sees it.
	ins := parseStmt(t, `INSERT INTO "s"."t" ("v") VALUES ($1::vector)`).(*Insert)
	if ph, ok := ins.Args[0].(Placeholder); !ok || ph.N != 1 {
		t.Errorf("args[0] = %+v; want Placeholder{1}", ins.Args[0])
	}
}

func TestExpr_StringLiteralPreserved(t *testing.T) {
	ins := parseStmt(t, `INSERT INTO "s"."t" ("name") VALUES ('foo bar')`).(*Insert)
	lit := ins.Args[0].(Literal)
	if lit.Kind != LitString || lit.Str != "foo bar" {
		t.Errorf("literal = %+v; want LitString 'foo bar'", lit)
	}
}

func TestExpr_NegativeIntLiteral(t *testing.T) {
	// pg_query collapses `-1` into a single A_Const with a negative
	// Ival rather than a unary-minus wrapper, so the translator
	// handles it as a normal integer literal. Pin the behaviour so
	// a future parser change that splits it into A_Expr{u-} would
	// fail loudly.
	ins := parseStmt(t, `INSERT INTO "s"."t" ("n") VALUES (-1)`).(*Insert)
	lit, ok := ins.Args[0].(Literal)
	if !ok {
		t.Fatalf("args[0] = %T; want Literal", ins.Args[0])
	}
	if lit.Kind != LitInt64 || lit.Int64 != -1 {
		t.Errorf("literal = %+v; want LitInt64 -1", lit)
	}
}

func TestExpr_IntLiteralPreserved(t *testing.T) {
	ins := parseStmt(t, `INSERT INTO "s"."t" ("n") VALUES (42)`).(*Insert)
	lit := ins.Args[0].(Literal)
	if lit.Kind != LitInt64 || lit.Int64 != 42 {
		t.Errorf("literal = %+v; want LitInt64 42", lit)
	}
}

// ─────────────────────────── error wrapping ───────────────────────────

func TestParse_ErrUnsupportedSentinelWrappedConsistently(t *testing.T) {
	// Every rejection path must wrap ErrUnsupported so callers can
	// branch on errors.Is. The conformance suite + pool tests rely
	// on this contract.
	cases := []string{
		"",                                     // empty
		"SELECT 1; SELECT 2",                   // multi-stmt
		`SELECT 1 FROM "s"."t" GROUP BY "a"`,   // GROUP BY
		`WITH x AS (SELECT 1) SELECT * FROM x`, // CTE
		`SELECT 1 UNION SELECT 2`,              // UNION
		`SELECT 1 FROM "a"."b" JOIN "a"."c" ON true`, // JOIN
		"this is not sql", // syntax error
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			_, err := Parse(src)
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("Parse(%q) err = %v; want errors.Is ErrUnsupported", src, err)
			}
		})
	}
}

// ─────────────────────────── round-trip basics ───────────────────────────

func TestInsert_RoundTripExecutorShape(t *testing.T) {
	// Smoke test the full codegen-emitted shape one full statement at a
	// time. The full sim tests in phase1_test.go cover the executor
	// behaviour; this just confirms the AST hits exactly the fields
	// the executor reads.
	stmt := parseStmt(t,
		`INSERT INTO "atlantis"."consumer_account" `+
			`("id", "email", "created_at") `+
			`VALUES ($1, $2, COALESCE($3::timestamptz, now())) `+
			`ON CONFLICT ("id") DO UPDATE SET "email" = EXCLUDED."email" `+
			`RETURNING "id", "created_at"`,
	)
	ins := stmt.(*Insert)
	if ins.Table.Qualified() != "atlantis.consumer_account" {
		t.Errorf("table = %s", ins.Table.Qualified())
	}
	if len(ins.Cols) != 3 || len(ins.Args) != 3 {
		t.Errorf("cols/args length mismatch: %d / %d", len(ins.Cols), len(ins.Args))
	}
	if ins.OnConflict != ConflictUpdate {
		t.Errorf("OnConflict = %v", ins.OnConflict)
	}
	if !reflect.DeepEqual(ins.Returning, []string{"id", "created_at"}) {
		t.Errorf("returning = %+v", ins.Returning)
	}
}
