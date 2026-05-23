package dsl

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// tableNamePat matches the `[schema.]table` shape allowed in the
// `table "..."` entity modifier. Each segment is the standard unquoted
// Postgres identifier shape. Stricter than Postgres itself — we don't
// accept anything that would need quoting, because the codegen already
// quotes everything.
var tableNamePat = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

// IR is the resolved, validated, JSON-serializable schema.
//
// It is what the codegen consumes (proto/server/client/sql/cache) and what
// the diff engine compares against the previous checkpoint
// (gen/.last-ir.json). It is intentionally flat:
//   - Every entity / hypertable appears in IR.Entities, with Kind selecting.
//   - Cross-entity references are resolved (parser AST holds dotted names;
//     IR holds direct pointers via stable IDs).
//   - All grammar validation rules are enforced before this struct
//     is considered well-formed.
//
// Wire stability:
//
//	The JSON shape of IR is a checkpoint format. Field tags are explicit; do
//	not rename fields without bumping IR.Version and writing a migrator.
type IR struct {
	Version    int               `json:"version"`
	Entities   []Entity          `json:"entities"`
	Queries    []CustomQuery     `json:"queries,omitempty"`
	Procedures []CustomProcedure `json:"procedures,omitempty"`
	Jobs       []Job             `json:"jobs,omitempty"`
	Workflows  []Workflow        `json:"workflows,omitempty"`
}

// Workflow is a resolved multi-step orchestration. Steps execute in
// declaration order; if a step's job fails after exhausting retries,
// compensations for prior steps run in reverse. The state block
// carries the typed inputs the caller provides at start time; each
// step's args can reference state fields via $name.
type Workflow struct {
	Name          string           `json:"name"`
	Namespace     string           `json:"namespace"`
	State         []Field          `json:"state,omitempty"`
	Steps         []WorkflowStepIR `json:"steps"`
	Compensations []WorkflowCompIR `json:"compensations,omitempty"`
	SourcePath    string           `json:"source_path,omitempty"`
	Pos           Position         `json:"-"`
}

func (w *Workflow) ID() string { return w.Namespace + "." + w.Name }

// WorkflowStepIR is one resolved step in a workflow.
type WorkflowStepIR struct {
	Name        string                `json:"name"`
	TargetJobID string                `json:"target_job_id"`
	Args        []EnqueueAssignmentIR `json:"args,omitempty"`
}

// WorkflowCompIR is one resolved compensation entry.
type WorkflowCompIR struct {
	StepName    string                `json:"step_name"`
	TargetJobID string                `json:"target_job_id"`
	Args        []EnqueueAssignmentIR `json:"args,omitempty"`
}

// CustomQuery is a resolved `query Name for Entity { ... }` declaration.
// Reads only. The shape of rows the query returns is either an entity's
// proto message body (Output.AsEntityID populated) or an explicit
// per-column list (Output.Columns populated) — never both. Tier-2 cache
// is keyed on the input hash + the generation counters of each entity
// in Touches, so an UPDATE on any touched entity invalidates every
// cached result of this query at the next debounced bump.
type CustomQuery struct {
	Name       string       `json:"name"`
	Owner      string       `json:"owner"` // canonical "namespace.Entity" id of the `for` target
	Inputs     []QueryParam `json:"inputs"`
	Output     CustomOutput `json:"output"`
	SQL        string       `json:"sql"`             // raw body, post-validation
	Touches    []string     `json:"touches"`         // canonical "namespace.Entity" ids
	Cache      *Cache       `json:"cache,omitempty"` // optional override of default 30s TTL
	SourcePath string       `json:"source_path,omitempty"`
	Pos        Position     `json:"-"` // not serialized; kept for error messages
}

// ID returns the "namespace.Name" identifier for a custom
// query. The namespace is inherited from the Owner entity so the same
// query name can exist in two namespaces without conflict.
func (q *CustomQuery) ID() string {
	// Owner is "namespace.Entity"; we want "namespace.QueryName".
	if i := strings.IndexByte(q.Owner, '.'); i >= 0 {
		return q.Owner[:i] + "." + q.Name
	}
	return q.Name
}

// Job is a resolved `job Name in ns { ... }` declaration.
//
// A job is a typed background-work declaration. atlantis-server emits
// a SubmitX RPC and a typed handler interface from this IR; the
// worker drains atlantis.jobs rows, deserializes Args JSON to the
// generated typed struct, and routes to the handler the caller
// registered at server startup. Retries / TimeoutMS / Queue /
// Schedule govern the worker's runtime behavior.
//
// Args reuses the entity-field shape so the existing type system
// (varchar(N), numeric(P,S), arrays, NOT NULL, defaults, checks)
// applies verbatim. Args MUST NOT carry storage-only modifiers
// (primary, identity, serial, unique, references, backfill); the
// lowering pass enforces this and emits a precise rejection so the
// caller knows which modifier to drop.
//
// Schedule is the raw cron spec verbatim — parsing + validation
// happens at runtime in the scheduler. The IR stores the string so
// subsequent diff / codegen runs see a stable, comparable value.
type Job struct {
	Name      string  `json:"name"`
	Namespace string  `json:"namespace"`
	Args      []Field `json:"args,omitempty"`
	Retries   int     `json:"retries,omitempty"`
	TimeoutMS int     `json:"timeout_ms,omitempty"`
	// TimeoutNone is the explicit `timeout none` form. Distinguishes
	// "no timeout was declared" (false, TimeoutMS=0; SubmitJob falls
	// back to the 30m default) from "this handler opts out of any
	// per-attempt deadline" (true; SubmitJob inserts NULL into
	// atlantis.jobs.timeout_ms so the worker skips context.WithTimeout).
	TimeoutNone bool     `json:"timeout_none,omitempty"`
	Queue       string   `json:"queue,omitempty"`
	Schedule    string   `json:"schedule,omitempty"`
	VisibleTo   string   `json:"visible_to,omitempty"` // caller name or "*"; empty = unrestricted
	SourcePath  string   `json:"source_path,omitempty"`
	Pos         Position `json:"-"`
}

// ID returns the "namespace.Name" identifier used to disambiguate
// jobs across callers. Mirrors Entity.ID() / CustomQuery.ID() so the
// shared uniqueness check in Lower can treat all named decls the
// same.
func (j *Job) ID() string {
	return j.Namespace + "." + j.Name
}

// CustomProcedure is a resolved `procedure Name for Entity { ... }`
// declaration. Multi-step writes inside a single tx. Every step
// (typed or raw) inherits the procedure's tx scope; the codegen wires
// outbox generation-bump enqueues for every entity touched. No
// external IO inside the tx — workloads requiring HTTP/Slack/etc.
// stay hand-written in `internal/server/<name>/`.
type CustomProcedure struct {
	Name       string            `json:"name"`
	Owner      string            `json:"owner"`
	Inputs     []QueryParam      `json:"inputs"`
	Steps      []ProcedureStepIR `json:"steps"`
	Invalidate string            `json:"invalidate,omitempty"` // optional tag template for bulk flush
	SourcePath string            `json:"source_path,omitempty"`
	Pos        Position          `json:"-"`
}

// ID is the "namespace.Name" id used by callers when invoking
// the procedure RPC.
func (p *CustomProcedure) ID() string {
	if i := strings.IndexByte(p.Owner, '.'); i >= 0 {
		return p.Owner[:i] + "." + p.Name
	}
	return p.Name
}

// QueryParam describes one entry in an input{} block: a typed
// parameter the caller supplies on the wire. Default carries the
// fallback value when the caller omits the input.
type QueryParam struct {
	Name    string    `json:"name"`
	Type    FieldType `json:"type"`
	Default *Default  `json:"default,omitempty"`
	Pos     Position  `json:"-"`
}

// CustomOutput is the row-shape contract a custom query promises to
// return. Exactly one of AsEntityID / Columns is populated.
type CustomOutput struct {
	AsEntityID string       `json:"as_entity_id,omitempty"`
	Columns    []QueryParam `json:"columns,omitempty"`
}

// ProcedureStepIR is one resolved step inside a procedure body.
// Exactly one of Typed / Raw / Enqueue is populated.
type ProcedureStepIR struct {
	Typed   *TypedStepIR   `json:"typed,omitempty"`
	Raw     *RawSQLIR      `json:"raw,omitempty"`
	Enqueue *EnqueueStepIR `json:"enqueue,omitempty"`
	Pos     Position       `json:"-"`
}

// EnqueueStepIR is a resolved `enqueue Job(args)` step. The codegen
// emits an INSERT into atlantis.jobs sharing the procedure's tx, so
// the job is enqueued atomically with the procedure's other writes.
//
// TargetJobID is the canonical "namespace.JobName" id matching one of
// IR.Jobs. Args lists the resolved argument expressions in the order
// the codegen serializes them into the args JSON; argument-name
// uniqueness + presence checks happen at IR-lowering time.
//
// Queue / MaxRetries / TimeoutMS are denormalized onto the step at
// lower time so the codegen doesn't have to re-walk IR.Jobs at emit
// time. If the target job's declaration later changes (different
// queue, retries), regenerating the procedure picks up the new
// values without a separate apply step.
type EnqueueStepIR struct {
	TargetJobID string                `json:"target_job_id"`
	Args        []EnqueueAssignmentIR `json:"args,omitempty"`
	Queue       string                `json:"queue,omitempty"`
	MaxRetries  int                   `json:"max_retries,omitempty"`
	TimeoutMS   int                   `json:"timeout_ms,omitempty"`
}

// EnqueueAssignmentIR is one resolved `name: value` pair in an
// enqueue step's arg list.
type EnqueueAssignmentIR struct {
	Name  string  `json:"name"`
	Value *ExprIR `json:"value"`
}

// TypedStepIR is a resolved typed mutation. Each field carries a
// validation invariant the codegen relies on:
//
//   - Verb is exactly one of "update" / "delete" / "insert" — the
//     codegen builds the SQL template directly from the verb name.
//   - TargetID resolves to an existing Entity in the same IR. The
//     codegen reads the entity's column list and table name to
//     render the statement; an unknown target would crash codegen,
//     so lowering rejects it.
//   - Assigns reference real columns on TargetID. Lowering rejects
//     unknown columns so a stale step doesn't reach SQL emission
//     and produce a query that explodes at runtime.
//   - Where, when present, is a Boolean expression tree limited to
//     literals, $args, field refs, and comparisons / AND. OR and NOT
//     are intentionally absent — a step that needs them belongs in a
//     raw `sql touches(...) { ... }` block, not a typed step.
type TypedStepIR struct {
	Verb     string         `json:"verb"`      // "update", "delete", "insert"
	TargetID string         `json:"target_id"` // canonical id
	Assigns  []AssignmentIR `json:"assigns,omitempty"`
	Where    *ExprIR        `json:"where,omitempty"`
	Pos      Position       `json:"-"`
}

// AssignmentIR is `field = expr` in a SET or INSERT column list.
type AssignmentIR struct {
	Field string  `json:"field"`
	Value *ExprIR `json:"value"`
}

