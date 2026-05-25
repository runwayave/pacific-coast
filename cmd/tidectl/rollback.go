package main

import (
	"context"
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

// cmdRollback — `tidectl rollback --to=<version> [--caller X] [--yes]`
func cmdRollback(args []string) int {
	fs := flagSet("rollback")
	endpoint := fs.String("endpoint", envDefault("ATL_ENDPOINT", "localhost:9090"), "Admin gRPC endpoint (host:port).")
	tlsCert := fs.String("tls-cert", os.Getenv("ATL_TLS_CERT"), "Client TLS cert (PEM).")
	tlsKey := fs.String("tls-key", os.Getenv("ATL_TLS_KEY"), "Client TLS key (PEM).")
	tlsCA := fs.String("tls-ca", os.Getenv("ATL_TLS_CA"), "Server CA bundle (PEM).")
	toVersion := fs.Int64("to", 0, "Target schema version to rollback to (required)")
	caller := fs.String("caller", "tidectl", "Caller identity for the rollback event")
	yes := fs.Bool("yes", false, "Skip confirmation prompt")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *toVersion <= 0 {
		fmt.Fprintln(os.Stderr, "tidectl rollback: --to=<version> is required")
		return 2
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
	client, err := dialAdmin(adminDialConfig{
		Endpoint: *endpoint, TLSCert: *tlsCert, TLSKey: *tlsKey, TLSCA: *tlsCA,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl rollback:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var resp rollbackSchemaResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/RollbackSchema",
		rollbackSchemaRequest{
			ToVersion: *toVersion,
			Caller:    *caller,
		}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tidectl rollback:", err)
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
