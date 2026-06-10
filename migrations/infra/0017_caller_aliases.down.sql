DROP INDEX IF EXISTS atlantis.caller_identities_aliases_idx;
ALTER TABLE atlantis.caller_identities DROP COLUMN IF EXISTS aliases;
