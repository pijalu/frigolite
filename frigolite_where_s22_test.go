// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"strings"
	"testing"
)

// The single-table ORDER BY delivery contracts of the S22 slice, pinned
// against the sqlite3 3.54 CLI oracle (the where.test cksort shapes):
//
//   - a stat1-priced seek wins at any selectivity below a full scan
//     (SEARCH t1 USING INDEX i1xy (x=?) at ~12% over stat1 "100 8 1") and
//     delivers the ORDER BY the index carries (nosort);
//   - an estimate tie (stat1 "1000 1000 1") goes to the scan;
//   - a trailing rowid-named ORDER BY term rides the index entry's rowid
//     tail (where-12.2: ORDER BY b, a over (b));
//   - a covering ORDER-BY-serving walk beats the seek+sorter path whenever
//     the sorter's priced cost makes it so (where-6.7.x: SCAN t3 USING
//     COVERING INDEX t3acb).

// s22Fixture executes every statement on a fresh in-memory database.
func s22Fixture(t *testing.T, stmts ...string) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range stmts {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	return db
}

// s22EQP returns the rendered EXPLAIN QUERY PLAN lines.
func s22EQP(t *testing.T, db *DB, sql string) string {
	t.Helper()
	res := db.Query("EXPLAIN QUERY PLAN " + sql)
	if res.Error != nil {
		t.Fatalf("EQP %s: %v", sql, res.Error)
	}
	var lines []string
	for _, row := range res.Rows {
		if len(row) > 0 {
			if s, ok := row[0].(string); ok {
				lines = append(lines, s)
			}
		}
	}
	return strings.Join(lines, "\n")
}

// s22Rows renders a query's rows as space-separated flattened tuples.
func s22Rows(t *testing.T, db *DB, sql string) string {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		t.Fatalf("%s: %v", sql, res.Error)
	}
	parts := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		cells := make([]string, 0, len(row))
		for _, v := range row {
			cells = append(cells, strings.TrimSpace(formatSQLiteValue("", v)))
		}
		parts = append(parts, strings.Join(cells, " "))
	}
	return strings.Join(parts, " ")
}

// s22SortMarker reports the cksort marker of one query ("sort"/"nosort").
func s22SortMarker(t *testing.T, db *DB, sql string) string {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		t.Fatalf("%s: %v", sql, res.Error)
	}
	if db.engine.StmtStatusSortPeek() > 0 {
		return "sort"
	}
	return "nosort"
}

// TestS22_SeekDeliversOrderUnderStat1 pins the seek threshold: with stat1
// present the seek wins at ~12% selectivity (C has no 10% cliff), and the
// equality-pinned index delivers the ORDER BY (where-7.1's nosort).
func TestS22_SeekDeliversOrderUnderStat1(t *testing.T) {
	db := s22Fixture(t,
		"CREATE TABLE t1(w int, x int, y int)",
		"CREATE INDEX i1w ON t1(w)",
		"CREATE INDEX i1xy ON t1(x,y)",
		"INSERT INTO t1 WITH RECURSIVE k(w) AS (SELECT 1 UNION ALL SELECT w+1 FROM k WHERE w<100) SELECT w, (w-1)/8+1, w*w FROM k",
		"ANALYZE",
	)
	if got := s22SortMarker(t, db, "SELECT w FROM t1 WHERE x=3 ORDER BY y"); got != "nosort" {
		t.Fatalf("x=3 ORDER BY y: marker %s, want nosort", got)
	}
	if got := s22SortMarker(t, db, "SELECT w FROM t1 WHERE x=3 AND y>100 ORDER BY y LIMIT 3"); got != "nosort" {
		t.Fatalf("x=3 AND y>100 ORDER BY y LIMIT 3: marker %s, want nosort", got)
	}
	if got := s22EQP(t, db, "SELECT w FROM t1 WHERE x=3 ORDER BY y"); !strings.Contains(got, "SEARCH t1 USING INDEX i1xy") {
		t.Fatalf("EQP: %s", got)
	}
}

