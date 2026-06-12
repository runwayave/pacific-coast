package admin

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/introspect"
)

// indexDriftError is the structured refusal `tide apply` returns when the
// live DB enforces a UNIQUE index the schema doesn't declare. It lists each
// drifting index with the exact remediation DDL (using the live index name,
// never a reconstructed one) and the override escape hatch. Mirrors
// extensionsMissingError's copy-paste-friendly layout.
func indexDriftError(drift []introspect.UniqueIndexDrift) error {
	var b strings.Builder
	b.WriteString("apply blocked: the live database enforces UNIQUE index(es) this schema does not declare.\n")
	b.WriteString("Applying would leave a hidden constraint that silently rejects legitimate writes.\n\n")
	for _, d := range drift {
		kind := "UNIQUE index"
		if d.Partial {
			kind = "partial UNIQUE index"
		}
		fmt.Fprintf(&b, "  %s.%s — %s on %s\n", d.Schema, d.Table, kind, d.Describe())
		fmt.Fprintf(&b, "    resolve: %s\n", d.DropStatement())
		if !d.Partial {
			fmt.Fprintf(&b, "    or declare the uniqueness in your .atl (field `unique`, or `unique by %s`)\n", strings.Join(d.Columns, ", "))
		}
	}
	b.WriteString("\nIf this index is intentional and you accept it, set ATLANTIS_ALLOW_INDEX_DRIFT=1 to apply anyway.")
	return errors.New(b.String())
}
