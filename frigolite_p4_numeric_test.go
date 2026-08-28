package frigolite

import (
	"math"
	"strings"
	"testing"
)

// P4 pre-tests: hand-written tests for G4.NUMERIC numeric functions and
// IEEE-754 special-value handling, written BEFORE running the TCL testgen
// packages (round, nan, zeroblob, unhex, percentile). Each expectation was
// verified against sqlite3 3.51 as the oracle; the tests document the exact
// SQLite semantics frigolite must match: NaN is always converted to NULL,
// +/-Inf are valid REALs that propagate through arithmetic (except when the
// result is NaN, which becomes NULL), division by zero yields NULL, REALs
// render with %.15g formatting, and ZEROBLOB(n) with negative n is the
// empty blob.

// TestP4Numeric_AbsRound covers abs() and round() edge cases: abs(NULL),
// abs of negative integer/real, round with 0/negative digits, round of
// values whose integer part overflows, and round(Inf).
func TestP4Numeric_AbsRound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT abs(-5), abs(5), abs(-5.5), abs(5.5)", "5 5 5.5 5.5"},
		{"SELECT ifnull(abs(NULL),'nil')", "nil"},
		{"SELECT abs(-9223372036854775807)", "9223372036854775807"},
		{"SELECT round(5.567, 2)", "5.57"},
		{"SELECT round(5.5)", "6.0"},
		{"SELECT round(4.5)", "5.0"}, // SQLite rounds half away from zero
		{"SELECT round(-4.5)", "-5.0"},
		{"SELECT round(1234.5678, -2)", "1235.0"}, // negative digits behave like 0
		{"SELECT round(1234.5678, -1)", "1235.0"},
		{"SELECT round(1234.5678, 0)", "1235.0"},
		{"SELECT round(5.567, NULL)", "NULL"},
		{"SELECT round(15.5, 1)", "15.5"},
		{"SELECT round(123456789012345678901234567890.0, 2)", "1.23456789012346e+29"},
		{"SELECT round(1e400, 2)", "Inf"},
		{"SELECT ifnull(round(NULL, 2),'nil')", "nil"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}

	// abs(MinInt64) overflows: SQLite raises "integer overflow".
	if err := queryError(db, "SELECT abs(-9223372036854775808)"); err == nil || !strings.Contains(err.Error(), "integer overflow") {
		t.Errorf("abs(MinInt64): got err %v, want 'integer overflow'", err)
	}
}

