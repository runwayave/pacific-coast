DROP TABLE IF EXISTS atlantis.cache_invalidations_dead;
ALTER TABLE atlantis.cache_invalidations DROP COLUMN IF EXISTS last_error_at;
