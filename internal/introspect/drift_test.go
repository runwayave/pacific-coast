package introspect

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// liveIdx is a tiny constructor for the unexported liveUniqueIndex so the
// table-driven cases below stay readable.
func liveIdx(schema, table, name string, partial bool, cols ...string) liveUniqueIndex {
	return liveUniqueIndex{schema: schema, table: table, name: name, columns: cols, partial: partial}
}

// declUnique builds a declaredUnique for one table from declared fields and
// declared unique column-sets.
func declUnique(entityID string, fields []string, uniques ...[]string) declaredUnique {
	du := declaredUnique{entityID: entityID, fields: map[string]bool{}, uniqueSets: map[string]bool{}}
	for _, f := range fields {
		du.fields[f] = true
	}
	for _, u := range uniques {
		du.uniqueSets[uniqueKey(u)] = true
	}
	return du
}

func TestClassifyUniqueIndexDrift(t *testing.T) {
	pv := physRef{"vendor", "product_variants"}

	cases := []struct {
		name     string
		declared map[physRef]declaredUnique
		live     map[physRef][]liveUniqueIndex
		wantN    int
		wantName string // first drift index name when wantN > 0
	}{
		{
			name: "incident: bare unique on a declared non-unique column",
			declared: map[physRef]declaredUnique{
				pv: declUnique("vendor.ProductVariant", []string{"id", "sku", "product_id"}),
			},
			live: map[physRef][]liveUniqueIndex{
				pv: {liveIdx("vendor", "product_variants", "idx_product_variants_sku_unique", false, "sku")},
			},
			wantN:    1,
			wantName: "idx_product_variants_sku_unique",
		},
		{
			name: "declared field-unique → not drift (matches atlantis-owned uniqueness)",
			declared: map[physRef]declaredUnique{
				pv: declUnique("vendor.ProductVariant", []string{"id", "sku"}, []string{"sku"}),
			},
			live: map[physRef][]liveUniqueIndex{
				pv: {liveIdx("vendor", "product_variants", "product_variants_sku_key", false, "sku")},
			},
			wantN: 0,
		},
		{
			name: "composite unique matches regardless of column order",
			declared: map[physRef]declaredUnique{
				pv: declUnique("vendor.ProductVariant", []string{"org_id", "slug"}, []string{"org_id", "slug"}),
			},
			live: map[physRef][]liveUniqueIndex{
				// live index reports columns in (slug, org_id) order
				pv: {liveIdx("vendor", "product_variants", "t_org_id_slug_key", false, "slug", "org_id")},
			},
			wantN: 0,
		},
		{
			name: "unique on an undeclared column → left alone (operator's private index)",
			declared: map[physRef]declaredUnique{
				pv: declUnique("vendor.ProductVariant", []string{"id", "sku"}),
			},
			live: map[physRef][]liveUniqueIndex{
				pv: {liveIdx("vendor", "product_variants", "legacy_legacy_code_uq", false, "legacy_code")},
			},
			wantN: 0,
		},
		{
			name: "partial unique is drift even when columns match a declared full unique",
			declared: map[physRef]declaredUnique{
				pv: declUnique("vendor.ProductVariant", []string{"sku"}, []string{"sku"}),
			},
			live: map[physRef][]liveUniqueIndex{
				pv: {liveIdx("vendor", "product_variants", "uq_sku_active", true, "sku")},
			},
			wantN:    1,
			wantName: "uq_sku_active",
		},
		{
			name: "live index on a table not in the schema → ignored",
			declared: map[physRef]declaredUnique{
				pv: declUnique("vendor.ProductVariant", []string{"sku"}),
			},
			live: map[physRef][]liveUniqueIndex{
				{"vendor", "some_other_table"}: {liveIdx("vendor", "some_other_table", "x_uq", false, "y")},
			},
			wantN: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyUniqueIndexDrift(tc.declared, tc.live)
			if len(got) != tc.wantN {
				t.Fatalf("got %d drift(s), want %d: %+v", len(got), tc.wantN, got)
			}
			if tc.wantN > 0 && got[0].IndexName != tc.wantName {
				t.Errorf("first drift = %q, want %q", got[0].IndexName, tc.wantName)
			}
		})
	}
}

// TestBuildDeclaredUniques verifies field-unique, composite unique, and PK
// all land in the declared uniqueness set, and every column registers as a
// declared field.
func TestBuildDeclaredUniques(t *testing.T) {
	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name:      "ProductVariant",
		Namespace: "vendor",
		TableName: "vendor.product_variants",
		Fields: []dsl.Field{
			{Name: "id", Primary: true},
			{Name: "sku"},
			{Name: "barcode", Unique: true},
			{Name: "org_id"},
			{Name: "slug"},
		},
		Uniques: []dsl.UniqueSpec{{Fields: []string{"org_id", "slug"}}},
	}}}

	got := buildDeclaredUniques(ir)
	du, ok := got[physRef{"vendor", "product_variants"}]
	if !ok {
		t.Fatalf("declared uniques missing for product_variants: %+v", got)
	}
	if !du.fields["sku"] || !du.fields["slug"] {
		t.Errorf("declared fields incomplete: %+v", du.fields)
	}
	if !du.uniqueSets[uniqueKey([]string{"id"})] {
		t.Error("primary key should register as a declared uniqueness")
	}
	if !du.uniqueSets[uniqueKey([]string{"barcode"})] {
		t.Error("field-level unique should register")
	}
	if !du.uniqueSets[uniqueKey([]string{"slug", "org_id"})] {
		t.Error("composite unique should register order-independently")
	}
	if du.uniqueSets[uniqueKey([]string{"sku"})] {
		t.Error("non-unique field sku must NOT be a declared uniqueness")
	}
}

func TestUniqueIndexDrift_DropStatementAndDescribe(t *testing.T) {
	d := UniqueIndexDrift{
		Schema: "vendor", Table: "product_variants",
		IndexName: "idx_product_variants_sku_unique",
		Columns:   []string{"sku"},
	}
	if got, want := d.DropStatement(), `DROP INDEX "vendor"."idx_product_variants_sku_unique";`; got != want {
		t.Errorf("DropStatement() = %q, want %q", got, want)
	}
	if got, want := d.Describe(), "(sku)"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}

	p := UniqueIndexDrift{Columns: []string{"sku"}, Partial: true, Predicate: "deleted_at IS NULL"}
	if got, want := p.Describe(), "(sku) WHERE deleted_at IS NULL"; got != want {
		t.Errorf("partial Describe() = %q, want %q", got, want)
	}
}
