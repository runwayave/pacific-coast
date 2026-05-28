package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rachitkumar205/atlantis/internal/cliout"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/workspace"
)

// cmdCodegen reads .atl files from either a workspace manifest (production
// path) or a single --schema-dir (legacy / fixture path), lowers them
// into an IR, loads the previous IR from -ir-checkpoint (or starts
// fresh), assigns stable proto numbers, and writes:
//
//   - <out>/proto/<file>.proto
//   - <out>/gen/go/server/*.go
//   - <out>/gen/go/client/*.go
//   - <out>/gen/go/keys/*.go
//   - <out>/gen/.last-ir.json     (updated checkpoint)
//
// Migration SQL is NOT emitted here — that's `tidectl plan`. Codegen is for
// the generated Go + proto surface that's checked into the repo; SQL
// goes through a separate human-review gate.
func cmdCodegen(args []string) int {
	fs := flagSet("codegen")
	schemaDir := fs.String("schema-dir", "testdata/schema", "Directory containing .atl files (recursive). Ignored when --workspace is set.")
	workspaceFile := fs.String("workspace", "", "Path to an atlantis.workspace.yaml manifest. When set, takes precedence over --schema-dir.")
	workspaceCache := fs.String("workspace-cache", ".workspace-cache", "Directory the workspace resolver clones caller repos into.")
	out := fs.String("out", ".", "Output root for proto/ and gen/")
	checkpoint := fs.String("ir-checkpoint", "gen/.last-ir.json", "Previous IR checkpoint for stable proto numbers")
	dryRun := fs.Bool("dry-run", false, "Print what would be written without writing")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	files, sourceLabel, err := loadCodegenSources(*workspaceFile, *workspaceCache, *schemaDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codegen:", err)
		return 1
	}
	if len(files) == 0 {
		// No .atl files is a legitimate state — a fresh clone of atlantis
		// source has none until an operator adds them (or points
		// --workspace at their callers). Emit a warning and exit 0 so the
		// downstream `buf generate` step still runs against the
		// hand-written common protos.
		fmt.Fprintf(os.Stderr, "codegen: no .atl files in %s (nothing to emit)\n", sourceLabel)
		return 0
	}
	fmt.Fprintf(os.Stderr, "codegen: %s (%d files)\n", sourceLabel, len(files))

	newIR, err := dsl.Lower(files)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codegen:", err)
		return 1
	}

	prior, _ := loadCheckpoint(*checkpoint) // missing checkpoint is fine on first run
	codegen.AssignProtoNumbers(prior, newIR)

	// Emit every artifact.
	emitters := []struct {
		name string
		fn   func() ([]codegen.GoFile, error)
	}{
		{"go server", func() ([]codegen.GoFile, error) { return codegen.EmitGoServer(newIR) }},
		{"go client", func() ([]codegen.GoFile, error) { return codegen.EmitGoClient(newIR, codegen.GenConfig{}) }},
		{"go keys", func() ([]codegen.GoFile, error) { return codegen.EmitGoCacheKeys(newIR) }},
		{"go custom server", func() ([]codegen.GoFile, error) { return codegen.EmitCustomServer(newIR) }},
		{"go custom client", func() ([]codegen.GoFile, error) { return codegen.EmitCustomClient(newIR, codegen.GenConfig{}) }},
		{"go jobs handlers", func() ([]codegen.GoFile, error) { return codegen.EmitJobsHandlers(newIR) }},
		{"go workflows", func() ([]codegen.GoFile, error) { return codegen.EmitWorkflows(newIR) }},
		{"go ephemerals", func() ([]codegen.GoFile, error) { return codegen.EmitEphemerals(newIR) }},
	}
	written := 0
	for _, em := range emitters {
		fs, err := em.fn()
		if err != nil {
			fmt.Fprintf(os.Stderr, "codegen: %s: %v\n", em.name, err)
			return 1
		}
		for _, f := range fs {
			path := filepath.Join(*out, f.Path)
			if *dryRun {
				fmt.Printf("would write %s (%d bytes)\n", path, len(f.Content))
				continue
			}
			if err := writeFile(path, []byte(f.Content)); err != nil {
				fmt.Fprintln(os.Stderr, "codegen:", err)
				return 1
			}
			written++
		}
	}

	// Proto files use their own type but the writer is identical.
	protos, err := codegen.EmitProto(newIR)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codegen: proto:", err)
		return 1
	}
	customProtos, err := codegen.EmitCustomProto(newIR)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codegen: custom proto:", err)
		return 1
	}
	protos = append(protos, customProtos...)
	for _, f := range protos {
		path := filepath.Join(*out, f.Path)
		if *dryRun {
			fmt.Printf("would write %s (%d bytes)\n", path, len(f.Content))
			continue
		}
		if err := writeFile(path, []byte(f.Content)); err != nil {
			fmt.Fprintln(os.Stderr, "codegen:", err)
			return 1
		}
		written++
	}

	// Update the IR checkpoint so subsequent runs preserve proto numbers.
	if !*dryRun {
		ckPath := filepath.Join(*out, *checkpoint)
		raw, err := newIR.EncodeJSON()
		if err != nil {
			fmt.Fprintln(os.Stderr, "codegen: encode ir:", err)
			return 1
		}
		if err := writeFile(ckPath, raw); err != nil {
			fmt.Fprintln(os.Stderr, "codegen: write checkpoint:", err)
			return 1
		}
	}
	cliout.Successf("codegen ok (%s files)", cliout.Bold(fmt.Sprintf("%d", written)))
	return 0
}

