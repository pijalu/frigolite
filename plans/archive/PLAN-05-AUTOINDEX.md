# PLAN-P5 — Auto-Index & JOIN Semantics

> **Prerequisite**: P4 (query planner), P1 (type system).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/`
>   - Automatic indexes: `src/where.c` (function `whereLoopAddAutomaticIndex`)
>   - JOIN execution: `src/where.c`, `src/select.c`
>   - NULL handling in JOINs: `src/vdbe.c` (NULL padding for OUTER JOIN)
> **Goal**: Implement automatic index creation for JOINs and fix NULL padding
> in OUTER JOIN results.

## Scope

~15 failures:
- `autoindex4`: 8 (automatic index for JOIN, result mismatches)
- `autoindex3`: ~4
- `autoindex2`: ~3

## Current State

- The `sortRowsWithMaps` panic (B0) was already fixed.
- Automatic indexes (created by SQLite for JOIN optimization) are not implemented.
- LEFT JOIN / RIGHT JOIN produce incorrect NULL padding.
- EXPLAIN QUERY PLAN reports incorrect access paths for JOINs.

## SQLite Automatic Index Behavior

When SQLite processes a JOIN like:
```sql
SELECT * FROM t1, t2 WHERE t1.a = t2.b
```

And there's no index on `t2.b`, SQLite may create a **temporary automatic index**
on `t2.b` to avoid a full nested-loop scan. This is controlled by
`PRAGMA automatic_index` (default ON).

EXPLAIN QUERY PLAN reports it as:
```
QUERY PLAN
|--SCAN t1
`--SEARCH t2 USING AUTOMATIC COVERING INDEX (b=?)
```

## Implementation Steps

### Step 1: Fix NULL padding in OUTER JOIN

**Problem:** LEFT JOIN should produce NULL values for columns from the right table
when there's no match. Frigolite may produce empty strings or skip rows.

**SQLite behavior** (`src/vdbe.c`):
- For each row from the left table, if no matching row exists in the right table:
  - Output the left table's columns + NULL for ALL right table columns.
- RIGHT JOIN: mirror — for each right table row with no left match, NULL for left.

**File:** `internal/exec/engine.go` — JOIN execution.

**Fix:**
1. In LEFT JOIN execution:
   - For each left row, search for matching right rows.
   - If no matches found: emit one result row with left columns + NULL padding.
   - NULL padding = `nil` for each right table column.
2. In RIGHT JOIN execution:
   - Swap the logic (or implement by reversing table order).
3. In FULL OUTER JOIN:
   - Do both: left-unmatched rows get NULL right padding, right-unmatched rows
     get NULL left padding.

**Verify:**
```bash
cd /Users/muaddib/dev/frigolite
go test -v -count=1 -run '^TestSQLiteSuite/autoindex4/' . 2>&1 | head -30
```

### Step 2: Implement automatic index for JOINs

**File:** `internal/exec/engine.go` — JOIN execution.

**Algorithm:**
1. For a JOIN with a WHERE constraint `t1.col = t2.col`:
   a. Check if `t2.col` has an index.
   b. If not, and `automatic_index` is ON:
      - Build an in-memory hash map: `value → []rowid` for the inner table.
      - Use the hash map for the join (avoids O(n*m) nested loop).
   c. Record the automatic index in the query plan for EXPLAIN QUERY PLAN.
2. The automatic index is ephemeral — discarded after the query.

**Implementation (pragmatic):**
```go
// Instead of a real B-tree index, use a Go map for the automatic index.
type autoIndex struct {
    hashMap map[interface{}][]int64 // value → rowids
}

func buildAutoIndex(table string, col string) *autoIndex {
    // Scan the table once, build hash map.
    idx := &autoIndex{hashMap: make(map[interface{}][]int64)}
    for each row in table {
        val := row[col]
        idx.hashMap[val] = append(idx.hashMap[val], row.rowid)
    }
    return idx
}
```

**Reference**: `/Users/muaddib/dev/sqlite/src/where.c` — function
`whereLoopAddAutomaticIndex` and the `WHERE_AUTO_INDEX` flag.

### Step 3: Fix EXPLAIN QUERY PLAN for JOINs

**File:** `internal/exec/engine.go` — `execExplain`.

**Format for JOIN with automatic index:**
```
QUERY PLAN
|--SCAN t1
`--SEARCH t2 USING AUTOMATIC COVERING INDEX (b=?)
```

**Format for JOIN with regular index:**
```
QUERY PLAN
|--SCAN t1
`--SEARCH t2 USING INDEX t2b (b=?)
```

**Format for JOIN with no index:**
```
QUERY PLAN
|--SCAN t1
`--SCAN t2
```

### Step 4: Fix result row ordering in JOINs

**Problem:** JOIN results may be in wrong order compared to SQLite.

**SQLite behavior**: Nested loop join produces results in the order of the outer
table. If ORDER BY is present, sort after the join.

**Fix:**
1. Ensure the outer table is scanned in rowid order (unless a different index is used).
2. For each outer row, scan the inner table in the appropriate order.
3. Apply ORDER BY as a post-processing sort step.

### Step 5: Fix JOIN with USING clause

**Problem:** `JOIN ... USING(col)` merges the join column — only one copy appears
in the output.

**SQLite behavior**: `USING(col)` is equivalent to `ON t1.col = t2.col`, but the
result has `col` only once (from the first table). Column ordering follows the
USING columns first, then remaining columns.

**File:** `internal/exec/engine.go` — JOIN result construction.

**Fix:**
1. When USING is specified:
   a. Include the USING column(s) once in the output.
   b. Include other columns from both tables.
   c. The USING column comes from the left table (or first non-NULL).

**Note:** The P0 fix removes the converter's incorrect filtering of `USING(`.

## Files Modified

| File | Change |
|------|--------|
| `internal/exec/engine.go` | Automatic index for JOINs; NULL padding; EXPLAIN QUERY PLAN; USING clause handling |

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite
for suite in autoindex4 autoindex3 autoindex2; do
  go test -v -count=1 -run "^TestSQLiteSuite/$suite" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq || echo "FAIL in $suite"
done
make quality
go test -run TestSOLID_ ./...
```
