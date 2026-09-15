package frigolite_test

import (
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteOrderByQualifiedTiebreakPin pins tkt-2a5629202f: with
// ORDER BY x.b, x.c the second term must break ties among the x.b NULLs
// ('four' < 'three'), matching SQLite's sorter. The pre-evaluation of the
// first qualified term must not disturb resolution of the later terms.
func TestSQLiteOrderByQualifiedTiebreakPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	mustExec("CREATE TABLE t8(b TEXT, c TEXT)")
	mustExec("INSERT INTO t8 VALUES('a','one')")
	mustExec("INSERT INTO t8 VALUES('b','two')")
	mustExec("INSERT INTO t8 VALUES(NULL,'three')")
	mustExec("INSERT INTO t8 VALUES(NULL,'four')")

	r := db.Query("SELECT coalesce(b,'null') || '/' || c FROM t8 x ORDER BY x.b, x.c")
	if r.Error != nil {
		t.Fatalf("query: %v", r.Error)
	}
	want := []string{"null/four", "null/three", "a/one", "b/two"}
	if len(r.Rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(r.Rows), len(want))
	}
	for i, w := range want {
		if r.Rows[i][0] != w {
			t.Fatalf("row %d: got %v, want %q", i, r.Rows[i][0], w)
		}
	}
}
