package entity

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// IRLoader reads the current IR checkpoint from Postgres.
// Injected from cmd/server/main.go so this package doesn't import admin.
type IRLoader func(ctx context.Context) (*dsl.IR, string, error)

// SchemaListener watches for atl_schema_changed notifications and
// triggers hot-reload of entity metadata. Uses a dedicated pool
// connection for reliable LISTEN delivery — same pattern as the
// outbox invalidation worker.
type SchemaListener struct {
	pool   *pgxpool.Pool
	server *Server
	loadIR IRLoader
	log    *slog.Logger
}

func NewSchemaListener(pool *pgxpool.Pool, server *Server, loadIR IRLoader, log *slog.Logger) *SchemaListener {
	return &SchemaListener{
		pool:   pool,
		server: server,
		loadIR: loadIR,
		log:    log,
	}
}

var (
	schemaReloadsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "atlantis_schema_reloads_total",
		Help: "Number of successful schema hot-reloads via LISTEN/NOTIFY.",
	})
	schemaReloadErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "atlantis_schema_reload_errors_total",
		Help: "Number of failed schema hot-reload attempts.",
	})
	schemaReloadDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "atlantis_schema_reload_duration_seconds",
		Help:    "Time to rebuild entity metadata on hot-reload.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
	})
)

// Run blocks until ctx is canceled, reconnecting on any LISTEN error.
func (l *SchemaListener) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := l.listenSession(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l.log.Warn("schema listener: session ended, reconnecting", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (l *SchemaListener) listenSession(ctx context.Context) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN atl_schema_changed"); err != nil {
		return err
	}
	l.log.Info("schema listener: LISTEN active")

	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}

		newHash := n.Payload
		if newHash == l.server.ContentHash() {
			continue
		}

		l.log.Info("schema change detected", "hash", truncHash(newHash))
		l.reload(ctx, newHash)
	}
}

func (l *SchemaListener) reload(ctx context.Context, expectedHash string) {
	start := time.Now()

	ir, contentHash, err := l.loadIR(ctx)
	if err != nil {
		l.log.Error("schema reload: load IR failed", "err", err)
		schemaReloadErrors.Inc()
		return
	}

	if contentHash == l.server.ContentHash() {
		return
	}

	if err := l.server.Reload(ir, contentHash); err != nil {
		l.log.Error("schema reload: rebuild failed", "err", err)
		schemaReloadErrors.Inc()
		return
	}

	elapsed := time.Since(start)
	schemaReloadDuration.Observe(elapsed.Seconds())
	schemaReloadsTotal.Inc()
	l.log.Info("schema hot-reload complete",
		"hash", truncHash(contentHash),
		"entities", len(ir.Entities),
		"elapsed", elapsed.Round(time.Millisecond))
}

func truncHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
