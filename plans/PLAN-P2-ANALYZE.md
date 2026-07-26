# PLAN-P2-ANALYZE.md — ANALYZE Command Implementation

## Scope
Implement ANALYZE command to collect and use table/index statistics for query planning.

## Current Failures (57+)
| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| analyzeC | 24 | ANALYZE creates/manages sqlite_stat1 table |
| analyzeE | 23 | ANALYZE with various edge cases |
| analyze7 | 14 | ANALYZE behavior with specific schemas |
| autoanalyze1 | 11 | Automatic analyze after data changes |
| analyze6 | 3 | ANALYZE with indexes |
| analyzeD | 2 | ANALYZE with corruption |
| analyze8 | 2 | ANALYZE with expressions in indexes |

## Current State
```go
func (e *Engine) execAnalyze(s *sql.AnalyzeStmt) *Result {
    // ANALYZE is a no-op in this implementation
    return &Result{}
}
```

## Implementation Approach
ANALYZE in SQLite scans tables, collects statistics about row distribution, and stores them in the `sqlite_stat1` (and optionally `sqlite_stat4`) tables. These statistics help the query planner choose better index strategies.

Since Frigolite implements B-tree operations directly, we need:

1. **sqlite_stat1 table** — a real table that ANALYZE creates/updates
   - Schema: `CREATE TABLE sqlite_stat1(tbl, idx, stat)`
   - Contains one row per index with statistics string (e.g., "10000 500 100" meaning: est rows=10000, est rows per prefix=500, 100)

2. **ANALYZE command** — scan each table/index and compute statistics
   - Count total rows in each table
   - For each index, compute the number of distinct prefix values
   - Store in sqlite_stat1

3. **Stat lookup during query planning** — use statistics for join ordering and index selection
   - Current heuristic cost estimation (line 776: `"rough heuristic"`) should use ANALYZE data

## Implementation Steps

### Step 1: Create sqlite_stat1 infrastructure
1. Define the sqlite_stat1 table schema as a system table (like sqlite_master)
2. Create table on first ANALYZE if it doesn't exist
3. Support INSERT/UPDATE/DELETE on sqlite_stat1 (for test purposes)
4. Support `SELECT ... FROM sqlite_stat1` in queries

### Step 2: Implement ANALYZE table scan
1. For each table, compute row count (total pages × leaf cell count heuristic)
2. For each index, compute distinct key prefix counts:
   - For a single-column index: count of distinct values
   - For a multi-column index: count of distinct (col1) and (col1, col2) etc.
3. Compute using B-tree traversal (count cells in each page)
4. Use sampling for large tables (SQLite default: sample ~100 rows per leaf page)

### Step 3: Implement ANALYZE name resolution
1. `ANALYZE` — analyze all tables and indexes in all attached databases
2. `ANALYZE table_name` — analyze specific table and its indexes
3. `ANALYZE schema_name.table_name` — analyze table in specific schema

### Step 4: Read statistics during query planning
1. When a table has indexes, look up sqlite_stat1 for statistics
2. Use stat data for join ordering:
   - Estimate output row count using stat data
   - Prefer smaller tables as inner tables in hash joins
3. Use stat data for index selection:
   - Prefer indexes with higher selectivity (fewer rows per prefix)

### Step 5: Handle sqlite_stat1 modifications
1. DROP TABLE on sqlite_stat1 should work (to clear statistics)
2. INSERT/UPDATE/DELETE on sqlite_stat1 should update the query planner
3. ANALYZE should re-populate sqlite_stat1

### Step 6: Handle regression/edge cases
1. Empty tables
2. Tables with no indexes
3. Indexes on expressions
4. Partial indexes (with WHERE clause)
5. UNIQUE indexes
6. Large tables (performance)

## Verification
```bash
go test -v -run "TestSQLiteSuite/analyze" . 2>&1 | grep -E "PASS|FAIL"
go test -v -run "TestSQLiteSuite/autoanalyze1" . 2>&1 | grep -E "PASS|FAIL"
```

## Completion Check
```bash
go test -v -run "TestSQLiteSuite/analyze" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/autoanalyze1" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files to Modify
- `internal/exec/engine.go` — `execAnalyze` function, query planner
- `internal/schema/schema.go` — sqlite_stat1 system table
- `internal/storage/storage.go` — if needed for stat storage
