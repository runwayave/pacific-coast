package dsl

import (
	"strings"
	"testing"
)

func TestParse_Job_Minimal(t *testing.T) {
	f := mustParse(t, `
job ShopifyImport in vendor {
  args {
    vendor_id varchar(7) not null
  }
}
`)
	if len(f.Decls) != 1 {
		t.Fatalf("expected 1 decl, got %d", len(f.Decls))
	}
	j, ok := f.Decls[0].(*JobDecl)
	if !ok {
		t.Fatalf("expected *JobDecl, got %T", f.Decls[0])
	}
	if j.Name != "ShopifyImport" || j.Namespace != "vendor" {
		t.Fatalf("name/ns = %q/%q", j.Name, j.Namespace)
	}
	if len(j.Args) != 1 || j.Args[0].Name != "vendor_id" {
		t.Fatalf("args = %+v", j.Args)
	}
}

func TestParse_Job_FullModifiers(t *testing.T) {
	f := mustParse(t, `
job ShopifyImport in vendor {
  args {
    vendor_id varchar(7) not null
    import_strategy varchar(20) not null default "skip"
  }
  retries 3
  timeout 30m
  heartbeat 10m
  queue "shopify"
  schedule "0 */15 * * *"
}
`)
	j := f.Decls[0].(*JobDecl)
	if j.Retries == nil || j.Retries.Count != 3 {
		t.Fatalf("retries = %+v", j.Retries)
	}
	if j.Timeout == nil || j.Timeout.Duration != "30m" {
		t.Fatalf("timeout = %+v", j.Timeout)
	}
	if j.Heartbeat == nil || j.Heartbeat.Duration != "10m" {
		t.Fatalf("heartbeat = %+v", j.Heartbeat)
	}
	if j.Queue == nil || j.Queue.Name != "shopify" {
		t.Fatalf("queue = %+v", j.Queue)
	}
	if j.Schedule == nil || j.Schedule.CronSpec != "0 */15 * * *" {
		t.Fatalf("schedule = %+v", j.Schedule)
	}
	if len(j.Args) != 2 {
		t.Fatalf("args = %+v", j.Args)
	}
}

func TestParse_Job_HeartbeatLowersToMS(t *testing.T) {
	f := mustParse(t, `
job LongHandler in vendor {
  args { x int }
  heartbeat 10m
}
`)
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if len(ir.Jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(ir.Jobs))
	}
	got := ir.Jobs[0].HeartbeatMS
	want := 10 * 60 * 1000 // 10 minutes in ms
	if got != want {
		t.Errorf("HeartbeatMS = %d, want %d", got, want)
	}
}

func TestParse_Job_HeartbeatRejectsInvalid(t *testing.T) {
	f := mustParse(t, `
job Bad in vendor {
  args { x int }
  heartbeat 0s
}
`)
	_, err := Lower([]*File{f})
	if err == nil {
		t.Fatal("expected lowering error for heartbeat 0s")
	}
}

func TestParse_Job_ModifiersInAnyOrder(t *testing.T) {
	f := mustParse(t, `
job J in v {
  queue "q"
  retries 1
  args {
    x int not null
  }
  timeout 5s
}
`)
	j := f.Decls[0].(*JobDecl)
	if j.Retries == nil || j.Timeout == nil || j.Queue == nil {
		t.Fatalf("expected all modifiers populated: %+v", j)
	}
	if len(j.Args) != 1 {
		t.Fatalf("args = %+v", j.Args)
	}
}

func TestLower_Job_Minimal(t *testing.T) {
	f := mustParse(t, `
job ShopifyImport in vendor {
  args {
    vendor_id varchar(7) not null
  }
  retries 3
  timeout 30m
  queue "shopify"
}
`)
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(ir.Jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(ir.Jobs))
	}
	j := ir.Jobs[0]
	if j.ID() != "vendor.ShopifyImport" {
		t.Fatalf("ID = %s", j.ID())
	}
	if j.Retries != 3 {
		t.Fatalf("retries = %d", j.Retries)
	}
	// 30m = 30 * 60 * 1000 = 1_800_000 ms
	if j.TimeoutMS != 1_800_000 {
		t.Fatalf("timeout_ms = %d", j.TimeoutMS)
	}
	if j.Queue != "shopify" {
		t.Fatalf("queue = %s", j.Queue)
	}
	if len(j.Args) != 1 || j.Args[0].Name != "vendor_id" || !j.Args[0].NotNull {
		t.Fatalf("args = %+v", j.Args)
	}
}

func TestLower_Job_RejectsStorageModifiersOnArgs(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "primary",
			src:  `job J in v { args { id bigint primary } }`,
			want: "cannot be 'primary'",
		},
		{
			name: "serial",
			src:  `job J in v { args { id bigint serial } }`,
			want: "cannot be 'serial'",
		},
		{
			name: "identity",
			src:  `job J in v { args { id bigint identity } }`,
			want: "cannot be 'identity'",
		},
		{
			name: "unique",
			src:  `job J in v { args { id bigint unique } }`,
			want: "cannot be 'unique'",
		},
		{
			name: "references",
			src:  `entity E in v { id bigint primary }` + "\n" + `job J in v { args { id bigint references v.E.id } }`,
			want: "cannot use 'references'",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := Parse("t.pc", []byte(c.src))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, err = Lower([]*File{f})
			if err == nil {
				t.Fatalf("expected error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestLower_Job_RejectsNegativeRetries(t *testing.T) {
	// retries -1 — lexer treats - as part of an integer? Need to use a separate raw form.
	// The token kind is TokInt with negative not supported by the int literal lexer.
	// Use the AST directly to confirm validation when retries is negative.
	src := `job J in v { args { x int not null } retries 0 }`
	f := mustParse(t, src)
	jd := f.Decls[0].(*JobDecl)
	jd.Retries = &JobRetries{Pos: jd.Pos, Count: -5}
	_, err := Lower([]*File{f})
	if err == nil || !strings.Contains(err.Error(), "retries must be non-negative") {
		t.Fatalf("expected non-negative retries error, got: %v", err)
	}
}

func TestLower_Job_DuplicateNameAcrossFiles(t *testing.T) {
	f1 := mustParse(t, `job J in v { args { x int not null } }`)
	f2 := mustParse(t, `job J in v { args { y int not null } }`)
	_, err := Lower([]*File{f1, f2})
	if err == nil || !strings.Contains(err.Error(), "duplicate job v.J") {
		t.Fatalf("expected duplicate job error, got: %v", err)
	}
}

func TestLower_Job_CollidesWithEntityName(t *testing.T) {
	f := mustParse(t, `
entity Foo in v { id bigint primary }
job Foo in v { args { x int not null } }
`)
	_, err := Lower([]*File{f})
	if err == nil || !strings.Contains(err.Error(), "collides with entity") {
		t.Fatalf("expected entity-collision error, got: %v", err)
	}
}

func TestLower_Job_SameNameDifferentNamespaces(t *testing.T) {
	src := `
job Cleanup in v { args { x int not null } }
job Cleanup in c { args { y int not null } }
`
	f := mustParse(t, src)
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(ir.Jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(ir.Jobs))
	}
}

func TestParse_Job_RejectsUnknownModifier(t *testing.T) {
	err := mustParseErr(t, `job J in v { args { x int not null } cache { read_through ttl=10m } }`)
	if !strings.Contains(err.Error(), "expected 'args', 'retries', 'timeout', 'heartbeat', 'queue', 'schedule'") {
		t.Fatalf("error message lacks hint: %v", err)
	}
}
