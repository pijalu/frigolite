# G05 — ALTER TABLE Token-Level Rename

> **Prerequisite**: G01 (engine decomposed — `internal/exec/alter.go` exists), G03 (full parser — trigger/view bodies may contain window functions/CTE).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/alter.c` (entire file — the spec for all ALTER operations).
> **Goal**: Implement ALTER TABLE RENAME using token-level processing, matching SQLite's behavior exactly. Fix all alter-related test failures.

---

## Context

SQLite's ALTER TABLE RENAME COLUMN / RENAME TABLE works by **token-level replacement** in
the stored text of triggers, views, and indexes — NOT by AST rewriting. When you rename
column `old` to `new`, SQLite:

1. Finds every trigger/view/index that references the table
2. Re-parses the stored SQL text
3. Walks the parse tree to find tokens matching the old name
4. Replaces them in the **original text** (preserving formatting)
5. Stores the updated text back

The token-level rename infrastructure already exists in `internal/exec/rename.go`
(`FindRenameTokens`, `ApplyRenames`). The issue is completeness and correctness of the
ALTER TABLE dispatch in `internal/exec/alter.go`.

---

## Current State

**File**: `internal/exec/alter.go` (1 037+ lines) — the ALTER TABLE dispatcher.
**File**: `internal/exec/rename.go` (362 lines) — the token-level rename engine.

Current functions in `alter.go`:
```go
func (e *Engine) execAlterTable(s *sql.AlterTableStmt) *Result              // line 18 — dispatcher
func (e *Engine) execAlterTableRename(s *sql.AlterTableStmt) *Result         // line 40 — RENAME TO
func (e *Engine) execAlterTableRenameColumn(s *sql.AlterTableStmt) *Result   // line 70 — RENAME COLUMN
func (e *Engine) execAlterTableAdd(s *sql.AlterTableStmt) *Result            // line 1037 — ADD COLUMN
func (e *Engine) execAlterTableDrop(s *sql.AlterTableStmt) *Result           // DROP COLUMN
func (e *Engine) execAlterTableAlter(s *sql.AlterTableStmt) *Result          // ALTER COLUMN
```

Token-level rename engine in `rename.go`:
```go
type RenameContext struct { ... }                                    // line ~20
func FindRenameTokens(sqlText string, ctx *RenameContext) ([]RenameRange, error)  // line 38
func ApplyRenames(sqlText string, ranges []RenameRange, replacement string) string // line 340
```

Failing tests:
- `altertab3.test` — 129 test cases, many failing
- `alterlegacy.test` — legacy ALTER TABLE behavior
- `altercons2.test` — constraint-related ALTER
- `altertab2.test` — column rename edge cases

---

## SQLite Reference

### ALTER TABLE RENAME TABLE (`alter.c:sqlite3AlterRenameTable`)
1. Get the CREATE TABLE SQL text
2. Find the old table name token in the text (it appears after `CREATE TABLE`)
3. Replace it with the new name
4. For every trigger/index/view that references the table:
   - Get the stored SQL text
   - Re-parse it
   - Walk the parse tree to find the table name token
   - Replace it in the original text
5. Update `sqlite_schema` entries

### ALTER TABLE RENAME COLUMN (`alter.c:sqlite3AlterRenameColumn`)
1. Get the CREATE TABLE SQL text
2. Re-parse it to build the column list
3. For each occurrence of the old column name in the text:
   - Check if it's an identifier token (not in a string, not part of a larger identifier)
   - Replace with the new name
4. For every trigger/view that references the column:
   - Same token-level replacement process
5. Update `sqlite_schema` entries

### `legacy_alter_table` PRAGMA (`alter.c` + `pragma.c`)
When `PRAGMA legacy_alter_table=ON`, SQLite uses the old (pre-3.25) behavior which only
does string replacement in CREATE TABLE text, not in triggers/views.

---

## Implementation Steps

### Step 1: Verify token-level name replacement works correctly

**File**: `internal/exec/rename.go`

The `FindRenameTokens` function (line 38) parses SQL text and finds all identifier tokens
matching the rename target. `ApplyRenames` (line 340) applies the replacements.

Signature (already exists):
```go
// FindRenameTokens parses sqlText, finds all identifier tokens matching ctx.Target,
// and returns their byte ranges. Only matches complete identifier tokens — not
// substrings, not string literals.
// Reference: SQLite alter.c:sqlite3RenameExprUnquote — token walking logic.
func FindRenameTokens(sqlText string, ctx *RenameContext) ([]RenameRange, error)

