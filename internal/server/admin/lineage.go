package admin

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/codegen"
)

// updateEntityLineage materializes per-entity and per-field blame rows
// in atlantis.entity_lineage. Each schema version creates or touches
// rows here so ownership queries are O(1) against the materialized
// table rather than requiring a full scan of schema_versions.
//
// A nil diff (seed/adopt with no prior state) is a no-op.
func updateEntityLineage(ctx context.Context, tx pgx.Tx, version int64, caller string, d *codegen.Diff) error {
	if d == nil {
		return nil
	}

	allChanges := make([]codegen.Change, 0, len(d.Additive)+len(d.BackfillRequired)+len(d.Breaking))
	allChanges = append(allChanges, d.Additive...)
	allChanges = append(allChanges, d.BackfillRequired...)
	allChanges = append(allChanges, d.Breaking...)

	for _, ch := range allChanges {
		switch ch.Kind {
		case codegen.KindEntityAdded:
			// Insert entity-level row (field_name = '').
			// ON CONFLICT handles re-add after a previous removal.
			if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.entity_lineage
    (entity_id, field_name, introduced_by, introduced_at, last_modified_by, last_modified_at, removed_at)
VALUES ($1, '', $2, $3, $2, $3, NULL)
ON CONFLICT (entity_id, field_name) DO UPDATE SET
    removed_at = NULL,
    last_modified_by = EXCLUDED.last_modified_by,
    last_modified_at = EXCLUDED.last_modified_at`,
				ch.EntityID, caller, version); err != nil {
				return err
			}

		case codegen.KindFieldAdded:
			if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.entity_lineage
    (entity_id, field_name, introduced_by, introduced_at, last_modified_by, last_modified_at, removed_at)
VALUES ($1, $2, $3, $4, $3, $4, NULL)
ON CONFLICT (entity_id, field_name) DO UPDATE SET
    removed_at = NULL,
    last_modified_by = EXCLUDED.last_modified_by,
    last_modified_at = EXCLUDED.last_modified_at`,
				ch.EntityID, ch.Field, caller, version); err != nil {
				return err
			}

		case codegen.KindEntityRemoved:
			if _, err := tx.Exec(ctx, `
UPDATE atlantis.entity_lineage
SET removed_at = $1, last_modified_by = $2, last_modified_at = $1
WHERE entity_id = $3 AND removed_at IS NULL`,
				version, caller, ch.EntityID); err != nil {
				return err
			}

		case codegen.KindFieldRemoved:
			if _, err := tx.Exec(ctx, `
UPDATE atlantis.entity_lineage
SET removed_at = $1, last_modified_by = $2, last_modified_at = $1
WHERE entity_id = $3 AND field_name = $4`,
				version, caller, ch.EntityID, ch.Field); err != nil {
				return err
			}

		default:
			// Any field modification (type changed, not-null tightened,
			// default changed, unique added/removed, reference changed,
			// serial added/removed, backfill changed, etc.): update
			// last_modified_by/at for the specific field row.
			if ch.Field != "" {
				if _, err := tx.Exec(ctx, `
UPDATE atlantis.entity_lineage
SET last_modified_by = $1, last_modified_at = $2
WHERE entity_id = $3 AND field_name = $4`,
					caller, version, ch.EntityID, ch.Field); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
