package frigolite_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteAutoincSequenceShapePin pins autoinc.test 12.4-12.7's
// sqlite_sequence shape contract (insert.c autoIncBegin, ticket
// d8dc2b3a58cd5dc2918a1d4acb): the sequence table must exist as an ordinary
// rowid table with EXACTLY two columns, read positionally —
//   - a writable_schema rewrite to a 1-column declaration corrupts the next
//     AUTOINCREMENT insert ("database disk image is malformed", 12.5);
//   - any other 2-column spelling keeps working because the sequence update
//     addresses the columns by position, not name (12.6: (x,y), 12.7:
//     (y INTEGER PRIMARY KEY, x)).
//
// The generated autoinc package cannot assert 12.6/12.7 (the transpiled
// catchsql drops the trailing PRAGMA integrity_check result), so the
// engine-visible behavior is pinned here.
func TestSQLiteAutoincSequenceShapePin(t *testing.T) {
	// 12.5: 1-column sqlite_sequence → malformed.
	t.Run("one_column_malformed", func(t *testing.T) {
		dir := t.TempDir()
		db, err := frigolite.Open(filepath.Join(dir, "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetDefensive(false)
		if r := db.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b TEXT);\n" +
			"INSERT INTO t1(b) VALUES('one');\n" +
			"PRAGMA writable_schema=on;\n" +
			"UPDATE sqlite_master SET sql='CREATE TABLE sqlite_sequence(x)' WHERE name='sqlite_sequence'"); r.Error != nil {
			t.Fatalf("setup: %v", r.Error)
		}
		// Reopen so the corrupted schema is parsed the way a fresh
		// connection sees it.
		db.Close()
		db2, err := frigolite.Open(filepath.Join(dir, "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db2.Close()
		r := db2.Exec("INSERT INTO t1(b) VALUES('two')")
		if r.Error == nil || !strings.Contains(r.Error.Error(), "database disk image is malformed") {
			t.Fatalf("12.5: got %v, want 'database disk image is malformed'", r.Error)
		}
	})

	// 12.6/12.7: renamed/reordered 2-column spellings keep working.
	for name, decl := range map[string]string{
		"renamed":      "CREATE TABLE sqlite_sequence(x, y INTEGER PRIMARY KEY)",
		"reordered":    "CREATE TABLE sqlite_sequence(y INTEGER PRIMARY KEY, x)",
		"renamed_bare": "CREATE TABLE sqlite_sequence(x, y)",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := frigolite.Open(filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetDefensive(false)
			rewrite := "UPDATE sqlite_master SET sql='" + decl + "' WHERE name='sqlite_sequence'"
			if r := db.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b TEXT);\n" +
				"INSERT INTO t1(b) VALUES('one');\n" +
				"PRAGMA writable_schema=on;\n" + rewrite); r.Error != nil {
				t.Fatalf("setup: %v", r.Error)
			}
			// More AUTOINCREMENT inserts over the renamed sequence, then an
			// integrity check — the 12.6/12.7 statement batch.
			if r := db.Exec("INSERT INTO t1(b) VALUES('two'),('three'),('four');\n" +
				"INSERT INTO t1(b) VALUES('five')"); r.Error != nil {
				t.Fatalf("%s inserts: %v", name, r.Error)
			}
			q := db.Query("PRAGMA integrity_check")
			if q.Error != nil {
				t.Fatalf("%s integrity_check: %v", name, q.Error)
			}
			if len(q.Rows) == 0 || q.Rows[0][0] != "ok" {
				t.Fatalf("%s integrity_check: got %v want ok", name, q.Rows)
			}
			// The sequence kept counting by position: the last docid is 5.
			q = db.Query("SELECT count(*) FROM t1")
			if q.Error != nil || q.Rows[0][0] != int64(5) {
				t.Fatalf("%s count: %v %v", name, q.Error, q.Rows)
			}
		})
	}
}
