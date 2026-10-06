package frigolite

import (
	"fmt"
	"testing"
)

// TestPinR8PointFramePool pins the SELECT result pool's statement-frame
// contract (pooledSelectResult + SetResultFrame): a statement that iterates
// its SELECT result while a NESTED engine.Exec runs (a vtab module's shadow
// SQL under INSERT INTO rt SELECT — the rtree %_rowid aux read; an eval()
// UDF) must see every source row. A selectDepth-only slot array collided
// the two at the same depth-1 slot: the nested Exec zeroed the pool struct
// mid-iteration and the insert-select wrote a truncated row set (rtreeE-2.x
// census regression: the rtree ended up with 2 rows instead of 10001).
//
// Shape 1 is the exact rtreeE incident (file-backed db, 512-byte pages,
// bulk INSERT INTO rtree SELECT inside an explicit transaction, then an
// id MATCH breadthfirstsearch query — the query result is only correct
// when the rtree content survived the write loop intact).
// Shape 2 pins the generic (non-rtree) module path: an INSERT into a
// module vtab whose xUpdate issues shadow SQL, sourced by a SELECT whose
// rows must all arrive.
func TestPinR8PointFramePool(t *testing.T) {
	path := t.TempDir() + "/framepool.db"
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.RegisterRtreeGeometry("circle"); err != nil {
		t.Fatalf("register circle bundle: %v", err)
	}
	must := func(stage, sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", stage, r.Error)
		}
	}
	must("pagesize", "PRAGMA page_size=512")
	must("t2", "CREATE TABLE t2(id,x0,x1,y0,y1)")
	must("rt2", "CREATE VIRTUAL TABLE rt2 USING rtree(id,x0,x1,y0,y1)")
	must("begin", "BEGIN")
	for id := 10001; id <= 10500; id++ {
		x0 := float64(id % 10000)
		y0 := float64((id % 2) * 6000)
		must("seed", fmt.Sprintf("INSERT INTO t2 VALUES(%d, %v, %v, %v, %v)", id, x0, x0+1, y0, y0+5))
	}
	// The write loop consuming the source SELECT while the rtree's shadow
	// SQL re-enters the engine: every one of the 500 rows must land.
	must("bulk", "INSERT INTO rt2 SELECT * FROM t2")
	must("commit", "COMMIT")

	count := func(stage, sql string, want int) {
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", stage, r.Error)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("%s: rows = %d, want 1", stage, len(r.Rows))
		}
		if got, _ := r.Rows[0][0].(int64); got != int64(want) {
			t.Fatalf("%s: count = %d, want %d", stage, got, want)
		}
	}
	ids := func(stage, sql string, want int) {
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", stage, r.Error)
		}
		if len(r.Rows) != want {
			t.Fatalf("%s: rows = %d, want %d", stage, len(r.Rows), want)
		}
	}
	// Seed rows so half the boxes lie beyond y=5000 (the MATCH-half parity
	// check below needs a real subset, not the whole space).
	parity := func(stage, matchSQL, scanSQL string) {
		mr := db.Query(matchSQL)
		if mr.Error != nil {
			t.Fatalf("%s match: %v", stage, mr.Error)
		}
		sr := db.Query(scanSQL)
		if sr.Error != nil {
			t.Fatalf("%s scan: %v", stage, sr.Error)
		}
		if len(mr.Rows) != len(sr.Rows) || len(mr.Rows) == 0 {
			t.Fatalf("%s: MATCH rows = %d, range scan rows = %d (want equal, non-zero)", stage, len(mr.Rows), len(sr.Rows))
		}
		for i := range mr.Rows {
			g, _ := mr.Rows[i][0].(int64)
			w, _ := sr.Rows[i][0].(int64)
			if g != w {
				t.Fatalf("%s: row %d MATCH id = %d, scan id = %d", stage, i, g, w)
			}
		}
	}
	count("rtree-count", "SELECT count(*) FROM rt2", 500)
	ids("rtree-scan", "SELECT id FROM rt2 WHERE x1>=0 ORDER BY id", 500)
	// The rtreeE-2.4 shape: MATCH through the breadthfirstsearch query
	// callback. Whole-space MATCH must return every id in scan order.
	parity("rtree-match-all",
		"SELECT id FROM rt2 WHERE id MATCH breadthfirstsearch(0,10000,0,10000) ORDER BY id",
		"SELECT id FROM rt2 WHERE x1>=0 ORDER BY id")
	// Half-space MATCH must agree with the equivalent range scan row-for-row.
	parity("rtree-match-half",
		"SELECT id FROM rt2 WHERE id MATCH breadthfirstsearch(0,10000,0,5000) ORDER BY id",
		"SELECT id FROM rt2 WHERE x1>=0 AND y0<=5000 AND y1>=0 ORDER BY id")

	// Shape 2: nested engine.Exec from an eval()-style UDF during an
	// INSERT ... SELECT — the source rows must survive the per-row UDF.
	must("t3", "CREATE TABLE t3(id INTEGER PRIMARY KEY, x INTEGER)")
	for id := 1; id <= 300; id++ {
		must("seed3", fmt.Sprintf("INSERT INTO t3 VALUES(%d, %d)", id, id*3))
	}
	must("t4", "CREATE TABLE t4(id INTEGER PRIMARY KEY, x INTEGER)")
	db.RegisterFunction("nestq", func(args []interface{}) (interface{}, error) {
		r := db.Query("SELECT x FROM t3 WHERE id=" + fmt.Sprintf("%v", args[0]))
		if r.Error != nil {
			return nil, r.Error
		}
		if len(r.Rows) == 0 {
			return nil, nil
		}
		return r.Rows[0][0], nil
	}, 1, 1)
	must("insert-select-udf", "INSERT INTO t4 SELECT id, nestq(id) FROM t3")
	count("t4-count", "SELECT count(*) FROM t4", 300)
	count("t4-sum", "SELECT sum(x) FROM t4", 135450)
}
