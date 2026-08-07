package frigolite

import (
	"strings"
	"testing"
)

// P3 pre-tests: hand-written tests for G3.COLLATE collation support, written
// BEFORE running the TCL testgen packages (collate, collateA, collateB). Each
// expectation was verified against sqlite3 3.51 (via the sqlite3 CLI) as the
// oracle; the tests document the exact SQLite semantics frigolite must match:
// BINARY (byte compare), NOCASE (ASCII case-insensitive), RTRIM (ignore
// trailing spaces), column-level COLLATE declarations, explicit COLLATE in
// expressions (overriding column collation), collation precedence (explicit >
// column; leftmost explicit wins), COLLATE in ORDER BY, LIKE always
// ASCII-case-insensitive, and = case-insensitive under NOCASE.

// TestP3Collate is the top-level entry for the P3 COLLATE pre-tests. The
// verify command runs it via `go test -run TestP3Collate -count=1 .`
func TestP3Collate(t *testing.T) {
	for _, sub := range []string{
		"Basics", "ColumnCollate", "ExprPrecedence",
		"OrderBy", "Like", "Propagation",
	} {
		ok := t.Run(sub, func(t *testing.T) {
			switch sub {
			case "Basics":
				TestP3Collate_Basics(t)
			case "ColumnCollate":
				TestP3Collate_ColumnCollate(t)
			case "ExprPrecedence":
				TestP3Collate_ExprPrecedence(t)
			case "OrderBy":
				TestP3Collate_OrderBy(t)
			case "Like":
				TestP3Collate_Like(t)
			case "Propagation":
				TestP3Collate_Propagation(t)
			}
		})
		if !ok {
			t.Fail()
		}
	}
}

