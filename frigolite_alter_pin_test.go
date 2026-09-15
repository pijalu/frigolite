package frigolite_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteAlterViewPin pins alter.test alter-12.x / alter-15.x: ALTER TABLE
// resolves views and system tables before rejecting the operation — a view
// target reports "view %s may not be altered" (RENAME) or "Cannot add a
// column to a view" (ADD COLUMN, alter.c sqlite3AlterFinishAddColumn), and
// schema/ stat tables report "table %s may not be altered" for both forms.
func TestSQLiteAlterViewPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t12(a, b, c); CREATE VIEW v1 AS SELECT * FROM t12"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}

	// alter-12.2: RENAME of a view is found, then rejected.
	r := db.Exec("ALTER TABLE v1 RENAME TO v2")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "view v1 may not be altered") {
		t.Fatalf("RENAME view: got %v, want 'view v1 may not be altered'", r.Error)
	}
	// alter-12.3: the view is untouched after the failed rename.
	if r := db.Query("SELECT * FROM v1"); r.Error != nil {
		t.Fatalf("SELECT from v1 after failed rename: %v", r.Error)
	}
	// alter-12.5: ADD COLUMN on a view.
	if r := db.Exec("ALTER TABLE v1 ADD COLUMN new_column"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "Cannot add a column to a view") {
		t.Fatalf("ADD COLUMN view: got %v, want 'Cannot add a column to a view'", r.Error)
	}
	// alter-15.x: system tables reject both forms. ANALYZE materializes
	// sqlite_stat1 first (the generated loop runs it before the foreach).
	if r := db.Exec("ANALYZE"); r.Error != nil {
		t.Fatalf("ANALYZE: %v", r.Error)
	}
	for _, tbl := range []string{"sqlite_master", "sqlite_stat1"} {
		r := db.Exec("ALTER TABLE " + tbl + " RENAME TO xyz")
		if r.Error == nil || !strings.Contains(r.Error.Error(), "table "+tbl+" may not be altered") {
			t.Fatalf("RENAME %s: got %v, want 'table %s may not be altered'", tbl, r.Error, tbl)
		}
		r = db.Exec("ALTER TABLE " + tbl + " ADD COLUMN xyz")
		if r.Error == nil || !strings.Contains(r.Error.Error(), "table "+tbl+" may not be altered") {
			t.Fatalf("ADD COLUMN %s: got %v, want 'table %s may not be altered'", tbl, r.Error, tbl)
		}
	}
}

// TestSQLiteAlterRenameTriggerBodyPin pins alter.test alter-17.100 and
// altertab2.test 1.x: ALTER TABLE RENAME succeeds when a trigger body
// references OTHER existing tables (including virtual tables) — unqualified
// columns in the body resolve against those body tables (rtree's "id"
// column), and the rename rewrites trigger bodies that reference the renamed
// virtual table without requiring it to resolve mid-flight (SQLite
// rename.c unmapping parse tolerates a schema in flux).
func TestSQLiteAlterRenameTriggerBodyPin(t *testing.T) {
	// alter-17.100: trigger on t1 deletes from rtree t2 by its "id" column.
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	setup := `
    CREATE TABLE t1(a INTEGER PRIMARY KEY, b);
    CREATE VIRTUAL TABLE t2 USING rtree(id,x0,x1);
    INSERT INTO t1 VALUES(1,'apple'),(2,'fig'),(3,'pear');
    INSERT INTO t2 VALUES(1,1.0,2.0),(2,2.0,3.0),(3,1.5,3.5);
    CREATE TRIGGER r1 AFTER UPDATE ON t1 BEGIN
      DELETE FROM t2 WHERE id = OLD.a;
    END;
    ALTER TABLE t1 RENAME TO t3;
    UPDATE t3 SET b='peach' WHERE a=2;
    SELECT * FROM t2 ORDER BY 1;`
	r := db.Query(setup)
	if r.Error != nil {
		t.Fatalf("rtree trigger rename: %v", r.Error)
	}
	// Oracle renders rtree floats as "1.0"/"2.0"; Go's %v prints "1"/"2".
	// Row id=2 must be gone (the trigger fired through the renamed table).
	if got := flattenRows(r.Rows); got != "1 1 2 3 1.5 3.5" {
		t.Fatalf("rtree trigger rename rows: got %q", got)
	}

	// altertab2-1.x: rename an fts5 vtab whose rows are fed by a trigger.
	db2, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db2.Close() })
	setup2 := `
    CREATE TABLE rr(a, b);
    CREATE VIRTUAL TABLE ff USING fts5(a, b);
    CREATE TRIGGER tr1 AFTER INSERT ON rr BEGIN
      INSERT INTO ff VALUES(new.a, new.b);
    END;
    INSERT INTO rr VALUES('hello', 'world');
    ALTER TABLE ff RENAME TO ffff;
    INSERT INTO rr VALUES('in', 'tcl');
    SELECT * FROM ffff;`
	r2 := db2.Query(setup2)
	if r2.Error != nil {
		t.Fatalf("fts5 vtab rename with trigger: %v", r2.Error)
	}
	if got := flattenRows(r2.Rows); got != "hello world in tcl" {
		t.Fatalf("fts5 vtab rename rows: got %q", got)
	}
	// The trigger body must follow the rename.
	tr := db2.Query("SELECT sql FROM sqlite_schema WHERE name='tr1'")
	if tr.Error != nil || len(tr.Rows) != 1 {
		t.Fatalf("trigger sql lookup: %v", tr.Error)
	}
	if s, _ := tr.Rows[0][0].(string); !strings.Contains(s, "ffff") {
		t.Fatalf("trigger body not rewritten: %q", s)
	}
}

func flattenRows(rows [][]interface{}) string {
	parts := make([]string, 0, len(rows)*4)
	for _, row := range rows {
		for _, v := range row {
			parts = append(parts, fmt.Sprintf("%v", v))
		}
	}
	return strings.Join(parts, " ")
}
