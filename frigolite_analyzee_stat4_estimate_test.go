package frigolite

import (
	"strings"
	"testing"
)

func TestAnalyzeEStat4ChoosesScanForBroadRange(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t1(a,b,c)",
		"WITH RECURSIVE cnt(x) AS (VALUES(1000) UNION ALL SELECT x+1 FROM cnt WHERE x<2000) INSERT INTO t1(a,b,c) SELECT x,x,123 FROM cnt",
		"CREATE INDEX t1ca ON t1(c,a)",
		"ANALYZE",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("exec %q: %v", stmt, res.Error)
		}
	}
	for _, tc := range []struct {
		where     string
		wantIndex bool
	}{
		{"a BETWEEN 500 AND 2500 AND c=123", false},
		{"a BETWEEN 1700 AND 1750 AND c=123", true},
	} {
		res := db.Query("EXPLAIN QUERY PLAN SELECT * FROM t1 WHERE " + tc.where)
		if res.Error != nil {
			t.Fatalf("EXPLAIN %s: %v", tc.where, res.Error)
		}
		plan := flatten(res)
		usesIndex := strings.Contains(plan, "USING INDEX t1ca")
		if usesIndex != tc.wantIndex {
			t.Errorf("plan for %q = %s, want uses index %v", tc.where, plan, tc.wantIndex)
		}
	}
}
