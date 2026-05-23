package dsl

import "testing"

func TestParse_TtlField(t *testing.T) {
	f := mustParse(t, `
entity Session in consumer {
  id         varchar(8) primary
  expires_at timestamptz not null
  ttl_field  expires_at
}
`)
	if len(f.Decls) != 1 {
		t.Fatalf("expected 1 decl, got %d", len(f.Decls))
	}
}

func TestLower_TtlField(t *testing.T) {
	f := mustParse(t, `
entity Session in consumer {
  id         varchar(8) primary
  expires_at timestamptz not null
  ttl_field  expires_at
}
`)
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if ir.Entities[0].TtlField != "expires_at" {
		t.Errorf("TtlField = %q, want expires_at", ir.Entities[0].TtlField)
	}
}
