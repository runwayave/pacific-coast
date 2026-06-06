package sandbox_test

// HTTP control plane tests. httptest.Server gives us a real
// net/http round-trip without binding a real port — same code path
// that `tide sandbox boot` exercises in production, but tied to the
// test lifecycle.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
)

func httpServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(sandbox.NewServer())
	t.Cleanup(srv.Close)
	return srv
}

func httpUserIR() *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "User",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, Identity: true},
				{Name: "email", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			},
		}},
	}
}

func postJSON(t *testing.T, url string, body any) (status int, decoded map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			// non-JSON or empty body — return nil decoded
			decoded = map[string]any{"_raw": string(raw)}
		}
	}
	return resp.StatusCode, decoded
}

func getJSON(t *testing.T, url string) (status int, decoded map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded
}

// createHTTPSandbox is the lifecycle prelude for nearly every HTTP
// test in this file.
func createHTTPSandbox(t *testing.T, srv *httptest.Server, ir *dsl.IR) string {
	t.Helper()
	status, body := postJSON(t, srv.URL+"/v1/sandbox", map[string]any{"ir": ir})
	if status != http.StatusCreated {
		t.Fatalf("create sandbox: status %d body %v", status, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("missing id in create response: %v", body)
	}
	return id
}

// TestHTTPCreateAndDestroy is the smallest viable round-trip: POST
// the IR, get an id back, DELETE it.
func TestHTTPCreateAndDestroy(t *testing.T) {
	srv := httpServer(t)
	id := createHTTPSandbox(t, srv, httpUserIR())

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/sandbox/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status: got %d want 204", resp.StatusCode)
	}

	// Subsequent operations on the deleted id return 404.
	status, _ := postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/exec", map[string]any{"sql": "SELECT 1"})
	if status != http.StatusNotFound {
		t.Fatalf("post-delete status: got %d want 404", status)
	}
}

func TestHTTPSQLExecAndQuery(t *testing.T) {
	srv := httpServer(t)
	id := createHTTPSandbox(t, srv, httpUserIR())

	// Insert via /sql/query (uses RETURNING so it's a query, not exec)
	insertBody := map[string]any{
		"sql":  `INSERT INTO "atlantis"."consumer_user" ("id", "email") VALUES ($1, $2) RETURNING "id"`,
		"args": []any{1, "a@b.com"},
	}
	status, body := postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/query", insertBody)
	if status != http.StatusOK {
		t.Fatalf("insert: status %d body %v", status, body)
	}
	rows, _ := body["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("insert rows: %v", rows)
	}

	// Plain SELECT
	selBody := map[string]any{
		"sql":  `SELECT "id", "email" FROM "atlantis"."consumer_user" WHERE "id" = $1`,
		"args": []any{1},
	}
	status, body = postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/query", selBody)
	if status != http.StatusOK {
		t.Fatalf("select: status %d body %v", status, body)
	}
	rows, _ = body["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("select rows: %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["email"] != "a@b.com" {
		t.Fatalf("email: %v", row["email"])
	}

	// UPDATE via /sql/exec returns rows_affected
	updBody := map[string]any{
		"sql":  `UPDATE "atlantis"."consumer_user" SET "email" = $1 WHERE "id" = $2`,
		"args": []any{"new@b.com", 1},
	}
	status, body = postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/exec", updBody)
	if status != http.StatusOK {
		t.Fatalf("update: status %d body %v", status, body)
	}
	ra, _ := body["rows_affected"].(float64)
	if ra != 1 {
		t.Fatalf("rows_affected: %v", body["rows_affected"])
	}
}

func TestHTTPInspectDescribeAndSample(t *testing.T) {
	srv := httpServer(t)
	id := createHTTPSandbox(t, srv, httpUserIR())

	// Seed two rows.
	for i := 1; i <= 2; i++ {
		postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/query", map[string]any{
			"sql":  `INSERT INTO "atlantis"."consumer_user" ("id", "email") VALUES ($1, $2) RETURNING "id"`,
			"args": []any{i, "u@y.com"},
		})
	}

	status, body := getJSON(t, srv.URL+"/v1/sandbox/"+id+"/inspect/describe?q=atlantis.consumer_user")
	if status != http.StatusOK {
		t.Fatalf("describe status: %d body %v", status, body)
	}
	if body["qualified"] != "atlantis.consumer_user" {
		t.Fatalf("describe qualified: %v", body["qualified"])
	}
	rowCount, _ := body["row_count"].(float64)
	if int(rowCount) != 2 {
		t.Fatalf("row_count: %v", body["row_count"])
	}

	status, body = getJSON(t, srv.URL+"/v1/sandbox/"+id+"/inspect/sample?q=atlantis.consumer_user&n=10")
	if status != http.StatusOK {
		t.Fatalf("sample status: %d", status)
	}
	rows, _ := body["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("sample rows: %v", rows)
	}
}

