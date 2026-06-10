package console

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rachitkumar205/atlantis/internal/console/vcs"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/atlprint"
)

const (
	sessionCookieName = "atl_console_session"
	adminBase         = "/atlantis.admin.v1.Admin/"
)

// Server is the atlantis console BFF.
type Server struct {
	cfg      Config
	atl      *adminClient
	db       *store
	mux      *http.ServeMux
	handler  http.Handler // mux wrapped with security headers
	log      *slog.Logger
	spaFS    fs.FS
	vcs      vcs.VCSProvider // nil when GITHUB_TOKEN is not set
	loginLim *loginLimiter

	// sandboxes owns the in-process sandbox runtime + per-user meta.
	// See internal/console/sandbox.go for the layer's design.
	sandboxes *sandboxLayer

	// Cancelled by Close() to stop background workers (audit retention,
	// sandbox TTL janitor).
	bgCtx    context.Context
	bgCancel context.CancelFunc
}

// New wires the console server. spaFS is the embedded SPA filesystem
// (the built dist/ directory from web/console). If nil, the SPA fallback
// returns 404 — useful during development when the SPA runs separately.
func New(cfg Config, spaFS fs.FS, log *slog.Logger) (*Server, error) {
	atl, err := dialAdmin(cfg)
	if err != nil {
		return nil, fmt.Errorf("dial atlantis: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := newStore(ctx, cfg.PGURL)
	if err != nil {
		_ = atl.Close()
		return nil, fmt.Errorf("open console db: %w", err)
	}
	if err := db.migrate(ctx); err != nil {
		db.close()
		_ = atl.Close()
		return nil, fmt.Errorf("console db migrate: %w", err)
	}

	var provider vcs.VCSProvider
	if cfg.GitHubToken != "" {
		provider = vcs.NewTokenProvider(cfg.GitHubToken)
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	s := &Server{
		cfg: cfg, atl: atl, db: db, log: log, spaFS: spaFS, vcs: provider,
		loginLim:  newLoginLimiter(),
		sandboxes: newSandboxLayer(cfg.SandboxPerUserLimit, cfg.SandboxTTL),
		bgCtx:     bgCtx, bgCancel: bgCancel,
	}
	// Clean up embedded-pg tempdirs left over from a prior crashed
	// process before any new sandboxes are booted; idempotent and
	// best-effort.
	sweepEmbeddedTempdirs(func(msg string, args ...any) {
		log.Warn(fmt.Sprintf(msg, args...))
	})
	s.buildMux()
	s.handler = s.withSecurityHeaders(s.mux)
	go s.auditRetentionLoop()
	go s.sandboxes.runJanitor(bgCtx, 60*time.Second)
	return s, nil
}

func (s *Server) Close() {
	if s.bgCancel != nil {
		s.bgCancel()
	}
	s.db.close()
	_ = s.atl.Close()
}

// auditRetentionLoop runs daily: creates next month's audit partition
// idempotently (so the very first INSERT on the first of a new month
// never fails for lack of a target partition) and drops every partition
// whose upper bound is older than cfg.AuditRetentionDays. Setting
// AuditRetentionDays to 0 disables the drop step but the partition
// creation still runs (otherwise inserts would fail at month rollover).
func (s *Server) auditRetentionLoop() {
	// First tick fires after a short delay so we don't compete with
	// startup work; subsequent ticks are 24h apart. A jittered first
	// tick isn't worth the complexity here — single-instance assumption.
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-s.bgCtx.Done():
			return
		case <-timer.C:
		}
		s.runAuditRetention()
		timer.Reset(24 * time.Hour)
	}
}

func (s *Server) runAuditRetention() {
	ctx, cancel := context.WithTimeout(s.bgCtx, 30*time.Second)
	defer cancel()

	// Roll forward: ensure this month and next month exist.
	// (See store.migrate for why we anchor on first-of-month, not today.)
	now := time.Now().UTC()
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if err := s.db.ensureAuditPartition(ctx, firstOfMonth); err != nil {
		s.log.Error("audit retention: ensure current month", "err", err)
		return
	}
	if err := s.db.ensureAuditPartition(ctx, firstOfMonth.AddDate(0, 1, 0)); err != nil {
		s.log.Error("audit retention: ensure next month", "err", err)
		return
	}

	if s.cfg.AuditRetentionDays <= 0 {
		return // retention disabled
	}
	cutoff := now.AddDate(0, 0, -s.cfg.AuditRetentionDays)
	dropped, err := s.db.dropAuditPartitionsOlderThan(ctx, cutoff)
	if err != nil {
		s.log.Error("audit retention: drop", "err", err)
		return
	}
	if len(dropped) > 0 {
		s.log.Info("audit retention: dropped expired partitions",
			"count", len(dropped),
			"cutoff", cutoff.Format("2006-01-02"),
			"names", dropped,
		)
	}

	// Session GC piggy-backs on the same daily tick. Expired sessions
	// don't grant access (getSessionInfo filters by expires_at > NOW)
	// but accumulating dead rows is unnecessary bloat.
	if n, err := s.db.deleteExpiredSessions(ctx); err != nil {
		s.log.Warn("session gc", "err", err)
	} else if n > 0 {
		s.log.Info("session gc: pruned expired", "count", n)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) buildMux() {
	mux := http.NewServeMux()

	// Setup + auth (no session required).
	mux.HandleFunc("GET /api/setup/status", s.handleSetupStatus)
	mux.HandleFunc("GET /api/setup/connectivity", s.handleSetupConnectivity)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)

	// Auth-required endpoints.
	mux.HandleFunc("POST /api/auth/logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /api/auth/me", s.auth(s.handleMe))

	// Admin RPC proxies — all auth-required.
	mux.HandleFunc("GET /api/schema", s.auth(s.handleGetMergedSchema))
	mux.HandleFunc("GET /api/schema/canonical", s.auth(s.handleGetCanonicalIR))
	mux.HandleFunc("GET /api/history", s.auth(s.handleGetSchemaHistory))
	mux.HandleFunc("GET /api/history/{version}", s.auth(s.handleGetSchemaVersion))
	mux.HandleFunc("GET /api/diff", s.auth(s.handleDiffSchemaVersions))
	mux.HandleFunc("GET /api/lineage/{entity}", s.auth(s.handleGetEntityLineage))
	// GetEntityOwners returns all entity→caller ownership; no per-entity filter in the RPC.
	mux.HandleFunc("GET /api/owners", s.auth(s.handleGetEntityOwners))
	mux.HandleFunc("GET /api/health", s.auth(s.handleHealth))

	// Schema editing — preview is read-only (any authenticated user); PR is admin-only.
	mux.HandleFunc("POST /api/schema/edit/preview", s.auth(s.csrf(s.handleEditPreview)))
	mux.HandleFunc("POST /api/schema/edit/pr", s.auth(s.requireRole("admin", s.csrf(s.handleEditPR))))

	// Caller→repo mapping management (admin-only mutations).
	mux.HandleFunc("GET /api/callers/repos", s.auth(s.handleListCallerRepos))
	mux.HandleFunc("PUT /api/callers/repos/{caller}", s.auth(s.requireRole("admin", s.csrf(s.handleUpsertCallerRepo))))

	// Caller management.
	mux.HandleFunc("GET /api/callers", s.auth(s.handleGetCallers))
	mux.HandleFunc("POST /api/callers", s.auth(s.requireRole("admin", s.csrf(s.handleRegisterCaller))))
	mux.HandleFunc("DELETE /api/callers/{caller}", s.auth(s.requireRole("admin", s.csrf(s.handleRevokeCaller))))
	mux.HandleFunc("POST /api/callers/{caller}/cert/issue", s.auth(s.requireRole("admin", s.csrf(s.handleIssueCert))))
	// Caller aliases (PR follow-up to the dispatcher security work).
	// GET is admin role-only; PUT is sudo-gated because changing aliases
	// widens the visible_to match set for a registered cert.
	mux.HandleFunc("GET /api/callers/{caller}/aliases", s.auth(s.requireRole("admin", s.handleGetCallerAliases)))
	mux.HandleFunc("PUT /api/callers/{caller}/aliases",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleSetCallerAliases)))))

	// Schema rollback — mutation, CSRF-protected, admin-only.
	mux.HandleFunc("POST /api/schema/rollback", s.auth(s.requireRole("admin", s.csrf(s.handleRollbackSchema))))
	mux.HandleFunc("POST /api/schema/rollback/preview", s.auth(s.requireRole("admin", s.csrf(s.handlePreviewRollback))))

	// Job queue management.
	mux.HandleFunc("GET /api/jobs/dead", s.auth(s.handleListDeadJobs))
	mux.HandleFunc("GET /api/jobs/{id}", s.auth(s.handleGetJobStatus))
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.auth(s.requireRole("admin", s.csrf(s.handleRetryDeadJob))))

	// Worker-poll dispatcher admin (PR 3). List + Get are read-only for
	// any authenticated user; Drain + Evict require admin role + sudo
	// (mirror of the Settings panel's destructive-action discipline).
	mux.HandleFunc("GET /api/admin/workers", s.auth(s.handleListConnectedWorkers))
	mux.HandleFunc("GET /api/admin/workers/{id}", s.auth(s.handleGetWorkerSession))
	mux.HandleFunc("POST /api/admin/workers/{id}/drain",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleDrainWorker)))))
	mux.HandleFunc("POST /api/admin/workers/{id}/evict",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleEvictWorker)))))

	// Audit log.
	mux.HandleFunc("GET /api/audit", s.auth(s.handleGetAuditLog))

	// Live log tail. Today this is a synthetic stream the BFF generates
	// so the Health page's tail surface works end-to-end; the eventual
	// implementation reads atlantis's in-process slog ring buffer via a
	// new admin RPC. Cursor-based: caller passes ?since=<seq> and gets
	// back any records with a higher sequence number plus the new
	// last_seq for the next poll.
	mux.HandleFunc("GET /api/logs", s.auth(s.handleGetLogs))

	// Schema edit PR — admin-only.
	// (preview is read-only so any authenticated user can access it)

	// User management — admin-only.
	mux.HandleFunc("GET /api/users", s.auth(s.requireRole("admin", s.handleListUsers)))
	mux.HandleFunc("POST /api/users", s.auth(s.requireRole("admin", s.csrf(s.handleCreateUser))))
	mux.HandleFunc("PUT /api/users/{id}/role", s.auth(s.requireRole("admin", s.csrf(s.handleSetUserRole))))
	mux.HandleFunc("DELETE /api/users/{id}", s.auth(s.requireRole("admin", s.csrf(s.handleDeleteUser))))

	// Settings-page operations.
	mux.HandleFunc("GET /api/instance", s.auth(s.handleInstance))
	mux.HandleFunc("POST /api/auth/password", s.auth(s.csrf(s.handleChangePassword)))
	mux.HandleFunc("POST /api/auth/sudo", s.auth(s.csrf(s.handleSudo)))
	mux.HandleFunc("POST /api/auth/sign-out-others", s.auth(s.csrf(s.handleSignOutOthers)))
	// Danger-zone — admin + CSRF + sudo (re-auth within sudoTTL).
	mux.HandleFunc("POST /api/auth/sign-out-all",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleSignOutAll)))))
	mux.HandleFunc("POST /api/callers/revoke-all",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleRevokeAllCallers)))))

	// Sandbox — schema-true in-memory testbed. See sandbox.go for the
	// per-user attribution, TTL janitor, and proxy-to-runtime layer.
	s.mountSandbox(mux)

	// SPA — serve the embedded dist/ for all non-API paths.
	mux.HandleFunc("/", s.handleSPA)

	s.mux = mux
}

