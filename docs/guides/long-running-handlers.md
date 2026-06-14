# Long-running handlers

After this guide you'll know the idempotency contract that keeps re-dispatched handlers safe, when to call `jobs.Checkpoint`, and when to override the lease window with the `heartbeat` modifier. Skip section 1 only if your handler runs in well under a minute.

## 1. Idempotency

atlantis dispatches each row to exactly one worker at a time. If the worker crashes or loses its lease, the row is re-claimed and the same arguments run again — possibly more than twice inside the `retries` budget.

**Your handler MUST be idempotent.** Run it twice with the same arguments and the observable side-effects should be the same as running it once.

- External calls (a payment charge, an object-store upload, an email send) need an idempotency key the external system honors, or a read-then-write against its state.
- Database writes use `ON CONFLICT DO NOTHING` / `ON CONFLICT DO UPDATE`, or check a per-logical-run state table first.

The standard pattern is a per-run state table. Each handler invocation opens with:

```sql
CREATE TABLE contact_import_state (
  account_id  varchar(64) PRIMARY KEY,
  status      text NOT NULL,     -- 'running' | 'complete' | 'failed'
  next_cursor text,
  started_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
```

```go
err := tx.QueryRow(ctx, `
    INSERT INTO contact_import_state (account_id, status)
    VALUES ($1, 'running')
    ON CONFLICT (account_id) DO UPDATE SET status = 'running'
    RETURNING next_cursor`, args.AccountId).Scan(&cursor)
```

The second invocation reads `next_cursor` and resumes; the work it re-does is overwriting the same destination rows, not duplicating them.

## 2. The lease

When a worker claims a row, atlantis sets `claimed_until = now() + heartbeat_budget`. The SDK auto-heartbeats at one third of that budget; each heartbeat re-extends `claimed_until`. As long as heartbeats keep landing, the lease never expires. The SDK installs the heartbeat goroutine automatically — handlers don't touch it.

The default budget is **2 minutes**. Most handlers complete inside that window and need no configuration.

Override per-job with the `heartbeat` modifier when a handler blocks on a single external call longer than the default — an upstream API page that takes 8 minutes, an ML training step that takes 20 minutes. Without the override the dispatcher will revoke and re-dispatch mid-call even though the worker is alive.

```atl
job ImportContacts in directory {
  args { account_id varchar(64) not null }
  retries   3
  timeout   30m
  heartbeat 10m
  queue     "contacts"
}
```

The override is symmetric: `heartbeat 30s` narrows the window so a dead worker is reclaimed in 30 seconds instead of 2 minutes. `heartbeat` is independent of `timeout` — `timeout` is the handler's wall-clock budget per attempt; `heartbeat` is the dispatcher's grace window before declaring the worker dead.

## 3. Checkpointing progress

The auto-heartbeat keeps the lease alive but says nothing about what the handler has accomplished. For that, call `jobs.Checkpoint`:

```go
jobs.Checkpoint(ctx, pct, msg)
```

- `pct` is 0–100; clamped server-side, so an out-of-range value won't error.
- `msg` is a short human-readable label, truncated server-side at 256 characters. Operators see it in `tide job status <id>` and on the console session detail.
- The return error is advisory; handlers typically `_ = jobs.Checkpoint(...)`.

Each call bumps the lease (same effect as a heartbeat) AND persists progress to the `progress_pct` / `progress_msg` / `progress_at` columns on `atlantis.jobs`. A handler that checkpoints every minute won't need an extended `heartbeat` modifier on top — the manual calls keep the lease fresh too.

**Call when:** each meaningful unit of work finishes (a page fetched, a thousand rows imported, an epoch trained) and right before a long blocking call.

**Don't call:** from a tight loop millions of iterations deep, or from a goroutine the handler has forked (see section 4).

For resume, store the cursor in your state table from section 1 — that's the source of truth. `progress_msg` is for operators reading a live job; relying on it for resume couples the handler to a column it was never meant to own.

## 4. Forking goroutines

```go
func (h *handler) Handle(ctx context.Context, args Args) error {
    go h.expensiveBackground(ctx, args)  // lost work
    return nil
}
```

