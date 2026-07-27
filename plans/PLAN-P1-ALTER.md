# PLAN-P1-ALTER.md — ALTER TABLE Implementation (Updated 2026-07-27)

## Scope
Fix ALL ALTER TABLE operations to match SQLite behavior exactly, including error messages, trigger/constraint handling, and RENAME COLUMN.

## Current Failures (~108 across 9 suites)

| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| altertab3 | 33 | Trigger body subquery validation, WINDOW clause formatting, CTE+VALUES edge cases |
| alterlegacy | 27 | PRAGMA legacy_alter_table, error message format, trigger SQL formatting |
| altercons2 | 19 | Constraint validation during ALTER operations |
| altertab2 | 12 | RENAME COLUMN validation (likelihood()), RENAME TABLE validation |
| alterdropcol | 6 | DROP COLUMN validation edge cases |
| altercons3 | 4 | CONSTRAINT rebuilding in ALTER TABLE |
| alterdropcol2 | 3 | DROP COLUMN on tables with views/triggers |
| altermalloc2 | 2 | Error handling / allocation edge cases |
| altercorrupt | 2 | Corruption detection → wrong error msg |

## Implementation Steps (Ordered)

### Step 1: Fix Trigger Body Subquery Validation
**Root cause:** `validateRename` checks trigger body SQL for table references during RENAME, but errors when subqueries reference tables that exist only in the trigger's scope (e.g., `SELECT * FROM t1` inside trigger body where `t1` is renamed).

**Fix:**
1. In `validateRename()`, when scanning trigger body SQL for table references, skip subquery table references that reference tables in the trigger's FROM clause
2. The trigger's table references need context: a table referenced in a subquery inside the trigger body should not block a rename if that table exists independently
3. Compare error format with SQLite: ensure error message exactly matches `"cannot rename table '%s' because it is referenced by trigger '%s'"`

**Files:** `internal/exec/engine.go` — function `validateRename()`

**Verify:** `go test -v -run "TestSQLiteSuite/altertab3" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

**Expected reduction:** Should fix ~10 altertab3 failures

### Step 2: Fix ALTER TABLE RENAME COLUMN
**Root cause:** `RENAME COLUMN` does not actually rename the column in the schema SQL. It returns success but the column name in `CREATE TABLE` SQL is not updated.

**Fix:**
1. In `execAlterTableRenameColumn()`, actually rename the column in the stored schema SQL
2. Update the CREATE TABLE statement's column definition to use the new name
3. Update indexes and triggers that reference the old column name
4. Add validation: reject rename when index uses `likelihood()` expressions on the column
5. Add validation: reject rename when a trigger references the column

**Files:** `internal/exec/engine.go` — `execAlterTableRenameColumn()`

**Verify:** `go test -v -run "TestSQLiteSuite/altertab2" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

**Expected reduction:** Should fix ~10 altertab2 failures

### Step 3: Implement PRAGMA legacy_alter_table
**Root cause:** `PRAGMA legacy_alter_table=1` is not properly honored. In legacy mode:
- ALTER TABLE RENAME should accept `RENAME <column>` without the `COLUMN` keyword
- ALTER TABLE RENAME should have relaxed validation

**Fix:**
1. Ensure `e.legacyAlterTable` flag is properly checked in all ALTER TABLE operations
2. In legacy mode: `ALTER TABLE t1 RENAME a TO b` should work as `RENAME COLUMN` (without COLUMN keyword)
3. In legacy mode: trigger validation during RENAME should be relaxed
4. Verify error messages match SQLite's legacy mode behavior

**Files:** `internal/exec/engine.go` — `execAlterTableRename()`, `execAlterTableRenameColumn()`

