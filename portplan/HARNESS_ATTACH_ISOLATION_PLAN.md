# Harness ATTACH Fixture Isolation Plan

## Scope
Route fixture SQL ATTACH database filenames into the current JSON fixture's temporary directory; preserve repository-root `test.db2` and all pre-existing edits. Keep fixture `delete_file` targeting in the same directory.

## Contract and diagnosis
The harness currently cleans `*.db` in the process working directory and only assigns `lifecycleDBPath` for fixtures with reopen markers. SQL fixture steps execute against shared relative ATTACH names, while delete_file routing is conditional on lifecycleDBPath. Consequently altercons can reuse/create root `test.db2`, and `ATTACH 'test.db2'` can observe stale table `t1`. Route SQL at harness boundary, not engine storage semantics.

## Exact changes
1. Add a helper that rewrites quoted relative filename operands of SQL `ATTACH [DATABASE]` statements to a fixture-owned temp directory, preserving absolute paths, URI names, and SQL otherwise.
2. Allocate one temp directory per JSON subtest independent of lifecycle reopen; choose lifecycle test.db inside same directory when needed.
3. Route `delete_file` basenames into that fixture directory consistently, preserving absolute/external targets.
4. Apply routing before fixture SQL dispatch (exec, catch, query, sqlite3_exec, query_assign); ensure expectations/errors still use original SQL as appropriate.
5. Add focused harness regression tests proving preexisting root `test.db2` bytes unchanged and relative attach path mapped into fixture directory; add altercons 9.0 clean-attachment coverage if not already present.

## Verification sequence
Run focused routing regression; exact altercons family (`FRIGOLITE_TEST=altercons go test . -run '^TestSQLiteSuite$' -count=1`); `git diff --check`; full `go test ./...` with complete stdout/stderr and elapsed time retained under portplan evidence. Continue earliest failure only after capturing full evidence.
