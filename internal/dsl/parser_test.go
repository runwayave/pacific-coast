package dsl

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *File {
	t.Helper()
	f, err := Parse("t.pc", []byte(src))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	return f
}

func mustParseErr(t *testing.T, src string) error {
	t.Helper()
	_, err := Parse("t.pc", []byte(src))
	if err == nil {
		t.Fatalf("expected parse error, got none")
	}
	return err
}

func TestParse_EmptyFile(t *testing.T) {
	f := mustParse(t, "")
	if len(f.Decls) != 0 {
		t.Fatalf("expected 0 decls, got %d", len(f.Decls))
	}
}

func TestParse_SimpleEntity(t *testing.T) {
	f := mustParse(t, `
entity Account in consumer {
  id    bigint primary
  email text not null unique
}
`)
	if len(f.Decls) != 1 {
		t.Fatalf("expected 1 decl, got %d", len(f.Decls))
	}
	e, ok := f.Decls[0].(*EntityDecl)
	if !ok {
		t.Fatalf("expected EntityDecl, got %T", f.Decls[0])
	}
	if e.Name != "Account" || e.Namespace != "consumer" {
		t.Fatalf("name/ns mismatch: %+v", e)
	}
	if len(e.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(e.Members))
	}
	id, ok := e.Members[0].(*FieldDecl)
	if !ok || id.Name != "id" || id.Type.Name != "bigint" || !id.HasModifier(ModPrimary) {
		t.Fatalf("id field shape wrong: %+v", e.Members[0])
	}
	email, ok := e.Members[1].(*FieldDecl)
	if !ok || email.Name != "email" || !email.HasModifier(ModNotNull) || !email.HasModifier(ModUnique) {
		t.Fatalf("email field shape wrong: %+v", e.Members[1])
	}
}

func TestParse_AllScalarTypes(t *testing.T) {
	f := mustParse(t, `
entity Kitchen in lab {
  a smallint
  b int
  c bigint
  d text
  e boolean
  f timestamptz
  g uuid
  h bytea
  i numeric(12, 4)
  j jsonb
  k vector(32)
  l []text
  m varchar(255)
  n citext
  o date
  p interval
}
`)
	e := f.Decls[0].(*EntityDecl)
	want := []struct {
		name, typ string
		arr       bool
		vecDim    int
		nump      int
		nums      int
		ln        int
	}{
		{"a", "smallint", false, 0, 0, 0, 0},
		{"b", "int", false, 0, 0, 0, 0},
		{"c", "bigint", false, 0, 0, 0, 0},
		{"d", "text", false, 0, 0, 0, 0},
		{"e", "boolean", false, 0, 0, 0, 0},
		{"f", "timestamptz", false, 0, 0, 0, 0},
		{"g", "uuid", false, 0, 0, 0, 0},
		{"h", "bytea", false, 0, 0, 0, 0},
		{"i", "numeric", false, 0, 12, 4, 0},
		{"j", "jsonb", false, 0, 0, 0, 0},
		{"k", "vector", false, 32, 0, 0, 0},
		{"l", "text", true, 0, 0, 0, 0},
		{"m", "varchar", false, 0, 0, 0, 255},
		{"n", "citext", false, 0, 0, 0, 0},
		{"o", "date", false, 0, 0, 0, 0},
		{"p", "interval", false, 0, 0, 0, 0},
	}
	for i, w := range want {
		fd := e.Members[i].(*FieldDecl)
		if fd.Name != w.name || fd.Type.Name != w.typ ||
			fd.Type.Array != w.arr || fd.Type.VecDim != w.vecDim ||
			fd.Type.NumP != w.nump || fd.Type.NumS != w.nums ||
			fd.Type.Len != w.ln {
			t.Errorf("field %d (%s): got %+v want %+v", i, w.name, fd.Type, w)
		}
	}
}

