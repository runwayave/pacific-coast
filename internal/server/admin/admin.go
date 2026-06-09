// Package admin implements the atlantis control-plane RPCs: schema
// lifecycle (plan, apply, adopt, rollback, history, lineage), caller
// identity management (register, revoke, cert-expiry tracking),
// declarative jobs and workflows (submit, status, dead/retry), and
// operational telemetry (entity owners, in-process log ring). All
// RPCs use a JSON envelope codec — see grpc.go.
package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/sqlvalidate"
	"github.com/rachitkumar205/atlantis/internal/obs"
)

// Service is safe for concurrent use; one instance per process.
//
// Mutation gating layers (defense in depth):
//
//  1. `allowApplyMutation` — a global wildcard. true means any
//     authenticated caller may mutate (dev only).
//  2. `mutationAllowed` — a per-CN allowlist. The intended prod posture:
//     `allowApplyMutation=false` and only CI's caller CNs in this set,
//     so a leaked app-server cert can't push schema.
//  3. `req.Caller == cert CN` on apply/backfill — a caller may only
//     mutate its OWN schema. Even if a CN is on the mutation allowlist,
//     it can't impersonate a different caller in the request body.
//
// (1) is unioned with (2). (3) is independent and always enforced when
// the server runs in TLS mode (a cert CN is available).
type Service struct {
	pool               *pgxpool.Pool
	mirrorDir          string
	mirrorEnabled      bool
	allowApplyMutation bool
	mutationAllowed    map[string]bool
	operatorAllowed    map[string]bool
	callerFromContext  func(context.Context) string
	backfillEnabled    bool
	logRing            *obs.LogRing
}

// Config holds optional toggles; the zero value is read-only with no mirror.
type Config struct {
	// MirrorDir is the root path for mirrored files. Ignored when MirrorEnabled is false.
	MirrorDir string

	// MirrorEnabled, when true, mirrors applied files to MirrorDir.
	MirrorEnabled bool

	// AllowApplyMutation is the legacy wildcard gate. When true any
	// authenticated caller may invoke mutating RPCs (kept for dev; in
	// production prefer MutationAllowedCallers so a leaked cert is
	// contained).
	AllowApplyMutation bool

	// MutationAllowedCallers is the per-CN allowlist of identities
	// permitted to invoke schema-mutating RPCs (ApplyMigration,
	// BeginBackfillPlan). Empty means no per-CN exceptions — only
	// AllowApplyMutation grants permission. Independent of (and in
	// addition to) the req.Caller-matches-CN check.
	MutationAllowedCallers []string

	// OperatorAllowedCallers is the per-CN allowlist of identities
	// permitted to invoke operator-only mutating RPCs (RevokeCaller,
	// RollbackSchema, AdoptBaseline). Typically a single entry: the
	// console's cert CN. Empty means fall back to AllowApplyMutation
	// for backward compatibility.
	OperatorAllowedCallers []string

	// CallerFromContext extracts the authenticated cert CN from the
	// request context. The admin service uses it to enforce that
	// req.Caller matches the connecting CN on apply/backfill so a
	// caller can't impersonate another caller's identity in the
	// request body. When nil the check is skipped (insecure dev mode).
	CallerFromContext func(context.Context) string

	// BackfillEnabled gates the BeginBackfillPlan RPC. Default false so
	// a server running without the backfill worker can't accept plans
	// that would pile up unprocessed. Operator sets this to true after
	// canarying the feature.
	BackfillEnabled bool

	// LogRing is the in-process slog ring buffer the GetLogs RPC reads
	// from. nil disables the RPC (it returns an empty page). The ring
	// itself is populated by the slog handler installed in
	// cmd/server/main.go's buildLogger — see internal/obs/logring.go.
	LogRing *obs.LogRing
}

// New returns a Service backed by pool.
func New(pool *pgxpool.Pool, cfg Config) *Service {
	toSet := func(in []string) map[string]bool {
		out := make(map[string]bool, len(in))
		for _, cn := range in {
			if cn = strings.TrimSpace(cn); cn != "" {
				out[cn] = true
			}
		}
		return out
	}
	return &Service{
		pool:               pool,
		mirrorDir:          cfg.MirrorDir,
		mirrorEnabled:      cfg.MirrorEnabled,
		allowApplyMutation: cfg.AllowApplyMutation,
		mutationAllowed:    toSet(cfg.MutationAllowedCallers),
		operatorAllowed:    toSet(cfg.OperatorAllowedCallers),
		callerFromContext:  cfg.CallerFromContext,
		backfillEnabled:    cfg.BackfillEnabled,
		logRing:            cfg.LogRing,
	}
}

// canMutate reports whether the given cert CN is permitted to invoke
// schema-mutating RPCs. The wildcard (AllowApplyMutation) and the per-CN
// allowlist (MutationAllowedCallers) are unioned: either grants permission.
func (s *Service) canMutate(cn string) bool {
	if s.allowApplyMutation {
		return true
	}
	return s.mutationAllowed[cn]
}

