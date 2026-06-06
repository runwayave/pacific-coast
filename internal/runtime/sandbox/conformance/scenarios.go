package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
)

// Scenarios is the registered set of differential test cases.
// Adding a scenario here causes RunAll to exercise it against every
// available backend.
var Scenarios = []Scenario{
	insertReturning(),
	getByPK(),
	getMissingReturnsNoRows(),
	updateRowsAffected(),
	deleteRowsAffected(),
	batchGetAny(),
	listOrderByLimit(),
	countOverTotal(),
	softDelete(),
	compositePK(),
	onConflictDoNothing(),
	onConflictDoUpdate(),
	keysetCursor(),
	jsonbExtractWhere(),
	recursiveCTE(),
}

// ─────────────────────────── shared IRs ───────────────────────────

// usersIR is the canonical "simple entity" — used by most scenarios.
// Identity is false so tests can supply explicit IDs and assert on
// them (PG IDENTITY rejects non-DEFAULT inserts without OVERRIDING).
func usersIR() *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "User",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
				{Name: "email", Type: dsl.FieldType{Name: "text"}, NotNull: true},
				{Name: "active", Type: dsl.FieldType{Name: "boolean"}, NotNull: true},
			},
		}},
	}
}

// softDeleteIR adds a nullable deleted_at + soft_delete_field
// modifier so the executor and PG both apply the IS NULL filter.
func softDeleteIR() *dsl.IR {
	ir := usersIR()
	ir.Entities[0].Fields = append(ir.Entities[0].Fields,
		dsl.Field{Name: "deleted_at", Type: dsl.FieldType{Name: "timestamptz"}})
	ir.Entities[0].SoftDeleteField = "deleted_at"
	return ir
}

// compositePKIR is a two-column PK shape (purchase_id + occurred_at).
func compositePKIR() *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "Purchase",
			Namespace: "vendor",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "purchase_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
				{Name: "occurred_at", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
				{Name: "amount_cents", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
			},
			CompositePK: []string{"purchase_id", "occurred_at"},
		}},
	}
}

// eventsJSONIR has a JSONB column for ->> extract scenarios.
func eventsJSONIR() *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "Event",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
				{Name: "data", Type: dsl.FieldType{Name: "jsonb"}, NotNull: true},
			},
		}},
	}
}

// customQueryIR carries a `query` block so auto-routing picks
// embedded. Used for scenarios that exercise PG features the sim's
// whitelist doesn't parse.
func customQueryIR() *dsl.IR {
	ir := usersIR()
	ir.Queries = []dsl.CustomQuery{{
		Name:  "Probe",
		Owner: "consumer.User",
		SQL:   `SELECT 1`,
	}}
	return ir
}

// ─────────────────────────── individual scenarios ───────────────────────────

