package frigolite

import "testing"

// TestNativeXferOptCounter pins the INSERT-transfer-optimization counter
// (SQLITE_TEST's sqlite3_xferopt_count) through the public API: the counter
// increments when insert.c's xferOptimization decision passes — identical
// schemas with equal CHECK constraints transfer (3.1), CHECK mismatches do
// not (1.1's t1(b>a) vs t2), RETURNING blocks it (10.4), and the counter is
// plan-time (a later constraint failure does not undo it). Expectations
// mirror ori/sqlite/test/insert4.test.
func TestNativeXferOptCounter(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		t.Helper()
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	must(`CREATE TABLE t1(a int, b int CHECK(b>a))`)
	must(`CREATE TABLE t2(x int, y int)`)
	must(`CREATE TABLE t3(a int, b int CHECK(b>a))`)
	must(`CREATE TABLE t4(a int, b int)`)
	must(`INSERT INTO t3 VALUES(1, 9)`)
	must(`INSERT INTO t4 VALUES(1, 9)`)

	// 1.1/1.2: t1's CHECK differs from t2's (absent) — no transfer. A
	// violating row makes the row-by-row insert fail the CHECK exactly
	// like insert4-1.1 (t2 starts empty otherwise).
	must(`INSERT INTO t2 VALUES(9, 1)`)
	db.engine.ResetXferoptCount()
	if res := db.Exec(`INSERT INTO t1 SELECT * FROM t2`); res.Error == nil {
		t.Fatalf("expected a CHECK failure for t1<-t2")
	}
	if got := db.engine.XferoptCount(); got != 0 {
		t.Fatalf("t1<-t2 (mismatched CHECK): xferopt = %d, want 0", got)
	}
	// 3.1: identical CHECK constraints transfer.
	db.engine.ResetXferoptCount()
	must(`INSERT INTO t1 SELECT * FROM t3`)
	if got := db.engine.XferoptCount(); got != 1 {
		t.Fatalf("t1<-t3 (equal CHECKs): xferopt = %d, want 1", got)
	}
	// 3.3: a dest with no CHECK accepts a src that has one.
	db.engine.ResetXferoptCount()
	must(`INSERT INTO t4 SELECT * FROM t3`)
	if got := db.engine.XferoptCount(); got != 1 {
		t.Fatalf("t4<-t3 (no dest CHECK): xferopt = %d, want 1", got)
	}
	// 10.4: RETURNING requires the row-by-row writer.
	db.engine.ResetXferoptCount()
	if res := db.Exec(`INSERT INTO t4 SELECT * FROM t3 RETURNING *`); res.Error != nil {
		t.Fatalf("RETURNING insert: %v", res.Error)
	}
	if got := db.engine.XferoptCount(); got != 0 {
		t.Fatalf("RETURNING insert: xferopt = %d, want 0", got)
	}
	// The rows transferred correctly through the ordinary writer: t4
	// accumulated the direct row plus the two transferred copies.
	res := db.Query(`SELECT count(*) FROM t4`)
	if len(res.Rows) != 1 || formatHarnessValue("", res.Rows[0][0]) != "3" {
		t.Fatalf("t4 count = %v, want 3", res.Rows)
	}
}
