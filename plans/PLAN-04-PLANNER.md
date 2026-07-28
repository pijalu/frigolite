# PLAN-P4 — Query Planner & ANALYZE

> **Prerequisite**: P1 (correct type system for statistics), P0 (test infrastructure).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/`
>   - ANALYZE command: `src/analyze.c` (2 012 lines)
>   - Query planner: `src/where.c` (the `whereLoopBuilder` and `bestIndex` functions)
>   - EXPLAIN QUERY PLAN: `src/where.c` (function `sqlite3WhereExplainOneScan`)
>   - sqlite_stat1 format: `src/analyze.c` (function `analysisInfo`)
> **Goal**: Implement ANALYZE to gather statistics, consume them in the query
> planner, and produce correct EXPLAIN QUERY PLAN output.

## Scope

~55 failures:
- `analyze7`: 15 (EXPLAIN QUERY PLAN — index selection)
- `analyzeE`: 14 (EXPLAIN QUERY PLAN with stat tables)
- `autoanalyze1`: ~8 (automatic ANALYZE)
- `analyzeC`: ~7
- `analyze6`: ~5
- `analyze8`: ~6

All of these check `EXPLAIN QUERY PLAN` output that expects `SEARCH ... USING INDEX`
but frigolite always reports `SCAN`.

## Current State

Frigolite has:
- `ANALYZE` is a partial no-op — it creates `sqlite_stat1` table but doesn't
  populate it with real statistics.
- `readStatSZs` reads `sqlite_stat1` but the data is never populated.
- The query planner is a simple heuristic (`shouldUseIndex`) — it always prefers
  full table scan or uses an index only if there's an equality constraint on an
  indexed column.
- EXPLAIN QUERY PLAN always outputs `SCAN table_name` — never reports index usage.

## SQLite's ANALYZE Architecture

### sqlite_stat1 table

Schema: `CREATE TABLE sqlite_stat1(tbl, idx, stat)`

Each row:
- `tbl`: table name
- `idx`: index name (or the table name itself for the rowid index)
- `stat`: space-separated integers representing:
  - Number of rows in the table/index
  - For each column in the index: average number of rows with the same value

**Example:** `sqlite_stat1` for table `t1` with index `t1b` on column `b`:
```
t1|t1b|100 10
```
Means: 100 rows total, ~10 rows per distinct value of `b`.

### sqlite_stat4 table (optional, advanced)

Schema: `CREATE TABLE sqlite_stat4(tbl, idx, nEq, nLt, nDLt, sample)`

Stores histogram samples for more accurate selectivity estimation. Not needed for
the basic analyze7/analyzeE tests — sqlite_stat1 is sufficient.

### How the planner uses statistics (`src/where.c`)

1. For each possible access path (full scan, each index):
   a. Estimate the cost (number of rows read).
   b. For an index lookup: use `sqlite_stat1` to estimate how many rows match.
   c. For a full scan: cost = total rows.
2. Choose the path with the lowest estimated cost.
3. Report the chosen path in EXPLAIN QUERY PLAN.

**EXPLAIN QUERY PLAN output format:**
```
--SCAN t1                              (full table scan)
--SEARCH t1 USING INDEX t1b (b=?)      (index lookup with equality)
--SEARCH t1 USING INDEX t1b (b>? AND b<?) (index range scan)
--SEARCH t1 USING COVERING INDEX i1 (a=?) (covering index — no table lookup needed)
```

## Implementation Steps

### Step 1: Implement real ANALYZE command

**File:** `internal/exec/engine.go` — `execAnalyze`.

**Algorithm:**
```
For each table in the database (excluding sqlite_* internal tables):
  1. Count total rows: SELECT count(*) FROM table
  2. For each index on the table:
     a. For each column in the index:
        - Count distinct values: SELECT count(DISTINCT col) FROM table
        - Compute average rows per distinct value: totalRows / distinctCount
     b. Build the stat string: "totalRows avgRowsForCol1 avgRowsForCol2 ..."
     c. INSERT INTO sqlite_stat1 VALUES(table, index, stat)
  3. Also insert a row for the table itself (rowid index):
     INSERT INTO sqlite_stat1 VALUES(table, table, "totalRows")
```

**Reference**: `/Users/muaddib/dev/sqlite/src/analyze.c` — function
`analysisInfo` and `analyzeOneTable`.

**Verify:**
```bash
cd /Users/muaddib/dev/frigolite
# After ANALYZE, sqlite_stat1 should have real data
sqlite3 :memory: "CREATE TABLE t1(a,b); INSERT INTO t1 VALUES(1,1),(2,1),(3,2); CREATE INDEX t1b ON t1(b); ANALYZE; SELECT * FROM sqlite_stat1;"
# Expected output: t1|t1b|3 2
```

### Step 2: Implement cost-based index selection

**File:** `internal/exec/engine.go` — index selection logic (`shouldUseIndex`).

**Design:**
```go
type IndexCost struct {
    IndexName  string
    Cost       float64 // estimated number of rows to read
    IsCovering bool
    Constraints string // e.g., "b=?" or "b>? AND b<?"
}

func (e *Engine) estimateScanCost(table string, where Expr) float64 {
    // Full scan cost = total rows
    return float64(e.tableRowCount(table))
}

