# PLAN-P7 — C API Go Recreation Layer

> **Prerequisite**: P0 (test infrastructure). Independent of P1–P6.
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/`
>   - Prepared statements: `src/prepare.c`
>   - Step/column model: `src/vdbe.c`
>   - Binding: `src/vdbeapi.c` (`sqlite3_bind_*`)
>   - Error codes: `src/sqlite3.h` (SQLITE_DONE, SQLITE_ROW, etc.)
> **Goal**: Implement a Go recreation of the SQLite C API (sqlite3_prepare,
> sqlite3_step, sqlite3_column_*, sqlite3_bind_*, sqlite3_finalize) so that
> tests using the C API can be converted and run against frigolite.

## Why This Is Needed

The TCL→test converters skip **123 test files** that contain any `sqlite3_*` C API
call. Many of these are SQL tests that merely use the C API for setup or
verification — they test real SQL behavior.

The user's directive: "C required API test should use a golang recreation."

This means: implement Go equivalents of the C API functions so that the converter
can include these test files.

## C API Functions Used in Tests

From the excluded test files, the most common C API patterns:

| C API function | Go equivalent | Purpose |
|----------------|---------------|---------|
| `sqlite3_prepare_v2(db, sql, -1, &stmt, &tail)` | `db.Prepare(sql) (*Stmt, error)` | Compile SQL |
| `sqlite3_step(stmt)` | `stmt.Step() int` | Execute one step |
| `sqlite3_column_text(stmt, col)` | `stmt.ColumnText(col) string` | Get column value |
| `sqlite3_column_int(stmt, col)` | `stmt.ColumnInt(col) int64` | Get column value |
| `sqlite3_column_count(stmt)` | `stmt.ColumnCount() int` | Number of columns |
| `sqlite3_column_name(stmt, col)` | `stmt.ColumnName(col) string` | Column name |
| `sqlite3_finalize(stmt)` | `stmt.Finalize() error` | Free statement |
| `sqlite3_bind_text(stmt, col, val, -1, -1)` | `stmt.BindText(col, val)` | Bind parameter |
| `sqlite3_bind_int(stmt, col, val)` | `stmt.BindInt(col, val)` | Bind parameter |
| `sqlite3_bind_null(stmt, col)` | `stmt.BindNull(col)` | Bind NULL |
| `sqlite3_reset(stmt)` | `stmt.Reset() error` | Reset for re-execution |
| `sqlite3_errcode(db)` | `db.ErrCode() int` | Last error code |
| `sqlite3_errmsg(db)` | `db.ErrMsg() string` | Last error message |
| `sqlite3_changes(db)` | `db.Changes() int64` | Rows affected |
| `sqlite3_exec(db, sql, callback, ...)` | `db.Exec(sql)` | Execute SQL |

**Error codes:**
```go
const (
    SQLITE_OK    = 0
    SQLITE_ROW   = 100
    SQLITE_DONE  = 101
    SQLITE_ERROR = 1
    SQLITE_BUSY  = 5
    SQLITE_CONSTRAINT = 19
    SQLITE_MISUSE = 21
)
```

## Implementation Steps

### Step 1: Create the `Stmt` type (Prepared Statement)

**File:** `frigolite.go` (public API) or new `frigolite_stmt.go`.

**Design:**
```go
// Stmt represents a prepared statement (analogous to sqlite3_stmt).
type Stmt struct {
    db       *DB
    sql      string
    stmts    []sql.Stmt  // parsed statements (frigolite AST)
    current  int         // index of current statement being stepped
    // Execution state for the current statement:
    rows     [][]interface{}
    columns  []string
    rowIdx   int         // current row position during Step()
    finished bool
    // Bound parameters:
    bindings map[int]interface{}
    err      error
}

