# PLAN-P1-ALTER.md — ALTER TABLE Implementation

## Scope
Fix ALL ALTER TABLE operations to match SQLite behavior exactly, including error messages.

## Current Failures (128+)
| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| altertab3 | 41 | RENAME COLUMN doesn't actually rename; error messages don't match; trigger/view SQL not updated properly |
| alterlegacy | 33 | Legacy mode behavior; column rename without COLUMN keyword; schema updates |
| altertab2 | 22 | Various rename/add scenarios; error messages |
| altercons2 | 18 | Constraint handling during ALTER; add/drop constraint |
| alterdropcol | 8 | DROP COLUMN validation errors; edge cases |
| altercons3 | 4 | More constraint edge cases |
| altercorrupt | 3 | Corruption handling during ALTER |
| altermalloc2 | 2 | Out-of-memory / error handling |

## Implementation Steps

### Step 1: Fix ALTER TABLE RENAME COLUMN
**Current:** `execAlterTable` (line 4365-4387) — RENAME with `s.Column != ""` returns empty result without actual column rename.

**Fix:**
1. Implement column renaming in `execAlterTable` for the RENAME+COLUMN case
2. Update schema entry SQL to reflect new column name
3. Update triggers, views, and indexes that reference the renamed column
4. Use `renameUpdateRelatedEntries` for cascade updates
5. Handle both `RENAME a TO b` and `RENAME COLUMN a TO b` syntax

### Step 2: Fix ALTER TABLE error messages
**Current:** Error messages don't match SQLite's canonical error text.

**Required error messages:**
- `"no such table: %s"` — when table doesn't exist
- `"no such column: %q"` — when column doesn't exist
- `"cannot drop column from view %q"` — dropping from view
- `"cannot drop column from virtual table %q"` — virtual table
- `"cannot drop PRIMARY KEY column: %q"` — PK column
- `"cannot drop UNIQUE column: %q"` — UNIQUE column
- `"no such column: %q"` — column not found
- `"duplicate column name: %q"` — ADD COLUMN with existing name
- `"datatype mismatch"` — type conflicts

**Fix:** Compare each failure message in the test output with SQLite's output and adjust the engine's error messages.

### Step 3: Implement ALTER TABLE DROP COLUMN properly
**Current:** `execAlterTableDrop` (line 4675) has partial implementation.

**Fixes needed:**
1. Properly handle FK (foreign key) constraint dependencies
2. Handle CHECK constraint references to dropped column
3. Handle indexes using dropped column
4. Handle triggers referencing dropped column (existing but may differ from SQLite)
5. Handle views referencing dropped column (existing but may differ)
6. Update the btree structure — drop column from record format
7. Rebuild indexes that reference the dropped column
8. Handle edge case: dropping the only remaining column (should fail)
9. Handle edge case: dropping from tables with complex constraints

### Step 4: Implement ALTER TABLE ADD COLUMN properly
**Current:** `execAlterTableAdd` (line 4649) updates colCache but doesn't update schema SQL.

**Fixes needed:**
1. Update the stored CREATE TABLE SQL in schema to include the new column
2. Handle NOT NULL constraints on ADD COLUMN (SQLite requires DEFAULT for NOT NULL)
3. Handle DEFAULT values
4. Handle REFERENCES constraints on new column
5. Handle CHECK constraints on new column
6. Validate column name uniqueness

### Step 5: Implement ALTER TABLE ADD/DROP CONSTRAINT
**Current:** Parser recognizes ADD/DROP CONSTRAINT but engine handling may be incomplete.

**Fixes needed:**
1. ADD CONSTRAINT — update schema SQL with constraint
2. DROP CONSTRAINT — remove constraint from schema SQL (partially exists)
3. Validate constraint names exist before dropping

### Step 6: Fix RENAME TO for views, indexes, triggers
**Current:** `execAlterTableRename` (line 4389) updates schema and calls `renameUpdateRelatedEntries`.

**Fixes needed:**
1. Ensure all references to old table name in views are updated
2. Ensure all references in triggers are updated
3. Ensure all references in indexes (WHERE clauses) are updated
4. Handle self-referencing views (should fail)
5. Handle qualified references (`main.t1`) in SQL
6. Update internal caches (table root pages, etc.)

### Step 7: Implement PRAGMA legacy_alter_table properly
**Current:** Partial implementation that sets a field but may not affect behavior.

**Fixes needed:**
1. In legacy mode, ALTER TABLE RENAME should reject RENAME COLUMN syntax
2. In legacy mode, trigger/view validation should be relaxed for old-style renames

### Step 8: Handle corruption and error paths
**Current:** altercorrupt and altermalloc2 tests fail.

**Fixes needed:**
1. Handle malformed database schema during alter
2. Handle out-of-memory / allocation failures gracefully
3. Validate schema integrity before allowing alter operations

## Verification
```bash
# After each step run the specific failing suites:
go test -v -run "TestSQLiteSuite/altertab3" . 2>&1 | grep -E "PASS|FAIL"
go test -v -run "TestSQLiteSuite/alterlegacy" . 2>&1 | grep -E "PASS|FAIL"
go test -v -run "TestSQLiteSuite/altertab2" . 2>&1 | grep -E "PASS|FAIL"
go test -v -run "TestSQLiteSuite/altercons2" . 2>&1 | grep -E "PASS|FAIL"
go test -v -run "TestSQLiteSuite/alterdropcol" . 2>&1 | grep -E "PASS|FAIL"
```

## Completion Check
```bash
go test -v -run "TestSQLiteSuite/alter" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
# All ALTER TABLE suites pass completely
```
