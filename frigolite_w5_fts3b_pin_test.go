package frigolite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/util"
)

// FULL-SUITE-DRIFT.T30-fts3b evidence file.
//
// Pins the engine fixes made for the regenerated testgen corpus (emitter
// got/want symmetry activated previously-suppressed assertions):
//
//   - NEAR pairing is STRICT (fts3near): fts3.c fts3PoslistPhraseMerge pairs
//     positions only when pos2 > pos1 (its exact clause iPos2==iPos1+nToken is
//     subsumed for every nToken >= 1), so a phrase can never pair with itself
//     and a NEAR whose operands never co-occur within the distance matches no
//     document. Filter, matchinfo and offsets must agree: a row the filter
//     accepts but whose phrases have no participating pair makes offsets()
//     return NULL for that row (fts3near 2.6's extra NULL row).
//   - Deleting the LAST document wipes the index (fts3d): fts3_write.c
//     fts3DeleteByRowid runs SQL_IS_EMPTY and, when the delete leaves the
//     table empty, fts3DeleteAll clears the pending-terms hash and every
//     shadow table — so the delete-marker flush writes nothing and the next
//     INSERT starts a fresh (level 0, idx 0) segment.
//   - A zero-length NON-NULL %_segdir root is malformed (fts3corrupt 2.2/3.2
//     and the merge=1 crafted row): a node buffer that cannot hold even one
//     varint cannot be a leaf or interior node. NULL stays the "empty
//     segment" marker (fts3corrupt4 6.1: MATCH succeeds).
//   - The multilanguage bitmask table (fts3ab) and the unicode61 tokenizer's
//     case folding + diacritics removal (fts4unicode) pin the engine behavior
//     behind the two transpiler fixes ([set $lang] dynamic variable reads and
//     [array names MAP] key enumeration), so the engine contract survives
//     emitter churn.
//
// Every expectation below was validated against /usr/bin/sqlite3 3.54.0
// (enhanced query syntax) and a 3.51.0 release build compiled WITHOUT
// SQLITE_ENABLE_FTS3_PARENTHESIS (the legacy syntax the TCL suite runs under
// — tester.tcl:2619 pins sqlite_fts3_enable_parentheses 0). NEAR queries are
// syntax-identical in both modes.

func w5fts3bOpen(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func w5fts3bExec(t *testing.T, db *DB, sql string) {
	t.Helper()
	if res := db.Exec(sql); res.Error != nil {
		t.Fatalf("exec %q: %v", sql, res.Error)
	}
}

func w5fts3bRows(t *testing.T, db *DB, sql string) [][]interface{} {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		t.Fatalf("query %q: %v", sql, res.Error)
	}
	return res.Rows
}

func w5fts3bRowIDs(t *testing.T, db *DB, sql string) []string {
	t.Helper()
	var out []string
	for _, row := range w5fts3bRows(t, db, sql) {
		for _, v := range row {
			out = append(out, strings.TrimSpace(util.SQLiteValueString(v)))
		}
	}
	return out
}

// TestW5FTS3BNearStrictPairing pins fts3PoslistPhraseMerge's strict pairing.
// 'four NEAR four' with at most one 'four' per document matches nothing
// (fts3near 1.14); it matches once a document holds two occurrences within
// the default span; widening the distance to NEAR/2 keeps the co-occurring
// document but excludes the 'A X B C D A B' style documents whose As are 5
// tokens apart — and offsets() never emits a NULL row for a matched document
// because the filter and the aux-function pairing are the same code
// (fts3near 2.6).
func TestW5FTS3BNearStrictPairing(t *testing.T) {
	db := w5fts3bOpen(t)
	w5fts3bExec(t, db, `CREATE VIRTUAL TABLE t1 USING fts3(content);
INSERT INTO t1(docid, content) VALUES(1, 'one two three');
INSERT INTO t1(docid, content) VALUES(2, 'two three four five');
INSERT INTO t1(docid, content) VALUES(3, 'three four five');
INSERT INTO t1(docid, content) VALUES(4, 'A X B C D A B');`)

	if got := w5fts3bRowIDs(t, db, `SELECT docid FROM t1 WHERE t1 MATCH 'four NEAR four'`); len(got) != 0 {
		t.Fatalf("'four NEAR four' with one occurrence per doc: got %v, want no rows", got)
	}
	w5fts3bExec(t, db, `INSERT INTO t1(docid, content) VALUES(5, 'four near four');`)
	if got := w5fts3bRowIDs(t, db, `SELECT docid FROM t1 WHERE t1 MATCH 'four NEAR four'`); len(got) != 1 || got[0] != "5" {
		t.Fatalf("'four NEAR four' with a co-occurring doc: got %v, want [5]", got)
	}
	// NEAR/2: only doc 5 pairs (its 'near'/'four' positions are within 2);
	// doc 4's As are 5 tokens apart and doc 4 has no other pair.
	got := w5fts3bRowIDs(t, db, `SELECT docid FROM t1 WHERE t1 MATCH 'A NEAR/2 A'`)
	if len(got) != 0 {
		t.Fatalf("'A NEAR/2 A' over docs with distant As: got %v, want no rows", got)
	}
	w5fts3bExec(t, db, `INSERT INTO t1(docid, content) VALUES(6, 'A A A');`)
	got = w5fts3bRowIDs(t, db, `SELECT docid FROM t1 WHERE t1 MATCH 'A NEAR/2 A'`)
	if len(got) != 1 || got[0] != "6" {
		t.Fatalf("'A NEAR/2 A' after adding a true pair: got %v, want [6]", got)
	}
	// offsets() agrees with the filter: exactly one row, non-NULL offsets.
	rows := w5fts3bRows(t, db, `SELECT offsets(t1) FROM t1 WHERE t1 MATCH 'A NEAR/2 A'`)
	if len(rows) != 1 {
		t.Fatalf("offsets rows: got %d, want 1", len(rows))
	}
	if v, ok := rows[0][0].(string); !ok || !strings.Contains(v, "0 0 0") {
		t.Fatalf("offsets content: got %v", rows[0][0])
	}
}

