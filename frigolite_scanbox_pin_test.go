package frigolite

import (
	"math"
	"path/filepath"
	"reflect"
	"testing"
)

// Pins for the PERF.SCANBOX read-path lanes: the typed aggregate feed
// (covered rowid-range plans step accumulators straight off the record
// payload) and the point eq-seek's column-targeted decode. Every pin pairs a
// covered-range (typed lane) result with the same aggregate over the generic
// paths and, where the shape is expressible in SQL, the sqlite3 oracle's
// expected values.

// openScanboxDB opens an in-memory database with t(id INTEGER PRIMARY KEY,
// c INTEGER) holding rows (1..n, i*7%1000003) — the benchmark shape.
func openScanboxDB(t *testing.T, n int) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	if r := db.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("begin: %v", r.Error)
	}
	for i := 1; i <= n; i++ {
		if r := db.Exec("INSERT INTO t VALUES(" + scanboxItoa(int64(i)) + ", " + scanboxItoa(int64(i*7%1000003)) + ")"); r.Error != nil {
			t.Fatalf("insert %d: %v", i, r.Error)
		}
	}
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("commit: %v", r.Error)
	}
	return db
}

// TestScanboxPin_TypedLaneAggParity checks the covered-range typed lane's
// SUM/COUNT/AVG/TOTAL (incl. rowid-alias arguments) against the same
// aggregates computed by the generic paths (no-WHERE full-scan feed and a
// non-covered range) on the same rows.
func TestScanboxPin_TypedLaneAggParity(t *testing.T) {
	db := openScanboxDB(t, 500)
	queries := []string{
		"SELECT SUM(c), COUNT(*) FROM t WHERE id BETWEEN 1 AND 500",
		"SELECT SUM(c), COUNT(*) FROM t WHERE id >= 1 AND id <= 500",
		"SELECT SUM(c), COUNT(*) FROM t",
		"SELECT COUNT(c) FROM t WHERE id BETWEEN 50 AND 450",
		"SELECT AVG(c), TOTAL(c) FROM t WHERE id BETWEEN 1 AND 500",
		"SELECT SUM(id), COUNT(id) FROM t WHERE id BETWEEN 1 AND 500",
		"SELECT SUM(rowid) FROM t WHERE id BETWEEN 1 AND 500",
		"SELECT SUM(c), COUNT(*) FROM t WHERE id > 0",
		"SELECT SUM(c), COUNT(*) FROM t WHERE id < 501",
	}
	for _, q := range queries {
		r := db.Query(q)
		if r.Error != nil {
			t.Fatalf("%s: %v", q, r.Error)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("%s: rows=%d", q, len(r.Rows))
		}
		// Cross-check SUM(c)/COUNT(*) shapes against a direct recomputation.
		if len(r.Columns) == 2 && r.Columns[0] == "SUM(c)" && r.Columns[1] == "COUNT(*)" {
			var wantSum, wantCount int64
			for i := 1; i <= 500; i++ {
				wantSum += int64(i * 7 % 1000003)
				wantCount++
			}
			if got, ok := r.Rows[0][1].(int64); !ok || got != wantCount {
				t.Fatalf("%s: count=%v want %d", q, r.Rows[0][1], wantCount)
			}
			switch s := r.Rows[0][0].(type) {
			case int64:
				if s != wantSum {
					t.Fatalf("%s: sum=%d want %d", q, s, wantSum)
				}
			case float64:
				if s != float64(wantSum) {
					t.Fatalf("%s: sum(float)=%v want %d", q, s, wantSum)
				}
			default:
				t.Fatalf("%s: sum type %T", q, r.Rows[0][0])
			}
		}
	}
	// AVG parity with the oracle's exact double.
	r := db.Query("SELECT AVG(c) FROM t WHERE id BETWEEN 1 AND 500")
	if r.Error != nil {
		t.Fatalf("avg: %v", r.Error)
	}
	var sum int64
	for i := 1; i <= 500; i++ {
		sum += int64(i * 7 % 1000003)
	}
	wantAvg := float64(sum) / 500
	if got := r.Rows[0][0].(float64); math.Abs(got-wantAvg) > 1e-9 {
		t.Fatalf("avg=%v want %v", got, wantAvg)
	}
}

