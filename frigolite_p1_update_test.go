package frigolite

import (
	"strings"
	"testing"
)

// TestP1UpdateSet covers UPDATE SET with single/multi-column assignments,
// row self-references (SET a=a+1), cross-column references (SET a=b, b=a),
// and expression RHS values referencing columns and functions.
func TestP1UpdateSet(t *testing.T) {
	t.Run("single column", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET b='z'"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][1] != "z" || r.Rows[1][1] != "z" {
			t.Errorf("unexpected rows: %v", r.Rows)
		}
	})

	t.Run("multi-column", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b INT, c INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,10,100)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET a=2, b=20, c=200"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a, b, c FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(2) || r.Rows[0][1] != int64(20) || r.Rows[0][2] != int64(200) {
			t.Errorf("unexpected row: %v", r.Rows[0])
		}
	})

	t.Run("self reference a=a+1", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2),(3)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET a=a+1"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT sum(a) FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// 1+1=2, 2+1=3, 3+1=4 -> sum 9
		if r.Rows[0][0] != int64(9) {
			t.Errorf("expected sum 9, got %v", r.Rows[0][0])
		}
	})

	t.Run("swap a=b, b=a", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,2),(3,4)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		// SQLite evaluates all RHS expressions against the OLD row, then
		// applies the assignments, so a=b, b=a swaps the values.
		if res := db.Exec("UPDATE t SET a=b, b=a"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[0][1] != int64(1) ||
			r.Rows[1][0] != int64(4) || r.Rows[1][1] != int64(3) {
			t.Errorf("expected swapped rows, got %v", r.Rows)
		}
	})

	t.Run("expression RHS with functions", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'abc'),(2,'def')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET b=upper(b), a=length(b)+a"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// row1: a=1+3=4, b='ABC'; row2: a=2+3=5, b='DEF'
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(4) || r.Rows[0][1] != "ABC" ||
			r.Rows[1][0] != int64(5) || r.Rows[1][1] != "DEF" {
			t.Errorf("unexpected rows: %v", r.Rows)
		}
	})

	t.Run("set null", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET b=NULL"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(1) || r.Rows[0][1] != nil {
			t.Errorf("expected NULL b, got %v", r.Rows[0])
		}
	})
}

// TestP1UpdateWhere covers WHERE selection on UPDATE and updating all rows
// when no WHERE is present.
func TestP1UpdateWhere(t *testing.T) {
	t.Run("where selection", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET b='hit' WHERE a=2"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 3 || r.Rows[0][1] != "x" || r.Rows[1][1] != "hit" || r.Rows[2][1] != "z" {
			t.Errorf("unexpected rows: %v", r.Rows)
		}
	})

	t.Run("no where updates all", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET b='all'"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT count(*), sum(CASE WHEN b='all' THEN 1 ELSE 0 END) FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(3) || r.Rows[0][1] != int64(3) {
			t.Errorf("expected all 3 rows updated, got %v", r.Rows[0])
		}
	})

	t.Run("where with expression", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z'),(4,'w')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET b='e' WHERE a%2=0"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 4 || r.Rows[0][1] != "x" || r.Rows[1][1] != "e" || r.Rows[2][1] != "z" || r.Rows[3][1] != "e" {
			t.Errorf("unexpected rows: %v", r.Rows)
		}
	})

	t.Run("changes count", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2),(3)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("UPDATE t SET a=a+1 WHERE a>1")
		if res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		if res.Changes != 2 {
			t.Errorf("expected 2 changes, got %d", res.Changes)
		}
	})
}