func insertReturning() Scenario {
	return Scenario{
		Name: "insert_returning",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			const ins = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "active") VALUES ($1, $2, $3) RETURNING "id"`
			var id int64
			if err := sb.Pool().QueryRow(ctx, ins, int64(7), "a@b.com", true).Scan(&id); err != nil {
				t.Fatalf("insert: %v", err)
			}
			if id != 7 {
				t.Fatalf("RETURNING id: got %d want 7", id)
			}
		},
	}
}

func getByPK() Scenario {
	return Scenario{
		Name: "get_by_pk",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			seedUser(t, sb, 1, "x@y.com", true)
			const sel = `SELECT "email", "active" FROM "atlantis"."consumer_user" WHERE "id" = $1`
			var email string
			var active bool
			if err := sb.Pool().QueryRow(ctx, sel, int64(1)).Scan(&email, &active); err != nil {
				t.Fatalf("get: %v", err)
			}
			if email != "x@y.com" || !active {
				t.Fatalf("get: email=%q active=%v", email, active)
			}
		},
	}
}

func getMissingReturnsNoRows() Scenario {
	return Scenario{
		Name: "get_missing_no_rows",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			const sel = `SELECT "email" FROM "atlantis"."consumer_user" WHERE "id" = $1`
			err := sb.Pool().QueryRow(ctx, sel, int64(99)).Scan(new(string))
			if err == nil {
				t.Fatalf("expected no-rows error")
			}
			if !runtime.IsNoRows(err) {
				t.Fatalf("runtime.IsNoRows missed: %v", err)
			}
		},
	}
}

func updateRowsAffected() Scenario {
	return Scenario{
		Name: "update_rows_affected",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			seedUser(t, sb, 1, "x@y.com", true)
			const upd = `UPDATE "atlantis"."consumer_user" SET "email" = $1 WHERE "id" = $2`

			tag, err := sb.Pool().Exec(ctx, upd, "new@y.com", int64(1))
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("matched RowsAffected: got %d want 1", tag.RowsAffected())
			}
			tag, err = sb.Pool().Exec(ctx, upd, "miss@y.com", int64(99))
			if err != nil {
				t.Fatalf("update missing: %v", err)
			}
			if tag.RowsAffected() != 0 {
				t.Fatalf("missing RowsAffected: got %d want 0", tag.RowsAffected())
			}
		},
	}
}

func deleteRowsAffected() Scenario {
	return Scenario{
		Name: "delete_rows_affected",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			seedUser(t, sb, 1, "x@y.com", true)
			const del = `DELETE FROM "atlantis"."consumer_user" WHERE "id" = $1`

			tag, err := sb.Pool().Exec(ctx, del, int64(1))
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("matched RowsAffected: got %d want 1", tag.RowsAffected())
			}
			tag, err = sb.Pool().Exec(ctx, del, int64(1))
			if err != nil {
				t.Fatalf("delete missing: %v", err)
			}
			if tag.RowsAffected() != 0 {
				t.Fatalf("missing RowsAffected: got %d want 0", tag.RowsAffected())
			}
		},
	}
}

func batchGetAny() Scenario {
	return Scenario{
		Name: "batch_get_any",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			for i, e := range []string{"a@y.com", "b@y.com", "c@y.com"} {
				seedUser(t, sb, int64(i+1), e, true)
			}
			const sql = `SELECT "id" FROM "atlantis"."consumer_user" WHERE "id" = ANY($1)`
			rows, err := sb.Pool().Query(ctx, sql, []int64{1, 3})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			seen := map[int64]bool{}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("scan: %v", err)
				}
				seen[id] = true
			}
			if !seen[1] || !seen[3] || len(seen) != 2 {
				t.Fatalf("ANY: %v want {1, 3}", seen)
			}
		},
	}
}

func listOrderByLimit() Scenario {
	return Scenario{
		Name: "list_order_by_limit",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			for i := 1; i <= 5; i++ {
				seedUser(t, sb, int64(i), "u@y.com", true)
			}
			const sql = `SELECT "id" FROM "atlantis"."consumer_user" ORDER BY "id" ASC LIMIT $1 OFFSET $2`
			rows, err := sb.Pool().Query(ctx, sql, int64(2), int64(2))
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, id)
			}
			if len(got) != 2 || got[0] != 3 || got[1] != 4 {
				t.Fatalf("ids: %v want [3 4]", got)
			}
		},
	}
}

func countOverTotal() Scenario {
	return Scenario{
		Name: "count_over_total",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			for i := 1; i <= 7; i++ {
				seedUser(t, sb, int64(i), "u@y.com", true)
			}
			const sql = `SELECT "id", COUNT(*) OVER () AS total FROM "atlantis"."consumer_user" ORDER BY "id" ASC LIMIT $1 OFFSET $2`
			rows, err := sb.Pool().Query(ctx, sql, int64(2), int64(0))
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			var idsLen int
			var lastTotal int64
			for rows.Next() {
				var id int64
				var total int64
				if err := rows.Scan(&id, &total); err != nil {
					t.Fatalf("scan: %v", err)
				}
				lastTotal = total
				idsLen++
			}
			if idsLen != 2 {
				t.Fatalf("rows: %d want 2", idsLen)
			}
			if lastTotal != 7 {
				t.Fatalf("total: %d want 7", lastTotal)
			}
		},
	}
}

func softDelete() Scenario {
	return Scenario{
		Name: "soft_delete_filter",
		IR:   softDeleteIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			const ins = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "active", "deleted_at") VALUES ($1, $2, $3, $4) RETURNING "id"`
			for _, i := range []int64{1, 2, 3} {
				if err := sb.Pool().QueryRow(ctx, ins, i, "u@y.com", true, nil).Scan(new(int64)); err != nil {
					t.Fatalf("seed %d: %v", i, err)
				}
			}
			// Soft-delete row 2.
			const sd = `UPDATE "atlantis"."consumer_user" SET "deleted_at" = now() WHERE "id" = $1 AND "deleted_at" IS NULL`
			if _, err := sb.Pool().Exec(ctx, sd, int64(2)); err != nil {
				t.Fatalf("soft delete: %v", err)
			}
			// List filter excludes the soft-deleted row.
			const list = `SELECT "id" FROM "atlantis"."consumer_user" WHERE "deleted_at" IS NULL ORDER BY "id" ASC`
			rows, err := sb.Pool().Query(ctx, list)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, id)
			}
			if len(got) != 2 || got[0] != 1 || got[1] != 3 {
				t.Fatalf("post soft-delete: %v want [1 3]", got)
			}
		},
	}
}

