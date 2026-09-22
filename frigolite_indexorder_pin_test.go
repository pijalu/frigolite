package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestIndexScanOrderPin pins the WHERE-driven index scan emission contract
// (oracle: sqlite3 3.54; TCL refs intpkey-1.12.2, intpkey-2.3.2/2.4.1/2.5):
// when a WHERE constraint lets an index drive a single-table scan, the
// surviving rows are emitted in that index's key order (NULLs first,
// rowid-ascending ties), and a rowid / INTEGER PRIMARY KEY constraint makes
// the table b-tree drive instead (rowid order beats the secondary index).
func TestIndexScanOrderPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c)",
		"INSERT INTO t1 VALUES(5,'hello','world')",
		"INSERT INTO t1 VALUES(3,'b','b')",
		"INSERT INTO t1 VALUES(4,'one two','four')",
		"INSERT INTO t1 VALUES(6,'second entry','six')",
		"INSERT INTO t1 VALUES(8,'y','z')",
		"CREATE INDEX i1 ON t1(b)",
		"CREATE TABLE t5(a)",
		"INSERT INTO t5 VALUES(3),(1),(NULL),(2)",
		"CREATE INDEX t5a ON t5(a)",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	rowsText := func(q string) string {
		t.Helper()
		r := db.Query(q)
		if r.Error != nil {
			t.Fatalf("%s => ERR %v", q, r.Error)
		}
		var sb strings.Builder
		for _, row := range r.Rows {
			for j, v := range row {
				if j > 0 {
					sb.WriteByte(' ')
				}
				if v == nil {
					sb.WriteString("{}")
					continue
				}
				sb.WriteString(fmt.Sprintf("%v", v))
			}
			sb.WriteByte('|')
		}
		return sb.String()
	}
	for _, tc := range []struct{ q, want string }{
		// intpkey-2.3.2: index-key order (b binary asc), not insertion order.
		{"SELECT rowid, * FROM t1 WHERE b<'second'", "3 3 b b|5 5 hello world|4 4 one two four|"},
		// intpkey-2.5: the index drives even at full selectivity (no stats).
		{"SELECT rowid, * FROM t1 WHERE b>'a'", "3 3 b b|5 5 hello world|4 4 one two four|6 6 second entry six|8 8 y z|"},
		// intpkey-2.4.1: the rowid range constraint makes the table b-tree
		// drive — rowid order beats index-key order on b ([3,4,5], not
		// [3,5,4]).
		{"SELECT rowid, * FROM t1 WHERE 8>rowid AND 'second'>b", "3 3 b b|4 4 one two four|5 5 hello world|"},
		// NULLs sort first in index-key order (OR-branch keeps the NULL row).
		{"SELECT a FROM t5 WHERE a IS NULL OR a=2", "{}|2|"},
		{"SELECT a FROM t5 WHERE a>=0", "1|2|3|"},
	} {
		if got := rowsText(tc.q); got != tc.want {
			t.Errorf("%s => got [%s] want [%s]", tc.q, got, tc.want)
		}
	}
	// intpkey-1.12.2: INTEGER PRIMARY KEY equality plans a rowid seek.
	r := db.Query("EXPLAIN QUERY PLAN SELECT * FROM t1 WHERE a==4")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var sb strings.Builder
	for _, row := range r.Rows {
		sb.WriteString(fmt.Sprintf("%v", row[0]))
	}
	if got := sb.String(); !strings.Contains(got, "SEARCH t1 USING INTEGER PRIMARY KEY (rowid=?)") {
		t.Errorf("ipk EQP => got [%s] want substring [SEARCH t1 USING INTEGER PRIMARY KEY (rowid=?)]", got)
	}
}
