# PLAN-P3-ATTACH.md — ATTACH / DETACH Database Implementation

## Scope
Implement multi-database support via ATTACH and DETACH statements.

## Current Failures (20)
| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| attach3 | 20 | ATTACH/DETACH don't create separate database contexts |

## Current State
```go
// AttachStmt is parsed but Attach case in Engine.Exec returns empty result
case *sql.AttachStmt:
    // no-op
```

## Implementation Approach
SQLite ATTACH opens an additional database file and associates it with a schema name (like "aux", "db2", etc.). Tables can be referenced as `schema.table`.

### Design: Multi-DB Engine
The Engine needs to support multiple database contexts, each with its own:
- Pager (file access)
- Schema Manager (tables, indexes, views, triggers)
- B-Tree trees

### Implementation Steps

#### Step 1: Create Multi-DB Engine Architecture
1. Design `DatabaseContext` struct holding { pager, schema, rootPages }
2. Modify `Engine` to hold a map of schema_name → DatabaseContext
3. Default "main" context is the primary database
4. Add `Attach(path string, schema string)` method that:
   - Opens/creates the database file via pager
   - Initializes schema manager
   - Validates schema name doesn't conflict with existing
   - Stores in the context map

#### Step 2: Implement ATTACH execution
1. In `execAttach` (to be created), parse the path and schema name
2. Open the target database file using pager
3. Initialize its schema
4. Add to engine's database contexts
5. Handle `:memory:` paths for in-memory attached databases
6. Handle `file:` URI paths for SQLite-compatible attachment
7. Validate schema name (no duplicates, no reserved names)

#### Step 3: Implement DETACH execution
1. Parse DETACH DATABASE schema_name
2. Remove the database context
3. Close the associated pager
4. Validate that main cannot be detached

#### Step 4: Support schema-qualified table references
1. Update table name resolution to handle `schema.table` syntax
2. When a table is referenced as `schema.table`:
   - Look up the schema in engine's database contexts
   - Look up the table in that schema's schema manager
3. When a table is referenced without schema:
   - Search "main" first, then attach databases in order
4. Schema - name mapping should use CaseInsensitive comparison

#### Step 5: Handle cross-database operations
1. SELECT from tables in different databases (cross-db joins)
2. INSERT INTO schema.table
3. CREATE TABLE schema.table
4. CREATE INDEX schema.index ON schema.table
5. DROP TABLE schema.table
6. PRAGMA schema.pragma_name
7. sqlite_schema/sqlite_master views for each database

#### Step 6: Handle attached database transactions
1. BEGIN/COMMIT/ROLLBACK should apply to ALL attached databases
2. Two-phase commit? (SQLite uses a super-journal for multi-db transactions)
3. For now: simple per-database transactions (may not be fully atomic)

#### Step 7: Handle edge cases
1. ATTACH the same database twice (should fail)
2. ATTACH a database that doesn't exist (with appropriate flags?)
3. DETACH while in transaction
4. DETACH a database with active prepared statements
5. Schema name conflicts with "main", "temp", "sqlite_master"

## Verification
```bash
go test -v -run "TestSQLiteSuite/attach3" . 2>&1 | grep -E "PASS|FAIL"
```

## Completion Check
```bash
go test -v -run "TestSQLiteSuite/attach3" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files
- `internal/exec/engine.go` — ATTACH/DETACH execution, schema. prefix resolution
- `internal/pager/pager.go` — multiple pager instances
- `internal/schema/schema.go` — multi-db schema management
