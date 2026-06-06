// cmd/tide sandbox — user-facing entry point to the schema-true
// in-memory simulator. Two subcommands:
//
//	tide sandbox boot  <path>        # start HTTP server bound to the IR
//	tide sandbox shell <path>        # interactive SQL REPL in-process
//
// path is either a single .atl file or a directory containing .atl
// files. The CLI compiles them via dsl.Parse + dsl.Lower, hands the
// IR to sandbox.New, and either serves HTTP or drops into a REPL.
//
// Boot prints connection info to stdout and blocks until SIGINT. The
// HTTP routes are the same set exercised by internal/runtime/sandbox/http_test.go.
//
// Shell uses the in-process Sandbox directly (no HTTP) so a single
// missing key on the keyboard doesn't time out — the simulator's
// <1ms response budget matters most here.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
)

func cmdSandbox(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "tide sandbox: missing subcommand")
		fmt.Fprintln(os.Stderr, "  tide sandbox boot  <path>      start HTTP control plane")
		fmt.Fprintln(os.Stderr, "  tide sandbox shell <path>      interactive SQL REPL")
		return 2
	}
	switch args[0] {
	case "boot":
		return cmdSandboxBoot(args[1:])
	case "shell":
		return cmdSandboxShell(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "tide sandbox: unknown subcommand %q\n", args[0])
		return 2
	}
}

// cmdSandboxBoot starts the HTTP control plane bound to the IR at
// `path`. Default address is 127.0.0.1:0 (kernel-chosen port); pass
// --addr to pin one.
func cmdSandboxBoot(args []string) int {
	fs := flag.NewFlagSet("sandbox boot", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:0", "host:port to bind")
	seed := fs.Int64("seed", 0, "seed for StrictDeterministic mode (0 = wall clock)")
	strict := fs.Bool("strict", false, "enable StrictDeterministic")
	backendFlag := fs.String("backend", "auto", "sim | embedded | auto (auto picks embedded for IRs with custom query/procedure/hypertable blocks)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "tide sandbox boot: missing schema path")
		return 2
	}
	ir, err := loadSchemaIR(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide sandbox boot: %v\n", err)
		return 1
	}

	srv := sandbox.NewServer()
	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler())

	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide sandbox boot: listen: %v\n", err)
		return 1
	}
	defer listener.Close()

	backend, reason, err := resolveBackend(*backendFlag, ir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide sandbox boot: %v\n", err)
		return 2
	}

	// Auto-create one sandbox with the loaded IR so the user can hit
	// /v1/sandbox/1 without an upfront POST. Print its id alongside
	// the URL.
	bootStart := time.Now()
	sb, err := sandbox.New(sandbox.Options{
		IR:          ir,
		Backend:     backend,
		Seed:        *seed,
		Determinism: determinismFromBool(*strict),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide sandbox boot: new: %v\n", err)
		return 1
	}
	id := srv.Register(sb)

	url := fmt.Sprintf("http://%s", listener.Addr().String())
	fmt.Printf("atlantis sandbox running at %s\n", url)
	fmt.Printf("  pre-created sandbox id: %s\n", id)
	fmt.Printf("  entities: %d\n", len(ir.Entities))
	fmt.Printf("  backend: %s (%s, booted in %s)\n", describeBackend(sb), reason, time.Since(bootStart).Round(time.Millisecond))
	fmt.Println("  endpoints:")
	fmt.Println("    POST   /v1/sandbox                 create from IR JSON")
	fmt.Println("    DELETE /v1/sandbox/{id}            destroy")
	fmt.Println("    POST   /v1/sandbox/{id}/sql/query  body {sql, args}")
	fmt.Println("    POST   /v1/sandbox/{id}/sql/exec   body {sql, args}")
	fmt.Println("    GET    /v1/sandbox/{id}/inspect/describe?q=…")
	fmt.Println("    GET    /v1/sandbox/{id}/inspect/sample?q=…&n=…")
	fmt.Println("    POST   /v1/sandbox/{id}/inspect/find")
	fmt.Println("    POST   /v1/sandbox/{id}/mark / /restore / /fork")
	fmt.Println("    GET/PUT /v1/sandbox/{id}/snapshot")
	fmt.Println("    POST   /v1/sandbox/{id}/fixtures/bulk")
	fmt.Println()
	fmt.Println("press Ctrl-C to stop")

	// Graceful shutdown on SIGINT/SIGTERM.
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigC
		fmt.Println("\nshutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "tide sandbox boot: serve: %v\n", err)
		return 1
	}
	return 0
}

