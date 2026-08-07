package frigolite

import (
	"strings"
	"testing"
)

// TestP2ViewBasic verifies CREATE VIEW and SELECT * FROM view, including
// the stored definition in sqlite_master. Oracle: sqlite3 CLI.
func TestP2ViewBasic(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a,b,c);
		INSERT INTO t1 VALUES(1,2,3),(4,5,6),(7,8,9);
		CREATE VIEW v1 AS SELECT a,b FROM t1;
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM v1 ORDER BY a;")
	want := "1 2 4 5 7 8"
	if got := flattenRows(rows); got != want {
		t.Errorf("view select got %q want %q", got, want)
	}
	// sqlite_master stores the view definition.
	rows = p2QueryRows(t, db, "SELECT type, name, sql FROM sqlite_master WHERE name='v1';")
	got := flattenRows(rows)
	want = "view v1 CREATE VIEW v1 AS SELECT a,b FROM t1"
	if got != want {
		t.Errorf("sqlite_master view got %q want %q", got, want)
	}
}

// TestP2ViewColumnList verifies CREATE VIEW with an explicit column list
// and that the declared names are used for resolution.
func TestP2ViewColumnList(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a,b,c);
		INSERT INTO t1 VALUES(1,2,3),(4,5,6);
		CREATE VIEW v1c(x,y) AS SELECT a, b FROM t1;
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM v1c ORDER BY x;")
	if got, want := flattenRows(rows), "1 2 4 5"; got != want {
		t.Errorf("column-list view got %q want %q", got, want)
	}
	// Declared column names are usable in the outer query.
	rows = p2QueryRows(t, db, "SELECT y FROM v1c WHERE x=4;")
	if got, want := flattenRows(rows), "5"; got != want {
		t.Errorf("column-list resolution got %q want %q", got, want)
	}
}

// TestP2ViewOverJoin verifies views over joins.
func TestP2ViewOverJoin(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a,b);
		CREATE TABLE t2(x,y);
		INSERT INTO t1 VALUES(1,2),(3,4);
		INSERT INTO t2 VALUES(2,20),(4,40);
		CREATE VIEW vj AS SELECT t1.a AS id, t2.y AS val FROM t1 JOIN t2 ON t1.b=t2.x;
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM vj ORDER BY id;")
	if got, want := flattenRows(rows), "1 20 3 40"; got != want {
		t.Errorf("view over join got %q want %q", got, want)
	}
	// Outer query can filter on view columns.
	rows = p2QueryRows(t, db, "SELECT id FROM vj WHERE val > 30;")
	if got, want := flattenRows(rows), "3"; got != want {
		t.Errorf("view over join filter got %q want %q", got, want)
	}
}

// TestP2ViewOverAggregate verifies a view over an aggregate query.
func TestP2ViewOverAggregate(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a,b);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30);
		CREATE VIEW va AS SELECT max(a) AS mx, sum(b) AS sm FROM t1;
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM va;")
	if got, want := flattenRows(rows), "3 60"; got != want {
		t.Errorf("view over aggregate got %q want %q", got, want)
	}
	// Expressions over view columns.
	rows = p2QueryRows(t, db, "SELECT mx+10, sm*2 FROM va;")
	if got, want := flattenRows(rows), "13 120"; got != want {
		t.Errorf("view aggregate expressions got %q want %q", got, want)
	}
}

// TestP2ViewNested verifies a view that selects from another view.
func TestP2ViewNested(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a,b);
		INSERT INTO t1 VALUES(1,2),(3,4);
		CREATE VIEW v1 AS SELECT a, b FROM t1;
		CREATE VIEW v2 AS SELECT a FROM v1 WHERE b > 2;
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM v2;")
	if got, want := flattenRows(rows), "3"; got != want {
		t.Errorf("nested view got %q want %q", got, want)
	}
}

