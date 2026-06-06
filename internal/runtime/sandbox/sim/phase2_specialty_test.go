package sim_test

// Specialty surface tests: pgvector distance ops, JSONB extract, and
// the hypertable warn-once contract.

import (
	"context"
	"math"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// ─────────────────────────── pgvector ───────────────────────────

func makeEmbeddingTable(t *testing.T) *sim.Pool {
	t.Helper()
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "consumer_embed",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
			{Name: "label", Kind: sim.KindString},
			{Name: "v", Kind: sim.KindVector},
		},
		PKCols: []string{"id"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return sim.NewPool(cat)
}

func seedVectors(t *testing.T, pool *sim.Pool, rows []struct {
	ID    int64
	Label string
	V     []float32
}) {
	t.Helper()
	const ins = `INSERT INTO "atlantis"."consumer_embed" ("id", "label", "v") VALUES ($1, $2, $3) RETURNING "id"`
	for _, r := range rows {
		if err := pool.QueryRow(context.Background(), ins, r.ID, r.Label, r.V).Scan(new(int64)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// TestVectorCosineANN exercises the codegen-emitted ANN search shape:
// project the distance with AS alias, ORDER BY the same distance.
// Closest vector wins — pinpoint correctness of the cosine path.
func TestVectorCosineANN(t *testing.T) {
	pool := makeEmbeddingTable(t)
	seedVectors(t, pool, []struct {
		ID    int64
		Label string
		V     []float32
	}{
		{1, "north", []float32{0, 1, 0}},
		{2, "east", []float32{1, 0, 0}},
		{3, "almost-north", []float32{0.1, 0.99, 0}},
	})
	ctx := context.Background()

	const sql = `SELECT "id", "label", "v" <=> $1::vector AS distance FROM "atlantis"."consumer_embed" ORDER BY "v" <=> $1::vector LIMIT $2`
	rows, err := pool.Query(ctx, sql, []float32{0, 1, 0}, int64(2))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	type r struct {
		id    int64
		label string
		dist  float64
	}
	var got []r
	for rows.Next() {
		var x r
		var v any // distance projection — bind as any so test reads however the executor returns it
		if err := rows.Scan(&x.id, &x.label, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		switch d := v.(type) {
		case float64:
			x.dist = d
		case float32:
			x.dist = float64(d)
		default:
			t.Fatalf("distance type %T", v)
		}
		got = append(got, x)
	}
	if len(got) != 2 {
		t.Fatalf("rows: got %d want 2", len(got))
	}
	// "north" must come first (exact match → distance 0).
	if got[0].id != 1 || got[0].dist > 1e-6 {
		t.Fatalf("closest: %+v", got[0])
	}
	if got[1].id != 3 {
		t.Fatalf("second closest: %+v want id=3 (almost-north)", got[1])
	}
}

// TestVectorL2 confirms the second distance operator's direction —
// closer vectors produce smaller distances.
func TestVectorL2(t *testing.T) {
	pool := makeEmbeddingTable(t)
	seedVectors(t, pool, []struct {
		ID    int64
		Label string
		V     []float32
	}{
		{1, "origin", []float32{0, 0, 0}},
		{2, "far", []float32{10, 10, 10}},
	})
	ctx := context.Background()

	const sql = `SELECT "id" FROM "atlantis"."consumer_embed" ORDER BY "v" <-> $1::vector LIMIT $2`
	rows, err := pool.Query(ctx, sql, []float32{0, 0, 0}, int64(2))
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
	// origin (distance 0) comes before far (distance > 0).
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("ids: %v want [1 2]", ids)
	}
}

// TestVectorIP confirms the inner-product operator (PG returns the
// negative dot product, so smaller = more similar).
func TestVectorIP(t *testing.T) {
	pool := makeEmbeddingTable(t)
	seedVectors(t, pool, []struct {
		ID    int64
		Label string
		V     []float32
	}{
		{1, "aligned", []float32{1, 0, 0}},
		{2, "perp", []float32{0, 1, 0}},
		{3, "opposite", []float32{-1, 0, 0}},
	})
	ctx := context.Background()

	const sql = `SELECT "id", "v" <#> $1::vector AS d FROM "atlantis"."consumer_embed" ORDER BY "v" <#> $1::vector`
	rows, err := pool.Query(ctx, sql, []float32{1, 0, 0})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	type r struct {
		id int64
		d  float64
	}
	var got []r
	for rows.Next() {
		var x r
		var d any
		if err := rows.Scan(&x.id, &d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		switch v := d.(type) {
		case float64:
			x.d = v
		case float32:
			x.d = float64(v)
		}
		got = append(got, x)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	// aligned ((1,0,0)·(1,0,0) = 1 → -1) sorts first; perp (0 → 0) middle;
	// opposite (-1 → 1) last.
	if got[0].id != 1 || got[2].id != 3 {
		t.Fatalf("order: %+v", got)
	}
	if math.Abs(got[0].d-(-1.0)) > 1e-6 {
		t.Fatalf("aligned distance: %v want -1", got[0].d)
	}
}

// ─────────────────────────── JSONB ───────────────────────────

func makeEventsJSONTable(t *testing.T) *sim.Pool {
	t.Helper()
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "consumer_event",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
			{Name: "data", Kind: sim.KindBytes},
		},
		PKCols: []string{"id"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return sim.NewPool(cat)
}

func TestJsonExtractEquality(t *testing.T) {
	pool := makeEventsJSONTable(t)
	ctx := context.Background()
	const ins = `INSERT INTO "atlantis"."consumer_event" ("id", "data") VALUES ($1, $2) RETURNING "id"`
	rows := []struct {
		id   int64
		data string
	}{
		{1, `{"type": "signup", "actor": "alice"}`},
		{2, `{"type": "login", "actor": "bob"}`},
		{3, `{"type": "signup", "actor": "carol"}`},
	}
	for _, r := range rows {
		if err := pool.QueryRow(ctx, ins, r.id, []byte(r.data)).Scan(new(int64)); err != nil {
			t.Fatalf("seed %d: %v", r.id, err)
		}
	}

	const sel = `SELECT "id" FROM "atlantis"."consumer_event" WHERE "data"->>'type' = $1 ORDER BY "id" ASC`
	r, err := pool.Query(ctx, sel, "signup")
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
		t.Fatalf("ids: %v want [1 3]", ids)
	}
}

func TestJsonExtractNested(t *testing.T) {
	pool := makeEventsJSONTable(t)
	ctx := context.Background()
	const ins = `INSERT INTO "atlantis"."consumer_event" ("id", "data") VALUES ($1, $2) RETURNING "id"`
	if err := pool.QueryRow(ctx, ins, int64(1), []byte(`{"user": {"name": "alice"}}`)).Scan(new(int64)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := pool.QueryRow(ctx, ins, int64(2), []byte(`{"user": {"name": "bob"}}`)).Scan(new(int64)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const sel = `SELECT "id" FROM "atlantis"."consumer_event" WHERE "data"->'user'->>'name' = $1`
	var id int64
	if err := pool.QueryRow(ctx, sel, "bob").Scan(&id); err != nil {
		t.Fatalf("query: %v", err)
	}
	if id != 2 {
		t.Fatalf("id: %d want 2", id)
	}
}

// ─────────────────────────── hypertable warn ───────────────────────────

func TestHypertableWarnOnce(t *testing.T) {
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "vendor_event",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
			{Name: "occurred_at", Kind: sim.KindTime},
		},
		PKCols:    []string{"id", "occurred_at"},
		TimeField: "occurred_at",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	var warnCount atomic.Int32
	var warnText atomic.Value
	pool := sim.NewPoolWithOptions(cat, sim.PoolOptions{
		Warn: func(msg string) {
			warnCount.Add(1)
			warnText.Store(msg)
		},
	})

	ctx := context.Background()
	const sel = `SELECT "id" FROM "atlantis"."vendor_event" WHERE "id" = $1`
	// First access — should warn.
	if err := pool.QueryRow(ctx, sel, int64(1)).Scan(new(int64)); err != nil {
		// No row exists but the warning fires before the no-rows
		// signal — that's fine; we ignore the error here.
		_ = err
	}
	// Second access — must NOT warn again.
	if err := pool.QueryRow(ctx, sel, int64(2)).Scan(new(int64)); err != nil {
		_ = err
	}
	if got := warnCount.Load(); got != 1 {
		t.Fatalf("warn fired %d times, want 1", got)
	}
	msg, _ := warnText.Load().(string)
	if !strings.Contains(msg, "hypertable atlantis.vendor_event") {
		t.Fatalf("warn message: %q", msg)
	}
	if !strings.Contains(msg, "production performance") {
		t.Fatalf("warn lacks performance disclaimer: %q", msg)
	}
}

// TestNonHypertableNoWarn confirms regular tables never trigger the
// warning regardless of how many times they're queried.
func TestNonHypertableNoWarn(t *testing.T) {
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "consumer_user",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
		},
		PKCols: []string{"id"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	var warned atomic.Int32
	pool := sim.NewPoolWithOptions(cat, sim.PoolOptions{
		Warn: func(string) { warned.Add(1) },
	})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = pool.QueryRow(ctx, `SELECT "id" FROM "atlantis"."consumer_user" WHERE "id" = $1`, int64(i)).Scan(new(int64))
	}
	if warned.Load() != 0 {
		t.Fatalf("regular table warned %d times, want 0", warned.Load())
	}
}
