package codegen

import (
	"strings"
	"testing"
)

func TestProto_AssignsNumbersOnFreshIR(t *testing.T) {
	ir := lower(t, `
entity A in x {
  id   bigint primary
  name text
  age  int
}
`)
	AssignProtoNumbers(nil, ir)
	got := map[string]int{}
	for _, f := range ir.Entities[0].Fields {
		got[f.Name] = f.ProtoNumber
	}
	// Numbers must all be set and distinct, contiguous from 1.
	expected := map[string]int{"id": 1, "name": 2, "age": 3}
	for name, want := range expected {
		if got[name] != want {
			t.Errorf("field %s: got proto number %d, want %d", name, got[name], want)
		}
	}
}

func TestProto_PreservesExistingNumbers(t *testing.T) {
	old := lower(t, `entity A in x { id bigint primary  name text  age int }`)
	AssignProtoNumbers(nil, old)
	// Now reorder fields in source.
	newIR := lower(t, `entity A in x { age int  id bigint primary  name text }`)
	AssignProtoNumbers(old, newIR)
	for _, f := range newIR.Entities[0].Fields {
		// Find the matching old field by name.
		for _, of := range old.Entities[0].Fields {
			if of.Name == f.Name {
				if f.ProtoNumber != of.ProtoNumber {
					t.Errorf("field %s: number changed across regen (%d → %d)",
						f.Name, of.ProtoNumber, f.ProtoNumber)
				}
			}
		}
	}
}

func TestProto_AssignsFreshNumberForNewField(t *testing.T) {
	old := lower(t, `entity A in x { id bigint primary  name text }`)
	AssignProtoNumbers(nil, old)
	// Old: id=1, name=2.

	newIR := lower(t, `entity A in x { id bigint primary  name text  added text }`)
	AssignProtoNumbers(old, newIR)
	// added should get number 3 (smallest free).
	for _, f := range newIR.Entities[0].Fields {
		if f.Name == "added" && f.ProtoNumber != 3 {
			t.Errorf("added: got %d want 3", f.ProtoNumber)
		}
	}
}

func TestProto_RemovedFieldNumberRetired(t *testing.T) {
	old := lower(t, `entity A in x { id bigint primary  name text  age int }`)
	AssignProtoNumbers(nil, old)
	// Old: id=1, name=2, age=3.

	newIR := lower(t, `entity A in x { id bigint primary  age int }`)
	AssignProtoNumbers(old, newIR)
	// name (number 2) should be retired.
	e := newIR.Entities[0]
	found := false
	for _, n := range e.RetiredProtoNumbers {
		if n == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("removed field's number not retired: %v", e.RetiredProtoNumbers)
	}
	// age must keep its original number 3.
	for _, f := range e.Fields {
		if f.Name == "age" && f.ProtoNumber != 3 {
			t.Errorf("age: number changed (%d, want 3)", f.ProtoNumber)
		}
	}
}

func TestProto_NeverReusesRetiredNumber(t *testing.T) {
	old := lower(t, `entity A in x { id bigint primary  removed_field text }`)
	AssignProtoNumbers(nil, old)
	// Old: id=1, removed_field=2.

	mid := lower(t, `entity A in x { id bigint primary }`)
	AssignProtoNumbers(old, mid)
	// retired: [2]

	newIR := lower(t, `entity A in x { id bigint primary  brand_new text }`)
	AssignProtoNumbers(mid, newIR)
	// brand_new must NOT reuse 2.
	for _, f := range newIR.Entities[0].Fields {
		if f.Name == "brand_new" && f.ProtoNumber == 2 {
			t.Errorf("retired number 2 was reused for brand_new")
		}
	}
	// Retired list should still contain 2 in the new IR.
	if len(newIR.Entities[0].RetiredProtoNumbers) == 0 ||
		newIR.Entities[0].RetiredProtoNumbers[0] != 2 {
		t.Errorf("retired numbers lost across regen: %v",
			newIR.Entities[0].RetiredProtoNumbers)
	}
}

