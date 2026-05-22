package dsl

// AST node types for the .atl DSL. The parser emits these; the
// IR-lowering pass (ir.go) converts them into resolved, validated form.
//
// Every node carries a Position for error reporting.

// File is the root of one parsed .atl file.
type File struct {
	Path  string // source path, for error messages
	Decls []Decl
}

// Decl is one top-level declaration: entity or hypertable.
type Decl interface {
	isDecl()
	Position() Position
	DeclName() string
}

// EntityDecl: `entity Name in namespace { ... }`.
type EntityDecl struct {
	Pos       Position
	Name      string
	Namespace string
	Members   []EntityMember
}

func (*EntityDecl) isDecl()              {}
func (e *EntityDecl) Position() Position { return e.Pos }
func (e *EntityDecl) DeclName() string   { return e.Name }

// HypertableDecl: `hypertable Name in namespace on time_field { ... }`.
type HypertableDecl struct {
	Pos       Position
	Name      string
	Namespace string
	TimeField string
	Members   []EntityMember
}

func (*HypertableDecl) isDecl()              {}
func (h *HypertableDecl) Position() Position { return h.Pos }
func (h *HypertableDecl) DeclName() string   { return h.Name }

// EntityMember is anything that can appear inside an entity body.
type EntityMember interface {
	isEntityMember()
	Position() Position
}

// FieldDecl describes one column.
type FieldDecl struct {
	Pos       Position
	Name      string
	Type      TypeRef
	Modifiers []FieldModifier
}

func (*FieldDecl) isEntityMember()      {}
func (f *FieldDecl) Position() Position { return f.Pos }

// HasModifier reports whether any modifier of the given kind is present.
func (f *FieldDecl) HasModifier(kind ModifierKind) bool {
	for _, m := range f.Modifiers {
		if m.ModifierKind() == kind {
			return true
		}
	}
	return false
}

// TypeRef captures a field type. Scalar types use Name; arrays set Array=true
// and Elem to the inner type; vector and numeric carry parameters.
type TypeRef struct {
	Pos     Position
	Name    string   // e.g. "bigint", "text", "varchar", "vector", "numeric", "jsonb"
	Array   bool     // true for `[]Type`
	Elem    *TypeRef // populated when Array is true
	VecDim  int      // populated when Name == "vector"
	Len     int      // populated when Name == "varchar"  (max char length)
	NumP    int      // numeric precision
	NumS    int      // numeric scale
	HasNumP bool     // distinguishes default numeric from explicit (0,0)
}

// ModifierKind enumerates what kind of modifier a FieldModifier carries.
type ModifierKind int

const (
	ModPrimary ModifierKind = iota
	ModIdentity
	ModSerial
	ModNotNull
	ModUnique
	ModCheck
	ModBackfill
	ModDefault
	ModReferences
)

// FieldModifier covers `primary`, `not null`, `unique`, `default X`, `references X`.
type FieldModifier interface {
	isFieldModifier()
	ModifierKind() ModifierKind
	Position() Position
}

// ModPrimaryDecl marks a column as the entity's PRIMARY KEY.
type ModPrimaryDecl struct{ Pos Position }

func (*ModPrimaryDecl) isFieldModifier()           {}
func (*ModPrimaryDecl) ModifierKind() ModifierKind { return ModPrimary }
func (m *ModPrimaryDecl) Position() Position       { return m.Pos }

// ModIdentityDecl marks a column GENERATED ALWAYS AS IDENTITY. The codegen
// excludes such columns from server-emitted INSERTs because Postgres
// supplies the value.
type ModIdentityDecl struct{ Pos Position }

func (*ModIdentityDecl) isFieldModifier()           {}
func (*ModIdentityDecl) ModifierKind() ModifierKind { return ModIdentity }
func (m *ModIdentityDecl) Position() Position       { return m.Pos }

// ModSerialDecl marks a column as BIGSERIAL (legacy auto-increment). Like
// Identity, it's excluded from server-emitted INSERTs; unlike Identity, the
// SQL emitter renders the column type as `BIGSERIAL` rather than
// `BIGINT GENERATED ALWAYS AS IDENTITY`. Only valid on `bigint` fields —
// the IR validator enforces this.
type ModSerialDecl struct{ Pos Position }

func (*ModSerialDecl) isFieldModifier()           {}
func (*ModSerialDecl) ModifierKind() ModifierKind { return ModSerial }
func (m *ModSerialDecl) Position() Position       { return m.Pos }