// authorizeOperator enforces the operator-mutation gate for RPCs that
// administrate other callers' state (revoke, rollback, adopt). Unlike
// self-apply there is no req.Caller-matches-CN check — the operator
// (typically the console) acts ON BEHALF OF a human admin and req.Caller
// names the TARGET caller, not the actor. When the OperatorAllowedCallers
// set is empty we fall back to the legacy global wildcard so existing
// deployments keep working.
func (s *Service) authorizeOperator(ctx context.Context) error {
	var cn string
	if s.callerFromContext != nil {
		cn = s.callerFromContext(ctx)
	}
	if len(s.operatorAllowed) == 0 {
		if s.allowApplyMutation {
			return nil
		}
		return fmt.Errorf("admin: operator mutation is disabled on this server (set ATL_OPERATOR_ALLOWED_CALLERS=<console-cn> or ATL_ALLOW_APPLY_MUTATION=true)")
	}
	if !s.operatorAllowed[cn] {
		return fmt.Errorf("admin: caller %q is not an operator (set ATL_OPERATOR_ALLOWED_CALLERS to permit)", cn)
	}
	return nil
}

// authorizeSelfApply enforces both mutation gates for RPCs that mutate
// the calling caller's OWN schema (apply, backfill). Returns nil iff:
//
//   - the connecting cert CN is allowed to mutate (via wildcard, env-var
//     allowlist, OR caller_identities.can_mutate=true), AND
//   - req.Caller matches the connecting cert CN (so a CN allowed to
//     mutate can only mutate its own namespace, not someone else's).
//
// In insecure dev mode (no TLS, no CallerFromContext) the same-CN check
// is skipped but the mutation gate still applies.
func (s *Service) authorizeSelfApply(ctx context.Context, reqCaller string) error {
	var cn string
	if s.callerFromContext != nil {
		cn = s.callerFromContext(ctx)
	}
	// Skip the same-CN check when no cert identity is available (dev),
	// but always evaluate the mutation gate.
	if cn != "" && cn != "anonymous" && reqCaller != cn {
		return fmt.Errorf("admin: req.caller %q does not match authenticated identity %q", reqCaller, cn)
	}
	// Cheap static gates first; only fall through to a DB round-trip
	// when neither the global wildcard nor the env-var allowlist
	// grants. The DB gate lets operators grant mutation permission at
	// runtime without an env-var edit + atlantis restart; only a real
	// CN is worth a lookup.
	if s.canMutate(cn) {
		return nil
	}
	if cn != "" && cn != "anonymous" {
		_, canMutate, err := s.isRegisteredCaller(ctx, cn)
		if err != nil {
			return fmt.Errorf("admin: identity lookup failed: %w", err)
		}
		if canMutate {
			return nil
		}
	}
	return fmt.Errorf("admin: caller %q is not permitted to mutate schema (register via console with 'allow apply' on, add to ATL_MUTATION_ALLOWED_CALLERS, or set ATL_ALLOW_APPLY_MUTATION=true for dev)", cn)
}

// SubmittedFile is one .atl file submitted by a caller; Path is repo-relative.
type SubmittedFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// PlanRequest is the input to PlanSchema.
type PlanRequest struct {
	Caller string
	Files  []SubmittedFile
}

// PlanResponse is the result of PlanSchema. PlanID is stable across calls
// with the same (caller, file-set, base-checkpoint) tuple; ApplyMigration
// re-derives it to detect drift.
type PlanResponse struct {
	PlanID         string        `json:"plan_id"`
	Class          ClassName     `json:"class"`
	UpSQL          string        `json:"up_sql"`
	DownSQL        string        `json:"down_sql"`
	ImpactReport   []ImpactEntry `json:"impact_report"`
	ParseErrors    []string      `json:"parse_errors"`
	BreakingDetail []string      `json:"breaking_detail"`

	// CheckpointHash is the content hash of the IR checkpoint at plan time.
	// Sent back in ApplyRequest for CAS conflict detection.
	CheckpointHash string `json:"checkpoint_hash"`

	// CustomSQLErrors lists pg_query_go validation failures for query/procedure blocks.
	// Empty if all custom SQL validates.
	CustomSQLErrors []string `json:"custom_sql_errors,omitempty"`

	// CustomCount tallies custom queries and procedures in the new IR.
	CustomCount CustomDeclCount `json:"custom_count"`

	// Phase-split outputs for `tide apply --backfill`. Empty for non-
	// backfill plans. BackfillFields drives the chunked-UPDATE worker:
	// one entry per field, with the user expression + PK column already
	// resolved against the new IR.
	PreBackfillUpSQL       string             `json:"pre_backfill_up_sql,omitempty"`
	PreBackfillIndexesSQL  string             `json:"pre_backfill_indexes_sql,omitempty"`
	PostBackfillUpSQL      string             `json:"post_backfill_up_sql,omitempty"`
	PostBackfillIndexesSQL string             `json:"post_backfill_indexes_sql,omitempty"`
	BackfillFields         []BackfillFieldRef `json:"backfill_fields,omitempty"`

	// Extensions lists the Postgres extensions the new IR requires, with
	// one of three actions per extension: "ok" (already enabled),
	// "enable" (atlantis will CREATE EXTENSION inside the apply tx),
	// "missing" (operator must install at the OS level — apply refuses).
	// Empty when the schema needs no extensions.
	Extensions []extensionStatus `json:"extensions,omitempty"`
}

// CustomDeclCount tallies custom queries and procedures.
type CustomDeclCount struct {
	Queries    int `json:"queries"`
	Procedures int `json:"procedures"`
}

// ClassName is the wire-side enum mirroring codegen.ChangeClass.
type ClassName string

const (
	ClassAdditive ClassName = "additive"
	ClassBackfill ClassName = "backfill_required"
	ClassBreaking ClassName = "cross_caller_breaking"
	ClassUnclean  ClassName = "unparseable" // returned when DSL itself doesn't parse
)

