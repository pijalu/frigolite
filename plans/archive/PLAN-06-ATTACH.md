# PLAN-P6 — ATTACH Database: Multi-Database Architecture

> **Prerequisite**: P0 (test infrastructure). Independent of P1–P5.
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/`
>   - ATTACH/DETACH: `src/attach.c` (631 lines)
>   - Schema management: `src/build.c` (functions `sqlite3SchemaToIndex`,
>     `sqlite3LocateTable`)
>   - Multi-database: `src/main.c` (the `Db` array in `sqlite3` struct)
> **Goal**: Implement ATTACH/DETACH with real multi-database support so that
> schema prefixes (`main.`, `temp.`, `aux.`) work for all DDL/DML/queries.

## Scope

~14 failures in `attach3`:
- `no such table: aux.t1` — attached database tables not found
- `no such table: temp.t4` — temp database not properly separate
- Result count mismatches — data from wrong database
- Trigger placement — triggers in wrong schema
- Error handling — `ATTACH null AS null` should give specific error

## Current State

Frigolite has:
- `findTable` / `findView` / `findTrigger` / `findIndex` that search across
  "attached databases" — but the attachment is a no-op.
- `execAttach` creates a schema entry but doesn't actually open a second database.
- `execDetach` removes the schema entry but doesn't close anything.
- Schema prefixes (`main.`, `aux.`) are parsed but the engine doesn't dispatch
  to the correct database.

## SQLite's Multi-Database Architecture

Each `sqlite3` connection has an array of `Db` structs (`src/sqlite3.h`):
```c
struct sqlite3 {
  Db *aDb;       // array of attached databases
  int nDb;       // number of databases
  // aDb[0] = "main" (the primary database)
  // aDb[1] = "temp" (temporary tables)
  // aDb[2+] = attached databases
};
```

Each `Db` has:
```c
struct Db {
  char *zName;           // schema name ("main", "temp", "aux", ...)
  Schema *pSchema;       // the schema (tables, indexes, triggers, views)
  Btree *pBt;            // the B-tree for this database
};
```

**Key behaviours:**
1. `ATTACH 'file.db' AS aux` → opens `file.db`, adds `aDb[2] = {name:"aux", schema:..., btree:...}`.
2. `DETACH aux` → closes `file.db`, removes from `aDb`.
3. `SELECT * FROM aux.t1` → looks up `t1` in `aDb[2].pSchema`.
4. `CREATE TABLE aux.t1(...)` → creates the table in `aux`'s schema and B-tree.
5. `main` and `temp` are always present (indices 0 and 1).
6. Attached databases share the same connection but have separate schemas and B-trees.

## Implementation Steps

### Step 1: Add multi-database support to the engine

**File:** `internal/exec/engine.go`, `internal/schema/schema.go`, `internal/pager/pager.go`.

**Design:**
```go
// In engine.go or a new internal/dbconn/ package:
type Database struct {
    Name   string          // "main", "temp", "aux", ...
    Schema *schema.Manager // this database's schema
    Pager  *pager.Pager    // this database's pager (file I/O)
}

type Engine struct {
    databases []*Database  // [0]=main, [1]=temp, [2+]=attached
    // ...
}

func (e *Engine) findDatabase(name string) *Database {
    for _, db := range e.databases {
        if strings.EqualFold(db.Name, name) {
            return db
        }
    }
    return nil
}
```

**Migration path:**
1. Currently, `Engine` has a single `schema *schema.Manager` and `pager *pager.Pager`.
2. Change to an array of `Database` structs.
3. `main` (index 0) is the primary database.
4. `temp` (index 1) is an in-memory database for temporary tables.
5. Attached databases (index 2+) open their own files.

**Important:** This is a deep architectural change. All `e.schema.X()` calls must
be updated to `e.findDatabase(schemaName).Schema.X()`. This touches many functions.

**Pragmatic approach for attach3 tests:**
- The attach3 tests use `ATTACH ':memory:' AS aux` (or file paths).
- For `:memory:` attachments, create a new in-memory pager.
- For file paths, open a real file pager.
- Since the test database is `:memory:`, attached databases are also in-memory
  (or temp files).

### Step 2: Implement ATTACH correctly

**File:** `internal/exec/engine.go` — `execAttach`.

**Algorithm:**
1. Parse the file path and schema name from `ATTACH 'path' AS name`.
2. Check if the schema name is already in use → error.
3. Check the schema name isn't `main` or `temp` → error.
4. Open the database file (or create in-memory if `:memory:`).
5. Load the schema from the file.
6. Add to `e.databases`.
7. Return success.

**Error handling:**
- `ATTACH null AS null` → SQLite parses `null` as a keyword, not a string.
  Error: `database  is already in use` (note the double space — empty name).
  Actually, let me check the exact SQLite error... Run:
  ```bash
  sqlite3 :memory: "ATTACH null AS null;"
  # Error: database  is already in use
  ```

### Step 3: Implement DETACH correctly

**File:** `internal/exec/engine.go` — `execDetach`.

**Algorithm:**
1. Find the database by schema name.
2. Cannot detach `main` or `temp` → error: `cannot detach database main`.
3. Close the pager.
4. Remove from `e.databases`.

**Error handling:**
- `DETACH NULL` → should give an error, not a parse error.
  Run: `sqlite3 :memory: "DETACH NULL"` → `no such database: NULL`

### Step 4: Implement schema-prefix dispatch

**File:** `internal/exec/engine.go` — all table/view/trigger/index lookup functions.

**Change every function that looks up a table:**
```go
// Before:
func (e *Engine) findTable(name string) (*schema.Entry, error) {
    return e.schema.FindTable(name)
}

