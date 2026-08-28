package frigolite

import (
	"strconv"
	"strings"
	"testing"
)

// floatCell renders a float64 the way SQLite's %!.15g formatting does: 15
// significant digits, fixed-point for exponents in range, always a decimal
// point (alternate form), exponential otherwise. Used to assert byte-for-byte
// float formatting parity with SQLite.
func floatCell(v float64) string {
	s := strconv.FormatFloat(v, 'g', 15, 64)
	if e := strings.IndexAny(s, "eE"); e >= 0 {
		mant := s[:e]
		if !strings.Contains(mant, ".") {
			mant += ".0"
		}
		return mant + s[e:]
	}
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// TestP1SelectProjection covers basic SELECT projection of columns, literals,
// and expressions, plus result-column names.
func TestP1SelectProjection(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec("CREATE TABLE t(a INTEGER, b TEXT, c REAL)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t VALUES(1,'one',1.5), (2,'two',2.5)"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	t.Run("column projection", func(t *testing.T) {
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || len(r.Rows[0]) != 2 {
			t.Fatalf("expected 2x2, got %v", r.Rows)
		}
		if r.Rows[0][0] != int64(1) || r.Rows[0][1] != "one" {
			t.Errorf("row0: %v", r.Rows[0])
		}
		if r.Rows[1][0] != int64(2) || r.Rows[1][1] != "two" {
			t.Errorf("row1: %v", r.Rows[1])
		}
	})
	t.Run("literal projection", func(t *testing.T) {
		r := db.Query("SELECT 42, 'hello', NULL")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 3 {
			t.Fatalf("expected 1x3, got %v", r.Rows)
		}
		if r.Rows[0][0] != int64(42) || r.Rows[0][1] != "hello" || r.Rows[0][2] != nil {
			t.Errorf("literal row: %v", r.Rows[0])
		}
	})
	t.Run("expression projection", func(t *testing.T) {
		r := db.Query("SELECT a+10, b || '!' FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(11) || r.Rows[0][1] != "one!" {
			t.Errorf("expr row0: %v", r.Rows[0])
		}
		if r.Rows[1][0] != int64(12) || r.Rows[1][1] != "two!" {
			t.Errorf("expr row1: %v", r.Rows[1])
		}
	})
}

// TestP1SelectStar covers * and t.* expansion, including qualified stars in
// multi-table queries.
func TestP1SelectStar(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec("CREATE TABLE t1(a, b); CREATE TABLE t2(c, d)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t1 VALUES(1,'x'); INSERT INTO t2 VALUES(1,'y'), (2,'z')"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	t.Run("star", func(t *testing.T) {
		r := db.Query("SELECT * FROM t1")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Columns) != 2 || r.Columns[0] != "a" || r.Columns[1] != "b" {
			t.Errorf("columns: %v", r.Columns)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "x" {
			t.Errorf("row: %v", r.Rows)
		}
	})
	t.Run("qualified star", func(t *testing.T) {
		r := db.Query("SELECT t2.* FROM t1 JOIN t2 ON t1.a=t2.c")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 2 {
			t.Fatalf("expected 1x2, got %v", r.Rows)
		}
		if r.Rows[0][0] != int64(1) || r.Rows[0][1] != "y" {
			t.Errorf("t2.* row: %v", r.Rows[0])
		}
	})
	t.Run("star with columns", func(t *testing.T) {
		r := db.Query("SELECT t1.*, t2.c FROM t1 JOIN t2 ON t1.a=t2.c")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows[0]) != 3 {
			t.Errorf("expected 3 cols, got %v", r.Rows[0])
		}
	})
	t.Run("aggregate with star", func(t *testing.T) {
		// SELECT count(*), * collapses to one row: the aggregate plus the
		// star's underlying columns from the first row (SQLite semantics).
		r := db.Query("SELECT count(*), * FROM t1")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Columns) != 3 || r.Columns[0] != "count(*)" || r.Columns[1] != "a" || r.Columns[2] != "b" {
			t.Errorf("columns: %v", r.Columns)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 3 {
			t.Fatalf("expected 1x3, got %v", r.Rows)
		}
		if r.Rows[0][0] != int64(1) || r.Rows[0][1] != int64(1) || r.Rows[0][2] != "x" {
			t.Errorf("row: %v", r.Rows[0])
		}
	})
	t.Run("aggregate with star empty", func(t *testing.T) {
		// Over zero rows the aggregate row still has the star's columns, all
		// NULL.
		r := db.Query("SELECT sum(b), * FROM t1 WHERE 0")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 3 {
			t.Fatalf("expected 1x3, got %v", r.Rows)
		}
		if r.Rows[0][0] != nil || r.Rows[0][1] != nil || r.Rows[0][2] != nil {
			t.Errorf("expected NULLs, got %v", r.Rows[0])
		}
	})
	t.Run("group by nocase collation", func(t *testing.T) {
		if res := db.Exec("CREATE TABLE gb(a COLLATE nocase); INSERT INTO gb VALUES('abc'),('aBC'),('Def'),('dEF')"); res.Error != nil {
			t.Fatalf("setup: %v", res.Error)
		}
		defer db.Exec("DROP TABLE gb")
		r := db.Query("SELECT count(*) FROM gb GROUP BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[1][0] != int64(2) {
			t.Errorf("expected two groups of 2, got %v", r.Rows)
		}
		// Unary plus must preserve the column's collation for grouping too.
		r2 := db.Query("SELECT count(*) FROM gb GROUP BY +a")
		if r2.Error != nil {
			t.Fatalf("select: %v", r2.Error)
		}
		if len(r2.Rows) != 2 {
			t.Errorf("expected two groups, got %v", r2.Rows)
		}
	})
}

// TestP1SelectAlias covers result-column aliases and their use in ORDER BY.
func TestP1SelectAlias(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec("CREATE TABLE t(a INTEGER, b TEXT)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t VALUES(3,'c'), (1,'a'), (2,'b')"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	t.Run("AS alias", func(t *testing.T) {
		r := db.Query("SELECT a AS x, b AS y FROM t ORDER BY x")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Columns[0] != "x" || r.Columns[1] != "y" {
			t.Errorf("columns: %v", r.Columns)
		}
		if r.Rows[0][0] != int64(1) || r.Rows[2][0] != int64(3) {
			t.Errorf("alias order: %v", r.Rows)
		}
	})
	t.Run("bare alias", func(t *testing.T) {
		r := db.Query("SELECT a x, b y FROM t ORDER BY x")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Columns[0] != "x" {
			t.Errorf("bare alias columns: %v", r.Columns)
		}
	})
	t.Run("alias in ORDER BY", func(t *testing.T) {
		r := db.Query("SELECT a AS val FROM t ORDER BY val DESC")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(3) {
			t.Errorf("alias DESC: %v", r.Rows[0])
		}
	})
}

// TestP1SelectDistinct covers DISTINCT and DISTINCT NULL handling (NULLs are
// equal for DISTINCT).
func TestP1SelectDistinct(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec("CREATE TABLE t(a INTEGER, b TEXT)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t VALUES(1,'x'), (1,'x'), (2,'y'), (NULL,'z'), (NULL,'z')"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	r := db.Query("SELECT DISTINCT a FROM t ORDER BY a")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	// DISTINCT collapses (1,1) and (NULL,NULL); order: NULL, 1, 2.
	if len(r.Rows) != 3 {
		t.Fatalf("expected 3 distinct rows, got %v", r.Rows)
	}
	if r.Rows[0][0] != nil || r.Rows[1][0] != int64(1) || r.Rows[2][0] != int64(2) {
		t.Errorf("distinct rows: %v", r.Rows)
	}
	// DISTINCT on full rows: (1,x) duplicated, (NULL,z) duplicated.
	r2 := db.Query("SELECT DISTINCT a, b FROM t")
	if r2.Error != nil {
		t.Fatalf("select2: %v", r2.Error)
	}
	if len(r2.Rows) != 3 {
		t.Errorf("expected 3 distinct full rows, got %v", r2.Rows)
	}
}

// TestP1SelectOrderBy covers ORDER BY with multiple keys, ASC/DESC, NULLS
// FIRST/LAST, expression keys, and COLLATE.
func TestP1SelectOrderBy(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec("CREATE TABLE t(a INTEGER, b TEXT)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t VALUES(2,'b'), (1,'a'), (NULL,'c'), (1,'A'), (3,'d')"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	t.Run("multiple keys", func(t *testing.T) {
		r := db.Query("SELECT a, b FROM t ORDER BY a, b")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// NULL first (default), then a=1 (A before a), a=2, a=3.
		if r.Rows[0][1] != "c" || r.Rows[1][1] != "A" || r.Rows[2][1] != "a" {
			t.Errorf("multi-key order: %v", r.Rows)
		}
	})
	t.Run("NULLS FIRST", func(t *testing.T) {
		r := db.Query("SELECT a FROM t ORDER BY a NULLS FIRST")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != nil {
			t.Errorf("NULLS FIRST: %v", r.Rows[0])
		}
	})
	t.Run("NULLS LAST", func(t *testing.T) {
		r := db.Query("SELECT a FROM t ORDER BY a NULLS LAST")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		last := r.Rows[len(r.Rows)-1][0]
		if last != nil {
			t.Errorf("NULLS LAST: last=%v", last)
		}
	})
	t.Run("expression key", func(t *testing.T) {
		r := db.Query("SELECT a, b FROM t WHERE a IS NOT NULL ORDER BY a*a, b")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 4 {
			t.Fatalf("expected 4 rows, got %v", r.Rows)
		}
		// a*a: 1, 1, 4, 9 → (1,A),(1,a) tie on a*a, then (2,b), (3,d).
		if r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(1) || r.Rows[2][0] != int64(2) || r.Rows[3][0] != int64(3) {
			t.Errorf("expr order: %v", r.Rows)
		}
	})
	t.Run("COLLATE", func(t *testing.T) {
		r := db.Query("SELECT b FROM t ORDER BY b COLLATE NOCASE DESC")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// NOCASE DESC: d, c, b, a, A (a/A equal)
		if r.Rows[0][0] != "d" {
			t.Errorf("collate order: %v", r.Rows)
		}
	})
	t.Run("ordinal", func(t *testing.T) {
		r := db.Query("SELECT b, a FROM t ORDER BY 2 DESC")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// Column 2 is a: DESC → 3 first
		if r.Rows[0][0] != "d" {
			t.Errorf("ordinal order: %v", r.Rows)
		}
	})
}

// TestP1SelectLimitOffset covers LIMIT, LIMIT with OFFSET, and LIMIT -1.
func TestP1SelectLimitOffset(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec("CREATE TABLE t(a INTEGER)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t VALUES(1),(2),(3),(4),(5)"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	t.Run("LIMIT n", func(t *testing.T) {
		r := db.Query("SELECT a FROM t ORDER BY a LIMIT 2")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(2) {
			t.Errorf("limit: %v", r.Rows)
		}
	})
	t.Run("LIMIT n OFFSET m", func(t *testing.T) {
		r := db.Query("SELECT a FROM t ORDER BY a LIMIT 2 OFFSET 2")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(3) || r.Rows[1][0] != int64(4) {
			t.Errorf("limit offset: %v", r.Rows)
		}
	})
	t.Run("LIMIT -1", func(t *testing.T) {
		r := db.Query("SELECT a FROM t ORDER BY a LIMIT -1")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 5 {
			t.Errorf("limit -1: %v", r.Rows)
		}
	})
	t.Run("OFFSET only", func(t *testing.T) {
		r := db.Query("SELECT a FROM t ORDER BY a LIMIT -1 OFFSET 3")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(4) {
			t.Errorf("offset: %v", r.Rows)
		}
	})
}

// TestP1SelectNoFrom covers FROM-less SELECT and SELECT ... WHERE 0.
func TestP1SelectNoFrom(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	t.Run("FROM-less literals", func(t *testing.T) {
		r := db.Query("SELECT 1, 'two', 3.5")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "two" {
			t.Errorf("no-from row: %v", r.Rows)
		}
	})
	t.Run("WHERE 0", func(t *testing.T) {
		r := db.Query("SELECT 1 WHERE 0")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 0 {
			t.Errorf("WHERE 0 should be empty, got %v", r.Rows)
		}
	})
	t.Run("WHERE 1", func(t *testing.T) {
		r := db.Query("SELECT 7 WHERE 1")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(7) {
			t.Errorf("WHERE 1: %v", r.Rows)
		}
	})
}

// TestP1SelectFloatFormat verifies byte-for-byte float formatting parity with
// SQLite's %!.15g output. The values are compared through the same rendering
// the test harness uses (tclRenderCell), which matches SQLite exactly.
func TestP1SelectFloatFormat(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT 1.0", "1.0"},
		{"SELECT 0.1+0.2", "0.3"},
		{"SELECT 3.14159", "3.14159"},
		{"SELECT 1e10", "10000000000.0"},
		{"SELECT 1e300", "1.0e+300"},
		{"SELECT 1e-300", "1.0e-300"},
		{"SELECT 123456789.123456789", "123456789.123457"},
		{"SELECT 0.5", "0.5"},
		{"SELECT -2.5", "-2.5"},
	}
	for _, c := range cases {
		r := db.Query(c.sql)
		if r.Error != nil {
			t.Errorf("%s: %v", c.sql, r.Error)
			continue
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
			t.Errorf("%s: expected 1x1, got %v", c.sql, r.Rows)
			continue
		}
		f, ok := r.Rows[0][0].(float64)
		if !ok {
			t.Errorf("%s: expected float64, got %T (%v)", c.sql, r.Rows[0][0], r.Rows[0][0])
			continue
		}
		got := floatCell(f)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.sql, got, c.want)
		}
	}
}

// TestP1SelectConcat covers || string concatenation in projections.
func TestP1SelectConcat(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec("CREATE TABLE t(a TEXT, b TEXT)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t VALUES('foo','bar'), (NULL,'baz')"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	r := db.Query("SELECT a || b, a || '-' || b FROM t ORDER BY 1")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	if len(r.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %v", r.Rows)
	}
	// NULL || 'baz' is NULL; 'foo' || 'bar' is 'foobar'.
	if r.Rows[0][0] != nil {
		t.Errorf("NULL concat: %v", r.Rows[0])
	}
	if r.Rows[1][0] != "foobar" || r.Rows[1][1] != "foo-bar" {
		t.Errorf("concat: %v", r.Rows[1])
	}
}

// TestP1SelectErrors covers SELECT error messages matching SQLite.
func TestP1SelectErrors(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec("CREATE TABLE t(a INTEGER, b TEXT)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	t.Run("no such column", func(t *testing.T) {
		r := db.Query("SELECT missing FROM t")
		if r.Error == nil || !strings.Contains(r.Error.Error(), "no such column: missing") {
			t.Errorf("expected no such column, got: %v", r.Error)
		}
	})
	t.Run("no such table", func(t *testing.T) {
		r := db.Query("SELECT * FROM nosuch")
		if r.Error == nil || !strings.Contains(r.Error.Error(), "no such table: nosuch") {
			t.Errorf("expected no such table, got: %v", r.Error)
		}
	})
	t.Run("ORDER BY out of range", func(t *testing.T) {
		r := db.Query("SELECT a FROM t ORDER BY 5")
		if r.Error == nil || !strings.Contains(r.Error.Error(), "ORDER BY term out of range") {
			t.Errorf("expected out of range, got: %v", r.Error)
		}
	})
}

// TestP1SelectJoinOn covers comma joins with an explicit ON clause and
// collation-aware DISTINCT over joined tables.
func TestP1SelectJoinOn(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	res := db.Exec("CREATE TABLE h1(a, b); INSERT INTO h1 VALUES(1, 'one'); INSERT INTO h1 VALUES(1, 'I'); INSERT INTO h1 VALUES(1, 'i'); INSERT INTO h1 VALUES(4, 'four'); INSERT INTO h1 VALUES(4, 'IV'); INSERT INTO h1 VALUES(4, 'iv'); CREATE TABLE h2(x COLLATE nocase); INSERT INTO h2 VALUES('One'); INSERT INTO h2 VALUES('Two'); INSERT INTO h2 VALUES('Three'); INSERT INTO h2 VALUES('Four'); INSERT INTO h2 VALUES('one'); INSERT INTO h2 VALUES('two'); INSERT INTO h2 VALUES('three'); INSERT INTO h2 VALUES('four');")
	if res.Error != nil {
		t.Fatalf("setup: %v", res.Error)
	}
	t.Run("comma join with ON filters rows", func(t *testing.T) {
		// FROM a, b ON (...) is an inner join; the ON must filter (not a
		// cross product).
		r := db.Query("SELECT x FROM h1, h2 ON (x=b)")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		got := flattenResult(r)
		if got != "One one Four four" {
			t.Errorf("got [%s] want [One one Four four]", got)
		}
	})
	t.Run("distinct respects joined column collation", func(t *testing.T) {
		// x is COLLATE nocase, so 'One' and 'one' dedupe.
		r := db.Query("SELECT DISTINCT x FROM h1, h2 ON (x=b)")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		got := flattenResult(r)
		if got != "One Four" {
			t.Errorf("got [%s] want [One Four]", got)
		}
	})
}

// TestP1SelectHavingMinMaxBare covers a bare output column whose source row is
// determined by a min/max aggregate in the HAVING clause (SQLite evaluates the
// bare expression on the row that produced the extreme).
func TestP1SelectHavingMinMaxBare(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	res := db.Exec("CREATE TABLE c1(up, down); INSERT INTO c1 VALUES('x', 1); INSERT INTO c1 VALUES('x', 2); INSERT INTO c1 VALUES('x', 4); INSERT INTO c1 VALUES('x', 8); INSERT INTO c1 VALUES('y', 16); INSERT INTO c1 VALUES('y', 32);")
	if res.Error != nil {
		t.Fatalf("setup: %v", res.Error)
	}
	r := db.Query("SELECT up||down FROM c1 GROUP BY (down<5) HAVING max(down)<10")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	got := flattenResult(r)
	if got != "x4" {
		t.Errorf("got [%s] want [x4]", got)
	}
}

// TestP1SelectUnionCollation covers compound UNION dedup collation resolution:
// the first member with a defined collation (including BINARY) determines the
// compound's collation, and a later member's explicit COLLATE wins when earlier
// members are bare literals.
func TestP1SelectUnionCollation(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, tc := range []struct {
		sql  string
		want string
	}{
		{"SELECT 'abc' COLLATE nocase UNION SELECT 'ABC'", "ABC"},
		{"SELECT 'abc' UNION SELECT 'ABC' COLLATE nocase", "ABC"},
		{"SELECT 'abc' COLLATE binary UNION SELECT 'ABC' COLLATE nocase", "ABC abc"},
		{"SELECT 'abc' COLLATE nocase UNION SELECT 'ABC' COLLATE binary", "ABC"},
	} {
		r := db.Query(tc.sql)
		if r.Error != nil {
			t.Errorf("query %q error: %v", tc.sql, r.Error)
			continue
		}
		got := flattenResult(r)
		if got != tc.want {
			t.Errorf("sql %q: got [%s] want [%s]", tc.sql, got, tc.want)
		}
	}
	// A column declared COLLATE binary in the first member stops the search
	// at BINARY, so 'Abc' and 'abc' do NOT dedupe.
	res := db.Exec("CREATE TABLE y1(a COLLATE nocase, b COLLATE binary); INSERT INTO y1 VALUES('Abc', 'abc')")
	if res.Error != nil {
		t.Fatalf("setup: %v", res.Error)
	}
	r := db.Query("SELECT b FROM y1 UNION SELECT a FROM y1")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	if got := flattenResult(r); got != "Abc abc" {
		t.Errorf("column binary: got [%s] want [Abc abc]", got)
	}
}

// TestP1SelectTempTableJoin covers joining a TEMP-schema table in a comma
// cross join (selectD regression): the empty-join short-circuit must resolve
// the row count through the temp database's own pager/schema, not the main
// schema manager.
func TestP1SelectTempTableJoin(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	res := db.Exec(`
		CREATE TABLE t1(a,b); INSERT INTO t1 VALUES(111,'x1');
		CREATE TABLE t2(a,b); INSERT INTO t2 VALUES(222,'x2');
		CREATE TEMP TABLE t3(a,b); INSERT INTO t3 VALUES(333,'x3');
		CREATE TABLE t4(a,b); INSERT INTO t4 VALUES(444,'x4');
	`)
	if res.Error != nil {
		t.Fatalf("setup: %v", res.Error)
	}
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT * FROM t1, t3", "111 x1 333 x3"},
		{"SELECT * FROM t1, t2, t3", "111 x1 222 x2 333 x3"},
		{"SELECT * FROM (t1), (t2), (t3), (t4) WHERE t4.a=t3.a+111 AND t3.a=t2.a+111 AND t2.a=t1.a+111", "111 x1 222 x2 333 x3 444 x4"},
	}
	for _, tc := range cases {
		r := db.Query(tc.sql)
		if r.Error != nil {
			t.Errorf("query %q error: %v", tc.sql, r.Error)
			continue
		}
		if got := flattenResult(r); got != tc.want {
			t.Errorf("sql %q: got [%s] want [%s]", tc.sql, got, tc.want)
		}
	}
}