// TestP4Numeric_Math covers the scalar math functions. Transcendental
// functions (sin, log, etc.) are compared with a relative tolerance because
// SQLite's libm may differ in the last ULP; pow/sqrt/floor/ceil/trunc/mod
// are exact for the tested inputs.
func TestP4Numeric_Math(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Exact cases (integer results or exactly representable).
	exact := []struct {
		sql  string
		want string
	}{
		{"SELECT sqrt(4), sqrt(0)", "2.0 0.0"},
		{"SELECT pow(2,10), power(2,10)", "1024.0 1024.0"},
		{"SELECT pow(2,-2)", "0.25"},
		{"SELECT pow(NULL,2)", "NULL"},
		{"SELECT sqrt(NULL)", "NULL"},
		{"SELECT floor(1.7), floor(-1.7), floor(5)", "1.0 -2.0 5"},
		{"SELECT ceil(1.2), ceil(-1.2), ceiling(1.2)", "2.0 -1.0 2.0"},
		{"SELECT trunc(1.777, 2), trunc(-1.777, 2)", "1.77 -1.77"},
		{"SELECT trunc(1.7), trunc(-1.7)", "1.0 -1.0"},
		{"SELECT mod(7,3), mod(-7,3), mod(7,-3), mod(-7,-3)", "1.0 -1.0 1.0 -1.0"},
		{"SELECT mod(5.5, 2)", "1.5"},
		{"SELECT mod(7.9, 2)", "1.9"},
		{"SELECT ifnull(mod(1e400, 3),'nil')", "nil"},
		{"SELECT ifnull(mod(7,0),'nil')", "nil"},
		{"SELECT sign(-5), sign(0), sign(5), sign(-5.5), sign(0.0)", "-1 0 1 -1 0"},
		{"SELECT ifnull(sign(NULL),'nil')", "nil"},
		{"SELECT degrees(3.141592653589793), radians(180)", "180.0 3.14159265358979"},
		{"SELECT asin(0), acos(1), atan(0)", "0.0 0.0 0.0"},
		{"SELECT sinh(0), cosh(0), tanh(0)", "0.0 1.0 0.0"},
		{"SELECT ifnull(sqrt(-1),'nil')", "nil"},
		{"SELECT ifnull(ln(-1),'nil')", "nil"},
		{"SELECT ifnull(log(-1),'nil')", "nil"},
		{"SELECT ifnull(log(0),'nil')", "nil"},
		{"SELECT ifnull(asin(2),'nil')", "nil"},
	}
	for _, c := range exact {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}

	// Tolerance cases: compute via the engine and compare to the oracle value.
	tol := []struct {
		sql     string
		want    float64
		relTol  float64
	}{
		{"SELECT sqrt(2)", math.Sqrt2, 1e-15},
		{"SELECT sin(1)", math.Sin(1), 1e-15},
		{"SELECT cos(1)", math.Cos(1), 1e-15},
		{"SELECT tan(1)", math.Tan(1), 1e-14},
		{"SELECT asin(0.5)", math.Asin(0.5), 1e-15},
		{"SELECT acos(0.5)", math.Acos(0.5), 1e-15},
		{"SELECT atan(1)", math.Atan(1), 1e-15},
		{"SELECT atan2(1,1)", math.Atan2(1, 1), 1e-15},
		{"SELECT log(100)", 2, 1e-15},
		{"SELECT log10(100)", 2, 1e-15},
		{"SELECT log2(8)", 3, 1e-15},
		{"SELECT ln(1)", 0, 1e-15},
		{"SELECT exp(1)", math.E, 1e-15},
		{"SELECT pi()", math.Pi, 1e-15},
		{"SELECT sinh(1)", math.Sinh(1), 1e-15},
		{"SELECT cosh(1)", math.Cosh(1), 1e-15},
		{"SELECT tanh(1)", math.Tanh(1), 1e-15},
		{"SELECT asinh(1)", math.Asinh(1), 1e-15},
		{"SELECT acosh(2)", math.Acosh(2), 1e-15},
		{"SELECT log(8, 2)", 0.3333333333333333, 1e-15}, // log(X, B) = log base X of B
	}
	for _, c := range tol {
		r := db.Query(c.sql)
		if r.Error != nil {
			t.Errorf("%s: query error: %v", c.sql, r.Error)
			continue
		}
		if len(r.Rows) == 0 || len(r.Rows[0]) == 0 {
			t.Errorf("%s: no result row", c.sql)
			continue
		}
		gotf, ok := r.Rows[0][0].(float64)
		if !ok {
			t.Errorf("%s: result %v is not float64", c.sql, r.Rows[0][0])
			continue
		}
		if math.Abs(gotf-c.want) > c.relTol*math.Max(1, math.Abs(c.want)) {
			t.Errorf("%s\n  got:  [%v]\n  want: [%v] (tol %v)", c.sql, gotf, c.want, c.relTol)
		}
	}
}

// TestP4Numeric_NaNInf covers IEEE-754 special values: 1e400 overflows to
// +Inf, Inf propagates through arithmetic except when the result is NaN
// (which becomes NULL), comparisons involving Inf, CAST to text, printf,
// and NaN always becoming NULL.
func TestP4Numeric_NaNInf(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT typeof(1e400), 1e400", "real Inf"},
		{"SELECT typeof(-1e400), -1e400", "real -Inf"},
		{"SELECT 1e400 + 1e400", "Inf"},
		{"SELECT -1e400 + -1e400", "-Inf"},
		{"SELECT 1e400 + 1", "Inf"},
		{"SELECT ifnull(1e400 - 1e400, 'nil')", "nil"},   // Inf-Inf = NaN -> NULL
		{"SELECT ifnull(1e400 * 0, 'nil')", "nil"},       // Inf*0 = NaN -> NULL
		{"SELECT ifnull(1e400 / 1e400, 'nil')", "nil"},   // Inf/Inf = NaN -> NULL
		{"SELECT ifnull(1.0 / 0.0, 'nil')", "nil"},       // division by zero -> NULL
		{"SELECT ifnull(-1.0 / 0.0, 'nil')", "nil"},
		{"SELECT ifnull(0.0 / 0.0, 'nil')", "nil"},
		{"SELECT ifnull(1 / 0, 'nil')", "nil"},           // integer division by zero
		{"SELECT 1e400 > 1e308", "1"},
		{"SELECT 1e400 = 1e400", "1"},
		{"SELECT 1e400 < -1e400", "0"},
		{"SELECT cast(1e400 AS text)", "Inf"},
		{"SELECT cast(-1e400 AS text)", "-Inf"},
		{"SELECT cast(1e-400 AS text)", "0.0"},
		{"SELECT printf('%f', 1e400)", "Inf"},
		{"SELECT printf('%e', 1e400)", "Inf"},
		{"SELECT printf('%g', 1e400)", "Inf"},
		{"SELECT printf('%.1f', -1e400)", "-Inf"},
		{"SELECT abs(-1e400)", "Inf"},
		{"SELECT round(1e400, 2)", "Inf"},
		{"SELECT hex(1e400)", "496E66"}, // hex of the text "Inf"
		{"SELECT typeof(1e400), 1e400 IS NULL", "real 0"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}

	// ORDER BY with Inf values must sort correctly (finite < +Inf, -Inf < finite).
	execTestSQL(t, db, "CREATE TABLE t1(x FLOAT); INSERT INTO t1 VALUES(1.0),(2.0),(-1e400),(1e400),(0.5);")
	got := flattenQuery(t, db, "SELECT x FROM t1 ORDER BY x")
	if got != "-Inf 0.5 1.0 2.0 Inf" {
		t.Errorf("ORDER BY with Inf: got [%s], want [-Inf 0.5 1.0 2.0 Inf]", got)
	}
}

