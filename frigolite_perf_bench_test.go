package frigolite_test

// P9.PERF.T1 micro-benchmarks mirroring the speed1/speed2 workload shapes
// (test/speed1.test, test/speed2.test in the SQLite TCL suite). Each
// benchmark opens a file-backed database with the same pragmas the speed
// tests use (page_size=1024, cache_size=8192, locking_mode=EXCLUSIVE) and
// reproduces one speed_trial phase at 50k rows.
//
// Run:
//
//	go test -run '^$' -bench BenchmarkPerf -benchtime 3x -count 5 .

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// perfBenchRows is the row count each benchmark fixture loads (speed1 uses
// 50,000 for t1/t2).
const perfBenchRows = 50000

// perfBenchOpen opens a file-backed database in dir with the speed-test
// pragmas applied and the t1/t2 tables plus i2a/i2b indexes created.
func perfBenchOpen(b *testing.B, dir string) *frigolite.DB {
	b.Helper()
	db, err := frigolite.Open(dir + "/test.db")
	if err != nil {
		b.Fatal(err)
	}
	r := db.Exec("PRAGMA page_size=1024;\n" +
		"PRAGMA cache_size=8192;\n" +
		"PRAGMA locking_mode=EXCLUSIVE;\n" +
		"CREATE TABLE t1(a INTEGER, b INTEGER, c TEXT);\n" +
		"CREATE TABLE t2(a INTEGER, b INTEGER, c TEXT);\n" +
		"CREATE INDEX i2a ON t2(a);\n" +
		"CREATE INDEX i2b ON t2(b);\n")
	if r.Error != nil {
		b.Fatal(r.Error)
	}
	return db
}

// perfBenchExec runs sql inside BEGIN/COMMIT like the speed trials do.
func perfBenchExec(b *testing.B, db *frigolite.DB, sql string) {
	b.Helper()
	db.Exec("BEGIN")
	if r := db.Exec(sql); r.Error != nil {
		b.Fatal(r.Error)
	}
	db.Exec("COMMIT")
}

// perfBenchLoad inserts n rows into table t (a INSERT statements in one
// transaction, speed1-insert1 shape).
func perfBenchLoad(b *testing.B, db *frigolite.DB, t string, n int) {
	b.Helper()
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "INSERT INTO %s VALUES(%d,0,'number_name %d');\n", t, i, i)
	}
	perfBenchExec(b, db, sb.String())
}

// perfBenchFixture loads perfBenchRows rows into t1 (and clears it first via
// DELETE when reuse demands it).
func perfBenchFixture(b *testing.B, db *frigolite.DB) {
	b.Helper()
	if n := db.Query("SELECT count(*) FROM t1"); n.Error != nil {
		b.Fatal(n.Error)
	} else if v, _ := n.Rows[0][0].(int64); v != 0 {
		return // already loaded
	}
	perfBenchLoad(b, db, "t1", perfBenchRows)
}

// BenchmarkPerfInsert1 mirrors speed1-insert1: 50k single-row INSERTs sent
// as one multi-statement string inside one transaction.
func BenchmarkPerfInsert1(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	var sb strings.Builder
	sb.Grow(perfBenchRows * 48)
	for i := 1; i <= perfBenchRows; i++ {
		fmt.Fprintf(&sb, "INSERT INTO t1 VALUES(%d,0,'number_name %d');\n", i, i)
	}
	sql := sb.String()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if r := db.Exec("DELETE FROM t1"); r.Error != nil {
			b.Fatal(r.Error)
		}
		b.StartTimer()
		perfBenchExec(b, db, sql)
	}
}

// BenchmarkPerfSelect1 mirrors speed1-select1: 50 range-aggregate scans
// (count(*), avg(b)) over t1.b, 100-row windows.
func BenchmarkPerfSelect1(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchFixture(b, db)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		lwr := 0
		b.StartTimer()
		for k := 0; k < 50; k++ {
			r := db.Exec(fmt.Sprintf(
				"SELECT count(*), avg(b) FROM t1 WHERE b>=%d AND b<%d;", lwr, (k+10)*100))
			if r.Error != nil {
				b.Fatal(r.Error)
			}
			lwr = k * 100
		}
	}
}

// BenchmarkPerfSelectRange5k mirrors speed1's 5000-query indexed-range loop
// (100-row windows over b after i1b exists).
func BenchmarkPerfSelectRange5k(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchFixture(b, db)
	perfBenchExec(b, db, "CREATE INDEX i1b ON t1(b);")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for k := 0; k < 500; k++ {
			lwr := k * 100
			r := db.Exec(fmt.Sprintf(
				"SELECT count(*), avg(b) FROM t1 WHERE b>=%d AND b<%d;", lwr, lwr+100))
			if r.Error != nil {
				b.Fatal(r.Error)
			}
		}
	}
}