// ImpactEntry describes how one caller is affected by a plan; includes the plan's own caller.
type ImpactEntry struct {
	Caller   string `json:"caller"`
	Affected bool   `json:"affected"`
	Detail   string `json:"detail,omitempty"`
}

// ApplyRequest is the input to ApplyMigration.
type ApplyRequest struct {
	Caller         string
	PlanID         string
	UpSQL          string // re-submitted by caller to detect drift since planning
	Files          []SubmittedFile
	CheckpointHash string // CAS token from PlanResponse; empty for pre-CAS clients
}

// ApplyResponse is returned on a successful apply.
type ApplyResponse struct {
	AppliedAt   string `json:"applied_at"`
	Version     int64  `json:"version"`
	ContentHash string `json:"content_hash"`
}

// GetMergedSchemaRequest asks for the union of every caller's registered files.
// SinceVersion is the last Version the client observed; the server omits Files
// when it matches the current version.
type GetMergedSchemaRequest struct {
	SinceVersion string
}

// GetMergedSchemaResponse carries the merged file set. Files is empty when
// SinceVersion equals Version.
type GetMergedSchemaResponse struct {
	Version string          `json:"version"`
	Files   []SubmittedFile `json:"files"`
}

// GetCallerFilesRequest identifies a single caller whose registered files
// should be returned.
type GetCallerFilesRequest struct {
	Caller string
}

// GetCallerFilesResponse carries the named caller's registered .atl files in
// file_path order. Empty if the caller has never applied.
type GetCallerFilesResponse struct {
	Files []SubmittedFile `json:"files"`
}

// GetCallerFiles returns all registered .atl files for a single caller,
// ordered by file_path. Read-only.
func (s *Service) GetCallerFiles(ctx context.Context, req GetCallerFilesRequest) (*GetCallerFilesResponse, error) {
	if req.Caller == "" {
		return nil, fmt.Errorf("caller is required")
	}
	rows, err := s.pool.Query(ctx, `
SELECT file_path, content
FROM atlantis.caller_registrations
WHERE caller = $1
ORDER BY file_path`, req.Caller)
	if err != nil {
		return nil, fmt.Errorf("load caller files: %w", err)
	}
	defer rows.Close()

	var files []SubmittedFile
	for rows.Next() {
		var path, content string
		if err := rows.Scan(&path, &content); err != nil {
			return nil, err
		}
		files = append(files, SubmittedFile{Path: path, Content: []byte(content)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &GetCallerFilesResponse{Files: files}, nil
}

// GetCanonicalIRRequest is the input to GetCanonicalIR. No fields today;
// the caller always wants the current checkpoint IR.
type GetCanonicalIRRequest struct{}

// GetCanonicalIRResponse carries the canonical IR exactly as stored in the
// checkpoint — proto field numbers included — so a caller generating a
// typed client produces wire-identical messages. ContentHash lets the
// caller pin the schema version it generated against.
type GetCanonicalIRResponse struct {
	IR          json.RawMessage `json:"ir"`
	ContentHash string          `json:"content_hash"`
}

// PlanSchema is the workhorse. The flow:
//
//  1. Validate the caller's submitted files parse.
//  2. Load every other caller's current files from caller_registrations.
//  3. Load the last applied IR checkpoint (or nil for an empty DB).
//  4. Lower the union of (caller's new files + everyone else's existing
//     files) into a new IR.
//  5. Diff new IR against the checkpoint.
//  6. Classify; emit SQL; produce impact report.
//
// We do NOT write to caller_registrations here — that happens only when
// ApplyMigration succeeds. PlanSchema is read-only.
func (s *Service) PlanSchema(ctx context.Context, req PlanRequest) (*PlanResponse, error) {
	if req.Caller == "" {
		return nil, errors.New("admin: caller identity is required")
	}

	// Pass 1: parse the caller's submitted files into one big File set so
	// we can detect DSL errors before merging with anything.
	callerFiles, parseErrs := parseSubmitted(req.Caller, req.Files)
	if len(parseErrs) > 0 {
		// Surface parse errors up front. The plan is "unclean" — no apply
		// is possible until the caller fixes its own DSL.
		return &PlanResponse{
			Class:       ClassUnclean,
			ParseErrors: parseErrs,
		}, nil
	}

	// Load every other caller's stored files.
	others, err := s.loadOtherCallers(ctx, req.Caller)
	if err != nil {
		return nil, fmt.Errorf("load other callers: %w", err)
	}

	// Load prior IR checkpoint; nil on first apply.
	prior, err := s.loadCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}

	// Lower the union; errors on duplicate entities or unresolved FKs.
	allFiles := append(callerFiles, others...)
	newIR, err := dsl.Lower(allFiles)
	if err != nil {
		return &PlanResponse{
			Class:       ClassUnclean,
			ParseErrors: []string{err.Error()},
		}, nil
	}

	// Assign proto numbers before diffing so both sides see stable IDs.
	codegen.AssignProtoNumbers(prior, newIR)

	// Build caller-ownership context so the diff engine can downgrade
	// removals that only affect the submitting caller.
	ownership := buildEntityOwnership(req.Caller, callerFiles, others)
	crossRefs := buildCrossCallerRefs(others)
	d := codegen.ComputeDiff(prior, newIR,
		codegen.WithCallerContext(req.Caller, ownership, crossRefs))

	// Validate every custom query/procedure with pg_query_go. Lowering catches
	// dep-free rules; this catches syntax and unresolved table refs.
	// Scoped to the submitting caller's content — stored content from other
	// callers is loaded into newIR for cross-caller table lookup but isn't
	// re-validated here. It was already validated when its owning caller
	// submitted it; re-validating under whatever rules are in force now
	// would block this caller's apply on drift in some unrelated caller.
	customSQLErrs := validateCustomSQL(newIR, req.Caller)

	// Emit SQL and build the impact report.
	var scripts codegen.SQLScripts
	if prior == nil {
		scripts, err = codegen.EmitInitial(newIR)
	} else {
		scripts, err = codegen.EmitSQL(prior, newIR, d)
	}
	if err != nil {
		return nil, fmt.Errorf("emit sql: %w", err)
	}

	// Surface the extension state so `tide plan` can warn before any
	// apply runs. Read-only: pg_available_extensions + pg_extension.
	// Errors here don't fail the plan — the extension check is best-
	// effort, and the apply path will hard-refuse if anything's missing.
	extStatuses, _ := inspectExtensions(ctx, s.pool, newIR)

	resp := &PlanResponse{
		PlanID:          computePlanID(req.Caller, callerFiles, prior),
		Class:           translateClass(d.HighestClass()),
		UpSQL:           scripts.Up,
		DownSQL:         scripts.Down,
		ImpactReport:    buildImpactReport(req.Caller, others, d, newIR),
		CheckpointHash:  s.loadCheckpointHash(ctx),
		CustomSQLErrors: customSQLErrs,
		CustomCount: CustomDeclCount{
			Queries:    len(newIR.Queries),
			Procedures: len(newIR.Procedures),
		},
		PreBackfillUpSQL:       scripts.PreBackfillUp,
		PreBackfillIndexesSQL:  scripts.PreBackfillIndexes,
		PostBackfillUpSQL:      scripts.PostBackfillUp,
		PostBackfillIndexesSQL: scripts.PostBackfillIndexes,
		BackfillFields:         translateBackfillFields(scripts.BackfillFields),
		Extensions:             extStatuses,
	}
	for _, ch := range d.Breaking {
		resp.BreakingDetail = append(resp.BreakingDetail,
			fmt.Sprintf("%s/%s: %s", ch.EntityID, ch.Field, ch.Detail))
	}
	// Custom-SQL failures mark the plan unparseable; nothing can apply until fixed.
	if len(customSQLErrs) > 0 {
		resp.Class = ClassUnclean
	}
	return resp, nil
}

// translateBackfillFields converts the codegen-side BackfillField slice
// to the admin-wire BackfillFieldRef slice so callers don't import
// internal/codegen.
func translateBackfillFields(in []codegen.BackfillField) []BackfillFieldRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]BackfillFieldRef, len(in))
	for i, f := range in {
		out[i] = BackfillFieldRef{
			EntityID:   f.EntityID,
			Field:      f.Field,
			Expression: f.Expression,
			PKColumn:   f.PKColumn,
			TableName:  f.TableName,
		}
	}
	return out
}

