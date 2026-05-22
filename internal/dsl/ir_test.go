package dsl

import (
	"strings"
	"testing"
)

func mustLower(t *testing.T, src string) *IR {
	t.Helper()
	f, err := Parse("t.pc", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	return ir
}

func mustLowerErr(t *testing.T, src string) error {
	t.Helper()
	f, perr := Parse("t.pc", []byte(src))
	if perr != nil {
		return perr
	}
	_, err := Lower([]*File{f})
	if err == nil {
		t.Fatalf("expected lower error, got none")
	}
	return err
}

func TestIR_SimpleEntity(t *testing.T) {
	ir := mustLower(t, `
entity Account in consumer {
  id    bigint primary
  email text not null unique
}
`)
	if ir.Version != CurrentIRVersion {
		t.Errorf("version: got %d want %d", ir.Version, CurrentIRVersion)
	}
	if len(ir.Entities) != 1 {
		t.Fatalf("want 1 entity, got %d", len(ir.Entities))
	}
	e := ir.Entities[0]
	if e.Kind != EntityKindRegular {
		t.Errorf("kind: %v", e.Kind)
	}
	if e.ID() != "consumer.Account" {
		t.Errorf("id: %q", e.ID())
	}
	pf := e.PrimaryField()
	if pf == nil || pf.Name != "id" || !pf.NotNull {
		t.Errorf("primary field shape: %+v", pf)
	}
	email := e.FindField("email")
	if email == nil || !email.NotNull || !email.Unique {
		t.Errorf("email: %+v", email)
	}
}

func TestIR_BackfillModifier(t *testing.T) {
	ir := mustLower(t, `
entity User in consumer {
  id           bigint primary
  first_name   text not null
  last_name    text not null
  display_name text not null backfill "first_name || ' ' || last_name"
}
`)
	dn := ir.Entities[0].FindField("display_name")
	if dn == nil {
		t.Fatalf("display_name not found")
	}
	want := `first_name || ' ' || last_name`
	if dn.Backfill != want {
		t.Errorf("backfill: got %q want %q", dn.Backfill, want)
	}
	if !dn.NotNull {
		t.Errorf("not_null modifier should still apply")
	}
}

func TestIR_PrimaryImpliesNotNull(t *testing.T) {
	ir := mustLower(t, `entity A in x { id bigint primary }`)
	pf := ir.Entities[0].PrimaryField()
	if !pf.NotNull {
		t.Errorf("primary should imply not null")
	}
}

func TestIR_DurationParsing(t *testing.T) {
	cases := map[string]int{
		"1s":  1000,
		"30s": 30000,
		"10m": 600_000,
		"1h":  3_600_000,
		"7d":  7 * 24 * 3_600_000,
	}
	for s, want := range cases {
		got, err := parseDurationMS(s)
		if err != nil || got != want {
			t.Errorf("parseDurationMS(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
}

func TestIR_TagPlaceholders(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"static", nil},
		{"{a}", []string{"a"}},
		{"consumer:{consumer_id}", []string{"consumer_id"}},
		{"{a}-{b}-{c}", []string{"a", "b", "c"}},
		{"unmatched {", nil},
	}
	for _, c := range cases {
		got := parseTagPlaceholders(c.in)
		if !slicesEq(got, c.want) {
			t.Errorf("parseTagPlaceholders(%q) = %v want %v", c.in, got, c.want)
		}
	}
}

func slicesEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIR_CacheLowering(t *testing.T) {
	ir := mustLower(t, `
entity Account in consumer {
  id          bigint primary
  consumer_id bigint

  cache {
    read_through ttl=10m tag="consumer:{consumer_id}"
    invalidate_on: write(self)
    consistency = strict
  }
}
`)
	c := ir.Entities[0].Cache
	if c == nil || !c.HasReadThrough || c.TTLMS != 600_000 {
		t.Errorf("cache shape: %+v", c)
	}
	if c.Consistency != ConsistencyStrict {
		t.Errorf("consistency: %v", c.Consistency)
	}
	if len(c.TagFields) != 1 || c.TagFields[0] != "consumer_id" {
		t.Errorf("tag fields: %v", c.TagFields)
	}
	if len(c.Invalidate) != 1 || !c.Invalidate[0].Self {
		t.Errorf("invalidate: %+v", c.Invalidate)
	}
}

func TestIR_ResolveReferences(t *testing.T) {
	ir := mustLower(t, `
entity Account in consumer {
  id bigint primary
}
entity Outfit in consumer {
  id bigint primary
  consumer_id bigint references consumer.Account.id on delete cascade
}
`)
	// alphabetical: Account then Outfit
	if ir.Entities[1].Name != "Outfit" {
		t.Fatalf("expected Outfit second, got %s", ir.Entities[1].Name)
	}
	outfit := ir.Entities[1]
	cidField := outfit.FindField("consumer_id")
	if cidField.Ref == nil || cidField.Ref.TargetID != "consumer.Account" || cidField.Ref.TargetField != "id" {
		t.Errorf("ref shape: %+v", cidField.Ref)
	}
	if cidField.Ref.OnDelete != RefActionCascade {
		t.Errorf("on delete: %v", cidField.Ref.OnDelete)
	}
}

func TestIR_ResolveRelations(t *testing.T) {
	ir := mustLower(t, `
entity Outfit in consumer {
  id bigint primary
  has_many items: OutfitItem via outfit_id
}
entity OutfitItem in consumer {
  id bigint primary
  outfit_id bigint references consumer.Outfit.id
}
`)
	var outfit *Entity
	for i := range ir.Entities {
		if ir.Entities[i].Name == "Outfit" {
			outfit = &ir.Entities[i]
		}
	}
	if len(outfit.Relations) != 1 || outfit.Relations[0].TargetID != "consumer.OutfitItem" {
		t.Errorf("relation resolution: %+v", outfit.Relations)
	}
}

func TestIR_QueryTimeoutBounds(t *testing.T) {
	// 50ms minimum — we only support s/m/h/d, so test 1s lower bound.
	mustLower(t, `entity A in x { id bigint primary  query_timeout = 1s }`)
	mustLower(t, `entity A in x { id bigint primary  query_timeout = 30s }`)
	err := mustLowerErr(t, `entity A in x { id bigint primary  query_timeout = 1m }`)
	if !strings.Contains(err.Error(), "30s maximum") {
		t.Errorf("expected upper-bound error, got: %v", err)
	}
}

// ---- validation rules ----

func TestIR_Rule1_NoPrimary(t *testing.T) {
	err := mustLowerErr(t, `entity A in x { id bigint }`)
	if !strings.Contains(err.Error(), "must have a primary key") {
		t.Errorf("expected primary-key error, got %v", err)
	}
}

func TestIR_Rule1_MultiplePrimaries(t *testing.T) {
	err := mustLowerErr(t, `entity A in x { id bigint primary  alt bigint primary }`)
	if !strings.Contains(err.Error(), "multiple fields carry primary") {
		t.Errorf("expected multiple-primary error, got %v", err)
	}
}

func TestIR_Rule1_CompositePK_OK(t *testing.T) {
	mustLower(t, `entity AB in x {
  a bigint
  b bigint
  primary by a, b
}`)
}

func TestIR_Rule1_CompositeAndFieldPrimary_Rejected(t *testing.T) {
	err := mustLowerErr(t, `entity A in x {
  a bigint primary
  b bigint
  primary by a, b
}`)
	if !strings.Contains(err.Error(), "cannot combine") {
		t.Errorf("expected combine error, got %v", err)
	}
}

func TestIR_Rule2_UnknownTargetEntity(t *testing.T) {
	err := mustLowerErr(t, `
entity A in x {
  id   bigint primary
  ref  bigint references y.NonExistent.id
}`)
	if !strings.Contains(err.Error(), "unknown entity") {
		t.Errorf("expected unknown-entity error, got %v", err)
	}
}

func TestIR_Rule2_UnknownTargetField(t *testing.T) {
	err := mustLowerErr(t, `
entity Account in x { id bigint primary }
entity B in x {
  id bigint primary
  ref bigint references x.Account.nope
}`)
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("expected unknown-field error, got %v", err)
	}
}

func TestIR_Rule2_TargetNotUniqueOrPrimary(t *testing.T) {
	err := mustLowerErr(t, `
entity Account in x {
  id    bigint primary
  email text
}
entity B in x {
  id  bigint primary
  ref text references x.Account.email
}`)
	if !strings.Contains(err.Error(), "neither primary nor unique") {
		t.Errorf("expected primary/unique error, got %v", err)
	}
}

func TestIR_Rule3_UnknownRelationTarget(t *testing.T) {
	err := mustLowerErr(t, `entity A in x { id bigint primary  has_many items: Nonexistent via x }`)
	if !strings.Contains(err.Error(), "target entity") {
		t.Errorf("expected relation-target error, got %v", err)
	}
}

func TestIR_Rule3_UnknownViaField(t *testing.T) {
	err := mustLowerErr(t, `
entity A in x { id bigint primary  has_many items: B via not_a_field }
entity B in x { id bigint primary }
`)
	if !strings.Contains(err.Error(), "via field") {
		t.Errorf("expected via-field error, got %v", err)
	}
}

func TestIR_Rule4_TagReferencesUnknownField(t *testing.T) {
	err := mustLowerErr(t, `
entity A in x {
  id bigint primary
  cache { read_through ttl=10m tag="{not_a_field}" }
}`)
	if !strings.Contains(err.Error(), "tag references unknown field") {
		t.Errorf("expected tag-field error, got %v", err)
	}
}

func TestIR_Rule5_HnswRequiresVector(t *testing.T) {
	err := mustLowerErr(t, `
entity A in x {
  id  bigint primary
  v   text
  index hnsw on v ops cosine
}`)
	if !strings.Contains(err.Error(), "must be vector") {
		t.Errorf("expected hnsw type error, got %v", err)
	}
}

func TestIR_Rule6_GinRequiresJsonbOrArray(t *testing.T) {
	err := mustLowerErr(t, `
entity A in x {
  id bigint primary
  s  text
  index gin on s
}`)
	if !strings.Contains(err.Error(), "must be jsonb or array") {
		t.Errorf("expected gin type error, got %v", err)
	}
}

func TestIR_Rule6_GinAcceptsArray(t *testing.T) {
	mustLower(t, `
entity A in x {
  id bigint primary
  tags []text
  index gin on tags
}`)
}

func TestIR_Rule7_DuplicateField(t *testing.T) {
	err := mustLowerErr(t, `entity A in x { id bigint primary  id text }`)
	if !strings.Contains(err.Error(), "duplicate field") {
		t.Errorf("expected duplicate-field error, got %v", err)
	}
}

func TestIR_Rule8_DuplicateEntityGlobally(t *testing.T) {
	// Same Name, different namespaces — still considered duplicate by ID, but
	// our ID is namespace.Name so different namespaces are *not* duplicates.
	// This test verifies that genuinely duplicate IDs trip the rule.
	src := `
entity A in x { id bigint primary }
entity A in x { id bigint primary }
`
	err := mustLowerErr(t, src)
	if !strings.Contains(err.Error(), "duplicate entity") {
		t.Errorf("expected duplicate-entity error, got %v", err)
	}
}

func TestIR_Rule8_SameNameDifferentNamespaceOK(t *testing.T) {
	// Different namespaces with the same bare name are allowed.
	mustLower(t, `
entity Account in consumer { id bigint primary }
entity Account in vendor   { id bigint primary }
`)
}

func TestIR_Hypertable(t *testing.T) {
	ir := mustLower(t, `
hypertable Purchase in vendor on purchased_at {
  id           bigint primary
  qty          int not null
  purchased_at timestamptz not null
}
`)
	if ir.Entities[0].Kind != EntityKindHypertable {
		t.Errorf("kind: %v", ir.Entities[0].Kind)
	}
	if ir.Entities[0].TimeField != "purchased_at" {
		t.Errorf("time field: %v", ir.Entities[0].TimeField)
	}
}

func TestIR_HypertableMissingTimeField(t *testing.T) {
	err := mustLowerErr(t, `
hypertable P in v on no_such {
  id bigint primary
  ts timestamptz
}`)
	if !strings.Contains(err.Error(), "time field") {
		t.Errorf("expected hypertable time-field error, got %v", err)
	}
}

func TestIR_HypertableTimeFieldWrongType(t *testing.T) {
	err := mustLowerErr(t, `
hypertable P in v on ts {
  id bigint primary
  ts text
}`)
	if !strings.Contains(err.Error(), "must be timestamptz") {
		t.Errorf("expected hypertable type error, got %v", err)
	}
}

// ---- JSON round-trip ----

func TestIR_JSONRoundTrip(t *testing.T) {
	ir := mustLower(t, `
entity Account in consumer {
  id    bigint primary
  email text not null unique
  cache { read_through ttl=10m tag="account:{id}" }
}
entity Outfit in consumer {
  id          bigint primary
  consumer_id bigint references consumer.Account.id on delete cascade
}
hypertable Purchase in vendor on purchased_at {
  id           bigint primary
  purchased_at timestamptz not null
}
`)
	data, err := ir.EncodeJSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeJSONIR(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Entities) != len(ir.Entities) {
		t.Errorf("entity count drift")
	}
	for i := range ir.Entities {
		if got.Entities[i].ID() != ir.Entities[i].ID() {
			t.Errorf("entity %d id drift: %s vs %s", i, got.Entities[i].ID(), ir.Entities[i].ID())
		}
	}
}

func TestIR_RefusesNewerVersion(t *testing.T) {
	// Hand-craft a checkpoint claiming a version we don't support.
	bad := []byte(`{"version":9999,"entities":[]}`)
	_, err := DecodeJSONIR(bad)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("expected version-mismatch error, got %v", err)
	}
}