// ExprIR is the lowered form of a typed-step expression. The Kind
// determines which fields are populated, mirroring the AST Expr family
// but with $arg references resolved to a known input name.
type ExprIR struct {
	Kind    ExprKind `json:"kind"`
	Op      string   `json:"op,omitempty"`       // for "binary"
	Left    *ExprIR  `json:"left,omitempty"`     // for "binary"
	Right   *ExprIR  `json:"right,omitempty"`    // for "binary"
	ArgName string   `json:"arg_name,omitempty"` // for "arg"
	Field   string   `json:"field,omitempty"`    // for "field"
	LitStr  string   `json:"lit_str,omitempty"`  // for "literal_string"
	LitInt  int64    `json:"lit_int,omitempty"`  // for "literal_int"
	LitBool bool     `json:"lit_bool,omitempty"` // for "literal_bool"
	// "literal_now" carries no payload.
}

// ExprKind enumerates the small set of expression shapes typed steps
// support. Anything richer (OR, NOT, function calls, casts) belongs in
// a raw SQL block rather than a typed step.
type ExprKind string

const (
	ExprArg         ExprKind = "arg"
	ExprField       ExprKind = "field"
	ExprLiteralStr  ExprKind = "literal_string"
	ExprLiteralInt  ExprKind = "literal_int"
	ExprLiteralBool ExprKind = "literal_bool"
	ExprLiteralNow  ExprKind = "literal_now"
	ExprBinary      ExprKind = "binary"
)

// RawSQLIR is a resolved raw `sql touches(...) { ... }` block. SQL is
// the body verbatim; Touches lists the entity ids whose
// generation counters must bump at commit time.
type RawSQLIR struct {
	SQL     string   `json:"sql"`
	Touches []string `json:"touches"`
}

// Entity is one resolved entity or hypertable in the IR.
type Entity struct {
	Name      string     `json:"name"`
	Namespace string     `json:"namespace"`
	Kind      EntityKind `json:"kind"`

	// For hypertable kind: name of the time column. Empty otherwise.
	TimeField string `json:"time_field,omitempty"`

	Fields    []Field      `json:"fields"`
	Indexes   []Index      `json:"indexes,omitempty"`
	Relations []Relation   `json:"relations,omitempty"`
	Uniques   []UniqueSpec `json:"uniques,omitempty"`
	Checks    []TableCheck `json:"checks,omitempty"`
	Cache     *Cache       `json:"cache,omitempty"`

	// QueryTimeoutMS overrides the default per-RPC deadline. 0 means use default.
	QueryTimeoutMS int `json:"query_timeout_ms,omitempty"`

	// CompositePK names the columns that form a composite PRIMARY KEY when
	// it doesn't fit on a single field. Empty when the entity uses the
	// single-column `primary` field modifier (which is by far the common
	// case). Mutually exclusive with any Field.Primary in the same entity.
	CompositePK []string `json:"composite_pk,omitempty"`

	// SoftDeleteField is the timestamptz column that marks soft-deleted
	// rows. Empty means hard-delete semantics. Set via `soft_delete by foo`.
	SoftDeleteField string `json:"soft_delete_field,omitempty"`

	// TouchOnUpdateField is the timestamptz column refreshed by a BEFORE
	// UPDATE trigger. Empty means no trigger is emitted for this entity.
	TouchOnUpdateField string `json:"touch_on_update_field,omitempty"`

	// PartitionField is the column used for multi-tenant isolation. When
	// non-empty, every generated QueryX handler unconditionally AND-joins
	// `WHERE <PartitionField> = $callerPartition` onto the user filter.
	// The auth layer supplies the value; the caller filter cannot override.
	// Empty means no partition predicate is injected.
	PartitionField string `json:"partition_field,omitempty"`

	// TtlField is the timestamptz column that anchors row-level expiry.
	// When non-empty, the built-in SweepExpired scheduled job DELETEs
	// rows where `<TtlField> < now()` on a 1-minute cron. Empty means
	// no automatic cleanup.
	TtlField string `json:"ttl_field,omitempty"`

	// TableName overrides the physical table name codegen would otherwise
	// compute as `atlantis.<namespace>_<snake>`. Format: `[schema.]table`,
	// each part matching `[A-Za-z_][A-Za-z0-9_]*`. Used when adopting an
	// existing database — declare `table "consumer.accounts"` on an entity
	// and atlantis points at that table instead of creating its own.
	// Empty means atlantis uses the computed default.
	TableName string `json:"table_name,omitempty"`

	// RetiredProtoNumbers lists protobuf field numbers that previously existed
	// on this entity but have since been removed. Tracked here (persisted in
	// the IR checkpoint) so we never reuse them — protobuf forbids reuse of
	// field numbers even after a field is dropped.
	RetiredProtoNumbers []int `json:"retired_proto_numbers,omitempty"`
}

// EntityKind selects between regular tables and TimescaleDB hypertables.
type EntityKind string

const (
	EntityKindRegular    EntityKind = "entity"
	EntityKindHypertable EntityKind = "hypertable"
)

// ID returns a stable "namespace.Name" identifier used by FK targets and diff.
func (e *Entity) ID() string { return e.Namespace + "." + e.Name }

// PrimaryField returns the entity's primary-key field, or nil if missing.
// (Lowering guarantees exactly one exists for well-formed entities.)
func (e *Entity) PrimaryField() *Field {
	for i := range e.Fields {
		if e.Fields[i].Primary {
			return &e.Fields[i]
		}
	}
	return nil
}

// FindField returns the field with the given name, or nil.
func (e *Entity) FindField(name string) *Field {
	for i := range e.Fields {
		if e.Fields[i].Name == name {
			return &e.Fields[i]
		}
	}
	return nil
}

// HasVectorField reports whether any column on the entity is a pgvector
// type. The codegen uses this to decide whether the emitted server file
// needs to import `github.com/pgvector/pgvector-go` (so non-vector
// entities don't pull the dep in their generated file).
func (e *Entity) HasVectorField() bool {
	for i := range e.Fields {
		if e.Fields[i].Type.Name == "vector" {
			return true
		}
	}
	return false
}

// Field is one resolved column.
type Field struct {
	Name string    `json:"name"`
	Type FieldType `json:"type"`

	Primary  bool   `json:"primary,omitempty"`
	Identity bool   `json:"identity,omitempty"` // GENERATED ALWAYS AS IDENTITY
	Serial   bool   `json:"serial,omitempty"`   // BIGSERIAL (legacy form; bigint only)
	NotNull  bool   `json:"not_null,omitempty"`
	Unique   bool   `json:"unique,omitempty"`
	Check    string `json:"check,omitempty"` // verbatim CHECK expression
	// Backfill is the SQL expression `tide apply --backfill` splices into
	// a chunked UPDATE to populate this column on existing rows. The raw
	// string is parsed and purity-checked by
	// sqlvalidate.ValidateBackfillExpression at admin.PlanSchema time —
	// keeping the dsl package CGO-free.
	Backfill string   `json:"backfill,omitempty"`
	Default  *Default `json:"default,omitempty"`
	Ref      *Ref     `json:"ref,omitempty"`

	// ProtoNumber is the stable protobuf field number for this column when
	// rendered as a message field. Assigned once and persisted in the IR
	// checkpoint; never reused even if the field is later removed. 0 means
	// "not yet assigned" — the codegen entry point fills these before any
	// .proto is emitted.
	ProtoNumber int `json:"proto_number,omitempty"`
}

// FieldType is a resolved column type. For primitives only Name matters.
// For vector/numeric/varchar/array, the parametric fields are populated.
type FieldType struct {
	Name    string     `json:"name"`              // "bigint", "text", "varchar", "vector", "numeric", "jsonb", ...
	Array   bool       `json:"array,omitempty"`   // true for []Type
	Elem    *FieldType `json:"elem,omitempty"`    // populated when Array is true
	VecDim  int        `json:"vec_dim,omitempty"` // for vector(N)
	Len     int        `json:"len,omitempty"`     // for varchar(N)
	NumP    int        `json:"num_p,omitempty"`   // numeric precision
	NumS    int        `json:"num_s,omitempty"`   // numeric scale
	HasNumP bool       `json:"has_num_p,omitempty"`
}

// Default captures a column DEFAULT expression. Exactly one of Str/Int/Bool/Now is meaningful.
type Default struct {
	Kind DefaultIRKind `json:"kind"`
	Str  string        `json:"str,omitempty"`
	Int  int64         `json:"int,omitempty"`
	Bool bool          `json:"bool,omitempty"`
}

// DefaultIRKind enumerates the variants a resolved Default can take.
type DefaultIRKind string

const (
	DefaultIRString DefaultIRKind = "string"
	DefaultIRInt    DefaultIRKind = "int"
	DefaultIRBool   DefaultIRKind = "bool"
	DefaultIRNow    DefaultIRKind = "now"
	DefaultIRRaw    DefaultIRKind = "raw" // verbatim SQL expression in Str
)

// Ref is a resolved foreign-key reference. TargetID is the canonical
// "namespace.Name" of the target entity; TargetField is the column on it.
//
// TargetTableName mirrors the target entity's `table "..."` modifier
// when it has one — captured at Lower time so the codegen can render
// the FK against the correct physical schema/table without re-walking
// the IR. Empty when the target uses default atlantis naming.
type Ref struct {
	TargetID        string    `json:"target_id"`
	TargetField     string    `json:"target_field"`
	TargetTableName string    `json:"target_table_name,omitempty"`
	OnDelete        RefAction `json:"on_delete,omitempty"`
	OnUpdate        RefAction `json:"on_update,omitempty"`
}

// Relation mirrors a parser RelationDecl but with Target resolved to an
// existing entity ID.
type Relation struct {
	Kind     RelationKind `json:"kind"`
	Name     string       `json:"name"`
	TargetID string       `json:"target_id"` // canonical namespace.Name
	Via      string       `json:"via"`
}

// UniqueSpec is a resolved composite UNIQUE constraint at entity scope.
// Single-column uniqueness is on the Field itself (Field.Unique); this is
// only for multi-column constraints.
type UniqueSpec struct {
	Fields []string `json:"fields"`
}

// TableCheck is a resolved table-level CHECK constraint. The Expr is
// passed verbatim to Postgres; Name is optional (the SQL emitter
// synthesizes a stable one if absent).
type TableCheck struct {
	Name string `json:"name,omitempty"`
	Expr string `json:"expr"`
}

// Index is a resolved index. Fields populated per Kind.
type Index struct {
	Kind   IndexKind    `json:"kind"`
	Fields []IndexField `json:"fields,omitempty"` // btree, partial
	Field  string       `json:"field,omitempty"`  // hnsw, gin
	VecOps VectorOps    `json:"vec_ops,omitempty"`
	Where  *PartialPred `json:"where,omitempty"` // partial only
}

