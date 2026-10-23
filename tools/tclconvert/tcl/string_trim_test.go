// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import "testing"

// TestStringTrimFamilyMatchesTcl pins the `string` trim family (and the
// neighbouring subcommands) against the Tcl oracle. The expectations come
// from /opt/homebrew/bin/tclsh.
//
// The trim family matters: round1.test computes its expectation with
// `string trimright $r 0`, and an unimplemented subcommand used to answer
// with its input — 9,356 wrong expectations.
func TestStringTrimFamilyMatchesTcl(t *testing.T) {
	cases := []struct {
		script string
		want   string
	}{
		{`string trimright 90821.8400 0`, "90821.84"},
		{`string trimleft "  x" " "`, "x"},
		{`string trim xxaxx x`, "a"},
		{`string trim "  x  "`, "x"},
		{`string last b abcb`, "3"},
		{`string cat a b`, "ab"},
		{`string reverse abc`, "cba"},
		{`string wordstart "hello world" 4`, "0"},
		{`string wordend "hello world" 4`, "5"},
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

// TestStringUnknownSubcommandIsAnError pins the no-silent-fallback rule: an
// unknown `string` subcommand must fail the conversion, never answer with a
// plausible value.
func TestStringUnknownSubcommandIsAnError(t *testing.T) {
	i := NewInterp()
	if err := i.Execute("set r [string nosuchsubcommand abc]"); err == nil {
		t.Fatalf("expected an error for an unknown string subcommand, got %q", i.vars["r"])
	}
}
