package backfill

import (
	"strings"
	"testing"
)

func TestChunkSQL_Shape(t *testing.T) {
	got := ChunkSQL(`"atlantis"."consumer_user"`, "id", "display_name", "first_name || ' ' || last_name")

	for _, want := range []string{
		`"atlantis"."consumer_user"`,
		`"id"`,
		`"display_name"`,
		`first_name || ' ' || last_name`,
		`WITH chunk AS`,
		`UPDATE "atlantis"."consumer_user" t`,
		`RETURNING t."id"`,
		`COALESCE(MAX("id"), $1)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ChunkSQL missing %q in:\n%s", want, got)
		}
	}
}

func TestChunkSQL_IdentifiersQuoted(t *testing.T) {
	// Field name with embedded quote — defense-in-depth, even though the
	// lexer rejects it. The quoting must double the quote, not break the
	// string literal.
	got := ChunkSQL(`"atlantis"."t"`, `id"x`, "v", "1")
	if !strings.Contains(got, `"id""x"`) {
		t.Errorf("embedded quote not doubled in identifier: %s", got)
	}
}