func TestIR_StableOrdering(t *testing.T) {
	ir := mustLower(t, `
entity C in z { id bigint primary }
entity A in y { id bigint primary }
entity B in y { id bigint primary }
`)
	want := []string{"y.A", "y.B", "z.C"}
	for i, w := range want {
		if ir.Entities[i].ID() != w {
			t.Errorf("entity %d: got %s want %s", i, ir.Entities[i].ID(), w)
		}
	}
}

func TestIR_PartitionBy_SetsField(t *testing.T) {
	ir := mustLower(t, `
entity Order in consumer {
  id          bigint primary
  consumer_id text not null
  partition by consumer_id
}
`)
	if got := ir.Entities[0].PartitionField; got != "consumer_id" {
		t.Errorf("PartitionField = %q, want consumer_id", got)
	}
}

func TestIR_PartitionBy_UnknownFieldRejects(t *testing.T) {
	err := mustLowerErr(t, `
entity Order in consumer {
  id          bigint primary
  partition by nonexistent
}
`)
	if err == nil {
		t.Fatalf("expected error for unknown partition field")
	}
}

func TestIR_PartitionBy_NullableRejects(t *testing.T) {
	// Nullable partition columns let rows escape tenant isolation
	// (NULL = NULL is false). Validation rejects.
	err := mustLowerErr(t, `
entity Order in consumer {
  id          bigint primary
  consumer_id text
  partition by consumer_id
}
`)
	if err == nil {
		t.Fatalf("expected error for nullable partition field")
	}
}

