package sim_test

// Keyset-pagination tests. The codegen-emitted shape uses comparison
// operators, multi-column ORDER BY (with mixed ASC/DESC + explicit
// NULLS handling), and OR-grouped predicates — codegen.runtime
// PaginationKeyset produces exactly this idiom for cursor-based List
// RPCs.

import (
	"context"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// makeEventsTable shapes an entity with (created_at, id) compound sort
// keys — the canonical "newest first, tiebroken by id" cursor shape.
func makeEventsTable(t *testing.T) *sim.Pool {
	t.Helper()
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "consumer_event",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
			{Name: "created_at", Kind: sim.KindInt64}, // int64 surrogate so tests stay deterministic
			{Name: "title", Kind: sim.KindString, Nullable: true},
		},
		PKCols: []string{"id"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return sim.NewPool(cat)
}

func seedEvents(t *testing.T, pool *sim.Pool, rows ...[3]int64) {
	t.Helper()
	const sqlIns = `INSERT INTO "atlantis"."consumer_event" ("id", "created_at", "title") VALUES ($1, $2, $3) RETURNING "id"`
	for _, r := range rows {
		title := ""
		if r[2] != 0 {
			title = "row" + itoa(int(r[2]))
		}
		var titleArg any = title
		if r[2] == -1 {
			titleArg = nil // explicit NULL
		}
		if err := pool.QueryRow(context.Background(), sqlIns, r[0], r[1], titleArg).Scan(new(int64)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// TestCmpGreaterThan exercises the simplest non-equality predicate
// path — the building block keyset cursors compose into nested OR.
func TestCmpGreaterThan(t *testing.T) {
	pool := makeEventsTable(t)
	seedEvents(t, pool,
		[3]int64{1, 100, 1},
		[3]int64{2, 200, 2},
		[3]int64{3, 300, 3},
	)
	ctx := context.Background()

	const sqlList = `SELECT "id" FROM "atlantis"."consumer_event" WHERE "created_at" > $1 ORDER BY "id" ASC`
	rows, err := pool.Query(ctx, sqlList, int64(150))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	want := []int64{2, 3}
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("> $1: got %v want %v", got, want)
	}
}

// TestCmpAllOps exercises each comparison operator the parser now
// recognizes. Catches accidental swap of LT/LE or GT/GE in the
// CmpOp table.
func TestCmpAllOps(t *testing.T) {
	pool := makeEventsTable(t)
	seedEvents(t, pool,
		[3]int64{1, 100, 1},
		[3]int64{2, 200, 2},
		[3]int64{3, 300, 3},
		[3]int64{4, 400, 4},
	)
	ctx := context.Background()

	cases := []struct {
		op   string
		arg  int64
		want []int64
	}{
		{"=", 200, []int64{2}},
		{"<", 200, []int64{1}},
		{"<=", 200, []int64{1, 2}},
		{">", 200, []int64{3, 4}},
		{">=", 200, []int64{2, 3, 4}},
		{"!=", 200, []int64{1, 3, 4}},
		{"<>", 200, []int64{1, 3, 4}}, // PG-style NE spelling
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			sql := `SELECT "id" FROM "atlantis"."consumer_event" WHERE "created_at" ` + tc.op + ` $1 ORDER BY "id" ASC`
			rows, err := pool.Query(ctx, sql, tc.arg)
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
			if len(got) != len(tc.want) {
				t.Fatalf("rows: got %v want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("rows[%d]: got %d want %d (full %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestOrGroupKeysetMixed exercises the codegen-emitted mixed-direction
// keyset shape exactly: "page after (created_at=200, id=2) sorted DESC
// by created_at, ASC by id" comes back as
//
//	WHERE
//	  ("created_at" < $1)
//	  OR ("created_at" = $1 AND "id" > $2)
//	ORDER BY "created_at" DESC, "id" ASC
//
// The page-after row (id=2) and rows BEFORE the cursor are excluded;
// rows after appear in the published order.
func TestOrGroupKeysetMixed(t *testing.T) {
	pool := makeEventsTable(t)
	// 5 rows; we want the cursor at (created_at=200, id=2) to return
	// (100, 1), (100, 3) — those after the cursor under
	// (created_at DESC, id ASC).
	seedEvents(t, pool,
		[3]int64{1, 100, 0}, // (100, id=1) — same created_at, id > 2 not, but created_at < 200 → matches
		[3]int64{2, 200, 0}, // cursor row itself; excluded
		[3]int64{3, 100, 0}, // (100, id=3) — created_at < 200 → matches
		[3]int64{4, 300, 0}, // (300, id=4) — created_at > 200 → excluded
		[3]int64{5, 200, 0}, // same created_at as cursor, id > 2 → matches
	)
	ctx := context.Background()

	const sqlList = `SELECT "id" FROM "atlantis"."consumer_event" WHERE ("created_at" < $1) OR ("created_at" = $1 AND "id" > $2) ORDER BY "created_at" DESC, "id" ASC`
	rows, err := pool.Query(ctx, sqlList, int64(200), int64(2))
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
	// Expected order under (created_at DESC, id ASC):
	//   id=5 (created_at=200), id=1 (created_at=100), id=3 (created_at=100)
	want := []int64{5, 1, 3}
	if len(got) != len(want) {
		t.Fatalf("rows: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows[%d]: got %d want %d (full %v)", i, got[i], want[i], got)
		}
	}
}

// TestMultiColOrderBy exercises plain multi-column ORDER BY without
// the keyset cursor predicate — useful when codegen uses OFFSET-based
// pagination on a multi-sorted view.
func TestMultiColOrderBy(t *testing.T) {
	pool := makeEventsTable(t)
	seedEvents(t, pool,
		[3]int64{1, 200, 0},
		[3]int64{2, 100, 0},
		[3]int64{3, 200, 0},
		[3]int64{4, 100, 0},
	)
	ctx := context.Background()

	const sqlList = `SELECT "id" FROM "atlantis"."consumer_event" ORDER BY "created_at" ASC, "id" DESC`
	rows, err := pool.Query(ctx, sqlList)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	want := []int64{4, 2, 3, 1} // created_at 100 ties broken by id DESC (4,2); then 200 (3,1).
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if len(got) != len(want) {
		t.Fatalf("rows: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows[%d]: got %d want %d (full %v)", i, got[i], want[i], got)
		}
	}
}

// TestExplicitNullsFirstLast exercises the per-column NULLS overrides.
// We seed one row with title=NULL and three with titles; the ORDER BY
// explicitly tells the sort where NULL goes.
func TestExplicitNullsFirstLast(t *testing.T) {
	pool := makeEventsTable(t)
	seedEvents(t, pool,
		[3]int64{1, 100, 1},
		[3]int64{2, 100, -1}, // NULL title
		[3]int64{3, 100, 3},
	)
	ctx := context.Background()

	cases := []struct {
		clause string
		want   []int64
	}{
		// ASC default: NULLS LAST → 1, 3, 2.
		{`ORDER BY "title" ASC`, []int64{1, 3, 2}},
		// ASC NULLS FIRST → 2, 1, 3.
		{`ORDER BY "title" ASC NULLS FIRST`, []int64{2, 1, 3}},
		// DESC default: NULLS FIRST → 2, 3, 1.
		{`ORDER BY "title" DESC`, []int64{2, 3, 1}},
		// DESC NULLS LAST → 3, 1, 2.
		{`ORDER BY "title" DESC NULLS LAST`, []int64{3, 1, 2}},
	}
	for _, tc := range cases {
		t.Run(tc.clause, func(t *testing.T) {
			sql := `SELECT "id" FROM "atlantis"."consumer_event" ` + tc.clause
			rows, err := pool.Query(ctx, sql)
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
			if len(got) != len(tc.want) {
				t.Fatalf("rows: got %v want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("rows[%d]: got %d want %d (full %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestOrGroupSinglePred confirms that a single-pred parenthesized
// group (the trivial degenerate case) parses and evaluates as if the
// parens weren't there. Codegen sometimes emits redundant parens for
// clarity.
func TestOrGroupSinglePred(t *testing.T) {
	pool := makeEventsTable(t)
	seedEvents(t, pool, [3]int64{1, 100, 1}, [3]int64{2, 200, 2})
	ctx := context.Background()

	const sql = `SELECT "id" FROM "atlantis"."consumer_event" WHERE ("created_at" = $1)`
	var id int64
	if err := pool.QueryRow(ctx, sql, int64(200)).Scan(&id); err != nil {
		t.Fatalf("query: %v", err)
	}
	if id != 2 {
		t.Fatalf("got id=%d want 2", id)
	}
}
