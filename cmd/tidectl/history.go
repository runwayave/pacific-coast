package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cliout"
)

type getSchemaHistoryRequest struct {
	Limit  int32  `json:"limit,omitempty"`
	Before int64  `json:"before,omitempty"`
	Caller string `json:"caller,omitempty"`
}

type schemaVersionSummary struct {
	Version     int64  `json:"version"`
	Caller      string `json:"caller"`
	PlanClass   string `json:"plan_class"`
	EventType   string `json:"event_type"`
	ChangeCount int    `json:"change_count"`
	CreatedAt   string `json:"created_at"`
}

type getSchemaHistoryResponse struct {
	Versions []schemaVersionSummary `json:"versions"`
	HasMore  bool                   `json:"has_more"`
}

// cmdHistory — `tidectl history [--limit N] [--caller X]`
func cmdHistory(args []string) int {
	fs := flagSet("history")
	endpoint := fs.String("endpoint", envDefault("ATL_ENDPOINT", "localhost:9090"), "Admin gRPC endpoint (host:port).")
	tlsCert := fs.String("tls-cert", os.Getenv("ATL_TLS_CERT"), "Client TLS cert (PEM).")
	tlsKey := fs.String("tls-key", os.Getenv("ATL_TLS_KEY"), "Client TLS key (PEM).")
	tlsCA := fs.String("tls-ca", os.Getenv("ATL_TLS_CA"), "Server CA bundle (PEM).")
	limit := fs.Int("limit", 25, "Max rows to return")
	caller := fs.String("caller", "", "Filter by caller name")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dialAdmin(adminDialConfig{
		Endpoint: *endpoint, TLSCert: *tlsCert, TLSKey: *tlsKey, TLSCA: *tlsCA,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl history:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var resp getSchemaHistoryResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/GetSchemaHistory",
		getSchemaHistoryRequest{Limit: int32(*limit), Caller: *caller}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tidectl history:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tidectl history:", err)
			return 3
		}
	case "table":
		printHistoryTable(resp)
	default:
		fmt.Fprintf(os.Stderr, "tidectl history: unknown --format %q\n", *format)
		return 3
	}
	return 0
}

func printHistoryTable(resp getSchemaHistoryResponse) {
	if len(resp.Versions) == 0 {
		fmt.Println(cliout.Grey("(no schema versions)"))
		return
	}
	fmt.Printf("%-8s %-16s %-14s %-10s %-8s %s\n",
		cliout.Bold("VERSION"), cliout.Bold("CALLER"),
		cliout.Bold("CLASS"), cliout.Bold("EVENT"),
		cliout.Bold("CHANGES"), cliout.Bold("CREATED"))
	for _, v := range resp.Versions {
		event := colorEventType(v.EventType)
		fmt.Printf("%-8d %-16s %-14s %-10s %-8d %s\n",
			v.Version, v.Caller, v.PlanClass, event, v.ChangeCount, cliout.Grey(v.CreatedAt))
	}
	if resp.HasMore {
		fmt.Println(cliout.Grey("(more rows available — use --limit or rerun with cursor)"))
	}
}

func colorEventType(s string) string {
	switch s {
	case "apply":
		return cliout.Green(s)
	case "rollback":
		return cliout.Yellow(s)
	case "adopt":
		return cliout.Cyan(s)
	case "seed":
		return cliout.Grey(s)
	}
	return s
}
