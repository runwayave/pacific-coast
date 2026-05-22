-- Refuse to roll back while a backfill is mid-flight; orphaning rows
-- would leave a partially-backfilled column in production. Operator
-- must wait for the worker to finish (or mark the plan failed) before
-- dropping these tables.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM atlantis.backfill_plan
        WHERE status IN ('phase2_running','phase3_running')
    ) THEN
        RAISE EXCEPTION 'cannot drop backfill tables: in-flight backfill exists. Wait for `tide backfill status` to report complete or mark failed first.';
    END IF;
END$$;

DROP INDEX IF EXISTS atlantis.backfill_plan_caller_started_idx;
DROP INDEX IF EXISTS atlantis.backfill_field_state_pending_idx;
DROP TABLE IF EXISTS atlantis.backfill_field_state;
DROP TABLE IF EXISTS atlantis.backfill_plan;
