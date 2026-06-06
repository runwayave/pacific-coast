package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// fakeService builds a Service with just the mutation-gate fields wired —
// enough to exercise authorizeSelfApply without touching pgx.
func fakeService(allowAll bool, allowed []string, cn string) *Service {
	set := map[string]bool{}
	for _, c := range allowed {
		set[c] = true
	}
	return &Service{
		allowApplyMutation: allowAll,
		mutationAllowed:    set,
		callerFromContext:  func(context.Context) string { return cn },
	}
}

func TestAuthorizeSelfApply_AllowWildcardGrantsAll(t *testing.T) {
	s := fakeService(true, nil, "backend")
	if err := s.authorizeSelfApply(context.Background(), "backend"); err != nil {
		t.Fatalf("wildcard should permit own caller: %v", err)
	}
}

func TestAuthorizeSelfApply_RejectsCrossCallerEvenWithWildcard(t *testing.T) {
	// Wildcard allows mutation, but req.Caller must still match the CN.
	// A leaked backend cert can't push to "vendor" even when the global
	// wildcard is on.
	s := fakeService(true, nil, "backend")
	err := s.authorizeSelfApply(context.Background(), "vendor")
	if err == nil {
		t.Fatal("expected error when req.Caller does not match CN")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("expected same-CN mismatch error, got %v", err)
	}
}

func TestAuthorizeSelfApply_PerCNAllowlist(t *testing.T) {
	s := fakeService(false, []string{"ci-backend", "ci-vendor"}, "ci-backend")
	if err := s.authorizeSelfApply(context.Background(), "ci-backend"); err != nil {
		t.Fatalf("ci-backend should be allowed: %v", err)
	}
}

func TestAuthorizeSelfApply_RejectsCNNotOnAllowlist(t *testing.T) {
	s := fakeService(false, []string{"ci-backend"}, "backend")
	err := s.authorizeSelfApply(context.Background(), "backend")
	if err == nil {
		t.Fatal("expected error: backend not on allowlist")
	}
	if !strings.Contains(err.Error(), "not permitted to mutate") {
		t.Errorf("expected not-permitted error, got %v", err)
	}
}

func TestAuthorizeSelfApply_InsecureDevModeWildcardPermits(t *testing.T) {
	// No CN identity (insecure dev) + wildcard on → permit.
	s := &Service{
		allowApplyMutation: true,
		mutationAllowed:    map[string]bool{},
		callerFromContext:  func(context.Context) string { return "" },
	}
	if err := s.authorizeSelfApply(context.Background(), "anything"); err != nil {
		t.Fatalf("dev mode + wildcard should permit: %v", err)
	}
}

func TestAuthorizeOperator_AllowlistPermitsConsole(t *testing.T) {
	s := &Service{
		operatorAllowed:   map[string]bool{"atlantis-console": true},
		callerFromContext: func(context.Context) string { return "atlantis-console" },
	}
	if err := s.authorizeOperator(context.Background()); err != nil {
		t.Fatalf("console should be permitted: %v", err)
	}
}

func TestAuthorizeOperator_AllowlistRejectsOtherCN(t *testing.T) {
	// Even a caller on the apply allowlist isn't an operator unless
	// they're separately on the operator list.
	s := &Service{
		mutationAllowed:   map[string]bool{"ci-backend": true},
		operatorAllowed:   map[string]bool{"atlantis-console": true},
		callerFromContext: func(context.Context) string { return "ci-backend" },
	}
	err := s.authorizeOperator(context.Background())
	if err == nil {
		t.Fatal("apply-allowed CN should not be an operator")
	}
}

func TestAuthorizeOperator_EmptyAllowlistFallsBackToWildcard(t *testing.T) {
	// Backward compat: empty operatorAllowed + wildcard on → permit.
	s := &Service{
		allowApplyMutation: true,
		callerFromContext:  func(context.Context) string { return "anything" },
	}
	if err := s.authorizeOperator(context.Background()); err != nil {
		t.Fatalf("legacy wildcard should still permit: %v", err)
	}
}

func TestAuthorizeOperator_EmptyAllowlistAndNoWildcardRejects(t *testing.T) {
	s := &Service{
		callerFromContext: func(context.Context) string { return "anything" },
	}
	err := s.authorizeOperator(context.Background())
	if err == nil {
		t.Fatal("expected reject: nothing grants operator permission")
	}
}

