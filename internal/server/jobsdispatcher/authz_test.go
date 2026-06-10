package jobsdispatcher

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func newTestIR() *dsl.IR {
	return &dsl.IR{
		Jobs: []dsl.Job{
			{Namespace: "vendor", Name: "ShopifyImport", VisibleTo: "vendor"},
			{Namespace: "consumer", Name: "SweepExpired", VisibleTo: "*"},
			{Namespace: "shop", Name: "Reconcile"}, // empty visible_to == permissive
		},
	}
}

func TestCheckWorkerAuthz_MatchingCaller(t *testing.T) {
	err := CheckWorkerAuthz("vendor", nil, []string{"vendor.ShopifyImport"}, newTestIR())
	if err != nil {
		t.Errorf("vendor handling its own job should pass: %v", err)
	}
}

func TestCheckWorkerAuthz_WildcardVisibleTo(t *testing.T) {
	err := CheckWorkerAuthz("backstage", nil, []string{"consumer.SweepExpired"}, newTestIR())
	if err != nil {
		t.Errorf("wildcard visible_to should pass for any caller: %v", err)
	}
}

func TestCheckWorkerAuthz_EmptyVisibleTo(t *testing.T) {
	err := CheckWorkerAuthz("anyone", nil, []string{"shop.Reconcile"}, newTestIR())
	if err != nil {
		t.Errorf("empty visible_to should pass for any caller: %v", err)
	}
}

func TestCheckWorkerAuthz_MismatchedCaller(t *testing.T) {
	err := CheckWorkerAuthz("backstage", nil, []string{"vendor.ShopifyImport"}, newTestIR())
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", status.Code(err))
	}
}

func TestCheckWorkerAuthz_AliasMatchesVisibleTo(t *testing.T) {
	// The PostgreSQL-roles / AD-SID pattern: a cert CN "backstage" with
	// alias "vendor" should satisfy visible_to="vendor". Schema doesn't
	// know about the identity rename; aliases bridge it.
	err := CheckWorkerAuthz("backstage", []string{"vendor"}, []string{"vendor.ShopifyImport"}, newTestIR())
	if err != nil {
		t.Errorf("alias should match visible_to: %v", err)
	}
}

func TestCheckWorkerAuthz_AliasUnmatchedStillRejected(t *testing.T) {
	// Alias is set but it doesn't match visible_to. Should still reject.
	err := CheckWorkerAuthz("backstage", []string{"other-alias"}, []string{"vendor.ShopifyImport"}, newTestIR())
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("non-matching alias should still reject, got %v", status.Code(err))
	}
}

func TestCheckWorkerAuthz_MultipleAliasesAnyMatches(t *testing.T) {
	// Multiple aliases; any one matching is sufficient.
	err := CheckWorkerAuthz("backstage",
		[]string{"old-name-1", "vendor", "old-name-2"},
		[]string{"vendor.ShopifyImport"}, newTestIR())
	if err != nil {
		t.Errorf("any matching alias should pass: %v", err)
	}
}

func TestCheckWorkerAuthz_UnknownJob(t *testing.T) {
	err := CheckWorkerAuthz("vendor", nil, []string{"vendor.Nonexistent"}, newTestIR())
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound, got %v", status.Code(err))
	}
}

func TestCheckWorkerAuthz_MixedJobsRejectFirstMismatch(t *testing.T) {
	err := CheckWorkerAuthz("vendor", nil, []string{"vendor.ShopifyImport", "consumer.SweepExpired"}, newTestIR())
	if err != nil {
		t.Errorf("vendor + wildcard should pass: %v", err)
	}
	err = CheckWorkerAuthz("vendor", nil, []string{"vendor.ShopifyImport", "shop.OtherUnknown"}, newTestIR())
	if err == nil {
		t.Fatal("expected error for mixed valid+invalid jobs")
	}
}

func TestCheckWorkerAuthz_NilIR(t *testing.T) {
	err := CheckWorkerAuthz("vendor", nil, []string{"any"}, nil)
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition for nil IR, got %v", status.Code(err))
	}
}

func TestCheckSingleAuthz_Wraps(t *testing.T) {
	err := CheckSingleAuthz("backstage", nil, "vendor.ShopifyImport", newTestIR())
	if err == nil {
		t.Fatal("expected error for caller mismatch")
	}
	if !strings.Contains(err.Error(), "backstage") {
		t.Errorf("error should name the caller: %v", err)
	}
}

func TestCheckSingleAuthz_AliasMatches(t *testing.T) {
	// Dispatch-time alias check. Used as the defense-in-depth re-check
	// after Open; a session that opened with alias [vendor] still
	// satisfies subsequent visible_to="vendor" dispatches.
	err := CheckSingleAuthz("backstage", []string{"vendor"}, "vendor.ShopifyImport", newTestIR())
	if err != nil {
		t.Errorf("alias should match at dispatch time: %v", err)
	}
}
