package frigolite_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pijalu/frigolite"
)

// buildSwarmFixture creates nSrc SQLite database files, each holding one
// intkey table tN with rowsPerSrc rows (rowids i*rowsPerSrc+1 .. i*rowsPerSrc
// +rowsPerSrc), plus a dir(f, t, imin, imax) catalog and a swarmvtab/unionvtab
// virtual table over it. Sources live in dir; paths recorded in the catalog
// are absolute so the test never depends on the process working directory.
func buildSwarmFixture(t *testing.T, db *frigolite.DB, dir, module string, nSrc, rowsPerSrc int) {
	t.Helper()
	if module == "swarmvtab" {
		mustExec(t, db, "CREATE TABLE dir(f, t, imin, imax);")
	} else {
		mustExec(t, db, "CREATE TABLE dir(db, t, imin, imax);")
	}
	for i := 0; i < nSrc; i++ {
		src := filepath.Join(dir, fmt.Sprintf("src%d.db", i))
		lo := i*rowsPerSrc + 1
		hi := (i + 1) * rowsPerSrc
		mustExec(t, db, fmt.Sprintf("ATTACH '%s' AS aux%d; CREATE TABLE aux%d.t%d (a INTEGER PRIMARY KEY, b TEXT);", src, i, i, i))
		// Same row-generation shape as swarmvtab.test 1.0 (recursive CTE).
		mustExec(t, db, fmt.Sprintf("WITH s(i) AS (SELECT %d UNION ALL SELECT i+1 FROM s WHERE i<%d) INSERT INTO aux%d.t%d SELECT i, hex(randomblob(8)) FROM s;", lo, hi, i, i))
		if module == "swarmvtab" {
			// swarm sources are independent FILES read on demand: detach after
			// loading and record the file path in the catalog (unionvtab.c:
			// zFile in column 0, zDb NULL).
			mustExec(t, db, fmt.Sprintf("DETACH aux%d;", i))
			mustExec(t, db, fmt.Sprintf("INSERT INTO dir VALUES('%s', 't%d', %d, %d);", src, i, lo, hi))
		} else {
			// union sources are (schema, table) pairs in the CURRENT
			// connection: the attach must stay live (unionvtab.c: zDb in
			// column 0).
			mustExec(t, db, fmt.Sprintf("INSERT INTO dir VALUES('aux%d', 't%d', %d, %d);", i, i, lo, hi))
		}
	}
	mustExec(t, db, fmt.Sprintf("CREATE VIRTUAL TABLE temp.s1 USING %s('SELECT * FROM dir');", module))
}


func mustExec(t *testing.T, db *frigolite.DB, q string) {
	t.Helper()
	if r := db.Exec(q); r.Error != nil {
		t.Fatalf("exec error: %v\n  sql: %s", r.Error, q)
	}
}

func mustQueryCount(t *testing.T, db *frigolite.DB, q string, want string) int64 {
	t.Helper()
	r := db.Query(q)
	if r.Error != nil {
		t.Fatalf("query error: %v\n  sql: %s", r.Error, q)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
		t.Fatalf("want single count cell, got %v\n  sql: %s", r.Rows, q)
	}
	got := fmt.Sprintf("%v", r.Rows[0][0])
	if got != want {
		t.Fatalf("count mismatch\n  got:  %s\n  want: %s\n  sql: %s", got, want, q)
	}
	return r.Rows[0][0].(int64)
}