// ModCheckDecl carries the body of a CHECK constraint (the SQL inside the
// parens). The expression is passed through verbatim — Postgres parses it.
type ModCheckDecl struct {
	Pos  Position
	Expr string
}

func (*ModCheckDecl) isFieldModifier()           {}
func (*ModCheckDecl) ModifierKind() ModifierKind { return ModCheck }
func (m *ModCheckDecl) Position() Position       { return m.Pos }

// ModBackfillDecl carries the SQL expression that `tide apply --backfill`
// splices into a chunked UPDATE to populate this column on existing rows.
// The expression is parsed and purity-checked (no subqueries, no
// non-whitelisted functions, refs must be entity columns) by
// sqlvalidate.ValidateBackfillExpression; the raw string lives here.
type ModBackfillDecl struct {
	Pos  Position
	Expr string
}

func (*ModBackfillDecl) isFieldModifier()           {}
func (*ModBackfillDecl) ModifierKind() ModifierKind { return ModBackfill }
func (m *ModBackfillDecl) Position() Position       { return m.Pos }

// ModNotNullDecl marks a column NOT NULL.
type ModNotNullDecl struct{ Pos Position }

func (*ModNotNullDecl) isFieldModifier()           {}
func (*ModNotNullDecl) ModifierKind() ModifierKind { return ModNotNull }
func (m *ModNotNullDecl) Position() Position       { return m.Pos }

// ModUniqueDecl marks a column UNIQUE.
type ModUniqueDecl struct{ Pos Position }

func (*ModUniqueDecl) isFieldModifier()           {}
func (*ModUniqueDecl) ModifierKind() ModifierKind { return ModUnique }
func (m *ModUniqueDecl) Position() Position       { return m.Pos }

// ModDefaultDecl: `default <value>`. Value is held loosely so the IR lowering
// can apply the right kind of quoting / generation.
type ModDefaultDecl struct {
	Pos   Position
	Value DefaultValue
}

func (*ModDefaultDecl) isFieldModifier()           {}
func (*ModDefaultDecl) ModifierKind() ModifierKind { return ModDefault }
func (m *ModDefaultDecl) Position() Position       { return m.Pos }

// DefaultValue is one of: string, integer, boolean, or now().
type DefaultValue struct {
	Pos  Position
	Kind DefaultKind
	Str  string
	Int  int64
	Bool bool
}

// DefaultKind enumerates the variants a DefaultValue can take.
type DefaultKind int

const (
	DefaultString DefaultKind = iota
	DefaultInt
	DefaultBool
	DefaultNow
	DefaultRaw // verbatim SQL expression in `Str`
)

// ModReferencesDecl: `references ns.Entity.field [on delete X] [on update Y]`.
type ModReferencesDecl struct {
	Pos          Position
	TargetNS     string
	TargetEntity string
	TargetField  string
	OnDelete     RefAction // RefActionUnset if not specified
	OnUpdate     RefAction
}

func (*ModReferencesDecl) isFieldModifier()           {}
func (*ModReferencesDecl) ModifierKind() ModifierKind { return ModReferences }
func (m *ModReferencesDecl) Position() Position       { return m.Pos }

// RefAction is the ON DELETE / ON UPDATE action on a foreign key.
type RefAction int

const (
	RefActionUnset RefAction = iota
	RefActionCascade
	RefActionRestrict
	RefActionSetNull
)

// String returns the SQL keyword form (CASCADE, RESTRICT, SET NULL).
func (a RefAction) String() string {
	switch a {
	case RefActionCascade:
		return "CASCADE"
	case RefActionRestrict:
		return "RESTRICT"
	case RefActionSetNull:
		return "SET NULL"
	default:
		return ""
	}
}

// RelationDecl: `has_many name: Entity via field`.
type RelationDecl struct {
	Pos    Position
	Kind   RelationKind
	Name   string // local alias for the relation, e.g. "items"
	Target string // target entity name
	Via    string // FK field on the target
}

func (*RelationDecl) isEntityMember()      {}
func (r *RelationDecl) Position() Position { return r.Pos }

// RelationKind selects between has_many and has_one.
type RelationKind int

const (
	RelHasMany RelationKind = iota
	RelHasOne
)

// IndexDecl covers all index kinds. Kind selects the variant; the relevant
// fields are populated per kind.
type IndexDecl struct {
	Pos  Position
	Kind IndexKind

	// For btree (default) and partial btree:
	Fields []IndexField

	// For partial:
	Where *PartialPredicate

	// For hnsw / gin:
	Field  string
	VecOps VectorOps // only for hnsw
}

