# PLAN-P2-ANALYZE.md — ANALYZE Command Implementation (Updated 2026-07-27)

## Scope
Implement ANALYZE command to collect and use table/index statistics for query planning.

## Current Failures (~48 across 7 suites)

| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| analyze7 | 15 | ANALYZE behavior with specific schemas |
| analyzeE | 14 | ANALYZE with various edge cases |
| autoanalyze1 | 10 | Automatic analyze after data changes |
| analyzeC | 5 | ANALYZE creates/manages sqlite_stat1 table |
| analyze6 | 2 | ANALYZE with indexes |
| analyze8 | 1 | ANALYZE with expressions in indexes |
| analyzeD | 1 | ANALYZE with corruption |

## Current State
```go
func (e *Engine) execAnalyze(s *sql.AnalyzeStmt) *Result {
    // ANALYZE is a no-op in this implementation
    return &Result{}
}
```

## Implementation Steps (Ordered)

### Step 1: Create sqlite_stat1 infrastructure
**Files:** `internal/schema/schema.go`, `internal/exec/engine.go`

Define the sqlite_stat1 system table and make it accessible to queries/operations:
1. Define `sqlite_stat1` schema: `CREATE TABLE sqlite_stat1(tbl, idx, stat)`
2. Register as a system table (like sqlite_master but real)
3. Support INSERT/UPDATE/DELETE on sqlite_stat1 (for test purposes)
4. Support `SELECT ... FROM sqlite_stat1` in queries

**Verify:** `go test -v -run "TestSQLiteSuite/analyzeC" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

### Step 2: Implement ANALYZE table scan
**Files:** `internal/exec/engine.go`

Implement the actual ANALYZE scan logic:
1. For each table, compute row count (total pages × leaf cell count heuristic)
2. For each index, compute distinct key prefix counts:
   - For a single-column index: count of distinct values
   - For a multi-column index: count of distinct (col1) and (col1, col2) etc.
3. Compute using B-tree traversal (count cells in each page)
4. Use sampling for large tables (SQLite default: sample ~100 rows per leaf page)
5. Store results in sqlite_stat1 table

**Verify:** `go test -v -run "TestSQLiteSuite/analyze7" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

### Step 3: Implement ANALYZE name resolution
**Files:** `internal/exec/engine.go`

Handle all ANALYZE syntax variants:
1. `ANALYZE` — analyze all tables and indexes in all attached databases
2. `ANALYZE table_name` — analyze specific table and its indexes
3. `ANALYZE schema.table_name` — analyze table in specific schema

**Verify:** `go test -v -run "TestSQLiteSuite/analyzeE" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

### Step 4: Read statistics during query planning
**Files:** `internal/exec/engine.go`

Use sqlite_stat1 data for cost estimation:
1. When a table has indexes, look up sqlite_stat1 for statistics
2. Use stat data for join ordering:
   - Estimate output row count using stat data
   - Prefer smaller tables as inner tables
3. Use stat data for index selection:
   - Prefer indexes with higher selectivity (fewer rows per prefix)

### Step 5: Implement auto-analyze
**Files:** `internal/exec/engine.go`

Implement automatic ANALYZE after data changes (triggers autoanalyze1 tests):
1. Track table modification count
2. After threshold modifications, automatically run ANALYZE
3. Skip if already analyzed recently

**Verify:** `go test -v -run "TestSQLiteSuite/autoanalyze1" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

### Step 6: Handle regression/edge cases
**Files:** `internal/exec/engine.go`

1. Empty tables (analyzeC-1.3x)
2. Tables with no indexes
3. Indexes on expressions (analyze8-3.0)
4. Partial indexes (with WHERE clause)
5. UNIQUE indexes
6. Large tables (performance)
7. Corruption handling (analyzeD-1.5)

**Verify:** `go test -v -run "TestSQLiteSuite/analyze6|analyze8|analyzeD" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

## Completion Check

```bash
go test -v -run "TestSQLiteSuite/analyze" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/autoanalyze1" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files to Modify

| File | Role |
|------|------|
| `internal/exec/engine.go` | `execAnalyze` function, query planner, auto-analyze |
| `internal/schema/schema.go` | sqlite_stat1 system table definition and storage |
| `internal/btree/btree.go` | B-tree traversal for stat collection |

## Go Standard Library Usage

| Feature | Go stdlib |
|---------|-----------|
| stat table storage | `map[string]*StatEntry` |
| Sorting index keys | `sort.Slice()` |
| String parsing | `strconv.Atoi()` for stat parsing |

## Goal Integration

```json
{
  "objective": "Implement full ANALYZE command: create sqlite_stat1 table, scan tables/indexes for statistics, store and use stats in query planning, implement auto-analyze after data changes",
  "completionCriterion": "All ANALYZE suites pass with zero FAIL: analyzeC, analyzeE, analyze7, autoanalyze1, analyze6, analyze8, analyzeD",
  "verifyCommand": "go test -v -run \"TestSQLiteSuite/analyze\" . 2>&1 | grep -c \"FAIL\" | xargs test 0 -eq && go test -v -run \"TestSQLiteSuite/autoanalyze1\" . 2>&1 | grep -c \"FAIL\" | xargs test 0 -eq"
}
```