// TestSwarmRowidEquiJoinNative validates the SQLite vtab join contract for
// rowid equi-constraints on swarm/union virtual tables: xBestIndex marks a
// rowid EQ constraint omitted+unique (unionvtab.c:1254-1301) and the planner
// re-runs xFilter with the outer row's value per loop iteration, so a k-way
// rowid equi-join costs O(N) vtab seeks instead of O(N^k) pair evaluations.
func TestSwarmRowidEquiJoinNative(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	buildSwarmFixture(t, db, dir, "swarmvtab", 4, 10)

	// Single-table literal range (source selection via ConsumeRowidRange).
	mustQueryCount(t, db, "SELECT count(*) FROM s1 WHERE rowid<15;", "14")
	// Two-way join with a one-sided literal range on the inner alias.
	mustQueryCount(t, db, "SELECT count(*) FROM s1 a, s1 b WHERE b.rowid<=20;", "800")
	// Three-way rowid equi-join: every rowid matches exactly once per alias.
	mustQueryCount(t, db, "SELECT count(*) FROM s1 a, s1 b, s1 c WHERE a.rowid=b.rowid AND b.rowid=c.rowid;", "40")
	// Equi-constraint that can never match: inner side stays empty.
	mustQueryCount(t, db, "SELECT count(*) FROM s1 a, s1 b WHERE b.rowid = a.a + 1000;", "0")
	// LEFT JOIN with a non-matching equi-constraint: NULL-padded right side,
	// left row count preserved.
	mustQueryCount(t, db, "SELECT count(*) FROM s1 a LEFT JOIN s1 b ON b.rowid = a.rowid + 1000;", "40")
	// LEFT JOIN matching: one right row per left row.
	mustQueryCount(t, db, "SELECT count(*) FROM s1 a LEFT JOIN s1 b ON b.rowid = a.rowid;", "40")
}

// TestRealTableRowidEquiJoinNative guards the joined-row-map rowid keys for
// ordinary tables: every alias must keep its own qualified rowid (a chained
// join used to fabricate b.rowid from the first table's bare rowid key,
// silently dropping rowid equi-conjuncts from the third alias on).
func TestRealTableRowidEquiJoinNative(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t0(a INTEGER PRIMARY KEY, b TEXT);")
	mustExec(t, db, "WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<40) INSERT INTO t0 SELECT i, hex(randomblob(8)) FROM s;")

	mustQueryCount(t, db, "SELECT count(*) FROM t0 a, t0 b WHERE a.rowid=b.rowid;", "40")
	mustQueryCount(t, db, "SELECT count(*) FROM t0 a, t0 b, t0 c WHERE a.rowid=b.rowid AND b.rowid=c.rowid;", "40")
	mustQueryCount(t, db, "SELECT count(*) FROM t0 a, t0 b, t0 c WHERE a.rowid=b.rowid AND b.rowid=c.rowid AND c.rowid=a.rowid;", "40")
	mustQueryCount(t, db, "SELECT count(*) FROM t0 a LEFT JOIN t0 b ON b.rowid = a.rowid + 1000;", "40")
}

// TestUnionRowidEquiJoinNative runs the same contract against the unionvtab
// module (same xBestIndex family in unionvtab.c; sources addressed by
// (schema, table) instead of file).
func TestUnionRowidEquiJoinNative(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	buildSwarmFixture(t, db, dir, "unionvtab", 4, 10)

	mustQueryCount(t, db, "SELECT count(*) FROM s1 WHERE rowid<15;", "14")
	mustQueryCount(t, db, "SELECT count(*) FROM s1 a, s1 b, s1 c WHERE a.rowid=b.rowid AND b.rowid=c.rowid;", "40")
}

// TestSwarmRowidEquiJoinScaling is the performance guard for the suite-scale
// fixture (40 sources x 10 rows = 400 rows, swarmvtab.test 1.5.2). With the
// per-row xFilter contract the join completes in well under a second; the
// nested-loop materialization it replaces needed 15+ minutes. The 60s bound
// leaves generous headroom for loaded CI machines while still failing loudly
// if the planner regresses to O(N^k) pair evaluation (~1.4us/pair).
func TestSwarmRowidEquiJoinScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("timing bound test")
	}
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	buildSwarmFixture(t, db, dir, "swarmvtab", 40, 10)

	start := time.Now()
	mustQueryCount(t, db, "SELECT count(*) FROM s1 a, s1 b, s1 c WHERE a.rowid=b.rowid AND b.rowid=c.rowid;", "400")
	elapsed := time.Since(start)
	if elapsed > 60*time.Second {
		t.Fatalf("3-way rowid equi-join over 400-row swarm took %s (bound 60s): planner is not seeking the vtab per outer row", elapsed)
	}
	t.Logf("3-way rowid equi-join over 400 rows: %s", elapsed)
}