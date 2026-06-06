package dsl

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParseAndLower fuzzes the DSL pipeline that every caller-submitted
// .atl byte stream flows through: Parse → Lower. Both are reachable from
// the network via the admin RPCs PlanSchema and ApplyMigration, where
// `req.Files[*].Content` is a raw []byte chosen by the caller (subject
// only to mTLS authentication, not authorization on content).
//
// The safety contract this fuzz asserts:
//
//  1. Parse never panics on any input. A parse error is fine; a runtime
//     crash is not — we never want a malformed .atl to take down the
//     server process.
//  2. When Parse returns NO error, the resulting *File must be safe to
//     Lower without panicking. Lower's error returns may still fire
//     (multi-file invariant checks happen there) but a runtime crash
//     means an unenforced post-parse invariant.
//
// Note: Parse intentionally returns both *File AND error on failure
// (a partial AST is more useful than nothing for incremental tooling).
// The admin layer drops the partial on error so it never reaches Lower
// in production; this fuzz mirrors that contract — Lower is only run
// when Parse signalled clean.
//
// Seeded with every .atl file in the repo's testdata + a few
// hand-crafted adversarial cases (nested braces, UTF-8 boundary chars,
// extremely long identifiers, ambiguous keyword/identifier mixes).
func FuzzParseAndLower(f *testing.F) {
	// Seed corpus: every .atl in the repo's testdata trees plus
	// pathological hand-crafted inputs.
	seeds := loadAtlSeeds(f)
	seeds = append(seeds,
		// Empty and near-empty.
		[]byte(""),
		[]byte("\n"),
		[]byte("//"),
		// Whitespace-only.
		[]byte("   \t\n\r  "),
		// A minimal valid entity.
		[]byte("entity A in x { id bigint primary }"),
		// Unbalanced braces.
		[]byte("entity A in x {"),
		[]byte("entity A in x }"),
		[]byte("entity A in x { { { } }"),
		// Stray keywords.
		[]byte("entity entity in entity { }"),
		[]byte("in in in"),
		// Ambiguous identifier / keyword soup.
		[]byte("entity A in x { entity bigint primary }"),
		// Comment-only.
		[]byte("// just a comment\n/* and another */"),
		// Long identifier.
		[]byte("entity "+repeatStr("x", 4096)+" in y { id bigint primary }"),
		// Long namespace.
		[]byte("entity A in "+repeatStr("x", 4096)+" { id bigint primary }"),
		// Vector with unusual dimensions.
		[]byte("entity A in x { id bigint primary v vector(0) }"),
		[]byte("entity A in x { id bigint primary v vector(-1) }"),
		[]byte("entity A in x { id bigint primary v vector(1000000) }"),
		// FK ref with empty target.
		[]byte("entity A in x { id bigint primary other text references . }"),
		// UTF-8 boundary.
		[]byte("entity \xff\xfe in x { id bigint primary }"),
		[]byte("entity A in x { id bigint primary nm text default \"\xc3\x28\" }"),
		// Null bytes.
		[]byte("entity A\x00B in x { id bigint primary }"),
	)
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// (1) Parse must not panic regardless of input.
		file, err := Parse("fuzz.atl", data)
		if err != nil {
			// Documented partial-AST-on-error path. Do not lower it —
			// admin.parseSubmitted drops it in production.
			return
		}
		if file == nil {
			t.Fatalf("Parse returned nil error and nil *File")
		}

		// (2) Lower must not panic on any AST that Parse accepted clean.
		_, _ = Lower([]*File{file})
	})
}

// loadAtlSeeds reads every .atl file under the repo's `schema/` and
// `migrations/` testdata trees. Each file is appended to the corpus so
// the fuzzer learns from real-world inputs the parser already handles
// before it starts mutating them.
func loadAtlSeeds(t testing.TB) [][]byte {
	t.Helper()
	var out [][]byte
	// Walk from two levels up — the test runs in internal/dsl/ but the
	// .atl corpus lives at the repo root.
	roots := []string{"../../schema"}
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if filepath.Ext(path) != ".atl" {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			out = append(out, b)
			return nil
		})
	}
	return out
}

func repeatStr(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