func TestAuthorizeSelfApply_InsecureDevModeWithoutWildcardRejects(t *testing.T) {
	// No CN identity + no wildcard + empty allowlist → reject.
	s := &Service{
		allowApplyMutation: false,
		mutationAllowed:    map[string]bool{},
		callerFromContext:  func(context.Context) string { return "" },
	}
	err := s.authorizeSelfApply(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected reject when nothing grants mutation permission")
	}
}

func TestParseSubmitted_ProducesFiles(t *testing.T) {
	files := []SubmittedFile{
		{Path: "a.pc", Content: []byte(`entity A in x { id bigint primary }`)},
		{Path: "b.pc", Content: []byte(`entity B in x { id bigint primary }`)},
	}
	parsed, errs := parseSubmitted("caller-1", files)
	if len(errs) != 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if len(parsed) != 2 {
		t.Errorf("expected 2 parsed files, got %d", len(parsed))
	}
	// Path is prefixed with caller so cross-caller error attribution works.
	if !strings.HasPrefix(parsed[0].Path, "caller-1:") {
		t.Errorf("expected caller prefix on path, got %q", parsed[0].Path)
	}
}

func TestParseSubmitted_SurfacesErrors(t *testing.T) {
	files := []SubmittedFile{
		{Path: "good.pc", Content: []byte(`entity A in x { id bigint primary }`)},
		{Path: "bad.pc", Content: []byte(`entity { definitely not a parseable .atl file }`)},
	}
	_, errs := parseSubmitted("caller-1", files)
	if len(errs) == 0 {
		t.Fatalf("expected at least one parse error")
	}
	if !strings.Contains(errs[0], "bad.pc") {
		t.Errorf("error should name the offending file, got %q", errs[0])
	}
}

func TestComputePlanID_StableForSameInput(t *testing.T) {
	files := []*dsl.File{
		{Path: "caller-1:a.pc"},
		{Path: "caller-1:b.pc"},
	}
	id1 := computePlanID("caller-1", files, nil)
	id2 := computePlanID("caller-1", files, nil)
	if id1 != id2 {
		t.Errorf("PlanID should be deterministic: %q != %q", id1, id2)
	}
}

func TestComputePlanID_ChangesWithCheckpoint(t *testing.T) {
	files := []*dsl.File{{Path: "caller-1:a.pc"}}
	idA := computePlanID("caller-1", files, nil)
	idB := computePlanID("caller-1", files, &dsl.IR{Version: 1})
	if idA == idB {
		t.Errorf("PlanID should change when checkpoint changes; both = %q", idA)
	}
}

func TestComputePlanID_StableUnderFileReorder(t *testing.T) {
	f1 := []*dsl.File{{Path: "caller-1:a.pc"}, {Path: "caller-1:b.pc"}}
	f2 := []*dsl.File{{Path: "caller-1:b.pc"}, {Path: "caller-1:a.pc"}}
	if computePlanID("caller-1", f1, nil) != computePlanID("caller-1", f2, nil) {
		t.Errorf("PlanID should be invariant under file order; reordering changed it")
	}
}

func TestComputePlanID_DiffersAcrossCallers(t *testing.T) {
	files := []*dsl.File{{Path: "caller:a.pc"}}
	idA := computePlanID("caller-A", files, nil)
	idB := computePlanID("caller-B", files, nil)
	if idA == idB {
		t.Errorf("different callers should produce different PlanIDs; got %q for both", idA)
	}
}

// Merged-schema version is the cache-key that lets `tide pull` short-circuit
// when nothing has changed. Three invariants pin its behavior so a quiet
// regression — say, switching the byte separator — can't slip past CI.

func TestComputeMergedSchemaVersion_StableForIdenticalInput(t *testing.T) {
	entries := []mergedEntry{
		{caller: "backend", path: "auth/schema.pc", content: "entity A in x {}"},
		{caller: "vendor-platform", path: "catalog/schema.pc", content: "entity B in x {}"},
	}
	v1 := computeMergedSchemaVersion(entries)
	v2 := computeMergedSchemaVersion(entries)
	if v1 != v2 {
		t.Errorf("version is not deterministic across calls: %s vs %s", v1, v2)
	}
}

