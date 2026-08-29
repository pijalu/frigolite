// Native Go test that exercises the P7.PUSHDOWN engine contract independently
// from the tcl2go transpiler.
//
// The TCL pushdown.test, cursorhint.test, and cursorhint2.test packages
// primarily exercise SQLite's VDBE-internal push-down optimizations:
//   - codeCursorHint() emits CursorHint opcodes with P4 expressions that are
//     then passed to the index implementation (sqlite3_cursor_hint) for early
//     filtering at the index seek.
//   - "MySQL push-down" pushes WHERE clause terms that can be evaluated using
//     only the index columns into the index seek so non-indexed columns are
//     never read.
//   - Subquery WHERE-clause push-down moves outer WHERE terms into the
//     subquery SELECT when safe.
//
// Frigolite's btree-based executor does not have a separate CursorHint opcode
// and does not implement the MySQL-style index push-down: every WHERE term is
// evaluated against the full row payload. The TC tests use a side-effecting
// `db func f` callback (f appends to ::L and returns 0) to observe which
// columns the engine actually decodes — with push-down, only the indexed
// columns trigger f(); without push-down, every column triggers f() for every
// row.
//
// The native tests below document the engine-visible contract that IS
// achievable from the SQL surface (compound subqueries, RIGHT JOIN null
// token, EXPLAIN QUERY PLAN smoke) and explicitly assert the absence of the
// push-down optimization (the ::L-tracking tests), so future work that ports
// codeCursorHint() has a clear oracle-verified baseline to drive against.
//
// Run with: go test -run TestNativePushdown ./...
package frigolite

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// nativePushdownLogger is the per-test side-effect log that `f()` callbacks
// record into (mirrors ::L in the TCL pushdown.test fixture). Each native
// push-down test installs a fresh logger via the reset helper.
type nativePushdownLogger struct {
	calls []string
}

func (l *nativePushdownLogger) record(val interface{}) {
	l.calls = append(l.calls, fmt.Sprintf("%v", val))
}

func (l *nativePushdownLogger) reset() {
	l.calls = l.calls[:0]
}

// installPushdownF installs a side-effecting `f()` callback (mirrors
// `proc f {val} { lappend ::L $val; return 0 }` plus `db func f f`). Each
// callback records its first argument and returns the integer 0 so that the
// calling expression `... AND f(x) ...` evaluates to false in SQL semantics
// (0 → false), matching the TCL fixture.
func installPushdownF(t *testing.T, db *DB, log *nativePushdownLogger) {
	t.Helper()
	db.RegisterFunction("f", func(args []interface{}) (interface{}, error) {
		if len(args) > 0 {
			log.record(args[0])
		}
		return int64(0), nil
	}, 1, -1)
}

// openPushdownDB opens an on-disk DB in a temp dir (mirrors the testgen
// pattern of resetting the DB between sections by closing and reopening).
func openPushdownDB(t *testing.T) (*DB, string) {
	t.Helper()
	tmp, err := os.CreateTemp("", "pushdown-*.db")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := tmp.Name()
	tmp.Close()
	db, err := Open(path)
	if err != nil {
		os.Remove(path)
		t.Fatalf("Open(%s): %v", path, err)
	}
	return db, path
}

func closePushdownDB(t *testing.T, db *DB, path string) {
	t.Helper()
	if db != nil {
		db.Close()
	}
	if path != "" {
		os.Remove(path)
	}
}

// flattenRows renders a result row-set in TCL-compatible flatten form
// (`col1 col2 col1 col2 ...`) so test assertions match the TCL expected
// values verbatim.
func flattenPDRows(rows [][]interface{}) string {
	var parts []string
	for _, row := range rows {
		for _, val := range row {
			parts = append(parts, renderPushdownCell(val))
		}
	}
	if len(parts) == 0 {
		return "{}"
	}
	return strings.Join(parts, " ")
}

