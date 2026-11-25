// SPDX-License-Identifier: GPL-3.0-or-later

package tcl

import (
	"strings"
	"testing"
)

// TestDoTestFinalMultiwordExprEmitsRuntimeExpr pins temptable2.test:121-124:
// an unbraced parenthesized `expr ($n >280 && $n <300)` is several TCL words,
// but it is still the do_test body's runtime value. The converter must join
// the words and emit a runtime_expr assertion, not fold the expression at
// conversion time and park `1` on the query_assign step.
func TestDoTestFinalMultiwordExprEmitsRuntimeExpr(t *testing.T) {
	i := NewInterp()
	src := `do_test temptable2-4.1.4 {
  set n [db one { PRAGMA temp.page_count }]
  expr ($n >280 && $n <300)
} {1}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 {
		t.Fatalf("statements = %#v, want query_assign then runtime_expr", stmts)
	}
	if stmts[0].Type != "query_assign" || stmts[0].Variable != "n" || stmts[0].Expected != "" {
		t.Fatalf("assignment step = %#v, want query_assign for n without expectation", stmts[0])
	}
	s := stmts[1]
	if s.Type != "runtime_expr" || strings.TrimSpace(s.SQL) != `($n >280 && $n <300)` || s.Expected != "1" {
		t.Fatalf("runtime expression step = %#v, want the joined source expr with expectation 1", s)
	}
}