func (*IndexDecl) isEntityMember()      {}
func (i *IndexDecl) Position() Position { return i.Pos }

// IndexKind selects between the supported index variants.
type IndexKind int

const (
	IndexBtree IndexKind = iota
	IndexPartial
	IndexHNSW
	IndexGIN
)

// IndexField is one entry in a btree / partial index field list. Two forms:
//
//	bare column     → Name set, IsExpr false
//	raw expression  → Expr set, IsExpr true.  e.g. expr "lower(email)"
type IndexField struct {
	Name   string
	Expr   string
	IsExpr bool
	Desc   bool // true for DESC
}

// VectorOps selects the pgvector operator class for an HNSW index.
type VectorOps int

const (
	VecOpsCosine VectorOps = iota
	VecOpsL2
	VecOpsIP
)

// String returns the pgvector operator-class identifier
// (vector_cosine_ops, vector_l2_ops, vector_ip_ops).
func (v VectorOps) String() string {
	switch v {
	case VecOpsCosine:
		return "vector_cosine_ops"
	case VecOpsL2:
		return "vector_l2_ops"
	case VecOpsIP:
		return "vector_ip_ops"
	}
	return ""
}

// PartialPredicate represents the `where ...` clause on a partial index.
// Two forms today:
//
//	field is [not] null     → Op == "", IsNull is true/false
//	field <op> <literal>    → Op in {"=","!=","<","<=",">",">="}, Literal set
//
// Both forms map to predicates Postgres accepts; the SQL emitter passes
// through verbatim (with quoting on string literals).
type PartialPredicate struct {
	Pos    Position
	Field  string
	IsNull bool // when Op == "": true → IS NULL, false → IS NOT NULL

	Op      string       // "" for null tests; otherwise "=", "!=", "<", "<=", ">", ">="
	Literal DefaultValue // rhs of the comparison; only meaningful when Op != ""
}

// CacheBlock holds the cache { ... } stanza.
type CacheBlock struct {
	Pos            Position
	HasReadThrough bool
	TTL            string // raw duration string, e.g. "10m"
	Tag            string // optional tag template, e.g. "consumer:{consumer_id}"
	Invalidate     []InvalidateClause
	Consistency    Consistency
}

func (*CacheBlock) isEntityMember()      {}
func (c *CacheBlock) Position() Position { return c.Pos }

// Consistency selects between eventual and strict read-through behavior.
type Consistency int

const (
	ConsistencyDefault Consistency = iota // == eventual
	ConsistencyEventual
	ConsistencyStrict
)

// InvalidateClause is one `write(target ...)` entry inside invalidate_on.
type InvalidateClause struct {
	Pos    Position
	Self   bool             // true for `write(self)`
	Target string           // entity name, when Self == false
	Where  *InvalidateWhere // optional `where field = self.field`
}

// InvalidateWhere is the optional `where field = self.field` clause on
// an InvalidateClause.
type InvalidateWhere struct {
	Field     string // field on the target entity
	SelfField string // field on the self entity to compare against
}

// QueryTimeoutDecl: `query_timeout = <duration>`. The parser accepts a
// duration token; IR lowering enforces the 50ms..30s range.
type QueryTimeoutDecl struct {
	Pos      Position
	Duration string
}

func (*QueryTimeoutDecl) isEntityMember()      {}
func (q *QueryTimeoutDecl) Position() Position { return q.Pos }

// UniqueDecl: `unique by field1, field2, ... [deferrable]` — composite
// UNIQUE constraint at the table level. Single-field uniqueness is still
// expressed via the `unique` field modifier.
//
// Deferrable maps to `DEFERRABLE INITIALLY DEFERRED`. It matters for
// transactions that mutate two rows whose final state is unique but whose
// intermediate state isn't — the check is postponed to COMMIT.
type UniqueDecl struct {
	Pos        Position
	Fields     []string
	Deferrable bool
}

func (*UniqueDecl) isEntityMember()      {}
func (u *UniqueDecl) Position() Position { return u.Pos }

// TableCheckDecl: `check "<expr>" [as name]` — table-level CHECK
// constraint. The body is passed verbatim to Postgres (and may reference
// multiple columns, unlike the per-field check modifier).
type TableCheckDecl struct {
	Pos  Position
	Expr string
	Name string // optional; empty → codegen synthesizes a stable name
}

func (*TableCheckDecl) isEntityMember()      {}
func (t *TableCheckDecl) Position() Position { return t.Pos }

