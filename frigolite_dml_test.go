package frigolite

import (
	"fmt"
	"testing"
)

func TestInsertAndSelect(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE users (id INTEGER, name TEXT, age INTEGER)")

	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("User %d", i)
		age := 20 + i
		res := db.Exec(fmt.Sprintf("INSERT INTO users VALUES (%d, '%s', %d)", i, name, age))
		if res.Error != nil {
			t.Fatalf("INSERT %d: %v", i, res.Error)
		}
	}

	res := db.Query("SELECT * FROM users")
	if res.Error != nil {
		t.Fatalf("SELECT: %v", res.Error)
	}
	if len(res.Rows) != 5 {
		t.Errorf("expected 5 rows, got %d", len(res.Rows))
	}
}

func TestBulkOperations(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")

	for i := 0; i < 100; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i*2))
	}

	res := db.Query("SELECT * FROM t")
	if res.Error != nil {
		t.Fatalf("SELECT after bulk insert: %v", res.Error)
	}
	if len(res.Rows) != 100 {
		t.Errorf("expected 100 rows, got %d", len(res.Rows))
	}
}

func TestStringLiterals(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (s TEXT)")
	db.Exec("INSERT INTO t VALUES ('hello world')")
	db.Exec("INSERT INTO t VALUES ('it''s working')")

	res := db.Query("SELECT * FROM t")
	if res.Error != nil {
		t.Fatalf("SELECT: %v", res.Error)
	}
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(res.Rows))
	}
	if res.Rows[1][0] != "it's working" {
		t.Errorf("expected 'it''s working', got '%v'", res.Rows[1][0])
	}
}

func TestColumnNames(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, 'hello')")

	res := db.Query("SELECT id, name FROM t")
	if res.Error != nil {
		t.Fatalf("SELECT: %v", res.Error)
	}
	if len(res.Columns) != 2 {
		t.Errorf("expected 2 columns, got %d: %v", len(res.Columns), res.Columns)
	}
}

func TestUpdate(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, 10)")
	db.Exec("INSERT INTO t VALUES (2, 20)")
	db.Exec("INSERT INTO t VALUES (3, 30)")

	res := db.Exec("UPDATE t SET val = 99 WHERE id = 2")
	if res.Error != nil {
		t.Fatalf("UPDATE: %v", res.Error)
	}
	if res.Changes != 1 {
		t.Errorf("expected 1 change, got %d", res.Changes)
	}

	res = db.Query("SELECT val FROM t WHERE id = 2")
	if res.Error != nil || len(res.Rows) == 0 {
		t.Fatalf("SELECT after UPDATE: %v", res.Error)
	}
	found := false
	for _, row := range res.Rows {
		if v, ok := row[0].(int64); ok && v == 99 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected val=99, got %v", res.Rows)
	}
}

func TestUpdateAll(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, 10)")
	db.Exec("INSERT INTO t VALUES (2, 20)")

	res := db.Exec("UPDATE t SET val = 0")
	if res.Error != nil {
		t.Fatalf("UPDATE all: %v", res.Error)
	}

	res = db.Query("SELECT val FROM t WHERE id = 1")
	if res.Error != nil {
		t.Fatalf("SELECT: %v", res.Error)
	}
	found := false
	for _, row := range res.Rows {
		if v, ok := row[0].(int64); ok && v == 0 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected at least one row with val=0, got %v", res.Rows)
	}
}

func TestDelete(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, 10)")
	db.Exec("INSERT INTO t VALUES (2, 20)")
	db.Exec("INSERT INTO t VALUES (3, 30)")

	res := db.Exec("DELETE FROM t WHERE id = 1")
	if res.Error != nil {
		t.Fatalf("DELETE: %v", res.Error)
	}

	res = db.Query("SELECT * FROM t")
	if res.Error != nil {
		t.Fatalf("SELECT after DELETE: %v", res.Error)
	}
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows after DELETE, got %d", len(res.Rows))
	}
}

// TestDeleteAll tests DELETE without WHERE (mirrors delete.test patterns)
func TestDeleteAll(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, 10)")
	db.Exec("INSERT INTO t VALUES (2, 20)")
	db.Exec("INSERT INTO t VALUES (3, 30)")

	res := db.Exec("DELETE FROM t")
	if res.Error != nil {
		t.Fatalf("DELETE all: %v", res.Error)
	}

	res = db.Query("SELECT * FROM t")
	if res.Error != nil {
		t.Fatalf("SELECT after DELETE all: %v", res.Error)
	}
	if len(res.Rows) != 0 {
		t.Errorf("expected 0 rows after DELETE all, got %d", len(res.Rows))
	}
}

// TestUpdateWithExpr tests UPDATE with expressions (mirrors update.test patterns)
func TestUpdateWithExpr(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, 2)")
	db.Exec("INSERT INTO t VALUES (2, 4)")
	db.Exec("INSERT INTO t VALUES (3, 8)")

	// UPDATE val = val * 3
	res := db.Exec("UPDATE t SET val = val * 3")
	if res.Error != nil {
		t.Fatalf("UPDATE with expr: %v", res.Error)
	}
	if res.Changes != 3 {
		t.Errorf("expected 3 changes, got %d", res.Changes)
	}

	res = db.Query("SELECT val FROM t WHERE id = 1")
	if res.Error != nil {
		t.Fatalf("SELECT: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != float64(6) {
		t.Errorf("expected 6, got %v", res.Rows[0][0])
	}

	// Swap columns: SET val = id, id = val
	res = db.Exec("UPDATE t SET val = id, id = val")
	if res.Error != nil {
		t.Fatalf("UPDATE swap: %v", res.Error)
	}

	res = db.Query("SELECT id, val FROM t WHERE id = 6.0")
	if res.Error != nil {
		t.Fatalf("SELECT after swap: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
}

// TestInsertNull tests INSERT with NULL values (mirrors insert.test patterns)
func TestInsertNull(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, NULL, 100)")
	db.Exec("INSERT INTO t VALUES (2, 'hello', NULL)")

	res := db.Query("SELECT * FROM t ORDER BY id")
	if res.Error != nil {
		t.Fatalf("SELECT: %v", res.Error)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][1] != nil {
		t.Errorf("expected nil for name, got %v", res.Rows[0][1])
	}
	if res.Rows[1][2] != nil {
		t.Errorf("expected nil for val, got %v", res.Rows[1][2])
	}
}
