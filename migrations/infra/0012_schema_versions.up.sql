-- Versioned schema registry: every tide apply / adopt / rollback creates
-- an append-only version row with the full IR snapshot, structural diff,
-- and generated SQL. entity_lineage materializes per-field blame so
-- ownership queries are O(1).

CREATE TABLE IF NOT EXISTS atlantis.schema_versions (
    version         BIGSERIAL   PRIMARY KEY,
    caller          TEXT        NOT NULL,
    plan_class      TEXT        NOT NULL,
    diff            JSONB       NOT NULL DEFAULT '[]'::jsonb,
    up_sql          TEXT        NOT NULL DEFAULT '',
    down_sql        TEXT        NOT NULL DEFAULT '',
    ir_snapshot     JSONB       NOT NULL,
    ir_hash         TEXT        NOT NULL,
    plan_id         TEXT        NOT NULL DEFAULT '',
    parent_version  BIGINT      REFERENCES atlantis.schema_versions(version),
    event_type      TEXT        NOT NULL DEFAULT 'apply',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CHECK (event_type IN ('apply', 'rollback', 'adopt', 'seed', 'backfill_complete'))
);

CREATE INDEX IF NOT EXISTS schema_versions_caller_idx
    ON atlantis.schema_versions (caller, created_at DESC);

CREATE INDEX IF NOT EXISTS schema_versions_event_type_idx
    ON atlantis.schema_versions (event_type);

CREATE INDEX IF NOT EXISTS schema_versions_ir_hash_idx
    ON atlantis.schema_versions (ir_hash);

CREATE TABLE IF NOT EXISTS atlantis.entity_lineage (
    entity_id        TEXT   NOT NULL,
    field_name       TEXT   NOT NULL DEFAULT '',
    introduced_by    TEXT   NOT NULL,
    introduced_at    BIGINT NOT NULL REFERENCES atlantis.schema_versions(version),
    last_modified_by TEXT   NOT NULL,
    last_modified_at BIGINT NOT NULL REFERENCES atlantis.schema_versions(version),
    removed_at       BIGINT REFERENCES atlantis.schema_versions(version),
    PRIMARY KEY (entity_id, field_name)
);

CREATE INDEX IF NOT EXISTS entity_lineage_caller_idx
    ON atlantis.entity_lineage (introduced_by);

CREATE INDEX IF NOT EXISTS entity_lineage_entity_idx
    ON atlantis.entity_lineage (entity_id);

-- Seed from existing checkpoint so a running system gets a baseline
-- version (version=1) without requiring a manual re-apply. Conditional
-- on schema_versions being empty — never re-seeds, so a TRUNCATE +
-- re-up would advance the BIGSERIAL past 1 and orphan any callers'
-- snapshotted parent_version references.
INSERT INTO atlantis.schema_versions
    (caller, plan_class, diff, ir_snapshot, ir_hash, event_type)
SELECT
    'system',
    'seed',
    '{"additive":[],"backfill_required":[],"breaking":[]}'::jsonb,
    c.ir,
    encode(sha256(convert_to(c.ir::text, 'UTF8')), 'hex'),
    'seed'
FROM atlantis.ir_checkpoint c
WHERE c.id = 1
  AND NOT EXISTS (SELECT 1 FROM atlantis.schema_versions LIMIT 1);
