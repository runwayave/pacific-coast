//go:build audit

// Caller audit. Walks every caller repo (backend, vendor-platform) and
// asserts that no file outside the explicit exemption list invokes a
// PG connection directly. The intent is to make a regression of the
// atlantis contract loud and immediate: once a caller has cut over
// to the typed gRPC client there is no reason for it to ever own a
// *pgxpool.Pool or to call pool.Query / pool.Exec / pool.QueryRow /
// pgx.Batch.Queue. A new direct-pool callsite slipping in (a hand-rolled
// repository, an "emergency" backfill script, a paste from another
// service) reintroduces the cross-repo coupling that atlantis was
// built to eliminate.
//
// Two-layer gate:
//
//  1. Methods of interest are matched by selector name only — `.Query`,
//     `.Exec`, `.QueryRow`, `.Queue`. Names are uncommon enough on
//     non-pool types that the false-positive rate is acceptable for an
//     allow-list approach (any incidental match goes into exemptions.yaml
//     with a one-line justification).
//
//  2. Files listed in exemptions.yaml are skipped. The list shrinks as
//     each 9b caller-cutover PR removes the repository file it replaces.
//     The target state is an empty exemption list — at that point the
//     audit gate goes red on any direct-pool callsite anywhere in the
//     monorepo.
//
// Configuration:
//
//   - AUDIT_CALLER_ROOTS: comma-separated absolute paths to scan. Defaults
//     to "../backend,../vendor-platform" (sibling-dir layout from the
//     atlantis repo root).
//   - AUDIT_EXEMPTIONS:   path to exemptions.yaml. Defaults to
//     tests/audit/exemptions.yaml.

package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// pgMethods are the selector names that, when called on what looks like a
// pool or transaction handle, are treated as direct PG access. We match by
// name rather than by resolved type because the audit walks dependency-free
// caller repos in isolation — a type-checked import graph is overkill for
// a regression gate, and false positives are cheap to silence.
var pgMethods = map[string]struct{}{
	"Query":     {},
	"QueryRow":  {},
	"Exec":      {},
	"Queue":     {}, // pgx.Batch.Queue
	"SendBatch": {},
	"CopyFrom":  {},
}

// poolReceiverHints are receiver-identifier substrings that flip the gate.
// A `foo.Query()` call is only flagged if `foo` looks like a pool / tx /
// connection. The hints are intentionally permissive — a stricter rule
// (require exact type match) trips on legitimate codepaths that hide the
// pool behind a one-letter alias (`p.Query(...)` is the worst offender).
var poolReceiverHints = []string{
	"pool", "db", "conn", "tx", "pgxpool", "dbtx", "querier", "store",
}

type exemptionsFile struct {
	// Files is a list of path globs (relative to the caller-root dir)
	// that are allowed to contain direct-pool calls. Each entry should
	// carry a comment in the YAML explaining why it exists and which
	// caller-cutover PR is expected to remove it.
	Files []string `yaml:"files"`
}

func TestNoDirectPGAccessInCallers(t *testing.T) {
	// Anchor relative paths to the atlantis repo root rather than the
	// test working directory. `go test` runs each package in its own dir,
	// so `../backend` would resolve to tests/backend without this.
	repoRoot := repoRootFromTestFile()

	defaultRoots := strings.Join([]string{
		filepath.Join(repoRoot, "..", "backend"),
		filepath.Join(repoRoot, "..", "vendor-platform"),
	}, ",")
	roots := strings.Split(getenv("AUDIT_CALLER_ROOTS", defaultRoots), ",")
	for i := range roots {
		roots[i] = strings.TrimSpace(roots[i])
	}

	defaultExemptions := filepath.Join(repoRoot, "tests", "audit", "exemptions.yaml")
	exemptions := loadExemptions(t, getenv("AUDIT_EXEMPTIONS", defaultExemptions))

	var violations []string
	for _, root := range roots {
		root = mustAbs(t, root)
		internal := filepath.Join(root, "internal")
		if _, err := os.Stat(internal); os.IsNotExist(err) {
			t.Logf("audit: skipping %s (no internal/ directory)", root)
			continue
		}
		violations = append(violations, scanRoot(t, root, internal, exemptions)...)
	}

	if len(violations) == 0 {
		return
	}
	sort.Strings(violations)
	t.Errorf("direct-pool callsites in caller repos (move through atlantis typed client or add to exemptions.yaml with a justification):\n  - %s",
		strings.Join(violations, "\n  - "))
}

