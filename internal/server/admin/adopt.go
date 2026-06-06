package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/introspect"
)

// AdoptBaselineRequest baselines an already-populated database. The
// server introspects the live schema, diffs it against the declared IR
// the operator submits, and — if the two match — records the IR as the
// authoritative checkpoint without running any DDL.
//
// Submissions is the multi-caller payload `tidectl adopt` sends: one
// entry per caller in atlantis.workspace.yaml, all baselined atomically
// in the same transaction. This is the canonical shape; the single
// Caller+Files pair is preserved as a thin shortcut for tooling that
// only knows about one caller at a time.
//
// AllowDrift bypasses the safety check: if true, the server baselines
// the declared IR even when introspection sees disagreement, and
// records the drift report into atlantis.adopt_history so future
// operators can audit what was lied about.
//
// AdoptedBy is the principal recorded into adopt_history. Filled by
// the CLI from $USER or an explicit --principal flag.
type AdoptBaselineRequest struct {
	Submissions []CallerSubmission `json:",omitempty"`

	// Single-caller shortcut. Server treats a non-empty Caller as a
	// one-element Submissions slice. Ignored when Submissions is set.
	Caller string          `json:",omitempty"`
	Files  []SubmittedFile `json:",omitempty"`

	AllowDrift bool
	AdoptedBy  string
}

// CallerSubmission is one row in a multi-caller adopt payload.
type CallerSubmission struct {
	Caller string
	Files  []SubmittedFile
}

// AdoptDriftItem is one row in the drift report. Mirrors codegen.Change
// across the wire so the CLI can render without importing internal/codegen.
//
// Severity splits the report into three Terraform-style buckets:
//   - "addition": declared by .atl but missing in live DB. Outstanding
//     work for `tide apply`, not a bug. Doesn't block baselining.
//   - "removal":  present in live DB but undeclared. Either an old prod
//     artifact the .atl never picked up, or a real omission. Doesn't
//     block baselining; surfacing it is the value.
//   - "mismatch": both sides have it but they disagree (type changed,
//     NOT NULL flipped, default rewritten). This is real drift and
//     blocks baseline unless --allow-drift.
type AdoptDriftItem struct {
	EntityID string `json:"entity_id"`
	Field    string `json:"field,omitempty"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"` // "addition" | "removal" | "mismatch"
	Detail   string `json:"detail,omitempty"`
}

// AdoptBaselineResponse is returned by AdoptBaseline. CheckpointWritten
// signals whether the IR checkpoint was actually inserted (false when
// drift was detected and --allow-drift was not set, or when a prior
// identical adopt completed successfully).
//
// Drift always contains the report regardless of whether the checkpoint
// was written — so the CLI can echo it back even on a clean adopt
// (Drift will be empty in that case).
type AdoptBaselineResponse struct {
	CheckpointWritten bool
	AlreadyAdopted    bool // true when a previous adopt with the same file hash succeeded
	Drift             []AdoptDriftItem
	Warnings          []string
}

