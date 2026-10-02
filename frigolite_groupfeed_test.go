package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// Native pins for the PERF grouped-aggregate feed (the streaming OP_AggStep/
// OP_AggFinal GROUP BY path in internal/execquery/select_agg_groupfeed.go):
// a GROUP BY whose terms are bare stored columns and whose output is bare
// aggregates (COUNT/SUM/AVG/TOTAL/MIN/MAX) or GROUP BY term projections
// accumulates per group straight from the scan. Parity is checked against the
// forced-generic path: appending an ORDER BY over the group key makes the
// generic route run (the feed declines ORDER BY) with one deterministic order
// the ORDER BY-free fast path must reproduce exactly — same rows, same types,
// same order. Key-class semantics (int/float spelling groups, text/blob/NULL
// separation, -0.0 folding, collated terms) are pinned against the sqlite3
// oracle expectations established in frigolite_aggfeed_test.go.

// groupFeedOpen builds the pin fixture: g is the benchmark shape (3000 rows,
// keys i%1000); tm mixes key storage classes in one column; tc carries a
// NOCASE-collated key column; tb carries blob/text/int keys; tnull keys
// include NULLs; tk mixes int64/float64 key values including exponent-spelled
// floats.
func groupFeedOpen(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	exec("CREATE TABLE g(id INTEGER PRIMARY KEY, k INTEGER, c INTEGER)")
	exec("BEGIN")
	for i := 1; i <= 3000; i++ {
		exec(fmt.Sprintf("INSERT INTO g(id,k,c) VALUES (%d,%d,%d)", i, i%1000, (i*7)%1009))
	}
	exec("COMMIT")
	// one mixed-class key column: INTEGER 5, REAL 5.0/5.5, TEXT '5', BLOB
	// x'35', NULL, 0.0/-0.0, and a big float.
	exec("CREATE TABLE tm(k, c)")
	for _, v := range []string{
		"(5, 1)", "(5.0, 2)", "('5', 3)", "(5.5, 4)", "(x'35', 5)",
		"(NULL, 6)", "(0.0, 7)", "(-0.0, 8)", "(9e99, 9)", "(6, 10)",
		"(5, 11)", "(5.0, 12)",
	} {
		exec("INSERT INTO tm(k,c) VALUES " + v)
	}
	exec("CREATE TABLE tc(k TEXT COLLATE nocase, c INTEGER)")
	exec("INSERT INTO tc VALUES ('abc',1),('ABC',2),('zzz',3),('aBc',4)")
	exec("CREATE TABLE tb(k, c)")
	exec("INSERT INTO tb VALUES (x'4142',1),(x'4142',2),('AB',3),(4,4),(4.0,5)")
	exec("CREATE TABLE tnull(k, c)")
	exec("INSERT INTO tnull VALUES (NULL,1),(NULL,2),(1,3),(1,4),(2,5)")
	exec("CREATE TABLE tk(k, c)")
	exec("INSERT INTO tk VALUES (5,1),(5.0,2),(5.5,3),(1000000000000000,4),(1e15,5)")
	return db
}

// groupFeedRows renders one query's full result (columns + typed cells, or
// the error text).
func groupFeedRows(t *testing.T, db *DB, sql string) string {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		return "ERR: " + res.Error.Error()
	}
	var sb strings.Builder
	for i, col := range res.Columns {
		if i > 0 {
			sb.WriteByte('|')
		}
		sb.WriteString(col)
	}
	sb.WriteString(" ;; ")
	for _, row := range res.Rows {
		for i, v := range row {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "%T:%v", v, v)
		}
		sb.WriteByte(';')
	}
	return sb.String()
}

// groupFeedParity runs one grouped query through the fast path and through
// the forced-generic path (the ORDER BY clause pins the group order; the feed
// declines ORDER BY statements) and fails on any divergence.
func groupFeedParity(t *testing.T, db *DB, fastSQL, genericSQL string) string {
	t.Helper()
	fast := groupFeedRows(t, db, fastSQL)
	generic := groupFeedRows(t, db, genericSQL)
	if fast != generic {
		t.Fatalf("parity diverged for %s\n  fast    %s\n  generic %s", fastSQL, fast, generic)
	}
	return fast
}

