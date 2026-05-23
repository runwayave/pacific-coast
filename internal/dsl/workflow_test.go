package dsl

import (
	"strings"
	"testing"
)

func TestParse_Workflow_Basic(t *testing.T) {
	f := mustParse(t, `
job ShopifyImport in vendor { args { vendor_id varchar(7) not null } }
job CleanupIntegration in vendor { args { vendor_id varchar(7) not null } }

workflow OnboardVendor in vendor {
  state {
    vendor_id varchar(7) not null
  }
  step setup_integration {
    job ShopifyImport
    args { vendor_id: $vendor_id }
  }
  compensate setup_integration {
    job CleanupIntegration
    args { vendor_id: $vendor_id }
  }
}
`)
	var wf *WorkflowDecl
	for _, d := range f.Decls {
		if w, ok := d.(*WorkflowDecl); ok {
			wf = w
			break
		}
	}
	if wf == nil {
		t.Fatalf("expected WorkflowDecl")
	}
	if wf.Name != "OnboardVendor" || wf.Namespace != "vendor" {
		t.Errorf("name/ns = %q/%q", wf.Name, wf.Namespace)
	}
	if len(wf.State) != 1 || wf.State[0].Name != "vendor_id" {
		t.Errorf("state = %+v", wf.State)
	}
	if len(wf.Steps) != 1 || wf.Steps[0].Name != "setup_integration" {
		t.Errorf("steps = %+v", wf.Steps)
	}
	if len(wf.Compensations) != 1 || wf.Compensations[0].StepName != "setup_integration" {
		t.Errorf("compensations = %+v", wf.Compensations)
	}
}

func TestLower_Workflow_Resolves(t *testing.T) {
	f := mustParse(t, `
job ShopifyImport in vendor { args { vendor_id varchar(7) not null } }
job Cleanup in vendor { args { vendor_id varchar(7) not null } }

workflow Onboard in vendor {
  state { vendor_id varchar(7) not null }
  step import {
    job ShopifyImport
    args { vendor_id: $vendor_id }
  }
  compensate import {
    job Cleanup
    args { vendor_id: $vendor_id }
  }
}
`)
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(ir.Workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(ir.Workflows))
	}
	wf := ir.Workflows[0]
	if wf.ID() != "vendor.Onboard" {
		t.Errorf("ID = %s", wf.ID())
	}
	if len(wf.Steps) != 1 || wf.Steps[0].TargetJobID != "vendor.ShopifyImport" {
		t.Errorf("steps = %+v", wf.Steps)
	}
	if len(wf.Compensations) != 1 || wf.Compensations[0].TargetJobID != "vendor.Cleanup" {
		t.Errorf("compensations = %+v", wf.Compensations)
	}
}

func TestLower_Workflow_RejectsUnknownJob(t *testing.T) {
	f := mustParse(t, `
workflow W in vendor {
  state { x int not null }
  step s1 {
    job DoesNotExist
    args { x: $x }
  }
}
`)
	_, err := Lower([]*File{f})
	if err == nil || !strings.Contains(err.Error(), "unknown job vendor.DoesNotExist") {
		t.Fatalf("expected unknown job error, got: %v", err)
	}
}

func TestLower_Workflow_RejectsDuplicateStep(t *testing.T) {
	f := mustParse(t, `
job J in vendor { args { x int not null } }
workflow W in vendor {
  state { x int not null }
  step dup { job J args { x: $x } }
  step dup { job J args { x: $x } }
}
`)
	_, err := Lower([]*File{f})
	if err == nil || !strings.Contains(err.Error(), "duplicate step name") {
		t.Fatalf("expected duplicate step error, got: %v", err)
	}
}

func TestLower_Workflow_RejectsUnknownCompStep(t *testing.T) {
	f := mustParse(t, `
job J in vendor { args { x int not null } }
workflow W in vendor {
  state { x int not null }
  step real { job J args { x: $x } }
  compensate fake { job J args { x: $x } }
}
`)
	_, err := Lower([]*File{f})
	if err == nil || !strings.Contains(err.Error(), "unknown step") {
		t.Fatalf("expected unknown step error, got: %v", err)
	}
}
