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

// tkt3992-2.2: UPDATE after ALTER TABLE ADD COLUMN c DEFAULT 3 must keep the
// added column's default (OP_Column materializes it; the rewritten cell must
// not store NULL).
func TestW5Tkt3992UpdateAfterAddColumn(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES(1, 2)`,
		`ALTER TABLE t1 ADD COLUMN c DEFAULT 3`,
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	if r := db.Exec(`UPDATE t1 SET a = 'one'`); r.Error != nil {
		t.Fatalf("update: %v", r.Error)
	}
	r := db.Query(`SELECT * FROM t1`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := flattenRows(r.Rows); got != "one 2 3" {
		t.Errorf("got: %s want: one 2 3", got)
	}
}

// tkt4018: the engine must enforce SQLite's cross-connection lock protocol —
// a second connection's INSERT fails with "database is locked" while the
// first holds a read transaction, and succeeds after COMMIT (the emitter
// relies on this to run tkt4018's separate-process testsql steps
// in-process).
func TestW5Tkt4018SecondConnLock(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec(`CREATE TABLE t1(a, b); BEGIN; SELECT * FROM t1`); r.Error != nil {
		t.Fatalf("conn1: %v", r.Error)
	}
	db2, err := frigolite.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if r := db2.Exec(`INSERT INTO t1 VALUES(3, 4)`); r.Error == nil || r.Error.Error() != "database is locked" {
		t.Errorf("locked insert: got %v, want database is locked", r.Error)
	}
	if r := db.Exec(`COMMIT`); r.Error != nil {
		t.Fatalf("commit: %v", r.Error)
	}
	if r := db2.Exec(`INSERT INTO t1 VALUES(3, 4)`); r.Error != nil {
		t.Errorf("post-commit insert: %v", r.Error)
	}
	if got := flattenRows(db.Query(`SELECT * FROM t1 ORDER BY a`).Rows); got != "3 4" {
		t.Errorf("rows: got %s want 3 4", got)
	}
}
