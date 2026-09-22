package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestAnalyzePartialIndexPin pins ANALYZE/partial-index planner contracts
// (oracle: sqlite3 3.54; TCL refs index6-1.10..1.12, index6-2.1/2.2,
// index7-1.10, index7-2.1/2.2, analyze7-2.3/3.2.1/3.6).
//
// 1. sqlite_stat1 gets a NULL-idx row for a table whose indexes are ALL
// partial (analyze.c needTableCnt), and each partial index's stat counts only
// rows satisfying its WHERE clause — including WITHOUT ROWID tables, whose
// records store PK columns first.
// 2. A partial index is used when the query WHERE implies the partial
// predicate ("a=5" implies "a IS NOT NULL").
// 3. The EQP SEARCH detail lists only constraints on the chosen index's own
// columns, bind parameters count as constraints, a fully constrained index
// prefix seeks directly (no skip-scan ANY()), and a WITHOUT ROWID table's
// index scan is COVERING.
func TestAnalyzePartialIndexPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t1(a,b,c)",
		"CREATE INDEX t1a ON t1(a) WHERE a IS NOT NULL",
		"CREATE INDEX t1b ON t1(b) WHERE b>10",
		"INSERT INTO t1(a,b,c) SELECT CASE WHEN value%3!=0 THEN value END, value, value FROM generate_series(1,20)",
		"CREATE TABLE w1(a,b PRIMARY KEY) WITHOUT ROWID",
		"CREATE INDEX w1a ON w1(a) WHERE a IS NOT NULL",
		"CREATE INDEX w1b ON w1(b) WHERE b>10",
		"INSERT INTO w1(a,b) SELECT CASE WHEN value%3!=0 THEN value END, value FROM generate_series(1,20)",
		"CREATE TABLE t2(a,b)",
		"INSERT INTO t2(a,b) SELECT value, value FROM generate_series(1,1000)",
		"UPDATE t2 SET a=NULL WHERE b%2==0",
		"CREATE INDEX t2a1 ON t2(a) WHERE a IS NOT NULL",
		"CREATE TABLE t3(c,d,b)",
		"CREATE INDEX t3cd ON t3(c,d)",
		"INSERT INTO t3 SELECT value, value, value FROM generate_series(1,100)",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	rowsText := func(q string) string {
		t.Helper()
		r := db.Query(q)
		if r.Error != nil {
			t.Fatalf("%s => ERR %v", q, r.Error)
		}
		var sb strings.Builder
		for _, row := range r.Rows {
			for j, v := range row {
				if j > 0 {
					sb.WriteByte(' ')
				}
				if v == nil {
					sb.WriteString("{}")
					continue
				}
				sb.WriteString(fmt.Sprintf("%v", v))
			}
			sb.WriteByte('|')
		}
		return sb.String()
	}

	if r := db.Exec("ANALYZE"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// index6-1.10 (rowid): the NULL-idx table row is present; index7-1.10
	// (WITHOUT ROWID): the table row is the PRIMARY KEY row named after the
	// table. Partial index rows are counted over indexed rows only.
	for _, tc := range []struct{ q, want string }{
		{"SELECT idx, stat FROM sqlite_stat1 WHERE tbl='t1' ORDER BY idx",
			"{} 20|t1a 14 1|t1b 10 1|"},
		{"SELECT idx, stat FROM sqlite_stat1 WHERE tbl='w1' ORDER BY idx",
			"w1 20 1|w1a 14 1|w1b 10 1|"},
	} {
		if got := rowsText(tc.q); got != tc.want {
			t.Errorf("%s => got [%s] want [%s]", tc.q, got, tc.want)
		}
	}

	plans := map[string]string{
		// index6-2.2: a=5 implies "a IS NOT NULL" -> partial index used.
		"EXPLAIN QUERY PLAN SELECT * FROM t2 WHERE a=5": "SEARCH t2 USING INDEX t2a1 (a=?)",
		// analyze7-3.2.1: bind parameters drive index searches.
		"EXPLAIN QUERY PLAN SELECT * FROM t3 WHERE c=?1": "SEARCH t3 USING INDEX t3cd",
		// analyze7-3.6: fully constrained prefix seeks directly (no ANY()).
		"EXPLAIN QUERY PLAN SELECT * FROM t3 WHERE c=123 AND d=123 AND b=123": "(c=? AND d=?)",
	}
	for q, substr := range plans {
		if got := rowsText(q); !strings.Contains(got, substr) {
			t.Errorf("%s => got [%s] want substring [%s]", q, got, substr)
		}
	}
	// index7-2.2: WITHOUT ROWID partial index scan is COVERING.
	if got := rowsText("EXPLAIN QUERY PLAN SELECT * FROM w1 WHERE a=7"); !strings.Contains(got, "COVERING INDEX w1a") {
		t.Errorf("wr covering => got [%s] want substring [COVERING INDEX w1a]", got)
	}
	// analyze7-2.3: detail omits constraints on columns outside the index.
	if got := rowsText("EXPLAIN QUERY PLAN SELECT * FROM t3 WHERE c=5 AND b=6"); strings.Contains(got, "b=?") {
		t.Errorf("non-index constraint listed: [%s]", got)
	}
}
