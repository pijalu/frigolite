package frigolite

import (
	"strings"
	"testing"
)

func TestAggregateMisuseAcrossCompoundDerivedTable(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	result := db.Exec("CREATE TABLE t0(c1, c2); INSERT INTO t0 VALUES(1,2); CREATE TABLE t1(c1); INSERT INTO t1 VALUES(11); CREATE TABLE t2(c2); INSERT INTO t2 VALUES(1); CREATE TABLE t3(c3, c4); INSERT INTO t3 VALUES(3,4)")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	valid := db.Query(`SELECT (SELECT 1 FROM (SELECT sum(c1))) FROM t1`)
	if valid.Error != nil {
		t.Fatalf("valid non-compound aggregate query: %v", valid.Error)
	}
	invalidQueries := []string{
		`SELECT * FROM t0 WHERE EXISTS (SELECT 1 FROM t3 GROUP BY c3 HAVING (SELECT count(*) FROM (SELECT 1 UNION ALL SELECT sum(DISTINCT c1))) BETWEEN 1 AND 1)`,
		`SELECT * FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE (SELECT COUNT(*) FROM (SELECT 1 UNION ALL SELECT SUM(c1))))`,
		`SELECT * FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE (SELECT COUNT(*) FROM (SELECT 1 WHERE true UNION ALL SELECT SUM(c1))))`,
	}
	for _, query := range invalidQueries {
		result = db.Query(query)
		if result.Error == nil || !strings.Contains(strings.ToLower(result.Error.Error()), "misuse of aggregate") {
			t.Errorf("query error = %v, want misuse of aggregate; SQL: %s", result.Error, query)
		}
	}
}
