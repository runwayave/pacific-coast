package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

// cmdPlan exits with:
//
//	0 — additive
//	1 — backfill required
//	2 — breaking
//	3 — operational error (parse / network / config)
//
// Same code map as cmdApply so the two commands compose in CI workflows
// without per-step translation. The plan RPC is read-only on the server
// side, so a `tide plan` is safe to run from any pre-merge environment
// (including against a production endpoint).
func cmdPlan(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	against := fs.String("against", "", "Server endpoint override (host:port); defaults to tide.yaml's endpoint")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	noPull := fs.Bool("no-pull", false, "Skip the pre-plan refresh of .tide-cache/")
	format := fs.String("format", "table", "Output format: table or json")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	if *against != "" {
		cfg.Endpoint = *against
	}

	files, err := collectPCFiles(cfg.SchemaPaths)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "tide: no .atl files found under %v\n", cfg.SchemaPaths)
		return 3
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Refresh the local cache so cross-caller references resolve against
	// the freshest merged view. Non-fatal on failure: the server is the
	// definitive view anyway, and a stale cache only affects local IDE
	// hints.
	if !*noPull {
		pullBeforeApply(ctx, cfg)
	}

	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var resp planResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/PlanSchema",
		planRequest{Caller: cfg.Caller, Files: files}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide plan:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide plan:", err)
			return 3
		}
	case "table":
		printPlanReport(resp)
	default:
		fmt.Fprintf(os.Stderr, "tide plan: unknown --format %q (want table|json)\n", *format)
		return 3
	}

	if len(resp.ParseErrors) > 0 {
		return 3
	}
	switch resp.Class {
	case "additive":
		return 0
	case "backfill_required":
		return 1
	case "cross_caller_breaking":
		return 2
	default:
		fmt.Fprintf(os.Stderr, "tide plan: unknown plan class %q\n", resp.Class)
		return 3
	}
}

// printPlanReport renders the plan to stdout in a shape friendly to PR
// comments and human review. Order: parse errors first (any other field
// is meaningless if the schema didn't parse), then class, impact, and
// breaking detail.
func printPlanReport(resp planResponse) {
	if len(resp.ParseErrors) > 0 {
		fmt.Println("schema validation failed:")
		for _, e := range resp.ParseErrors {
			fmt.Println("  ", e)
		}
		return
	}
	fmt.Printf("plan_id: %s\n", resp.PlanID)
	fmt.Printf("class:   %s\n", resp.Class)
	if len(resp.ImpactReport) > 0 {
		fmt.Println("impact:")
		for _, e := range resp.ImpactReport {
			mark := " "
			if e.Affected {
				mark = "*"
			}
			fmt.Printf("  %s %-24s %s\n", mark, e.Caller, e.Detail)
		}
	}
	if len(resp.BreakingDetail) > 0 {
		fmt.Println("breaking:")
		for _, d := range resp.BreakingDetail {
			fmt.Println("  ", d)
		}
	}
}
