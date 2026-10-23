// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import "testing"

// TestSQLiteOperatorOverrideUsesUserFunction pins SQLite's operator-override
// semantics: `X LIKE Y` is compiled into the function call like(Y,X), so an
// application function registered under that name REPLACES the built-in
// operator (expr.c sqlite3ExprCodeTarget, TK_LIKE_KW). The same holds for
// GLOB and REGEXP, and the negated forms apply SQL NOT to the function's
// result.
func TestSQLiteOperatorOverrideUsesUserFunction(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var likeCalls [][]interface{}
	db.RegisterFunction("like", func(args []interface{}) (interface{}, error) {
		likeCalls = append(likeCalls, append([]interface{}{}, args...))
		return int64(1), nil
	}, 2, 3)
	db.RegisterFunction("glob", func(args []interface{}) (interface{}, error) {
		return int64(1), nil
	}, 2, 2)
	db.RegisterFunction("regexp", func(args []interface{}) (interface{}, error) {
		return int64(1), nil
	}, 2, 2)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{"SELECT 'abc' LIKE 'def'", int64(1)},
		{"SELECT 'abc' NOT LIKE 'def'", int64(0)},
		{"SELECT 'abc' GLOB 'xyz'", int64(1)},
		{"SELECT 'abc' NOT GLOB 'xyz'", int64(0)},
		{"SELECT 'abc' REGEXP 'xyz'", int64(1)},
		{"SELECT 'abc' NOT REGEXP 'xyz'", int64(0)},
		// The function form resolves to the same application function.
		{"SELECT like('a%', 'abc')", int64(1)},
	}
	for _, c := range cases {
		res := db.Query(c.sql)
		if res.Error != nil {
			t.Fatalf("%s: %v", c.sql, res.Error)
		}
		if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
			t.Fatalf("%s: rows = %v", c.sql, res.Rows)
		}
		if got := res.Rows[0][0]; got != c.want {
			t.Errorf("%s = %v (%T), want %v", c.sql, got, got, c.want)
		}
	}
	// The pattern comes first, the string second (like(Y,X)).
	if len(likeCalls) == 0 {
		t.Fatal("the like() override was never called")
	}
	first := likeCalls[0]
	if len(first) != 2 || first[0] != "def" || first[1] != "abc" {
		t.Errorf("like() arguments = %v, want [def abc]", first)
	}
}

// TestSQLiteOperatorOverrideWithoutUserFunction pins that the built-in
// operators still work when no application function shadows them.
func TestSQLiteOperatorOverrideWithoutUserFunction(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql  string
		want interface{}
	}{
		{"SELECT 'abc' LIKE 'a%'", int64(1)},
		{"SELECT 'abc' LIKE 'b%'", int64(0)},
		{"SELECT 'abc' NOT LIKE 'b%'", int64(1)},
		{"SELECT 'abc' GLOB 'a*'", int64(1)},
		{"SELECT 'abc' GLOB 'b*'", int64(0)},
	}
	for _, c := range cases {
		res := db.Query(c.sql)
		if res.Error != nil {
			t.Fatalf("%s: %v", c.sql, res.Error)
		}
		if got := res.Rows[0][0]; got != c.want {
			t.Errorf("%s = %v, want %v", c.sql, got, c.want)
		}
	}
}
