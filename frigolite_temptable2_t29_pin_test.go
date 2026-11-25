package frigolite

import "testing"

// TestS64TempPageCount pins the engine-visible contract behind
// temptable2-4.1.1: the harness opens :memory:, so an in-memory main database
// must still report SQLite's temp page count of 9 after the setup SQL. The
// TCL case regressed under the S64 journal finalizer.
func TestS64TempPageCount(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query(`
PRAGMA main.cache_size = 10;
PRAGMA temp.cache_size = 10;

CREATE TEMP TABLE t1(a, b);
CREATE INDEX i1 ON t1(a, b);

WITH x(i) AS ( SELECT 1 UNION ALL SELECT i+1 FROM x WHERE i<10 )
INSERT INTO t1 SELECT randomblob(100), randomblob(100) FROM x;

SELECT count(*) FROM t1;
PRAGMA temp.page_count;`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 2 || len(r.Rows[0]) != 1 || len(r.Rows[1]) != 1 {
		t.Fatalf("rows = %#v, want [[10] [9]]", r.Rows)
	}
	if got, want := r.Rows[0][0], int64(10); got != want {
		t.Errorf("count = %v, want %v", got, want)
	}
	if got, want := r.Rows[1][0], int64(9); got != want {
		t.Errorf("temp.page_count = %v, want %v", got, want)
	}
}