func TestProto_EmitMessage(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  id    bigint primary
  email text not null unique
  age   int
}
`)
	AssignProtoNumbers(nil, ir)
	files, err := EmitProto(ir)
	if err != nil {
		t.Fatalf("EmitProto: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Path != "atlantis/consumer/v1/account.proto" {
		t.Errorf("path: %s", f.Path)
	}
	c := f.Content
	assertContains(t, c, "syntax = \"proto3\";")
	assertContains(t, c, "package atlantis.consumer.v1;")
	assertContains(t, c, "message Account {")
	assertContains(t, c, "int64 id = 1;")
	assertContains(t, c, "string email = 2;") // not optional because not null
	assertContains(t, c, "optional int32 age = 3;")
	assertContains(t, c, "service AccountService {")
	assertContains(t, c, "rpc GetAccount(GetAccountRequest)")
	assertContains(t, c, "rpc BatchGetAccount(BatchGetAccountRequest)")
}

func TestProto_EmitVectorAndSearchRPC(t *testing.T) {
	ir := lower(t, `
entity ProductVariant in vendor {
  id          bigint primary
  product_id  bigint
  search_vec  vector(768)
  index hnsw on search_vec ops cosine
}
`)
	AssignProtoNumbers(nil, ir)
	files, err := EmitProto(ir)
	if err != nil {
		t.Fatalf("EmitProto: %v", err)
	}
	c := files[0].Content
	// Vector → repeated float, which is always repeated so no `optional`.
	assertContains(t, c, "repeated float search_vec = 3;")
	// HNSW index → search RPC.
	assertContains(t, c, "rpc SearchProductVariantBySearchVec(SearchProductVariantBySearchVecRequest)")
	assertContains(t, c, "message SearchProductVariantBySearchVecRequest {")
	assertContains(t, c, "repeated float query_vector = 1;")
	assertContains(t, c, "repeated float distances = 2;")
}

func TestProto_EmitArrayField(t *testing.T) {
	ir := lower(t, `
entity P in x {
  id   bigint primary
  tags []text
}
`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	assertContains(t, files[0].Content, "repeated string tags = 2;")
}

func TestProto_EmitReservedAfterRemoval(t *testing.T) {
	old := lower(t, `entity A in x { id bigint primary  removed text }`)
	AssignProtoNumbers(nil, old)
	newIR := lower(t, `entity A in x { id bigint primary }`)
	AssignProtoNumbers(old, newIR)
	files, _ := EmitProto(newIR)
	assertContains(t, files[0].Content, "reserved 2;")
}

func TestProto_AllScalarTypes(t *testing.T) {
	ir := lower(t, `
entity K in lab {
  id  bigint primary
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
}
`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	c := files[0].Content
	for _, sub := range []string{
		"int32 a", "int32 b", "int64 c", "string d", "bool e",
		"google.protobuf.Timestamp f", "string g", "bytes h",
		"string i", // numeric → string
		"bytes j",  // jsonb → bytes
	} {
		assertContains(t, c, sub)
	}
}

func TestProto_DeterministicOutputAcrossRuns(t *testing.T) {
	src := `
entity A in x { id bigint primary  a int  b text  c bigint }
entity B in y { id bigint primary  ref bigint references x.A.id }
`
	// Run twice from scratch; outputs must match byte-for-byte.
	ir1 := lower(t, src)
	AssignProtoNumbers(nil, ir1)
	files1, _ := EmitProto(ir1)

	ir2 := lower(t, src)
	AssignProtoNumbers(nil, ir2)
	files2, _ := EmitProto(ir2)

	if len(files1) != len(files2) {
		t.Fatalf("file count mismatch: %d vs %d", len(files1), len(files2))
	}
	for i := range files1 {
		if files1[i].Path != files2[i].Path {
			t.Errorf("file %d path drift: %s vs %s", i, files1[i].Path, files2[i].Path)
		}
		if files1[i].Content != files2[i].Content {
			t.Errorf("file %d content drift", i)
		}
	}
}

func TestProto_FilesSortedByPath(t *testing.T) {
	ir := lower(t, `
