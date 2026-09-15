package frigolite_test

import (
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteAlter3VacuumPin pins alter3.test 6.x/7.x: after creating main and
// TEMP triggers over a main table (t1_a in sqlite_schema, t1_b in
// sqlite_temp_master) and ALTER TABLE ADD COLUMN, VACUUM rebuilds the schema
// and must tolerate the temp trigger (its ON table lives in main, not temp).
func TestSQLiteAlter3VacuumPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	setup := `
      CREATE TABLE t1(a, b);
      CREATE TABLE log(trig, a, b);

      CREATE TRIGGER t1_a AFTER INSERT ON t1 BEGIN
        INSERT INTO log VALUES('a', new.a, new.b);
      END;
      CREATE TEMP TRIGGER t1_b AFTER INSERT ON t1 BEGIN
        INSERT INTO log VALUES('b', new.a, new.b);
      END;

      INSERT INTO t1 VALUES(1, 2);
      SELECT * FROM log;`
	r := db.Query(setup)
	if r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	if r := db.Exec("ALTER TABLE t1 ADD COLUMN c DEFAULT 'c'"); r.Error != nil {
		t.Fatalf("add column: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO t1(a, b) VALUES(3, 4)"); r.Error != nil {
		t.Fatalf("insert after add column: %v", r.Error)
	}
	if r := db.Exec("VACUUM"); r.Error != nil {
		t.Fatalf("VACUUM with temp trigger over main table: %v", r.Error)
	}
	q := db.Query("SELECT * FROM log ORDER BY rowid")
	if q.Error != nil {
		t.Fatalf("select after vacuum: %v", q.Error)
	}
	// Oracle: the TEMP trigger fires before the main trigger (b then a).
	if got := flattenRows(q.Rows); got != "b 1 2 a 1 2 b 3 4 a 3 4" {
		t.Fatalf("log rows after vacuum: got %q", got)
	}
}
