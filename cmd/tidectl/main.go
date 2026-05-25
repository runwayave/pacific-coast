// tidectl — server-side admin CLI for atlantis.
//
// Subcommands:
//
//	tidectl codegen   Regenerate proto / Go / SQL / keys from a directory of .atl files.
//	tidectl plan      Diff a directory of .atl files against the IR checkpoint and
//	                emit the staged migration .up.sql / .down.sql.
//	tidectl approve   Move a staged migration into the migrations directory.
//	tidectl lint      Parse + lower every .atl file in a directory, exit 0 iff clean.
//	tidectl migrate-up   Run golang-migrate up against $PG_URL.
//	tidectl migrate-down Run golang-migrate down 1 against $PG_URL.
//	tidectl dev       One-shot local-dev loop: codegen + buf generate + go build +
//	                exec atlantis-server. Reads atlantis.dev.yaml (working-tree
//	                paths via source: local). Use for iteration; production
//	                deployments use the workspace.yaml + git refs path.
//	tidectl version   Print tidectl version.
//
// tidectl is *operator-shaped*. It runs against local files and (for migrate)
// the database. Day-to-day developer workflow is `tide apply`, not tidectl.
// tidectl is what the on-call operator reaches for when they need to
// inspect or repair state.
package main

import (
	"flag"
	"fmt"
	"os"
)

const version = "0.1.0"

type command struct {
	name string
	help string
	fn   func(args []string) int
}

func main() {
	cmds := []command{
		{"codegen", "Regenerate proto / Go / SQL / keys from .atl files", cmdCodegen},
		{"plan", "Stage a migration from the current .atl file set", cmdPlan},
		{"approve", "Promote a staged migration into migrations/", cmdApprove},
		{"lint", "Parse + lower every .pc; exit 0 iff clean", cmdLint},
		{"migrate-up", "Run golang-migrate up against $PG_URL", cmdMigrateUp},
		{"migrate-down", "Run golang-migrate down 1 against $PG_URL", cmdMigrateDown},
		{"dev", "Codegen + build + exec server from atlantis.dev.yaml (local iteration)", cmdDev},
		{"adopt", "Verify the live DB matches the declared .atl files and seed the IR checkpoint as the baseline", cmdAdopt},
		{"history", "Show schema version history", cmdHistory},
		{"blame", "Show per-field provenance for an entity", cmdBlame},
		{"owners", "Show entity ownership map", cmdOwners},
		{"rollback", "Revert to a prior schema version", cmdRollback},
		{"version", "Print tidectl version", cmdVersion},
	}

	if len(os.Args) < 2 {
		printUsage(cmds)
		os.Exit(2)
	}

	sub := os.Args[1]
	for _, c := range cmds {
		if c.name == sub {
			os.Exit(c.fn(os.Args[2:]))
		}
	}
	fmt.Fprintf(os.Stderr, "tidectl: unknown subcommand %q\n\n", sub)
	printUsage(cmds)
	os.Exit(2)
}

func printUsage(cmds []command) {
	fmt.Fprintln(os.Stderr, "usage: tidectl <subcommand> [args...]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "subcommands:")
	for _, c := range cmds {
		fmt.Fprintf(os.Stderr, "  %-14s %s\n", c.name, c.help)
	}
}

func cmdVersion(_ []string) int {
	fmt.Println("tidectl", version)
	return 0
}

// flagSet is a small wrapper that always prefixes the subcommand into the
// usage message so `tidectl plan -h` reads naturally.
func flagSet(sub string) *flag.FlagSet {
	fs := flag.NewFlagSet("tidectl "+sub, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
