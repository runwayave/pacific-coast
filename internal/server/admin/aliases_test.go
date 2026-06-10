package admin

import (
	"reflect"
	"strings"
	"testing"
)

// TestNormalizeAliases_DedupsAndSorts pins the normalize contract:
// duplicates collapsed, whitespace trimmed, output sorted, empty
// strings dropped. Documents the operator-facing semantics so a
// future contributor doesn't accidentally swap to insertion-order
// preservation.
func TestNormalizeAliases_DedupsAndSorts(t *testing.T) {
	got, err := normalizeAliases("backstage", []string{"vendor", "  vendor  ", "consumer", "", "vendor"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"consumer", "vendor"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalizeAliases = %v, want %v", got, want)
	}
}

// TestNormalizeAliases_RejectsSelfAlias pins the no-self-loop rule.
// An alias equal to the caller name is redundant and probably a copy-
// paste error; refuse it explicitly so operators get a clear signal.
func TestNormalizeAliases_RejectsSelfAlias(t *testing.T) {
	_, err := normalizeAliases("backstage", []string{"vendor", "backstage"})
	if err == nil {
		t.Fatal("expected error for self-alias")
	}
	if !strings.Contains(err.Error(), "backstage") {
		t.Errorf("error should name the offending alias: %v", err)
	}
}

// TestNormalizeAliases_RejectsReserved pins the "atlantis/anonymous/*"
// reservation. Aliasing to a reserved name would let an operator
// accidentally widen authz to identities atlantis treats specially.
func TestNormalizeAliases_RejectsReserved(t *testing.T) {
	for _, reserved := range []string{"atlantis", "atlantis-console", "atlantis-signer", "anonymous", "*"} {
		t.Run(reserved, func(t *testing.T) {
			_, err := normalizeAliases("backstage", []string{reserved})
			if err == nil {
				t.Errorf("reserved alias %q should be rejected", reserved)
			}
		})
	}
}

// TestNormalizeAliases_EmptyInputReturnsEmpty pins that a clear-all
// operation produces an empty (non-nil) slice. The non-nil-ness
// matters because pgx writes nil as NULL and the column is NOT NULL.
func TestNormalizeAliases_EmptyInputReturnsEmpty(t *testing.T) {
	got, err := normalizeAliases("backstage", []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Error("normalizeAliases([]) returned nil; should return non-nil empty slice for NOT NULL column")
	}
	if len(got) != 0 {
		t.Errorf("normalizeAliases([]) = %v, want []", got)
	}
}

// TestNormalizeAliases_WhitespaceOnlyDropped pins that aliases that
// are entirely whitespace are silently dropped. Prevents an
// accidentally-pasted leading-space alias from becoming a "real"
// matchable identity.
func TestNormalizeAliases_WhitespaceOnlyDropped(t *testing.T) {
	got, err := normalizeAliases("backstage", []string{"vendor", "   ", "\t"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"vendor"}) {
		t.Errorf("got %v, want [vendor]", got)
	}
}
