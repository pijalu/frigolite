package frigolite_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestLockCrossConnPin pins the lock.test 5.x cross-connection contract: a
// UDF registered on db may run SQL on a SECOND connection (db2) from within
// a db statement — db holds the file's RESERVED lock while db2's reads take
// SHARED (pager.c allows SHARED under RESERVED), so the inner reads
// succeed. The transpiled suite cannot express the fixture UDF's TCL body
// (db2 eval $sql), so lock-5.5/5.7/5.9 are evidence-skipped with this
// native port as the engine-correctness pointer.
func TestLockCrossConnPin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db2, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	must := func(cn *frigolite.DB, sql string) {
		t.Helper()
		if r := cn.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	// lock.test fixture state at 5.x: t1=(2,1); t2=(9,8) (x/y swapped at 1.19).
	must(db, "CREATE TABLE t1(a,b)")
	must(db, "INSERT INTO t1 VALUES(1,2),(2,1)")
	must(db, "DELETE FROM t1 WHERE a=1")
	must(db, "CREATE TABLE t2(x,y)")
	must(db, "INSERT INTO t2 VALUES(8,9)")
	must(db, "UPDATE t2 SET x=y, y=x")
	db.RegisterFunction("tx_exec", func(args []interface{}) (interface{}, error) {
		sql, _ := args[0].(string)
		r := db2.Query(sql)
		if r.Error != nil {
			return nil, r.Error
		}
		if len(r.Rows) == 0 || len(r.Rows[0]) == 0 {
			return nil, nil
		}
		return fmt.Sprint(r.Rows[0][0]), nil
	}, 1, 1)
	// 5.2/5.4/5.5: db2 reads from inside db writes yield the t2 values.
	must(db, "INSERT INTO t1(a,b) SELECT 3, tx_exec('SELECT y FROM t2 LIMIT 1')")
	must(db, "CREATE TEMP TABLE t3(x)")
	must(db, "INSERT INTO t3 SELECT tx_exec('SELECT y FROM t2 LIMIT 1')")
	if got := db.Query("SELECT * FROM t3"); got.Error != nil || fmt.Sprint(got.Rows) != "[[8]]" {
		t.Fatalf("5.5: got %v / %v", got.Rows, got.Error)
	}
	// 5.6/5.7: UPDATE driven by db2 reads.
	must(db, "UPDATE t1 SET a=tx_exec('SELECT x FROM t2')")
	if got := db.Query("SELECT * FROM t1"); got.Error != nil || fmt.Sprint(got.Rows) != "[[9 1] [9 8]]" {
		t.Fatalf("5.7: got %v / %v", got.Rows, got.Error)
	}
	// 5.8/5.9.
	must(db, "UPDATE t3 SET x=tx_exec('SELECT x FROM t2')")
	if got := db.Query("SELECT * FROM t3"); got.Error != nil || fmt.Sprint(got.Rows) != "[[9]]" {
		t.Fatalf("5.9: got %v / %v", got.Rows, got.Error)
	}
}

// TestLockStatusPreparedReadPin pins the lock.test 7.2 contract: a prepared
// SELECT stepped but not yet reset/finalized holds its read transaction
// open, so PRAGMA lock_status reports main "shared" until Reset
// (sqlite3_step returning SQLITE_ROW leaves the statement mid-run).
func TestLockStatusPreparedReadPin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t4(a PRIMARY KEY, b)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	st, err := db.Prepare("SELECT * FROM sqlite_master")
	if err != nil {
		t.Fatal(err)
	}
	if more, err := st.Step(); err != nil || !more {
		t.Fatalf("first step: more=%v err=%v", more, err)
	}
	r := db.Query("PRAGMA lock_status")
	if r.Error != nil || r.Rows[0][1] != "shared" {
		t.Fatalf("mid-step lock_status: want main shared, got %v / %v", r.Rows, r.Error)
	}
	if err := st.Reset(); err != nil {
		t.Fatal(err)
	}
	r = db.Query("PRAGMA lock_status")
	if r.Error != nil || r.Rows[0][1] != "unlocked" {
		t.Fatalf("post-reset lock_status: want unlocked, got %v / %v", r.Rows, r.Error)
	}
}
