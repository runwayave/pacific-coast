// Package sql defines the AST types the simulator's executor consumes.
// pg_query_go (libpg_query bound via cgo) does the actual parsing in
// pgparse.go; this file owns the post-translation node types only.
//
// The accepted surface is a whitelist of shapes the executor models:
// INSERT (with optional ON CONFLICT DO NOTHING/UPDATE and RETURNING),
// SELECT (single-table FROM, WHERE with AND/OR groups, ORDER BY with
// per-column ASC/DESC and NULLS FIRST/LAST, LIMIT, OFFSET, COUNT(*)
// OVER () window total, SELECT * expansion, vector distance and JSONB
// extract in projection and ORDER BY), UPDATE, and DELETE. Anything
// outside this surface returns ErrUnsupported at translation time.
package sql

// Stmt is any parsed top-level statement.
type Stmt interface{ stmt() }

// TableRef is a schema-qualified table name. Both components are
// required — sim catalog lookups key on "schema.table".
type TableRef struct {
	Schema string
	Name   string
}

// Qualified returns "schema.name". The sim catalog keys tables by this
// string so the executor never has to plumb the split.
func (t TableRef) Qualified() string { return t.Schema + "." + t.Name }

// Expr is anything that produces a value at execute time: a bound
// placeholder, a literal, a function call, or a COALESCE wrapper.
type Expr interface{ expr() }

// Placeholder is $N (1-indexed in source).
type Placeholder struct{ N int }

func (Placeholder) expr() {}

// Literal is a parsed constant. Kind tags the value's Go type:
// KindString for 'foo', KindInt64 for 123. The simulator stores it
// pre-typed so the executor can stream it into a scan target without
// re-parsing.
type Literal struct {
	Kind  LiteralKind
	Str   string // valid when Kind == KindString
	Int64 int64  // valid when Kind == KindInt64
}

// LiteralKind names the constant subtype. Kept separate from the
// catalog's ColKind because literals carry less type detail (a parsed
// string literal might land in a text, varchar, citext, or uuid
// column — the executor decides at assignment time).
type LiteralKind uint8

const (
	LitString LiteralKind = iota + 1
	LitInt64
)

func (Literal) expr() {}

// FuncCall is a function invocation. The executor recognizes one name:
// now() — used by codegen-emitted soft-delete UPDATE and as a DEFAULT
// inside COALESCE. The Name field carries the upper-cased identifier
// so the executor can switch on it without case-folding twice.
type FuncCall struct {
	Name string
	Args []Expr
}

func (FuncCall) expr() {}

// Coalesce is COALESCE(placeholder, default). The :: cast on the
// placeholder is parsed but discarded — Go-side bind values are
// already typed; the cast only exists in source SQL to satisfy PG's
// inference. Default carries whatever expression follows (a literal,
// a function call, or a nested COALESCE).
type Coalesce struct {
	Value   Placeholder
	Default Expr
}

func (Coalesce) expr() {}

// Pred is any WHERE predicate. The shapes below cover everything
// codegen-emitted reads need: comparison (=, <, <=, >, >=, !=) for
// PK lookup AND keyset cursors, IS [NOT] NULL for the soft-delete
// filter, = ANY($N) for BatchGet, and parenthesized AND/OR groups
// for the mixed-direction keyset shape.
type Pred interface{ pred() }

// CmpOp is the operator on a comparison predicate. The set is closed
// to PG's standard scalar comparators; codegen emits all six.
type CmpOp uint8

const (
	OpEq CmpOp = iota + 1
	OpLT
	OpLE
	OpGT
	OpGE
	OpNE
)

// String renders the op for diagnostics.
func (o CmpOp) String() string {
	switch o {
	case OpEq:
		return "="
	case OpLT:
		return "<"
	case OpLE:
		return "<="
	case OpGT:
		return ">"
	case OpGE:
		return ">="
	case OpNE:
		return "!="
	}
	return "?"
}

// CmpPred is `ident <op> expr`. The right side is Expr (not Placeholder)
// because UPDATE SET also uses this shape with function-call values
// (SET col = now()) and INSERT defaults use COALESCE expressions;
// reusing the parse path keeps the grammar small.
type CmpPred struct {
	Column string
	Op     CmpOp
	Value  Expr
}

func (CmpPred) pred() {}

// IsNullPred is `ident IS [NOT] NULL`. Not is true for IS NOT NULL.
type IsNullPred struct {
	Column string
	Not    bool
}

