package frigolite_test

import (
	"testing"

	frigo "github.com/pijalu/frigolite"
)

// TestUpdateOrFailKeepsPriorRows pins SQLite's ON CONFLICT FAIL semantics
// for UPDATE (check-6.5/6.6): rows updated before the violating row survive
// the failed statement. Regression guard for the P1 change-detection gate —
// its first cut skipped the row WRITE (not just the uniqueness scan) inside
// runUpdateFail whenever no constrained column changed, so an UPDATE OR
// FAIL on a table without unique constraints applied nothing.
func TestUpdateOrFailKeepsPriorRows(t *testing.T) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must("CREATE TABLE t1(x, y, CHECK(x<5))")
	must("INSERT INTO t1 VALUES(4,11.0)")
	must("INSERT INTO t1 VALUES(2,20.0)")

	r := db.Exec("UPDATE OR FAIL t1 SET x=7-x, y=y+1")
	if r.Error == nil || r.Error.Error() != "CHECK constraint failed: x<5" {
		t.Fatalf("expected CHECK violation, got %v", r.Error)
	}
	q := db.Query("SELECT x, y FROM t1 ORDER BY x DESC")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	want := [][]interface{}{{int64(3), 12.0}, {int64(2), 20.0}}
	if len(q.Rows) != len(want) {
		t.Fatalf("rows = %v, want %v", q.Rows, want)
	}
	for i, row := range q.Rows {
		x, xok := asInt(row[0])
		y, yok := asFloat(row[1])
		wx, wok := asInt(want[i][0])
		wy, wyok := asFloat(want[i][1])
		if !xok || !yok || !wok || !wyok || x != wx || y != wy {
			t.Fatalf("row %d = %v, want %v", i, row, want[i])
		}
	}
}

func asInt(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

func asFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	}
	return 0, false
}
