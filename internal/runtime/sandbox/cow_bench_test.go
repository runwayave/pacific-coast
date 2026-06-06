package sandbox_test

// Benchmarks that pin the CoW Mark / Fork cost contract. The plan
// committed to O(num_tables) for both ops — these benchmarks scale
// the row count while holding table count constant, demonstrating
// per-mark cost stays flat.
//
// Run with:
//
//	go test -bench BenchmarkMark -benchmem ./internal/runtime/sandbox
//	go test -bench BenchmarkFork -benchmem ./internal/runtime/sandbox

import (
	"context"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
)

// benchUserIR is a minimal entity used by both benchmarks. Identity
// is off so the bench can supply explicit IDs without needing PG
// OVERRIDING semantics — sim sees pure-Go inserts either way.
func benchUserIR() *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "User",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
				{Name: "email", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			},
		}},
	}
}

// seedBench inserts `n` rows into the sandbox. Reused by both
// benchmark targets so the seeding cost stays outside the timed
// region.
func seedBench(b *testing.B, sb *sandbox.Sandbox, n int) {
	b.Helper()
	ctx := context.Background()
	const sql = `INSERT INTO "atlantis"."consumer_user" ("id", "email") VALUES ($1, $2) RETURNING "id"`
	for i := 0; i < n; i++ {
		if err := sb.Pool().QueryRow(ctx, sql, int64(i+1), "u@y.com").Scan(new(int64)); err != nil {
			b.Fatalf("seed %d: %v", i, err)
		}
	}
}

// BenchmarkMark_1k_rows exercises Mark with 1k rows in one table.
// Under O(N) Mark this would copy 1k row entries; under CoW it
// captures one pointer.
func BenchmarkMark_1k_rows(b *testing.B) {
	sb, err := sandbox.New(sandbox.Options{IR: benchUserIR()})
	if err != nil {
		b.Fatalf("new: %v", err)
	}
	defer sb.Close()
	seedBench(b, sb, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sb.Mark()
	}
}

// BenchmarkMark_100k_rows exercises Mark with 100k rows. If the cost
// is O(N), this benchmark takes ~100x longer than the 1k variant.
// Under CoW it stays the same — Mark just captures pointers.
func BenchmarkMark_100k_rows(b *testing.B) {
	sb, err := sandbox.New(sandbox.Options{IR: benchUserIR()})
	if err != nil {
		b.Fatalf("new: %v", err)
	}
	defer sb.Close()
	seedBench(b, sb, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sb.Mark()
	}
}

// BenchmarkFork_100k_rows exercises Fork(8) — eight child sandboxes
// from a parent holding 100k rows. Under O(N) this would deep-copy
// all rows 8 times. Under CoW each child gets a shared pointer.
func BenchmarkFork_100k_rows(b *testing.B) {
	sb, err := sandbox.New(sandbox.Options{IR: benchUserIR()})
	if err != nil {
		b.Fatalf("new: %v", err)
	}
	defer sb.Close()
	seedBench(b, sb, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kids, err := sb.Fork(8)
		if err != nil {
			b.Fatalf("fork: %v", err)
		}
		// Release the references so the benchmark loop doesn't
		// accumulate sandboxes.
		for _, k := range kids {
			_ = k.Close()
		}
	}
}

// BenchmarkRestoreTo_100k_rows exercises RestoreTo over 100k rows.
// CoW: pointer swap. Pre-CoW: O(N) row reinstall.
func BenchmarkRestoreTo_100k_rows(b *testing.B) {
	sb, err := sandbox.New(sandbox.Options{IR: benchUserIR()})
	if err != nil {
		b.Fatalf("new: %v", err)
	}
	defer sb.Close()
	seedBench(b, sb, 100_000)
	mark := sb.Mark()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sb.RestoreTo(mark); err != nil {
			b.Fatalf("restore: %v", err)
		}
	}
}
