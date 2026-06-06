# Configuration

Environment variables read by the Atlantis server. The full list lives in `cmd/server/config.go`; `.env.example` ships with defaults that match this page.

## Required

`PG_URL` — PostgreSQL connection string in libpq URL format. The only variable without a default; the server exits at startup if it's unset.

## Reloading and precedence

All variables are read once at startup. Changes require a restart; `SIGHUP` is not handled. Process environment wins over `.env`-file values; no other source is read.

Parse errors are silent: `PG_MAX_CONNS=fifty` runs with the default of `50` and never warns. The startup config dump (info-level log line `config_loaded`) is the only confirmation that your override took effect.

## gRPC listener

| Variable | Default | Notes |
|---|---|---|
| `GRPC_LISTEN` | `:9090` | Address the gRPC server binds to. |
| `TLS_CERT_FILE` | (unset) | Server's TLS certificate. |
| `TLS_KEY_FILE` | (unset) | Server's TLS key. |
| `TLS_CA_FILE` | (unset) | CA certificate for verifying client certs (mTLS). |

The three TLS variables must be set together or all left empty. A partial set is rejected at startup. The server does not support TLS without client-certificate verification; mTLS is mandatory whenever TLS is enabled. With all three empty, TLS is disabled (dev only).

## Postgres pool

| Variable | Default |
|---|---|
| `PG_MAX_CONNS` | `50` |
| `PG_MIN_CONNS` | `10` |
| `PG_MAX_CONN_IDLE` | `5m` |
| `PG_MAX_CONN_LIFETIME` | `1h` |
| `PG_HEALTHCHECK_PERIOD` | `30s` |
| `PG_QUERY_TIMEOUT_DEFAULT` | `2s` |

Durations use Go syntax (`5m`, `30s`, `1h`, `500ms`).

`PG_QUERY_TIMEOUT_DEFAULT` is the default per-query deadline at the storage layer, enforced via Go context. When the deadline fires, pgx aborts the in-flight query. Raise it for legitimately slow analytical reads; lower it and a runaway query can't pin a pool connection. Per-RPC override is not yet supported.

## Memcached

| Variable | Default | Notes |
|---|---|---|
| `MEMCACHED_ADDR` | `localhost:11211` | Comma-separated for multiple nodes. The `localhost` default fits dev only; production must point at a real memcached, otherwise every read falls through to Postgres with no warning. |
| `MEMCACHED_TIMEOUT` | `100ms` | Per-operation timeout. A node past this deadline causes a cache miss (the read falls through to Postgres) rather than a request error. Raise it to absorb GC pauses on the memcached box. |

## Cache

| Variable | Default | Notes |
|---|---|---|
| `CACHE_LRU_SIZE` | `1024` | Process-wide tier-0 LRU shared across every entity (keys are `entity/id`). Sits in front of memcached. |
| `CACHE_DEFAULT_TTL` | `10m` | Default TTL when an entity's `cache { ... }` block does not declare `ttl=`. |
| `CACHE_XFETCH_BETA` | `1.0` | Probabilistic early-refresh beta. `0` disables (TTL becomes a hard expiry); `1.0` is the published default; `>2` trades cache hits for fewer thundering-herd reloads. Tune only if you see synchronised expiry spikes in the cache-miss histogram. |

## Outbox worker

| Variable | Default | Notes |
|---|---|---|
| `OUTBOX_BATCH_SIZE` | `100` | Rows processed per worker tick. |
| `OUTBOX_DRAIN_INTERVAL` | `250ms` | Time between worker ticks. |
| `OUTBOX_ALERT_LAG` | `5m` | The worker emits a warning log line when the oldest unprocessed row is older than this. |
| `OUTBOX_POINTER_TTL` | `24h` | Memcached TTL on body-cache pointer keys. Must exceed your longest reasonable read latency under load; an expired pointer forces the next reader to refetch from Postgres. |

## Rate limiting

