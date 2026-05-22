package codegen

import (
	"strings"
	"testing"
)

// TestRewriteAtlantisTableRefs_HonorsOverride: when an entity declares
// `table "consumer.accounts"`, user-authored SQL that references the
// atlantis-flat form gets rewritten to the override location at emit
// time. This is the load-bearing case for layering atlantis on top of
// an existing database — procedures keep working without per-procedure
// hand edits.
func TestRewriteAtlantisTableRefs_HonorsOverride(t *testing.T) {
	ir := lower(t, `entity Account in consumer {
  table "consumer.accounts"
  id    bigint primary
  email text not null
}`)
	sql := `INSERT INTO atlantis.consumer_account (email) VALUES ($1)`
	out := rewriteAtlantisTableRefs(sql, ir)
	if !strings.Contains(out, `"consumer"."accounts"`) {
		t.Errorf("expected override-form name, got: %s", out)
	}
	if strings.Contains(out, "atlantis.consumer_account") {
		t.Errorf("legacy atlantis-flat name still present: %s", out)
	}
}

// TestRewriteAtlantisTableRefs_NoOverridePassesThrough: when no
// override is declared, the rewrite still happens but the substituted
// value is functionally equivalent (the atlantis schema, properly
// quoted). Regression guard against emitting nonsense for schemas
// that don't use the modifier.
func TestRewriteAtlantisTableRefs_NoOverridePassesThrough(t *testing.T) {
	ir := lower(t, `entity Account in consumer {
  id    bigint primary
  email text not null
}`)
	sql := `INSERT INTO atlantis.consumer_account (email) VALUES ($1)`
	out := rewriteAtlantisTableRefs(sql, ir)
	if !strings.Contains(out, `"atlantis"."consumer_account"`) {
		t.Errorf("expected quoted atlantis-flat form, got: %s", out)
	}
}

// TestRewriteAtlantisTableRefs_MultipleRefs: every reference in a
// single SQL body gets rewritten, not just the first.
func TestRewriteAtlantisTableRefs_MultipleRefs(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  table "consumer.accounts"
  id bigint primary
}
entity Session in consumer {
  table "consumer.sessions"
  id          bigint primary
  consumer_id bigint not null references consumer.Account.id
}
`)
	sql := `SELECT s.* FROM atlantis.consumer_session s
JOIN atlantis.consumer_account a ON a.id = s.consumer_id
WHERE a.id = $1`
	out := rewriteAtlantisTableRefs(sql, ir)
	if !strings.Contains(out, `"consumer"."sessions"`) {
		t.Errorf("Session not rewritten: %s", out)
	}
	if !strings.Contains(out, `"consumer"."accounts"`) {
		t.Errorf("Account not rewritten: %s", out)
	}
	if strings.Contains(out, "atlantis.consumer_") {
		t.Errorf("at least one legacy ref survived: %s", out)
	}
}

// TestRewriteAtlantisTableRefs_AliasPreserved: SQL aliases on the
// rewritten table reference (`FROM atlantis.foo f`) survive the
// rewrite — only the table token changes, not what follows.
func TestRewriteAtlantisTableRefs_AliasPreserved(t *testing.T) {
	ir := lower(t, `entity Account in consumer {
  table "consumer.accounts"
  id bigint primary
}`)
	sql := `SELECT a.id FROM atlantis.consumer_account a WHERE a.id = $1`
	out := rewriteAtlantisTableRefs(sql, ir)
	if !strings.Contains(out, ` a `) || !strings.Contains(out, `a.id`) {
		t.Errorf("alias not preserved: %s", out)
	}
}

// TestRewriteAtlantisTableRefs_UnknownRefSurvives: a reference to
// atlantis.<something> that doesn't match any entity in the IR passes
// through unchanged. The validator catches unknown refs at plan time;
// the rewrite stays out of validation's lane.
func TestRewriteAtlantisTableRefs_UnknownRefSurvives(t *testing.T) {
	ir := lower(t, `entity Account in consumer {
  id bigint primary
}`)
	sql := `SELECT * FROM atlantis.consumer_nonexistent WHERE id = $1`
	out := rewriteAtlantisTableRefs(sql, ir)
	if out != sql {
		t.Errorf("unknown ref shouldn't be touched, got: %s", out)
	}
}

// TestRewriteAtlantisTableRefs_CrossNamespace: a consumer-side
// procedure referencing a vendor entity gets the vendor entity's
// override applied. Mirrors the real-world pattern of consumer
// queries joining vendor catalog tables.
func TestRewriteAtlantisTableRefs_CrossNamespace(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  table "consumer.accounts"
  id bigint primary
}
entity Product in vendor {
  table "products"
  id bigint primary
}
`)
	sql := `SELECT * FROM atlantis.consumer_account a, atlantis.vendor_product p WHERE a.id = $1`
	out := rewriteAtlantisTableRefs(sql, ir)
	if !strings.Contains(out, `"consumer"."accounts"`) {
		t.Errorf("consumer ref not rewritten: %s", out)
	}
	if !strings.Contains(out, `"public"."products"`) {
		t.Errorf("vendor ref (default public) not rewritten: %s", out)
	}
}

// TestRewriteAtlantisTableRefs_NilIRIsNoop: defensive guard. Callers
// pass `ir` from the codegen pipeline; if for some reason `ir` is nil
// the rewrite returns the SQL unchanged rather than panicking.
func TestRewriteAtlantisTableRefs_NilIRIsNoop(t *testing.T) {
	sql := `INSERT INTO atlantis.foo VALUES ($1)`
	if out := rewriteAtlantisTableRefs(sql, nil); out != sql {
		t.Errorf("nil IR should pass SQL through unchanged, got: %s", out)
	}
}