// TestP1UpdateOrderLimit covers UPDATE ... ORDER BY ... LIMIT n and plain
// LIMIT (rowid order when no ORDER BY), a SQLite extension.
func TestP1UpdateOrderLimit(t *testing.T) {
	t.Run("order by limit", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(id INT, v TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(3,'c'),(1,'a'),(2,'b')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET v='X' ORDER BY id LIMIT 2"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT id, v FROM t ORDER BY id")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 3 || r.Rows[0][1] != "X" || r.Rows[1][1] != "X" || r.Rows[2][1] != "c" {
			t.Errorf("expected first 2 by id updated, got %v", r.Rows)
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
		if res := db.Exec("UPDATE t SET v='Y' LIMIT 1"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT id, v FROM t ORDER BY id")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// SQLite scans in rowid order. Rowids are assigned in INSERT order:
		// rowid 1 = (3,'c'), rowid 2 = (1,'a'), rowid 3 = (2,'b'). LIMIT 1
		// updates the first rowid (3,'c'), so the row with id=3 changes.
		if len(r.Rows) != 3 || r.Rows[0][1] != "a" || r.Rows[1][1] != "b" || r.Rows[2][1] != "Y" {
			t.Errorf("expected rowid 1 (id=3) updated, got %v", r.Rows)
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
		if res := db.Exec("UPDATE t SET v='Z' WHERE id>1 ORDER BY id DESC LIMIT 1"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT id, v FROM t ORDER BY id")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// DESC: id=3 is first, updated.
		if len(r.Rows) != 3 || r.Rows[0][1] != "a" || r.Rows[1][1] != "b" || r.Rows[2][1] != "Z" {
			t.Errorf("expected id 3 updated, got %v", r.Rows)
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
		if res := db.Exec("UPDATE t SET v='W' ORDER BY id LIMIT 1 OFFSET 1"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT id, v FROM t ORDER BY id")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// OFFSET 1 skips id=1, updates id=2.
		if len(r.Rows) != 3 || r.Rows[0][1] != "a" || r.Rows[1][1] != "W" || r.Rows[2][1] != "c" {
			t.Errorf("expected id 2 updated, got %v", r.Rows)
		}
	})
}

// TestP1UpdateReturning covers UPDATE RETURNING: * expansion, explicit
// columns, expressions, and WHERE filtering of which rows are updated.
func TestP1UpdateReturning(t *testing.T) {
	t.Run("returning star", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("UPDATE t SET b='z' RETURNING *")
		if r.Error != nil {
			t.Fatalf("update returning: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "z" || r.Rows[1][0] != int64(2) || r.Rows[1][1] != "z" {
			t.Errorf("unexpected returning rows: %v", r.Rows)
		}
	})

	t.Run("returning explicit columns", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("UPDATE t SET b='y' RETURNING a, b")
		if r.Error != nil {
			t.Fatalf("update returning: %v", r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 2 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "y" {
			t.Errorf("unexpected returning row: %v", r.Rows)
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
		r := db.Query("UPDATE t SET b=b||'!' RETURNING a, upper(b), a*2")
		if r.Error != nil {
			t.Fatalf("update returning: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "X!" || r.Rows[0][2] != int64(2) ||
			r.Rows[1][0] != int64(2) || r.Rows[1][1] != "Y!" || r.Rows[1][2] != int64(4) {
			t.Errorf("unexpected returning rows: %v", r.Rows)
		}
	})

	t.Run("where filters returned rows", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("UPDATE t SET b='hit' WHERE a>1 RETURNING a, b")
		if r.Error != nil {
			t.Fatalf("update returning: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[1][0] != int64(3) {
			t.Errorf("expected rows 2 and 3 returned, got %v", r.Rows)
		}
		for _, row := range r.Rows {
			if row[1] != "hit" {
				t.Errorf("expected hit, got %v", row)
			}
		}
	})
}

// TestP1UpdateConflict covers UPDATE OR IGNORE / REPLACE / FAIL / ABORT /
// ROLLBACK conflict resolution on UNIQUE constraint conflicts.
func TestP1UpdateConflict(t *testing.T) {
	t.Run("or ignore skips conflicting row", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT UNIQUE, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		// a=1 -> 2 conflicts with row 2; the row is skipped, no error.
		if res := db.Exec("UPDATE OR IGNORE t SET a=2 WHERE a=1"); res.Error != nil {
			t.Fatalf("or ignore: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "x" || r.Rows[1][0] != int64(2) || r.Rows[1][1] != "y" {
			t.Errorf("row should be unchanged, got %v", r.Rows)
		}
	})

	t.Run("or ignore multi-row", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT UNIQUE)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2),(3)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		// a=a+1: 1->2 conflicts (row 2 original), 2->3 conflicts (row 3
		// original), 3->4 ok.
		if res := db.Exec("UPDATE OR IGNORE t SET a=a+1"); res.Error != nil {
			t.Fatalf("or ignore: %v", res.Error)
		}
		r := db.Query("SELECT a FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 3 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(2) || r.Rows[2][0] != int64(4) {
			t.Errorf("expected 1,2,4 got %v", r.Rows)
		}
	})

	t.Run("or replace deletes conflict", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT UNIQUE, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		// a=2 -> 1 conflicts with row 1; REPLACE deletes row 1 and inserts 2->1.
		if res := db.Exec("UPDATE OR REPLACE t SET a=1 WHERE a=2"); res.Error != nil {
			t.Fatalf("or replace: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "y" {
			t.Errorf("expected single row (1,y), got %v", r.Rows)
		}
	})

	t.Run("or fail errors", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT UNIQUE)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("UPDATE OR FAIL t SET a=2 WHERE a=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "UNIQUE constraint failed") {
			t.Errorf("expected UNIQUE error, got: %v", res.Error)
		}
	})

	t.Run("or abort errors", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT UNIQUE)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("UPDATE OR ABORT t SET a=2 WHERE a=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "UNIQUE constraint failed") {
			t.Errorf("expected UNIQUE error, got: %v", res.Error)
		}
	})

	t.Run("or rollback errors", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT UNIQUE)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("UPDATE OR ROLLBACK t SET a=2 WHERE a=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "UNIQUE constraint failed") {
			t.Errorf("expected UNIQUE error, got: %v", res.Error)
		}
	})
}

