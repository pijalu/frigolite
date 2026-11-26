package frigolite

import (
	"fmt"
	"testing"
)

// TestSumBlobCoercion pins SQLite's SUM/AVG/TOTAL treatment of BLOB inputs.
// vdbemem.c sqlite3VdbeRealValue converts a BLOB to REAL through sqlite3AtoF
// on its bytes (the longest numeric prefix, no integer classification), and
// func.c sumStep counts it on the approximate path. Expected values were
// verified against /usr/bin/sqlite3 3.54.0.
func TestSumBlobCoercion(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	check := func(sql, want string) {
		t.Helper()
		res := db.Query(sql)
		if res.Error != nil {
			t.Errorf("%s: %v", sql, res.Error)
			return
		}
		got := ""
		for i, c := range res.Columns {
			if i > 0 {
				got += "|"
			}
			got += c
		}
		got += " ;; "
		for i, row := range res.Rows {
			if i > 0 {
				got += ";"
			}
			for j, v := range row {
				if j > 0 {
					got += ","
				}
				got += fmt.Sprintf("%T:%v", v, v)
			}
		}
		got += ";"
		if got != want {
			t.Errorf("%s\n  got  %s\n  want %s", sql, got, want)
		}
	}

	// Scalar constants: BLOB bytes are read as text by sqlite3AtoF. A numeric
	// prefix converts (possibly fractional, signed, exponent); garbage or an
	// embedded NUL before the digits reads 0.0. Every result is REAL.
	for _, tc := range []struct{ lit, val string }{
		{"x'31'", "float64:1"},
		{"x'312e35'", "float64:1.5"},
		{"x'4142'", "float64:0"},
		{"x'0031'", "float64:0"},
		{"x'3120'", "float64:1"},
		{"x'2b31'", "float64:1"},
		{"x'2d31'", "float64:-1"},
		{"x''", "float64:0"},
		{"x'696e66'", "float64:0"},
	} {
		check("SELECT sum("+tc.lit+"), typeof(sum("+tc.lit+"))",
			"sum("+tc.lit+")|typeof(sum("+tc.lit+")) ;; "+tc.val+",string:real;")
	}
	check("SELECT avg(x'31'), total(x'31'), count(x'31')",
		"avg(x'31')|total(x'31')|count(x'31') ;; float64:1,float64:1,int64:1;")

	// Column BLOBs feed the same approximate path, including the typed
	// aggregate lane that steps directly from the record bytes.
	exec("CREATE TABLE b(v BLOB)")
	exec("INSERT INTO b VALUES(x'31'),(x'312e35'),(x'33306532'),(x'4142'),(x'0031'),(x'3120'),(x'2031'),(x'2b31'),(x'2d31'),(x'')")
	check("SELECT sum(v), avg(v), total(v), count(v), typeof(sum(v)) FROM b",
		"sum(v)|avg(v)|total(v)|count(v)|typeof(sum(v)) ;; float64:3004.5,float64:300.45,float64:3004.5,int64:10,string:real;")
}
