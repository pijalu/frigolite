package frigolite

import (
	"fmt"
	"testing"
)

// TestDerivedTableCollationAndColumnName pins the e_select-1.7.4b.1 contract:
// a derived-table result column written as `a COLLATE nocase` keeps BOTH its
// bare column name (C sqlite3ColumnsFromExprList's
// sqlite3ExprSkipCollateAndLikely) and its NOCASE collation in an outer join
// ON comparison (C sqlite3SubqueryColumnTypes). Before the fix the outer
// query saw `x.a` as an expression-named column with BINARY collation and
// returned zero rows where SQLite returns the case-insensitive matches.
func TestDerivedTableCollationAndColumnName(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	setup := `
CREATE TABLE t5(a COLLATE nocase, b COLLATE binary);
INSERT INTO t5 VALUES('AA', 'cc');
INSERT INTO t5 VALUES('BB', 'dd');
INSERT INTO t5 VALUES(NULL, NULL);
CREATE TABLE t6(a COLLATE binary, b COLLATE nocase);
INSERT INTO t6 VALUES('aa', 'cc');
INSERT INTO t6 VALUES('bb', 'DD');
INSERT INTO t6 VALUES(NULL, NULL);`
	if r := db.Exec(setup); r.Error != nil {
		t.Fatal(r.Error)
	}

	// sqlite3 3.54: PRAGMA short_column_names=ON reports "a" here, and
	// SELECT * FROM (...) AS x, t5 ON (x.a=t5.a) returns exactly two rows.
	name := db.Query("SELECT a COLLATE nocase FROM t6")
	if name.Error != nil {
		t.Fatal(name.Error)
	}
	if cols := name.Columns; len(cols) != 1 || cols[0] != "a" {
		t.Fatalf("derived column name = %v, want [a]", cols)
	}

	q := db.Query(`SELECT * FROM (SELECT a COLLATE nocase, b FROM t6) AS x, t5 ON (x.a=t5.a)`)
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	want := []string{
		"[aa cc AA cc]",
		"[bb DD BB dd]",
	}
	if len(q.Rows) != len(want) {
		t.Fatalf("rows = %v, want %v", q.Rows, want)
	}
	for i, row := range q.Rows {
		if got := fmt.Sprint(row); got != want[i] {
			t.Fatalf("row %d = %s, want %s", i, got, want[i])
		}
	}
}