func (IsNullPred) pred() {}

// AnyPred is `ident = ANY($N)`. The placeholder carries a slice
// (Go []int64 / []string / etc.); the executor iterates the slice
// at evaluate time.
type AnyPred struct {
	Column string
	Arg    Placeholder
}

func (AnyPred) pred() {}

// Connective is the boolean combinator inside a GroupPred.
type Connective uint8

const (
	ConnAnd Connective = iota + 1
	ConnOr
)

// GroupPred is a parenthesized predicate group: `( pred OR pred OR ... )`
// or the AND-grouped variant. WHERE-top-level lists are implicitly
// AND-ed; explicit groups exist so the codegen-emitted keyset shape
// (mixed-direction nested OR) parses without ambiguity.
//
// Single-child groups (a paren around one pred) are also legal and
// pass through unchanged at execute time — useful when the codegen
// puts parens around the LHS for clarity.
type GroupPred struct {
	Connective Connective
	Preds      []Pred
}

func (GroupPred) pred() {}

// JsonExtractPred is `col -> 'k' [->> 'k']... OP value` in WHERE.
// Supports comparison on the extracted text or subtree
// against a bound value — most commonly `data->>'type' = $1` in
// audit-log / event-store schemas.
type JsonExtractPred struct {
	Column string
	Path   []string
	AsText bool
	Op     CmpOp
	Value  Expr
}

func (JsonExtractPred) pred() {}

// Projection is one SELECT-list item. Three shapes:
//
//   - Bare column: only Column is set.
//   - Window total `COUNT(*) OVER () AS alias`: only WindowCountAlias is set.
//   - Expression with alias `expr AS alias`: Expr + Alias are set (used by
//     vector distance and Phase-2-onwards JSONB extracts in SELECT).
//
// The executor's projection layer dispatches by checking these in
// priority order. Exactly one form is meaningful per Projection
// instance — the parser never produces ambiguous values.
type Projection struct {
	Column           string
	WindowCountAlias string
	Expr             Expr
	Alias            string

	// Star is true for `SELECT *` projections. The executor expands
	// Star into one bare-column Projection per catalog column at
	// execSelect time, so downstream code never sees Star directly —
	// only the translator emits it and the expansion consumes it.
	Star bool
}

// IsWindowCount reports whether this projection is the window total.
func (p Projection) IsWindowCount() bool { return p.WindowCountAlias != "" }

// IsExpr reports whether this projection is an expression-with-alias
// (the vector-distance and JSONB-extract paths). When true, the
// executor evaluates Expr against each row and stores the value
// under Alias in the output.
func (p Projection) IsExpr() bool { return p.Expr != nil }

// IsStar reports whether this projection is `*`. Mutually exclusive
// with the other shapes; executor expands Star at the top of the
// projection pipeline so the rest of the code path sees only the
// concrete-column projections.
func (p Projection) IsStar() bool { return p.Star }

// Insert is INSERT INTO table (cols) VALUES (exprs)
// [ON CONFLICT (...) DO NOTHING | DO UPDATE SET ...] [RETURNING cols].
//
// Args holds the per-column value expression in column order — usually
// a bare Placeholder, sometimes a Coalesce for columns with a SQL
// DEFAULT declared. Conflict resolution is opt-in; absent ON CONFLICT,
// a duplicate-PK INSERT fails the same way it always did.
type Insert struct {
	Table     TableRef
	Cols      []string
	Args      []Expr
	Returning []string

	// OnConflict is the resolution policy when the proposed row's
	// conflict target columns match an existing row. ConflictNone
	// (zero value) means "no clause; let the insert fail."
	OnConflict ConflictAction

	// ConflictTarget names the columns the conflict is detected
	// against. The target must match the table's PK exactly —
	// secondary unique-constraint targets are not yet wired.
	ConflictTarget []string

	// ConflictUpdates is the SET assignments applied when
	// OnConflict == ConflictUpdate. The Value Expr may be an
	// ExcludedRef referencing the proposed (new) row's value for a
	// column — the EXCLUDED.col PG idiom.
	ConflictUpdates []Assign
}

func (*Insert) stmt() {}

// ConflictAction enumerates what to do when ON CONFLICT detects a
// duplicate.
type ConflictAction uint8

const (
	ConflictNone ConflictAction = iota
	ConflictNothing
	ConflictUpdate
)

