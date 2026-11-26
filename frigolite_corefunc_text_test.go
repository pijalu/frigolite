package frigolite

import (
	"fmt"
	"testing"
)

// TestCoreFuncTextPrefix pins SQLite's sqlite3AtoF longest-prefix conversion
// for abs/round TEXT arguments. func.c absFunc's default arm and roundFunc
// call sqlite3_value_double unconditionally; vdbemem.c memRealValue runs
// sqlite3AtoF, which consumes the longest numeric prefix (leading whitespace
// accepted, dangling exponent ignored, non-numeric => 0.0). Expected values
// verified against /usr/bin/sqlite3 3.54.0. Numeric_type-gated functions must
// still reject non-numeric TEXT with NULL.
func TestCoreFuncTextPrefix(t *testing.T) {
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
		// Prefix, whitespace, sign, exponent, dangling exponent, non-numeric.
		{"SELECT abs('31abc')", "float64:31"},
		{"SELECT abs(' 31 ')", "float64:31"},
		{"SELECT abs('+31')", "float64:31"},
		{"SELECT abs('3e-2')", "float64:0.03"},
		{"SELECT abs('31e')", "float64:31"},
		{"SELECT abs('abc')", "float64:0"},
		{"SELECT abs('')", "float64:0"},
		{"SELECT abs(char(0)||'31')", "float64:0"},
		{"SELECT abs('31'||char(0)||'abc')", "float64:31"},
		{"SELECT round('31abc')", "float64:31"},
		{"SELECT round('31abc',1)", "float64:31"},
		{"SELECT round(' 31.5 ')", "float64:32"},
		{"SELECT round('-31.5')", "float64:-32"},
		{"SELECT round('31e2')", "float64:3100"},
		{"SELECT round('31e')", "float64:31"},
		{"SELECT round('abc')", "float64:0"},
		{"SELECT round('9e900')", "float64:+Inf"},
		// Previously passing shapes must stay exact.
		{"SELECT abs(-5)", "int64:5"},
		{"SELECT round(31.5)", "float64:32"},
		{"SELECT abs(3.5)", "float64:3.5"},
		{"SELECT round('31.25',1)", "float64:31.3"},
		// sqlite3_value_numeric_type-gated functions reject non-numeric TEXT.
		{"SELECT exp('31abc')", "<nil>:<nil>"},
		{"SELECT sign('31abc')", "<nil>:<nil>"},
		{"SELECT pow('31abc',2)", "<nil>:<nil>"},
	} {
		check(tc.sql, tc.want)
	}
}