// PrimaryDecl: `primary by field1, field2, ...` — composite PRIMARY KEY.
// Mutually exclusive with a per-field `primary` modifier; the IR validator
// enforces this.
type PrimaryDecl struct {
	Pos    Position
	Fields []string
}

func (*PrimaryDecl) isEntityMember()      {}
func (p *PrimaryDecl) Position() Position { return p.Pos }

// SoftDeleteDecl: `soft_delete by <field>` — declares the entity is soft-
// deletable. Generated reads filter `WHERE <field> IS NULL`; generated
// Delete becomes `UPDATE ... SET <field> = now()`. The field itself is
// still declared by the engineer as a normal `timestamptz` column.
type SoftDeleteDecl struct {
	Pos   Position
	Field string
}

func (*SoftDeleteDecl) isEntityMember()      {}
func (s *SoftDeleteDecl) Position() Position { return s.Pos }

// TouchOnUpdateDecl: `touch_on_update by <field>` — declares a column that
// a BEFORE UPDATE trigger refreshes to now() on every row update. The
// codegen emits the trigger function + per-table trigger in the initial
// migration.
type TouchOnUpdateDecl struct {
	Pos   Position
	Field string
}

func (*TouchOnUpdateDecl) isEntityMember()      {}
func (t *TouchOnUpdateDecl) Position() Position { return t.Pos }

// PartitionByDecl: `partition by <field>` — declares a multi-tenant
// partition column. Codegen statically injects
// `WHERE <field> = $caller` into every generated QueryX handler. The
// caller-side authorization layer supplies the value; the user-supplied
// filter cannot override or subvert it. Optional — entities without this
// declaration have no partition predicate.
type PartitionByDecl struct {
	Pos   Position
	Field string
}

func (*PartitionByDecl) isEntityMember()      {}
func (p *PartitionByDecl) Position() Position { return p.Pos }

// ---- Custom query and procedure declarations ----
//
// QueryDecl + ProcedureDecl are the platform's escape hatch for caller
// workloads QueryX can't model: GROUP BY aggregations, DISTINCT ON,
// seeded-random sampling, multi-entity transactions, anything the
// typed predicate surface doesn't express. They live at the file's
// top level (alongside EntityDecl / HypertableDecl) so callers can
// declare them in the same .atl file as the entity they target — no
// atlantis PR needed to introduce a new read shape.
//
// The DSL grammar is small here on purpose. The expressiveness lives
// inside the raw SQL block, validated at plan-time via pg_query_go;
// the DSL itself only declares the typed signature (inputs, outputs)
// and the cache-invariant metadata (touches, cache TTL, invalidate
// tag). Procedures add typed mutation steps (update / delete / insert
// on an entity) so the codegen can automatically bump the right
// generation counters without the caller having to enumerate them.

// QueryDecl: `query <Name> for <Entity> { input { ... } output { ... } sql touches(...) { ... } cache { ... } }`.
// Reads only — no mutation arms. The output shape is either `as <Entity>`
// (the query returns rows of the target entity) or an explicit column
// list when the query joins or aggregates beyond the entity's shape.
type QueryDecl struct {
	Pos    Position
	Name   string
	Target EntityRef // `for <Entity>` — the primary entity the query reads
	Inputs []InputParam
	Output *QueryOutput
	SQL    *SQLBlock
	Cache  *CacheBlock // optional override of the default 30s TTL
}

func (*QueryDecl) isDecl()              {}
func (q *QueryDecl) Position() Position { return q.Pos }
func (q *QueryDecl) DeclName() string   { return q.Name }

// ProcedureDecl: `procedure <Name> for <Entity> { input { ... } steps { ... } invalidate: tag("...") }`.
// Multi-step writes inside one transaction. The atomicity is structural:
// every typed step routes through the entity's existing sqlUpdate /
// sqlDelete / sqlInsert constant, and every raw `sql touches(...) { ... }`
// step inside steps{} runs against the same tx. Outbox bumps fire once per
// touched entity at commit. No external IO inside the tx.
type ProcedureDecl struct {
	Pos        Position
	Name       string
	Target     EntityRef
	Inputs     []InputParam
	Steps      []ProcedureStep
	Invalidate *ProcedureInvalidate // optional trailing `invalidate: tag("...")` for bulk tag flushes
}

func (*ProcedureDecl) isDecl()              {}
func (p *ProcedureDecl) Position() Position { return p.Pos }
func (p *ProcedureDecl) DeclName() string   { return p.Name }

