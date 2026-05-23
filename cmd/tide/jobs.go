package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cliout"
)

type submitJobRequest struct {
	JobName     string          `json:"JobName"`
	Args        json.RawMessage `json:"Args,omitempty"`
	ScheduledAt string          `json:"ScheduledAt,omitempty"`
	SubmittedBy string          `json:"SubmittedBy,omitempty"`
}

type submitJobResponse struct {
	JobID string `json:"JobID"`
}

type getJobStatusRequest struct {
	JobID string `json:"JobID"`
}

type jobStatus struct {
	JobID        string          `json:"JobID"`
	JobName      string          `json:"JobName"`
	Queue        string          `json:"Queue"`
	Args         json.RawMessage `json:"Args"`
	Status       string          `json:"Status"`
	Attempts     int             `json:"Attempts"`
	MaxRetries   int             `json:"MaxRetries"`
	LastError    string          `json:"LastError,omitempty"`
	LastErrorAt  string          `json:"LastErrorAt,omitempty"`
	ScheduledFor string          `json:"ScheduledFor"`
	StartedAt    string          `json:"StartedAt,omitempty"`
	CompletedAt  string          `json:"CompletedAt,omitempty"`
	EnqueuedAt   string          `json:"EnqueuedAt"`
	SubmittedBy  string          `json:"SubmittedBy,omitempty"`
}

type getJobStatusResponse struct {
	Found bool      `json:"Found"`
	Job   jobStatus `json:"Job,omitempty"`
}

type listDeadJobsRequest struct {
	JobName string `json:"JobName,omitempty"`
	Limit   int    `json:"Limit,omitempty"`
}

type listDeadJobsResponse struct {
	Jobs []jobStatus `json:"Jobs"`
}

type retryDeadJobRequest struct {
	JobID string `json:"JobID"`
}

type retryDeadJobResponse struct {
	JobID string `json:"JobID"`
}

// cmdJob is the operator escape hatch for the declarative-job runtime.
// The 95% submission path is the typed Go SDK (a generated
// `client.Submit<Job>` once it lands); this CLI handles ad-hoc ops:
// kick off a one-shot job, inspect a stuck row, drain the DLQ.
//
//	tide job submit <job-name> [--args=JSON]
//	tide job status <job-id>
//	tide job dead [--job-name=...] [--limit=N]
//	tide job retry <dead-job-id>
func cmdJob(args []string) int {
	if len(args) < 1 {
		printJobUsage()
		return 2
	}
	switch args[0] {
	case "submit":
		return cmdJobSubmit(args[1:])
	case "status":
		return cmdJobStatus(args[1:])
	case "dead":
		return cmdJobDead(args[1:])
	case "retry":
		return cmdJobRetry(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "tide job: unknown subcommand %q\n", args[0])
		printJobUsage()
		return 2
	}
}

func printJobUsage() {
	fmt.Fprintln(os.Stderr, "usage: tide job submit <job-name> [--args=JSON] [--scheduled-at=RFC3339]")
	fmt.Fprintln(os.Stderr, "       tide job status <job-id>")
	fmt.Fprintln(os.Stderr, "       tide job dead [--job-name=...] [--limit=N]")
	fmt.Fprintln(os.Stderr, "       tide job retry <dead-job-id>")
}

func cmdJobSubmit(args []string) int {
	fs := flag.NewFlagSet("job submit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	argsJSON := fs.String("args", "{}", "Args as a JSON object")
	scheduledAt := fs.String("scheduled-at", "", "Defer execution until this RFC3339 time")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "tide job submit: missing job-name")
		return 2
	}
	jobName := fs.Arg(0)

	// Validate args parses as JSON before sending — surfacing the
	// error here gives a precise position the caller can fix.
	var probe json.RawMessage
	if err := json.Unmarshal([]byte(*argsJSON), &probe); err != nil {
		fmt.Fprintf(os.Stderr, "tide job submit: --args is not valid JSON: %v\n", err)
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	principal := os.Getenv("USER")
	if principal == "" {
		principal = "tide"
	}
	var resp submitJobResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/SubmitJob",
		submitJobRequest{
			JobName:     jobName,
			Args:        probe,
			ScheduledAt: *scheduledAt,
			SubmittedBy: "cli:" + principal,
		}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide job submit:", err)
		return 3
	}
	cliout.Successf("submitted %s as job %s", cliout.Bold(jobName), cliout.Bold(resp.JobID))
	fmt.Printf("       monitor with: %s\n", cliout.Bold("tide job status "+resp.JobID))
	return 0
}