// ---- Step 7.5: custom query / procedure lowering + validation ----
//
// Lowering tests check the resolved IR shape; validation tests pin the
// rejection cases the cache / safety invariants depend on. Identifier
// resolution INSIDE the raw SQL body is the pg_query_go layer's job
// (covered in the tidectl plan tests); this file covers the dep-free
// validator that runs on every codegen pass.

const customQuerySchemaFixture = `
entity Account in consumer {
  id          bigint primary
  consumer_id text not null
  deleted_at  timestamptz
}

entity SavedOutfit in consumer {
  id          bigint primary
  consumer_id bigint not null references consumer.Account.id
  name        text not null
  deleted_at  timestamptz
}
`

func TestIR_LowerQuery_BasicShape(t *testing.T) {
	src := customQuerySchemaFixture + `
query OutfitsForConsumer for SavedOutfit {
  input { consumer_id: bigint, limit: int default 20 }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT * FROM consumer_saved_outfit
    WHERE consumer_id = $consumer_id AND deleted_at IS NULL
    LIMIT $limit
  }
}
`
	ir := mustLower(t, src)
	if len(ir.Queries) != 1 {
		t.Fatalf("queries: got %d, want 1", len(ir.Queries))
	}
	q := ir.Queries[0]
	if q.Name != "OutfitsForConsumer" {
		t.Errorf("name: %q", q.Name)
	}
	if q.Owner != "consumer.SavedOutfit" {
		t.Errorf("owner: %q, want consumer.SavedOutfit", q.Owner)
	}
	if q.ID() != "consumer.OutfitsForConsumer" {
		t.Errorf("id: %q", q.ID())
	}
	if q.Output.AsEntityID != "consumer.SavedOutfit" {
		t.Errorf("output.AsEntityID: %q", q.Output.AsEntityID)
	}
	if len(q.Touches) != 1 || q.Touches[0] != "consumer.SavedOutfit" {
		t.Errorf("touches: %+v", q.Touches)
	}
	if len(q.Inputs) != 2 {
		t.Errorf("inputs: %d", len(q.Inputs))
	}
	if q.Inputs[1].Default == nil || q.Inputs[1].Default.Kind != DefaultIRInt || q.Inputs[1].Default.Int != 20 {
		t.Errorf("limit default: %+v", q.Inputs[1].Default)
	}
}