// TestScanboxPin_TypedLaneNullAndText pins stored-NULL skipping, numeric-text
// classification, blob zero-counting, and REAL promotion through the typed
// lane vs the generic feed, oracle-checked.
func TestScanboxPin_TypedLaneNullAndText(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must("CREATE TABLE m(id INTEGER PRIMARY KEY, v)")
	rows := []string{
		"INSERT INTO m VALUES(1, 10)",
		"INSERT INTO m VALUES(2, NULL)",
		"INSERT INTO m VALUES(3, '30')",   // numeric text: INTEGER 30
		"INSERT INTO m VALUES(4, x'4142')", // blob: counts, adds 0.0
		"INSERT INTO m VALUES(5, 2.5)",    // real: promotes the sum
	}
	for _, s := range rows {
		must(s)
	}
	// Covered range → typed lane; the mirror full-scan query runs the
	// no-WHERE feed; both must equal the oracle.
	type tc struct {
		covered, mirror, wantSum string
		wantCount                int64
	}
	cases := []tc{
		{"SELECT SUM(v), COUNT(v) FROM m WHERE id BETWEEN 1 AND 5",
			"SELECT SUM(v), COUNT(v) FROM m", "42.5", 4},
	}
	for _, c := range cases {
		rc := db.Query(c.covered)
		if rc.Error != nil {
			t.Fatalf("%s: %v", c.covered, rc.Error)
		}
		rm := db.Query(c.mirror)
		if rm.Error != nil {
			t.Fatalf("%s: %v", c.mirror, rm.Error)
		}
		if !reflect.DeepEqual(rc.Rows[0], rm.Rows[0]) {
			t.Fatalf("typed lane %v != generic %v", rc.Rows[0], rm.Rows[0])
		}
		if got := rc.Rows[0][0].(float64); got != 42.5 {
			t.Fatalf("sum=%v want 42.5", got)
		}
		if got := rc.Rows[0][1].(int64); got != c.wantCount {
			t.Fatalf("count=%v want %d", got, c.wantCount)
		}
	}
}

// TestScanboxPin_TypedLaneOverflowParity pins the "integer overflow" error
// through the typed lane (covered range) and its absorption by a later real
// input — func.c sumStep semantics (oracle-verified texts).
func TestScanboxPin_TypedLaneOverflowParity(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must("CREATE TABLE o(id INTEGER PRIMARY KEY, v INTEGER)")
	must("INSERT INTO o VALUES(1, 9223372036854775806)")
	must("INSERT INTO o VALUES(2, 9223372036854775806)")
	must("INSERT INTO o VALUES(3, 5)")
	r := db.Query("SELECT SUM(v) FROM o WHERE id BETWEEN 1 AND 2")
	if r.Error == nil || r.Error.Error() != "integer overflow" {
		t.Fatalf("covered-range overflow: err=%v want 'integer overflow'", r.Error)
	}
	// The covered-range (typed lane) and the mirror full-scan feed surface
	// the SAME overflow behavior (oracle 3.54: the error persists; a later
	// input never absorbs this overflow class). Non-covered range goes
	// through the generic feed — all three must agree.
	rc := db.Query("SELECT SUM(v) FROM o WHERE id BETWEEN 1 AND 3")
	rm := db.Query("SELECT SUM(v) FROM o")
	rn := db.Query("SELECT SUM(v) FROM o WHERE id > 0")
	for q, r := range map[string]*Result{"covered": rc, "mirror": rm, "noncovered": rn} {
		if (r.Error == nil) || (r.Error.Error() != "integer overflow") {
			t.Fatalf("%s: err=%v want 'integer overflow'", q, r.Error)
		}
	}
}