// PartialPred is the resolved form of a partial-index predicate.
//
// Two forms:
//
//	IS [NOT] NULL test: Op == "", IsNull bool.
//	Comparison:         Op in {"=","!=","<","<=",">",">="}, Literal set.
type PartialPred struct {
	Field   string   `json:"field"`
	IsNull  bool     `json:"is_null,omitempty"`
	Op      string   `json:"op,omitempty"`
	Literal *Default `json:"literal,omitempty"`
}

// Cache holds the resolved cache stanza.
type Cache struct {
	HasReadThrough bool         `json:"has_read_through,omitempty"`
	TTLMS          int          `json:"ttl_ms,omitempty"`
	Tag            string       `json:"tag,omitempty"`
	TagFields      []string     `json:"tag_fields,omitempty"` // fields referenced by {placeholders} in Tag
	Invalidate     []Invalidate `json:"invalidate,omitempty"`
	Consistency    Consistency  `json:"consistency,omitempty"`
}

// Invalidate is a resolved invalidate_on clause. If Self is true, this entity
// is the target. Otherwise TargetID is the namespace.Name of the
// referenced entity.
type Invalidate struct {
	Self     bool        `json:"self,omitempty"`
	TargetID string      `json:"target_id,omitempty"`
	Where    *InvalWhere `json:"where,omitempty"`
}

// InvalWhere is the resolved form of an `invalidate_on where ...` clause.
type InvalWhere struct {
	Field     string `json:"field"`      // field on the target
	SelfField string `json:"self_field"` // field on self
}

// CurrentIRVersion bumps whenever the JSON shape of IR changes
// incompatibly. Used by the diff engine to refuse stale checkpoints.
const CurrentIRVersion = 1

// ---- Lower: AST -> IR ----

// Lower converts a set of parsed Files into a single IR, resolving FK targets
// and enforcing the grammar's validation rules.
//
// Multiple files are merged: each file's entities are added to the IR.
// Duplicate entity names (globally, across all namespaces) are a hard error.
// Unresolved references are a hard error.
//
// The returned IR is well-formed only if err == nil.
func Lower(files []*File) (*IR, error) {
	ir := &IR{Version: CurrentIRVersion}
	var errs []error

	// Pass 1: collect entities first. QueryDecl/ProcedureDecl lowering
	// depends on every entity being resolvable, so we deliberately make
	// two parser-AST passes — one for entities, one for queries +
	// procedures. Other decl shapes (future enum/view) would slot into
	// pass 1 alongside entities.
	//
	// File-source paths are threaded into custom queries/procedures so
	// pg_query_go error messages can point at the right .atl file.
	seen := map[string]Position{} // canonical id -> position of first decl
	type queryAST struct {
		file *File
		decl Decl
	}
	var deferred []queryAST
	for _, f := range files {
		for _, d := range f.Decls {
			switch d.(type) {
			case *QueryDecl, *ProcedureDecl, *JobDecl, *WorkflowDecl:
				deferred = append(deferred, queryAST{file: f, decl: d})
				continue
			}
			e, perr := lowerDecl(f.Path, d)
			if perr != nil {
				errs = append(errs, perr...)
				continue
			}
			if e == nil {
				continue
			}
			id := e.ID()
			if first, ok := seen[id]; ok {
				errs = append(errs, fmt.Errorf("%s: duplicate entity %s (first declared at %s)", d.Position(), id, first))
				continue
			}
			seen[id] = d.Position()
			ir.Entities = append(ir.Entities, *e)
		}
	}

	// Stable order — tests and diffs both depend on a canonical ordering.
	sort.Slice(ir.Entities, func(i, j int) bool {
		return ir.Entities[i].ID() < ir.Entities[j].ID()
	})

	// Build index for pass 2 lookups.
	byID := make(map[string]*Entity, len(ir.Entities))
	for i := range ir.Entities {
		byID[ir.Entities[i].ID()] = &ir.Entities[i]
	}

	// Pass 2: resolve references, validate entities.
	for i := range ir.Entities {
		e := &ir.Entities[i]
		errs = append(errs, validateEntity(e, byID)...)
	}

	// Pass 2.5: cross-entity invariants. Today: two entities can't claim
	// the same physical table (the `table "<schema.table>"` modifier).
	// Localizing it here keeps validateEntity entity-scoped.
	errs = append(errs, validateUniqueTableNames(ir.Entities)...)

	// Pass 2.6: thread the target entity's `table "..."` override onto
	// every Ref so the codegen renders FKs against the override location
	// without needing the IR map. Skipped Refs (dangling TargetID, no
	// override on target) leave TargetTableName empty and fall through to
	// the computed atlantis name at emit time.
	for i := range ir.Entities {
		for j := range ir.Entities[i].Fields {
			ref := ir.Entities[i].Fields[j].Ref
			if ref == nil {
				continue
			}
			if target, ok := byID[ref.TargetID]; ok && target.TableName != "" {
				ref.TargetTableName = target.TableName
			}
		}
	}

	// Pass 3: lower queries and procedures now that every entity exists.
	// Each construct's owning namespace is the namespace of its target;
	// unqualified entity references inside `for`, `touches`, and typed
	// steps inherit that namespace.
	customSeen := map[string]Position{}

	// Sub-pass 3a: lower jobs first so the byJobID lookup the
	// procedure-step lowering needs (for `enqueue` step validation) is
	// populated before any procedure runs through lowerProcedure.
	for _, q := range deferred {
		d, ok := q.decl.(*JobDecl)
		if !ok {
			continue
		}
		job, jerrs := lowerJob(q.file.Path, d)
		errs = append(errs, jerrs...)
		if job == nil {
			continue
		}
		id := job.ID()
		if first, ok := customSeen[id]; ok {
			errs = append(errs, fmt.Errorf("%s: duplicate job %s (first declared at %s)", d.Position(), id, first))
			continue
		}
		if first, ok := seen[id]; ok {
			errs = append(errs, fmt.Errorf("%s: job %s collides with entity declared at %s", d.Position(), id, first))
			continue
		}
		customSeen[id] = d.Position()
		ir.Jobs = append(ir.Jobs, *job)
	}
	byJobID := make(map[string]*Job, len(ir.Jobs))
	for i := range ir.Jobs {
		byJobID[ir.Jobs[i].ID()] = &ir.Jobs[i]
	}

	// Sub-pass 3b: queries + procedures. Procedures with `enqueue`
	// steps consult byJobID to resolve the target job and pull its
	// queue / max_retries / timeout into the step IR.
	for _, q := range deferred {
		switch d := q.decl.(type) {
		case *QueryDecl:
			cq, qerrs := lowerQuery(q.file.Path, d, byID)
			errs = append(errs, qerrs...)
			if cq == nil {
				continue
			}
			id := cq.ID()
			if first, ok := customSeen[id]; ok {
				errs = append(errs, fmt.Errorf("%s: duplicate query %s (first declared at %s)", d.Position(), id, first))
				continue
			}
			customSeen[id] = d.Position()
			ir.Queries = append(ir.Queries, *cq)
		case *ProcedureDecl:
			cp, perrs := lowerProcedure(q.file.Path, d, byID, byJobID)
			errs = append(errs, perrs...)
			if cp == nil {
				continue
			}
			id := cp.ID()
			if first, ok := customSeen[id]; ok {
				errs = append(errs, fmt.Errorf("%s: duplicate procedure %s (first declared at %s)", d.Position(), id, first))
				continue
			}
			customSeen[id] = d.Position()
			ir.Procedures = append(ir.Procedures, *cp)
		}
	}
	// Sub-pass 3c: workflows. Reference jobs by id (already in byJobID).
	for _, q := range deferred {
		d, ok := q.decl.(*WorkflowDecl)
		if !ok {
			continue
		}
		wf, werrs := lowerWorkflow(q.file.Path, d, byJobID)
		errs = append(errs, werrs...)
		if wf == nil {
			continue
		}
		id := wf.ID()
		if first, ok := customSeen[id]; ok {
			errs = append(errs, fmt.Errorf("%s: duplicate workflow %s (first declared at %s)", d.Position(), id, first))
			continue
		}
		customSeen[id] = d.Position()
		ir.Workflows = append(ir.Workflows, *wf)
	}

	sort.Slice(ir.Queries, func(i, j int) bool { return ir.Queries[i].ID() < ir.Queries[j].ID() })
	sort.Slice(ir.Procedures, func(i, j int) bool { return ir.Procedures[i].ID() < ir.Procedures[j].ID() })
	sort.Slice(ir.Jobs, func(i, j int) bool { return ir.Jobs[i].ID() < ir.Jobs[j].ID() })
	sort.Slice(ir.Workflows, func(i, j int) bool { return ir.Workflows[i].ID() < ir.Workflows[j].ID() })

	if len(errs) > 0 {
		return ir, errors.Join(errs...)
	}
	return ir, nil
}

// lowerDecl converts one AST top-level decl into an IR Entity.
// Per-decl errors are returned alongside a best-effort entity so other decls
// can still be validated.
func lowerDecl(path string, d Decl) (*Entity, []error) {
	var errs []error
	switch dd := d.(type) {
	case *EntityDecl:
		e := &Entity{
			Name:      dd.Name,
			Namespace: dd.Namespace,
			Kind:      EntityKindRegular,
		}
		errs = append(errs, lowerMembers(path, dd.Members, e)...)
		return e, errs
	case *HypertableDecl:
		e := &Entity{
			Name:      dd.Name,
			Namespace: dd.Namespace,
			Kind:      EntityKindHypertable,
			TimeField: dd.TimeField,
		}
		errs = append(errs, lowerMembers(path, dd.Members, e)...)
		return e, errs
	case *QueryDecl, *ProcedureDecl, *JobDecl, *WorkflowDecl:
		// Handled in Lower's pass 3 after every entity has resolved.
		// The Lower dispatcher already skips these before calling
		// lowerDecl; this case keeps the switch exhaustive.
		return nil, nil
	default:
		return nil, []error{fmt.Errorf("%s: unknown decl type %T", d.Position(), d)}
	}
}

