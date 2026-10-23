package frigolite

import (
	"reflect"
	"testing"
)

// TestChanges2PreparedStatementLifecycle pins the sqlite3_prepare_v2/step/reset
// contract used by changes2.test: changes() remains tied to preceding write,
// reset permits same prepared INSERT to run again, finalize retains both rows.
func TestChanges2PreparedStatementLifecycle(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{"CREATE TABLE t1(a,b)", "CREATE TABLE log(t)", "INSERT INTO t1 VALUES(1,'v1'),(2,'v2')"} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("Exec(%q): %v", sql, r.Error)
		}
	}
	stmt, err := db.Prepare("INSERT INTO log VALUES(changes() || ' changes')")
	if err != nil {
		t.Fatal(err)
	}
	if row, stepErr := stmt.Step(); stepErr != nil || row {
		t.Fatalf("first step = (%v, %v), want SQLITE_DONE", row, stepErr)
	}
	if err := stmt.Reset(); err != nil {
		t.Fatal(err)
	}
	if r := db.Exec("INSERT INTO t1 VALUES(3,'v1'),(4,'v2')"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if row, stepErr := stmt.Step(); stepErr != nil || row {
		t.Fatalf("second step = (%v, %v), want SQLITE_DONE", row, stepErr)
	}
	if err := stmt.Finalize(); err != nil {
		t.Fatal(err)
	}
	r := db.Query("SELECT t FROM log ORDER BY rowid")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got, want := r.Rows, [][]interface{}{{"2 changes"}, {"2 changes"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %#v, want %#v", got, want)
	}

}

// TestPreparedUpdateReturningPreservesChanges verifies DML RETURNING results
// retain sqlite3_changes() while Stmt.Step returns its row and DONE status.
func TestPreparedUpdateReturningPreservesChanges(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t(a,b); INSERT INTO t VALUES(1,'v1')"); r.Error != nil {
		t.Fatal(r.Error)
	}
	stmt, err := db.Prepare("UPDATE t SET b='v2' WHERE a=1 RETURNING a")
	if err != nil {
		t.Fatal(err)
	}
	row, err := stmt.Step()
	if err != nil || !row {
		t.Fatalf("first step = (%v,%v), want row", row, err)
	}
	if got := db.engine.LastChanges(); got != 1 {
		t.Fatalf("changes after RETURNING row = %d, want 1", got)
	}
	row, err = stmt.Step()
	if err != nil || row {
		t.Fatalf("second step = (%v,%v), want DONE", row, err)
	}
	if got := db.engine.LastChanges(); got != 1 {
		t.Fatalf("changes after DONE = %d, want 1", got)
	}
	if err := stmt.Finalize(); err != nil {
		t.Fatal(err)
	}
}
