// atlantis gRPC server entrypoint.
//
// Composition root. Reads env config, builds the runtime tier (pgx pool,
// memcached client, reader, outbox, invalidation worker), constructs the
// gRPC server with mTLS + interceptors + health + reflection, and runs
// until the process is signaled.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/rachitkumar205/atlantis/internal/auth"
	"github.com/rachitkumar205/atlantis/internal/backfill"
	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/cache/memcached"
	"github.com/rachitkumar205/atlantis/internal/cache/queryresult"
	"github.com/rachitkumar205/atlantis/internal/cache/read"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/obs"
	"github.com/rachitkumar205/atlantis/internal/server/admin"
	"github.com/rachitkumar205/atlantis/internal/server/entity"
	"github.com/rachitkumar205/atlantis/internal/server/interceptors"
	"github.com/rachitkumar205/atlantis/internal/server/jobsdispatcher"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
	"github.com/rachitkumar205/atlantis/jobs"
)

// podID returns the local pod identifier used in the dispatcher's
// claimed_by formatting. Hostname-pid mirrors DefaultConfig in
// clients/go/jobs.
func podID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "atlantis"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// version is stamped at build time via -ldflags "-X main.version=...".
// Unstamped local builds report "dev".
var version = "dev"

func main() {
	cfg, err := loadConfig()
	if err != nil {
		// Use stderr directly — the logger isn't built yet.
		_, _ = os.Stderr.WriteString("config: " + err.Error() + "\n")
		os.Exit(2)
	}

	log, logRing := buildLogger(cfg)
	log.Info("atlantis starting", "version", version)

	// Top-level context cancels on SIGINT / SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, log, logRing); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// run is the top-level orchestrator. Resources are constructed in
// dependency order so each Close-defer runs LIFO at the end: pool is
// created first so the invalidation worker (which acquires a connection)
// can register cleanup before its Run starts.
func run(ctx context.Context, cfg config, log *slog.Logger, logRing *obs.LogRing) error {
	if cfg.AutoMigrate {
		if err := runAutoMigrate(cfg.PGURL, cfg.MigrationsDir, log); err != nil {
			return err
		}
	}

	pool, err := pg.New(ctx, pg.Config{
		URL:                 cfg.PGURL,
		MaxConns:            cfg.PGMaxConns,
		MinConns:            cfg.PGMinConns,
		MaxConnIdleTime:     cfg.PGMaxConnIdle,
		MaxConnLifetime:     cfg.PGMaxConnLifetime,
		HealthCheckPeriod:   cfg.PGHealthCheckPeriod,
		QueryTimeoutDefault: cfg.PGQueryTimeoutDefault,
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("pg pool ready", "max_conns", cfg.PGMaxConns)

	obs.RegisterPoolStats(nil, pool.Raw())

	// Auth allowlist is loaded once at startup and refreshed on its own
	// goroutine. The refresher uses a ctx detached from SIGTERM so a
	// blocking reload during shutdown doesn't lock callers out mid-drain.
	authAllowlist := auth.New(pool.Raw(), log.With("component", "auth"))
	if err := authAllowlist.Reload(ctx); err != nil {
		return fmt.Errorf("load auth allowlist: %w", err)
	}
	log.Info("auth allowlist ready", "callers", authAllowlist.Size())
	allowlistCtx, cancelAllowlist := context.WithCancel(context.Background())
	defer cancelAllowlist()
	go authAllowlist.RunRefresher(allowlistCtx, 30*time.Second)

	mc, err := memcached.New(memcached.Config{
		Addrs:        cfg.MemcachedAddrs,
		Timeout:      cfg.MemcachedTimeout,
		MaxIdleConns: 8,
	})
	if err != nil {
		return err
	}
	defer func() { _ = mc.Close() }()
	log.Info("memcached client ready", "addrs", cfg.MemcachedAddrs)

	reader, err := read.New(mc, read.Config{
		LRUSize:       cfg.CacheLRUSize,
		MaxValueBytes: 1 << 20,
		DefaultTTL:    cfg.CacheDefaultTTL,
		XFetchBeta:    cfg.CacheXFetchBeta,
	})
	if err != nil {
		return err
	}

	// One Cache per process. The worker drives BumpGeneration on
	// generation_bump outbox rows; the entity servers (wired below)
	// drive Generation + Lookup + Store on every Query<Entity> RPC.
	queryCache := queryresult.New(mc)

	worker, err := invalidate.NewWorker(pool.Raw(), mc, reader, queryCache, invalidate.WorkerConfig{
		Schema:        "atlantis",
		DrainInterval: cfg.OutboxDrainInterval,
		BatchSize:     cfg.OutboxBatchSize,
		PointerTTL:    cfg.OutboxPointerTTL,
		AlertLag:      cfg.OutboxAlertLag,
		Logger:        log.With("component", "outbox-worker"),
	})
	if err != nil {
		return err
	}

	// Worker uses a ctx detached from SIGTERM so it keeps draining during
	// the gRPC GracefulStop window. cancelWorker fires post-GracefulStop.
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()

	var workerWG sync.WaitGroup
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("outbox worker panic", "panic", rec)
			}
		}()
		if err := worker.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("outbox worker exited", "err", err)
		}
	}()

	log.Debug("init: health http server")
	healthHTTP := newHealthServer(cfg.HealthAddr, healthDeps{
		Pool:               pool.Raw(),
		MC:                 mc,
		Worker:             worker,
		WorkerMaxStaleness: 3 * cfg.OutboxDrainInterval,
		ProbeTimeout:       cfg.HealthProbeTimeout,
		StartedAt:          time.Now(),
		Version:            version,
	}, ctx)
	go func() {
		log.Info("health http listening", "addr", cfg.HealthAddr)
		if err := healthHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health http server", "err", err)
		}
	}()

	log.Debug("init: build transport creds")
	creds, err := transportCreds(cfg, log)
	if err != nil {
		return err
	}

	log.Debug("init: build rate-limit interceptor")
	rateLimit := interceptors.NewRateLimit(pool.Raw(), interceptors.RateLimitConfig{
		DefaultQPS:           cfg.RateLimitDefaultQPS,
		Burst:                cfg.RateLimitBurst,
		PerCaller:            cfg.RateLimitPerCaller,
		PoolSaturationCutoff: cfg.RateLimitSaturationCutoff,
		CallerFromContext:    callerFromContext,
	})

	log.Debug("init: grpc.NewServer")
	// One AuthChecker → two interceptors (Unary + Stream) that share
	// the same allowlist + exempt-prefix set. Admin RPCs are exempt so
	// a new caller can register schema before they appear in the
	// entity-RPC allowlist; health + reflection are infrastructure
	// probes.
	authChecker := interceptors.NewAuthChecker(interceptors.AuthConfig{
		Allowlist:         authAllowlist,
		Enforce:           cfg.TLSCertFile != "",
		CallerFromContext: callerFromContext,
		ExemptPrefixes: []string{
			"/atlantis.admin.v1.Admin/",
			"/grpc.health.v1.Health/",
			"/grpc.reflection.",
		},
	})

	// admin.Service is constructed early because the cert-binding
	// interceptor needs its LookupCallerCertBinding method. Register on
	// the gRPC server happens after server construction below.
	adminSvc := admin.New(pool.Raw(), admin.Config{
		MirrorDir:              cfg.AdminMirrorDir,
		MirrorEnabled:          cfg.AdminMirrorSchema,
		AllowApplyMutation:     cfg.AdminAllowApplyMutation,
		MutationAllowedCallers: cfg.AdminMutationAllowedCallers,
		OperatorAllowedCallers: cfg.AdminOperatorAllowedCallers,
		// Share the cert-CN extractor with the auth + rate-limit
		// interceptors so every layer agrees on caller identity for the
		// same request — a divergence here would let a CN authorized
		// by one layer be evaluated as a different identity by another.
		CallerFromContext: callerFromContext,
		BackfillEnabled:   cfg.BackfillWorkerEnabled,
		LogRing:           logRing,
	})

	// Cert binding: bind each caller_identities row to a specific leaf
	// fingerprint. Every authenticated RPC must present a cert whose
	// SHA-256 matches the row's stored fingerprint; mismatch or missing
	// row → Unauthenticated. This is what makes re-issue + revoke
	// actually invalidate prior certs at the auth layer without a CRL.
	//
	// Exempt: the management-plane CN (the console BFF) doesn't have a
	// fingerprint of its own — its auth is the session cookie + sudo
	// layer in front of the BFF — so we skip binding for it. Operators
	// can add more CNs via ATL_CERT_BINDING_EXEMPT_CALLERS if they have
	// a similar bootstrap CN that authenticates by other means.
	// One CertBindingChecker → both interceptor flavors share one
	// TTL cache, so a stream lookup for "vendor" and a unary lookup
	// for "vendor" hit the same cache entry instead of duplicating
	// the DB round-trip across two parallel caches.
	certBindingChecker := interceptors.NewCertBindingChecker(interceptors.CertBindingConfig{
		Lookup:            adminSvc.LookupCallerCertBinding,
		Enforce:           cfg.TLSCertFile != "",
		CallerFromContext: callerFromContext,
		ExemptCallers:     cfg.CertBindingExemptCallers,
		Log:               log,
	})

	// Stream chain mirrors the unary chain order for everything that
	// applies on a per-stream basis. Rate limiting is intentionally
	// excluded — it's an RPCs/sec concept and a long-lived stream
	// (one per worker pod, hours long) doesn't fit. Cert binding +
	// allowlist are reapplied at stream open via the same shared
	// check helpers, so revoked certs and unregistered callers can't
	// bypass via the streaming surface.
	srv := grpc.NewServer(
		grpc.Creds(creds),
		grpc.ChainUnaryInterceptor(
			recoveryInterceptor(log),
			interceptors.NewMetrics(),
			resolveCallerInterceptor(),
			certBindingChecker.Unary(),
			authChecker.Unary(),
			rateLimit,
			loggingInterceptor(log),
		),
		grpc.ChainStreamInterceptor(
			recoveryStreamInterceptor(log),
			interceptors.NewMetricsStream(),
			resolveCallerStreamInterceptor(),
			certBindingChecker.Stream(),
			authChecker.Stream(),
			loggingStreamInterceptor(log),
		),
	)

	log.Debug("init: health + reflection")
	healthSrv := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	reflection.Register(srv)

	log.Debug("init: register admin service")
	admin.Register(srv, adminSvc)

	// Backfill worker — gated by ATL_BACKFILL_WORKER_ENABLED. Shares
	// workerCtx with the invalidate worker so SIGTERM stops both, and
	// uses defer-recover so a panic in one row's chunk doesn't take the
	// process down.
	if cfg.BackfillWorkerEnabled {
		bfWorker := backfill.NewWorker(pool.Raw(), backfill.Config{
			Schema:       "atlantis",
			PollInterval: time.Second,
			ChunkSize:    10000,
			Throttle:     100 * time.Millisecond,
			Logger:       log.With("component", "backfill-worker"),
		})
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("backfill worker panic", "panic", rec)
				}
			}()
			if err := bfWorker.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("backfill worker exited", "err", err)
			}
		}()
		log.Info("backfill worker enabled")
	}

	// Jobs worker pool — one Worker per queue named in
	// ATL_JOBS_QUEUES. The Registry stays empty in this slice; once
	// caller code uses the generated SDK to RegisterJobHandlers, the
	// worker's runtime lookups will resolve. Until then, submitted
	// jobs sit in atlantis.jobs awaiting handler deployment.
	var jobsRegistry *jobs.Registry
	if cfg.JobsWorkerEnabled {
		jobsRegistry = jobs.NewRegistry()
		for jobName, addr := range cfg.JobsRemoteHandlers {
			jobs.RegisterRemote(jobsRegistry, jobName, addr)
			log.Info("jobs remote handler registered", "job", jobName, "addr", addr)
		}
		for _, queue := range cfg.JobsQueues {
			queue := queue
			w := jobs.NewWorker(pool.Raw(), jobsRegistry, queue, jobs.Config{
				Schema:        "atlantis",
				DrainInterval: time.Second,
				BatchSize:     50,
				Logger:        log.With("component", "jobs-worker", "queue", queue),
			})
			workerWG.Add(1)
			go func() {
				defer workerWG.Done()
				defer func() {
					if rec := recover(); rec != nil {
						log.Error("jobs worker panic", "queue", queue, "panic", rec)
					}
				}()
				if err := w.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
					log.Error("jobs worker exited", "queue", queue, "err", err)
				}
			}()
			log.Info("jobs worker enabled", "queue", queue)
		}
	}
	_ = jobsRegistry // stashed for future public access; today only the in-process worker goroutines need it.

	// Worker-poll dispatcher — gated by ATL_JOBS_DISPATCHER_ENABLED.
	// Drains atlantis.jobs against this pod's PG and pushes work over
	// a bidi gRPC stream to remote workers (laptop-dev or any caller
	// without direct PG access). Coexists safely with direct-PG SDK
	// workers via the shared SKIP LOCKED claim helpers.
	var dispatcher *jobsdispatcher.Dispatcher
	if cfg.JobsDispatcherEnabled {
		dispatcher = jobsdispatcher.New(pool.Raw(), jobsdispatcher.Config{
			// 5-minute default is the Temporal-aligned lease budget
			// for long-running handlers. Short-running handlers can
			// override down via the DSL `heartbeat 30s` modifier;
			// long-running ones override up via `heartbeat 10m`.
			// (Auto-heartbeat tick is HeartbeatBudget/3, sent to the
			// SDK via SessionAccepted at session open.)
			HeartbeatBudget: 5 * time.Minute,
			DrainInterval:   time.Second,
			BatchSize:       50,
			AckTimeoutMS:    150000, // HeartbeatBudget / 2
			ShutdownBudget:  30 * time.Second,
			PodID:           podID(),
			IRLoader: func(_ context.Context) (*dsl.IR, error) {
				ir, _, err := loadIRCheckpoint(pool)
				return ir, err
			},
			// Alias loader lets the dispatcher accept a caller whose CN
			// satisfies visible_to via an operator-configured alias
			// (PostgreSQL-roles / AD-SID / DNS-CNAME pattern). Fetched
			// once per stream at Open; cached on the session for its
			// lifetime so steady-state workers never re-fetch.
			AliasLoader:       adminSvc.LookupCallerAliases,
			CallerFromContext: callerFromContext,
			Logger:            log.With("component", "jobs-dispatcher"),
		})
		jobsdispatcher.Register(srv, dispatcher)
		adminSvc.SetDispatcher(newDispatcherAdapter(dispatcher))
		for _, queue := range cfg.JobsDispatcherQueues {
			queue := queue
			workerWG.Add(1)
			go func() {
				defer workerWG.Done()
				defer func() {
					if rec := recover(); rec != nil {
						log.Error("jobs dispatcher panic", "queue", queue, "panic", rec)
					}
				}()
				dispatcher.RunQueue(workerCtx, queue)
			}()
			log.Info("jobs dispatcher enabled", "queue", queue)
		}
	} else {
		log.Info("jobs dispatcher disabled (set ATL_JOBS_DISPATCHER_ENABLED=true to enable)")
	}

	log.Debug("init: load IR checkpoint")
	ir, irHash, err := loadIRCheckpoint(pool)
	if err != nil {
		return fmt.Errorf("load IR checkpoint: %w", err)
	}
	if irHash != "" {
		log.Info("loaded IR checkpoint", "hash", irHash[:min(12, len(irHash))], "entities", len(ir.Entities))
	}

	log.Debug("init: register entity services")
	dynServer := entity.NewServer(pool, mc, invalidate.NewOutbox(), queryCache)
	if err := dynServer.Register(srv, ir); err != nil {
		return fmt.Errorf("register entity services: %w", err)
	}

	log.Debug("init: schema listener")
	schemaListener := entity.NewSchemaListener(pool.Raw(), dynServer, func(ctx context.Context) (*dsl.IR, string, error) {
		return loadIRCheckpoint(pool)
	}, log.With("component", "schema-listener"))
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("schema listener panic", "panic", rec)
			}
		}()
		if err := schemaListener.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("schema listener exited", "err", err)
		}
	}()
	log.Info("schema listener enabled")

	log.Debug("init: net.Listen", "addr", cfg.GRPCAddr)
	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}
	defer func() { _ = lis.Close() }()
	log.Info("grpc listening", "addr", cfg.GRPCAddr)

	// Serve until ctx is canceled; then GracefulStop.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(lis); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	// Graceful shutdown: stop accepting new RPCs, wait for in-flight up
	// to a bounded budget, then force.
	healthSrv.Shutdown()
	shutdownDone := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(15 * time.Second):
		log.Warn("graceful shutdown timed out; forcing")
		srv.Stop()
	}

	// Dispatcher drain runs AFTER gRPC GracefulStop (so no new
	// WorkerSession streams arrive) and BEFORE pool.Close() (because
	// the drain writes release rows to PG). Sessions get Goodbye'd,
	// in-flight rows wait for the configured budget, anything
	// remaining is force-released back to pending.
	if dispatcher != nil {
		dispatchShutdownCtx, cancelDispatch := context.WithTimeout(context.Background(), 45*time.Second)
		released := dispatcher.Shutdown(dispatchShutdownCtx)
		cancelDispatch()
		if released > 0 {
			log.Warn("dispatcher shutdown force-released rows", "count", released)
		}
	}

	// Final drain pass: stop the worker loop, wait for the goroutine,
	// then flush rows that in-flight RPCs enqueued during GracefulStop.
	// Otherwise a pod restart leaves pending invalidations for the next
	// pod and readers see stale cache in the gap.
	cancelWorker()
	workerWG.Wait()

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDrain()
	if err := worker.Drain(drainCtx); err != nil {
		log.Warn("final outbox drain incomplete", "err", err)
	}

	healthShutdownCtx, cancelHealthShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelHealthShutdown()
	if err := healthHTTP.Shutdown(healthShutdownCtx); err != nil {
		log.Warn("health http shutdown", "err", err)
	}
	return nil
}

