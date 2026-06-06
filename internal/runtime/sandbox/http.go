package sandbox

// HTTP control plane. Wraps every existing
// Sandbox method in JSON-over-HTTP so cross-process callers and the
// `tide sandbox boot` CLI can drive a sandbox over the wire.
//
// The shape is deliberately thin: every endpoint is a near-1:1 mirror
// of a Go method on Sandbox / Inspector / Fixtures. Auth, rate-limit,
// and persistence policy stay out of scope here — they belong to the
// hosting layer that wraps Server.ServeHTTP. Tests use httptest +
// Server.ServeHTTP directly, no networking.
//
// Endpoint catalogue:
//
//	POST   /v1/sandbox                  — create sandbox from IR, returns {id}
//	DELETE /v1/sandbox/{id}             — destroy
//	POST   /v1/sandbox/{id}/sql/exec    — run UPDATE/DELETE/INSERT-no-RETURNING
//	POST   /v1/sandbox/{id}/sql/query   — run SELECT / INSERT-RETURNING, returns rows
//	GET    /v1/sandbox/{id}/inspect/describe?q=…
//	GET    /v1/sandbox/{id}/inspect/sample?q=…&n=…
//	POST   /v1/sandbox/{id}/inspect/find
//	POST   /v1/sandbox/{id}/inspect/diff
//	GET    /v1/sandbox/{id}/snapshot    — bytes
//	PUT    /v1/sandbox/{id}/snapshot    — bytes
//	POST   /v1/sandbox/{id}/mark        — returns {mark_id}
//	POST   /v1/sandbox/{id}/restore     — body {mark_id}
//	POST   /v1/sandbox/{id}/fork        — body {n}, returns {ids: [...]}
//	POST   /v1/sandbox/{id}/fixtures/bulk
//
// Everything that returns rows uses []map[string]any so JSON encoding
// preserves column names and Go-native values (time.Time → RFC3339,
// []byte → base64, vectors → JSON arrays).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Server is the HTTP control plane. It owns a registry of live
// sandboxes keyed by an opaque id, plus per-sandbox mark registries
// (since marks are in-process Go pointers and can't cross the wire
// directly — the wire form is an opaque string the server resolves
// back into the live Mark).
type Server struct {
	mu        sync.RWMutex
	sandboxes map[string]*serverSandbox
	nextID    atomic.Int64
}

// serverSandbox is one Sandbox + the marks the agent has stashed on
// the server side. Marks are tracked per-sandbox; restoring a mark
// from sandbox A into sandbox B is rejected (we just look it up by
// the local map, so cross-sandbox lookups simply miss).
type serverSandbox struct {
	id    string
	sb    *Sandbox
	mu    sync.Mutex
	marks map[string]*Mark
}

// NewServer returns an empty Server. Use Handler to obtain an
// http.Handler; tests usually invoke ServeHTTP directly via httptest.
func NewServer() *Server {
	return &Server{sandboxes: map[string]*serverSandbox{}}
}

// Handler returns an http.Handler with every route registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sandbox", s.routeRoot)
	mux.HandleFunc("/v1/sandbox/", s.routeChild)
	return mux
}

// ServeHTTP lets Server be used directly as an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// routeRoot handles `POST /v1/sandbox` — the create-sandbox endpoint.
// Anything else lands as 405; "list sandboxes" is intentionally out
// of scope so the surface stays minimal.
func (s *Server) routeRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	s.createSandbox(w, r)
}

// routeChild dispatches under /v1/sandbox/{id}/...
func (s *Server) routeChild(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sandbox/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "missing sandbox id")
		return
	}
	id := parts[0]
	tail := ""
	if len(parts) == 2 {
		tail = parts[1]
	}
	ss, ok := s.lookup(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no sandbox %q", id)
		return
	}

	// The DELETE case targets the bare id with no tail.
	if tail == "" {
		switch r.Method {
		case http.MethodDelete:
			s.deleteSandbox(w, id)
		default:
			writeError(w, http.StatusMethodNotAllowed, "use DELETE on /{id}")
		}
		return
	}

	switch tail {
	case "sql/exec":
		ss.handleSQLExec(w, r)
	case "sql/query":
		ss.handleSQLQuery(w, r)
	case "inspect/catalog":
		ss.handleInspectCatalog(w, r)
	case "inspect/describe":
		ss.handleInspectDescribe(w, r)
	case "inspect/sample":
		ss.handleInspectSample(w, r)
	case "inspect/find":
		ss.handleInspectFind(w, r)
	case "inspect/diff":
		ss.handleInspectDiff(w, r)
	case "snapshot":
		ss.handleSnapshot(w, r)
	case "mark":
		ss.handleMark(w, r)
	case "restore":
		ss.handleRestore(w, r)
	case "fork":
		s.handleFork(w, r, ss)
	case "fixtures/bulk":
		ss.handleFixturesBulk(w, r)
	default:
		writeError(w, http.StatusNotFound, "no endpoint %q", tail)
	}
}

