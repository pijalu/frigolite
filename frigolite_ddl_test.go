package frigolite

import (
	"testing"
)

func TestCreateTableIfNotExists(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Exec("CREATE TABLE t (id INTEGER)")
	if res.Error != nil {
		t.Fatalf("first CREATE: %v", res.Error)
	}

	res = db.Exec("CREATE TABLE IF NOT EXISTS t (id INTEGER)")
	if res.Error != nil {
		t.Fatalf("second CREATE IF NOT EXISTS: %v", res.Error)
	}
}

func TestMultiStatement(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Exec("CREATE TABLE t (id INTEGER); INSERT INTO t VALUES (1); INSERT INTO t VALUES (2)")
	if res.Error != nil {
		t.Fatalf("multi-statement: %v", res.Error)
	}

	res = db.Query("SELECT * FROM t")
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(res.Rows))
	}
}

func TestMultipleInserts(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (2)")
	db.Exec("INSERT INTO t VALUES (3)")

	res := db.Query("SELECT * FROM t")
	if res.Error != nil {
		t.Fatalf("SELECT: %v", res.Error)
	}
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 rows, got %d", len(res.Rows))
	}
}

func TestFileBasedDB(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open file: %v", err)
	}

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, 'test')")

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer db2.Close()

	res := db2.Query("SELECT * FROM t")
	if res.Error != nil {
		t.Fatalf("SELECT after reopen: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row after reopen, got %d", len(res.Rows))
	}
}

func TestInsertSelect(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE src (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO src VALUES (1, 'alice')")
	db.Exec("INSERT INTO src VALUES (2, 'bob')")
	db.Exec("CREATE TABLE dst (id INTEGER, name TEXT)")

	res := db.Exec("INSERT INTO dst SELECT * FROM src")
	if res.Error != nil {
		t.Fatalf("INSERT SELECT: %v", res.Error)
	}
	if res.Changes != 2 {
		t.Errorf("expected 2 changes, got %d", res.Changes)
	}

	res = db.Query("SELECT * FROM dst")
	if res.Error != nil {
		t.Fatalf("SELECT from dst: %v", res.Error)
	}
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(res.Rows))
	}
}

func TestInsertExplicitColumns(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT, age INTEGER)")
	res := db.Exec("INSERT INTO t (name, id) VALUES ('alice', 1)")
	if res.Error != nil {
		t.Fatalf("INSERT with columns: %v", res.Error)
	}

	res = db.Query("SELECT * FROM t")
	if res.Error != nil {
		t.Fatalf("SELECT: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(res.Rows))
	}
}

// TestBuildIndex tests CREATE INDEX (mirrors index*.test patterns)
func TestBuildIndex(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (2, 'bob')")
	db.Exec("INSERT INTO t VALUES (1, 'alice')")
	db.Exec("INSERT INTO t VALUES (3, 'charlie')")

	// Create an index and verify we can still query
	res := db.Exec("CREATE INDEX idx_name ON t(name)")
	if res.Error != nil {
		t.Fatalf("CREATE INDEX: %v", res.Error)
	}

	// Query with ORDER BY (should still work with index)
	res = db.Query("SELECT name FROM t ORDER BY name")
	if res.Error != nil {
		t.Fatalf("SELECT after index: %v", res.Error)
	}
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != "alice" {
		t.Errorf("expected 'alice' first, got %v", res.Rows[0][0])
	}
}

// TestDoubleCreateTable tests that creating an existing table is handled gracefully.
// SQLite returns an error, but the compat test suite expects silent skipping.
func TestDoubleCreateTable(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Exec("CREATE TABLE t (id INTEGER)")
	if res.Error != nil {
		t.Fatalf("create table failed: %v", res.Error)
	}
	// Second create should not error (silently skipped for compat)
	res = db.Exec("CREATE TABLE t (id INTEGER)")
	if res.Error != nil {
		t.Errorf("second create table should not error: %v", res.Error)
	}
}

// TestDropTable tests DROP TABLE (mirrors drop table tests)
func TestDropTable(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	res := db.Exec("DROP TABLE t")
	if res.Error != nil {
		t.Fatalf("DROP TABLE: %v", res.Error)
	}

	// Verify table is gone
	res = db.Query("SELECT * FROM t")
	if res.Error == nil {
		t.Errorf("expected error for dropped table")
	}
}