// ExcludedRef references a column on the proposed (would-be-inserted)
// row inside an ON CONFLICT DO UPDATE SET clause: `EXCLUDED.col`.
// The executor resolves it against the per-statement bind values.
type ExcludedRef struct {
	Column string
}

func (ExcludedRef) expr() {}

// ColumnRef is a bare column reference (`col`). The executor also
// this so JSONB extract paths — `col->'k'` and `col->>'k'` — and
// vector distance expressions — `col <=> $1::vector` — can carry the
// column on the LHS as an Expr without expanding CmpPred. Existing
// "column on the LHS" predicates still use the Column-string form on
// CmpPred for backward compat; only the new expression shapes route
// through ColumnRef.
type ColumnRef struct {
	Name string
}

func (ColumnRef) expr() {}

// VectorOp is the pgvector distance operator. The three Postgres
// pgvector ops have distinct semantics (cosine = 1 − cos similarity;
// L2 = Euclidean; IP = negated inner product) — the sim implements
// all three because codegen-emitted vector-search SQL switches by
// declared distance metric.
type VectorOp uint8

const (
	VecCosine VectorOp = iota + 1 // <=>
	VecL2                         // <->
	VecIP                         // <#>
)

// String renders the operator for diagnostics.
func (o VectorOp) String() string {
	switch o {
	case VecCosine:
		return "<=>"
	case VecL2:
		return "<->"
	case VecIP:
		return "<#>"
	}
	return "?"
}

// VectorDistance is the codegen-emitted `col <op> $N::vector`
// expression — appears in projection (with AS alias) and ORDER BY
// for ANN search handlers. The executor materializes the float
// distance per row; pre-Phase-2 storage is a brute-force scan, the
// HNSW index is stubbed.
type VectorDistance struct {
	Column string
	Op     VectorOp
	Arg    Placeholder
}

func (VectorDistance) expr() {}

// JsonExtract is `col -> 'k1' -> 'k2' ... [->> 'kn']`. AsText is true
// when the final operator is `->>` (returns text); false for `->`
// (returns nested jsonb subtree, still a structured value).
type JsonExtract struct {
	Column string
	Path   []string
	AsText bool
}

func (JsonExtract) expr() {}

// Select is SELECT cols [, total] FROM table [WHERE ...] [ORDER BY ...]
// [LIMIT ...] [OFFSET ...]. OrderBy is nil if absent; an explicit empty
// slice means parsed-then-rejected at execute time (never produced).
//
// Limit and Offset are Expr because PG accepts both `$N` placeholders
// AND integer literals (`LIMIT 5`). The executor evaluates the Expr at
// run time and asserts the result is an int64; anything else returns
// an executor error rather than a parse-time rejection.
type Select struct {
	Table   TableRef
	Cols    []Projection
	Where   []Pred
	OrderBy []OrderByCol
	Limit   Expr
	Offset  Expr
}

// NullsOrder controls explicit NULLS FIRST / NULLS LAST. Default lets
// the executor apply PG's defaults: NULLS LAST under ASC, NULLS FIRST
// under DESC.
type NullsOrder uint8

const (
	NullsDefault NullsOrder = iota
	NullsFirst
	NullsLast
)

// OrderByCol is one column (or expression) in an ORDER BY clause.
// Direction and NULLS handling are per item. The executor supports
// "column only" restriction by introducing Expr: when non-nil, the
// executor evaluates Expr per row and orders by the result. This is
// how codegen-emitted vector-search SQL sorts by distance:
//
//	ORDER BY "embedding" <=> $1::vector
//
// becomes an OrderByCol{Expr: VectorDistance{...}}.
type OrderByCol struct {
	Column string
	Expr   Expr // non-nil means sort by Expr per row
	Desc   bool
	Nulls  NullsOrder
}

func (*Select) stmt() {}

// Assign is one column = expr pair in UPDATE SET.
type Assign struct {
	Column string
	Value  Expr
}

// Update is UPDATE table SET ... [WHERE ...]. Where is empty for
// codegen-emitted writes (which always key by PK), but the parser
// allows it to mirror PG's grammar — the executor errors at runtime
// if Where is empty AND no full-table-write opt-in is set.
type Update struct {
	Table TableRef
	Set   []Assign
	Where []Pred
}

func (*Update) stmt() {}

// Delete is DELETE FROM table [WHERE ...]. Same WHERE caveat as Update.
type Delete struct {
	Table TableRef
	Where []Pred
}

func (*Delete) stmt() {}