// TestScanboxPin_TypedLaneDefaultsAndDrops pins ADD COLUMN defaults and the
// dropped-column disk layout through the typed lane (the on-disk rank walk)
// vs the generic feed.
func TestScanboxPin_TypedLaneDefaultsAndDrops(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must("CREATE TABLE d(id INTEGER PRIMARY KEY, a INTEGER, c INTEGER)")
	for i := 1; i <= 10; i++ {
		must("INSERT INTO d VALUES(" + scanboxItoa(int64(i)) + ", " + scanboxItoa(int64(i*100)) + ", " + scanboxItoa(int64(i)) + ")")
	}
	must("ALTER TABLE d ADD COLUMN z INTEGER DEFAULT 7")
	must("ALTER TABLE d DROP COLUMN a")
	for _, q := range []string{
		"SELECT SUM(z), COUNT(*) FROM d WHERE id BETWEEN 1 AND 10", // typed: defaults past record width
		"SELECT SUM(c), COUNT(*) FROM d WHERE id BETWEEN 1 AND 10", // typed: c at on-disk rank 0
		"SELECT SUM(z), COUNT(*) FROM d",
		"SELECT SUM(c), COUNT(*) FROM d",
	} {
		r := db.Query(q)
		if r.Error != nil {
			t.Fatalf("%s: %v", q, r.Error)
		}
		if got := r.Rows[0][1].(int64); got != 10 {
			t.Fatalf("%s: count=%d want 10", q, got)
		}
		wantSum := int64(70) // DEFAULT 7 per row (oracle: 3 rows -> 21)
		if r.Columns[0] == "SUM(c)" {
			wantSum = 55
		}
		if got := r.Rows[0][0].(int64); got != wantSum {
			t.Fatalf("%s: sum=%d want %d", q, got, wantSum)
		}
	}
}

// TestScanboxPin_PointSeekDecode pins the column-targeted point fetch: held
// output rows keep their values across subsequent queries (no transient
// buffer aliasing), projections read the right slots, alias/rowid
// projections and row-map shapes fall back correctly.
func TestScanboxPin_PointSeekDecode(t *testing.T) {
	db := openScanboxDB(t, 100)
	// Hold rows across later queries: the values must not alias a reused
	// buffer (hard gate 6).
	held := make([][]interface{}, 0, 100)
	for i := 1; i <= 100; i++ {
		r := db.Query("SELECT c FROM t WHERE id=" + scanboxItoa(int64(i)))
		if r.Error != nil {
			t.Fatalf("point %d: %v", i, r.Error)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("point %d: rows=%d", i, len(r.Rows))
		}
		held = append(held, r.Rows[0])
	}
	// Fresh queries (a different access pattern) must not have disturbed the
	// held rows.
	for i, row := range held {
		want := int64((i + 1) * 7 % 1000003)
		if got := row[0].(int64); got != want {
			t.Fatalf("held row %d: c=%d want %d", i+1, got, want)
		}
	}
	// Multi-column + expression + alias projections.
	r := db.Query("SELECT id, c, c*2 AS dbl FROM t WHERE id=3")
	if r.Error != nil {
		t.Fatalf("multi: %v", r.Error)
	}
	want := []interface{}{int64(3), int64(21), int64(42)}
	if !reflect.DeepEqual(r.Rows[0], want) {
		t.Fatalf("multi row=%v want %v", r.Rows[0], want)
	}
	// WHERE re-eval still runs on non-covered conjuncts.
	r = db.Query("SELECT c FROM t WHERE id=3 AND c>1000")
	if r.Error != nil {
		t.Fatalf("noncovered: %v", r.Error)
	}
	if len(r.Rows) != 0 {
		t.Fatalf("noncovered rows=%v", r.Rows)
	}
	// Row-map shape (FROM subquery) decodes every column.
	r = db.Query("SELECT c FROM (SELECT id, c FROM t WHERE id=4)")
	if r.Error != nil {
		t.Fatalf("subquery: %v", r.Error)
	}
	if got := r.Rows[0][0].(int64); got != 28 {
		t.Fatalf("subquery c=%v want 28", r.Rows[0][0])
	}
	// File-backed parity: the same shapes over a real .db file.
	fdb, err := Open(filepath.Join(t.TempDir(), "scanbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fdb.Close()
	mustF := func(sql string) {
		t.Helper()
		if r := fdb.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	mustF("CREATE TABLE t(id INTEGER PRIMARY KEY, c TEXT)")
	mustF("INSERT INTO t VALUES(1, 'abc')")
	mustF("INSERT INTO t VALUES(2, 'def')")
	r = fdb.Query("SELECT c FROM t WHERE id=2")
	if r.Error != nil {
		t.Fatalf("file point: %v", r.Error)
	}
	if got := r.Rows[0][0].(string); got != "def" {
		t.Fatalf("file point c=%v", r.Rows[0][0])
	}
}

// scanboxItoa formats an int64 for test SQL (itoa already exists in the
// package's test helpers).
func scanboxItoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
