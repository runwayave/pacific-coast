package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/backfill"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// BeginBackfillPlanRequest is the input for kicking off a phase-split
// apply. The caller has already run PlanSchema and received the
// PreBackfill / PostBackfill scripts + the BackfillFields list; the
// request just re-submits those (and the original ApplyMigration shape)
// so the server can validate drift + atomically write the plan/field-
// state rows.
type BeginBackfillPlanRequest struct {
	Caller string
	PlanID string
	Files  []SubmittedFile

	PreBackfillUpSQL       string
	PreBackfillIndexesSQL  string
	PostBackfillUpSQL      string
	PostBackfillIndexesSQL string

	BackfillFields []BackfillFieldRef
}

// BackfillFieldRef wire-mirrors codegen.BackfillField so the CLI doesn't
// need to import internal/codegen.
type BackfillFieldRef struct {
	EntityID   string
	Field      string
	Expression string
	PKColumn   string
	TableName  string
}

// BeginBackfillPlanResponse is what the CLI gets back.
type BeginBackfillPlanResponse struct {
	PlanHash        string
	Accepted        bool
	AlreadyRunning  bool
	AlreadyComplete bool
	Message         string
}

// GetBackfillStatusRequest queries one plan's state. PlanHash overrides
// LatestForCaller if both are set.
type GetBackfillStatusRequest struct {
	PlanHash        string
	LatestForCaller string
}

// GetBackfillStatusResponse describes a plan's progress.
type GetBackfillStatusResponse struct {
	PlanHash    string
	Caller      string
	Status      string
	ErrorMsg    string
	StartedAt   string
	CompletedAt string
	Fields      []BackfillFieldStatus
}

// BackfillFieldStatus is per-field progress for the status RPC.
type BackfillFieldStatus struct {
	EntityID      string
	Field         string
	Status        string
	RowsProcessed int64
	LastPK        string
	ErrorMsg      string
}

// BeginBackfillPlan validates the plan against the persisted state and
// (atomically) records a backfill_plan row + one backfill_field_state
// row per declared field, then runs the Pre-backfill SQL inside the
// same advisory-locked tx. The partial-index CREATE CONCURRENTLY runs
// outside the tx since CONCURRENTLY is incompatible with one.
//
// Idempotency: re-submitting an in-flight plan returns AlreadyRunning
// rather than inserting duplicates; resubmitting a completed plan
// returns AlreadyComplete.
func (s *Service) BeginBackfillPlan(ctx context.Context, req BeginBackfillPlanRequest) (*BeginBackfillPlanResponse, error) {
	if req.Caller == "" {
		return nil, errors.New("admin: caller identity is required")
	}
	// Mutation gate + same-CN binding (see ApplyMigration for details).
	if err := s.authorizeSelfApply(ctx, req.Caller); err != nil {
		return nil, err
	}
	if !s.backfillEnabled {
		return nil, errors.New("admin: backfill worker is disabled on this server (set ATL_BACKFILL_WORKER_ENABLED=true to enable)")
	}
	if req.PlanID == "" {
		return nil, errors.New("admin: plan_id is required")
	}
	if len(req.BackfillFields) == 0 {
		return nil, errors.New("admin: backfill request has no fields — was this plan really backfill-required?")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x70636661706c79)); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}

	// Idempotency: short-circuit if this plan_hash already exists.
	existing, err := loadBackfillPlanStatus(ctx, tx, req.PlanID)
	if err != nil {
		return nil, fmt.Errorf("load existing plan: %w", err)
	}
	switch existing {
	case "phase2_running", "phase3_running":
		return &BeginBackfillPlanResponse{
			PlanHash:       req.PlanID,
			Accepted:       false,
			AlreadyRunning: true,
			Message:        "plan is already in flight; monitor with `tide backfill status`",
		}, nil
	case "complete":
		return &BeginBackfillPlanResponse{
			PlanHash:        req.PlanID,
			Accepted:        false,
			AlreadyComplete: true,
			Message:         "plan already completed",
		}, nil
	case "failed":
		return nil, fmt.Errorf("admin: plan %s previously failed; manual triage required", req.PlanID)
	}

	if err := s.upsertCallerFiles(ctx, tx, req.Caller, req.Files); err != nil {
		return nil, err
	}

	parsed, parseErrs := parseSubmitted(req.Caller, req.Files)
	if len(parseErrs) > 0 {
		return nil, fmt.Errorf("admin: parse failed during begin-backfill: %v", parseErrs)
	}
	others, err := s.loadOtherCallersTx(ctx, tx, req.Caller)
	if err != nil {
		return nil, err
	}
	newIR, err := dsl.Lower(append(parsed, others...))
	if err != nil {
		return nil, fmt.Errorf("admin: lower failed during begin-backfill: %w", err)
	}
	prior, err := s.loadCheckpointTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	codegen.AssignProtoNumbers(prior, newIR)

	gotPlanID := computePlanID(req.Caller, parsed, prior)
	if gotPlanID != req.PlanID {
		return nil, fmt.Errorf("admin: plan %s is stale; current plan is %s — re-run tide apply",
			req.PlanID, gotPlanID)
	}

	// Capture ir_checkpoint hash NOW so Phase 3 can detect drift.
	irHash, err := backfill.IRCheckpointHash(ctx, tx, "atlantis")
	if err != nil {
		// Empty checkpoint (initial apply) — fall back to empty hash; the
		// Phase 3 validator compares to whatever is captured here.
		irHash = ""
	}

	// Insert backfill_plan + every backfill_field_state row inside the
	// same tx. All-or-nothing — a crash here rolls back both, so a stuck
	// "phase2_running with no fields" state can't happen.
	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.backfill_plan
    (plan_hash, caller, status, post_sql, post_indexes_sql, ir_checkpoint_hash)
