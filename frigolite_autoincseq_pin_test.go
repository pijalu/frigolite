// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import "testing"

// TestAutoincSeqWritePin pins the AUTOINCREMENT → sqlite_sequence write on
// the shapes the insert statement-end-hook gate must not miss:
//
//  1. the FIRST insert into a freshly created AUTOINCREMENT table (the gate
//     runs before this connection has parsed the table's column
//     definitions — the CREATE TABLE DDL does not populate that cache), and
//  2. the first insert after ANY intervening DDL (DDL wipes the parsed
//     column-definition cache wholesale, re-arming the same cold ask).
//
// insert.c: every AUTOINCREMENT insert records the largest rowid ever used
// in sqlite_sequence at statement end; a dropped write leaves zero/stale
// rows ("SELECT * FROM sqlite_sequence" → [] instead of [t1 12]).
//
// REGRESSION (fleet/r9-insert merge, fixed in fleet/fix-r9ins): the gate's
// autoincrement verdict memo cached a cold not-derivable-yet negative under
// the schema fingerprint, and the hook-free fast path then skipped the
// statement-end sequence write — sqlite_sequence stayed empty for the whole
// connection. The gate now asks the conservative form
// (TableMayHaveAutoIncrement: positive or not-yet-derivable); the
// authoritative verdict (tableHasAutoIncrement, post-ParseColumnDefs)
// decides the actual work.
func TestAutoincSeqWritePin(t *testing.T) {
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
	seqRows := func(sql string) int {
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		return len(r.Rows)
	}
	seqValue := func(sql string) interface{} {
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		if len(r.Rows) == 0 || len(r.Rows[0]) == 0 {
			t.Fatalf("%s: no rows", sql)
		}
		return r.Rows[0][0]
	}

	// Shape 1: first insert into a brand-new AUTOINCREMENT table.
	must("CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b TEXT)")
	must("INSERT INTO t1(b) VALUES('x')")
	must("INSERT INTO t1(b) VALUES('y')")
	must("INSERT INTO t1(b) VALUES('z')")
	if got := seqRows("SELECT * FROM sqlite_sequence"); got != 1 {
		t.Fatalf("shape1: sqlite_sequence rows = %d, want 1", got)
	}
	if got := seqValue("SELECT seq FROM sqlite_sequence WHERE name = 't1'"); got != int64(3) {
		t.Fatalf("shape1: t1 seq = %v, want 3", got)
	}

	// Shape 2: first insert after intervening DDL (cache wipe re-arms the
	// gate's cold ask).
	must("CREATE TABLE noise(x)")
	must("INSERT INTO t1(b) VALUES('w')")
	if got := seqValue("SELECT seq FROM sqlite_sequence WHERE name = 't1'"); got != int64(4) {
		t.Fatalf("shape2: t1 seq = %v, want 4", got)
	}

	// Shape 3: explicit rowid bumps the sequence to the largest rowid ever
	// used (insert.c: sqlite3AutoincrementEnd records max(seq, max rowid)).
	must("INSERT INTO t1(a, b) VALUES(10000, 'big')")
	if got := seqValue("SELECT seq FROM sqlite_sequence WHERE name = 't1'"); got != int64(10000) {
		t.Fatalf("shape3: t1 seq = %v, want 10000", got)
	}

	// Shape 4: a new AUTOINCREMENT table created and populated in one
	// multi-statement batch (the CREATE's own DDL wipe precedes the insert).
	must("CREATE TABLE t2(id INTEGER PRIMARY KEY AUTOINCREMENT, v INTEGER); INSERT INTO t2(v) VALUES(7)")
	if got := seqValue("SELECT seq FROM sqlite_sequence WHERE name = 't2'"); got != int64(1) {
		t.Fatalf("shape4: t2 seq = %v, want 1", got)
	}
	if got := seqRows("SELECT name FROM sqlite_sequence"); got != 2 {
		t.Fatalf("final: sqlite_sequence rows = %d, want 2", got)
	}
}
