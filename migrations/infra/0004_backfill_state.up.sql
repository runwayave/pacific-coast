-- Backfill state tables. Populated by admin.BeginBackfillPlan; drained
-- by the background backfill worker; queried by admin.GetBackfillStatus
-- and the tide CLI's `tide backfill status` command.
--
-- backfill_plan tracks the high-level apply state. post_sql is the SQL
-- the worker runs after Phase 2 completes to install the NOT NULL
-- constraints that were deferred from Phase 1. ir_checkpoint_hash is
-- captured at Phase 1 commit and validated at Phase 3 — if a concurrent
-- apply has shifted the checkpoint, Phase 3 refuses to run so the schema
-- can't end up in an undefined state.
--
-- backfill_field_state is one row per field being backfilled. The worker
-- claims rows with FOR UPDATE SKIP LOCKED so concurrent pods never
-- duplicate work on the same field. last_pk is the resume cursor — the
-- worker updates it inside the same tx as the chunked UPDATE so a crash
-- never leaves the cursor inconsistent with the data.

CREATE TABLE IF NOT EXISTS atlantis.backfill_plan (
    plan_hash             TEXT PRIMARY KEY,
    caller                TEXT        NOT NULL,
    status                TEXT        NOT NULL,
    post_sql              TEXT        NOT NULL,
    post_indexes_sql      TEXT        NOT NULL DEFAULT '',
    ir_checkpoint_hash    TEXT        NOT NULL,
    error_msg             TEXT,
    started_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at          TIMESTAMPTZ,
    CHECK (status IN ('phase2_running','phase3_running','complete','failed'))
);

CREATE TABLE IF NOT EXISTS atlantis.backfill_field_state (
    plan_hash      TEXT        NOT NULL REFERENCES atlantis.backfill_plan(plan_hash) ON DELETE CASCADE,
    entity_id      TEXT        NOT NULL,
    field          TEXT        NOT NULL,
    expression     TEXT        NOT NULL,
    pk_column      TEXT        NOT NULL,
    table_name     TEXT        NOT NULL,
    last_pk        TEXT,
    rows_processed BIGINT      NOT NULL DEFAULT 0,
    status         TEXT        NOT NULL DEFAULT 'pending',
    error_msg      TEXT,
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    PRIMARY KEY (plan_hash, entity_id, field),
    CHECK (status IN ('pending','running','complete','failed'))
);

-- Partial index for the worker's claim query. FOR UPDATE SKIP LOCKED
-- on a small predicated set is cheap.
CREATE INDEX IF NOT EXISTS backfill_field_state_pending_idx
    ON atlantis.backfill_field_state (plan_hash, status)
    WHERE status IN ('pending','running');

-- Index for caller-scoped lookups (`tide backfill status` without args).
CREATE INDEX IF NOT EXISTS backfill_plan_caller_started_idx
    ON atlantis.backfill_plan (caller, started_at DESC);
