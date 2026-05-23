package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// SweepExpiredJobName is the canonical id for the built-in TTL
// sweeper job. atlantis-server registers this as a handler at
// startup and inserts a job_schedules row so the sweeper fires
// periodically without any caller-side declaration.
const SweepExpiredJobName = "atlantis.SweepExpired"

// SweepExpiredArgs is empty: the sweeper reads the IR checkpoint
// at runtime to discover which entities have a ttl_field, so it
// adapts to schema changes without a redeploy.
type SweepExpiredArgs struct{}

// SweepExpiredHandler implements the built-in TTL sweeper. One
// invocation scans every entity with a ttl_field and DELETEs rows
// whose TTL column is in the past. Batched with a LIMIT to avoid
// holding a lock for too long; the scheduler will fire again on
// the next cron tick to catch remaining rows.
type SweepExpiredHandler struct {
	Pool       *pgxpool.Pool
	Logger     *slog.Logger
	BatchLimit int
}

// Handle is the jobs.Handler implementation.
func (h *SweepExpiredHandler) Handle(ctx context.Context, argsJSON []byte) error {
	// Load the current IR checkpoint to discover which entities have
	// ttl_field set. This makes the sweeper schema-aware without
	// needing a restart when an entity gains or loses a ttl_field.
	var irRaw []byte
	err := h.Pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&irRaw)
	if err != nil {
		return fmt.Errorf("sweep: load checkpoint: %w", err)
	}
	ir, err := dsl.DecodeJSONIR(irRaw)
	if err != nil {
		return fmt.Errorf("sweep: decode ir: %w", err)
	}

	limit := h.BatchLimit
	if limit <= 0 {
		limit = 1000
	}

	total := int64(0)
	for _, e := range ir.Entities {
		if e.TtlField == "" {
			continue
		}
		schema, table := resolvePhysical(&e)
		sql := fmt.Sprintf(`DELETE FROM %s.%s WHERE %s < now() LIMIT %d`,
			schema, table, e.TtlField, limit)
		tag, err := h.Pool.Exec(ctx, sql)
		if err != nil {
			h.Logger.Warn("sweep: delete failed", "entity", e.ID(), "err", err)
			continue
		}
		n := tag.RowsAffected()
		if n > 0 {
			h.Logger.Info("sweep: deleted expired rows", "entity", e.ID(), "count", n)
			total += n
		}
	}

	if total > 0 {
		_ = Checkpoint(ctx, 100, fmt.Sprintf("swept %d expired row(s)", total))
	}
	return nil
}

// resolvePhysical returns the (schema, table) for an entity, honoring
// the table override. Mirrors introspect.physical without importing
// it to avoid a circular dependency.
func resolvePhysical(e *dsl.Entity) (schema, table string) {
	if e.TableName == "" {
		return "atlantis", e.Namespace + "_" + snakeCaseSweep(e.Name)
	}
	for i, c := range e.TableName {
		if c == '.' {
			return e.TableName[:i], e.TableName[i+1:]
		}
	}
	return "atlantis", e.TableName
}

func snakeCaseSweep(name string) string {
	var out []byte
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out = append(out, '_')
			}
			out = append(out, byte(r+('a'-'A')))
			continue
		}
		out = append(out, byte(r))
	}
	return string(out)
}

// RegisterSweeper wires the built-in SweepExpired job into the
// registry. Called from cmd/server/main.go when the jobs worker is
// enabled; no caller-side opt-in is needed because this is a
// platform-level sweeper, not a domain handler.
func RegisterSweeper(reg *Registry, pool *pgxpool.Pool, logger *slog.Logger) {
	handler := &SweepExpiredHandler{
		Pool:       pool,
		Logger:     logger,
		BatchLimit: 1000,
	}
	reg.Register(SweepExpiredJobName, HandlerFunc(func(ctx context.Context, argsJSON []byte) error {
		return handler.Handle(ctx, argsJSON)
	}))
}

// EnsureSweepSchedule inserts the atlantis.SweepExpired row into
// atlantis.job_schedules if it doesn't already exist. The sweeper
// fires every minute; operators can UPDATE the cron_spec or set
// enabled=false to change cadence or pause.
func EnsureSweepSchedule(ctx context.Context, pool *pgxpool.Pool) error {
	const upsert = `
INSERT INTO atlantis.job_schedules (job_name, cron_spec, default_args)
VALUES ($1, '* * * * *', $2)
ON CONFLICT (job_name) DO NOTHING`
	args, _ := json.Marshal(SweepExpiredArgs{})
	_, err := pool.Exec(ctx, upsert, SweepExpiredJobName, args)
	return err
}
