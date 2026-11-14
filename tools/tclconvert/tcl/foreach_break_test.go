package tcl

import (
	"strings"
	"testing"
)

// The break-terminated var-init idiom binds its variables on the first
// iteration (TCL binds before the body's break runs). The harness reads the
// globals through registered callbacks (e_expr's `var` proc), so the
// conversion must emit runtime_expr steps with the first-chunk values.
func TestForeachBreakEmitsVarInit(t *testing.T) {
	i := NewInterp()
	src := `foreach {a b c} {0 1 0} break
set varlist [list]`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	inits := 0
	for _, stmt := range i.Stmts() {
		if stmt.Type != "runtime_expr" {
			continue
		}
		inits++
		vars[stmt.Variable] = stmt.SQL
	}
	if inits != 3 || vars["a"] != "0" || vars["b"] != "1" || vars["c"] != "0" {
		t.Fatalf("var-init steps = %v (inits=%d)", vars, inits)
	}
	// The conversion-time bindings keep the plain values for $ substitution.
	got, _ := i.Variable("a")
	if got != "0" {
		t.Fatalf("interp binding a = %q, want 0", got)
	}
}

// A list shorter than the variable list binds the tail to the empty string:
// the harness's empty-expression reset form (empty SQL).
func TestForeachBreakShortListBindsEmpty(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`foreach {a b} {7} break`); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	for _, stmt := range i.Stmts() {
		if stmt.Type == "runtime_expr" {
			vars[stmt.Variable] = stmt.SQL
		}
	}
	if vars["a"] != "7" || vars["b"] != "" {
		t.Fatalf("short-list bindings = %v", vars)
	}
}

// Non-numeric values become braced literals the harness expr evaluator
// parses as strings.
func TestForeachBreakBracedLiteral(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`foreach {x} hello break`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range i.Stmts() {
		if stmt.Type == "runtime_expr" && stmt.Variable == "x" {
			if stmt.SQL != "{hello}" {
				t.Fatalf("literal = %q, want {hello}", stmt.SQL)
			}
			return
		}
	}
	t.Fatal("no runtime_expr step emitted")
}

// Elements of an evaluated list ([expr 3]) are Tcl integer objects: the
// emitted binding carries valueType "integer" so the harness returns the
// SQL value with its object type (e_expr-22.1.1's CASE var('a') WHEN 3).
// Braced-literal elements are string objects: no valueType.
func TestForeachBreakValueTypeFollowsOrigin(t *testing.T) {
	i := NewInterp()
	src := `foreach {a b c} {0 0 0} break
db eval {CREATE TABLE t1(a)}
foreach {a b c} [list [expr 3] [expr 4] [expr 5]] break`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	var typed, untyped int
	for _, stmt := range i.Stmts() {
		if stmt.Type != "runtime_expr" {
			continue
		}
		switch stmt.Variable {
		case "a", "b", "c":
		default:
			continue
		}
		if stmt.ValueType == "integer" {
			typed++
			if !isCanonicalInteger(stmt.SQL) {
				t.Fatalf("typed binding %s SQL = %q", stmt.Variable, stmt.SQL)
			}
		} else {
			untyped++
			if stmt.ValueType != "" {
				t.Fatalf("unexpected valueType %q", stmt.ValueType)
			}
		}
	}
	if typed != 3 || untyped != 3 {
		t.Fatalf("typed=%d untyped=%d, want 3/3", typed, untyped)
	}
}

// A braced body whose first command is break is the same idiom.
func TestForeachBracedBreakBody(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`foreach {a} {5} {break}`); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, stmt := range i.Stmts() {
		if stmt.Type == "runtime_expr" && stmt.Variable == "a" && stmt.SQL == "5" {
			found = true
		}
	}
	if !found {
		t.Fatal("braced-break body emitted no var-init step")
	}
}

// A loop whose body does more than break is NOT the init idiom: the loop
// unrolls fully and emits no var-init steps (no behavior change).
func TestForeachNonBreakUnrolls(t *testing.T) {
	i := NewInterp()
	src := `foreach {v} {1 2} {
  db eval "CREATE TABLE t$v(a)"
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range i.Stmts() {
		if stmt.Type == "runtime_expr" {
			t.Fatalf("non-break loop emitted %v", stmt)
		}
	}
	var creates int
	for _, stmt := range i.Stmts() {
		if strings.HasPrefix(stmt.SQL, "CREATE TABLE t") {
			creates++
		}
	}
	if creates != 2 {
		t.Fatalf("creates = %d, want 2", creates)
	}
}