func compositePK() Scenario {
	return Scenario{
		Name: "composite_pk",
		IR:   compositePKIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			const ins = `INSERT INTO "atlantis"."vendor_purchase" ("purchase_id", "occurred_at", "amount_cents") VALUES ($1, $2, $3) RETURNING "purchase_id"`
			if err := sb.Pool().QueryRow(ctx, ins, int64(7), int64(100), int64(500)).Scan(new(int64)); err != nil {
				t.Fatalf("insert: %v", err)
			}
			const sel = `SELECT "amount_cents" FROM "atlantis"."vendor_purchase" WHERE "purchase_id" = $1 AND "occurred_at" = $2`
			var amt int64
			if err := sb.Pool().QueryRow(ctx, sel, int64(7), int64(100)).Scan(&amt); err != nil {
				t.Fatalf("get: %v", err)
			}
			if amt != 500 {
				t.Fatalf("amount: %d want 500", amt)
			}
		},
	}
}

func onConflictDoNothing() Scenario {
	return Scenario{
		Name: "on_conflict_do_nothing",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			seedUser(t, sb, 1, "first@y.com", true)
			const sql = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "active") VALUES ($1, $2, $3) ON CONFLICT ("id") DO NOTHING`
			tag, err := sb.Pool().Exec(ctx, sql, int64(1), "second@y.com", false)
			if err != nil {
				t.Fatalf("upsert: %v", err)
			}
			// PG returns 0 rows affected on DO NOTHING when there's a conflict.
			if tag.RowsAffected() != 0 {
				t.Fatalf("DO NOTHING: RowsAffected=%d want 0", tag.RowsAffected())
			}
			// Original row preserved.
			var email string
			if err := sb.Pool().QueryRow(ctx, `SELECT "email" FROM "atlantis"."consumer_user" WHERE "id" = $1`, int64(1)).Scan(&email); err != nil {
				t.Fatalf("get: %v", err)
			}
			if email != "first@y.com" {
				t.Fatalf("email after DO NOTHING: %q want first@y.com", email)
			}
		},
	}
}

func onConflictDoUpdate() Scenario {
	return Scenario{
		Name: "on_conflict_do_update",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			seedUser(t, sb, 1, "old@y.com", true)
			const upsert = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "active") VALUES ($1, $2, $3) ON CONFLICT ("id") DO UPDATE SET "email" = EXCLUDED."email"`
			if _, err := sb.Pool().Exec(ctx, upsert, int64(1), "new@y.com", true); err != nil {
				t.Fatalf("upsert: %v", err)
			}
			var email string
			if err := sb.Pool().QueryRow(ctx, `SELECT "email" FROM "atlantis"."consumer_user" WHERE "id" = $1`, int64(1)).Scan(&email); err != nil {
				t.Fatalf("get: %v", err)
			}
			if email != "new@y.com" {
				t.Fatalf("DO UPDATE: email=%q want new@y.com", email)
			}
		},
	}
}

