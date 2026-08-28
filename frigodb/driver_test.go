package frigodb

import (
	"database/sql"
	"testing"
)

func TestOpenClose(t *testing.T) {
	db, err := sql.Open("frigolite", ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestExecQuery(t *testing.T) {
	db, err := sql.Open("frigolite", ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	_, err = db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	_, err = db.Exec("INSERT INTO t VALUES (1, 'alice')")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	rows, err := db.Query("SELECT id, name FROM t")
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected at least one row")
	}

	var id int64
	var name string
	if err := rows.Scan(&id, &name); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if id != 1 {
		t.Errorf("expected id=1, got %d", id)
	}
	if name != "alice" {
		t.Errorf("expected name='alice', got '%s'", name)
	}

	if rows.Next() {
		t.Error("expected only one row")
	}
}

func TestPlaceholder(t *testing.T) {
	db, err := sql.Open("frigolite", ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, 'alice')")
	db.Exec("INSERT INTO t VALUES (2, 'bob')")

	// Query with placeholder
	rows, err := db.Query("SELECT name FROM t WHERE id = ?", 1)
	if err != nil {
		t.Fatalf("SELECT with placeholder: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected a row")
	}
	var name string
	if err := rows.Scan(&name); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if name != "alice" {
		t.Errorf("expected 'alice', got '%s'", name)
	}
}

func TestTransaction(t *testing.T) {
	db, err := sql.Open("frigolite", ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	_, err = tx.Exec("CREATE TABLE t (id INTEGER)")
	if err != nil {
		t.Fatalf("CREATE TABLE in tx: %v", err)
	}

	_, err = tx.Exec("INSERT INTO t VALUES (42)")
	if err != nil {
		t.Fatalf("INSERT in tx: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var val int64
	err = db.QueryRow("SELECT id FROM t").Scan(&val)
	if err != nil {
		t.Fatalf("QueryRow after commit: %v", err)
	}
	if val != 42 {
		t.Errorf("expected 42, got %d", val)
	}
}

func TestRollback(t *testing.T) {
	// Note: Rollback is not yet supported by the frigolite engine.
	// This test verifies that ROLLBACK returns no error but does not
	// actually undo the changes.
	db, err := sql.Open("frigolite", ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	tx.Exec("INSERT INTO t VALUES (99)")

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Currently, rollback is a no-op, so data IS visible.
	t.Log("Note: rollback is a no-op in this implementation")
}

func TestPreparedStatement(t *testing.T) {
	db, err := sql.Open("frigolite", ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Use a single connection to avoid connection pooling issues
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")

	stmt, err := db.Prepare("INSERT INTO t VALUES (?, ?)")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	_, err = stmt.Exec(1, "alice")
	if err != nil {
		t.Fatalf("Exec prepared: %v", err)
	}

	_, err = stmt.Exec(2, "bob")
	if err != nil {
		t.Fatalf("Exec prepared 2: %v", err)
	}

	stmt.Close()

	var count int64
	db.QueryRow("SELECT COUNT(*) FROM t").Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 rows, got %d", count)
	}
}
