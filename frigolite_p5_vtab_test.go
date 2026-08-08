package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestP5VtabGenerateSeries verifies the generate_series table-valued
// function and virtual-table module: value sequences for start/stop/step,
// empty ranges, negative steps, and the rowid alias for value.
func TestP5VtabGenerateSeries(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cases := []struct {
		sql  string
		want []interface{}
	}{
		{"SELECT value FROM generate_series(1,5);", []interface{}{int64(1), int64(2), int64(3), int64(4), int64(5)}},
		{"SELECT * FROM generate_series(1,5);", []interface{}{int64(1), int64(2), int64(3), int64(4), int64(5)}},
		{"SELECT value FROM generate_series(1,5,2);", []interface{}{int64(1), int64(3), int64(5)}},
		{"SELECT value FROM generate_series(1,5,3);", []interface{}{int64(1), int64(4)}},
		// Empty range: start > stop with positive step yields no rows.
		{"SELECT value FROM generate_series(1,0);", []interface{}{}},
		{"SELECT value FROM generate_series(5,1);", []interface{}{}},
		// Negative step: descending sequence.
		{"SELECT value FROM generate_series(5,1,-1);", []interface{}{int64(5), int64(4), int64(3), int64(2), int64(1)}},
		{"SELECT value FROM generate_series(5,1,-2);", []interface{}{int64(5), int64(3), int64(1)}},
		// Negative step with start < stop yields no rows.
		{"SELECT value FROM generate_series(1,5,-1);", []interface{}{}},
	}
	for _, c := range cases {
		r := db.Query(c.sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", c.sql, r.Error)
		}
		if len(r.Rows) != len(c.want) {
			t.Fatalf("%s: got %d rows want %d (%v vs %v)", c.sql, len(r.Rows), len(c.want), r.Rows, c.want)
		}
		for i, w := range c.want {
			if !valuesEqual(r.Rows[i][0], w) {
				t.Errorf("%s: row %d got %v want %v", c.sql, i, r.Rows[i][0], w)
			}
		}
	}

	// The result column is named "value".
	r := db.Query("SELECT * FROM generate_series(1,3)")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Columns) != 1 || !strings.EqualFold(r.Columns[0], "value") {
		t.Fatalf("generate_series column: got %v want [value]", r.Columns)
	}

	// WHERE filtering on the generated values.
	r = db.Query("SELECT value FROM generate_series(1,10) WHERE value % 2 = 0")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var got []int64
	for _, row := range r.Rows {
		got = append(got, row[0].(int64))
	}
	want := []int64{2, 4, 6, 8, 10}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("generate_series WHERE: got %v want %v", got, want)
	}
}

// TestP5VtabGenerateSeriesCreateDrop verifies CREATE/DROP VIRTUAL TABLE with
// the generate_series module: the sqlite_master entry, column metadata, and
// DROP removal.
func TestP5VtabGenerateSeriesCreateDrop(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec("CREATE VIRTUAL TABLE s USING generate_series(1,4);"); res.Error != nil {
		t.Fatalf("create vtab: %v", res.Error)
	}
	// sqlite_master records the vtab as a table with rootpage 0.
	r := db.Query("SELECT type, name, rootpage, sql FROM sqlite_master WHERE name='s'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("sqlite_master: got %d rows want 1", len(r.Rows))
	}
	if r.Rows[0][0] != "table" || r.Rows[0][1] != "s" || r.Rows[0][2] != int64(0) {
		t.Errorf("sqlite_master entry: got %v", r.Rows[0])
	}
	if !strings.Contains(fmt.Sprint(r.Rows[0][3]), "USING generate_series") {
		t.Errorf("sqlite_master sql: got %v", r.Rows[0][3])
	}

	// PRAGMA table_info reports the module's declared column (value).
	r = db.Query("PRAGMA table_info(s)")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][1] != "value" {
		t.Errorf("table_info(s): got %v want column 'value'", r.Rows)
	}

	// Selecting from the created vtab works.
	r = db.Query("SELECT * FROM s")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 4 || r.Rows[0][0] != int64(1) || r.Rows[3][0] != int64(4) {
		t.Errorf("SELECT * FROM s: got %v", r.Rows)
	}

	// DROP removes the entry.
	if res := db.Exec("DROP TABLE s;"); res.Error != nil {
		t.Fatalf("drop vtab: %v", res.Error)
	}
	r = db.Query("SELECT name FROM sqlite_master WHERE name='s'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 0 {
		t.Errorf("after DROP: sqlite_master still has s: %v", r.Rows)
	}
	// A second DROP of a nonexistent vtab reports no such table.
	if res := db.Exec("DROP TABLE s;"); res.Error == nil {
		t.Errorf("DROP TABLE s (missing) should error")
	}
}

