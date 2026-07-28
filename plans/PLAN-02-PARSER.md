# PLAN-P2 — SQL Parser Completeness: Window Functions, CTE, FILTER

> **Prerequisite**: P0 (test infrastructure).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/`
>   - Grammar: `src/parse.y` (the Lemon grammar — the authoritative syntax spec)
>   - Window function codegen: `src/window.c`
>   - CTE handling: `src/select.c` (functions `sqlite3WithPush`, `sqlite3Select`)
>   - FILTER clause: `src/expr.c` (function `sqlite3FunctionAggInfo`)
> **Goal**: Parse (and store) all SQL constructs that appear in trigger/view bodies
> so that ALTER TABLE RENAME (P3) can walk the parse tree.

## Why This Is Before ALTER TABLE

SQLite's ALTER TABLE RENAME re-parses every trigger and view body using the **full
SQL parser**. If the body contains a WINDOW clause, CTE, or FILTER that frigolite's
parser can't handle, the re-parse fails and the ALTER aborts — even if the feature
is never executed.

Many `altertab3` test cases have triggers with window functions, CTEs, and FILTER
clauses. The CREATE TRIGGER itself fails at parse time.

**This phase implements PARSING only.** Execution of window functions is a separate
concern (most tests just need the trigger to be stored and renamed, not executed).
Full window-function execution can be added later if tests require it.

## Scope

Enables P3 (ALTER TABLE). Also fixes standalone parse failures in compat tests.

**Key constructs to parse:**
1. **Window functions**: `OVER (window-spec)`, `OVER window-name`, `WINDOW` clause
2. **CTE**: `WITH name AS (subquery) SELECT ...`
3. **FILTER**: `aggregate(...) FILTER (WHERE condition)`
4. **RETURNING**: `INSERT/UPDATE/DELETE ... RETURNING cols`
5. **NOT in scope for this phase**: Execution of window functions (parsing only).

## SQLite Grammar Reference

All syntax is defined in `src/parse.y`. Key productions:

### Window Functions (`src/parse.y`, `src/window.c`)
```
% Following the "filtered aggreggate" and "window function" grammar rules:

frame_opt ::= .                           // empty = default frame
frame_opt ::= range_or_rows frame_bound_s.
frame_opt ::= range_or_rows BETWEEN frame_bound_s AND frame_bound_e.

range_or_rows ::= RANGE.
range_or_rows ::= ROWS.
range_or_rows ::= GROUPS.

frame_bound_s ::= frame_bound.           // frame start
frame_bound_s ::= BETWEEN frame_bound.   // frame start (in BETWEEN)

frame_bound ::= UNBOUNDED PRECEDING.
frame_bound ::= UNBOUNDED FOLLOWING.
frame_bound ::= CURRENT ROW.
frame_bound ::= expr PRECEDING.
frame_bound ::= expr FOLLOWING.

window_clause ::= WINDOW window_defn_list.
window_defn ::= nm AS window_spec.
window_spec ::= '(' part_opt orderby_opt frame_opt ')'.
```

The `OVER` clause attaches to a function call:
```
expr(A) ::= id(X) LP distinct exprlist(Y) RP filter_over(Z).
    // X is function name, Y is args, Z is [FILTER (WHERE ...)] [OVER (...)]
```

### CTE (`src/parse.y`)
```
% WITH clause (Common Table Expressions):
with_clause ::= WITH cte_table_name AS LP select RP.
with_clause ::= WITH RECURSIVE cte_table_name AS LP select RP.
```

### FILTER (`src/parse.y`)
```
filter_over ::= filter_clause.
filter_over ::= filter_clause over_clause.
filter_over ::= over_clause.

filter_clause ::= FILTER LP WHERE expr RP.
```

## Implementation Steps

### Step 1: Add FILTER clause to function-call parsing

**File:** `internal/sql/lexer.go`, `internal/sql/parser.go`, `internal/sql/ast.go`.

**AST addition:**
```go
// In ast.go — extend FunctionCall:
type FunctionCall struct {
    Name     string
    Args     []Expr
    Distinct bool
    Star     bool        // COUNT(*)
    Filter   Expr        // FILTER (WHERE condition) — nil if no FILTER
    Over     *OverClause // OVER (...) — nil if no OVER
}