func TestGroupFeedParityShapes(t *testing.T) {
	db := groupFeedOpen(t)
	defer db.Close()
	for _, tc := range [][2]string{
		// the benchmark shape
		{"SELECT k, COUNT(*), SUM(c) FROM g GROUP BY k",
			"SELECT k, COUNT(*), SUM(c) FROM g GROUP BY k ORDER BY k"},
		{"SELECT k, COUNT(*), COUNT(c), SUM(c), AVG(c), TOTAL(c) FROM g GROUP BY k",
			"SELECT k, COUNT(*), COUNT(c), SUM(c), AVG(c), TOTAL(c) FROM g GROUP BY k ORDER BY k"},
		{"SELECT k, MIN(c), MAX(c) FROM g GROUP BY k",
			"SELECT k, MIN(c), MAX(c) FROM g GROUP BY k ORDER BY k"},
		// aggregates-only output (ORDER BY key resolves from the group's
		// representative row map, exactly like the generic pass)
		{"SELECT SUM(c), COUNT(*) FROM g GROUP BY k",
			"SELECT SUM(c), COUNT(*) FROM g GROUP BY k ORDER BY k"},
		{"SELECT COUNT(*), SUM(c), AVG(c) FROM g WHERE id <= 300 GROUP BY k",
			"SELECT COUNT(*), SUM(c), AVG(c) FROM g WHERE id <= 300 GROUP BY k ORDER BY k"},
		// WHERE + GROUP BY: range seek feeds, plain scan feeds
		{"SELECT k, COUNT(*), SUM(c) FROM g WHERE id BETWEEN 100 AND 2900 GROUP BY k",
			"SELECT k, COUNT(*), SUM(c) FROM g WHERE id BETWEEN 100 AND 2900 GROUP BY k ORDER BY k"},
		{"SELECT k, COUNT(*) FROM g WHERE c > 500 GROUP BY k",
			"SELECT k, COUNT(*) FROM g WHERE c > 500 GROUP BY k ORDER BY k"},
		{"SELECT k, COUNT(*), SUM(c) FROM g WHERE id >= 2500 GROUP BY k",
			"SELECT k, COUNT(*), SUM(c) FROM g WHERE id >= 2500 GROUP BY k ORDER BY k"},
		// equality seek feed
		{"SELECT k, COUNT(*) FROM g WHERE id = 1234 GROUP BY k",
			"SELECT k, COUNT(*) FROM g WHERE id = 1234 GROUP BY k ORDER BY k"},
		// multi-term GROUP BY
		{"SELECT k, c%2, COUNT(*) FROM g GROUP BY k, c%2",
			"SELECT k, c%2, COUNT(*) FROM g GROUP BY k, c%2 ORDER BY k, c%2"},
		{"SELECT k, c%2, SUM(c) FROM g WHERE id < 500 GROUP BY k, c%2",
			"SELECT k, c%2, SUM(c) FROM g WHERE id < 500 GROUP BY k, c%2 ORDER BY k, c%2"},
		// rowid pseudo-column key and argument
		{"SELECT rowid, COUNT(*) FROM g WHERE id <= 50 GROUP BY rowid",
			"SELECT rowid, COUNT(*) FROM g WHERE id <= 50 GROUP BY rowid ORDER BY rowid"},
		{"SELECT k, SUM(rowid) FROM g WHERE id <= 50 GROUP BY k",
			"SELECT k, SUM(rowid) FROM g WHERE id <= 50 GROUP BY k ORDER BY k"},
		// LIMIT/OFFSET over the grouped output
		{"SELECT k, COUNT(*) FROM g GROUP BY k LIMIT 3",
			"SELECT k, COUNT(*) FROM g GROUP BY k ORDER BY k LIMIT 3"},
		{"SELECT k, COUNT(*) FROM g GROUP BY k LIMIT 3 OFFSET 2",
			"SELECT k, COUNT(*) FROM g GROUP BY k ORDER BY k LIMIT 3 OFFSET 2"},
		// GROUP BY without aggregates: pure term projection
		{"SELECT k FROM g WHERE id <= 20 GROUP BY k",
			"SELECT k FROM g WHERE id <= 20 GROUP BY k ORDER BY k"},
	} {
		groupFeedParity(t, db, tc[0], tc[1])
	}
	// compound chain: both variants take the generic pass (the feed declines
	// UNION); pin the merged shape — head groups then the member row.
	got2 := groupFeedRows(t, db, "SELECT k, COUNT(*) FROM g WHERE id <= 300 GROUP BY k UNION ALL SELECT 1, 1")
	if !strings.HasPrefix(got2, "k|COUNT(*) ;; int64:1,int64:1;") || !strings.HasSuffix(got2, "int64:1,int64:1;") {
		t.Fatalf("compound chain shape: %.80s ... %.40s", got2, got2[len(got2)-40:])
	}
	if n := strings.Count(got2, ";"); n != 303 {
		t.Fatalf("compound chain rows: %d, want 303", n)
	}
}

