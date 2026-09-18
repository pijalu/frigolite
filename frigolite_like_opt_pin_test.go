package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
	"github.com/pijalu/frigolite/internal/vtab"
)

// likeOptT1 mirrors like.test's t1: twelve TEXT rows whose like/GLOB
// prefix-range visit counts are documented by the like-3.x/4.x/5.x TCL
// assertions (transpiled into testgen/like).
var likeOptT1 = []string{
	"a", "ab", "abc", "abcd", "acd", "abd",
	"bc", "bcd", "xyz", "ABC", "CDE", "ABC abc xyz",
}

func likeOptSetup(t *testing.T, ddl string) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range strings.Split(ddl, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if r := db.Exec(stmt); r.Error != nil {
			t.Fatalf("exec %q: %v", stmt, r.Error)
		}
	}
	return db
}

// likeOptQuery runs sql and asserts both the returned x column and the
// number of like()/glob() invocations (func.c sqlite3_like_count) observed
// for it: rows must be correct AND the range must decide them exactly the
// way SQLite's LIKE optimization does.
func likeOptQuery(t *testing.T, db *frigolite.DB, sql, wantRows string, wantCount int64) {
	t.Helper()
	db.ResetLikeCallCount()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
	got := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		if len(row) == 0 {
			continue
		}
		if s, ok := row[0].(string); ok {
			got = append(got, s)
		} else {
			got = append(got, "?")
		}
	}
	if gotRows := strings.Join(got, "|"); gotRows != wantRows {
		t.Errorf("%s: rows [%s], want [%s]", sql, gotRows, wantRows)
	}
	if gotCount := db.LikeCallCount(); gotCount != wantCount {
		t.Errorf("%s: like calls %d, want %d", sql, gotCount, wantCount)
	}
}

// TestSQLiteLikeRangeSynthesisPin pins like.c's prefix-range synthesis
// (whereexpr.c exprAnalyze: x LIKE 'abc%' gains x>='abc' AND x<'abd') and
// the like()-call elision for complete patterns (like.test 3.3.100.cnt: 0),
// including the partial-optimization counts for patterns whose first
// wildcard is not trailing (like-3.6: 6, like-3.8: 4, like-3.10: 6,
// like-3.12: 12). CSL=on with the BINARY index i1.
func TestSQLiteLikeRangeSynthesisPin(t *testing.T) {
	db := likeOptSetup(t, "CREATE TABLE t1(x TEXT); PRAGMA case_sensitive_like=on; CREATE INDEX i1 ON t1(x)")
	for _, s := range likeOptT1 {
		if r := db.Exec("INSERT INTO t1 VALUES('" + s + "')"); r.Error != nil {
			t.Fatalf("insert %q: %v", s, r.Error)
		}
	}
	cases := []struct {
		sql   string
		rows  string
		count int64
	}{
		{"SELECT x FROM t1 WHERE x LIKE 'abc%'", "abc|abcd", 0},
		{"SELECT x FROM t1 WHERE x LIKE 'a_c'", "abc", 6},
		{"SELECT x FROM t1 WHERE x LIKE 'ab%d'", "abcd|abd", 4},
		{"SELECT x FROM t1 WHERE x LIKE 'a_c%'", "abc|abcd", 6},
		{"SELECT x FROM t1 WHERE x LIKE '%bcd'", "abcd|bcd", 12},
		{"SELECT x FROM t1 WHERE x LIKE 'a'", "a", 6},
		{"SELECT x FROM t1 WHERE x LIKE 'abcd'", "abcd", 1},
		{"SELECT x FROM t1 WHERE x LIKE 'abcde'", "", 0},
		{"SELECT x FROM t1 WHERE x LIKE 'a_c' ORDER BY 1", "abc", 6},
	}
	for _, tc := range cases {
		likeOptQuery(t, db, tc.sql, tc.rows, tc.count)
	}
}

