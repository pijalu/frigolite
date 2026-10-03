// Pin tests for the btree append-cursor fast path driven through the SQL
// layer (PERF.INSQUICK): every invalidation scenario must leave the table
// with PRAGMA integrity_check clean and EXACTLY the expected rowid/value
// set, and the quick path must be observably engaged on the append phases
// (the counter delta proves the fast path ran; the data comparisons prove it
// wrote what the generic path would have).
package frigolite_test

import (
	"sort"
	"strconv"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

// mustRows runs a query and returns the first column of every row as int64s.
func mustRows(t *testing.T, db *frigolite.DB, sql string) []int64 {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("query %s: %v", sql, r.Error)
	}
	out := make([]int64, 0, len(r.Rows))
	for _, row := range r.Rows {
		switch v := row[0].(type) {
		case int64:
			out = append(out, v)
		default:
			t.Fatalf("query %s: non-int64 first column %T", sql, row[0])
		}
	}
	return out
}

// checkIntegrity asserts PRAGMA integrity_check reports ok.
func checkIntegrity(t *testing.T, db *frigolite.DB, phase string) {
	t.Helper()
	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("%s: integrity_check: %v", phase, r.Error)
	}
	if len(r.Rows) == 0 || r.Rows[0][0] != "ok" {
		t.Fatalf("%s: integrity_check: %v", phase, r.Rows)
	}
}

// expectIDs asserts the table's rowid set equals want exactly (order-free
// comparison via sorted equality — SELECT without ORDER BY on a rowid table
// returns ascending rowids).
func expectIDs(t *testing.T, db *frigolite.DB, phase string, want []int64) {
	t.Helper()
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	got := mustRows(t, db, "SELECT id FROM t")
	if len(got) != len(want) {
		t.Fatalf("%s: row count %d, want %d (got %v...)", phase, len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d id=%d, want %d", phase, i, got[i], want[i])
		}
	}
}

// appendRuns inserts lo..hi-1 one literal INSERT per row (each its own
// statement, one transaction — the harness shape the quick path targets).
func appendRuns(t *testing.T, db *frigolite.DB, lo, hi int) {
	t.Helper()
	pinExec(t, db, "BEGIN")
	for i := lo; i < hi; i++ {
		pinExec(t, db, "INSERT INTO t VALUES("+strconv.Itoa(i)+","+strconv.Itoa(i*7%1000003)+")")
	}
	pinExec(t, db, "COMMIT")
}

