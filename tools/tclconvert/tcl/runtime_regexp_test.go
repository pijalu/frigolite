// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"strings"
	"testing"
)

// TestDoTestRegexpListOverRuntimeCapture pins misc3-6.11-utf8's converter
// shape: the EXPLAIN capture is an input to the regexp probes, and the
// do_test expectation describes the collected booleans.
func TestDoTestRegexpListOverRuntimeCapture(t *testing.T) {
	i := NewInterp()
	err := i.Execute(`do_test misc3-6.11-utf8 {
		set x [execsql {EXPLAIN SELECT 1}]
		set y [regexp { 123456789012 } $x]
		lappend y [regexp { 4.5678 } $x]
		lappend y [regexp { hello } $x]
		lappend y [regexp {,-B} $x]
	} {1 1 1 1}`)
	if err != nil {
		t.Fatalf("do_test: %v", err)
	}
	var expr *Stmt
	for idx := range i.stmts {
		s := &i.stmts[idx]
		if s.Type == "query_assign" && s.Expected != "" {
			t.Errorf("query_assign carries the do_test expectation")
		}
		if s.Type == "runtime_expr" && strings.HasPrefix(s.SQL, "list [regexp ") {
			expr = s
		}
	}
	if expr == nil {
		t.Fatalf("no runtime regexp list step in %#v", i.stmts)
	}
	if expr.Expected != "1 1 1 1" {
		t.Errorf("regexp list expectation = %q, want %q", expr.Expected, "1 1 1 1")
	}
	if strings.Count(expr.SQL, "[regexp ") != 4 {
		t.Errorf("regexp list = %q, want four probes", expr.SQL)
	}
}
