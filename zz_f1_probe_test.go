package frigolite

import "testing"

func TestZZF1Probe(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t1(a,b); INSERT INTO t1 VALUES(1,1); INSERT INTO t1 VALUES(2,2);",
		"CREATE TABLE t2(x,y); INSERT INTO t2 VALUES(1,1);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	for _, q := range []string{
		"SELECT (SELECT COUNT(a) FILTER(WHERE x) FROM t2) FROM t1",
		"SELECT (SELECT COUNT(a) FROM t2) FROM t1",
		"SELECT (SELECT COUNT(x) FILTER(WHERE x) FROM t2) FROM t1",
		"SELECT (SELECT SUM(a) FILTER(WHERE x) FROM t2) FROM t1",
	} {
		r := db.Query(q)
		t.Logf("RES %s -> rows=%v err=%v", q, r.Rows, r.Error)
	}
}
