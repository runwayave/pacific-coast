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

type startWorkflowRequest struct {
	WorkflowName string          `json:"WorkflowName"`
	State        json.RawMessage `json:"State,omitempty"`
	SubmittedBy  string          `json:"SubmittedBy,omitempty"`
}

type startWorkflowResponse struct {
	WorkflowID string `json:"WorkflowID"`
}

type getWorkflowStatusRequest struct {
	WorkflowID string `json:"WorkflowID"`
}

type workflowStatus struct {
	WorkflowID   string `json:"WorkflowID"`
	WorkflowName string `json:"WorkflowName"`
	Status       string `json:"Status"`
	CurrentStep  string `json:"CurrentStep,omitempty"`
	StartedAt    string `json:"StartedAt"`
	CompletedAt  string `json:"CompletedAt,omitempty"`
	ErrorMsg     string `json:"ErrorMsg,omitempty"`
	SubmittedBy  string `json:"SubmittedBy,omitempty"`
}

type getWorkflowStatusResponse struct {
	Found    bool           `json:"Found"`
	Workflow workflowStatus `json:"Workflow,omitempty"`
}

func cmdWorkflow(args []string) int {
	if len(args) < 1 {
		printWorkflowUsage()
		return 2
	}
	switch args[0] {
	case "start":
		return cmdWorkflowStart(args[1:])
	case "status":
		return cmdWorkflowStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "tide workflow: unknown subcommand %q\n", args[0])
		printWorkflowUsage()
		return 2
	}
}

func printWorkflowUsage() {
	fmt.Fprintln(os.Stderr, "usage: tide workflow start <workflow-name> [--state=JSON]")
	fmt.Fprintln(os.Stderr, "       tide workflow status <workflow-id>")
}

func cmdWorkflowStart(args []string) int {
	fs := flag.NewFlagSet("workflow start", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	stateJSON := fs.String("state", "{}", "State as a JSON object")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "tide workflow start: missing workflow-name")
		return 2
	}
	wfName := fs.Arg(0)

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
	var resp startWorkflowResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/StartWorkflow",
		startWorkflowRequest{
			WorkflowName: wfName,
			State:        json.RawMessage(*stateJSON),
			SubmittedBy:  "cli:" + principal,
		}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide workflow start:", err)
		return 3
	}
	cliout.Successf("started %s as workflow %s", cliout.Bold(wfName), cliout.Bold(resp.WorkflowID))
	fmt.Printf("       monitor with: %s\n", cliout.Bold("tide workflow status "+resp.WorkflowID))
	return 0
}

func cmdWorkflowStatus(args []string) int {
	fs := flag.NewFlagSet("workflow status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "tide workflow status: missing workflow-id")
		return 2
	}
	wfID := fs.Arg(0)

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

	var resp getWorkflowStatusResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/GetWorkflowStatus",
		getWorkflowStatusRequest{WorkflowID: wfID}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide workflow status:", err)
		return 3
	}
	if !resp.Found {
		cliout.Errorf("workflow %s not found", wfID)
		return 1
	}
	switch *format {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(resp.Workflow); err != nil {
			fmt.Fprintln(os.Stderr, "tide workflow status:", err)
			return 3
		}
	case "table":
		printWorkflowStatusRow(resp.Workflow)
	}
	if resp.Workflow.Status == "failed" {
		return 1
	}
	return 0
}

func printWorkflowStatusRow(w workflowStatus) {
	fmt.Printf("%s       %s\n", cliout.Grey("workflow-id"), cliout.Bold(w.WorkflowID))
	fmt.Printf("%s     %s\n", cliout.Grey("workflow-name"), w.WorkflowName)
	fmt.Printf("%s           %s\n", cliout.Grey("status"), colorWorkflowStatus(w.Status))
	if w.CurrentStep != "" {
		fmt.Printf("%s     %s\n", cliout.Grey("current-step"), cliout.Cyan(w.CurrentStep))
	}
	fmt.Printf("%s          %s\n", cliout.Grey("started"), w.StartedAt)
	if w.CompletedAt != "" {
		fmt.Printf("%s        %s\n", cliout.Grey("completed"), w.CompletedAt)
	}
	if w.ErrorMsg != "" {
		fmt.Printf("%s            %s\n", cliout.Red("error"), cliout.Red(w.ErrorMsg))
	}
}

func colorWorkflowStatus(s string) string {
	switch s {
	case "complete":
		return cliout.Green(s)
	case "running", "completing", "compensating":
		return cliout.Yellow(s)
	case "failed", "cancelled":
		return cliout.Red(cliout.Bold(s))
	}
	return s
}
