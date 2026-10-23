// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"math"
	"testing"
)

// TestTclFormatDouble pins Tcl_PrintDouble (tclUtil.c): shortest round-trip
// digits, E format when the exponent is < -4 or > 16, otherwise F format with
// an always-present fractional part. Expected strings printed by tclsh 9.1.
// date5.test compares julianday() output against these TCL representations, so
// Go's 'g' verb (2.6064665e+06) is wrong where TCL prints 2606466.5.
func TestTclFormatDouble(t *testing.T) {
	// Runtime operands: Go would constant-fold `0.1 + 0.2` at 256-bit
	// precision and produce 0.3 instead of the float64 sum.
	a, b := 0.1, 0.2
	cases := []struct {
		v    float64
		want string
	}{
		{2.0, "2.0"},
		{3.0, "3.0"},
		{2.5, "2.5"},
		{2606466.5, "2606466.5"},
		{1000000.0, "1000000.0"},
		{1e15, "1000000000000000.0"},
		{1e16, "10000000000000000.0"},
		{1e17, "1e+17"},
		{1.5e17, "1.5e+17"},
		{1e18, "1e+18"},
		{1e300, "1e+300"},
		{0.1, "0.1"},
		{a + b, "0.30000000000000004"},
		{0.0001, "0.0001"},
		{0.00001, "1e-5"},
		{2.5e-10, "2.5e-10"},
		{1.2345678901234567, "1.2345678901234567"},
		{math.Inf(1), "Inf"},
		{math.Inf(-1), "-Inf"},
	}
	for _, c := range cases {
		if got := tclFormatDouble(c.v); got != c.want {
			t.Errorf("tclFormatDouble(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// TestExprFloatFormatting pins the expr result path (formatExprResult ->
// tclFormatDouble): `expr {2606466.5}` must render 2606466.5, not Go's
// scientific form.
func TestExprFloatFormatting(t *testing.T) {
	i := NewInterp()
	for expr, want := range map[string]string{
		"2606466.5": "2606466.5",
		"3.0":       "3.0",
		"1e17":      "1e+17",
		"1e16":      "10000000000000000.0",
		"2+2":       "4",
	} {
		got, err := EvalExpr(expr, i, nil)
		if err != nil {
			t.Fatalf("EvalExpr(%q): %v", expr, err)
		}
		if got != want {
			t.Errorf("EvalExpr(%q) = %q, want %q", expr, got, want)
		}
	}
}
