package frigolite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestBtree01LargeOverflowWithoutRowidLeftJoin pins interior-index traversal
// when a WITHOUT ROWID table is materialized as a LEFT JOIN's right operand.
func TestBtree01LargeOverflowWithoutRowidLeftJoin(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "btree01.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{
		"PRAGMA page_size=1024",
		"CREATE TABLE t1(a INT PRIMARY KEY, b BLOB, c INT) WITHOUT ROWID",
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<100) INSERT INTO t1(a,b,c) SELECT x*2, zeroblob(100), x FROM c",
		"UPDATE t1 SET b=zeroblob(1000) WHERE a=198",
		"CREATE TABLE t2(x INTEGER PRIMARY KEY, y INT)",
		"INSERT INTO t2(y) VALUES(198),(187),(100)",
	} {
		if result := db.Exec(sql); result.Error != nil {
			t.Fatalf("%s: %v", sql, result.Error)
		}
	}
	result := db.Query("SELECT y, c FROM t2 LEFT JOIN t1 ON y=a ORDER BY x")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	var rows []string
	for _, row := range result.Rows {
		rows = append(rows, fmt.Sprint(row[0], " ", row[1]))
	}
	if got, want := strings.Join(rows, " | "), "198 99 | 187 <nil> | 100 50"; got != want {
		t.Fatalf("rows = %q, want %q", got, want)
	}
}
