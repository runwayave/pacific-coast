package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// config carries everything the server needs to boot. Every field is
// populated from environment variables; defaults aim to match a developer
// running `make dev-isolated` against the local docker-compose stack.
//
// The shape mirrors what is shipped in .env.example so operators have a
// single reference. Adding a new knob means: new field here, new env lookup
// in loadConfig, new line in .env.example.
type config struct {
	// gRPC listener
	GRPCAddr string

	// Postgres
	PGURL                 string
	PGMaxConns            int32
	PGMinConns            int32
	PGMaxConnIdle         time.Duration
	PGMaxConnLifetime     time.Duration
	PGHealthCheckPeriod   time.Duration
	PGQueryTimeoutDefault time.Duration

	// Memcached (comma-separated)
	MemcachedAddrs   []string
	MemcachedTimeout time.Duration

	// Cache read path
	CacheLRUSize    int
	CacheDefaultTTL time.Duration
	CacheXFetchBeta float64

	// Outbox worker
	OutboxBatchSize     int
	OutboxDrainInterval time.Duration
	OutboxAlertLag      time.Duration
	OutboxPointerTTL    time.Duration

	// mTLS — empty paths disable TLS (development only).
	TLSCertFile string
	TLSKeyFile  string
	TLSCAFile   string

	// Rate limit. DefaultQPS applies when no per-caller
	// override is set; PerCaller carries `caller=qps` pairs parsed from
	// RATE_LIMIT_PER_CALLER. Saturation cutoff is the AcquiredConns / MaxConns
	// ratio at which low-priority RPCs start shedding.
	RateLimitDefaultQPS       int
	RateLimitBurst            int
	RateLimitPerCaller        map[string]int
	RateLimitSaturationCutoff float64

	// AutoMigrate runs migrations on boot when true. Default false in
	// production (operators apply migrations explicitly via tidectl /
	// golang-migrate); compose / make dev-isolated flip this on so first-
	// day workflows don't need an extra step.
	AutoMigrate   bool
	MigrationsDir string

	// Schema-management toggles surfaced to the admin Service.
	//
	// AdminMirrorSchema writes the caller's submitted .atl files to
	// AdminMirrorDir on a successful ApplyMigration, partitioned by
	// caller name. Intended for local development so a file-watcher can
	// regenerate typed code and restart the server when a caller pushes
	// schema. Off in production; the binary never touches its own
	// checkout there.
	//
	// AdminAllowApplyMutation gates the ApplyMigration RPC. On by
	// default: caller CI runs `tide apply` against this server, the
	// server applies the DDL and persists the new IR checkpoint under
	// an advisory lock. Per-caller cert identity is pinned by mTLS and
	// the diff classifier prevents one caller from breaking another's
	// schema. Set to false only for regulated workloads (SOX/HIPAA/PCI)
	// that require literal SQL review before any production database
	// change — schema changes then flow through a PR against the
	// atlantis deployment repo. Read-only admin RPCs (plan, pull)
	// remain available regardless.
	AdminMirrorSchema       bool
	AdminMirrorDir          string
	AdminAllowApplyMutation bool

	// AdminMutationAllowedCallers is the per-CN allowlist for mutating
	// admin RPCs (ApplyMigration, BeginBackfillPlan). Use it in
	// regulated deployments (AdminAllowApplyMutation=false) to scope
	// mutations to specific CI cert CNs, or in default deployments to
	// tighten the wildcard. Empty = no per-CN exceptions; only
	// AdminAllowApplyMutation grants permission.
	AdminMutationAllowedCallers []string

	// AdminOperatorAllowedCallers gates operator-only mutating admin
	// RPCs (RevokeCaller, RollbackSchema, AdoptBaseline). Typically just
	// the console's cert CN. Empty = fall back to AdminAllowApplyMutation
	// for backward compatibility.
	AdminOperatorAllowedCallers []string

	// BackfillWorkerEnabled toggles the chunked-UPDATE backfill worker
	// + the BeginBackfillPlan admin RPC. Default false until the feature
	// is canaried in staging.
	BackfillWorkerEnabled bool

	// JobsWorkerEnabled toggles the declarative-job worker pool. Default
	// false during initial canary so the SubmitJob RPC is reachable for
	// smoke testing without immediately processing claimed work; flip
	// to true once handlers are registered and the operator's ready for
	// jobs to actually run.
	JobsWorkerEnabled bool

	// JobsQueues is the comma-separated list of queue names this pod
	// drains. Each queue gets its own Worker goroutine so concurrent
	// drainers don't contend across logical workloads (e.g. "shopify"
	// vs "default"). Empty defaults to a single "default" queue.
	JobsQueues []string

	// JobsRemoteHandlers maps job names to remote gRPC endpoints.
	// Format: "vendor.ShopifyImport=localhost:50051,consumer.SweepExpired=localhost:50052"
	JobsRemoteHandlers map[string]string

	// Observability
	LogLevel string

	// LogRingSize is the in-process slog ring buffer the console's
	// Health page tail reads from. Power-of-two; default 8192. Memory
	// cost ≈ size × 400 B.
	LogRingSize int

	// Health HTTP server (k8s probes hit this on a separate port).
	HealthAddr         string
	HealthProbeTimeout time.Duration
}

