package sqlvalidate

import (
	"strings"
	"testing"
)

func cols(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestBackfill_Concat_OK(t *testing.T) {
	err := ValidateBackfillExpression(
		`first_name || ' ' || last_name`,
		cols("first_name", "last_name"),
		"display_name",
	)
	if err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

func TestBackfill_Coalesce_OK(t *testing.T) {
	err := ValidateBackfillExpression(
		`coalesce(first_name, 'unknown')`,
		cols("first_name"),
		"display_name",
	)
	if err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

func TestBackfill_TypeCast_OK(t *testing.T) {
	err := ValidateBackfillExpression(
		`text_col::int + 1`,
		cols("text_col"),
		"int_col",
	)
	if err != nil {
		t.Errorf("type cast should be allowed: %v", err)
	}
}

func TestBackfill_CaseExpression_OK(t *testing.T) {
	err := ValidateBackfillExpression(
		`case when active then 'yes' else 'no' end`,
		cols("active"),
		"label",
	)
	if err != nil {
		t.Errorf("case expression should be allowed: %v", err)
	}
}

func TestBackfill_EmptyExpr_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(``, cols(), "x")
	if err == nil {
		t.Errorf("empty expression should be rejected")
	}
}

func TestBackfill_ParseFailure_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(`!!! not sql !!!`, cols(), "x")
	if err == nil {
		t.Errorf("garbage SQL should be rejected at parse time")
	}
}

func TestBackfill_Subquery_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`(SELECT password FROM secrets WHERE id = 1)`,
		cols("password"),
		"derived",
	)
	if err == nil || !strings.Contains(err.Error(), "subquer") {
		t.Errorf("subquery should be rejected with a subquery error, got %v", err)
	}
}

func TestBackfill_Nextval_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`nextval('users_id_seq')`,
		cols(),
		"id",
	)
	if err == nil || !strings.Contains(err.Error(), "nextval") {
		t.Errorf("nextval should be rejected by whitelist, got %v", err)
	}
}

func TestBackfill_CurrentSetting_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`current_setting('app.something')`,
		cols(),
		"x",
	)
	if err == nil || !strings.Contains(err.Error(), "current_setting") {
		t.Errorf("current_setting should be rejected, got %v", err)
	}
}

func TestBackfill_PgReadFile_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`pg_read_file('/etc/passwd')`,
		cols(),
		"x",
	)
	if err == nil || !strings.Contains(err.Error(), "pg_read_file") {
		t.Errorf("pg_read_file should be rejected, got %v", err)
	}
}

func TestBackfill_UnknownColumn_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`first_name || ' ' || last_nam`,
		cols("first_name", "last_name"),
		"display_name",
	)
	if err == nil || !strings.Contains(err.Error(), "last_nam") {
		t.Errorf("unknown column should be rejected, got %v", err)
	}
}

func TestBackfill_SelfReference_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`display_name || '!'`,
		cols("display_name"),
		"display_name",
	)
	if err == nil || !strings.Contains(err.Error(), "display_name") {
		t.Errorf("self-reference should be rejected, got %v", err)
	}
}

func TestBackfill_NestedFuncCall_DeepCheck(t *testing.T) {
	err := ValidateBackfillExpression(
		`coalesce(lower(first_name), 'x')`,
		cols("first_name"),
		"display_name",
	)
	if err != nil {
		t.Errorf("nested whitelisted calls should be ok: %v", err)
	}
}

func TestBackfill_NestedUnsafeFuncCall_Rejected(t *testing.T) {
	// Outer call is whitelisted but the nested one isn't — must still be rejected.
	err := ValidateBackfillExpression(
		`coalesce(nextval('s'), 0)`,
		cols(),
		"id",
	)
	if err == nil || !strings.Contains(err.Error(), "nextval") {
		t.Errorf("nested unsafe call should be rejected, got %v", err)
	}
}

func TestBackfill_NullTestAndArithmetic_OK(t *testing.T) {
	err := ValidateBackfillExpression(
		`case when a is null then 0 else a + b end`,
		cols("a", "b"),
		"sum_or_zero",
	)
	if err != nil {
		t.Errorf("null test + arithmetic should be ok: %v", err)
	}
}