| Variable | Default | Notes |
|---|---|---|
| `RATE_LIMIT_DEFAULT_QPS` | `1000` | Token-bucket refill rate per caller without a `RATE_LIMIT_PER_CALLER` entry. |
| `RATE_LIMIT_BURST` | `200` | Maximum bucket capacity (the largest instantaneous burst allowed before throttling). A caller out of tokens gets `RESOURCE_EXHAUSTED`. |
| `RATE_LIMIT_PER_CALLER` | (unset) | Comma-separated `caller=qps` overrides. The self-host compose bundle seeds this with `atlantis-console=${CONSOLE_RATE_LIMIT_QPS:-5000}`; setting it via env replaces the seed entirely. |
| `RATE_LIMIT_SATURATION_CUTOFF` | `0.80` | Pool-saturation threshold. When pgxpool `AcquiredConns/MaxConns` crosses this, the server returns `RESOURCE_EXHAUSTED` on low-priority RPCs (today hard-coded as method names starting with `List` or `Search`; CRUD and Get never shed). Set to `0` to disable shedding entirely — Postgres then becomes your only backpressure. |

`RATE_LIMIT_PER_CALLER` format: `caller1=qps1,caller2=qps2`. Whitespace around tokens is trimmed. Pairs where the QPS does not parse as a positive integer, or where the `caller=` form is malformed, are silently dropped. Check startup logs to confirm the parsed map.

## Migrations

| Variable | Default | Notes |
|---|---|---|
| `AUTO_MIGRATE` | `false` | Apply pending migrations on boot. |
| `MIGRATIONS_DIR` | `migrations` | Directory passed to the bundled migrate runner. Resolved relative to the server's working directory. |

Set `AUTO_MIGRATE=false` in production. Boot-time migrations race rolling restarts: golang-migrate serializes on a Postgres advisory lock, but a losing replica crash-loops until the leader finishes — visible to your orchestrator as a flapping pod.

## Admin RPC gating

`ATL_ALLOW_APPLY_MUTATION` selects the schema-change flow. Default (`true`) is the per-caller-CI flow: callers run `tide apply` against the server and the server runs the DDL + IR write under an advisory lock. Set to `false` only when a regulator requires literal SQL review on a deployment-repo PR before any database change (SOX, HIPAA, PCI). The plan and pull RPCs remain available regardless. See [schema flow](../architecture/schema-flow.md) for the two flows in full.

Three independent gates grant mutation permission:

- `ATL_ALLOW_APPLY_MUTATION=true` — wildcard grant for any authenticated caller.
- `ATL_MUTATION_ALLOWED_CALLERS` — per-CN allowlist, comma-separated. Use it in regulated deployments to scope mutations to specific CI cert CNs.
- `caller_identities.can_mutate=true` — runtime per-caller flag (set via the console) so operators can grant mutation permission without an env-var change.

| Variable | Default | Notes |
|---|---|---|
| `ATL_ALLOW_APPLY_MUTATION` | `true` | Gates the `ApplyMigration` RPC. Default flow accepts mutations from any authenticated caller; the diff classifier and per-caller cert identity stop one caller from breaking another. Set to `false` for the regulated opt-in. |
| `ATL_MUTATION_ALLOWED_CALLERS` | (empty) | Comma-separated CN allowlist. Empty = no per-CN exceptions. |
| `ATL_OPERATOR_ALLOWED_CALLERS` | (empty) | Operator-only RPCs (`RevokeCaller`, `RollbackSchema`, `AdoptBaseline`). Empty falls back to `ATL_ALLOW_APPLY_MUTATION`. The self-host compose bundle defaults this to `atlantis-console` so operator actions from the console work without an extra env step. |

## Schema mirror (dev only)

These variables enable the local-development workflow where the server mirrors caller-submitted `.atl` files to disk so a file watcher can react to schema changes. Both must be `false` in production.

| Variable | Default | Notes |
|---|---|---|
| `ATL_MIRROR_SCHEMA` | `false` | When `true`, the server writes each successful `ApplyMigration` submission to `ATL_MIRROR_DIR`, partitioned by caller. |
| `ATL_MIRROR_DIR` | `schema` | Destination for mirrored caller files. Ignored when `ATL_MIRROR_SCHEMA=false`. |

## Console BFF

Read by `cmd/console`, not the Atlantis server. The self-host compose bundle wires these through `${VAR:-default}` substitutions in `docker-compose.self-host.yml`.

