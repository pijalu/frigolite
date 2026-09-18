package frigolite_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteTriggerDeclaredRowidPin pins triggerD.test 1.1/1.2: a table that
// DECLARES columns named rowid/oid/_rowid_ shadows the pseudo-rowid, so
// trigger references new.rowid / new.oid / new._rowid_ resolve to the
// declared columns' values — in a BEFORE INSERT trigger too, where the
// pseudo-rowid is still unassigned (-1). The engine's plain-SELECT side of
// the same shadowing rule is pinned by TestSQLiteRowidAliasNamePin.
func TestSQLiteTriggerDeclaredRowidPin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t1(rowid, oid, _rowid_, x);\n" +
		"CREATE TABLE log(a, b, c, d, e);\n" +
		"CREATE TRIGGER r1 BEFORE INSERT ON t1 BEGIN\n" +
		"  INSERT INTO log VALUES('r1', new.rowid, new.oid, new._rowid_, new.x);\n" +
		"END;\n" +
		"CREATE TRIGGER r2 AFTER INSERT ON t1 BEGIN\n" +
		"  INSERT INTO log VALUES('r2', new.rowid, new.oid, new._rowid_, new.x);\n" +
		"END"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO t1 VALUES(100, 200, 300, 400)"); r.Error != nil {
		t.Fatalf("insert: %v", r.Error)
	}
	q := db.Query("SELECT * FROM log")
	if q.Error != nil {
		t.Fatalf("log query: %v", q.Error)
	}
	if len(q.Rows) != 2 {
		t.Fatalf("log rows: %v", q.Rows)
	}
	for _, row := range q.Rows {
		if got, want := fmt.Sprintf("%s %v %v %v %v", row[0], row[1], row[2], row[3], row[4]),
			row[0].(string)+" 100 200 300 400"; got != want {
			t.Fatalf("trigger %s row: got [%s] want [%s]", row[0], got, want)
		}
	}
}
