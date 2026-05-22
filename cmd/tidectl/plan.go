package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/sqlvalidate"
)

// cmdPlan diffs the .atl file set against the IR checkpoint and writes a
// staged migration pair into <stage-dir>. The staged files are NOT placed
// directly in migrations/ — that's `tidectl approve`'s job (the human
// review gate).
//
// Exit code:
//
//	0 — plan emitted (or no changes).
//	1 — plan emitted but contains backfill-required or breaking changes;
//	    caller must add --destructive to the approve step.
//	2 — bad args / IO error.
//
// A staged migration is named NNNN_tidectl_staged.up.sql / .down.sql, where
// NNNN is the next sequence number derived from the existing migrations
// directory. If you stage two consecutive plans without approving the
// first, the second will overwrite the first staged pair — staging is a
// scratch area by design.
func cmdPlan(args []string) int {
	fs := flagSet("plan")
	schemaDir := fs.String("schema-dir", "schema", "Directory containing .atl files")
	checkpoint := fs.String("ir-checkpoint", "gen/.last-ir.json", "Previous IR checkpoint")
	stageDir := fs.String("stage-dir", "migrations/tidectl/_staged", "Where to write the staged migration")
	migrationsDir := fs.String("migrations-dir", "migrations/tidectl", "Existing migrations (for sequence number)")
	allowDestructive := fs.Bool("destructive", false, "Allow backfill-required / breaking changes")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	files, err := loadATLFiles(*schemaDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan:", err)
		return 2
	}

	newIR, err := dsl.Lower(files)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan:", err)
		return 1
	}
	// pg_query_go validation runs ON TOP of IR lowering. IR lowering
	// catches the dep-free errors ($arg references, touches resolution,
	// typed-step column existence); the sqlvalidate pass catches what
	// only the actual PG parser can know — syntax errors, table
	// references that don't resolve, statements of the wrong kind for
	// the construct (DDL in a query body, etc.). Reporting them at
	// plan time means the apply path never reaches PG with bad SQL.
	if err := validateCustomSQL(newIR); err != nil {
		fmt.Fprintln(os.Stderr, "plan: custom SQL validation:")
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	prior, _ := loadCheckpoint(*checkpoint)
	codegen.AssignProtoNumbers(prior, newIR)
	diff := codegen.ComputeDiff(prior, newIR)

	var scripts codegen.SQLScripts
	if prior == nil {
		scripts, err = codegen.EmitInitial(newIR)
	} else {
		scripts, err = codegen.EmitSQL(prior, newIR, diff)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan:", err)
		return 1
	}

	// Decide whether the plan needs explicit operator approval.
	class := diff.HighestClass()
	if (class == codegen.ClassBackfillRequired || class == codegen.ClassCrossCallerBreaking) && !*allowDestructive {
		fmt.Fprintf(os.Stderr, "plan: %s changes detected — re-run with --destructive\n", class)
		printDiffSummary(diff)
		return 1
	}

	seq := nextMigrationSeq(*migrationsDir)
	name := fmt.Sprintf("%04d_tidectl_staged", seq)
	upPath := filepath.Join(*stageDir, name+".up.sql")
	downPath := filepath.Join(*stageDir, name+".down.sql")

	if err := writeFile(upPath, []byte(scripts.Up)); err != nil {
		fmt.Fprintln(os.Stderr, "plan:", err)
		return 2
	}
	if err := writeFile(downPath, []byte(scripts.Down)); err != nil {
		fmt.Fprintln(os.Stderr, "plan:", err)
		return 2
	}

	fmt.Printf("plan staged at %s (class=%s, %d additive, %d backfill, %d breaking)\n",
		upPath, class, len(diff.Additive), len(diff.BackfillRequired), len(diff.Breaking))
	return 0
}

func printDiffSummary(d *codegen.Diff) {
	fmt.Fprintln(os.Stderr, "")
	if len(d.BackfillRequired) > 0 {
		fmt.Fprintln(os.Stderr, "backfill-required changes:")
		for _, ch := range d.BackfillRequired {
			fmt.Fprintf(os.Stderr, "  %s/%s: %s\n", ch.EntityID, ch.Field, ch.Detail)
		}
	}
	if len(d.Breaking) > 0 {
		fmt.Fprintln(os.Stderr, "breaking changes:")
		for _, ch := range d.Breaking {
			fmt.Fprintf(os.Stderr, "  %s/%s: %s\n", ch.EntityID, ch.Field, ch.Detail)
		}
	}
}

// nextMigrationSeq scans migrationsDir for files named NNNN_*.sql and
// returns NNNN+1. Returns 1 if the directory doesn't exist or is empty.
func nextMigrationSeq(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 1
	}
	max := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Files look like "0001_foo.up.sql"; pull the leading digits.
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		// Read up to first non-digit.
		i := 0
		for i < len(name) && name[i] >= '0' && name[i] <= '9' {
			i++
		}
		if i == 0 {
			continue
		}
		n := atoiOrZero(name[:i])
		if n > max {
			max = n
		}
	}
	return max + 1
}

func atoiOrZero(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// validateCustomSQL runs pg_query_go-backed validation over every
// custom query and procedure in the IR. Returns a joined error
// surfacing every failure across the entire IR so an engineer fixing
// one query sees the others' problems too. Returns nil when the IR
// has no custom decls or when every one validates cleanly.
//
// The validator is intentionally invoked from the CLI rather than the
// IR-lowering hot path: it carries a CGO dependency (libpg_query) and
// not every consumer (codegen tests, embed-mode harnesses) wants to
// pull that in. Plan-time is the right floor — by the time a migration
// goes near a real DB, this has run.
func validateCustomSQL(ir *dsl.IR) error {
	var errs []error
	for i := range ir.Queries {
		if err := sqlvalidate.ValidateCustomQuery(ir, &ir.Queries[i]); err != nil {
			errs = append(errs, err)
		}
	}
	for i := range ir.Procedures {
		if err := sqlvalidate.ValidateCustomProcedure(ir, &ir.Procedures[i]); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
