package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cliout"
)

type rollbackSchemaRequest struct {
	ToVersion int64  `json:"to_version"`
	Caller    string `json:"caller"`
}

type rollbackSchemaResponse struct {
	NewVersion int64  `json:"new_version"`
	UpSQL      string `json:"up_sql"`
}

// cmdRollback — `tide rollback --to=<version> [--dry-run] [--yes]`
//
// Reverts the live schema to the state captured by a prior version.
// The server diffs current -> target, emits the migration SQL, and
// applies it in a single transaction.
//
// --dry-run prints the SQL that would run but does not apply.
// --yes skips the confirmation prompt.
func cmdRollback(args []string) int {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	toVersion := fs.Int64("to", 0, "Target schema version to rollback to (required)")
	dryRun := fs.Bool("dry-run", false, "Print the SQL without executing")
	yes := fs.Bool("yes", false, "Skip confirmation prompt")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *toVersion <= 0 {
		fmt.Fprintln(os.Stderr, "tide rollback: --to=<version> is required")
		return 2
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}

	if *dryRun {
		// In dry-run mode, use DiffSchemaVersions to show what would happen
		// without actually loading the target version vs current. We use
		// GetSchemaHistory to find the current version, then diff.
		return rollbackDryRun(cfg, *toVersion, *timeout)
	}

	if !*yes {
		fmt.Fprintf(os.Stderr, "Rolling back to schema version %d. This will modify the live database.\n", *toVersion)
		fmt.Fprint(os.Stderr, "Continue? [y/N] ")
		var answer string
		if _, err := fmt.Scanln(&answer); err != nil || (answer != "y" && answer != "Y") {
			fmt.Fprintln(os.Stderr, "aborted")
			return 1
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var resp rollbackSchemaResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/RollbackSchema",
		rollbackSchemaRequest{
			ToVersion: *toVersion,
			Caller:    cfg.Caller,
		}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide rollback:", err)
		return 3
	}

	cliout.Successf("rolled back to version %d. New version: %s",
		*toVersion, cliout.Bold(fmt.Sprintf("%d", resp.NewVersion)))
	if resp.UpSQL != "" {
		fmt.Println()
		fmt.Println(cliout.Grey("Applied SQL:"))
		fmt.Println(resp.UpSQL)
	}
	return 0
}

func rollbackDryRun(cfg *tideConfig, toVersion int64, timeout time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	// Find current version from history.
	var histResp getSchemaHistoryResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/GetSchemaHistory",
		getSchemaHistoryRequest{Limit: 1}, &histResp); err != nil {
		fmt.Fprintln(os.Stderr, "tide rollback:", err)
		return 3
	}
	if len(histResp.Versions) == 0 {
		cliout.Errorf("no schema versions found")
		return 1
	}
	currentVersion := histResp.Versions[0].Version

	var diffResp diffSchemaVersionsResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/DiffSchemaVersions",
		diffSchemaVersionsRequest{
			FromVersion: currentVersion,
			ToVersion:   toVersion,
		}, &diffResp); err != nil {
		fmt.Fprintln(os.Stderr, "tide rollback:", err)
		return 3
	}

	fmt.Printf("%s v%d -> v%d\n\n",
		cliout.Bold("Rollback preview:"), currentVersion, toVersion)

	var d struct {
		Additive         []json.RawMessage `json:"additive"`
		BackfillRequired []json.RawMessage `json:"backfill_required"`
		Breaking         []json.RawMessage `json:"breaking"`
	}
	if err := json.Unmarshal(diffResp.Diff, &d); err == nil {
		total := len(d.Additive) + len(d.BackfillRequired) + len(d.Breaking)
		if total == 0 {
			fmt.Println(cliout.Grey("(no changes — schemas are identical)"))
			return 0
		}
		fmt.Printf("%d change(s) would be applied.\n", total)
	}
	fmt.Println(cliout.Grey("(use without --dry-run to execute)"))
	return 0
}