func TestParse_DefaultValues(t *testing.T) {
	f := mustParse(t, `
entity D in x {
  a int default 42
  b text default "hi"
  c boolean default true
  d boolean default false
  e timestamptz default now()
}
`)
	e := f.Decls[0].(*EntityDecl)
	checks := []DefaultKind{DefaultInt, DefaultString, DefaultBool, DefaultBool, DefaultNow}
	for i, want := range checks {
		fd := e.Members[i].(*FieldDecl)
		var got DefaultValue
		for _, m := range fd.Modifiers {
			if dm, ok := m.(*ModDefaultDecl); ok {
				got = dm.Value
			}
		}
		if got.Kind != want {
			t.Errorf("field %d: got default kind %v want %v", i, got.Kind, want)
		}
	}
}

func TestParse_References(t *testing.T) {
	f := mustParse(t, `
entity Item in shop {
  id          bigint primary
  product_id  bigint references vendor.Product.id
  variant_id  bigint references vendor.Variant.id on delete cascade
  account_id  bigint references consumer.Account.id on delete set null on update restrict
}
`)
	e := f.Decls[0].(*EntityDecl)
	productRef := e.Members[1].(*FieldDecl).Modifiers[0].(*ModReferencesDecl)
	if productRef.TargetNS != "vendor" || productRef.TargetEntity != "Product" || productRef.TargetField != "id" {
		t.Errorf("product ref shape: %+v", productRef)
	}
	if productRef.OnDelete != RefActionUnset {
		t.Errorf("product ref should have no on delete, got %v", productRef.OnDelete)
	}
	variantRef := e.Members[2].(*FieldDecl).Modifiers[0].(*ModReferencesDecl)
	if variantRef.OnDelete != RefActionCascade {
		t.Errorf("variant on delete: got %v want cascade", variantRef.OnDelete)
	}
	accountRef := e.Members[3].(*FieldDecl).Modifiers[0].(*ModReferencesDecl)
	if accountRef.OnDelete != RefActionSetNull {
		t.Errorf("account on delete: got %v want set null", accountRef.OnDelete)
	}
	if accountRef.OnUpdate != RefActionRestrict {
		t.Errorf("account on update: got %v want restrict", accountRef.OnUpdate)
	}
}

func TestParse_Relations(t *testing.T) {
	f := mustParse(t, `
entity Outfit in consumer {
  id          bigint primary
  has_many items: OutfitItem via outfit_id
  has_one cover: Image via image_id
}
`)
	e := f.Decls[0].(*EntityDecl)
	r1 := e.Members[1].(*RelationDecl)
	if r1.Kind != RelHasMany || r1.Name != "items" || r1.Target != "OutfitItem" || r1.Via != "outfit_id" {
		t.Errorf("has_many shape: %+v", r1)
	}
	r2 := e.Members[2].(*RelationDecl)
	if r2.Kind != RelHasOne || r2.Name != "cover" {
		t.Errorf("has_one shape: %+v", r2)
	}
}

func TestParse_Indexes(t *testing.T) {
	f := mustParse(t, `
entity P in x {
  id        bigint primary
  consumer  bigint
  created   timestamptz
  vec       vector(32)
  meta      jsonb

  index by consumer, created desc
  index hnsw on vec ops cosine
  index gin on meta
  index partial by consumer where deleted_at is null
}
`)
	e := f.Decls[0].(*EntityDecl)
	idxs := []*IndexDecl{}
	for _, m := range e.Members {
		if id, ok := m.(*IndexDecl); ok {
			idxs = append(idxs, id)
		}
	}
	if len(idxs) != 4 {
		t.Fatalf("expected 4 indexes, got %d", len(idxs))
	}
	if idxs[0].Kind != IndexBtree || len(idxs[0].Fields) != 2 ||
		idxs[0].Fields[0].Name != "consumer" ||
		idxs[0].Fields[1].Name != "created" || !idxs[0].Fields[1].Desc {
		t.Errorf("btree index shape: %+v", idxs[0])
	}
	if idxs[1].Kind != IndexHNSW || idxs[1].Field != "vec" || idxs[1].VecOps != VecOpsCosine {
		t.Errorf("hnsw index shape: %+v", idxs[1])
	}
	if idxs[2].Kind != IndexGIN || idxs[2].Field != "meta" {
		t.Errorf("gin index shape: %+v", idxs[2])
	}
	if idxs[3].Kind != IndexPartial || idxs[3].Where == nil ||
		idxs[3].Where.Field != "deleted_at" || !idxs[3].Where.IsNull {
		t.Errorf("partial index shape: %+v %+v", idxs[3], idxs[3].Where)
	}
}

