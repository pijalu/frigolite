// Package main implements the tcl2go tool.
//
// transpiler_test.go adds focused unit tests for Gap G transpiler recognition
// (the patterns used by autovacuum.test / incrvacuum*.test). These tests pin
// the contract between the TCL command surface and the Go expressions emitted
// to the generated test code:
//
//	`proc make_str {a b} {...}`  → tclMakeStr($a, $b)
//	`proc file_pages {} {...}`    → strconv.Itoa(tclFilePages("test.db"))
//	`[eval concat $list]`         → tclConcat(<rendered args>)
//	`[lsort -integer $VAR]`       → tclSortInt(<VAR>)
//	`proc joinx {args} {return [join $args -]}` → separator "-" recognized
package main

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// TestTranspileMakeStr verifies that collectSpecialFuncs recognizes a
// `proc make_str {a b} {...}` definition (autovacuum.test / incrvacuum*.test
// pattern) and substitutes it with a tclMakeStr($a, $b) call template.
func TestTranspileMakeStr(t *testing.T) {
	src := `proc make_str {a b} {
		return [string repeat $a $b]
	}`
	specials := collectSpecialFuncs(tcl.ParseCommands(src))
	got, ok := specials["make_str"]
	if !ok {
		t.Fatalf("make_str not recognized by collectSpecialFuncs; specials=%v", specials)
	}
	want := "tclMakeStr($a, $b)"
	if got != want {
		t.Fatalf("make_str template mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestTranspileFilePages verifies that collectSpecialFuncs recognizes a
// `proc file_pages {} {...}` definition (autovacuum.test / incrvacuum*.test
// pattern) and substitutes it with a strconv.Itoa(tclFilePages("test.db"))
// call template. The template must use tclFilePages (not [file size ...])
// because the harness runtime uses the page_size-aware Go helper.
func TestTranspileFilePages(t *testing.T) {
	src := `proc file_pages {} {
		expr [file size test.db] / 1024
	}`
	specials := collectSpecialFuncs(tcl.ParseCommands(src))
	got, ok := specials["file_pages"]
	if !ok {
		t.Fatalf("file_pages not recognized by collectSpecialFuncs; specials=%v", specials)
	}
	want := `strconv.Itoa(tclFilePages("test.db"))`
	if got != want {
		t.Fatalf("file_pages template mismatch:\n  got:  %s\n  want: %s", got, want)
	}
	if !strings.Contains(got, "tclFilePages") {
		t.Fatalf("file_pages template must route through tclFilePages helper: %s", got)
	}
}

// TestTranspileEvalConcat verifies that the `[eval concat ...]` pattern
// (used in autovacuum.test 1.x to flatten list-of-lists before lsort) flows
// through cmdExpr and resolves to a tclConcat(...) call. The handler must
// accept the trailing `[concat $delete_order]` shape that the harness emits.
func TestTranspileEvalConcat(t *testing.T) {
	tp := &transpiler{}
	// Direct path: [concat $a $b] → tclConcat(<expr a>, <expr b>).
	got := tp.cmdExprConcat("concat", "concat $a $b", []string{"$a", "$b"})
	if !strings.Contains(got, "tclConcat(") {
		t.Fatalf("cmdExprConcat must emit tclConcat(...): got=%q", got)
	}
	// Indirect path: [eval concat $list] should dispatch via cmdExprDefault's
	// eval branch, which re-tokenizes and re-routes to cmdExprConcat.
	gotEval := tp.cmdExpr("eval concat $delete_order")
	if !strings.Contains(gotEval, "tclConcat(") {
		t.Fatalf("[eval concat ...] must resolve to tclConcat(...): got=%q", gotEval)
	}
	// Empty concat must render a stable literal (avoids crashing callers).
	if gotEmpty := tp.cmdExprConcat("concat", "concat", nil); gotEmpty == "" {
		t.Fatalf("cmdExprConcat with no args must return a stable literal, got empty")
	}
}

// TestTranspileLsortInteger verifies that `[lsort -integer $VAR]` (used by
// autovacuum.test 1.x to numerically sort a TCL list of integer codes) is
// rendered through tclSortInt. Without the -integer flag, the handler must
// emit a text-sort call (tclSort).
func TestTranspileLsortInteger(t *testing.T) {
	tp := &transpiler{}
	// -integer path: numeric compare.
	gotInt := tp.cmdExprLSort("lsort", "lsort -integer $delete_order",
		[]string{"-integer", "$delete_order"})
	if !strings.Contains(gotInt, "tclSortInt(") {
		t.Fatalf("lsort -integer must route through tclSortInt: got=%q", gotInt)
	}
	// Default path (no flag): lexicographic compare.
	gotText := tp.cmdExprLSort("lsort", "lsort $delete_order", []string{"$delete_order"})
	if !strings.Contains(gotText, "tclSort(") {
		t.Fatalf("plain lsort must route through tclSort: got=%q", gotText)
	}
	if strings.Contains(gotText, "tclSortInt(") {
		t.Fatalf("plain lsort must not use tclSortInt: got=%q", gotText)
	}
	// Combined -integer -decreasing must route through tclSortIntDesc.
	gotDesc := tp.cmdExprLSort("lsort", "lsort -integer -decreasing $x",
		[]string{"-integer", "-decreasing", "$x"})
	if !strings.Contains(gotDesc, "tclSortIntDesc(") {
		t.Fatalf("lsort -integer -decreasing must route through tclSortIntDesc: got=%q", gotDesc)
	}
}

// TestTranspileJoinSeparator verifies that joinProcValue extracts the
// separator from a join-proc body like `{ return [join $args -] }` (the
// func8.test `proc joinx {args} { return [join $args -] }` pattern). The
// extracted separator is used by the db-func registration code to render
// the proc's runtime join call.
func TestTranspileJoinSeparator(t *testing.T) {
	cases := []struct {
		name string
		body string
		sep  string
	}{
		{
			name: "dash separator (func8.joinx)",
			body: `{ return [join $args -] }`,
			sep:  "-",
		},
		{
			name: "comma-space separator (common test infra)",
			body: `{return [join $args {, }]}`,
			sep:  ", ",
		},
		{
			name: "underscore separator",
			body: `{ return [join $args _] }`,
			sep:  "_",
		},
		{
			name: "empty separator (single char)",
			body: `{ return [join $args |] }`,
			sep:  "|",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := joinProcValue(tc.body)
			if got != tc.sep {
				t.Fatalf("joinProcValue(%q):\n  got:  %q\n  want: %q", tc.body, got, tc.sep)
			}
		})
	}
	// Negative cases: non-join bodies must return "" so the caller can fall
	// back to default proc handling.
	negCases := []struct {
		body string
	}{
		{`{ return "literal" }`},
		{`{ return [list $a $b] }`},
		{`{ incr x }`},
	}
	for _, tc := range negCases {
		got := joinProcValue(tc.body)
		if got != "" {
			t.Fatalf("joinProcValue(%q) must return empty for non-join bodies, got=%q", tc.body, got)
		}
	}
}