func TestGroupFeedKeyClasses(t *testing.T) {
	db := groupFeedOpen(t)
	defer db.Close()
	// Mixed-class keys (oracle-checked semantics): INTEGER 5 and REAL 5.0
	// share one group whose representative is the first row's (integer 5);
	// 0.0/-0.0 share one (representative 0.0); TEXT '5', BLOB x'35', 5.5,
	// 9e99 and NULL stay separate; 6 is its own group.
	got := groupFeedParity(t, db,
		"SELECT count(*), typeof(k) FROM tm GROUP BY k",
		"SELECT count(*), typeof(k) FROM tm GROUP BY k ORDER BY k")
	want := "count(*)|typeof(k) ;; " +
		"int64:1,string:null;" + // NULL
		"int64:2,string:real;" + // 0.0 + -0.0
		"int64:4,string:integer;" + // 5, 5.0, 5, 5.0
		"int64:1,string:real;" + // 5.5
		"int64:1,string:integer;" + // 6
		"int64:1,string:real;" + // 9e99
		"int64:1,string:text;" + // '5'
		"int64:1,string:blob;" // x'35'
	if got != want {
		t.Fatalf("tm groups:\n  got  %s\n  want %s", got, want)
	}
}

func TestGroupFeedKeyEqualityIntFloat(t *testing.T) {
	db := groupFeedOpen(t)
	defer db.Close()
	// int64 5 and float64 5.0 group together (shared spelling "5"); 5.5 is
	// separate; the int64 1000000000000000 and the REAL 1e15 keep separate
	// groups (exponent spelling vs digits — the engine's serialized-key
	// semantics both paths share; the fast path's typed bucket declines the
	// float and files it under the same string key).
	got := groupFeedParity(t, db,
		"SELECT k, count(*), sum(c) FROM tk GROUP BY k",
		"SELECT k, count(*), sum(c) FROM tk GROUP BY k ORDER BY k")
	wantGot := "k|count(*)|sum(c) ;; " +
		"int64:5,int64:2,int64:3;" +
		"float64:5.5,int64:1,int64:3;" +
		"int64:1000000000000000,int64:1,int64:4;" +
		"float64:1e+15,int64:1,int64:5;"
	if got != wantGot {
		t.Fatalf("tk groups:\n  got  %s\n  want %s", got, wantGot)
	}
}

func TestGroupFeedCollatedAndNullKeys(t *testing.T) {
	db := groupFeedOpen(t)
	defer db.Close()
	// NOCASE folds 'abc'/'ABC'/'aBc' into one group; the representative is
	// the first row's spelling.
	got := groupFeedParity(t, db,
		"SELECT k, count(*), sum(c) FROM tc GROUP BY k",
		"SELECT k, count(*), sum(c) FROM tc GROUP BY k ORDER BY k")
	wantGot := "k|count(*)|sum(c) ;; string:abc,int64:3,int64:7;string:zzz,int64:1,int64:3;"
	if got != wantGot {
		t.Fatalf("tc groups:\n  got  %s\n  want %s", got, wantGot)
	}
	// blob vs text keys stay separate; int 4 and float 4.0 share a group
	got = groupFeedParity(t, db,
		"SELECT k, count(*) FROM tb GROUP BY k",
		"SELECT k, count(*) FROM tb GROUP BY k ORDER BY k")
	wantGot = "k|count(*) ;; int64:4,int64:2;string:AB,int64:1;[]uint8:[65 66],int64:2;"
	if got != wantGot {
		t.Fatalf("tb groups:\n  got  %s\n  want %s", got, wantGot)
	}
	// NULL keys share one group and sort first
	got = groupFeedParity(t, db,
		"SELECT k, count(*) FROM tnull GROUP BY k",
		"SELECT k, count(*) FROM tnull GROUP BY k ORDER BY k")
	wantGot = "k|count(*) ;; <nil>:<nil>,int64:2;int64:1,int64:2;int64:2,int64:1;"
	if got != wantGot {
		t.Fatalf("tnull groups:\n  got  %s\n  want %s", got, wantGot)
	}
}

func TestGroupFeedMinMax(t *testing.T) {
	db := groupFeedOpen(t)
	defer db.Close()
	// MIN/MAX per group over the full table; k=0 rows are ids 1000, 2000,
	// 3000 → c = 946, 883, 820, so MIN=820, MAX=946 and the first (key
	// ordered) group pins the exact values.
	got := groupFeedParity(t, db,
		"SELECT k, MIN(c), MAX(c) FROM g GROUP BY k",
		"SELECT k, MIN(c), MAX(c) FROM g GROUP BY k ORDER BY k")
	if !strings.HasPrefix(got, "k|MIN(c)|MAX(c) ;; int64:0,int64:820,int64:946;") {
		t.Fatalf("min/max over g: %s", got)
	}
	// MIN/MAX under the NOCASE group reduce within the folded group
	got = groupFeedParity(t, db,
		"SELECT k, MIN(k), MAX(k) FROM tc GROUP BY k",
		"SELECT k, MIN(k), MAX(k) FROM tc GROUP BY k ORDER BY k")
	wantGot := "k|MIN(k)|MAX(k) ;; string:abc,string:abc,string:abc;string:zzz,string:zzz,string:zzz;"
	if got != wantGot {
		t.Fatalf("tc min/max:\n  got  %s\n  want %s", got, wantGot)
	}
}