func lowerMembers(_ string, ms []EntityMember, e *Entity) []error {
	var errs []error
	fieldNames := map[string]Position{}

	for _, m := range ms {
		switch mm := m.(type) {
		case *FieldDecl:
			if first, dup := fieldNames[mm.Name]; dup {
				errs = append(errs, fmt.Errorf("%s: duplicate field %s (first at %s)", mm.Pos, mm.Name, first))
				continue
			}
			fieldNames[mm.Name] = mm.Pos
			f, ferrs := lowerField(mm)
			errs = append(errs, ferrs...)
			e.Fields = append(e.Fields, f)
		case *RelationDecl:
			e.Relations = append(e.Relations, Relation{
				Kind: mm.Kind,
				Name: mm.Name,
				// Target is resolved in validateEntity (needs cross-entity context).
				// We stash the bare name here; namespace inference happens later.
				TargetID: mm.Target,
				Via:      mm.Via,
			})
		case *IndexDecl:
			e.Indexes = append(e.Indexes, lowerIndex(mm))
		case *UniqueDecl:
			// Canonicalize: single-field `unique by foo` is the same shape
			// as the field-level `unique` modifier. Lower both into
			// Field.Unique so the IR has one spelling and downstream
			// (adopt drift, codegen, diff) doesn't see them as different.
			if len(mm.Fields) == 1 {
				for i := range e.Fields {
					if e.Fields[i].Name == mm.Fields[0] {
						e.Fields[i].Unique = true
						break
					}
				}
				break
			}
			e.Uniques = append(e.Uniques, UniqueSpec{Fields: slices.Clone(mm.Fields)})
		case *PrimaryDecl:
			e.CompositePK = slices.Clone(mm.Fields)
		case *TableCheckDecl:
			e.Checks = append(e.Checks, TableCheck{Name: mm.Name, Expr: mm.Expr})
		case *SoftDeleteDecl:
			e.SoftDeleteField = mm.Field
		case *TouchOnUpdateDecl:
			e.TouchOnUpdateField = mm.Field
		case *PartitionByDecl:
			e.PartitionField = mm.Field
		case *TtlFieldDecl:
			e.TtlField = mm.Field
		case *TableNameDecl:
			e.TableName = mm.Name
		case *CacheBlock:
			c, cerrs := lowerCache(mm, e)
			errs = append(errs, cerrs...)
			e.Cache = c
		case *QueryTimeoutDecl:
			ms, perr := parseDurationMS(mm.Duration)
			if perr != nil {
				errs = append(errs, fmt.Errorf("%s: invalid query_timeout %q: %v", mm.Pos, mm.Duration, perr))
				continue
			}
			e.QueryTimeoutMS = ms
		}
	}
	return errs
}

func lowerField(fd *FieldDecl) (Field, []error) {
	var errs []error
	f := Field{
		Name: fd.Name,
		Type: lowerType(fd.Type),
	}
	for _, mod := range fd.Modifiers {
		switch m := mod.(type) {
		case *ModPrimaryDecl:
			f.Primary = true
			// Primary implies NotNull.
			f.NotNull = true
		case *ModIdentityDecl:
			f.Identity = true
			// Identity implies NotNull — Postgres won't accept NULL on a
			// GENERATED ALWAYS column anyway.
			f.NotNull = true
		case *ModSerialDecl:
			f.Serial = true
			f.NotNull = true
		case *ModNotNullDecl:
			f.NotNull = true
		case *ModUniqueDecl:
			f.Unique = true
		case *ModCheckDecl:
			f.Check = m.Expr
		case *ModBackfillDecl:
			f.Backfill = m.Expr
		case *ModDefaultDecl:
			d, derr := lowerDefault(m.Value)
			if derr != nil {
				errs = append(errs, fmt.Errorf("%s: %v", m.Pos, derr))
				continue
			}
			f.Default = d
		case *ModReferencesDecl:
			f.Ref = &Ref{
				TargetID:    m.TargetNS + "." + m.TargetEntity,
				TargetField: m.TargetField,
				OnDelete:    m.OnDelete,
				OnUpdate:    m.OnUpdate,
			}
		}
	}
	return f, errs
}

func lowerType(t TypeRef) FieldType {
	ft := FieldType{
		Name:    t.Name,
		VecDim:  t.VecDim,
		Len:     t.Len,
		NumP:    t.NumP,
		NumS:    t.NumS,
		HasNumP: t.HasNumP,
	}
	if t.Array {
		ft.Array = true
		inner := lowerType(*t.Elem)
		ft.Elem = &inner
	}
	return ft
}

func lowerDefault(dv DefaultValue) (*Default, error) {
	switch dv.Kind {
	case DefaultString:
		return &Default{Kind: DefaultIRString, Str: dv.Str}, nil
	case DefaultInt:
		return &Default{Kind: DefaultIRInt, Int: dv.Int}, nil
	case DefaultBool:
		return &Default{Kind: DefaultIRBool, Bool: dv.Bool}, nil
	case DefaultNow:
		return &Default{Kind: DefaultIRNow}, nil
	case DefaultRaw:
		return &Default{Kind: DefaultIRRaw, Str: dv.Str}, nil
	default:
		return nil, fmt.Errorf("unknown default kind %v", dv.Kind)
	}
}

func lowerIndex(d *IndexDecl) Index {
	idx := Index{
		Kind:   d.Kind,
		Fields: d.Fields,
		Field:  d.Field,
		VecOps: d.VecOps,
	}
	if d.Where != nil {
		idx.Where = &PartialPred{
			Field:  d.Where.Field,
			IsNull: d.Where.IsNull,
			Op:     d.Where.Op,
		}
		if d.Where.Op != "" {
			lit, err := lowerDefault(d.Where.Literal)
			if err == nil {
				idx.Where.Literal = lit
			}
		}
	}
	return idx
}

func lowerCache(cb *CacheBlock, _ *Entity) (*Cache, []error) {
	c := &Cache{
		HasReadThrough: cb.HasReadThrough,
		Tag:            cb.Tag,
		Consistency:    cb.Consistency,
	}
	var errs []error
	if cb.TTL != "" {
		ms, err := parseDurationMS(cb.TTL)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid ttl %q: %v", cb.Pos, cb.TTL, err))
		} else {
			c.TTLMS = ms
		}
	}
	// Extract {placeholder} fields from the tag template; validation happens
	// in validateEntity once we know the entity's field set.
	c.TagFields = parseTagPlaceholders(cb.Tag)

	for _, ic := range cb.Invalidate {
		i := Invalidate{Self: ic.Self, TargetID: ic.Target}
		if ic.Where != nil {
			i.Where = &InvalWhere{Field: ic.Where.Field, SelfField: ic.Where.SelfField}
		}
		c.Invalidate = append(c.Invalidate, i)
	}
	return c, errs
}

// parseDurationMS converts our DSL duration literal (e.g. "10m", "1h", "30s",
// "7d") into milliseconds. Returns an error for unknown units.
func parseDurationMS(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	unit := s[len(s)-1]
	numPart := s[:len(s)-1]
	n, err := strconv.Atoi(numPart)
	if err != nil {
		return 0, fmt.Errorf("not a number: %s", numPart)
	}
	switch unit {
	case 's':
		return n * 1000, nil
	case 'm':
		return n * 60 * 1000, nil
	case 'h':
		return n * 60 * 60 * 1000, nil
	case 'd':
		return n * 24 * 60 * 60 * 1000, nil
	default:
		return 0, fmt.Errorf("unknown duration unit %q (use s/m/h/d)", unit)
	}
}

// parseTagPlaceholders extracts {name} placeholders from a tag template.
// Returns nil for an empty / placeholder-free string.
func parseTagPlaceholders(tag string) []string {
	if tag == "" {
		return nil
	}
	var out []string
	for {
		open := strings.IndexByte(tag, '{')
		if open < 0 {
			return out
		}
		close := strings.IndexByte(tag[open:], '}')
		if close < 0 {
			return out
		}
		out = append(out, tag[open+1:open+close])
		tag = tag[open+close+1:]
	}
}

// ---- Validation ----

