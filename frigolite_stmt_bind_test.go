package frigolite

// Prepared-statement bind tests: parameter binding of every value kind,
// literal-vs-bound result parity, type preservation through the clone
// (the template-cache kind gates), error surfaces, and the no-reparse
// property of the repeat path.

import (
	"fmt"
	"runtime"
	"testing"
)

func bindTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func bindExec(t *testing.T, db *DB, sql string) *Result {
	t.Helper()
	r := db.Exec(sql)
	if r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
	return r
}

// TestStmtBindAllTypes exercises every bindable Go kind and the storage type
// each lands as (typeof parity with the equivalent literal SQL).
func TestStmtBindAllTypes(t *testing.T) {
	db := bindTestDB(t)
	bindExec(t, db, "CREATE TABLE t(v)")

	st, err := db.Prepare("INSERT INTO t VALUES(?)")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, v := range []interface{}{
		int64(5), int(7), int32(9), uint64(11),
		8.0, "text", true, []byte{0xDE, 0xAD},
	} {
		if r := st.Exec(v); r.Error != nil {
			t.Fatalf("bind %T: %v", v, r.Error)
		}
	}
	// Cleared bindings bind as SQL NULL (sqlite3_clear_bindings then step).
	if err := st.ClearBindings(); err != nil {
		t.Fatal(err)
	}
	if r := st.Exec(); r.Error != nil {
		t.Fatal(r.Error)
	}
	st.Close()

	q := db.Query("SELECT typeof(v), quote(v) FROM t ORDER BY rowid")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	want := [][2]string{
		{"integer", "5"}, {"integer", "7"}, {"integer", "9"}, {"integer", "11"},
		{"real", "8.0"}, {"text", "'text'"}, {"integer", "1"},
		{"blob", "X'DEAD'"}, {"null", "NULL"},
	}
	if len(q.Rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(q.Rows), len(want), q.Rows)
	}
	for i, w := range want {
		gotKind, _ := q.Rows[i][0].(string)
		gotVal := fmt.Sprint(q.Rows[i][1])
		if gotKind != w[0] || gotVal != w[1] {
			t.Errorf("row %d: got %s|%s, want %s|%s", i, gotKind, gotVal, w[0], w[1])
		}
	}
}

// TestStmtBindNullModes covers the three NULL shapes: explicit nil argument,
// BindNull, and a parameter never bound at all.
func TestStmtBindNullModes(t *testing.T) {
	db := bindTestDB(t)
	bindExec(t, db, "CREATE TABLE t(a, b, c)")

	st, err := db.Prepare("INSERT INTO t VALUES(?, ?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BindNull(2); err != nil {
		t.Fatal(err)
	}
	if r := st.Exec(nil, nil, int64(3)); r.Error != nil {
		t.Fatal(r.Error)
	}
	st.Close()

	q := db.Query("SELECT a IS NULL, b IS NULL, c FROM t")
	if q.Error != nil || len(q.Rows) != 1 {
		t.Fatalf("query: %v %v", q.Error, q.Rows)
	}
	if got := q.Rows[0]; got[0] != int64(1) || got[1] != int64(1) || got[2] != int64(3) {
		t.Fatalf("null modes: %v", got)
	}
}

// TestStmtBindParameterForms covers ?NNN, :name, @name and $name via
// positional Exec arguments (slots follow resolve.c).
func TestStmtBindParameterForms(t *testing.T) {
	db := bindTestDB(t)
	cases := []struct {
		sql  string
		args []interface{}
		want interface{}
	}{
		{"?1, ?2", []interface{}{"a", "b"}, []interface{}{"a", "b"}},
		{"?2, ?1", []interface{}{"a", "b"}, []interface{}{"b", "a"}},
		{":a || '/' || :b", []interface{}{"x", "y"}, []interface{}{"x/y"}},
		{"@x + @y", []interface{}{int64(1), int64(2)}, []interface{}{int64(3)}},
		{"$p || '!'", []interface{}{"hi"}, []interface{}{"hi!"}},
		{"?3 || ?1", []interface{}{"r", "s", "t"}, []interface{}{"tr"}},
	}
	for i, c := range cases {
		st, err := db.Prepare("SELECT " + c.sql)
		if err != nil {
			t.Fatalf("case %d prepare: %v", i, err)
		}
		r := st.Query(c.args...)
		if r.Error != nil {
			t.Fatalf("case %d: %v", i, r.Error)
		}
		if got := fmt.Sprint(r.Rows[0]); got != fmt.Sprint(c.want) {
			t.Errorf("case %d (%s): got %v, want %v", i, c.sql, got, c.want)
		}
		st.Close()
	}
}

