// pc — caller-side CLI invoked from inside backend / vendor-platform repos.
//
//	tide apply [--no-pull]                     submit + apply this repo's .atl files
//	tide apply --backfill                      kick off declarative backfill for a backfill_required plan
//	tide apply --dry-run
//	tide plan  [--against URL] [--format FMT]   dry-run plan against a server; no mutation
//	tide pull  [--force]                        refresh .tide-cache from server
//	tide list                                   print every entity in the merged schema
//	tide show  <path-substring>                 print one .atl file from the merged schema
//	tide backfill status [plan-hash]            monitor a backfill kicked off by `tide apply --backfill`
//	tide version
//
// Reads tide.yaml from cwd to discover schema paths + the atlantis
// endpoint. Submits the .atl files to AdminService.PlanSchema. Routes the
// outcome:
//
//   - additive               → ApplyMigration, regenerate local client
//   - backfill_required      → print expected backfill, exit 1
//   - cross_caller_breaking  → print impact report, exit 2 (CLI hints
//     that a PR in atlantis is required)
//
// `tide apply` auto-runs `tide pull` first so cross-caller references resolve
// against the freshest merged schema. Suppress with --no-pull when the
// network is unavailable or the cache is known-current.
//
// Network transport is hand-rolled JSON-over-gRPC; mTLS material is loaded
// from TIDE_TLS_* env vars or tide.yaml. The server side
// (internal/server/admin/grpc.go) speaks the same JSON envelope; this keeps
// `pc` independent of buf-generated stubs.
package main

import (
	"fmt"
	"os"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "apply":
		os.Exit(cmdApply(os.Args[2:]))
	case "plan":
		os.Exit(cmdPlan(os.Args[2:]))
	case "pull":
		os.Exit(cmdPull(os.Args[2:]))
	case "list":
		os.Exit(cmdList(os.Args[2:]))
	case "show":
		os.Exit(cmdShow(os.Args[2:]))
	case "backfill":
		os.Exit(cmdBackfill(os.Args[2:]))
	case "job":
		os.Exit(cmdJob(os.Args[2:]))
	case "workflow":
		os.Exit(cmdWorkflow(os.Args[2:]))
	case "history":
		os.Exit(cmdHistory(os.Args[2:]))
	case "diff":
		os.Exit(cmdDiff(os.Args[2:]))
	case "blame":
		os.Exit(cmdBlame(os.Args[2:]))
	case "owners":
		os.Exit(cmdOwners(os.Args[2:]))
	case "rollback":
		os.Exit(cmdRollback(os.Args[2:]))
	case "version":
		fmt.Println("tide", version)
	default:
		fmt.Fprintf(os.Stderr, "tide: unknown subcommand %q\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: tide apply    [--backfill] [--dry-run] [--no-pull]")
	fmt.Fprintln(os.Stderr, "       tide plan     [--against URL] [--format table|json] [--no-pull]")
	fmt.Fprintln(os.Stderr, "       tide pull     [--force]")
	fmt.Fprintln(os.Stderr, "       tide list")
	fmt.Fprintln(os.Stderr, "       tide show     <path-substring>")
	fmt.Fprintln(os.Stderr, "       tide backfill status [plan-hash]")
	fmt.Fprintln(os.Stderr, "       tide job      submit|status|dead|retry ...")
	fmt.Fprintln(os.Stderr, "       tide workflow start|status ...")
	fmt.Fprintln(os.Stderr, "       tide history  [--limit N] [--caller X]")
	fmt.Fprintln(os.Stderr, "       tide diff     <from-version> <to-version>")
	fmt.Fprintln(os.Stderr, "       tide blame    <entity-id>")
	fmt.Fprintln(os.Stderr, "       tide owners")
	fmt.Fprintln(os.Stderr, "       tide rollback --to=<version> [--dry-run] [--yes]")
	fmt.Fprintln(os.Stderr, "       tide version")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Reads ./tide.yaml for schema paths and atlantis endpoint.")
}