// validateEntity enforces every grammar validation rule against a
// fully-lowered entity, resolving cross-entity references against byID.
//
// Rules:
//  1. Exactly one primary field per entity.
//  2. All references targets must resolve to an existing entity+field; target
//     field must be primary or unique.
//  3. has_many/has_one target entity must exist and the via field must exist.
//  4. Tag templates may only interpolate fields declared on this entity.
//  5. index hnsw requires a vector(...) field.
//  6. index gin requires jsonb or []Type field.
//  7. Field names unique within an entity (enforced at lower time).
//  8. Entity names globally unique across namespaces (enforced at lower time).
//  9. vector(n) dimensions match across all uses of the same field.
//     (Only relevant if we ever index across multiple vectors of mismatched
//     dims — the index reads the field's own type so this is auto-ok.)
//
// 10. query_timeout must be 50ms..30s.
func validateEntity(e *Entity, byID map[string]*Entity) []error {
	var errs []error

	// Rule 1: exactly one primary key. Either a single field carries the
	// `primary` modifier OR the entity has a `primary by` composite, but
	// not both, and not neither.
	primaries := 0
	for _, f := range e.Fields {
		if f.Primary {
			primaries++
		}
	}
	hasComposite := len(e.CompositePK) > 0
	switch {
	case primaries > 1:
		errs = append(errs, fmt.Errorf("%s: multiple fields carry primary; use `primary by a, b` for composite keys", e.ID()))
	case primaries == 1 && hasComposite:
		errs = append(errs, fmt.Errorf("%s: cannot combine field-level `primary` with `primary by`", e.ID()))
	case primaries == 0 && !hasComposite:
		errs = append(errs, fmt.Errorf("%s: must have a primary key (single `primary` field or `primary by ...`)", e.ID()))
	case hasComposite:
		// Validate each named field exists.
		for _, name := range e.CompositePK {
			if e.FindField(name) == nil {
				errs = append(errs, fmt.Errorf("%s: primary by references unknown field %q", e.ID(), name))
			}
		}
	}

	// Rule 2: references resolve and target is primary/unique.
	for i := range e.Fields {
		f := &e.Fields[i]
		if f.Ref == nil {
			continue
		}
		target, ok := byID[f.Ref.TargetID]
		if !ok {
			errs = append(errs, fmt.Errorf("%s.%s references unknown entity %s", e.ID(), f.Name, f.Ref.TargetID))
			continue
		}
		tf := target.FindField(f.Ref.TargetField)
		if tf == nil {
			errs = append(errs, fmt.Errorf("%s.%s references unknown field %s.%s", e.ID(), f.Name, f.Ref.TargetID, f.Ref.TargetField))
			continue
		}
		if !tf.Primary && !tf.Unique {
			errs = append(errs, fmt.Errorf("%s.%s references %s.%s which is neither primary nor unique", e.ID(), f.Name, f.Ref.TargetID, f.Ref.TargetField))
		}
	}

	// Rule 3: has_many / has_one — target exists, via field exists on target.
	// AST stored the bare target name; we infer namespace by searching.
	for ri := range e.Relations {
		r := &e.Relations[ri]
		// If the user wrote "OtherEntity" without ns, find the entity by bare
		// name. Multiple namespaces may declare an entity with the same name
		// (e.g., consumer.CartItem and vendor.CartItem); we resolve in favor
		// of the referrer's own namespace before giving up as ambiguous.
		resolved, ok := resolveByNameInNS(byID, r.TargetID, e.Namespace)
		if !ok {
			errs = append(errs, fmt.Errorf("%s relation %s: target entity %s not found", e.ID(), r.Name, r.TargetID))
			continue
		}
		r.TargetID = resolved.ID()
		if resolved.FindField(r.Via) == nil {
			errs = append(errs, fmt.Errorf("%s relation %s: via field %q not found on %s", e.ID(), r.Name, r.Via, resolved.ID()))
		}
	}

	// Rule 4: tag template placeholders reference only this entity's fields.
	if e.Cache != nil {
		for _, name := range e.Cache.TagFields {
			if e.FindField(name) == nil {
				errs = append(errs, fmt.Errorf("%s cache tag references unknown field %q", e.ID(), name))
			}
		}
		// Validate Invalidate targets too.
		for ii := range e.Cache.Invalidate {
			inv := &e.Cache.Invalidate[ii]
			if inv.Self {
				continue
			}
			resolved, ok := resolveByNameInNS(byID, inv.TargetID, e.Namespace)
			if !ok {
				errs = append(errs, fmt.Errorf("%s cache invalidate_on: target entity %s not found", e.ID(), inv.TargetID))
				continue
			}
			inv.TargetID = resolved.ID()
			if inv.Where != nil {
				if resolved.FindField(inv.Where.Field) == nil {
					errs = append(errs, fmt.Errorf("%s cache invalidate_on: target %s has no field %q",
						e.ID(), resolved.ID(), inv.Where.Field))
				}
				if e.FindField(inv.Where.SelfField) == nil {
					errs = append(errs, fmt.Errorf("%s cache invalidate_on: self has no field %q",
						e.ID(), inv.Where.SelfField))
				}
			}
		}
	}

	// Rule: `table "<schema.table>"` value must be a well-formed identifier
	// pair. Format: `[schema.]table`, each part matching the standard
	// Postgres unquoted-identifier shape. Defense-in-depth against the
	// emitter wrapping a malicious value into the SQL it executes.
	if e.TableName != "" {
		if err := validateTableNameShape(e.TableName); err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid table name %q: %v", e.ID(), e.TableName, err))
		}
	}

	// Rule: soft_delete by <field> must reference an existing timestamptz
	// column. The column itself stays declared by the engineer; this
	// declaration just opts in to the soft-delete generated behavior.
	if e.SoftDeleteField != "" {
		sf := e.FindField(e.SoftDeleteField)
		if sf == nil {
			errs = append(errs, fmt.Errorf("%s: soft_delete references unknown field %q",
				e.ID(), e.SoftDeleteField))
		} else if sf.Type.Name != "timestamptz" {
			errs = append(errs, fmt.Errorf("%s: soft_delete field %q must be timestamptz, got %s",
				e.ID(), e.SoftDeleteField, sf.Type.Name))
		} else if sf.NotNull {
			errs = append(errs, fmt.Errorf("%s: soft_delete field %q must be nullable (active rows hold NULL)",
				e.ID(), e.SoftDeleteField))
		}
	}

	// Rule: touch_on_update by <field> must reference an existing
	// timestamptz column. The trigger sets it to now() on every UPDATE,
	// so a non-timestamp column would be a confusing miscompile.
	if e.TouchOnUpdateField != "" {
		tf := e.FindField(e.TouchOnUpdateField)
		if tf == nil {
			errs = append(errs, fmt.Errorf("%s: touch_on_update references unknown field %q",
				e.ID(), e.TouchOnUpdateField))
		} else if tf.Type.Name != "timestamptz" {
			errs = append(errs, fmt.Errorf("%s: touch_on_update field %q must be timestamptz, got %s",
				e.ID(), e.TouchOnUpdateField, tf.Type.Name))
		}
	}

	// Rule: partition by <field> must reference an existing NOT NULL column.
	// A nullable partition column would let some rows escape tenant
	// isolation (NULL = NULL is false), defeating the safety property.
	if e.PartitionField != "" {
		pf := e.FindField(e.PartitionField)
		if pf == nil {
			errs = append(errs, fmt.Errorf("%s: partition by references unknown field %q",
				e.ID(), e.PartitionField))
		} else if !pf.NotNull {
			errs = append(errs, fmt.Errorf("%s: partition field %q must be NOT NULL (nullable partition columns break tenant isolation)",
				e.ID(), e.PartitionField))
		}
	}

	// Rule: composite UNIQUE field names must exist on the entity.
	for _, u := range e.Uniques {
		if len(u.Fields) == 0 {
			errs = append(errs, fmt.Errorf("%s: unique by must list at least one field", e.ID()))
		}
		for _, n := range u.Fields {
			if e.FindField(n) == nil {
				errs = append(errs, fmt.Errorf("%s: unique references unknown field %q", e.ID(), n))
			}
		}
	}

	// Rules 5 & 6: index kind matches field type.
	for _, idx := range e.Indexes {
		switch idx.Kind {
		case IndexHNSW:
			f := e.FindField(idx.Field)
			if f == nil {
				errs = append(errs, fmt.Errorf("%s hnsw index: field %q not found", e.ID(), idx.Field))
				continue
			}
			if f.Type.Name != "vector" {
				errs = append(errs, fmt.Errorf("%s hnsw index: field %q is %s, must be vector(N)", e.ID(), idx.Field, f.Type.Name))
			}
		case IndexGIN:
			f := e.FindField(idx.Field)
			if f == nil {
				errs = append(errs, fmt.Errorf("%s gin index: field %q not found", e.ID(), idx.Field))
				continue
			}
			if f.Type.Name != "jsonb" && !f.Type.Array {
				errs = append(errs, fmt.Errorf("%s gin index: field %q is %s, must be jsonb or array",
					e.ID(), idx.Field, f.Type.Name))
			}
		case IndexBtree, IndexPartial:
			for _, ifld := range idx.Fields {
				if ifld.IsExpr {
					// Raw SQL expression — Postgres validates at migration
					// time. We don't try to parse it here.
					continue
				}
				if e.FindField(ifld.Name) == nil {
					errs = append(errs, fmt.Errorf("%s btree index: field %q not found", e.ID(), ifld.Name))
				}
			}
			if idx.Where != nil && e.FindField(idx.Where.Field) == nil {
				errs = append(errs, fmt.Errorf("%s partial index: predicate field %q not found", e.ID(), idx.Where.Field))
			}
		}
	}

	// Rule 10: query_timeout bounds.
	if e.QueryTimeoutMS != 0 {
		if e.QueryTimeoutMS < 50 {
			errs = append(errs, fmt.Errorf("%s query_timeout %dms < 50ms minimum", e.ID(), e.QueryTimeoutMS))
		}
		if e.QueryTimeoutMS > 30_000 {
			errs = append(errs, fmt.Errorf("%s query_timeout %dms > 30s maximum", e.ID(), e.QueryTimeoutMS))
		}
	}

	// `serial` is only valid on bigint columns. Postgres's BIGSERIAL is
	// strictly a bigint+sequence shorthand; smaller integer widths have
	// their own SERIAL / SMALLSERIAL forms which we don't expose because
	// the legacy schema uses BIGSERIAL exclusively. `identity` + `serial`
	// together is rejected; they're alternative strategies.
	for _, f := range e.Fields {
		if f.Serial && f.Type.Name != "bigint" {
			errs = append(errs, fmt.Errorf("%s.%s: serial is only valid on bigint, got %s",
				e.ID(), f.Name, f.Type.Name))
		}
		if f.Serial && f.Identity {
			errs = append(errs, fmt.Errorf("%s.%s: cannot combine `serial` and `identity`",
				e.ID(), f.Name))
		}
	}

	// Vector dimension bounds. pgvector itself caps storable
	// dim at 16000 and HNSW-indexable dim at 2000; allowing arbitrary N in
	// the DSL means a hostile schema or honest typo can produce a Postgres
	// error at migration time AND let a caller submit huge []float32
	// payloads that exhaust server memory before PG rejects them.
	for _, f := range e.Fields {
		if f.Type.Name == "vector" {
			if f.Type.VecDim <= 0 {
				errs = append(errs, fmt.Errorf("%s.%s: vector dim must be > 0", e.ID(), f.Name))
			}
			if f.Type.VecDim > 16000 {
				errs = append(errs, fmt.Errorf("%s.%s: vector(%d) exceeds pgvector's 16000 maximum",
					e.ID(), f.Name, f.Type.VecDim))
			}
		}
		if f.Type.Array && f.Type.Elem != nil && f.Type.Elem.Name == "vector" {
			errs = append(errs, fmt.Errorf("%s.%s: arrays of vectors are not supported", e.ID(), f.Name))
		}
	}

	// Hypertable sanity: time field must exist and be a timestamp.
	if e.Kind == EntityKindHypertable {
		tf := e.FindField(e.TimeField)
		if tf == nil {
			errs = append(errs, fmt.Errorf("%s hypertable: time field %q not found", e.ID(), e.TimeField))
		} else if tf.Type.Name != "timestamptz" {
			errs = append(errs, fmt.Errorf("%s hypertable: time field %q must be timestamptz, got %s",
				e.ID(), e.TimeField, tf.Type.Name))
		}
	}

	return errs
}

// resolveByNameInNS resolves a bare entity name with a same-namespace
// preference. When the IR contains entities of the same bare name in
// different namespaces (common during cutover when consumer.CartItem and
// vendor.CartItem coexist), this hint disambiguates without forcing every
// `has_many` / `invalidate_on` reference to spell out the full qualified
// form (the grammar's single-Ident shape can't carry a namespace).
//
// Resolution order:
//  1. If `name` is already namespace-qualified ("ns.Entity"), use it.
//  2. If exactly one entity in `preferNS` matches the bare name, return it.
//  3. If exactly one entity across all namespaces matches, return it.
//  4. Multiple cross-namespace matches → ambiguous, no hit.
//
// validateUniqueTableNames enforces that no two entities claim the same
// physical table via the `table "<schema.table>"` modifier. A duplicate
// would silently route writes for one entity into another entity's
// rows; we fail loud at lower time instead.
func validateUniqueTableNames(entities []Entity) []error {
	seen := map[string]string{} // table-name -> first entity ID that claimed it
	var errs []error
	for i := range entities {
		e := &entities[i]
		if e.TableName == "" {
			continue
		}
		if prior, ok := seen[e.TableName]; ok {
			errs = append(errs, fmt.Errorf("table %q is claimed by both %s and %s — each `table \"...\"` value must be unique",
				e.TableName, prior, e.ID()))
			continue
		}
		seen[e.TableName] = e.ID()
	}
	return errs
}

// validateTableNameShape rejects values that aren't well-formed
// `[schema.]table` pairs. The codegen later splits on the first `.` and
// emits `"<schema>"."<table>"` verbatim, so anything that wouldn't be a
// safe Postgres unquoted identifier has to fail here.
func validateTableNameShape(name string) error {
	if !tableNamePat.MatchString(name) {
		return fmt.Errorf("must match [schema.]table where each part is [A-Za-z_][A-Za-z0-9_]*")
	}
	return nil
}