// buildLogger wires slog from the LOG_LEVEL env var. JSON in non-dev, text
// when LOG_LEVEL is "debug" so local runs are readable.
//
// The returned LogRing is teed off the same handler — every slog call
// also publishes into it for the console's Health page tail. The ring
// is lock-free (see internal/obs/logring.go) so the tee adds ~50-100 ns
// per emit, invisible at millisecond-scale RPC latencies.
func buildLogger(cfg config) (*slog.Logger, *obs.LogRing) {
	lvl := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	var base slog.Handler
	if cfg.LogLevel == "debug" {
		base = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	} else {
		base = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	}
	ring := obs.NewLogRing(cfg.LogRingSize)
	h := obs.NewRingHandler(base, ring)
	log := slog.New(h)
	slog.SetDefault(log)
	return log, ring
}

// loadIRCheckpoint reads the current IR and content hash from
// atlantis.ir_checkpoint. If no checkpoint exists yet (fresh database),
// an empty IR is returned so the server can start and accept PlanSchema.
func loadIRCheckpoint(pool *pg.Pool) (*dsl.IR, string, error) {
	var raw []byte
	var contentHash *string
	err := pool.QueryRow(context.Background(),
		`SELECT ir, content_hash FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&raw, &contentHash)
	if err != nil {
		return &dsl.IR{Version: dsl.CurrentIRVersion}, "", nil
	}
	ir, err := dsl.DecodeJSONIR(raw)
	if err != nil {
		return nil, "", err
	}
	hash := ""
	if contentHash != nil {
		hash = *contentHash
	}
	return ir, hash, nil
}
