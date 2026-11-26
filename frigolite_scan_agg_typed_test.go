package frigolite

import (
	"fmt"
	"testing"
)

// TestScanAggTypedFeed pins the engine-visible result of simple aggregates over
// a full table scan (no WHERE). This is the scan counterpart of the covered
// rowid-range typed feed: SUM/COUNT/AVG/TOTAL/MIN/MAX over integer, REAL,
// text-with-numeric-content, NULL, ALTER-added-default and rowid-alias inputs
// must match SQLite 3.54 regardless of which internal feed lane serves them.
// It is the pure-Go discriminator required before wiring the unboxed lane into
// the scan path (GUIDELINES §1a) — asserting values, never the plan.
func TestScanAggTypedFeed(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	exec("CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)")
	for i := 1; i <= 200; i++ {
		exec(fmt.Sprintf("INSERT INTO t(id,c) VALUES(%d,%d)", i, i*7-350))
	}
	// Numeric dynamics the typed accumulator must classify exactly like the
	// boxed Step: NULL skip, text->numeric, REAL, large ints, negative ints.
	// (BLOB-to-numeric coercion is a separate SUM-affinity gap, not this lane.)
	exec("CREATE TABLE d(id INTEGER PRIMARY KEY, v)")
	for _, row := range []string{
		"(1, 10)", "(2, -5)", "(3, NULL)", "(4, '12')", "(5, 2.5)",
		"(6, 9223372036854775807)", "(7, -9223372036854775808)",
		"(8, '3e2')", "(9, 0)",
	} {
		exec("INSERT INTO d(id,v) VALUES " + row)
	}
	// ALTER-added defaults: rows predating the column read the default.
	exec("CREATE TABLE s(id INTEGER PRIMARY KEY, a INTEGER)")
	exec("INSERT INTO s(id,a) VALUES(1,1),(2,2),(3,3)")
	exec("ALTER TABLE s ADD COLUMN d INTEGER DEFAULT 42")
	// NULL column values skip (a declared rowid-alias substitutes the rowid;
	// an ordinary column like v keeps its stored NULL).
	exec("CREATE TABLE ipk(id INTEGER PRIMARY KEY, v INTEGER)")
	exec("INSERT INTO ipk(id,v) VALUES(1,NULL),(2,5),(3,NULL)")

	cases := []struct{ sql, want string }{
		{"SELECT SUM(c), COUNT(*), AVG(c), TOTAL(c) FROM t",
			"SUM(c)|COUNT(*)|AVG(c)|TOTAL(c) ;; int64:70700,int64:200,float64:353.5,float64:70700;"},
		{"SELECT COUNT(v), SUM(v), TOTAL(v) FROM d",
			"COUNT(v)|SUM(v)|TOTAL(v) ;; int64:8,float64:318.5,float64:318.5;"},
		{"SELECT SUM(a), SUM(d), COUNT(d) FROM s",
			"SUM(a)|SUM(d)|COUNT(d) ;; int64:6,int64:126,int64:3;"},
		{"SELECT SUM(id), COUNT(id), SUM(v) FROM ipk",
			"SUM(id)|COUNT(id)|SUM(v) ;; int64:6,int64:3,int64:5;"},
		{"SELECT MIN(c), MAX(c), COUNT(c) FROM t",
			"MIN(c)|MAX(c)|COUNT(c) ;; int64:-343,int64:1050,int64:200;"},
	}
	for _, tc := range cases {
		if got := aggFeedQuery(t, db, tc.sql); got != tc.want {
			t.Errorf("%s\n  got  %s\n  want %s", tc.sql, got, tc.want)
		}
	}
}