func TestHTTPInspectFind(t *testing.T) {
	srv := httpServer(t)
	id := createHTTPSandbox(t, srv, httpUserIR())

	for i, email := range []string{"a@y.com", "b@y.com", "a@y.com"} {
		postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/query", map[string]any{
			"sql":  `INSERT INTO "atlantis"."consumer_user" ("id", "email") VALUES ($1, $2) RETURNING "id"`,
			"args": []any{i + 1, email},
		})
	}

	findBody := map[string]any{
		"qualified": "atlantis.consumer_user",
		"predicates": []map[string]any{
			{"column": "email", "op": "=", "value": "a@y.com"},
		},
	}
	status, body := postJSON(t, srv.URL+"/v1/sandbox/"+id+"/inspect/find", findBody)
	if status != http.StatusOK {
		t.Fatalf("find status: %d body %v", status, body)
	}
	rows, _ := body["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("find rows: %v", rows)
	}
}

func TestHTTPMarkAndRestore(t *testing.T) {
	srv := httpServer(t)
	id := createHTTPSandbox(t, srv, httpUserIR())

	// Seed one row.
	postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/query", map[string]any{
		"sql":  `INSERT INTO "atlantis"."consumer_user" ("id", "email") VALUES ($1, $2) RETURNING "id"`,
		"args": []any{1, "first@y.com"},
	})

	// Mark
	status, body := postJSON(t, srv.URL+"/v1/sandbox/"+id+"/mark", nil)
	if status != http.StatusCreated {
		t.Fatalf("mark status: %d", status)
	}
	markID, _ := body["mark_id"].(string)
	if markID == "" {
		t.Fatalf("mark_id missing: %v", body)
	}

	// Mutate after mark.
	postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/query", map[string]any{
		"sql":  `INSERT INTO "atlantis"."consumer_user" ("id", "email") VALUES ($1, $2) RETURNING "id"`,
		"args": []any{2, "second@y.com"},
	})
	_, body = getJSON(t, srv.URL+"/v1/sandbox/"+id+"/inspect/describe?q=atlantis.consumer_user")
	if rc, _ := body["row_count"].(float64); int(rc) != 2 {
		t.Fatalf("pre-restore row_count: %v", body["row_count"])
	}

	// Restore.
	status, body = postJSON(t, srv.URL+"/v1/sandbox/"+id+"/restore", map[string]any{"mark_id": markID})
	if status != http.StatusNoContent {
		t.Fatalf("restore status: %d body %v", status, body)
	}
	_, body = getJSON(t, srv.URL+"/v1/sandbox/"+id+"/inspect/describe?q=atlantis.consumer_user")
	if rc, _ := body["row_count"].(float64); int(rc) != 1 {
		t.Fatalf("post-restore row_count: %v want 1", body["row_count"])
	}
}

