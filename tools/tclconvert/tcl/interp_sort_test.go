// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"testing"
)

// TestLSortMatchesTcl pins `lsort` against the Tcl oracle: the default
// comparison is -ascii, -integer/-real compare numerically, -index compares
// the indexed element of each list, and -decreasing reverses the order.
func TestLSortMatchesTcl(t *testing.T) {
	cases := []struct {
		script string
		want   string
	}{
		{`lsort {b a c}`, "a b c"},
		{`lsort -integer {10 9 100}`, "9 10 100"},
		{`lsort -real {10.5 9.25 100}`, "9.25 10.5 100"},
		{`lsort -decreasing {a b c}`, "c b a"},
		{`lsort -index 1 {{x 30} {y 10} {z 20}}`, "{y 10} {z 20} {x 30}"},
		{`lsort -index 0 -integer {{30 a} {10 b} {20 c}}`, "{10 b} {20 c} {30 a}"},
		{`lsort -dictionary {B a}`, "a B"},
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

// TestMD5CommandMatchesTcl pins the `md5` command the testfixture provides:
// the lowercase hex MD5 digest of the argument.
func TestMD5CommandMatchesTcl(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "d41d8cd98f00b204e9800998ecf8427e"},
		{"abc", "900150983cd24fb0d6963f7d28e17f72"},
		{"hello", "5d41402abc4b2a76b9719d911017c592"},
	}
	for _, c := range cases {
		i := NewInterp()
		if err := i.Execute("set r [md5 {" + c.in + "}]"); err != nil {
			t.Fatalf("md5 %q: %v", c.in, err)
		}
		if got := i.vars["r"]; got != c.want {
			t.Errorf("md5 %q = %q, want %q", c.in, got, c.want)
		}
	}
}