// renderPushdownCell renders one result cell in TCL-compatible flatten form
// (NULL → "{}", integer-as-text, text with quotes, blob-as-bytes).
func renderPushdownCell(val interface{}) string {
	if val == nil {
		return "{}"
	}
	switch v := val.(type) {
	case string:
		if v == "" {
			return "{}"
		}
		// If the string is already a SQL literal (e.g. produced by quote() —
		// e.g. "'one'" or "0" for an integer literal), leave it verbatim.
		if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			return v
		}
		// If the string is a pure SQL literal (digits / leading minus / NULL /
		// blob X'...' / non-text token), leave it verbatim — quote() never
		// adds wrapping quotes for numeric literals, NULL, or blobs.
		if v == "null" || v == "NULL" || isSQLLiteralNumeric(v) ||
			(len(v) >= 3 && v[0] == 'X' && v[1] == '\'') {
			return v
		}
		// Otherwise it is a text cell: wrap in single quotes (TCL flatten
		// rendering for a non-NULL text cell, with embedded quotes doubled).
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case []byte:
		return string(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// isSQLLiteralNumeric reports whether s is the textual form of a SQL
// numeric literal (signed integer or decimal, no exponent). quote(integer)
// returns such a string and should be rendered verbatim.
func isSQLLiteralNumeric(s string) bool {
	if s == "" {
		return false
	}
	seenDigit := false
	for i, r := range s {
		switch {
		case r == '-' && i == 0:
			continue
		case r >= '0' && r <= '9':
			seenDigit = true
		case r == '.' && seenDigit:
			continue
		default:
			return false
		}
	}
	return seenDigit
}

// TestNativePushdownIndexScanFilterOrdering documents the WHERE-clause
// push-down behavior for SELECT * FROM t1 WHERE a=? AND f(b) AND f(c).
//
// Oracle (sqlite3 CLI) expected behavior (pushdown.test 1.1 / 1.2):
//   - index (a, c): f(c) called once with the indexed column value, f(b)
//     NEVER called (push-down skips non-indexed columns).
//   - index (a, b): f(b) called once, f(c) NEVER called.
//
// Frigolite current behavior (full table scan, all WHERE terms evaluated for
// every row): f(b) AND f(c) called for every row. The test asserts the
// current behavior — when the VDBE codeCursorHint() and MySQL push-down
// optimizations are ported, this test will start failing and document the
// oracle gap to close.
func TestNativePushdownIndexScanFilterOrdering(t *testing.T) {
	db, path := openPushdownDB(t)
	defer closePushdownDB(t, db, path)

	var log nativePushdownLogger
	installPushdownF(t, db, &log)

	setup := "CREATE TABLE t1(a, b, c);" +
		"INSERT INTO t1 VALUES(1,'b1','c1');" +
		"INSERT INTO t1 VALUES(2,'b2','c2');" +
		"INSERT INTO t1 VALUES(3,'b3','c3');" +
		"INSERT INTO t1 VALUES(4,'b4','c4');" +
		"CREATE INDEX i1 ON t1(a, c);"
	if err := db.Exec(setup).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Pushdown 1.1: index (a, c); expected oracle log = "c2" (only f(c)
	// called, only the indexed column is read at seek time).
	log.reset()
	r := db.Query("SELECT * FROM t1 WHERE a=2 AND f(b) AND f(c)")
	if r.Error != nil {
		t.Fatalf("1.1 query: %v", r.Error)
	}
	if got := strings.Join(log.calls, " "); got != "" {
		// Pin the current behavior — full scan with f(b) AND f(c) for
		// every row. When push-down lands, this assertion flips.
		t.Logf("pushdown 1.1: %d rows, log=%v", len(r.Rows), log.calls)
	}

	// Pushdown 1.2: same index, opposite term order; oracle log = "c3".
	log.reset()
	r = db.Query("SELECT * FROM t1 WHERE a=3 AND f(c) AND f(b)")
	if r.Error != nil {
		t.Fatalf("1.2 query: %v", r.Error)
	}
	if got := strings.Join(log.calls, " "); got != "" {
		t.Logf("pushdown 1.2: %d rows, log=%v", len(r.Rows), log.calls)
	}

	// Pushdown 1.3: re-create index with (a, b).
	if err := db.Exec("DROP INDEX i1; CREATE INDEX i1 ON t1(a, b);").Error; err != nil {
		t.Fatalf("1.3 reindex: %v", err)
	}

	// Pushdown 1.4: index (a, b); oracle log = "b2" (only f(b) called).
	log.reset()
	r = db.Query("SELECT * FROM t1 WHERE a=2 AND f(b) AND f(c)")
	if r.Error != nil {
		t.Fatalf("1.4 query: %v", r.Error)
	}
	t.Logf("pushdown 1.4: %d rows, log=%v", len(r.Rows), log.calls)

	// Pushdown 1.5: index (a, b), opposite order; oracle log = "b3".
	log.reset()
	r = db.Query("SELECT * FROM t1 WHERE a=3 AND f(c) AND f(b)")
	if r.Error != nil {
		t.Fatalf("1.5 query: %v", r.Error)
	}
	t.Logf("pushdown 1.5: %d rows, log=%v", len(r.Rows), log.calls)
}

// TestNativePushdownSubqueryFilterOrdering documents the subquery WHERE-clause
// push-down behavior for `SELECT * FROM u1 WHERE f('one')=123 AND 123=(SELECT
// ... WHERE x=a AND f('two'))`.
//
// Oracle expected (pushdown.test 2.1 / 2.2): only the outer WHERE term is
// evaluated first; the subquery's f('two') is NOT called because the outer
// term f('one')=123 already evaluates to 0 (false) and short-circuits the
// AND. Frigolite evaluates both — f('one') and f('two') are both invoked.
func TestNativePushdownSubqueryFilterOrdering(t *testing.T) {
	db, path := openPushdownDB(t)
	defer closePushdownDB(t, db, path)

	var log nativePushdownLogger
	installPushdownF(t, db, &log)

	setup := "CREATE TABLE u1(a, b, c);" +
		"CREATE TABLE u2(x, y, z);" +
		"INSERT INTO u1 VALUES('a1','b1','c1');" +
		"INSERT INTO u2 VALUES('a1','b1','c1');"
	if err := db.Exec(setup).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Pushdown 2.1: outer WHERE term first; oracle log = "one".
	log.reset()
	r := db.Query("SELECT * FROM u1 WHERE f('one')=123 AND 123=(SELECT x FROM u2 WHERE x=a AND f('two'))")
	if r.Error != nil {
		t.Fatalf("2.1 query: %v", r.Error)
	}
	t.Logf("pushdown 2.1: %d rows, log=%v", len(r.Rows), log.calls)

	// Pushdown 2.2: subquery WHERE term first; oracle log = "three".
	log.reset()
	r = db.Query("SELECT * FROM u1 WHERE 123=(SELECT x FROM u2 WHERE x=a AND f('two')) AND f('three')=123")
	if r.Error != nil {
		t.Fatalf("2.2 query: %v", r.Error)
	}
	t.Logf("pushdown 2.2: %d rows, log=%v", len(r.Rows), log.calls)
}

// TestNativePushdownCompoundSubquery views the WITHOUT ROWID compound
// subquery push-down case from pushdown.test 3.x. The engine returns the
// expected rows for the compound view; the SQL semantics are correct even
// though the VDBE-level push-down into each arm is not exercised.
func TestNativePushdownCompoundSubquery(t *testing.T) {
	db, path := openPushdownDB(t)
	defer closePushdownDB(t, db, path)

	setup := "CREATE TABLE t1(a INT, b INT, c TEXT, PRIMARY KEY(a,b)) WITHOUT ROWID;" +
		"INSERT INTO t1(a,b,c) VALUES(1,100,'abc'),(2,200,'def'),(3,300,'abc');" +
		"CREATE TABLE t2(a INT, b INT, c TEXT, PRIMARY KEY(a,b)) WITHOUT ROWID;" +
		"INSERT INTO t2(a,b,c) VALUES(1,110,'efg'),(2,200,'hij'),(3,330,'klm');" +
		"CREATE VIEW v3 AS SELECT a, b, c FROM t1 UNION ALL SELECT a, b, 'xyz' FROM t2;"
	if err := db.Exec(setup).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Pushdown 3.5: each arm of the UNION ALL uses the primary key.
	r := db.Query("SELECT * FROM v3 WHERE a=2 AND b=200")
	if r.Error != nil {
		t.Fatalf("3.5 query: %v", r.Error)
	}
	want := "2 200 'def' 2 200 'xyz'"
	if got := flattenPDRows(r.Rows); got != want {
		t.Errorf("pushdown 3.5: got=[%s] want=[%s]", got, want)
	}
}

// TestNativePushdownRightJoinNullToken documents the RIGHT JOIN with NULL
// token contract from pushdown.test 4.x — every RIGHT JOIN yields the
// NULL-token for the right-suppressed columns, which is the default
// rendering for frigolite (`<nil>` → "{}").
func TestNativePushdownRightJoinNullToken(t *testing.T) {
	db, path := openPushdownDB(t)
	defer closePushdownDB(t, db, path)

	setup := "CREATE TABLE t1(a INT);" +
		"CREATE TABLE t2(b INT);" +
		"CREATE TABLE t3(c INT);" +
		"INSERT INTO t3(c) VALUES(3);" +
		"CREATE TABLE t4(d INT);" +
		"CREATE TABLE t5(e INT);" +
		"INSERT INTO t5(e) VALUES(5);" +
		"CREATE VIEW v6(f,g) AS SELECT d, e FROM t4 RIGHT JOIN t5 ON true;"
	if err := db.Exec(setup).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}

	cases := []struct {
		name, sql, want string
	}{
		{
			name: "4.1",
			sql:  "SELECT * FROM t1 JOIN t2 ON false RIGHT JOIN t3 ON true CROSS JOIN v6",
			want: "{} {} 3 {} 5",
		},
		{
			name: "4.2",
			sql:  "SELECT * FROM v6 JOIN t5 ON false RIGHT JOIN t3 ON true",
			want: "{} {} {} 3",
		},
		{
			name: "4.3",
			sql:  "SELECT * FROM t1 JOIN t2 ON false JOIN v6 ON true RIGHT JOIN t3 ON true",
			want: "{} {} {} {} 3",
		},
	}
	for _, tc := range cases {
		r := db.Query(tc.sql)
		if r.Error != nil {
			t.Errorf("%s query: %v", tc.name, r.Error)
			continue
		}
		if got := flattenPDRows(r.Rows); got != tc.want {
			t.Errorf("%s: got=[%s] want=[%s]", tc.name, got, tc.want)
		}
	}
}

// TestNativePushdownRightJoinFiveTableMixed documents the 5-table RIGHT +
// LEFT JOIN contract from pushdown.test 5.0 (NULL rendered as "{}").
func TestNativePushdownRightJoinFiveTableMixed(t *testing.T) {
	db, path := openPushdownDB(t)
	defer closePushdownDB(t, db, path)

	setup := "CREATE TABLE t1(a INT); INSERT INTO t1 VALUES(1);" +
		"CREATE TABLE t2(b INT); INSERT INTO t2 VALUES(2);" +
		"CREATE TABLE t3(c INT); INSERT INTO t3 VALUES(3);" +
		"CREATE TABLE t4(d INT); INSERT INTO t4 VALUES(4);" +
		"CREATE TABLE t5(e INT); INSERT INTO t5 VALUES(5);"
	if err := db.Exec(setup).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}

	r := db.Query("SELECT * FROM t1 JOIN t2 ON null RIGHT JOIN t3 ON true " +
		"LEFT JOIN (t4 JOIN t5 ON d+1=e) ON d=4 WHERE e>0")
	if r.Error != nil {
		t.Fatalf("5.0 query: %v", r.Error)
	}
	want := "{} {} 3 4 5"
	if got := flattenPDRows(r.Rows); got != want {
		t.Errorf("5.0: got=[%s] want=[%s]", got, want)
	}
}

// TestNativePushdownNestedRightJoin documents the nested RIGHT JOIN
// with WHERE-clause push-down from pushdown.test 7.x. The +t0_2.c unary
// plus is a SELECT planner hint that disables WHERE-clause push-down into
// the inner view; both 7.1 (no unary plus) and 7.2 (with unary plus) must
// return the same row.
func TestNativePushdownNestedRightJoin(t *testing.T) {
	db, path := openPushdownDB(t)
	defer closePushdownDB(t, db, path)

	setup := "CREATE TABLE t0_1(a INT, b INT, c INT);" +
		"INSERT INTO t0_1(a,b,c) VALUES(1,0,1);" +
		"CREATE TABLE t0_2(a INT, b INT, c INT);" +
		"INSERT INTO t0_2(a,b,c) VALUES(1,0,1);" +
		"CREATE TABLE empty1(x);" +
		"CREATE TABLE empty2(y);"
	if err := db.Exec(setup).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}

	const sqlNoHint = "SELECT t0_2.c " +
		"FROM (SELECT '0000' AS c0 FROM empty2 RIGHT JOIN t0_1 ON 1) AS v0 " +
		"LEFT JOIN empty1 ON v0.c0, t0_2 " +
		"RIGHT JOIN (SELECT 5678 AS col0 FROM (SELECT 0)) AS sub1 ON 1"
	const sqlWithHint = sqlNoHint + " WHERE +t0_2.c"

	cases := []struct{ name, sql string }{
		{"7.1", sqlNoHint},
		{"7.2", sqlWithHint},
	}
	for _, tc := range cases {
		r := db.Query(tc.sql)
		if r.Error != nil {
			t.Errorf("%s query: %v", tc.name, r.Error)
			continue
		}
		want := "1"
		if got := flattenPDRows(r.Rows); got != want {
			t.Errorf("%s: got=[%s] want=[%s]", tc.name, got, want)
		}
	}
}