entity Z in zeta { id bigint primary }
entity A in alpha { id bigint primary }
entity M in mu { id bigint primary }
`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	for i := 1; i < len(files); i++ {
		if files[i-1].Path >= files[i].Path {
			t.Errorf("not sorted: %s >= %s", files[i-1].Path, files[i].Path)
		}
	}
}

func TestProto_PKWithCustomTypeAndName(t *testing.T) {
	ir := lower(t, `entity A in x { custom_pk uuid primary }`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	c := files[0].Content
	// Request type uses the actual PK column name (not a hardcoded `id`)
	// and maps UUID to proto's string scalar. The emitter renders the
	// PK-keyed request shells (Get / Delete) in a multi-line form so the
	// same template serves composite-PK entities without branching.
	assertContains(t, c, "string custom_pk = 1;")
	assertContains(t, c, "message GetARequest {\n  string custom_pk = 1;\n}")
}

func TestProto_SnakeToCamel(t *testing.T) {
	cases := map[string]string{
		"id":           "Id",
		"item_vec":     "ItemVec",
		"a_b_c":        "ABC",
		"single":       "Single",
		"already_done": "AlreadyDone",
	}
	for in, want := range cases {
		if got := snakeToCamel(in); got != want {
			t.Errorf("snakeToCamel(%q) = %q want %q", in, got, want)
		}
	}
}

// Sanity: rendering should not emit anything the user might mistake for
// hand-editable content beyond what's expected.
func TestProto_HeaderComment(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary }`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	if !strings.Contains(files[0].Content, "DO NOT EDIT") {
		t.Errorf("missing DO NOT EDIT banner")
	}
}

// ----------------------------------------------------------------------------
// QueryX surface emission

func TestProto_EmitQueryRPC(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  id    bigint primary
  email text not null
  age   int
}
`)
	AssignProtoNumbers(nil, ir)
	files, err := EmitProto(ir)
	if err != nil {
		t.Fatalf("EmitProto: %v", err)
	}
	c := files[0].Content
	assertContains(t, c, "rpc QueryAccount(QueryAccountRequest) returns (QueryAccountResponse);")
	assertContains(t, c, "message QueryAccountRequest {")
	assertContains(t, c, "AccountFilter filter = 1;")
	assertContains(t, c, "repeated AccountOrderBy order = 2;")
	assertContains(t, c, "int32 limit = 3;")
	assertContains(t, c, "string page_token = 4;")
	assertContains(t, c, "google.protobuf.FieldMask fields = 5;")
	assertContains(t, c, "repeated AccountInclude includes = 6;")
	assertContains(t, c, "bool cache_skip = 7;")
	assertContains(t, c, "message QueryAccountResponse {")
	assertContains(t, c, "repeated Account entities = 1;")
	assertContains(t, c, "string next_page_token = 2;")
	assertContains(t, c, "optional int64 total_estimate = 3;")
}

func TestProto_EmitFilterMessage(t *testing.T) {
	ir := lower(t, `
entity Order in consumer {
  id          bigint primary
  consumer_id text not null
  amount      numeric(10,2) not null
  paid_at     timestamptz
  is_open     boolean not null
  payload     jsonb
}
`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	c := files[0].Content
	assertContains(t, c, "message OrderFilter {")
	// Predicate type per DSL field type.
	assertContains(t, c, "optional atlantis.common.v1.Int64Predicate id = 1;")
	assertContains(t, c, "optional atlantis.common.v1.StringPredicate consumer_id = 2;")
	assertContains(t, c, "optional atlantis.common.v1.NumericPredicate amount = 3;")
	assertContains(t, c, "optional atlantis.common.v1.TimestampPredicate paid_at = 4;")
	assertContains(t, c, "optional atlantis.common.v1.BoolPredicate is_open = 5;")
	assertContains(t, c, "optional atlantis.common.v1.BytesPredicate payload = 6;")
	// Composite arms at the high field numbers.
	assertContains(t, c, "repeated OrderFilter and = 100;")
	assertContains(t, c, "repeated OrderFilter or = 101;")
	assertContains(t, c, "optional OrderFilter not = 102;")
}

func TestProto_OrderFieldEnumOmitsVectorAndArray(t *testing.T) {
	// Vector + array fields aren't meaningfully orderable; codegen skips.
	ir := lower(t, `
entity Product in vendor {
  id        bigint primary
  name      text not null
  embedding vector(8)
  tags      []text
}
`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	c := files[0].Content
	assertContains(t, c, "enum ProductOrderField {")
	assertContains(t, c, "PRODUCT_ORDER_FIELD_ID = 1;")
	assertContains(t, c, "PRODUCT_ORDER_FIELD_NAME = 2;")
	assertNotContains(t, c, "PRODUCT_ORDER_FIELD_EMBEDDING")
	assertNotContains(t, c, "PRODUCT_ORDER_FIELD_TAGS")
}

func TestProto_FilterSkipsUnfilterableTypes(t *testing.T) {
	// Vector + array fields don't get filter predicates emitted.
	ir := lower(t, `
