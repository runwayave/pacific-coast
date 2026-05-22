package codegen

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// entityServerFile returns the content of the single per-entity server
// file in files, ignoring the always-present register.go aggregator. Many
// fixtures lower a one-entity IR and want the entity file's content; the
// sort order depends on namespace name versus "register.go" so indexing
// by files[0] is brittle.
func entityServerFile(t *testing.T, files []GoFile) string {
	t.Helper()
	var hits []GoFile
	for _, f := range files {
		if f.Path != "gen/go/server/register.go" {
			hits = append(hits, f)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("entityServerFile: want exactly one entity file, got %d", len(hits))
	}
	return hits[0].Content
}

// parseAsGo verifies the emitted code is at least syntactically valid Go.
// We don't try to type-check it (that would need a full module setup);
// passing the parser catches the most common emission bugs.
func parseAsGo(t *testing.T, src string) {
	t.Helper()
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, "generated.go", src, parser.AllErrors); err != nil {
		t.Fatalf("emitted code does not parse:\n%v\n---\n%s", err, src)
	}
}

func TestEmitGoServer_SimpleEntity_ParsesAsGo(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  id    bigint primary
  email text not null unique
}
`)
	files, err := EmitGoServer(ir)
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	// 2 = one per-entity file + the top-level register.go aggregator.
	// Every codegen run produces register.go regardless of entity count.
	if len(files) != 2 {
		t.Fatalf("want 2 files (entity + register.go), got %d", len(files))
	}
	for _, f := range files {
		parseAsGo(t, f.Content)
	}
}

func TestEmitGoServer_PathFormat(t *testing.T) {
	// Per-namespace package layout: gen/go/server/<ns>/<entity>_server.go.
	// Mirrors the proto layout and fixes the historical Cart/CartItem-style
	// collision (PHASE_D.md decision #9).
	ir := lower(t, `entity SavedOutfit in consumer { id bigint primary }`)
	files, _ := EmitGoServer(ir)
	if files[0].Path != "gen/go/server/consumer/saved_outfit_server.go" {
		t.Errorf("path: %s", files[0].Path)
	}
}

func TestEmitGoServer_PackageHeader(t *testing.T) {
	ir := lower(t, `entity Account in consumer { id bigint primary }`)
	files, _ := EmitGoServer(ir)
	// `package consumer`, not `package server` — per the per-namespace
	// layout. Vendor entities would be `package vendor`.
	assertContains(t, entityServerFile(t, files), "package consumer")
	assertNotContains(t, entityServerFile(t, files), "package server")
}

func TestEmitGoServer_EmitsAllSixCoreMethods(t *testing.T) {
	// Method names match the buf-generated XxxServiceServer interface
	// (verb + entity, e.g. GetAccount, not Get + Server-typed Get).
	ir := lower(t, `entity A in x { id bigint primary  v text }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	for _, sig := range []string{
		"func (s *AServer) GetA(",
		"func (s *AServer) ListA(",
		"func (s *AServer) BatchGetA(",
		"func (s *AServer) CreateA(",
		"func (s *AServer) UpdateA(",
		"func (s *AServer) DeleteA(",
	} {
		assertContains(t, c, sig)
	}
}

