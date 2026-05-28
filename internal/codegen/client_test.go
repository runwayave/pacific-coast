package codegen

import (
	"strings"
	"testing"
)

func TestEmitGoClient_ProducesPerEntityFilesPerNamespace(t *testing.T) {
	// Per-namespace package layout. Two entities in the same namespace →
	// two files in clients/go/client/<ns>/. No top-level aggregator anymore —
	// callers reach for `consumer.NewAccountClient(conn)` directly.
	ir := lower(t, `
entity Account in consumer { id bigint primary  email text not null }
entity Outfit  in consumer { id bigint primary  name  text not null }
`)
	files, err := EmitGoClient(ir, GenConfig{})
	if err != nil {
		t.Fatalf("EmitGoClient: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("file count: got %d want 2 (%v)", len(files), paths(files))
	}
	want := map[string]bool{
		"clients/go/client/consumer/account_client.go": false,
		"clients/go/client/consumer/outfit_client.go":  false,
	}
	for _, p := range paths(files) {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected path: %s", p)
		}
		want[p] = true
	}
	for p, ok := range want {
		if !ok {
			t.Errorf("missing expected path: %s", p)
		}
	}
}

func TestEmitGoClient_PackageHeader(t *testing.T) {
	ir := lower(t, `entity Account in consumer { id bigint primary }`)
	files, _ := EmitGoClient(ir, GenConfig{})
	c := files[0].Content
	// `package consumer`, mirroring gen/go/server/consumer/.
	assertContains(t, c, "package consumer")
	assertNotContains(t, c, "package client")
}

func TestEmitGoClient_ParsesAsGo(t *testing.T) {
	// Mixed schema covering single-PK, hypertable, HNSW, composite-PK
	// stub. Every emitted file must parse as valid Go.
	ir := lower(t, `
entity Account in consumer { id bigint primary  email text not null  age int }
hypertable Purchase in vendor on purchased_at {
  id           bigint primary
  qty          int not null
  purchased_at timestamptz not null
}
entity ProductVariant in vendor {
  id         bigint primary
  search_vec vector(768)
  index hnsw on search_vec ops cosine
}
entity CartItem in consumer {
  cart_id    bigint not null
  variant_id bigint not null
  primary by cart_id, variant_id
}
`)
	files, err := EmitGoClient(ir, GenConfig{})
	if err != nil {
		t.Fatalf("EmitGoClient: %v", err)
	}
	for _, f := range files {
		parseAsGo(t, f.Content)
	}
}

func TestEmitGoClient_NoValueStructEmission(t *testing.T) {
	// Phase D drops the hand-rolled `<Entity>` value struct. The proto
	// message is the canonical value type. A stray `type Account struct`
	// in the emitted client file means a partial revert.
	ir := lower(t, `entity Account in consumer { id bigint primary  email text not null }`)
	files, _ := EmitGoClient(ir, GenConfig{})
	c := files[0].Content
	assertNotContains(t, c, "type Account struct")
}

func TestEmitGoClient_InterfaceUsesProtoTypes(t *testing.T) {
	// Every method takes/returns proto types directly — matching the
	// buf-generated <Entity>ServiceClient surface. The wrapper exists
	// purely so callers depend on one stable interface that we can
	// extend with retries / metrics later without touching buf output.
	ir := lower(t, `entity A in x { id bigint primary  v text }`)
	files, _ := EmitGoClient(ir, GenConfig{})
	c := findFile(t, files, "clients/go/client/x/a_client.go")
	for _, sig := range []string{
		"type AClient interface {",
		"GetA(ctx context.Context, req *pb.GetARequest, opts ...grpc.CallOption) (*pb.GetAResponse, error)",
		"ListA(ctx context.Context, req *pb.ListARequest, opts ...grpc.CallOption) (*pb.ListAResponse, error)",
		"BatchGetA(ctx context.Context, req *pb.BatchGetARequest, opts ...grpc.CallOption) (*pb.BatchGetAResponse, error)",
		"CreateA(ctx context.Context, req *pb.CreateARequest, opts ...grpc.CallOption) (*pb.CreateAResponse, error)",
		"UpdateA(ctx context.Context, req *pb.UpdateARequest, opts ...grpc.CallOption) (*pb.UpdateAResponse, error)",
		"DeleteA(ctx context.Context, req *pb.DeleteARequest, opts ...grpc.CallOption) (*pb.DeleteAResponse, error)",
	} {
		assertContains(t, c, sig)
	}
}

func TestEmitGoClient_ConcreteDialsBufStub(t *testing.T) {
	// The concrete struct delegates to the buf-generated service client.
	// One-line bodies; no business logic. If retries / metrics show up
	// here later, that's the right place — buf-generated stubs stay
	// untouched.
	ir := lower(t, `entity Account in consumer { id bigint primary  email text not null }`)
	files, _ := EmitGoClient(ir, GenConfig{})
	c := findFile(t, files, "clients/go/client/consumer/account_client.go")
	assertContains(t, c, "inner pb.AccountServiceClient")
	assertContains(t, c, "func NewAccountClient(cc grpc.ClientConnInterface) AccountClient")
	assertContains(t, c, "inner: pb.NewAccountServiceClient(cc)")
	// One representative method body.
	assertContains(t, c, "return c.inner.GetAccount(ctx, req, opts...)")
}

