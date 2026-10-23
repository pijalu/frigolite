package frigolite

import (
	"fmt"
	"testing"
)

func TestAnalyzeEEmptyTextBetweenIndexedBounds(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	for _, stmt := range []string{
		"PRAGMA encoding='UTF-16'",
		"CREATE TABLE t0(c1 TEXT)",
		"INSERT INTO t0 VALUES('')",
		"CREATE INDEX i0 ON t0(c1)",
		"ANALYZE",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("exec %q: %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT count(*) FROM t0 WHERE c1 BETWEEN '' AND ABS('')")
	if res.Error != nil {
		t.Fatalf("query: %v", res.Error)
	}
	if len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != "1" {
		t.Fatalf("empty TEXT BETWEEN count rows = %#v, want one row containing 1", res.Rows)
	}
}
