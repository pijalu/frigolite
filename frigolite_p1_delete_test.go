package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestP1DeleteBasic covers DELETE all rows, DELETE with WHERE, and the
// changes() count.
func TestP1DeleteBasic(t *testing.T) {
	t.Run("delete all rows", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("DELETE FROM t")
		if res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		if res.Changes != 3 {
			t.Errorf("expected 3 changes, got %d", res.Changes)
		}
		r := db.Query("SELECT count(*) FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(0) {
			t.Errorf("expected empty table, got %v", r.Rows)
		}
	})

	t.Run("delete with where", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t WHERE a=2"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(3) {
			t.Errorf("expected rows 1 and 3, got %v", r.Rows)
		}
	})

	t.Run("delete no match changes zero", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("DELETE FROM t WHERE a=99")
		if res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		if res.Changes != 0 {
			t.Errorf("expected 0 changes, got %d", res.Changes)
		}
	})

	t.Run("delete with expression where", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z'),(4,'w')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t WHERE a%2=0"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT a FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(3) {
			t.Errorf("expected rows 1 and 3, got %v", r.Rows)
		}
	})
}

// TestP1DeleteQualifiedWhere covers DELETE with table-qualified column
// references in the WHERE clause during the scan (DELETE FROM t WHERE t.x = 1).
func TestP1DeleteQualifiedWhere(t *testing.T) {
	t.Run("qualified column", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(x INT, y INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,10),(2,20),(3,30)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t WHERE t.x = 2"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT x FROM t ORDER BY x")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(3) {
			t.Errorf("expected rows 1 and 3, got %v", r.Rows)
		}
	})

	t.Run("qualified column with alias", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t6(x INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t6 VALUES(1),(2),(3)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t6 WHERE t6.x > 1"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT x FROM t6")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
			t.Errorf("expected only row 1, got %v", r.Rows)
		}
	})

	t.Run("qualified and unqualified mixed", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,10),(2,20),(3,30)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t WHERE t.a = 1 AND b = 10"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT a FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[1][0] != int64(3) {
			t.Errorf("expected rows 2 and 3, got %v", r.Rows)
		}
	})
}

// TestP1DeleteOrderLimit covers DELETE ... ORDER BY ... LIMIT n and plain
// LIMIT (rowid order when no ORDER BY), a SQLite extension.
func TestP1DeleteOrderLimit(t *testing.T) {
	t.Run("order by limit", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(id INT, v TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(3,'c'),(1,'a'),(2,'b')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t ORDER BY id LIMIT 1"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT id, v FROM t ORDER BY id")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// ORDER BY id: id=1 is deleted first.
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[1][0] != int64(3) {
			t.Errorf("expected rows 2 and 3, got %v", r.Rows)
		}
	})

	t.Run("limit without order by uses rowid order", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(id INT, v TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(3,'c'),(1,'a'),(2,'b')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t LIMIT 1"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT id, v FROM t ORDER BY id")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// SQLite scans in rowid order. Rowids are assigned in INSERT order:
		// rowid 1 = (3,'c'), rowid 2 = (1,'a'), rowid 3 = (2,'b'). LIMIT 1
		// deletes the first rowid (3,'c'), so the row with id=3 goes away.
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(2) {
			t.Errorf("expected rows 1 and 2, got %v", r.Rows)
		}
	})

	t.Run("order by desc limit", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(id INT, v TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'a'),(2,'b'),(3,'c')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t WHERE id>1 ORDER BY id DESC LIMIT 1"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT id, v FROM t ORDER BY id")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// DESC: id=3 is first, deleted.
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(2) {
			t.Errorf("expected rows 1 and 2, got %v", r.Rows)
		}
	})

	t.Run("limit offset", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(id INT, v TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'a'),(2,'b'),(3,'c')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t ORDER BY id LIMIT 1 OFFSET 1"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT id, v FROM t ORDER BY id")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// OFFSET 1 skips id=1, deletes id=2.
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(3) {
			t.Errorf("expected rows 1 and 3, got %v", r.Rows)
		}
	})

	t.Run("order by without limit is an error", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(id INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		res := db.Exec("DELETE FROM t ORDER BY id")
		if res.Error == nil {
			t.Errorf("expected ORDER BY without LIMIT to error")
		}
	})

	t.Run("order by limit returning is an error", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(id INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		res := db.Query("DELETE FROM t ORDER BY id LIMIT 1 RETURNING id")
		if res.Error == nil {
			t.Errorf("expected ORDER BY/LIMIT with RETURNING to error")
		}
	})
}

