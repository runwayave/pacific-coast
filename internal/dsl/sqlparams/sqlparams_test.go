package sqlparams

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func TestNormalizeNamed_RewritesAndOrders(t *testing.T) {
	inputs := []dsl.QueryParam{
		{Name: "user_id", Type: dsl.FieldType{Name: "varchar"}},
		{Name: "since", Type: dsl.FieldType{Name: "timestamptz"}},
	}
	got, order, err := NormalizeNamed(
		"SELECT * FROM t WHERE user_id = $user_id AND created_at >= $since",
		inputs,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "SELECT * FROM t WHERE user_id = $1 AND created_at >= $2" {
		t.Errorf("got SQL: %s", got)
	}
	if len(order) != 2 || order[0] != "user_id" || order[1] != "since" {
		t.Errorf("got order: %v", order)
	}
}

func TestNormalizeNamed_DedupesRepeatedReferences(t *testing.T) {
	inputs := []dsl.QueryParam{{Name: "id", Type: dsl.FieldType{Name: "bigint"}}}
	got, order, err := NormalizeNamed(
		"SELECT * FROM t WHERE id = $id OR parent_id = $id",
		inputs,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "SELECT * FROM t WHERE id = $1 OR parent_id = $1" {
		t.Errorf("got SQL: %s", got)
	}
	if len(order) != 1 || order[0] != "id" {
		t.Errorf("got order: %v", order)
	}
}

func TestNormalizeNamed_RejectsUndeclared(t *testing.T) {
	inputs := []dsl.QueryParam{{Name: "id", Type: dsl.FieldType{Name: "bigint"}}}
	_, _, err := NormalizeNamed("SELECT $bogus", inputs)
	if err == nil {
		t.Fatal("expected error for undeclared parameter")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the bad parameter; got: %v", err)
	}
}

func TestNormalizeNamed_PassesThroughLiterals(t *testing.T) {
	// String literals containing `$name` patterns must not be rewritten,
	// otherwise we'd corrupt user data the query is comparing against.
	inputs := []dsl.QueryParam{{Name: "x", Type: dsl.FieldType{Name: "text"}}}
	got, _, err := NormalizeNamed(
		`SELECT * FROM t WHERE col = $x AND label = 'looks like $x but is a literal'`,
		inputs,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `SELECT * FROM t WHERE col = $1 AND label = 'looks like $x but is a literal'`
	if got != want {
		t.Errorf("got:  %s\nwant: %s", got, want)
	}
}

func TestNormalizeNamed_PreservesExistingPositional(t *testing.T) {
	// A mixed-mode query that already has `$1` should keep it untouched.
	// Discouraged in practice but the scanner has to handle it cleanly.
	inputs := []dsl.QueryParam{{Name: "name", Type: dsl.FieldType{Name: "text"}}}
	got, _, err := NormalizeNamed("SELECT $1, $name", inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "SELECT $1, $1" {
		// $1 (existing) passes through; $name becomes its own $1 in this
		// scheme. That's the existing codegen semantics — leaving it
		// alone preserves the contract.
		t.Errorf("got: %s", got)
	}
}