func TestParse_CacheBlock(t *testing.T) {
	f := mustParse(t, `
entity Outfit in consumer {
  id          bigint primary
  consumer_id bigint

  cache {
    read_through ttl=10m tag="consumer:{consumer_id}"
    invalidate_on: write(self), write(OutfitItem where outfit_id = self.id)
    consistency = strict
  }
}
`)
	e := f.Decls[0].(*EntityDecl)
	var cb *CacheBlock
	for _, m := range e.Members {
		if c, ok := m.(*CacheBlock); ok {
			cb = c
		}
	}
	if cb == nil {
		t.Fatal("expected cache block")
	}
	if !cb.HasReadThrough || cb.TTL != "10m" || cb.Tag != "consumer:{consumer_id}" {
		t.Errorf("read_through shape: %+v", cb)
	}
	if len(cb.Invalidate) != 2 {
		t.Fatalf("expected 2 invalidate clauses, got %d", len(cb.Invalidate))
	}
	if !cb.Invalidate[0].Self {
		t.Errorf("first invalidate should be self")
	}
	c2 := cb.Invalidate[1]
	if c2.Target != "OutfitItem" || c2.Where == nil ||
		c2.Where.Field != "outfit_id" || c2.Where.SelfField != "id" {
		t.Errorf("second invalidate shape: %+v %+v", c2, c2.Where)
	}
	if cb.Consistency != ConsistencyStrict {
		t.Errorf("consistency: got %v want strict", cb.Consistency)
	}
}

func TestParse_QueryTimeout(t *testing.T) {
	f := mustParse(t, `
entity Q in x {
  id bigint primary
  query_timeout = 30s
}
`)
	e := f.Decls[0].(*EntityDecl)
	var qt *QueryTimeoutDecl
	for _, m := range e.Members {
		if q, ok := m.(*QueryTimeoutDecl); ok {
			qt = q
		}
	}
	if qt == nil || qt.Duration != "30s" {
		t.Errorf("query_timeout shape: %+v", qt)
	}
}

func TestParse_Hypertable(t *testing.T) {
	f := mustParse(t, `
hypertable Purchase in vendor on purchased_at {
  id           bigint primary
  qty          int not null
  purchased_at timestamptz not null
}
`)
	if len(f.Decls) != 1 {
		t.Fatalf("expected 1 decl, got %d", len(f.Decls))
	}
	h, ok := f.Decls[0].(*HypertableDecl)
	if !ok {
		t.Fatalf("expected HypertableDecl, got %T", f.Decls[0])
	}
	if h.Name != "Purchase" || h.Namespace != "vendor" || h.TimeField != "purchased_at" {
		t.Errorf("hypertable shape: %+v", h)
	}
}

func TestParse_MultipleDecls(t *testing.T) {
	f := mustParse(t, `
entity A in x { id bigint primary }
entity B in y { id bigint primary }
hypertable C in z on t { id bigint primary  t timestamptz }
`)
	if len(f.Decls) != 3 {
		t.Fatalf("expected 3 decls, got %d", len(f.Decls))
	}
}

func TestParse_Error_MissingBrace(t *testing.T) {
	err := mustParseErr(t, `entity A in x { id bigint primary`)
	if !strings.Contains(err.Error(), "expected }") {
		t.Logf("err: %v", err)
	}
}