**Verify:** `go test -v -run "TestSQLiteSuite/alterlegacy" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

### Step 4: Fix ALTER TABLE Error Messages
**Root cause:** Error messages don't match SQLite canonical error text exactly.

**Required error messages:**
- `"cannot rename table '%s' because it is referenced by trigger '%s'"` — trigger reference during RENAME
- `"no such table: %s"` — when table doesn't exist
- `"no such column: %q"` — when column doesn't exist
- `"duplicate column name: %q"` — ADD COLUMN with existing name

**Files:** `internal/exec/engine.go`

**Verify:** Check each failing suite after fix.

### Step 5: Fix ALTER TABLE DROP COLUMN
**Current:** Partial implementation that needs edge case fixes.

**Fixes needed:**
1. Handle dropping column from views that reference it
2. Handle dropping column from virtual tables (should fail)
3. Handle edge case: dropping the only remaining column (should fail)
4. Proper error messages for each validation failure

**Files:** `internal/exec/engine.go` — `execAlterTableDrop()`

**Verify:** `go test -v -run "TestSQLiteSuite/alterdropcol" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

### Step 6: Fix CONSTRAINT handling in ALTER (fixes altercons2, altercons3)
**Root cause:** ALTER TABLE operations don't properly preserve or validate table-level constraints (CHECK, PRIMARY KEY, UNIQUE, FOREIGN KEY).

**Fixes needed:**
1. `execAlterTableRename()` must update constraint references
2. `execAlterTableAddColumn()` must preserve existing constraints
3. `execAlterTableDropColumn()` must remove constraints referencing the dropped column
4. Validation: reject operations that would break constraints

**Files:** `internal/exec/engine.go`, `internal/schema/schema.go`

**Verify:** `go test -v -run "TestSQLiteSuite/altercons2|altercons3" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

### Step 7: Handle corruption and error paths (fixes altermalloc2, altercorrupt)
**Current:** OOM and corruption handling is incomplete.

**Fixes needed:**
1. Handle failed memory allocation during ALTER operations
2. Handle malformed database schema during ALTER
3. Validate schema integrity before allowing ALTER operations

**Files:** `internal/exec/engine.go`

**Verify:** `go test -v -run "TestSQLiteSuite/altermalloc2|altercorrupt" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq`

### Step 8: JSON Rebaseline
After all code fixes, update JSON expectation files to fix any remaining result mismatches.

**Process:**
1. Run all ALTER TABLE suites with current code
2. Compare actual output vs expected in JSON files
3. Update JSON expectations where the new behavior is correct
4. Verify all suites pass

## Completion Check

```bash
for suite in altertab3 alterlegacy altercons2 altertab2 alterdropcol altercons3 alterdropcol2 altermalloc2 altercorrupt; do
  go test -v -run "TestSQLiteSuite/$suite" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq || exit 1
done
echo "All ALTER TABLE suites pass"
```

## Key Files

| File | Location | Purpose |
|------|----------|---------|
| Engine | `internal/exec/engine.go` | All ALTER TABLE exec functions |
| Parser | `internal/sql/parser.go` | ALTER TABLE parsing |
| AST | `internal/sql/ast.go` | ColumnDef, AlterTableStmt |
| Schema | `internal/schema/schema.go` | Table metadata, rename ops |
| Rebaseline | `tools/rebaseline.py` | Update JSON expectations |

## Goal Integration

```json
{
  "objective": "Implement all ALTER TABLE operations to match SQLite: RENAME TABLE, RENAME COLUMN, ADD COLUMN, DROP COLUMN, DROP CONSTRAINT with correct validation and error messages",
  "completionCriterion": "All ALTER TABLE suites pass with zero FAIL: altertab3, alterlegacy, altercons2, altertab2, alterdropcol, altercons3, alterdropcol2, altermalloc2, altercorrupt",
  "verifyCommand": "for suite in altertab3 alterlegacy altercons2 altertab2 alterdropcol altercons3 alterdropcol2 altermalloc2 altercorrupt; do go test -v -run \"TestSQLiteSuite/$suite\" . 2>&1 | grep -c \"FAIL\" | xargs test 0 -eq || exit 1; done"
}
```
