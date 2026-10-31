package exec

import (
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
)

// Integrity-check index pins: the e_reindex-1.3 contract (oracle-verified
// against /usr/bin/sqlite3, SQLite 3.54 pragma.c integrityCheck). The
// corruption setup is exactly e_reindex-1.1/1.2 across connection reopen
// boundaries (the harness models them as close_db/reopen_db steps): index
// entries are frozen by deleting the index rows from sqlite_master
// (writable_schema) and reopening, the table then loses rowid 2 and gains
// rowids 4 and 5 with no index maintenance, and the index definitions are
// restored — leaving each index b-tree holding entries for rowids 1,2,3
// while the table holds rows at rowids 1,3,4,5.

// integrityFileEngine opens a fresh engine over a file-backed database.
func integrityFileEngine(t *testing.T, path string) *Engine {
	t.Helper()
	pg, err := pager.Open(path, pager.DefaultPageSize)
	if err != nil {
		t.Fatalf("open pager: %v", err)
	}
	e := NewEngine(pg)
	if err := e.schema.Init(); err != nil {
		t.Fatalf("schema init: %v", err)
	}
	return e
}

// closeIntegrityEngine closes the engine (and its pager) the way the
// harness's close_db step does.
func closeIntegrityEngine(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Close(); err != nil {
		t.Fatalf("engine close: %v", err)
	}
}

// newIntegrityCorruptEngine runs the e_reindex-1.1 + reopen + e_reindex-1.2
// + reopen sequence and returns the reopened connection (the 1.3 state).
func newIntegrityCorruptEngine(t *testing.T, path string) *Engine {
	t.Helper()
	e := integrityFileEngine(t, path)
	execAll(t, e,
		"CREATE TABLE t1(a, b)",
		"CREATE INDEX i1 ON t1(a, b)",
		"CREATE INDEX i2 ON t1(b, a)",
		"INSERT INTO t1 VALUES(1, 2)",
		"INSERT INTO t1 VALUES(3, 4)",
		"INSERT INTO t1 VALUES(5, 6)",
		"CREATE TABLE saved(a,b,c,d,e)",
		"INSERT INTO saved SELECT * FROM sqlite_master WHERE type = 'index'",
		"PRAGMA writable_schema = 1",
		"DELETE FROM sqlite_master WHERE type = 'index'",
	)
	closeIntegrityEngine(t, e)

	// Reopen (harness setup_1): the connection no longer knows the indexes,
	// so the following DML performs no index maintenance.
	e = integrityFileEngine(t, path)
	execAll(t, e,
		"DELETE FROM t1 WHERE a = 3",
		"INSERT INTO t1 VALUES(7, 8)",
		"INSERT INTO t1 VALUES(9, 10)",
		"PRAGMA writable_schema = 1",
		"INSERT INTO sqlite_master SELECT * FROM saved",
		"DROP TABLE saved",
	)
	closeIntegrityEngine(t, e)

	// Reopen (harness setup_2): the stale index btrees are now visible.
	return integrityFileEngine(t, path)
}

func execAll(t *testing.T, e *Engine, sqls ...string) {
	t.Helper()
	for _, s := range sqls {
		if res := e.Exec(mustParse(t, s)); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
}

// pragmaStrings runs a pragma query and returns its single-column rows.
func pragmaStrings(t *testing.T, e *Engine, pragma string) []string {
	t.Helper()
	res := e.Exec(mustParse(t, pragma))
	if res.Error != nil {
		t.Fatalf("%s: %v", pragma, res.Error)
	}
	out := make([]string, 0, len(res.Rows))
	for i, r := range res.Rows {
		if len(r) != 1 {
			t.Fatalf("%s: row %d has %d columns", pragma, i, len(r))
		}
		s, ok := r[0].(string)
		if !ok {
			t.Fatalf("%s: row %d is %T, want string", pragma, i, r[0])
		}
		out = append(out, s)
	}
	return out
}

func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d = %q, want %q (full: got %v, want %v)", what, i, got[i], want[i], got, want)
		}
	}
}

// TestPinIntegrityCheckStaleIndexReports pins the exact e_reindex-1.3 rows
// and their order: per-index entry-count mismatches first (newest index
// first — SQLite's Table.pIndex list is prepend-ordered), then the per-row
// missing entries in rowid scan order. The "row N" number is the 1-based
// scan ordinal, not the rowid (the missing rows sit at rowids 4 and 5).
func TestPinIntegrityCheckStaleIndexReports(t *testing.T) {
	e := newIntegrityCorruptEngine(t, filepath.Join(t.TempDir(), "corrupt.db"))
	defer closeIntegrityEngine(t, e)

	assertStrings(t, "integrity_check", pragmaStrings(t, e, "PRAGMA integrity_check"),
		[]string{
			"wrong # of entries in index i2",
			"wrong # of entries in index i1",
			"row 3 missing from index i2",
			"row 3 missing from index i1",
			"row 4 missing from index i2",
			"row 4 missing from index i1",
		})

	// quick_check runs the entry-count comparison but skips the per-row
	// presence pass (pragma.c: "Omit the remaining tests for quick_check").
	assertStrings(t, "quick_check", pragmaStrings(t, e, "PRAGMA quick_check"),
		[]string{
			"wrong # of entries in index i2",
			"wrong # of entries in index i1",
		})

	// REINDEX rebuilds the index btrees; the check returns to ok
	// (e_reindex-1.4).
	execAll(t, e, "REINDEX")
	assertStrings(t, "integrity_check after REINDEX", pragmaStrings(t, e, "PRAGMA integrity_check"),
		[]string{"ok"})
}

