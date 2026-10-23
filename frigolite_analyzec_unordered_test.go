package frigolite

import (
	"strings"
	"testing"
)

func TestAnalyzeCUnorderedIndexExcludedFromSearchAndOrder(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t1(a,b,c)",
		"INSERT INTO t1 VALUES(1,2,3),(7,8,9),(4,5,6),(10,11,12),(4,8,12),(1,11,111)",
		"CREATE INDEX t1a ON t1(a)",
		"CREATE INDEX t1b ON t1(b)",
		"ANALYZE",
		"UPDATE sqlite_stat1 SET stat='12345 2 unordered' WHERE idx='t1a'",
		"ANALYZE sqlite_master",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("exec %q: %v", stmt, res.Error)
		}
	}
	equality := flatten(db.Query("EXPLAIN QUERY PLAN SELECT * FROM t1 WHERE a=4"))
	if !strings.Contains(equality, "USING INDEX t1a") {
		t.Errorf("unordered index should remain eligible for equality search: %s", equality)
	}
	for _, query := range []string{
		"EXPLAIN QUERY PLAN SELECT *, '#' FROM t1 WHERE a BETWEEN 3 AND 8 ORDER BY c",
		"EXPLAIN QUERY PLAN SELECT c FROM t1 ORDER BY a",
	} {
		res := db.Query(query)
		if res.Error != nil {
			t.Fatalf("query %q: %v", query, res.Error)
		}
		plan := flatten(res)
		if strings.Contains(plan, "USING INDEX t1a") || strings.Contains(plan, "SCAN t1 USING INDEX t1a") {
			t.Errorf("unordered index used by plan %q: %s", query, plan)
		}
		if strings.Contains(query, "ORDER BY a") && !strings.Contains(plan, "B-TREE FOR ORDER BY") {
			t.Errorf("unordered index should require ORDER BY temp b-tree, plan: %s", plan)
		}
	}
}