func resolveByNameInNS(byID map[string]*Entity, name string, preferNS string) (*Entity, bool) {
	if strings.Contains(name, ".") {
		if e, ok := byID[name]; ok {
			return e, true
		}
		return nil, false
	}
	var sameNS *Entity
	var anyMatch *Entity
	multiple := false
	for _, e := range byID {
		if e.Name != name {
			continue
		}
		if preferNS != "" && e.Namespace == preferNS {
			if sameNS != nil {
				// Two entities with the same bare name in the same
				// namespace — that's a genuine duplicate the validator
				// should already catch. Falling through with "ambiguous"
				// is the safe behavior.
				return nil, false
			}
			sameNS = e
		}
		if anyMatch != nil {
			multiple = true
		}
		anyMatch = e
	}
	if sameNS != nil {
		return sameNS, true
	}
	if multiple {
		return nil, false
	}
	return anyMatch, anyMatch != nil
}

// ---- JSON checkpoint helpers ----

// LookupEntity returns the entity with the given canonical id
// ("namespace.Name"), or nil if no such entity exists in the IR.
// Linear scan since the typical IR holds a few dozen entities; if
// this ever shows up in profiles we'd add a map cached on the IR.
func (ir *IR) LookupEntity(id string) *Entity {
	if ir == nil {
		return nil
	}
	for i := range ir.Entities {
		if ir.Entities[i].ID() == id {
			return &ir.Entities[i]
		}
	}
	return nil
}

// EncodeJSON serializes the IR into the checkpoint format used by
// gen/.last-ir.json. The output is deterministic (stable map iteration) and
// pretty-printed for diff-friendliness in code review.
func (ir *IR) EncodeJSON() ([]byte, error) {
	return json.MarshalIndent(ir, "", "  ")
}

// DecodeJSONIR is the inverse of EncodeJSON. Refuses checkpoints written by a
// future IR version.
func DecodeJSONIR(data []byte) (*IR, error) {
	var ir IR
	if err := json.Unmarshal(data, &ir); err != nil {
		return nil, fmt.Errorf("ir checkpoint: %w", err)
	}
	if ir.Version > CurrentIRVersion {
		return nil, fmt.Errorf("ir checkpoint version %d newer than supported %d", ir.Version, CurrentIRVersion)
	}
	return &ir, nil
}

// ---- Custom query / procedure lowering ----

// lowerQuery turns a QueryDecl into a resolved CustomQuery. The
// validation surface covers four things in addition to the structural
// parser-level checks: (1) every $arg referenced anywhere in the SQL
// or expression tree must be declared in input{}, (2) every
// EntityRef in `for`, `output as ...`, and `touches(...)` must resolve
// to a real entity, (3) every touches() entity is listed exactly once
// (duplicates would double-bump generation counters and waste outbox
// work for no semantic reason), and (4) the raw SQL body must mention
// every declared input at least once — an unused input is almost
// always a typo or a stale schema.
//
// Identifier resolution INSIDE the raw SQL body (table/column names
// referring to real entities) lives in the pg_query_go validator,
// which runs at plan time. Lowering keeps this function lean enough
// that codegen tests can run without CGO; the deeper SQL validation
// is layered on top by tidectl plan.
func lowerQuery(path string, d *QueryDecl, byID map[string]*Entity) (*CustomQuery, []error) {
	var errs []error
	// Resolve the target entity. An unqualified name binds to the
	// namespace of the first entity declared with that name — there's
	// no per-file namespace because .atl files can declare entities in
	// multiple namespaces.
	owner, err := resolveEntityRef(d.Target, byID, "")
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: query %s: %w", d.Pos, d.Name, err))
	}
	ownerNS := ""
	if owner != nil {
		ownerNS = owner.Namespace
	}

	cq := &CustomQuery{
		Name:       d.Name,
		Inputs:     lowerInputs(d.Inputs),
		Pos:        d.Pos,
		SourcePath: path,
	}
	if owner != nil {
		cq.Owner = owner.ID()
	}

	// Output: exactly one of AsEntity / Columns is populated by the
	// parser; lowering picks the resolved entity ID or the column list.
	switch {
	case d.Output == nil:
		errs = append(errs, fmt.Errorf("%s: query %s: missing output{} or output as <entity>", d.Pos, d.Name))
	case d.Output.AsEntity != nil:
		outEnt, oerr := resolveEntityRef(*d.Output.AsEntity, byID, ownerNS)
		if oerr != nil {
			errs = append(errs, fmt.Errorf("%s: query %s output: %w", d.Output.Pos, d.Name, oerr))
		} else if outEnt != nil {
			cq.Output.AsEntityID = outEnt.ID()
		}
	default:
		cq.Output.Columns = lowerInputs(d.Output.Columns)
	}

	// SQL block + touches.
	if d.SQL == nil {
		errs = append(errs, fmt.Errorf("%s: query %s: missing sql{} block", d.Pos, d.Name))
	} else {
		cq.SQL = d.SQL.Raw
		touches, tErrs := resolveTouches(d.SQL.Touches, byID, ownerNS, fmt.Sprintf("query %s", d.Name))
		errs = append(errs, tErrs...)
		cq.Touches = touches
		// Queries have one SQL body, so declared-check and used-check
		// run over the same scan in a single pass.
		declared := make(map[string]bool, len(cq.Inputs))
		for _, p := range cq.Inputs {
			declared[p.Name] = true
		}
		used, dErrs := scanRawArgRefs(cq.SQL, declared, d.SQL.Pos, fmt.Sprintf("query %s", d.Name))
		errs = append(errs, dErrs...)
		errs = append(errs, validateInputUsage(cq.Inputs, used, fmt.Sprintf("query %s", d.Name))...)
	}

	cq.Cache = lowerCacheForCustom(d.Cache)
	return cq, errs
}

// lowerProcedure turns a ProcedureDecl into a resolved CustomProcedure.
// In addition to the per-query validation rules, procedures check:
//   - Every typed step's target entity resolves.
//   - Every assigned column exists on that target entity.
//   - Every $arg referenced anywhere (typed-step exprs OR raw-SQL
//     bodies) is declared in input{}.
//   - Field references in typed-step where/set expressions resolve to
//     a column on the step's target (caller can't write `where unknown
//     = $arg`).
//   - touches() lists union out — duplicates collapsed because the
//     codegen otherwise emits N identical generation bumps per step.
func lowerProcedure(path string, d *ProcedureDecl, byID map[string]*Entity, byJobID map[string]*Job) (*CustomProcedure, []error) {
	var errs []error
	owner, err := resolveEntityRef(d.Target, byID, "")
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: procedure %s: %w", d.Pos, d.Name, err))
	}
	ownerNS := ""
	if owner != nil {
		ownerNS = owner.Namespace
	}

	cp := &CustomProcedure{
		Name:       d.Name,
		Inputs:     lowerInputs(d.Inputs),
		Pos:        d.Pos,
		SourcePath: path,
	}
	if owner != nil {
		cp.Owner = owner.ID()
	}

	if len(d.Steps) == 0 {
		errs = append(errs, fmt.Errorf("%s: procedure %s: at least one step is required", d.Pos, d.Name))
	}

	inputNames := make(map[string]bool, len(cp.Inputs))
	for _, p := range cp.Inputs {
		inputNames[p.Name] = true
	}

	// usedAcrossSteps unions every $arg reference across raw + typed
	// steps. The "declared but never used" check runs once at the
	// procedure level against this union, rather than per-step — a
	// procedure that splits work across N steps will naturally have
	// inputs referenced by only some of them.
	usedAcrossSteps := map[string]bool{}

	for i, step := range d.Steps {
		switch {
		case step.Typed != nil:
			ts, tErrs := lowerTypedStep(step.Typed, byID, ownerNS, inputNames, i, d.Name)
			errs = append(errs, tErrs...)
			if ts != nil {
				cp.Steps = append(cp.Steps, ProcedureStepIR{Typed: ts, Pos: step.Pos})
				for name := range collectTypedStepArgRefs(ts) {
					usedAcrossSteps[name] = true
				}
			}
		case step.Raw != nil:
			rsErrs := []error{}
			touches, tErrs := resolveTouches(step.Raw.Touches, byID, ownerNS, fmt.Sprintf("procedure %s step %d", d.Name, i+1))
			rsErrs = append(rsErrs, tErrs...)
			refs, dErrs := scanRawArgRefs(step.Raw.Raw, inputNames, step.Raw.Pos, fmt.Sprintf("procedure %s step %d", d.Name, i+1))
			rsErrs = append(rsErrs, dErrs...)
			for name := range refs {
				usedAcrossSteps[name] = true
			}
			errs = append(errs, rsErrs...)
			cp.Steps = append(cp.Steps, ProcedureStepIR{
				Raw: &RawSQLIR{SQL: step.Raw.Raw, Touches: touches},
				Pos: step.Pos,
			})
		case step.Enqueue != nil:
			es, eErrs := lowerEnqueueStep(step.Enqueue, byJobID, ownerNS, inputNames, i, d.Name)
			errs = append(errs, eErrs...)
			if es != nil {
				cp.Steps = append(cp.Steps, ProcedureStepIR{Enqueue: es, Pos: step.Pos})
				for _, a := range es.Args {
					if a.Value != nil && a.Value.Kind == ExprArg {
						usedAcrossSteps[a.Value.ArgName] = true
					}
				}
			}
		default:
			errs = append(errs, fmt.Errorf("%s: procedure %s step %d: empty step", step.Pos, d.Name, i+1))
		}
	}

	errs = append(errs, validateInputUsage(cp.Inputs, usedAcrossSteps, fmt.Sprintf("procedure %s", d.Name))...)

	if d.Invalidate != nil {
		cp.Invalidate = d.Invalidate.TagTpl
	}
	return cp, errs
}

// lowerTypedStep resolves a TypedStep (update/delete/insert on an
// entity) against the IR. Column references on the LHS of assigns and
// inside the where-clause are checked against the target's field list;
// $arg references are checked against the procedure's input set.
func lowerTypedStep(s *TypedStep, byID map[string]*Entity, defaultNS string, inputs map[string]bool, idx int, procName string) (*TypedStepIR, []error) {
	var errs []error
	target, err := resolveEntityRef(s.Target, byID, defaultNS)
	if err != nil {
		return nil, []error{fmt.Errorf("%s: procedure %s step %d: %w", s.Pos, procName, idx+1, err)}
	}
	out := &TypedStepIR{Verb: s.Verb, TargetID: target.ID(), Pos: s.Pos}
	for _, a := range s.Assigns {
		if target.FindField(a.Field) == nil {
			errs = append(errs, fmt.Errorf("%s: procedure %s step %d: unknown column %q on %s", a.Pos, procName, idx+1, a.Field, target.ID()))
			continue
		}
		expr, eerrs := lowerExpr(a.Value, target, inputs, procName, idx+1)
		errs = append(errs, eerrs...)
		out.Assigns = append(out.Assigns, AssignmentIR{Field: a.Field, Value: expr})
	}
	if s.WhereExpr != nil {
		expr, eerrs := lowerExpr(s.WhereExpr, target, inputs, procName, idx+1)
		errs = append(errs, eerrs...)
		out.Where = expr
	}
	return out, errs
}