// TestStmtTypePreservation pins the clone kind gates: a bound 8.0 stays REAL
// and a bound 5 stays INTEGER, matching the literal equivalents.
func TestStmtTypePreservation(t *testing.T) {
	db := bindTestDB(t)
	bindExec(t, db, "CREATE TABLE a(v)")
	bindExec(t, db, "CREATE TABLE b(v)")

	st, err := db.Prepare("INSERT INTO a VALUES(?)")
	if err != nil {
		t.Fatal(err)
	}
	if r := st.Exec(8.0); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := st.Exec(int64(5)); r.Error != nil {
		t.Fatal(r.Error)
	}
	st.Close()

	// Literal equivalents through the text path.
	bindExec(t, db, "INSERT INTO b VALUES(8.0)")
	bindExec(t, db, "INSERT INTO b VALUES(5)")

	gotA := db.Query("SELECT typeof(v), v FROM a ORDER BY rowid")
	gotB := db.Query("SELECT typeof(v), v FROM b ORDER BY rowid")
	if fmt.Sprint(gotA.Rows) != fmt.Sprint(gotB.Rows) {
		t.Fatalf("bound %v != literal %v", gotA.Rows, gotB.Rows)
	}
}

// TestStmtQueryParityLiteral runs a battery of bound queries and asserts the
// rows equal the literal-text equivalents.
func TestStmtQueryParityLiteral(t *testing.T) {
	db := bindTestDB(t)
	bindExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, g int, s text)")
	st, _ := db.Prepare("INSERT INTO t VALUES(?, ?, ?)")
	for i := 1; i <= 50; i++ {
		if r := st.Exec(int64(i), int64(i%5), fmt.Sprintf("row%02d", i)); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	st.Close()

	queries := []struct {
		tmpl string
		lit  func(a interface{}) string
		arg  interface{}
	}{
		{"SELECT s FROM t WHERE id = ?", func(a interface{}) string {
			return fmt.Sprintf("SELECT s FROM t WHERE id = %d", a)
		}, int64(17)},
		{"SELECT count(*) FROM t WHERE g = ?", func(a interface{}) string {
			return fmt.Sprintf("SELECT count(*) FROM t WHERE g = %d", a)
		}, int64(3)},
		{"SELECT id FROM t WHERE s LIKE ? ORDER BY id LIMIT ?", nil, "%02d%"},
		{"SELECT id FROM t WHERE id IN (?, ?, ?) ORDER BY id", nil, nil},
		{"SELECT sum(g) FROM t WHERE id > ? AND id <= ?", nil, nil},
		{"SELECT upper(s) FROM t WHERE g = ? ORDER BY id DESC LIMIT 3", func(a interface{}) string {
			return fmt.Sprintf("SELECT upper(s) FROM t WHERE g = %d ORDER BY id DESC LIMIT 3", a)
		}, int64(2)},
	}
	for i, tc := range queries {
		st, err := db.Prepare(tc.tmpl)
		if err != nil {
			t.Fatalf("query %d prepare: %v", i, err)
		}
		var args []interface{}
		var litSQL string
		switch i {
		case 3:
			args, litSQL = []interface{}{int64(10), int64(20), int64(30)},
				"SELECT id FROM t WHERE id IN (10, 20, 30) ORDER BY id"
		case 4:
			args, litSQL = []interface{}{int64(5), int64(25)},
				"SELECT sum(g) FROM t WHERE id > 5 AND id <= 25"
		case 2:
			args = []interface{}{"%02d%", int64(4)}
			litSQL = "SELECT id FROM t WHERE s LIKE '%02d%' ORDER BY id LIMIT 4"
		default:
			args = []interface{}{tc.arg}
			litSQL = tc.lit(tc.arg)
		}
		bound := st.Query(args...)
		lit := db.Query(litSQL)
		if bound.Error != nil || lit.Error != nil {
			t.Fatalf("query %d: %v / %v", i, bound.Error, lit.Error)
		}
		if fmt.Sprint(bound.Rows) != fmt.Sprint(lit.Rows) {
			t.Errorf("query %d (%s): bound %v != literal %v", i, tc.tmpl, bound.Rows, lit.Rows)
		}
		st.Close()
	}
}

