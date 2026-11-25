package frigolite

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestCrossEngineFileMatrix extends TestS66SQLiteFileCompatibility beyond its
// single blob-index shape. For each on-disk shape both engines write the same
// database, then the other engine reads it back: integrity_check plus the
// complete row set must agree. This is the file-format contract the parity
// goal requires, not a comparison of internal page layout.
func TestCrossEngineFileMatrix(t *testing.T) {
	sqlite3, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 oracle unavailable")
	}

	shapes := []struct {
		name    string
		ddl     string
		inserts string
		query   string
	}{
		{
			name: "rowid-overflow",
			ddl:  "CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b BLOB);",
			inserts: func() string {
				var b strings.Builder
				for i := 1; i <= 8; i++ {
					// 3000-byte text forces overflow pages at 4K pages.
					fmt.Fprintf(&b, "INSERT INTO t VALUES(%d, '%s', X'0A0B0C');\n", i, strings.Repeat("v", 3000))
				}
				return b.String()
			}(),
			query: "SELECT id FROM t ORDER BY id;",
		},
		{
			name:    "without-rowid",
			ddl:     "CREATE TABLE t(k TEXT PRIMARY KEY, v INTEGER) WITHOUT ROWID;",
			inserts: "INSERT INTO t VALUES('alpha',1);\nINSERT INTO t VALUES('beta',2);\nINSERT INTO t VALUES('gamma',3);\n",
			query:   "SELECT k||':'||v FROM t ORDER BY k;",
		},
		{
			name:    "indexed",
			ddl:     "CREATE TABLE t(a INTEGER, b TEXT);\nCREATE INDEX ti ON t(a,b);",
			inserts: "INSERT INTO t VALUES(3,'c');\nINSERT INTO t VALUES(1,'a');\nINSERT INTO t VALUES(2,'b');\n",
			query:   "SELECT a||':'||b FROM t ORDER BY a;",
		},
		{
			name:    "auto-vacuum",
			ddl:     "PRAGMA auto_vacuum=FULL;\nCREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT);",
			inserts: "INSERT INTO t VALUES(1,'one');\nINSERT INTO t VALUES(2,'two');\nDELETE FROM t WHERE a=1;\n",
			query:   "SELECT a||':'||b FROM t ORDER BY a;",
		},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			dir := t.TempDir()
			frigPath := filepath.Join(dir, "frigolite.db")
			sqlPath := filepath.Join(dir, "sqlite.db")
			script := shape.ddl + "\n" + shape.inserts

			// Frigolite writes, sqlite3 reads.
			fdb, err := Open(frigPath)
			if err != nil {
				t.Fatal(err)
			}
			if r := fdb.Exec(script); r.Error != nil {
				t.Fatalf("frigolite write: %v", r.Error)
			}
			if err := fdb.Close(); err != nil {
				t.Fatal(err)
			}
			fromSQLite := readSQLiteFile(t, sqlite3, frigPath, shape.query)

			// sqlite3 writes, Frigolite reads.
			cmd := exec.Command(sqlite3, sqlPath)
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("sqlite3 write: %v (%s)", err, out)
			}
			sdb, err := Open(sqlPath)
			if err != nil {
				t.Fatal(err)
			}
			defer sdb.Close()
			fromFrigolite := readFrigoliteFile(t, sdb, shape.query)

			if !reflect.DeepEqual(fromSQLite, fromFrigolite) {
				t.Fatalf("row sets differ: sqlite3 reading Frigolite file = %v; Frigolite reading sqlite3 file = %v", fromSQLite, fromFrigolite)
			}
		})
	}
}

// readSQLiteFile checks the Frigolite-written file's integrity and returns
// the query's rows as sqlite3 renders them.
func readSQLiteFile(t *testing.T, sqlite3, path, query string) []string {
	t.Helper()
	out, err := exec.Command(sqlite3, "-noheader", path, "PRAGMA integrity_check;", query).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 read Frigolite file: %v (%s)", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "ok" {
		t.Fatalf("sqlite3 report on Frigolite file = %q, want integrity_check ok + rows", lines)
	}
	rows := make([]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		rows = append(rows, strings.TrimSpace(line))
	}
	return rows
}

// readFrigoliteFile checks the sqlite3-written file's integrity and returns
// the query's rows using the same text rendering as the sqlite3 CLI.
func readFrigoliteFile(t *testing.T, db *DB, query string) []string {
	t.Helper()
	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil || len(r.Rows) != 1 || fmt.Sprint(r.Rows[0][0]) != "ok" {
		t.Fatalf("Frigolite integrity_check = %v %v", r.Error, r.Rows)
	}
	r = db.Query(query)
	if r.Error != nil {
		t.Fatalf("Frigolite query %q: %v", query, r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatalf("Frigolite query %q returned no rows", query)
	}
	rows := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		cells := make([]string, 0, len(row))
		for _, cell := range row {
			cells = append(cells, formatCrossEngineCell(cell))
		}
		rows = append(rows, strings.Join(cells, "|"))
	}
	return rows
}

func formatCrossEngineCell(v interface{}) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
