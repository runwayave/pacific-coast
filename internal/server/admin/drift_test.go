package admin

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/introspect"
)

func TestIndexDriftError_Layout(t *testing.T) {
	err := indexDriftError([]introspect.UniqueIndexDrift{
		{
			Schema: "vendor", Table: "product_variants",
			IndexName: "idx_product_variants_sku_unique",
			Columns:   []string{"sku"},
		},
		{
			Schema: "vendor", Table: "product_variants",
			IndexName: "uq_sku_active",
			Columns:   []string{"sku"},
			Partial:   true, Predicate: "deleted_at IS NULL",
		},
	})
	msg := err.Error()

	// The exact remediation DDL must use the live index name verbatim.
	if !strings.Contains(msg, `DROP INDEX "vendor"."idx_product_variants_sku_unique";`) {
		t.Errorf("missing full-index DROP statement:\n%s", msg)
	}
	if !strings.Contains(msg, `DROP INDEX "vendor"."uq_sku_active";`) {
		t.Errorf("missing partial-index DROP statement:\n%s", msg)
	}
	// Full unique offers the declare-it alternative; partial does not (the
	// DSL can't express a partial unique).
	if !strings.Contains(msg, "unique by sku") {
		t.Errorf("full-unique drift should suggest declaring it:\n%s", msg)
	}
	if strings.Contains(msg, "WHERE deleted_at IS NULL") == false {
		t.Errorf("partial predicate should be shown:\n%s", msg)
	}
	// The escape hatch must be discoverable.
	if !strings.Contains(msg, "ATLANTIS_ALLOW_INDEX_DRIFT=1") {
		t.Errorf("missing override hint:\n%s", msg)
	}
}
