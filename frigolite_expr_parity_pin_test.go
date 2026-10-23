// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"fmt"
	"testing"
)

// TestSQLiteCurrentTimeKeywordWithFrom pins that CURRENT_TIME /
// CURRENT_DATE / CURRENT_TIMESTAMP stay keywords when the statement has a FROM
// clause: they are the statement-cached current time, never column names
// (e_expr-12.3.6: "SELECT CURRENT_TIME FROM tblname").
func TestSQLiteCurrentTimeKeywordWithFrom(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE t(c)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("INSERT INTO t VALUES('x')"); res.Error != nil {
		t.Fatal(res.Error)
	}
	for _, sql := range []string{"SELECT CURRENT_TIME FROM t", "SELECT CURRENT_DATE FROM t", "SELECT CURRENT_TIMESTAMP FROM t"} {
		res := db.Query(sql)
		if res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
		if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
			t.Fatalf("%s: rows = %v", sql, res.Rows)
		}
	}
}

// TestSQLiteParameterNameWithDollar pins the parameter-name syntax: a ':' or
// '@' parameter takes an identifier run, and '$' is an identifier character
// in SQLite, so ":_$_" and "@_$_" are valid names (e_expr-11.2.4).
func TestSQLiteParameterNameWithDollar(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{"SELECT :_$_", "SELECT @_$_", "SELECT $_$_"} {
		res := db.Query(sql)
		if res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
}

// TestSQLiteParameterLimitErrors pins the two distinct prepare-time parameter
// errors of expr.c sqlite3ExprAssignVarNumber: an out-of-range explicit index
// gives "variable number must be between ?1 and ?N", while an assigned index
// above the limit gives "too many SQL variables".
func TestSQLiteParameterLimitErrors(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT ?0", "variable number must be between ?1 and ?32766"},
		{"SELECT ?32767", "variable number must be between ?1 and ?32766"},
		{"SELECT ?32766, ?", "too many SQL variables"},
	}
	for _, c := range cases {
		res := db.Query(c.sql)
		if res.Error == nil {
			t.Fatalf("%s: expected an error", c.sql)
		}
		if res.Error.Error() != c.want {
			t.Errorf("%s: error = %q, want %q", c.sql, res.Error.Error(), c.want)
		}
		// The Exec path reports the same error.
		if res := db.Exec(c.sql); res.Error == nil || res.Error.Error() != c.want {
			t.Errorf("%s (exec): error = %v, want %q", c.sql, res.Error, c.want)
		}
	}
}

// TestSQLiteNumericCastOverflow pins the NUMERIC cast of an integer that does
// not fit in int64: SQLite falls back to sqlite3AtoF and yields a REAL (the
// INTEGER cast still clamps).
func TestSQLiteNumericCastOverflow(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT typeof(CAST('9223372036854775808' AS NUMERIC))", "real"},
		{"SELECT typeof(CAST('9223372036854775807' AS NUMERIC))", "integer"},
		{"SELECT typeof(CAST('9.223372036854775808' AS NUMERIC))", "real"},
		{"SELECT typeof(CAST('9223372036854775808' AS INTEGER))", "integer"},
	}
	for _, c := range cases {
		res := db.Query(c.sql)
		if res.Error != nil {
			t.Fatalf("%s: %v", c.sql, res.Error)
		}
		if got := fmt.Sprint(res.Rows[0][0]); got != c.want {
			t.Errorf("%s = %s, want %s", c.sql, got, c.want)
		}
	}
}

// TestSQLiteBitwiseCoercion pins vdbe.c's OP_BitAnd/OP_BitOr operand
// coercion (sqlite3VdbeIntValue): text and blobs are parsed as integers, reals
// truncate, and the result is an INTEGER.
func TestSQLiteBitwiseCoercion(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql  string
		want interface{}
	}{
		{"SELECT '2245' & 66", int64(64)},
		{"SELECT 2.5 & 3", int64(2)},
		{"SELECT X'414243' & 3", int64(0)},
		{"SELECT 12 | 10", int64(14)},
		{"SELECT 1 << 3", int64(8)},
	}
	for _, c := range cases {
		res := db.Query(c.sql)
		if res.Error != nil {
			t.Fatalf("%s: %v", c.sql, res.Error)
		}
		if got := res.Rows[0][0]; got != c.want {
			t.Errorf("%s = %v (%T), want %v", c.sql, got, got, c.want)
		}
	}
}
