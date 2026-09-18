package frigolite_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteRowidAliasNamePin pins rowid.test 1.8-1.10's engine contract: the
// rowid pseudo-column is reachable under all its names (rowid, RowID with any
// casing, _rowid_, oid) and renders the same integer values, so the flat
// result of "SELECT x, <alias> FROM t1 ORDER BY x" equals
// "1 <rowid1> 3 <rowid3>" for every alias spelling. The generated rowid
// package compares the TCL flat list with a raw string equality over a
// newline-joined helper rendering, which cannot pass regardless of the
// engine; the package's engine-visible assertion is pinned here instead.
func TestSQLiteRowidAliasNamePin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t1(x int, y int);\n" +
		"INSERT INTO t1 VALUES(1,2);\n" +
		"INSERT INTO t1 VALUES(3,4)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	// rowid-1.2: the rowids of x=1 and x=3.
	rowids := db.Query("SELECT rowid FROM t1 ORDER BY x")
	if rowids.Error != nil {
		t.Fatalf("rowid query: %v", rowids.Error)
	}
	if len(rowids.Rows) != 2 {
		t.Fatalf("rowid rows: %v", rowids.Rows)
	}
	flat := func(rows [][]interface{}) string {
		parts := make([]string, 0, len(rows)*2)
		for _, row := range rows {
			for _, v := range row {
				parts = append(parts, fmt.Sprintf("%v", v))
			}
		}
		return strings.Join(parts, " ")
	}
	want := "1 " + fmt.Sprintf("%v", rowids.Rows[0][0]) + " 3 " + fmt.Sprintf("%v", rowids.Rows[1][0])
	for _, alias := range []string{"oid", "RowID", "_rowid_", "OID", "_ROWID_"} {
		q := db.Query("SELECT x, " + alias + " FROM t1 order by x")
		if q.Error != nil {
			t.Fatalf("alias %s: %v", alias, q.Error)
		}
		if got := flat(q.Rows); got != want {
			t.Fatalf("alias %s: got [%s] want [%s]", alias, got, want)
		}
	}
}

// TestSQLiteRowidDeclaredColumnDeletePin pins rowid.test 4.2's delete
// contract: a table that DECLARES a column named rowid keeps its rows
// addressable by the true btree rowid — DELETE FROM must remove every row,
// not rows keyed by the declared column's values (which shadow the "rowid"
// name for expression resolution only).
func TestSQLiteRowidDeclaredColumnDeletePin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t2(rowid int, x int, y int);\n" +
		"INSERT INTO t2 VALUES(0,2,3);\n" +
		"INSERT INTO t2 VALUES(4,5,6);\n" +
		"INSERT INTO t2 VALUES(7,8,9)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	if r := db.Exec("DELETE FROM t2"); r.Error != nil {
		t.Fatalf("delete: %v", r.Error)
	}
	q := db.Query("SELECT rowid, x, y, _rowid_ FROM t2")
	if q.Error != nil {
		t.Fatalf("query: %v", q.Error)
	}
	if len(q.Rows) != 0 {
		t.Fatalf("DELETE FROM left rows behind: %v", q.Rows)
	}
}