func TestIR_LowerQuery_ColumnOutput(t *testing.T) {
	// `output { col: type, ... }` lowers to CustomOutput.Columns rather
	// than AsEntityID. The shape is what codegen needs to emit a
	// synthetic `<Query>Row` proto message for non-entity-shaped
	// returns (GROUP BY aggregates, joined projections).
	src := customQuerySchemaFixture + `
query CountOutfits for Account {
  input { consumer_id: bigint }
  output { total: bigint }
  sql touches(SavedOutfit) {
    SELECT COUNT(*) FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
  }
}
`
	ir := mustLower(t, src)
	q := ir.Queries[0]
	if q.Output.AsEntityID != "" {
		t.Errorf("output should not have AsEntityID, got %q", q.Output.AsEntityID)
	}
	if len(q.Output.Columns) != 1 || q.Output.Columns[0].Name != "total" || q.Output.Columns[0].Type.Name != "bigint" {
		t.Errorf("columns: %+v", q.Output.Columns)
	}
}

func TestIR_LowerQuery_RejectsUnknownTarget(t *testing.T) {
	// `for Widget` doesn't resolve — caller typo'd the entity name.
	err := mustLowerErr(t, customQuerySchemaFixture+`
query Foo for Widget {
  input { x: bigint }
  output as Account
  sql touches(Account) { SELECT * FROM consumer_account WHERE id = $x }
}
`)
	if !strings.Contains(err.Error(), "unknown entity") {
		t.Errorf("error should mention unknown entity, got %v", err)
	}
}

