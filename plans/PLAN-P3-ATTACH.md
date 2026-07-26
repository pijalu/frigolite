# PLAN-P3-ATTACH.md — ATTACH / DETACH Database Implementation

## Scope
Implement multi-database support via ATTACH and DETACH statements.

## Current Failures (20)
| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| attach3 | 20 | ATTACH/DETACH don't create separate database contexts |

## Current State
```go
case *sql.AttachStmt:
    return &Result{}  // no-op
case *sql.DetachStmt:
    return &Result{}  // no-op
```

## Implementation Architecture

### DatabaseContext
Create a struct that holds all per-database state:

```go
type DatabaseContext struct {
    Name       string          // schema name ("main", "aux", etc.)
    Pager      *pager.Pager    // file access for this database
    Schema     *schema.Manager // tables, indexes, views for this db
    RootPages  map[string]uint32 // table → root page mapping
    FilePath   string          // path to .db file
    IsMemory   bool            // in-memory database
    IsTemp     bool            // temp database
}
```

Modify Engine:
```go
type Engine struct {
    // ... existing fields ...
    databases map[string]*DatabaseContext // schema_name → context
    mainDB    *DatabaseContext            // shortcut for "main"
}
```

## Implementation Steps

### Step 1: Multi-DB Engine Architecture
**Files:** `internal/exec/engine.go`, `internal/schema/schema.go`

**Changes:**
1. Create `DatabaseContext` struct with pager, schema, root pages
2. Modify `Engine` to hold `map[string]*DatabaseContext`
3. Refactor `NewEngine` to create default "main" context
4. All existing table lookups go through `mainDB` initially
5. Add `getDB(name string) *DatabaseContext` helper that's case-insensitive

### Step 2: Implement ATTACH execution
**File:** `internal/exec/engine.go`

1. Create `execAttach(s *sql.AttachStmt)` method:
   - Resolve path (handle `:memory:`, file: URI)
   - Open database file via pager
   - Initialize schema manager
   - Validate schema name (no "main", "temp", duplicates)
   - Add to databases map
2. Handle errors: file not found (attach3-11.0 expects error)

### Step 3: Implement DETACH execution
**File:** `internal/exec/engine.go`

1. Create `execDetach(s *sql.DetachStmt)` method:
   - Validate schema exists and is not "main"
   - Close pager
   - Remove from databases map
2. Handle edge cases: DETACH NULL (attach3-12.11 expects error)

### Step 4: Schema-qualified table references
**Files:** `internal/exec/engine.go`, `internal/schema/schema.go`

1. Update `FindTable(name string)` to handle `schema.table` format
2. When schema is specified: look up schema in databases map, then table
3. When schema is not specified: search main first, then attached databases
4. Update `CreateTable`, `DropTable`, `CreateIndex`, etc. to accept schema parameter

### Step 5: Cross-database operations
**File:** `internal/exec/engine.go`

- SELECT from `schema.table` — resolve schema context
- INSERT INTO `schema.table` — resolve schema context
- CREATE TABLE `schema.table` — create in specific schema
- CREATE INDEX `schema.index` ON `schema.table` — resolve schema
- DROP TABLE `schema.table` — check schema
- PRAGMA `schema.pragma` — run pragma on specific schema

### Step 6: Per-database sqlite_master
**File:** `internal/schema/schema.go`

Each database should have its own `sqlite_master` table that lists tables in that database.
- `main.sqlite_master` — tables in main database
- `aux.sqlite_master` — tables in aux database
- `temp.sqlite_master` — temp tables

### Step 7: Transactions across databases
**File:** `internal/exec/engine.go`

- BEGIN applies to all attached databases
- COMMIT applies to all attached databases
- ROLLBACK applies to all attached databases
- For now: simple per-database transactions (no cross-db atomicity)

## Verification

```bash
go test -v -run "TestSQLiteSuite/attach3" . 2>&1 | grep -E "PASS|FAIL"
```

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite && go test -v -run "TestSQLiteSuite/attach3" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files

| File | Changes |
|------|---------|
| `internal/exec/engine.go` | New struct fields, ATTACH/DETACH impl, schema prefix resolution |
| `internal/pager/pager.go` | Multiple pager instances (already supports file open) |
| `internal/schema/schema.go` | Multi-db schema management, qualified name resolution |
| `internal/sql/ast.go` | AttachStmt/DetachStmt already defined |

## SQLite Reference

```sql
-- ATTACH syntax
ATTACH DATABASE 'file.db' AS aux;

-- Schema-qualified references
SELECT * FROM aux.t1;

-- Per-database sqlite_master
SELECT * FROM aux.sqlite_master;

-- DETACH
DETACH DATABASE aux;
```
