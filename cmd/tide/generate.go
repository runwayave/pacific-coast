package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	atlantiscommon "github.com/rachitkumar205/atlantis/atlantis/common"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

type getCanonicalIRResponse struct {
	IR          json.RawMessage `json:"IR"`
	ContentHash string          `json:"ContentHash"`
}

// cmdGenerate is `tide generate`: fetch the canonical IR from the server,
// scope it to the namespaces this caller consumes, and emit a typed Go
// client SDK into the caller's own module (output_dir). The generated code
// belongs to the caller's repo — there is no shared central SDK and no
// dependency on a checkout of the atlantis repo.
func cmdGenerate(args []string) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	if len(cfg.Generate) == 0 {
		fmt.Fprintln(os.Stderr, "tide generate: `generate:` in tide.yaml must list at least one namespace")
		return 3
	}
	if cfg.OutputDir == "" {
		fmt.Fprintln(os.Stderr, "tide generate: `output_dir` in tide.yaml is required")
		return 3
	}

	modulePath, err := callerModulePath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return 3
	}
	modulePrefix := modulePath + "/" + filepath.ToSlash(filepath.Clean(cfg.OutputDir))

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	ir, err := fetchCanonicalIR(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return 3
	}
	scoped := codegen.FilterIR(ir, cfg.Generate)
	if len(scoped.Entities) == 0 && len(scoped.Queries) == 0 && len(scoped.Procedures) == 0 {
		fmt.Fprintf(os.Stderr, "tide generate: no declarations found for namespaces %v\n", cfg.Generate)
		return 3
	}

	if err := generateSDK(scoped, cfg.OutputDir, modulePrefix); err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return 3
	}

	fmt.Printf("tide: ✓ generated client for %s under %s\n",
		strings.Join(cfg.Generate, ", "), cfg.OutputDir)
	return 0
}

func fetchCanonicalIR(ctx context.Context, cfg *tideConfig) (*dsl.IR, error) {
	client, err := dial(cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	var resp getCanonicalIRResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/GetCanonicalIR",
		struct{}{}, &resp); err != nil {
		return nil, err
	}
	if len(resp.IR) == 0 || string(resp.IR) == "null" {
		return nil, fmt.Errorf("server has no schema yet — run `tide apply` first")
	}
	return dsl.DecodeJSONIR(resp.IR)
}

// generateSDK writes proto sources + typed Go client wrappers into outDir,
// then runs buf to produce the .pb.go wire types and gofmt to tidy the
// result. Layout under outDir: atlantis/<ns>/v1/*.proto (sources),
// pb/atlantis/<ns>/v1/*.pb.go (buf output), client/<ns>/*.go (wrappers).
func generateSDK(ir *dsl.IR, outDir, modulePrefix string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// Proto sources: caller namespaces + the embedded common protos so
	// `import "atlantis/common/v1/...";` resolves locally.
	protoFiles, err := codegen.EmitProto(ir)
	if err != nil {
		return fmt.Errorf("emit proto: %w", err)
	}
	customProto, err := codegen.EmitCustomProto(ir)
	if err != nil {
		return fmt.Errorf("emit custom proto: %w", err)
	}
	for _, pf := range append(protoFiles, customProto...) {
		if err := writeFile(filepath.Join(outDir, pf.Path), pf.Content); err != nil {
			return err
		}
	}
	if err := writeCommonProtos(outDir); err != nil {
		return err
	}

	// Typed Go client wrappers, scoped to the caller's module path.
	cfg := codegen.GenConfig{ModulePrefix: modulePrefix}
	clientFiles, err := codegen.EmitGoClient(ir, cfg)
	if err != nil {
		return fmt.Errorf("emit go client: %w", err)
	}
	customClient, err := codegen.EmitCustomClient(ir, cfg)
	if err != nil {
		return fmt.Errorf("emit custom client: %w", err)
	}
	for _, gf := range append(clientFiles, customClient...) {
		// Emitter paths are repo-relative (clients/go/client/<ns>/...);
		// remap onto the caller's output dir as client/<ns>/...
		rel := strings.TrimPrefix(gf.Path, "clients/go/")
		if err := writeFile(filepath.Join(outDir, rel), gf.Content); err != nil {
			return err
		}
	}

	if err := writeBufConfig(outDir, modulePrefix); err != nil {
		return err
	}
	if err := runBuf(outDir); err != nil {
		return err
	}
	return gofmtDir(outDir)
}

func writeCommonProtos(outDir string) error {
	return fs.WalkDir(atlantiscommon.Protos, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := atlantiscommon.Protos.ReadFile(path)
		if err != nil {
			return err
		}
		// Embedded paths are "v1/<file>.proto"; land them under
		// atlantis/common/ to match the proto package atlantis.common.v1.
		dst := filepath.Join(outDir, "atlantis", "common", path)
		return writeFile(dst, string(content))
	})
}

// writeBufConfig writes a single-prefix buf.gen.yaml + minimal buf.yaml so
// `buf generate` emits every proto in outDir (caller namespaces + common)
// under one Go module prefix.
func writeBufConfig(outDir, modulePrefix string) error {
	bufGen := fmt.Sprintf(`# Generated by tide generate. DO NOT EDIT.
version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: %s/pb
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.6
    out: pb
    opt:
      - paths=source_relative
  - remote: buf.build/grpc/go:v1.5.1
    out: pb
    opt:
      - paths=source_relative
      - require_unimplemented_servers=true
`, modulePrefix)
	if err := writeFile(filepath.Join(outDir, "buf.gen.yaml"), bufGen); err != nil {
		return err
	}
	return writeFile(filepath.Join(outDir, "buf.yaml"), "version: v2\n")
}

func runBuf(outDir string) error {
	if _, err := exec.LookPath("buf"); err != nil {
		return fmt.Errorf("buf not found on PATH — install it (https://buf.build/docs/installation) and re-run")
	}
	cmd := exec.Command("buf", "generate")
	cmd.Dir = outDir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("buf generate: %w", err)
	}
	return nil
}

func gofmtDir(outDir string) error {
	cmd := exec.Command("gofmt", "-w", outDir)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gofmt: %w", err)
	}
	return nil
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// callerModulePath reads the `module` line from the go.mod in the current
// working directory (the caller repo root).
func callerModulePath() (string, error) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		return "", fmt.Errorf("read go.mod (run from the caller repo root): %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("no module directive in go.mod")
}
