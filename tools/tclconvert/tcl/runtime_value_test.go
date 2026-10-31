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

// TestDBStatusStandaloneBodyEmitsStep pins the where.test shape: `db status
// step` as a do_test body's own command must become a db_status step whose
// value the harness reads from the engine counters — it must NOT flag the
// previous test's statement with the counter suffix (that corrupted the
// earlier statement's rendering: where-1.1.4 gained " scan 99 sort 0").
func TestDBStatusStandaloneBodyEmitsStep(t *testing.T) {
	i := NewInterp()
	script := `do_test where-1.1.3 { db status step } {0}
do_test where-1.1.4 { db eval {SELECT x, y, w FROM t1 WHERE +w=10} } {3 121 10}
do_test where-1.1.5 { db status step } {99}
do_test where-1.1.6 { db status sort } {0}`
	if err := i.Execute(script); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 4 {
		t.Fatalf("captured %d statements, want 4: %#v", len(stmts), stmts)
	}
	for idx, want := range map[int]struct{ typ, sql, expect string }{
		0: {"db_status", "step", "0"},
		2: {"db_status", "step", "99"},
		3: {"db_status", "sort", "0"},
	} {
		s := stmts[idx]
		if s.Type != want.typ || s.SQL != want.sql || s.Expected != want.expect {
			t.Fatalf("statement %d = %#v, want type=%s sql=%s expect=%s", idx, s, want.typ, want.sql, want.expect)
		}
	}
	if stmts[1].StmtStatus {
		t.Fatalf("the plain query must not carry the counter flag: %#v", stmts[1])
	}
	if stmts[1].Expected != "3 121 10" {
		t.Fatalf("query expectation = %q, want the rows", stmts[1].Expected)
	}
}

// TestDBStatusInlineHelperStillFlagsQuery pins that the inline-helper shape
// (count_steps_sort) still flags the query of the SAME body and emits no
// db_status step.
func TestDBStatusInlineHelperStillFlagsQuery(t *testing.T) {
	i := NewInterp()
	script := `proc count_steps_sort {sql} {
  set r [lsort -integer [db eval $sql]]
  return "$r scan [db status step] sort [db status sort]"
}
do_test where7-2.2.1 { count_steps_sort {SELECT a FROM t2} } {1 2 3 scan 0 sort 0}
do_test where7-2.2.2 { db status step } {0}`
	if err := i.Execute(script); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 {
		t.Fatalf("captured %d statements, want 2: %#v", len(stmts), stmts)
	}
	if !stmts[0].StmtStatus {
		t.Fatalf("the helper's query must keep the counter flag: %#v", stmts[0])
	}
	if stmts[1].Type != "db_status" || stmts[1].SQL != "step" {
		t.Fatalf("standalone read = %#v, want a db_status step", stmts[1])
	}
}

// containsRuntimeMarker reports whether a string carries a runtime marker.
func containsRuntimeMarker(s string) bool {
	return len(s) != len(stripRuntimeValues(s))
}

// TestTrackedVarResetEmitsStep pins that a plain empty `set` on a
// callback-recorded global becomes a runtime_var_reset step: the harness's
// runtime list only grows (the callback appends per firing), so without the
// reset every later comparison sees the whole history
// (e_droptrigger 2.1.2's ::triggers_fired accumulated "temp.tr1" from 2.1.1).
func TestTrackedVarResetEmitsStep(t *testing.T) {
	i := NewInterp()
	script := `proc droptrigger_reopen_db {} {
  db close
  set ::triggers_fired [list]
  proc r {x} { lappend ::triggers_fired $x }
  db eval {CREATE TEMP TABLE t1(a, b); CREATE TRIGGER temp.tr1 BEFORE INSERT ON t1 BEGIN SELECT r('temp.tr1'); END}
}
do_test 2.1.1 { droptrigger_reopen_db; db eval {INSERT INTO t1 VALUES(1, 2)}; set ::triggers_fired } {temp.tr1}
do_test 2.1.2 { droptrigger_reopen_db; db eval {DROP TRIGGER tr1; INSERT INTO t1 VALUES(1, 2)}; set ::triggers_fired } {}`
	if err := i.Execute(script); err != nil {
		t.Fatal(err)
	}
	resets := 0
	for _, s := range i.Stmts() {
		if s.Type == "runtime_var_reset" {
			resets++
			if s.Variable != "triggers_fired" {
				t.Fatalf("reset variable = %q, want triggers_fired", s.Variable)
			}
		}
	}
	if resets == 0 {
		t.Fatalf("no runtime_var_reset emitted: %#v", i.Stmts())
	}
}