func cmdJobStatus(args []string) int {
	fs := flag.NewFlagSet("job status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "tide job status: missing job-id")
		return 2
	}
	jobID := fs.Arg(0)

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var resp getJobStatusResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/GetJobStatus",
		getJobStatusRequest{JobID: jobID}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide job status:", err)
		return 3
	}
	if !resp.Found {
		cliout.Errorf("job %s not found", jobID)
		return 1
	}
	switch *format {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(resp.Job); err != nil {
			fmt.Fprintln(os.Stderr, "tide job status:", err)
			return 3
		}
	case "table":
		printJobStatusRow(resp.Job)
	default:
		fmt.Fprintf(os.Stderr, "tide job status: unknown --format %q\n", *format)
		return 3
	}
	if resp.Job.Status == "failed" {
		return 1
	}
	return 0
}

func cmdJobDead(args []string) int {
	fs := flag.NewFlagSet("job dead", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	jobName := fs.String("job-name", "", "Filter by canonical job id (namespace.JobName); empty = all")
	limit := fs.Int("limit", 25, "Max rows to return")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var resp listDeadJobsResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/ListDeadJobs",
		listDeadJobsRequest{JobName: *jobName, Limit: *limit}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide job dead:", err)
		return 3
	}
	switch *format {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide job dead:", err)
			return 3
		}
	case "table":
		printDeadJobs(resp.Jobs)
	default:
		fmt.Fprintf(os.Stderr, "tide job dead: unknown --format %q\n", *format)
		return 3
	}
	return 0
}

func cmdJobRetry(args []string) int {
	fs := flag.NewFlagSet("job retry", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "tide job retry: missing dead-job-id")
		return 2
	}
	jobID := fs.Arg(0)

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var resp retryDeadJobResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/RetryDeadJob",
		retryDeadJobRequest{JobID: jobID}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide job retry:", err)
		return 3
	}
	cliout.Successf("requeued dead job %s -> %s", cliout.Bold(jobID), cliout.Bold(resp.JobID))
	fmt.Printf("       monitor with: %s\n", cliout.Bold("tide job status "+resp.JobID))
	return 0
}

// printJobStatusRow renders one job row in the same shape across
// status / dead listings. Status colors mirror the backfill
// convention: green for terminal-good, yellow for in-flight, red
// for failure.
func printJobStatusRow(j jobStatus) {
	fmt.Printf("%s     %s\n", cliout.Grey("job-id"), cliout.Bold(j.JobID))
	fmt.Printf("%s   %s\n", cliout.Grey("job-name"), j.JobName)
	fmt.Printf("%s      %s\n", cliout.Grey("queue"), j.Queue)
	fmt.Printf("%s     %s\n", cliout.Grey("status"), colorJobStatus(j.Status))
	fmt.Printf("%s   %d / %d\n", cliout.Grey("attempts"), j.Attempts, j.MaxRetries)
	fmt.Printf("%s   %s\n", cliout.Grey("enqueued"), j.EnqueuedAt)
	if j.ScheduledFor != "" {
		fmt.Printf("%s  %s\n", cliout.Grey("scheduled"), j.ScheduledFor)
	}
	if j.StartedAt != "" {
		fmt.Printf("%s    %s\n", cliout.Grey("started"), j.StartedAt)
	}
	if j.CompletedAt != "" {
		fmt.Printf("%s  %s\n", cliout.Grey("completed"), j.CompletedAt)
	}
	if j.SubmittedBy != "" {
		fmt.Printf("%s     %s\n", cliout.Grey("submitter"), j.SubmittedBy)
	}
	if j.LastError != "" {
		fmt.Printf("%s     %s %s\n", cliout.Red("error"), cliout.Grey("(at "+j.LastErrorAt+")"), cliout.Red(j.LastError))
	}
	if len(j.Args) > 0 && string(j.Args) != "{}" {
		fmt.Printf("%s       %s\n", cliout.Grey("args"), string(j.Args))
	}
}

// printDeadJobs renders the DLQ list as a compact summary, sorted by
// the inherited moved-at order (the RPC returns DESC).
func printDeadJobs(rows []jobStatus) {
	if len(rows) == 0 {
		fmt.Println(cliout.Grey("(no dead jobs)"))
		return
	}
	// Sort: most recent first. Server already orders DESC, but
	// preserve that here in case a caller batches us with another
	// source.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CompletedAt > rows[j].CompletedAt })
	fmt.Printf("%s %s\n", cliout.Bold(fmt.Sprintf("%d", len(rows))), cliout.Bold("dead job(s):"))
	for _, j := range rows {
		fmt.Printf("  %s %s %s\n",
			cliout.Red("✖"),
			cliout.Bold(j.JobID),
			cliout.Cyan(j.JobName))
		fmt.Printf("    %s  attempts=%d/%d  %s\n",
			cliout.Grey(j.CompletedAt),
			j.Attempts,
			j.MaxRetries,
			cliout.Red(j.LastError))
	}
}

func colorJobStatus(s string) string {
	switch s {
	case "complete":
		return cliout.Green(s)
	case "running", "pending":
		return cliout.Yellow(s)
	case "failed":
		return cliout.Red(cliout.Bold(s))
	}
	return s
}
