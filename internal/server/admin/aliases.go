// Caller identity aliases. Lets a registered caller's cert CN satisfy
// `visible_to` predicates declared for OTHER identity names, without
// rewriting schema. The PostgreSQL-roles / AD-SID / DNS-CNAME pattern.
//
// Authentication is unaffected: a cert still authenticates as its CN.
// Aliases only widen which `visible_to` predicates that CN matches at
// authz time. Operator-controlled — callers can't self-declare aliases.
//
// See migrations/infra/0017_caller_aliases.up.sql for the column +
// index. Wire shape mirrors the existing GetCallers / RegisterCaller
// pattern: typed Request/Response + a Service method with the same
// authz contract (mutations gated by authorizeOperator).

package admin

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// LookupCallerAliases returns the aliases configured for a caller.
// Returns an empty slice when the caller is registered but has no
// aliases; returns (nil, nil) when the caller isn't registered at all
// — the latter is treated by callers as "no aliases" (an unregistered
// caller can't have aliases, by construction).
//
// Hot-path safe: one indexed PRIMARY KEY lookup. The dispatcher reads
// this once per session at Open, not per-dispatch, so even without
// caching the cost is bounded by reconnection rate.
//
// A nil pool returns (nil, nil) so unit tests that don't stand up
// Postgres can exercise the "no aliases configured" branch.
func (s *Service) LookupCallerAliases(ctx context.Context, caller string) ([]string, error) {
	if s.pool == nil {
		return nil, nil
	}
	var aliases []string
	err := s.pool.QueryRow(ctx, `
SELECT aliases FROM atlantis.caller_identities WHERE caller = $1`, caller).Scan(&aliases)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup caller_identities.aliases: %w", err)
	}
	return aliases, nil
}

// SetCallerAliasesRequest replaces a caller's alias set wholesale.
// PUT-style semantics (not PATCH) so the operator's intent is
// explicit: this is the complete list. Empty Aliases clears the
// list. Atomic relative to PG.
type SetCallerAliasesRequest struct {
	Caller  string   `json:"caller"`
	Aliases []string `json:"aliases"`
}

type SetCallerAliasesResponse struct {
	Caller  string   `json:"caller"`
	Aliases []string `json:"aliases"`
}

// SetCallerAliases replaces the alias set for an existing caller.
//
// Operator-allowlist gated: only the console CN (or any other CN in
// ATL_OPERATOR_ALLOWED_CALLERS) can invoke this via gRPC. The console
// BFF wraps this with admin role + sudo gating.
//
// Validation:
//   - The caller must already exist in caller_identities. Aliases
//     can't be set on a non-existent identity; register first.
//   - Aliases are deduplicated and sorted before persisting. Eliminates
//     spurious diffs when an operator submits the same set in a
//     different order.
//   - Each alias must be a non-empty string, non-equal to its own
//     caller (no self-loop — pointless), and not collide with any
//     other caller's canonical name (would create an ambiguous
//     authentication-vs-alias split). The collision check is a
//     defense-in-depth follow-up; today we allow it and operators
//     are responsible for not creating conflicts.
//   - Reserved names ("atlantis", "atlantis-console", etc.) are
//     rejected the same way they are in RegisterCaller.
func (s *Service) SetCallerAliases(ctx context.Context, req SetCallerAliasesRequest) (*SetCallerAliasesResponse, error) {
	if err := s.authorizeOperator(ctx); err != nil {
		return nil, err
	}
	if req.Caller == "" {
		return nil, errors.New("admin: caller is required")
	}

	cleaned, err := normalizeAliases(req.Caller, req.Aliases)
	if err != nil {
		return nil, err
	}

	if s.pool == nil {
		// No-PG test path: succeed without writing anything.
		return &SetCallerAliasesResponse{Caller: req.Caller, Aliases: cleaned}, nil
	}

	res, err := s.pool.Exec(ctx, `
UPDATE atlantis.caller_identities
   SET aliases = $2
 WHERE caller = $1`, req.Caller, cleaned)
	if err != nil {
		return nil, fmt.Errorf("update aliases: %w", err)
	}
	if res.RowsAffected() == 0 {
		return nil, fmt.Errorf("admin: caller %q is not registered; register first via RegisterCaller", req.Caller)
	}

	return &SetCallerAliasesResponse{Caller: req.Caller, Aliases: cleaned}, nil
}

type GetCallerAliasesRequest struct {
	Caller string `json:"caller"`
}

type GetCallerAliasesResponse struct {
	Caller  string   `json:"caller"`
	Aliases []string `json:"aliases"`
}

// GetCallerAliases returns the alias set for an existing caller.
// Read-only; no operator gate, but the gRPC + BFF chains already
// gate on admin role.
//
// Returns the alias list (empty if no aliases set). Returns an error
// when the caller isn't registered so the BFF can distinguish
// "registered, no aliases" from "404 — no such caller."
func (s *Service) GetCallerAliases(ctx context.Context, req GetCallerAliasesRequest) (*GetCallerAliasesResponse, error) {
	if req.Caller == "" {
		return nil, errors.New("admin: caller is required")
	}
	if s.pool == nil {
		return &GetCallerAliasesResponse{Caller: req.Caller, Aliases: nil}, nil
	}
	var aliases []string
	err := s.pool.QueryRow(ctx, `
SELECT aliases FROM atlantis.caller_identities WHERE caller = $1`, req.Caller).Scan(&aliases)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("admin: caller %q is not registered", req.Caller)
	}
	if err != nil {
		return nil, fmt.Errorf("lookup aliases: %w", err)
	}
	if aliases == nil {
		aliases = []string{}
	}
	return &GetCallerAliasesResponse{Caller: req.Caller, Aliases: aliases}, nil
}

// normalizeAliases trims, dedups, sorts, and validates. Centralised
// so SetCallerAliases and any future bulk-import path stay in lockstep
// on what's acceptable.
//
// Returns a never-nil slice (even when input is empty) so the SQL
// driver writes an empty array literal '{}' rather than NULL —
// matches the column's NOT NULL DEFAULT '{}' constraint and the
// "registered but no aliases" semantics.
func normalizeAliases(caller string, in []string) ([]string, error) {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		// Trim is intentional but doesn't lower-case — CNs are
		// case-sensitive per RFC 5280; aliases follow the same rule.
		a := raw
		for len(a) > 0 && (a[0] == ' ' || a[0] == '\t') {
			a = a[1:]
		}
		for len(a) > 0 && (a[len(a)-1] == ' ' || a[len(a)-1] == '\t') {
			a = a[:len(a)-1]
		}
		if a == "" {
			continue
		}
		if a == caller {
			return nil, fmt.Errorf("admin: alias %q equals caller; self-aliasing is redundant", a)
		}
		switch a {
		case "atlantis", "atlantis-console", "atlantis-signer", "anonymous", "*":
			return nil, fmt.Errorf("admin: alias %q is reserved", a)
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}
