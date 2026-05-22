package codegen

import (
	"strings"
	"testing"
)

// assertContains is a small helper for substring assertions on emitted SQL.
// It's loose by design: we want to verify the emitter generates the right
// shape, not pin every whitespace detail.
func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected output to contain:\n  %q\nfull output:\n%s", needle, haystack)
	}
}

func assertNotContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("expected output NOT to contain:\n  %q\nfull output:\n%s", needle, haystack)
	}
}

func TestEmit_Initial_BareEntity(t *testing.T) {
	ir := lower(t, `entity Account in consumer { id bigint primary  email text not null unique }`)
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	up := scripts.Up
	assertContains(t, up, "CREATE SCHEMA IF NOT EXISTS atlantis")
	assertContains(t, up, "CREATE EXTENSION IF NOT EXISTS vector")
	assertContains(t, up, "CREATE EXTENSION IF NOT EXISTS timescaledb")
	assertContains(t, up, `CREATE TABLE IF NOT EXISTS "atlantis"."consumer_account"`)
	assertContains(t, up, `"id" BIGINT`)
	assertContains(t, up, `"email" TEXT NOT NULL UNIQUE`)
	assertContains(t, up, `CONSTRAINT "consumer_account_pkey" PRIMARY KEY ("id")`)
	assertContains(t, scripts.Down, `DROP TABLE IF EXISTS "atlantis"."consumer_account" CASCADE`)
	assertContains(t, scripts.Down, "DROP SCHEMA IF EXISTS atlantis CASCADE")
}

func TestEmit_Initial_AllScalarTypes(t *testing.T) {
	ir := lower(t, `
entity K in lab {
  id bigint primary
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
}
`)
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	up := scripts.Up
	for _, sub := range []string{
		`"a" SMALLINT`, `"b" INTEGER`, `"c" BIGINT`, `"d" TEXT`, `"e" BOOLEAN`,
		`"f" TIMESTAMPTZ`, `"g" UUID`, `"h" BYTEA`, `"i" NUMERIC(12, 4)`,
		`"j" JSONB`, `"k" vector(32)`, `"l" TEXT[]`,
	} {
		assertContains(t, up, sub)
	}
}

func TestEmit_Initial_FKBetweenEntities(t *testing.T) {
	ir := lower(t, `
entity Account in consumer { id bigint primary }
entity Outfit in consumer {
  id bigint primary
  consumer_id bigint references consumer.Account.id on delete cascade
}
`)
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	up := scripts.Up
	// Account must be created before Outfit (FK target first).
	accIdx := strings.Index(up, `CREATE TABLE IF NOT EXISTS "atlantis"."consumer_account"`)
	outfitIdx := strings.Index(up, `CREATE TABLE IF NOT EXISTS "atlantis"."consumer_outfit"`)
	if accIdx < 0 || outfitIdx < 0 {
		t.Fatalf("expected both CREATEs:\n%s", up)
	}
	if accIdx >= outfitIdx {
		t.Errorf("Account should be created before Outfit (topo order). Account@%d Outfit@%d", accIdx, outfitIdx)
	}
	assertContains(t, up, `FOREIGN KEY ("consumer_id") REFERENCES "atlantis"."consumer_account" ("id")`)
	assertContains(t, up, "ON DELETE CASCADE")
}

func TestEmit_Initial_AllIndexKinds(t *testing.T) {
	ir := lower(t, `
entity P in v {
  id        bigint primary
  consumer  bigint
  created   timestamptz
  vec       vector(32)
  meta      jsonb
  deleted_at timestamptz

  index by consumer, created desc
  index hnsw on vec ops cosine
  index gin on meta
  index partial by consumer where deleted_at is null
}
`)
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	up := scripts.Up
	assertContains(t, up, `CREATE INDEX IF NOT EXISTS "v_p_consumer_created_idx" ON "atlantis"."v_p" ("consumer", "created" DESC);`)
	assertContains(t, up, `CREATE INDEX IF NOT EXISTS "v_p_vec_hnsw_idx" ON "atlantis"."v_p" USING hnsw ("vec" vector_cosine_ops);`)
	assertContains(t, up, `CREATE INDEX IF NOT EXISTS "v_p_meta_gin_idx" ON "atlantis"."v_p" USING gin ("meta");`)
	assertContains(t, up, `CREATE INDEX IF NOT EXISTS "v_p_consumer_partial_idx" ON "atlantis"."v_p" ("consumer") WHERE "deleted_at" IS NULL;`)
}

