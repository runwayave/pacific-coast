package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/cache/memcached"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// healthDeps is the process state the readiness handler inspects on
// each /readyz call. Liveness has no dependencies — the process being
// up is enough.
type healthDeps struct {
	Pool   *pgxpool.Pool
	MC     *memcached.Client
	Worker *invalidate.Worker

	// WorkerMaxStaleness caps the age of Worker.LastDrainAt before
	// /readyz fails. Sized at ~3x the drain interval to absorb jitter
	// without false negatives.
	WorkerMaxStaleness time.Duration

	// ProbeTimeout bounds each dependency check on a single /readyz
	// call so a wedged dep cannot block a probe for the full request.
	ProbeTimeout time.Duration
}

// newHealthServer wires /healthz (liveness — process is up) and /readyz
// (readiness — pg, memcached, outbox-worker all serving) on addr. Once
// shutdownSignal cancels, /readyz returns 503 immediately so the load
// balancer drains the pod before the HTTP server itself shuts down.
func newHealthServer(addr string, deps healthDeps, shutdownSignal context.Context) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", livenessHandler)
	mux.HandleFunc("/readyz", readyHandler(deps, shutdownSignal))
	mux.Handle("/metrics", promhttp.Handler())
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func livenessHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

func readyHandler(deps healthDeps, shutdownSignal context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if shutdownSignal.Err() != nil {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), deps.ProbeTimeout)
		defer cancel()

		if err := deps.Pool.Ping(ctx); err != nil {
			http.Error(w, "pg: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		// gomemcache has no Ping primitive — a probe Get returns
		// ErrCacheMiss when memcached is reachable and the key doesn't
		// exist; any other error is a network or timeout failure.
		if _, err := deps.MC.Get(ctx, "atl:readyz:probe"); err != nil && !errors.Is(err, runtime.ErrCacheMiss) {
			http.Error(w, "memcached: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if age := time.Since(deps.Worker.LastDrainAt()); age > deps.WorkerMaxStaleness {
			http.Error(w, fmt.Sprintf("outbox worker stale by %s", age), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	}
}
