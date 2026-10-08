// SPDX-License-Identifier: GPL-3.0-or-later

package tcl

import "testing"

// TestTclSQLLiteral pins the SQL rendering of bound TCL variable values: the
// real tcl interface binds $name/:name/@name parameters with the variable's
// TCL type (tclsqlite.c:1519/:1526/:1537), so numeric values must render as
// bare SQL literals and everything else as quoted text. See interp_sql.go's
// tclSQLLiteral for the rationale (recursive CTE `WHERE i<$nrow`).
func TestTclSQLLiteral(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"10", "10"},
		{"-5", "-5"},
		{"+3", "+3"},
		{"1.5", "1.5"},
		{".5", ".5"},
		{"5.", "5."},
		{"1e3", "1e3"},
		{"1E-3", "1E-3"},
		{"  10  ", "10"},
		{"", "''"},
		{"abc", "'abc'"},
		{"O'Brien", "'O''Brien'"},
		{"10abc", "'10abc'"},
		{"0x10", "'0x10'"},
		{"1.2.3", "'1.2.3'"},
		{"1e", "'1e'"},
		{"Inf", "'Inf'"},
		{"+", "'+'"},
		{"1 0", "'1 0'"},
	}
	for _, c := range cases {
		if got := tclSQLLiteral(c.in); got != c.want {
			t.Errorf("tclSQLLiteral(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
