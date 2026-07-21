package frigolite

import (
	"fmt"
	"testing"
)

// TestSelectWhere tests WHERE clause with > comparison (mirrors where.test patterns)
func TestSelectWhere(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, 10)")
	db.Exec("INSERT INTO t VALUES (2, 20)")
	db.Exec("INSERT INTO t VALUES (3, 30)")

	res := db.Query("SELECT * FROM t WHERE val > 15")
	if res.Error != nil {
		t.Fatalf("SELECT WHERE: %v", res.Error)
	}
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows for val > 15, got %d", len(res.Rows))
	}
}

// TestLikeOperator tests LIKE operator (mirrors like.test patterns)
func TestLikeOperator(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (name TEXT)")
	db.Exec("INSERT INTO t VALUES ('Alice')")
	db.Exec("INSERT INTO t VALUES ('Bob')")
	db.Exec("INSERT INTO t VALUES ('Charlie')")

	res := db.Query("SELECT * FROM t WHERE name LIKE 'A%'")
	if res.Error != nil {
		t.Fatalf("LIKE: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row for LIKE 'A%%', got %d", len(res.Rows))
	}
}

// TestNullValues tests IS NULL (mirrors null.test patterns)
func TestNullValues(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, NULL)")

	res := db.Query("SELECT * FROM t WHERE name IS NULL")
	if res.Error != nil {
		t.Fatalf("SELECT IS NULL: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row for IS NULL, got %d", len(res.Rows))
	}
}

// TestIsNotNull tests IS NOT NULL (mirrors null.test patterns)
func TestIsNotNull(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, NULL)")
	db.Exec("INSERT INTO t VALUES (2, 'hello')")

	res := db.Query("SELECT * FROM t WHERE name IS NOT NULL")
	if res.Error != nil {
		t.Fatalf("SELECT IS NOT NULL: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row for IS NOT NULL, got %d", len(res.Rows))
	}
	if res.Rows[0][1] != "hello" {
		t.Errorf("expected 'hello', got %v", res.Rows[0][1])
	}
}

// TestDistinct tests DISTINCT (mirrors distinct.test patterns)
func TestDistinct(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (2)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (3)")
	db.Exec("INSERT INTO t VALUES (2)")

	res := db.Query("SELECT DISTINCT val FROM t")
	if res.Error != nil {
		t.Fatalf("SELECT DISTINCT: %v", res.Error)
	}
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 distinct values, got %d", len(res.Rows))
	}
}

// TestLimit tests LIMIT clause (mirrors limit.test patterns)
func TestLimit(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	for i := 1; i <= 5; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d)", i))
	}

	res := db.Query("SELECT * FROM t LIMIT 3")
	if res.Error != nil {
		t.Fatalf("SELECT LIMIT: %v", res.Error)
	}
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 rows, got %d", len(res.Rows))
	}
}

// TestLimitOffset tests LIMIT with OFFSET (mirrors limit.test patterns)
func TestLimitOffset(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	for i := 1; i <= 10; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d)", i))
	}

	res := db.Query("SELECT * FROM t LIMIT 3 OFFSET 5")
	if res.Error != nil {
		t.Fatalf("SELECT LIMIT OFFSET: %v", res.Error)
	}
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 rows, got %d", len(res.Rows))
	}

	// LIMIT larger than available rows
	res = db.Query("SELECT * FROM t ORDER BY id LIMIT 100")
	if res.Error != nil {
		t.Fatalf("SELECT LIMIT large: %v", res.Error)
	}
	if len(res.Rows) != 10 {
		t.Errorf("expected 10 rows for LIMIT 100, got %d", len(res.Rows))
	}

	// LIMIT with OFFSET beyond available rows
	res = db.Query("SELECT * FROM t ORDER BY id LIMIT 3 OFFSET 50")
	if res.Error != nil {
		t.Fatalf("SELECT LIMIT OFFSET beyond: %v", res.Error)
	}
	if len(res.Rows) != 0 {
		t.Errorf("expected 0 rows for LIMIT 3 OFFSET 50, got %d", len(res.Rows))
	}
}

