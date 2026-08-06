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