// TestP2ViewNameResolution verifies column-name resolution through views,
// including aliased columns.
func TestP2ViewNameResolution(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a,b);
		INSERT INTO t1 VALUES(1,2),(3,4);
		CREATE VIEW v1 AS SELECT a AS x, b AS y FROM t1;
	`)
	// Outer query can use the aliased view column names.
	rows := p2QueryRows(t, db, "SELECT x, y FROM v1 WHERE x=3;")
	if got, want := flattenRows(rows), "3 4"; got != want {
		t.Errorf("view name resolution got %q want %q", got, want)
	}
	// Qualified reference uses the view name.
	rows = p2QueryRows(t, db, "SELECT v1.y FROM v1 WHERE v1.x=1;")
	if got, want := flattenRows(rows), "2"; got != want {
		t.Errorf("qualified view resolution got %q want %q", got, want)
	}
	// Aliased view in FROM.
	rows = p2QueryRows(t, db, "SELECT w.y FROM v1 AS w WHERE w.x=1;")
	if got, want := flattenRows(rows), "2"; got != want {
		t.Errorf("view alias resolution got %q want %q", got, want)
	}
}

// TestP2ViewReadOnly verifies INSERT/UPDATE/DELETE on a plain view fail
// with the SQLite error text. Oracle: sqlite3 CLI.
func TestP2ViewReadOnly(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a,b);
		INSERT INTO t1 VALUES(1,2);
		CREATE VIEW v1 AS SELECT a,b FROM t1;
	`)
	if r := db.Exec("INSERT INTO v1 VALUES(9,9);"); r.Error == nil || !strings.Contains(r.Error.Error(), "cannot modify v1 because it is a view") {
		t.Errorf("INSERT into view: got %v, want 'cannot modify v1 because it is a view'", r.Error)
	}
	if r := db.Exec("UPDATE v1 SET a=1;"); r.Error == nil || !strings.Contains(r.Error.Error(), "cannot modify v1 because it is a view") {
		t.Errorf("UPDATE view: got %v, want 'cannot modify v1 because it is a view'", r.Error)
	}
	if r := db.Exec("DELETE FROM v1;"); r.Error == nil || !strings.Contains(r.Error.Error(), "cannot modify v1 because it is a view") {
		t.Errorf("DELETE view: got %v, want 'cannot modify v1 because it is a view'", r.Error)
	}
	// Underlying table is unchanged.
	rows := p2QueryRows(t, db, "SELECT * FROM t1;")
	if got, want := flattenRows(rows), "1 2"; got != want {
		t.Errorf("underlying table changed: got %q want %q", got, want)
	}
}

// TestP2ViewDrop verifies DROP VIEW, DROP VIEW IF EXISTS, and the
// type-checked errors. Oracle: sqlite3 CLI.
func TestP2ViewDrop(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a,b);
		CREATE VIEW v1 AS SELECT a,b FROM t1;
	`)
	// DROP VIEW on a table fails and leaves the table intact.
	if r := db.Exec("DROP VIEW t1;"); r.Error == nil || !strings.Contains(r.Error.Error(), "use DROP TABLE to delete table t1") {
		t.Errorf("DROP VIEW on table: got %v, want 'use DROP TABLE to delete table t1'", r.Error)
	}
	rows := p2QueryRows(t, db, "SELECT count(*) FROM t1;")
	if got, want := flattenRows(rows), "0"; got != want {
		t.Errorf("DROP VIEW removed table: got %q want %q", got, want)
	}
	// DROP TABLE on a view fails and leaves the view intact.
	if r := db.Exec("DROP TABLE v1;"); r.Error == nil || !strings.Contains(r.Error.Error(), "use DROP VIEW to delete view v1") {
		t.Errorf("DROP TABLE on view: got %v, want 'use DROP VIEW to delete view v1'", r.Error)
	}
	rows = p2QueryRows(t, db, "SELECT count(*) FROM v1;")
	if got, want := flattenRows(rows), "0"; got != want {
		t.Errorf("DROP TABLE removed view: got %q want %q", got, want)
	}
	// DROP VIEW on a nonexistent view errors; IF EXISTS succeeds silently.
	if r := db.Exec("DROP VIEW nosuchview;"); r.Error == nil || !strings.Contains(r.Error.Error(), "no such view: nosuchview") {
		t.Errorf("DROP VIEW nosuchview: got %v, want 'no such view: nosuchview'", r.Error)
	}
	if r := db.Exec("DROP VIEW IF EXISTS nosuchview;"); r.Error != nil {
		t.Errorf("DROP VIEW IF EXISTS nosuchview: got %v", r.Error)
	}
	// DROP VIEW actually removes the view.
	mustExec(t, db, "DROP VIEW v1;")
	if r := db.Query("SELECT * FROM v1;"); r.Error == nil || !strings.Contains(r.Error.Error(), "no such table: v1") {
		t.Errorf("query dropped view: got %v, want 'no such table: v1'", r.Error)
	}
}

// TestP2ViewTempScoping verifies CREATE TEMP VIEW stores the view in the
// temp schema, not main, and that a temp view shadows a main table of the
// same name only for unqualified top-level queries (not inside a main view
// body). Oracle: sqlite3 CLI.
func TestP2ViewTempScoping(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a,b,c);
		INSERT INTO t1 VALUES(1,2,3);
	`)
	// Create a temp view with the same name as the main table.
	if r := db.Exec("CREATE TEMP VIEW t1 AS SELECT a,b FROM t1;"); r.Error != nil {
		t.Fatalf("create temp view: %v", r.Error)
	}
	// Direct unqualified query resolves temp view t1 -> circular (SQLite
	// reports the view as circularly defined).
	if r := db.Query("SELECT * FROM t1 LIMIT 1;"); r.Error == nil || !strings.Contains(r.Error.Error(), "view t1 is circularly defined") {
		t.Errorf("temp shadowing direct query: got %v, want 'view t1 is circularly defined'", r.Error)
	}
	// main.t1 still works.
	rows := p2QueryRows(t, db, "SELECT * FROM main.t1;")
	if got, want := flattenRows(rows), "1 2 3"; got != want {
		t.Errorf("main.t1 after shadow: got %q want %q", got, want)
	}
	// A main view body resolves t1 to the main schema (not the temp view).
	mustExec(t, db, "CREATE VIEW vmain AS SELECT a AS x, b AS y FROM t1;")
	rows = p2QueryRows(t, db, "SELECT * FROM vmain;")
	if got, want := flattenRows(rows), "1 2"; got != want {
		t.Errorf("main view over shadowed name: got %q want %q", got, want)
	}
	// temp.t1 is visible via qualified name; drop it and the main table
	// becomes visible again.
	mustExec(t, db, "DROP VIEW temp.t1;")
	rows = p2QueryRows(t, db, "SELECT * FROM t1;")
	if got, want := flattenRows(rows), "1 2 3"; got != want {
		t.Errorf("t1 after temp view drop: got %q want %q", got, want)
	}
}

