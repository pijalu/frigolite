package frigolite

// frigolite_walconformance_test.go — UCL read-parity: replay the observable
// state of the oracle-generated walconformance fixtures (U2) through the
// frigolite storage layer. Row expectations come from the scenario SQL
// executed by the oracle CLI (U1), never from frigolite output.
//
// Fixture state notes:
//   - wal-* scenarios: the oracle session closed cleanly, so the committed
//     .db holds ALL scenario rows (auto-checkpoint on close).
//   - jrnl-* scenarios: the final transaction was left open at snapshot
//     time and rolled back on oracle exit, so the .db holds only the rows
//     committed BEFORE the final BEGIN. The -journal sidecar is the live
//     mid-transaction image decoded by internal/pager (jrnlview.go); it is
//     not copied here because its zeroed-magic pre-sync header
//     (pager.c L1488) must not trigger rollback playback.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fixtureAbs resolves a committed fixture to an absolute path (root
// package tests may chdir, so relative paths are unreliable).
func fixtureAbs(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata", "walconformance", name+".db")
}

// openFixtureDB copies a committed fixture into a temp dir (frigolite may
// write on open) and opens it.
func openFixtureDB(t *testing.T, name string) *DB {
	t.Helper()
	buf, err := os.ReadFile(fixtureAbs(t, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	dst := filepath.Join(t.TempDir(), name+".db")
	if err := os.WriteFile(dst, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dst)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func queryStrings(t *testing.T, db *DB, q string) []string {
	t.Helper()
	r := db.Query(q)
	if r.Error != nil {
		t.Fatalf("query %q: %v", q, r.Error)
	}
	var out []string
	for _, row := range r.Rows {
		for _, col := range row {
			out = append(out, fmt.Sprintf("%v", col))
		}
	}
	return out
}

func TestWALConformanceReadParity(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		// All three autocommit inserts checkpointed into the db on close.
		{"wal-single-commit", "SELECT a, b FROM t1 ORDER BY a",
			[]string{"1", "one", "2", "two"}},
		// CREATE + insert + explicit BEGIN/COMMIT + t2 with zeroblob.
		{"wal-multi-commit", "SELECT a, b FROM t1 ORDER BY a",
			[]string{"1", "one", "2", "two", "3", "three"}},
		{"wal-multi-commit", "SELECT length(x) FROM t2",
			[]string{"3000"}},
		// RESTART checkpoint then two more inserts: all four rows present.
		{"wal-after-checkpoint", "SELECT a FROM t1 ORDER BY a",
			[]string{"1", "2", "3"}},
		// jrnl scenarios: rows from committed statements only; the final
		// open transaction (VALUES 3/4 resp. 999) rolled back.
		{"jrnl-persist-basic", "SELECT a, b FROM t1 ORDER BY a",
			[]string{"1", "one", "2", "two"}},
		{"jrnl-persist-multi", "SELECT count(*) FROM t1",
			[]string{"8"}},
		{"jrnl-persist-multi", "SELECT b FROM t1 WHERE a=1",
			[]string{"last"}},
	}
	for _, c := range cases {
		db := openFixtureDB(t, c.name)
		got := queryStrings(t, db, c.sql)
		if len(got) != len(c.want) {
			t.Fatalf("%s: query %q returned %v, want %v (first divergence: row count)", c.name, c.sql, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: query %q got %v, want %v (first divergence at column %d)",
					c.name, c.sql, got, c.want, i)
			}
		}
	}
}
