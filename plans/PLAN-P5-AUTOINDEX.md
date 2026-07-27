# PLAN-P5-AUTOINDEX.md — Automatic Index Creation for Joins (Updated 2026-07-27)

## Scope
Implement automatic transient index creation during query execution for JOIN operations lacking suitable indexes.

## Current Failures (13 across 3 suites)

| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| autoindex4 | 6 | Wrong JOIN results (NULL padding), panic in sortRowsWithMaps |
| autoindex3 | 5 | EXPLAIN QUERY PLAN shows SCAN instead of AUTO for auto-indexed joins |
| autoindex2 | 2 | EXPLAIN QUERY PLAN shows SEARCH instead of AUTO |

## SQLite Auto-Index Behavior

- Default: `PRAGMA automatic_index=ON`
- Creates a temporary index on the inner table's join column during query execution
- Index exists only for the duration of the statement
- Only created when no suitable index exists and estimated benefit > overhead
- Size threshold: ~100 rows (below this, full table scan is faster)

## Implementation Steps (Ordered)

### Step 1: PRAGMA automatic_index
**File:** `internal/exec/engine.go`

1. Add `automaticIndex bool` field to Engine (default: true)
2. Handle `PRAGMA automatic_index=ON|OFF` and `PRAGMA automatic_index` (read)
3. This is a simple toggle with no other dependencies

**Verify:** After implementation, `PRAGMA automatic_index;` should return `1` by default.

### Step 2: Auto-index data structure
**File:** `internal/exec/engine.go` or new `internal/autoindex/autoindex.go`

**Recommended approach for speed:** Use `map[string][]RowLocation` — simple, fast, and sufficient for test passage.

```go
type AutoIndex struct {
    tableName string
    columnIdx int
    entries   map[string][]RowLocation
}

type RowLocation struct {
    Page uint32
    Cell int
}
```

### Step 3: Join analysis for auto-index eligibility
**File:** `internal/exec/engine.go`

In `execSelect`, before executing JOIN:
1. Analyze join conditions for equality predicates: `t1.col = t2.col`
2. For each join pair (outer, inner):
   - Check if inner table has an index on the join column
   - If not, and auto-index is enabled:
     - Estimate inner table size
     - If size > threshold (~100), create auto-index
3. Store auto-index info in the join context

### Step 4: Auto-index creation
**File:** `internal/exec/engine.go`

To create an auto-index:
1. Scan the inner table completely
2. For each row, extract the join column value and row location
3. Index by the join column value
4. If values are duplicate (non-unique join), store multiple locations

### Step 5: Auto-index usage in nested-loop join
**File:** `internal/exec/engine.go`

Modify nested-loop join execution:
1. When processing a join pair with an auto-index:
   - Get the outer row's join column value
   - Look up in auto-index → list of matching inner row locations
   - For each matching location, fetch the inner row and join
2. This replaces the full table scan of the inner table

### Step 6: EXPLAIN QUERY PLAN update
**File:** `internal/exec/engine.go`

Update the query planner's cost estimation:
1. When auto-index is created, show `AUTO` or `USING AUTOMATIC INDEX`
2. Currently showing `SCAN t1` — should show `SEARCH t1 USING AUTOMATIC INDEX`

### Step 7: Fix sortRowsWithMaps panic (autoindex4-2.0)
**File:** `internal/exec/engine.go`

**Bug:** `index out of range [1] with length 1` in autoindex4-2.0
- `sortRowsWithMaps` expects all rows to have the same number of columns
- But some rows may have fewer columns due to NULL padding in JOIN results
- **Fix:** Before accessing `rowMap[colIdx]`, check bounds. If colIdx >= len(rowMap), return nil (SQL NULL).

### Step 8: Fix NULL padding in JOIN results (autoindex4 failures)
**File:** `internal/exec/engine.go`

**Bug:** autoindex4 tests show `got: [123 abc  ]` instead of `[123 abc NULL NULL]`
- This is a JOIN result formatting issue
- **Fix:** In `flattenResult` or row construction, ensure all expected columns are present, padding with NULL

## Completion Check

```bash
go test -v -run "TestSQLiteSuite/autoindex2" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/autoindex3" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/autoindex4" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files

| File | Role |
|------|------|
| `internal/exec/engine.go` | Join execution, auto-index lifecycle, EXPLAIN QP |
| `internal/pager/pager.go` | In-memory temp storage for auto-index (if using btree) |

## Go Standard Library Usage

| Feature | Go stdlib |
|---------|-----------|
| Auto-index map storage | `map[string][]RowLocation` |
| Sorting join column values | `sort.Slice()` |
| Value to string key | `fmt.Sprintf("%v", val)` or `util.FormatValue()` |

## Goal Integration

```json
{
  "objective": "Implement automatic transient indexes for JOIN queries: PRAGMA automatic_index toggle, auto-index creation (map-based), nested-loop join using auto-indexes, EXPLAIN QUERY PLAN display, fix NULL padding and sortRowsWithMaps panic",
  "completionCriterion": "All autoindex suites pass with zero FAIL: autoindex2, autoindex3, autoindex4",
  "verifyCommand": "go test -v -run \"TestSQLiteSuite/autoindex2\" . 2>&1 | grep -c \"FAIL\" | xargs test 0 -eq && go test -v -run \"TestSQLiteSuite/autoindex3\" . 2>&1 | grep -c \"FAIL\" | xargs test 0 -eq && go test -v -run \"TestSQLiteSuite/autoindex4\" . 2>&1 | grep -c \"FAIL\" | xargs test 0 -eq"
}
```
