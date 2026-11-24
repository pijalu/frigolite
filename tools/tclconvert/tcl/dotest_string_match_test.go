// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import "testing"

// TestDoTestStringMatchOverRuntimeVarEmitsRuntimeExpr pins trigger7-2.x's
// converter shape: the do_test body's value is `string match` over a
// runtime-captured `db eval` result, not the EXPLAIN rows themselves.
func TestDoTestStringMatchOverRuntimeVarEmitsRuntimeExpr(t *testing.T) {
	i := NewInterp()
	err := i.Execute(`do_test trigger7-2.1 {
		set txt [db eval {EXPLAIN UPDATE t1 SET x=5}]
		string match *___update_t1.x___* $txt
	} {1}`)
	if err != nil {
		t.Fatalf("do_test: %v", err)
	}
	var assignExpected bool
	var match *Stmt
	for idx := range i.stmts {
		s := &i.stmts[idx]
		if s.Type == "query_assign" && s.Expected != "" {
			assignExpected = true
		}
		if s.Type == "runtime_expr" && s.SQL == "string match *___update_t1.x___* $txt" {
			match = s
		}
	}
	if assignExpected {
		t.Errorf("query_assign carries the do_test expectation; it belongs to the string match")
	}
	if match == nil {
		t.Fatalf("no runtime_expr string match step in %#v", i.stmts)
	}
	if match.Expected != "1" {
		t.Errorf("string match expectation = %q, want 1", match.Expected)
	}
}
