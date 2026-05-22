// Package backfill is the Phase-2 runtime for `tide apply --backfill`.
// The package drains atlantis.backfill_field_state rows by running a
// chunked UPDATE template (the splicer) against the caller's table.
// When every field in a plan completes, the worker performs Phase 3 —
// SET NOT NULL + ir_checkpoint update — inside an advisory-locked tx.
package backfill

import (
	"fmt"
	"strings"
)

// ChunkSQL builds the parameterized chunked-UPDATE statement for one
// backfill field. Parameters at runtime:
//
//	$1 — last_pk (bigint; 0 for the initial chunk)
//	$2 — chunk size (int)
//
// The expression is embedded verbatim; sqlvalidate.ValidateBackfillExpression
// at PlanSchema time has already rejected subqueries, non-whitelisted
// function calls, and unknown column refs, so the only injection surface
// is fields the operator legitimately owns.
//
// The single-statement form (CTE + UPDATE + final SELECT) is what makes
// the UPDATE atomic with the cursor read — the runner reads max(pk) and
// rows_updated in the same query that did the work, so a pod crash either
// commits both or neither.
//
// Single-PK only in v1. The runner rejects composite-PK entities at
// BeginBackfillPlan time so this template never sees them.
func ChunkSQL(qualifiedTable, pkColumn, field, expression string) string {
	return fmt.Sprintf(`WITH chunk AS (
    SELECT %[2]s FROM %[1]s
    WHERE %[3]s IS NULL AND %[2]s > $1
    ORDER BY %[2]s
    LIMIT $2
),
updated AS (
    UPDATE %[1]s t
    SET %[3]s = %[4]s
    FROM chunk
    WHERE t.%[2]s = chunk.%[2]s
    RETURNING t.%[2]s
)
SELECT COALESCE(MAX(%[2]s), $1) AS new_last_pk, COUNT(*) AS rows_updated FROM updated`,
		qualifiedTable,
		quoteIdent(pkColumn),
		quoteIdent(field),
		expression,
	)
}

// quoteIdent mirrors codegen/sql.go's quoteIdent — double-quotes the
// identifier with embedded quote doubling. Defense-in-depth so a future
// grammar widening can't smuggle a reserved word through.
func quoteIdent(s string) string {
	if !strings.ContainsAny(s, `"`+"\n") {
		return `"` + s + `"`
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
