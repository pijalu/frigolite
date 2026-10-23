# avtrans File-Backed Reopen Lifecycle Plan

## Evidence
- Source `../sqlite/test/avtrans.test:820-843` closes DB and reopens `test.db`, then creates/populates t3; 9.x loops and `signature` procedure require this persistent database state.
- Fixture regenerated with faithful `execsql2` support now contains `close_db`, `reopen_db(test.db)`, and live Tcl procedure-driven setup markers. Harness `TestSQLiteSuite` currently allocates `lifecycleDBPath` only for `vtab1`; reopen_db branch requires it and fails `avtrans-9.1` with `unexpected reopen marker path test.db`.
- The subsequent missing t3 and transaction failures cascade from failed reopen/setup; not DDL engine failures.

## Plan before edits
1. Update harness initialization: detect file-backed lifecycle markers from loaded `TestFileData`, allocate a scoped temp `test.db` path for any such fixture, open that file initially; retain `vtab1` unregistered-echo behavior.
2. Ensure reset_db removes/recreates this same scoped path and reopen_db preserves file contents; close/reopen errors should remain explicit.
3. Add a harness lifecycle regression test or isolate avtrans 9.1 and later cases.
4. Verify avtrans earlier section and 9.1 setup; run avtrans isolated. Validate unrelated vtab1 behavior and `git diff --check`.

## Implementation evidence
- `TestSQLiteSuite` now allocates a scoped path when any fixture has `reopen_db`, opens it before setup, and reopens/resets the same path at marker positions. Non-vtab1 fixtures restore default echo registration; vtab1 still starts unregistered.
- `FRIGOLITE_TEST=avtrans go test . -run '^TestSQLiteSuite$' -count=1` passes all 267 ordered test groups, including generated 9.x loops and setup markers, 25.146s.
