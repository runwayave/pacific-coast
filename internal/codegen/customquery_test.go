package codegen

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// lowerCustom is the test-only equivalent of the test helpers in
// dsl/ir_test.go — it parses + lowers a fixture and returns the IR
// for the emitters to consume. Keeping it local to this file avoids
// reaching into another package's test-only helpers.
func lowerCustom(t *testing.T, src string) *dsl.IR {
	t.Helper()
	f, err := dsl.Parse("t.pc", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	AssignProtoNumbers(nil, ir)
	return ir
}

// customSchemaFixture is the two-entity schema used as the reference
// when lowering custom queries / procedures. Entities are minimal
// enough that proto numbers stay deterministic across runs.
const customSchemaFixture = `
entity Account in consumer {
  id          bigint primary
  consumer_id text not null
  email       text not null
  deleted_at  timestamptz
}

entity SavedOutfit in consumer {
  id          bigint primary
  consumer_id bigint not null references consumer.Account.id
  name        text not null
  deleted_at  timestamptz
}
`

func findProto(t *testing.T, files []ProtoFile, path string) string {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f.Content
		}
	}
	t.Fatalf("proto file %q not emitted; got %d files", path, len(files))
	return ""
}

func TestEmitCustomProto_Service(t *testing.T) {
	ir := lowerCustom(t, customSchemaFixture+`
query OutfitsForConsumer for SavedOutfit {
  input { consumer_id: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT id, consumer_id, name FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
  }
}

procedure DeleteOutfit for SavedOutfit {
  input { outfit_id: bigint }
  steps {
    update SavedOutfit set deleted_at = now() where id = $outfit_id
  }
}
`)
	files, err := EmitCustomProto(ir)
	if err != nil {
		t.Fatalf("EmitCustomProto: %v", err)
	}
	c := findProto(t, files, "atlantis/consumer/v1/custom.proto")

	for _, want := range []string{
		`package atlantis.consumer.v1;`,
		`service CustomService {`,
		`rpc DeleteOutfit(DeleteOutfitRequest) returns (DeleteOutfitResponse);`,
		`rpc OutfitsForConsumer(OutfitsForConsumerRequest) returns (OutfitsForConsumerResponse);`,
		`message OutfitsForConsumerRequest {`,
		`int64 consumer_id = 1;`,
		`message OutfitsForConsumerResponse {`,
		`repeated SavedOutfit rows = 1;`,
		`message DeleteOutfitResponse {`,
		`int64 rows_affected = 1;`,
		`import "atlantis/consumer/v1/saved_outfit.proto";`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

// TestEmitCustomProto_ColumnOutput pins the synthetic-Row shape for
// queries that return arbitrary projections. The Row message lives
// inside the response so two queries with the same column name can't
// collide on a top-level proto message.
func TestEmitCustomProto_ColumnOutput(t *testing.T) {
	ir := lowerCustom(t, customSchemaFixture+`
query CountOutfits for Account {
  input { consumer_id: bigint }
  output { count: bigint, last_seen: timestamptz }
  sql touches(SavedOutfit) {
    SELECT COUNT(*), MAX(deleted_at) FROM consumer_saved_outfit
    WHERE consumer_id = $consumer_id
  }
}
`)
	files, err := EmitCustomProto(ir)
	if err != nil {
		t.Fatalf("EmitCustomProto: %v", err)
	}
	c := findProto(t, files, "atlantis/consumer/v1/custom.proto")
	for _, want := range []string{
		`message CountOutfitsResponse {`,
		`message Row {`,
		`int64 count = 1;`,
		`google.protobuf.Timestamp last_seen = 2;`,
		`repeated Row rows = 1;`,
		// Timestamp import lights up because the output column needs it.
		`import "google/protobuf/timestamp.proto";`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

func TestEmitCustomProto_NoFilesWhenNoCustom(t *testing.T) {
	// IR has entities but no queries / procedures → no custom.proto.
	ir := lowerCustom(t, customSchemaFixture)
	files, err := EmitCustomProto(ir)
	if err != nil {
		t.Fatalf("EmitCustomProto: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected no custom proto files, got %d", len(files))
	}
}

func TestEmitCustomServer_QueryHandler(t *testing.T) {
	ir := lowerCustom(t, customSchemaFixture+`
query OutfitsForConsumer for SavedOutfit {
  input { consumer_id: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT id, consumer_id, name FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
  }
}
`)
	files, err := EmitCustomServer(ir)
	if err != nil {
		t.Fatalf("EmitCustomServer: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 custom server file, got %d", len(files))
	}
	c := files[0].Content
	parseAsGo(t, c)
	for _, want := range []string{
		`package consumer`,
		`type CustomServer struct {`,
		`func NewCustomServer(db runtime.Pool, outbox runtime.Outbox)`,
		// Const has the DSL placeholder rewritten to PG positional.
		`const sqlCustom_OutfitsForConsumer = `,
		`WHERE consumer_id = $1`,
		// Method signature matches the buf interface.
		`func (s *CustomServer) OutfitsForConsumer(ctx context.Context, req *pb.OutfitsForConsumerRequest) (*pb.OutfitsForConsumerResponse, error)`,
		// Args ordered to match the rewritten placeholder.
		`req.GetConsumerId()`,
		// Scan loop uses the entity's scan helper.
		`scanIntoSavedOutfit(rows, row)`,
		// Interface-satisfaction assertion at file end.
		`var _ pb.CustomServiceServer = (*CustomServer)(nil)`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

func TestEmitCustomServer_ColumnOutputHandler(t *testing.T) {
	ir := lowerCustom(t, customSchemaFixture+`
query CountOutfits for Account {
  input { consumer_id: bigint }
  output { count: bigint }
  sql touches(SavedOutfit) {
    SELECT COUNT(*) FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
  }
}
`)
	files, _ := EmitCustomServer(ir)
	c := files[0].Content
	parseAsGo(t, c)
	for _, want := range []string{
		`row := &pb.CountOutfitsResponse_Row{}`,
		`var v0 int64`,
		`if err := rows.Scan(&v0)`,
		`row.Count = v0`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

func TestEmitCustomServer_ProcedureTypedSteps(t *testing.T) {
	// Typed steps lower to hand-built SQL constants per step, with
	// runtime.* helpers for the value bindings. Outbox bumps run once
	// per touched entity inside the tx.
	ir := lowerCustom(t, customSchemaFixture+`
procedure DeleteConsumerCascade for Account {
  input { account_id: bigint }
  steps {
    update SavedOutfit set deleted_at = now() where consumer_id = $account_id
    delete Account where id = $account_id
  }
}
`)
	files, err := EmitCustomServer(ir)
	if err != nil {
		t.Fatalf("EmitCustomServer: %v", err)
	}
	c := files[0].Content
	parseAsGo(t, c)
	for _, want := range []string{
		// Each typed step's SQL is baked as a local const.
		"const sqlProc_DeleteConsumerCascade_step1 = ",
		`UPDATE "atlantis"."consumer_saved_outfit" SET "deleted_at" = now() WHERE "consumer_id" = $1`,
		"const sqlProc_DeleteConsumerCascade_step2 = ",
		`DELETE FROM "atlantis"."consumer_account" WHERE "id" = $1`,
		// Both touched entities get one generation bump, sorted
		// alphabetically by canonical id.
		`s.Outbox.EnqueueGenerationBump(ctx, tx, "consumer.Account")`,
		`s.Outbox.EnqueueGenerationBump(ctx, tx, "consumer.SavedOutfit")`,
		// rowsAffected accumulates across steps.
		`rowsAffected += tag0.RowsAffected()`,
		`rowsAffected += tag1.RowsAffected()`,
		`tx.Commit(ctx)`,
		// Response carries the running total.
		`return &pb.DeleteConsumerCascadeResponse{RowsAffected: rowsAffected}`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

func TestEmitCustomServer_ProcedureRawStep(t *testing.T) {
	ir := lowerCustom(t, customSchemaFixture+`
procedure RawCascade for SavedOutfit {
  input { consumer_id: bigint }
  steps {
    sql touches(SavedOutfit) {
      DELETE FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
    }
  }
}
`)
	files, _ := EmitCustomServer(ir)
	c := files[0].Content
	parseAsGo(t, c)
	for _, want := range []string{
		// Raw step's SQL has named placeholders rewritten.
		`DELETE FROM consumer_saved_outfit WHERE consumer_id = $1`,
		`tx.Exec(ctx, sqlProcRaw_step1, req.GetConsumerId())`,
		`s.Outbox.EnqueueGenerationBump(ctx, tx, "consumer.SavedOutfit")`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

func TestEmitCustomClient_Shape(t *testing.T) {
	ir := lowerCustom(t, customSchemaFixture+`
query OutfitsForConsumer for SavedOutfit {
  input { consumer_id: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT id FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
  }
}

procedure DeleteOutfit for SavedOutfit {
  input { outfit_id: bigint }
  steps { delete SavedOutfit where id = $outfit_id }
}
`)
	files, err := EmitCustomClient(ir, GenConfig{})
	if err != nil {
		t.Fatalf("EmitCustomClient: %v", err)
	}
	c := files[0].Content
	parseAsGo(t, c)
	for _, want := range []string{
		`type CustomClient interface {`,
		`OutfitsForConsumer(ctx context.Context, req *pb.OutfitsForConsumerRequest, opts ...grpc.CallOption) (*pb.OutfitsForConsumerResponse, error)`,
		`DeleteOutfit(ctx context.Context, req *pb.DeleteOutfitRequest, opts ...grpc.CallOption) (*pb.DeleteOutfitResponse, error)`,
		`func NewCustomClient(cc grpc.ClientConnInterface) CustomClient`,
		`pb.NewCustomServiceClient(cc)`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

func TestEmitCustomServer_RegisterAggregator(t *testing.T) {
	// register.go must mount the per-namespace CustomService when at
	// least one query / procedure exists in that namespace.
	ir := lowerCustom(t, customSchemaFixture+`
query Sample for Account {
  input { x: bigint }
  output as Account
  sql touches(Account) { SELECT id FROM consumer_account WHERE id = $x }
}
`)
	files, _ := EmitGoServer(ir)
	var reg string
	for _, f := range files {
		if f.Path == "gen/go/server/register.go" {
			reg = f.Content
		}
	}
	if reg == "" {
		t.Fatal("register.go not emitted")
	}
	for _, want := range []string{
		`pbconsumer.RegisterCustomServiceServer(srv, consumer.NewCustomServer(deps.Pool, deps.Outbox))`,
	} {
		if !strings.Contains(reg, want) {
			t.Errorf("register.go missing %q in:\n%s", want, reg)
		}
	}
}

func TestNormalizeSQLParams_RewritesAndOrders(t *testing.T) {
	inputs := []dsl.QueryParam{
		{Name: "consumer_id", Type: dsl.FieldType{Name: "bigint"}},
		{Name: "limit", Type: dsl.FieldType{Name: "int"}},
	}
	sql := "SELECT * FROM t WHERE c = $consumer_id LIMIT $limit"
	out, order, err := normalizeSQLParams(sql, inputs)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !strings.Contains(out, "WHERE c = $1") {
		t.Errorf("expected $1 substitution, got %q", out)
	}
	if !strings.Contains(out, "LIMIT $2") {
		t.Errorf("expected $2 substitution, got %q", out)
	}
	if len(order) != 2 || order[0] != "consumer_id" || order[1] != "limit" {
		t.Errorf("order: %+v", order)
	}
}

func TestNormalizeSQLParams_DedupesRepeatedReferences(t *testing.T) {
	// An input referenced multiple times in the same SQL body collapses
	// to a single positional placeholder so the bind layer sends one
	// value per declared input, not one per textual reference. Without
	// this, pgx errors with "expected N args, got M" because the SQL
	// text and the args slice disagree on count.
	inputs := []dsl.QueryParam{
		{Name: "vendor_id", Type: dsl.FieldType{Name: "varchar"}},
		{Name: "search", Type: dsl.FieldType{Name: "text"}},
		{Name: "limit", Type: dsl.FieldType{Name: "int"}},
	}
	sql := "SELECT * FROM t WHERE vendor_id = $vendor_id " +
		"AND ($search = '' OR title ILIKE $search OR descr ILIKE $search) " +
		"LIMIT $limit"
	out, order, err := normalizeSQLParams(sql, inputs)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !strings.Contains(out, "vendor_id = $1") {
		t.Errorf("expected vendor_id = $1, got %q", out)
	}
	if !strings.Contains(out, "$2 = ''") || !strings.Contains(out, "title ILIKE $2") || !strings.Contains(out, "descr ILIKE $2") {
		t.Errorf("expected all three search references to be $2, got %q", out)
	}
	if !strings.Contains(out, "LIMIT $3") {
		t.Errorf("expected LIMIT $3, got %q", out)
	}
	if len(order) != 3 || order[0] != "vendor_id" || order[1] != "search" || order[2] != "limit" {
		t.Errorf("order should have one entry per unique input in first-reference order, got %+v", order)
	}
}

func TestNormalizeSQLParams_RejectsUndeclaredArg(t *testing.T) {
	inputs := []dsl.QueryParam{
		{Name: "consumer_id", Type: dsl.FieldType{Name: "bigint"}},
	}
	_, _, err := normalizeSQLParams("SELECT $unknown", inputs)
	if err == nil {
		t.Fatal("expected error for undeclared $unknown")
	}
}

// vectorSchemaFixture adds an entity with vector + jsonb columns so the
// vector/array regression tests have realistic catalog-shaped targets
// to scan and bind against.
const vectorSchemaFixture = `
entity Account in consumer {
  id   bigint primary
  name text not null
}

entity ProductVariant in vendor {
  id         bigint primary
  search_vec vector(768)
  attrs      jsonb
}
`

// Regression for the codegen gap surfaced by HNSWSearchVariants: a
// vector(N) input must wrap through pgvector.NewVector at bind time
// so pgx sees the right binary format, and the file must import the
// pgvector package conditionally on the namespace using vectors.
func TestEmitCustomServer_QueryVectorInput(t *testing.T) {
	ir := lowerCustom(t, vectorSchemaFixture+`
query VectorSearch for vendor.ProductVariant {
  input { query_vec: vector(768), limit: int }
  output { id: bigint }
  sql touches(vendor.ProductVariant) {
    SELECT id FROM vendor_product_variant
    ORDER BY search_vec <=> $query_vec
    LIMIT $limit
  }
}
`)
	files, err := EmitCustomServer(ir)
	if err != nil {
		t.Fatalf("EmitCustomServer: %v", err)
	}
	var c string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/vendorpkg/custom_server.go") {
			c = f.Content
		}
	}
	if c == "" {
		t.Fatal("vendor custom_server.go not emitted")
	}
	parseAsGo(t, c)
	for _, want := range []string{
		// Conditional import lights up because a query in the namespace
		// uses vector(N).
		`pgvector "github.com/pgvector/pgvector-go"`,
		// Vector input wraps through pgvector.NewVector for pgx bind.
		`pgvector.NewVector(req.GetQueryVec())`,
		// Plain scalar input still passes through bare.
		`req.GetLimit()`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

// Regression for the codegen gap surfaced by FullSyncCatalogPage: a
// vector(N) output must scan into pgvector.Vector then convert via
// runtime.VectorToFloat32 onto the []float32 proto field — the prior
// code scanned into `var vN any` and failed type-check at compile.
func TestEmitCustomServer_QueryVectorOutput(t *testing.T) {
	ir := lowerCustom(t, vectorSchemaFixture+`
query VectorFetch for vendor.ProductVariant {
  input { id: bigint }
  output { id: bigint, vec: vector(768) }
  sql touches(vendor.ProductVariant) {
    SELECT id, search_vec FROM vendor_product_variant WHERE id = $id
  }
}
`)
	files, _ := EmitCustomServer(ir)
	var c string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/vendorpkg/custom_server.go") {
			c = f.Content
		}
	}
	if c == "" {
		t.Fatal("vendor custom_server.go not emitted")
	}
	parseAsGo(t, c)
	for _, want := range []string{
		`pgvector "github.com/pgvector/pgvector-go"`,
		// Scan target is pgvector.Vector, not any.
		`var v1 pgvector.Vector`,
		// Post-scan conversion routes through the runtime helper.
		`row.Vec = runtime.VectorToFloat32(v1.Slice())`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

// Regression for the codegen gap surfaced by VibeOutfitsByMessage: an
// []text output from array_agg(text) must scan into []string and
// assign directly — the prior code emitted `var vN string` against a
// []string proto field and failed type-check.
func TestEmitCustomServer_QueryArrayOutput(t *testing.T) {
	ir := lowerCustom(t, vectorSchemaFixture+`
query VariantIDs for vendor.ProductVariant {
  input { limit: int }
  output { ids: []text }
  sql touches(vendor.ProductVariant) {
    SELECT array_agg(id::text) FROM vendor_product_variant LIMIT $limit
  }
}
`)
	files, _ := EmitCustomServer(ir)
	var c string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/vendorpkg/custom_server.go") {
			c = f.Content
		}
	}
	if c == "" {
		t.Fatal("vendor custom_server.go not emitted")
	}
	parseAsGo(t, c)
	for _, want := range []string{
		`var v0 []string`,
		`row.Ids = v0`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
	// Negative: the array output must NOT trigger the pgvector import
	// since no vector columns are involved.
	if strings.Contains(c, `pgvector "github.com/pgvector/pgvector-go"`) {
		t.Errorf("pgvector import emitted for an array-only namespace; should be conditional")
	}
}

// Smoke test that the conditional pgvector import stays OFF for a
// namespace whose queries use only plain scalars — guards against a
// regression that unconditionally imports pgvector everywhere.
func TestEmitCustomServer_NoPgvectorImportWhenUnused(t *testing.T) {
	ir := lowerCustom(t, customSchemaFixture+`
query OutfitsForConsumer for SavedOutfit {
  input { consumer_id: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT id FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
  }
}
`)
	files, _ := EmitCustomServer(ir)
	c := files[0].Content
	parseAsGo(t, c)
	if strings.Contains(c, "pgvector") {
		t.Errorf("pgvector mentioned in custom_server.go for a vector-free namespace:\n%s", c)
	}
}
