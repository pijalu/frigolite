package frigolite

import (
	"fmt"
	"testing"
)

// Native pins for the PERF.P7-scan simple-aggregate feed (the OP_AggStep
// parity fast path in internal/execquery/select_agg_feed.go): a bare
// COUNT/SUM/AVG/TOTAL select over one real rowid table accumulates straight
// from the scan/seek loop's decoded values. Every test asserts the
// engine-visible contract (values, types, errors), not the plan, so a feed
// regression — including one that silently routes around the fast path —
// fails here.

// aggFeedOpen builds the pin fixture: t mirrors the benchmark shape; tr holds
// REAL/TEXT/BLOB/NULL dynamics the SUM classifier must survive; ts shadows
// the rowid pseudo-column with a declared column; tshort carries ALTER
// TABLE-added defaults (short records).
func aggFeedOpen(t *testing.T) *DB {
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
	exec("CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)")
	for i := 1; i <= 200; i++ {
		exec(fmt.Sprintf("INSERT INTO t(id,c) VALUES (%d,%d)", i, i*7-350))
	}
	exec("CREATE TABLE tr(id INTEGER PRIMARY KEY, r REAL, s TEXT, b BLOB, n INTEGER)")
	for _, v := range []string{
		"(1, 1.5, '12', x'414243', NULL)",
		"(2, -2.25, 'abc', x'00', 5)",
		"(3, 3.0, ' 42 ', NULL, NULL)",
		"(4, 1e308, 'xyz', x'FF01', 7)",
		"(5, NULL, '9e2', x'', -3)",
		"(6, 0.1, '', x'DEADBEEF', 9223372036854775807)",
		"(7, -1e308, '007', NULL, -9223372036854775808)",
		"(8, 9e999, '18446744073709551616', x'31', 100)",
		"(9, 2.5, '+12', x'32332e35', NULL)",
		"(10, 4.0, '1_000', x'2d31', 0)",
	} {
		exec("INSERT INTO tr(id,r,s,b,n) VALUES " + v)
	}
	exec("CREATE TABLE ts(id INTEGER PRIMARY KEY, rowid INTEGER)")
	for i := 1; i <= 20; i++ {
		exec(fmt.Sprintf("INSERT INTO ts(id,rowid) VALUES (%d,%d)", i, i*100))
	}
	exec("CREATE TABLE tshort(id INTEGER PRIMARY KEY, a INTEGER)")
	exec("INSERT INTO tshort(id,a) VALUES (1,1),(2,2),(3,3)")
	exec("ALTER TABLE tshort ADD COLUMN d INTEGER DEFAULT 42")
	exec("ALTER TABLE tshort ADD COLUMN e TEXT DEFAULT 'zz'")
	exec("CREATE TABLE tnull(id INTEGER PRIMARY KEY, c INTEGER)")
	for i := 1; i <= 50; i++ {
		if i%3 == 0 {
			exec(fmt.Sprintf("INSERT INTO tnull(id,c) VALUES (%d,NULL)", i))
			continue
		}
		exec(fmt.Sprintf("INSERT INTO tnull(id,c) VALUES (%d,%d)", i, i))
	}
	exec("CREATE TABLE tf(id INTEGER PRIMARY KEY, c INTEGER)")
	exec("INSERT INTO tf(id,c) VALUES (1,4294967296),(2,4294967296),(3,-4294967296),(4,-4294967296)")
	return db
}

// aggFeedQuery renders one query's full result (columns, cells with Go
// types, or the error text) so every pin asserts the whole engine-visible
// contract.
func aggFeedQuery(t *testing.T, db *DB, sql string) string {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		return "ERR: " + res.Error.Error()
	}
	out := ""
	for i, col := range res.Columns {
		if i > 0 {
			out += "|"
		}
		out += col
	}
	out += " ;; "
	for _, row := range res.Rows {
		for i, v := range row {
			if i > 0 {
				out += ","
			}
			out += fmt.Sprintf("%T:%v", v, v)
		}
		out += ";"
	}
	return out
}