// New type for OVER clause:
type OverClause struct {
    Name      string   // named window reference, or "" for inline
    Partition []Expr   // PARTITION BY
    OrderBy   []OrderTerm // ORDER BY
    Frame     *WindowFrame // frame specification
}

type WindowFrame struct {
    Mode    string // "RANGE", "ROWS", "GROUPS"
    Start   string // "UNBOUNDED PRECEDING", "CURRENT ROW", "expr PRECEDING", etc.
    End     string // same as Start, or ""
    StartExpr Expr // for "expr PRECEDING/FOLLOWING"
    EndExpr   Expr
}
```

**Parser changes** (`internal/sql/parser.go` — function call parsing):
After parsing the argument list and closing `)`, check for:
1. `FILTER` keyword → parse `( WHERE expr )`.
2. `OVER` keyword → parse `( window-spec )` or identifier (named window).

**Lexer changes** (`internal/sql/lexer.go`):
- Ensure `FILTER`, `OVER`, `PARTITION`, `WINDOW`, `PRECEDING`, `FOLLOWING`,
  `UNBOUNDED`, `CURRENT`, `ROWS`, `RANGE`, `GROUPS` are recognised as keywords.

**Verify:**
```bash
cd /Users/muaddib/dev/frigolite
# Create a trigger with FILTER and verify it parses without error
go test -v -count=1 -run '^TestSQLiteSuite/altertab3/17.2' . 2>&1 | head -5
```

### Step 2: Add OVER clause parsing

**File:** `internal/sql/parser.go`.

**Parse `OVER ( window-spec )`:**
1. After `OVER`, expect `(` or an identifier (named window reference).
2. If `(`: parse optional `PARTITION BY expr-list`, optional `ORDER BY ...`,
   optional frame (`ROWS BETWEEN ... AND ...`).
3. Store in `OverClause`.

**Parse standalone `WINDOW` clause:**
After the main query body (after GROUP BY, HAVING, ORDER BY, LIMIT), check for
`WINDOW` keyword:
1. Parse `window_name AS ( window-spec )` entries, comma-separated.
2. Store on the `SelectStmt`.

```go
// In ast.go — extend SelectStmt:
type SelectStmt struct {
    // ... existing fields ...
    Window []WindowDef // WINDOW clause definitions
}

type WindowDef struct {
    Name string
    Spec OverClause
}
```

**Verify:**
```bash
go test -v -count=1 -run '^TestSQLiteSuite/altertab3/7.1' . 2>&1 | head -5
go test -v -count=1 -run '^TestSQLiteSuite/altertab3/12.1' . 2>&1 | head -5
```

### Step 3: Add CTE (WITH clause) parsing

**File:** `internal/sql/parser.go`, `internal/sql/ast.go`.

**AST addition:**
```go
type WithClause struct {
    Recursive bool
    CTEs      []CTEDef
}

type CTEDef struct {
    Name    string       // CTE table name
    Columns []string     // optional column list
    Query   *SelectStmt  // the subquery
}

// Extend SelectStmt and DML statements:
type SelectStmt struct {
    With *WithClause // WITH clause, nil if none
    // ... existing fields ...
}
```

**Parser changes:**
1. At the start of a SELECT (and INSERT/UPDATE/DELETE), check for `WITH`.
2. Parse `[RECURSIVE] cte_name AS ( select ) [, ...]`.
3. Attach to the statement.

**Verify:**
```bash
go test -v -count=1 -run '^TestSQLiteSuite/altertab3/16.1' . 2>&1 | head -5
# The trigger body has: SELECT (WITH t2 AS (WITH t3 AS (...) SELECT * FROM t3))
```

### Step 4: Add RETURNING clause parsing

**File:** `internal/sql/parser.go`, `internal/sql/ast.go`.

**AST addition:**
```go
// Extend InsertStmt, UpdateStmt, DeleteStmt:
type InsertStmt struct {
    // ... existing fields ...
    Returning []ResultColumn // RETURNING clause
}
```

**Parser changes:**
After the full INSERT/UPDATE/DELETE statement, check for `RETURNING`:
1. Parse a result-column list (same syntax as SELECT columns).

**Verify:**
```bash
# RETURNING is used in some compat tests
go build ./...
```

### Step 5: Make CREATE TRIGGER store raw SQL without requiring full body execution

**File:** `internal/exec/engine.go` — `execCreateTrigger`.

**Current problem:** CREATE TRIGGER tries to fully validate/parse the trigger body.
If the body uses unsupported syntax (even though it parses), execution fails.

**Fix:**
1. Parse the trigger body to validate SYNTAX (now possible with Steps 1–4).
2. Store the original SQL text (for ALTER TABLE to re-parse).
3. Do NOT attempt to compile/execute the trigger body at CREATE time.
4. Only execute the trigger body when the trigger fires (at INSERT/UPDATE/DELETE time).

**Reference**: `/Users/muaddib/dev/sqlite/src/trigger.c` — `sqlite3CreateTrigger`.
SQLite parses the trigger body at CREATE time (to validate) but defers compilation
to first fire.

**Verify:**
```bash
# altertab3 test 12.1 creates a trigger with complex WINDOW clause
go test -v -count=1 -run '^TestSQLiteSuite/altertab3/12.1' . 2>&1 | head -5
# Should PASS (trigger is stored without error)
```

### Step 6: Window function execution (optional — only if tests require it)

**Scope decision:** Many tests only need window functions to PARSE (for trigger
bodies during ALTER TABLE). If standalone window-function tests fail, implement
basic execution:

1. **Frame evaluation**: For each row, compute the window frame (set of rows).
2. **Aggregate over frame**: Apply the window function (rank, row_number, sum, etc.)
   over the frame rows.
3. **Partition handling**: Group rows by PARTITION BY columns.

**Priority:** LOW — only implement if compat tests explicitly check window function
OUTPUT (not just parsing). Most window-function-specific tests are in `ifcapable
windowfunc` blocks that the converter already handles.

**Reference**: `/Users/muaddib/dev/sqlite/ext/fts3/` — no, wrong file.
**Correct reference**: `/Users/muaddib/dev/sqlite/src/window.c`.

## Files Modified

| File | Change |
|------|--------|
| `internal/sql/ast.go` | Add `OverClause`, `WindowFrame`, `WindowDef`, `WithClause`, `CTEDef`; extend `FunctionCall`, `SelectStmt`, `InsertStmt` |
| `internal/sql/lexer.go` | Add keywords: FILTER, OVER, PARTITION, WINDOW, PRECEDING, FOLLOWING, UNBOUNDED, RANGE, GROUPS, RETURNING, RECURSIVE |
| `internal/sql/parser.go` | Parse FILTER, OVER, WINDOW, WITH/CTE, RETURNING |
| `internal/exec/engine.go` | Store trigger body without executing; defer to fire time |

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# Triggers with window functions can be created
go test -v -count=1 -run '^TestSQLiteSuite/altertab3/7.1.0' . 2>&1 | grep -c FAIL | xargs test 0 -eq
go test -v -count=1 -run '^TestSQLiteSuite/altertab3/12.1' . 2>&1 | grep -c FAIL | xargs test 0 -eq

# Triggers with CTE can be created
go test -v -count=1 -run '^TestSQLiteSuite/altertab3/16.1' . 2>&1 | grep -c FAIL | xargs test 0 -eq

# Standalone CTE parses
go test -v -count=1 -run '^TestSQLiteSuite/altertab3/16.2' . 2>&1 | grep -c FAIL | xargs test 0 -eq

# Quality
make quality
go test -run TestSOLID_ ./...
```

## Notes for P3 (ALTER TABLE)

After P2, the parser can handle all SQL constructs in trigger/view bodies. P3
will re-parse these bodies using the parser to find table/column name tokens for
renaming. The AST nodes added in P2 (OverClause, WithClause, etc.) must carry
position information (byte offsets) so that P3 can map AST nodes back to text
positions for token-level replacement.