func TestComputeMergedSchemaVersion_ShiftsOnContentChange(t *testing.T) {
	base := []mergedEntry{{caller: "x", path: "p.pc", content: "alpha"}}
	bumped := []mergedEntry{{caller: "x", path: "p.pc", content: "beta"}}
	if computeMergedSchemaVersion(base) == computeMergedSchemaVersion(bumped) {
		t.Errorf("content change must shift the version (otherwise tide pull would never refresh)")
	}
}

// Length-prefix-free encodings can collide when values contain the
// separator. We use NUL bytes to avoid that — pin it.
func TestComputeMergedSchemaVersion_FieldBoundariesDoNotCollide(t *testing.T) {
	a := []mergedEntry{{caller: "ab", path: "cd", content: "ef"}}
	// Splice in a way that would collide if we joined fields without a
	// terminator byte: (caller="a", path="bcd", content="ef").
	b := []mergedEntry{{caller: "a", path: "bcd", content: "ef"}}
	if computeMergedSchemaVersion(a) == computeMergedSchemaVersion(b) {
		t.Errorf("field boundaries must be preserved in the hash; got collision")
	}
}

func TestTranslateClass(t *testing.T) {
	cases := map[codegen.ChangeClass]ClassName{
		codegen.ClassAdditive:            ClassAdditive,
		codegen.ClassBackfillRequired:    ClassBackfill,
		codegen.ClassCrossCallerBreaking: ClassBreaking,
	}
	for in, want := range cases {
		if got := translateClass(in); got != want {
			t.Errorf("translateClass(%v) = %s want %s", in, got, want)
		}
	}
}

func TestImpactReport_IncludesPlanCaller(t *testing.T) {
	d := &codegen.Diff{
		Additive: []codegen.Change{
			{Kind: codegen.KindEntityAdded, EntityID: "x.A", Detail: "added"},
		},
	}
	rep := buildImpactReport("caller-1", nil, d, nil)
	if len(rep) != 1 {
		t.Fatalf("want 1 entry, got %d", len(rep))
	}
	if rep[0].Caller != "caller-1" {
		t.Errorf("expected planning caller in report, got %q", rep[0].Caller)
	}
	if !rep[0].Affected {
		t.Errorf("planning caller should always be marked affected")
	}
}

func TestImpactReport_SortedByCaller(t *testing.T) {
	d := &codegen.Diff{
		Additive: []codegen.Change{
			{Kind: codegen.KindEntityAdded, EntityID: "x.A", Detail: "added"},
		},
	}
	others := []*dsl.File{
		{Path: "zeta-caller:a.pc"},
		{Path: "alpha-caller:a.pc"},
		{Path: "mu-caller:a.pc"},
	}
	rep := buildImpactReport("planning-caller", others, d, nil)
	for i := 1; i < len(rep); i++ {
		if rep[i-1].Caller > rep[i].Caller {
			t.Errorf("report not sorted: %s > %s", rep[i-1].Caller, rep[i].Caller)
		}
	}
}

func TestIndexOf(t *testing.T) {
	cases := []struct {
		s    string
		c    byte
		want int
	}{
		{"hello", 'l', 2},
		{"hello", 'z', -1},
		{"", 'a', -1},
		{"a:b:c", ':', 1},
	}
	for _, c := range cases {
		if got := indexOf(c.s, c.c); got != c.want {
			t.Errorf("indexOf(%q, %c) = %d want %d", c.s, c.c, got, c.want)
		}
	}
}

// ---- buildEntityOwnership tests ----

func TestBuildEntityOwnership_AssignsCallerCorrectly(t *testing.T) {
	callerFiles := parseSrc(t, "vendor", `entity Product in vendor { id bigint primary }`)
	otherFiles := parseSrc(t, "consumer", `entity Account in consumer { id bigint primary }`)
	own := buildEntityOwnership("vendor", callerFiles, otherFiles)

	if own["vendor.Product"] != "vendor" {
		t.Errorf("vendor.Product should be owned by vendor, got %q", own["vendor.Product"])
	}
	if own["consumer.Account"] != "consumer" {
		t.Errorf("consumer.Account should be owned by consumer, got %q", own["consumer.Account"])
	}
}

// ---- buildCrossCallerRefs tests ----