// TestSQLiteLikeRangeRefusalsPin pins every condition that must REFUSE the
// optimization so the like() function keeps running per row
// (like-3.2/3.13/3.14/3.16: 12): no index, case_sensitive_like=off against a
// BINARY index, a leading wildcard, a non-constant (+x, concatenated)
// pattern or LHS.
func TestSQLiteLikeRangeRefusalsPin(t *testing.T) {
	build := func(t *testing.T) *frigolite.DB {
		db := likeOptSetup(t, "CREATE TABLE t1(x TEXT)")
		for _, s := range likeOptT1 {
			if r := db.Exec("INSERT INTO t1 VALUES('" + s + "')"); r.Error != nil {
				t.Fatalf("insert %q: %v", s, r.Error)
			}
		}
		return db
	}
	// No index at all (like-3.2): 12 calls.
	db := build(t)
	likeOptQuery(t, db, "SELECT x FROM t1 WHERE x LIKE 'abc%'", "abc|abcd|ABC|ABC abc xyz", 12)
	// BINARY index but case-insensitive LIKE (like-3.13/3.14): refused.
	db2 := likeOptSetup(t, "CREATE TABLE t1(x TEXT); CREATE INDEX i1 ON t1(x)")
	for _, s := range likeOptT1 {
		if r := db2.Exec("INSERT INTO t1 VALUES('" + s + "')"); r.Error != nil {
			t.Fatalf("insert %q: %v", s, r.Error)
		}
	}
	likeOptQuery(t, db2, "SELECT x FROM t1 WHERE x LIKE 'abc%'", "abc|abcd|ABC|ABC abc xyz", 12)
	// Index dropped again (like-3.15/3.16, CSL=on): 12 calls.
	if r := db2.Exec("PRAGMA case_sensitive_like=on; DROP INDEX i1"); r.Error != nil {
		t.Fatalf("drop: %v", r.Error)
	}
	likeOptQuery(t, db2, "SELECT x FROM t1 WHERE x LIKE 'abc%'", "abc|abcd", 12)
	// Leading wildcard (like-3.11/3.12): index exists (CSL=on) but the
	// pattern has no usable prefix: full table, 12 calls.
	if r := db2.Exec("PRAGMA case_sensitive_like=on; CREATE INDEX i1 ON t1(x)"); r.Error != nil {
		t.Fatalf("recreate: %v", r.Error)
	}
	likeOptQuery(t, db2, "SELECT x FROM t1 WHERE x LIKE '%bcd'", "abcd|bcd", 12)
	// +x defeats the LHS column (like-4.3/4.4), a concatenated pattern is
	// not a constant (like-4.5/4.6): 12 calls each.
	likeOptQuery(t, db2, "SELECT x FROM t1 WHERE +x LIKE 'abc%'", "abc|abcd", 12)
	likeOptQuery(t, db2, "SELECT x FROM t1 WHERE x LIKE ('ab' || 'c%')", "abc|abcd", 12)
}

