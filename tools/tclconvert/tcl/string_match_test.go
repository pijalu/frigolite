// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import "testing"

// TestStringMatchMatchesTcl pins `string match` against the Tcl oracle
// (tclsh 9.1.0): the glob syntax has `*`, `?`, `[class]` (with ranges, but no
// negation) and backslash escapes. e_expr.test relies on it to decide whether
// a SQL fragment contains "EXPR"/"EXPR2" before substituting it.
func TestStringMatchMatchesTcl(t *testing.T) {
	cases := []struct {
		pattern string
		str     string
		want    string
	}{
		{"*EXPR*", "+ EXPR", "1"},
		{"*EXPR2*", "+ EXPR", "0"},
		{"*EXPR2*", "EXPR1", "0"},
		{"a?c", "abc", "1"},
		{"a?c", "ac", "0"},
		{"[a-z]*", "zzz", "1"},
		{"[a-z]*", "ZZZ", "0"},
		{"[^a]", "b", "0"},
		{"[^a]", "a", "1"},
		{"[!a]", "b", "0"},
		{`a\*b`, "a*b", "1"},
		{"*", "", "1"},
		{"a*z", "abcdez", "1"},
		{"a*z", "abc", "0"},
		{"*x*", "abc", "0"},
	}
	for _, c := range cases {
		i := NewInterp()
		script := "set r [string match {" + c.pattern + "} {" + c.str + "}]"
		if err := i.Execute(script); err != nil {
			t.Fatalf("string match %q %q: %v", c.pattern, c.str, err)
		}
		if got := i.vars["r"]; got != c.want {
			t.Errorf("string match %q %q = %q, want %q", c.pattern, c.str, got, c.want)
		}
	}
}

// TestStringIsMatchesTcl pins `string is` for the classes the test suite uses.
func TestStringIsMatchesTcl(t *testing.T) {
	cases := []struct {
		class string
		str   string
		want  string
	}{
		{"integer", "34", "1"},
		{"integer", " 34", "1"},
		{"integer", "0x1f", "1"},
		{"integer", "1.5", "0"},
		{"integer", "", "1"},
		{"double", "1e5", "1"},
		{"double", "abc", "0"},
		{"double", "Inf", "1"},
		{"boolean", "true", "1"},
		{"boolean", "2", "0"},
		{"alpha", "abc", "1"},
		{"alnum", "a1", "1"},
		{"digit", "12", "1"},
		{"space", "  ", "1"},
		{"list", "a b", "1"},
		{"list", `a "b`, "0"},
	}
	for _, c := range cases {
		i := NewInterp()
		script := "set r [string is " + c.class + " {" + c.str + "}]"
		if err := i.Execute(script); err != nil {
			t.Fatalf("string is %s %q: %v", c.class, c.str, err)
		}
		if got := i.vars["r"]; got != c.want {
			t.Errorf("string is %s %q = %q, want %q", c.class, c.str, got, c.want)
		}
	}
}

// TestStringReplaceMatchesTcl pins `string replace`: a negative first index is
// clamped to 0, an inverted or empty range leaves the string unchanged and
// `end` refers to the last character.
func TestStringReplaceMatchesTcl(t *testing.T) {
	cases := []struct {
		script string
		want   string
	}{
		{`string replace "hello world" 0 4 "HI"`, "HI world"},
		{`string replace "hello" 2 1 "X"`, "hello"},
		{`string replace "hello" 3 end "LLO"`, "helLLO"},
		{`string replace "hello" -1 2 "X"`, "Xlo"},
		{`string replace "hello" 0 -1 "X"`, "hello"},
		{`string replace "hello" 0 5 "X"`, "X"},
		{`string replace "hello" 5 9 "X"`, "hello"},
		{`string replace "hello" 1 1 ""`, "hllo"},
	}
	for _, c := range cases {
		i := NewInterp()
		if err := i.Execute("set r [" + c.script + "]"); err != nil {
			t.Fatalf("%s: %v", c.script, err)
		}
		if got := i.vars["r"]; got != c.want {
			t.Errorf("%s = %q, want %q", c.script, got, c.want)
		}
	}
}

// TestSQLiteMaxVariableNumberDefault pins the SQLITE_MAX_* globals the
// testfixture exposes to the TCL scripts. e_expr.test builds the expected
// "variable number must be between ?1 and ?N" message from
// $SQLITE_MAX_VARIABLE_NUMBER, so the converter must know it (32766, the
// post-3.32 default and this engine's own limit).
func TestSQLiteMaxVariableNumberDefault(t *testing.T) {
	i := NewInterp()
	if got := i.vars["SQLITE_MAX_VARIABLE_NUMBER"]; got != "32766" {
		t.Fatalf("SQLITE_MAX_VARIABLE_NUMBER = %q, want 32766", got)
	}
	if err := i.Execute("set r $SQLITE_MAX_VARIABLE_NUMBER"); err != nil {
		t.Fatal(err)
	}
	if got := i.vars["r"]; got != "32766" {
		t.Fatalf("expanded SQLITE_MAX_VARIABLE_NUMBER = %q, want 32766", got)
	}
}