// ─────────────────────────── lifecycle ───────────────────────────

type createSandboxRequest struct {
	IR          *dsl.IR     `json:"ir"`
	Backend     Backend     `json:"backend,omitempty"`
	Seed        int64       `json:"seed,omitempty"`
	Determinism Determinism `json:"determinism,omitempty"`
}

type createSandboxResponse struct {
	ID string `json:"id"`
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req createSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	if req.IR == nil {
		writeError(w, http.StatusBadRequest, "missing ir field")
		return
	}
	sb, err := New(Options{
		Backend:     req.Backend,
		IR:          req.IR,
		Seed:        req.Seed,
		Determinism: req.Determinism,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "new sandbox: %v", err)
		return
	}
	id := s.register(sb)
	writeJSONTimed(w, http.StatusCreated, createSandboxResponse{ID: id}, start)
}

func (s *Server) register(sb *Sandbox) string {
	id := strconv.FormatInt(s.nextID.Add(1), 36)
	s.mu.Lock()
	s.sandboxes[id] = &serverSandbox{id: id, sb: sb, marks: map[string]*Mark{}}
	s.mu.Unlock()
	return id
}

// Register pre-installs a sandbox on the server so callers can route
// to it without the POST /v1/sandbox round-trip. The CLI uses this so
// `tide sandbox boot <schema>` produces a working endpoint
// immediately. Returns the assigned id.
func (s *Server) Register(sb *Sandbox) string { return s.register(sb) }

// Unregister evicts a sandbox from the Server's internal map without
// invoking Close on it — the caller (typically the BFF's TTL janitor)
// has its own Close lifecycle, and a Server-side Close here would race
// with concurrent in-flight requests. Returns true if a sandbox by
// that id was registered. Idempotent; safe to call twice.
//
// Used by the console BFF's TTL janitor to drop idle sandboxes; the
// runtime keeps no opinion on lifecycle policy.
func (s *Server) Unregister(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sandboxes[id]; !ok {
		return false
	}
	delete(s.sandboxes, id)
	return true
}

// Sandbox returns the live *Sandbox registered under id, or nil if
// none. Used by the console BFF's Fork handler to call Sandbox.Fork
// directly — going through the HTTP /fork endpoint would auto-register
// the children under runtime-mint ids the BFF's owner-tracking map
// would never see, producing instant 404s on subsequent calls.
//
// Bypassing the HTTP layer lets the BFF own pubID minting for children
// at the same point as the parent.
func (s *Server) Sandbox(id string) *Sandbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ss, ok := s.sandboxes[id]
	if !ok {
		return nil
	}
	return ss.sb
}

func (s *Server) lookup(id string) (*serverSandbox, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ss, ok := s.sandboxes[id]
	return ss, ok
}

func (s *Server) deleteSandbox(w http.ResponseWriter, id string) {
	s.mu.Lock()
	ss, ok := s.sandboxes[id]
	if ok {
		delete(s.sandboxes, id)
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no sandbox %q", id)
		return
	}
	_ = ss.sb.Close()
	w.WriteHeader(http.StatusNoContent)
}

// ─────────────────────────── SQL ───────────────────────────

type sqlRequest struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args,omitempty"`
}

type execResponse struct {
	RowsAffected int64 `json:"rows_affected"`
}

type queryResponse struct {
	Rows []map[string]any `json:"rows"`
}

