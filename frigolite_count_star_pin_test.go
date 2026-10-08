package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// Pins for the ISimpleCount shortcut (R13-L4): `SELECT count(*) FROM t` is
// answered from the b-tree page headers (sqlite3BtreeCount/OP_Count) instead of
// a row scan. These tests drive the engine directly and assert the observable
// contract: identical counts to the row-by-row forms, on multi-page tables,
// after deletes (empty leaves survive), for views, and WITHOUT ROWID tables —
// and that the arg form count(x) still skips NULLs (it must NOT take the
// OP_Count path; sqlite gives SQLITE_FUNC_COUNT only to the 0-arg count).

func countStarDB(t *testing.T, schema string, seed int) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExecCount(t, db, schema)
	var sb strings.Builder
	for i := 0; i < seed; i++ {
		var v string
		if i%3 == 0 {
			v = "NULL"
		} else {
			v = fmt.Sprintf("%d", i)
		}
		fmt.Fprintf(&sb, "INSERT INTO t VALUES(%d,%s);\n", i, v)
	}
	mustExecCount(t, db, sb.String())
	return db
}

func mustExecCount(t *testing.T, db *DB, sql string) {
	t.Helper()
	if r := db.Exec(sql); r.Error != nil {
		t.Fatalf("exec %.60q: %v", sql, r.Error)
	}
}

func countOf(t *testing.T, db *DB, sql string) int64 {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("query %.60q: %v", sql, r.Error)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
		t.Fatalf("query %.60q: got %d rows", sql, len(r.Rows))
	}
	n, ok := r.Rows[0][0].(int64)
	if !ok {
		t.Fatalf("query %.60q: value %v (%T), want int64", sql, r.Rows[0][0], r.Rows[0][0])
	}
	return n
}

// TestPinCountStarMatchesRowForms checks the shortcut against the forms that
// still scan, on a multi-page table (page_size 512, 5000 rows) and after
// deletes that leave empty leaves behind.
func TestPinCountStarMatchesRowForms(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	mustExecCount(t, db, "PRAGMA page_size=512")
	mustExecCount(t, db, "CREATE TABLE t(a INTEGER, b)")
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&sb, "INSERT INTO t VALUES(%d,%d);\n", i, i)
	}
	mustExecCount(t, db, sb.String())

	if got := countOf(t, db, "SELECT count(*) FROM t"); got != 5000 {
		t.Fatalf("count(*) = %d, want 5000", got)
	}
	if got := countOf(t, db, "SELECT count(a) FROM t"); got != 5000 {
		t.Fatalf("count(a) = %d, want 5000", got)
	}

	// Scattered deletes: the engine keeps empty leaves in the tree, so the
	// page-header count must not count them as rows.
	mustExecCount(t, db, "DELETE FROM t WHERE a%2=0")
	if got, want := countOf(t, db, "SELECT count(*) FROM t"), countOf(t, db, "SELECT count(a) FROM t"); got != want || got != 2500 {
		t.Fatalf("count(*) after deletes = %d, count(a) = %d, want 2500", got, want)
	}

	// WHERE and grouped shapes must stay on the scanning paths.
	// Oracle (sqlite3 3.54, same script): 2500 rows survive the delete (odd a),
	// of which a>2400 leaves 1300.
	if got := countOf(t, db, "SELECT count(*) FROM t WHERE a>2400"); got != 1300 {
		t.Fatalf("filtered count(*) = %d, want 1300", got)
	}
	mustExecCount(t, db, "DELETE FROM t")
	if got := countOf(t, db, "SELECT count(*) FROM t"); got != 0 {
		t.Fatalf("count(*) after DELETE FROM t = %d, want 0", got)
	}
}

// TestPinCountArgFormSkipsNulls is the guard that count(x) never takes the
// OP_Count path: sqlite's SQLITE_FUNC_COUNT is set on the 0-arg count only, so
// count(x) must ignore NULLs.
func TestPinCountArgFormSkipsNulls(t *testing.T) {
	db := countStarDB(t, "CREATE TABLE t(a,b)", 30)
	for _, q := range []string{
		"SELECT count(b) FROM t",
		"SELECT count(*) FROM t",
	} {
		want := int64(30)
		if strings.Contains(q, "count(b)") {
			want = 20 // 30 rows, every third b is NULL
		}
		if got := countOf(t, db, q); got != want {
			t.Fatalf("%s = %d, want %d", q, got, want)
		}
	}
	if got := countOf(t, db, "SELECT count(DISTINCT b) FROM t"); got != 20 {
		t.Fatalf("count(DISTINCT b) = %d, want 20", got)
	}
}

// TestPinCountStarViewsAndWithoutRowid covers the FROM shapes the shortcut
// must not get wrong: a view over the table, a view with its own WHERE, and a
// WITHOUT ROWID table (whose rows live in an index b-tree).
func TestPinCountStarViewsAndWithoutRowid(t *testing.T) {
	db := countStarDB(t, "CREATE TABLE t(a,b)", 40)
	mustExecCount(t, db, "CREATE VIEW v AS SELECT * FROM t")
	mustExecCount(t, db, "CREATE VIEW vw AS SELECT * FROM t WHERE a>9")
	if got := countOf(t, db, "SELECT count(*) FROM v"); got != 40 {
		t.Fatalf("count(*) over view = %d, want 40", got)
	}
	if got := countOf(t, db, "SELECT count(*) FROM vw"); got != 30 {
		t.Fatalf("count(*) over filtered view = %d, want 30", got)
	}

	mustExecCount(t, db, "CREATE TABLE wr(k TEXT PRIMARY KEY, n INTEGER) WITHOUT ROWID")
	var sb strings.Builder
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&sb, "INSERT INTO wr VALUES('k%04d',%d);\n", i, i)
	}
	mustExecCount(t, db, sb.String())
	if got := countOf(t, db, "SELECT count(*) FROM wr"); got != 1200 {
		t.Fatalf("count(*) on WITHOUT ROWID = %d, want 1200", got)
	}
	mustExecCount(t, db, "DELETE FROM wr WHERE n%2=0")
	if got, want := countOf(t, db, "SELECT count(*) FROM wr"), countOf(t, db, "SELECT count(k) FROM wr"); got != want || got != 600 {
		t.Fatalf("count(*) on WITHOUT ROWID after delete = %d (count(k)=%d), want 600", got, want)
	}
}

// TestPinCountStarColumnName pins the reported column name so the shortcut
// cannot drift from the scanning aggregate path's naming.
func TestPinCountStarColumnName(t *testing.T) {
	db := countStarDB(t, "CREATE TABLE t(a,b)", 3)
	r := db.Query("SELECT count(*) FROM t")
	if r.Error != nil {
		t.Fatalf("query: %v", r.Error)
	}
	if len(r.Columns) != 1 || r.Columns[0] != "count(*)" {
		t.Fatalf("columns = %v, want [count(*)]", r.Columns)
	}
	if r := db.Query("SELECT count(*) AS n, count(*) FROM t"); r.Error != nil {
		t.Fatalf("two-aggregate query must still work: %v", r.Error)
	}
}