| Variable | Default | Notes |
|---|---|---|
| `CONSOLE_LISTEN` | `:3000` | Bind address for the BFF + SPA. |
| `CONSOLE_PG_URL` | (unset; required) | Connection string for the BFF's audit / session tables. The bundle points this at the same Postgres instance, separate schema. |
| `CONSOLE_SESSION_SECRET` | (unset; required, ≥32 chars) | HMAC key for session cookies. Console refuses to start below 32 chars, so `changeme` placeholders trip a fatal startup error — set this before first boot. |
| `CONSOLE_COOKIE_SECURE` | `false` | Sets the `Secure` flag on session cookies. Default false so `http://localhost` works for first boot; flip to `true` once a TLS terminator (reverse proxy, LB) sits in front. |
| `CONSOLE_AUDIT_RETENTION_DAYS` | `365` | Audit-row retention. Covers the typical SOC 2 audit window and PCI DSS §10.5.1's 12-month online minimum. HIPAA = 2190 (6 years); SOX = 2555 (7 years). `0` keeps every partition forever. |
| `ATL_ENDPOINT` | `localhost:9090` | atlantis-server endpoint the BFF dials over mTLS. |
| `ATL_TLS_CERT`, `ATL_TLS_KEY`, `ATL_TLS_CA` | (unset) | Client cert / key / CA for the BFF's mTLS connection to atlantis-server. |
| `ATL_HEALTH_LISTEN` | `localhost:8081` | atlantis-server HTTP health endpoint the BFF surfaces on the console's Health page. |
| `ATL_SIGNER_ADDR` | (unset) | Signer HTTP endpoint for cert issuance from the console's Callers page. |
| `GITHUB_TOKEN` | (unset) | Fine-grained PAT for the "Open PR" button on the Schema page. Needs `contents:write` + `pull_requests:write` on each caller repo. Unset disables the button (preview still works). |
| `SANDBOX_PER_USER_LIMIT` | `3` | Maximum concurrent sandboxes per authenticated user. A boot beyond this returns HTTP `429`. The limit also caps fork count — forking N children requires `N + parent` headroom. |
| `SANDBOX_TTL` | `30m` | Idle window after which the BFF's janitor evicts a sandbox. Go duration syntax. Set lower (`10s`) for CI; higher (`2h`) for long agent loops. |

The 256 MiB cap on `PUT /api/sandbox/{id}/snapshot` is a compile-time constant, not configurable. See [Sandbox HTTP API](sandbox-api.md#limits).

## Observability

| Variable | Default | Notes |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (unset) | OTLP gRPC collector endpoint (e.g. `otel-collector:4317`). Empty disables OTel export; Prometheus metrics on `:8081/metrics` and structured logs are unaffected. |

## Logging

| Variable | Default | Notes |
|---|---|---|
| `LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. |

## Parsing

Booleans accept `1`, `true`, `yes`, `on` (case-insensitive) for true and `0`, `false`, `no`, `off` for false.

All typed variables (int, float, duration, bool) silently fall back to their default on parse error — see [Reloading and precedence](#reloading-and-precedence) above for how to verify your value actually took effect.

## Shutdown

The server installs `SIGINT` / `SIGTERM` handlers that cancel a top-level context and call gRPC `GracefulStop`. Shutdown blocks until in-flight RPCs return. Supervisors should issue `SIGKILL` after their own deadline if a stuck RPC prevents exit.

## Example: local development

```
PG_URL=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable
MEMCACHED_ADDR=localhost:11211
GRPC_LISTEN=:9090
AUTO_MIGRATE=true
ATL_MIRROR_SCHEMA=true
ATL_ALLOW_APPLY_MUTATION=true
LOG_LEVEL=debug
```

## Example: production (default flow)

```
PG_URL=postgres://atlantis@db.internal:5432/atlantis?sslmode=require
MEMCACHED_ADDR=memcache-0.internal:11211,memcache-1.internal:11211
GRPC_LISTEN=:9090
TLS_CERT_FILE=/etc/atlantis/tls.crt
TLS_KEY_FILE=/etc/atlantis/tls.key
TLS_CA_FILE=/etc/atlantis/ca.crt
AUTO_MIGRATE=false
ATL_MIRROR_SCHEMA=false
ATL_ALLOW_APPLY_MUTATION=true
LOG_LEVEL=info
```

## Example: production (regulated opt-in)

For SOX, HIPAA, or PCI workloads that require literal SQL review before any database change:

```
# ...same as above, except:
ATL_ALLOW_APPLY_MUTATION=false
ATL_MUTATION_ALLOWED_CALLERS=ci.deploy.internal   # optional: tighten further
```