func TestIR_LowerQuery_RejectsUnknownTouches(t *testing.T) {
	// Wrong `touches()` = silent stale cache. The validator catches it.
	err := mustLowerErr(t, customQuerySchemaFixture+`
query Foo for Account {
  input { x: bigint }
  output as Account
  sql touches(Widget) { SELECT * FROM consumer_account WHERE id = $x }
}
`)
	if !strings.Contains(err.Error(), "touches") || !strings.Contains(err.Error(), "Widget") {
		t.Errorf("error should mention touches/Widget, got %v", err)
	}
}

func TestIR_LowerQuery_RejectsDuplicateTouches(t *testing.T) {
	err := mustLowerErr(t, customQuerySchemaFixture+`
query Foo for Account {
  input { x: bigint }
  output as Account
  sql touches(Account, Account) { SELECT * FROM consumer_account WHERE id = $x }
}
`)
	if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("error should mention duplicate touches, got %v", err)
	}
}

func TestIR_LowerQuery_RejectsUndeclaredArg(t *testing.T) {
	// $consumer_id is used in SQL but not declared in input{}.
	// Without this check, callers could exfiltrate "data for the
	// previous request's $consumer_id" through cache key reuse.
	err := mustLowerErr(t, customQuerySchemaFixture+`
query Foo for SavedOutfit {
  input { x: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT * FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
  }
}
`)
	if !strings.Contains(err.Error(), "$consumer_id") || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("error should mention undeclared arg, got %v", err)
	}
}

