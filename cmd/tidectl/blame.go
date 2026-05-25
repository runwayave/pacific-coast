package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cliout"
)

type getEntityLineageRequest struct {
	EntityID string `json:"entity_id"`
}

type entityLineageEntry struct {
	EntityID       string `json:"entity_id"`
	FieldName      string `json:"field_name"`
	IntroducedBy   string `json:"introduced_by"`
	IntroducedAt   int64  `json:"introduced_at"`
	LastModifiedBy string `json:"last_modified_by"`
	LastModifiedAt int64  `json:"last_modified_at"`
	RemovedAt      *int64 `json:"removed_at,omitempty"`
}

type getEntityLineageResponse struct {
	Entries []entityLineageEntry `json:"entries"`
}

// cmdBlame — `tidectl blame <entity-id>`
func cmdBlame(args []string) int {
	fs := flagSet("blame")
	endpoint := fs.String("endpoint", envDefault("ATL_ENDPOINT", "localhost:9090"), "Admin gRPC endpoint (host:port).")
	tlsCert := fs.String("tls-cert", os.Getenv("ATL_TLS_CERT"), "Client TLS cert (PEM).")
	tlsKey := fs.String("tls-key", os.Getenv("ATL_TLS_KEY"), "Client TLS key (PEM).")
	tlsCA := fs.String("tls-ca", os.Getenv("ATL_TLS_CA"), "Server CA bundle (PEM).")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: tidectl blame <entity-id>")
		return 2
	}
	entityID := fs.Arg(0)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dialAdmin(adminDialConfig{
		Endpoint: *endpoint, TLSCert: *tlsCert, TLSKey: *tlsKey, TLSCA: *tlsCA,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl blame:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var resp getEntityLineageResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/GetEntityLineage",
		getEntityLineageRequest{EntityID: entityID}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tidectl blame:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tidectl blame:", err)
			return 3
		}
	case "table":
		printBlameTable(entityID, resp)
	default:
		fmt.Fprintf(os.Stderr, "tidectl blame: unknown --format %q\n", *format)
		return 3
	}
	return 0
}

func printBlameTable(entityID string, resp getEntityLineageResponse) {
	if len(resp.Entries) == 0 {
		cliout.Errorf("no lineage found for %s", entityID)
		return
	}
	fmt.Printf("%s %s\n\n", cliout.Bold("Blame:"), cliout.Cyan(entityID))
	fmt.Printf("%-24s %-16s %-8s %-16s %-8s %s\n",
		cliout.Bold("FIELD"), cliout.Bold("INTRODUCED BY"),
		cliout.Bold("AT"), cliout.Bold("MODIFIED BY"),
		cliout.Bold("AT"), cliout.Bold("STATUS"))
	for _, e := range resp.Entries {
		field := e.FieldName
		if field == "" {
			field = cliout.Grey("(entity)")
		}
		status := cliout.Green("active")
		if e.RemovedAt != nil {
			status = cliout.Red(fmt.Sprintf("removed@v%d", *e.RemovedAt))
		}
		fmt.Printf("%-24s %-16s %-8d %-16s %-8d %s\n",
			field, e.IntroducedBy, e.IntroducedAt,
			e.LastModifiedBy, e.LastModifiedAt, status)
	}
}
