// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"testing"
	"time"
)

// TestCStrftimeMatchesLibc pins the conversions date4.test compares against:
// the C library's strftime() on gmtime() (test1.c strftime_cmd). Expected
// strings were produced by the platform libc for the same timestamps.
func TestCStrftimeMatchesLibc(t *testing.T) {
	const fmtDate4 = "%d,%e,%F,%H,%I,%j,%p,%R,%u,%w,%W,%%"
	cases := []struct {
		ts   int64
		want string
	}{
		{0, "01, 1,1970-01-01,00,12,001,AM,00:00,4,4,00,%"},
		{86390, "01, 1,1970-01-01,23,11,001,PM,23:59,4,4,00,%"},
		{172780, "02, 2,1970-01-02,23,11,002,PM,23:59,5,5,00,%"},
		{951782400, "29,29,2000-02-29,00,12,060,AM,00:00,2,2,09,%"},
		{1712275200, "05, 5,2024-04-05,00,12,096,AM,00:00,5,5,14,%"},
	}
	for _, c := range cases {
		got := cStrftime(fmtDate4, time.Unix(c.ts, 0).UTC())
		if got != c.want {
			t.Errorf("cStrftime(%q, %d) = %q, want %q", fmtDate4, c.ts, got, c.want)
		}
	}
}

// TestStrftimeCommandEvaluatesTCLVars pins the date4.test shape:
// `[strftime $FMT $TS]` with both values coming from TCL variables.
func TestStrftimeCommandEvaluatesTCLVars(t *testing.T) {
	i := NewInterp()
	err := i.Execute(`set FMT {%d,%e,%F,%H,%I,%j,%p,%R,%u,%w,%W,%%}
set TS 86390
set out [strftime $FMT $TS]`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := i.vars["out"], "01, 1,1970-01-01,23,11,001,PM,23:59,4,4,00,%"; got != want {
		t.Fatalf("strftime result = %q, want %q", got, want)
	}
}
