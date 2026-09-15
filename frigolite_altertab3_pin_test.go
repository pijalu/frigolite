package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteAlterTab3GenColumnPin pins altertab3.test 27.x and view.test
// 2.x: (a) a generated column whose expression contains a subquery/CTE is
// accepted at CREATE TABLE time (SQLite only PARSES the expression there —
// "subqueries prohibited in generated columns" fires when the column is
// evaluated, not when the table is created); (b) CREATE INDEX on a view is
// rejected with "views may not be indexed" (build.c), which requires the
// index target lookup to see views.
func TestSQLiteAlterTab3GenColumnPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// altertab3-27.1: WITH-in-generated-column CREATE succeeds.
	if r := db.Exec("CREATE TABLE t1(a, b AS ((WITH w1 (xyz) AS (SELECT t1.b FROM t1) SELECT 123) IN ()), c)"); r.Error != nil {
		t.Fatalf("CREATE TABLE with WITH-generated column: %v", r.Error)
	}

	// view-2.x: CREATE INDEX on a view must report "views may not be indexed".
	if r := db.Exec("CREATE VIEW v1 AS SELECT a FROM t1"); r.Error != nil {
		t.Fatalf("CREATE VIEW: %v", r.Error)
	}
	r := db.Exec("CREATE INDEX i1v1 ON v1(a)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "views may not be indexed") {
		t.Fatalf("CREATE INDEX on view: got %v, want 'views may not be indexed'", r.Error)
	}
}