// TestS22_SeekAtHalfSelectivity pins the seek-vs-scan boundary at 50%:
// the oracle plans the SEARCH even when the equality matches half the
// table (stat1 "1000 500 1").
func TestS22_SeekAtHalfSelectivity(t *testing.T) {
	db := s22Fixture(t,
		"CREATE TABLE t(a int, b int)",
		"CREATE INDEX iab ON t(a,b)",
		"INSERT INTO t WITH RECURSIVE k(w) AS (SELECT 1 UNION ALL SELECT w+1 FROM k WHERE w<1000) SELECT w%2, w FROM k",
		"ANALYZE",
	)
	if got := s22EQP(t, db, "SELECT b FROM t WHERE a=1"); !(strings.Contains(got, "SEARCH t") && strings.Contains(got, "iab")) {
		t.Fatalf("EQP: %s", got)
	}
}

// TestS22_SeekTieGoesToScan pins the tie: when the index's estimated rows
// equal the table's (stat1 "1000 1000 1"), the scan wins.
func TestS22_SeekTieGoesToScan(t *testing.T) {
	db := s22Fixture(t,
		"CREATE TABLE t(a int, b int)",
		"CREATE INDEX iab ON t(a,b)",
		"INSERT INTO t WITH RECURSIVE k(w) AS (SELECT 1 UNION ALL SELECT w+1 FROM k WHERE w<1000) SELECT 7, w FROM k",
		"ANALYZE",
	)
	if got := s22EQP(t, db, "SELECT b FROM t WHERE a=7"); !strings.Contains(got, "SCAN t\n") && !strings.HasSuffix(got, "SCAN t") {
		t.Fatalf("EQP: %s", got)
	}
}

// TestS22_RowidTailOrderTerm pins where-12.2: the trailing rowid-named
// ORDER BY term is delivered by the index entry's rowid tail, forward and
// backward.
func TestS22_RowidTailOrderTerm(t *testing.T) {
	db := s22Fixture(t,
		"CREATE TABLE t6(a INTEGER PRIMARY KEY, b TEXT)",
		"INSERT INTO t6 VALUES(1,'one')",
		"INSERT INTO t6 VALUES(4,'four')",
		"CREATE INDEX t6i1 ON t6(b)",
	)
	if got := s22SortMarker(t, db, "SELECT * FROM t6 ORDER BY b, a"); got != "nosort" {
		t.Fatalf("ORDER BY b, a: marker %s, want nosort", got)
	}
	if got := s22Rows(t, db, "SELECT * FROM t6 ORDER BY b, a"); got != "4 four 1 one" {
		t.Fatalf("rows %q, want 4 four 1 one", got)
	}
	if got := s22SortMarker(t, db, "SELECT * FROM t6 ORDER BY b DESC, a DESC"); got != "nosort" {
		t.Fatalf("ORDER BY b DESC, a DESC: marker %s, want nosort", got)
	}
	if got := s22Rows(t, db, "SELECT * FROM t6 ORDER BY b DESC, a DESC"); got != "1 one 4 four" {
		t.Fatalf("rows %q, want 1 one 4 four", got)
	}
}

// TestS22_CoveringOrderWalkBeatsSeekSorter pins where-6.7.x: with ANALYZE,
// WHERE b>0 ORDER BY a plans the covering a-order walk (no sorter), not
// the (b,c) seek plus a temp b-tree.
func TestS22_CoveringOrderWalkBeatsSeekSorter(t *testing.T) {
	db := s22Fixture(t,
		"CREATE TABLE t3(a,b,c)",
		"CREATE INDEX t3a ON t3(a)",
		"CREATE INDEX t3bc ON t3(b,c)",
		"CREATE INDEX t3acb ON t3(a,c,b)",
		"INSERT INTO t3 WITH RECURSIVE k(w) AS (SELECT 1 UNION ALL SELECT w+1 FROM k WHERE w<100) SELECT w, 101-w, w*w FROM k",
		"ANALYZE",
	)
	eqp := s22EQP(t, db, "SELECT * FROM t3 WHERE b>0 ORDER BY a LIMIT 1")
	if !strings.Contains(eqp, "SCAN t3 USING COVERING INDEX t3acb") {
		t.Fatalf("EQP: %s", eqp)
	}
	if got := s22SortMarker(t, db, "SELECT * FROM t3 WHERE b>0 ORDER BY a LIMIT 1"); got != "nosort" {
		t.Fatalf("marker %s, want nosort", got)
	}
	if got := s22Rows(t, db, "SELECT * FROM t3 WHERE b>0 ORDER BY a LIMIT 2"); got != "1 100 1 2 99 4" {
		t.Fatalf("rows %q, want the a-order prefix", got)
	}
}