// TestP3Collate_Basics covers the three built-in collations in comparison
// expressions: BINARY is byte-wise, NOCASE is ASCII-case-insensitive, RTRIM
// ignores trailing spaces.
func TestP3Collate_Basics(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// BINARY default: 'a' != 'A', 'a' != 'a  '.
	if got := flattenQuery(t, db, "SELECT 'a' = 'A';"); got != "0" {
		t.Errorf("BINARY 'a'='A': got %s, want 0", got)
	}
	if got := flattenQuery(t, db, "SELECT 'a' = 'a  ';"); got != "0" {
		t.Errorf("BINARY 'a'='a  ': got %s, want 0", got)
	}

	// NOCASE makes = case-insensitive.
	if got := flattenQuery(t, db, "SELECT 'a' = 'A' COLLATE NOCASE;"); got != "1" {
		t.Errorf("NOCASE 'a'='A': got %s, want 1", got)
	}
	// Ordering: 'a' < 'B' under NOCASE (a<b), 'A' not < 'a'.
	if got := flattenQuery(t, db, "SELECT 'a' < 'B' COLLATE NOCASE;"); got != "1" {
		t.Errorf("NOCASE 'a'<'B': got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT 'A' < 'a' COLLATE NOCASE;"); got != "0" {
		t.Errorf("NOCASE 'A'<'a': got %s, want 0", got)
	}
	if got := flattenQuery(t, db, "SELECT 'b' > 'A' COLLATE NOCASE;"); got != "1" {
		t.Errorf("NOCASE 'b'>'A': got %s, want 1", got)
	}

	// RTRIM ignores trailing spaces (both directions).
	if got := flattenQuery(t, db, "SELECT 'a' = 'a  ' COLLATE RTRIM;"); got != "1" {
		t.Errorf("RTRIM 'a'='a  ': got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT 'a  ' = 'a' COLLATE RTRIM;"); got != "1" {
		t.Errorf("RTRIM 'a  '='a': got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT '  ' = '' COLLATE RTRIM;"); got != "1" {
		t.Errorf("RTRIM '  '='': got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT '  ' = '' COLLATE BINARY;"); got != "0" {
		t.Errorf("BINARY '  '='': got %s, want 0", got)
	}
	if got := flattenQuery(t, db, "SELECT '  ' = '      ' COLLATE RTRIM;"); got != "1" {
		t.Errorf("RTRIM '  '='      ': got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT ''<>'  ' COLLATE RTRIM;"); got != "0" {
		t.Errorf("RTRIM ''<>'  ': got %s, want 0", got)
	}

	// NOCASE works on bare literals without a column.
	if got := flattenQuery(t, db, "SELECT 'X' = 'x' COLLATE NOCASE;"); got != "1" {
		t.Errorf("NOCASE 'X'='x': got %s, want 1", got)
	}
}

// TestP3Collate_ColumnCollate covers column-level COLLATE declarations: the
// declared collation applies to comparisons against literals and to ORDER BY.
func TestP3Collate_ColumnCollate(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "CREATE TABLE t1(a TEXT COLLATE NOCASE, b TEXT COLLATE RTRIM, c TEXT COLLATE BINARY);")
	must(t, db, "INSERT INTO t1 VALUES('Hello','x  ','y');")
	must(t, db, "INSERT INTO t1 VALUES('hello','x','Y');")

	// Column collation applies to = with a literal.
	if got := flattenQuery(t, db, "SELECT count(*) FROM t1 WHERE a='HELLO';"); got != "2" {
		t.Errorf("NOCASE column a='HELLO': got %s, want 2", got)
	}
	// RTRIM column: 'x' matches 'x  ' (trailing spaces ignored).
	if got := flattenQuery(t, db, "SELECT count(*) FROM t1 WHERE b='x';"); got != "2" {
		t.Errorf("RTRIM column b='x': got %s, want 2", got)
	}
	// BINARY column: 'y' vs 'Y' differ.
	if got := flattenQuery(t, db, "SELECT count(*) FROM t1 WHERE c='y';"); got != "1" {
		t.Errorf("BINARY column c='y': got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT count(*) FROM t1 WHERE c='Y';"); got != "1" {
		t.Errorf("BINARY column c='Y': got %s, want 1", got)
	}

	// Column collation applies to ORDER BY.
	if got := flattenQuery(t, db, "SELECT a FROM t1 ORDER BY a;"); got != "Hello hello" {
		t.Errorf("ORDER BY NOCASE column: got [%s], want [Hello hello]", got)
	}
	if got := flattenQuery(t, db, "SELECT b FROM t1 ORDER BY b;"); got != "x x  " {
		t.Errorf("ORDER BY RTRIM column: got [%s], want [x x  ]", got)
	}
	if got := flattenQuery(t, db, "SELECT c FROM t1 ORDER BY c;"); got != "Y y" {
		t.Errorf("ORDER BY BINARY column: got [%s], want [Y y]", got)
	}

	// Explicit COLLATE in ORDER BY overrides the column collation.
	if got := flattenQuery(t, db, "SELECT c FROM t1 ORDER BY c COLLATE NOCASE;"); got != "y Y" {
		t.Errorf("ORDER BY c COLLATE NOCASE: got [%s], want [y Y]", got)
	}
	if got := flattenQuery(t, db, "SELECT c FROM t1 ORDER BY c COLLATE NOCASE DESC;"); got != "y Y" {
		t.Errorf("ORDER BY c COLLATE NOCASE DESC: got [%s], want [y Y]", got)
	}
	// NOCASE column with explicit BINARY ORDER BY: binary order (Y before y).
	if got := flattenQuery(t, db, "SELECT a FROM t1 ORDER BY a COLLATE BINARY;"); got != "Hello hello" {
		t.Errorf("ORDER BY NOCASE column COLLATE BINARY: got [%s], want [Hello hello]", got)
	}
}

// TestP3Collate_ExprPrecedence covers SQLite's collation precedence rules:
// an explicit COLLATE on either operand wins over column collation, and when
// both sides are explicit the left one wins.
func TestP3Collate_ExprPrecedence(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "CREATE TABLE t2(x TEXT COLLATE NOCASE);")
	must(t, db, "INSERT INTO t2 VALUES('abc');")
	must(t, db, "INSERT INTO t2 VALUES('ABC');")

	// Column collation (NOCASE) applies to = with a literal.
	if got := flattenQuery(t, db, "SELECT count(*) FROM t2 WHERE x='abc';"); got != "2" {
		t.Errorf("NOCASE column x='abc': got %s, want 2", got)
	}
	// Explicit BINARY on the right overrides the column's NOCASE.
	if got := flattenQuery(t, db, "SELECT count(*) FROM t2 WHERE x='abc' COLLATE BINARY;"); got != "1" {
		t.Errorf("x='abc' COLLATE BINARY: got %s, want 1", got)
	}
	// Explicit BINARY on the right still applies when left is a literal.
	if got := flattenQuery(t, db, "SELECT count(*) FROM t2 WHERE 'abc'=x COLLATE BINARY;"); got != "1" {
		t.Errorf("'abc'=x COLLATE BINARY: got %s, want 1", got)
	}

	// Leftmost explicit wins when both sides are explicit.
	if got := flattenQuery(t, db, "SELECT 'a' COLLATE NOCASE = 'A' COLLATE BINARY;"); got != "1" {
		t.Errorf("left explicit NOCASE wins: got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT 'a' COLLATE BINARY = 'A' COLLATE NOCASE;"); got != "0" {
		t.Errorf("left explicit BINARY wins: got %s, want 0", got)
	}
	// Double COLLATE on one side: the outermost (rightmost) applies.
	if got := flattenQuery(t, db, "SELECT 'a' = 'A' COLLATE NOCASE COLLATE BINARY;"); got != "0" {
		t.Errorf("double COLLATE rightmost BINARY: got %s, want 0", got)
	}
	// NOCASE via COLLATE on a column reference propagates case-insensitivity.
	if got := flattenQuery(t, db, "SELECT 'abc' = x COLLATE NOCASE FROM t2 WHERE x='abc' LIMIT 1;"); got != "1" {
		t.Errorf("x COLLATE NOCASE: got %s, want 1", got)
	}
}

// TestP3Collate_OrderBy covers COLLATE in ORDER BY terms: positional,
// expression, and column references, plus DESC with collation.
func TestP3Collate_OrderBy(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "CREATE TABLE p(a,b);")
	must(t, db, "INSERT INTO p VALUES('z','B'),('a','A'),('m','c');")

	// Positional ORDER BY with COLLATE applies the collation to the column.
	if got := flattenQuery(t, db, "SELECT a FROM p ORDER BY 1 COLLATE NOCASE;"); got != "a m z" {
		t.Errorf("ORDER BY 1 COLLATE NOCASE: got [%s], want [a m z]", got)
	}
	if got := flattenQuery(t, db, "SELECT b FROM p ORDER BY 1 COLLATE NOCASE;"); got != "A B c" {
		t.Errorf("ORDER BY 1 COLLATE NOCASE (b): got [%s], want [A B c]", got)
	}
	// BINARY order of b: A B c (65 < 66 < 99).
	if got := flattenQuery(t, db, "SELECT b FROM p ORDER BY b;"); got != "A B c" {
		t.Errorf("ORDER BY b BINARY: got [%s], want [A B c]", got)
	}
	// DESC with NOCASE reverses.
	if got := flattenQuery(t, db, "SELECT a FROM p ORDER BY a COLLATE NOCASE DESC;"); got != "z m a" {
		t.Errorf("ORDER BY a COLLATE NOCASE DESC: got [%s], want [z m a]", got)
	}

	// Index with explicit COLLATE in the key.
	must(t, db, "CREATE INDEX pi ON p(b COLLATE NOCASE);")
	// The index's collation does not change a plain ORDER BY b (column
	// default BINARY), matching SQLite when the index is not selected for
	// the ordering; an explicit COLLATE NOCASE in ORDER BY does apply.
	if got := flattenQuery(t, db, "SELECT b FROM p ORDER BY b COLLATE NOCASE;"); got != "A B c" {
		t.Errorf("ORDER BY b COLLATE NOCASE with index: got [%s], want [A B c]", got)
	}
}

// TestP3Collate_Like covers LIKE: it is always ASCII-case-insensitive on the
// ASCII letters, regardless of the column's collation.
func TestP3Collate_Like(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "CREATE TABLE t3(v TEXT);")
	must(t, db, "INSERT INTO t3 VALUES('abc'),('ABC'),('AbC');")
	must(t, db, "CREATE TABLE t4(v TEXT COLLATE BINARY);")
	must(t, db, "INSERT INTO t4 VALUES('abc'),('ABC');")

	// LIKE matches case-insensitively on a plain column.
	if got := flattenQuery(t, db, "SELECT count(*) FROM t3 WHERE v LIKE 'abc';"); got != "3" {
		t.Errorf("LIKE 'abc' (default collation): got %s, want 3", got)
	}
	if got := flattenQuery(t, db, "SELECT count(*) FROM t3 WHERE v LIKE 'ABC';"); got != "3" {
		t.Errorf("LIKE 'ABC' (default collation): got %s, want 3", got)
	}
	// LIKE stays case-insensitive even on a BINARY column.
	if got := flattenQuery(t, db, "SELECT count(*) FROM t4 WHERE v LIKE 'abc';"); got != "2" {
		t.Errorf("LIKE 'abc' on BINARY column: got %s, want 2", got)
	}
	// = on the BINARY column is case-sensitive (contrast with LIKE).
	if got := flattenQuery(t, db, "SELECT count(*) FROM t4 WHERE v='abc';"); got != "1" {
		t.Errorf("= 'abc' on BINARY column: got %s, want 1", got)
	}
	// = on a NOCASE column is case-insensitive.
	must(t, db, "CREATE TABLE t5(v TEXT COLLATE NOCASE);")
	must(t, db, "INSERT INTO t5 VALUES('abc'),('ABC');")
	if got := flattenQuery(t, db, "SELECT count(*) FROM t5 WHERE v='abc';"); got != "2" {
		t.Errorf("= 'abc' on NOCASE column: got %s, want 2", got)
	}
}

// TestP3Collate_Propagation covers the compile-time collation propagation
// through expression structures: the COLLATE operator on an operand of a
// function call (upper/max), || concatenation, and CASE expressions affects
// the collation used by an enclosing comparison (collate8 semantics).
func TestP3Collate_Propagation(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// COLLATE on a || operand propagates to the concatenation result.
	if got := flattenQuery(t, db, "SELECT 'abc'==('ABC'||'') COLLATE nocase;"); got != "1" {
		t.Errorf("('ABC'||'') COLLATE nocase: got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT 'abc'==('ABC'||'' COLLATE nocase);"); got != "1" {
		t.Errorf("('ABC'||'' COLLATE nocase): got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT 'abc'==('ABC'||('' COLLATE nocase));"); got != "1" {
		t.Errorf("('ABC'||('' COLLATE nocase)): got %s, want 1", got)
	}
	// COLLATE propagates through a function call (upper).
	if got := flattenQuery(t, db, "SELECT 'abc'==('ABC'||upper('' COLLATE nocase));"); got != "1" {
		t.Errorf("upper('' COLLATE nocase): got %s, want 1", got)
	}
	// max() propagates the leftmost explicit collation.
	if got := flattenQuery(t, db, "SELECT 'abc'==('ABC'||max('' COLLATE nocase,'' COLLATE binary));"); got != "1" {
		t.Errorf("max(nocase, binary): got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT 'abc'==('ABC'||max('' COLLATE binary,'' COLLATE nocase));"); got != "0" {
		t.Errorf("max(binary, nocase): got %s, want 0", got)
	}
	// CASE: the compile-time collation comes from the THEN branch (checked
	// before ELSE) even when the ELSE branch is taken at runtime.
	if got := flattenQuery(t, db, "SELECT 'abc'==('ABC'||CASE WHEN 1-1=2 THEN '' COLLATE nocase ELSE '' COLLATE binary END);"); got != "1" {
		t.Errorf("CASE THEN nocase ELSE binary (false WHEN): got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT 'abc'==('ABC'||CASE WHEN 1+1=2 THEN '' COLLATE nocase ELSE '' COLLATE binary END);"); got != "1" {
		t.Errorf("CASE THEN nocase ELSE binary (true WHEN): got %s, want 1", got)
	}
	if got := flattenQuery(t, db, "SELECT 'abc'==('ABC'||CASE WHEN 1=2 THEN '' COLLATE binary ELSE '' COLLATE nocase END);"); got != "0" {
		t.Errorf("CASE THEN binary ELSE nocase: got %s, want 0", got)
	}

	// UNION ALL subquery: the union result column inherits the leftmost
	// member's column collation for an outer WHERE comparison (collate5 5.1).
	must(t, db, "CREATE TABLE t1(a, b COLLATE nocase);")
	must(t, db, "CREATE TABLE t2(c, d);")
	must(t, db, "INSERT INTO t2 VALUES(1, 'bbb');")
	if got := flattenQuery(t, db, "SELECT * FROM (SELECT a, b FROM t1 UNION ALL SELECT c, d FROM t2) WHERE b='BbB';"); got != "1 bbb" {
		t.Errorf("UNION ALL outer WHERE with NOCASE: got [%s], want [1 bbb]", got)
	}
}

// ensure the strings import is used by this file's helper-free body (guards
// against accidental removal in future edits).
var _ = strings.TrimSpace