func TestEmit_Initial_Hypertable(t *testing.T) {
	ir := lower(t, `
hypertable Purchase in vendor on purchased_at {
  id           bigint primary
  qty          int not null
  purchased_at timestamptz not null
}
`)
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	assertContains(t, scripts.Up, `create_hypertable('"atlantis"."vendor_purchase"', 'purchased_at'`)
}

func TestEmit_Initial_DefaultValues(t *testing.T) {
	ir := lower(t, `
entity D in x {
  id bigint primary
  a int default 42
  b text default "hi"
  c boolean default true
  d boolean default false
  e timestamptz default now()
  f text default "O'Brien"
}
`)
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	up := scripts.Up
	for _, sub := range []string{
		"DEFAULT 42", `DEFAULT 'hi'`, "DEFAULT TRUE", "DEFAULT FALSE",
		"DEFAULT now()", `DEFAULT 'O''Brien'`, // single quote escaped
	} {
		assertContains(t, up, sub)
	}
}

// ---- diff-based emission ----

func TestEmit_Diff_NoChanges(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary }`)
	d := ComputeDiff(ir, ir)
	scripts, err := EmitSQL(ir, ir, d)
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	assertContains(t, scripts.Up, "(no schema changes)")
}

func TestEmit_Diff_EntityAdded(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `
entity A in x { id bigint primary }
entity B in x { id bigint primary  name text }
`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, `CREATE TABLE IF NOT EXISTS "atlantis"."x_b"`)
	assertContains(t, scripts.Down, `DROP TABLE IF EXISTS "atlantis"."x_b"`)
	// No A changes.
	assertNotContains(t, scripts.Up, `CREATE TABLE IF NOT EXISTS "atlantis"."x_a"`)
}

func TestEmit_Diff_EntityRemovedIsBreakingBanner(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary } entity B in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, "BREAKING — REVIEW CAREFULLY")
	assertContains(t, scripts.Up, `DROP TABLE IF EXISTS "atlantis"."x_b"`)
}

func TestEmit_Diff_FieldAddedNullable(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary  name text }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, `ALTER TABLE "atlantis"."x_a" ADD COLUMN "name" TEXT`)
	assertContains(t, scripts.Down, `ALTER TABLE "atlantis"."x_a" DROP COLUMN "name"`)
}

func TestEmit_Diff_FieldAddedNotNullDefault(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text not null default "" }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, `ADD COLUMN "v" TEXT NOT NULL DEFAULT ''`)
}

func TestEmit_Diff_FieldAddedNotNullNoDefaultIsBackfillBanner(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text not null }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, "BACKFILL REQUIRED")
}

func TestEmit_Diff_FieldRemoved(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text }`)
	newIR := lower(t, `entity A in x { id bigint primary }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, `ALTER TABLE "atlantis"."x_a" DROP COLUMN "v"`)
	// Down reverses by re-adding the column.
	assertContains(t, scripts.Down, `ALTER TABLE "atlantis"."x_a" ADD COLUMN "v" TEXT`)
}

func TestEmit_Diff_NotNullTightenedLoosened(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text not null }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, `ALTER COLUMN "v" SET NOT NULL`)
	assertContains(t, scripts.Down, `ALTER COLUMN "v" DROP NOT NULL`)
}

func TestEmit_Diff_TypeChange(t *testing.T) {
	oldIR := lower(t, `entity A in x { id smallint primary }`)
	newIR := lower(t, `entity A in x { id int primary }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, `ALTER COLUMN "id" TYPE INTEGER`)
	assertContains(t, scripts.Down, `ALTER COLUMN "id" TYPE SMALLINT`)
}