// EntityRef is a `[Namespace.]Entity` reference. Resolved against the
// merged IR at lowering time. Unqualified names default to the file's
// owning namespace.
type EntityRef struct {
	Pos       Position
	Namespace string // empty means "same namespace as the declaration"
	Name      string
}

// InputParam: one entry in the `input { name: type, ... }` block.
// Optional default value (string literal, int, true/false, or `now`)
// supplied when the caller omits the input on the wire.
type InputParam struct {
	Pos     Position
	Name    string
	Type    TypeRef
	Default Expr // nil if no default
}

// QueryOutput describes the shape of rows a custom query returns. One of
// two forms:
//
//   - AsEntity != nil: `output as <Entity>` (rows match the target entity's
//     proto message — no codegen of a new output type)
//   - Columns != nil: `output { col1: type, col2: type }` (explicit per-
//     column shape; codegen emits a synthetic <Query>Row proto message)
//
// Exactly one of the two is populated.
type QueryOutput struct {
	Pos      Position
	AsEntity *EntityRef
	Columns  []InputParam // reuse InputParam shape: name + type, no default
}

// SQLBlock holds a raw SQL body sliced verbatim from the source between
// the `{` and `}` tokens. The byte offsets on Pos and EndPos let the
// parser carry source line/column information forward so pg_query_go
// errors land on the right line of the .atl file.
type SQLBlock struct {
	Pos     Position // the opening `{`
	EndPos  Position // the closing `}`
	Touches []EntityRef
	Raw     string
}

// ProcedureStep is one entry inside a `steps { ... }` block. Exactly one
// of Typed or Raw is populated.
type ProcedureStep struct {
	Pos   Position
	Typed *TypedStep
	Raw   *SQLBlock
}

// TypedStep represents one of `update Entity set ... where ...`,
// `delete Entity where ...`, or `insert Entity { ... }`. The verb +
// target name go directly into the entity's existing sqlUpdate /
// sqlDelete / sqlInsert paths at codegen time, so typed steps inherit
// every cache + outbox invariant the entity already enforces.
type TypedStep struct {
	Pos       Position
	Verb      string // "update", "delete", or "insert"
	Target    EntityRef
	Assigns   []SetAssignment // populated for update/insert
	WhereExpr Expr            // populated for update/delete; nil for insert
}

// SetAssignment: one entry in an UPDATE's SET list or an INSERT's
// column initializer.
type SetAssignment struct {
	Pos   Position
	Field string
	Value Expr
}

// Expr is the typed-step expression form: literals, arg references,
// field references, and `now()`. Deliberately tiny — anything more
// complex belongs in a raw `sql touches(...) { ... }` step.
type Expr interface {
	isExpr()
	Position() Position
}

// LiteralExpr: an int, string, bool, or `now` literal.
type LiteralExpr struct {
	Pos   Position
	Kind  string // "int", "string", "bool", "now"
	Value string // raw textual form; type-checked at IR lowering
}

func (*LiteralExpr) isExpr()              {}
func (l *LiteralExpr) Position() Position { return l.Pos }

// ArgExpr: a `$name` reference to an input parameter.
type ArgExpr struct {
	Pos  Position
	Name string
}

func (*ArgExpr) isExpr()              {}
func (a *ArgExpr) Position() Position { return a.Pos }

// FieldExpr: a bare identifier referring to a column on the typed step's
// target entity. Used in WHERE clauses (`where consumer_id = $arg`).
type FieldExpr struct {
	Pos  Position
	Name string
}

func (*FieldExpr) isExpr()              {}
func (f *FieldExpr) Position() Position { return f.Pos }

// BinaryExpr: an equality / inequality comparison in a WHERE clause.
// Currently only `=` is supported in typed steps — anything richer
// belongs in a raw SQL step.
type BinaryExpr struct {
	Pos   Position
	Op    string // "=", "!=", "<", "<=", ">", ">="
	Left  Expr
	Right Expr
}

func (*BinaryExpr) isExpr()              {}
func (b *BinaryExpr) Position() Position { return b.Pos }

// ProcedureInvalidate: trailing `invalidate: tag("...")` on a procedure.
// Triggers a bulk tag flush at commit alongside the per-entity
// generation bumps the steps produced. Named separately from the entity-
// cache InvalidateClause because the shapes differ — entity cache
// invalidation reads `write(Self)` / `write(Other where ...)`, procedure
// invalidation reads a tag template only.
type ProcedureInvalidate struct {
	Pos    Position
	TagTpl string // the tag template, e.g. "consumer:{account_id}"
}
