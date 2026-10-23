package frigolite

import (
	"strings"
	"testing"
)

func TestCreateTableRejectsColumnAndTablePrimaryKey(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	result := db.Exec("CREATE TABLE t5(a INTEGER PRIMARY KEY, b, c, PRIMARY KEY(a))")
	if result.Error == nil || !strings.Contains(result.Error.Error(), `table "t5" has more than one primary key`) {
		t.Fatalf("duplicate primary key error = %v, want SQLite duplicate-primary-key error", result.Error)
	}
	if rows := db.Query("SELECT name FROM sqlite_master WHERE name='t5'"); rows.Error != nil || len(rows.Rows) != 0 {
		t.Fatalf("failed CREATE left schema entry: rows=%v error=%v", rows.Rows, rows.Error)
	}
}