// validateCustomSQL runs pg_query_go validation over the submitting
// caller's custom queries and procedures, using the full IR's entity
// set so cross-caller table references still resolve.
//
// Scope is intentional: stored content from other callers was already
// validated when its owning caller submitted it. If we re-validated it
// here under whatever rules are in force at this moment, drift in some
// unrelated caller's stored content would block this caller's apply —
// for example, a caller that hasn't re-applied since a `table "..."`
// override was added on one of its entities would have stale references
// in its stored procedures, and every other caller would be unable to
// plan until that caller cleaned up. Validating only the submitting
// caller's content keeps each caller responsible for its own SQL while
// still letting the planner see the full schema for type lookup.
//
// caller is the SubmittingCaller; SourcePath on every IR decl is
// formatted as "<caller>:<file-path>" by parseSubmitted / loadOtherCallers,
// so a strings.HasPrefix on "<caller>:" is the right ownership test.
func validateCustomSQL(ir *dsl.IR, caller string) []string {
	prefix := caller + ":"
	var msgs []string
	for i := range ir.Queries {
		if !strings.HasPrefix(ir.Queries[i].SourcePath, prefix) {
			continue
		}
		if err := sqlvalidate.ValidateCustomQuery(ir, &ir.Queries[i]); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	for i := range ir.Procedures {
		if !strings.HasPrefix(ir.Procedures[i].SourcePath, prefix) {
			continue
		}
		if err := sqlvalidate.ValidateCustomProcedure(ir, &ir.Procedures[i]); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	return msgs
}

// ApplyMigration runs the planned SQL in a tx, upserts the caller's files,
// and writes a new IR checkpoint. Serialized by a cluster-wide advisory lock.
// A stale PlanID is rejected; any failure rolls back.
func (s *Service) ApplyMigration(ctx context.Context, req ApplyRequest) (*ApplyResponse, error) {
	if req.Caller == "" {
		return nil, errors.New("admin: caller identity is required")
	}
	// Enforce per-CN mutation allowlist + that req.Caller matches the
	// connecting cert CN. A leaked cert can therefore only push schema
	// for ITS OWN caller namespace, not anyone else's.
	if err := s.authorizeSelfApply(ctx, req.Caller); err != nil {
		return nil, err
	}
	if req.PlanID == "" {
		return nil, errors.New("admin: plan_id is required")
	}

	// Refuse to apply on top of an in-flight backfill — between Phase 1
	// and Phase 3 the schema is in a partially-applied state and a
	// concurrent unrelated apply can leave it corrupted.
	if planHash, inflight, err := hasInflightBackfill(ctx, s.pool, req.Caller); err == nil && inflight {
		return nil, fmt.Errorf("admin: backfill %s is in flight for this caller — wait for it to complete (or fail) before applying", planHash)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Advisory lock id is a stable 64-bit hash; same value across pods.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x70636661706c79)); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}

	if err := s.upsertCallerFiles(ctx, tx, req.Caller, req.Files); err != nil {
		return nil, err
	}

	// Re-plan from the persisted state to detect drift.
	parsed, parseErrs := parseSubmitted(req.Caller, req.Files)
	if len(parseErrs) > 0 {
		return nil, fmt.Errorf("admin: parse failed during apply: %v", parseErrs)
	}
	others, err := s.loadOtherCallersTx(ctx, tx, req.Caller)
	if err != nil {
		return nil, err
	}
	newIR, err := dsl.Lower(append(parsed, others...))
	if err != nil {
		return nil, fmt.Errorf("admin: lower failed during apply: %w", err)
	}
	prior, err := s.loadCheckpointTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	codegen.AssignProtoNumbers(prior, newIR)

	// Build caller-ownership context for the diff (same as PlanSchema).
	applyOwnership := buildEntityOwnership(req.Caller, parsed, others)
	applyCrossRefs := buildCrossCallerRefs(others)
	d := codegen.ComputeDiff(prior, newIR,
		codegen.WithCallerContext(req.Caller, applyOwnership, applyCrossRefs))

	// Re-validate inside the lock: another caller's apply between plan and apply
	// can change which tables are visible. Same caller-scoping rationale as
	// the PlanSchema call site above.
	if msgs := validateCustomSQL(newIR, req.Caller); len(msgs) > 0 {
		return nil, fmt.Errorf("admin: custom SQL validation failed: %v", msgs)
	}

	gotPlanID := computePlanID(req.Caller, parsed, prior)
	if gotPlanID != req.PlanID {
		return nil, fmt.Errorf("admin: plan %s is stale; current plan is %s — re-run tide apply",
			req.PlanID, gotPlanID)
	}
	if d.HighestClass() == codegen.ClassCrossCallerBreaking {
		return nil, fmt.Errorf("admin: plan is breaking and cannot be auto-applied")
	}

	var scripts codegen.SQLScripts
	if prior == nil {
		scripts, err = codegen.EmitInitial(newIR)
	} else {
		scripts, err = codegen.EmitSQL(prior, newIR, d)
	}
	if err != nil {
		return nil, fmt.Errorf("emit sql: %w", err)
	}

	// Auto-enable extensions required by the new IR but not yet enabled
	// in this database. Refuses with a structured error if any required
	// extension is missing at OS level. Runs INSIDE the apply tx so the
	// enable + DDL commit atomically — no half-applied state.
	if _, err := prepareExtensions(ctx, tx, newIR); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, scripts.Up); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}

	// Use client-provided hash when available (what they planned against);
	// fall back to reading it server-side inside the advisory-locked tx.
	expectedHash := req.CheckpointHash
	if expectedHash == "" {
		expectedHash, _ = loadCheckpointHashTx(ctx, tx)
	}
	meta := versionMeta{
		Caller:       req.Caller,
		PlanClass:    d.HighestClass().String(),
		Diff:         d,
		UpSQL:        scripts.Up,
		DownSQL:      scripts.Down,
		PlanID:       gotPlanID,
		EventType:    "apply",
		ExpectedHash: expectedHash,
	}
	version, err := s.persistCheckpoint(ctx, tx, newIR, meta)
	if err != nil {
		return nil, err
	}

	// Read the newly written content hash for the response.
	newHash, _ := loadCheckpointHashTx(ctx, tx)

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	if s.mirrorEnabled {
		if err := mirrorFiles(s.mirrorDir, req.Caller, req.Files); err != nil {
			fmt.Fprintf(os.Stderr, "admin: mirror after apply (caller=%s): %v\n", req.Caller, err)
		}
	}

	return &ApplyResponse{AppliedAt: nowUTC(), Version: version, ContentHash: newHash}, nil
}