// TestW5FTS3BDeleteAllWipesIndex pins fts3DeleteByRowid's empty-table
// shortcut: deleting the last document discards the pending delete markers
// and every shadow table, so %_segdir is empty afterwards and the next INSERT
// rebuilds the index at (level 0, idx 0) — not at the next free idx.
func TestW5FTS3BDeleteAllWipesIndex(t *testing.T) {
	db := w5fts3bOpen(t)
	w5fts3bExec(t, db, `CREATE VIRTUAL TABLE t1 USING fts3(c);
INSERT INTO t1 (docid, c) VALUES (1, 'This is a test');
INSERT INTO t1 (docid, c) VALUES (2, 'That was a test');
INSERT INTO t1 (docid, c) VALUES (3, 'This is a test');`)
	if got := len(w5fts3bRows(t, db, `SELECT level, idx FROM t1_segdir`)); got != 3 {
		t.Fatalf("segdir rows after 3 inserts: got %d, want 3", got)
	}
	w5fts3bExec(t, db, `DELETE FROM t1 WHERE 1=1`)
	if got := len(w5fts3bRows(t, db, `SELECT level, idx FROM t1_segdir`)); got != 0 {
		t.Fatalf("segdir rows after deleting every row: got %d, want 0", got)
	}
	w5fts3bExec(t, db, `INSERT INTO t1 (docid, c) VALUES (1, 'This is a test')`)
	rows := w5fts3bRows(t, db, `SELECT level, idx FROM t1_segdir ORDER BY level, idx`)
	if len(rows) != 1 {
		t.Fatalf("segdir rows after the re-INSERT: got %d, want 1", len(rows))
	}
	if util.SQLiteValueString(rows[0][0]) != "0" || util.SQLiteValueString(rows[0][1]) != "0" {
		t.Fatalf("re-INSERT segment: got %v/%v, want 0/0", rows[0][0], rows[0][1])
	}
	// The re-inserted document is fully queryable again.
	if got := w5fts3bRowIDs(t, db, `SELECT docid FROM t1 WHERE t1 MATCH 'test'`); len(got) != 1 || got[0] != "1" {
		t.Fatalf("MATCH after wipe + re-INSERT: got %v, want [1]", got)
	}
}

