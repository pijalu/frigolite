//go:build testgen
// +build testgen

package misc3

import "testing"

// Hand-written pin (NOT generated): tclFpnumCompare ports src/test1.c
// fpnum_compare — the TCL suite's do_test fallback comparator. These cases
// were validated against the C original's code paths (test1.c 6168-6303).
// Note the comparator is intentionally STRICTER than its doc comment: a
// token missing the '.' (1e-100 vs 1.0e-100) breaks out at the fraction
// pairing and compares unequal, and exponent sign characters must match
// literally (1e+5 vs 1e5 is unequal).
func TestT33FpnumComparePin(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.000000000000000e-225", "1.0000000000000e-225", true},  // misc3-2.5
		{"1.0", "1.0", true},
		{"1.0", "1", false},
		{"1e-100", "1.0e-100", false},
		{"1e+5", "1e5", false},
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"1 2 3", "1 2 3", true},
		{"1 2 4", "1 2 3", false},
		{"2.5", "2.6", false},
		{"0.9999999999999999", "1.0", false},
		{"1.0000000000000000000001", "1.0", true},
		{"1 2 3 4 5 6 7 bam {}", "1 2 3 4 5 6 7 bam {}", true},
		{"-1.5", "-1.5", true},
		{"-1.5", "1.5", false},
		{"1e225", "1e226", false},
		{"{} bam bam", "{} bam bam", true},
		{"hello world", "hello world", true},
		{"3.0", "3.00", true},
		{"12345678901234567890", "12345678901234567891", false},
	}
	for _, c := range cases {
		if got := tclFpnumCompare(c.a, c.b); got != c.want {
			t.Errorf("tclFpnumCompare(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
