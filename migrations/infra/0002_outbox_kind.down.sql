-- Drop kind discriminator. Queued rows are NOT dropped — only the column.
-- Any pending generation_bump rows will be reinterpreted by the worker as
-- 'invalidation'-shaped, causing a malformed per-row invalidation
-- (row_id was a placeholder for bumps). Drain the outbox to empty before
-- rolling back.

ALTER TABLE atlantis.cache_invalidations
    DROP CONSTRAINT IF EXISTS cache_invalidations_kind_check;

DROP INDEX IF EXISTS atlantis.cache_invalidations_kind_enqueued_idx;

ALTER TABLE atlantis.cache_invalidations
    DROP COLUMN IF EXISTS kind;