// handleSQLExec runs Pool.Exec — the path codegen emits for UPDATE /
// DELETE / INSERT-no-RETURNING. Returns rowsAffected verbatim plus
// `t_server_us` so the console's right-rail counter shows true op cost
// rather than wall-clock-around-fetch.
func (ss *serverSandbox) handleSQLExec(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req sqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: %v", err)
		return
	}
	tag, err := ss.sb.Pool().Exec(context.Background(), req.SQL, coerceArgs(req.Args)...)
	if err != nil {
		writeError(w, http.StatusBadRequest, "exec: %v", err)
		return
	}
	writeJSONTimed(w, http.StatusOK, execResponse{RowsAffected: tag.RowsAffected()}, start)
}

// handleSQLQuery runs Pool.Query and walks every row into a map. The
// shape is intentionally generic — the HTTP surface doesn't require
// callers to know column types in advance, which matches how an
// LLM-driven agent would consume the response.
func (ss *serverSandbox) handleSQLQuery(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req sqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: %v", err)
		return
	}
	rows, err := ss.sb.Pool().Query(context.Background(), req.SQL, coerceArgs(req.Args)...)
	if err != nil {
		writeError(w, http.StatusBadRequest, "query: %v", err)
		return
	}
	defer rows.Close()

	// Column names come from the rows cursor itself when it implements
	// the optional Columns() extension — simRows and returningRows both
	// do, and they know the post-expansion projection shape (notably
	// for SELECT *, which a SQL string scanner can't know). Falls back
	// to projectionColumns(req.SQL) when the rows implementation
	// doesn't expose Columns().
	type columnsProvider interface{ Columns() []string }
	var cols []string
	if cp, ok := rows.(columnsProvider); ok {
		cols = cp.Columns()
	} else {
		cols, err = projectionColumns(req.SQL)
		if err != nil {
			writeError(w, http.StatusBadRequest, "parse projection: %v", err)
			return
		}
	}

	var out []map[string]any
	for rows.Next() {
		// Build a slice of scan targets matching column count.
		dests := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dests {
			ptrs[i] = &dests[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			writeError(w, http.StatusInternalServerError, "scan: %v", err)
			return
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = normalizeWireValue(dests[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "rows: %v", err)
		return
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSONTimed(w, http.StatusOK, queryResponse{Rows: out}, start)
}

// ─────────────────────────── Inspect ───────────────────────────

// requireSim writes a 400 and returns true when the sandbox is
// embedded-backed — every Inspect / Mark / Snapshot / Fixtures handler
// touches `s.pool` / `s.Catalog()` directly, both of which nil-panic
// on embedded. Until the embedded backend grows real implementations
// for these (likely via information_schema + pg_dump), gate at the
// HTTP boundary so the SPA gets a clean message instead of a 500.
func (ss *serverSandbox) requireSim(w http.ResponseWriter, feature string) bool {
	if ss.sb.IsEmbedded() {
		writeError(w, http.StatusBadRequest,
			"%s is in-memory only; boot a Sim sandbox to use it", feature)
		return true
	}
	return false
}

// handleInspectCatalog returns the list of qualified entity names the
// sandbox has registered. Used by the console's Tables tab to populate
// a dropdown — without this, the operator has to guess the qualified
// form, which depends on whether the entity declared a `table` override
// (e.g. consumer.accounts) or uses the codegen default
// (atlantis.<ns>_<entity>).
func (ss *serverSandbox) handleInspectCatalog(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if ss.requireSim(w, "Catalog") {
		return
	}
	cat := ss.sb.Catalog()
	names := []string{}
	if cat != nil {
		names = cat.QualifiedNames()
	}
	writeJSONTimed(w, http.StatusOK, map[string]any{"entities": names}, start)
}

func (ss *serverSandbox) handleInspectDescribe(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if ss.requireSim(w, "Describe") {
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "missing q (qualified name)")
		return
	}
	d, err := ss.sb.Inspect().Describe(q)
	if err != nil {
		writeError(w, http.StatusNotFound, "%v", err)
		return
	}
	writeJSONTimed(w, http.StatusOK, d, start)
}

func (ss *serverSandbox) handleInspectSample(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if ss.requireSim(w, "Sample") {
		return
	}
	q := r.URL.Query().Get("q")
	n := 5
	if ns := r.URL.Query().Get("n"); ns != "" {
		if parsed, err := strconv.Atoi(ns); err == nil && parsed >= 0 {
			n = parsed
		}
	}
	rows, err := ss.sb.Inspect().Sample(q, n)
	if err != nil {
		writeError(w, http.StatusNotFound, "%v", err)
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	for i := range rows {
		for k, v := range rows[i] {
			rows[i][k] = normalizeWireValue(v)
		}
	}
	writeJSONTimed(w, http.StatusOK, map[string]any{"rows": rows}, start)
}

type findRequest struct {
	Qualified  string         `json:"qualified"`
	Predicates []predicateDTO `json:"predicates"`
}

type predicateDTO struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	Value  any    `json:"value,omitempty"`
}

func (ss *serverSandbox) handleInspectFind(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if ss.requireSim(w, "Find") {
		return
	}
	var req findRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: %v", err)
		return
	}
	preds := make([]Predicate, len(req.Predicates))
	for i, p := range req.Predicates {
		preds[i] = Predicate{
			Column: p.Column,
			Op:     PredicateOp(p.Op),
			Value:  coerceArg(p.Value),
		}
	}
	rows, err := ss.sb.Inspect().Find(req.Qualified, preds...)
	if err != nil {
		writeError(w, http.StatusBadRequest, "find: %v", err)
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	for i := range rows {
		for k, v := range rows[i] {
			rows[i][k] = normalizeWireValue(v)
		}
	}
	writeJSONTimed(w, http.StatusOK, map[string]any{"rows": rows}, start)
}

type diffRequest struct {
	BeforeMarkID string `json:"before_mark_id"`
	AfterMarkID  string `json:"after_mark_id"`
}

func (ss *serverSandbox) handleInspectDiff(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if ss.requireSim(w, "Compare") {
		return
	}
	var req diffRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: %v", err)
		return
	}
	ss.mu.Lock()
	a := ss.marks[req.BeforeMarkID]
	b := ss.marks[req.AfterMarkID]
	ss.mu.Unlock()
	if a == nil || b == nil {
		writeError(w, http.StatusNotFound, "unknown mark id(s)")
		return
	}
	d, err := ss.sb.Inspect().Diff(a, b)
	if err != nil {
		writeError(w, http.StatusBadRequest, "diff: %v", err)
		return
	}
	writeJSONTimed(w, http.StatusOK, d, start)
}

// ─────────────────────────── Snapshot ───────────────────────────

func (ss *serverSandbox) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method == http.MethodGet || r.Method == http.MethodPut {
		if ss.requireSim(w, "Snapshot") {
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		blob, err := ss.sb.Snapshot()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "snapshot: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		// Binary response: surface server-measured timing via header
		// since we can't slip it into a JSON body. Console reads this
		// to populate the right-rail counter.
		w.Header().Set("X-Atl-Server-Us", strconv.FormatInt(time.Since(start).Microseconds(), 10))
		_, _ = w.Write(blob)
	case http.MethodPut:
		blob, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "read body: %v", err)
			return
		}
		if err := ss.sb.Restore(blob); err != nil {
			writeError(w, http.StatusBadRequest, "restore: %v", err)
			return
		}
		w.Header().Set("X-Atl-Server-Us", strconv.FormatInt(time.Since(start).Microseconds(), 10))
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET or PUT")
	}
}