func TestEmitGoServer_NativeProtoSignatures(t *testing.T) {
	// Every method takes a proto request and returns a proto response.
	// This is the load-bearing assertion of Phase D: the handler IS the
	// buf-generated service interface; no adapter shim.
	ir := lower(t, `entity Account in consumer { id bigint primary  email text not null }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	for _, sig := range []string{
		"GetAccount(ctx context.Context, req *pb.GetAccountRequest) (*pb.GetAccountResponse, error)",
		"ListAccount(ctx context.Context, req *pb.ListAccountRequest) (*pb.ListAccountResponse, error)",
		"BatchGetAccount(ctx context.Context, req *pb.BatchGetAccountRequest) (*pb.BatchGetAccountResponse, error)",
		"CreateAccount(ctx context.Context, req *pb.CreateAccountRequest) (*pb.CreateAccountResponse, error)",
		"UpdateAccount(ctx context.Context, req *pb.UpdateAccountRequest) (*pb.UpdateAccountResponse, error)",
		"DeleteAccount(ctx context.Context, req *pb.DeleteAccountRequest) (*pb.DeleteAccountResponse, error)",
	} {
		assertContains(t, c, sig)
	}
}

func TestEmitGoServer_InterfaceSatisfactionAssertion(t *testing.T) {
	// The trailing `var _ pb.<Entity>ServiceServer = (*<Entity>Server)(nil)`
	// line is what catches a handler-signature drift at `go build` time
	// — way before a real RPC is ever issued. PHASE_D decision #4.
	ir := lower(t, `entity Account in consumer { id bigint primary  email text not null }`)
	files, _ := EmitGoServer(ir)
	assertContains(t, entityServerFile(t, files), "var _ pb.AccountServiceServer = (*AccountServer)(nil)")
}

func TestEmitGoServer_EmbedsUnimplementedServer(t *testing.T) {
	// `require_unimplemented_servers=true` in buf.gen.yaml means the
	// AccountServiceServer interface requires the embed. Without it,
	// adding a new RPC to the .proto would break every existing server.
	ir := lower(t, `entity Account in consumer { id bigint primary }`)
	files, _ := EmitGoServer(ir)
	assertContains(t, entityServerFile(t, files), "pb.UnimplementedAccountServiceServer")
}

func TestEmitGoServer_NoLegacyRowStruct(t *testing.T) {
	// Phase D drops the `<Entity>Row` struct — the proto message IS the
	// canonical row type. A stray `type AccountRow struct` would signal
	// a partial revert.
	ir := lower(t, `entity Account in consumer { id bigint primary }`)
	files, _ := EmitGoServer(ir)
	assertNotContains(t, entityServerFile(t, files), "type AccountRow struct")
}

func TestEmitGoServer_BakedSQLStatements(t *testing.T) {
	ir := lower(t, `entity Account in consumer { id bigint primary  email text not null }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	// SQL constants are unchanged from pre-Phase-D — Phase D only retargets
	// the I/O at the proto types, the SQL stays the same. Identifiers are
	// double-quoted (defense-in-depth against PG reserved words).
	assertContains(t, c, `SELECT "id", "email" FROM "atlantis"."consumer_account" WHERE "id" = $1`)
	assertContains(t, c, `SELECT "id", "email", COUNT(*) OVER () AS total FROM "atlantis"."consumer_account"`)
	assertContains(t, c, `INSERT INTO "atlantis"."consumer_account" ("id", "email") VALUES ($1, $2) RETURNING "id"`)
	assertContains(t, c, `UPDATE "atlantis"."consumer_account" SET "email" = $1 WHERE "id" = $2`)
	assertContains(t, c, `DELETE FROM "atlantis"."consumer_account" WHERE "id" = $1`)
	assertContains(t, c, `SELECT "id", "email" FROM "atlantis"."consumer_account" WHERE "id" = ANY($1)`)
}

func TestEmitGoServer_InsertWrapsDefaultableColumnsInCOALESCE(t *testing.T) {
	// Columns with declared SQL DEFAULT round-trip through COALESCE so an
	// unset proto field (binding NULL) resolves to the column default
	// instead of writing literal NULL. This is the durable fix for the
	// caller-side `nowIfZero` shims — see plans/codegen-insert-default-and-autoid.md.
	ir := lower(t, `
entity Thing in consumer {
  id            bigint primary
  name          text not null
  status        varchar(20) default "pending"
  created_at    timestamptz default now()
  updated_at    timestamptz not null default now()
  retry_count   int not null default 0
  paid_at       timestamptz
}
`)
	files, err := EmitGoServer(ir)
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	c := entityServerFile(t, files)

	// Per-column placement in the VALUES clause depends on DSL declaration
	// order (id, name, status, created_at, updated_at, retry_count, paid_at).
	// `id` and `name` carry no default → plain $N. `status`, `created_at`,
	// `updated_at`, `retry_count` carry defaults → COALESCE. `paid_at` is
	// nullable without a default → plain $N (the bind layer will pass NULL,
	// PG accepts NULL since the column is nullable).
	assertContains(t, c, `VALUES ($1, $2, COALESCE($3::VARCHAR(20), 'pending'), COALESCE($4::TIMESTAMPTZ, now()), COALESCE($5::TIMESTAMPTZ, now()), COALESCE($6::INTEGER, 0), $7)`)
}

func TestEmitGoServer_BindForInsertRoutesDefaultableThroughNullable(t *testing.T) {
	// NOT NULL columns with a declared DEFAULT are emitted as `optional`
	// in the proto so callers can omit them. The bind layer must route
	// these through the runtime.NullableX helpers (pointer-aware) rather
	// than the value-direct getters, otherwise the Go zero value lands on
	// the wire and COALESCE never sees NULL.
	ir := lower(t, `
entity Thing in consumer {
  id          bigint primary
  status      varchar(20) not null default "pending"
  retry_count int not null default 0
  is_active   boolean not null default true
  created_at  timestamptz not null default now()
}
`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)

	// Bind expressions for every NOT-NULL-WITH-DEFAULT field should use the
	// pointer-style helper, mirroring how nullable fields bind.
	assertContains(t, c, "runtime.NullableString(in.Status)")
	assertContains(t, c, "runtime.NullableInt32(in.RetryCount)")
	assertContains(t, c, "runtime.NullableBool(in.IsActive)")
	assertContains(t, c, "runtime.ProtoToTimePtr(in.CreatedAt)")
}

func TestEmitGoServer_InsertNoDefaultColumnsUnchanged(t *testing.T) {
	// Regression: entities with zero default columns should emit the
	// original plain $N placeholder shape — no COALESCE wrapping.
	ir := lower(t, `entity Plain in consumer { id bigint primary  name text not null }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	assertContains(t, c, `VALUES ($1, $2) RETURNING`)
	if strings.Contains(c, "COALESCE(") {
		t.Fatalf("expected no COALESCE in INSERT for entity with no defaults, got:\n%s", c)
	}
}

func TestEmitGoServer_ScanIntoAndBindForHelpers(t *testing.T) {
	// Per-entity inlined helpers — sqlc / Ent pattern, no reflection.
	// scanInto<E> + scanInto<E>WithTotal + scanInto<E>WithDistance for
	// the three SELECT-shape variants; bindFor<E>Insert + bindFor<E>Update
	// for the two write-shape variants.
	ir := lower(t, `entity Account in consumer { id bigint primary  email text not null }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	for _, sig := range []string{
		"func scanIntoAccount(src interface{ Scan(dest ...any) error }, out *pb.Account) error",
		"func scanIntoAccountWithTotal(",
		"func bindForAccountInsert(in *pb.Account) []any",
		// bindForUpdate takes the PK as variadic so one signature serves
		// single-PK (one $N placeholder) and composite-PK (N placeholders)
		// entities. The caller splats a []any holding PK columns in DSL
		// declaration order, which is the same order the SQL emitter used
		// when building the WHERE clause.
		"func bindForAccountUpdate(in *pb.Account, pk ...any) []any",
	} {
		assertContains(t, c, sig)
	}
}

func TestEmitGoServer_ScanIntoNullableFields(t *testing.T) {
	// Nullable scalars route through sql.NullX → runtime.<X>PtrFromNull.
	// The proto field is `*string` / `*int32` etc., so the scan target is
	// a NullX local and the assign goes through the runtime helper.
	ir := lower(t, `entity A in x {
  id    bigint primary
  email text not null
  alias text
  age   int
}`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	for _, sub := range []string{
		"var emailLocal string",                             // not null → bare string
		"var aliasLocal sql.NullString",                     // nullable → NullString
		"var ageLocal sql.NullInt32",                        // nullable int → NullInt32
		"out.Alias = runtime.StringPtrFromNull(aliasLocal)", // post-assign via helper
		"out.Age = runtime.Int32PtrFromNull(ageLocal)",
	} {
		assertContains(t, c, sub)
	}
}

func TestEmitGoServer_BindForNullableFields(t *testing.T) {
	// Inverse direction: proto `*T` field → sql.NullX via NullableX helper.
	ir := lower(t, `entity A in x {
  id    bigint primary
  email text not null
  alias text
  age   int
}`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	// Required string field reads via getter (returns "" if absent).
	assertContains(t, c, "in.GetEmail()")
	// Nullable fields read the pointer directly so NullableX can branch.
	assertContains(t, c, "runtime.NullableString(in.Alias)")
	assertContains(t, c, "runtime.NullableInt32(in.Age)")
}

func TestEmitGoServer_TimestampHandling(t *testing.T) {
	// timestamptz NOT NULL: scan into time.Time, wrap via TimeToProto.
	// timestamptz NULLABLE: scan into sql.NullTime, conditional TimeToProto
	// (inline because TimePtrToProto takes *time.Time, not NullTime).
	ir := lower(t, `entity A in x {
  id         bigint primary
  created_at timestamptz not null
  deleted_at timestamptz
}`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	assertContains(t, c, "var createdAtLocal time.Time")
	assertContains(t, c, "out.CreatedAt = runtime.TimeToProto(createdAtLocal)")
	assertContains(t, c, "var deletedAtLocal sql.NullTime")
	assertContains(t, c, "out.DeletedAt = runtime.TimeToProto(deletedAtLocal.Time)")
}

func TestEmitGoServer_VectorHandling(t *testing.T) {
	// Vector scan target is pgvector.Vector; on the read path we unwrap
	// via .Slice() and route through runtime.VectorToFloat32. On bind
	// we wrap the proto []float32 into pgvector.NewVector.
	ir := lower(t, `entity P in x {
  id  bigint primary
  vec vector(8)
}`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	assertContains(t, c, "pgvector \"github.com/pgvector/pgvector-go\"")
	assertContains(t, c, "var vecLocal pgvector.Vector")
	assertContains(t, c, "out.Vec = runtime.VectorToFloat32(vecLocal.Slice())")
	assertContains(t, c, "pgvector.NewVector(in.GetVec())")
}

func TestEmitGoServer_NoPgvectorImportWithoutVectorField(t *testing.T) {
	// Entities without a vector field shouldn't pull the pgvector dep
	// into their generated file.
	ir := lower(t, `entity A in x { id bigint primary  email text not null }`)
	files, _ := EmitGoServer(ir)
	assertNotContains(t, entityServerFile(t, files), "github.com/pgvector/pgvector-go")
}

func TestEmitGoServer_VectorSearchMethodForHNSWIndex(t *testing.T) {
	ir := lower(t, `
entity ProductVariant in vendor {
  id          bigint primary
  search_vec  vector(768)
  index hnsw on search_vec ops cosine
}
`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	parseAsGo(t, c)

	// Search method takes the proto request, returns proto response.
	assertContains(t, c, "SearchProductVariantBySearchVec(ctx context.Context, req *pb.SearchProductVariantBySearchVecRequest)")
	assertContains(t, c, `"search_vec" <=> $1::vector AS distance`)
	assertContains(t, c, `ORDER BY "search_vec" <=> $1::vector LIMIT $2`)
	// The query vector arrives via the proto's GetQueryVector() (the
	// codegen converts `repeated float` to []float32 in proto land).
	assertContains(t, c, "pgvector.NewVector(req.GetQueryVector())")
}

func TestEmitGoServer_OtherVectorOps(t *testing.T) {
	cases := map[string]string{
		"l2":     "<->",
		"ip":     "<#>",
		"cosine": "<=>",
	}
	for ops, op := range cases {
		ir := lower(t, "entity P in x { id bigint primary  v vector(8)  index hnsw on v ops "+ops+" }")
		files, _ := EmitGoServer(ir)
		assertContains(t, entityServerFile(t, files), `"v" `+op+" $1::vector")
	}
}

func TestEmitGoServer_NoVectorSearchWithoutHNSW(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary  v vector(8) }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	// No HNSW index → no search method emitted.
	assertNotContains(t, c, "SearchA")
}

func TestEmitGoServer_CustomQueryTimeout(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary  query_timeout = 30s }`)
	files, _ := EmitGoServer(ir)
	assertContains(t, entityServerFile(t, files), "const aQueryTimeoutMS = 30000")
}

func TestEmitGoServer_DefaultQueryTimeout(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary }`)
	files, _ := EmitGoServer(ir)
	assertContains(t, entityServerFile(t, files), "const aQueryTimeoutMS = 2000")
}

// TestEmitGoServer_GetWrapsQuery pins the back-compat shim's shape: the
// generated GetX builds a typed PK-eq filter against the entity's
// Filter message and calls QueryX. The pre-Step-8 direct-PG sqlGet path
// is gone from this handler (it survives only as the Update read-back).
// Removing this shim later is one emitter delete.
func TestEmitGoServer_GetWrapsQuery(t *testing.T) {
	ir := lower(t, `entity Account in consumer { id varchar(15) primary  email text not null }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)

	// Wrapper body constructs the typed filter on the PK column,
	// delegates to QueryX with Limit=1, and surfaces ErrNotFound from
	// an empty entity list. Direct s.DB.QueryRow on sqlGetAccount is
	// gone from the Get handler.
	for _, sub := range []string{
		"filter := &pb.AccountFilter{",
		"Id: &commonpb.StringPredicate{Op: &commonpb.StringPredicate_Eq{Eq: req.GetId()}}",
		"qResp, err := s.QueryAccount(ctx, &pb.QueryAccountRequest{Filter: filter, Limit: 1})",
		"return nil, runtime.ErrNotFound",
		"return &pb.GetAccountResponse{Entity: qResp.GetEntities()[0]}, nil",
	} {
		assertContains(t, c, sub)
	}

	// Deprecation note is in the doc comment so the next reader knows
	// the shim is scheduled for removal.
	assertContains(t, c, "Deprecated: thin wrapper around QueryAccount")

	// The sqlGetAccount const still exists for Update's trigger
	// read-back, but the Get handler no longer references it directly.
	getStart := strings.Index(c, "func (s *AccountServer) GetAccount(")
	getEnd := strings.Index(c[getStart:], "\nfunc ")
	if getStart < 0 || getEnd < 0 {
		t.Fatal("could not locate GetAccount handler body")
	}
	body := c[getStart : getStart+getEnd]
	if strings.Contains(body, "sqlGetAccount") {
		t.Errorf("GetAccount handler still references sqlGetAccount — the wrapper should delegate to QueryAccount instead")
	}
}

// TestEmitGoServer_ListRejectsOffset pins the AIP-158-style offset
// rejection on the List shim. The pre-Step-8 LIMIT/OFFSET path can't be
// mapped onto QueryX's opaque keyset cursor without either re-scanning
// (slow) or returning unstable results under concurrent writes (wrong),
// so the shim rejects req.offset > 0 outright and points callers at
// page_token. List's response shape gained NextPageToken so callers can
// move to keyset without leaving the legacy RPC entirely.
func TestEmitGoServer_ListRejectsOffset(t *testing.T) {
	ir := lower(t, `entity Account in consumer { id varchar(15) primary  email text not null }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)

	for _, sub := range []string{
		"if req.GetOffset() > 0 {",
		"codes.InvalidArgument",
		"offset is deprecated",
		"page_token via QueryAccount",
		"s.QueryAccount(ctx, &pb.QueryAccountRequest{Limit: req.GetLimit()})",
		"NextPageToken: qResp.GetNextPageToken()",
		"Total:         qResp.GetTotalEstimate()",
	} {
		assertContains(t, c, sub)
	}
}

// TestEmitGoServer_BatchGetCappedAt200 pins both halves of the BatchGet
// shim's safety floor: the typed in-list filter through QueryX, and the
// 200-id cap that matches query.MaxInListSize (an explicit error here
// gives callers a clearer signal than the deeper-stack translator
// rejection). String PKs use StringPredicate_In + StringList; other PK
// types route through predicateListGoName to the matching wrapper.
func TestEmitGoServer_BatchGetCappedAt200(t *testing.T) {
	ir := lower(t, `entity Account in consumer { id varchar(15) primary  email text not null }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)

	for _, sub := range []string{
		"ids := req.GetIds()",
		"if len(ids) > 200 {",
		"BatchGetAccount: at most 200 ids per call",
		"&commonpb.StringPredicate{Op: &commonpb.StringPredicate_In{In: &commonpb.StringList{Values: ids}}}",
		"s.QueryAccount(ctx, &pb.QueryAccountRequest{Filter: filter, Limit: int32(len(ids))})",
		"return &pb.BatchGetAccountResponse{Entities: qResp.GetEntities()}, nil",
	} {
		assertContains(t, c, sub)
	}
}

// TestEmitGoServer_BatchGetFallsBackForUnsupportedPK confirms that an
// entity whose PK type lacks a typed `_In` arm in the predicates proto
// (numeric / bool / timestamp) routes to the original direct-PG
// BatchGet rather than emitting a broken wrapper. No production schema
// uses such a PK today, but the codegen must not silently break if one
// arrives.
func TestEmitGoServer_BatchGetFallsBackForUnsupportedPK(t *testing.T) {
	ir := lower(t, `entity Snapshot in x { taken_at timestamptz primary  payload bytea }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)

	// Fallback emits the direct-PG ANY($1) form on sqlBatchGet rather
	// than the QueryX wrapper.
	assertContains(t, c, "rows, err := s.DB.Query(ctx, sqlBatchGetSnapshot,")
	assertNotContains(t, c, "BatchGetSnapshot: at most 200")
}

func TestEmitGoServer_CacheAndOutboxWiring(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary  v text }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)

	// Constructor takes the four runtime dependencies; QueryCache is the
	// tier-2 entry point and is threaded through ServerDeps in register.go.
	assertContains(t, c, "func NewAServer(db runtime.Pool, cache runtime.Cache, outbox runtime.Outbox, queryCache *queryresult.Cache)")
	assertContains(t, c, "QueryCache *queryresult.Cache")

	// cur+1 (not literal 1) preserves memcached's monotonic version guard
	// across PK reuse (soft-delete-then-recreate). cacheID via CompositeID
	// keeps the storage layer agnostic to PK arity — single-PK passes one
	// arg, composite passes each PK column. Update/Delete construct id as
	// a []any so the same `id...` splat lands in both tx.Exec and
	// CompositeID regardless of arity.
	assertContains(t, c, "cacheID := runtime.CompositeID(newPK)")
	assertContains(t, c, "cacheID := runtime.CompositeID(id...)")
	assertContains(t, c, "s.Cache.CurrentVersion(ctx, \"x.A\", cacheID)")
	assertContains(t, c, "s.Outbox.Enqueue(ctx, tx, \"x.A\", cacheID, cur+1)")
	assertContains(t, c, "cur+1")

	// Every write enqueues a per-entity generation_bump inside the same tx
	// as the row update so the tier-2 cache invalidation is atomic with the
	// write that caused it.
	assertContains(t, c, "s.Outbox.EnqueueGenerationBump(ctx, tx, \"x.A\")")

	// Update / Delete must report ErrNotFound when the row's gone, not a
	// silent no-op.
	assertContains(t, c, "tag.RowsAffected() == 0")
	assertContains(t, c, "runtime.ErrNotFound")
}

func TestEmitGoServer_DeterministicAcrossRuns(t *testing.T) {
	src := `
entity A in x { id bigint primary  v text  vec vector(16)  index hnsw on vec ops cosine }
entity B in y { id bigint primary  ref bigint references x.A.id }
`
	ir1 := lower(t, src)
	files1, _ := EmitGoServer(ir1)

	ir2 := lower(t, src)
	files2, _ := EmitGoServer(ir2)

	if len(files1) != len(files2) {
		t.Fatalf("file count mismatch")
	}
	for i := range files1 {
		if files1[i].Path != files2[i].Path {
			t.Errorf("path drift at %d: %s vs %s", i, files1[i].Path, files2[i].Path)
		}
		if files1[i].Content != files2[i].Content {
			t.Errorf("content drift at %d", i)
		}
	}
}

func TestEmitGoServer_HypertablesProduceFiles(t *testing.T) {
	ir := lower(t, `
hypertable Purchase in vendor on purchased_at {
  id           bigint primary
  qty          int not null
  purchased_at timestamptz not null
}
`)
	files, _ := EmitGoServer(ir)
	// 2 = the hypertable's entity file + register.go aggregator.
	if len(files) != 2 {
		t.Fatalf("want 2 files (entity + register.go), got %d", len(files))
	}
	// Hypertable entity sorts before register.go alphabetically.
	parseAsGo(t, entityServerFile(t, files))
}

// Composite-PK service interface is deferred to v0.2 (PHASE_D out-of-scope).
// The emitter writes a header-only stub so the per-namespace Go package
// keeps compiling and the path reserves itself for the day composite-PK
// services land.

// TestEmitGoServer_CompositePK_EmitsRealHandlers replaces the earlier
// stub assertion (composite-PK entities previously emitted a header-only
// file). The lift in Step 7 routes composite-PK entities through the
// same emitter as single-PK ones; this test pins the resulting shape so
// any future regression that re-introduces a stub is caught at compile
// time.
func TestEmitGoServer_CompositePK_EmitsRealHandlers(t *testing.T) {
	ir := lower(t, `
entity CartItem in consumer {
  cart_id    bigint not null
  variant_id bigint not null
  quantity   int not null

  primary by cart_id, variant_id
}
`)
	files, err := EmitGoServer(ir)
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	// 2 = the composite-PK handler file + register.go aggregator. The
	// stub file is gone; register.go now mounts CartItem alongside every
	// other entity.
	if len(files) != 2 {
		t.Fatalf("want 2 files (handler + register.go), got %d", len(files))
	}
	c := entityServerFile(t, files)
	parseAsGo(t, c)

	// Real handler: package header, struct, constructor, all seven RPC
	// methods. The earlier "deferred to v0.2" stub is no longer emitted.
	assertContains(t, c, "package consumer")
	assertNotContains(t, c, "deferred to v0.2")
	assertContains(t, c, "type CartItemServer struct")
	for _, method := range []string{
		"func (s *CartItemServer) GetCartItem(",
		"func (s *CartItemServer) DeleteCartItem(",
		"func (s *CartItemServer) UpdateCartItem(",
		"func (s *CartItemServer) QueryCartItem(",
	} {
		assertContains(t, c, method)
	}
	// Delete + Update construct id as a []any holding both PK columns
	// (DSL declaration order). This is what lets one template serve both
	// PK arities — the runtime.CompositeID splat works for either.
	assertContains(t, c, "id := []any{req.GetCartId(), req.GetVariantId()}")
	assertContains(t, c, "id := []any{in.GetCartId(), in.GetVariantId()}")
	// register.go MUST register CartItem now that the proto declares a
	// service block. Composite-PK entities are no longer exempt.
	var regContent string
	for _, f := range files {
		if f.Path == "gen/go/server/register.go" {
			regContent = f.Content
		}
	}
	if regContent == "" {
		t.Fatal("register.go not emitted")
	}
	assertContains(t, regContent, "RegisterCartItemServiceServer")

	if files[0].Path != "gen/go/server/consumer/cart_item_server.go" {
		t.Errorf("path: %s", files[0].Path)
	}
}

func TestEmitGoServer_CompositePK_Determinism(t *testing.T) {
	src := `
entity CartItem in consumer {
  cart_id    bigint not null
  variant_id bigint not null
  quantity   int not null

  primary by cart_id, variant_id
}
`
	a, _ := EmitGoServer(lower(t, src))
	b, _ := EmitGoServer(lower(t, src))
	if len(a) != len(b) || a[0].Content != b[0].Content {
		t.Fatalf("composite-PK handler emission is not deterministic across runs")
	}
}

func TestEmitGoServer_RegisterAggregator(t *testing.T) {
	// One Register(srv, deps) function aggregates every entity service.
	// The function lives at gen/go/server/register.go, package server,
	// importing each namespaced server package + the buf-generated proto.
	// Adding an entity to the .pc schema rewires this file automatically
	// on the next codegen run.
	ir := lower(t, `
entity Account in consumer { id bigint primary  email text not null }
entity Outfit  in consumer { id bigint primary  name  text not null }
entity Vendor  in vendor   { id bigint primary  name  text not null }
entity CartItem in consumer {
  cart_id    bigint not null
  variant_id bigint not null
  primary by cart_id, variant_id
}
`)
	files, err := EmitGoServer(ir)
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}

	var reg string
	for _, f := range files {
		if f.Path == "gen/go/server/register.go" {
			reg = f.Content
		}
	}
	if reg == "" {
		t.Fatal("register.go not emitted")
	}
	parseAsGo(t, reg)

	// Top-level package `server` — distinct from the per-namespace
	// `consumer` / `vendor` sub-packages.
	assertContains(t, reg, "package server")

	// ServerDeps consolidates the runtime tier into one value so the
	// Register signature stays short regardless of how many entities exist.
	for _, sub := range []string{
		"type ServerDeps struct {",
		"Pool       runtime.Pool",
		"Cache      runtime.Cache",
		"Outbox     runtime.Outbox",
		"QueryCache *queryresult.Cache",
		"func Register(srv *grpc.Server, deps ServerDeps) {",
	} {
		assertContains(t, reg, sub)
	}

	// Per-namespace alias scheme: `<ns>` for the server package, `pb<ns>`
	// for the buf-generated proto. Avoids collision with arbitrary
	// namespace names.
	assertContains(t, reg, `consumer "github.com/rachitkumar205/atlantis/gen/go/server/consumer"`)
	assertContains(t, reg, `pbconsumer "github.com/rachitkumar205/atlantis-go/pb/atlantis/consumer/v1"`)
	// The DSL namespace `vendor` collides with Go's reserved vendor/
	// directory name, so codegen remaps it to `vendorpkg` for the Go +
	// proto layer (see goNamespace in proto.go). DSL stays `entity X in
	// vendor`; SQL table prefix stays `vendor_*`.
	assertContains(t, reg, `vendorpkg "github.com/rachitkumar205/atlantis/gen/go/server/vendorpkg"`)
	assertContains(t, reg, `pbvendorpkg "github.com/rachitkumar205/atlantis-go/pb/atlantis/vendorpkg/v1"`)

	// Every entity gets a registration line — single-PK and composite-PK
	// alike. Order: namespace alphabetical, then entity alphabetical
	// within namespace.
	assertContains(t, reg, "pbconsumer.RegisterAccountServiceServer(srv, consumer.NewAccountServer(deps.Pool, deps.Cache, deps.Outbox, deps.QueryCache))")
	assertContains(t, reg, "pbconsumer.RegisterCartItemServiceServer(srv, consumer.NewCartItemServer(deps.Pool, deps.Cache, deps.Outbox, deps.QueryCache))")
	assertContains(t, reg, "pbconsumer.RegisterOutfitServiceServer(srv, consumer.NewOutfitServer(deps.Pool, deps.Cache, deps.Outbox, deps.QueryCache))")
	assertContains(t, reg, "pbvendorpkg.RegisterVendorServiceServer(srv, vendorpkg.NewVendorServer(deps.Pool, deps.Cache, deps.Outbox, deps.QueryCache))")
}

func TestEmitGoServer_DoNotEditBanner(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary }`)
	files, _ := EmitGoServer(ir)
	if !strings.Contains(entityServerFile(t, files), "DO NOT EDIT") {
		t.Errorf("missing DO NOT EDIT banner")
	}
}

func TestGoFieldType_ScalarTypes(t *testing.T) {
	// goFieldType is now only used for composite-PK struct fields and the
	// HNSW search method's queryVec parameter. Proto field types come from
	// buf-generated code, not from this function. The test pins the
	// mapping for the remaining call sites.
	cases := []struct {
		dslType string
		notNull bool
		want    string
	}{
		{"bigint", true, "int64"},
		{"bigint", false, "*int64"},
		{"int", true, "int32"},
		{"text", true, "string"},
		{"text", false, "*string"},
		{"boolean", true, "bool"},
		{"timestamptz", false, "*time.Time"},
		{"uuid", true, "string"},
		{"bytea", true, "[]byte"},
		{"bytea", false, "[]byte"},
		{"jsonb", false, "[]byte"},
		{"numeric", true, "string"},
	}
	for _, c := range cases {
		ft := dsl.FieldType{Name: c.dslType}
		if got := goFieldType(ft, c.notNull); got != c.want {
			t.Errorf("goFieldType(%s, notNull=%v) = %q want %q", c.dslType, c.notNull, got, c.want)
		}
	}
}

// ----------------------------------------------------------------------------
// Phase E — Go server emission for QueryX

func TestEmitGoServer_QueryHandlerEmitted(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  id    bigint primary
  email text not null
  age   int
}
`)
	files, err := EmitGoServer(ir)
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	c := entityServerFile(t, files)
	assertContains(t, c, "func (s *AccountServer) QueryAccount(ctx context.Context, req *pb.QueryAccountRequest) (*pb.QueryAccountResponse, error)")
	assertContains(t, c, "query.TranslateFilter(accountFilterSpec, req.GetFilter().ProtoReflect(),")
	assertContains(t, c, "const sqlQueryAccountPrefix = ")
	assertContains(t, c, "var accountFilterSpec = query.FilterSpec{")
	assertContains(t, c, "func (s *AccountServer) buildAccountKeysetCols(orders []*pb.AccountOrderBy) []runtime.KeysetColumn")
	assertContains(t, c, "func extractAccountCursor(ent *pb.Account, orders []*pb.AccountOrderBy) []any")
	// The handler must fetch limit+1 so the trailing row can supply the
	// next-page cursor.
	assertContains(t, c, "args = append(args, limit+1)")
	// limit+1 detection drops the boundary row before returning.
	assertContains(t, c, "if int32(len(resp.Entities)) > limit {")
	assertContains(t, c, "runtime.EncodePageToken(")
	assertContains(t, c, "runtime.DecodePageToken(req.GetPageToken(),")
	// Cursor predicate is AND-joined on top of any caller filter.
	assertContains(t, c, "runtime.KeysetPredicate(keysetCols, cursorVals, len(args)+1)")
}

