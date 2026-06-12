package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// SubmittedFile mirrors internal/server/admin.SubmittedFile. We don't
// import the server package because the CLI must stay a leaf module (no
// transitive deps on pgx, etc.). The JSON envelope is shared on the wire.
type SubmittedFile struct {
	Path    string `json:"Path"`
	Content []byte `json:"Content"`
}

type planRequest struct {
	Caller string          `json:"Caller"`
	Files  []SubmittedFile `json:"Files"`
}

type impactEntry struct {
	Caller   string `json:"caller"`
	Affected bool   `json:"affected"`
	Detail   string `json:"detail"`
}

type planResponse struct {
	PlanID         string        `json:"plan_id"`
	Class          string        `json:"class"`
	UpSQL          string        `json:"up_sql"`
	DownSQL        string        `json:"down_sql"`
	ImpactReport   []impactEntry `json:"impact_report"`
	ParseErrors    []string      `json:"parse_errors"`
	BreakingDetail []string      `json:"breaking_detail"`
	CheckpointHash string        `json:"checkpoint_hash,omitempty"`

	// CustomSQLErrors surface failures from the server-side pg_query_go
	// validator. Server returns class="unparseable" when this list is
	// non-empty; without surfacing the messages, "unparseable" gives the
	// operator nothing to act on.
	CustomSQLErrors []string `json:"custom_sql_errors,omitempty"`

	PreBackfillUpSQL       string             `json:"pre_backfill_up_sql,omitempty"`
	PreBackfillIndexesSQL  string             `json:"pre_backfill_indexes_sql,omitempty"`
	PostBackfillUpSQL      string             `json:"post_backfill_up_sql,omitempty"`
	PostBackfillIndexesSQL string             `json:"post_backfill_indexes_sql,omitempty"`
	BackfillFields         []backfillFieldRef `json:"backfill_fields,omitempty"`

	Extensions []extensionStatus `json:"extensions,omitempty"`

	IndexDrift      []indexDriftItem `json:"index_drift,omitempty"`
	IndexDriftNotes []string         `json:"index_drift_notes,omitempty"`
	IndexDriftError string           `json:"index_drift_error,omitempty"`
}

// indexDriftItem mirrors introspect.UniqueIndexDrift over the JSON wire.
type indexDriftItem struct {
	Schema    string   `json:"schema"`
	Table     string   `json:"table"`
	IndexName string   `json:"index_name"`
	Columns   []string `json:"columns"`
	Partial   bool     `json:"partial,omitempty"`
	Predicate string   `json:"predicate,omitempty"`
}

type extensionStatus struct {
	Name        string `json:"name"`
	Trigger     string `json:"trigger"`
	Action      string `json:"action"` // ok | enable | missing
	InstallHint string `json:"install_hint,omitempty"`
}

type backfillFieldRef struct {
	EntityID   string `json:"EntityID"`
	Field      string `json:"Field"`
	Expression string `json:"Expression"`
	PKColumn   string `json:"PKColumn"`
	TableName  string `json:"TableName"`
}

type beginBackfillRequest struct {
	Caller                 string             `json:"Caller"`
	PlanID                 string             `json:"PlanID"`
	Files                  []SubmittedFile    `json:"Files"`
	PreBackfillUpSQL       string             `json:"PreBackfillUpSQL"`
	PreBackfillIndexesSQL  string             `json:"PreBackfillIndexesSQL"`
	PostBackfillUpSQL      string             `json:"PostBackfillUpSQL"`
	PostBackfillIndexesSQL string             `json:"PostBackfillIndexesSQL"`
	BackfillFields         []backfillFieldRef `json:"BackfillFields"`
}

type beginBackfillResponse struct {
	PlanHash        string `json:"PlanHash"`
	Accepted        bool   `json:"Accepted"`
	AlreadyRunning  bool   `json:"AlreadyRunning"`
	AlreadyComplete bool   `json:"AlreadyComplete"`
	Message         string `json:"Message"`
}

type applyRequest struct {
	Caller         string          `json:"Caller"`
	PlanID         string          `json:"PlanID"`
	UpSQL          string          `json:"UpSQL"`
	Files          []SubmittedFile `json:"Files"`
	CheckpointHash string          `json:"CheckpointHash,omitempty"`
}