// TestP1UpdateConstraints covers NOT NULL / CHECK / UNIQUE violation on
// UPDATE with exact error messages and statement-level rollback.
func TestP1UpdateConstraints(t *testing.T) {
	t.Run("not null violation", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT NOT NULL)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("UPDATE t SET a=NULL WHERE a=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "NOT NULL constraint failed: t.a") {
			t.Errorf("expected NOT NULL error, got: %v", res.Error)
		}
		// Statement rollback: no rows changed.
		r := db.Query("SELECT count(*) FROM t WHERE a IS NULL")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(0) {
			t.Errorf("expected no NULL rows after rollback, got %v", r.Rows[0][0])
		}
	})

	t.Run("check violation", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT CHECK(a>0))"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("UPDATE t SET a=-1 WHERE a=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "CHECK constraint failed") {
			t.Errorf("expected CHECK error, got: %v", res.Error)
		}
		// Statement rollback.
		r := db.Query("SELECT count(*) FROM t WHERE a=-1")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(0) {
			t.Errorf("expected no -1 rows after rollback, got %v", r.Rows[0][0])
		}
	})

	t.Run("check passes when true", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT CHECK(a>0))"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		// a=5 satisfies CHECK(a>0).
		if res := db.Exec("UPDATE t SET a=5"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(5) {
			t.Errorf("expected 5, got %v", r.Rows[0][0])
		}
	})

	t.Run("unique violation", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT UNIQUE)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("UPDATE t SET a=2 WHERE a=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "UNIQUE constraint failed: t.a") {
			t.Errorf("expected UNIQUE error, got: %v", res.Error)
		}
		// Statement rollback: a=1 still present.
		r := db.Query("SELECT count(*) FROM t WHERE a=1")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(1) {
			t.Errorf("expected a=1 to survive rollback, got %v", r.Rows[0][0])
		}
	})

	t.Run("unique one-pass conflict", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT UNIQUE)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1),(2),(3)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		// a=a+1: row 2's new value 3 conflicts with row 3's original 3.
		res := db.Exec("UPDATE t SET a=a+1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "UNIQUE constraint failed") {
			t.Errorf("expected UNIQUE error, got: %v", res.Error)
		}
		r := db.Query("SELECT sum(a) FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// Statement rolled back: sum is still 1+2+3=6.
		if r.Rows[0][0] != int64(6) {
			t.Errorf("expected sum 6 after rollback, got %v", r.Rows[0][0])
		}
	})

	t.Run("unique update that does not conflict", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT UNIQUE)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(2),(3),(4)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		// a=a-1: 2->1, 3->2, 4->3 — no conflict with originals.
		if res := db.Exec("UPDATE t SET a=a-1"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT sum(a) FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(6) {
			t.Errorf("expected sum 6, got %v", r.Rows[0][0])
		}
	})
}

// TestP1UpdateWithoutRowid covers UPDATE on WITHOUT ROWID tables and updating
// the PRIMARY KEY column.
func TestP1UpdateWithoutRowid(t *testing.T) {
	t.Run("update pk column", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT PRIMARY KEY, b TEXT) WITHOUT ROWID"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET a=3 WHERE a=1"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[0][1] != "y" || r.Rows[1][0] != int64(3) || r.Rows[1][1] != "x" {
			t.Errorf("unexpected rows: %v", r.Rows)
		}
	})

	t.Run("update pk conflict errors", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT PRIMARY KEY, b TEXT) WITHOUT ROWID"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("UPDATE t SET a=2 WHERE a=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "UNIQUE constraint failed") {
			t.Errorf("expected UNIQUE error, got: %v", res.Error)
		}
	})

	t.Run("regular update on without rowid", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT PRIMARY KEY, b TEXT) WITHOUT ROWID"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET b=upper(b)"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][1] != "X" || r.Rows[1][1] != "Y" {
			t.Errorf("unexpected rows: %v", r.Rows)
		}
	})
}