func (e *Engine) estimateIndexCost(table, index string, where Expr) IndexCost {
    // For each column in the index that has a constraint in WHERE:
    //   Look up sqlite_stat1 to get average rows per value
    //   Estimate matching rows
    // Return the estimated cost
}
```

**Algorithm:**
1. Parse the WHERE clause to find constraints on indexed columns.
2. For each index:
   a. Check which index columns have constraints (=, <, >, <=, >=, IN, IS NULL).
   b. Estimate matching rows using sqlite_stat1 statistics.
   c. If no statistics available, use a heuristic (e.g., 10 rows for equality,
      total/3 for range).
3. Compare costs: choose the index with the lowest estimated cost, unless full
   scan is cheaper.
4. A full scan is preferred if the index doesn't reduce rows enough (e.g., if
   more than ~25% of rows match, full scan is cheaper due to no index lookup overhead).

**Reference**: `/Users/muaddib/dev/sqlite/src/where.c` — functions
`whereLoopAddBtreeIndex`, `bestIndex`.

### Step 3: Implement EXPLAIN QUERY PLAN output

**File:** `internal/exec/engine.go` — `execExplain`.

**Current:** Always outputs `QUERY PLAN\n` + `` `--SCAN table ``.

**Fix:**
1. After the query planner selects an access path (Step 2), record the plan.
2. Format the plan as SQLite does:
   ```
   QUERY PLAN
   `--SCAN t1
   ```
   or
   ```
   QUERY PLAN
   `--SEARCH t1 USING INDEX t1b (b=?)
   ```
3. For multi-table queries (JOINs), output one line per table:
   ```
   QUERY PLAN
   |--SCAN t1
   `--SEARCH t2 USING INDEX t2a (a=?)
   ```

**Output format rules** (from `src/where.c:sqlite3WhereExplainOneScan`):
- `SCAN` = full table scan (no index).
- `SEARCH` = index is used.
- `USING INDEX idxname` = which index.
- `USING COVERING INDEX idxname` = covering index (all columns from index).
- `(constraint)` = which constraints drove the index choice, e.g., `(b=?)`.

**Verify:**
```bash
go test -v -count=1 -run '^TestSQLiteSuite/analyze7/analyze7-1.1' . 2>&1 | head -10
# Expected: PASS (SEARCH t1 USING INDEX t1b (b=?))
```

### Step 4: Implement automatic ANALYZE

**File:** `internal/exec/engine.go`.

**SQLite behavior**: When `PRAGMA automatic_index` or the `sqlite_stat1` table
exists and stats are stale, SQLite can automatically run ANALYZE.

Actually, the relevant PRAGMA is different. SQLite runs ANALYZE automatically when:
- The `sqlite_stat1` table exists AND
- The schema has changed significantly (e.g., many rows inserted since last ANALYZE)

For the `autoanalyze1` tests, the key behavior is:
1. After creating sqlite_stat1 and running queries, ANALYZE should run automatically.
2. The `sqlite_stat1` table should be populated.

**Implementation:**
- Track a "stats valid" flag.
- Set it to false after INSERT/UPDATE/DELETE.
- Before query planning, if stats are invalid and sqlite_stat1 exists, re-run ANALYZE.
- OR: just ensure ANALYZE works correctly (Step 1) and the tests call it explicitly.

**Check the actual test expectations:**
```bash
cat /Users/muaddib/dev/frigolite/ori/sqlite/test/autoanalyze1.test | head -50
```

### Step 5: Fix remaining analyze suite failures

After Steps 1–3, run each analyze suite and fix remaining issues:
```bash
for suite in analyze7 analyzeE autoanalyze1 analyzeC analyze6 analyze8 analyzeD; do
  echo "=== $suite ==="
  go test -v -count=1 -run "^TestSQLiteSuite/$suite" . 2>&1 | grep -c FAIL
done
```

Common remaining issues:
- EXPLAIN QUERY PLAN format nuances (spaces, parentheses).
- Index column order in the constraint string.
- Multi-column index handling.
- JOIN planning (nested loops vs hash join).

## Files Modified

| File | Change |
|------|--------|
| `internal/exec/engine.go` | Real ANALYZE; cost-based index selection; EXPLAIN QUERY PLAN output |
| `internal/exec/planner.go` (NEW, optional) | Extract planning logic into separate file for clarity |

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite
for suite in analyze7 analyzeE autoanalyze1 analyzeC analyze6 analyze8 analyzeD; do
  go test -v -count=1 -run "^TestSQLiteSuite/$suite" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq || echo "FAIL in $suite"
done
make quality
go test -run TestSOLID_ ./...
```

## Notes

### Heuristic fallback when no statistics

When `sqlite_stat1` is empty or doesn't exist, SQLite uses heuristics:
- Equality constraint (`col = value`): assume 1/N rows match (N = total rows, or
  a default like 10 if unknown).
- Range constraint: assume 1/3 of rows match.
- IS NULL: assume 1/10 of rows match.
- Full scan: N rows.

These heuristics are sufficient for tests that don't run ANALYZE but still expect
index usage. The analyze7 tests DO run ANALYZE, so real statistics are needed.
