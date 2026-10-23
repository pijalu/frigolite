# ALTER COLUMN Trigger UPDATE OF Rename Plan

## Evidence and diagnosis

- Full-suite command: `go test ./... -count=1`; exit status and first-family finding retained in `portplan/FULL_SUITE_REFRESH.status` and this plan. First family: altercol, `altercol-7.1.3` expected trigger increment 2, got 1.
- Native test `TestAlterColumnRenameUpdatesTriggerUpdateOf` added and run RED: same update after `ALTER TABLE ... RENAME COLUMN` leaves count 1.
- SQLite `alter.c` `renameQuotefixFunc` reparses trigger and resolves expressions before token walking. A quoted token is a column when resolution finds it in table schema, even if identifier spelling contains spaces (`"col c"`). Current Go `quoteFixToken` requires `isSQLIdentifier(content)` before checking `colSet`, incorrectly converts resolved spaced column names to string literals; then trigger's UPDATE OF list cannot be renamed.

## Implemented fixes and validation

1. `quoteFixToken` now preserves names resolved in the table column set even when they contain spaces; otherwise the trigger's `UPDATE OF` token is converted to a string literal before rename.
2. CREATE TABLE DQS quote-fix preserves double-quoted tokens in column type spans and applies expression fix only after constraints begin. This retains SQLite's `CREATE TABLE t1('a'"b",c)` rename result `CREATE TABLE t1("x" "b",c)`.
3. Native regressions `TestAlterColumnRenameUpdatesTriggerUpdateOf` and `TestAlterColumnRenameMixedSingleDoubleQuotedName` each passed after first failing RED. Exact `FRIGOLITE_TEST=altercol go test . -run '^TestSQLiteSuite$' -count=1` passed after both fixes; DQS native alterqf test passed.
4. Full-suite rerun in progress; preserve exit status and concise failure summary in `portplan/FULL_SUITE_REFRESH.status` and this plan; keep complete stdout outside the repository.

No fixture edits in this family; preserve workspace and root `test.db2`.
