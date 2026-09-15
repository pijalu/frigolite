package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteOrderByRangePin pins two ORDER BY resolution contracts from
// tkt2822 (SQLite resolve.c):
//   - an integer ORDER BY term outside 1..nResult — including 0 — errors
//     "Nth ORDER BY term out of range" (resolve.c resolveOrderGroupBy:
//     iCol<1 or iCol>nExpr), while FLOAT literals are ordinary expressions;
//   - a compound SELECT's ORDER BY expression must structurally match a
//     result-column expression (resolveCompoundOrderBy → resolveOrderByTermToExprList):
//     CAST(b AS INTEGER) does not match result column CAST(b AS TEXT).
func TestSQLiteOrderByRangePin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	mustExec("CREATE TABLE t7(a1,a2,a3)")
	mustExec("CREATE TABLE t1(a,b,c)")
	mustExec("INSERT INTO t1 VALUES(1,2,3)")

	r := db.Query("SELECT * FROM t7 ORDER BY 1, 0")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "2nd ORDER BY term out of range - should be between 1 and 3") {
		t.Fatalf("ORDER BY 1, 0: got %v, want out-of-range error", r.Error)
	}
	if r := db.Query("SELECT * FROM t7 ORDER BY 1.5"); r.Error != nil {
		t.Fatalf("ORDER BY 1.5 must be an ordinary expression, got %v", r.Error)
	}
	mustExec("INSERT INTO t1 VALUES(1,2,4)")
	r = db.Query("SELECT a, CAST(b AS TEXT) AS x, c FROM t1 UNION ALL SELECT a, b, c FROM t1 ORDER BY CAST(b AS INTEGER)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "1st ORDER BY term does not match any column in the result set") {
		t.Fatalf("compound ORDER BY CAST mismatch: got %v, want no-match error", r.Error)
	}
	r = db.Query("SELECT a, CAST(b AS TEXT) AS x, c FROM t1 UNION ALL SELECT a, b, c FROM t1 ORDER BY CAST(b AS TEXT)")
	if r.Error != nil {
		t.Fatalf("compound ORDER BY matching result expression must succeed: %v", r.Error)
	}
}
