// Package obs owns atlantis's Prometheus metrics surface. Every collector
// is a package-level var registered against prometheus.DefaultRegisterer
// at import time via promauto. /metrics is served by cmd/server's health
// HTTP listener.
//
// The "atlantis" namespace is fixed; subsystems are: grpc, cache_tier0,
// cache_tier1, cache_tier2, outbox, pgx. Label cardinality is bounded —
// gRPC labels are method (closed set, ~tens) and code (closed set, ~16),
// outbox labels are kind (normalized to a 3-value set) and result.
package obs

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// gRPC server.
var (
	GRPCRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "atlantis",
		Subsystem: "grpc",
		Name:      "request_duration_seconds",
		Help:      "Unary RPC duration in seconds, labeled by full method and grpc status code.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"method", "code"})

	GRPCRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "grpc",
		Name:      "requests_total",
		Help:      "Total unary RPCs, labeled by full method and grpc status code.",
	}, []string{"method", "code"})
)

// Cache tiers. Tier-0 is the in-process LRU, tier-1 is the memcached body
// cache (pk-versioned indirection), tier-2 is the memcached query-result
// cache (filter-hash-keyed). Each tier counts its own hits and misses so
// the operator can see where reads land.
var (
	CacheTier0Hits = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "cache_tier0", Name: "hits_total",
		Help: "Tier-0 (in-process LRU) cache hits.",
	})
	CacheTier0Misses = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "cache_tier0", Name: "misses_total",
		Help: "Tier-0 (in-process LRU) cache misses — fell through to tier-1.",
	})

	CacheTier1Hits = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "cache_tier1", Name: "hits_total",
		Help: "Tier-1 (memcached body) cache hits.",
	})
	CacheTier1Misses = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "cache_tier1", Name: "misses_total",
		Help: "Tier-1 (memcached body) cache misses — fell through to loader.",
	})
	CacheTier1Errors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "cache_tier1", Name: "errors_total",
		Help: "Tier-1 lookup errors (network/timeout, not misses).",
	})

	CacheTier2Hits = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "cache_tier2", Name: "hits_total",
		Help: "Tier-2 (memcached query-result) cache hits.",
	})
	CacheTier2Misses = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "cache_tier2", Name: "misses_total",
		Help: "Tier-2 (memcached query-result) cache misses.",
	})
)

// Outbox worker.
var (
	OutboxProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "outbox", Name: "processed_total",
		Help: "Outbox rows processed. Labels: kind (invalidation|generation_bump|unknown), result (success|failure|panic).",
	}, []string{"kind", "result"})

	OutboxDLQ = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "outbox", Name: "dlq_moves_total",
		Help: "Outbox rows moved to cache_invalidations_dead after exceeding MaxAttempts.",
	})

	// OutboxLagSeconds is refreshed by the worker's lag poller every
	// few seconds; querying on every scrape would be wasteful. 0 means
	// the table is empty.
	OutboxLagSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "atlantis", Subsystem: "outbox", Name: "lag_seconds",
		Help: "Age in seconds of the oldest unprocessed cache_invalidations row; 0 when empty.",
	})
)

// Backfill worker.
var (
	BackfillChunksProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "backfill", Name: "chunks_processed_total",
		Help: "Chunked-UPDATE batches the backfill worker has processed. Labels: plan_hash (truncated to 8 chars), entity, field, result (success|failure).",
	}, []string{"plan_hash", "entity", "field", "result"})

	BackfillRowsProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis", Subsystem: "backfill", Name: "rows_processed_total",
		Help: "Rows updated by the backfill worker. Labels: plan_hash (truncated), entity, field.",
	}, []string{"plan_hash", "entity", "field"})

	BackfillChunkDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "atlantis", Subsystem: "backfill", Name: "chunk_duration_seconds",
		Help:    "Duration in seconds of one chunked UPDATE batch. Labels: entity, field.",
		Buckets: prometheus.DefBuckets,
	}, []string{"entity", "field"})

	BackfillPlansInFlight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis", Subsystem: "backfill", Name: "plans_in_flight",
		Help: "Backfill plans by current phase. Labels: phase (phase2_running|phase3_running).",
	}, []string{"phase"})
)

// NormalizeOutboxKind clamps the kind label to a closed set so a
// corrupt or unknown row can't blow up the metrics cardinality.
func NormalizeOutboxKind(kind string) string {
	switch kind {
	case "", "invalidation":
		return "invalidation"
	case "generation_bump":
		return "generation_bump"
	default:
		return "unknown"
	}
}

// RegisterPoolStats wires pgx pool gauges that read pool.Stat() on each
// scrape. Stat() is cheap (atomic reads), so callback-on-scrape is fine
// here. Call once at boot after the pool is constructed.
func RegisterPoolStats(reg prometheus.Registerer, pool *pgxpool.Pool) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "atlantis", Subsystem: "pgx", Name: "acquired_conns",
			Help: "Currently-acquired pgx pool connections.",
		}, func() float64 { return float64(pool.Stat().AcquiredConns()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "atlantis", Subsystem: "pgx", Name: "idle_conns",
			Help: "Idle pgx pool connections.",
		}, func() float64 { return float64(pool.Stat().IdleConns()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "atlantis", Subsystem: "pgx", Name: "total_conns",
			Help: "Total pgx pool connections (idle + acquired + constructing).",
		}, func() float64 { return float64(pool.Stat().TotalConns()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "atlantis", Subsystem: "pgx", Name: "max_conns",
			Help: "Configured max pgx pool connections.",
		}, func() float64 { return float64(pool.Stat().MaxConns()) }),
	)
}