func TestIR_LowerQuery_RejectsUnusedInput(t *testing.T) {
	// Declared inputs that the SQL never references are almost always
	// typos. We surface them as errors so the engineer can clean up
	// before the proto goes on the wire.
	err := mustLowerErr(t, customQuerySchemaFixture+`
query Foo for SavedOutfit {
  input { consumer_id: bigint, never_used: text }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT * FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
  }
}
`)
	if !strings.Contains(err.Error(), "never_used") {
		t.Errorf("error should mention unused input, got %v", err)
	}
}

func TestIR_LowerQuery_NamespaceQualifiedTouches(t *testing.T) {
	// `touches(consumer.Foo, vendor.Bar)` resolves cross-namespace.
	src := `
entity Cart in consumer { id varchar(9) primary  consumer_id text not null }
entity Cart in vendor { id varchar(10) primary  vendor_id text not null }

query CrossNS for consumer.Cart {
  input { id: text }
  output as consumer.Cart
  sql touches(consumer.Cart, vendor.Cart) {
    SELECT * FROM consumer_cart WHERE id = $id
  }
}
`
	ir := mustLower(t, src)
	q := ir.Queries[0]
	if len(q.Touches) != 2 {
		t.Fatalf("touches: %+v", q.Touches)
	}
	// Touches sorted for determinism.
	if q.Touches[0] != "consumer.Cart" || q.Touches[1] != "vendor.Cart" {
		t.Errorf("touches order: %+v", q.Touches)
	}
}

func TestIR_LowerProcedure_TypedSteps(t *testing.T) {
	src := customQuerySchemaFixture + `
procedure DeleteConsumerCascade for Account {
  input { account_id: bigint }
  steps {
    update SavedOutfit set deleted_at = now() where consumer_id = $account_id
    delete Account where id = $account_id
  }
  invalidate: tag("consumer:{account_id}")
}
`
	ir := mustLower(t, src)
	if len(ir.Procedures) != 1 {
		t.Fatalf("procedures: got %d", len(ir.Procedures))
	}
	p := ir.Procedures[0]
	if p.Owner != "consumer.Account" {
		t.Errorf("owner: %q", p.Owner)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("steps: %d", len(p.Steps))
	}
	step0 := p.Steps[0].Typed
	if step0 == nil || step0.Verb != "update" || step0.TargetID != "consumer.SavedOutfit" {
		t.Errorf("step 0: %+v", step0)
	}
	if len(step0.Assigns) != 1 || step0.Assigns[0].Field != "deleted_at" {
		t.Errorf("step 0 assigns: %+v", step0.Assigns)
	}
	if step0.Assigns[0].Value.Kind != ExprLiteralNow {
		t.Errorf("step 0 value kind: %v", step0.Assigns[0].Value.Kind)
	}
	if step0.Where == nil || step0.Where.Kind != ExprBinary {
		t.Errorf("step 0 where: %+v", step0.Where)
	}
	if p.Invalidate != "consumer:{account_id}" {
		t.Errorf("invalidate: %q", p.Invalidate)
	}
}

// Procedure-level $arg usage check unions references across every
// step before flagging unused inputs. A procedure that legitimately
// uses $a only in step 1 and $b only in step 2 must validate cleanly
// — the previous per-step check rejected this shape because step 2
// "didn't reference" $a.
func TestIR_LowerProcedure_AggregatesArgRefsAcrossSteps(t *testing.T) {
	src := customQuerySchemaFixture + `
procedure SplitWork for Account {
  input { account_id: bigint, new_consumer_id: text }
  steps {
    sql touches(SavedOutfit) {
      DELETE FROM consumer_saved_outfit WHERE consumer_id = $account_id
    }
    sql touches(Account) {
      UPDATE consumer_account SET consumer_id = $new_consumer_id WHERE id = 1
    }
  }
}
`
	ir := mustLower(t, src)
	if len(ir.Procedures) != 1 {
		t.Fatalf("expected one procedure, got %d", len(ir.Procedures))
	}
}

// Genuinely unused inputs still surface — declared but referenced
// by zero steps stays a hard error, attributed to the input's
// declaration position.
func TestIR_LowerProcedure_RejectsUnusedInputAcrossAllSteps(t *testing.T) {
	err := mustLowerErr(t, customQuerySchemaFixture+`
procedure SplitWork for Account {
  input { account_id: bigint, never_used: text }
  steps {
    sql touches(SavedOutfit) {
      DELETE FROM consumer_saved_outfit WHERE consumer_id = $account_id
    }
    sql touches(Account) {
      UPDATE consumer_account SET deleted_at = now() WHERE id = $account_id
    }
  }
}
`)
	if !strings.Contains(err.Error(), "never_used") {
		t.Errorf("error should mention unused input, got %v", err)
	}
}

