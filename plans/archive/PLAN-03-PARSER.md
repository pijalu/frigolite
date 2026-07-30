# G03 — SQL Parser Completeness

> **Prerequisite**: G01 (engine decomposed), G02 (types fixed — some parser tests depend on correct type coercion).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/parse.y` (SQLite grammar), `/Users/muaddib/dev/sqlite/src/select.c`, `window.c`.
> **Goal**: Implement window functions, CTE (WITH), aggregate FILTER clause, and RETURNING. Remove these features from `knownUnsupported`.

---

## Context

The parser (`internal/sql/parser.go`, 4 179 lines, 177 functions) handles most core SQL
but lacks four significant features that appear in hundreds of SQLite tests:

1. **Window functions** (OVER clause) — 300+ test cases
2. **Common Table Expressions** (WITH clause) — 100+ test cases
3. **Aggregate FILTER** (FILTER WHERE clause) — 50+ test cases
4. **RETURNING** (INSERT/UPDATE/DELETE RETURNING) — 30+ test cases

---

## Current State

### What the parser already handles
- Standard SELECT with WHERE, GROUP BY (parsed), HAVING (parsed), ORDER BY, LIMIT/OFFSET
- JOIN (INNER, LEFT, CROSS, NATURAL)
- Subqueries (scalar, IN, EXISTS)
- UNION/INTERSECT/EXCEPT
- CASE expressions, BETWEEN, IN, LIKE, GLOB
- DDL: CREATE/DROP TABLE/INDEX/VIEW/TRIGGER

### What's missing

#### Window functions
```
SELECT sum(x) OVER (PARTITION BY y ORDER BY z) FROM t1
SELECT row_number() OVER w FROM t1 WINDOW w AS (PARTITION BY y)
SELECT lag(x, 2) OVER (ORDER BY y ROWS BETWEEN 1 PRECEDING AND CURRENT ROW)
```
No AST node for window definitions, frame specs, or window function calls.

#### CTE (WITH)
```
WITH RECURSIVE counter(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM counter WHERE x < 10)
SELECT * FROM counter
```
No AST node for WITH clause or recursive CTE.

#### Aggregate FILTER
```
SELECT sum(x) FILTER (WHERE x > 0) FROM t1
```
No AST support for FILTER clause on aggregate calls.

#### RETURNING
```
INSERT INTO t1 VALUES (1) RETURNING *
UPDATE t1 SET x = 2 WHERE id = 1 RETURNING id, x
```
No AST support for RETURNING clause.

---

## SQLite Reference

### Window functions (`window.c`)
- `/Users/muaddib/dev/sqlite/src/window.c` — entire file
- Key functions: `sqlite3WindowCreate`, `sqlite3WindowAssemble`, `sqlite3WindowCodeStep`
- Grammar: `/Users/muaddib/dev/sqlite/src/parse.y` lines with `window_defs`, `window_clause`, `frame_clause`

Window function categories:
- Ranking: `ROW_NUMBER()`, `RANK()`, `DENSE_RANK()`, `NTILE(n)`
- Value: `LAG(expr, offset, default)`, `LEAD(...)`, `FIRST_VALUE(expr)`, `LAST_VALUE(expr)`, `NTH_VALUE(expr, n)`
- Aggregate: `SUM()`, `AVG()`, `COUNT()`, `MIN()`, `MAX()` with OVER clause
- Frame types: `RANGE`, `ROWS`, `GROUPS` with `BETWEEN ... PRECEDING AND ... FOLLOWING`

### CTE (`select.c`)
- Grammar: `with_clause`, `cte_table_name`, `recursive_cte`
- Key: recursive CTE has two parts (initial query + recursive query), joined by UNION ALL
- `/Users/muaddib/dev/sqlite/src/select.c` — function `sqlite3WithAppend` / `sqlite3SelectNew`

### FILTER (`parse.y`)
- Grammar: `filter_clause ::= FILTER LP WHERE expr RP`
- Applied to aggregate function calls

### RETURNING (`parse.y`, `update.c`, `delete.c`, `insert.c`)
- Grammar: `returning_clause`
- `/Users/muaddib/dev/sqlite/src/resolve.c` — function `sqlite3ResolveExprListNames`

---

## Implementation Steps

### Step 1: Add window function AST nodes

**File**: `internal/sql/ast.go`

```go
// WindowDef defines a named or inline window.
type WindowDef struct {
    Name        string       // named window reference (empty for inline)
    Base        string       // base window name (for window inheritance)
    PartitionBy []Expr       // PARTITION BY expressions
    OrderBy     []OrderTerm  // ORDER BY terms
    Frame       *FrameSpec   // frame specification (nil = default RANGE)
}