// lowerExpr lowers a typed-step expression. Field references must
// resolve on `target`; $arg references must appear in `inputs`; bool /
// int / string / now literals pass through unchanged.
func lowerExpr(e Expr, target *Entity, inputs map[string]bool, procName string, stepIdx int) (*ExprIR, []error) {
	switch x := e.(type) {
	case *LiteralExpr:
		switch x.Kind {
		case "int":
			n, err := strconv.ParseInt(x.Value, 10, 64)
			if err != nil {
				return nil, []error{fmt.Errorf("%s: procedure %s step %d: malformed integer literal %q", x.Pos, procName, stepIdx, x.Value)}
			}
			return &ExprIR{Kind: ExprLiteralInt, LitInt: n}, nil
		case "string":
			return &ExprIR{Kind: ExprLiteralStr, LitStr: x.Value}, nil
		case "bool":
			return &ExprIR{Kind: ExprLiteralBool, LitBool: x.Value == "true"}, nil
		case "now":
			return &ExprIR{Kind: ExprLiteralNow}, nil
		default:
			return nil, []error{fmt.Errorf("%s: procedure %s step %d: unknown literal kind %q", x.Pos, procName, stepIdx, x.Kind)}
		}
	case *ArgExpr:
		if !inputs[x.Name] {
			return nil, []error{fmt.Errorf("%s: procedure %s step %d: $%s is not declared in input{}", x.Pos, procName, stepIdx, x.Name)}
		}
		return &ExprIR{Kind: ExprArg, ArgName: x.Name}, nil
	case *FieldExpr:
		if target.FindField(x.Name) == nil {
			return nil, []error{fmt.Errorf("%s: procedure %s step %d: unknown column %q on %s", x.Pos, procName, stepIdx, x.Name, target.ID())}
		}
		return &ExprIR{Kind: ExprField, Field: x.Name}, nil
	case *BinaryExpr:
		left, lerrs := lowerExpr(x.Left, target, inputs, procName, stepIdx)
		right, rerrs := lowerExpr(x.Right, target, inputs, procName, stepIdx)
		errs := append(lerrs, rerrs...)
		if len(errs) > 0 {
			return nil, errs
		}
		return &ExprIR{Kind: ExprBinary, Op: x.Op, Left: left, Right: right}, nil
	}
	return nil, []error{fmt.Errorf("unknown expression type %T", e)}
}

// lowerInputs turns parser InputParam slices into the IR's QueryParam
// list, lowering each type via the same helper used by entity fields.
func lowerInputs(params []InputParam) []QueryParam {
	out := make([]QueryParam, 0, len(params))
	for _, p := range params {
		ip := QueryParam{Name: p.Name, Type: lowerType(p.Type), Pos: p.Pos}
		if p.Default != nil {
			ip.Default = lowerExprToDefault(p.Default)
		}
		out = append(out, ip)
	}
	return out
}

// lowerExprToDefault collapses a parser Expr to a Default value. Only
// literal-shaped exprs are accepted in an input{}'s default slot —
// anything richer would require evaluating arbitrary expressions
// per-request, which the wire shape doesn't allow.
func lowerExprToDefault(e Expr) *Default {
	lit, ok := e.(*LiteralExpr)
	if !ok {
		return nil
	}
	switch lit.Kind {
	case "int":
		n, _ := strconv.ParseInt(lit.Value, 10, 64)
		return &Default{Kind: DefaultIRInt, Int: n}
	case "string":
		return &Default{Kind: DefaultIRString, Str: lit.Value}
	case "bool":
		return &Default{Kind: DefaultIRBool, Bool: lit.Value == "true"}
	case "now":
		return &Default{Kind: DefaultIRNow}
	}
	return nil
}

// resolveEntityRef finds the entity an EntityRef points to. The
// namespace logic mirrors how cross-namespace FKs work in entity
// fields: explicit "ns.Entity" is required for cross-namespace, while
// an unqualified name resolves against defaultNS first, then falls
// back to any entity with that name (matching the only-one-of-them
// case so a top-level `for Account` works when only consumer.Account
// exists).
func resolveEntityRef(ref EntityRef, byID map[string]*Entity, defaultNS string) (*Entity, error) {
	if ref.Namespace != "" {
		id := ref.Namespace + "." + ref.Name
		if e, ok := byID[id]; ok {
			return e, nil
		}
		return nil, fmt.Errorf("unknown entity %s", id)
	}
	if defaultNS != "" {
		if e, ok := byID[defaultNS+"."+ref.Name]; ok {
			return e, nil
		}
	}
	// Last resort: search every namespace for a unique match. Ambiguity
	// (same Name in two namespaces) is an error — the caller must
	// qualify.
	var found *Entity
	for id, e := range byID {
		if strings.HasSuffix(id, "."+ref.Name) {
			if found != nil {
				return nil, fmt.Errorf("entity %q is ambiguous (found in multiple namespaces — qualify with namespace.Name)", ref.Name)
			}
			found = e
		}
	}
	if found == nil {
		return nil, fmt.Errorf("unknown entity %q", ref.Name)
	}
	return found, nil
}

