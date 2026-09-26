package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// Native pins for the fts5prefix column-filter contracts (T33r-fts,
// 2026-09-26; testgen/fts5prefix 3.3/4.1/4.2 whose transpiled forms were
// mangled — brace-group test names executed as SQL, `{a b} : $pattern`
// query text dropped, and TCL's `c1:x*` column filter rewritten to
// `c1<x>*`). Expectations mirror the TCL test's own gmatch/ghl procs
// (lsearch -glob over the whitespace-split column value) and were
// cross-checked against the sqlite3 CLI oracle 3.54.0.
func TestFTS5ColumnListAndFilterPrefixPin(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	must := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must(`CREATE VIRTUAL TABLE t3 USING fts5(a, b, c);
INSERT INTO t3(t3, rank) VALUES('pgsz', 32);
INSERT INTO t3 VALUES('acb ccc bba', 'cca bba bca', 'bbc ccc bca');
INSERT INTO t3 VALUES('cbb cac cab', 'abb aac bba', 'aab ccc cac');
INSERT INTO t3 VALUES('aac bcb aac', 'acb bcb caa', 'aca bab bca');
INSERT INTO t3 VALUES('aab ccb ccc', 'aca cba cca', 'aca aac cbb');`)

	// gmatchRows computes the TCL gmatch expectation: rowids whose named
	// column holds a token matching the glob pattern.
	glob := func(pat, tok string) bool {
		segs := strings.Split(pat, "*")
		if !strings.HasPrefix(tok, segs[0]) {
			return false
		}
		tok = tok[len(segs[0]):]
		for _, seg := range segs[1:] {
			if seg == "" {
				continue
			}
			i := strings.Index(tok, seg)
			if i < 0 {
				return false
			}
			tok = tok[i+len(seg):]
		}
		return true
	}
	// 3.3 colset rowids — the two-column list filter {a b} : c* must match
	// exactly the docs whose column a OR column b holds a c* token
	// (real matches: the corpus's xa*..xj* globs match nothing, so the pin
	// uses a pattern with hits; the emptiness parity is covered upstream).
	want := ""
	for _, row := range db.Query(`SELECT rowid, a, b FROM t3 ORDER BY rowid`).Rows {
		hit := false
		for _, v := range row[1:3] {
			for _, tok := range strings.Fields(v.(string)) {
				if glob("c*", tok) {
					hit = true
				}
			}
		}
		if hit {
			want += fmt.Sprint(row[0].(int64)) + " "
		}
	}
	r := db.Query(`SELECT rowid FROM t3('{a b} : c*')`)
	if r.Error != nil {
		t.Fatalf("column-list MATCH: %v", r.Error)
	}
	got := ""
	for _, row := range r.Rows {
		got += fmt.Sprint(row[0].(int64)) + " "
	}
	if strings.TrimSpace(got) != strings.TrimSpace(want) {
		t.Errorf("'{a b} : c*': got [%s], want [%s]", strings.TrimSpace(got), strings.TrimSpace(want))
	}

	// highlight() over a column-list query wraps exactly the matched tokens.
	r = db.Query(`SELECT highlight(t3, 0, '*', '*') FROM t3('{a} : cb*')`)
	if r.Error != nil {
		t.Fatalf("highlight over column-list: %v", r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatalf("'{a} : cb*' matched no rows; want the a-column cb* docs")
	}
	for _, row := range r.Rows {
		s := row[0].(string)
		for _, tok := range strings.Fields(s) {
			if strings.HasPrefix(tok, "cb") && !strings.HasPrefix(tok, "*") {
				t.Errorf("highlight missed token %q in %q", tok, s)
			}
		}
	}

	// 4.1/4.2 — the column-filter prefix form c1:x* must track UPDATEs of
	// the filtered column exactly (4096-row doubling corpus from the TCL).
	must(`CREATE VIRTUAL TABLE t2 USING fts5(c1, c2);
INSERT INTO t2 VALUES('xa xb', 'xb xa');`)
	for i := 0; i < 12; i++ {
		must(`INSERT INTO t2 SELECT c1||' '||c1, c2||' '||c2 FROM t2;`)
	}
	count := func(q string) int64 {
		rows := db.Query(q).Rows
		return rows[0][0].(int64)
	}
	if n := count(`SELECT count(*) FROM t2('c1:x*')`); n != 4096 {
		t.Errorf("4.0: c1:x* = %d, want 4096", n)
	}
	must(`UPDATE t2 SET c2 = 'ya yb';`)
	if n := count(`SELECT count(*) FROM t2('c1:x*')`); n != 4096 {
		t.Errorf("4.1: c1:x* = %d, want 4096", n)
	}
	if n := count(`SELECT count(*) FROM t2('c2:x*')`); n != 0 {
		t.Errorf("4.1: c2:x* = %d, want 0", n)
	}
	must(`UPDATE t2 SET c2 = 'xa';`)
	if n := count(`SELECT count(*) FROM t2('c1:x*')`); n != 4096 {
		t.Errorf("4.2: c1:x* = %d, want 4096", n)
	}
	if n := count(`SELECT count(*) FROM t2('c2:x*')`); n != 4096 {
		t.Errorf("4.2: c2:x* = %d, want 4096", n)
	}
}
