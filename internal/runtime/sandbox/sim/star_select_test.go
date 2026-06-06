package sim_test

// End-to-end coverage for `SELECT *` expansion. The parser emits a
// Star-marked Projection; the executor must expand it against the
// catalog into one bare-column Projection per declared column. Tests
// here pin both the column-order semantics (catalog declaration order)
// and the integration with WHERE / LIMIT / SELECT * mixed with
// explicit columns.

import (
	"context"
	"errors"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
	simsql "github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim/sql"
)

// makeFourColTable installs a four-column entity so column-order
// assertions are visible. Order: id, email, status, score.
func makeFourColTable(t *testing.T) *sim.Pool {
	t.Helper()
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "consumer_account",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
			{Name: "email", Kind: sim.KindString},
			{Name: "status", Kind: sim.KindString, Nullable: true},
			{Name: "score", Kind: sim.KindInt64, Nullable: true},
		},
		PKCols: []string{"id"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return sim.NewPool(cat)
}

func seedAccount(t *testing.T, pool *sim.Pool, id int64, email, status string, score int64) {
	t.Helper()
	const sqlIns = `INSERT INTO "atlantis"."consumer_account" ("id", "email", "status", "score") VALUES ($1, $2, $3, $4) RETURNING "id"`
	if err := pool.QueryRow(context.Background(), sqlIns, id, email, status, score).Scan(new(int64)); err != nil {
		t.Fatalf("seed id=%d: %v", id, err)
	}
}

func TestSelectStar_ExpandsToCatalogOrder(t *testing.T) {
	pool := makeFourColTable(t)
	seedAccount(t, pool, 1, "a@x", "active", 42)

	rows, err := pool.Query(context.Background(), `SELECT * FROM "atlantis"."consumer_account"`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatalf("expected one row, got none")
	}
	var id int64
	var email, status string
	var score int64
	if err := rows.Scan(&id, &email, &status, &score); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if id != 1 || email != "a@x" || status != "active" || score != 42 {
		t.Fatalf("got id=%d email=%q status=%q score=%d", id, email, status, score)
	}
	if rows.Next() {
		t.Fatalf("expected no more rows")
	}
}