// resolveTouches validates and dedupes a touches() list. Returns
// canonical entity IDs in sorted order so diffs and codegen are
// deterministic, and so two procedures listing the same entities in
// different orders share their generation-bump set.
func resolveTouches(refs []EntityRef, byID map[string]*Entity, defaultNS string, context string) ([]string, []error) {
	if len(refs) == 0 {
		return nil, []error{fmt.Errorf("%s: touches() requires at least one entity", context)}
	}
	var errs []error
	seen := make(map[string]bool, len(refs))
	var ids []string
	for _, r := range refs {
		e, err := resolveEntityRef(r, byID, defaultNS)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: touches: %w", context, err))
			continue
		}
		id := e.ID()
		if seen[id] {
			errs = append(errs, fmt.Errorf("%s: touches lists %s more than once", context, id))
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, errs
}

// scanRawArgRefs walks one raw SQL block once, returning the set of
// `$ident` placeholder references it contains and emitting an error
// for any reference not in declared. Caller unions ref sets across
// multiple SQL blocks (e.g., a procedure with several steps) for the
// per-procedure unused-input check.
//
// This is the lightweight, dep-free pass; the deeper pg_query_go pass
// (which validates table/column references too) runs at plan time.
// Dollar-quoted strings (`$tag$ ... $tag$`) are skipped — pg_query_go
// catches malformed ones at plan time, and treating them as
// placeholders here would produce noisy false positives. The scan
// only commits to a placeholder when the trailing character after the
// identifier is NOT a `$`.
func scanRawArgRefs(sql string, declared map[string]bool, pos Position, context string) (map[string]bool, []error) {
	refs := map[string]bool{}
	if sql == "" {
		return refs, nil
	}
	var errs []error
	for i := 0; i < len(sql); i++ {
		if sql[i] != '$' {
			continue
		}
		j := i + 1
		if j >= len(sql) || !(isIdentStart(rune(sql[j])) || sql[j] == '_') {
			continue
		}
		k := j
		for k < len(sql) && (isIdentRune(rune(sql[k])) || sql[k] == '_') {
			k++
		}
		if k < len(sql) && sql[k] == '$' {
			i = k
			continue
		}
		name := sql[j:k]
		i = k - 1
		if refs[name] {
			continue
		}
		refs[name] = true
		if !declared[name] {
			errs = append(errs, fmt.Errorf("%s: $%s referenced in SQL but not declared in input{} (declared at %s)", context, name, pos))
		}
	}
	return refs, errs
}

// collectTypedStepArgRefs walks the assigns and where-clause of one
// typed step, returning the set of `$arg` names referenced. Mirrors
// the visitor pattern lowerExpr uses, but only gathers names — the
// "must be declared" check already runs inside lowerExpr when the
// typed step is first lowered, so collection happens against an
// already-validated tree.
func collectTypedStepArgRefs(step *TypedStepIR) map[string]bool {
	refs := map[string]bool{}
	if step == nil {
		return refs
	}
	for _, a := range step.Assigns {
		collectArgRefsFromExpr(a.Value, refs)
	}
	collectArgRefsFromExpr(step.Where, refs)
	return refs
}

func collectArgRefsFromExpr(e *ExprIR, into map[string]bool) {
	if e == nil {
		return
	}
	switch e.Kind {
	case ExprArg:
		into[e.ArgName] = true
	case ExprBinary:
		collectArgRefsFromExpr(e.Left, into)
		collectArgRefsFromExpr(e.Right, into)
	}
}

// validateInputUsage checks that every declared input appears in the
// supplied used set. Caller constructs `used` by union-ing references
// across whatever scopes the rule applies to — a single SQL block for
// queries, every step for procedures. One error per unused input,
// attributed to its declaration position.
func validateInputUsage(inputs []QueryParam, used map[string]bool, context string) []error {
	var errs []error
	for _, p := range inputs {
		if !used[p.Name] {
			errs = append(errs, fmt.Errorf("%s: input $%s is declared but never referenced in SQL (declared at %s)", context, p.Name, p.Pos))
		}
	}
	return errs
}

// lowerCacheForCustom adapts an entity-style CacheBlock to the IR's
// Cache type for use on a custom query. Custom queries care about
// `ttl`, `tag`, and `invalidate_on` semantics in the same shape as
// entities, so we reuse the same lowering path.
func lowerCacheForCustom(cb *CacheBlock) *Cache {
	if cb == nil {
		return nil
	}
	// Reuse the entity cache lowering; the entity-bound fields it sets
	// (TagFields cross-checks against entity fields) are not validated
	// here — for custom queries the tag template references input args,
	// not entity fields, so cross-checking would produce false errors.
	c, _ := lowerCache(cb, &Entity{})
	return c
}

// lowerJob converts a JobDecl AST into an IR Job. The function performs
// three categories of validation:
//
//  1. Arg modifier whitelist. Storage-only modifiers (primary, identity,
//     serial, unique, references, backfill) don't make sense on a
//     function-call argument; reject with a precise position. NotNull,
//     Default, and Check survive — they constrain values the caller
//     supplies on the wire.
//
//  2. Runtime modifier shape. retries must be non-negative. timeout
//     parses through the shared parseDurationMS helper used by cache
//     TTLs and query-timeout modifiers, so the DSL has a single
//     duration grammar. queue and schedule are stored verbatim;
//     cron-spec validation is deferred to the scheduler component
//     where the cron library lives.
//
//  3. Uniqueness across the merged IR. Caller is responsible (via the
//     customSeen / seen maps in Lower) — lowerJob only assembles the
//     Job value; the caller intercepts collisions.
func lowerJob(path string, d *JobDecl) (*Job, []error) {
	var errs []error
	job := &Job{
		Name:       d.Name,
		Namespace:  d.Namespace,
		SourcePath: path,
		Pos:        d.Pos,
	}

	// Args: each FieldDecl rows lowers through the shared lowerField
	// helper to inherit the type system + default-value lowering, then
	// we reject the modifiers that don't apply to function-call args.
	for _, fd := range d.Args {
		f, ferrs := lowerField(fd)
		errs = append(errs, ferrs...)
		// Reject storage-only modifiers. lowerField already populated
		// the flags; we surface them as errors and zero them so the
		// downstream IR is well-formed even when we keep going.
		for _, mod := range fd.Modifiers {
			switch mod.(type) {
			case *ModPrimaryDecl:
				errs = append(errs, fmt.Errorf("%s: job arg %q cannot be 'primary' — args are values, not stored rows", mod.Position(), fd.Name))
			case *ModIdentityDecl:
				errs = append(errs, fmt.Errorf("%s: job arg %q cannot be 'identity'", mod.Position(), fd.Name))
			case *ModSerialDecl:
				errs = append(errs, fmt.Errorf("%s: job arg %q cannot be 'serial'", mod.Position(), fd.Name))
			case *ModUniqueDecl:
				errs = append(errs, fmt.Errorf("%s: job arg %q cannot be 'unique'", mod.Position(), fd.Name))
			case *ModReferencesDecl:
				errs = append(errs, fmt.Errorf("%s: job arg %q cannot use 'references' — FKs are between stored rows", mod.Position(), fd.Name))
			case *ModBackfillDecl:
				errs = append(errs, fmt.Errorf("%s: job arg %q cannot use 'backfill' — that modifier applies to schema migrations only", mod.Position(), fd.Name))
			}
		}
		f.Primary = false
		f.Identity = false
		f.Serial = false
		f.Unique = false
		f.Ref = nil
		f.Backfill = ""
		job.Args = append(job.Args, f)
	}

	if d.Retries != nil {
		if d.Retries.Count < 0 {
			errs = append(errs, fmt.Errorf("%s: retries must be non-negative, got %d", d.Retries.Pos, d.Retries.Count))
		} else {
			job.Retries = d.Retries.Count
		}
	}
	if d.Timeout != nil {
		if d.Timeout.Duration == "none" {
			job.TimeoutNone = true
		} else {
			ms, perr := parseDurationMS(d.Timeout.Duration)
			if perr != nil {
				errs = append(errs, fmt.Errorf("%s: invalid timeout %q: %v", d.Timeout.Pos, d.Timeout.Duration, perr))
			} else {
				job.TimeoutMS = ms
			}
		}
	}
	if d.Queue != nil {
		job.Queue = d.Queue.Name
	}
	if d.Schedule != nil {
		// Cron-spec validation is deferred to the scheduler runtime
		// where the parser library is wired in. The IR layer only
		// preserves the spec verbatim; an invalid spec surfaces at
		// scheduler startup, not at DSL lowering, so a working
		// `tide plan` doesn't depend on the cron library.
		job.Schedule = d.Schedule.CronSpec
	}
	if d.VisibleTo != nil {
		job.VisibleTo = d.VisibleTo.Caller
	}

	return job, errs
}

// lowerEnqueueStep resolves an `enqueue Job(args)` step against the
// IR. Target job must exist in byJobID; arg names must match the
// job's declared args block exactly (no extras, no duplicates, and
// every NOT-NULL arg without a default must be supplied).
//
// Expression lowering accepts literals + $arg references; field
// references (FieldExpr) are rejected because there's no entity
// context in an enqueue step. Callers wanting to enqueue with a
// row's column value should bind it to an input first.
func lowerEnqueueStep(s *EnqueueStep, byJobID map[string]*Job, defaultNS string, inputs map[string]bool, idx int, procName string) (*EnqueueStepIR, []error) {
	var errs []error

	ns := s.Target.Namespace
	if ns == "" {
		ns = defaultNS
	}
	jobID := ns + "." + s.Target.Name
	job, ok := byJobID[jobID]
	if !ok {
		return nil, []error{fmt.Errorf("%s: procedure %s step %d: unknown job %s", s.Pos, procName, idx+1, jobID)}
	}

	// Build a map of declared arg names so we can verify the caller's
	// keys + spot duplicates + report missing required ones.
	declared := make(map[string]*Field, len(job.Args))
	for i := range job.Args {
		declared[job.Args[i].Name] = &job.Args[i]
	}
	supplied := make(map[string]bool, len(s.Args))
	out := &EnqueueStepIR{
		TargetJobID: jobID,
		Queue:       job.Queue,
		MaxRetries:  job.Retries,
		TimeoutMS:   job.TimeoutMS,
	}
	for _, a := range s.Args {
		if _, isDup := supplied[a.Name]; isDup {
			errs = append(errs, fmt.Errorf("%s: procedure %s step %d: duplicate enqueue arg %q", a.Pos, procName, idx+1, a.Name))
			continue
		}
		if _, ok := declared[a.Name]; !ok {
			errs = append(errs, fmt.Errorf("%s: procedure %s step %d: enqueue arg %q is not declared on job %s", a.Pos, procName, idx+1, a.Name, jobID))
			continue
		}
		supplied[a.Name] = true
		val, vErrs := lowerEnqueueValue(a.Value, inputs, procName, idx)
		errs = append(errs, vErrs...)
		if val == nil {
			continue
		}
		out.Args = append(out.Args, EnqueueAssignmentIR{Name: a.Name, Value: val})
	}

	// Required args must be supplied. "Required" = NotNull with no
	// default value declared; anything else has a fallback the worker
	// can fill in from the column metadata at row-insert time.
	for name, f := range declared {
		if supplied[name] {
			continue
		}
		if f.NotNull && f.Default == nil {
			errs = append(errs, fmt.Errorf("%s: procedure %s step %d: enqueue %s is missing required arg %q", s.Pos, procName, idx+1, jobID, name))
		}
	}

	return out, errs
}

// lowerEnqueueValue lowers a single enqueue argument expression.
// Restricted to literals + $arg references; field references are
// rejected because there's no entity context in an enqueue step.
func lowerEnqueueValue(e Expr, inputs map[string]bool, procName string, stepIdx int) (*ExprIR, []error) {
	switch x := e.(type) {
	case *LiteralExpr:
		switch x.Kind {
		case "int":
			n, err := strconv.ParseInt(x.Value, 10, 64)
			if err != nil {
				return nil, []error{fmt.Errorf("%s: procedure %s step %d: malformed integer literal %q", x.Pos, procName, stepIdx+1, x.Value)}
			}
			return &ExprIR{Kind: ExprLiteralInt, LitInt: n}, nil
		case "string":
			return &ExprIR{Kind: ExprLiteralStr, LitStr: x.Value}, nil
		case "bool":
			return &ExprIR{Kind: ExprLiteralBool, LitBool: x.Value == "true"}, nil
		case "now":
			return &ExprIR{Kind: ExprLiteralNow}, nil
		default:
			return nil, []error{fmt.Errorf("%s: procedure %s step %d: unknown literal kind %q in enqueue arg", x.Pos, procName, stepIdx+1, x.Kind)}
		}
	case *ArgExpr:
		if !inputs[x.Name] {
			return nil, []error{fmt.Errorf("%s: procedure %s step %d: $%s is not declared in input{}", x.Pos, procName, stepIdx+1, x.Name)}
		}
		return &ExprIR{Kind: ExprArg, ArgName: x.Name}, nil
	case *FieldExpr:
		return nil, []error{fmt.Errorf("%s: procedure %s step %d: enqueue arg cannot reference a row field; bind it to an input first", x.Pos, procName, stepIdx+1)}
	case *BinaryExpr:
		return nil, []error{fmt.Errorf("%s: procedure %s step %d: enqueue arg cannot be a compound expression", x.Pos, procName, stepIdx+1)}
	}
	return nil, []error{fmt.Errorf("procedure %s step %d: unknown enqueue arg type %T", procName, stepIdx+1, e)}
}

func lowerWorkflow(path string, d *WorkflowDecl, byJobID map[string]*Job) (*Workflow, []error) {
	var errs []error
	wf := &Workflow{
		Name:       d.Name,
		Namespace:  d.Namespace,
		SourcePath: path,
		Pos:        d.Pos,
	}

	// State fields: same lowering as job args (reject storage modifiers).
	stateNames := make(map[string]bool, len(d.State))
	for _, fd := range d.State {
		f, ferrs := lowerField(fd)
		errs = append(errs, ferrs...)
		for _, mod := range fd.Modifiers {
			switch mod.(type) {
			case *ModPrimaryDecl, *ModIdentityDecl, *ModSerialDecl,
				*ModUniqueDecl, *ModReferencesDecl, *ModBackfillDecl:
				errs = append(errs, fmt.Errorf("%s: workflow state field %q: storage modifiers not allowed", mod.Position(), fd.Name))
			}
		}
		f.Primary = false
		f.Identity = false
		f.Serial = false
		f.Unique = false
		f.Ref = nil
		f.Backfill = ""
		wf.State = append(wf.State, f)
		stateNames[fd.Name] = true
	}

	// Steps: resolve job refs, lower args.
	stepNames := make(map[string]bool, len(d.Steps))
	for i, s := range d.Steps {
		if stepNames[s.Name] {
			errs = append(errs, fmt.Errorf("%s: workflow %s: duplicate step name %q", s.Pos, d.Name, s.Name))
			continue
		}
		stepNames[s.Name] = true

		ns := s.JobRef.Namespace
		if ns == "" {
			ns = d.Namespace
		}
		jobID := ns + "." + s.JobRef.Name
		if _, ok := byJobID[jobID]; !ok {
			errs = append(errs, fmt.Errorf("%s: workflow %s step %d %q: unknown job %s", s.Pos, d.Name, i+1, s.Name, jobID))
			continue
		}

		step := WorkflowStepIR{Name: s.Name, TargetJobID: jobID}
		for _, a := range s.Args {
			val, vErrs := lowerEnqueueValue(a.Value, stateNames, d.Name, i)
			errs = append(errs, vErrs...)
			if val != nil {
				step.Args = append(step.Args, EnqueueAssignmentIR{Name: a.Name, Value: val})
			}
		}
		wf.Steps = append(wf.Steps, step)
	}

	// Compensations: each references a declared step.
	for _, c := range d.Compensations {
		if !stepNames[c.StepName] {
			errs = append(errs, fmt.Errorf("%s: workflow %s: compensate references unknown step %q", c.Pos, d.Name, c.StepName))
			continue
		}
		ns := c.JobRef.Namespace
		if ns == "" {
			ns = d.Namespace
		}
		jobID := ns + "." + c.JobRef.Name
		if _, ok := byJobID[jobID]; !ok {
			errs = append(errs, fmt.Errorf("%s: workflow %s compensate %q: unknown job %s", c.Pos, d.Name, c.StepName, jobID))
			continue
		}

		comp := WorkflowCompIR{StepName: c.StepName, TargetJobID: jobID}
		for _, a := range c.Args {
			val, vErrs := lowerEnqueueValue(a.Value, stateNames, d.Name, 0)
			errs = append(errs, vErrs...)
			if val != nil {
				comp.Args = append(comp.Args, EnqueueAssignmentIR{Name: a.Name, Value: val})
			}
		}
		wf.Compensations = append(wf.Compensations, comp)
	}

	return wf, errs
}