// TestOrderBy tests ORDER BY ASC (mirrors orderby1.test patterns)
func TestOrderBy(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (2, 'bob')")
	db.Exec("INSERT INTO t VALUES (1, 'alice')")
	db.Exec("INSERT INTO t VALUES (3, 'charlie')")

	res := db.Query("SELECT name FROM t ORDER BY id")
	if res.Error != nil {
		t.Fatalf("SELECT ORDER BY: %v", res.Error)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != "alice" {
		t.Errorf("expected 'alice' first, got %v", res.Rows[0][0])
	}
}

// TestFunctions tests scalar functions (mirrors func.test patterns)
func TestFunctions(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, 'hello')")
	db.Exec("INSERT INTO t VALUES (2, 'world')")

	// UPPER
	res := db.Query("SELECT upper(name) FROM t ORDER BY id")
	if res.Error != nil {
		t.Fatalf("SELECT upper: %v", res.Error)
	}
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != "HELLO" {
		t.Errorf("expected HELLO, got %v", res.Rows[0][0])
	}

	// LENGTH
	res = db.Query("SELECT length(name) FROM t WHERE id = 1")
	if res.Error != nil {
		t.Fatalf("SELECT length: %v", res.Error)
	}
	if res.Rows[0][0] != int64(5) {
		t.Errorf("expected 5, got %v", res.Rows[0][0])
	}

	// LOWER
	res = db.Query("SELECT lower(name) FROM t WHERE id = 1")
	if res.Error != nil {
		t.Fatalf("SELECT lower: %v", res.Error)
	}
	if res.Rows[0][0] != "hello" {
		t.Errorf("expected 'hello', got %v", res.Rows[0][0])
	}

	// SUBSTR
	res = db.Query("SELECT substr(name, 1, 3) FROM t WHERE id = 1")
	if res.Error != nil {
		t.Fatalf("SELECT substr: %v", res.Error)
	}
	if res.Rows[0][0] != "hel" {
		t.Errorf("expected 'hel', got %v", res.Rows[0][0])
	}
}

// TestSelectExpr tests SELECT with expressions (mirrors expr.test patterns)
func TestSelectExpr(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// Test arithmetic expressions in SELECT list (mirrors expr-1.1 to expr-1.5)
	db.Exec("CREATE TABLE t (i1 INTEGER, i2 INTEGER)")
	db.Exec("INSERT INTO t VALUES (10, 20)")

	tests := []struct {
		sql    string
		want   interface{}
	}{
		{"SELECT i1 + i2 FROM t", int64(30)},
		{"SELECT i1 - i2 FROM t", int64(-10)},
		{"SELECT i1 * i2 FROM t", int64(200)},
		{"SELECT i2 / i1 FROM t", int64(2)},
		{"SELECT -i1 FROM t", float64(-10)},
		{"SELECT (i1 + i2) * 2 FROM t", int64(60)},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			res := db.Query(tt.sql)
			if res.Error != nil {
				t.Fatalf("query %q: %v", tt.sql, res.Error)
			}
			if len(res.Rows) != 1 {
				t.Fatalf("expected 1 row, got %d", len(res.Rows))
			}
			if res.Rows[0][0] != tt.want {
				t.Errorf("got %v (%T), want %v (%T)", res.Rows[0][0], res.Rows[0][0], tt.want, tt.want)
			}
		})
	}
}

// TestSelectWhereComparison tests comparison operators in WHERE (mirrors expr.test patterns)
func TestSelectWhereComparison(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (i1 INTEGER, i2 INTEGER)")
	db.Exec("INSERT INTO t VALUES (10, 20)")
	db.Exec("INSERT INTO t VALUES (20, 20)")
	db.Exec("INSERT INTO t VALUES (30, 20)")

	tests := []struct {
		sql    string
		want   int
	}{
		{"SELECT * FROM t WHERE i1 < i2", 1},   // 10 < 20
		{"SELECT * FROM t WHERE i1 <= i2", 2},   // 10 <= 20, 20 <= 20
		{"SELECT * FROM t WHERE i1 > i2", 1},    // 30 > 20
		{"SELECT * FROM t WHERE i1 >= i2", 2},   // 20 >= 20, 30 >= 20
		{"SELECT * FROM t WHERE i1 = i2", 1},    // 20 = 20
		{"SELECT * FROM t WHERE i1 <> i2", 2},   // 10 <> 20, 30 <> 20
		{"SELECT * FROM t WHERE i1 != i2", 2},   // 10 != 20, 30 != 20
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			res := db.Query(tt.sql)
			if res.Error != nil {
				t.Fatalf("query %q: %v", tt.sql, res.Error)
			}
			if len(res.Rows) != tt.want {
				t.Errorf("expected %d rows, got %d", tt.want, len(res.Rows))
			}
		})
	}
}

