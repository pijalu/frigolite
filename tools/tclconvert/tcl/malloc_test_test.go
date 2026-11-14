package tcl

import (
	"strings"
	"testing"
)

// do_malloc_test's every trial deletes test.db* and opens an empty database,
// so the conversion must lead with a reset_db marker; the SQL body runs once
// (the no-fault trial) and the -tclprep/-cleanup scripts run through the
// normal command paths (fts3expr-malloc-2's parentheses toggles).
func TestDoMallocTestEmitsResetAndBody(t *testing.T) {
	i := NewInterp()
	src := `do_malloc_test malloc-1 -sqlbody {
  SELECT fts3_exprtest('simple', 'a b c "d e f"', 'a', 'b', 'c')
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 {
		t.Fatalf("stmts = %d, want 2", len(stmts))
	}
	if stmts[0].Type != "reset_db" {
		t.Fatalf("first stmt = %s, want reset_db", stmts[0].Type)
	}
	if stmts[1].Type != "query" || stmts[1].TestName != "malloc-1" ||
		strings.TrimSpace(stmts[1].SQL) != "SELECT fts3_exprtest('simple', 'a b c \"d e f\"', 'a', 'b', 'c')" {
		t.Fatalf("body stmt = %#v", stmts[1])
	}
}

// The -tclprep/-cleanup scripts execute through the interpreter: the
// sqlite_fts3_enable_parentheses set emits set_fts3_parens steps around the
// body, in order.
func TestDoMallocTestPrepAndCleanup(t *testing.T) {
	i := NewInterp()
	src := `do_malloc_test malloc-2 -tclprep {
  set sqlite_fts3_enable_parentheses 0
} -sqlbody {
  SELECT fts3_exprtest('simple', 'a -b', 'a', 'b', 'c')
} -cleanup {
  set sqlite_fts3_enable_parentheses 1
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	want := []string{"reset_db", "set_fts3_parens", "query", "set_fts3_parens"}
	if len(stmts) != len(want) {
		t.Fatalf("stmts = %d, want %d (%v)", len(stmts), len(want), stmts)
	}
	for k, typ := range want {
		if stmts[k].Type != typ {
			t.Fatalf("stmt[%d] = %s, want %s", k, stmts[k].Type, typ)
		}
	}
	if stmts[1].SQL != "0" || stmts[3].SQL != "1" {
		t.Fatalf("paren toggles = %q %q", stmts[1].SQL, stmts[3].SQL)
	}
}

// An unknown option must fail loudly (never silently skipped).
func TestDoMallocTestUnknownOptionErrors(t *testing.T) {
	i := NewInterp()
	err := i.Execute(`do_malloc_test m -nosuchopt {x} -sqlbody {SELECT 1}`)
	if err == nil {
		t.Fatal("unknown do_malloc_test option did not error")
	}
}
