package frigolite

import "testing"

// TestExpandTCLRuntimeVarsWholeReferences pins the whole-reference rule: a
// runtime variable named `t` must not corrupt the unrelated `$tcl` parameter
// (e_expr-12.3.13.1) or a TCL array reference `$tcl(key)`.
func TestExpandTCLRuntimeVarsWholeReferences(t *testing.T) {
	values := map[string]string{"t": "null", "n": "5", "tcl": "'abc'"}
	cases := []struct{ in, want string }{
		{"SELECT $t FROM x", "SELECT null FROM x"},
		{"SELECT $tcl FROM x", "SELECT 'abc' FROM x"},
		{"SELECT $tcl(array) FROM x", "SELECT $tcl(array) FROM x"},
		{"SELECT $tclx FROM x", "SELECT $tclx FROM x"},
		{"SELECT $t$tcl FROM x", "SELECT $t$tcl FROM x"},
		{"SELECT $n+1 FROM x", "SELECT 5+1 FROM x"},
		{"SELECT $n FROM x", "SELECT 5 FROM x"},
		{"SELECT $n:123 FROM x", "SELECT 5:123 FROM x"},
		{"SELECT $n::123 FROM x", "SELECT $n::123 FROM x"},
		{"SELECT $::n FROM x", "SELECT 5 FROM x"},
	}
	for _, c := range cases {
		if got := expandTCLRuntimeVars(c.in, values); got != c.want {
			t.Errorf("expandTCLRuntimeVars(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExpandTCLRuntimeVarsWithoutTclVarKeepsParameter pins the e_expr-12.3.13.1
// shape once the unrelated `t` variable is absent: the parameter must survive
// untouched for the engine to evaluate as an unbound (NULL) parameter.
func TestExpandTCLRuntimeVarsWithoutTclVarKeepsParameter(t *testing.T) {
	values := map[string]string{"t": "null"}
	got := expandTCLRuntimeVars("SELECT $tcl FROM tblname", values)
	if got != "SELECT $tcl FROM tblname" {
		t.Fatalf("expandTCLRuntimeVars kept %q, want the $tcl parameter untouched", got)
	}
}