// TestSelectLogical tests AND/OR/NOT in WHERE (mirrors expr.test patterns)
func TestSelectLogical(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (i1 INTEGER, i2 INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, 1)")
	db.Exec("INSERT INTO t VALUES (1, 0)")
	db.Exec("INSERT INTO t VALUES (0, 1)")
	db.Exec("INSERT INTO t VALUES (0, 0)")

	tests := []struct {
		sql    string
		want   int
	}{
		{"SELECT * FROM t WHERE i1 = 1 AND i2 = 1", 1},
		{"SELECT * FROM t WHERE i1 = 1 AND i2 = 0", 1},
		{"SELECT * FROM t WHERE i1 = 1 OR i2 = 1", 3},
		{"SELECT * FROM t WHERE i1 = 0 AND i2 = 0", 1},
		{"SELECT * FROM t WHERE NOT i1 = 1", 2},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			res := db.Query(tt.sql)
			if res.Error != nil {
				t.Fatalf("query %q: %v", tt.sql, res.Error)
			}
			if len(res.Rows) != tt.want {
				t.Errorf("expected %d rows, got %d", tt.want, len(res.Rows))
			}
		})
	}
}

// TestSelectBetween tests BETWEEN operator (mirrors between.test and in.test patterns)
func TestSelectBetween(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (a INTEGER, b INTEGER)")
	for i := 1; i <= 10; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, 1<<i))
	}

	tests := []struct {
		sql    string
		want   int
	}{
		{"SELECT a FROM t WHERE b BETWEEN 10 AND 50", 2},
		{"SELECT a FROM t WHERE b NOT BETWEEN 10 AND 50", 8},
		{"SELECT a FROM t WHERE a BETWEEN 3 AND 7", 5},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			res := db.Query(tt.sql)
			if res.Error != nil {
				t.Fatalf("query %q: %v", tt.sql, res.Error)
			}
			if len(res.Rows) != tt.want {
				t.Errorf("expected %d rows, got %d: results=%v", tt.want, len(res.Rows), res.Rows)
			}
		})
	}
}

// TestSelectIn tests IN operator (mirrors in.test patterns)
func TestSelectIn(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (a INTEGER, b INTEGER)")
	for i := 1; i <= 10; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, 1<<i))
	}

	tests := []struct {
		sql    string
		want   int
	}{
		{"SELECT a FROM t WHERE b IN (8, 12, 16, 24, 32)", 3},
		{"SELECT a FROM t WHERE b NOT IN (8, 12, 16, 24, 32)", 7},
		{"SELECT a FROM t WHERE a IN (1, 3, 5)", 3},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			res := db.Query(tt.sql)
			if res.Error != nil {
				t.Fatalf("query %q: %v", tt.sql, res.Error)
			}
			if len(res.Rows) != tt.want {
				t.Errorf("expected %d rows, got %d: results=%v", tt.want, len(res.Rows), res.Rows)
			}
		})
	}
}

// TestSelectIfNotNull tests IFNULL function
func TestSelectIfNotNull(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, 'Hello')")
	db.Exec("INSERT INTO t VALUES (2, NULL)")

	res := db.Query("SELECT ifnull(name, 'default') FROM t ORDER BY id")
	if res.Error != nil {
		t.Fatalf("IFNULL: %v", res.Error)
	}
	if res.Rows[0][0] != "Hello" {
		t.Errorf("expected 'Hello', got %v", res.Rows[0][0])
	}
	if res.Rows[1][0] != "default" {
		t.Errorf("expected 'default', got %v", res.Rows[1][0])
	}
}

// TestSelectCoalesce tests COALESCE function
func TestSelectCoalesce(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, 'Hello')")
	db.Exec("INSERT INTO t VALUES (2, NULL)")

	res := db.Query("SELECT coalesce(name, 'fallback') FROM t ORDER BY id")
	if res.Error != nil {
		t.Fatalf("COALESCE: %v", res.Error)
	}
	if res.Rows[1][0] != "fallback" {
		t.Errorf("expected 'fallback', got %v", res.Rows[1][0])
	}
}

// TestSelectAbs tests ABS function
func TestSelectAbs(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, -42)")
	db.Exec("INSERT INTO t VALUES (2, NULL)")

	res := db.Query("SELECT abs(val) FROM t WHERE id = 1")
	if res.Error != nil {
		t.Fatalf("ABS: %v", res.Error)
	}
	if res.Rows[0][0] != int64(42) {
		t.Errorf("expected 42, got %v (type: %T)", res.Rows[0][0], res.Rows[0][0])
	}

	res = db.Query("SELECT abs(val) FROM t WHERE id = 2")
	if res.Error != nil {
		t.Fatalf("ABS NULL: %v", res.Error)
	}
	if res.Rows[0][0] != nil {
		t.Errorf("expected nil, got %v", res.Rows[0][0])
	}
}