func TestAggFeedRangeShapes(t *testing.T) {
	db := aggFeedOpen(t)
	defer db.Close()
	for _, tc := range []struct{ sql, want string }{
		{"SELECT SUM(c), COUNT(*) FROM t WHERE id BETWEEN 50 AND 150",
			"SUM(c)|COUNT(*) ;; int64:35350,int64:101;"},
		{"SELECT SUM(c), COUNT(*) FROM t WHERE id >= 50 AND id <= 150",
			"SUM(c)|COUNT(*) ;; int64:35350,int64:101;"},
		{"SELECT COUNT(*), COUNT(c), SUM(c), AVG(c), TOTAL(c) FROM t WHERE id BETWEEN 2 AND 199",
			"COUNT(*)|COUNT(c)|SUM(c)|AVG(c)|TOTAL(c) ;; int64:198,int64:198,int64:69993,float64:353.5,float64:69993;"},
		{"SELECT SUM(c) FROM t WHERE id BETWEEN 300 AND 400",
			"SUM(c) ;; <nil>:<nil>;"},
		{"SELECT COUNT(*) FROM t WHERE id = 999999",
			"COUNT(*) ;; int64:0;"},
		{"SELECT SUM(c) FROM t WHERE id = 10",
			"SUM(c) ;; int64:-280;"},
		{"SELECT SUM(id), COUNT(id), AVG(id) FROM t WHERE id BETWEEN 5 AND 20",
			"SUM(id)|COUNT(id)|AVG(id) ;; int64:200,int64:16,float64:12.5;"},
		// rowid pseudo-column arguments (slot reads the loop's rowid).
		{"SELECT SUM(rowid), COUNT(rowid) FROM t WHERE id BETWEEN 5 AND 20",
			"SUM(rowid)|COUNT(rowid) ;; int64:200,int64:16;"},
		{"SELECT SUM(_rowid_) FROM t WHERE id BETWEEN 5 AND 20",
			"SUM(_rowid_) ;; int64:200;"},
		{"SELECT SUM(t.id) FROM t WHERE id BETWEEN 5 AND 20",
			"SUM(t.id) ;; int64:200;"},
		// case-insensitive function and column spellings
		{"SELECT sum(C) FROM t WHERE id BETWEEN 5 AND 20",
			"sum(C) ;; int64:-4200;"},
		// WHERE shapes: bound semantics through the seek's own re-check
		{"SELECT COUNT(*) FROM t WHERE id BETWEEN '50' AND '150'",
			"COUNT(*) ;; int64:101;"},
		{"SELECT COUNT(*) FROM t WHERE id BETWEEN 50.5 AND 150.5",
			"COUNT(*) ;; int64:100;"},
		{"SELECT COUNT(*) FROM t WHERE id > 1e18",
			"COUNT(*) ;; int64:0;"},
		{"SELECT COUNT(*) FROM t WHERE id >= 50 AND id < 150 AND c > 0",
			"COUNT(*) ;; int64:99;"},
		{"SELECT COUNT(*) FROM t WHERE id BETWEEN 5 AND (SELECT MAX(id) FROM t WHERE id < 10)",
			"COUNT(*) ;; int64:5;"},
		{"SELECT SUM(c) FROM t WHERE id BETWEEN 5 AND 20 AND EXISTS (SELECT 1 FROM t WHERE id=5)",
			"SUM(c) ;; int64:-4200;"},
		{"SELECT COUNT(*) FROM t WHERE id BETWEEN NULL AND 150",
			"COUNT(*) ;; int64:0;"},
		{"SELECT COUNT(*) FROM t WHERE id NOT BETWEEN 50 AND 150",
			"COUNT(*) ;; int64:99;"},
		// ORDER BY/LIMIT finalize the single aggregate row like the generic path
		{"SELECT SUM(c) AS s FROM t WHERE id BETWEEN 5 AND 20 LIMIT 1",
			"s ;; int64:-4200;"},
	} {
		if got := aggFeedQuery(t, db, tc.sql); got != tc.want {
			t.Errorf("%s\n  got  %s\n  want %s", tc.sql, got, tc.want)
		}
	}
}