// TestP2ViewColumnCount verifies SQLite's view column-count validation
// ("expected N columns for 'v' but got M"), surfaced when the view is used.
func TestP2ViewColumnCount(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a,b,c);
		INSERT INTO t1 VALUES(1,2,3);
		CREATE VIEW v2(x,y) AS SELECT a,b,c FROM t1;
	`)
	if r := db.Query("SELECT * FROM v2;"); r.Error == nil || !strings.Contains(r.Error.Error(), "expected 2 columns for 'v2' but got 3") {
		t.Errorf("view column count: got %v, want 'expected 2 columns for 'v2' but got 3'", r.Error)
	}
}

// TestP2ViewSubquery verifies a view whose body contains a subquery.
func TestP2ViewSubquery(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a);
		INSERT INTO t1 VALUES(1),(2),(3);
		CREATE VIEW vs AS SELECT a FROM (SELECT a FROM t1 WHERE a>1);
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM vs ORDER BY a;")
	if got, want := flattenRows(rows), "2 3"; got != want {
		t.Errorf("view over subquery got %q want %q", got, want)
	}
}

// TestP2ViewSelfJoin verifies a view (over an aggregate with GROUP BY) can
// be joined to itself. Oracle: sqlite3 view-26.0.
func TestP2ViewSelfJoin(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t16(a, b, c UNIQUE);
		INSERT INTO t16 VALUES(1, 1, 1);
		INSERT INTO t16 VALUES(2, 2, 2);
		INSERT INTO t16 VALUES(3, 3, 3);
		CREATE VIEW v16 AS SELECT max(a) AS mx, min(b) AS mn FROM t16 GROUP BY c;
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM v16 AS one, v16 AS two WHERE one.mx=1;")
	want := "1 1 1 1 1 1 2 2 1 1 3 3"
	if got := flattenRows(rows); got != want {
		t.Errorf("view self-join got %q want %q", got, want)
	}
}

// TestP2ViewUnion verifies a view whose body is a compound SELECT (UNION).
func TestP2ViewUnion(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a);
		INSERT INTO t1 VALUES(1),(2),(3);
		CREATE VIEW vu AS SELECT a FROM t1 UNION SELECT a*10 FROM t1;
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM vu ORDER BY a;")
	if got, want := flattenRows(rows), "1 2 3 10 20 30"; got != want {
		t.Errorf("view union got %q want %q", got, want)
	}
}
