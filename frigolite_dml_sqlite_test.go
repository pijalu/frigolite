package frigolite_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestDMLSQLitePersisted compares ordinary transactional DML and constraint errors
// against SQLite, closing and reopening both persisted databases between phases.
func TestSQLiteInsertUpdateDeletePersisted(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 oracle unavailable")
	}
	frigPath, sqlitePath := filepath.Join(t.TempDir(), "frigolite.db"), filepath.Join(t.TempDir(), "sqlite.db")
	frig, err := frigolite.Open(frigPath)
	if err != nil {
		t.Fatal(err)
	}
	mustExec := func(db *frigolite.DB, sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("Frigolite %s: %v", sql, r.Error)
		}
	}
	mustExec(frig, "CREATE TABLE items(id INTEGER PRIMARY KEY, qty INTEGER NOT NULL, note TEXT UNIQUE)")
	runSQLite(t, sqlitePath, "CREATE TABLE items(id INTEGER PRIMARY KEY, qty INTEGER NOT NULL, note TEXT UNIQUE);")
	var insert strings.Builder
	insert.WriteString("BEGIN;")
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&insert, "INSERT INTO items VALUES(%d,%d,'item-%d');", i, i%13, i)
	}
	insert.WriteString("COMMIT;")
	mustExec(frig, insert.String())
	runSQLite(t, sqlitePath, insert.String())
	mustExec(frig, "CREATE INDEX items_qty ON items(qty)")
	runSQLite(t, sqlitePath, "CREATE INDEX items_qty ON items(qty);")
	if err := frig.Close(); err != nil {
		t.Fatal(err)
	}
	frig, err = frigolite.Open(frigPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := frig.Close(); err != nil {
			t.Errorf("close Frigolite database: %v", err)
		}
	}()
	for _, sql := range []string{"BEGIN; UPDATE items SET qty=qty+10 WHERE id BETWEEN 101 AND 250; COMMIT;", "BEGIN; DELETE FROM items WHERE qty>=20; COMMIT;"} {
		if r := frig.Exec(sql); r.Error != nil {
			t.Fatalf("Frigolite %s: %v", sql, r.Error)
		}
		runSQLite(t, sqlitePath, sql)
		if err := frig.Close(); err != nil {
			t.Fatal(err)
		}
		frig, err = frigolite.Open(frigPath)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"SELECT id,qty,note FROM items ORDER BY id", "SELECT count(*),sum(qty) FROM items", "SELECT id FROM items WHERE note='item-300'"} {
		got := frig.Query(q)
		if got.Error != nil {
			t.Fatal(got.Error)
		}
		want := querySQLiteRows(t, sqlitePath, q)
		if len(got.Rows) != len(want) {
			t.Fatalf("query %s row counts: Frigolite %d SQLite %d", q, len(got.Rows), len(want))
		}
		for i := range want {
			for j := range want[i] {
				if fmt.Sprint(got.Rows[i][j]) != fmt.Sprint(want[i][j]) {
					t.Fatalf("query %s row %d column %d: Frigolite %v SQLite %v", q, i, j, got.Rows[i][j], want[i][j])
				}
			}
		}
	}
	for _, sql := range []string{"INSERT INTO items VALUES(900,1,'item-1')", "UPDATE items SET qty=NULL WHERE id=1", "DELETE FROM missing WHERE id=1"} {
		got, want := frig.Exec(sql), runSQLiteError(t, sqlitePath, sql)
		if got.Error == nil || !strings.Contains(want, strings.ToLower(got.Error.Error())) {
			t.Errorf("error mismatch for %q: frigolite=%v sqlite contains %q", sql, got.Error, want)
		}
	}
}

func runSQLite(t *testing.T, path, sql string) string {
	t.Helper()
	cmd := exec.Command("sqlite3", path)
	cmd.Stdin = strings.NewReader(sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 %q: %v: %s", sql, err, out)
	}
	return string(out)
}

func runSQLiteError(t *testing.T, path, sql string) string {
	t.Helper()
	cmd := exec.Command("sqlite3", path)
	cmd.Stdin = strings.NewReader(sql)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected sqlite error for %q", sql)
	}
	return strings.ToLower(strings.TrimSpace(string(out)))
}
func querySQLiteRows(t *testing.T, path, q string) [][]interface{} {
	t.Helper()
	out := runSQLite(t, path, ".mode tabs\n"+q+";")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	rows := make([][]interface{}, 0, len(lines))
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		row := make([]interface{}, len(fields))
		for i, field := range fields {
			if n, err := strconv.ParseInt(field, 10, 64); err == nil {
				row[i] = n
			} else {
				row[i] = field
			}
		}
		rows = append(rows, row)
	}
	return rows
}