// ApplyRenames replaces the byte ranges in sqlText with the replacement string.
func ApplyRenames(sqlText string, ranges []RenameRange, replacement string) string
```

**Critical edge cases to verify**:
- `t1.old_col` vs `t2.old_col` — only rename when qualification matches the target table
- Names in string literals (`'old_col'`) must NOT be renamed
- Names that are substrings of other identifiers (`old_col_suffix`) must NOT be renamed
- Case-insensitive matching (SQLite identifiers are case-insensitive)

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/alter.c` function
`sqlite3AlterRenameColumn()` (the column rename entry point) and `tokenize.c` (the
tokenizer used to walk SQL text).

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
go test -v -count=1 -run "^TestSQLiteSuite/altertab2$" . 2>&1 | tail -10
```
**Expected outcome**: `go build ./...` succeeds. altertab2 tests show current pass/fail/skip
state. Record the baseline count.

### Step 2: Implement ALTER TABLE RENAME TABLE correctly

**File**: `internal/exec/alter.go`, function `execAlterTableRename` (line 40)

```go
// execAlterTableRename handles ALTER TABLE old_name RENAME TO new_name.
// Reference: SQLite alter.c:sqlite3AlterRenameTable() (~line 500).
func (e *Engine) execAlterTableRename(s *sql.AlterTableStmt) *Result {
    oldName := s.Table
    newName := s.NewName

    // 1. Update CREATE TABLE text — find the table name token after CREATE TABLE
    entry := e.schema.FindTable(oldName)
    createSQL := entry.SQL
    newCreateSQL := renameTableInCreateSQL(createSQL, oldName, newName)

    // 2. Update all referencing triggers, views, indexes
    //    Only when legacy_alter_table is OFF (default)
    if !e.legacyAlterTable {
        entries, _ := e.schema.GetEntries("")
        ctx := &RenameContext{Table: oldName, NewTable: newName}
        for _, e2 := range entries {
            if referencesTable(e2, oldName) {
                ranges, _ := FindRenameTokens(e2.SQL, ctx)
                e2.SQL = ApplyRenames(e2.SQL, ranges, newName)
                if e2.TblName == oldName {
                    e2.TblName = newName
                }
            }
        }
    }

    // 3. Update sqlite_schema entry
    entry.SQL = newCreateSQL
    entry.Name = newName
    e.schema.RenameEntry(oldName, newName)

    // 4. Re-parse and update column cache
    e.invalidateColumnCache(oldName)
    return &Result{}
}
```

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/alter.c` function
`sqlite3AlterRenameTable()` (approximately line 500) and `renameTableTrigger()` (the
function that walks trigger bodies).

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
go test -v -count=1 -run "^TestSQLiteSuite/altertab3$" . 2>&1 | grep -c "FAIL"
```
**Expected outcome**: FAIL count for altertab3 decreases. `go build` succeeds. The FAIL
count should be lower than the baseline from Step 1.

### Step 3: Implement ALTER TABLE RENAME COLUMN correctly

**File**: `internal/exec/alter.go`, function `execAlterTableRenameColumn` (line 70)

```go
// execAlterTableRenameColumn handles ALTER TABLE t RENAME COLUMN old TO new.
// Reference: SQLite alter.c:sqlite3AlterRenameColumn() (~line 800).
func (e *Engine) execAlterTableRenameColumn(s *sql.AlterTableStmt) *Result {
    table := s.Table
    oldCol := s.OldColumnName
    newCol := s.NewColumnName

    // 1. Update CREATE TABLE text (column definition + constraints)
    entry := e.schema.FindTable(table)
    ctx := &RenameContext{
        Table:    table,
        Column:   oldCol,
        NewColumn: newCol,
    }
    ranges, _ := FindRenameTokens(entry.SQL, ctx)
    entry.SQL = ApplyRenames(entry.SQL, ranges, newCol)

    // 2. Update all triggers and views referencing this column
    //    Only when legacy_alter_table is OFF
    if !e.legacyAlterTable {
        entries, _ := e.schema.GetEntries("")
        for _, e2 := range entries {
            if e2.TblName == table && (e2.Type == "trigger" || e2.Type == "view") {
                ranges, _ := FindRenameTokens(e2.SQL, ctx)
                e2.SQL = ApplyRenames(e2.SQL, ranges, newCol)
            }
        }
    }

    // 3. Update index definitions if they reference the column
    // 4. Update column cache
    e.invalidateColumnCache(table)
    return &Result{}
}
```

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/alter.c` function
`sqlite3AlterRenameColumn()` (approximately line 800) and `renameColumnTrigger()`.

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
go test -v -count=1 -run "^TestSQLiteSuite/(altertab2|altertab3)$" . 2>&1 | grep -c "FAIL"
```
**Expected outcome**: FAIL count decreases from baseline. Column renames work in trigger
and view bodies.

### Step 4: Handle other ALTER TABLE operations

**File**: `internal/exec/alter.go`

- `ALTER TABLE ADD COLUMN` (`execAlterTableAdd`, line 1037): append column to CREATE TABLE
  text, update column cache
  - **SQLite ref**: `alter.c:sqlite3AlterFinishAddColumn()` (~line 700)
- `ALTER TABLE DROP COLUMN` (`execAlterTableDrop`): remove from CREATE TABLE text
  (token-level), rebuild
  - **SQLite ref**: `alter.c:sqlite3AlterDropColumn()` 
- `ALTER TABLE RENAME TO`: same as RENAME TABLE (dispatched by `execAlterTableRename`)
- `ALTER TABLE DROP CONSTRAINT`: remove constraint from text

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
go test -v -count=1 -run "^TestSQLiteSuite/altertab" . 2>&1 | grep -c "FAIL"
```
**Expected outcome**: FAIL count stable or decreasing. `go build` succeeds.