// TestPinIntegrityCheckHealthyIndexesOk pins the common path: a database
// exercising every index shape the walk understands (multi-column, DESC,
// expression key, partial, WITHOUT ROWID secondary index) reports exactly
// "ok" for both pragmas.
//
// The WITHOUT ROWID table's secondary index is exercised with inserts only:
// the engine's WR DELETE path removes [key..., 0] while inserts wrote
// [key..., insert-time rowid], so a deleted WR row's secondary-index entry
// survives as a genuine desync (pre-existing execdml behavior, outside this
// unit) that integrity_check correctly reports as a wrong entry count.
func TestPinIntegrityCheckHealthyIndexesOk(t *testing.T) {
	e := NewEngine(pager.OpenInMemory(pager.DefaultPageSize))
	if err := e.schema.Init(); err != nil {
		t.Fatalf("schema init: %v", err)
	}
	execAll(t, e,
		"CREATE TABLE t1(a, b, c)",
		"CREATE INDEX i_plain ON t1(a, b)",
		"CREATE INDEX i_desc ON t1(b DESC, a)",
		"CREATE INDEX i_expr ON t1(substr(c, 2, 3), a)",
		"CREATE INDEX i_part ON t1(a) WHERE b > 10",
		"INSERT INTO t1 VALUES(1, 20, 'xabc')",
		"INSERT INTO t1 VALUES(2, 5, 'ydef')",
		"INSERT INTO t1 VALUES(3, 30, 'zghi')",
		"DELETE FROM t1 WHERE a = 2",
		"INSERT INTO t1 VALUES(4, 40, 'wjkl')",
		"CREATE TABLE wr(p TEXT PRIMARY KEY, v INTEGER) WITHOUT ROWID",
		"CREATE INDEX wr_v ON wr(v)",
		"INSERT INTO wr VALUES('k1', 1)",
		"INSERT INTO wr VALUES('k2', 2)",
	)
	assertStrings(t, "integrity_check healthy", pragmaStrings(t, e, "PRAGMA integrity_check"),
		[]string{"ok"})
	assertStrings(t, "quick_check healthy", pragmaStrings(t, e, "PRAGMA quick_check"),
		[]string{"ok"})

	// Scoped forms on a healthy database stay "ok" too.
	assertStrings(t, "scoped integrity_check", pragmaStrings(t, e, "PRAGMA integrity_check(t1)"),
		[]string{"ok"})
}

// TestPinIntegrityCheckRowOrdinalNotRowid pins the scan-ordinal semantics
// across a reopen: rows at rowids 1 and 2 keep their index entries, the
// post-reopen insert (rowid 3) does not, and the missing row reports as
// "row 3" — the third scanned row, which here coincides with the rowid.
// The e_reindex-1.3 pin above covers the divergent case (ordinals 3 and 4
// for rowids 4 and 5).
func TestPinIntegrityCheckRowOrdinalNotRowid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ordinal.db")
	e := integrityFileEngine(t, path)
	execAll(t, e,
		"CREATE TABLE t1(a, b)",
		"CREATE INDEX i1 ON t1(a, b)",
		"INSERT INTO t1 VALUES(1, 2)",
		"INSERT INTO t1 VALUES(3, 4)",
		"CREATE TABLE saved(a,b,c,d,e)",
		"INSERT INTO saved SELECT * FROM sqlite_master WHERE type = 'index'",
		"PRAGMA writable_schema = 1",
		"DELETE FROM sqlite_master WHERE type = 'index'",
	)
	closeIntegrityEngine(t, e)

	e = integrityFileEngine(t, path)
	defer closeIntegrityEngine(t, e)
	execAll(t, e,
		"INSERT INTO t1 VALUES(5, 6)",
		"PRAGMA writable_schema = 1",
		"INSERT INTO sqlite_master SELECT * FROM saved",
		"DROP TABLE saved",
	)
	assertStrings(t, "integrity_check", pragmaStrings(t, e, "PRAGMA integrity_check"),
		[]string{
			"wrong # of entries in index i1",
			"row 3 missing from index i1",
		})
}
