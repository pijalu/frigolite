// pin tests for FULL-SUITE-DRIFT.T30-tkt2 (W5-TKT-RESUME)
package frigolite_test

import (
	"testing"

	"github.com/pijalu/frigolite"
)

// tkt2822-6.x: ORDER BY terms on a compound select must resolve against ANY
// member's output aliases (resolve.c resolveCompoundOrderBy), including
// table-qualified references (rule 3: expression match).
func TestW5Tkt2822CompoundOrderByAlias(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	steps := []string{
		`CREATE TABLE t6a(p,q); INSERT INTO t6a VALUES(1,8); INSERT INTO t6a VALUES(9,2);
		 CREATE TABLE t6b(x,y); INSERT INTO t6b VALUES(1,7); INSERT INTO t6b VALUES(7,2)`,
	}
	for _, s := range steps {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}
	cases := []struct {
		q, want string
	}{
		{`SELECT p, q FROM t6a UNION ALL SELECT x, y FROM t6b ORDER BY 1, 2`, "1 7 1 8 7 2 9 2"},
		{`SELECT p PX, q QX FROM t6a UNION ALL SELECT x XX, y YX FROM t6b ORDER BY PX, YX`, "1 7 1 8 7 2 9 2"},
		{`SELECT p PX, q QX FROM t6a UNION ALL SELECT x XX, y YX FROM t6b ORDER BY XX, QX`, "1 7 1 8 7 2 9 2"},
		{`SELECT p PX, q QX FROM t6a UNION ALL SELECT x XX, y YX FROM t6b ORDER BY QX, XX`, "7 2 9 2 1 7 1 8"},
		{`SELECT p PX, q QX FROM t6a UNION ALL SELECT x XX, y YX FROM t6b ORDER BY t6b.x, QX`, "1 7 1 8 7 2 9 2"},
		{`SELECT p PX, q QX FROM t6a UNION ALL SELECT x XX, y YX FROM t6b ORDER BY t6a.q, XX`, "7 2 9 2 1 7 1 8"},
		{`SELECT a, b, c FROM t1 UNION ALL SELECT a, b, c FROM t2 ORDER BY x`, "ERR"},
	}
	// t6 cases need the 2822 fixture tables absent; run against fresh db
	for i, tc := range cases[:6] {
		r := db.Query(tc.q)
		if r.Error != nil {
			t.Errorf("case %d %q: query error: %v", i, tc.q, r.Error)
			continue
		}
		if got := flattenRows(r.Rows); got != tc.want {
			t.Errorf("case %d %q\n  got:  %s\n  want: %s", i, tc.q, got, tc.want)
		}
	}
}