// TestStmtExecResultParity pins that Stmt.Exec surfaces the same Result
// fields (Changes, LastInsertRowID, Columns, Rows) as DB.Exec.
func TestStmtExecResultParity(t *testing.T) {
	db := bindTestDB(t)
	bindExec(t, db, "CREATE TABLE t1(a, b)")
	bindExec(t, db, "CREATE TABLE t2(a, b)")

	lit := bindExec(t, db, "INSERT INTO t1 VALUES(1, 'x')")
	st, err := db.Prepare("INSERT INTO t2 VALUES(?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bnd := st.Exec(int64(1), "x")
	if bnd.Error != nil {
		t.Fatal(bnd.Error)
	}
	if bnd.Changes != lit.Changes || bnd.LastInsertRowID != lit.LastInsertRowID {
		t.Fatalf("changes/rowid: bound %d/%d, literal %d/%d",
			bnd.Changes, bnd.LastInsertRowID, lit.Changes, lit.LastInsertRowID)
	}

	// SELECT via Exec carries rows and columns like DB.Exec.
	sq, _ := db.Prepare("SELECT a, b FROM t2 WHERE a = ?")
	defer sq.Close()
	r := sq.Exec(int64(1))
	if r.Error != nil || len(r.Rows) != 1 || r.Columns[0] != "a" {
		t.Fatalf("exec select: %v %v %v", r.Error, r.Columns, r.Rows)
	}
}

// TestStmtRepeatExecNoReparse asserts the no-reparse property: repeating a
// bound Exec stays within the literal-text equivalent's allocation range
// (which pays normalize + template lookup per distinct text but substitutes
// through the slot-path live clone — no walk, no clone allocation on a
// hit). A regression to the text-rendering re-parse would blow far past the
// literal path's allocation count.
func TestStmtRepeatExecNoReparse(t *testing.T) {
	db := bindTestDB(t)
	bindExec(t, db, "CREATE TABLE t(a INTEGER, b INTEGER, c TEXT)")

	st, err := db.Prepare("INSERT INTO t VALUES(?, ?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if r := st.Exec(int64(-1), int64(-1), "warm"); r.Error != nil {
		t.Fatal(r.Error)
	}
	bindExec(t, db, "INSERT INTO t VALUES(-2, -2, 'warm')")
	bindExec(t, db, "DELETE FROM t")

	const n = 4000
	bound := allocsPerOp(n)(func() {
		for i := 0; i < n; i++ {
			if r := st.Exec(int64(i), int64(i*2), fmt.Sprintf("x%d", i)); r.Error != nil {
				t.Fatal(r.Error)
			}
		}
	})
	lit := allocsPerOp(n)(func() {
		for i := 0; i < n; i++ {
			if r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d, %d, 'x%d')", i, i*2, i)); r.Error != nil {
				t.Fatal(r.Error)
			}
		}
	})
	if bound > lit+lit/10 {
		t.Fatalf("bound repeat allocs/op (%d) must stay within 10%% of literal (%d)", bound, lit)
	}
	t.Logf("allocs/op: bound=%d literal=%d", bound, lit)
}

// allocsPerOp builds a total-alloc-per-iteration counter for n iterations.
func allocsPerOp(n int) func(func()) int64 {
	return func(fn func()) int64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		fn()
		runtime.ReadMemStats(&after)
		return int64(after.TotalAlloc-before.TotalAlloc) / int64(n)
	}
}