func TestHTTPFork(t *testing.T) {
	srv := httpServer(t)
	id := createHTTPSandbox(t, srv, httpUserIR())
	// Seed parent.
	postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/query", map[string]any{
		"sql":  `INSERT INTO "atlantis"."consumer_user" ("id", "email") VALUES ($1, $2) RETURNING "id"`,
		"args": []any{1, "parent@y.com"},
	})

	status, body := postJSON(t, srv.URL+"/v1/sandbox/"+id+"/fork", map[string]any{"n": 3})
	if status != http.StatusCreated {
		t.Fatalf("fork status: %d body %v", status, body)
	}
	ids, _ := body["ids"].([]any)
	if len(ids) != 3 {
		t.Fatalf("fork ids: %v want 3", ids)
	}
	// Each child sees the parent row.
	for _, kid := range ids {
		_, body := getJSON(t, srv.URL+"/v1/sandbox/"+kid.(string)+"/inspect/describe?q=atlantis.consumer_user")
		if rc, _ := body["row_count"].(float64); int(rc) != 1 {
			t.Fatalf("child %v row_count: %v want 1", kid, body["row_count"])
		}
	}
}

func TestHTTPFixturesBulk(t *testing.T) {
	srv := httpServer(t)
	id := createHTTPSandbox(t, srv, httpUserIR())

	body := map[string]any{
		"qualified": "atlantis.consumer_user",
		"n":         5,
		"seed":      7,
	}
	status, resp := postJSON(t, srv.URL+"/v1/sandbox/"+id+"/fixtures/bulk", body)
	if status != http.StatusOK {
		t.Fatalf("bulk status: %d body %v", status, resp)
	}
	if ins, _ := resp["inserted"].(float64); int(ins) != 5 {
		t.Fatalf("inserted: %v", resp["inserted"])
	}

	_, info := getJSON(t, srv.URL+"/v1/sandbox/"+id+"/inspect/describe?q=atlantis.consumer_user")
	if rc, _ := info["row_count"].(float64); int(rc) != 5 {
		t.Fatalf("post-bulk row_count: %v", info["row_count"])
	}
}

func TestHTTPSnapshotRoundTrip(t *testing.T) {
	srv := httpServer(t)
	id := createHTTPSandbox(t, srv, httpUserIR())
	postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/query", map[string]any{
		"sql":  `INSERT INTO "atlantis"."consumer_user" ("id", "email") VALUES ($1, $2) RETURNING "id"`,
		"args": []any{1, "snap@y.com"},
	})

	// GET snapshot
	resp, err := http.Get(srv.URL + "/v1/sandbox/" + id + "/snapshot")
	if err != nil {
		t.Fatalf("snapshot GET: %v", err)
	}
	blob, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(blob) == 0 {
		t.Fatalf("empty snapshot blob")
	}

	// Mutate
	postJSON(t, srv.URL+"/v1/sandbox/"+id+"/sql/exec", map[string]any{
		"sql":  `DELETE FROM "atlantis"."consumer_user" WHERE "id" = $1`,
		"args": []any{1},
	})

	// PUT snapshot restores
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/sandbox/"+id+"/snapshot", bytes.NewReader(blob))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("snapshot PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT snapshot status: %d", resp.StatusCode)
	}

	// Row count back to 1.
	_, info := getJSON(t, srv.URL+"/v1/sandbox/"+id+"/inspect/describe?q=atlantis.consumer_user")
	if rc, _ := info["row_count"].(float64); int(rc) != 1 {
		t.Fatalf("post-restore row_count: %v", info["row_count"])
	}
}

func TestHTTPMissingIR(t *testing.T) {
	srv := httpServer(t)
	status, body := postJSON(t, srv.URL+"/v1/sandbox", map[string]any{"seed": 1})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing ir, got %d body %v", status, body)
	}
	if e, _ := body["error"].(string); !strings.Contains(e, "ir") {
		t.Fatalf("error message: %v", body["error"])
	}
}
