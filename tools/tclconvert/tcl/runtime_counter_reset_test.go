package tcl

import "testing"

// TestTrackedVarNumericResetEmitsSetStep pins e_expr-26.1.x's
// `set ::evalcount 0` between callback-count probes. A numeric reset of a
// callback-recorded global must reach the harness or the counter accumulates
// across probes (26.1.6 saw 8 instead of 5).
func TestTrackedVarNumericResetEmitsSetStep(t *testing.T) {
	i := NewInterp()
	script := `proc ceval {x} {
  incr ::evalcount
  return $x
}
db func ceval ceval
do_test 1 { db eval {SELECT ceval(1)}; set ::evalcount } {1}
set ::evalcount 0
do_test 2 { db eval {SELECT ceval(1)}; set ::evalcount } {1}`
	if err := i.Execute(script); err != nil {
		t.Fatal(err)
	}
	for _, s := range i.Stmts() {
		if s.Type == "runtime_expr" && s.Variable == "evalcount" && s.SQL == "0" {
			return
		}
	}
	t.Fatalf("no numeric runtime reset emitted: %#v", i.Stmts())
}