func TestGroupFeedAggregateErrors(t *testing.T) {
	db := groupFeedOpen(t)
	defer db.Close()
	// int64 SUM overflow inside a group raises "integer overflow" exactly
	// like the generic path (sumFinalize at group finalize)
	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	exec("CREATE TABLE tof(k, c)")
	exec("INSERT INTO tof VALUES (1, 9223372036854775807), (1, 1), (2, 5)")
	if got := groupFeedRows(t, db, "SELECT k, SUM(c) FROM tof GROUP BY k"); got != "ERR: integer overflow" {
		t.Fatalf("overflow: %s", got)
	}
	// the ORDER BY variant (generic path) surfaces the identical error
	if got := groupFeedRows(t, db, "SELECT k, SUM(c) FROM tof GROUP BY k ORDER BY k"); got != "ERR: integer overflow" {
		t.Fatalf("overflow (generic): %s", got)
	}
}

func TestGroupFeedEmptyAndFallbackShapes(t *testing.T) {
	db := groupFeedOpen(t)
	defer db.Close()
	// zero input rows: the grouped output is empty (no row), like the
	// generic GROUP BY pass
	got := groupFeedRows(t, db, "SELECT k, COUNT(*) FROM g WHERE id > 999999 GROUP BY k")
	if got != "k|COUNT(*) ;; " {
		t.Fatalf("empty groups: %q", got)
	}
	// shapes that must keep the generic path but stay correct (parity with
	// their own generic run, which they take in both variants)
	for _, tc := range [][2]string{
		// HAVING filters groups
		{"SELECT k, COUNT(*) FROM g WHERE id <= 300 GROUP BY k HAVING COUNT(*) > 1",
			"SELECT k, COUNT(*) FROM g WHERE id <= 300 GROUP BY k HAVING COUNT(*) > 1 ORDER BY k"},
		// DISTINCT output
		{"SELECT DISTINCT k, COUNT(*) FROM g WHERE id <= 300 GROUP BY k",
			"SELECT DISTINCT k, COUNT(*) FROM g WHERE id <= 300 GROUP BY k ORDER BY k"},
		// expression output and expression GROUP BY
		{"SELECT k+0, COUNT(*) FROM g WHERE id <= 300 GROUP BY k",
			"SELECT k+0, COUNT(*) FROM g WHERE id <= 300 GROUP BY k ORDER BY k+0"},
		{"SELECT c%2, COUNT(*) FROM g WHERE id <= 300 GROUP BY c%2",
			"SELECT c%2, COUNT(*) FROM g WHERE id <= 300 GROUP BY c%2 ORDER BY 1"},
		// window over grouped rows
		{"SELECT k, COUNT(*), COUNT(*) OVER () FROM g WHERE id <= 300 GROUP BY k",
			"SELECT k, COUNT(*), COUNT(*) OVER () FROM g WHERE id <= 300 GROUP BY k ORDER BY k"},
		// bare non-aggregate output column (per-group representative row)
		{"SELECT k, c FROM g WHERE id <= 300 GROUP BY k",
			"SELECT k, c FROM g WHERE id <= 300 GROUP BY k ORDER BY k"},
		// GROUP BY ordinal resolution keeps working
		{"SELECT k, COUNT(*) FROM g WHERE id <= 300 GROUP BY 1",
			"SELECT k, COUNT(*) FROM g WHERE id <= 300 GROUP BY 1 ORDER BY k"},
	} {
		groupFeedParity(t, db, tc[0], tc[1])
	}
	// compound chain: both variants take the generic pass (the feed declines
	// UNION); pin the merged shape — head groups then the member row.
	got2 := groupFeedRows(t, db, "SELECT k, COUNT(*) FROM g WHERE id <= 300 GROUP BY k UNION ALL SELECT 1, 1")
	if !strings.HasPrefix(got2, "k|COUNT(*) ;; int64:1,int64:1;") || !strings.HasSuffix(got2, "int64:1,int64:1;") {
		t.Fatalf("compound chain shape: %.80s ... %.40s", got2, got2[len(got2)-40:])
	}
	if n := strings.Count(got2, ";"); n != 303 {
		t.Fatalf("compound chain rows: %d, want 303", n)
	}
}