// loadCodegenSources resolves the workspace manifest if one is set,
// otherwise falls back to the legacy --schema-dir walk. The returned
// label is for diagnostic logging ("workspace foo.yaml" vs "schema/").
//
// The workspace path is the production authoritative: caller repos
// pinned at git refs. The --schema-dir path is preserved for tests and
// for the bundled demo schemas under internal/codegen/testdata.
func loadCodegenSources(manifestPath, cacheDir, schemaDir string) ([]*dsl.File, string, error) {
	if manifestPath != "" {
		w, err := workspace.Load(manifestPath)
		if err != nil {
			return nil, manifestPath, err
		}
		resolved, err := w.Resolve(cacheDir)
		if err != nil {
			return nil, manifestPath, err
		}
		files, err := loadATLFilesFromResolved(resolved)
		return files, fmt.Sprintf("workspace %s (%d callers)", manifestPath, len(resolved)), err
	}
	files, err := loadATLFiles(schemaDir)
	return files, fmt.Sprintf("schema-dir %s", schemaDir), err
}

// loadATLFilesFromResolved parses every .atl file in every resolved
// caller. The file path stored on dsl.File uses the caller-relative
// shape (<caller>/<repo-relative-path>) so error messages name the
// caller, not the absolute checkout path the resolver wrote into.
func loadATLFilesFromResolved(resolved []*workspace.ResolvedCaller) ([]*dsl.File, error) {
	var out []*dsl.File
	for _, rc := range resolved {
		for _, abs := range rc.Files {
			data, err := os.ReadFile(abs)
			if err != nil {
				return nil, fmt.Errorf("caller %s: read %s: %w", rc.Name, abs, err)
			}
			rel, _ := filepath.Rel(rc.CloneRoot, abs)
			displayPath := filepath.Join(rc.Name, rel)
			f, err := dsl.Parse(displayPath, data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", displayPath, err)
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// loadATLFiles parses every .atl file under root recursively. Path is relative
// to root so generated files end up in a consistent layout.
func loadATLFiles(root string) ([]*dsl.File, error) {
	var out []*dsl.File
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
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
		rel, _ := filepath.Rel(root, path)
		f, err := dsl.Parse(rel, data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func loadCheckpoint(path string) (*dsl.IR, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return dsl.DecodeJSONIR(raw)
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