VALUES ($1, $2, 'phase2_running', $3, $4, $5)`,
		req.PlanID, req.Caller, req.PostBackfillUpSQL, req.PostBackfillIndexesSQL, irHash); err != nil {
		return nil, fmt.Errorf("insert backfill_plan: %w", err)
	}

	for _, f := range req.BackfillFields {
		if f.PKColumn == "" {
			return nil, fmt.Errorf("admin: backfill field %s.%s has no PK column (composite PKs are not supported by the backfill worker — split the field into a non-composite-PK entity or backfill manually)",
				f.EntityID, f.Field)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.backfill_field_state
    (plan_hash, entity_id, field, expression, pk_column, table_name, status)
VALUES ($1, $2, $3, $4, $5, $6, 'pending')`,
			req.PlanID, f.EntityID, f.Field, f.Expression, f.PKColumn, f.TableName); err != nil {
			return nil, fmt.Errorf("insert backfill_field_state %s.%s: %w", f.EntityID, f.Field, err)
		}
	}

	// Run the additive part of the migration (ADD COLUMN nullable, etc).
	// SET NOT NULL on backfilled fields is deferred to PostBackfillUpSQL,
	// which the worker runs after the chunked backfill completes.
	if req.PreBackfillUpSQL != "" {
		if _, err := tx.Exec(ctx, req.PreBackfillUpSQL); err != nil {
			return nil, fmt.Errorf("pre-backfill apply: %w", err)
		}
	}

	backfillDiff := codegen.ComputeDiff(prior, newIR)
	bfHash, _ := loadCheckpointHashTx(ctx, tx)
	_, err = s.persistCheckpoint(ctx, tx, newIR, versionMeta{
		Caller:       req.Caller,
		PlanClass:    backfillDiff.HighestClass().String(),
		Diff:         backfillDiff,
		UpSQL:        req.PreBackfillUpSQL,
		PlanID:       req.PlanID,
		EventType:    "apply",
		ExpectedHash: bfHash,
	})
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// CREATE INDEX CONCURRENTLY must run outside a transaction. Each
	// line is a separate statement; failures here are non-fatal in the
	// sense that they don't roll back Phase 1 (already committed), but
	// they fail the backfill plan so the operator can triage.
	if req.PreBackfillIndexesSQL != "" {
		for _, stmt := range splitSQLStatements(req.PreBackfillIndexesSQL) {
			if _, err := s.pool.Exec(ctx, stmt); err != nil {
				// Mark plan failed.
				_, _ = s.pool.Exec(ctx, `
UPDATE atlantis.backfill_plan
SET status='failed', error_msg=$1, completed_at=now()
WHERE plan_hash=$2`, "pre-backfill index create: "+err.Error(), req.PlanID)
				return nil, fmt.Errorf("pre-backfill index create (%q): %w", stmt, err)
			}
		}
	}

	return &BeginBackfillPlanResponse{
		PlanHash: req.PlanID,
		Accepted: true,
		Message:  fmt.Sprintf("backfill accepted for %d fields", len(req.BackfillFields)),
	}, nil
}

