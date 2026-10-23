// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import "testing"

// TestFormatCommandMatchesTcl pins `format` conversion semantics used by the
// SQLite test scripts: C/Tcl parse the argument according to the conversion
// (floating point for a/e/f/g, 6 significant digits by default for %g,
// character codes for %c), and C length modifiers (l/ll) are ignored.
// Expected values were produced with tclsh.
func TestFormatCommandMatchesTcl(t *testing.T) {
	cases := []struct {
		format string
		args   []string
		want   string
	}{
		{format: "%.32e", args: []string{"3.5"}, want: "3.50000000000000000000000000000000e+00"},
		{format: "%g", args: []string{"3.14159265358979"}, want: "3.14159"},
		{format: "%f", args: []string{"2.5"}, want: "2.500000"},
		{format: "%d", args: []string{"42"}, want: "42"},
		{format: "%i", args: []string{"42"}, want: "42"},
		{format: "%u", args: []string{"-1"}, want: "4294967295"},
		{format: "%x", args: []string{"255"}, want: "ff"},
		{format: "%X", args: []string{"255"}, want: "FF"},
		{format: "%c", args: []string{"65"}, want: "A"},
		{format: "%lld", args: []string{"12"}, want: "12"},
		{format: "%llx", args: []string{"255"}, want: "ff"},
		{format: "%5.2f", args: []string{"3.14159"}, want: " 3.14"},
		{format: "%08.3f", args: []string{"-3.14159"}, want: "-003.142"},
		{format: "%+.1e", args: []string{"12345.678"}, want: "+1.2e+04"},
		{format: "x=%s y=%d", args: []string{"a b", "7"}, want: "x=a b y=7"},
		{format: "100%%", args: nil, want: "100%"},
	}
	for _, tc := range cases {
		i := NewInterp()
		script := "set out [format " + quoteTclWord(tc.format)
		for _, a := range tc.args {
			script += " " + quoteTclWord(a)
		}
		script += "]"
		if err := i.Execute(script); err != nil {
			t.Fatalf("format %q: %v", tc.format, err)
		}
		if got := i.vars["out"]; got != tc.want {
			t.Errorf("format %q %v = %q, want %q", tc.format, tc.args, got, tc.want)
		}
	}
}
