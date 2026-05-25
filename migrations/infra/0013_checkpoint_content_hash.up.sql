-- Content hash on ir_checkpoint enables compare-and-swap writes:
-- "apply only if the current state matches what I planned against."
-- The hash is sha256 of the canonical IR JSON — same content always
-- produces the same identity, independent of environment or ordering.

ALTER TABLE atlantis.ir_checkpoint
  ADD COLUMN IF NOT EXISTS content_hash TEXT NOT NULL DEFAULT '';

-- Backfill the existing row so the CAS check works immediately after
-- migration without requiring a fresh tide apply.
UPDATE atlantis.ir_checkpoint
SET content_hash = encode(sha256(convert_to(ir::text, 'UTF8')), 'hex')
WHERE id = 1 AND content_hash = '';

-- Trigger-based NOTIFY ensures every code path that writes
-- ir_checkpoint (ApplyMigration, AdoptBaseline, BeginBackfillPlan,
-- RollbackSchema) fires a notification without any Go-side changes.
-- Follows the same pattern as outbox (0000) and jobs (0006) triggers.
-- Postgres delivers NOTIFY only after the enclosing transaction
-- commits, so the new row is guaranteed visible when listeners read it.
CREATE OR REPLACE FUNCTION atlantis.notify_schema_changed() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('atl_schema_changed', NEW.content_hash);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_schema_changed ON atlantis.ir_checkpoint;
CREATE TRIGGER trg_schema_changed
    AFTER INSERT OR UPDATE ON atlantis.ir_checkpoint
    FOR EACH ROW EXECUTE FUNCTION atlantis.notify_schema_changed();