// cmdSandboxShell opens an interactive SQL REPL backed by an in-
// process sandbox. The REPL accepts:
//
//   - SQL statements (terminated by newline; semicolon optional)
//   - .describe <qualified>     print entity description
//   - .sample <qualified> [n]   print sample rows
//   - .tables                   list registered entities
//   - .quit                     exit
//
// Inputs that look like SELECT/RETURNING route through Pool.Query;
// everything else through Pool.Exec.
func cmdSandboxShell(args []string) int {
	fs := flag.NewFlagSet("sandbox shell", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "tide sandbox shell: missing schema path")
		return 2
	}
	ir, err := loadSchemaIR(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide sandbox shell: %v\n", err)
		return 1
	}
	sb, err := sandbox.New(sandbox.Options{IR: ir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide sandbox shell: %v\n", err)
		return 1
	}
	defer sb.Close()

	fmt.Println("atlantis sandbox shell — in-memory schema-true REPL")
	fmt.Println("  .tables                          list entities")
	fmt.Println("  .describe <qualified>            entity schema")
	fmt.Println("  .sample <qualified> [n]          first n rows")
	fmt.Println("  .quit                            exit")
	fmt.Printf("  loaded %d entities\n\n", len(ir.Entities))

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for {
		fmt.Print("sandbox> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == ".quit" || line == ".exit" {
			break
		}
		if strings.HasPrefix(line, ".") {
			if err := shellMeta(sb, line); err != nil {
				fmt.Println("error:", err)
			}
			continue
		}
		runShellSQL(sb, line)
	}
	return 0
}

func shellMeta(sb *sandbox.Sandbox, line string) error {
	parts := strings.Fields(line)
	switch parts[0] {
	case ".tables":
		entities := listEntities(sb)
		if len(entities) == 0 {
			fmt.Println("(no entities registered)")
			return nil
		}
		for _, q := range entities {
			fmt.Println(" ", q)
		}
		return nil
	case ".describe":
		if len(parts) < 2 {
			return errors.New("usage: .describe <qualified>")
		}
		d, err := sb.Inspect().Describe(parts[1])
		if err != nil {
			return err
		}
		fmt.Printf("table %s (%d rows)\n", d.Qualified, d.RowCount)
		fmt.Println("  PK:", strings.Join(d.PrimaryKey, ", "))
		if d.SoftDeleteField != "" {
			fmt.Println("  soft_delete:", d.SoftDeleteField)
		}
		if d.PartitionField != "" {
			fmt.Println("  partition_by:", d.PartitionField)
		}
		fmt.Println("  columns:")
		for _, c := range d.Columns {
			nul := "NOT NULL"
			if c.Nullable {
				nul = "NULL"
			}
			fmt.Printf("    %-20s %-12s %s\n", c.Name, c.Kind, nul)
		}
		return nil
	case ".sample":
		if len(parts) < 2 {
			return errors.New("usage: .sample <qualified> [n]")
		}
		n := 5
		if len(parts) >= 3 {
			if v, err := parseN(parts[2]); err == nil {
				n = v
			}
		}
		rows, err := sb.Inspect().Sample(parts[1], n)
		if err != nil {
			return err
		}
		printRows(rows)
		return nil
	}
	return fmt.Errorf("unknown command %q", parts[0])
}

func runShellSQL(sb *sandbox.Sandbox, line string) {
	// Trim trailing semicolons for ergonomics.
	line = strings.TrimRight(line, "; \t")
	upper := strings.ToUpper(line)
	isQuery := strings.HasPrefix(upper, "SELECT") || strings.Contains(upper, "RETURNING")

	if isQuery {
		rows, err := sb.Pool().Query(context.Background(), line)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		defer rows.Close()
		// pgx doesn't expose the column count before the first Scan, so
		// printShellRows probes by retrying with 1..32 destination slots
		// until one succeeds. 32 matches the codegen projection ceiling;
		// wider rows need the structured HTTP API.
		printShellRows(rows)
		return
	}
	tag, err := sb.Pool().Exec(context.Background(), line)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("(%d rows affected)\n", tag.RowsAffected())
}