func TestSelectStar_WithWhereAndLimit(t *testing.T) {
	pool := makeFourColTable(t)
	for i := int64(1); i <= 5; i++ {
		seedAccount(t, pool, i, "u@x", "active", i*10)
	}

	rows, err := pool.Query(context.Background(),
		`SELECT * FROM "atlantis"."consumer_account" WHERE "score" >= $1 LIMIT $2`,
		int64(30), int64(2),
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var seen int
	for rows.Next() {
		var id int64
		var email, status string
		var score int64
		if err := rows.Scan(&id, &email, &status, &score); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if score < 30 {
			t.Errorf("WHERE filter leaked: id=%d score=%d", id, score)
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("LIMIT: got %d rows, want 2", seen)
	}
}

func TestSelectStar_MixedWithExplicitColumn(t *testing.T) {
	// SELECT *, "id" — pg accepts; executor expands * to all four cols,
	// then appends the explicit "id" as a fifth projection.
	pool := makeFourColTable(t)
	seedAccount(t, pool, 7, "z@x", "active", 1)

	rows, err := pool.Query(context.Background(),
		`SELECT *, "id" FROM "atlantis"."consumer_account"`,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatalf("expected one row, got none")
	}
	var id1 int64
	var email, status string
	var score int64
	var id2 int64
	if err := rows.Scan(&id1, &email, &status, &score, &id2); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if id1 != 7 || id2 != 7 {
		t.Errorf("ids = %d, %d; want both 7", id1, id2)
	}
}

func TestSelect_LiteralLimit(t *testing.T) {
	// `LIMIT 5` (integer literal, not $N) — common human-typed form.
	// Translator emits a Literal; executor evaluates it to 5.
	pool := makeFourColTable(t)
	for i := int64(1); i <= 10; i++ {
		seedAccount(t, pool, i, "u@x", "active", i)
	}

	rows, err := pool.Query(context.Background(),
		`SELECT "id" FROM "atlantis"."consumer_account" LIMIT 3`,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
	}
	if n != 3 {
		t.Errorf("LIMIT 3 returned %d rows, want 3", n)
	}
}

func TestSelect_LiteralOffset(t *testing.T) {
	pool := makeFourColTable(t)
	for i := int64(1); i <= 5; i++ {
		seedAccount(t, pool, i, "u@x", "active", i)
	}
	rows, err := pool.Query(context.Background(),
		`SELECT "id" FROM "atlantis"."consumer_account" ORDER BY "id" OFFSET 2`,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	want := []int64{3, 4, 5}
	if len(ids) != len(want) {
		t.Fatalf("OFFSET 2 returned %d rows, want %d (%+v)", len(ids), len(want), ids)
	}
	for i, id := range ids {
		if id != want[i] {
			t.Errorf("id[%d] = %d; want %d", i, id, want[i])
		}
	}
}

func TestSelect_MixedLimitPlaceholderOffsetLiteral(t *testing.T) {
	// Common debugging shape: pagination via $N but offset hardcoded.
	pool := makeFourColTable(t)
	for i := int64(1); i <= 8; i++ {
		seedAccount(t, pool, i, "u@x", "active", i)
	}
	rows, err := pool.Query(context.Background(),
		`SELECT "id" FROM "atlantis"."consumer_account" ORDER BY "id" LIMIT $1 OFFSET 3`,
		int64(2),
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	want := []int64{4, 5}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Errorf("got %+v; want %+v", ids, want)
	}
}

// columnsProvider mirrors the optional extension interface http.go
// type-asserts on. Kept local to the test so it doesn't pollute the
// public sim API.
type columnsProvider interface{ Columns() []string }

func TestSelectStar_Columns_FullExpansion(t *testing.T) {
	// Columns() must report the post-expansion projection — what the
	// HTTP layer uses to label the response. Without this, SELECT *
	// blows up downstream with a "N projections vs 1 dest" mismatch.
	pool := makeFourColTable(t)
	seedAccount(t, pool, 1, "a@x", "active", 1)

	rows, err := pool.Query(context.Background(), `SELECT * FROM "atlantis"."consumer_account"`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	cp, ok := rows.(columnsProvider)
	if !ok {
		t.Fatalf("rows %T does not implement Columns()", rows)
	}
	got := cp.Columns()
	want := []string{"id", "email", "status", "score"}
	if len(got) != len(want) {
		t.Fatalf("Columns() = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Columns()[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

func TestSelectStar_Columns_MixedWithExplicit(t *testing.T) {
	pool := makeFourColTable(t)
	seedAccount(t, pool, 1, "a@x", "active", 1)
	rows, err := pool.Query(context.Background(),
		`SELECT *, "id" FROM "atlantis"."consumer_account"`,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	cp := rows.(columnsProvider)
	got := cp.Columns()
	want := []string{"id", "email", "status", "score", "id"}
	if len(got) != len(want) {
		t.Fatalf("Columns() = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Columns()[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

func TestSelect_Columns_NameAlias(t *testing.T) {
	// `expr AS alias` projects under the alias; Columns() reflects it.
	pool := makeFourColTable(t)
	seedAccount(t, pool, 1, "a@x", "active", 1)
	rows, err := pool.Query(context.Background(),
		`SELECT "id" AS account_id FROM "atlantis"."consumer_account"`,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	cp := rows.(columnsProvider)
	got := cp.Columns()
	if len(got) != 1 || got[0] != "account_id" {
		t.Errorf("Columns() = %+v; want [account_id]", got)
	}
}

func TestSelectStar_WithoutFromRejected(t *testing.T) {
	pool := makeFourColTable(t)
	// PG rejects `SELECT *` without FROM with a syntax error during
	// parse. Our translator allows the shape (it's a perfectly valid
	// AST), but execSelectNoFrom turns the Star projection into a
	// clean ErrUnsupported.
	_, err := pool.Query(context.Background(), `SELECT *`)
	if err == nil {
		t.Fatalf("expected error for SELECT * without FROM")
	}
	if !errors.Is(err, simsql.ErrUnsupported) {
		t.Errorf("err = %v; want errors.Is ErrUnsupported", err)
	}
}