func TestEmitGoClient_NoErrNotWiredSentinel(t *testing.T) {
	// Pre-Phase-D client emitted `errNotWired` placeholders. Phase D
	// dials the real buf stub; the sentinel is dead code.
	ir := lower(t, `entity A in x { id bigint primary }`)
	files, _ := EmitGoClient(ir, GenConfig{})
	assertNotContains(t, files[0].Content, "errNotWired")
}

func TestEmitGoClient_VectorSearchMethod(t *testing.T) {
	ir := lower(t, `
entity ProductVariant in vendor {
  id         bigint primary
  search_vec vector(768)
  index hnsw on search_vec ops cosine
}
`)
	files, _ := EmitGoClient(ir, GenConfig{})
	c := findFile(t, files, "clients/go/client/vendorpkg/product_variant_client.go")
	// Search method takes the proto request, returns proto response —
	// matches the buf-generated stub's signature.
	assertContains(t, c, "SearchProductVariantBySearchVec(ctx context.Context, req *pb.SearchProductVariantBySearchVecRequest, opts ...grpc.CallOption) (*pb.SearchProductVariantBySearchVecResponse, error)")
}

func TestEmitGoClient_NoSearchWithoutHNSW(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary  v vector(8) }`)
	files, _ := EmitGoClient(ir, GenConfig{})
	c := findFile(t, files, "clients/go/client/x/a_client.go")
	assertNotContains(t, c, "SearchBy")
}

// TestEmitGoClient_CompositePK_EmitsRealClient replaces the earlier
// stub assertion. With the proto declaring a full service block for
// composite-PK entities, the client wrapper dials it just like any
// single-PK service — the wrapper is PK-arity-agnostic because the PK
// columns live inside the request messages buf generated from the proto.
func TestEmitGoClient_CompositePK_EmitsRealClient(t *testing.T) {
	ir := lower(t, `
entity CartItem in consumer {
  cart_id    bigint not null
  variant_id bigint not null
  primary by cart_id, variant_id
}
`)
	files, _ := EmitGoClient(ir, GenConfig{})
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	c := files[0].Content
	parseAsGo(t, c)
	assertContains(t, c, "package consumer")
	assertNotContains(t, c, "deferred to v0.2")
	assertContains(t, c, "type CartItemClient interface")
	assertContains(t, c, "QueryCartItem(ctx context.Context, req *pb.QueryCartItemRequest")
	assertContains(t, c, "func NewCartItemClient(cc grpc.ClientConnInterface)")
	if files[0].Path != "clients/go/client/consumer/cart_item_client.go" {
		t.Errorf("path: %s", files[0].Path)
	}
}

func TestEmitGoClient_DeterministicAcrossRuns(t *testing.T) {
	src := `
entity A in x { id bigint primary  v text  vec vector(16)  index hnsw on vec ops cosine }
entity B in y { id bigint primary  ref bigint references x.A.id }
`
	ir1 := lower(t, src)
	files1, _ := EmitGoClient(ir1, GenConfig{})

	ir2 := lower(t, src)
	files2, _ := EmitGoClient(ir2, GenConfig{})

	if len(files1) != len(files2) {
		t.Fatalf("file count mismatch")
	}
	for i := range files1 {
		if files1[i].Path != files2[i].Path {
			t.Errorf("path drift at %d", i)
		}
		if files1[i].Content != files2[i].Content {
			t.Errorf("content drift at %d", i)
		}
	}
}

func TestEmitGoClient_DoNotEditBanner(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary }`)
	files, _ := EmitGoClient(ir, GenConfig{})
	for _, f := range files {
		if !strings.Contains(f.Content, "DO NOT EDIT") {
			t.Errorf("missing DO NOT EDIT in %s", f.Path)
		}
	}
}

// findFile returns the Content of the file matching p, or fails the test.
func findFile(t *testing.T, files []GoFile, p string) string {
	t.Helper()
	for _, f := range files {
		if f.Path == p {
			return f.Content
		}
	}
	t.Fatalf("file %q not in output (%v)", p, paths(files))
	return ""
}

func paths(files []GoFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// ----------------------------------------------------------------------------
// Phase E — Go client emission for QueryX

func TestEmitGoClient_QueryMethodEmitted(t *testing.T) {
	ir := lower(t, `entity Account in consumer { id bigint primary email text not null }`)
	files, _ := EmitGoClient(ir, GenConfig{})
	c := findFile(t, files, "clients/go/client/consumer/account_client.go")
	// Interface signature.
	assertContains(t, c, "QueryAccount(ctx context.Context, req *pb.QueryAccountRequest, opts ...grpc.CallOption) (*pb.QueryAccountResponse, error)")
	// Concrete delegate body.
	assertContains(t, c, "func (c *accountClient) QueryAccount(ctx context.Context, req *pb.QueryAccountRequest, opts ...grpc.CallOption) (*pb.QueryAccountResponse, error)")
	assertContains(t, c, "return c.inner.QueryAccount(ctx, req, opts...)")
}
