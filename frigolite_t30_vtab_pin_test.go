package frigolite_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
	"github.com/pijalu/frigolite/internal/vtab"
)

// t30Flat renders a result as the harness flatten() would ({} for NULL).
func t30Flat(t *testing.T, r *frigolite.Result) string {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	parts := make([]string, 0, 16)
	for _, row := range r.Rows {
		for _, v := range row {
			if v == nil {
				parts = append(parts, "{}")
				continue
			}
			parts = append(parts, fmt.Sprintf("%v", v))
		}
	}
	return strings.Join(parts, " ")
}

func t30MustExec(t *testing.T, db *frigolite.DB, sql string) {
	t.Helper()
	if err := db.Exec(sql).Error; err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func t30OpenEcho(t *testing.T) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.RegisterEchoModule()
	return db
}

// TestT30PinInsertSelectNullPK pins the INSERT ... SELECT fix: a generated
// rowid must never leak into a non-INTEGER PRIMARY KEY column (plain
// `a PRIMARY KEY` is an ordinary unique column, so a NULL stays NULL).
// Oracle 3.54: SELECT rowid, quote(a) shows NULL for both generated rows.
func TestT30PinInsertSelectNullPK(t *testing.T) {
	db := t30OpenEcho(t)
	defer db.Close()
	t30MustExec(t, db, `CREATE TABLE real_abc(a PRIMARY KEY, b, c)`)
	t30MustExec(t, db, `CREATE VIRTUAL TABLE echo_abc USING echo(real_abc)`)
	t30MustExec(t, db, `INSERT INTO echo_abc VALUES(1, 2, 3)`)
	t30MustExec(t, db, `INSERT INTO echo_abc(rowid) VALUES(31427)`)
	t30MustExec(t, db, `INSERT INTO echo_abc SELECT a||'.v2', b, c FROM echo_abc`)
	got := t30Flat(t, db.Query(`SELECT rowid, a, b, c FROM echo_abc`))
	want := "1 1 2 3 31427 {} {} {} 31428 1.v2 2 3 31429 {} {} {}"
	if got != want {
		t.Errorf("vtab1.7-5:\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// TestT30PinEchoBeginFail pins the echo module's xBegin veto
// (echo_module_begin_fail == source table): the INSERT fails with
// "SQL logic error" (bare SQLITE_ERROR) and NO row reaches the source table
// (test8.c echoBegin; oracle: {1 {SQL logic error}}, vtab1.10-3).
func TestT30PinEchoBeginFail(t *testing.T) {
	db := t30OpenEcho(t)
	defer db.Close()
	t30MustExec(t, db, `CREATE TABLE r(a, b, c)`)
	t30MustExec(t, db, `CREATE VIRTUAL TABLE e USING echo(r, e_log)`)
	vtab.TclVarSet("echo_module_begin_fail", "", "r")
	defer vtab.TclVarSet("echo_module_begin_fail", "", "")
	if err := db.Exec(`INSERT INTO e VALUES(1, 2, 3)`).Error; err == nil {
		t.Fatalf("expected echo xBegin veto error, got success")
	} else if !strings.Contains(err.Error(), "SQL logic error") {
		t.Errorf("expected \"SQL logic error\", got %q", err.Error())
	}
	if n := t30Flat(t, db.Query(`SELECT count(*) FROM e`)); n != "0" {
		t.Errorf("xBegin veto must write nothing, count=%s", n)
	}
}

// TestT30PinEchoNullNotIn pins vtab1-14.013: a NULL NOT IN (non-empty list)
// is unknown (NULL) on the vtab scan path too — the wrapped column value must
// count as NULL (oracle 3.54: only row (3,G,H) is returned).
func TestT30PinEchoNullNotIn(t *testing.T) {
	db := t30OpenEcho(t)
	defer db.Close()
	t30MustExec(t, db, `CREATE TABLE c(a UNIQUE, b, c2)`)
	t30MustExec(t, db, `INSERT INTO c VALUES(3,'G','H'),(NULL,15,16),(15,NULL,16)`)
	t30MustExec(t, db, `CREATE VIRTUAL TABLE echo_c USING echo(c)`)
	got := t30Flat(t, db.Query(`SELECT * FROM echo_c WHERE a NOT IN (1,8,'x',15,24)`))
	want := "3 G H"
	if got != want {
		t.Errorf("vtab1-14.013:\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// TestT30PinLowercaseNaturalJoin pins that NATURAL JOIN is case-insensitive
// (oracle 3.54: lowercase `natural join` is the same join as NATURAL JOIN —
// the keywords feed sqlite3JoinType case-insensitively).
func TestT30PinLowercaseNaturalJoin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t30MustExec(t, db, `
CREATE TABLE r1(a,b,c); CREATE TABLE r2(b,c,d); CREATE TABLE r3(c,d,e);
INSERT INTO r1 VALUES(1,2,3); INSERT INTO r1 VALUES(2,3,4); INSERT INTO r1 VALUES(3,4,5);
INSERT INTO r2 VALUES(1,2,3); INSERT INTO r2 VALUES(2,3,4); INSERT INTO r2 VALUES(3,4,5);
INSERT INTO r3 VALUES(2,3,4); INSERT INTO r3 VALUES(3,4,5); INSERT INTO r3 VALUES(4,5,6);`)
	two := t30Flat(t, db.Query(`SELECT * FROM r1 natural join r2`))
	if want := "1 2 3 4 2 3 4 5"; two != want {
		t.Errorf("lowercase 2-way natural join:\n  got:  [%s]\n  want: [%s]", two, want)
	}
	three := t30Flat(t, db.Query(`SELECT * FROM r1 natural join r2 natural join r3`))
	if want := "1 2 3 4 5 2 3 4 5 6"; three != want {
		t.Errorf("lowercase 3-way natural join:\n  got:  [%s]\n  want: [%s]", three, want)
	}
}

// TestT30PinOrderByOrdinalCollation pins vtab5-2.1/2.2: an ORDER BY ordinal
// sorts by the output column's declared collation (oracle 3.54: NOCASE
// order; also holds over an echo vtab mirroring the column).
func TestT30PinOrderByOrdinalCollation(t *testing.T) {
	db := t30OpenEcho(t)
	defer db.Close()
	t30MustExec(t, db, `CREATE TABLE strings(str COLLATE NOCASE)`)
	for _, v := range []string{"'abc1'", "'Abc3'", "'ABc2'", "'aBc4'"} {
		t30MustExec(t, db, `INSERT INTO strings VALUES(`+v+`)`)
	}
	want := "abc1 ABc2 Abc3 aBc4"
	if got := t30Flat(t, db.Query(`SELECT str FROM strings ORDER BY 1`)); got != want {
		t.Errorf("plain ORDER BY 1:\n  got:  [%s]\n  want: [%s]", got, want)
	}
	t30MustExec(t, db, `CREATE VIRTUAL TABLE echo_strings USING echo(strings)`)
	if got := t30Flat(t, db.Query(`SELECT str FROM echo_strings ORDER BY 1`)); got != want {
		t.Errorf("vtab ORDER BY 1:\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// TestT30PinMultiIndexOROrder pins vtabD-1.8's row order: an equality OR is
// answered by SQLite's MULTI-INDEX OR plan, whose rows emerge branch by
// branch (oracle 3.54 EXPLAIN: MULTI-INDEX OR over i1(a=?) then i2(b=?)).
func TestT30PinMultiIndexOROrder(t *testing.T) {
	db := t30OpenEcho(t)
	defer db.Close()
	t30MustExec(t, db, `CREATE TABLE t1(a, b); CREATE INDEX i1 ON t1(a); CREATE INDEX i2 ON t1(b)`)
	t30MustExec(t, db, `CREATE VIRTUAL TABLE tv1 USING echo(t1)`)
	for _, v := range []string{"(900,810000)", "(90001,8100180001)", "(1,1)", "(2,4)"} {
		t30MustExec(t, db, `INSERT INTO tv1 VALUES`+v)
	}
	got := t30Flat(t, db.Query(`SELECT * FROM tv1 WHERE a = 90001 OR b = 810000`))
	want := "90001 8100180001 900 810000"
	if got != want {
		t.Errorf("MULTI-INDEX OR row order:\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// TestT30PinSeriesStepZero pins tabfunc01-1370's CURRENT oracle behavior:
// series.c xFilter normalizes a zero STEP to 1, so generate_series(0,0,0)
// yields the single row 0 (sqlite3 3.54 verified; the TCL file's {} predates
// the normalization).
func TestT30PinSeriesStepZero(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got, want := t30Flat(t, db.Query(`SELECT * FROM generate_series(0,0,0)`)), "0"; got != want {
		t.Errorf("generate_series(0,0,0):\n  got:  [%s]\n  want: [%s]", got, want)
	}
	if got, want := t30Flat(t, db.Query(`SELECT * FROM generate_series(0,2,0)`)), "0 1 2"; got != want {
		t.Errorf("generate_series(0,2,0):\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// TestT30PinEchoIPKAlias pins vtab6-8.x: reading an INTEGER PRIMARY KEY
// column through an echo vtab yields the rowid (btree.c stores NULL for the
// alias; the echo module reads its source through SQL), so joins over such
// vtabs match like the plain tables do.
func TestT30PinEchoIPKAlias(t *testing.T) {
	db := t30OpenEcho(t)
	defer db.Close()
	t30MustExec(t, db, `CREATE TABLE real_t10(x INTEGER PRIMARY KEY, y); CREATE TABLE real_t11(p INTEGER PRIMARY KEY, q)`)
	t30MustExec(t, db, `CREATE VIRTUAL TABLE t10 USING echo(real_t10); CREATE VIRTUAL TABLE t11 USING echo(real_t11)`)
	t30MustExec(t, db, `INSERT INTO t10 VALUES(1,2); INSERT INTO t10 VALUES(3,3); INSERT INTO t11 VALUES(2,111); INSERT INTO t11 VALUES(3,333)`)
	if got, want := t30Flat(t, db.Query(`SELECT x, q FROM t10, t11 WHERE t10.y=t11.p`)), "1 111 3 333"; got != want {
		t.Errorf("view join over echo vtabs with IPK columns:\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// TestT30PinCSVBlobAffinity pins csv01-2.3/2.4: csv fields stay TEXT
// (csvtabColumn uses sqlite3_result_text) and the declared schema drives
// affinity — d BLOB never numerically coerces, so d=12 matches nothing while
// d='12' matches (oracle 3.54-identical behavior).
func TestT30PinCSVBlobAffinity(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t30MustExec(t, db, "CREATE VIRTUAL TABLE temp.t2 USING csv(data='9,10,11,12\n', columns=4, schema='CREATE TABLE t2(a INT, b TEXT, c REAL, d BLOB)')")
	if got := t30Flat(t, db.Query(`SELECT * FROM temp.t2 WHERE d=12`)); got != "" {
		t.Errorf("d=12 must match nothing (BLOB affinity):\n  got:  [%s]", got)
	}
	if got, want := t30Flat(t, db.Query(`SELECT * FROM temp.t2 WHERE d='12'`)), "9 10 11 12"; got != want {
		t.Errorf("d='12' must match:\n  got:  [%s]\n  want: [%s]", got, want)
	}
	if got, want := t30Flat(t, db.Query(`SELECT * FROM temp.t2 WHERE a=9`)), "9 10 11 12"; got != want {
		t.Errorf("a=9 (INT affinity) must match:\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// TestT30PinRTreeNonNumericConstraints pins rtree1-8.x/14.7: a NULL or
// non-numeric comparison operand against an rtree column rewrites the
// constraint per rtree.c xFilter (NULL → RTREE_FALSE; non-numeric text →
// RTREE_TRUE for < / <=, RTREE_FALSE otherwise), and a non-integral float
// rowid equality matches nothing (sqlite3IntFloatCompare tail).
func TestT30PinRTreeNonNumericConstraints(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t30MustExec(t, db, `CREATE VIRTUAL TABLE t6 USING rtree(ii, x1, x2); INSERT INTO t6 VALUES(1, 3, 7); INSERT INTO t6 VALUES(2, 4, 6)`)
	for _, q := range []string{
		`SELECT ii FROM t6 WHERE x1>''`,
		`SELECT ii FROM t6 WHERE x1>null`,
		`SELECT ii FROM t6 WHERE x1>=''`,
		`SELECT ii FROM t6 WHERE x1>=null`,
		`SELECT ii FROM t6 WHERE x1<null`,
		`SELECT ii FROM t6 WHERE x1<=null`,
	} {
		if got := t30Flat(t, db.Query(q)); got != "" {
			t.Errorf("%s must match nothing:\n  got:  [%s]", q, got)
		}
	}
	if got, want := t30Flat(t, db.Query(`SELECT ii FROM t6 WHERE x1>='4'`)), "2"; got != want {
		t.Errorf("x1>='4' (numeric text):\n  got:  [%s]\n  want: [%s]", got, want)
	}
	// rtree1-24.2: a non-integral float rowid equality matches nothing.
	t30MustExec(t, db, `CREATE VIRTUAL TABLE rt1 USING rtree_i32(rid, c1, c2); INSERT INTO rt1(rid, c1, c2) VALUES(1,2,3)`)
	if got := t30Flat(t, db.Query(`SELECT * FROM rt1 WHERE rid=1.005`)); got != "" {
		t.Errorf("rtree_i32 rid=1.005 must match nothing:\n  got:  [%s]", got)
	}
	if got, want := t30Flat(t, db.Query(`SELECT * FROM rt1 WHERE rid=1`)), "1 2 3"; got != want {
		t.Errorf("rtree_i32 rid=1:\n  got:  [%s]\n  want: [%s]", got, want)
	}
}