// mirrorFiles writes each file atomically to <root>/<caller>/<path>.
// Identical content is skipped to avoid mtime churn.
func mirrorFiles(root, caller string, files []SubmittedFile) error {
	if root == "" {
		return errors.New("mirror dir is empty")
	}
	if caller == "" {
		return errors.New("caller is empty")
	}
	// Per-caller subdir prevents path collisions across callers.
	callerRoot := filepath.Join(root, caller)
	for _, f := range files {
		// Reject paths that escape the caller root; the wire input is untrusted.
		clean := filepath.Clean(f.Path)
		if clean == "." || strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") || filepath.IsAbs(clean) {
			return fmt.Errorf("invalid file path %q", f.Path)
		}
		dst := filepath.Join(callerRoot, clean)

		if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, f.Content) {
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		tmp, err := os.CreateTemp(filepath.Dir(dst), ".pc-mirror-*")
		if err != nil {
			return fmt.Errorf("temp file: %w", err)
		}
		tmpPath := tmp.Name()
		if _, err := tmp.Write(f.Content); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("write %s: %w", tmpPath, err)
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("fsync %s: %w", tmpPath, err)
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("close %s: %w", tmpPath, err)
		}
		if err := os.Rename(tmpPath, dst); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("rename %s -> %s: %w", tmpPath, dst, err)
		}
	}
	return nil
}

