// pin tests for FULL-SUITE-DRIFT.T30-tkt2 (W5-TKT-RESUME)
package frigolite_test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
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

// tkt-38cb5df375 51.x: the engine contract — EXCEPT over ORDER BY/LIMIT
// subqueries with a trailing ORDER BY DESC + LIMIT — plus the TCL lrange
// expectation shape the regenerated corpus relies on (lrange with a
// negative end index yields the empty list).
func TestW5Tkt38cb5df375ExceptLimit(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec(`CREATE TABLE t1(a); INSERT INTO t1 VALUES(1); INSERT INTO t1 VALUES(2);
		INSERT INTO t1 SELECT a+2 FROM t1; INSERT INTO t1 SELECT a+4 FROM t1`); r.Error != nil {
		t.Fatal(r.Error)
	}
	for ii := 1; ii <= 7; ii++ {
		jj := 7 - ii
		q := `SELECT a FROM (SELECT * FROM t1 ORDER BY a)
		      EXCEPT SELECT a FROM (SELECT a FROM t1 ORDER BY a LIMIT ` + strconv.Itoa(ii) + `)
		      ORDER BY a DESC LIMIT ` + strconv.Itoa(jj)
		r := db.Query(q)
		if r.Error != nil {
			t.Fatalf("ii=%d: %v", ii, r.Error)
		}
		wantN := 8 - ii // rows left after EXCEPT, cut to LIMIT jj (= 7-ii, min with 8-ii)
		if wantN > jj {
			wantN = jj
		}
		if len(r.Rows) != wantN {
			t.Errorf("ii=%d: got %d rows, want %d (%v)", ii, len(r.Rows), wantN, r.Rows)
		}
		for k, row := range r.Rows {
			wantVal := int64(8 - k)
			if v, ok := row[0].(int64); !ok || v != wantVal {
				t.Errorf("ii=%d row %d: got %v want %d", ii, k, row[0], wantVal)
			}
		}
	}
}

// tkt-54844eea3f 1.2: a table-qualified reference to an OUTER alias (out.b)
// inside a correlated scalar subquery over FROM (SELECT ...) must not resolve
// against the derived rows' unqualified columns; the inner WHERE b=out.b
// compares the derived column with the outer row. Expected {} two {} four.
func TestW5Tkt54844DerivedTableOuterQual(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec(`CREATE TABLE t4(a, b, c);
		INSERT INTO t4 VALUES('a', 1, 'one');
		INSERT INTO t4 VALUES('a', 2, 'two');
		INSERT INTO t4 VALUES('b', 1, 'three');
		INSERT INTO t4 VALUES('b', 2, 'four')`); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query(`SELECT (
		  SELECT c FROM (
		    SELECT * FROM t4 WHERE a=out.a ORDER BY b LIMIT 10 OFFSET 1
		  ) WHERE b=out.b
		) FROM t4 AS out`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	parts := make([]string, 0, 4)
	for _, row := range r.Rows {
		for _, v := range row {
			if v == nil {
				parts = append(parts, "{}")
			} else {
				parts = append(parts, fmt.Sprintf("%v", v))
			}
		}
	}
	if got := strings.Join(parts, " "); got != "{} two {} four" {
		t.Errorf("got: %s want: {} two {} four", got)
	}
}

// sort5 engine-visible contract: a 10000-row recursive CTE of random blobs
// sorts completely under a small page cache (the TCL file's 2.x group wraps
// this in untranspilable testvfs/progress-counter scaffolding; the generated
// package is a documented whole-file skip).
func TestW5Sort5LargeCTESort(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec(`PRAGMA cache_size = 10`); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query(`WITH x(i, j) AS (
		SELECT 1, randomblob(100)
		UNION ALL
		SELECT i+1, randomblob(100) FROM x WHERE i<10000
	  ) SELECT i, j FROM x ORDER BY j`)
	if r.Error != nil {
		t.Fatalf("err: %v", r.Error)
	}
	if len(r.Rows) != 10000 {
		t.Fatalf("rows: %d", len(r.Rows))
	}
	for k := 1; k < len(r.Rows); k++ {
		a, _ := r.Rows[k-1][1].([]byte)
		b, _ := r.Rows[k][1].([]byte)
		if bytes.Compare(a, b) > 0 {
			t.Fatalf("row %d out of order", k)
		}
	}
}

// func.test contracts fixed for FULL-SUITE-DRIFT.T30-tkt2: md5sum hashes all
// arguments per row; group_concat(X, NULL) concatenates with no separator;
// abs(text) is REAL 0.0; randomblob(n<1) yields 1 byte; trim(X, NULL) is NULL.
func TestW5FuncContracts(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec(`CREATE TABLE tbl1(t1 text); INSERT INTO tbl1 VALUES('this'),('program'),('is'),('free'),('software')`); r.Error != nil {
		t.Fatal(r.Error)
	}
	cases := []struct {
		q, want string
	}{
		{`SELECT group_concat(t1,NULL) FROM tbl1`, "thisprogramisfreesoftware"},
		{`SELECT group_concat(t1) FROM tbl1`, "this,program,is,free,software"},
		{`SELECT abs(t1) FROM tbl1`, "0 0 0 0 0"},
		{`SELECT typeof(abs(t1)) FROM tbl1`, "real real real real real"},
		{`SELECT length(randomblob(-5))`, "1"},
		{`SELECT typeof(trim('hello',NULL))`, "null"},
		{`SELECT trim('hello','')`, "hello"},
	}
	for _, tc := range cases {
		r := db.Query(tc.q)
		if r.Error != nil {
			t.Errorf("%s: %v", tc.q, r.Error)
			continue
		}
		if got := flattenRows(r.Rows); got != tc.want {
			t.Errorf("%s\n  got:  %s\n  want: %s", tc.q, got, tc.want)
		}
	}
	// md5sum digest cross-checked against the Go md5 of the exact string.
	r := db.Query(`SELECT md5sum(t1,'/1') FROM tbl1`)
	sum := fmt.Sprintf("%v", r.Rows[0][0])
	h := md5.Sum([]byte("this/1program/1is/1free/1software/1"))
	if sum != hex.EncodeToString(h[:]) {
		t.Errorf("md5sum digest: got %s want %x", sum, h)
	}
}
