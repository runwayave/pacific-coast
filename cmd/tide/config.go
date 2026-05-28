package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// tideConfig is the shape of tide.yaml. Lives at the root of every caller repo.
//
// Example:
//
//	caller: backend
//	endpoint: atlantis-dev.runway-avenue.internal:443
//	schema_paths:
//	  - internal/outfit
//	  - internal/cart
//	tls:
//	  cert: ~/.config/runway/dev-client.pem
//	  key:  ~/.config/runway/dev-client-key.pem
//	  ca:   ~/.config/runway/runway-ca.pem
//
// Every field is overridable via env (ATL_CALLER, ATL_ENDPOINT, TIDE_TLS_*) so
// CI runners can configure the same `tide` binary without touching tide.yaml.
type tideConfig struct {
	Caller      string   `yaml:"caller"`
	Endpoint    string   `yaml:"endpoint"`
	SchemaPaths []string `yaml:"schema_paths"`
	TLS         struct {
		Cert string `yaml:"cert"`
		Key  string `yaml:"key"`
		CA   string `yaml:"ca"`
	} `yaml:"tls"`
	OutputDir string `yaml:"output_dir"`
	// Generate lists the namespaces `tide generate` emits a typed client
	// for — the caller's own namespace plus any it consumes cross-namespace.
	Generate []string `yaml:"generate"`
}

func loadPCConfig(path string) (*tideConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c tideConfig
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	applyEnvOverrides(&c)
	// YAML doesn't interpolate ${VAR} — a literal placeholder is almost
	// always a misconfiguration ("I thought yaml expanded this"). Catch
	// it here rather than letting the TLS cert loader fail with an opaque
	// "no such file" later.
	if isEnvPlaceholder(c.TLS.Cert) || isEnvPlaceholder(c.TLS.Key) || isEnvPlaceholder(c.TLS.CA) {
		return nil, fmt.Errorf("%s: tls.{cert,key,ca} contain literal ${VAR} placeholders; "+
			"YAML does not expand env vars — leave the field blank in the file and set the "+
			"TIDE_TLS_CERT / TIDE_TLS_KEY / TIDE_TLS_CA env vars instead", path)
	}
	if c.Caller == "" {
		return nil, fmt.Errorf("%s: `caller` is required", path)
	}
	if c.Endpoint == "" {
		return nil, fmt.Errorf("%s: `endpoint` is required", path)
	}
	if len(c.SchemaPaths) == 0 {
		return nil, fmt.Errorf("%s: `schema_paths` must list at least one directory", path)
	}
	return &c, nil
}

// isEnvPlaceholder reports whether s looks like an unexpanded shell-style
// `${VAR}` template. Used by loadPCConfig to fail fast on the common
// "I thought YAML expanded env vars" misconfiguration.
func isEnvPlaceholder(s string) bool {
	return len(s) >= 4 && s[0] == '$' && s[1] == '{' && s[len(s)-1] == '}'
}

func applyEnvOverrides(c *tideConfig) {
	if v := os.Getenv("ATL_CALLER"); v != "" {
		c.Caller = v
	}
	if v := os.Getenv("ATL_ENDPOINT"); v != "" {
		c.Endpoint = v
	}
	if v := os.Getenv("TIDE_TLS_CERT"); v != "" {
		c.TLS.Cert = v
	}
	if v := os.Getenv("TIDE_TLS_KEY"); v != "" {
		c.TLS.Key = v
	}
	if v := os.Getenv("TIDE_TLS_CA"); v != "" {
		c.TLS.CA = v
	}
	if v := os.Getenv("ATL_GENERATE"); v != "" {
		c.Generate = nil
		for _, ns := range strings.Split(v, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				c.Generate = append(c.Generate, ns)
			}
		}
	}
}
