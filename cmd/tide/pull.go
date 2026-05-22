package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// pull / list / show wrap AdminService.GetMergedSchema. The merged-schema
// view is the mirror of every caller's currently-registered .atl
// files after every successful `tide apply`.
//
// Local cache layout:
//
//	<caller-repo-root>/
//	  .tide-cache/
//	    schema/
//	      <namespace>/
//	        <entity>.pc     ← exactly what the server returned
//	    version.json        ← {"version": "abc123..."} from the last pull
//
// `.tide-cache/` is gitignored (recommended in docs/DSL_AUTHORING.md). The
// version file lets a subsequent `tide pull` ask the server "anything new
// since N?" and no-op when nothing has changed.

const (
	tideCacheDir  = ".tide-cache"
	pcSchemaDir   = "schema"
	pcVersionFile = "version.json"
)

type getMergedSchemaRequest struct {
	SinceVersion string `json:"SinceVersion"`
}

type getMergedSchemaResponse struct {
	Version string          `json:"Version"`
	Files   []SubmittedFile `json:"Files"`
}

type pullCachedVersion struct {
	Version string `json:"version"`
}

// cmdPull is `tide pull`: download the merged schema mirror into .tide-cache/.
// Exit codes mirror the other subcommands: 0 = success (incl. no-op),
// 3 = operational error.
func cmdPull(args []string) int {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	force := fs.Bool("force", false, "Pull even if local cache is already current")
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

	since := ""
	if !*force {
		since = readCachedVersion()
	}

	resp, err := callGetMergedSchema(ctx, cfg, since)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide pull:", err)
		return 3
	}

	if len(resp.Files) == 0 && since == resp.Version {
		fmt.Printf("tide: already up to date (version %s)\n", resp.Version)
		return 0
	}

	if err := writeMergedSchema(resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide pull:", err)
		return 3
	}
	fmt.Printf("tide: ✓ pulled %d file(s) (version %s)\n", len(resp.Files), resp.Version)
	return 0
}

// cmdList is `tide list`: print one line per entity in the merged schema.
// Designed for ad-hoc browsing; doesn't write to the cache.
func cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
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

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	resp, err := callGetMergedSchema(ctx, cfg, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide list:", err)
		return 3
	}

	paths := make([]string, len(resp.Files))
	for i, f := range resp.Files {
		paths[i] = f.Path
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Println(p)
	}
	return 0
}

// cmdShow is `tide show <Entity-or-Path>`: print the .atl text for
// one entity, fetched live from the server. Useful when a quick "what does
// vendor.Product look like right now?" is faster than `tide pull && cat`.
func cmdShow(args []string) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: tide show <path-substring>")
		return 3
	}
	needle := rest[0]

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	resp, err := callGetMergedSchema(ctx, cfg, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide show:", err)
		return 3
	}

	var hits []SubmittedFile
	for _, f := range resp.Files {
		if strings.Contains(f.Path, needle) {
			hits = append(hits, f)
		}
	}
	if len(hits) == 0 {
		fmt.Fprintf(os.Stderr, "tide: no file matches %q\n", needle)
		return 3
	}
	for _, f := range hits {
		fmt.Printf("--- %s ---\n", f.Path)
		fmt.Println(string(f.Content))
	}
	return 0
}

// callGetMergedSchema dials the server and invokes the RPC. Shared between
// pull / list / show so credentials, codec, and method-string formatting
// live in one place.
func callGetMergedSchema(ctx context.Context, cfg *tideConfig, sinceVersion string) (*getMergedSchemaResponse, error) {
	client, err := dial(cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	var resp getMergedSchemaResponse
	err = client.invoke(ctx, "/atlantis.admin.v1.Admin/GetMergedSchema",
		getMergedSchemaRequest{SinceVersion: sinceVersion}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// writeMergedSchema replaces the contents of .tide-cache/schema with the
// server's reply, then records the new version so subsequent pulls can
// short-circuit. Stale files are removed first so a deletion on the server
// is reflected locally.
func writeMergedSchema(resp *getMergedSchemaResponse) error {
	schemaDir := filepath.Join(tideCacheDir, pcSchemaDir)
	if err := os.RemoveAll(schemaDir); err != nil {
		return fmt.Errorf("clear cache: %w", err)
	}
	for _, f := range resp.Files {
		dst := filepath.Join(schemaDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, f.Content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}
	v, err := json.Marshal(pullCachedVersion{Version: resp.Version})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(tideCacheDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(tideCacheDir, pcVersionFile), v, 0o644)
}

// readCachedVersion returns the last-recorded merged-schema version, or ""
// when no cache exists yet. A read error (corrupt file, permissions, …)
// also returns "" — we'd rather over-fetch than refuse to pull.
func readCachedVersion() string {
	data, err := os.ReadFile(filepath.Join(tideCacheDir, pcVersionFile))
	if err != nil {
		return ""
	}
	var v pullCachedVersion
	if err := json.Unmarshal(data, &v); err != nil {
		return ""
	}
	return v.Version
}

// pullBeforeApply is called by `tide apply` (unless --no-pull) so cross-
// caller references resolve in the local cache before the submit. Network
// failures are non-fatal — the server-side planning RPC will catch any
// reference issues authoritatively.
func pullBeforeApply(ctx context.Context, cfg *tideConfig) {
	resp, err := callGetMergedSchema(ctx, cfg, readCachedVersion())
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide: pre-apply pull failed (continuing):", err)
		return
	}
	if len(resp.Files) == 0 {
		return
	}
	if err := writeMergedSchema(resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide: pre-apply cache write failed (continuing):", err)
	}
}