// TestDBStatusBareStyleAndSearchCount pins where8.test's helper shapes:
// execsql_status concatenates the counters bare (no "scan"/"sort" labels →
// statusStyle "bare") and execsql_status2 additionally resets and appends
// SQLITE_TEST's search counter (searchCount). The labeled helper
// (count_steps_sort, where7) keeps the default render.
func TestDBStatusBareStyleAndSearchCount(t *testing.T) {
	i := NewInterp()
	script := `proc execsql_status {sql {db db}} {
  set result [uplevel $db eval [list $sql]]
  concat $result [db status step] [db status sort]
}
proc execsql_status2 {sql {db db}} {
  set ::sqlite_search_count 0
  set result [uplevel [list execsql_status $sql $db]]
  concat $result $::sqlite_search_count
}
proc count_steps_sort {sql} {
  set r [lsort -integer [db eval $sql]]
  return "$r scan [db status step] sort [db status sort]"
}
do_test where8-1.9 { execsql_status {SELECT c FROM t1 WHERE a = 1 OR b = 'three' ORDER BY rowid} } {I III 0 1}
do_test where8-1.2 { execsql_status2 {SELECT c FROM t1 WHERE a = 1 OR b = 'nine'} } {I IX 0 0 6}
do_test where7-2.2.1 { count_steps_sort {SELECT a FROM t1} } {1 2 3 scan 0 sort 0}`
	if err := i.Execute(script); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 3 {
		t.Fatalf("captured %d statements, want 3: %#v", len(stmts), stmts)
	}
	if !stmts[0].StmtStatus || stmts[0].StatusStyle != "bare" || stmts[0].SearchCount {
		t.Fatalf("execsql_status = %#v, want bare style without the search counter", stmts[0])
	}
	if !stmts[1].StmtStatus || stmts[1].StatusStyle != "bare" || !stmts[1].SearchCount {
		t.Fatalf("execsql_status2 = %#v, want bare style with the search counter", stmts[1])
	}
	if !stmts[2].StmtStatus || stmts[2].StatusStyle != "" || stmts[2].SearchCount {
		t.Fatalf("count_steps_sort = %#v, want the default labeled style", stmts[2])
	}
}

// TestCatchLappendBodyValueEmitsRuntimeList pins select1.test's catch shape:
// `set v [catch {execsql2 ...} msg]; lappend v $msg` — the body's value is
// the catch code followed by the rows, so the expectation belongs to a
// runtime_list step and the query keeps no expectation of its own.
func TestCatchLappendBodyValueEmitsRuntimeList(t *testing.T) {
	i := NewInterp()
	script := `do_test select1-6.1 {
  set v [catch {execsql2 {SELECT f1 FROM test1 ORDER BY f2}} msg]
  lappend v $msg
} {0 {f1 11 f1 33}}`
	if err := i.Execute(script); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 {
		t.Fatalf("captured %d statements, want 2: %#v", len(stmts), stmts)
	}
	q := stmts[0]
	if q.Type != "query2" || q.Expected != "" || q.CatchMessage != "msg" || !q.CatchResult {
		t.Fatalf("query = %#v, want an expectation-free query2 with the catch markers", q)
	}
	l := stmts[1]
	if l.Type != "runtime_list" || len(l.ListValues) != 2 || l.ListValues[0] != "$v" || l.ListValues[1] != "$msg" || l.Expected != "0 {f1 11 f1 33}" {
		t.Fatalf("runtime_list = %#v, want [$v $msg] with the joined expectation", l)
	}
}