// TestP1DeleteReturning covers DELETE RETURNING: * expansion, explicit columns,
// expressions, and WHERE filtering of which rows are returned.
func TestP1DeleteReturning(t *testing.T) {
	t.Run("returning star", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("DELETE FROM t RETURNING *")
		if r.Error != nil {
			t.Fatalf("delete returning: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "x" ||
			r.Rows[1][0] != int64(2) || r.Rows[1][1] != "y" {
			t.Errorf("unexpected returning rows: %v", r.Rows)
		}
	})

	t.Run("returning explicit columns", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("DELETE FROM t WHERE a>1 RETURNING a, b")
		if r.Error != nil {
			t.Fatalf("delete returning: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[0][1] != "y" ||
			r.Rows[1][0] != int64(3) || r.Rows[1][1] != "z" {
			t.Errorf("unexpected returning rows: %v", r.Rows)
		}
		// The rows are actually deleted.
		rc := db.Query("SELECT count(*) FROM t")
		if rc.Error != nil || rc.Rows[0][0] != int64(1) {
			t.Errorf("expected 1 row remaining, got %v", rc.Rows)
		}
	})

	t.Run("returning expressions", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("DELETE FROM t RETURNING a, upper(b), a*2")
		if r.Error != nil {
			t.Fatalf("delete returning: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "X" || r.Rows[0][2] != int64(2) ||
			r.Rows[1][0] != int64(2) || r.Rows[1][1] != "Y" || r.Rows[1][2] != int64(4) {
			t.Errorf("unexpected returning rows: %v", r.Rows)
		}
	})

	t.Run("returning all rows deleted", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2),(3)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("DELETE FROM t RETURNING a")
		if r.Error != nil {
			t.Fatalf("delete returning: %v", r.Error)
		}
		if len(r.Rows) != 3 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(2) || r.Rows[2][0] != int64(3) {
			t.Errorf("expected 3 returning rows, got %v", r.Rows)
		}
	})

	t.Run("returning subquery aggregates see post-delete state", func(t *testing.T) {
		// RETURNING subqueries that read the same table must observe the
		// table with the current row already removed. When the last row is
		// deleted (table empty), nested aggregates inside wrapper expressions
		// (round(avg(x),2)) must yield NULL, not a stale value from the
		// previous row (returning1 20.2).
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40),(6,60),(8,80)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query(`
			DELETE FROM t1
			  RETURNING a,
			            (SELECT min(a) FROM t1),
			            (SELECT max(a) FROM t1),
			            (SELECT round(avg(a),2) FROM t1)`)
		if r.Error != nil {
			t.Fatalf("delete returning: %v", r.Error)
		}
		want := [][]interface{}{
			{int64(1), int64(2), int64(8), 4.6},
			{int64(2), int64(3), int64(8), 5.25},
			{int64(3), int64(4), int64(8), 6.0},
			{int64(4), int64(6), int64(8), 7.0},
			{int64(6), int64(8), int64(8), 8.0},
			{int64(8), nil, nil, nil},
		}
		if len(r.Rows) != len(want) {
			t.Fatalf("expected %d returning rows, got %v", len(want), r.Rows)
		}
		for i, row := range want {
			for j, v := range row {
				if !returningValueEqual(r.Rows[i][j], v) {
					t.Errorf("row %d col %d: got %v, want %v (full: %v)", i, j, r.Rows[i][j], v, r.Rows)
				}
			}
		}
	})

	t.Run("returning correlated subquery aggregates over emptied table", func(t *testing.T) {
		// A correlated RETURNING subquery aggregate over the same table must
		// see zero rows after the current row is deleted, so min/max/avg of
		// the inner table become NULL regardless of the outer column value
		// (returning1 20.3).
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40),(6,60),(8,80)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query(`
			DELETE FROM t1
			  RETURNING a,
			            (SELECT min(t2.a)+t1.a*100 FROM t1 AS t2),
			            (SELECT max(t2.a)+t1.a*100 FROM t1 AS t2),
			            (SELECT round(avg(t2.a),2)+t1.a*100 FROM t1 AS t2)`)
		if r.Error != nil {
			t.Fatalf("delete returning: %v", r.Error)
		}
		if len(r.Rows) != 6 {
			t.Fatalf("expected 6 returning rows, got %v", r.Rows)
		}
		if r.Rows[5][0] != int64(8) || r.Rows[5][1] != nil || r.Rows[5][2] != nil || r.Rows[5][3] != nil {
			t.Errorf("expected last row [8 nil nil nil], got %v", r.Rows[5])
		}
	})
}