func TestEmitGoServer_FilterSpecMatchesFieldTypes(t *testing.T) {
	ir := lower(t, `
entity Order in consumer {
  id          bigint primary
  amount      numeric(10,2) not null
  paid_at     timestamptz
  is_open     boolean not null
}
`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	assertContains(t, c, `"id": {Column: "id", Kind: query.PredicateInt64}`)
	assertContains(t, c, `"amount": {Column: "amount", Kind: query.PredicateNumeric}`)
	assertContains(t, c, `"paid_at": {Column: "paid_at", Kind: query.PredicateTimestamp}`)
	assertContains(t, c, `"is_open": {Column: "is_open", Kind: query.PredicateBool}`)
}

func TestEmitGoServer_SoftDeleteInjectedAsExtra(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  id         bigint primary
  email      text not null
  deleted_at timestamptz
  soft_delete by deleted_at
}
`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	// The QueryX handler should inject the soft-delete filter as an extra
	// predicate. The translator AND-joins extras; caller filter can't
	// subvert.
	assertContains(t, c, `extras = append(extras, "\"deleted_at\" IS NULL")`)
}

func TestEmitGoServer_PartitionInjected(t *testing.T) {
	ir := lower(t, `
entity Order in consumer {
  id          bigint primary
  consumer_id text not null
  partition by consumer_id
}
`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	// Partition predicate uses $1; caller filter placeholders start at $2.
	assertContains(t, c, "runtime.CallerPartition(ctx)")
	assertContains(t, c, `extras = append(extras, "\"consumer_id\" = $1")`)
	assertContains(t, c, "args = append([]any{partitionVal}, args...)")
	// placeholderStart should be 2 when partition is set.
	assertContains(t, c, "req.GetFilter().ProtoReflect(), 2")
}

func TestEmitGoServer_QueryHandlerLimitCap(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary }`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	assertContains(t, c, "limit = 100")  // default
	assertContains(t, c, "limit = 1000") // cap
}

