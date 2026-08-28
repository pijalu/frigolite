package frigolite

import (
	"fmt"
	"testing"
)

// TestSQLDialect verifies comprehensive SQL dialect support.
// Each test runs the SQL through both frigolite and validates results.

func TestDialectCase(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, 'alice')")
	db.Exec("INSERT INTO t VALUES (2, 'bob')")

	res := db.Query("SELECT CASE WHEN id = 1 THEN 'one' ELSE 'other' END FROM t ORDER BY id")
	if res.Error != nil {
		t.Fatalf("CASE: %v", res.Error)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != "one" {
		t.Errorf("expected 'one', got %v", res.Rows[0][0])
	}
}

func TestDialectCaseExpr(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Query("SELECT CASE 1 WHEN 1 THEN 'one' WHEN 2 THEN 'two' ELSE 'other' END")
	if res.Error != nil {
		t.Fatalf("CASE expr: %v", res.Error)
	}
	if res.Rows[0][0] != "one" {
		t.Errorf("expected 'one', got %v", res.Rows[0][0])
	}
}

func TestDialectCast(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	db.Exec("INSERT INTO t VALUES (42)")

	res := db.Query("SELECT CAST(id AS TEXT) FROM t")
	if res.Error != nil {
		t.Fatalf("CAST: %v", res.Error)
	}
}

func TestDialectExists(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")

	res := db.Query("SELECT EXISTS(SELECT * FROM t WHERE id = 1)")
	if res.Error != nil {
		t.Fatalf("EXISTS: %v", res.Error)
	}
	if res.Rows[0][0] != int64(1) {
		t.Errorf("expected true, got %v", res.Rows[0][0])
	}

	res = db.Query("SELECT NOT EXISTS(SELECT * FROM t WHERE id = 999)")
	if res.Error != nil {
		t.Fatalf("NOT EXISTS: %v", res.Error)
	}
	if res.Rows[0][0] != int64(1) {
		t.Errorf("expected true for NOT EXISTS, got %v", res.Rows[0][0])
	}
}

func TestDialectSubquery(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, 100)")
	db.Exec("INSERT INTO t VALUES (2, 200)")

	res := db.Query("SELECT * FROM t WHERE id IN (SELECT id FROM t WHERE val > 150)")
	if res.Error != nil {
		t.Fatalf("Subquery IN: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(res.Rows))
	}
}

func TestDialectSubqueryScalar(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, 100)")
	db.Exec("INSERT INTO t VALUES (2, 200)")

	res := db.Query("SELECT * FROM t WHERE val > (SELECT val FROM t WHERE id = 1)")
	if res.Error != nil {
		t.Fatalf("Subquery scalar: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(res.Rows))
	}
}

func TestDialectJoin(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t1 (id INTEGER, name TEXT)")
	db.Exec("CREATE TABLE t2 (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t1 VALUES (1, 'alice')")
	db.Exec("INSERT INTO t1 VALUES (2, 'bob')")
	db.Exec("INSERT INTO t2 VALUES (1, 100)")
	db.Exec("INSERT INTO t2 VALUES (2, 200)")

	res := db.Query("SELECT t1.name, t2.val FROM t1 JOIN t2 ON t1.id = t2.id")
	if res.Error != nil {
		t.Fatalf("JOIN: %v", res.Error)
	}
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(res.Rows))
	}
}

func TestDialectAggregate(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (val INTEGER)")
	db.Exec("INSERT INTO t VALUES (10)")
	db.Exec("INSERT INTO t VALUES (20)")
	db.Exec("INSERT INTO t VALUES (30)")

	res := db.Query("SELECT COUNT(*), SUM(val), AVG(val), MIN(val), MAX(val) FROM t")
	if res.Error != nil {
		t.Fatalf("Aggregates: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != int64(3) {
		t.Errorf("COUNT: expected 3, got %v", res.Rows[0][0])
	}
}

func TestDialectOrderByDesc(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (2)")
	db.Exec("INSERT INTO t VALUES (3)")

	res := db.Query("SELECT id FROM t ORDER BY id DESC")
	if res.Error != nil {
		t.Fatalf("ORDER BY DESC: %v", res.Error)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != int64(3) {
		t.Errorf("expected 3 first, got %v", res.Rows[0][0])
	}
}

func TestDialectLimitOffset(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	for i := 1; i <= 10; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d)", i))
	}

	res := db.Query("SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 2")
	if res.Error != nil {
		t.Fatalf("LIMIT OFFSET: %v", res.Error)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != int64(3) {
		t.Errorf("expected 3 first, got %v", res.Rows[0][0])
	}
}

func TestDialectDistinct(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (2)")
	db.Exec("INSERT INTO t VALUES (1)")

	res := db.Query("SELECT DISTINCT val FROM t ORDER BY val")
	if res.Error != nil {
		t.Fatalf("DISTINCT: %v", res.Error)
	}
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(res.Rows))
	}
}

func TestDialectNotIn(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (2)")
	db.Exec("INSERT INTO t VALUES (3)")

	res := db.Query("SELECT id FROM t WHERE id NOT IN (2, 3)")
	if res.Error != nil {
		t.Fatalf("NOT IN: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(res.Rows))
	}
}