// ─────────────────────────── Mark / Restore ───────────────────────────

type markResponse struct {
	MarkID string `json:"mark_id"`
}

func (ss *serverSandbox) handleMark(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if ss.requireSim(w, "Checkpoint") {
		return
	}
	m := ss.sb.Mark()
	ss.mu.Lock()
	id := strconv.FormatInt(int64(len(ss.marks)+1), 36)
	ss.marks[id] = m
	ss.mu.Unlock()
	writeJSONTimed(w, http.StatusCreated, markResponse{MarkID: id}, start)
}

type restoreRequest struct {
	MarkID string `json:"mark_id"`
}

func (ss *serverSandbox) handleRestore(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req restoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: %v", err)
		return
	}
	ss.mu.Lock()
	m := ss.marks[req.MarkID]
	ss.mu.Unlock()
	if m == nil {
		writeError(w, http.StatusNotFound, "no mark %q", req.MarkID)
		return
	}
	if err := ss.sb.RestoreTo(m); err != nil {
		writeError(w, http.StatusBadRequest, "restore: %v", err)
		return
	}
	// Restore returns 204 NoContent so we use the header for timing.
	w.Header().Set("X-Atl-Server-Us", strconv.FormatInt(time.Since(start).Microseconds(), 10))
	w.WriteHeader(http.StatusNoContent)
}