func TestEmit_Diff_DefaultChanged(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v int default 1 }`)
	newIR := lower(t, `entity A in x { id bigint primary  v int default 2 }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, "SET DEFAULT 2")
	assertContains(t, scripts.Down, "SET DEFAULT 1")
}

func TestEmit_Diff_UniqueAddedRemoved(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text unique }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, `ADD CONSTRAINT "x_a_v_key" UNIQUE ("v")`)
	assertContains(t, scripts.Down, `DROP CONSTRAINT IF EXISTS "x_a_v_key"`)
}

func TestEmit_Diff_FKAddedRemoved(t *testing.T) {
	oldIR := lower(t, `
entity Account in x { id bigint primary }
entity B in x { id bigint primary  account_id bigint }
`)
	newIR := lower(t, `
entity Account in x { id bigint primary }
entity B in x { id bigint primary  account_id bigint references x.Account.id on delete restrict }
`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, `ADD CONSTRAINT "x_b_account_id_fkey" FOREIGN KEY ("account_id") REFERENCES "atlantis"."x_account" ("id") ON DELETE RESTRICT`)
	assertContains(t, scripts.Down, `DROP CONSTRAINT IF EXISTS "x_b_account_id_fkey"`)
}

func TestEmit_Diff_FKModified(t *testing.T) {
	oldIR := lower(t, `
entity A in x { id bigint primary }
entity B in x { id bigint primary }
entity C in x { id bigint primary  ref bigint references x.A.id }
`)
	newIR := lower(t, `
entity A in x { id bigint primary }
entity B in x { id bigint primary }
entity C in x { id bigint primary  ref bigint references x.B.id }
`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	// Drop old + add new on up; reverse on down.
	assertContains(t, scripts.Up, `DROP CONSTRAINT IF EXISTS "x_c_ref_fkey"`)
	assertContains(t, scripts.Up, `REFERENCES "atlantis"."x_b" ("id")`)
	assertContains(t, scripts.Down, `REFERENCES "atlantis"."x_a" ("id")`)
}

func TestEmit_Diff_IndexAddedRemoved(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v int }`)
	newIR := lower(t, `entity A in x { id bigint primary  v int  index by v }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, `CREATE INDEX IF NOT EXISTS "x_a_v_idx" ON "atlantis"."x_a" ("v")`)
	assertContains(t, scripts.Down, `DROP INDEX IF EXISTS "atlantis"."x_a_v_idx"`)
}

func TestEmit_Diff_CacheChangeIsNoSQL(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary  cache { read_through ttl=10m } }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, _ := EmitSQL(oldIR, newIR, d)
	assertContains(t, scripts.Up, "no SQL: cache")
	// And there shouldn't be any ALTER TABLE statements.
	assertNotContains(t, scripts.Up, "ALTER TABLE")
}

func TestEmit_TopoSort_CycleDetected(t *testing.T) {
	// Two entities referencing each other — true cycle, not supported in v0.1.
	ir := lower(t, `
entity A in x { id bigint primary  b_id bigint references x.B.id }
entity B in x { id bigint primary  a_id bigint references x.A.id }
`)
	_, err := EmitInitial(ir)
	if err == nil || !strings.Contains(err.Error(), "FK cycle") {
		t.Errorf("expected FK cycle error, got %v", err)
	}
}

func TestEmit_TopoSort_SelfReferenceOK(t *testing.T) {
	ir := lower(t, `entity Node in x { id bigint primary  parent_id bigint references x.Node.id }`)
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("self-reference should be supported: %v", err)
	}
	assertContains(t, scripts.Up, `FOREIGN KEY ("parent_id") REFERENCES "atlantis"."x_node" ("id")`)
}

func TestSnakeCase(t *testing.T) {
	cases := map[string]string{
		"Account":       "account",
		"SavedOutfit":   "saved_outfit",
		"OAuthToken":    "o_auth_token",
		"APIKey":        "api_key",
		"ProductV2":     "product_v2",
		"id":            "id",
		"already_snake": "already_snake",
	}
	for in, want := range cases {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q want %q", in, got, want)
		}
	}
}
