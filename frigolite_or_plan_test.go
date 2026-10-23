// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"strconv"
	"strings"
	"testing"
)

// flattenRowsForTest renders the rows of a result as a space-separated list
// of their first column.
func flattenRowsForTest(rows [][]interface{}) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		switch v := row[0].(type) {
		case nil:
			parts = append(parts, "NULL")
		case int64:
			parts = append(parts, strconv.FormatInt(v, 10))
		case string:
			parts = append(parts, v)
		default:
			parts = append(parts, strings.TrimSpace(formatSQLiteValue(v)))
		}
	}
	return strings.Join(parts, " ")
}

// The OR-index plan (SQLite's multi-index OR) serves a branch with the index
// that constrains it. SQLite renders the branch kinds it can serve as:
//
//	SEARCH t2 USING INDEX t2b (b=?)            an equality
//	SEARCH t2 USING INDEX t2d (d>? AND d<?)    a range
//	SEARCH t2 USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)  a rowid range
//	SEARCH t2 USING INDEX t2f (f>? AND f<?)    a GLOB/LIKE prefix range
//	SEARCH t2 USING INDEX t2e (e=?)            "e IS NULL"
//
// The tests below pin the branch kinds the plan must serve, and the row set
// each of them must produce (the oracle: sqlite3 3.54).

// orPlanFixture builds the where7-style t2 table (a rowid alias, the
// single-column indexes and a text column) used by the branch tests.
func orPlanFixture(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	stmts := []string{
		"CREATE TABLE t2(a INTEGER PRIMARY KEY,b,c,d,e,f TEXT,g)",
		"INSERT INTO t2 VALUES(13,143,1103,13.013,130.3,'nopqrstuv','gfedcba')",
		"INSERT INTO t2 VALUES(39,429,1103,39.039,390.3,'nopqrstuv','fedcbaz')",
		"INSERT INTO t2 VALUES(53,583,1204,53.053,530.3,'opqrstuvw','edcbazy')",
		"INSERT INTO t2 VALUES(65,715,1304,65.065,650.3,'nopqrstuv','edcbazy')",
		"INSERT INTO t2 VALUES(91,1001,1404,91.091,910.3,'nopqrstuv','fedcbaz')",
		"INSERT INTO t2 VALUES(97,1067,1304,97.097,970.3,'uvwxyzabc','edcbazy')",
		"CREATE INDEX t2b ON t2(b)",
		"CREATE INDEX t2d ON t2(d)",
		"CREATE INDEX t2e ON t2(e)",
		"CREATE INDEX t2f ON t2(f)",
	}
	for _, s := range stmts {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	return db
}

// TestOrPlanServesGlobPrefixBranch pins that a GLOB with a literal prefix
// serves an OR branch: SQLite scans the index over the prefix's range
// ("SEARCH t2 USING INDEX t2f (f>? AND f<?)"). The prefix stops at the first
// GLOB wildcard ('*', '?' or '['), so "?opqr*" has no prefix and "nopq*" has
// the prefix "nopq".
func TestOrPlanServesGlobPrefixBranch(t *testing.T) {
	db := orPlanFixture(t)
	res := db.Query("SELECT a FROM t2 WHERE (f GLOB '?opqr*' AND f GLOB 'nopq*') OR b=583")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	got := flattenRowsForTest(res.Rows)
	want := "13 39 65 91 53"
	if got != want {
		t.Fatalf("rows = %q, want %q", got, want)
	}
}

// TestOrPlanServesIsNullBranch pins that "col IS NULL" serves an OR branch:
// SQLite seeks the index for the NULL ("SEARCH t2 USING INDEX t2e (e=?)").
func TestOrPlanServesIsNullBranch(t *testing.T) {
	db := orPlanFixture(t)
	// The e column holds a non-NULL value for every row: the branch must
	// select nothing, and the other branch must still deliver its row.
	res := db.Query("SELECT a FROM t2 WHERE e IS NULL OR b=583")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	got := flattenRowsForTest(res.Rows)
	if got != "53" {
		t.Fatalf("rows = %q, want %q", got, "53")
	}
}

// TestOrPlanServesFlippedRangeBranch pins that a range whose constant is on
// the left serves an OR branch: "1000000<b" is the range b > 1000000.
func TestOrPlanServesFlippedRangeBranch(t *testing.T) {
	db := orPlanFixture(t)
	res := db.Query("SELECT a FROM t2 WHERE 1000000<b OR b=583")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	got := flattenRowsForTest(res.Rows)
	if got != "53" {
		t.Fatalf("rows = %q, want %q", got, "53")
	}
}

// TestOrPlanServesRowidRangeBranch pins that a range on the INTEGER PRIMARY
// KEY serves an OR branch ("USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)").
func TestOrPlanServesRowidRangeBranch(t *testing.T) {
	db := orPlanFixture(t)
	res := db.Query("SELECT a FROM t2 WHERE (a BETWEEN 39 AND 65) OR b=583")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	got := flattenRowsForTest(res.Rows)
	want := "39 53 65"
	if got != want {
		t.Fatalf("rows = %q, want %q", got, want)
	}
}

// TestOrderByRowidAliasOrdinalNeedsNoSorter pins that an ORDER BY on the
// rowid alias needs no sorter when the rows come from a plain table scan —
// including when the ORDER BY names the column by its ordinal. SQLite emits
// OP_Sort only when the loop does not deliver the ordering, and the
// statement-status SORT counter follows (where7's count_steps helper).
func TestOrderByRowidAliasOrdinalNeedsNoSorter(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY,b,c,d)",
		"INSERT INTO t1 VALUES(1,2,3,4)",
		"INSERT INTO t1 VALUES(2,3,4,5)",
		"CREATE INDEX t1b ON t1(b)",
		"CREATE INDEX t1c ON t1(c)",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	// The "+b=3" branch cannot drive an index (a unary plus strips the
	// affinity), so the plan is a plain table scan and its rows are in
	// rowid order.
	res := db.Query("SELECT a FROM t1 WHERE +b=3 OR c=6 ORDER BY 1")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := flattenRowsForTest(res.Rows); got != "2" {
		t.Fatalf("rows = %q, want %q", got, "2")
	}
	if n := db.engine.StmtStatusSort(); n != 0 {
		t.Fatalf("sort counter = %d, want 0 (the scan delivers the order)", n)
	}
}