// GetBackfillStatus returns one plan's state. PlanHash takes precedence
// over LatestForCaller when both are set.
func (s *Service) GetBackfillStatus(ctx context.Context, req GetBackfillStatusRequest) (*GetBackfillStatusResponse, error) {
	if req.PlanHash == "" && req.LatestForCaller == "" {
		return nil, errors.New("admin: backfill status requires plan_hash or latest_for_caller")
	}

	var planRow struct {
		PlanHash    string
		Caller      string
		Status      string
		ErrorMsg    *string
		StartedAt   time.Time
		CompletedAt *time.Time
	}

	if req.PlanHash != "" {
		err := s.pool.QueryRow(ctx, `
SELECT plan_hash, caller, status, error_msg, started_at, completed_at
FROM atlantis.backfill_plan
WHERE plan_hash = $1`, req.PlanHash).Scan(
			&planRow.PlanHash, &planRow.Caller, &planRow.Status,
			&planRow.ErrorMsg, &planRow.StartedAt, &planRow.CompletedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("admin: backfill plan %s not found", req.PlanHash)
		}
		if err != nil {
			return nil, err
		}
	} else {
		err := s.pool.QueryRow(ctx, `
SELECT plan_hash, caller, status, error_msg, started_at, completed_at
FROM atlantis.backfill_plan
WHERE caller = $1
ORDER BY started_at DESC
LIMIT 1`, req.LatestForCaller).Scan(
			&planRow.PlanHash, &planRow.Caller, &planRow.Status,
			&planRow.ErrorMsg, &planRow.StartedAt, &planRow.CompletedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("admin: no backfill plans found for caller %s", req.LatestForCaller)
		}
		if err != nil {
			return nil, err
		}
	}

	resp := &GetBackfillStatusResponse{
		PlanHash:  planRow.PlanHash,
		Caller:    planRow.Caller,
		Status:    planRow.Status,
		StartedAt: planRow.StartedAt.UTC().Format(time.RFC3339),
	}
	if planRow.ErrorMsg != nil {
		resp.ErrorMsg = *planRow.ErrorMsg
	}
	if planRow.CompletedAt != nil {
		resp.CompletedAt = planRow.CompletedAt.UTC().Format(time.RFC3339)
	}

	rows, err := s.pool.Query(ctx, `
SELECT entity_id, field, status, rows_processed, last_pk, error_msg
FROM atlantis.backfill_field_state
WHERE plan_hash = $1
ORDER BY entity_id, field`, planRow.PlanHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f BackfillFieldStatus
		var lastPK, errMsg *string
		if err := rows.Scan(&f.EntityID, &f.Field, &f.Status, &f.RowsProcessed, &lastPK, &errMsg); err != nil {
			return nil, err
		}
		if lastPK != nil {
			f.LastPK = *lastPK
		}
		if errMsg != nil {
			f.ErrorMsg = *errMsg
		}
		resp.Fields = append(resp.Fields, f)
	}
	return resp, rows.Err()
}

// loadBackfillPlanStatus returns the plan's current status, or "" if it
// doesn't exist.
func loadBackfillPlanStatus(ctx context.Context, tx pgx.Tx, planHash string) (string, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM atlantis.backfill_plan WHERE plan_hash = $1`, planHash).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return status, err
}

// hasInflightBackfill reports whether the caller has any backfill plan
// in phase2_running or phase3_running. ApplyMigration calls this before
// applying so a concurrent unrelated apply can't corrupt the schema by
// landing between Phase 1 and Phase 3.
func hasInflightBackfill(ctx context.Context, pool *pgxpool.Pool, caller string) (string, bool, error) {
	var planHash string
	err := pool.QueryRow(ctx, `
SELECT plan_hash FROM atlantis.backfill_plan
WHERE caller = $1 AND status IN ('phase2_running','phase3_running')
LIMIT 1`, caller).Scan(&planHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return planHash, true, nil
}

// splitSQLStatements breaks a multi-line script into individual
// statements. Each non-blank, non-comment line is treated as one
// statement (CREATE INDEX CONCURRENTLY is a single-line statement;
// multi-line statements would need a real splitter, which we don't need
// until a use case demands it).
func splitSQLStatements(script string) []string {
	var out []string
	for _, line := range splitLines(script) {
		line = trimSpace(line)
		if line == "" || hasPrefix(line, "--") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// Small string helpers — inlined to avoid the extra import; the codebase
// already pulls in strings elsewhere but the admin/backfill.go file
// stays trim by keeping these local.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
