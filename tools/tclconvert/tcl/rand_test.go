// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"math"
	"strconv"
	"testing"
)

// TestRandMatchesTcl pins expr rand()/srand() to TCL 9.1's generator
// (TclRand in tclBasic.c): a Park-Miller LCG with Schrage's method whose
// result is multiplied by the double constant 1.0/RAND_IM. srand() consumes
// one draw, so the first rand() after srand(1) is the second LCG step
// (16807^2 / (2^31-1) = 0.13153778814316625). Expected values printed by
// tclsh 9.1.
func TestRandMatchesTcl(t *testing.T) {
	i := NewInterp()
	if _, err := EvalExpr("srand(1)", i, nil); err != nil {
		t.Fatalf("srand(1): %v", err)
	}
	want := []float64{
		0.13153778814316625,
		0.7556053221950332,
		0.4586501319234493,
		0.5327672374121692,
		0.21895918632809036,
		0.04704461621448613,
	}
	for idx, w := range want {
		got, err := EvalExpr("rand()", i, nil)
		if err != nil {
			t.Fatalf("rand() #%d: %v", idx, err)
		}
		f, err := strconv.ParseFloat(got, 64)
		if err != nil {
			t.Fatalf("rand() #%d = %q: %v", idx, got, err)
		}
		if math.Float64bits(f) != math.Float64bits(w) {
			t.Errorf("rand() #%d = %.17g, want %.17g", idx, f, w)
		}
	}
}

// TestRandMatchesTclAtof1Loop pins the exact loop shape atof1.test uses after
// `expr srand(1)`: each iteration draws pow, then two values for x. Expected
// values printed by tclsh 9.1 (see the atof1.test header).
func TestRandMatchesTclAtof1Loop(t *testing.T) {
	i := NewInterp()
	if _, err := EvalExpr("srand(1)", i, nil); err != nil {
		t.Fatalf("srand(1): %v", err)
	}
	wantPow := []string{"-12", "1", "6", "-4", "-16"}
	wantX := []string{
		"36226224.13051589",
		"-0.02644291443960379",
		"0.0014178266267947184",
		"922308.1027911354",
		"158963.53825906324",
	}
	for n := range wantX {
		pow, err := EvalExpr("int((rand()-0.5)*35)", i, nil)
		if err != nil {
			t.Fatalf("iteration %d pow: %v", n+1, err)
		}
		if pow != wantPow[n] {
			t.Errorf("iteration %d pow = %s, want %s", n+1, pow, wantPow[n])
		}
		xStr, err := EvalExpr("pow((rand()-0.5)*2*rand(),"+pow+")", i, nil)
		if err != nil {
			t.Fatalf("iteration %d x: %v", n+1, err)
		}
		got, err := strconv.ParseFloat(xStr, 64)
		if err != nil {
			t.Fatalf("iteration %d x = %q: %v", n+1, xStr, err)
		}
		want, err := strconv.ParseFloat(wantX[n], 64)
		if err != nil {
			t.Fatalf("bad want value %q: %v", wantX[n], err)
		}
		// Go's math.Pow is not bit-identical to the platform libm pow that
		// TCL calls (±1 ulp on a few of these inputs). The fixture only
		// asserts AtoF(format(x)) == x for its own x, so a few ulps here do
		// not change what the test proves.
		if diff := math.Abs(got - want); diff > 0 {
			step := math.Nextafter(want, math.Inf(1)) - want
			if step <= 0 {
				step = math.SmallestNonzeroFloat64
			}
			if ulps := diff / step; ulps > 4 {
				t.Errorf("iteration %d x = %.17g, want %.17g (%.1f ulp)", n+1, got, want, ulps)
			}
		}
	}
}