// TestSelectTypeof tests TYPEOF function
func TestSelectTypeof(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")

	res := db.Query("SELECT typeof(42), typeof('hello'), typeof(NULL) FROM t WHERE id = 1")
	if res.Error != nil {
		t.Fatalf("TYPEOF: %v", res.Error)
	}
	if res.Rows[0][0] != "integer" {
		t.Errorf("expected 'integer', got %v", res.Rows[0][0])
	}
	if res.Rows[0][1] != "text" {
		t.Errorf("expected 'text', got %v", res.Rows[0][1])
	}
	if res.Rows[0][2] != "null" {
		t.Errorf("expected 'null', got %v", res.Rows[0][2])
	}
}

// TestSelectTrim tests TRIM function
func TestSelectTrim(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (s TEXT)")
	db.Exec("INSERT INTO t VALUES ('  hello  ')")

	res := db.Query("SELECT trim(s) FROM t")
	if res.Error != nil {
		t.Fatalf("TRIM: %v", res.Error)
	}
	if res.Rows[0][0] != "hello" {
		t.Errorf("expected 'hello', got %v", res.Rows[0][0])
	}
}

// TestSelectRound tests ROUND function
func TestSelectRound(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Query("SELECT round(3.14159, 2)")
	if res.Error != nil {
		t.Fatalf("ROUND: %v", res.Error)
	}
	if res.Rows[0][0] != float64(3.14) {
		t.Errorf("expected 3.14, got %v", res.Rows[0][0])
	}
}

// TestSelectReplace tests REPLACE function
func TestSelectReplace(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, 'Hello World')")

	res := db.Query("SELECT replace(name, 'Hello', 'Hi') FROM t WHERE id = 1")
	if res.Error != nil {
		t.Fatalf("REPLACE: %v", res.Error)
	}
	if res.Rows[0][0] != "Hi World" {
		t.Errorf("expected 'Hi World', got %v", res.Rows[0][0])
	}
}

// TestSelectFunctions is kept for backward compatibility.
func TestSelectFunctions(t *testing.T) { TestSelectIfNotNull(t); TestSelectCoalesce(t); TestSelectAbs(t); TestSelectTypeof(t); TestSelectTrim(t); TestSelectRound(t); TestSelectReplace(t) }

// TestSelectUnion tests UNION (mirrors select1.test patterns)
func TestSelectUnion(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t1 (id INTEGER)")
	db.Exec("INSERT INTO t1 VALUES (1)")
	db.Exec("INSERT INTO t1 VALUES (2)")
	db.Exec("CREATE TABLE t2 (id INTEGER)")
	db.Exec("INSERT INTO t2 VALUES (2)")
	db.Exec("INSERT INTO t2 VALUES (3)")

	res := db.Query("SELECT id FROM t1 UNION SELECT id FROM t2 ORDER BY id")
	if res.Error != nil {
		t.Fatalf("UNION: %v", res.Error)
	}
	// UNION removes duplicates: {1, 2, 3}
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 rows, got %d: %v", len(res.Rows), res.Rows)
	}
}

// TestSelectAliases tests column and table aliases (mirrors select1.test patterns)
func TestSelectAliases(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, 'alice')")

	res := db.Query("SELECT id AS user_id, name AS user_name FROM t")
	if res.Error != nil {
		t.Fatalf("SELECT aliases: %v", res.Error)
	}
	if res.Columns[0] != "user_id" {
		t.Errorf("expected column 'user_id', got %v", res.Columns[0])
	}
	if res.Columns[1] != "user_name" {
		t.Errorf("expected column 'user_name', got %v", res.Columns[1])
	}
	if res.Rows[0][0] != int64(1) {
		t.Errorf("expected 1, got %v", res.Rows[0][0])
	}
	if res.Rows[0][1] != "alice" {
		t.Errorf("expected 'alice', got %v", res.Rows[0][1])
	}
}

// TestSelectNoFrom tests SELECT without FROM (mirrors select1.test / expr.test patterns)
func TestSelectNoFrom(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	tests := []struct {
		sql    string
		want   interface{}
	}{
		{"SELECT 1", int64(1)},
		{"SELECT 1 + 2", int64(3)},
		{"SELECT 3 * 4", int64(12)},
		{"SELECT 'hello'", "hello"},
		{"SELECT 10 > 5", true},
		{"SELECT 10 < 5", false},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			res := db.Query(tt.sql)
			if res.Error != nil {
				t.Fatalf("query %q: %v", tt.sql, res.Error)
			}
			if len(res.Rows) != 1 {
				t.Fatalf("expected 1 row, got %d", len(res.Rows))
			}
			if res.Rows[0][0] != tt.want {
				t.Errorf("got %v, want %v", res.Rows[0][0], tt.want)
			}
		})
	}
}