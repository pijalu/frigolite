package frigolite

import (
	"testing"
)

// TestDBStatDeclaredColumnTypes pins the stat-0.0/0.1b contract: PRAGMA
// table_info(dbstat) must expose the types declared by dbstat.c's
// zDbstatSchema, not empty strings. sqlite3 3.54 reports exactly these ten
// visible columns (schema and aggregate are hidden).
func TestDBStatDeclaredColumnTypes(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r := db.Query("PRAGMA table_info(dbstat)")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	want := []struct {
		name string
		typ  string
	}{
		{"name", "TEXT"},
		{"path", "TEXT"},
		{"pageno", "INTEGER"},
		{"pagetype", "TEXT"},
		{"ncell", "INTEGER"},
		{"payload", "INTEGER"},
		{"unused", "INTEGER"},
		{"mx_payload", "INTEGER"},
		{"pgoffset", "INTEGER"},
		{"pgsize", "INTEGER"},
	}
	if len(r.Rows) != len(want) {
		t.Fatalf("table_info(dbstat) rows = %v, want %d", r.Rows, len(want))
	}
	for i, w := range want {
		if len(r.Rows[i]) < 3 || r.Rows[i][1] != w.name || r.Rows[i][2] != w.typ {
			t.Fatalf("column %d = %v, want %s/%s", i, r.Rows[i], w.name, w.typ)
		}
	}
}