// AdoptBaseline implements the "verify, then baseline" flow. See
// docs/guides/adopt-an-existing-database.md for the user-facing model.
//
// Multi-caller atomicity: every caller in req.Submissions is parsed
// and lowered into one union IR; introspection diffs that whole union
// in a single transaction. Either every caller baselines or none do.
// FK refs that cross caller namespaces resolve naturally because the
// IR lowering sees the full set.
func (s *Service) AdoptBaseline(ctx context.Context, req AdoptBaselineRequest) (*AdoptBaselineResponse, error) {
	if err := s.authorizeOperator(ctx); err != nil {
		return nil, err
	}
	subs := req.Submissions
	if len(subs) == 0 {
		if req.Caller == "" || len(req.Files) == 0 {
			return nil, errors.New("admin: at least one CallerSubmission is required")
		}
		subs = []CallerSubmission{{Caller: req.Caller, Files: req.Files}}
	}
	for i, s := range subs {
		if s.Caller == "" {
			return nil, fmt.Errorf("admin: submission[%d]: caller is required", i)
		}
		if len(s.Files) == 0 {
			return nil, fmt.Errorf("admin: submission[%d] %s: at least one .atl file is required", i, s.Caller)
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Same advisory lock id as ApplyMigration so adopt + apply can't
	// race.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x70636661706c79)); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}

	// Idempotency: every caller's last adopt hash must match the new
	// hash, OR we proceed. Mixed match/no-match → proceed (operator is
	// re-doing one caller and re-confirming the rest).
	hashesNow := make(map[string]string, len(subs))
	allMatch := true
	for _, sub := range subs {
		h := hashFiles(sub.Files)
		hashesNow[sub.Caller] = h
		prior, ok, err := lastAdoptHash(ctx, tx, sub.Caller)
		if err != nil {
			return nil, fmt.Errorf("read adopt history for %s: %w", sub.Caller, err)
		}
		if !ok || prior != h {
			allMatch = false
		}
	}
	if allMatch {
		return &AdoptBaselineResponse{AlreadyAdopted: true, CheckpointWritten: true}, nil
	}

	// Parse + lower the union of every submission. Other callers
	// already registered (from a prior partial adopt or earlier
	// applies) are also pulled so cross-caller FKs resolve.
	submitterNames := make(map[string]bool, len(subs))
	var parsed []*dsl.File
	for _, sub := range subs {
		submitterNames[sub.Caller] = true
		ps, errs := parseSubmitted(sub.Caller, sub.Files)
		if len(errs) > 0 {
			return nil, fmt.Errorf("admin: parse failed for %s: %v", sub.Caller, errs)
		}
		parsed = append(parsed, ps...)
	}
	others, err := s.loadOtherCallersExcluding(ctx, tx, submitterNames)
	if err != nil {
		return nil, err
	}
	declaredIR, err := dsl.Lower(append(parsed, others...))
	if err != nil {
		return nil, fmt.Errorf("admin: lower failed: %w", err)
	}

	introspectedIR, existingIDs, warnings, err := introspect.FromPostgres(ctx, tx, declaredIR)
	if err != nil {
		return nil, fmt.Errorf("introspect: %w", err)
	}
	codegen.AssignProtoNumbers(introspectedIR, declaredIR)
	d := codegen.ComputeDiff(introspectedIR, declaredIR)
	drift := translateDrift(d)

	// Only mismatches block baselining. Additions / removals are
	// outstanding-work indicators; surfacing them is the value, blocking
	// on them is the bug.
	mismatchCount := 0
	for _, di := range drift {
		if di.Severity == "mismatch" {
			mismatchCount++
		}
	}
	if mismatchCount > 0 && !req.AllowDrift {
		return &AdoptBaselineResponse{
			CheckpointWritten: false,
			Drift:             drift,
			Warnings:          warnings,
		}, nil
	}

	// Write each caller's files + one adopt_history row per caller +
	// one rewrite of the (single-row) IR checkpoint.
	for _, sub := range subs {
		if err := s.upsertCallerFiles(ctx, tx, sub.Caller, sub.Files); err != nil {
			return nil, err
		}
		if err := insertAdoptHistory(ctx, tx, sub.Caller, hashesNow[sub.Caller], drift, req.AllowDrift, req.AdoptedBy); err != nil {
			return nil, fmt.Errorf("insert adopt history for %s: %w", sub.Caller, err)
		}
	}
	// The checkpoint records only entities that physically exist in the
	// live DB. Entities declared in .atl but not yet applied are
	// excluded so a subsequent `tide apply` sees them as additive
	// changes and emits the CREATE TABLE statements. Without this
	// filtering, adopt would baseline phantom entities (RPCs reachable
	// but failing at runtime against missing tables).
	baseline := filterToExistingEntities(declaredIR, existingIDs)
	adoptHash, _ := loadCheckpointHashTx(ctx, tx)
	_, err = s.persistCheckpoint(ctx, tx, baseline, versionMeta{
		Caller:       "adopt",
		PlanClass:    "adopt",
		Diff:         d,
		EventType:    "adopt",
		ExpectedHash: adoptHash,
	})
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return &AdoptBaselineResponse{
		CheckpointWritten: true,
		Drift:             drift,
		Warnings:          warnings,
	}, nil
}

// loadOtherCallersExcluding loads files for every caller NOT in the
// supplied set. Mirror of loadOtherCallersTx but generalized for the
// multi-caller adopt case.
func (s *Service) loadOtherCallersExcluding(ctx context.Context, tx pgx.Tx, exclude map[string]bool) ([]*dsl.File, error) {
	rows, err := tx.Query(ctx, `
SELECT caller, file_path, content
FROM atlantis.caller_registrations
ORDER BY caller, file_path`)
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
		if exclude[caller] {
			continue
		}
		f, err := dsl.Parse(caller+":"+path, []byte(content))
		if err != nil {
			return nil, fmt.Errorf("caller %s: stored file %s no longer parses: %w", caller, path, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// hashFiles returns a stable sha256 over (path, content) tuples for
// dedup-and-idempotency checks. The order is forced so two semantically
// equivalent submissions hash the same.
func hashFiles(files []SubmittedFile) string {
	type fh struct{ Path, ContentHash string }
	tmp := make([]fh, len(files))
	for i, f := range files {
		h := sha256.Sum256(f.Content)
		tmp[i] = fh{Path: f.Path, ContentHash: hex.EncodeToString(h[:])}
	}
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].Path < tmp[j].Path })
	h := sha256.New()
	for _, t := range tmp {
		h.Write([]byte(t.Path))
		h.Write([]byte{0})
		h.Write([]byte(t.ContentHash))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// lastAdoptHash returns the declared-IR hash from the most recent
// adopt for the caller. (false, nil) when no prior adopt exists.
func lastAdoptHash(ctx context.Context, tx pgx.Tx, caller string) (string, bool, error) {
	var h string
	err := tx.QueryRow(ctx, `
SELECT declared_hash
FROM atlantis.adopt_history
WHERE caller = $1
ORDER BY adopted_at DESC
LIMIT 1`, caller).Scan(&h)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return h, true, nil
}

func insertAdoptHistory(ctx context.Context, tx pgx.Tx, caller, hash string, drift []AdoptDriftItem, allowDrift bool, adoptedBy string) error {
	if adoptedBy == "" {
		adoptedBy = "unknown"
	}
	body, err := json.Marshal(drift)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO atlantis.adopt_history (caller, declared_hash, drift_count, drift_report, allow_drift, adopted_by)
VALUES ($1, $2, $3, $4, $5, $6)`,
		caller, hash, len(drift), body, allowDrift, adoptedBy)
	return err
}

// translateDrift converts a codegen.Diff to wire-side AdoptDriftItems
// and categorizes each by severity. The categorization mirrors how
// Terraform talks about a plan: + creates, - destroys, ~ modifies.
// Only the ~ (mismatch) bucket is a real disagreement; the others are
// outstanding work for the operator to apply.
func translateDrift(d *codegen.Diff) []AdoptDriftItem {
	if d == nil || d.IsEmpty() {
		return nil
	}
	var out []AdoptDriftItem
	add := func(changes []codegen.Change) {
		for _, ch := range changes {
			out = append(out, AdoptDriftItem{
				EntityID: ch.EntityID,
				Field:    ch.Field,
				Kind:     string(ch.Kind),
				Severity: classifyDriftSeverity(string(ch.Kind)),
				Detail:   ch.Detail,
			})
		}
	}
	add(d.Additive)
	add(d.BackfillRequired)
	add(d.Breaking)
	return out
}

// filterToExistingEntities returns a shallow-cloned IR with only the
// entities whose physical table exists in the live DB. Atlantis-only
// metadata (cache, partition_field, indexes, relations) survives
// unchanged for the entities that do exist; declarations for tables
// that don't yet exist are dropped so they remain pending for
// `tide apply` to materialize.
//
// Procedures and custom queries are preserved verbatim because they
// have no physical SQL footprint until invoked.
func filterToExistingEntities(in *dsl.IR, existing map[string]bool) *dsl.IR {
	out := &dsl.IR{
		Queries:    append([]dsl.CustomQuery(nil), in.Queries...),
		Procedures: append([]dsl.CustomProcedure(nil), in.Procedures...),
	}
	for _, e := range in.Entities {
		if existing[e.ID()] {
			out.Entities = append(out.Entities, e)
		}
	}
	return out
}

// classifyDriftSeverity assigns a Terraform-style severity. "Added" /
// "removed" kinds are pure additions or removals; everything else is a
// modification (both sides exist, they disagree). The string match is
// against codegen.ChangeKind values verbatim so a new kind there fails
// loudly here (default = mismatch, the safer assumption).
func classifyDriftSeverity(kind string) string {
	switch kind {
	case "entity_added",
		"field_added",
		"field_reference_added",
		"field_unique_added",
		"field_serial_added",
		"field_backfill_added":
		return "addition"
	case "entity_removed",
		"field_removed",
		"field_reference_removed",
		"field_unique_removed",
		"field_serial_removed",
		"field_backfill_removed":
		return "removal"
	}
	return "mismatch"
}
