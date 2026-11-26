// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitListUnescapesQuotedElements(t *testing.T) {
	got := splitList(`a "foreign key mismatch - \"c2\" referencing \"p2\""`)
	want := []string{"a", `foreign key mismatch - "c2" referencing "p2"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitList() = %#v, want %#v", got, want)
	}
}

// TestSplitListUnescapesUnicode pins fts4umlaut's foreach list: TCL \uXXXX
// escapes inside a quoted list element must become real Unicode characters.
func TestSplitListUnescapesUnicode(t *testing.T) {
	got := splitList(`4 "Ha N\u1ed9i" "Ha N\u006f\u0302i" 0`)
	want := []string{"4", "Ha Nội", "Ha Nôi", "0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitList() = %#v, want %#v", got, want)
	}
}

// TestSplitListTrailingBackslashFromBlob pins vacuum3's random-blob
// expectation: a substituted blob may end in a backslash, which must stay in
// the final element instead of walking the parser past the string end.
func TestSplitListTrailingBackslashFromBlob(t *testing.T) {
	in := "1 2 3 " + string([]byte{0xa3, '\\'})
	got := SplitList(in)
	want := []string{"1", "2", "3", string([]byte{0xa3, '\\'})}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitList(%q) = %#v, want %#v", in, got, want)
	}
}

// TestSplitCommandMatchesTcl pins `split` semantics used by pragma4.test
// (`lindex [split $sql =] 0`): any separator character splits, empty elements
// are preserved, the default separator is whitespace, and an empty separator
// splits into characters (verified against tclsh 8.6).
func TestSplitCommandMatchesTcl(t *testing.T) {
	cases := []struct{ in, sep, want string }{
		{in: "a  b", sep: " ", want: "a {} b"},
		{in: "  a b  ", sep: " ", want: "{} {} a b {} {}"},
		{in: "a==b", sep: "=", want: "a {} b"},
		{in: "abc", sep: "", want: "a b c"},
		{in: "", sep: "=", want: "{}"},
		{in: "PRAGMA application_id = 10", sep: "=", want: "{PRAGMA application_id } { 10}"},
	}
	for _, tc := range cases {
		i := NewInterp()
		script := "set out [split " + quoteTclWord(tc.in) + " " + quoteTclWord(tc.sep) + "]"
		if err := i.Execute(script); err != nil {
			t.Fatal(err)
		}
		if got := i.vars["out"]; got != tc.want {
			t.Errorf("split(%q, %q) = %q, want %q", tc.in, tc.sep, got, tc.want)
		}
	}
}

// quoteTclWord braces a word for embedding in a probe script.
func quoteTclWord(s string) string {
	return "{" + strings.ReplaceAll(s, "}", `\}`) + "}"
}
