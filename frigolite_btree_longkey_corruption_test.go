package frigolite_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

func TestLongKeyIndexSplitIntegrity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long-key.db")
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExecLongKey(t, db, "CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)")
	mustExecLongKey(t, db, "CREATE INDEX ti ON t(b)")
	mustExecLongKey(t, db, "BEGIN")
	for i := 0; i < 20000; i++ {
		mustExecLongKey(t, db, fmt.Sprintf("INSERT INTO t(b) VALUES('v%d')", i))
	}
	mustExecLongKey(t, db, "COMMIT")
	mustExecLongKey(t, db, "BEGIN")
	for k := 0; k < 23; k++ {
		i := k * 3
		mustExecLongKey(t, db, fmt.Sprintf("INSERT INTO t(a,b) VALUES(%d,'longvalue%dxxxxxxxxxxxxxxxxxxxxxxxxxxxx')", i+30000, i))
		assertLongKeyIntegrity(t, db, fmt.Sprintf("after long-key insert %d", k+1))
	}
	mustExecLongKey(t, db, "COMMIT")
	assertLongKeyIntegrity(t, db, "after indexed inserts")
	rows := db.Query("SELECT b FROM t INDEXED BY ti ORDER BY b")
	if rows.Error != nil {
		t.Fatal(rows.Error)
	}
	for i := 1; i < len(rows.Rows); i++ {
		if rows.Rows[i-1][0].(string) > rows.Rows[i][0].(string) {
			t.Fatalf("index order descends at row %d: %q > %q", i, rows.Rows[i-1][0], rows.Rows[i][0])
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	assertSQLiteIntegrityLongKey(t, path, "before VACUUM")
	db, err = frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExecLongKey(t, db, "VACUUM")
	assertLongKeyIntegrity(t, db, "after VACUUM")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	assertSQLiteIntegrityLongKey(t, path, "after VACUUM")
}

func TestLongKeyUpdateGrowthIntegrity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long-key-update.db")
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExecLongKey(t, db, "CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)")
	mustExecLongKey(t, db, "CREATE INDEX ti ON t(b)")
	mustExecLongKey(t, db, "BEGIN")
	for i := 0; i < 20000; i++ {
		mustExecLongKey(t, db, fmt.Sprintf("INSERT INTO t(b) VALUES('v%d')", i))
	}
	mustExecLongKey(t, db, "COMMIT")
	mustExecLongKey(t, db, "BEGIN")
	for i := 0; i < 6667; i++ {
		rowID := i * 3
		mustExecLongKey(t, db, fmt.Sprintf("UPDATE t SET b='longvalue%dxxxxxxxxxxxxxxxxxxxxxxxxxxxx' WHERE a=%d", rowID, rowID))
	}
	mustExecLongKey(t, db, "COMMIT")
	mustExecLongKey(t, db, "DELETE FROM t WHERE a%5=0")
	assertLongKeyIntegrity(t, db, "after UPDATE growth and DELETE")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	assertSQLiteIntegrityLongKey(t, path, "after UPDATE growth, before VACUUM")
	db, err = frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExecLongKey(t, db, "VACUUM")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	assertSQLiteIntegrityLongKey(t, path, "after UPDATE growth VACUUM")
}

func assertSQLiteIntegrityLongKey(t *testing.T, path, phase string) {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 CLI unavailable for independent integrity_check")
	}
	output, err := exec.Command(bin, path, "PRAGMA integrity_check;").CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 integrity_check %s: %v: %s", phase, err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "ok" {
		t.Fatalf("SQLite integrity_check %s = %q, want ok", phase, got)
	}
}

func mustExecLongKey(t *testing.T, db *frigolite.DB, sql string) {
	t.Helper()
	if r := db.Exec(sql); r != nil && r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
}

func assertLongKeyIntegrity(t *testing.T, db *frigolite.DB, phase string) {
	t.Helper()
	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != "ok" {
		t.Fatalf("%s integrity_check = %v, error %v", phase, r.Rows, r.Error)
	}
}