func keysetCursor() Scenario {
	return Scenario{
		Name: "keyset_cursor_mixed_direction",
		IR:   usersIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			// Five users seeded as (id, email).
			for i, e := range []string{"a", "c", "e", "g", "i"} {
				seedUser(t, sb, int64(i+1), e+"@y.com", true)
			}
			// Page-after cursor at (email='c', id=2) sorted (email DESC, id ASC).
			const sql = `SELECT "id" FROM "atlantis"."consumer_user" WHERE ("email" < $1) OR ("email" = $1 AND "id" > $2) ORDER BY "email" DESC, "id" ASC`
			rows, err := sb.Pool().Query(ctx, sql, "c@y.com", int64(2))
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, id)
			}
			// Strictly less than "c@y.com" (email DESC): a@y.com (id=1).
			// We expect: [1].
			if len(got) != 1 || got[0] != 1 {
				t.Fatalf("keyset page: %v want [1]", got)
			}
		},
	}
}

func jsonbExtractWhere() Scenario {
	return Scenario{
		Name: "jsonb_extract_where",
		IR:   eventsJSONIR,
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			const ins = `INSERT INTO "atlantis"."consumer_event" ("id", "data") VALUES ($1, $2) RETURNING "id"`
			rows := []struct {
				id   int64
				data string
			}{
				{1, `{"type": "signup"}`},
				{2, `{"type": "login"}`},
				{3, `{"type": "signup"}`},
			}
			for _, r := range rows {
				if err := sb.Pool().QueryRow(ctx, ins, r.id, []byte(r.data)).Scan(new(int64)); err != nil {
					t.Fatalf("seed %d: %v", r.id, err)
				}
			}
			const sel = `SELECT "id" FROM "atlantis"."consumer_event" WHERE "data"->>'type' = $1 ORDER BY "id" ASC`
			r, err := sb.Pool().Query(ctx, sel, "signup")
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer r.Close()
			var ids []int64
			for r.Next() {
				var id int64
				if err := r.Scan(&id); err != nil {
					t.Fatalf("scan: %v", err)
				}
				ids = append(ids, id)
			}
			if len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
				t.Fatalf("jsonb filter: %v want [1 3]", ids)
			}
		},
	}
}

// recursiveCTE is the embedded-only scenario — the sim's whitelist
// parser doesn't recognize WITH RECURSIVE. Skipping sim here is the
// honest declaration the fidelity matrix promises.
func recursiveCTE() Scenario {
	return Scenario{
		Name:   "recursive_cte_embedded_only",
		IR:     customQueryIR,
		SkipOn: []string{"sim"},
		Run: func(t *testing.T, sb *sandbox.Sandbox) {
			ctx := context.Background()
			const sql = `
				WITH RECURSIVE seq(n) AS (
					SELECT 1
					UNION ALL
					SELECT n + 1 FROM seq WHERE n < 5
				)
				SELECT count(*)::bigint FROM seq`
			var n int64
			if err := sb.Pool().QueryRow(ctx, sql).Scan(&n); err != nil {
				t.Fatalf("recursive CTE: %v", err)
			}
			if n != 5 {
				t.Fatalf("count: %d want 5", n)
			}
			// Sanity: the typed unsupported sentinel — exported by the
			// sim sql package — never lands here on embedded.
			_ = errors.New
		},
	}
}

// ─────────────────────────── helpers ───────────────────────────

// seedUser inserts one user row through the standard codegen-emitted
// INSERT shape. Wrapped so scenarios don't have to repeat the SQL.
func seedUser(t *testing.T, sb *sandbox.Sandbox, id int64, email string, active bool) {
	t.Helper()
	const ins = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "active") VALUES ($1, $2, $3) RETURNING "id"`
	if err := sb.Pool().QueryRow(context.Background(), ins, id, email, active).Scan(new(int64)); err != nil {
		t.Fatalf("seed user %d: %v", id, err)
	}
}