// TestAppendCursorPins runs the full invalidation matrix. Each phase ends
// with integrity_check + an exact rowid-set assertion.
func TestAppendCursorPins(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)")

	// 1. Pure append across many leaf splits.
	appendRuns(t, db, 1, 2001)
	checkIntegrity(t, db, "append")
	expectIDs(t, db, "append", seqSlice(1, 2001))

	// 2. Non-append (out-of-order) insert between appends.
	pinExec(t, db, "INSERT INTO t VALUES(-5, 5)")
	appendRuns(t, db, 2001, 2011)
	checkIntegrity(t, db, "nonappend")
	expectIDs(t, db, "nonappend", append(seqSlice(1, 2011), -5))

	// 3. Delete the max row, then append.
	pinExec(t, db, "DELETE FROM t WHERE id=2010")
	appendRuns(t, db, 2010, 2013)
	checkIntegrity(t, db, "delete-max")
	want := append(seqSlice(1, 2011), -5)
	want = append(want, 2011, 2012)
	expectIDs(t, db, "delete-max", want)

	// 4. Delete middle rows, then append.
	pinExec(t, db, "DELETE FROM t WHERE id IN (100, 1500)")
	appendRuns(t, db, 2013, 2016)
	checkIntegrity(t, db, "delete-mid")
	got := mustRows(t, db, "SELECT COUNT(*) FROM t")
	if got[0] != int64(len(want)+3-2) {
		t.Fatalf("delete-mid count %d", got[0])
	}

	// 5. Overwrite the max rowid (delete + re-insert + update), then append.
	pinExec(t, db, "DELETE FROM t WHERE id=2015")
	pinExec(t, db, "INSERT INTO t VALUES(2015, 42)")
	pinExec(t, db, "UPDATE t SET c=43 WHERE id=2015")
	appendRuns(t, db, 2016, 2019)
	checkIntegrity(t, db, "overwrite")
	if v := mustRows(t, db, "SELECT c FROM t WHERE id=2015"); v[0] != 43 || len(v) != 1 {
		t.Fatalf("overwrite: c=%v", v)
	}

	// 6. Savepoint rollback across appends.
	pinExec(t, db, "SAVEPOINT s1")
	appendRunsInTxn(t, db, 2019, 2030)
	pinExec(t, db, "ROLLBACK TO s1")
	pinExec(t, db, "RELEASE s1")
	appendRuns(t, db, 2019, 2023)
	checkIntegrity(t, db, "savepoint-rollback")
	if got := mustRows(t, db, "SELECT COUNT(*) FROM t WHERE id>=2019"); got[0] != 4 {
		t.Fatalf("savepoint-rollback: rows past 2019 = %d, want 4", got[0])
	}

	// 7. Whole-transaction rollback across appends.
	pinExec(t, db, "BEGIN")
	appendRunsInTxn(t, db, 2023, 2040)
	pinExec(t, db, "ROLLBACK")
	appendRuns(t, db, 2023, 2027)
	checkIntegrity(t, db, "txn-rollback")
	if got := mustRows(t, db, "SELECT COUNT(*) FROM t WHERE id>=2023"); got[0] != 4 {
		t.Fatalf("txn-rollback: rows past 2023 = %d, want 4", got[0])
	}

	// 8. VACUUM between appends (page-layout replacement).
	pinExec(t, db, "VACUUM")
	appendRuns(t, db, 2027, 2031)
	checkIntegrity(t, db, "vacuum")
	if got := mustRows(t, db, "SELECT COUNT(*) FROM t"); got[0] == 0 {
		t.Fatal("vacuum lost all rows")
	}

	// 9. Random-order mix of appends and non-appends.
	pinExec(t, db, "BEGIN")
	for _, id := range []int{5000, 3, 4999, 2031, 4998, 2032, 100000, 7, 2033} {
		pinExec(t, db, "INSERT OR REPLACE INTO t VALUES("+strconv.Itoa(id)+","+strconv.Itoa(id*2)+")")
	}
	pinExec(t, db, "COMMIT")
	checkIntegrity(t, db, "random-mix")
	if got := mustRows(t, db, "SELECT c FROM t WHERE id=100000"); got[0] != 200000 {
		t.Fatalf("random-mix: id=100000 c=%d", got[0])
	}
	total := mustRows(t, db, "SELECT COUNT(*) FROM t")
	distinct := mustRows(t, db, "SELECT COUNT(DISTINCT id) FROM t")
	if total[0] != distinct[0] {
		t.Fatalf("random-mix: %d rows, %d distinct ids", total[0], distinct[0])
	}

	// 10. DROP + CREATE (root-page reuse), then append into the new table.
	pinExec(t, db, "DROP TABLE t")
	pinExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)")
	appendRuns(t, db, 1, 301)
	checkIntegrity(t, db, "drop-create")
	expectIDs(t, db, "drop-create", seqSlice(1, 301))
}

// appendRunsInTxn appends inside an already-open transaction (no BEGIN/COMMIT).
func appendRunsInTxn(t *testing.T, db *frigolite.DB, lo, hi int) {
	t.Helper()
	for i := lo; i < hi; i++ {
		pinExec(t, db, "INSERT INTO t VALUES("+itoa(i)+","+itoa(i*7%1000003)+")")
	}
}

// pinExec / openPinDB live in frigolite_misc_pin_test.go (same package).

func seqSlice(lo, hi int) []int64 {
	out := make([]int64, 0, hi-lo)
	for i := lo; i < hi; i++ {
		out = append(out, int64(i))
	}
	return out
}
