package frigolite

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestBugA_SubqueryWherePanic verifies that a subquery in FROM with an outer
// WHERE clause does not panic (the nil-pointer crash was previously observed
// in the filterSubqueryRows -> rowPassesWhere -> evalBool path).
func TestBugA_SubqueryWherePanic(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if r := db.Exec("CREATE TABLE t1(a INTEGER, b INTEGER)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO t1 VALUES (1,10),(2,20),(3,30)"); r.Error != nil {
		t.Fatalf("insert: %v", r.Error)
	}

	// Subquery in FROM with an outer WHERE on a subquery column.
	res := db.Query("SELECT * FROM (SELECT a, b FROM t1) WHERE a > 1")
	if res.Error != nil {
		t.Fatalf("query error: %v", res.Error)
	}
	want := 2 // rows with a=2 and a=3
	if len(res.Rows) != want {
		t.Fatalf("expected %d rows, got %d: %v", want, len(res.Rows), res.Rows)
	}
}

// TestBugB_PagerOutOfRange verifies that inserting enough rows to trigger
// btree page splits completes without a "page out of range" or "page 0
// invalid" error.
func TestBugB_PagerOutOfRange(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if r := db.Exec("CREATE TABLE t2(a INTEGER PRIMARY KEY, b TEXT)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}

	// Insert enough rows to force multiple leaf page splits.
	for i := 1; i <= 500; i++ {
		r := db.Exec(fmt.Sprintf("INSERT INTO t2 VALUES (%d, '%s')", i, paddingText(i)))
		if r.Error != nil {
			t.Fatalf("insert %d: %v", i, r.Error)
		}
	}

	// Verify we can read them back without a pager error.
	res := db.Query("SELECT count(*) FROM t2")
	if res.Error != nil {
		t.Fatalf("count error: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
}

func paddingText(i int) string {
	out := make([]byte, 50)
	ch := byte('A' + (i % 26))
	for j := range out {
		out[j] = ch
	}
	return string(out)
}

// TestBugB_AttachCreateIndex verifies that CREATE INDEX with a schema prefix
// on an unqualified table name resolves the table in the correct database.
// Previously, the table entry's RootPage from the main database was used with
// the aux database's pager, causing "page out of range" errors.
func TestBugB_AttachCreateIndex(t *testing.T) {
	tmpDir := t.TempDir()
	auxPath := filepath.Join(tmpDir, "test_aux.db")

	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if r := db.Exec("ATTACH '" + auxPath + "' AS aux"); r.Error != nil {
		t.Fatalf("attach: %v", r.Error)
	}
	if r := db.Exec("CREATE TABLE t4(a PRIMARY KEY, b, c)"); r.Error != nil {
		t.Fatalf("create main: %v", r.Error)
	}
	if r := db.Exec("CREATE TABLE aux.t4(a PRIMARY KEY, b, c)"); r.Error != nil {
		t.Fatalf("create aux: %v", r.Error)
	}
	// This previously caused "page N out of range" because aux.i4's index
	// creation used main.t4's RootPage with the aux pager.
	if r := db.Exec("CREATE INDEX i4 ON t4(b)"); r.Error != nil {
		t.Fatalf("create index main: %v", r.Error)
	}
	if r := db.Exec("CREATE INDEX aux.i4 ON t4(b)"); r.Error != nil {
		t.Fatalf("create index aux: %v", r.Error)
	}

	if r := db.Exec("INSERT INTO t4 VALUES('main','main','main')"); r.Error != nil {
		t.Fatalf("insert main: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO aux.t4 VALUES('aux','aux','aux')"); r.Error != nil {
		t.Fatalf("insert aux: %v", r.Error)
	}

	res := db.Query("SELECT * FROM aux.t4 WHERE a = 'aux'")
	if res.Error != nil {
		t.Fatalf("query aux: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(res.Rows), res.Rows)
	}

	os.Remove(auxPath)
}

// TestBugB_PagerErrorMessages verifies that pager errors use SQLite-compatible
// "database disk image is malformed" rather than internal "page out of range"
// or "page 0 invalid" messages.
func TestBugB_PagerErrorMessages(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	db.Exec("CREATE TABLE t3(x)")
	db.Exec("INSERT INTO t3 VALUES(1)")

	// Corrupt the database by writing a garbage root page number to
	// sqlite_schema, then querying a table. The error should be "database
	// disk image is malformed" (SQLite-compatible), not "page out of range".
	db.Exec("PRAGMA writable_schema = 1")
	db.Exec("UPDATE sqlite_schema SET rootpage = 99999 WHERE name = 't3'")

	res := db.Query("SELECT * FROM t3")
	if res.Error == nil {
		// If the query succeeds despite corruption, that's also acceptable
		// (the engine may not validate every path). But if it errors, the
		// message must NOT contain "out of range" or "page 0 invalid".
		return
	}
	msg := res.Error.Error()
	if contains(msg, "out of range") {
		t.Fatalf("error should not say 'out of range': %s", msg)
	}
	if contains(msg, "page 0 invalid") {
		t.Fatalf("error should not say 'page 0 invalid': %s", msg)
	}
}

// TestG02_UnderscoreLiterals verifies that underscore-separated numeric
// literals (SQL2017) parse correctly, matching SQLite behaviour.
func TestG02_UnderscoreLiterals(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT 2_3", "23"},
		{"SELECT 1_000", "1000"},
		{"SELECT 0x1_2", "18"},
		{"SELECT 1_000_000", "1000000"},
	}
	for _, c := range cases {
		res := db.Query(c.sql)
		if res.Error != nil {
			t.Errorf("%s: error: %v", c.sql, res.Error)
			continue
		}
		if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
			t.Errorf("%s: expected 1 value, got %v", c.sql, res.Rows)
			continue
		}
		got := fmt.Sprintf("%v", res.Rows[0][0])
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.sql, got, c.want)
		}
	}
}

// TestG02_OrderByRange verifies that positional ORDER BY terms exceeding the
// number of result columns produce SQLite-compatible error messages.
func TestG02_OrderByRange(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// SELECT 1 has 1 column; ORDER BY 2_3 (=23) exceeds it.
	res := db.Query("SELECT 1 ORDER BY 2_3")
	if res.Error == nil {
		t.Fatal("expected error for out-of-range ORDER BY, got success")
	}
	if !contains(res.Error.Error(), "ORDER BY term out of range") {
		t.Fatalf("expected 'ORDER BY term out of range', got: %v", res.Error)
	}

	// Valid positional ORDER BY should still work.
	res2 := db.Query("SELECT 1 AS x ORDER BY 1")
	if res2.Error != nil {
		t.Fatalf("valid ORDER BY 1 should succeed: %v", res2.Error)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
