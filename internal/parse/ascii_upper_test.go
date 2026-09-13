// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger

package parse

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/sql"
)

// TestAsciiUpperLengthPreserving guards the byte-length invariant that lets
// scanners index a case-folded copy of the SQL with positions from the
// original. strings.ToUpper shrinks some code points when upper-cased
// (U+017F LATIN SMALL LETTER LONG S "ſ" is 2 bytes; its uppercase "S" is 1),
// which panicked nextFuncCallParen with an index-out-of-range when SQL
// contained such characters (fts5tok2 feeds "abc%Cxyz" for every code point
// below 65536). SQLite folds only ASCII (sqlite3UpperToLower), so asciiUpper
// mirrors that: byte-length preserving by construction.
func TestAsciiUpperLengthPreserving(t *testing.T) {
	inputs := []string{
		"", "plain ascii", "ALREADY UPPER", "SELECT input FROM t3 WHERE input=abcſxyz",
		"abcﬅxyz", "ŉeon", "ß sharp", "\xcd\xbf mid-bytes \xcd\xbf", "aA0_zZ",
	}
	for _, in := range inputs {
		got := asciiUpper(in)
		if len(got) != len(in) {
			t.Errorf("asciiUpper(%q) changed byte length: %d -> %d", in, len(in), len(got))
		}
		for i := 0; i < len(in); i++ {
			want := in[i]
			if want >= 'a' && want <= 'z' {
				want -= 'a' - 'A'
			}
			if got[i] != want {
				t.Errorf("asciiUpper(%q)[%d] = %q, want %q", in, i, got[i], want)
				break
			}
		}
	}
}

// TestCollectFuncCallOrderByNonASCII drives the function-call ORDER BY
// recovery scanner over SQL containing every code point whose upper-case
// form changes byte length, plus the fts5tok2 statement shapes that
// originally panicked.
func TestCollectFuncCallOrderByNonASCII(t *testing.T) {
	// fts5tok2 loop shapes: input appended inside a vtab TVF arg list and
	// after a bare WHERE comparison (outside any parens).
	for cp := 0; cp < 0x180; cp++ {
		in := "abc" + string(rune(cp)) + "xyz"
		_ = collectFuncCallOrderBy("SELECT input, token FROM t5(" + in + ")")
		_ = collectFuncCallOrderBy("SELECT input FROM t3 WHERE input=" + in)
	}
	// The scanner still recovers ORDER BY from a real function call.
	stmts, err := ParseSQL("SELECT group_concat(name ORDER BY name DESC) FROM t")
	if err != nil {
		t.Fatalf("ParseSQL: %v", err)
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || len(sel.Columns) != 1 {
		t.Fatalf("unexpected parse: %#v", stmts)
	}
	fc, ok := sel.Columns[0].Expr.(*sql.FuncCall)
	if !ok || len(fc.OrderBy) != 1 {
		t.Fatalf("ORDER BY not recovered: %#v", sel.Columns[0].Expr)
	}
	if got := strings.ToUpper(fc.Name); got != "GROUP_CONCAT" {
		t.Fatalf("func name = %q", got)
	}
}
