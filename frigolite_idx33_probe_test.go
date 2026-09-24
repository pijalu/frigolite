package frigolite_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// Probe for index-14.3: WHERE on second column of (a,b) index must table-scan.
func TestProbeIdx33SecondColConstraint(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	steps := []string{
		"CREATE TABLE t6(a,b,c)",
		"CREATE INDEX t6i1 ON t6(a,b)",
		"INSERT INTO t6 VALUES('','',1)",
		"INSERT INTO t6 VALUES('',NULL,2)",
		"INSERT INTO t6 VALUES(NULL,'',3)",
		"INSERT INTO t6 VALUES('abc',123,4)",
		"INSERT INTO t6 VALUES(123,'abc',5)",
	}
	for _, s := range steps {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
	for _, q := range []struct {
		sql  string
		want string
	}{
		{"SELECT c FROM t6 ORDER BY a,b", "3 5 2 1 4"},
		{"SELECT c FROM t6 WHERE b=''", "1 3"},
		{"SELECT c FROM t6 WHERE a=''", "2 1"},
	} {
		r := db.Query(q.sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", q.sql, r.Error)
		}
		var parts []string
		for _, row := range r.Rows {
			parts = append(parts, fmt.Sprintf("%v", row[0]))
		}
		got := strings.Join(parts, " ")
		if got != q.want {
			t.Errorf("%s: got [%s] want [%s]", q.sql, got, q.want)
		}
		eq := db.Query("EXPLAIN QUERY PLAN " + q.sql)
		if eq.Error != nil {
			t.Fatalf("eqp: %v", eq.Error)
		}
		for _, row := range eq.Rows {
			t.Logf("EQP %s => %s", q.sql, fmt.Sprintf("%v", row[len(row)-1]))
		}
	}
}