// Prepare compiles a SQL statement (analogous to sqlite3_prepare_v2).
// Multiple semicolon-separated statements can be prepared; Step() processes
// them sequentially.
func (db *DB) Prepare(sqlStr string) (*Stmt, error) {
    parser := sql.NewParser(sqlStr)
    stmts := parser.Parse()
    if parser.Err() != nil {
        return nil, fmt.Errorf("frigolite: prepare: %w", parser.Err())
    }
    return &Stmt{
        db:       db,
        sql:      sqlStr,
        stmts:    stmts,
        bindings: make(map[int]interface{}),
    }, nil
}
```

### Step 2: Implement `Step()` (sqlite3_step equivalent)

**File:** `frigolite_stmt.go`.

**Design:**
```go
// Step advances the statement to the next result row.
// Returns SQLITE_ROW if a row is available, SQLITE_DONE if no more rows.
func (s *Stmt) Step() int {
    if s.finished {
        return SQLITE_DONE
    }
    // If we have pre-fetched rows and haven't exhausted them:
    if s.rows != nil && s.rowIdx < len(s.rows) {
        s.rowIdx++
        if s.rowIdx < len(s.rows) {
            return SQLITE_ROW
        }
        // Fall through to next statement
    }

    // Advance to next statement
    for s.current < len(s.stmts) {
        stmt := s.stmts[s.current]
        s.current++
        res := s.db.engine.Exec(stmt)
        if res.Error != nil {
            s.err = res.Error
            return SQLITE_ERROR
        }
        if len(res.Rows) > 0 {
            s.rows = res.Rows
            s.columns = res.Columns
            s.rowIdx = 0
            return SQLITE_ROW
        }
        // Non-row statement (DDL/DML): continue to next
    }

    s.finished = true
    return SQLITE_DONE
}
```

**Important:** Apply bindings before execution. Since frigolite currently uses
inline SQL (no parameter binding), we need to support `?` placeholders:

1. Before executing a statement, replace `?` placeholders with bound values.
2. Or: parse the statement with placeholder markers and substitute at execution.

**Parameter handling:**
- `?` and `?N` are positional parameters.
- `$N` are also positional (SQLite style).
- Named parameters (`:name`, `@name`) are also supported by SQLite.

**Pragmatic approach:** Parse the SQL, find `?` placeholders, replace with bound
values (quoted as SQL literals) before execution.

### Step 3: Implement `Column*` accessors

**File:** `frigolite_stmt.go`.

```go
func (s *Stmt) ColumnText(col int) string {
    if s.rowIdx >= len(s.rows) { return "" }
    row := s.rows[s.rowIdx]
    if col >= len(row) { return "" }
    if row[col] == nil { return "" } // NULL → empty string (SQLite behavior)
    return fmt.Sprintf("%v", row[col])
}

func (s *Stmt) ColumnInt(col int) int64 {
    if s.rowIdx >= len(s.rows) { return 0 }
    row := s.rows[s.rowIdx]
    if col >= len(row) { return 0 }
    switch v := row[col].(type) {
    case int64: return v
    case int:   return int64(v)
    case float64: return int64(v)
    case string: // try to parse
        n, _ := strconv.ParseInt(v, 10, 64)
        return n
    }
    return 0
}

func (s *Stmt) ColumnBlob(col int) []byte { ... }
func (s *Stmt) ColumnReal(col int) float64 { ... }
func (s *Stmt) ColumnType(col int) int { ... } // SQLITE_INTEGER, SQLITE_TEXT, etc.
func (s *Stmt) ColumnCount() int { return len(s.columns) }
func (s *Stmt) ColumnName(col int) string { ... }
```

### Step 4: Implement `Bind*` methods

**File:** `frigolite_stmt.go`.

```go
func (s *Stmt) BindText(col int, val string) {
    s.bindings[col] = val
}
func (s *Stmt) BindInt(col int, val int64) {
    s.bindings[col] = val
}
func (s *Stmt) BindBlob(col int, val []byte) {
    s.bindings[col] = val
}
func (s *Stmt) BindNull(col int) {
    s.bindings[col] = nil
}
func (s *Stmt) BindParameterCount() int {
    // Count ? placeholders in the SQL
}
```

**Binding application:** Before `Step()` executes a statement, substitute bound
values for `?` placeholders. This is done by:
1. Scanning the SQL for `?` (or `?N`, `$N`) tokens.
2. Replacing them with the bound value as a SQL literal.

### Step 5: Implement `Reset()` and `Finalize()`

```go
func (s *Stmt) Reset() error {
    // Reset to the beginning for re-execution with new bindings.
    s.current = 0
    s.rows = nil
    s.rowIdx = 0
    s.finished = false
    return nil
}

