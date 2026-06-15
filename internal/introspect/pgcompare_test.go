package introspect

import "testing"

// TestNormalizedEqual exercises the commutative-order comparator on already
// Postgres-normalized deparse strings: it must accept reordering of AND/OR
// operands and IN/ANY elements, and reject any genuine difference.
func TestNormalizedEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "(deleted_at IS NULL)", "(deleted_at IS NULL)", true},
		{"and reorder", "((a IS NULL) AND (b IS NULL))", "((b IS NULL) AND (a IS NULL))", true},
		{"or reorder", "((a IS NULL) OR (b IS NULL))", "((b IS NULL) OR (a IS NULL))", true},
		{"any reorder", "(tier = ANY (ARRAY[1, 2, 3]))", "(tier = ANY (ARRAY[3, 2, 1]))", true},
		{"cast preserved", "((status)::text = 'active'::text)", "((status)::text = 'active'::text)", true},
		{"nested reorder", "(((a IS NULL) AND (b IS NULL)) OR (c IS NULL))", "((c IS NULL) OR ((b IS NULL) AND (a IS NULL)))", true},

		{"and vs or", "((a IS NULL) AND (b IS NULL))", "((a IS NULL) OR (b IS NULL))", false},
		{"value differs", "(tier > 3)", "(tier > 4)", false},
		{"extra conjunct", "((a IS NULL) AND (b IS NULL))", "((a IS NULL) AND (b IS NULL) AND (c IS NULL))", false},
		{"cast differs", "((status)::text = 'x'::text)", "((status)::varchar = 'x'::text)", false},
		{"order-significant op", "(a < b)", "(b < a)", false},
		{"different element", "(tier = ANY (ARRAY[1, 2]))", "(tier = ANY (ARRAY[1, 3]))", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizedEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("normalizedEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