func TestEmitGoServer_IncludeAttachFuncEmitted(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  id bigint primary
}
entity Address in consumer {
  id          bigint primary
  consumer_id bigint not null references consumer.Account.id
}
`)
	files, err := EmitGoServer(ir)
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	// Account is the include target. Its server file should hold the
	// attach helper and the dispatch case for the inbound FK.
	var accountSrc string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/consumer/account_server.go") {
			accountSrc = f.Content
		}
	}
	if accountSrc == "" {
		t.Fatalf("missing account_server.go")
	}
	assertContains(t, accountSrc, "func (s *AccountServer) attachAccountIncludeAddressByConsumerId(ctx context.Context, parents []*pb.Account) error")
	assertContains(t, accountSrc, "const sqlAttachAccountIncludeAddressByConsumerId = ")
	assertContains(t, accountSrc, "case pb.AccountInclude_ACCOUNT_INCLUDE_CONSUMER_ADDRESS_BY_CONSUMER_ID:")
	assertContains(t, accountSrc, "grouped := map[string][]*pb.Address{}")
	assertContains(t, accountSrc, "p.IncludedAddressByConsumerId = grouped[key]")
}

func TestEmitGoServer_KeysetCasesMatchEnum(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  id    bigint primary
  email text not null
}
`)
	files, _ := EmitGoServer(ir)
	c := entityServerFile(t, files)
	// One case arm per orderable field, mapping the typed enum to the
	// quoted SQL identifier ready for ORDER BY and the keyset predicate.
	assertContains(t, c, "case pb.AccountOrderField_ACCOUNT_ORDER_FIELD_ID:")
	assertContains(t, c, `ident = "\"id\""`)
	assertContains(t, c, "case pb.AccountOrderField_ACCOUNT_ORDER_FIELD_EMAIL:")
	assertContains(t, c, `ident = "\"email\""`)
	// PK is always appended as an ASC tiebreaker if the caller didn't
	// list it explicitly.
	assertContains(t, c, `cols = append(cols, runtime.KeysetColumn{QuotedIdent: "\"id\"", Desc: false})`)
}
