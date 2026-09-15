package frigolite_test

import (
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteEchoVtabJoinPin pins tkt3121: two echo virtual tables (proxies
// for shadow tables r1/r2) must join like ordinary tables — the vtab join
// materialization mirrors the source table's rows and columns.
func TestSQLiteEchoVtabJoinPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	mustExec("CREATE TABLE r1(field)")
	mustExec("CREATE TABLE r2(col PRIMARY KEY, descr)")
	mustExec("INSERT INTO r1 VALUES('abcd')")
	mustExec("INSERT INTO r2 VALUES('abcd', 'A nice description')")
	mustExec("INSERT INTO r2 VALUES('efgh', 'Another description')")
	mustExec("CREATE VIRTUAL TABLE t1 USING echo(r1)")
	mustExec("CREATE VIRTUAL TABLE t2 USING echo(r2)")

	r := db.Query("SELECT t1.field, t2.descr FROM t1 INNER JOIN t2 ON t1.field = t2.col ORDER BY t1.field")
	if r.Error != nil {
		t.Fatalf("echo vtab join: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "abcd" || r.Rows[0][1] != "A nice description" {
		t.Fatalf("got %v, want [[abcd A nice description]]", r.Rows)
	}
}
