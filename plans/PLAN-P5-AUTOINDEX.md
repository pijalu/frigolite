# PLAN-P5-AUTOINDEX.md — Automatic Index Creation for Joins

## Scope
Implement automatic index creation during query execution for JOIN operations that lack suitable indexes.

## Current Failures (15)
| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| autoindex3 | 6 | No auto-index created for joins without indexes |
| autoindex4 | 5 | No auto-index created for joins without indexes |
| autoindex2 | 2 | No auto-index created for joins without indexes |

## Current State
SQLite's query planner can create **automatic, transient indexes** during query execution when joining tables and no suitable index exists. These auto-indexes are created on the fly, used for the query, and discarded afterward.

From SQLite docs:
> "If the automatic indexing option is enabled and the query planner cannot find a usable index, then it might attempt to create an automatic index that lasts only for the duration of the SQL statement."

The `PRAGMA automatic_index` pragma controls this:
- `PRAGMA automatic_index=ON` (default) — allow automatic indexes
- `PRAGMA automatic_index=OFF` — disable automatic indexes

## Implementation Approach

### Step 1: Implement PRAGMA automatic_index
Already partially implemented? Let me check.

The engine needs:
1. An `automaticIndex` boolean field in Engine struct
2. Default to true (matching SQLite default)
3. Handle `PRAGMA automatic_index=ON|OFF`
4. Handle `SELECT @@automatic_index` or `PRAGMA automatic_index` read

### Step 2: Create automatic index infrastructure
An auto-index is a temporary B-tree that:
1. Exists only for the duration of a single statement
2. Is stored in temporary storage (temp database or memory)
3. Is created when a table join needs to look up rows by a key column
4. Contains rows keyed by the join column values

### Step 3: Implement auto-index creation heuristics
SQLite creates an auto-index when:
1. Two tables are joined with an equality condition (t1.col = t2.col)
2. t1 is the outer table (already being scanned)
3. t2 is the inner table (needs lookup by col)
4. No suitable index exists on t2.col
5. The estimated size of t2 exceeds a threshold (where it's worth building an index)

**Algorithm:**
1. For each join condition, check if the inner table has an index on the join column
2. If not, and auto-indexing is enabled:
   a. Estimate the number of rows in the inner table
   b. If rows > threshold (e.g., 100), build a temporary B-tree index
   c. Insert all rows from the inner table keyed by join column value
3. Use the auto-index for probe lookups during join execution

### Step 4: Implement auto-index lifecycle
1. Create auto-index at the start of SELECT execution (when join is detected)
2. Populate it before the join loop starts
3. Use it during join probe (inner table lookups)
4. Drop it after SELECT execution completes
5. Handle errors during auto-index creation (OOM, etc.)

### Step 5: Handle nested loop join with auto-index
Modify the join execution logic:
1. Before joining table B, check if there's a useful index on B
2. If not, consider creating an auto-index on B's join column
3. The auto-index maps join column values → row locations (page + cell)
4. During probe, lookup join column value in auto-index instead of scanning B

### Step 6: Implement auto-index storage
Auto-index can be:
1. An in-memory B-tree (fastest for small-medium tables)
2. A temporary table in the temp database

**Approach:** Use the existing B-tree implementation with an in-memory pager for auto-index storage.

## Verification
```bash
go test -v -run "TestSQLiteSuite/autoindex" . 2>&1 | grep -E "PASS|FAIL"
```

## Completion Check
```bash
go test -v -run "TestSQLiteSuite/autoindex" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files
- `internal/exec/engine.go` — join execution, auto-index lifecycle
- `internal/pager/pager.go` — in-memory temporary storage for auto-index
- `internal/btree/btree.go` — auto-index B-tree creation
