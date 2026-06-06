package sandbox_test

// Sandbox-level integration with the embedded backend. Exercises:
//
//   - BackendAuto auto-routes to embedded when the IR has a custom
//     query block (and to sim otherwise).
//   - Explicit BackendEmbedded works end-to-end.
//   - sim-only features (Mark / Fork / RestoreTo) return
//     ErrFeatureRequiresSim on an embedded sandbox.
//
// These spin up real Postgres so they're slow (~4 s per test). The
// fidelity backend's whole job is to be slow but correct.

import (
	"context"
	"errors"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
)

// plainEntityIR routes to sim under BackendAuto — no custom SQL.
func plainEntityIR() *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "Account",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
				{Name: "email", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			},
		}},
	}
}

// customQueryIR forces auto-routing to embedded — a user-authored
// query block means the sim's whitelist parser can't parse the
// caller's full SQL surface.
func customQueryIR() *dsl.IR {
	ir := plainEntityIR()
	ir.Queries = []dsl.CustomQuery{{
		Name:  "ListActive",
		Owner: "consumer.Account",
		SQL:   `SELECT * FROM atlantis.consumer_account WHERE 1 = 1`,
	}}
	return ir
}

func TestAutoRoutingPlainSchemaUsesSim(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: plainEntityIR(), Backend: sandbox.BackendAuto})
	if sb.IsEmbedded() {
		t.Fatalf("plain schema should route to sim under BackendAuto")
	}
}

func TestAutoRoutingCustomQueryUsesEmbedded(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: customQueryIR(), Backend: sandbox.BackendAuto})
	if !sb.IsEmbedded() {
		t.Fatalf("schema with custom query should route to embedded under BackendAuto")
	}
	if sb.EmbeddedBackend() == nil {
		t.Fatalf("EmbeddedBackend should return non-nil")
	}
}

func TestExplicitEmbeddedRoundTrip(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: plainEntityIR(), Backend: sandbox.BackendEmbedded})
	if !sb.IsEmbedded() {
		t.Fatalf("BackendEmbedded should produce an embedded sandbox")
	}

	ctx := context.Background()
	const ins = `INSERT INTO "atlantis"."consumer_account" ("id", "email") VALUES ($1, $2) RETURNING "id"`
	var id int64
	if err := sb.Pool().QueryRow(ctx, ins, int64(7), "x@y.com").Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id != 7 {
		t.Fatalf("RETURNING: got %d want 7", id)
	}
}

func TestEmbeddedMarkReturnsNil(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: plainEntityIR(), Backend: sandbox.BackendEmbedded})
	if m := sb.Mark(); m != nil {
		t.Fatalf("Mark on embedded should return nil; got %+v", m)
	}
}

func TestEmbeddedRestoreToErrors(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: plainEntityIR(), Backend: sandbox.BackendEmbedded})
	err := sb.RestoreTo(nil) // nil mark; embedded check fires first
	if !errors.Is(err, sandbox.ErrFeatureRequiresSim) {
		t.Fatalf("RestoreTo on embedded: got %v want ErrFeatureRequiresSim", err)
	}
}

func TestEmbeddedForkErrors(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: plainEntityIR(), Backend: sandbox.BackendEmbedded})
	_, err := sb.Fork(3)
	if !errors.Is(err, sandbox.ErrFeatureRequiresSim) {
		t.Fatalf("Fork on embedded: got %v want ErrFeatureRequiresSim", err)
	}
}

func TestEmbeddedCatalogIsNil(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: plainEntityIR(), Backend: sandbox.BackendEmbedded})
	if sb.Catalog() != nil {
		t.Fatalf("Catalog() should be nil for embedded sandbox")
	}
}
