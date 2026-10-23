package frigolite

import (
	"path/filepath"
	"testing"
)

func TestAnalyzeRejectsCorruptStatSchemaAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analyze.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE t1(a)",
		"INSERT INTO t1 VALUES(1)",
		"ANALYZE",
		"PRAGMA writable_schema=ON",
		"UPDATE sqlite_master SET sql='nonsense' WHERE name='sqlite_stat1'",
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("exec %q: %v", statement, result.Error)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result := db.Exec("ANALYZE")
	if result.Error == nil || result.Error.Error() != "malformed database schema (sqlite_stat1)" {
		t.Fatalf("ANALYZE error = %v, want malformed database schema (sqlite_stat1)", result.Error)
	}
}
