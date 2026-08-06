package frigolite

import (
	"reflect"
	"testing"
)

// oracle compares the engine's queryRows result for q against the system
// sqlite3 CLI run over the same setup SQL. It skips when sqlite3 is missing.
func oracle(t *testing.T, q, setup string) [][]string {
	t.Helper()
	return oracleRowsWith(t, q, setup)
}

// TestP1WhereComparison covers the comparison operators = <> != < <= > >=
// with numeric, text, blob, and mixed-affinity operands, verified against the
// oracle (system sqlite3 CLI) so the engine matches SQLite exactly.
func TestP1WhereComparison(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER, b TEXT, c REAL, d BLOB);
		INSERT INTO t VALUES(1, '1', 1.0, x'31');
		INSERT INTO t VALUES(2, '2', 2.0, x'32');
		INSERT INTO t VALUES(NULL, NULL, NULL, NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT a=1, a<>1, a!=1, a<2, a<=1, a>0, a>=1 FROM t ORDER BY a",
		"SELECT b='1', b='2' FROM t ORDER BY a",
		"SELECT a='1', a=1, c=1.0, c='1.0' FROM t ORDER BY a",
		"SELECT count(*) FROM t WHERE a>0",
		"SELECT count(*) FROM t WHERE a<>1",
		"SELECT count(*) FROM t WHERE a>=1",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1WhereNullLogic covers SQLite's three-valued (Kleene) logic for
// NULLs in comparisons and boolean expressions.
func TestP1WhereNullLogic(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER, b INTEGER);
		INSERT INTO t VALUES(1, NULL);
		INSERT INTO t VALUES(NULL, 2);
		INSERT INTO t VALUES(NULL, NULL);
		INSERT INTO t VALUES(3, 4);
	`
	runSQL(t, db, setup)
	cases := []string{
		"SELECT count(*) FROM t WHERE a = a",
		"SELECT count(*) FROM t WHERE a = 1",
		"SELECT count(*) FROM t WHERE a <> 1",
		"SELECT count(*) FROM t WHERE NOT (a = 1)",
		"SELECT count(*) FROM t WHERE a = 1 OR b = 2",
		"SELECT count(*) FROM t WHERE a = 1 AND b = 2",
		"SELECT count(*) FROM t WHERE (a = 1 OR b = 2) AND a IS NOT NULL",
		"SELECT NULL = NULL, NULL <> 1, NOT NULL",
		"SELECT NULL OR 1, NULL OR 0, NULL AND 0, NULL AND 1",
	}
	for _, q := range cases {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1WhereIsNull covers IS NULL / IS NOT NULL and the NULL-safe IS /
// IS NOT operators, including the IS '' vs IS NULL distinction.
func TestP1WhereIsNull(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a TEXT);
		INSERT INTO t VALUES(NULL);
		INSERT INTO t VALUES('');
		INSERT INTO t VALUES('x');
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT a IS NULL FROM t",
		"SELECT a IS NOT NULL FROM t",
		"SELECT a IS '' FROM t",
		"SELECT count(*) FROM t WHERE a IS NULL",
		"SELECT count(*) FROM t WHERE a IS NOT NULL",
		"SELECT count(*) FROM t WHERE a IS ''",
		"SELECT count(*) FROM t WHERE a IS NOT ''",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1WhereBetween covers BETWEEN / NOT BETWEEN including NULL bounds and
// mixed types.
func TestP1WhereBetween(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER);
		INSERT INTO t VALUES(1),(5),(10),(NULL);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT count(*) FROM t WHERE a BETWEEN 1 AND 5",
		"SELECT count(*) FROM t WHERE a BETWEEN 1 AND 10",
		"SELECT count(*) FROM t WHERE a NOT BETWEEN 1 AND 5",
		"SELECT count(*) FROM t WHERE a BETWEEN 1 AND NULL",
		"SELECT count(*) FROM t WHERE a BETWEEN NULL AND 10",
		"SELECT a BETWEEN 1 AND 5 FROM t",
		"SELECT 5 BETWEEN 1 AND 10",
		"SELECT 5 NOT BETWEEN 1 AND 10",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1WhereIn covers IN with a scalar list, IN with a subquery, and NOT IN
// with NULL in the list (which makes the result unknown → no rows).
func TestP1WhereIn(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER);
		INSERT INTO t VALUES(1),(2),(3),(NULL);
		CREATE TABLE u(b INTEGER);
		INSERT INTO u VALUES(2),(3);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT count(*) FROM t WHERE a IN (1,2)",
		"SELECT count(*) FROM t WHERE a IN (1,2) OR a IS NULL",
		"SELECT count(*) FROM t WHERE a IN (SELECT b FROM u)",
		"SELECT count(*) FROM t WHERE a NOT IN (1)",
		"SELECT count(*) FROM t WHERE a NOT IN (1,NULL)",
		"SELECT count(*) FROM t WHERE a NOT IN (SELECT b FROM u)",
		"SELECT count(*) FROM t WHERE a IN (1,NULL)",
		"SELECT a IN (1,2) FROM t",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1WhereLike covers LIKE patterns (%, _), case-insensitive ASCII
// matching, ESCAPE, GLOB, and REGEXP error behavior.
func TestP1WhereLike(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a TEXT);
		INSERT INTO t VALUES('abc'),('ABC'),('a_c'),('xyz');
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT count(*) FROM t WHERE a LIKE 'abc'",
		"SELECT count(*) FROM t WHERE a LIKE 'ABC'",
		"SELECT count(*) FROM t WHERE a LIKE 'a%'",
		"SELECT count(*) FROM t WHERE a LIKE '_bc'",
		"SELECT count(*) FROM t WHERE a LIKE 'a_c'",
		"SELECT count(*) FROM t WHERE a LIKE 'a\\_c' ESCAPE '\\'",
		"SELECT count(*) FROM t WHERE a GLOB 'a*'",
		"SELECT count(*) FROM t WHERE a GLOB 'A*'",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1WhereCollate covers COLLATE in WHERE clauses overriding the column's
// declared collation (BINARY/NOCASE/RTRIM).
func TestP1WhereCollate(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a TEXT COLLATE NOCASE);
		INSERT INTO t VALUES('abc'),('ABC'),('xyz');
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT count(*) FROM t WHERE a = 'ABC'",
		"SELECT count(*) FROM t WHERE a = 'ABC' COLLATE BINARY",
		"SELECT count(*) FROM t WHERE a = 'ABC' COLLATE NOCASE",
		"SELECT count(*) FROM t WHERE a LIKE 'ABC'",
		"SELECT count(*) FROM t WHERE a < 'b'",
		"SELECT count(*) FROM t WHERE a < 'b' COLLATE BINARY",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestP1WherePrecedence covers AND/OR/NOT precedence and parenthesization.
func TestP1WherePrecedence(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `
		CREATE TABLE t(a INTEGER, b INTEGER, c INTEGER);
		INSERT INTO t VALUES(1,1,0),(1,0,1),(0,1,1),(0,0,0);
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT count(*) FROM t WHERE a=1 AND b=1 OR c=1",
		"SELECT count(*) FROM t WHERE a=1 AND (b=1 OR c=1)",
		"SELECT count(*) FROM t WHERE NOT a=1 AND b=1",
		"SELECT count(*) FROM t WHERE NOT (a=1 AND b=1)",
		"SELECT count(*) FROM t WHERE a=1 OR b=1 AND c=1",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}