// FrameSpec defines the window frame.
type FrameSpec struct {
    Type     FrameType  // RANGE, ROWS, GROUPS
    Start    FrameBound
    End      FrameBound
    Exclude  FrameExclude  // NO OTHERS, CURRENT ROW, TIES, GROUP
}

type FrameType int
const (
    FrameRange FrameType = iota
    FrameRows
    FrameGroups
)

type FrameBound struct {
    Type   BoundType  // UNBOUNDED PRECEDING, CURRENT ROW, UNBOUNDED FOLLOWING, <expr> PRECEDING/FOLLOWING
    Expr   Expr       // offset expression (nil for UNBOUNDED/CURRENT ROW)
}

// WindowFuncCall is a function call with an OVER clause.
type WindowFuncCall struct {
    FuncName string
    Args     []Expr
    Filter   Expr   // FILTER (WHERE expr) — nil if no filter
    Over     *WindowDef
}
```

### Step 2: Parse window functions

**File**: `internal/sql/parser.go`

Add to `parsePrimaryExpr` or `parseFuncCall`: after parsing function arguments, check for
`OVER` keyword or `FILTER` keyword.

```go
func (p *Parser) parseAggregateOrWindow(name string) Expr {
    args := p.parseFuncArgs()
    var filter Expr
    if p.peek() == TOKEN_FILTER {
        p.advance()
        p.expect(TOKEN_LPAREN)
        p.expect(TOKEN_WHERE)
        filter = p.parseExpr()
        p.expect(TOKEN_RPAREN)
    }
    if p.peek() == TOKEN_OVER {
        p.advance()
        window := p.parseWindowSpec()
        return &WindowFuncCall{FuncName: name, Args: args, Filter: filter, Over: window}
    }
    // regular aggregate or scalar function
    return &FuncCall{Name: name, Args: args, Filter: filter}
}

func (p *Parser) parseWindowSpec() *WindowDef {
    // Parse: ( [base_window] [PARTITION BY exprs] [ORDER BY terms] [frame] )
    // Or:   window_name ( [refinements] )
}
```

Add `WINDOW` clause parsing to SELECT:
```sql
SELECT ... FROM t1 WINDOW w1 AS (...), w2 AS (...)
```

### Step 3: Execute window functions

**File**: `internal/exec/select.go` (or new `internal/exec/window.go`)

Window function execution algorithm:
1. Read all input rows (window functions need the full partition)
2. Sort by PARTITION BY + ORDER BY
3. For each row, compute the window function value over its frame
4. Append the window column to the output

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/window.c` function
`sqlite3WindowCodeStep()` — this is the VDBE code generator for window functions. In Go,
we execute directly.

Frame computation:
- `ROWS BETWEEN N PRECEDING AND M FOLLOWING`: fixed row offset
- `RANGE BETWEEN ...`: based on ORDER BY value differences (more complex)
- `GROUPS BETWEEN ...`: based on peer groups (rows with same ORDER BY value)

Built-in window functions to implement first:
- `ROW_NUMBER()` — sequential number within partition
- `RANK()` — rank with gaps
- `DENSE_RANK()` — rank without gaps
- `SUM()`, `AVG()`, `COUNT()`, `MIN()`, `MAX()` — running aggregate over frame
- `LAG()`, `LEAD()` — offset access

**Verify**:
```bash
go test -count=1 -run "^TestSQLiteSuite/window1$" .
```
**Expected outcome**: Window function tests pass — ROW_NUMBER(), RANK(), DENSE_RANK(),
SUM/AVG/COUNT/MIN/MAX with OVER clause produce correct results.

### Step 4: Add CTE AST nodes and parsing

**File**: `internal/sql/ast.go`

