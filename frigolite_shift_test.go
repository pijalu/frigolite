package frigolite

import "testing"

// TestBitwiseShiftSemantics pins SQLite's OP_ShiftLeft/OP_ShiftRight rules
// (vdbe.c): a negative count reverses direction, a count of 64 or more gives 0
// (or -1 when right-shifting a negative value), and MinInt64 is treated as a
// 64-bit count. Before this pin, `1 << -9223372036854775808` recursed between
// shiftLeft and shiftRight until the goroutine stack overflowed (run9 aborted
// the whole suite with "fatal error: stack overflow").
func TestBitwiseShiftSemantics(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT 1 << -9223372036854775808", "0"},
		{"SELECT -1 << -9223372036854775808", "-1"},
		{"SELECT 1 >> -9223372036854775808", "0"},
		{"SELECT -1 >> -9223372036854775808", "0"},
		{"SELECT 1 << -1", "0"},
		{"SELECT -1 << -1", "-1"},
		{"SELECT 1 << 64", "0"},
		{"SELECT -1 << 64", "0"},
		{"SELECT -1 >> 64", "-1"},
		{"SELECT 1 >> 64", "0"},
		{"SELECT 3 << 2", "12"},
		{"SELECT -8 >> 1", "-4"},
	}
	for _, c := range cases {
		r := db.Query(c.sql)
		if r.Error != nil {
			t.Errorf("%s: %v", c.sql, r.Error)
			continue
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
			t.Errorf("%s: rows = %v", c.sql, r.Rows)
			continue
		}
		if got := formatSQLiteValue(r.Rows[0][0]); got != c.want {
			t.Errorf("%s = %s, want %s", c.sql, got, c.want)
		}
	}
}
