-- Outbox dead-letter table + last_error_at column.
--
-- last_error_at on the main table feeds the exponential-backoff filter
-- in claimInTx so a recently-failed row isn't hammered every drain tick.
--
-- cache_invalidations_dead receives rows whose retry count meets the
-- worker's MaxAttempts threshold. Moving (not silently dropping) keeps
-- the main outbox small while preserving forensic evidence for the
-- operator triaging a poison row.

ALTER TABLE atlantis.cache_invalidations
    ADD COLUMN IF NOT EXISTS last_error_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS atlantis.cache_invalidations_dead (
    id            BIGINT      PRIMARY KEY,
    entity        TEXT        NOT NULL,
    row_id        TEXT        NOT NULL,
    new_version   BIGINT      NOT NULL,
    kind          TEXT        NOT NULL,
    enqueued_at   TIMESTAMPTZ NOT NULL,
    attempts      INT         NOT NULL,
    last_error    TEXT,
    last_error_at TIMESTAMPTZ,
    moved_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS cache_invalidations_dead_moved_at_idx
    ON atlantis.cache_invalidations_dead (moved_at);