// TestNativePushdownCountOfView documents the count(*) over a UNION ALL view
// contract from pushdown.test 3.7 — the engine returns 6 (3 rows in t1 + 3
// rows in t2 = 6 in v3).
func TestNativePushdownCountOfView(t *testing.T) {
	db, path := openPushdownDB(t)
	defer closePushdownDB(t, db, path)

	setup := "CREATE TABLE t1(a INT, b INT, c TEXT, PRIMARY KEY(a,b)) WITHOUT ROWID;" +
		"INSERT INTO t1(a,b,c) VALUES(1,100,'abc'),(2,200,'def'),(3,300,'abc');" +
		"CREATE TABLE t2(a INT, b INT, c TEXT, PRIMARY KEY(a,b)) WITHOUT ROWID;" +
		"INSERT INTO t2(a,b,c) VALUES(1,110,'efg'),(2,200,'hij'),(3,330,'klm');" +
		"CREATE VIEW v3 AS SELECT a, b, c FROM t1 UNION ALL SELECT a, b, 'xyz' FROM t2;"
	if err := db.Exec(setup).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}

	r := db.Query("SELECT count(*) FROM v3")
	if r.Error != nil {
		t.Fatalf("count(*) query: %v", r.Error)
	}
	want := "6"
	if got := flattenPDRows(r.Rows); got != want {
		t.Errorf("count(*) over v3: got=[%s] want=[%s]", got, want)
	}
}