// TestSQLiteGlobRangePin pins the GLOB flavor: always BINARY, independent of
// case_sensitive_like (like-3.19/3.20/3.21/3.22: 0 with the index, 12
// without it; like-3.23/3.24: 6 for the bracket pattern 'a[bc]d').
func TestSQLiteGlobRangePin(t *testing.T) {
	build := func(t *testing.T, ddl string) *frigolite.DB {
		db := likeOptSetup(t, ddl)
		for _, s := range likeOptT1 {
			if r := db.Exec("INSERT INTO t1 VALUES('" + s + "')"); r.Error != nil {
				t.Fatalf("insert %q: %v", s, r.Error)
			}
		}
		return db
	}
	// No index: 12 calls (like-3.17/3.18).
	db := build(t, "CREATE TABLE t1(x TEXT)")
	likeOptQuery(t, db, "SELECT x FROM t1 WHERE x GLOB 'abc*'", "abc|abcd", 12)
	// With the index: elided regardless of case_sensitive_like.
	db2 := build(t, "CREATE TABLE t1(x TEXT); PRAGMA case_sensitive_like=on; CREATE INDEX i1 ON t1(x)")
	likeOptQuery(t, db2, "SELECT x FROM t1 WHERE x GLOB 'abc*'", "abc|abcd", 0)
	db3 := build(t, "CREATE TABLE t1(x TEXT); CREATE INDEX i1 ON t1(x)")
	likeOptQuery(t, db3, "SELECT x FROM t1 WHERE x GLOB 'abc*'", "abc|abcd", 0)
	// Bracket pattern: prefix 'a' only, not complete: 6 in-range calls.
	likeOptQuery(t, db3, "SELECT x FROM t1 WHERE x GLOB 'a[bc]d'", "acd|abd", 6)
	// Wildcard-free GLOB patterns (ticket e090183531fc2747).
	likeOptQuery(t, db3, "SELECT x FROM t1 WHERE x GLOB 'a'", "a", 6)
	likeOptQuery(t, db3, "SELECT x FROM t1 WHERE x GLOB 'abcd'", "abcd", 1)
	likeOptQuery(t, db3, "SELECT x FROM t1 WHERE x GLOB 'abcde'", "", 0)
}

// TestSQLiteLikeRangeNocasePin pins the NOCASE flavor: a NOCASE column+index
// with case-insensitive LIKE synthesizes case-folded bounds under the NOCASE
// collation (like-5.3/5.4: 0, like-5.13/5.14: 0) while case_sensitive_like=on
// refuses it (like-5.6: 12), and GLOB refuses a NOCASE index entirely
// (like-5.8: 12).
func TestSQLiteLikeRangeNocasePin(t *testing.T) {
	build := func(t *testing.T) *frigolite.DB {
		db := likeOptSetup(t, "CREATE TABLE t2(x TEXT COLLATE NOCASE); CREATE INDEX i2 ON t2(x COLLATE NOCASE)")
		for _, s := range likeOptT1 {
			if r := db.Exec("INSERT INTO t2 VALUES('" + s + "')"); r.Error != nil {
				t.Fatalf("insert %q: %v", s, r.Error)
			}
		}
		return db
	}
	// case-insensitive LIKE over the NOCASE index: fully elided.
	db := build(t)
	likeOptQuery(t, db, "SELECT x FROM t2 WHERE x LIKE 'abc%'", "abc|abcd|ABC|ABC abc xyz", 0)
	db2 := build(t)
	likeOptQuery(t, db2, "SELECT x FROM t2 WHERE x LIKE 'ABC%'", "abc|abcd|ABC|ABC abc xyz", 0)
	// case_sensitive_like=on: LIKE is case-sensitive, the NOCASE keys cannot
	// bound it: refused, 12 calls (like-5.5/5.6).
	db3 := build(t)
	if r := db3.Exec("PRAGMA case_sensitive_like=on"); r.Error != nil {
		t.Fatalf("csl: %v", r.Error)
	}
	likeOptQuery(t, db3, "SELECT x FROM t2 WHERE x LIKE 'abc%'", "abc|abcd", 12)
	// GLOB needs BINARY ordering: NOCASE index refused (like-5.7/5.8).
	db4 := build(t)
	likeOptQuery(t, db4, "SELECT x FROM t2 WHERE x GLOB 'abc*'", "abc|abcd", 12)
	// Mixed-case data matching through the folded bounds (like-5.21-5.24
	// shape): zz% matches all four foldings; elided (NOCASE, complete).
	db5 := likeOptSetup(t, "CREATE TABLE t2(x TEXT COLLATE NOCASE); CREATE INDEX i2 ON t2(x COLLATE NOCASE);"+
		"INSERT INTO t2 VALUES('ZZ-upper'),('zZ-lower'),('Zz-mixed'),('zz-lower2')")
	likeOptQuery(t, db5, "SELECT x FROM t2 WHERE x LIKE 'zz%'", "ZZ-upper|zZ-lower|Zz-mixed|zz-lower2", 0)
}