// ── middleware ────────────────────────────────────────────────────────────────

type contextKey int

const (
	ctxUser         contextKey = iota // *User
	ctxSessionToken                   // string (raw cookie token)
	ctxSudoUntil                      // *time.Time (nil if not in sudo mode)
)

// auth wraps a handler requiring a valid session cookie. 401 if missing or
// expired; the authenticated user is stored in the request context.
//
// Performs sliding renewal: when the session's remaining TTL is below
// sessionTouchThreshold of the full window, it bumps expires_at and
// rewrites the cookie's MaxAge. The threshold keeps the renewal write
// off the hot path (~1 UPDATE per 6h for an active user with 12h TTL).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			jsonError(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		info, err := s.db.getSessionInfo(r.Context(), cookie.Value)
		if errors.Is(err, ErrNotFound) {
			jsonError(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		if err != nil {
			s.log.Error("session lookup", "err", err)
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Sliding renewal — see sessionTouchThreshold in store.go.
		remaining := time.Until(info.ExpiresAt)
		if remaining < time.Duration(float64(sessionTTL)*sessionTouchThreshold) {
			if err := s.db.touchSession(r.Context(), cookie.Value); err != nil {
				s.log.Warn("touch session", "err", err)
				// Non-fatal: an authenticated request still proceeds with
				// the existing expiry; only the renewal write failed.
			} else {
				// Rewrite the cookie so the browser's MaxAge matches the
				// new server-side expiry. Cookies without MaxAge are
				// session-cookies; we want a persistent one matching TTL.
				setSessionCookie(w, cookie.Value, s.cfg.CookieSecure)
			}
		}

		ctx := context.WithValue(r.Context(), ctxUser, info.User)
		ctx = context.WithValue(ctx, ctxSessionToken, cookie.Value)
		ctx = context.WithValue(ctx, ctxSudoUntil, info.SudoUntil)
		next(w, r.WithContext(ctx))
	}
}

// requireSudo gates an authenticated handler on the session being in
// sudo mode (i.e. the user has re-authenticated within sudoTTL). Use on
// destructive admin actions: sign-out-all, revoke-all-callers.
//
// 403 with a stable error code so the SPA knows to prompt for re-auth.
func (s *Server) requireSudo(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		su, _ := r.Context().Value(ctxSudoUntil).(*time.Time)
		if su == nil || !su.After(time.Now()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"sudo required","code":"sudo_required"}`))
			return
		}
		next(w, r)
	}
}

// ── setup + auth handlers ──────────────────────────────────────────────────

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	has, err := s.db.hasAnyUser(r.Context())
	if err != nil {
		s.log.Error("setup status", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]bool{"configured": has})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	// Guard against re-initialisation after the first operator is created.
	has, err := s.db.hasAnyUser(r.Context())
	if err != nil {
		s.log.Error("setup check", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if has {
		jsonError(w, "already configured", http.StatusConflict)
		return
	}

	var body struct {
		Email     string `json:"email"`
		Password  string `json:"password"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	body.FirstName = strings.TrimSpace(body.FirstName)
	body.LastName = strings.TrimSpace(body.LastName)
	if body.Email == "" || body.Password == "" {
		jsonError(w, "email and password are required", http.StatusBadRequest)
		return
	}
	if len(body.Password) < 8 {
		jsonError(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}

	user, err := s.db.createUser(r.Context(), body.Email, body.Password, "admin", body.FirstName, body.LastName)
	if err != nil {
		s.log.Error("create user", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	token, err := s.db.createSession(r.Context(), user.ID)
	if err != nil {
		s.log.Error("create session", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	setSessionCookie(w, token, s.cfg.CookieSecure)
	jsonOK(w, map[string]bool{"ok": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit. bcrypt's ~250ms is the primary throttle on
	// credential-spraying; this is the secondary one that bounds the
	// rate of attempts a single host can stack up. Failed AND successful
	// attempts both count — we don't want an attacker who already has a
	// valid password to also have unbounded login bandwidth.
	if ok, retry := s.loginLim.allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		s.log.Warn("login rate-limited", "ip", clientIP(r))
		jsonError(w, "too many login attempts; try again shortly", http.StatusTooManyRequests)
		return
	}

	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	user, err := s.db.authenticateUser(r.Context(), body.Email, body.Password)
	if errors.Is(err, ErrNotFound) {
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if err != nil {
		s.log.Error("authenticate user", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	token, err := s.db.createSession(r.Context(), user.ID)
	if err != nil {
		s.log.Error("create session", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	setSessionCookie(w, token, s.cfg.CookieSecure)
	jsonOK(w, map[string]bool{"ok": true})
}

// handleSudo grants the current session a short window of elevated
// permission (sudoTTL ~ 5 minutes) on successful re-authentication with
// the user's password. Required by destructive endpoints — sign-out-all,
// revoke-all — so a stolen session cookie alone cannot trigger them.
//
// Rate-limited the same way login is to prevent spraying a session
// cookie at this endpoint to escalate.
func (s *Server) handleSudo(w http.ResponseWriter, r *http.Request) {
	if ok, retry := s.loginLim.allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		jsonError(w, "too many attempts; try again shortly", http.StatusTooManyRequests)
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Password == "" {
		jsonError(w, "password is required", http.StatusBadRequest)
		return
	}

	u := r.Context().Value(ctxUser).(*User)
	if _, err := s.db.authenticateUser(r.Context(), u.Email, body.Password); err != nil {
		// Generic message — don't leak whether the password was wrong vs
		// some other internal error.
		jsonError(w, "invalid password", http.StatusUnauthorized)
		return
	}

	token, _ := r.Context().Value(ctxSessionToken).(string)
	if token == "" {
		jsonError(w, "session not found", http.StatusUnauthorized)
		return
	}
	if err := s.db.grantSudo(r.Context(), token); err != nil {
		s.log.Error("grant sudo", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.db.logAction(r.Context(), u.ID, "sudo_granted", map[string]any{
		"ttl_seconds": int(sudoTTL.Seconds()),
	})
	jsonOK(w, map[string]any{"ok": true, "expires_in_seconds": int(sudoTTL.Seconds())})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, _ := r.Cookie(sessionCookieName)
	if cookie != nil {
		_ = s.db.deleteSession(r.Context(), cookie.Value)
	}
	clearSessionCookie(w, s.cfg.CookieSecure)
	jsonOK(w, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)
	jsonOK(w, map[string]any{
		"id":         u.ID,
		"email":      u.Email,
		"role":       u.Role,
		"first_name": u.FirstName,
		"last_name":  u.LastName,
		"created_at": u.CreatedAt,
	})
}

// ── admin RPC proxies ─────────────────────────────────────────────────────────
// These forward the request to atlantis and pipe the raw JSON response back
// to the browser unchanged. This avoids maintaining duplicate type definitions
// for every admin response struct — the browser receives exactly what the
// admin service returns.

func (s *Server) handleGetMergedSchema(w http.ResponseWriter, r *http.Request) {
	s.proxyRPC(w, r, adminBase+"GetMergedSchema", struct{}{})
}

func (s *Server) handleGetCanonicalIR(w http.ResponseWriter, r *http.Request) {
	s.proxyRPC(w, r, adminBase+"GetCanonicalIR", struct{}{})
}

func (s *Server) handleGetSchemaHistory(w http.ResponseWriter, r *http.Request) {
	req := map[string]any{"limit": intQuery(r, "limit", 50)}
	// `before` and `caller` are server-side filters; forward them when set.
	if before, ok := int64Query(r, "before"); ok {
		req["before"] = before
	}
	if caller := r.URL.Query().Get("caller"); caller != "" {
		req["caller"] = caller
	}
	s.proxyRPC(w, r, adminBase+"GetSchemaHistory", req)
}

func (s *Server) handleGetSchemaVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || version <= 0 {
		jsonError(w, "version must be a positive integer", http.StatusBadRequest)
		return
	}
	s.proxyRPC(w, r, adminBase+"GetSchemaVersion", map[string]any{"version": version})
}

func (s *Server) handleDiffSchemaVersions(w http.ResponseWriter, r *http.Request) {
	from, fromOK := int64Query(r, "from")
	to, toOK := int64Query(r, "to")
	if !fromOK || !toOK {
		jsonError(w, "from and to are required positive integers", http.StatusBadRequest)
		return
	}
	// Server-side field names are from_version / to_version (history.go:179).
	s.proxyRPC(w, r, adminBase+"DiffSchemaVersions", map[string]any{
		"from_version": from,
		"to_version":   to,
	})
}

func (s *Server) handleGetEntityLineage(w http.ResponseWriter, r *http.Request) {
	// Admin RPC expects {"entity_id": "..."}.
	entity := r.PathValue("entity")
	s.proxyRPC(w, r, adminBase+"GetEntityLineage", map[string]any{"entity_id": entity})
}

// handleGetEntityOwners returns all entity→caller ownership.
// GetEntityOwners takes no arguments — it always returns the full set.
func (s *Server) handleGetEntityOwners(w http.ResponseWriter, r *http.Request) {
	s.proxyRPC(w, r, adminBase+"GetEntityOwners", struct{}{})
}

// handleHealth proxies atlantis's HTTP health endpoints, not an admin RPC.
// Returns the shape the SPA's HealthResponse type expects:
//
//	{ atlantis: {
//	    status, checks,
//	    readyz_code, healthz_code,
//	    started_at, server_version, schema_version,
//	    metrics_series } }
//
// Each chip on the Health page maps to one of these fields:
//
//   - /readyz, /healthz codes come from the upstream HTTP status.
//   - uptime is computed by the SPA from started_at.
//   - version is the schema version reported by /status (the design
//     screenshot shows v0048 which is the schema version, not the
//     server build version).
//   - metrics_series is the count of non-comment lines in /metrics.
//
// All atlantis HTTP calls share a tight ProbeTimeout so a wedged
// upstream can't stall the SPA's 1Hz health poll.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	type checkItem struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}

	probe := func(path string) (int, string) {
		resp, err := http.Get("http://" + s.cfg.HealthListen + path) //nolint:noctx
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close() //nolint:errcheck
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		msg := strings.TrimSpace(string(body))
		if msg == "ok" {
			msg = ""
		}
		return resp.StatusCode, msg
	}

	readyzCode, readyzMsg := probe("/readyz")
	healthzCode, healthzMsg := probe("/healthz")

	checkStatus := func(code int) string {
		if code == http.StatusOK {
			return "healthy"
		}
		return "unhealthy"
	}

	checks := []checkItem{
		{Name: "readyz", Status: checkStatus(readyzCode), Message: readyzMsg},
		{Name: "healthz", Status: checkStatus(healthzCode), Message: healthzMsg},
	}

	overall := "healthy"
	if readyzCode != http.StatusOK || healthzCode != http.StatusOK {
		overall = "unhealthy"
	}

	// /status — uptime + version + schema version. Best-effort: if
	// atlantis is down or pre-this-version, the fields stay zero and
	// the SPA renders an em-dash.
	var startedAt, serverVer string
	var schemaVer int64
	if resp, err := http.Get("http://" + s.cfg.HealthListen + "/status"); err == nil { //nolint:noctx
		var body struct {
			StartedAt     string `json:"started_at"`
			Version       string `json:"version"`
			SchemaVersion int64  `json:"schema_version"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 4*1024)).Decode(&body) == nil {
			startedAt = body.StartedAt
			serverVer = body.Version
			schemaVer = body.SchemaVersion
		}
		_ = resp.Body.Close()
	}

	// /metrics series count — every non-comment, non-blank line in
	// the Prometheus text format is one series. We don't need an exact
	// count, just a stable "N series" surface that moves with reality.
	metricsSeries := 0
	if resp, err := http.Get("http://" + s.cfg.HealthListen + "/metrics"); err == nil { //nolint:noctx
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			metricsSeries++
		}
		_ = resp.Body.Close()
	}

	atl := map[string]any{
		"status":         overall,
		"checks":         checks,
		"readyz_code":    readyzCode,
		"healthz_code":   healthzCode,
		"metrics_series": metricsSeries,
	}
	if startedAt != "" {
		atl["started_at"] = startedAt
	}
	if serverVer != "" {
		atl["server_version"] = serverVer
	}
	if schemaVer > 0 {
		atl["schema_version"] = schemaVer
	}

	jsonOK(w, map[string]any{"atlantis": atl})
}

// requireRole wraps a handler enforcing that the authenticated user has one of
// the given roles. Must be composed inside auth() so ctxUser is populated.
func (s *Server) requireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := r.Context().Value(ctxUser).(*User)
		if u.Role != role {
			jsonError(w, fmt.Sprintf("forbidden: %s role required", role), http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// ── CSRF middleware ────────────────────────────────────────────────────────────

// csrf rejects requests whose Origin header doesn't match the server host.
// Combined with SameSite=Strict session cookies this prevents cross-site
// request forgery on the console's state-changing endpoints.
func (s *Server) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			// Strip scheme, compare host only.
			origin = strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
			if origin != r.Host {
				jsonError(w, "CSRF check failed", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// ── Login rate limiter ────────────────────────────────────────────────────────

// loginLimiter is a per-IP sliding-window leaky bucket sized for a
// human-paced login UX. bcrypt's intrinsic ~250ms cost is the primary
// throttle on credential-spraying; this is the secondary one that
// keeps a single host from issuing thousands of attempts per minute.
//
// Memory bound: at most loginLimiterMaxIPs entries, each holding up to
// loginLimiterMax timestamps. ~10KB ceiling under sustained attack.
const (
	loginLimiterMax      = 10          // attempts allowed per window per IP
	loginLimiterWindow   = time.Minute // sliding window
	loginLimiterMaxIPs   = 10_000      // cap to bound memory
	loginLimiterSweepAge = 10 * time.Minute
)

type loginLimiter struct {
	mu      sync.Mutex
	hits    map[string][]time.Time
	lastSwp time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{hits: make(map[string][]time.Time), lastSwp: time.Now()}
}

// allow returns (ok, retryAfter). When ok is false, retryAfter is the
// seconds until the oldest in-window attempt rotates out.
func (l *loginLimiter) allow(ip string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-loginLimiterWindow)

	// Periodic sweep so the map doesn't grow forever after an attack.
	if now.Sub(l.lastSwp) > loginLimiterSweepAge {
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(now.Add(-loginLimiterSweepAge)) {
				delete(l.hits, k)
			}
		}
		l.lastSwp = now
	}

	// Hard cap on tracked IPs: if we're full and this IP is new, refuse.
	// Better to fail-closed than to silently amnesty entries under load.
	if len(l.hits) >= loginLimiterMaxIPs {
		if _, known := l.hits[ip]; !known {
			return false, int(loginLimiterWindow.Seconds())
		}
	}

	// Trim this IP's timestamps to the current window.
	hits := l.hits[ip]
	idx := 0
	for ; idx < len(hits); idx++ {
		if hits[idx].After(cutoff) {
			break
		}
	}
	hits = hits[idx:]

	if len(hits) >= loginLimiterMax {
		l.hits[ip] = hits
		retry := int(loginLimiterWindow.Seconds() - now.Sub(hits[0]).Seconds())
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}

	l.hits[ip] = append(hits, now)
	return true, 0
}

// clientIP returns the best-guess IP for rate-limiting. RemoteAddr is
// the immediate peer (the BFF talks to a reverse-proxy/LB in prod, so
// real-IP headers can be honored when configured); for the self-host
// single-VM case RemoteAddr is the actual client.
func clientIP(r *http.Request) string {
	// X-Forwarded-For is "client, proxy1, proxy2" — first hop is the
	// closest-to-client. Only trust when the deploy explicitly opts in
	// (CONSOLE_TRUST_PROXY=true), otherwise spoofable.
	if os.Getenv("CONSOLE_TRUST_PROXY") == "true" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ── Security headers ─────────────────────────────────────────────────────────

// withSecurityHeaders wraps a handler so every response carries a
// hardened header set: CSP, Referrer-Policy, X-Frame-Options, etc.
// HSTS only ships when CookieSecure (i.e. HTTPS), since HSTS on plain
// HTTP is a foot-gun.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	// The SPA fetches Geist + Geist Mono from Google Fonts (index.html +
	// tokens.css). Allow stylesheet and font origins explicitly; nothing
	// else is allowed.
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		// 'unsafe-inline' for styles is required because Vite's prod build
		// inlines a small style block + we use React style={{...}} props.
		// 'unsafe-inline' for scripts is *not* set — that's the dangerous one.
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com",
		"font-src 'self' https://fonts.gstatic.com",
		"img-src 'self' data:",
		"connect-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	}, "; ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// Disable browser APIs the console doesn't use, so an XSS-injected
		// script can't reach for geolocation / mic / camera / payment.
		h.Set("Permissions-Policy",
			"camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// ── schema editing ─────────────────────────────────────────────────────────────

// editRequest is the JSON body for /api/schema/edit/preview and /pr.
type editRequest struct {
	Namespace string `json:"namespace"`
	Entity    string `json:"entity"`
	Field     string `json:"field"`
	Op        string `json:"op"`         // "add" | "replace" | "remove"
	FieldText string `json:"field_text"` // used for add/replace
}

// resolvedSource is the result of locating an entity's .atl source.
type resolvedSource struct {
	caller    string
	ownerPath string       // server-stored file path that declares the entity
	files     []callerFile // full set of the caller's files (path + content)
	ownerIdx  int          // index into files for the owning file
}

type callerFile struct {
	path    string
	content []byte
}

// resolveEntitySource finds which caller owns the entity and which of its
// files declares it. Returns the caller's full file set so PlanSchema gets
// all files (not just the edited one).
func (s *Server) resolveEntitySource(ctx context.Context, namespace, entity string) (*resolvedSource, error) {
	// GetEntityOwners returns all ownership; filter client-side.
	type ownersResp struct {
		Owners []struct {
			EntityID     string `json:"entity_id"`
			IntroducedBy string `json:"introduced_by"`
		} `json:"owners"`
	}
	var owners ownersResp
	if err := s.atl.invoke(ctx, adminBase+"GetEntityOwners", struct{}{}, &owners); err != nil {
		return nil, fmt.Errorf("GetEntityOwners: %w", err)
	}

	entityID := namespace + "." + entity
	var caller string
	for _, o := range owners.Owners {
		if o.EntityID == entityID {
			caller = o.IntroducedBy
			break
		}
	}
	if caller == "" {
		return nil, fmt.Errorf("entity %s not found in ownership data", entityID)
	}

	// Fetch all of this caller's files.
	type callerFilesResp struct {
		Files []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"files"`
	}
	var cf callerFilesResp
	if err := s.atl.invoke(ctx, adminBase+"GetCallerFiles", map[string]string{"caller": caller}, &cf); err != nil {
		return nil, fmt.Errorf("GetCallerFiles: %w", err)
	}
	if len(cf.Files) == 0 {
		return nil, fmt.Errorf("caller %s has no registered files", caller)
	}

	files := make([]callerFile, len(cf.Files))
	for i, f := range cf.Files {
		files[i] = callerFile{path: f.Path, content: []byte(f.Content)}
	}

	// Parse each file to find the one declaring this entity.
	ownerIdx := -1
	var ownerPath string
	for i, f := range files {
		parsed, err := dsl.Parse(f.path, f.content)
		if err != nil {
			continue
		}
		for _, d := range parsed.Decls {
			if e, ok := d.(*dsl.EntityDecl); ok && e.Namespace == namespace && e.Name == entity {
				if ownerIdx >= 0 {
					return nil, fmt.Errorf("entity %s declared in multiple files (%s and %s)", entityID, ownerPath, f.path)
				}
				ownerIdx = i
				ownerPath = f.path
			}
		}
	}
	if ownerIdx < 0 {
		return nil, fmt.Errorf("entity %s not found in caller %s's files", entityID, caller)
	}

	return &resolvedSource{
		caller:    caller,
		ownerPath: ownerPath,
		files:     files,
		ownerIdx:  ownerIdx,
	}, nil
}

// applyEdit applies the requested field operation to src and returns the result.
func applyEdit(src []byte, namespace, entity, field, op, fieldText string) ([]byte, error) {
	switch op {
	case "add":
		return atlprint.AddField(src, namespace, entity, fieldText)
	case "replace":
		return atlprint.ReplaceField(src, namespace, entity, field, fieldText)
	case "remove":
		return atlprint.RemoveField(src, namespace, entity, field)
	default:
		return nil, fmt.Errorf("unknown op %q: must be add, replace, or remove", op)
	}
}

// handleEditPreview returns a read-only plan preview for a proposed field edit.
// It never mutates server state.
func (s *Server) handleEditPreview(w http.ResponseWriter, r *http.Request) {
	var req editRequest
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" || req.Entity == "" || req.Op == "" {
		jsonError(w, "namespace, entity, and op are required", http.StatusBadRequest)
		return
	}

	src, err := s.resolveEntitySource(r.Context(), req.Namespace, req.Entity)
	if err != nil {
		s.log.Error("resolve entity source", "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	oldContent := src.files[src.ownerIdx].content
	newContent, err := applyEdit(oldContent, req.Namespace, req.Entity, req.Field, req.Op, req.FieldText)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	// Build the full file set for PlanSchema, swapping the edited file.
	type submittedFile struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	planFiles := make([]submittedFile, len(src.files))
	for i, f := range src.files {
		planFiles[i] = submittedFile{Path: f.path, Content: string(f.content)}
	}
	planFiles[src.ownerIdx].Content = string(newContent)

	// Call PlanSchema read-only.
	type planResp struct {
		Class          string   `json:"class"`
		UpSQL          string   `json:"up_sql"`
		DownSQL        string   `json:"down_sql"`
		ImpactReport   any      `json:"impact_report"`
		BreakingDetail []string `json:"breaking_detail"`
		ParseErrors    []string `json:"parse_errors"`
		CheckpointHash string   `json:"checkpoint_hash"`
	}
	var plan planResp
	if err := s.atl.invoke(r.Context(), adminBase+"PlanSchema", map[string]any{
		"caller": src.caller,
		"files":  planFiles,
	}, &plan); err != nil {
		s.log.Error("PlanSchema", "err", err)
		jsonError(w, "plan failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	jsonOK(w, map[string]any{
		"owner_path":      src.ownerPath,
		"caller":          src.caller,
		"old_content":     string(oldContent),
		"new_content":     string(newContent),
		"plan_class":      plan.Class,
		"up_sql":          plan.UpSQL,
		"down_sql":        plan.DownSQL,
		"impact":          plan.ImpactReport,
		"breaking":        plan.BreakingDetail,
		"parse_errors":    plan.ParseErrors,
		"checkpoint_hash": plan.CheckpointHash,
	})
}

// handleEditPR opens a GitHub PR with the proposed field edit.
func (s *Server) handleEditPR(w http.ResponseWriter, r *http.Request) {
	var req struct {
		editRequest
		Title              string `json:"title"`
		Body               string `json:"body"`
		BaseCheckpointHash string `json:"base_checkpoint_hash"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" || req.Entity == "" || req.Op == "" {
		jsonError(w, "namespace, entity, and op are required", http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		req.Title = fmt.Sprintf("schema: %s %s.%s.%s", req.Op, req.Namespace, req.Entity, req.Field)
	}

	if s.vcs == nil {
		jsonError(w, "GitHub not configured: set GITHUB_TOKEN in environment", http.StatusBadRequest)
		return
	}

	src, err := s.resolveEntitySource(r.Context(), req.Namespace, req.Entity)
	if err != nil {
		s.log.Error("resolve entity source", "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Look up the caller→repo mapping (acts as an allowlist).
	repoMapping, err := s.db.getCallerRepo(r.Context(), src.caller)
	if errors.Is(err, ErrNotFound) {
		jsonError(w, fmt.Sprintf("no repo mapping for caller %q — configure it in Settings", src.caller), http.StatusConflict)
		return
	}
	if err != nil {
		s.log.Error("get caller repo", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	repo := vcs.RepoRef{
		Owner:  repoMapping.Owner,
		Repo:   repoMapping.Repo,
		Branch: repoMapping.DefaultBranch,
	}

	// Map server-stored file path to repo-relative path using path_prefix.
	repoPath := src.ownerPath
	if repoMapping.SchemaPathPrefix != "" {
		repoPath = strings.TrimPrefix(repoPath, repoMapping.SchemaPathPrefix)
		repoPath = strings.TrimPrefix(repoPath, "/")
	}

	// Fetch the file from the repo's actual HEAD so the PR diff is minimal.
	repoContent, err := s.vcs.GetFileContent(r.Context(), repo, repoPath)
	if err != nil {
		s.log.Error("fetch repo file", "path", repoPath, "err", err)
		jsonError(w, fmt.Sprintf("could not fetch %s from GitHub: %s", repoPath, err), http.StatusBadGateway)
		return
	}

	newContent, err := applyEdit(repoContent, req.Namespace, req.Entity, req.Field, req.Op, req.FieldText)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	// Branch name: atlantis/console/<entity>-<timestamp-ms>
	branch := fmt.Sprintf("atlantis/console/%s-%d", strings.ToLower(req.Entity), time.Now().UnixMilli())

	result, err := s.vcs.OpenPR(r.Context(), repo, branch, []vcs.FileChange{
		{Path: repoPath, Content: newContent},
	}, req.Title, req.Body)
	if err != nil {
		s.log.Error("open PR", "err", err)
		jsonError(w, "failed to open PR: "+err.Error(), http.StatusBadGateway)
		return
	}

	s.log.Info("PR opened",
		"pr_url", result.URL,
		"caller", src.caller,
		"entity", req.Namespace+"."+req.Entity,
		"op", req.Op,
	)

	jsonOK(w, map[string]any{
		"pr_url": result.URL,
		"number": result.Number,
	})
}

// ── caller→repo mapping endpoints ─────────────────────────────────────────────

func (s *Server) handleListCallerRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.db.listCallerRepos(r.Context())
	if err != nil {
		s.log.Error("list caller repos", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	type wire struct {
		Caller           string `json:"caller"`
		Owner            string `json:"owner"`
		Repo             string `json:"repo"`
		DefaultBranch    string `json:"default_branch"`
		SchemaPathPrefix string `json:"schema_path_prefix"`
	}
	out := make([]wire, 0, len(repos))
	for _, r := range repos {
		out = append(out, wire{
			Caller:           r.Caller,
			Owner:            r.Owner,
			Repo:             r.Repo,
			DefaultBranch:    r.DefaultBranch,
			SchemaPathPrefix: r.SchemaPathPrefix,
		})
	}
	jsonOK(w, map[string]any{"repos": out})
}

func (s *Server) handleUpsertCallerRepo(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	var body struct {
		Owner            string `json:"owner"`
		Repo             string `json:"repo"`
		DefaultBranch    string `json:"default_branch"`
		SchemaPathPrefix string `json:"schema_path_prefix"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Owner == "" || body.Repo == "" {
		jsonError(w, "owner and repo are required", http.StatusBadRequest)
		return
	}
	if body.DefaultBranch == "" {
		body.DefaultBranch = "main"
	}
	if err := s.db.upsertCallerRepo(r.Context(), &CallerRepo{
		Caller:           caller,
		Owner:            body.Owner,
		Repo:             body.Repo,
		DefaultBranch:    body.DefaultBranch,
		SchemaPathPrefix: body.SchemaPathPrefix,
	}); err != nil {
		s.log.Error("upsert caller repo", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]bool{"ok": true})
}

// ── User management ──────────────────────────────────────────────────

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.db.listUsers(r.Context())
	if err != nil {
		s.log.Error("list users", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	type wire struct {
		ID        int64  `json:"id"`
		Email     string `json:"email"`
		Role      string `json:"role"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]wire, 0, len(users))
	for _, u := range users {
		out = append(out, wire{
			ID:        u.ID,
			Email:     u.Email,
			Role:      u.Role,
			CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	jsonOK(w, map[string]any{"users": out})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email     string `json:"email"`
		Password  string `json:"password"`
		Role      string `json:"role"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	body.FirstName = strings.TrimSpace(body.FirstName)
	body.LastName = strings.TrimSpace(body.LastName)
	if body.Email == "" || body.Password == "" {
		jsonError(w, "email and password are required", http.StatusBadRequest)
		return
	}
	if len(body.Password) < 8 {
		jsonError(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	if body.Role != "admin" && body.Role != "viewer" {
		body.Role = "viewer"
	}

	user, err := s.db.createUser(r.Context(), body.Email, body.Password, body.Role, body.FirstName, body.LastName)
	if err != nil {
		s.log.Error("create user", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	actor := r.Context().Value(ctxUser).(*User)
	s.db.logAction(r.Context(), actor.ID, "create_user", map[string]any{
		"email":      user.Email,
		"role":       user.Role,
		"first_name": user.FirstName,
		"last_name":  user.LastName,
	})
	jsonOK(w, map[string]any{
		"id":         user.ID,
		"email":      user.Email,
		"role":       user.Role,
		"first_name": user.FirstName,
		"last_name":  user.LastName,
	})
}

func (s *Server) handleSetUserRole(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	var id int64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil || id <= 0 {
		jsonError(w, "invalid user id", http.StatusBadRequest)
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Role != "admin" && body.Role != "viewer" {
		jsonError(w, `role must be "admin" or "viewer"`, http.StatusBadRequest)
		return
	}

	// Prevent self-demotion so there's always at least one admin.
	actor := r.Context().Value(ctxUser).(*User)
	if actor.ID == id && body.Role != "admin" {
		jsonError(w, "cannot change your own role", http.StatusConflict)
		return
	}

	if err := s.db.setUserRole(r.Context(), id, body.Role); err != nil {
		if errors.Is(err, ErrNotFound) {
			jsonError(w, "user not found", http.StatusNotFound)
			return
		}
		s.log.Error("set user role", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.db.logAction(r.Context(), actor.ID, "set_user_role", map[string]any{
		"target_user_id": id,
		"role":           body.Role,
	})
	jsonOK(w, map[string]bool{"ok": true})
}

// handleDeleteUser removes an operator. ON DELETE CASCADE on
// console.sessions handles their active sessions in the same statement
// — they're signed out immediately. Self-deletion is refused so the
// last admin can never accidentally lock themselves out.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	var id int64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil || id <= 0 {
		jsonError(w, "invalid user id", http.StatusBadRequest)
		return
	}
	actor := r.Context().Value(ctxUser).(*User)
	if actor.ID == id {
		jsonError(w, "cannot delete your own account", http.StatusConflict)
		return
	}
	if err := s.db.deleteUser(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			jsonError(w, "user not found", http.StatusNotFound)
			return
		}
		s.log.Error("delete user", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.db.logAction(r.Context(), actor.ID, "delete_user", map[string]any{
		"target_user_id": id,
	})
	jsonOK(w, map[string]bool{"ok": true})
}

// ── Settings-page operations ─────────────────────────────────────────────────

// handleInstance returns small, non-secret runtime facts the Settings
// page renders: the gRPC endpoint callers connect to. Auth-required so
// it's not exposed publicly.
func (s *Server) handleInstance(w http.ResponseWriter, _ *http.Request) {
	jsonOK(w, map[string]any{
		"endpoint": s.cfg.ATLEndpoint,
	})
}

// handleChangePassword verifies the caller's current password before
// rotating it. On success every session for the user is invalidated
// (including the calling one) so the user is forced to sign in again
// with the new password — defensive against the new credential being
// reused via a stolen cookie before they noticed.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Current == "" || body.New == "" {
		jsonError(w, "current_password and new_password are required", http.StatusBadRequest)
		return
	}
	if len(body.New) < 8 {
		jsonError(w, "new password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	if err := s.db.changePassword(r.Context(), u.ID, body.Current, body.New); err != nil {
		if errors.Is(err, ErrNotFound) {
			jsonError(w, "current password is incorrect", http.StatusUnauthorized)
			return
		}
		s.log.Error("change password", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.db.logAction(r.Context(), u.ID, "change_password", nil)
	clearSessionCookie(w, s.cfg.CookieSecure)
	jsonOK(w, map[string]bool{"ok": true})
}

// handleSignOutOthers terminates every session for the calling user
// except the one whose token is on the current request. The user stays
// signed in on this device.
func (s *Server) handleSignOutOthers(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		jsonError(w, "no session", http.StatusUnauthorized)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	count, err := s.db.deleteSessionsForUserExcept(r.Context(), u.ID, cookie.Value)
	if err != nil {
		s.log.Error("sign out others", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.db.logAction(r.Context(), u.ID, "sign_out_others", map[string]any{
		"sessions_removed": count,
	})
	jsonOK(w, map[string]any{"ok": true, "sessions_removed": count})
}

// handleSignOutAll terminates every session for every operator — the
// caller included. Admin-only. The danger-zone confirmation modal on
// the SPA enforces explicit consent before this hits.
func (s *Server) handleSignOutAll(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(*User)
	count, err := s.db.deleteAllSessions(r.Context())
	if err != nil {
		s.log.Error("sign out all", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.db.logAction(r.Context(), actor.ID, "sign_out_all", map[string]any{
		"sessions_removed": count,
	})
	clearSessionCookie(w, s.cfg.CookieSecure)
	jsonOK(w, map[string]any{"ok": true, "sessions_removed": count})
}

// handleRevokeAllCallers iterates every known caller and calls the
// existing single-revoke admin RPC for each. This drops them from
// caller_identities so the server stops accepting their certs (the
// certs themselves remain cryptographically valid until expiry; the
// allowlist is the trust gate). Schema files in caller_registrations
// are also removed, matching the single-revoke semantics — the design
// label "Revoke all caller certificates" frames the user-visible
// effect, not the underlying table operation.
//
// Admin-only, CSRF-protected. The SPA additionally requires the user
// to type "revoke all" before allowing the call.
func (s *Server) handleRevokeAllCallers(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(*User)

	// List all known callers via the existing admin RPC; iterate revokes.
	listRaw, err := s.atl.invokeRaw(r.Context(), adminBase+"GetCallers", map[string]any{})
	if err != nil {
		s.log.Error("RevokeAll: GetCallers", "err", err)
		jsonError(w, "GetCallers: "+err.Error(), http.StatusBadGateway)
		return
	}
	var listBody struct {
		Callers []struct {
			Caller string `json:"caller"`
		} `json:"callers"`
	}
	if err := json.Unmarshal(listRaw, &listBody); err != nil {
		s.log.Error("RevokeAll: parse callers", "err", err)
		jsonError(w, "invalid GetCallers response", http.StatusBadGateway)
		return
	}

	revoked := 0
	failures := []string{}
	for _, c := range listBody.Callers {
		if c.Caller == "" {
			continue
		}
		if _, err := s.atl.invokeRaw(r.Context(), adminBase+"RevokeCaller", map[string]string{
			"caller": c.Caller,
		}); err != nil {
			s.log.Warn("RevokeAll: revoke caller", "caller", c.Caller, "err", err)
			failures = append(failures, c.Caller)
			continue
		}
		revoked++
	}

	s.db.logAction(r.Context(), actor.ID, "revoke_all_callers", map[string]any{
		"revoked":  revoked,
		"failures": failures,
	})
	jsonOK(w, map[string]any{
		"ok":       true,
		"revoked":  revoked,
		"failures": failures,
	})
}

// ── Caller management ─────────────────────────────────────────────────

func (s *Server) handleGetCallers(w http.ResponseWriter, r *http.Request) {
	s.proxyRPC(w, r, adminBase+"GetCallers", struct{}{})
}

func (s *Server) handleRegisterCaller(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Caller    string `json:"caller"`
		CanMutate bool   `json:"can_mutate"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}

	actor := r.Context().Value(ctxUser).(*User)
	raw, err := s.atl.invokeRaw(r.Context(), adminBase+"RegisterCaller", map[string]any{
		"caller":     body.Caller,
		"can_mutate": body.CanMutate,
		"created_by": actor.Email,
	})
	if err != nil {
		s.log.Error("RegisterCaller", "caller", body.Caller, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	s.db.logAction(r.Context(), actor.ID, "register_caller", map[string]any{
		"caller":     body.Caller,
		"can_mutate": body.CanMutate,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handleGetCallerAliases proxies the GetCallerAliases admin RPC. Read-
// only; gated by admin role at the route. The atlantis-side returns
// 404-equivalent when the caller isn't registered.
func (s *Server) handleGetCallerAliases(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	s.proxyRPC(w, r, adminBase+"GetCallerAliases", map[string]string{"caller": caller})
}

// handleSetCallerAliases proxies the SetCallerAliases admin RPC.
// Sudo-gated by the route. Audit-logged with the new alias set so an
// operator review of caller permissions can trace which CN was granted
// which aliases when.
func (s *Server) handleSetCallerAliases(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	var body struct {
		Aliases []string `json:"aliases"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	raw, err := s.atl.invokeRaw(r.Context(), adminBase+"SetCallerAliases", map[string]any{
		"caller":  caller,
		"aliases": body.Aliases,
	})
	if err != nil {
		s.log.Error("SetCallerAliases", "caller", caller, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	s.db.logAction(r.Context(), u.ID, "set_caller_aliases", map[string]any{
		"caller":  caller,
		"aliases": body.Aliases,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Server) handleRevokeCaller(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}

	raw, err := s.atl.invokeRaw(r.Context(), adminBase+"RevokeCaller", map[string]string{"caller": caller})
	if err != nil {
		s.log.Error("RevokeCaller", "caller", caller, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	u := r.Context().Value(ctxUser).(*User)
	s.db.logAction(r.Context(), u.ID, "revoke_caller", map[string]any{"caller": caller})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// ── Schema rollback ───────────────────────────────────────────────────

func (s *Server) handleRollbackSchema(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ToVersion int64 `json:"to_version"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.ToVersion <= 0 {
		jsonError(w, "to_version is required", http.StatusBadRequest)
		return
	}

	// req.Caller on the admin RPC is the actor identity recorded in
	// schema_versions.caller for audit — not a filter (rollback always
	// applies globally). Auto-fill from the logged-in operator's email
	// so the user doesn't have to type their own identity and the audit
	// row distinguishes operator-driven rollbacks from caller-driven
	// applies on inspection. Prefixed with "console:" so future
	// tooling can grep operator events apart from caller events.
	u := r.Context().Value(ctxUser).(*User)
	caller := "console:" + u.Email

	raw, err := s.atl.invokeRaw(r.Context(), adminBase+"RollbackSchema", map[string]any{
		"to_version": body.ToVersion,
		"caller":     caller,
	})
	if err != nil {
		s.log.Error("RollbackSchema", "to_version", body.ToVersion, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	s.db.logAction(r.Context(), u.ID, "rollback_schema", map[string]any{
		"to_version": body.ToVersion,
		"caller":     caller,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handlePreviewRollback proxies to the read-only PreviewRollback admin RPC.
// Returns the SQL the rollback would execute + plan class + change count,
// without taking the advisory lock or persisting anything. The user
// reviews this before clicking Execute, which calls the real
// /api/schema/rollback that recomputes from a fresh snapshot.
func (s *Server) handlePreviewRollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ToVersion int64 `json:"to_version"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.ToVersion <= 0 {
		jsonError(w, "to_version is required", http.StatusBadRequest)
		return
	}
	raw, err := s.atl.invokeRaw(r.Context(), adminBase+"PreviewRollback", map[string]any{
		"to_version": body.ToVersion,
	})
	if err != nil {
		s.log.Error("PreviewRollback", "to_version", body.ToVersion, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// ── Job queue management ─────────────────────────────────────────────

func (s *Server) handleListDeadJobs(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 50)
	jobName := r.URL.Query().Get("job_name")
	s.proxyRPC(w, r, adminBase+"ListDeadJobs", map[string]any{
		"limit":    limit,
		"job_name": jobName,
	})
}

func (s *Server) handleGetJobStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.proxyRPC(w, r, adminBase+"GetJobStatus", map[string]string{"job_id": id})
}

func (s *Server) handleRetryDeadJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}

	raw, err := s.atl.invokeRaw(r.Context(), adminBase+"RetryDeadJob", map[string]string{"job_id": id})
	if err != nil {
		s.log.Error("RetryDeadJob", "job_id", id, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	u := r.Context().Value(ctxUser).(*User)
	s.db.logAction(r.Context(), u.ID, "retry_dead_job", map[string]any{"job_id": id})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// ── Worker dispatcher ────────────────────────────────────────────────

func (s *Server) handleListConnectedWorkers(w http.ResponseWriter, r *http.Request) {
	s.proxyRPC(w, r, adminBase+"ListConnectedWorkers", map[string]any{})
}

func (s *Server) handleGetWorkerSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}
	s.proxyRPC(w, r, adminBase+"GetWorkerSession", map[string]string{"session_id": id})
}

func (s *Server) handleDrainWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}
	raw, err := s.atl.invokeRaw(r.Context(), adminBase+"DrainWorker", map[string]string{"session_id": id})
	if err != nil {
		s.log.Error("DrainWorker", "session_id", id, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	s.db.logAction(r.Context(), u.ID, "worker_drained", map[string]any{"session_id": id})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Server) handleEvictWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}
	raw, err := s.atl.invokeRaw(r.Context(), adminBase+"EvictWorker", map[string]string{"session_id": id})
	if err != nil {
		s.log.Error("EvictWorker", "session_id", id, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	s.db.logAction(r.Context(), u.ID, "worker_evicted", map[string]any{"session_id": id})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// ── Cert issuance ────────────────────────────────────────────────────

// handleIssueCert generates a fresh ECDSA keypair + CSR on the server side,
// posts the CSR to the signer service, and returns the full cert bundle
// (cert_pem, key_pem, ca_pem, expires_at) to the operator for download.
// The CA private key never touches the console.
func (s *Server) handleIssueCert(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	if s.cfg.SignerAddr == "" {
		jsonError(w, "cert signer not configured: set ATL_SIGNER_ADDR in environment", http.StatusServiceUnavailable)
		return
	}

	// Generate a fresh P-256 key. The private key never leaves the console
	// BFF — it is generated here, used to create the CSR, then returned to
	// the operator browser for download alongside the signed cert.
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		s.log.Error("generate key", "caller", caller, "err", err)
		jsonError(w, "key generation failed", http.StatusInternalServerError)
		return
	}

	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: caller},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, privKey)
	if err != nil {
		s.log.Error("create CSR", "caller", caller, "err", err)
		jsonError(w, "CSR creation failed", http.StatusInternalServerError)
		return
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))

	// POST to signer.
	type signerReq struct {
		Caller string `json:"caller"`
		CSRPEM string `json:"csr_pem"`
	}
	type signerResp struct {
		CertPEM   string `json:"cert_pem"`
		CAPEM     string `json:"ca_pem"`
		ExpiresAt string `json:"expires_at"`
		Error     string `json:"error"`
	}

	body, _ := json.Marshal(signerReq{Caller: caller, CSRPEM: csrPEM})
	resp, err := http.Post(s.cfg.SignerAddr+"/issue", "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		s.log.Error("call signer", "caller", caller, "err", err)
		jsonError(w, "signer unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	var signerRespBody signerResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&signerRespBody); err != nil {
		s.log.Error("decode signer response", "caller", caller, "err", err)
		jsonError(w, "invalid signer response", http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusOK {
		msg := signerRespBody.Error
		if msg == "" {
			msg = fmt.Sprintf("signer returned %d", resp.StatusCode)
		}
		jsonError(w, msg, http.StatusBadGateway)
		return
	}

	// Encode the private key as PKCS8 PEM for the download bundle.
	keyDER, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		s.log.Error("marshal key", "caller", caller, "err", err)
		jsonError(w, "key encoding failed", http.StatusInternalServerError)
		return
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	u := r.Context().Value(ctxUser).(*User)
	s.db.logAction(r.Context(), u.ID, "issue_cert", map[string]any{
		"caller":     caller,
		"expires_at": signerRespBody.ExpiresAt,
	})

	// Compute the leaf's SHA-256 fingerprint. The server's cert-binding
	// interceptor reads this column on every authenticated RPC and
	// rejects any peer cert whose hash doesn't match — so persisting the
	// new fingerprint atomically supersedes the previous cert at the
	// auth layer (rotation/revoke without a CRL).
	//
	// PEM decode is parser-strict — anything but a CERTIFICATE block is
	// rejected so a malformed signer response can't land a bogus
	// fingerprint that locks the caller out.
	var fingerprintHex string
	if block, _ := pem.Decode([]byte(signerRespBody.CertPEM)); block != nil && block.Type == "CERTIFICATE" {
		sum := sha256.Sum256(block.Bytes)
		fingerprintHex = hex.EncodeToString(sum[:])
	} else {
		s.log.Error("decode signed cert for fingerprint", "caller", caller)
		jsonError(w, "signer returned malformed cert", http.StatusBadGateway)
		return
	}

	// Persist NotAfter + fingerprint. The fingerprint write is
	// load-bearing — if it fails, the old cert keeps authenticating
	// until natural expiry, so surface the error to the operator rather
	// than swallowing it like the pre-binding implementation did.
	if _, err := s.atl.invokeRaw(r.Context(), adminBase+"RecordCallerCertExpiry", map[string]string{
		"caller":      caller,
		"expires_at":  signerRespBody.ExpiresAt,
		"fingerprint": fingerprintHex,
	}); err != nil {
		s.log.Error("RecordCallerCertExpiry", "caller", caller, "err", err)
		jsonError(w, "cert minted but binding write failed; the previous cert still authenticates — retry to rotate", http.StatusBadGateway)
		return
	}

	s.log.Info("cert issued",
		"caller", caller,
		"operator", u.Email,
		"expires_at", signerRespBody.ExpiresAt,
	)

	jsonOK(w, map[string]any{
		"cert_pem":   signerRespBody.CertPEM,
		"key_pem":    keyPEM,
		"ca_pem":     signerRespBody.CAPEM,
		"expires_at": signerRespBody.ExpiresAt,
	})
}

// ── audit log ─────────────────────────────────────────────────────────────────

func (s *Server) handleGetAuditLog(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 100)
	entries, err := s.db.listAuditLog(r.Context(), limit)
	if err != nil {
		s.log.Error("list audit log", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type wire struct {
		ID        int64           `json:"id"`
		UserID    int64           `json:"user_id"`
		UserEmail string          `json:"user_email"`
		Action    string          `json:"action"`
		Detail    json.RawMessage `json:"detail,omitempty"`
		CreatedAt string          `json:"created_at"`
	}
	out := make([]wire, 0, len(entries))
	for _, e := range entries {
		out = append(out, wire{
			ID:        e.ID,
			UserID:    e.UserID,
			UserEmail: e.UserEmail,
			Action:    e.Action,
			Detail:    json.RawMessage(e.Detail),
			CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	jsonOK(w, map[string]any{"entries": out})
}

// ── Live log tail ────────────────────────────────────────────────────────────
// handleGetLogs proxies the SPA's poll into atlantis's GetLogs admin RPC,
// which reads from the lock-free in-process slog ring buffer (see
// internal/obs/logring.go). Cursor-based: the client passes ?since=N and
// receives records with Seq > N plus the new last_seq for the next poll.

func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	limit := intQuery(r, "limit", 0)

	raw, err := s.atl.invokeRaw(r.Context(), adminBase+"GetLogs", map[string]any{
		"since": since,
		"limit": limit,
	})
	if err != nil {
		s.log.Error("GetLogs", "err", err)
		jsonError(w, "GetLogs: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// ── SPA handler ───────────────────────────────────────────────────────────────

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if s.spaFS == nil {
		http.Error(w, "console SPA not built yet — run: make build-console-spa", http.StatusNotFound)
		return
	}

	// Serve static assets from dist/assets/* directly.
	// For all other paths serve index.html and let the SPA router handle it.
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "."
	}

	if _, err := fs.Stat(s.spaFS, path); err == nil && path != "." {
		http.FileServerFS(s.spaFS).ServeHTTP(w, r)
		return
	}

	// SPA fallback: serve index.html.
	f, err := s.spaFS.Open("index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close() //nolint:errcheck
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", fi.ModTime(), f.(io.ReadSeeker))
}

// ── helpers ───────────────────────────────────────────────────────────────────

// proxyRPC calls an admin RPC and writes the raw JSON response to w.
func (s *Server) proxyRPC(w http.ResponseWriter, r *http.Request, method string, req any) {
	raw, err := s.atl.invokeRaw(r.Context(), method, req)
	if err != nil {
		s.log.Error("admin rpc", "method", method, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(r *http.Request, dst any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst)
}

func intQuery(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}

// int64Query parses a positive int64 query parameter. The second return
// value is true only when the param was present and parsed successfully —
// used for optional filters where "missing" must be distinguished from
// "zero" so we don't accidentally pass before=0 to the server.
func int64Query(r *http.Request, key string) (int64, bool) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func setSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}