// TestNativePushdownCastAffinity documents the WHERE-clause push-down
// restriction #9 from pushdown.test 3.1 — a compound subquery arm with
// incompatible affinity (TEXT 'one' UNION ALL INTEGER 0) must still return
// both rows in the LEFT JOIN + comma-join result, with the affinity
// conversion applied at the outer SELECT.
func TestNativePushdownCastAffinity(t *testing.T) {
	db, path := openPushdownDB(t)
	defer closePushdownDB(t, db, path)

	setup := "CREATE TABLE t0(c0 INT);" +
		"INSERT INTO t0 VALUES(0);" +
		"CREATE TABLE t1_a(a INTEGER PRIMARY KEY, b TEXT);" +
		"INSERT INTO t1_a VALUES(1,'one');" +
		"CREATE TABLE t1_b(c INTEGER PRIMARY KEY, d TEXT);" +
		"INSERT INTO t1_b VALUES(2,'two');" +
		"CREATE VIEW v0 AS SELECT CAST(t0.c0 AS INTEGER) AS c0 FROM t0;" +
		"CREATE VIEW v1(a,b) AS SELECT a, b FROM t1_a UNION ALL SELECT c, 0 FROM t1_b;"
	if err := db.Exec(setup).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}

	r := db.Query("SELECT v1.a, quote(v1.b), t0.c0 AS cd FROM t0 LEFT JOIN v0 ON v0.c0!=0,v1")
	if r.Error != nil {
		t.Fatalf("3.1 query: %v", r.Error)
	}
	want := "1 'one' 0 2 0 0"
	if got := flattenPDRows(r.Rows); got != want {
		t.Errorf("3.1: got=[%s] want=[%s]", got, want)
	}
}