// ─────────────────────────── Fork ───────────────────────────

type forkRequest struct {
	N int `json:"n"`
}

type forkResponse struct {
	IDs []string `json:"ids"`
}

func (s *Server) handleFork(w http.ResponseWriter, r *http.Request, ss *serverSandbox) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req forkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: %v", err)
		return
	}
	kids, err := ss.sb.Fork(req.N)
	if err != nil {
		writeError(w, http.StatusBadRequest, "fork: %v", err)
		return
	}
	ids := make([]string, len(kids))
	for i, k := range kids {
		ids[i] = s.register(k)
	}
	writeJSONTimed(w, http.StatusCreated, forkResponse{IDs: ids}, start)
}

// ─────────────────────────── Fixtures ───────────────────────────

type fixturesBulkRequest struct {
	Qualified string `json:"qualified"`
	N         int    `json:"n"`
	Seed      int64  `json:"seed,omitempty"`
	PKStart   int64  `json:"pk_start,omitempty"`
}

type fixturesBulkResponse struct {
	Inserted int `json:"inserted"`
}

func (ss *serverSandbox) handleFixturesBulk(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if ss.requireSim(w, "Seed") {
		return
	}
	var req fixturesBulkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: %v", err)
		return
	}
	n, err := ss.sb.Fixtures().Bulk(context.Background(), req.Qualified, req.N, BulkOptions{
		Seed:    req.Seed,
		PKStart: req.PKStart,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "bulk: %v", err)
		return
	}
	writeJSONTimed(w, http.StatusOK, fixturesBulkResponse{Inserted: n}, start)
}

// ─────────────────────────── helpers ───────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeJSONTimed wraps any JSON-serialisable body with a top-level
// `t_server_us` field capturing the elapsed time since `start`. Used
// by every sandbox endpoint so the console's right-rail "live perf
// counters" report server-measured latency, not wall-clock-around-fetch
// (which would conflate sub-microsecond op cost with transport ms).
//
// Implemented by marshalling, unmarshalling to a generic map, adding
// the field, then re-marshalling. The double-encode cost (single-digit
// µs) is negligible against the network round-trip and gives us a
// one-line conversion at every handler site.
func writeJSONTimed(w http.ResponseWriter, status int, body any, start time.Time) {
	raw, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal: %v", err)
		return
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		m = map[string]any{}
	}
	m["t_server_us"] = time.Since(start).Microseconds()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(m)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, args...)})
}

// coerceArgs normalizes JSON-decoded args before passing to the
// executor. JSON numbers come back as float64; integer-shaped values
// are converted to int64. Base64 strings prefixed with "b64:" are
// decoded into []byte so JSONB bind values can be carried over JSON.
func coerceArgs(in []any) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = coerceArg(v)
	}
	return out
}

func coerceArg(v any) any {
	switch x := v.(type) {
	case float64:
		// JSON numbers default to float64. Integer-valued floats are
		// almost always the int64 the caller meant; preserve fractional
		// values as float64 so vector-distance args still arrive as
		// the right Go type.
		if x == float64(int64(x)) {
			return int64(x)
		}
		return x
	case string:
		if strings.HasPrefix(x, "b64:") {
			if dec, err := base64.StdEncoding.DecodeString(x[4:]); err == nil {
				return dec
			}
		}
		return x
	case []any:
		// Likely a vector — convert numeric-array shapes to []float32
		// (the bind type pgvector handlers expect) when every element
		// is a number. Otherwise pass through as []any so the executor's
		// ANY($1) path can iterate.
		all := true
		fs := make([]float32, 0, len(x))
		for _, e := range x {
			f, ok := e.(float64)
			if !ok {
				all = false
				break
			}
			fs = append(fs, float32(f))
		}
		if all {
			return fs
		}
	}
	return v
}

// normalizeWireValue prepares a value for JSON encoding. []byte is
// base64-encoded with the "b64:" prefix so wire round-trips stay
// reversible; []float32 is converted to []float64 so json.Marshal
// produces standard numeric arrays.
func normalizeWireValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return "b64:" + base64.StdEncoding.EncodeToString(x)
	case []float32:
		out := make([]float64, len(x))
		for i, f := range x {
			out[i] = float64(f)
		}
		return out
	}
	return v
}