func TestParse_Error_UnknownTopLevel(t *testing.T) {
	err := mustParseErr(t, `widget Foo in x {}`)
	if !strings.Contains(err.Error(), "expected 'entity', 'hypertable', 'query', or 'procedure'") {
		t.Errorf("error message lacks hint: %v", err)
	}
}

func TestParse_Error_BadType(t *testing.T) {
	err := mustParseErr(t, `entity A in x { id = bigint }`)
	if err == nil {
		t.Fatal("expected error on bad type position")
	}
}

func TestParse_FullExample(t *testing.T) {
	src := `
entity SavedOutfit in consumer {
  id           bigint primary
  consumer_id  bigint references consumer.Account.id on delete cascade
  name         text not null
  created_at   timestamptz default now()

  has_many items: SavedOutfitItem via outfit_id
  index by consumer_id, created_at desc

  cache {
    read_through ttl=10m tag="consumer:{consumer_id}"
    invalidate_on: write(self), write(SavedOutfitItem where outfit_id = self.id)
    consistency = eventual
  }

  query_timeout = 30s
}

entity ProductVariant in vendor {
  id           bigint primary
  product_id   bigint references vendor.Product.id
  sku          text unique
  price_cents  int not null
  item_vec     vector(32)
  search_vec   vector(768)

  index hnsw on search_vec ops cosine
  cache { read_through ttl=1h tag="product:{product_id}" }
}

hypertable Purchase in vendor on purchased_at {
  id           bigint primary
  vendor_id    bigint references vendor.Vendor.id
  variant_id   bigint references vendor.ProductVariant.id
  qty          int not null
  total_cents  int not null
  purchased_at timestamptz not null
}
`
	f := mustParse(t, src)
	if len(f.Decls) != 3 {
		t.Fatalf("expected 3 decls, got %d", len(f.Decls))
	}
}

// ---- Step 7.5: custom queries and procedures ----
//
// These tests pin the parsed shape of the new top-level constructs.
// The grammar's load-bearing piece is raw-SQL capture inside the lexer
// (so SQL chars like `'`, `--`, `*` don't crash the regular scanner),
// so every test exercises a non-trivial SQL body to keep that wire
// honest. Validation rules (identifier resolution, $arg presence, type
// checks) live in IR lowering — covered separately in ir_test.go.