### Step 5: Handle qualified column references in triggers

When renaming a column, trigger bodies may reference it as `OLD.colname` or `NEW.colname`.
The token-level replacement must handle both `OLD.oldname`, `NEW.oldname`, and bare
`oldname`.

The `RenameContext` struct in `rename.go` already tracks the table name so that
`collectColumnRefRange` (line 287) can check whether a qualified reference
(`t1.old_col`) belongs to the table being altered.

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/alter.c` function
`renameColumnTrigger()` — walks trigger body expressions and finds column references.

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go test -v -count=1 -run "^TestSQLiteSuite/altertrigger" . 2>&1 | tail -10
```
**Expected outcome**: Trigger column references are renamed correctly. No FAIL lines.

### Step 6: Fix `legacy_alter_table` PRAGMA

**File**: `internal/exec/pragma.go` and `internal/exec/alter.go`

```go
// legacy_alter_table=ON: only update CREATE TABLE text, not triggers/views
// legacy_alter_table=OFF (default): update everything
// Reference: SQLite alter.c and pragma.c
```

The `Engine.legacyAlterTable` field (engine.go line 62) must be set by the PRAGMA handler.

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/pragma.c` function `pragmaHandler()`
(search for "legacy_alter_table") and `/Users/muaddib/dev/sqlite/src/alter.c` function
`sqlite3AlterRenameTable()` (the `if( bLegacy )` guard).

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
go test -v -count=1 -run "^TestSQLiteSuite/alterlegacy$" . 2>&1 | tail -10
```
**Expected outcome**: alterlegacy tests pass — legacy mode only updates CREATE TABLE text,
non-legacy mode updates triggers/views too.

---

## Files Modified

| File | Lines (est.) | Change |
|------|------|--------|
| `internal/exec/alter.go` | 1 037+ | Complete RENAME TABLE, RENAME COLUMN, ADD/DROP COLUMN; `legacy_alter_table` guard |
| `internal/exec/rename.go` | 362 | Verify/fix token-level matching edge cases |
| `internal/exec/pragma.go` | ~400 | `legacy_alter_table` PRAGMA dispatch |

---

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# 1. ALTER TABLE tests pass
go test -count=1 -run "^TestSQLiteSuite/(altertab|alterlegacy|altercons)" .

# 2. No regressions in other tests
make quality
go test -run TestSOLID_ ./...

# 3. Token-level rename is correct
go test -v -count=1 -run "^TestSQLiteSuite/altertab3$" . 2>&1 | grep -c "FAIL"
# Should be 0 (or close to 0 — any remaining FAILs should be documented)
```

**Expected final outcome**:
- `go test -count=1 -run "^TestSQLiteSuite/(altertab|alterlegacy|altercons)" .` → ok (all pass)
- `make quality` → passes (vet, staticcheck, gocognit, gocyclo all OK)
- `go test -run TestSOLID_ ./...` → passes
- altertab3 FAIL count → 0

## What G05 Does NOT Do

- G05 does not implement new ALTER TABLE variants (e.g., ALTER TABLE without RENAME).
- G05 does not change how CREATE TABLE itself works — only how the stored text is edited.