func TestAggFeedValueDynamics(t *testing.T) {
	db := aggFeedOpen(t)
	defer db.Close()
	for _, tc := range []struct{ sql, want string }{
		// REAL/TEXT/BLOB/NULL classification (registry sumStep mirror)
		{"SELECT SUM(r), COUNT(r), AVG(r), TOTAL(r) FROM tr",
			"SUM(r)|COUNT(r)|AVG(r)|TOTAL(r) ;; float64:+Inf,int64:9,float64:+Inf,float64:+Inf;"},
		{"SELECT SUM(s), COUNT(s) FROM tr",
			"SUM(s)|COUNT(s) ;; float64:1.8446744073709552e+19,int64:10;"},
		{"SELECT SUM(b), COUNT(b) FROM tr",
			"SUM(b)|COUNT(b) ;; float64:0,int64:8;"},
		{"SELECT SUM(n), COUNT(n), AVG(n) FROM tr",
			"ERR: integer overflow"},
		{"SELECT SUM(r) FROM tr WHERE id BETWEEN 2 AND 6",
			"SUM(r) ;; float64:1e+308;"},
		{"SELECT TOTAL(n) FROM tr WHERE id <= 3",
			"TOTAL(n) ;; float64:5;"},
		// NULL-heavy input
		{"SELECT SUM(c), COUNT(c), COUNT(*), AVG(c) FROM tnull WHERE id BETWEEN 3 AND 10",
			"SUM(c)|COUNT(c)|COUNT(*)|AVG(c) ;; int64:34,int64:5,int64:8,float64:6.8;"},
		// ALTER TABLE ADD COLUMN defaults on short records
		{"SELECT SUM(a), SUM(d), COUNT(e), COUNT(*) FROM tshort",
			"SUM(a)|SUM(d)|COUNT(e)|COUNT(*) ;; int64:6,int64:126,int64:3,int64:3;"},
		// declared rowid column shadows the pseudo-column
		{"SELECT SUM(rowid) FROM ts WHERE rowid BETWEEN 100 AND 400",
			"SUM(rowid) ;; int64:1000;"},
		// exact-int overflow promotes to the compensated double sum
		{"SELECT SUM(c) FROM tf WHERE id BETWEEN 3 AND 4",
			"SUM(c) ;; int64:-8589934592;"},
	} {
		if got := aggFeedQuery(t, db, tc.sql); got != tc.want {
			t.Errorf("%s\n  got  %s\n  want %s", tc.sql, got, tc.want)
		}
	}
}

// TestAggFeedOverflowTable pins the integer-overflow table (tf: two
// +2^32 and two -2^32 values): the exact int64 sum cancels to 0 while a
// half-range keeps the exact integer.
func TestAggFeedOverflowTable(t *testing.T) {
	db := aggFeedOpen(t)
	defer db.Close()
	for _, tc := range []struct{ sql, want string }{
		{"SELECT SUM(c), typeof(SUM(c)) FROM tf WHERE id BETWEEN 1 AND 2",
			"SUM(c)|typeof(SUM(c)) ;; int64:8589934592,string:integer;"},
		{"SELECT SUM(c) FROM tf",
			"SUM(c) ;; int64:0;"},
		{"SELECT TOTAL(c) FROM tf",
			"TOTAL(c) ;; float64:0;"},
	} {
		if got := aggFeedQuery(t, db, tc.sql); got != tc.want {
			t.Errorf("%s\n  got  %s\n  want %s", tc.sql, got, tc.want)
		}
	}
}

// TestAggFeedFallbackShapes pins shapes that must keep the generic aggregate
// path (expressions, MIN/MAX, DISTINCT, GROUP BY, HAVING, compound chains)
// and their exact outputs.
func TestAggFeedFallbackShapes(t *testing.T) {
	db := aggFeedOpen(t)
	defer db.Close()
	for _, tc := range []struct{ sql, want string }{
		{"SELECT SUM(c)+1, COUNT(*)*2 FROM t WHERE id BETWEEN 5 AND 20",
			"SUM(c)+1|COUNT(*)*2 ;; int64:-4199,int64:32;"},
		{"SELECT MIN(c), MAX(c) FROM t WHERE id BETWEEN 5 AND 20",
			"MIN(c)|MAX(c) ;; int64:-315,int64:-210;"},
		{"SELECT COUNT(DISTINCT c%10) FROM t WHERE id BETWEEN 5 AND 20",
			"COUNT(c % 10) ;; int64:10;"},
		{"SELECT c%2, COUNT(*) FROM t WHERE id BETWEEN 5 AND 20 GROUP BY c%2 ORDER BY 1",
			"c%2|COUNT(*) ;; int64:-1,int64:8;int64:0,int64:8;"},
		{"SELECT COUNT(*) FROM t WHERE id BETWEEN 5 AND 20 HAVING COUNT(*) > 3",
			"COUNT(*) ;; int64:16;"},
		{"SELECT SUM(c) FROM t WHERE id BETWEEN 5 AND 20 UNION ALL SELECT 1",
			"SUM(c) ;; int64:-4200;int64:1;"},
		// WITHOUT ROWID table: the feed declines, results still exact
		{"SELECT COUNT(*) FROM (SELECT c FROM t WHERE id BETWEEN 5 AND 20) sub",
			"COUNT(*) ;; int64:16;"},
	} {
		if got := aggFeedQuery(t, db, tc.sql); got != tc.want {
			t.Errorf("%s\n  got  %s\n  want %s", tc.sql, got, tc.want)
		}
	}
}