// returningValueEqual compares a RETURNING result value with an expected
// value, treating numerically equal int64/float64 values as equal.
func returningValueEqual(got, want interface{}) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	gf, ok1 := got.(float64)
	wf, ok2 := want.(float64)
	if ok1 && ok2 {
		return gf == wf
	}
	gi, ok1 := got.(int64)
	wi, ok2 := want.(int64)
	if ok1 && ok2 {
		return gi == wi
	}
	return fmt.Sprint(got) == fmt.Sprint(want)
}

// TestP1DeleteTrigger covers DELETE triggers capturing OLD.* values in
// BEFORE/AFTER DELETE triggers.
func TestP1DeleteTrigger(t *testing.T) {
	t.Run("after delete captures old row", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE log(x INT, y TEXT)"); res.Error != nil {
			t.Fatalf("create log: %v", res.Error)
		}
		if res := db.Exec("CREATE TRIGGER tr AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a, old.b); END"); res.Error != nil {
			t.Fatalf("create trigger: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t WHERE a=1"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT x, y FROM log")
		if r.Error != nil {
			t.Fatalf("select log: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "x" {
			t.Errorf("expected log (1,x), got %v", r.Rows)
		}
	})

	t.Run("before delete sees old values", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE log(x INT, y TEXT)"); res.Error != nil {
			t.Fatalf("create log: %v", res.Error)
		}
		if res := db.Exec("CREATE TRIGGER tr BEFORE DELETE ON t BEGIN INSERT INTO log VALUES(old.a, old.b); END"); res.Error != nil {
			t.Fatalf("create trigger: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(5,'v')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT x, y FROM log")
		if r.Error != nil {
			t.Fatalf("select log: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(5) || r.Rows[0][1] != "v" {
			t.Errorf("expected log (5,v), got %v", r.Rows)
		}
	})

	t.Run("old rowid in trigger", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE log(rowid_val INT)"); res.Error != nil {
			t.Fatalf("create log: %v", res.Error)
		}
		if res := db.Exec("CREATE TRIGGER tr AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.rowid); END"); res.Error != nil {
			t.Fatalf("create trigger: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(7)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT rowid_val FROM log")
		if r.Error != nil {
			t.Fatalf("select log: %v", r.Error)
		}
		// Rowid 1 was inserted first.
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
			t.Errorf("expected log rowid 1, got %v", r.Rows)
		}
	})

	t.Run("trigger order multiple rows", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE log(x INT)"); res.Error != nil {
			t.Fatalf("create log: %v", res.Error)
		}
		if res := db.Exec("CREATE TRIGGER tr AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END"); res.Error != nil {
			t.Fatalf("create trigger: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2),(3)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM t WHERE a>1"); res.Error != nil {
			t.Fatalf("delete: %v", res.Error)
		}
		r := db.Query("SELECT x FROM log ORDER BY x")
		if r.Error != nil {
			t.Fatalf("select log: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[1][0] != int64(3) {
			t.Errorf("expected log (2,3), got %v", r.Rows)
		}
	})
}

