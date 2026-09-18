package frigolite

// Pins for the FULL-SUITE-DRIFT.T26-fts34 fts4-side engine fixes (oracle
// verified against sqlite3 3.54).

import (
	"strings"
	"testing"
)

// TestPinFTS4UpFromFTS5ScalarWrite pins that an UPDATE ... FROM whose SET
// source resolves to a joined FTS5 row cell writes the plain scalar, not the
// affinity wrapper's Go rendering ("&{apple 0}") — fts4upfrom 1.0.3.
func TestPinFTS4UpFromFTS5ScalarWrite(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE VIRTUAL TABLE ft USING fts5(a, b, c)",
		"INSERT INTO ft(a, b, c) VALUES('a', NULL, 'apple')",
		"INSERT INTO ft(a, b, c) VALUES('b', NULL, 'banana')",
		"INSERT INTO ft(a, b, c) VALUES('c', NULL, 'cherry')",
		"INSERT INTO ft(a, b, c) VALUES('d', NULL, 'damson plum')",
		"UPDATE ft SET b=o.c FROM ft AS o WHERE (ft.a == char(unicode(o.a)+1))",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	r := db.Query("SELECT a, b, c FROM ft ORDER BY rowid")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	got := flattenRowsP2(r.Rows)
	want := "a <nil> apple b apple banana c banana cherry d cherry damson plum"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestPinFTS4ContentHoleKeepsIntactDocs pins crash tolerance after a direct
// %_content delete (e_fts3 10.1.x): the intact document still serves its text
// (the reload restores document text from %_content), and the document whose
// content row is gone reports "database disk image is malformed" on read.
func TestPinFTS4ContentHoleKeepsIntactDocs(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE VIRTUAL TABLE ta USING fts3",
		"INSERT INTO ta VALUES('During a summer vacation in 1790')",
		"INSERT INTO ta VALUES('Wordsworth went on a walking tour')",
		"DELETE FROM ta_content WHERE rowid = 2",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	r := db.Query("SELECT * FROM ta WHERE ta MATCH 'summer'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	got := flattenRowsP2(r.Rows)
	if got != "During a summer vacation in 1790" {
		t.Fatalf("intact doc text lost: %q", got)
	}
	r2 := db.Query("SELECT * FROM ta WHERE ta MATCH 'walking'")
	if r2.Error == nil || !strings.Contains(r2.Error.Error(), "database disk image is malformed") {
		t.Fatalf("missing content row must report malformed, got %v", r2.Error)
	}
}

// TestPinFTS4SelfReferentialTVFReadFails pins the TVF form on an FTS3/4
// table (fts4content 12.1.3): FROM t1('abc') flows through the FTS scan, so
// a self-referential content source fails the read with "SQL logic error"
// instead of resolve-time "'t1' is not a function".
func TestPinFTS4SelfReferentialTVFReadFails(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE VIRTUAL TABLE t1 USING fts4(a, content=t1)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query("SELECT * FROM t1('abc')")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "SQL logic error") {
		t.Fatalf("expected SQL logic error, got %v", r.Error)
	}
}

// TestPinFTS4ViewRowidNotAmbiguous pins that a rowid-shadowing view operand
// keeps its qualified rowid reference unambiguous: the ambiguity map must
// count the declared rowid column of a view once — the implicit
// rowid/_rowid_/oid pseudo-columns are not added a second time for the same
// operand (fts4upfrom 1.3.8: WITH x1 ... SELECT ft.rowid ... FROM ft, x1
// where ft = CREATE VIEW ft AS SELECT rowid, a, b, c FROM real).
func TestPinFTS4ViewRowidNotAmbiguous(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE real(a, b, c)",
		"INSERT INTO real VALUES('apple', 'x', 'y')",
		"CREATE VIEW ft AS SELECT rowid, a, b, c FROM real",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	r := db.Query("WITH x1(o, n) AS (VALUES(1, 11)) SELECT ft.rowid, a, o, n FROM ft, x1 WHERE ft.rowid = o")
	if r.Error != nil {
		t.Fatalf("qualified view rowid must not be ambiguous: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("expected 1 row, got %v", r.Rows)
	}
}