type applyResponse struct {
	AppliedAt   string `json:"applied_at"`
	Version     int64  `json:"version,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

// Exit codes:
//
//	0 — plan applied (or dry-run / no changes).
//	1 — backfill required.
//	2 — breaking changes; need a atlantis PR.
//	3 — operational error (parse error, network failure, etc).
//
// cmdApply is the main user touchpoint for tide apply.
func cmdApply(args []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	backfill := fs.Bool("backfill", false, "Kick off the declarative backfill flow for a backfill_required plan (calls BeginBackfillPlan; monitor with `tide backfill status`)")
	dryRun := fs.Bool("dry-run", false, "Plan only; do not apply")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	noPull := fs.Bool("no-pull", false, "Skip the pre-apply `tide pull` refresh of .tide-cache/")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
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

	// Refresh the local merged-schema cache so cross-caller references the
	// CLI is about to validate (e.g., `references vendor.Product.id` from a
	// new backend entity) resolve against the latest server-side state. A
	// network failure here is non-fatal; the server's planning RPC has the
	// definitive view.
	if !*noPull {
		pullBeforeApply(ctx, cfg)
	}

	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var planResp planResponse
	err = client.invoke(ctx, "/atlantis.admin.v1.Admin/PlanSchema",
		planRequest{Caller: cfg.Caller, Files: files}, &planResp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide plan:", err)
		return 3
	}

	// Print parse / lower errors and bail. The server bundles DSL parse
	// errors and IR-lowering errors (missing FK targets, etc.) into the
	// same field; the message distinguishes them so a "references unknown
	// entity vendor.Foo" error doesn't send you hunting for syntax issues
	// when really vendor-platform just hasn't run `tide apply` yet.
	if len(planResp.ParseErrors) > 0 {
		fmt.Fprintln(os.Stderr, "tide: schema validation failed:")
		for _, e := range planResp.ParseErrors {
			fmt.Fprintln(os.Stderr, "  ", e)
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "If errors mention 'references unknown entity vendor.X' or 'consumer.X',")
		fmt.Fprintln(os.Stderr, "the *other* caller hasn't registered its .atl files yet — run `tide apply`")
		fmt.Fprintln(os.Stderr, "from that caller's repo first.")
		return 3
	}

	printImpactReport(planResp)

	switch planResp.Class {
	case "additive":
		if *dryRun {
			fmt.Println("tide: additive plan; would apply (--dry-run set)")
			return 0
		}
		return doApply(ctx, client, cfg, planResp, files)

	case "backfill_required":
		if !*backfill {
			fmt.Fprintln(os.Stderr, "tide: this change is backfill-required.")
			if len(planResp.BackfillFields) > 0 {
				fmt.Fprintln(os.Stderr, "    Declared backfills:")
				for _, f := range planResp.BackfillFields {
					fmt.Fprintf(os.Stderr, "      %s.%s ← %s\n", f.EntityID, f.Field, f.Expression)
				}
				fmt.Fprintln(os.Stderr, "    Re-run with --backfill to kick off the chunked backfill.")
			} else {
				fmt.Fprintln(os.Stderr, "    No `backfill \"<expr>\"` modifiers declared on the relevant fields —")
				fmt.Fprintln(os.Stderr, "    add one in your .atl files and re-plan, or apply the backfill out of band.")
			}
			return 1
		}
		return doBeginBackfill(ctx, client, cfg, planResp, files)

	case "cross_caller_breaking":
		fmt.Fprintln(os.Stderr, "tide: this change is breaking other callers:")
		for _, d := range planResp.BreakingDetail {
			fmt.Fprintln(os.Stderr, "  ", d)
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "tide: open a PR in the atlantis repo to coordinate the change.")
		return 2

	case "unparseable":
		// Server marks the plan unparseable when pg_query_go validation on
		// any custom-query SQL body fails. Surface the actual failures so
		// the operator can fix the .atl file without spelunking the server logs.
		fmt.Fprintln(os.Stderr, "tide: plan is unparseable — custom-query SQL validation failed.")
		if len(planResp.CustomSQLErrors) > 0 {
			fmt.Fprintln(os.Stderr, "")
			for _, e := range planResp.CustomSQLErrors {
				fmt.Fprintln(os.Stderr, "  ", e)
			}
		}
		if len(planResp.ParseErrors) > 0 {
			fmt.Fprintln(os.Stderr, "")
			for _, e := range planResp.ParseErrors {
				fmt.Fprintln(os.Stderr, "  ", e)
			}
		}
		return 3

	default:
		fmt.Fprintf(os.Stderr, "tide: unknown plan class %q\n", planResp.Class)
		return 3
	}
}