When the handler returns, the worker reports the job complete. The forked goroutine is now running on its own — if the pod restarts, the work vanishes silently and the row is `complete` with nothing to show for it. No retry, no DLQ, no trace.

Do the work synchronously inside `Handle`, checkpoint as you go, and return only when the work is actually done. If the work is genuinely independent, declare it as a separate job and `enqueue` it from a procedure.

## 5. Worked example — contact import

A handler that imports an account's full contact list from an upstream CRM API. The API paginates at ~100 contacts per request; each request takes 1–3 seconds; a large account has hundreds of thousands of contacts. A typical run is 8–15 minutes; the tail blows past an hour.

### Schema

```atl
job ImportContacts in directory {
  args { account_id varchar(64) not null }
  retries   3
  timeout   2h
  heartbeat 10m
  queue     "contacts"
}
```

### State table

```sql
CREATE TABLE contact_import_state (
  account_id    varchar(64) PRIMARY KEY,
  status        text NOT NULL,         -- 'running' | 'complete' | 'failed'
  next_cursor   text,                  -- upstream API page cursor
  pages_done    int NOT NULL DEFAULT 0,
  contacts_done int NOT NULL DEFAULT 0,
  started_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
```

### Handler

```go
func (h *importContactsHandler) Handle(ctx context.Context, args directory.ImportContactsArgs) error {
    // 1. Idempotent open: resume cursor + counters from the state table.
    var state importState
    err := h.db.QueryRow(ctx, `
        INSERT INTO contact_import_state (account_id, status)
        VALUES ($1, 'running')
        ON CONFLICT (account_id) DO UPDATE SET status = 'running', updated_at = now()
        RETURNING next_cursor, pages_done, contacts_done`,
        args.AccountId,
    ).Scan(&state.cursor, &state.pages, &state.contacts)
    if err != nil {
        return fmt.Errorf("open import state: %w", err)
    }
    _ = jobs.Checkpoint(ctx, 0, fmt.Sprintf("resuming pages=%d contacts=%d", state.pages, state.contacts))

    // 2. Paginated fetch + write. Each iteration is one upstream page.
    for {
        page, err := h.crm.FetchContacts(ctx, args.AccountId, state.cursor)
        if err != nil {
            return fmt.Errorf("fetch page after cursor=%q: %w", state.cursor, err)
        }
        if err := h.writeContacts(ctx, args.AccountId, page.Contacts); err != nil {
            return fmt.Errorf("write contacts: %w", err)
        }

        // Persist progress in our state table (source of truth for resume).
        state.cursor = page.NextCursor
        state.pages++
        state.contacts += len(page.Contacts)
        _, err = h.db.Exec(ctx, `
            UPDATE contact_import_state
               SET next_cursor = $1, pages_done = $2, contacts_done = $3, updated_at = now()
             WHERE account_id = $4`,
            state.cursor, state.pages, state.contacts, args.AccountId)
        if err != nil {
            return fmt.Errorf("checkpoint state: %w", err)
        }

        // 3. Mirror to atlantis for `tide job status` and the console.
        _ = jobs.Checkpoint(ctx,
            percentDone(state.contacts, page.TotalEstimate),
            fmt.Sprintf("page=%d contacts=%d", state.pages, state.contacts))

        if page.NextCursor == "" {
            break
        }
    }

    // 4. Mark complete. Idempotent: re-running is a no-op.
    _, err = h.db.Exec(ctx, `
        UPDATE contact_import_state
           SET status = 'complete', updated_at = now()
         WHERE account_id = $1`, args.AccountId)
    return err
}
```

A crash mid-import becomes a non-event: the lease expires after 10 minutes, the row is re-dispatched, the new handler reads `next_cursor` and picks up at the same page. `tide job status <id>` shows live progress. `timeout 2h` is the hard ceiling; if the upstream API gets unresponsive for two hours straight, the per-attempt deadline kills the handler and the second attempt resumes from the saved cursor.

## Related

- [Declarative jobs](declarative-jobs.md) — end-to-end recipe for declaring a job and registering a handler.
- [Jobs and workflows](../concepts/jobs-and-workflows.md) — runtime model, including the lease, DLQ, and workflow compensations.
- [DSL grammar reference](../reference/dsl-grammar.md) — full grammar for `job` modifiers.