// TestSQLiteLikeRangeNocaseBlobPin pins wherecode.c's two-pass
// LIKE-optimization scan: with case-folded (NOCASE) bounds the like() call
// survives on the BLOB pass (TERM_LIKECOND), so a blob whose bytes fold to
// the prefix still matches AND still counts one like() call; the TEXT rows
// are decided by the range alone (0 extra calls).
func TestSQLiteLikeRangeNocaseBlobPin(t *testing.T) {
	db := likeOptSetup(t, "CREATE TABLE t2(x TEXT COLLATE NOCASE); CREATE INDEX i2 ON t2(x COLLATE NOCASE);"+
		"INSERT INTO t2 VALUES('abc'),('abcd'),('ABC')")
	if r := db.Exec("INSERT INTO t2 VALUES(x'616263')"); r.Error != nil {
		t.Fatalf("blob insert: %v", r.Error)
	}
	db.ResetLikeCallCount()
	r := db.Query("SELECT quote(x), typeof(x) FROM t2 WHERE x LIKE 'abc%' ORDER BY x")
	if r.Error != nil {
		t.Fatalf("query: %v", r.Error)
	}
	if len(r.Rows) != 4 {
		t.Fatalf("rows: got %v, want the 3 text rows plus the blob", r.Rows)
	}
	last := r.Rows[3]
	if s, ok := last[1].(string); !ok || s != "blob" {
		t.Fatalf("last row type: got %v, want blob", last)
	}
	if got := db.LikeCallCount(); got != 1 {
		t.Fatalf("like calls: got %d, want 1 (blob pass only)", got)
	}
}

// TestSQLiteLikeRangeEscapePin pins prefix handling under ESCAPE: an escaped
// wildcard is a literal prefix character; the pattern 'ab/%d%' ESCAPE '/'
// is complete (single trailing wildcard) and elides, 'ab/%x_' keeps the
// like() call for its in-range rows, and 'ab/%' ESCAPE '/' has no wildcard
// at all (its range covers no rows). Row sets verified against sqlite3.
func TestSQLiteLikeRangeEscapePin(t *testing.T) {
	db := likeOptSetup(t, "CREATE TABLE t1e(x TEXT); PRAGMA case_sensitive_like=on; CREATE INDEX i1e ON t1e(x);"+
		"INSERT INTO t1e VALUES('ab%d'),('ab%e'),('ab%fx'),('abx'),('ab%x'),('ab%xy')")
	likeOptQuery(t, db, "SELECT x FROM t1e WHERE x LIKE 'ab/%d%' ESCAPE '/'", "ab%d", 0)
	likeOptQuery(t, db, "SELECT x FROM t1e WHERE x LIKE 'ab/%x%' ESCAPE '/'", "ab%x|ab%xy", 0)
	likeOptQuery(t, db, "SELECT x FROM t1e WHERE x LIKE 'ab/%x_' ESCAPE '/'", "ab%xy", 2)
	likeOptQuery(t, db, "SELECT x FROM t1e WHERE x LIKE 'ab/%' ESCAPE '/'", "", 5)
}

// TestSQLiteLikeRangeAtFoldPin pins whereexpr.c's '@' rule: with folded
// (NOCASE) bounds a prefix ending in '@' must NOT elide the like() call
// (incrementing '@' lands in the alphabet where case conversion breaks the
// inequality), so the in-range rows keep counting. Row set verified against
// sqlite3.
func TestSQLiteLikeRangeAtFoldPin(t *testing.T) {
	db := likeOptSetup(t, "CREATE TABLE t9(x TEXT COLLATE NOCASE); CREATE INDEX i9 ON t9(x COLLATE NOCASE);"+
		"INSERT INTO t9 VALUES('ab@'),('abA'),('abZ'),('ab['),('aba'),('ab`')")
	// In-range rows under the lower-folded NOCASE ordering: ab@, ab[, ab`
	// (all below 'aba', the fold of the incremented bound 'abA') — three
	// like() calls, matching sqlite3 exactly.
	likeOptQuery(t, db, "SELECT x FROM t9 WHERE x LIKE 'ab@%'", "ab@", 3)
}

