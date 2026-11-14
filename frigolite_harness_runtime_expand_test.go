package frigolite

import "testing"

// TestExpandTCLVarRefsAdjacentPlaceholders pins the expansion of the
// adjacent runtime-value placeholders the converter freezes into an
// expectation: `[list "${as}${bs}"]` with query_assign-backed as/bs bakes
// the text "$as$bs", which scanTCLParamRef reads as ONE reference ('$' is a
// valid identifier byte) — the expansion must split at the inner '$' and
// resolve each segment (e_expr-5.1/5.2). A single reference whose name
// merely starts with a known one ("$tcl" vs "t") must stay intact.
func TestExpandTCLVarRefsAdjacentPlaceholders(t *testing.T) {
	identity := func(v string) string { return v }
	vars := map[string]string{"as": "helloworld", "bs": "12345", "t": "nullcl"}
	cases := []struct {
		in   string
		want string
	}{
		{"$as$bs", "helloworld12345"},
		{"$bs$as", "12345helloworld"},
		{"$as$bs$as", "helloworld12345helloworld"},
		{"SELECT '$as$bs'", "SELECT 'helloworld12345'"},
		// An unresolvable segment keeps its own tail verbatim; a ref whose
		// first segment is unknown stays verbatim (scanTCLParamRef already
		// consumed the whole adjacent run as one key).
		{"$as$missing", "helloworld$missing"},
		{"$missing$bs", "$missing$bs"},
		// A whole-key hit wins over a prefix split ("as" is a key here).
		{"$as x", "helloworld x"},
	}
	for _, c := range cases {
		if got := expandTCLVarRefs(c.in, vars, identity); got != c.want {
			t.Errorf("expandTCLVarRefs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// "$tcl" resolves as the whole name "tcl" when that key exists, and
	// stays verbatim when only the shorter "t" is known (no split: the key
	// "tcl" contains no '$').
	if got := expandTCLVarRefs("$tcl", map[string]string{"tcl": "X", "t": "Y"}, identity); got != "X" {
		t.Errorf(`expandTCLVarRefs("$tcl") = %q, want "X"`, got)
	}
	if got := expandTCLVarRefs("$tcl", map[string]string{"t": "Y"}, identity); got != "$tcl" {
		t.Errorf(`expandTCLVarRefs("$tcl" with only t) = %q, want "$tcl"`, got)
	}
}