func TestBuildCrossCallerRefs_DetectsFK(t *testing.T) {
	otherFiles := parseSrc(t, "consumer", `
entity Account in consumer {
  id bigint primary
  product_id bigint references vendor.Product.id
}
`)
	refs := buildCrossCallerRefs(otherFiles)

	if !refs["vendor.Product"] {
		t.Error("expected vendor.Product in cross-caller refs (entity-level)")
	}
	if !refs["vendor.Product.id"] {
		t.Error("expected vendor.Product.id in cross-caller refs (field-level)")
	}
}

func TestBuildCrossCallerRefs_EmptyWhenNoRefs(t *testing.T) {
	otherFiles := parseSrc(t, "consumer", `entity Account in consumer { id bigint primary }`)
	refs := buildCrossCallerRefs(otherFiles)
	if len(refs) != 0 {
		t.Errorf("expected empty refs, got %v", refs)
	}
}

// ---- impact report fix test ----

func TestImpactReport_OnlyAffectedCallersMarked(t *testing.T) {
	d := &codegen.Diff{
		Additive: []codegen.Change{
			{Kind: codegen.KindFieldAdded, EntityID: "vendor.Product", Detail: "added"},
		},
	}
	// consumer declares consumer.Account, not vendor.Product
	others := parseSrc(t, "consumer", `entity Account in consumer { id bigint primary }`)
	rep := buildImpactReport("vendor", others, d, nil)

	for _, entry := range rep {
		if entry.Caller == "consumer" && entry.Affected {
			t.Error("consumer should NOT be marked affected — the diff only touches vendor.Product")
		}
	}
}

func TestImpactReport_AffectedCallerMarked(t *testing.T) {
	d := &codegen.Diff{
		Additive: []codegen.Change{
			{Kind: codegen.KindFieldAdded, EntityID: "vendor.Product", Detail: "added"},
		},
	}
	// other caller also declares vendor.Product
	others := parseSrc(t, "other", `entity Product in vendor { id bigint primary  name text }`)
	rep := buildImpactReport("submitter", others, d, nil)

	found := false
	for _, entry := range rep {
		if entry.Caller == "other" {
			found = true
			if !entry.Affected {
				t.Error("other caller who declares vendor.Product should be marked affected")
			}
		}
	}
	if !found {
		t.Error("other caller should appear in the impact report")
	}
}

// parseSrc is a test helper that parses a single .atl source string as if
// submitted by the given caller, returning the parsed file slice.
func parseSrc(t *testing.T, caller, src string) []*dsl.File {
	t.Helper()
	f, err := dsl.Parse(caller+":test.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse %s: %v", caller, err)
	}
	return []*dsl.File{f}
}

// Full Plan / Apply paths require Postgres and are covered in the
// integration harness (task #25). The pure-Go pieces above pin the
// classification, ID stability, and impact-report shape — the parts that
// would silently break if a refactor regresses them.

func TestValidateCustomSQL_EmptyIR(t *testing.T) {
	if msgs := validateCustomSQL(&dsl.IR{}); len(msgs) != 0 {
		t.Errorf("empty IR should produce no errors, got %v", msgs)
	}
}

func TestValidateCustomSQL_HappyPath(t *testing.T) {
	src := `
entity Account in consumer {
  id          bigint primary
  consumer_id text not null
}

query OutfitsForConsumer for Account {
  input { id: bigint }
  output as Account
  sql touches(Account) {
    SELECT id, consumer_id FROM consumer_account WHERE id = $id
  }
}
`
	f, err := dsl.Parse("t.pc", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if msgs := validateCustomSQL(ir); len(msgs) != 0 {
		t.Errorf("expected no errors, got: %v", msgs)
	}
}

func TestValidateCustomSQL_SurfacesPGErrors(t *testing.T) {
	// A query that lowers cleanly (every $arg is declared, touches()
	// resolves) but whose SQL references a nonexistent table can only
	// be caught by the pg_query_go pass. This test pins that wiring.
	src := `
entity Account in consumer {
  id          bigint primary
  consumer_id text not null
}

query BadTable for Account {
  input { id: bigint }
  output as Account
  sql touches(Account) {
    SELECT id FROM consumer_widget WHERE id = $id
  }
}
`
	f, err := dsl.Parse("t.pc", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	msgs := validateCustomSQL(ir)
	if len(msgs) == 0 {
		t.Fatal("expected at least one error for unknown table")
	}
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "consumer_widget") {
		t.Errorf("error should mention the bad table; got: %s", joined)
	}
}
