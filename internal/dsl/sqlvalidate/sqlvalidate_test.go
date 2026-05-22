package sqlvalidate

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// lowerWithCustom parses a fixture containing a query or procedure and
// returns the lowered CustomQuery/CustomProcedure for the validator
// tests. We use the IR-lowering path (which already passes the dep-
// free checks) so the validator tests focus on pg_query_go semantics
// rather than overlap with IR validation.
func lowerWithCustom(t *testing.T, extra string) *dsl.IR {
	t.Helper()
	src := `
entity Account in consumer {
  id          bigint primary
  consumer_id text not null
  email       text not null
  deleted_at  timestamptz
}

entity SavedOutfit in consumer {
  id          bigint primary
  consumer_id bigint not null references consumer.Account.id
  name        text not null
  deleted_at  timestamptz
}
` + extra
	f, err := dsl.Parse("t.pc", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	return ir
}

func TestValidateCustomQuery_HappyPath(t *testing.T) {
	ir := lowerWithCustom(t, `
query OutfitsForConsumer for SavedOutfit {
  input { consumer_id: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT id, consumer_id, name FROM consumer_saved_outfit
    WHERE consumer_id = $consumer_id AND deleted_at IS NULL
  }
}
`)
	if err := ValidateCustomQuery(ir, &ir.Queries[0]); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateCustomQuery_RejectsSyntaxError(t *testing.T) {
	// pg_query rejects the SQL outright. IR lowering passes (no
	// undeclared $args, touches resolves) so the only way to reach
	// the failure is via pg_query_go.
	ir := lowerWithCustom(t, `
query Broken for SavedOutfit {
  input { x: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT FROM WHERE $x AND $x AND $x
  }
}
`)
	err := ValidateCustomQuery(ir, &ir.Queries[0])
	if err == nil {
		t.Fatal("expected syntax error, got nil")
	}
	if !strings.Contains(err.Error(), "parse failed") {
		t.Errorf("error should mention parse failure: %v", err)
	}
}

func TestValidateCustomQuery_RejectsNonSelect(t *testing.T) {
	// Queries must be reads — DML is the procedure surface, DDL is
	// the migration surface. Trying to slip an UPDATE into a query{}
	// block surfaces here.
	ir := lowerWithCustom(t, `
query Sneaky for SavedOutfit {
  input { id: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    UPDATE consumer_saved_outfit SET deleted_at = now() WHERE id = $id
  }
}
`)
	err := ValidateCustomQuery(ir, &ir.Queries[0])
	if err == nil {
		t.Fatal("expected non-SELECT rejection")
	}
	if !strings.Contains(err.Error(), "must be SELECT") {
		t.Errorf("error should mention SELECT requirement: %v", err)
	}
}

func TestValidateCustomQuery_RejectsDDL(t *testing.T) {
	// DDL statements never expose a free `$arg` slot, so the IR
	// validator's "input is unused" rule would also catch this. We
	// dodge that by using the input inside a SELECT comment-style
	// trick: input { x } is consumed by a no-op SELECT.
	ir := lowerWithCustom(t, `
query DDL for SavedOutfit {
  input { x: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    DROP TABLE consumer_saved_outfit /* uses $x for input check */
  }
}
`)
	err := ValidateCustomQuery(ir, &ir.Queries[0])
	if err == nil {
		t.Fatal("expected DDL rejection")
	}
	if !strings.Contains(err.Error(), "disallowed") {
		t.Errorf("error should mention disallowed statement: %v", err)
	}
}

func TestValidateCustomQuery_RejectsUnknownTable(t *testing.T) {
	// SQL references a table the IR doesn't know about. Common case:
	// engineer typo'd the table name, or moved the entity and forgot
	// to update the SQL.
	ir := lowerWithCustom(t, `
query MissingTable for SavedOutfit {
  input { x: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT id FROM consumer_widget WHERE id = $x
  }
}
`)
	err := ValidateCustomQuery(ir, &ir.Queries[0])
	if err == nil {
		t.Fatal("expected unknown-table error")
	}
	if !strings.Contains(err.Error(), "consumer_widget") {
		t.Errorf("error should mention the bad table: %v", err)
	}
}

func TestValidateCustomQuery_TouchesCoverage(t *testing.T) {
	// SQL references SavedOutfit but touches() only lists Account.
	// Without the coverage check, an UPDATE on SavedOutfit wouldn't
	// invalidate this query's cached PK list — silent staleness.
	ir := lowerWithCustom(t, `
query Mismatched for Account {
  input { id: bigint }
  output as Account
  sql touches(Account) {
    SELECT a.id FROM consumer_account a
    JOIN consumer_saved_outfit s ON s.consumer_id = a.id
    WHERE a.id = $id
  }
}
`)
	err := ValidateCustomQuery(ir, &ir.Queries[0])
	if err == nil {
		t.Fatal("expected touches-coverage error")
	}
	if !strings.Contains(err.Error(), "consumer.SavedOutfit") || !strings.Contains(err.Error(), "touches()") {
		t.Errorf("error should mention missing SavedOutfit in touches(): %v", err)
	}
}

func TestValidateCustomQuery_PacificCoastSchemaQualifier(t *testing.T) {
	// Fully-qualified `atlantis.consumer_account` works too.
	ir := lowerWithCustom(t, `
query Qualified for Account {
  input { id: bigint }
  output as Account
  sql touches(Account) {
    SELECT id FROM atlantis.consumer_account WHERE id = $id
  }
}
`)
	if err := ValidateCustomQuery(ir, &ir.Queries[0]); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateCustomQuery_ForeignSchemaRejected(t *testing.T) {
	// `public.consumer_account` is a real table in some clusters but
	// not the one atlantis owns; rejected to keep callers from
	// accidentally reading the wrong DB.
	ir := lowerWithCustom(t, `
query Wrong for Account {
  input { id: bigint }
  output as Account
  sql touches(Account) {
    SELECT id FROM public.consumer_account WHERE id = $id
  }
}
`)
	err := ValidateCustomQuery(ir, &ir.Queries[0])
	if err == nil {
		t.Fatal("expected foreign-schema rejection")
	}
}

func TestValidateCustomProcedure_DMLAllowed(t *testing.T) {
	// Procedure raw steps may use UPDATE / DELETE / INSERT (unlike
	// queries). DDL still rejected.
	ir := lowerWithCustom(t, `
procedure Cascade for Account {
  input { account_id: bigint }
  steps {
    sql touches(Account) {
      UPDATE consumer_account SET deleted_at = now() WHERE id = $account_id
    }
  }
}
`)
	if err := ValidateCustomProcedure(ir, &ir.Procedures[0]); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateCustomProcedure_DDLStillRejected(t *testing.T) {
	ir := lowerWithCustom(t, `
procedure DropEm for Account {
  input { x: bigint }
  steps {
    sql touches(Account) {
      DROP TABLE consumer_account /* $x for input ref check */
    }
  }
}
`)
	err := ValidateCustomProcedure(ir, &ir.Procedures[0])
	if err == nil {
		t.Fatal("expected DDL rejection")
	}
}

func TestValidateCustomProcedure_TouchesCoverageAcrossSteps(t *testing.T) {
	// Each step's touches() is checked independently. A step that
	// touches two entities but only declares one fails.
	ir := lowerWithCustom(t, `
procedure WideUpdate for Account {
  input { account_id: bigint }
  steps {
    sql touches(Account) {
      UPDATE consumer_account a SET deleted_at = now()
      FROM consumer_saved_outfit s
      WHERE s.consumer_id = a.id AND a.id = $account_id
    }
  }
}
`)
	err := ValidateCustomProcedure(ir, &ir.Procedures[0])
	if err == nil {
		t.Fatal("expected touches-coverage error")
	}
}

func TestValidateCustomQuery_CTE(t *testing.T) {
	// CTEs (WITH ...) are common in real workloads; the walker must
	// recurse into the CTE body to find table refs.
	ir := lowerWithCustom(t, `
query CTEQuery for Account {
  input { id: bigint }
  output as Account
  sql touches(Account) {
    WITH recent AS (SELECT id FROM consumer_account WHERE id = $id)
    SELECT a.id, a.email FROM consumer_account a JOIN recent r ON r.id = a.id
  }
}
`)
	if err := ValidateCustomQuery(ir, &ir.Queries[0]); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

// TestNormalizeNamedParams pins the surgical rewrite that lets pg_query
// see DSL-style `$ident` placeholders as PG positional `$1`. Strings
// and identifiers are left alone — a literal like `'$foo'` would be a
// string containing the characters `$foo`, not a parameter.
func TestNormalizeNamedParams(t *testing.T) {
	cases := []struct{ in, want string }{
		{"WHERE id = $consumer_id", "WHERE id = $1"},
		{"WHERE id = $1 AND name = $foo", "WHERE id = $1 AND name = $1"},
		{"WHERE col = '$inside_string'", "WHERE col = '$inside_string'"},
		{"WHERE \"$ident\" = $arg", "WHERE \"$ident\" = $1"},
		{"WHERE col = 'it''s a $string'", "WHERE col = 'it''s a $string'"},
		{"$ AND $ AND $arg", "$ AND $ AND $1"},
	}
	for _, c := range cases {
		got := normalizeNamedParams(c.in)
		if got != c.want {
			t.Errorf("normalizeNamedParams(%q):\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
