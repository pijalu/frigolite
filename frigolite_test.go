package frigolite

import (
	"os"
	"testing"
)

func setupDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

func TestOpenClose(t *testing.T) {
	db := setupDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestExec(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Exec("CREATE TABLE test (id INTEGER, name TEXT)")
	if res.Error != nil {
		t.Fatalf("CREATE TABLE: %v", res.Error)
	}

	res = db.Exec("INSERT INTO test VALUES (1, 'Alice')")
	if res.Error != nil {
		t.Fatalf("INSERT: %v", res.Error)
	}
	if res.Changes != 1 {
		t.Errorf("INSERT changes: got %d, want 1", res.Changes)
	}
}

func TestEmptyResult(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Query("SELECT * FROM nonexistent")
	if res.Error == nil {
		t.Errorf("expected error for nonexistent table")
	}
}

func TestParseErrors(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Exec("INVALID SQL")
	if res.Error == nil {
		t.Errorf("expected error for invalid SQL")
	}
}

func TestFileExists(t *testing.T) {
	path := t.TempDir() + "/test.db"
	if FileExists(path) {
		t.Errorf("file should not exist yet")
	}
	db, _ := Open(path)
	db.Close()
	if !FileExists(path) {
		t.Errorf("file should exist now")
	}
	os.Remove(path)
}

func TestDumpAll(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t1 (id INTEGER)")
	db.Exec("INSERT INTO t1 VALUES (1)")

	db.DumpAll()
}