// doBeginBackfill submits the phase-split scripts + field driver list to
// the server's BeginBackfillPlan RPC. The server runs Phase 1 inline
// (additive parts + nullable ADD COLUMN), records the backfill_plan +
// backfill_field_state rows, and returns immediately. The background
// worker picks up the field rows and runs the chunked UPDATE loop;
// operators monitor with `tide backfill status`.
func doBeginBackfill(ctx context.Context, client *adminClient, cfg *tideConfig, plan planResponse, files []SubmittedFile) int {
	if len(plan.BackfillFields) == 0 {
		fmt.Fprintln(os.Stderr, "tide: --backfill set but no fields declare `backfill \"<expr>\"`.")
		fmt.Fprintln(os.Stderr, "    Add the modifier in your .atl files (see docs), re-plan, then re-run.")
		return 1
	}
	req := beginBackfillRequest{
		Caller:                 cfg.Caller,
		PlanID:                 plan.PlanID,
		Files:                  files,
		PreBackfillUpSQL:       plan.PreBackfillUpSQL,
		PreBackfillIndexesSQL:  plan.PreBackfillIndexesSQL,
		PostBackfillUpSQL:      plan.PostBackfillUpSQL,
		PostBackfillIndexesSQL: plan.PostBackfillIndexesSQL,
		BackfillFields:         plan.BackfillFields,
	}
	var resp beginBackfillResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/BeginBackfillPlan", req, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide backfill:", err)
		return 3
	}
	switch {
	case resp.AlreadyComplete:
		cliout.Successf("backfill already complete (plan-hash=%s)", cliout.Bold(resp.PlanHash))
		return 0
	case resp.AlreadyRunning:
		cliout.Infof("backfill already in flight (plan-hash=%s)", cliout.Bold(resp.PlanHash))
		fmt.Printf("      monitor with: %s %s\n", cliout.Bold("tide backfill status"), resp.PlanHash)
		return 0
	case resp.Accepted:
		cliout.Successf("backfill accepted (plan-hash=%s)", cliout.Bold(resp.PlanHash))
		if resp.Message != "" {
			fmt.Printf("      %s\n", cliout.Grey(resp.Message))
		}
		fmt.Printf("      monitor with: %s %s\n", cliout.Bold("tide backfill status"), resp.PlanHash)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "tide backfill: unexpected response: %+v\n", resp)
		return 3
	}
}

func doApply(ctx context.Context, client *adminClient, cfg *tideConfig, plan planResponse, files []SubmittedFile) int {
	var applyResp applyResponse
	err := client.invoke(ctx, "/atlantis.admin.v1.Admin/ApplyMigration",
		applyRequest{
			Caller:         cfg.Caller,
			PlanID:         plan.PlanID,
			UpSQL:          plan.UpSQL,
			Files:          files,
			CheckpointHash: plan.CheckpointHash,
		}, &applyResp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide apply:", err)
		return 3
	}
	cliout.Successf("applied at %s", applyResp.AppliedAt)
	if applyResp.ContentHash != "" {
		cliout.Field(os.Stdout, "content", applyResp.ContentHash[:12])
	}
	if cfg.OutputDir != "" {
		cliout.Infof("regenerate the typed client under %s with `tide generate`", cfg.OutputDir)
	}
	return 0
}

func printImpactReport(p planResponse) {
	if len(p.ImpactReport) > 0 {
		cliout.Header(os.Stdout, "impact")
		for _, e := range p.ImpactReport {
			if e.Affected {
				cliout.Row(os.Stdout, "warn", cliout.Bold(e.Caller), e.Detail)
			} else {
				cliout.Row(os.Stdout, "muted", cliout.Faint(e.Caller), e.Detail)
			}
		}
		fmt.Println()
	}
	if len(p.Extensions) > 0 {
		printExtensions(p.Extensions)
		fmt.Println()
	}
	printIndexDrift(p)
}

// printIndexDrift surfaces live UNIQUE indexes the schema doesn't declare.
// These don't change the plan class, but `tide apply` will refuse on them
// unless ATLANTIS_ALLOW_INDEX_DRIFT=1 — so the warning is the operator's
// heads-up before they commit to applying.
func printIndexDrift(p planResponse) {
	if p.IndexDriftError != "" {
		cliout.Header(os.Stdout, "index drift")
		cliout.Row(os.Stdout, "warn", "check skipped", p.IndexDriftError)
		fmt.Println()
		return
	}
	if len(p.IndexDrift) == 0 && len(p.IndexDriftNotes) == 0 {
		return
	}
	cliout.Header(os.Stdout, "index drift")
	for _, d := range p.IndexDrift {
		desc := "(" + strings.Join(d.Columns, ", ") + ")"
		if d.Partial {
			desc += " WHERE " + d.Predicate
		}
		cliout.Row(os.Stdout, "coral", d.Schema+"."+d.IndexName, "undeclared UNIQUE on "+desc)
		cliout.SubRow(os.Stdout, fmt.Sprintf(`DROP INDEX "%s"."%s";  (or ATLANTIS_ALLOW_INDEX_DRIFT=1 to apply anyway)`, d.Schema, d.IndexName))
	}
	for _, n := range p.IndexDriftNotes {
		cliout.Row(os.Stdout, "muted", "note", n)
	}
	fmt.Println()
}

// collectPCFiles walks every schema path and reads every .atl file. Paths
// are stored relative to the caller's repo root so the server's error
// messages are useful in the caller's context.
func collectPCFiles(paths []string) ([]SubmittedFile, error) {
	var out []SubmittedFile
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".atl" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out = append(out, SubmittedFile{Path: path, Content: data})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	return out, nil
}