// BenchmarkPerfCreateIdx mirrors speed1-createidx: three indexes over 50k
// rows in one transaction.
func BenchmarkPerfCreateIdx(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchFixture(b, db)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db.Exec("DROP INDEX IF EXISTS i1a; DROP INDEX IF EXISTS i1b; DROP INDEX IF EXISTS i1c;")
		b.StartTimer()
		perfBenchExec(b, db, "CREATE INDEX i1a ON t1(a);\nCREATE INDEX i1b ON t1(b);\nCREATE INDEX i1c ON t1(c);")
	}
}

// BenchmarkPerfUpdate2 mirrors speed1-update2: point UPDATEs by a=
// (i1a-indexed, as speed1 creates i1a before this phase) in one
// transaction. The engine still plans UPDATE ... WHERE as a full-table
// SCAN (see P9.PERF next-tranche list), so the table is scaled to 5k rows
// to keep the current O(updates x rows) scan cost measurable; the
// benchmark doubles as the before/after measure for the future seek fix.
func BenchmarkPerfUpdate2(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchLoad(b, db, "t1", 5000)
	perfBenchExec(b, db, "CREATE INDEX i1a ON t1(a);")
	var sb strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&sb, "UPDATE t1 SET b=0 WHERE a=%d;", i)
	}
	sql := sb.String()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		perfBenchExec(b, db, sql)
	}
}

// BenchmarkPerfUpdate3 mirrors speed1-update3: one full-table UPDATE
// (SET c=a) over 50k rows.
func BenchmarkPerfUpdate3(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchFixture(b, db)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		perfBenchExec(b, db, "UPDATE t1 SET c=a;")
	}
}

// BenchmarkPerfDelete1 mirrors speed1-delete1: DELETE FROM t1 with 50k rows.
func BenchmarkPerfDelete1(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchFixture(b, db)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		perfBenchLoad(b, db, "t1", perfBenchRows) // refill between iterations
		b.StartTimer()
		perfBenchExec(b, db, "DELETE FROM t1")
	}
}

// BenchmarkPerfCopy1 mirrors speed1-copy1: INSERT INTO t1 SELECT * FROM t2.
func BenchmarkPerfCopy1(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchLoad(b, db, "t2", perfBenchRows)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if r := db.Exec("DELETE FROM t1"); r.Error != nil {
			b.Fatal(r.Error)
		}
		b.StartTimer()
		perfBenchExec(b, db, "INSERT INTO t1 SELECT * FROM t2")
	}
}

// BenchmarkPerfRandom1 mirrors speed1-random1: ORDER BY random() over 50k
// rows.
func BenchmarkPerfRandom1(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchFixture(b, db)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := db.Exec("SELECT rowid FROM t1 ORDER BY random() LIMIT 20000;")
		if r.Error != nil {
			b.Fatal(r.Error)
		}
	}
}

// BenchmarkPerfLookupByRowid drives 10k point lookups by rowid (the
// speed2/update shape's read side).
func BenchmarkPerfLookupByRowid(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchFixture(b, db)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for k := 1; k <= 1000; k++ {
			r := db.Query("SELECT a, b, c FROM t1 WHERE rowid=" + strconv.Itoa(k))
			if r.Error != nil {
				b.Fatal(r.Error)
			}
		}
	}
}

// BenchmarkPerfIndexSeekUpdate drives indexed point-lookup UPDATEs through
// the DML seek path (planDMLSeek → index candidate collection): the
// P9.PERF.T3 before/after measure for replacing the value-scan byte
// prefilter with the btree KeyInfo record-compare seek. 500 statements x
// 20k-row index visits per iteration make the O(index) candidate-scan cost
// the dominant term.
func BenchmarkPerfIndexSeekUpdate(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchLoad(b, db, "t2", 20000) // t2 carries index i2a on (a)
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&sb, "UPDATE t2 SET b=1 WHERE a=%d;\n", i)
	}
	sql := sb.String()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		perfBenchExec(b, db, sql)
	}
}

// BenchmarkPerfIndexSeekMiss is the pure candidate-collection measure: the
// same point-lookup shape as BenchmarkPerfIndexSeekUpdate with probes that
// match no row, so every iteration is exactly the index candidate scan.
func BenchmarkPerfIndexSeekMiss(b *testing.B) {
	dir := b.TempDir()
	db := perfBenchOpen(b, dir)
	defer db.Close()
	perfBenchLoad(b, db, "t2", 20000)
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&sb, "UPDATE t2 SET b=1 WHERE a=%d;\n", -i)
	}
	sql := sb.String()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		perfBenchExec(b, db, sql)
	}
}