// loadConfig reads env vars with sensible defaults. Returns an error iff a
// required value is missing AND has no default (currently only PG_URL,
// since the server cannot start without a database).
func loadConfig() (config, error) {
	c := config{
		GRPCAddr:              envStr("GRPC_LISTEN", ":9090"),
		PGURL:                 os.Getenv("PG_URL"),
		PGMaxConns:            envInt32("PG_MAX_CONNS", 50),
		PGMinConns:            envInt32("PG_MIN_CONNS", 10),
		PGMaxConnIdle:         envDuration("PG_MAX_CONN_IDLE", 5*time.Minute),
		PGMaxConnLifetime:     envDuration("PG_MAX_CONN_LIFETIME", time.Hour),
		PGHealthCheckPeriod:   envDuration("PG_HEALTHCHECK_PERIOD", 30*time.Second),
		PGQueryTimeoutDefault: envDuration("PG_QUERY_TIMEOUT_DEFAULT", 2*time.Second),

		MemcachedAddrs:   splitCSV(envStr("MEMCACHED_ADDR", "localhost:11211")),
		MemcachedTimeout: envDuration("MEMCACHED_TIMEOUT", 100*time.Millisecond),

		CacheLRUSize:    envInt("CACHE_LRU_SIZE", 1024),
		CacheDefaultTTL: envDuration("CACHE_DEFAULT_TTL", 10*time.Minute),
		CacheXFetchBeta: envFloat("CACHE_XFETCH_BETA", 1.0),

		OutboxBatchSize:     envInt("OUTBOX_BATCH_SIZE", 100),
		OutboxDrainInterval: envDuration("OUTBOX_DRAIN_INTERVAL", 250*time.Millisecond),
		OutboxAlertLag:      envDuration("OUTBOX_ALERT_LAG", 5*time.Minute),
		OutboxPointerTTL:    envDuration("OUTBOX_POINTER_TTL", 24*time.Hour),

		TLSCertFile: os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile:  os.Getenv("TLS_KEY_FILE"),
		TLSCAFile:   os.Getenv("TLS_CA_FILE"),

		RateLimitDefaultQPS:       envInt("RATE_LIMIT_DEFAULT_QPS", 1000),
		RateLimitBurst:            envInt("RATE_LIMIT_BURST", 200),
		RateLimitPerCaller:        parsePerCallerLimits(os.Getenv("RATE_LIMIT_PER_CALLER")),
		RateLimitSaturationCutoff: envFloat("RATE_LIMIT_SATURATION_CUTOFF", 0.80),

		AutoMigrate:   envBool("AUTO_MIGRATE", false),
		MigrationsDir: envStr("MIGRATIONS_DIR", "migrations"),

		AdminMirrorSchema:           envBool("ATL_MIRROR_SCHEMA", false),
		AdminMirrorDir:              envStr("ATL_MIRROR_DIR", "schema"),
		AdminAllowApplyMutation:     envBool("ATL_ALLOW_APPLY_MUTATION", true),
		AdminMutationAllowedCallers: splitCSV(os.Getenv("ATL_MUTATION_ALLOWED_CALLERS")),
		AdminOperatorAllowedCallers: splitCSV(os.Getenv("ATL_OPERATOR_ALLOWED_CALLERS")),

		BackfillWorkerEnabled: envBool("ATL_BACKFILL_WORKER_ENABLED", false),

		JobsWorkerEnabled:  envBool("ATL_JOBS_WORKER_ENABLED", false),
		JobsQueues:         splitCSV(envStr("ATL_JOBS_QUEUES", "default")),
		JobsRemoteHandlers: parseRemoteHandlers(envStr("ATL_JOBS_REMOTE_HANDLERS", "")),

		LogLevel:    envStr("LOG_LEVEL", "info"),
		LogRingSize: envInt("LOG_RING_SIZE", 8192),

		HealthAddr:         envStr("HEALTH_LISTEN", ":8081"),
		HealthProbeTimeout: envDuration("HEALTH_PROBE_TIMEOUT", time.Second),
	}
	if c.PGURL == "" {
		return c, fmt.Errorf("PG_URL is required")
	}
	// All three TLS files must be present together; a partial set is an error
	// because it almost certainly means a misconfiguration rather than an
	// intentional dev mode.
	cert, key, ca := c.TLSCertFile, c.TLSKeyFile, c.TLSCAFile
	hasAny := cert != "" || key != "" || ca != ""
	hasAll := cert != "" && key != "" && ca != ""
	if hasAny && !hasAll {
		return c, fmt.Errorf("TLS_CERT_FILE, TLS_KEY_FILE, TLS_CA_FILE must all be set (or all empty for dev)")
	}
	return c, nil
}

func envStr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envInt32(name string, def int32) int32 {
	n := envInt(name, int(def))
	return int32(n)
}

func envFloat(name string, def float64) float64 {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return n
}

func envBool(name string, def bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// parsePerCallerLimits parses "caller1=qps1,caller2=qps2" into a map.
// Malformed pairs are silently dropped — the misconfig surfaces at startup
// via the resulting empty map, and the default QPS applies. A future
// enhancement could log dropped entries, but the dev-friendliness of a
// permissive parse outweighs that today.
func parsePerCallerLimits(s string) map[string]int {
	if s == "" {
		return nil
	}
	out := map[string]int{}
	for _, kv := range strings.Split(s, ",") {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 || eq == len(kv)-1 {
			continue
		}
		caller := strings.TrimSpace(kv[:eq])
		qps, err := strconv.Atoi(strings.TrimSpace(kv[eq+1:]))
		if err != nil || qps <= 0 {
			continue
		}
		out[caller] = qps
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseRemoteHandlers(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return out
}