// TestW5FTS3BEmptySegdirRootMalformed pins the zero-length non-NULL root
// rule: MATCH (and the merge command's validation) must fail with
// "database disk image is malformed" after UPDATE t1_segdir SET root = ”,
// while a NULL root keeps its "empty segment" meaning and MATCHes cleanly
// (fts3corrupt4 6.1).
func TestW5FTS3BEmptySegdirRootMalformed(t *testing.T) {
	db := w5fts3bOpen(t)
	w5fts3bExec(t, db, `CREATE VIRTUAL TABLE t1 USING fts3;
INSERT INTO t1 VALUES('hello');
INSERT INTO t1 VALUES('world');`)
	w5fts3bExec(t, db, `UPDATE t1_segdir SET root = ''`)
	res := db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH 'hello'`)
	if res.Error == nil || !strings.Contains(res.Error.Error(), "database disk image is malformed") {
		t.Fatalf("MATCH with root='': got error %v, want database disk image is malformed", res.Error)
	}
	// NULL root keeps the empty-segment semantics (fts3corrupt4 6.1).
	db2 := w5fts3bOpen(t)
	w5fts3bExec(t, db2, `CREATE VIRTUAL TABLE t0 USING fts3(a)`)
	w5fts3bExec(t, db2, `INSERT INTO t0_segdir VALUES(1,NULL,1,NULL,NULL,NULL)`)
	if res := db2.Query(`SELECT count(*) FROM t0 WHERE t0 MATCH 'zzz'`); res.Error != nil {
		t.Fatalf("MATCH with NULL root: got error %v, want clean EOF", res.Error)
	}
}

// TestW5FTS3BMultiLanguageBitmaskTable pins the fts3ab engine contract: the
// corpus's fill_multilanguage_fulltext_t1 fills row i's three columns with
// the language words for the bits of i. The transpiler emitted that data
// prep with the literal column names until [set $lang] became a dynamic
// registry read, so the engine behavior is pinned here natively.
func TestW5FTS3BMultiLanguageBitmaskTable(t *testing.T) {
	db := w5fts3bOpen(t)
	langs := []string{"one two three four five", "un dos tres cuatro cinco", "eine zwei drei vier funf"}
	var sb strings.Builder
	sb.WriteString(`CREATE VIRTUAL TABLE t1 USING fts3(english,spanish,german);`)
	for i := 1; i <= 31; i++ {
		words := func(wordlist string) string {
			parts := strings.Fields(wordlist)
			var sel []string
			for j, k := 0, 1; j < 5; j, k = j+1, k*2 {
				if k&i != 0 {
					sel = append(sel, parts[j])
				}
			}
			return strings.Join(sel, " ")
		}
		sb.WriteString("INSERT INTO t1(english,spanish,german) VALUES('" + words(langs[0]) + "','" + words(langs[1]) + "','" + words(langs[2]) + "');")
	}
	w5fts3bExec(t, db, sb.String())

	odd := func() string {
		var out []string
		for i := 1; i <= 31; i += 2 {
			out = append(out, util.SQLiteValueString(i))
		}
		return strings.Join(out, " ")
	}()
	if got := strings.Join(w5fts3bRowIDs(t, db, `SELECT rowid FROM t1 WHERE english MATCH 'one'`), " "); got != odd {
		t.Fatalf("english MATCH 'one': got [%s], want [%s]", got, odd)
	}
	if got := strings.Join(w5fts3bRowIDs(t, db, `SELECT rowid FROM t1 WHERE spanish MATCH 'one'`), " "); got != "" {
		t.Fatalf("spanish MATCH 'one': got [%s], want empty", got)
	}
	// 'one dos drei' spans english+spanish+german: only rows 7/15/23/31 hold
	// all three words.
	if got := strings.Join(w5fts3bRowIDs(t, db, `SELECT rowid FROM t1 WHERE t1 MATCH 'one dos drei'`), " "); got != "7 15 23 31" {
		t.Fatalf("t1 MATCH 'one dos drei': got [%s], want [7 15 23 31]", got)
	}
	row := w5fts3bRows(t, db, `SELECT english, spanish, german FROM t1 WHERE rowid=1`)
	if len(row) != 1 {
		t.Fatalf("rowid=1 rows: got %d, want 1", len(row))
	}
	for i, want := range []string{"one", "un", "eine"} {
		if got := util.SQLiteValueString(row[0][i]); got != want {
			t.Fatalf("rowid=1 column %d: got %q, want %q", i, got, want)
		}
	}
}

// TestW5FTS3BUnicode61Diacritics pins the fts4unicode engine contract behind
// the transpiler's [array names] fix: the unicode61 tokenizer folds case and
// removes diacritics (remove_diacritics defaults on), so a query matches the
// accented document regardless of which side carries the accents — oracle
// /usr/bin/sqlite3 3.54: both 'ol francais' and 'Öl' hit the 'Öl Français'
// row.
func TestW5FTS3BUnicode61Diacritics(t *testing.T) {
	db := w5fts3bOpen(t)
	w5fts3bExec(t, db, `CREATE VIRTUAL TABLE uni USING fts4(tokenize=unicode61, x)`)
	w5fts3bExec(t, db, `INSERT INTO uni VALUES('Öl Français')`)
	for _, q := range []string{"ol", "français", "Öl", "FRANCAIS"} {
		if got := w5fts3bRowIDs(t, db, `SELECT rowid FROM uni WHERE uni MATCH '`+q+`'`); len(got) != 1 || got[0] != "1" {
			t.Fatalf("unicode61 MATCH %q: got %v, want [1]", q, got)
		}
	}
}