```go
// CTE represents a single common table expression.
type CTE struct {
    Name    string    // CTE table name
    Columns []string  // optional column list
    Query   *SelectStmt // the CTE body (SELECT or compound SELECT)
    Recursive bool     // for WITH RECURSIVE
}

// WithClause represents a WITH clause.
type WithClause struct {
    Recursive bool
    CTEs      []CTE
}
```

Add `With *WithClause` field to `SelectStmt`.

**File**: `internal/sql/parser.go`

Parse `WITH [RECURSIVE] name [(cols)] AS (select) [, ...]` at the start of a SELECT, or
before INSERT/UPDATE/DELETE.

### Step 5: Execute CTEs

**File**: `internal/exec/select.go` (or `internal/exec/cte.go`)

Non-recursive CTE: execute the CTE body, store results as a named temporary table, then
resolve references in the main query.

Recursive CTE: iterative evaluation.
1. Execute the initial query → seed set
2. Execute the recursive query with the seed set as input
3. Union the results
4. Repeat until no new rows

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/select.c` function
`sqlite3Select()` — CTE handling (search for `pWith`).

**Verify**:
```bash
go test -count=1 -run "^TestSQLiteSuite/with1$" .
```
**Expected outcome**: CTE tests pass — both non-recursive and recursive CTEs work. The
counter example (`WITH RECURSIVE counter(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM counter
WHERE x < 10)`) produces rows 1 through 10.

### Step 6: Add RETURNING clause

**File**: `internal/sql/ast.go`

Add `Returning []ResultColumn` to `InsertStmt`, `UpdateStmt`, `DeleteStmt`.

**File**: `internal/sql/parser.go`

Parse `RETURNING *` or `RETURNING col1, col2, expr AS alias, ...` at the end of
INSERT/UPDATE/DELETE.

**File**: `internal/exec/insert.go` (and `update.go`, `delete.go`)

After executing the DML, collect the affected rows and return them as a result set (like
a SELECT). This requires tracking which rows were inserted/updated/deleted before modifying.

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/insert.c` function
`sqlite3Insert()` — search for `pReturning`.

**Verify**:
```bash
go test -count=1 -run "^TestSQLiteSuite/returning" .
```
**Expected outcome**: RETURNING tests pass — INSERT/UPDATE/DELETE with RETURNING returns the
affected rows as a result set.

### Step 7: Remove from `knownUnsupported`

**File**: `frigolite_test.go` (and `frigolite_harness_test.go`)

Remove these patterns from `unsupportedFeaturePatterns`:
```go
// REMOVE:
{regexp.MustCompile(`(?i)\b(OVER|WINDOW)\b`), "window functions", "G03"},
{regexp.MustCompile(`(?i)\bFILTER\s*\(`), "aggregate FILTER clause", "G03"},
{regexp.MustCompile(`(?i)\bWITH\s+\w+.*AS\s*\(`), "CTE", "G03"},
{regexp.MustCompile(`(?i)\bRETURNING\b`), "RETURNING clause", "G03"},
{regexp.MustCompile(`(?i)\bRAISE\b`), "trigger RAISE", "G03"},
```

---

## Files Modified

| File | Change |
|------|--------|
| `internal/sql/ast.go` | Add WindowDef, FrameSpec, WindowFuncCall, CTE, WithClause, Returning |
| `internal/sql/parser.go` | Parse OVER, WINDOW, WITH, FILTER, RETURNING |
| `internal/exec/select.go` | Execute window functions, CTEs |
| `internal/exec/window.go` | NEW — window function execution |
| `internal/exec/cte.go` | NEW — CTE execution (recursive) |
| `internal/exec/insert.go` | RETURNING support |
| `internal/exec/update.go` | RETURNING support |
| `internal/exec/delete.go` | RETURNING support |
| `frigolite_test.go` | Remove window/CTE/FILTER/RETURNING from `knownUnsupported` |

---

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# 1. Window function tests pass
go test -count=1 -run "^TestSQLiteSuite/window" .

# 2. CTE tests pass
go test -count=1 -run "^TestSQLiteSuite/with" .

# 3. No new failures elsewhere
make quality
go test -run TestSOLID_ ./...

# 4. Features no longer skipped
go test -v -count=1 -run "^TestSQLiteSuite/window1$" . 2>&1 | grep -c "SKIP"
# Should be 0
```