// projectionColumns pulls the column list out of a SELECT (top-level
// projection) or an INSERT/UPDATE/DELETE … RETURNING clause so the
// HTTP layer can map row values back to column names. We parse this
// manually rather than reusing the simulator's parser to avoid an
// import cycle (the SQL package can't import sandbox).
//
// Best-effort: anything beyond a comma-separated list of `"col"` /
// `col [AS alias]` / `expr AS alias` lands as a positional "col_N"
// placeholder, so callers can still consume the row map even when
// the parser falls behind codegen.
func projectionColumns(sql string) ([]string, error) {
	// Strip SQL line comments before scanning — without this, a user
	// who pastes `-- SELECT * FROM old_table` above their real query
	// would have the projection picked from the comment text rather
	// than the actual SELECT below it.
	sql = stripSQLLineComments(sql)
	upper := strings.ToUpper(sql)

	// SELECT path — projection is between SELECT and FROM. When there
	// is no FROM (SELECT 1, SELECT now(), constant probes), treat the
	// remainder of the statement as the projection list. The default
	// editor body uses SELECT 1; failing to parse it would surface a
	// loud "no SELECT or RETURNING in query" error on the very first
	// Execute click.
	if si := strings.Index(upper, "SELECT"); si >= 0 {
		fi := strings.Index(upper[si:], " FROM ")
		var list string
		if fi >= 0 {
			list = sql[si+len("SELECT") : si+fi]
		} else {
			rest := strings.TrimSpace(sql[si+len("SELECT"):])
			rest = strings.TrimRight(rest, ";")
			list = rest
		}
		return splitProjectionList(list), nil
	}

	// RETURNING path — projection is everything after the last
	// RETURNING token. Used by INSERT/UPDATE/DELETE handlers that
	// route through the Query path.
	if ri := strings.LastIndex(upper, "RETURNING"); ri >= 0 {
		list := sql[ri+len("RETURNING"):]
		return splitProjectionList(list), nil
	}

	return nil, fmt.Errorf("no SELECT or RETURNING in query")
}

// stripSQLLineComments removes `-- ...` line comments from the SQL.
// Keeps the rest of the bytes intact so position-sensitive parsers
// (and the runtime simulator's own lexer, which already handles
// comments) still see the same source.
func stripSQLLineComments(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	i := 0
	for i < len(sql) {
		// Match a `--` that isn't inside a string literal. We don't
		// track quoting state here because projectionColumns runs
		// against operator-facing SQL where embedded `--` inside a
		// string is exceptionally rare; the sim's lexer is the real
		// authority and handles quotes correctly.
		if i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-' {
			j := i + 2
			for j < len(sql) && sql[j] != '\n' {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(sql[i])
		i++
	}
	return b.String()
}

// splitProjectionList carves a SELECT list into per-projection
// column-name strings honoring quoted identifiers and parens. Returns
// "col_N" placeholders when a projection's name can't be inferred
// (e.g. a function call with no AS alias).
func splitProjectionList(list string) []string {
	var out []string
	depth := 0
	inQuote := false
	last := 0
	push := func(end int) {
		piece := strings.TrimSpace(list[last:end])
		if piece == "" {
			return
		}
		out = append(out, projectionName(piece, len(out)))
	}
	for i := 0; i < len(list); i++ {
		c := list[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
			// inside a quoted identifier; ignore commas/parens
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			push(i)
			last = i + 1
		}
	}
	push(len(list))
	return out
}

// projectionName extracts the user-facing column name from one
// projection token. Rules (best-effort):
//
//   - `expr AS alias`  → alias
//   - `"colname"`      → colname
//   - `bare`           → bare
//   - anything else    → "col_N"
func projectionName(piece string, idx int) string {
	// AS alias takes precedence — works regardless of left side.
	if i := strings.LastIndex(strings.ToUpper(piece), " AS "); i >= 0 {
		alias := strings.TrimSpace(piece[i+4:])
		alias = strings.Trim(alias, `"`)
		if alias != "" {
			return alias
		}
	}
	// Bare quoted ident.
	trimmed := strings.TrimSpace(piece)
	if strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`) {
		return strings.Trim(trimmed, `"`)
	}
	// Bare unquoted ident with no spaces / operators.
	if !strings.ContainsAny(trimmed, " \t()<>=,->") {
		return trimmed
	}
	return fmt.Sprintf("col_%d", idx)
}
