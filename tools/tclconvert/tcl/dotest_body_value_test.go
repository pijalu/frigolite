// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import "testing"

// TestDoTestExpectationDescribesTheBodyValue pins SQLite's testfixture
// semantics: `do_test NAME { body } { expected }` compares the *value of the
// body* (the result of its last command) with the expectation — never the
// result of the last statement the body happened to capture.
//
// rtree2 is the driving case: the body ends with `set rc` (the boolean of
// `$t1 eq $t2`), so the expectation "1" belongs to that variable. Attaching
// it to the last captured SELECT compared 1000 rows with "1" (2,010 leaves).
func TestDoTestExpectationDescribesTheBodyValue(t *testing.T) {
	i := NewInterp()
	err := i.Execute(`do_test rtree2-1.2 {
		set t1 [execsql {SELECT * FROM t1 ORDER BY ii}]
		set t2 [execsql {SELECT * FROM t2 ORDER BY ii}]
		set rc [expr {$t1 eq $t2}]
		set rc
	} {1}`)
	if err != nil {
		t.Fatalf("do_test: %v", err)
	}
	stmts := i.Stmts()
	var sawQueryAssign, sawRuntimeExpr, sawRuntimeVar bool
	for _, s := range stmts {
		switch s.Type {
		case "query_assign":
			sawQueryAssign = true
			if s.Expected != "" {
				t.Errorf("query_assign %q carries the expectation %q; the do_test expectation belongs to the body's value", s.SQL, s.Expected)
			}
			if s.Variable == "t1" && !s.ResultAll {
				t.Errorf("execsql capture of t1 must keep every row (ResultAll)")
			}
		case "runtime_expr":
			sawRuntimeExpr = true
			if s.SQL != "$t1 eq $t2" {
				t.Errorf("runtime expression = %q, want %q", s.SQL, "$t1 eq $t2")
			}
			if s.Variable != "rc" {
				t.Errorf("runtime expression variable = %q, want rc", s.Variable)
			}
		case "runtime_var":
			sawRuntimeVar = true
			if s.Variable != "rc" {
				t.Errorf("runtime variable = %q, want rc", s.Variable)
			}
			if s.Expected != "1" {
				t.Errorf("runtime variable expectation = %q, want 1", s.Expected)
			}
		}
	}
	if !sawQueryAssign {
		t.Errorf("the body's execsql captures must become query_assign steps")
	}
	if !sawRuntimeExpr {
		t.Errorf("`set rc [expr {$t1 eq $t2}]` must be captured as a runtime expression")
	}
	if !sawRuntimeVar {
		t.Errorf("the body's value (`set rc`) must be captured as a runtime variable")
	}
}
