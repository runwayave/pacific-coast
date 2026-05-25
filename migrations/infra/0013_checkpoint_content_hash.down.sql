DROP TRIGGER IF EXISTS trg_schema_changed ON atlantis.ir_checkpoint;
DROP FUNCTION IF EXISTS atlantis.notify_schema_changed();
ALTER TABLE atlantis.ir_checkpoint DROP COLUMN IF EXISTS content_hash;
