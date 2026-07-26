# PLAN-P8-MISC.md — Miscellaneous Fixes

## Scope
Fix remaining test failures that don't fit into the major categories.

## Current Failures
| Test | Failures | Issue |
|------|----------|-------|
| atomic2 | 2 | Atomic commit behavior / ROLLBACK specifics |
| analyzeD | 2 | ANALYZE with corrupt database |
| analyze8 | 2 | ANALYZE with expressions in indexes |
| atomic | 2 | Atomic DML operations |
| atof1 | (if passing) | Float parsing |
| atof2 | (if passing) | Float parsing edge cases |
| TestUpdateWithExpr | 1 | UPDATE with expression evaluation |

### 1. TestUpdateWithExpr
**Current:** Fails with "expected 1 row, got 0"

**Root cause:** UPDATE SET val = id, id = val — the expression evaluation for the SET clause may not handle column references properly when columns are being swapped.

**Fix:** Ensure UPDATE SET clause processes all value expressions using the pre-update row state (before any column modifications), matching SQLite behavior.

### 2. atomic2
**Current:** Fails with result mismatch or exec errors

**Issue:** Atomic transaction handling — need to ensure:
1. BEGIN IMMEDIATE / BEGIN EXCLUSIVE parsing
2. Transaction state tracking for nested savepoints
3. Proper ROLLBACK behavior in edge cases
4. Error recovery during atomic operations

**Fix:** Implement proper transaction state machine and atomic commit logic.

### 3. TestHarness/cleanExpected issues
**Issue:** Some tests may fail due to the `cleanExpected` function not correctly parsing TCL-formatted expected values, especially:
1. Nested braces in expected values
2. Empty list representations
3. NULL value representations

**Fix:** Audit the harness and fix edge cases in expected value parsing.

### 4. Query result formatting
**Issue:** The `formatSQLiteValue` function (used by `flattenResult` for comparison) may not format values identically to SQLite's default string representation.

**Fix:** Ensure value formatting matches SQLite:
- Integers: no decimal point
- Reals: with decimal point, may use scientific notation
- Text: as-is
- Blobs: hex representation with X'...' prefix (or just hex)
- NULL: "NULL"

### 5. PRAGMA handling gaps
Some tests may use PRAGMAs that aren't implemented:
- `PRAGMA schema_version`
- `PRAGMA user_version`
- `PRAGMA application_id`
- `PRAGMA page_count`

**Fix:** Implement missing PRAGMAs as needed based on test failures.

## Implementation Steps (unordered — fix as encountered)

### Step 1: Fix TestUpdateWithExpr
1. Trace UPDATE execution for `SET val = id, id = val`
2. Ensure column values from pre-update row are used for all SET expressions
3. Current code evaluates right-hand sides using the pre-update row — verify this works

### Step 2: Fix atomic transaction handling
1. Implement BEGIN IMMEDIATE and BEGIN EXCLUSIVE
2. Ensure ROLLBACK properly reverts all changes within the transaction
3. Handle edge cases: ROLLBACK after error, nested savepoints

### Step 3: Audit and fix test harness
1. Test cleanExpected with complex TCL list formats
2. Test flattenResult with various data types
3. Ensure error message matching works for all test patterns

## Verification
```bash
go test -v -run "TestUpdateWithExpr" .
go test -v -run "TestSQLiteSuite/atomic" .
```

## Completion Check
```bash
go test -v -run "TestUpdateWithExpr" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/atomic" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```