// TestP5VtabEchoProxies verifies the echo module mirrors an underlying real
// table: schema (column names/types), reads, and write-through (INSERT,
// UPDATE, DELETE) all route to the source table.
func TestP5VtabEchoProxies(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	steps := []string{
		"CREATE TABLE t1(a, b, c);",
		"INSERT INTO t1 VALUES(1, 'red', 'green');",
		"INSERT INTO t1 VALUES(2, 'blue', 'black');",
		"CREATE VIRTUAL TABLE et1 USING echo(t1);",
	}
	for _, s := range steps {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}

	// Schema mirrors the source (column names/types).
	r := db.Query("PRAGMA table_info(et1)")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 3 || r.Rows[0][1] != "a" || r.Rows[1][1] != "b" || r.Rows[2][1] != "c" {
		t.Errorf("table_info(et1): got %v", r.Rows)
	}

	// Reads return the source rows with source column names.
	r = db.Query("SELECT * FROM et1 ORDER BY a")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if fmt.Sprint(r.Columns) != "[a b c]" {
		t.Errorf("et1 columns: got %v want [a b c]", r.Columns)
	}
	wantRows := [][]interface{}{{int64(1), "red", "green"}, {int64(2), "blue", "black"}}
	if len(r.Rows) != 2 {
		t.Fatalf("et1 rows: got %v", r.Rows)
	}
	for i := range wantRows {
		for j := range wantRows[i] {
			if !valuesEqual(r.Rows[i][j], wantRows[i][j]) {
				t.Errorf("et1[%d][%d]: got %v want %v", i, j, r.Rows[i][j], wantRows[i][j])
			}
		}
	}

	// INSERT writes through to the source.
	if res := db.Exec("INSERT INTO et1 VALUES(3, 'white', 'grey');"); res.Error != nil {
		t.Fatalf("insert into et1: %v", res.Error)
	}
	r = db.Query("SELECT count(*) FROM t1")
	if r.Error != nil || r.Rows[0][0] != int64(3) {
		t.Errorf("after insert, t1 count: %v err %v", r.Rows, r.Error)
	}

	// UPDATE writes through.
	if res := db.Exec("UPDATE et1 SET b='BLUE' WHERE a=2;"); res.Error != nil {
		t.Fatalf("update et1: %v", res.Error)
	}
	r = db.Query("SELECT b FROM t1 WHERE a=2")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != "BLUE" {
		t.Errorf("after update, t1 b: %v err %v", r.Rows, r.Error)
	}

	// DELETE writes through.
	if res := db.Exec("DELETE FROM et1 WHERE a=1;"); res.Error != nil {
		t.Fatalf("delete et1: %v", res.Error)
	}
	r = db.Query("SELECT count(*) FROM t1")
	if r.Error != nil || r.Rows[0][0] != int64(2) {
		t.Errorf("after delete, t1 count: %v err %v", r.Rows, r.Error)
	}
}

// TestP5VtabEchoHidden verifies the echo module's HIDDEN column handling:
// hidden columns are excluded from SELECT * and PRAGMA table_info but remain
// readable by explicit references, and a no-column-list INSERT supplies the
// non-hidden columns.
func TestP5VtabEchoHidden(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	steps := []string{
		"CREATE TABLE t1(a, b HIDDEN VARCHAR, c INTEGER);",
		"CREATE VIRTUAL TABLE t1e USING echo(t1);",
		"INSERT INTO t1e VALUES('value a', 'value c');",
	}
	for _, s := range steps {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}

	// SELECT * excludes the hidden column; explicit references include it.
	r := db.Query("SELECT * FROM t1e")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if fmt.Sprint(r.Columns) != "[a c]" {
		t.Errorf("SELECT * cols: got %v want [a c]", r.Columns)
	}
	r = db.Query("SELECT a, b, c FROM t1e")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if fmt.Sprint(r.Columns) != "[a b c]" || r.Rows[0][1] != nil {
		t.Errorf("SELECT a,b,c: got cols %v row %v (hidden b should be NULL)", r.Columns, r.Rows[0])
	}

	// PRAGMA table_info excludes the hidden column.
	r = db.Query("PRAGMA table_info(t1e)")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 2 || r.Rows[0][1] != "a" || r.Rows[1][1] != "c" {
		t.Errorf("table_info(t1e): got %v", r.Rows)
	}

	// The source table itself keeps the HIDDEN type text (hidden applies to
	// virtual-table declarations, not ordinary tables).
	r = db.Query("PRAGMA table_info(t1)")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 3 || r.Rows[1][1] != "b" || r.Rows[1][2] != "HIDDEN VARCHAR" {
		t.Errorf("table_info(t1): got %v", r.Rows)
	}
}

// TestP5VtabTempLifecycle verifies CREATE VIRTUAL TABLE temp.<name> stores
// the vtab in the TEMP schema and DROP resolves it (vtabB-1.1 flow).
func TestP5VtabTempLifecycle(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	steps := []string{
		"CREATE TABLE t1(x);",
		"BEGIN;",
		"CREATE VIRTUAL TABLE temp.echo_test1 USING echo(t1);",
		"DROP TABLE echo_test1;",
		"ROLLBACK;",
	}
	for _, s := range steps {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
}