// TestSQLiteLikeRangeVarPatternPin pins the bound-parameter branch of
// isLikeOrGlob: a $var pattern resolves through the TCL variable table (the
// sqlite3 TCL driver binds $::name as a parameter) and drives the range
// optimization when the query planner stability guarantee is OFF
// (like-3.3.102/3.3.103: count 0), but not when QPSG is ON
// (like-3.3.104/3.3.105: count 12).
func TestSQLiteLikeRangeVarPatternPin(t *testing.T) {
	db := likeOptSetup(t, "CREATE TABLE t1(x TEXT); PRAGMA case_sensitive_like=on; CREATE INDEX i1 ON t1(x)")
	for _, s := range likeOptT1 {
		if r := db.Exec("INSERT INTO t1 VALUES('" + s + "')"); r.Error != nil {
			t.Fatalf("insert %q: %v", s, r.Error)
		}
	}
	vtab.TclVarSet("likepat_pin", "", "ab%")
	t.Cleanup(func() { vtab.TclVarSet("likepat_pin", "", "") })
	// QPSG off (default): the bound pattern drives the range, elided.
	likeOptQuery(t, db, "SELECT x FROM t1 WHERE x LIKE $::likepat_pin", "ab|abc|abcd|abd", 0)
	// QPSG on: planning must not read the runtime pattern value.
	db.SetQPSG(true)
	likeOptQuery(t, db, "SELECT x FROM t1 WHERE x LIKE $::likepat_pin", "ab|abc|abcd|abd", 12)
	db.SetQPSG(false)
	// An unbound $var stays NULL: like() never runs, no rows.
	likeOptQuery(t, db, "SELECT x FROM t1 WHERE x LIKE $::nosuchvar_pin", "", 0)
}

// TestSQLiteLikeUtf8FFDFPin pins like.c's invalid-UTF-8 equivalence
// (util.c sqlite3Utf8Read: 0xFE and 0xFF both decode to U+FFFD), so a
// pattern containing raw byte 0xFE matches a value containing raw byte 0xFF
// (like.test 9.4.2/9.5.1).
func TestSQLiteLikeUtf8FFDFPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"CREATE TABLE t2(x TEXT COLLATE NOCASE)",
		"INSERT INTO t2 VALUES(CAST(x'ff' AS TEXT) || 'hello')",
		"INSERT INTO t2 VALUES('x')",
		"INSERT INTO t2 VALUES('xyz')",
	} {
		if r := db.Exec(stmt); r.Error != nil {
			t.Fatalf("%s: %v", stmt, r.Error)
		}
	}
	// 0xFE pattern vs 0xFF value: both normalize to U+FFFD.
	db.ResetLikeCallCount()
	r := db.Query("SELECT 1 FROM t2 WHERE x LIKE '%' || CAST(x'fe' AS TEXT) || '%' ORDER BY 1")
	if r.Error != nil {
		t.Fatalf("query: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("0xFE vs 0xFF: got %v rows, want 1", r.Rows)
	}
	if got := db.LikeCallCount(); got != 3 {
		t.Fatalf("like calls: got %d, want 3 (leading-wildcard pattern: one per row)", got)
	}
	// 0xFF pattern vs 0xFF value (like-9.4.2 shape).
	r = db.Query("SELECT substr(x,2) FROM t2 WHERE x LIKE '%' || CAST(x'ff' AS TEXT) || '%'")
	if r.Error != nil {
		t.Fatalf("query: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("0xFF vs 0xFF: got %v rows, want 1", r.Rows)
	}
}