// After:
func (e *Engine) findTable(qualifiedName string) (*Database, *schema.Entry, error) {
    schemaName, tableName := splitSchemaPrefix(qualifiedName)
    if schemaName != "" {
        db := e.findDatabase(schemaName)
        if db == nil {
            return nil, nil, fmt.Errorf("no such database: %s", schemaName)
        }
        entry, err := db.Schema.FindTable(tableName)
        return db, entry, err
    }
    // No prefix: search main first, then temp, then attached
    for _, db := range e.databases {
        entry, err := db.Schema.FindTable(tableName)
        if err == nil {
            return db, entry, nil
        }
    }
    return nil, nil, fmt.Errorf("no such table: %s", tableName)
}
```

**Functions to update:**
- `findTable`, `findView`, `findTrigger`, `findIndex`
- `execSelect` (FROM clause resolution)
- `execInsert` (INTO clause)
- `execUpdate` (table name)
- `execDelete` (table name)
- `execCreateTable` (schema prefix)
- `execDropTable` (schema prefix)
- `execCreateIndex` / `execDropIndex`
- `execCreateTrigger` / `execDropTrigger`
- `execCreateView` / `execDropView`
- All PRAGMA implementations that access tables

### Step 5: Implement `sqlite_master` per database

**Problem:** `SELECT name FROM aux.sqlite_master` should list tables in the `aux`
database, not `main`.

**Fix:**
1. `sqlite_master` and `sqlite_schema` are virtual tables that read from the
   schema of the CURRENT database context.
2. When querying `aux.sqlite_master`, return entries from `aux`'s schema.
3. When querying `sqlite_master` without prefix, search `main` first.

**File:** `internal/exec/engine.go` — special handling for `sqlite_master`.

### Step 6: Fix cross-database queries

**Problem:** `SELECT * FROM main.t1, aux.t2 WHERE main.t1.a = aux.t2.a`

**Fix:**
1. Parse qualified table names (`schema.table`) in the FROM clause.
2. Resolve each table to its database.
3. Execute the JOIN across databases (the tables are in different pagers, but
   the data is loaded into memory for the query).

## Files Modified

| File | Change |
|------|--------|
| `internal/exec/engine.go` | Multi-database support; schema-prefix dispatch; ATTACH/DETACH |
| `internal/schema/schema.go` | Per-database schema isolation |
| `internal/pager/pager.go` | Support multiple pager instances in one engine |
| `frigolite.go` | Update `DB` to hold the multi-database engine |

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite
go test -v -count=1 -run '^TestSQLiteSuite/attach3/' . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
make quality
go test -run TestSOLID_ ./...
```

## Risk Assessment

This is the **highest-risk** phase because it changes the engine's core
architecture (from single-database to multi-database). Every table lookup is
affected.

**Mitigation:**
1. Do the migration incrementally — add `Database` struct first, then update
   lookups one function at a time.
2. Run the full test suite after each function update to catch regressions.
3. The `main` database (index 0) should behave identically to the current
   single-database setup.
4. Add `temp` (index 1) as an always-present in-memory database.
5. Attached databases (index 2+) are the only new functionality.

## Alternative: Pragmatic approach for attach3 only

If the full multi-database architecture is too risky, a **lighter approach** can
pass the attach3 tests:

1. Keep the single-pager architecture.
2. For ATTACH: open a second in-memory database, load its schema into a map
   keyed by `schemaName.tableName`.
3. For schema-prefixed lookups: check the map first.
4. For data access: store attached-database tables in a separate section of the
   same pager (different root pages).
5. For DETACH: remove the map entries.

This avoids the deep architectural change but may not support all edge cases.
