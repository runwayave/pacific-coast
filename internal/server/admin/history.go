package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// ---------------------------------------------------------------------------
// GetSchemaHistory — paginated version list
// ---------------------------------------------------------------------------

// GetSchemaHistoryRequest asks for a page of schema version summaries,
// ordered newest-first. Before is a cursor (version number); only
// versions < Before are returned. Caller filters to a single caller.
type GetSchemaHistoryRequest struct {
	Limit  int32  `json:"limit,omitempty"`
	Before int64  `json:"before,omitempty"`
	Caller string `json:"caller,omitempty"`
}

// SchemaVersionSummary is the compact shape rendered by `tide history`.
type SchemaVersionSummary struct {
	Version     int64  `json:"version"`
	Caller      string `json:"caller"`
	PlanClass   string `json:"plan_class"`
	EventType   string `json:"event_type"`
	ChangeCount int    `json:"change_count"`
	CreatedAt   string `json:"created_at"`
}

// GetSchemaHistoryResponse carries one page of versions plus a flag
// indicating whether more rows exist beyond this page.
type GetSchemaHistoryResponse struct {
	Versions []SchemaVersionSummary `json:"versions"`
	HasMore  bool                   `json:"has_more"`
}

func (s *Service) GetSchemaHistory(ctx context.Context, req GetSchemaHistoryRequest) (*GetSchemaHistoryResponse, error) {
	limit := int32(25)
	if req.Limit > 0 && req.Limit <= 100 {
		limit = req.Limit
	}
	// Fetch limit+1 so we can detect whether there are more rows.
	fetchLimit := limit + 1

	var rows pgx.Rows
	var err error
	if req.Caller != "" && req.Before > 0 {
		rows, err = s.pool.Query(ctx, `
SELECT version, caller, plan_class, event_type, diff, created_at
FROM atlantis.schema_versions
WHERE version < $1 AND caller = $2
ORDER BY version DESC
LIMIT $3`, req.Before, req.Caller, fetchLimit)
	} else if req.Caller != "" {
		rows, err = s.pool.Query(ctx, `
SELECT version, caller, plan_class, event_type, diff, created_at
FROM atlantis.schema_versions
WHERE caller = $1
ORDER BY version DESC
LIMIT $2`, req.Caller, fetchLimit)
	} else if req.Before > 0 {
		rows, err = s.pool.Query(ctx, `
SELECT version, caller, plan_class, event_type, diff, created_at
FROM atlantis.schema_versions
WHERE version < $1
ORDER BY version DESC
LIMIT $2`, req.Before, fetchLimit)
	} else {
		rows, err = s.pool.Query(ctx, `
SELECT version, caller, plan_class, event_type, diff, created_at
FROM atlantis.schema_versions
ORDER BY version DESC
LIMIT $1`, fetchLimit)
	}
	if err != nil {
		return nil, fmt.Errorf("query schema_versions: %w", err)
	}
	defer rows.Close()

	var versions []SchemaVersionSummary
	for rows.Next() {
		var v SchemaVersionSummary
		var diffJSON []byte
		var createdAt interface{}
		if err := rows.Scan(&v.Version, &v.Caller, &v.PlanClass, &v.EventType, &diffJSON, &createdAt); err != nil {
			return nil, err
		}
		v.CreatedAt = fmt.Sprintf("%v", createdAt)
		v.ChangeCount = countDiffChanges(diffJSON)
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hasMore := int32(len(versions)) > limit
	if hasMore {
		versions = versions[:limit]
	}

	return &GetSchemaHistoryResponse{
		Versions: versions,
		HasMore:  hasMore,
	}, nil
}

// countDiffChanges unmarshals the diff JSON just enough to count
// total changes. Tolerant of malformed JSON — returns 0.
func countDiffChanges(raw []byte) int {
	var d struct {
		Additive         []json.RawMessage `json:"additive"`
		BackfillRequired []json.RawMessage `json:"backfill_required"`
		Breaking         []json.RawMessage `json:"breaking"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return 0
	}
	return len(d.Additive) + len(d.BackfillRequired) + len(d.Breaking)
}

// ---------------------------------------------------------------------------
// GetSchemaVersion — full data for one version
// ---------------------------------------------------------------------------

type GetSchemaVersionRequest struct {
	Version int64 `json:"version"`
}

type GetSchemaVersionResponse struct {
	Version    int64           `json:"version"`
	Caller     string          `json:"caller"`
	PlanClass  string          `json:"plan_class"`
	EventType  string          `json:"event_type"`
	Diff       json.RawMessage `json:"diff"`
	UpSQL      string          `json:"up_sql"`
	DownSQL    string          `json:"down_sql"`
	IRSnapshot json.RawMessage `json:"ir_snapshot"`
	CreatedAt  string          `json:"created_at"`
	ParentVer  *int64          `json:"parent_version,omitempty"`
}

func (s *Service) GetSchemaVersion(ctx context.Context, req GetSchemaVersionRequest) (*GetSchemaVersionResponse, error) {
	if req.Version <= 0 {
		return nil, errors.New("admin: version must be a positive integer")
	}
	var resp GetSchemaVersionResponse
	var createdAt interface{}
	err := s.pool.QueryRow(ctx, `
SELECT version, caller, plan_class, event_type, diff, up_sql, down_sql,
       ir_snapshot, created_at, parent_version
FROM atlantis.schema_versions
WHERE version = $1`, req.Version).Scan(
		&resp.Version, &resp.Caller, &resp.PlanClass, &resp.EventType,
		&resp.Diff, &resp.UpSQL, &resp.DownSQL,
		&resp.IRSnapshot, &createdAt, &resp.ParentVer,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("admin: schema version %d not found", req.Version)
		}
		return nil, fmt.Errorf("query schema_versions: %w", err)
	}
	resp.CreatedAt = fmt.Sprintf("%v", createdAt)
	return &resp, nil
}

// ---------------------------------------------------------------------------
// DiffSchemaVersions — compute diff between two historical versions
// ---------------------------------------------------------------------------

type DiffSchemaVersionsRequest struct {
	FromVersion int64 `json:"from_version"`
	ToVersion   int64 `json:"to_version"`
}

type DiffSchemaVersionsResponse struct {
	FromVersion int64           `json:"from_version"`
	ToVersion   int64           `json:"to_version"`
	Diff        json.RawMessage `json:"diff"`
	FromIR      json.RawMessage `json:"from_ir,omitempty"`
	ToIR        json.RawMessage `json:"to_ir,omitempty"`
}

func (s *Service) DiffSchemaVersions(ctx context.Context, req DiffSchemaVersionsRequest) (*DiffSchemaVersionsResponse, error) {
	if req.FromVersion <= 0 || req.ToVersion <= 0 {
		return nil, errors.New("admin: both from_version and to_version must be positive integers")
	}

	loadSnapshot := func(ver int64) ([]byte, *dsl.IR, error) {
		var raw []byte
		err := s.pool.QueryRow(ctx, `
SELECT ir_snapshot FROM atlantis.schema_versions WHERE version = $1`, ver).Scan(&raw)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil, fmt.Errorf("admin: schema version %d not found", ver)
			}
			return nil, nil, err
		}
		ir, err := dsl.DecodeJSONIR(raw)
		return raw, ir, err
	}

	fromRaw, fromIR, err := loadSnapshot(req.FromVersion)
	if err != nil {
		return nil, err
	}
	toRaw, toIR, err := loadSnapshot(req.ToVersion)
	if err != nil {
		return nil, err
	}

	d := codegen.ComputeDiff(fromIR, toIR)
	diffJSON, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("marshal diff: %w", err)
	}

	return &DiffSchemaVersionsResponse{
		FromVersion: req.FromVersion,
		ToVersion:   req.ToVersion,
		Diff:        diffJSON,
		FromIR:      fromRaw,
		ToIR:        toRaw,
	}, nil
}

// ---------------------------------------------------------------------------
// GetEntityLineage — blame for one entity
// ---------------------------------------------------------------------------

type GetEntityLineageRequest struct {
	EntityID string `json:"entity_id"`
}

type EntityLineageEntry struct {
	EntityID       string `json:"entity_id"`
	FieldName      string `json:"field_name"`
	IntroducedBy   string `json:"introduced_by"`
	IntroducedAt   int64  `json:"introduced_at"`
	LastModifiedBy string `json:"last_modified_by"`
	LastModifiedAt int64  `json:"last_modified_at"`
	RemovedAt      *int64 `json:"removed_at,omitempty"`
}

type GetEntityLineageResponse struct {
	Entries []EntityLineageEntry `json:"entries"`
}

func (s *Service) GetEntityLineage(ctx context.Context, req GetEntityLineageRequest) (*GetEntityLineageResponse, error) {
	if req.EntityID == "" {
		return nil, errors.New("admin: entity_id is required")
	}

	rows, err := s.pool.Query(ctx, `
SELECT entity_id, field_name, introduced_by, introduced_at,
       last_modified_by, last_modified_at, removed_at
FROM atlantis.entity_lineage
WHERE entity_id = $1
ORDER BY field_name`, req.EntityID)
	if err != nil {
		return nil, fmt.Errorf("query entity_lineage: %w", err)
	}
	defer rows.Close()

	var entries []EntityLineageEntry
	for rows.Next() {
		var e EntityLineageEntry
		if err := rows.Scan(&e.EntityID, &e.FieldName, &e.IntroducedBy, &e.IntroducedAt,
			&e.LastModifiedBy, &e.LastModifiedAt, &e.RemovedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &GetEntityLineageResponse{Entries: entries}, nil
}

// ---------------------------------------------------------------------------
// GetEntityOwners — entity -> caller map
// ---------------------------------------------------------------------------

type GetEntityOwnersRequest struct{}

type EntityOwnerEntry struct {
	EntityID     string `json:"entity_id"`
	IntroducedBy string `json:"introduced_by"`
	IntroducedAt int64  `json:"introduced_at"`
	FieldCount   int    `json:"field_count"`
}

type GetEntityOwnersResponse struct {
	Owners []EntityOwnerEntry `json:"owners"`
}

func (s *Service) GetEntityOwners(ctx context.Context, _ GetEntityOwnersRequest) (*GetEntityOwnersResponse, error) {
	rows, err := s.pool.Query(ctx, `
SELECT e.entity_id, e.introduced_by, e.introduced_at,
       COALESCE(f.cnt, 0) AS field_count
FROM atlantis.entity_lineage e
LEFT JOIN (
    SELECT entity_id, COUNT(*) AS cnt
    FROM atlantis.entity_lineage
    WHERE field_name != '' AND removed_at IS NULL
    GROUP BY entity_id
) f ON f.entity_id = e.entity_id
WHERE e.field_name = '' AND e.removed_at IS NULL
ORDER BY e.entity_id`)
	if err != nil {
		return nil, fmt.Errorf("query entity_lineage owners: %w", err)
	}
	defer rows.Close()

	var owners []EntityOwnerEntry
	for rows.Next() {
		var o EntityOwnerEntry
		if err := rows.Scan(&o.EntityID, &o.IntroducedBy, &o.IntroducedAt, &o.FieldCount); err != nil {
			return nil, err
		}
		owners = append(owners, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &GetEntityOwnersResponse{Owners: owners}, nil
}

// ---------------------------------------------------------------------------
// RollbackSchema — revert to a prior version
// ---------------------------------------------------------------------------

type RollbackSchemaRequest struct {
	ToVersion int64  `json:"to_version"`
	Caller    string `json:"caller"`
}

type RollbackSchemaResponse struct {
	NewVersion int64  `json:"new_version"`
	UpSQL      string `json:"up_sql"`
}

func (s *Service) RollbackSchema(ctx context.Context, req RollbackSchemaRequest) (*RollbackSchemaResponse, error) {
	if !s.allowApplyMutation {
		return nil, errors.New("admin: rollback is disabled on this server (set ATL_ALLOW_APPLY_MUTATION=true to enable)")
	}
	if req.ToVersion <= 0 {
		return nil, errors.New("admin: to_version must be a positive integer")
	}
	if req.Caller == "" {
		return nil, errors.New("admin: caller identity is required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Same advisory lock as apply/adopt so rollback can't race.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x70636661706c79)); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}

	// Load target version's IR snapshot.
	var targetIRRaw []byte
	err = tx.QueryRow(ctx, `
SELECT ir_snapshot FROM atlantis.schema_versions WHERE version = $1`, req.ToVersion).Scan(&targetIRRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("admin: schema version %d not found", req.ToVersion)
		}
		return nil, fmt.Errorf("load target version: %w", err)
	}
	targetIR, err := dsl.DecodeJSONIR(targetIRRaw)
	if err != nil {
		return nil, fmt.Errorf("decode target IR: %w", err)
	}

	// Load current IR from ir_checkpoint.
	currentIR, err := s.loadCheckpointTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("load current checkpoint: %w", err)
	}
	if currentIR == nil {
		return nil, errors.New("admin: no current checkpoint to rollback from")
	}

	// Compute diff from current to target (so the SQL takes us from
	// current state to target state).
	codegen.AssignProtoNumbers(currentIR, targetIR)
	d := codegen.ComputeDiff(currentIR, targetIR)
	scripts, err := codegen.EmitSQL(currentIR, targetIR, d)
	if err != nil {
		return nil, fmt.Errorf("emit rollback sql: %w", err)
	}

	// Execute the rollback SQL.
	if scripts.Up != "" {
		if _, err := tx.Exec(ctx, scripts.Up); err != nil {
			return nil, fmt.Errorf("rollback apply: %w", err)
		}
	}

	// Persist checkpoint with rollback metadata.
	parentVer := req.ToVersion
	version, err := s.persistCheckpoint(ctx, tx, targetIR, versionMeta{
		Caller:    req.Caller,
		PlanClass: d.HighestClass().String(),
		Diff:      d,
		UpSQL:     scripts.Up,
		DownSQL:   scripts.Down,
		EventType: "rollback",
		ParentVer: &parentVer,
	})
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return &RollbackSchemaResponse{
		NewVersion: version,
		UpSQL:      scripts.Up,
	}, nil
}