// TestP1UpdateIndex covers index maintenance: updating a column used in an
// index makes the index reflect the new value.
func TestP1UpdateIndex(t *testing.T) {
	t.Run("index reflects updated column", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("CREATE INDEX idx_b ON t(b)"); res.Error != nil {
			t.Fatalf("create index: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,10),(2,20)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		if res := db.Exec("UPDATE t SET b=99 WHERE a=1"); res.Error != nil {
			t.Fatalf("update: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t WHERE b=99")
		if r.Error != nil {
			t.Fatalf("select via index: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != int64(99) {
			t.Errorf("expected (1,99), got %v", r.Rows)
		}
		// Old index value gone.
		r = db.Query("SELECT count(*) FROM t WHERE b=10")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(0) {
			t.Errorf("expected no rows with b=10, got %v", r.Rows[0][0])
		}
	})

	t.Run("unique index conflict", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INT, b INT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("CREATE UNIQUE INDEX idx_b ON t(b)"); res.Error != nil {
			t.Fatalf("create index: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,10),(2,20)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		res := db.Exec("UPDATE t SET b=20 WHERE a=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "UNIQUE constraint failed") {
			t.Errorf("expected UNIQUE error, got: %v", res.Error)
		}
	})
}

// TestP1UpdateForeignKey covers FK ON UPDATE CASCADE / SET NULL / RESTRICT
// actions (PRAGMA foreign_keys=ON).
func TestP1UpdateForeignKey(t *testing.T) {
	t.Run("on update cascade", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("PRAGMA foreign_keys=ON"); res.Error != nil {
			t.Fatalf("pragma: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE p(id INT PRIMARY KEY)"); res.Error != nil {
			t.Fatalf("create parent: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE c(pid INT REFERENCES p(id) ON UPDATE CASCADE)"); res.Error != nil {
			t.Fatalf("create child: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO p VALUES(1),(2)"); res.Error != nil {
			t.Fatalf("insert parent: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO c VALUES(1),(1)"); res.Error != nil {
			t.Fatalf("insert child: %v", res.Error)
		}
		if res := db.Exec("UPDATE p SET id=10 WHERE id=1"); res.Error != nil {
			t.Fatalf("update parent: %v", res.Error)
		}
		r := db.Query("SELECT pid FROM c")
		if r.Error != nil {
			t.Fatalf("select child: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(10) || r.Rows[1][0] != int64(10) {
			t.Errorf("expected cascaded children (10,10), got %v", r.Rows)
		}
	})

	t.Run("on update set null", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("PRAGMA foreign_keys=ON"); res.Error != nil {
			t.Fatalf("pragma: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE p(id INT PRIMARY KEY)"); res.Error != nil {
			t.Fatalf("create parent: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE c(pid INT REFERENCES p(id) ON UPDATE SET NULL)"); res.Error != nil {
			t.Fatalf("create child: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO p VALUES(1)"); res.Error != nil {
			t.Fatalf("insert parent: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO c VALUES(1)"); res.Error != nil {
			t.Fatalf("insert child: %v", res.Error)
		}
		if res := db.Exec("UPDATE p SET id=5 WHERE id=1"); res.Error != nil {
			t.Fatalf("update parent: %v", res.Error)
		}
		r := db.Query("SELECT pid FROM c")
		if r.Error != nil {
			t.Fatalf("select child: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != nil {
			t.Errorf("expected child pid NULL, got %v", r.Rows)
		}
	})

	t.Run("on update restrict", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("PRAGMA foreign_keys=ON"); res.Error != nil {
			t.Fatalf("pragma: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE p(id INT PRIMARY KEY)"); res.Error != nil {
			t.Fatalf("create parent: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE c(pid INT REFERENCES p(id) ON UPDATE RESTRICT)"); res.Error != nil {
			t.Fatalf("create child: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO p VALUES(1)"); res.Error != nil {
			t.Fatalf("insert parent: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO c VALUES(1)"); res.Error != nil {
			t.Fatalf("insert child: %v", res.Error)
		}
		res := db.Exec("UPDATE p SET id=9 WHERE id=1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "FOREIGN KEY constraint failed") {
			t.Errorf("expected FK error, got: %v", res.Error)
		}
	})
}