func scanRoot(t *testing.T, root, internal string, exemptions map[string]bool) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(internal, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendored deps and mock packages — mocks frequently
			// implement pool-shaped interfaces verbatim.
			if d.Name() == "vendor" || d.Name() == "mocks" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if isExempt(rel, exemptions) {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			// Parse errors are caller-side bugs — surface but don't fail
			// the audit on them.
			t.Logf("audit: parse %s: %v", rel, err)
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, want := pgMethods[sel.Sel.Name]; !want {
				return true
			}
			// The receiver token is whatever is immediately to the left of
			// `.Method`. For `r.db.Query(...)` it's the inner SelectorExpr's
			// trailing field (`db`); for `pool.Query(...)` it's the Ident
			// (`pool`). Both forms reach the same gate.
			if !receiverLooksLikePool(sel.X) {
				return true
			}
			pos := fset.Position(call.Pos())
			hits = append(hits, rel+":"+itoa(pos.Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("audit: walk %s: %v", internal, err)
	}
	return hits
}

func looksLikePool(name string) bool {
	lower := strings.ToLower(name)
	for _, hint := range poolReceiverHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// receiverLooksLikePool walks the receiver expression to find the closest
// identifier-shaped token and checks it against the hint list. Handles
// both `pool.Query(...)` (Ident) and the more common `r.db.Query(...)`
// (SelectorExpr chain) shape repositories use. Anything else (function
// calls returning a pool, casts) falls through unflagged — those are rare
// and the false-negative is acceptable for an allow-list gate.
func receiverLooksLikePool(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return looksLikePool(e.Name)
	case *ast.SelectorExpr:
		// `r.db.Query(...)` → e.Sel is `db` — the field that owns the
		// pool inside the surrounding struct. That's the strongest
		// signal we have without full type info.
		return looksLikePool(e.Sel.Name)
	}
	return false
}

func isExempt(rel string, exemptions map[string]bool) bool {
	// Exact match first (the common case — one entry per repository file).
	if exemptions[rel] {
		return true
	}
	// Glob match for directory-level exemptions (e.g. "platform/database/*").
	for pat := range exemptions {
		if matched, _ := filepath.Match(pat, rel); matched {
			return true
		}
	}
	return false
}

func loadExemptions(t *testing.T, path string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatalf("audit: read exemptions: %v", err)
	}
	var f exemptionsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		t.Fatalf("audit: parse exemptions: %v", err)
	}
	for _, p := range f.Files {
		out[p] = true
	}
	return out
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("audit: abs %q: %v", p, err)
	}
	return abs
}

// TestReceiverLooksLikePool locks in the two receiver shapes the AST walker
// has to handle. `pool.Query()` is the obvious one; `r.db.Query()` is the
// shape every repository in the monorepo uses (struct field holding a pool)
// and is the one the original implementation missed.
func TestReceiverLooksLikePool(t *testing.T) {
	parse := func(src string) ast.Expr {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), "x.go",
			"package x\nfunc _() { "+src+" }", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		// Pull the first ExpressionStatement.CallExpr.SelectorExpr.X out.
		stmt := f.Decls[0].(*ast.FuncDecl).Body.List[0].(*ast.ExprStmt)
		call := stmt.X.(*ast.CallExpr)
		return call.Fun.(*ast.SelectorExpr).X
	}
	cases := []struct {
		src  string
		want bool
	}{
		{"pool.Query(ctx, q)", true},      // Ident
		{"r.db.Query(ctx, q)", true},      // SelectorExpr chain (struct field)
		{"r.pool.Exec(ctx, q)", true},     // SelectorExpr, different field name
		{"r.conn.QueryRow(ctx, q)", true}, // conn-shaped field
		{"svc.cache.Get(ctx, k)", false},  // cache is not a pool hint
		{"foo.Bar.Baz(x)", false},         // arbitrary chain
	}
	for _, tc := range cases {
		got := receiverLooksLikePool(parse(tc.src))
		if got != tc.want {
			t.Errorf("receiverLooksLikePool(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// repoRootFromTestFile derives the atlantis repo root from this file's
// location at compile time. tests/audit/audit_test.go is two dirs deep, so
// the repo root is the parent's parent.
func repoRootFromTestFile() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