// Typed-step expressions feed into the union too — an input referenced
// only inside a typed WHERE clause must not be flagged as unused.
func TestIR_LowerProcedure_TypedStepArgRefsCountedTowardUsage(t *testing.T) {
	src := customQuerySchemaFixture + `
procedure DeleteOne for Account {
  input { id: bigint }
  steps {
    delete Account where id = $id
  }
}
`
	ir := mustLower(t, src)
	if len(ir.Procedures) != 1 {
		t.Fatalf("expected one procedure, got %d", len(ir.Procedures))
	}
}

func TestIR_LowerProcedure_RejectsUnknownColumn(t *testing.T) {
	err := mustLowerErr(t, customQuerySchemaFixture+`
procedure Bad for Account {
  input { x: bigint }
  steps {
    update Account set unknown_col = $x where id = $x
  }
}
`)
	if !strings.Contains(err.Error(), "unknown column") {
		t.Errorf("error should mention unknown column, got %v", err)
	}
}

func TestIR_LowerProcedure_RejectsUndeclaredArgInWhere(t *testing.T) {
	err := mustLowerErr(t, customQuerySchemaFixture+`
procedure Bad for Account {
  input { x: bigint }
  steps {
    update Account set consumer_id = $x where id = $missing
  }
}
`)
	if !strings.Contains(err.Error(), "$missing") {
		t.Errorf("error should mention undeclared arg, got %v", err)
	}
}

func TestIR_LowerProcedure_RawSQLStep(t *testing.T) {
	src := customQuerySchemaFixture + `
procedure CascadeOutfitItems for SavedOutfit {
  input { consumer_id: bigint }
  steps {
    sql touches(SavedOutfit) {
      DELETE FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
    }
  }
}
`
	ir := mustLower(t, src)
	p := ir.Procedures[0]
	if len(p.Steps) != 1 || p.Steps[0].Raw == nil {
		t.Fatalf("steps: %+v", p.Steps)
	}
	if len(p.Steps[0].Raw.Touches) != 1 || p.Steps[0].Raw.Touches[0] != "consumer.SavedOutfit" {
		t.Errorf("raw touches: %+v", p.Steps[0].Raw.Touches)
	}
}

func TestIR_LowerProcedure_RejectsDuplicateName(t *testing.T) {
	err := mustLowerErr(t, customQuerySchemaFixture+`
procedure Foo for Account {
  input { x: bigint }
  steps { delete Account where id = $x }
}

procedure Foo for SavedOutfit {
  input { x: bigint }
  steps { delete SavedOutfit where id = $x }
}
`)
	if !strings.Contains(err.Error(), "duplicate procedure") {
		t.Errorf("error should mention duplicate procedure, got %v", err)
	}
}

func TestIR_LowerProcedure_RejectsEmptySteps(t *testing.T) {
	err := mustLowerErr(t, customQuerySchemaFixture+`
procedure Empty for Account {
  input { x: bigint }
  steps {}
}
`)
	if !strings.Contains(err.Error(), "at least one step") {
		t.Errorf("error should mention empty steps, got %v", err)
	}
}

func TestIR_LowerCustom_DeterministicOrder(t *testing.T) {
	// Queries + procedures sorted by ID so codegen output is stable.
	src := customQuerySchemaFixture + `
query Zeta for Account {
  input { x: bigint }
  output as Account
  sql touches(Account) { SELECT * FROM consumer_account WHERE id = $x }
}

query Alpha for Account {
  input { x: bigint }
  output as Account
  sql touches(Account) { SELECT * FROM consumer_account WHERE id = $x }
}
`
	ir := mustLower(t, src)
	if ir.Queries[0].Name != "Alpha" || ir.Queries[1].Name != "Zeta" {
		t.Errorf("query order: %v", []string{ir.Queries[0].Name, ir.Queries[1].Name})
	}
}
