package frigolite

import (
	"fmt"
	"testing"
)

func TestAggnestedPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t1(a1 INTEGER)",
		"INSERT INTO t1 VALUES(1),(2),(3)",
		"CREATE TABLE t2(b1 INTEGER)",
		"INSERT INTO t2 VALUES(4),(5)",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	for _, tc := range []struct {
		q    string
		want string
	}{
		{"SELECT (SELECT string_agg(a1,'x') FROM t2) FROM t1", "1x2x3"},      // 1.1
		{"SELECT (SELECT string_agg(b1,a1) FROM t2) FROM t1", "415 425 435"}, // 1.3
	} {
		r := db.Query(tc.q)
		if r.Error != nil {
			t.Errorf("%s => ERR %v", tc.q, r.Error)
			continue
		}
		got := aggPinFlatten(r.Rows)
		if got != tc.want {
			t.Errorf("%s => got [%s] want [%s]", tc.q, got, tc.want)
		}
	}

	// filter1-6.x (separate schema, mirrors upstream filter1.test 6.0)
	db2, err := Open(t.TempDir() + "/f.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	for _, s := range []string{
		"CREATE TABLE t1(a,b)",
		"INSERT INTO t1 VALUES(1,1),(2,2)",
		"CREATE TABLE t2(x,y)",
		"INSERT INTO t2 VALUES(1,1)",
	} {
		if r := db2.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	for _, tc := range []struct {
		q    string
		want string
	}{
		{"SELECT (SELECT COUNT(a) FILTER(WHERE x) FROM t2) FROM t1", "1 1"}, // 6.1
		{"SELECT (SELECT COUNT(a+x) FROM t2) FROM t1", "1 1"},               // 6.2
		{"SELECT (SELECT COUNT(a) FROM t2) FROM t1", "2"},                   // 6.3
	} {
		r := db2.Query(tc.q)
		if r.Error != nil {
			t.Errorf("%s => ERR %v", tc.q, r.Error)
			continue
		}
		got := aggPinFlatten(r.Rows)
		if got != tc.want {
			t.Errorf("%s => got [%s] want [%s]", tc.q, got, tc.want)
		}
	}
}

// aggPinFlatten joins all row values with single spaces (testgen flatten).
func aggPinFlatten(rows [][]interface{}) string {
	out := ""
	for _, row := range rows {
		for _, v := range row {
			if out != "" {
				out += " "
			}
			if v == nil {
				out += "NULL"
			} else {
				out += fmt.Sprintf("%v", v)
			}
		}
	}
	return out
}

func TestAggnestedPin311(t *testing.T) {
	db, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t1(id1, value1)",
		"INSERT INTO t1 VALUES(4469,12),(4469,11),(4470,34)",
		"CREATE INDEX t1id1 ON t1(id1)",
		"CREATE TABLE t2 (value2)",
		"INSERT INTO t2 VALUES(12),(34),(34)",
		"INSERT INTO t2 SELECT value2 FROM t2",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	r := db.Query("SELECT max(value1), (SELECT count(*) FROM t2 WHERE value2=max(value1)) FROM t1 GROUP BY id1")
	if r.Error != nil {
		t.Fatalf("3.11: %v", r.Error)
	}
	if got, want := aggPinFlatten(r.Rows), "12 2 34 4"; got != want {
		t.Errorf("3.11: got [%s] want [%s]", got, want)
	}
}
