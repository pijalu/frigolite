# PLAN-PF0-AGGREGATE.md — Aggregate Function Fixes

## Scope
Fix aggregate function evaluation so all aggregate tests pass correctly.

## Current Failures
- **Manual tests (7):** TestAggregateSum, TestAggregateAvg, TestAggregateMinMax, TestAggregateTotal, TestAggregateGroupConcat, TestAggregateWithWhere
- **Compat sub-tests (25):** aggnested (12), aggorderby (13)

## Root Causes Identified
1. **Aggregates returning struct objects** — In some paths, the aggregator struct itself is returned instead of calling `Final()`. This is visible when test output shows `&{0 84}` (groupConcatAgg with `values: []string(nil), sep: ","` → struct dump because `Final()` path isn't reached)
2. **ORDER BY in aggregates broken** — GROUP_CONCAT and other aggregates with ORDER BY produce wrong output format
3. **Aggregate-evaluation path for non-aggregate columns in aggregate queries** — When a SELECT has both aggregate and non-aggregate columns, the non-aggregate column value comes from an incorrect row
4. **AVG returns 0** — Empty-set handling or type conversion issue
5. **GROUP_CONCAT separator handling** — Separator from the *last* Step call overwrites previous separator, should use first provided

## Implementation Steps

### Step 1: Debug the struct-returning path
1. Add a test case at `TestAggregateMinMax` level to isolate
2. Trace `evalAggFuncCall` (line 3359) — confirm `agg.Final()` always called
3. Check `evalAggregateExpr` (line 3343) for fallback paths that skip `Final()`
4. Check `evalExpr` (line 5334, FuncCall case) for unhandled aggregate paths
5. **Fix:** Ensure all code paths for aggregate FuncCall resolve through `Final()`

### Step 2: Fix GROUP_CONCAT with ORDER BY
1. Trace the `aggorderby` test failures — output shows `&{values sep}` struct
2. Verify the GROUP_CONCAT `Step()` receives correct values after ORDER BY sorting
3. **Check:** The issue may be in a `rowMaps` being empty or wrong data flowing through
4. **Fix:** Ensure proper ordering before calling `Step()`; verify separator consistency

### Step 3: Fix aggregate empty-set handling
1. `evalAggregatesEmpty` (line 3092) — verify all aggregate functions produce correct empty-set results
2. COUNT(*) should return 0 (already works)
3. SUM should return NULL (nil)
4. AVG should return NULL (nil)
5. TOTAL should return 0.0 (already correct in `totalAgg.Final()`)
6. MIN/MAX should return NULL (nil)
7. GROUP_CONCAT should return NULL (nil)
8. **Fix:** Add proper empty-set handling for each aggregate in `evalAggregatesEmpty`

### Step 4: Fix non-aggregate column evaluation in aggregate context
1. In `evalAggregates` (line 3034), when a column is bare (non-aggregate) in an aggregate query, its value comes from `rowMaps[0]`
2. This is wrong for GROUP BY queries — SQLite requires bare columns in GROUP BY or aggregate context
3. **Fix:** For non-aggregate columns, use the correct row (first for MIN, last for MAX, first for no ORDER BY)
4. But actually SQLite errors on non-aggregate bare columns in aggregate queries without GROUP BY... let me check the test cases

### Step 5: Fix nested aggregate handling
1. `aggnested` tests fail with wrong results
2. Check `findNestedAggregate` (line 3428) — detects nested aggregates
3. SQLite prohibits nested aggregates but some tests expect `string_agg(a1,'x')` to work with subquery
4. **Fix:** Ensure proper detection and error messages for illegal nested aggregates; proper evaluation for legal ones

## Verification
```bash
# After each step, run:
go test -v -run "TestAggregate" .
go test -v -run "TestSQLiteSuite/aggnested" .
go test -v -run "TestSQLiteSuite/aggorderby" .
```

## Completion Check
```bash
# All 7 manual aggregate tests pass:
go test -v -run "TestAggregate" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
# No aggnested or aggorderby failures:
go test -v -run "TestSQLiteSuite/aggnested" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/aggorderby" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```
