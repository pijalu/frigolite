package frigolite

import (
	"strings"
	"testing"
)

// Native tests for the carray table-valued function (ext carray.c port),
// written before any TCL/testgen validation per the triage protocol.

func TestNativeCarraySchema(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Unconstrained eponymous instance: no rows, no error (carray02-3.0).
	r := db.Query("SELECT * FROM carray")
	if r.Error != nil {
		t.Fatalf("FROM carray: %v", r.Error)
	}
	if len(r.Rows) != 0 {
		t.Fatalf("FROM carray: want 0 rows, got %v", r.Rows)
	}
	// Only "value" is visible: pointer/count/ctype are HIDDEN.
	if len(r.Columns) != 1 || r.Columns[0] != "value" {
		t.Fatalf("columns: got %v want [value]", r.Columns)
	}
}

func TestNativeCarrayPointerNotBound(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A text value is not a bound pointer: sqlite3_value_pointer yields 0,
	// so the table is empty but must not error (carray02-3.0.1/3.0.2).
	for _, q := range []string{
		"SELECT * FROM carray('0xFFFF', 5)",
		"SELECT * FROM carray('0xFFFF')",
	} {
		r := db.Query(q)
		if r.Error != nil {
			t.Fatalf("%s: %v", q, r.Error)
		}
		if len(r.Rows) != 0 {
			t.Fatalf("%s: want 0 rows, got %v", q, r.Rows)
		}
	}
}

func TestNativeCarrayWhereConstraint(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec(`CREATE TABLE t1(x); INSERT INTO t1 VALUES('0xFFFF');`).Error; err != nil {
		t.Fatal(err)
	}
	// WHERE pushdown of a hidden column with a non-pointer value: empty
	// result, no error (carray02-3.0.3).
	r := db.Query("SELECT * FROM t1, carray WHERE carray.pointer = t1.x")
	if r.Error != nil {
		t.Fatalf("join query: %v", r.Error)
	}
	if len(r.Rows) != 0 {
		t.Fatalf("join query: want 0 rows, got %v", r.Rows)
	}
}

func TestNativeCarrayUnknownType(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Unknown ctype raises "unknown datatype" at run time (carray02-3.1).
	r := db.Query("SELECT * FROM carray(inttoptr('intarray_addr 1 2'), 5, 'apples')")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "unknown datatype") {
		t.Fatalf("want unknown datatype error, got %v", r.Error)
	}
}

// intarrayAddr mirrors tabfunc01-700/701's harness array: the text form
// 'intarray_addr 5 7 13 17 23' names an int32 C array.
func TestNativeCarrayInttoptrJoin(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	steps := []string{
		"CREATE TABLE t600(a INTEGER PRIMARY KEY, b TEXT)",
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<100)" +
			" INSERT INTO t600(a,b) SELECT x, printf('(%03d)',x) FROM c",
	}
	for _, s := range steps {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// tabfunc01-700: join over the bound array values.
	r := db.Query("SELECT b FROM t600, carray(inttoptr('intarray_addr 5 7 13 17 23'),5) WHERE a=value ORDER BY a")
	if r.Error != nil {
		t.Fatalf("carray join: %v", r.Error)
	}
	var got []string
	for _, row := range r.Rows {
		got = append(got, row[0].(string))
	}
	want := "(005) (007) (013) (017) (023)"
	if strings.Join(got, " ") != want {
		t.Fatalf("got [%s] want [%s]", strings.Join(got, " "), want)
	}
}

func TestNativeCarrayInExpression(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	steps := []string{
		"CREATE TABLE t600(a INTEGER PRIMARY KEY, b TEXT)",
		"INSERT INTO t600(a,b) VALUES(5,'(005)'),(7,'(007)'),(9,'(009)'),(23,'(023)')",
	}
	for _, s := range steps {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// tabfunc01-701: IN with an explicit ctype argument.
	r := db.Query("SELECT b FROM t600 WHERE a IN carray(inttoptr('intarray_addr 5 7 13 17 23'),5,'int32') ORDER BY a")
	if r.Error != nil {
		t.Fatalf("IN carray: %v", r.Error)
	}
	var got []string
	for _, row := range r.Rows {
		got = append(got, row[0].(string))
	}
	want := "(005) (007) (023)"
	if strings.Join(got, " ") != want {
		t.Fatalf("got [%s] want [%s]", strings.Join(got, " "), want)
	}
}