// printShellRows scans into a generic any-pointer slice. We bound the
// projection width to 32 because every codegen-emitted shape stays well
// below that; if the row has more columns, we report the excess so the
// user knows to use the structured HTTP API instead.
func printShellRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) {
	const maxCols = 32
	first := true
	for rows.Next() {
		// Probe by scanning into N slots until one succeeds.
		var (
			dests   []any
			lastErr error
		)
		for n := 1; n <= maxCols; n++ {
			d := make([]any, n)
			p := make([]any, n)
			for i := range d {
				p[i] = &d[i]
			}
			if err := rows.Scan(p...); err != nil {
				lastErr = err
				continue
			}
			dests = d
			break
		}
		if dests == nil {
			fmt.Println("scan error:", lastErr)
			continue
		}
		if first {
			fmt.Println(strings.Repeat("─", 40))
			first = false
		}
		parts := make([]string, len(dests))
		for i, v := range dests {
			parts[i] = formatShellValue(v)
		}
		fmt.Println(strings.Join(parts, "\t"))
	}
	if err := rows.Err(); err != nil {
		fmt.Println("rows error:", err)
	}
}

func formatShellValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(x))
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case []float32:
		if len(x) > 8 {
			return fmt.Sprintf("[%g, … %d values]", x[0], len(x))
		}
		return fmt.Sprintf("%v", x)
	}
	return fmt.Sprint(v)
}

func printRows(rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Println("(no rows)")
		return
	}
	cols := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	fmt.Println(strings.Join(cols, "\t"))
	fmt.Println(strings.Repeat("─", 40))
	for _, r := range rows {
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = formatShellValue(r[c])
		}
		fmt.Println(strings.Join(parts, "\t"))
	}
}

func listEntities(sb *sandbox.Sandbox) []string {
	// We don't have a direct "list catalog" public method on Sandbox.
	// Catalog().QualifiedNames() does exist on the underlying sim.Catalog,
	// reachable via sb.Catalog().
	cat := sb.Catalog()
	return cat.QualifiedNames()
}

func parseN(s string) (int, error) {
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		v = v*10 + int(c-'0')
	}
	return v, nil
}

func determinismFromBool(strict bool) sandbox.Determinism {
	if strict {
		return sandbox.DeterminismStrict
	}
	return sandbox.DeterminismOff
}

// resolveBackend turns the --backend flag value into a sandbox.Backend
// and a human-readable reason string. Auto-routing inspects the IR
// the same way sandbox.New does, but here we get the reason text up
// front so we can print it before booting (which on embedded means
// ~4 s of waiting the user wouldn't otherwise see explained).
func resolveBackend(flag string, ir *dsl.IR) (sandbox.Backend, string, error) {
	switch flag {
	case "", "auto":
		if reason, embeddedNeeded := whyEmbedded(ir); embeddedNeeded {
			return sandbox.BackendEmbedded, "auto: " + reason, nil
		}
		return sandbox.BackendSim, "auto: pure-Go sim (no user-authored SQL or hypertable)", nil
	case "sim":
		return sandbox.BackendSim, "explicit --backend=sim", nil
	case "embedded":
		return sandbox.BackendEmbedded, "explicit --backend=embedded", nil
	}
	return "", "", fmt.Errorf("unknown --backend %q (use sim | embedded | auto)", flag)
}

// whyEmbedded reports whether the IR has features that force the
// embedded backend, and a short string explaining which.
func whyEmbedded(ir *dsl.IR) (reason string, embedded bool) {
	if ir == nil {
		return "", false
	}
	if len(ir.Queries) > 0 {
		return fmt.Sprintf("schema has %d custom query block(s)", len(ir.Queries)), true
	}
	if len(ir.Procedures) > 0 {
		return fmt.Sprintf("schema has %d custom procedure block(s)", len(ir.Procedures)), true
	}
	for i := range ir.Entities {
		if ir.Entities[i].Kind == dsl.EntityKindHypertable {
			return "schema declares a hypertable entity", true
		}
	}
	return "", false
}

// describeBackend renders the currently-active backend as a short
// label suitable for the boot banner.
func describeBackend(sb *sandbox.Sandbox) string {
	if sb.IsEmbedded() {
		if be := sb.EmbeddedBackend(); be != nil {
			return fmt.Sprintf("embedded postgres on port %d", be.Port())
		}
		return "embedded"
	}
	return "sim (in-memory)"
}

// loadSchemaIR reads `path` — either a single .atl file or a directory
// — and compiles every .atl in it into a single IR. dsl.Lower handles
// cross-namespace FK resolution + validation.
func loadSchemaIR(path string) (*dsl.IR, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	var paths []string
	if info.IsDir() {
		err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(p, ".atl") {
				paths = append(paths, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		paths = []string{path}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no .atl files found at %s", path)
	}
	var files []*dsl.File
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		f, err := dsl.Parse(p, src)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		files = append(files, f)
	}
	ir, err := dsl.Lower(files)
	if err != nil {
		return nil, fmt.Errorf("lower: %w", err)
	}
	return ir, nil
}
