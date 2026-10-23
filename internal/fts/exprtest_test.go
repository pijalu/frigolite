// SPDX-License-Identifier: GPL-3.0-or-later
package fts

import (
	"testing"

	"github.com/pijalu/frigolite/internal/vtab"
)

// TestFts3ExprTestRendering pins the FTS3 query expression parser's test
// rendering against the expectations of fts3expr.test (the oracle: SQLite
// 3.54's fts3_exprtest()).
func TestFts3ExprTestRendering(t *testing.T) {
	// fts3expr.test sets sqlite_fts3_enable_parentheses to 1: the
	// parenthesis syntax recognizes the AND/OR/NOT/NEAR keywords.
	vtab.TclVarSet("sqlite_fts3_enable_parentheses", "", "1")
	defer vtab.TclVarSet("sqlite_fts3_enable_parentheses", "", "")
	cases := []struct {
		expr string
		want string
	}{
		{"abcd", "PHRASE 3 0 abcd"},
		{"(,(blah-),)", "PHRASE 3 0 blah"},
		{" tag ", "PHRASE 3 0 tag"},
		{"ab AND cd", "AND {PHRASE 3 0 ab} {PHRASE 3 0 cd}"},
		{"ab cd", "AND {PHRASE 3 0 ab} {PHRASE 3 0 cd}"},
		{"ab OR cd", "OR {PHRASE 3 0 ab} {PHRASE 3 0 cd}"},
		{"ab NOT cd", "NOT {PHRASE 3 0 ab} {PHRASE 3 0 cd}"},
		{"ab NEAR cd", "NEAR/10 {PHRASE 3 0 ab} {PHRASE 3 0 cd}"},
		{"ab NEAR/5 cd", "NEAR/5 {PHRASE 3 0 ab} {PHRASE 3 0 cd}"},
		{"ab NEAR/87654321 cd", "NEAR/87654321 {PHRASE 3 0 ab} {PHRASE 3 0 cd}"},
		{`"one two three"`, "PHRASE 3 0 one two three"},
		{`zero "one two three" four`, "AND {AND {PHRASE 3 0 zero} {PHRASE 3 0 one two three}} {PHRASE 3 0 four}"},
		{`zero AND "one two three" four`, "AND {AND {PHRASE 3 0 zero} {PHRASE 3 0 one two three}} {PHRASE 3 0 four}"},
		{"ab*", "PHRASE 3 0 ab+"},
	}
	for _, c := range cases {
		got, err := ExprTest("simple", c.expr, []string{"a", "b", "c"}, false)
		if err != nil {
			t.Errorf("ExprTest(%q) error: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("ExprTest(%q) = %q, want %q", c.expr, got, c.want)
		}
	}
}
