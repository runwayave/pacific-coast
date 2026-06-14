# Declarative jobs

After this recipe you'll have a typed background job declared in `.atl`, a Go handler registered at server startup, and a working submit-claim-complete loop running against your local atlantis.

Prereqs:

- atlantis running locally (`tidectl dev` or a manual `go build + exec`).
- `ATL_JOBS_WORKER_ENABLED=true` in the server's environment.
- `buf` and `go` (1.25+) on `$PATH`.

## 1. Declare the job in `.atl`

In your caller repo (e.g., `backend/internal/directory/schema.atl`):

```atl
job ImportContacts in directory {
  args {
    account_id      varchar(64) not null
    import_strategy varchar(20) not null default "skip"
  }
  retries  3
  timeout  30m
  queue    "contacts"
}
```

- `args` uses the same type grammar as entity fields (varchar, int, jsonb, arrays, etc.).
- `schedule "cron-spec"` adds periodic invocation if you want the job to fire on a timer.
- `visible_to "directory"` restricts which callers can submit.

## 2. Run codegen

```bash
tidectl codegen --workspace=atlantis.dev.yaml
```

This emits `gen/go/server/directory/jobs.go` (Go package `directory`) with:

- `ImportContactsArgs` struct (typed, json-tagged). Field names are PascalCase in Go (the snake_case names in `.atl` are converted automatically).
- `ImportContactsHandler` interface (`Handle(ctx, args) error`).
- `ImportContactsJobName` const (`"directory.ImportContacts"`).
- `RegisterImportContacts(reg, handler)` helper.

## 3. Implement the handler

In your server code:

```go
type importContactsHandler struct {
    crm *crmapi.Client
}

func (h *importContactsHandler) Handle(ctx context.Context, args directory.ImportContactsArgs) error {
    // Checkpoint(ctx, progressPercent, message) — reports progress
    // visible in `tide job status`. Best-effort; does not fail the job.
    jobs.Checkpoint(ctx, 10, "fetching contacts")

    contacts, err := h.crm.FetchContacts(ctx, args.AccountId)
    if err != nil {
        return err // worker retries up to the declared retries limit, then DLQs
    }

    jobs.Checkpoint(ctx, 80, "importing contacts")
    // ... import logic ...

    return nil
}
```

`crmapi` here stands in for your upstream provider's Go client — substitute your own.

## 4. Register at server startup

In `cmd/server/main.go` (or your fork's equivalent):

```go
reg := jobs.NewRegistry()
directory.RegisterImportContacts(reg, &importContactsHandler{crm: crmClient})
// pass reg to the worker (see "In-process worker pattern" below)
```

## 5. Apply the schema

```bash
tide apply
```

This writes the job's runtime config (retries, timeout, queue) to the database so `SubmitJob` can look it up at submit time.

## 6. Submit a job

Use the generated `Args` struct for type safety, then submit via the admin RPC:

```go
args := directory.ImportContactsArgs{
    AccountId:      "acct_123",
    ImportStrategy: "replace",
}
argsJSON, _ := json.Marshal(args)
client.SubmitJob(ctx, admin.SubmitJobRequest{
    JobName: directory.ImportContactsJobName,
    Args:    argsJSON,
})
```

Or from the CLI (operator ad-hoc):

```bash
tide job submit directory.ImportContacts --args='{"account_id":"acct_123","import_strategy":"replace"}'
```

## 7. Monitor

```bash
tide job status <job-id>
tide job dead --job-name=directory.ImportContacts
tide job retry <dead-job-id>
```

## Enqueue from a procedure

Jobs can be enqueued atomically with a write transaction:

```atl
procedure ConnectAccount for directory.Account {
  input { account_id: varchar(64) }
  steps {
    update Account set status = "connected" where account_id = $account_id
    enqueue directory.ImportContacts(account_id: $account_id, import_strategy: "replace")
  }
}
```

The job row shares the procedure's tx. If the procedure rolls back, the job is never enqueued.

This declares a **new** procedure, so its gRPC method registers only on the next rolling server restart — `tide apply` records the declaration but can't add the method to a running server. (Editing an existing procedure's steps hot-reloads with no restart.)

## In-process worker pattern

The job handler runs inside your application binary, not inside the atlantis server. See [Jobs and workflows](../concepts/jobs-and-workflows.md) for the conceptual model. In short: atlantis stores jobs and workers pull them via `FOR UPDATE SKIP LOCKED`; the handler code runs in the app process that owns the business logic.

```go
package main

import (
    "context"
    "log/slog"
    "net/http"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/rachitkumar205/atlantis/clients/go/jobs"
    "gen/go/server/directory"
)

func main() {
    ctx := context.Background()
    crmClient := crmapi.NewClient(/* ... */)

    // Connect to the atlantis database.
    pool, _ := pgxpool.New(ctx, pgURL)

    // Build the job registry and register handlers.
    registry := jobs.NewRegistry()
    directory.RegisterImportContacts(registry, &importContactsHandler{crm: crmClient})

    // Start the worker. It polls the atlantis job queue,
    // claims rows, and calls the matching handler.
    w := jobs.NewWorker(pool, registry, "contacts", jobs.Config{
        Schema:        "atlantis",
        DrainInterval: time.Second,
        BatchSize:     10,
        Logger:        slog.Default(),
    })
    go w.Run(ctx)

    // Start the HTTP server in the same binary.
    http.ListenAndServe(":8080", router)
}
```

If a handler returns an error, the worker marks the job for retry (up to the declared `retries` limit) or moves it to the dead-letter queue. Scale workers by scaling app replicas — multiple workers on the same queue coordinate via `SKIP LOCKED`.

## Common errors

- `unknown job "directory.ImportContacts"` — run `tide apply` to record the declaration into the IR checkpoint.
- `no handler registered for directory.ImportContacts` — call `RegisterImportContacts(reg, handler)` at server startup. The worker retries until a pod with the handler claims the row.
- `caller "X" is not allowed to submit` — the job declares `visible_to "Y"`. Either submit from the right caller or update the visibility.

## Related

- [Jobs and workflows](../concepts/jobs-and-workflows.md) — how the runtime works under the hood
- [Row-level TTL](row-ttl.md) — automatic expiry using the job runtime's built-in sweeper
- [Local development](local-development.md) — running atlantis locally with `tidectl dev`