// TestP1DeleteForeignKey covers FK ON DELETE CASCADE / SET NULL / RESTRICT /
// NO ACTION actions (PRAGMA foreign_keys=ON).
func TestP1DeleteForeignKey(t *testing.T) {
	t.Run("on delete cascade", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("PRAGMA foreign_keys=ON"); res.Error != nil {
			t.Fatalf("pragma: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE p(id INT PRIMARY KEY)"); res.Error != nil {
			t.Fatalf("create parent: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE c(pid INT REFERENCES p(id) ON DELETE CASCADE)"); res.Error != nil {
			t.Fatalf("create child: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO p VALUES(1),(2)"); res.Error != nil {
			t.Fatalf("insert parent: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO c VALUES(1),(1),(2)"); res.Error != nil {
			t.Fatalf("insert child: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM p WHERE id=1"); res.Error != nil {
			t.Fatalf("delete parent: %v", res.Error)
		}
		r := db.Query("SELECT pid FROM c")
		if r.Error != nil {
			t.Fatalf("select child: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
			t.Errorf("expected only child 2, got %v", r.Rows)
		}
	})

	t.Run("on delete set null", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("PRAGMA foreign_keys=ON"); res.Error != nil {
			t.Fatalf("pragma: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE p(id INT PRIMARY KEY)"); res.Error != nil {
			t.Fatalf("create parent: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE c(pid INT REFERENCES p(id) ON DELETE SET NULL)"); res.Error != nil {
			t.Fatalf("create child: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO p VALUES(1)"); res.Error != nil {
			t.Fatalf("insert parent: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO c VALUES(1)"); res.Error != nil {
			t.Fatalf("insert child: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM p WHERE id=1"); res.Error != nil {
			t.Fatalf("delete parent: %v", res.Error)
		}
		r := db.Query("SELECT pid FROM c")
		if r.Error != nil {
			t.Fatalf("select child: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != nil {
			t.Errorf("expected child pid NULL, got %v", r.Rows)
		}
	})

	t.Run("on delete set default", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("PRAGMA foreign_keys=ON"); res.Error != nil {
			t.Fatalf("pragma: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE p(id INT PRIMARY KEY)"); res.Error != nil {
			t.Fatalf("create parent: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE c(pid INT DEFAULT 5 REFERENCES p(id) ON DELETE SET DEFAULT)"); res.Error != nil {
			t.Fatalf("create child: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO p VALUES(1),(5)"); res.Error != nil {
			t.Fatalf("insert parent: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO c VALUES(1)"); res.Error != nil {
			t.Fatalf("insert child: %v", res.Error)
		}
		if res := db.Exec("DELETE FROM p WHERE id=1"); res.Error != nil {
			t.Fatalf("delete parent: %v", res.Error)
		}
		r := db.Query("SELECT pid FROM c")
		if r.Error != nil {
			t.Fatalf("select child: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(5) {
			t.Errorf("expected child pid 5 (default), got %v", r.Rows)
		}
	})

	t.Run("on delete restrict", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("PRAGMA foreign_keys=ON"); res.Error != nil {
			t.Fatalf("pragma: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE p(id INT PRIMARY KEY)"); res.Error != nil {
			t.Fatalf("create parent: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE c(pid INT REFERENCES p(id) ON DELETE RESTRICT)"); res.Error != nil {
			t.Fatalf("create child: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO p VALUES(1)"); res.Error != nil {
			t.Fatalf("insert parent: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO c VALUES(1)"); res.Error != nil {
			t.Fatalf("insert child: %v", res.Error)
		}
		res := db.Exec("DELETE FROM p WHERE id=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "FOREIGN KEY constraint failed") {
			t.Errorf("expected FK error, got: %v", res.Error)
		}
		// The statement was rolled back: parent row remains.
		r := db.Query("SELECT count(*) FROM p")
		if r.Error != nil || r.Rows[0][0] != int64(1) {
			t.Errorf("expected parent row to remain after rollback, got %v", r.Rows)
		}
	})

	t.Run("on delete no action default", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("PRAGMA foreign_keys=ON"); res.Error != nil {
			t.Fatalf("pragma: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE p(id INT PRIMARY KEY)"); res.Error != nil {
			t.Fatalf("create parent: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE c(pid INT REFERENCES p(id))"); res.Error != nil {
			t.Fatalf("create child: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO p VALUES(1)"); res.Error != nil {
			t.Fatalf("insert parent: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO c VALUES(1)"); res.Error != nil {
			t.Fatalf("insert child: %v", res.Error)
		}
		res := db.Exec("DELETE FROM p WHERE id=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "FOREIGN KEY constraint failed") {
			t.Errorf("expected FK error, got: %v", res.Error)
		}
	})
}

// TestP1DeleteRollback covers statement-level rollback when a trigger or
// constraint error occurs mid-delete: rows deleted before the error are
// restored.
func TestP1DeleteRollback(t *testing.T) {
	t.Run("trigger error mid-delete rolls back", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE log(x INT)"); res.Error != nil {
			t.Fatalf("create log: %v", res.Error)
		}
		if res := db.Exec("CREATE TRIGGER tr AFTER DELETE ON t WHEN old.a=2 BEGIN SELECT RAISE(ABORT, 'stop'); END"); res.Error != nil {
			t.Fatalf("create trigger: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2),(3)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("DELETE FROM t")
		if res.Error == nil {
			t.Fatalf("expected trigger error")
		}
		// All rows restored: the whole statement rolled back.
		r := db.Query("SELECT count(*) FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(3) {
			t.Errorf("expected all 3 rows restored, got %v", r.Rows)
		}
	})

	t.Run("trigger side effects rolled back", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE log(x INT)"); res.Error != nil {
			t.Fatalf("create log: %v", res.Error)
		}
		// The BEFORE trigger logs rows; the AFTER trigger errors. The log
		// inserts from BEFORE triggers must also be rolled back.
		if res := db.Exec("CREATE TRIGGER tr_b BEFORE DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END"); res.Error != nil {
			t.Fatalf("create trigger: %v", res.Error)
		}
		if res := db.Exec("CREATE TRIGGER tr_a AFTER DELETE ON t WHEN old.a=2 BEGIN SELECT RAISE(ABORT, 'stop'); END"); res.Error != nil {
			t.Fatalf("create trigger: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("DELETE FROM t")
		if res.Error == nil {
			t.Fatalf("expected trigger error")
		}
		r := db.Query("SELECT count(*) FROM log")
		if r.Error != nil {
			t.Fatalf("select log: %v", r.Error)
		}
		if r.Rows[0][0] != int64(0) {
			t.Errorf("expected log rolled back to empty, got %v", r.Rows)
		}
	})

	t.Run("fk error mid-delete rolls back", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("PRAGMA foreign_keys=ON"); res.Error != nil {
			t.Fatalf("pragma: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE p(id INT PRIMARY KEY)"); res.Error != nil {
			t.Fatalf("create parent: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE c(pid INT REFERENCES p(id) ON DELETE RESTRICT)"); res.Error != nil {
			t.Fatalf("create child: %v", res.Error)
		}
		// Row 1 has no child (deletable), row 2 has a child (blocks delete).
		if res := db.Exec("INSERT INTO p VALUES(1),(2)"); res.Error != nil {
			t.Fatalf("insert parent: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO c VALUES(2)"); res.Error != nil {
			t.Fatalf("insert child: %v", res.Error)
		}
		res := db.Exec("DELETE FROM p")
		if res.Error == nil {
			t.Fatalf("expected FK error")
		}
		// The whole statement rolled back: both parent rows remain.
		r := db.Query("SELECT count(*) FROM p")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(2) {
			t.Errorf("expected both parent rows restored, got %v", r.Rows)
		}
	})
}