func TestParse_QueryDecl_BasicShape(t *testing.T) {
	src := `
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
	f := mustParse(t, src)
	if len(f.Decls) != 1 {
		t.Fatalf("expected 1 decl, got %d", len(f.Decls))
	}
	q, ok := f.Decls[0].(*QueryDecl)
	if !ok {
		t.Fatalf("expected *QueryDecl, got %T", f.Decls[0])
	}
	if q.Name != "OutfitsForConsumer" {
		t.Errorf("name: got %q", q.Name)
	}
	if q.Target.Name != "SavedOutfit" || q.Target.Namespace != "" {
		t.Errorf("target: got %q/%q want SavedOutfit/<empty>", q.Target.Namespace, q.Target.Name)
	}
	if len(q.Inputs) != 2 {
		t.Fatalf("inputs: got %d, want 2", len(q.Inputs))
	}
	if q.Inputs[1].Name != "limit" || q.Inputs[1].Default == nil {
		t.Errorf("limit input missing default")
	}
	if q.Output == nil || q.Output.AsEntity == nil {
		t.Fatal("output missing or not 'as' form")
	}
	if q.Output.AsEntity.Name != "SavedOutfit" {
		t.Errorf("output.AsEntity: got %q", q.Output.AsEntity.Name)
	}
	if q.SQL == nil {
		t.Fatal("SQL block missing")
	}
	if len(q.SQL.Touches) != 1 || q.SQL.Touches[0].Name != "SavedOutfit" {
		t.Errorf("touches: got %+v", q.SQL.Touches)
	}
	// Raw SQL body survives whitespace + the $arg references unmolested.
	if !strings.Contains(q.SQL.Raw, "SELECT *") {
		t.Errorf("SQL body missing SELECT *; got %q", q.SQL.Raw)
	}
	if !strings.Contains(q.SQL.Raw, "$consumer_id") {
		t.Errorf("SQL body missing $consumer_id; got %q", q.SQL.Raw)
	}
}

// TestParse_QueryDecl_RawSQLPreservesSpecialChars confirms the lexer's
// raw-mode capture leaves the body byte-identical. The body contains
// SQL single-quoted strings (with embedded escape `”`), SQL line
// comments (`-- comment`), and a `*` — chars the regular DSL lexer
// would otherwise choke on. This is the load-bearing invariant that
// lets pg_query_go see the source as PG itself would.
func TestParse_QueryDecl_RawSQLPreservesSpecialChars(t *testing.T) {
	src := `
query VendorByName for Vendor {
  input { name: text }
  output as Vendor
  sql touches(Vendor) {
    -- find a vendor whose name matches (case-insensitive, escapes ' as '')
    SELECT * FROM vendor_vendor
    WHERE name ILIKE $name || '%'
    AND deleted_at IS NULL /* soft-delete guard */
  }
}
`
	f := mustParse(t, src)
	q := f.Decls[0].(*QueryDecl)
	for _, want := range []string{
		"SELECT *",
		"-- find a vendor",
		"ILIKE $name || '%'",
		"/* soft-delete guard */",
	} {
		if !strings.Contains(q.SQL.Raw, want) {
			t.Errorf("raw SQL missing %q; got:\n%s", want, q.SQL.Raw)
		}
	}
}

func TestParse_ProcedureDecl_TypedSteps(t *testing.T) {
	src := `
procedure DeleteConsumerCascade for Account {
  input { account_id: bigint }

  steps {
    update SavedOutfit set deleted_at = now() where consumer_id = $account_id
    update Cart set deleted_at = now() where consumer_id = $account_id
    delete Session where consumer_id = $account_id
  }

  invalidate: tag("consumer:{account_id}")
}
`
	f := mustParse(t, src)
	pd, ok := f.Decls[0].(*ProcedureDecl)
	if !ok {
		t.Fatalf("expected *ProcedureDecl, got %T", f.Decls[0])
	}
	if pd.Name != "DeleteConsumerCascade" {
		t.Errorf("name: %q", pd.Name)
	}
	if len(pd.Steps) != 3 {
		t.Fatalf("steps: got %d, want 3", len(pd.Steps))
	}
	step0 := pd.Steps[0].Typed
	if step0 == nil || step0.Verb != "update" || step0.Target.Name != "SavedOutfit" {
		t.Errorf("step 0: %+v", step0)
	}
	if len(step0.Assigns) != 1 || step0.Assigns[0].Field != "deleted_at" {
		t.Errorf("step 0 assign: %+v", step0.Assigns)
	}
	step2 := pd.Steps[2].Typed
	if step2 == nil || step2.Verb != "delete" || step2.Target.Name != "Session" {
		t.Errorf("step 2: %+v", step2)
	}
	if pd.Invalidate == nil || pd.Invalidate.TagTpl != "consumer:{account_id}" {
		t.Errorf("invalidate clause: %+v", pd.Invalidate)
	}
}

// TestParse_ProcedureDecl_RawSQLStep covers the raw-SQL escape inside a
// procedure body: when typed CRUD can't express what's needed (e.g.
// subquery-driven cascade), the engineer drops to `sql touches(...)
// { ... }` inside steps{}. The touches() list is what tells the cache
// layer which generation counters to bump.
func TestParse_ProcedureDecl_RawSQLStep(t *testing.T) {
	src := `
procedure CascadeDeleteOutfitItems for SavedOutfit {
  input { consumer_id: bigint }

  steps {
    sql touches(SavedOutfitItem) {
      DELETE FROM consumer_saved_outfit_item
      WHERE outfit_id IN (
        SELECT id FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
      )
    }
  }
}
`
	f := mustParse(t, src)
	pd := f.Decls[0].(*ProcedureDecl)
	if len(pd.Steps) != 1 || pd.Steps[0].Raw == nil {
		t.Fatalf("expected one raw step; got %+v", pd.Steps)
	}
	raw := pd.Steps[0].Raw
	if len(raw.Touches) != 1 || raw.Touches[0].Name != "SavedOutfitItem" {
		t.Errorf("touches: %+v", raw.Touches)
	}
	if !strings.Contains(raw.Raw, "DELETE FROM consumer_saved_outfit_item") {
		t.Errorf("raw body: %q", raw.Raw)
	}
}

func TestParse_QueryDecl_ColumnOutput(t *testing.T) {
	src := `
query VendorRevenue for Vendor {
  input { vendor_id: bigint, since: timestamptz }
  output { month: timestamptz, revenue_cents: bigint }
  sql touches(Purchase) {
    SELECT date_trunc('month', purchased_at), SUM(amount_cents)::bigint
    FROM vendor_purchase
    WHERE vendor_id = $vendor_id AND purchased_at >= $since
    GROUP BY 1
  }
}
`
	f := mustParse(t, src)
	q := f.Decls[0].(*QueryDecl)
	if q.Output == nil || q.Output.AsEntity != nil {
		t.Fatal("output should be explicit columns, not 'as'")
	}
	if len(q.Output.Columns) != 2 {
		t.Fatalf("columns: got %d, want 2", len(q.Output.Columns))
	}
	if q.Output.Columns[0].Name != "month" || q.Output.Columns[0].Type.Name != "timestamptz" {
		t.Errorf("col 0: %+v", q.Output.Columns[0])
	}
}

func TestParse_QueryDecl_MultipleTouches(t *testing.T) {
	src := `
query DashboardFeed for Account {
  input { consumer_id: bigint }
  output as Account
  sql touches(Account, SavedOutfit, Cart) {
    SELECT * FROM consumer_account WHERE id = $consumer_id
  }
}
`
	f := mustParse(t, src)
	q := f.Decls[0].(*QueryDecl)
	if len(q.SQL.Touches) != 3 {
		t.Fatalf("touches: got %d, want 3", len(q.SQL.Touches))
	}
}

func TestParse_QueryDecl_NamespaceQualified(t *testing.T) {
	src := `
query CrossNS for Cart {
  input { id: bigint }
  output as Cart
  sql touches(vendor.Cart, consumer.Cart) {
    SELECT 1
  }
}
`
	f := mustParse(t, src)
	q := f.Decls[0].(*QueryDecl)
	if q.SQL.Touches[0].Namespace != "vendor" || q.SQL.Touches[0].Name != "Cart" {
		t.Errorf("touches[0]: %+v", q.SQL.Touches[0])
	}
	if q.SQL.Touches[1].Namespace != "consumer" {
		t.Errorf("touches[1]: %+v", q.SQL.Touches[1])
	}
}

func TestParse_QueryDecl_MissingFor(t *testing.T) {
	// `for` is required so every custom query has a primary entity for
	// the cache key namespace and the IR lowering pass's
	// scope-resolution.
	_ = mustParseErr(t, `query Foo SavedOutfit { input {} output as SavedOutfit  sql touches(SavedOutfit) { SELECT 1 } }`)
}

func TestParse_QueryDecl_UnterminatedSQL(t *testing.T) {
	// EOF inside a raw SQL block surfaces as a TokError pointing at the
	// opening `{`. The parser produces a non-nil File with the partial
	// AST and an error.
	_ = mustParseErr(t, `query Foo for X { input {} output as X  sql touches(X) { SELECT 1`)
}