// GetMergedSchema returns the union of every caller's registered files.
// When req.SinceVersion equals the current version, Files is omitted.
func (s *Service) GetMergedSchema(ctx context.Context, req GetMergedSchemaRequest) (*GetMergedSchemaResponse, error) {
	rows, err := s.pool.Query(ctx, `
SELECT caller, file_path, content
FROM atlantis.caller_registrations
ORDER BY caller, file_path`)
	if err != nil {
		return nil, fmt.Errorf("load registrations: %w", err)
	}
	defer rows.Close()

	var raw []mergedEntry
	for rows.Next() {
		var e mergedEntry
		if err := rows.Scan(&e.caller, &e.path, &e.content); err != nil {
			return nil, err
		}
		raw = append(raw, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	version := computeMergedSchemaVersion(raw)
	resp := &GetMergedSchemaResponse{Version: version}
	if req.SinceVersion == version {
		// Client is up to date — return version only.
		return resp, nil
	}
	for _, e := range raw {
		resp.Files = append(resp.Files, SubmittedFile{
			Path:    e.path,
			Content: []byte(e.content),
		})
	}
	return resp, nil
}

// GetCanonicalIR returns the checkpoint IR verbatim — the raw bytes stored
// at apply time, with proto field numbers intact. Callers generate their
// typed client from this so wire encoding matches the server exactly;
// re-lowering the .atl files locally could assign different field numbers.
// Read-only. Returns an empty IR on a fresh database.
func (s *Service) GetCanonicalIR(ctx context.Context, _ GetCanonicalIRRequest) (*GetCanonicalIRResponse, error) {
	var raw []byte
	var contentHash string
	err := s.pool.QueryRow(ctx,
		`SELECT ir, content_hash FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&raw, &contentHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &GetCanonicalIRResponse{IR: json.RawMessage("null")}, nil
		}
		return nil, fmt.Errorf("load canonical IR: %w", err)
	}
	return &GetCanonicalIRResponse{IR: raw, ContentHash: contentHash}, nil
}

// mergedEntry is the in-memory shape of one caller_registrations row used
// only by GetMergedSchema. Kept private — the wire shape is SubmittedFile.
type mergedEntry struct {
	caller, path, content string
}

// computeMergedSchemaVersion hashes (caller, path, content) in their query
// order so any byte-level change to any registered file shifts the version.
// Truncated to 16 hex chars — still 64 bits of collision resistance, with
// a compact value clients can log without overwhelming the line.
func computeMergedSchemaVersion(entries []mergedEntry) string {
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.caller))
		h.Write([]byte{0})
		h.Write([]byte(e.path))
		h.Write([]byte{0})
		h.Write([]byte(e.content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// upsertCallerFiles replaces this caller's full submission set inside the tx.
// We DELETE then INSERT (vs ON CONFLICT) so files that the caller dropped
// from their submission are removed from storage too — otherwise a caller
// could leave orphan registrations behind.
func (s *Service) upsertCallerFiles(ctx context.Context, tx pgx.Tx, caller string, files []SubmittedFile) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM atlantis.caller_registrations WHERE caller = $1`, caller); err != nil {
		return fmt.Errorf("upsert: clear prior: %w", err)
	}
	for _, f := range files {
		h := sha256.Sum256(f.Content)
		_, err := tx.Exec(ctx, `
INSERT INTO atlantis.caller_registrations (caller, file_path, content, sha256)
VALUES ($1, $2, $3, $4)`,
			caller, f.Path, string(f.Content), hex.EncodeToString(h[:]))
		if err != nil {
			return fmt.Errorf("upsert: insert %s: %w", f.Path, err)
		}
	}
	return nil
}

func parseSubmitted(caller string, files []SubmittedFile) ([]*dsl.File, []string) {
	var out []*dsl.File
	var errs []string
	for _, f := range files {
		parsed, err := dsl.Parse(caller+":"+f.Path, f.Content)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		out = append(out, parsed)
	}
	return out, errs
}

func (s *Service) loadOtherCallers(ctx context.Context, exclude string) ([]*dsl.File, error) {
	rows, err := s.pool.Query(ctx, `
SELECT caller, file_path, content
FROM atlantis.caller_registrations
WHERE caller != $1
ORDER BY caller, file_path`, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dsl.File
	for rows.Next() {
		var caller, path, content string
		if err := rows.Scan(&caller, &path, &content); err != nil {
			return nil, err
		}
		f, err := dsl.Parse(caller+":"+path, []byte(content))
		if err != nil {
			// Another caller's submission is broken. We surface this as an
			// error rather than silently dropping the file — a broken
			// caller shouldn't permit this caller to plan around them.
			return nil, fmt.Errorf("caller %s: stored file %s no longer parses: %w", caller, path, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Service) loadOtherCallersTx(ctx context.Context, tx pgx.Tx, exclude string) ([]*dsl.File, error) {
	rows, err := tx.Query(ctx, `
SELECT caller, file_path, content
FROM atlantis.caller_registrations
WHERE caller != $1
ORDER BY caller, file_path`, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dsl.File
	for rows.Next() {
		var caller, path, content string
		if err := rows.Scan(&caller, &path, &content); err != nil {
			return nil, err
		}
		f, err := dsl.Parse(caller+":"+path, []byte(content))
		if err != nil {
			return nil, fmt.Errorf("caller %s: stored file %s no longer parses: %w", caller, path, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Service) loadCheckpoint(ctx context.Context) (*dsl.IR, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return dsl.DecodeJSONIR(raw)
}

func (s *Service) loadCheckpointTx(ctx context.Context, tx pgx.Tx) (*dsl.IR, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return dsl.DecodeJSONIR(raw)
}

func (s *Service) loadCheckpointHash(ctx context.Context) string {
	var hash string
	err := s.pool.QueryRow(ctx, `SELECT content_hash FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&hash)
	if err != nil {
		return ""
	}
	return hash
}

// versionMeta holds the metadata for one schema_versions row. Passed to
// persistCheckpoint so the caller can supply the diff, SQL, plan ID, and
// event type without persistCheckpoint needing to know how they were
// produced.
type versionMeta struct {
	Caller       string
	PlanClass    string
	Diff         *codegen.Diff
	UpSQL        string
	DownSQL      string
	PlanID       string
	EventType    string // "apply", "rollback", "adopt"
	ParentVer    *int64
	ExpectedHash string // CAS token — if set, reject when current checkpoint hash differs
}

func (s *Service) persistCheckpoint(ctx context.Context, tx pgx.Tx, ir *dsl.IR, meta versionMeta) (int64, error) {
	raw, err := ir.EncodeJSON()
	if err != nil {
		return 0, err
	}

	h := sha256.Sum256(raw)
	irHash := hex.EncodeToString(h[:])

	// CAS: reject if the checkpoint has moved since the caller planned.
	if meta.ExpectedHash != "" {
		got, err := loadCheckpointHashTx(ctx, tx)
		if err != nil {
			return 0, fmt.Errorf("cas: %w", err)
		}
		if got != "" && got != meta.ExpectedHash {
			return 0, fmt.Errorf("admin: checkpoint has moved (expected %s, got %s) — re-plan and retry",
				meta.ExpectedHash[:min(12, len(meta.ExpectedHash))],
				got[:min(12, len(got))])
		}
	}

	_, err = tx.Exec(ctx, `
INSERT INTO atlantis.ir_checkpoint (id, ir, applied_by, content_hash) VALUES (1, $1, $2, $3)
ON CONFLICT (id) DO UPDATE SET ir = EXCLUDED.ir, applied_at = now(), applied_by = EXCLUDED.applied_by, content_hash = EXCLUDED.content_hash`,
		raw, meta.Caller, irHash)
	if err != nil {
		return 0, fmt.Errorf("upsert ir_checkpoint: %w", err)
	}

	var diffJSON []byte
	if meta.Diff != nil {
		diffJSON, err = json.Marshal(meta.Diff)
		if err != nil {
			return 0, fmt.Errorf("marshal diff: %w", err)
		}
	} else {
		diffJSON = []byte(`{"additive":[],"backfill_required":[],"breaking":[]}`)
	}

	var version int64
	err = tx.QueryRow(ctx, `
INSERT INTO atlantis.schema_versions
    (caller, plan_class, diff, up_sql, down_sql, ir_snapshot, ir_hash, plan_id, parent_version, event_type)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING version`,
		meta.Caller, meta.PlanClass, diffJSON, meta.UpSQL, meta.DownSQL,
		raw, irHash, meta.PlanID, meta.ParentVer, meta.EventType,
	).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("insert schema_versions: %w", err)
	}

	if err := updateEntityLineage(ctx, tx, version, meta.Caller, meta.Diff); err != nil {
		return 0, fmt.Errorf("update entity lineage: %w", err)
	}

	return version, nil
}

// loadCheckpointHashTx reads the current content hash from ir_checkpoint
// within an existing transaction. Returns "" if no checkpoint exists.
func loadCheckpointHashTx(ctx context.Context, tx pgx.Tx) (string, error) {
	var hash string
	err := tx.QueryRow(ctx, `SELECT content_hash FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return hash, nil
}

// computePlanID hashes (caller, files, prior checkpoint hash) so applies can
// detect drift since planning. Stable across reruns of the same plan.
func computePlanID(caller string, files []*dsl.File, prior *dsl.IR) string {
	h := sha256.New()
	h.Write([]byte(caller))
	h.Write([]byte{0})
	// Sort files by path for determinism.
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	sort.Strings(paths)
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	if prior != nil {
		b, _ := prior.EncodeJSON()
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func translateClass(c codegen.ChangeClass) ClassName {
	switch c {
	case codegen.ClassAdditive:
		return ClassAdditive
	case codegen.ClassBackfillRequired:
		return ClassBackfill
	case codegen.ClassCrossCallerBreaking:
		return ClassBreaking
	}
	return ClassUnclean
}

// buildImpactReport summarizes how each known caller is affected.
//
// "Affected" = at least one Change in the diff names an entity this caller
// has files for. The plan's own caller is included (with their own caller-
// scoped detail) so the CLI can render a single unified table.
func buildImpactReport(planCaller string, others []*dsl.File, d *codegen.Diff, _ *dsl.IR) []ImpactEntry {
	// Bucket every entity ID touched in the diff.
	touched := map[string]bool{}
	count := 0
	for _, ch := range d.Additive {
		touched[ch.EntityID] = true
		count++
	}
	for _, ch := range d.BackfillRequired {
		touched[ch.EntityID] = true
		count++
	}
	for _, ch := range d.Breaking {
		touched[ch.EntityID] = true
		count++
	}

	// Group other callers by name and tally how many of their entities are touched.
	otherByCaller := map[string][]string{}
	for _, f := range others {
		// Path is "caller:filename"; split on the first colon.
		c := f.Path
		if i := indexOf(c, ':'); i >= 0 {
			c = c[:i]
		}
		otherByCaller[c] = append(otherByCaller[c], f.Path)
	}

	// Build a set of entity IDs declared in each other caller's files so
	// we can determine whether a caller is actually affected by the diff.
	callerEntities := map[string]map[string]bool{} // caller → set of entityIDs
	for _, f := range others {
		c := f.Path
		if i := indexOf(c, ':'); i >= 0 {
			c = c[:i]
		}
		if callerEntities[c] == nil {
			callerEntities[c] = map[string]bool{}
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *dsl.EntityDecl:
				callerEntities[c][d.Namespace+"."+d.Name] = true
			case *dsl.HypertableDecl:
				callerEntities[c][d.Namespace+"."+d.Name] = true
			}
		}
	}

	var report []ImpactEntry
	for caller := range otherByCaller {
		affected := false
		if ents, ok := callerEntities[caller]; ok {
			for entID := range touched {
				if entID != "" && ents[entID] {
					affected = true
					break
				}
			}
		}
		detail := fmt.Sprintf("%d change(s) touched the union", count)
		if affected {
			detail = fmt.Sprintf("%d change(s) touch entities this caller declares", count)
		}
		report = append(report, ImpactEntry{
			Caller:   caller,
			Affected: affected,
			Detail:   detail,
		})
	}
	report = append(report, ImpactEntry{
		Caller:   planCaller,
		Affected: true,
		Detail:   fmt.Sprintf("%d change(s) in this plan", count),
	})
	sort.Slice(report, func(i, j int) bool { return report[i].Caller < report[j].Caller })
	return report
}

// indexOf returns the first index of c in s, or -1.
func indexOf(s string, c byte) int {
	for i := range len(s) {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// buildEntityOwnership returns a map of entityID → caller for every entity
// declared across the submitting caller's files and all other callers' files.
// The caller name is extracted from the file path prefix ("caller:path").
func buildEntityOwnership(callerName string, callerFiles []*dsl.File, otherFiles []*dsl.File) map[string]string {
	out := map[string]string{}

	// Helper: walk one file's decls and attribute entities to the given caller.
	register := func(caller string, f *dsl.File) {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *dsl.EntityDecl:
				out[d.Namespace+"."+d.Name] = caller
			case *dsl.HypertableDecl:
				out[d.Namespace+"."+d.Name] = caller
			}
		}
	}

	for _, f := range callerFiles {
		register(callerName, f)
	}
	for _, f := range otherFiles {
		c := f.Path
		if i := indexOf(c, ':'); i >= 0 {
			c = c[:i]
		}
		register(c, f)
	}
	return out
}

// buildCrossCallerRefs scans other callers' parsed files and returns a set
// of entity IDs and "entityID.fieldName" strings that are referenced by
// callers other than the submitter. A reference is any `references
// ns.Entity.field` modifier on a FieldDecl in another caller's file.
//
// This is a conservative scan over the AST — it does not need the lowered IR.
func buildCrossCallerRefs(otherFiles []*dsl.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range otherFiles {
		for _, decl := range f.Decls {
			var members []dsl.EntityMember
			switch d := decl.(type) {
			case *dsl.EntityDecl:
				members = d.Members
			case *dsl.HypertableDecl:
				members = d.Members
			default:
				// QueryDecl / ProcedureDecl: scan touches() for entity refs.
				scanDeclTouches(decl, out)
				continue
			}
			for _, m := range members {
				fd, ok := m.(*dsl.FieldDecl)
				if !ok {
					continue
				}
				for _, mod := range fd.Modifiers {
					ref, ok := mod.(*dsl.ModReferencesDecl)
					if !ok {
						continue
					}
					targetID := ref.TargetNS + "." + ref.TargetEntity
					out[targetID] = true
					out[targetID+"."+ref.TargetField] = true
				}
			}
		}
	}
	return out
}

// scanDeclTouches extracts entity references from touches() clauses on
// QueryDecl and ProcedureDecl and adds them to the refs set. This ensures
// that entities mentioned in another caller's custom queries/procedures are
// treated as cross-referenced.
func scanDeclTouches(decl dsl.Decl, refs map[string]bool) {
	switch d := decl.(type) {
	case *dsl.QueryDecl:
		if d.SQL != nil {
			for _, t := range d.SQL.Touches {
				if t.Namespace != "" {
					refs[t.Namespace+"."+t.Name] = true
				}
			}
		}
	case *dsl.ProcedureDecl:
		for _, step := range d.Steps {
			if step.Raw != nil {
				for _, t := range step.Raw.Touches {
					if t.Namespace != "" {
						refs[t.Namespace+"."+t.Name] = true
					}
				}
			}
			if step.Typed != nil && step.Typed.Target.Namespace != "" {
				refs[step.Typed.Target.Namespace+"."+step.Typed.Target.Name] = true
			}
		}
	}
}

// nowUTC returns the current UTC time formatted as RFC3339. Broken out so
// tests can swap it via a build-tag override.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}