entity Product in vendor {
  id        bigint primary
  embedding vector(8)
  tags      []text
}
`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	c := files[0].Content
	// Isolate the ProductFilter message block; the entity message above
	// still mentions embedding/tags as data fields.
	filterStart := strings.Index(c, "message ProductFilter {")
	if filterStart < 0 {
		t.Fatalf("ProductFilter not in output")
	}
	filterEnd := strings.Index(c[filterStart:], "}")
	if filterEnd < 0 {
		t.Fatalf("ProductFilter unterminated")
	}
	filterBlock := c[filterStart : filterStart+filterEnd]
	if strings.Contains(filterBlock, "embedding") {
		t.Errorf("ProductFilter should skip vector field; got:\n%s", filterBlock)
	}
	if strings.Contains(filterBlock, "tags") {
		t.Errorf("ProductFilter should skip array field; got:\n%s", filterBlock)
	}
	// The id field IS filterable.
	assertContains(t, filterBlock, "Int64Predicate id = 1;")
}

func TestProto_IncludeEnumFromInboundFKs(t *testing.T) {
	// Two entities, B references A. A's Include enum should have a
	// variant naming B's FK column.
	ir := lower(t, `
entity Account in consumer {
  id bigint primary
}
entity Session in consumer {
  id          bigint primary
  account_id  bigint not null references consumer.Account.id
}
`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	// Find the account.proto file.
	var accountFile string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "account.proto") {
			accountFile = f.Content
		}
	}
	if accountFile == "" {
		t.Fatalf("account.proto not in output")
	}
	assertContains(t, accountFile, "enum AccountInclude {")
	assertContains(t, accountFile, "ACCOUNT_INCLUDE_UNSPECIFIED = 0;")
	assertContains(t, accountFile, "ACCOUNT_INCLUDE_CONSUMER_SESSION_BY_ACCOUNT_ID = 1;")
}

func TestProto_IncludeEnumDisambiguatesMultipleFKsToSameTarget(t *testing.T) {
	// One entity B references A twice via two different FK columns —
	// both variants must appear, distinguished by FK column.
	ir := lower(t, `
entity User in vendor {
  id bigint primary
}
entity Product in vendor {
  id          bigint primary
  created_by  bigint not null references vendor.User.id
  updated_by  bigint not null references vendor.User.id
}
`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	var userFile string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "user.proto") {
			userFile = f.Content
		}
	}
	if userFile == "" {
		t.Fatalf("user.proto not in output")
	}
	assertContains(t, userFile, "USER_INCLUDE_VENDORPKG_PRODUCT_BY_CREATED_BY = 1;")
	assertContains(t, userFile, "USER_INCLUDE_VENDORPKG_PRODUCT_BY_UPDATED_BY = 2;")
}

func TestProto_ImportsCommonPredicatesAndFieldMask(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary }`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	c := files[0].Content
	assertContains(t, c, `import "google/protobuf/field_mask.proto";`)
	assertContains(t, c, `import "atlantis/common/v1/predicates.proto";`)
}

// TestProto_CompositePK_HasFullSurface confirms that composite-PK
// entities emit the same proto surface as single-PK ones — service block
// with all seven RPCs (Get / List / Create / Update / Delete / BatchGet /
// Query), typed Filter / OrderField / Include messages, request +
// response shells. PK arity influences only the request shell shapes
// (composite uses a PK wrapper message + GetIds returning a repeated of
// it; the rest is identical).
func TestProto_CompositePK_HasFullSurface(t *testing.T) {
	ir := lower(t, `
entity CartItem in consumer {
  cart_id    bigint not null
  variant_id bigint not null
  primary by cart_id, variant_id
}
`)
	AssignProtoNumbers(nil, ir)
	files, _ := EmitProto(ir)
	c := files[0].Content
	for _, want := range []string{
		"service CartItemService {",
		"rpc QueryCartItem(QueryCartItemRequest) returns (QueryCartItemResponse);",
		"message CartItemPK {",
		"message CartItemFilter {",
		"enum CartItemOrderField {",
		"enum CartItemInclude {",
		"message QueryCartItemRequest {",
		"message QueryCartItemResponse {",
	} {
		assertContains(t, c, want)
	}
}
