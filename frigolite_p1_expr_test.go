package frigolite

import (
	"reflect"
	"strings"
	"testing"
)

// TestP1ExprArithmetic covers arithmetic operators with SQLite affinity
// rules: INTEGER division truncates (5/2=2) while REAL division keeps the
// fraction (5.0/2=2.5); modulo; unary minus; and NULL propagation.
func TestP1ExprArithmetic(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER, r REAL);
		INSERT INTO t VALUES(5, 5.0);
		INSERT INTO t VALUES(7, 7.5);
		INSERT INTO t VALUES(NULL, NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT 5/2, 5.0/2, 5/2.0, 7/2",
		"SELECT a/2, a%2, -a, +a FROM t ORDER BY a",
		"SELECT r/2, r%2 FROM t ORDER BY r",
		"SELECT 5/2, 5%2, 5.0/2, -5, +5",
		"SELECT 1/0, 1.0/0.0",
		"SELECT a/0 FROM t ORDER BY a",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprConcat covers the || concatenation operator including NULL
// propagation ('a'||NULL is NULL) and numeric coercion.
func TestP1ExprConcat(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER, s TEXT);
		INSERT INTO t VALUES(1, 'x');
		INSERT INTO t VALUES(NULL, 'y');
		INSERT INTO t VALUES(2, NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT 'a'||'b', 'a'||NULL, NULL||'b', NULL||NULL",
		"SELECT 1||2, 1.5||'x', 'x'||1",
		"SELECT a||s FROM t ORDER BY a",
		"SELECT s||a FROM t ORDER BY a",
		"SELECT a||a FROM t ORDER BY a",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprBooleanLogic covers AND/OR/NOT with SQLite's three-valued logic
// (Kleene): TRUE AND NULL = NULL, FALSE AND NULL = FALSE, TRUE OR NULL = TRUE.
func TestP1ExprBooleanLogic(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER, b INTEGER);
		INSERT INTO t VALUES(1, 1);
		INSERT INTO t VALUES(1, 0);
		INSERT INTO t VALUES(NULL, 1);
		INSERT INTO t VALUES(NULL, 0);
		INSERT INTO t VALUES(NULL, NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT 1 AND 1, 1 AND 0, 0 AND 1, 0 AND 0",
		"SELECT 1 AND NULL, 0 AND NULL, NULL AND 1, NULL AND 0, NULL AND NULL",
		"SELECT 1 OR 1, 1 OR 0, 0 OR 1, 0 OR 0",
		"SELECT 1 OR NULL, 0 OR NULL, NULL OR 1, NULL OR 0, NULL OR NULL",
		"SELECT NOT 1, NOT 0, NOT NULL",
		"SELECT a AND b, a OR b, NOT a FROM t ORDER BY a, b",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprComparison3VL covers comparison operators returning NULL on
// NULL operands and IS / IS NOT / IS NULL / IS NOT NULL / IS TRUE / IS FALSE.
func TestP1ExprComparison3VL(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER, b INTEGER);
		INSERT INTO t VALUES(1, 1);
		INSERT INTO t VALUES(2, 1);
		INSERT INTO t VALUES(NULL, 1);
		INSERT INTO t VALUES(NULL, NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT 1=1, 1=2, NULL=1, 1=NULL, NULL=NULL",
		"SELECT 1<>2, 1<>1, NULL<>1, 1<>NULL",
		"SELECT 1<2, 2<1, NULL<1, 1<NULL, NULL<NULL",
		"SELECT a=b, a<b, a>=b FROM t ORDER BY a, b",
		"SELECT 1 IS 1, 1 IS 2, NULL IS NULL, NULL IS 1, 1 IS NULL",
		"SELECT 1 IS NOT 2, 1 IS NOT 1, NULL IS NOT NULL, NULL IS NOT 1",
		"SELECT a IS NULL, a IS NOT NULL FROM t ORDER BY a, b",
		"SELECT 1 IS TRUE, 0 IS TRUE, NULL IS TRUE, 1 IS FALSE, 0 IS FALSE, NULL IS FALSE",
		"SELECT 1 IS NOT TRUE, 0 IS NOT TRUE, NULL IS NOT TRUE, 0 IS NOT FALSE",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprBetween covers BETWEEN and NOT BETWEEN, including NULL bounds
// and the 3-valued result.
func TestP1ExprBetween(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER);
		INSERT INTO t VALUES(1), (5), (10), (NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT 5 BETWEEN 1 AND 10, 0 BETWEEN 1 AND 10, 11 BETWEEN 1 AND 10",
		"SELECT 5 NOT BETWEEN 1 AND 10, 0 NOT BETWEEN 1 AND 10",
		"SELECT 5 BETWEEN NULL AND 10, 5 BETWEEN 1 AND NULL, NULL BETWEEN 1 AND 10",
		"SELECT a BETWEEN 1 AND 5 FROM t ORDER BY a",
		"SELECT a NOT BETWEEN 1 AND 5 FROM t ORDER BY a",
		"SELECT 5 BETWEEN 10 AND 1, 5 BETWEEN 5 AND 5",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprIn covers IN / NOT IN with literal lists, including NULL
// semantics: a NULL on the left yields NULL unless a match is found; a NULL
// in the list with no match yields NULL.
func TestP1ExprIn(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER);
		INSERT INTO t VALUES(1), (2), (3), (NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT 2 IN (1,2,3), 4 IN (1,2,3), 2 NOT IN (1,2,3), 4 NOT IN (1,2,3)",
		"SELECT NULL IN (1,2,3), NULL IN (NULL), NULL NOT IN (1,2,3)",
		"SELECT 1 IN (1,NULL), 2 IN (1,NULL), 2 NOT IN (1,NULL)",
		"SELECT a IN (1,2) FROM t ORDER BY a",
		"SELECT a NOT IN (1,2) FROM t ORDER BY a",
		"SELECT 2 IN (1,2,NULL), 3 IN (1,2,NULL), 3 NOT IN (1,2,NULL)",
		"SELECT 1 IN (), 1 NOT IN ()",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprCase covers searched CASE (CASE WHEN ... THEN ... ELSE ... END)
// and simple CASE (CASE x WHEN ... THEN ...), including NULL in WHEN and
// missing ELSE (which yields NULL).
func TestP1ExprCase(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER);
		INSERT INTO t VALUES(1), (2), (3), (NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT CASE WHEN 1 THEN 'yes' ELSE 'no' END",
		"SELECT CASE WHEN 0 THEN 'yes' ELSE 'no' END",
		"SELECT CASE WHEN NULL THEN 'yes' ELSE 'no' END",
		"SELECT CASE WHEN NULL THEN 'yes' END",
		"SELECT CASE 2 WHEN 1 THEN 'one' WHEN 2 THEN 'two' ELSE 'other' END",
		"SELECT CASE NULL WHEN NULL THEN 'null' ELSE 'not-null' END",
		"SELECT CASE a WHEN 1 THEN 'one' WHEN 2 THEN 'two' ELSE 'other' END FROM t ORDER BY a",
		"SELECT CASE WHEN a=1 THEN 'one' WHEN a=2 THEN 'two' END FROM t ORDER BY a",
		"SELECT CASE WHEN a THEN 't' ELSE 'f' END FROM t ORDER BY a",
		"SELECT CASE WHEN 1 THEN 1 WHEN 0 THEN 0 END",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprCast covers CAST to INTEGER, REAL, TEXT, BLOB, and NUMERIC,
// including NULL.
func TestP1ExprCast(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, q := range []string{
		"SELECT CAST('123' AS INTEGER), CAST(123.9 AS INTEGER), CAST('x' AS INTEGER)",
		"SELECT CAST('1.5' AS REAL), CAST(1 AS REAL)",
		"SELECT CAST(123 AS TEXT), CAST(1.5 AS TEXT), CAST(x'6869' AS TEXT)",
		"SELECT CAST('hi' AS BLOB), hex(CAST('hi' AS BLOB))",
		"SELECT CAST('12' AS NUMERIC), CAST('12.5' AS NUMERIC), CAST('x' AS NUMERIC)",
		"SELECT CAST(NULL AS INTEGER), typeof(CAST(NULL AS TEXT))",
		"SELECT CAST(5 AS INTEGER), CAST(5.5 AS INTEGER), CAST(5.5 AS TEXT)",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, "")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprLiterals covers integer, real, text (single-quoted), blob (x'..'),
// hex (0x..), NULL, and double-quoted string (DQS) literals.
func TestP1ExprLiterals(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, q := range []string{
		"SELECT 5, 5.5, 'abc', NULL, hex(x'6869')",
		"SELECT typeof(5), typeof(5.5), typeof('abc'), typeof(NULL), typeof(x'6869')",
		"SELECT hex(x'00ff'), 0x10, 0xFF, typeof(0x10)",
		"SELECT 'it''s', ''",
		"SELECT \"abc\", \"\"",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, "")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprPrecedence covers operator precedence: || before arithmetic,
// arithmetic before comparison, comparison before AND/OR, NOT before AND.
func TestP1ExprPrecedence(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER);
		INSERT INTO t VALUES(2), (4), (NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		// || binds tighter than + : '1'||'2' + 3 → '123'? No: 1||2 is text,
		// then +3 coerces to 125? Check against oracle.
		"SELECT 1||2+3",
		"SELECT 1+2*3, (1+2)*3, 2*3+1",
		"SELECT 1=1 AND 2=2, 1=2 OR 2=2, NOT 1=1, NOT 1=2",
		"SELECT NOT 1=1 AND 2=2, NOT (1=1 AND 2=2)",
		"SELECT 2+3=5 AND 1<2, 2+3=4 OR 1<2",
		"SELECT a+1*2 FROM t ORDER BY a",
		"SELECT -a+1, -(a+1) FROM t ORDER BY a",
		"SELECT 1<2=1, (1<2)=1",
		"SELECT 'x'||1+2, 'x'||(1+2)",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprExists covers EXISTS / NOT EXISTS with subqueries.
func TestP1ExprExists(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER);
		INSERT INTO t VALUES(1), (2);
		CREATE TABLE e(a INTEGER);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT EXISTS(SELECT 1 FROM t), EXISTS(SELECT 1 FROM e)",
		"SELECT NOT EXISTS(SELECT 1 FROM t), NOT EXISTS(SELECT 1 FROM e)",
		"SELECT EXISTS(SELECT 1 FROM t WHERE a=2), EXISTS(SELECT 1 FROM t WHERE a=9)",
		"SELECT EXISTS(SELECT 1), EXISTS(SELECT 1 WHERE 0)",
		"SELECT 1 WHERE EXISTS(SELECT 1 FROM t)",
		"SELECT 1 WHERE NOT EXISTS(SELECT 1 FROM e)",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprScalarMinMax covers the scalar two-argument min()/max() forms
// (per-row, not aggregate) and NULL semantics.
func TestP1ExprScalarMinMax(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER, b INTEGER);
		INSERT INTO t VALUES(1, 5), (3, 2), (NULL, 4), (7, NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT min(3,1,2), max(3,1,2), min(1), max(1)",
		"SELECT min(NULL, 5), max(NULL, 5), min(1, NULL), max(1, NULL)",
		"SELECT min(a,b), max(a,b) FROM t ORDER BY a",
		"SELECT min(a, 5), max(a, 5) FROM t ORDER BY a",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprCoalesceNullif covers COALESCE/IFNULL/NULLIF argument evaluation
// and NULL semantics.
func TestP1ExprCoalesceNullif(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER, b INTEGER, c INTEGER);
		INSERT INTO t VALUES(1, NULL, 3);
		INSERT INTO t VALUES(NULL, 2, NULL);
		INSERT INTO t VALUES(NULL, NULL, NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT coalesce(NULL, NULL, 3), coalesce(NULL, NULL, NULL)",
		"SELECT ifnull(NULL, 5), ifnull(1, 5)",
		"SELECT nullif(1, 1), nullif(1, 2), nullif(NULL, NULL)",
		"SELECT coalesce(a,b,c), ifnull(a,b), nullif(a,b) FROM t ORDER BY a",
		"SELECT coalesce(a, NOT a, -a, a*123, b) FROM t ORDER BY a",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprDefaultTrueFalse covers bare DEFAULT true/false (boolean
// literals as default values) and DEFAULT(true)/DEFAULT(false).
func TestP1ExprDefaultTrueFalse(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := "CREATE TABLE t(a INTEGER PRIMARY KEY, b BOOLEAN DEFAULT true, c BOOLEAN DEFAULT(false), d BOOLEAN DEFAULT false, e BOOLEAN DEFAULT(true));"
	oracleSetup := setup + "\nINSERT INTO t DEFAULT VALUES;\nINSERT INTO t(a) VALUES(2);"
	runSQL(t, db,
		setup,
		"INSERT INTO t DEFAULT VALUES",
		"INSERT INTO t(a) VALUES(2)",
	)
	for _, q := range []string{
		"SELECT a, b, c, d, e, typeof(b), typeof(c), typeof(d), typeof(e) FROM t ORDER BY a",
		"SELECT true, false, not true, not false",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, oracleSetup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1ExprNoSuchColumn ensures unresolvable column references (qualified
// or not, in a FROM-less SELECT) raise "no such column", matching SQLite.
func TestP1ExprNoSuchColumn(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, q := range []string{
		"SELECT false.false",
		"SELECT 9 IN (false.false)",
		"SELECT nonexistent",
	} {
		res := db.Query(q)
		if res.Error == nil {
			t.Errorf("%s: expected error, got nil", q)
			continue
		}
		if !strings.Contains(res.Error.Error(), "no such column") {
			t.Errorf("%s: error %q does not mention no such column", q, res.Error)
		}
	}
}
