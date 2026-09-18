package frigolite_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteAlterConsSetNotNullPin pins altercons.test's ALTER COLUMN SET/DROP
// NOT NULL contract (the corpus test came from a newer trunk tree, so the
// engine behavior is pinned here):
//   - SET NOT NULL on any column of a populated table succeeds when no row
//     holds NULL — including the INTEGER PRIMARY KEY rowid-alias column,
//     whose record slot is always NULL but whose rowid value never is
//     (altercons-8.1.1; mirrors C's record layout);
//   - SET NOT NULL over a row with NULL in the column fails with the standard
//     NOT NULL violation message and the SQLITE_CONSTRAINT result code
//     (altercons-5.2.1/5.2.2, sqlite3_errcode semantics);
//   - the declared schema keeps the SET NOT NULL (a later NULL insert fails).
func TestSQLiteAlterConsSetNotNullPin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY, b NOT NULL, c CHECK (c!=555), d);\n" +
		"INSERT INTO t1 VALUES(1, 1, 1, 1);\n" +
		"INSERT INTO t1 VALUES(2, 2, 2, 2);\n" +
		"INSERT INTO t1 VALUES(3, 3, 3, 3)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}

	// 8.1.1: SET NOT NULL over every column of a populated table, including
	// the rowid-alias column a.
	for _, col := range []string{"a", "b", "c", "d"} {
		if r := db.Exec("ALTER TABLE t1 ALTER " + col + " SET NOT NULL"); r.Error != nil {
			t.Fatalf("SET NOT NULL %s: %v", col, r.Error)
		}
	}
	if r := db.Query("SELECT count(*) FROM t1"); r.Error != nil || len(r.Rows) == 0 {
		t.Fatalf("rows after SET NOT NULL: %v %v", r.Error, r.Rows)
	}

	// 5.2.x: a NULL row blocks SET NOT NULL with SQLITE_CONSTRAINT.
	if r := db.Exec("CREATE TABLE t3(a INTEGER PRIMARY KEY, b);\n" +
		"INSERT INTO t3 VALUES(1000, NULL)"); r.Error != nil {
		t.Fatalf("t3 setup: %v", r.Error)
	}
	r := db.Exec("ALTER TABLE t3 ALTER b SET NOT NULL")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "constraint failed") {
		t.Fatalf("SET NOT NULL with NULL row: got %v, want 'constraint failed'", r.Error)
	}
	if got := db.ErrorCodeFor(r.Error); got != "SQLITE_CONSTRAINT" {
		t.Fatalf("SET NOT NULL with NULL row errcode: got %s want SQLITE_CONSTRAINT", got)
	}
	// The failed ALTER did not persist the constraint.
	if r := db.Exec("INSERT INTO t3 VALUES(1001, NULL)"); r.Error != nil {
		t.Fatalf("INSERT NULL after failed SET NOT NULL: %v", r.Error)
	}

	// The successful SET NOT NULL is enforced afterwards.
	if r := db.Exec("INSERT INTO t1 VALUES(4, 4, 4, NULL)"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "NOT NULL constraint failed") {
		t.Fatalf("NULL into SET-NOT-NULL column: got %v, want NOT NULL violation", r.Error)
	}

	// DROP NOT NULL releases the column again (7.2 shape).
	if r := db.Exec("ALTER TABLE t1 ALTER d DROP NOT NULL"); r.Error != nil {
		t.Fatalf("DROP NOT NULL: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO t1 VALUES(5, 5, 5, NULL)"); r.Error != nil {
		t.Fatalf("INSERT NULL after DROP NOT NULL: %v", r.Error)
	}
}