// TestP4Numeric_Blob covers ZEROBLOB and RANDOMBLOB: length/type/hex of
// zeroblob, negative length (empty blob), the too-big error, and
// RANDOMBLOB length/type. unhex()/hex() round-trips and separator handling
// are also checked.
func TestP4Numeric_Blob(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT length(zeroblob(5)), typeof(zeroblob(5)), hex(zeroblob(5))", "5 blob 0000000000"},
		{"SELECT length(zeroblob(0)), typeof(zeroblob(0))", "0 blob"},
		{"SELECT length(zeroblob(-1)), typeof(zeroblob(-1)), hex(zeroblob(-1))", "0 blob "},
		{"SELECT quote(zeroblob(-1444444444444444))", "X''"},
		{"SELECT length(zeroblob(-1))", "0"},
		{"SELECT zeroblob(-1) | 1", "1"},
		{"SELECT cast(zeroblob(100) AS REAL)", "0.0"},
		{"SELECT cast(zeroblob(100) AS INTEGER)", "0"},
		{"SELECT hex(zeroblob(2) || x'61')", "000061"},
		{"SELECT typeof(randomblob(8)), length(randomblob(8))", "blob 8"},
		{"SELECT length(randomblob(0))", "0"},
		{"SELECT hex(unhex('0000'))", "0000"},
		{"SELECT hex(unhex('FFFF', ' -'))", "FFFF"},
		{"SELECT hex(unhex('FFFF  ABCD', ' -'))", "FFFFABCD"},
		{"SELECT typeof(unhex(' ', ' -')), length(unhex('-', ' -'))", "blob 0"},
		{"SELECT typeof(unhex(''))", "blob"},
		{"SELECT ifnull(unhex('ABC'), 'nil')", "nil"},
		{"SELECT ifnull(unhex('123456x7'), 'nil')", "nil"},
		{"SELECT ifnull(unhex(NULL), 'nil')", "nil"},
		{"SELECT ifnull(unhex('1234', NULL), 'nil')", "nil"},
		{"SELECT hex(unhex('กABCDข', 'กข'))", "ABCD"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}

	// zeroblob too big: SQLite raises "string or blob too big".
	err := queryError(db, "SELECT zeroblob(5000 * 1024 * 1024)")
	if err == nil || !strings.Contains(err.Error(), "string or blob too big") {
		t.Errorf("zeroblob(5000MB): got err %v, want 'string or blob too big'", err)
	}

	// unhex() argument-count errors.
	err = queryError(db, "SELECT unhex()")
	if err == nil || !strings.Contains(err.Error(), "wrong number of arguments to function unhex()") {
		t.Errorf("unhex(): got err %v, want 'wrong number of arguments'", err)
	}
	err = queryError(db, "SELECT unhex('AB', 'CD', 'EF')")
	if err == nil || !strings.Contains(err.Error(), "wrong number of arguments to function unhex()") {
		t.Errorf("unhex(3 args): got err %v, want 'wrong number of arguments'", err)
	}
}

// TestP4Numeric_ModSign covers MOD and SIGN edge cases beyond the Math test:
// MOD of negative operands follows SQLite's truncated-integer semantics.
func TestP4Numeric_ModSign(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT mod(10, 3)", "1.0"},
		{"SELECT mod(-10, 3)", "-1.0"},
		{"SELECT mod(10, -3)", "1.0"},
		{"SELECT mod(-10, -3)", "-1.0"},
		{"SELECT mod(7.5, 2)", "1.5"},
		{"SELECT mod(7.9, 2)", "1.9"},
		{"SELECT ifnull(mod(1e400, 3), 'nil')", "nil"},
		{"SELECT ifnull(mod(7, 0), 'nil')", "nil"},
		{"SELECT ifnull(mod(NULL, 2), 'nil')", "nil"},
		{"SELECT ifnull(mod(7, NULL), 'nil')", "nil"},
		{"SELECT sign(-0.0001), sign(0.0001)", "-1 1"},
		{"SELECT sign(-1e400), sign(1e400)", "-1 1"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// execTestSQL runs a SQL statement and fails the test on error.
func execTestSQL(t *testing.T, db *DB, sql string) {
	t.Helper()
	if res := db.Exec(sql); res.Error != nil {
		t.Fatalf("exec error: %v\n  sql: %s", res.Error, sql)
	}
}
