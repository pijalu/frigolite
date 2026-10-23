// SPDX-License-Identifier: GPL-3.0-or-later

package tcl

import "testing"

// TestLSortOfQueryRecordsTheTransform pins that a sort applied to a query's
// rows is recorded in the fixture.
//
// The rows of a query only exist at run time, so the converter cannot
// evaluate the sort; it records it and the harness applies it to the rows it
// renders. where7.test's count_steps_sort helper is the motivating case:
//
//	proc count_steps_sort {sql} {
//	  set r [lsort -integer [db eval $sql]]
//	  return "$r scan [db status step] sort [db status sort]"
//	}
func TestLSortOfQueryRecordsTheTransform(t *testing.T) {
	i := NewInterp()
	script := `proc count_steps_sort {sql} {
  set r [lsort -integer [db eval $sql]]
  return "$r scan [db status step] sort [db status sort]"
}
do_test where7-2.2.1 { count_steps_sort {SELECT a FROM t2} } {1 2 3 scan 0 sort 0}`
	if err := i.Execute(script); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 {
		t.Fatalf("captured %d statements, want 1: %#v", len(stmts), stmts)
	}
	if stmts[0].Type != "query" || stmts[0].SQL != "SELECT a FROM t2" {
		t.Fatalf("captured statement = %#v, want the query", stmts[0])
	}
	if stmts[0].ResultTransform != "lsort:integer" {
		t.Fatalf("ResultTransform = %q, want %q", stmts[0].ResultTransform, "lsort:integer")
	}
	if !stmts[0].StmtStatus {
		t.Fatalf("StmtStatus = false, want true (the helper reads `db status step`)")
	}
}

// TestLSortOfQueryThroughVariableRecordsTheTransform pins that the sort is
// recorded when the query's rows travel through a variable first
// (`set r [db eval $sql]` then `lsort -integer $r`).
func TestLSortOfQueryThroughVariableRecordsTheTransform(t *testing.T) {
	i := NewInterp()
	script := `do_test x-1.1 {
  set r [db eval {SELECT a FROM t2}]
  lsort -integer $r
} {1 2 3}`
	if err := i.Execute(script); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 {
		t.Fatalf("captured %d statements, want 1: %#v", len(stmts), stmts)
	}
	if stmts[0].ResultTransform != "lsort:integer" {
		t.Fatalf("ResultTransform = %q, want %q", stmts[0].ResultTransform, "lsort:integer")
	}
}

// TestRuntimeValueNeverLeaksIntoAnExpectation pins that the internal marker
// of a runtime query value never reaches an expectation: the value of a
// query is only known at run time, so the expectation stays empty (the step
// is checked by the harness against the rendered rows).
func TestRuntimeValueNeverLeaksIntoAnExpectation(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_test x-1.2 { set r [db eval {SELECT a FROM t2}] } $r`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	for _, st := range stmts {
		if st.Expected != "" {
			t.Fatalf("expectation %q leaked the runtime marker", st.Expected)
		}
	}
}

// TestRuntimeValueNeverLeaksIntoSQL pins that the marker never reaches a SQL
// statement (the value of a query is bound into a later statement's text).
func TestRuntimeValueNeverLeaksIntoSQL(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set r [db eval {SELECT a FROM t2}]
db eval {SELECT $r}`); err != nil {
		t.Fatal(err)
	}
	for _, st := range i.Stmts() {
		if containsRuntimeMarker(st.SQL) {
			t.Fatalf("SQL %q leaked the runtime marker", st.SQL)
		}
	}
}

// containsRuntimeMarker reports whether a string carries a runtime marker.
func containsRuntimeMarker(s string) bool {
	return len(s) != len(stripRuntimeValues(s))
}