func (s *Stmt) Finalize() error {
    // Release resources.
    s.stmts = nil
    s.rows = nil
    s.finished = true
    return nil
}
```

### Step 6: Implement error codes and helper functions

**File:** `frigolite.go`.

```go
// SQLite error codes (for C API compatibility).
const (
    SQLITE_OK         = 0
    SQLITE_ERROR      = 1
    SQLITE_BUSY       = 5
    SQLITE_CONSTRAINT = 19
    SQLITE_MISUSE     = 21
    SQLITE_ROW        = 100
    SQLITE_DONE       = 101
)

func (db *DB) ErrCode() int { ... }
func (db *DB) ErrMsg() string { ... }
func (db *DB) Changes() int64 { ... }
func (db *DB) TotalChanges() int64 { ... }
func (db *DB) LastInsertRowid() int64 { ... }
```

### Step 7: Update the converter to include C API test files

**File:** `tools/convert_compat_test.py`, `tools/convert_compat_json.py`.

**Changes:**
1. Remove the `C_API_RE` exclusion.
2. Instead, convert C API calls to Go equivalents:
   - `sqlite3_prepare_v2 db sql -1 stmt tail` → `stmt := db.Prepare(sql)`
   - `sqlite3_step stmt` → `stmt.Step()`
   - `sqlite3_column_text stmt 0` → `stmt.ColumnText(0)`
   - `sqlite3_finalize stmt` → `stmt.Finalize()`
   - `sqlite3_bind_text stmt 1 "val" -1` → `stmt.BindText(1, "val")`
   - etc.
3. The converter needs a TCL-to-Go translator for these C API patterns.

**This is the hardest part** — the C API calls are embedded in TCL control flow:
```tcl
set stmt [sqlite3_prepare_v2 db "SELECT ?" -1 T tail]
sqlite3_bind_text $stmt 1 "hello" -1
while {[sqlite3_step $stmt] == "SQLITE_ROW"} {
    lappend res [sqlite3_column_text $stmt 0]
}
sqlite3_finalize $stmt
```

The converter must translate this to:
```go
stmt, _ := db.Prepare("SELECT ?")
stmt.BindText(1, "hello")
for stmt.Step() == SQLITE_ROW {
    res = append(res, stmt.ColumnText(0))
}
stmt.Finalize()
```

**Alternative approach:** Instead of complex TCL translation, write hand-crafted
Go tests for the most important C API behaviors. Focus on:
- Prepared statement compilation and execution.
- Parameter binding.
- Error code checking.
- Multiple statement stepping.

### Step 8: Add error code constants to the Result struct

**File:** `frigolite.go`.

```go
type Result struct {
    // ... existing fields ...
    ErrCode int  // SQLite error code (SQLITE_CONSTRAINT, etc.)
}
```

Set `ErrCode` based on the error type:
- Constraint violation → `SQLITE_CONSTRAINT` (19)
- Syntax error → `SQLITE_ERROR` (1)
- No such table/column → `SQLITE_ERROR` (1)

## Files Modified

| File | Change |
|------|--------|
| `frigolite_stmt.go` (NEW) | Stmt type: Prepare, Step, Column*, Bind*, Reset, Finalize |
| `frigolite.go` | Error code constants; ErrCode on Result; ErrCode/ErrMsg/Changes methods |
| `tools/convert_compat_test.py` | Translate C API patterns to Go equivalents |
| `tools/convert_compat_json.py` | Same |

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# Basic prepared statement works
go test -v -count=1 -run '^TestPrepare' . 2>&1

# C API pattern tests compile and run
go build ./...

# Quality
make quality
go test -run TestSOLID_ ./...
```

## Risk and Scope Notes

**High effort, high reward.** The C API recreation unblocks 123 test files.
However, the TCL→Go translation of C API patterns is complex.

**Recommended approach:**
1. First, implement the Go API (Steps 1–6) — this is straightforward.
2. Write hand-crafted tests for the key C API behaviors.
3. Then, incrementally update the converter to handle common C API patterns.
4. Don't try to translate ALL TCL patterns — focus on the 10 most common ones.

**What NOT to do:**
- Do NOT implement a full TCL interpreter.
- Do NOT try to translate complex TCL control flow (nested procs, eval, etc.).
- Focus on the C API patterns that appear in SQL tests.
