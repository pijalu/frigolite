package frigolite

import (
	"fmt"
	"testing"
)

// TestCoreFuncBlobCoercion pins SQLite's core-function BLOB→REAL conversion:
// func.c absFunc's default arm and roundFunc both call sqlite3_value_double,
// which vdbemem.c memRealValue implements as sqlite3AtoF over the blob bytes.
// Math-extension functions (exp/log/sign/radians) instead gate on
// sqlite3_value_numeric_type and return NULL for BLOB; pow/mod gate similarly.
// Expected values verified against /usr/bin/sqlite3 3.54.0.
func TestCoreFuncBlobCoercion(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	check := func(sql, want string) {
		t.Helper()
		res := db.Query(sql)
		if res.Error != nil {
			t.Errorf("%s: %v", sql, res.Error)
			return
		}
		if len(res.Rows) != 1 || len(res.Rows[0]) == 0 {
			t.Errorf("%s: no row", sql)
			return
		}
		got := ""
		for i, v := range res.Rows[0] {
			if i > 0 {
				got += ","
			}
			got += fmt.Sprintf("%T:%v", v, v)
		}
		if got != want {
			t.Errorf("%s\n  got  %s\n  want %s", sql, got, want)
		}
	}

	for _, tc := range []struct{ sql, want string }{
		{"SELECT abs(x'31')", "float64:1"},
		{"SELECT abs(x'312e35')", "float64:1.5"},
		{"SELECT abs(x'2d31')", "float64:1"},
		{"SELECT abs(x'4142')", "float64:0"},
		{"SELECT abs(x'0031')", "float64:0"},
		{"SELECT abs(x'')", "float64:0"},
		{"SELECT round(x'31')", "float64:1"},
		{"SELECT round(x'312e35')", "float64:2"},
		{"SELECT round(x'2d312e35')", "float64:-2"},
		{"SELECT round(x'4142')", "float64:0"},
		{"SELECT round(x'31', 1)", "float64:1"},
	} {
		check(tc.sql, tc.want)
	}

	// Same value syntax as SQLite's blob column storage; the typed/no-WHERE
	// path must produce the same numbers.
	if r := db.Exec("CREATE TABLE b(v BLOB)"); r.Error != nil {
		t.Fatalf("CREATE: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO b VALUES(x'31'),(x'312e35'),(x'4142')"); r.Error != nil {
		t.Fatalf("INSERT: %v", r.Error)
	}
	check("SELECT abs(v), round(v) FROM b WHERE rowid=2", "float64:1.5,float64:2")

	// BLOB must stay NULL for numeric_type-gated functions.
	for _, sql := range []string{
		"SELECT exp(x'31')", "SELECT sign(x'31')", "SELECT radians(x'31')",
		"SELECT pow(x'31',2)", "SELECT mod(x'31',3)", "SELECT log(x'31')",
	} {
		check(sql, "<nil>:<nil>")
	}
}
