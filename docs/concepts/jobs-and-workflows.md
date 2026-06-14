# Jobs and workflows

Jobs are typed background-work declarations. You declare the args a handler receives, how many times to retry on failure, and how long each attempt can run. atlantis generates a typed Go SDK from the declaration, runs an in-Postgres worker pool, and routes claimed rows to the handler you register at server startup.

```atl
job ImportContacts in directory {
  args {
    account_id      varchar(64) not null
    import_strategy varchar(20) not null default "skip"
  }
  retries  3
  timeout  30m
  queue    "contacts"
  schedule "0 */15 * * *"
}
```

Workflows are multi-step orchestrations built on top of jobs. Each step runs a declared job; steps execute in declaration order. If a step fails after exhausting retries, compensations for prior steps run in reverse to undo the work.

```atl
workflow OnboardAccount in directory {
  state {
    account_id varchar(64) not null
    provider   varchar(255) not null
  }
  step import_contacts {
    job ImportContacts
    args { account_id: $account_id, import_strategy: "replace" }
  }
  compensate import_contacts {
    job PurgeContacts
    args { account_id: $account_id }
  }
}
```

## How jobs run

1. A caller submits a job via `SubmitJob` (with the generated `ImportContactsArgs` struct marshaled to JSON) or a procedure's `enqueue` step. The server INSERTs a row into `atlantis.jobs`.
2. The worker pool wakes on LISTEN/NOTIFY (or a 1s ticker), claims the row with `FOR UPDATE SKIP LOCKED`, and dispatches to the registered handler.
3. The handler runs under a per-attempt timeout (`context.WithTimeout`). A heartbeat extends the row's lease to prevent duplicate claims.
4. On success the row moves to `complete`. On failure the row retries up to `max_retries`, then moves to `atlantis.jobs_dead` (the DLQ).

## Handler registration

Handlers live in your Go binary. atlantis codegen emits a typed interface per job; you implement it and register at server startup:

```go
directory.RegisterImportContacts(reg, &importContactsHandler{crm: crmClient})
```

For non-Go handlers, `RegisterRemote(reg, jobID, addr)` dispatches over gRPC to an external service. Any language that can serve a JSON-envelope gRPC endpoint can act as a handler.

## Submission paths

| Path | When to use |
|---|---|
| Typed Go SDK | The 95% case. Marshal the generated `Args` struct + call `SubmitJob`. |
| Procedure `enqueue` step | Atomic with a write transaction. The job row shares the procedure's tx. |
| `schedule "cron-spec"` | Periodic invocation. The scheduler component INSERTs rows on the cron. |
| `tide job submit` CLI | Operator ad-hoc triage. Not the standard path. |

## Runtime modifiers

| Modifier | Default | Effect |
|---|---|---|
| `retries N` | 0 | Max retry count before DLQ. |
| `timeout 30m` | 30m | Per-attempt deadline. `timeout none` disables the deadline entirely. |
| `heartbeat 10m` | 2m | Per-attempt lease window. Widen for handlers that block on a single external call longer than the worker's default; narrow to fail fast on quick jobs. |
| `queue "name"` | `"default"` | Named queue for partitioning worker pools. |
| `schedule "cron"` | (none) | Cron spec for periodic invocation. |
| `visible_to "caller"` | (any) | RBAC: only the named caller can submit. `"*"` for any. |

## Checkpointing

Long-running handlers call `jobs.Checkpoint(ctx, pct, msg)` to report progress. Each call bumps the row's lease (same path as an auto-heartbeat) AND persists `progress_pct` / `progress_msg` so the operator sees live status in `tide job status` and the console session detail. `Checkpoint` returns an error, but handlers typically discard it — the worker never fails a claim because of a checkpoint write failure, so treating checkpoint errors as advisory keeps the handler focused on its real work.

See [Long-running handlers](../guides/long-running-handlers.md) for the full handler contract — idempotency, the heartbeat / checkpoint distinction, resume-from-progress, and a worked contact-import example.

## Distributed tracing

When the caller has an active OpenTelemetry span, `SubmitJob` captures the W3C traceparent into the row's `trace_ctx` column. The worker resumes the trace on claim and starts a child span around `handler.Handle`, so the submit-side and worker-side spans stitch into one distributed trace in Jaeger / Tempo / Datadog.

## How workflows run

1. `StartWorkflow` inserts a `workflow_instances` row and enqueues the first step's job.
2. When that job completes, the workflow engine advances to the next step by enqueuing its job.
3. When the last step completes, the workflow is marked `complete`.
4. If any step's job fails (moves to DLQ), the engine runs compensations for prior completed steps in reverse order, then marks the workflow `failed`.

Compensations are themselves jobs. A compensation that fails moves the workflow to `failed` with diagnostic detail; the operator intervenes manually.

## Crash recovery

If a pod crashes mid-handler, the row's lease (`claimed_until`) expires. Another pod's worker claims the row on its next drain pass and retries the handler from scratch. The `attempts` counter tracks how many times the row has been claimed, so idempotent handlers are the documented contract — same as Asynq, Sidekiq, and Riverqueue.

## Related

- [Declarative jobs guide](../guides/declarative-jobs.md). Step-by-step recipe for declaring and running a job.
- [Custom queries and procedures](custom-queries-and-procedures.md). The synchronous counterpart to jobs.
- [DSL grammar reference](../reference/dsl-grammar.md). Full grammar including `job`, `workflow`, `enqueue`, `ttl_field`.
