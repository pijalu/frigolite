# altermalloc2 Fixture Reset Repair

## Scope
Fix converted `testdata/altermalloc2.json` lifecycle boundaries only; preserve other edits and SQLite semantics.

## Evidence / source contract
Full baseline `go test ./...` output is retained at `/tmp/frigolite-suite-evidence/current.{stdout,stderr,status}`; exit 1, `real 365.05`. First failing family is altermalloc2. Reproduction: `FRIGOLITE_TEST=altermalloc2 go test . -run '^TestSQLiteSuite$' -count=1 -v`; failures 2.0, 3.0 and 5.0 are duplicate tables. SQLite `../sqlite/test/altermalloc2.test` calls `reset_db` before sections 3, 4, and 5 (lines 64, 81, 106); between 1.0 and 2.0 source closes `db`, forcedeletes `test.db`, and reopens it (lines 44-46); encode ordered close/delete/reopen markers. `reset_db` precedes 3.0, 4.0, and 5.0 (lines 64, 81, 106).

## Exact changes
1. Inspect converter lifecycle marker support and neighboring fixtures.
2. Represent source forcedelete/reopen boundary before 2.0 using ordered fixture steps, and insert reset markers before 3.0, 4.0, 5.0 exactly as source indicates.
3. Add native harness regression covering ordered reset and isolated file lifecycle if existing harness assertions cannot pin this.
4. Run exact altermalloc2 family, full suite; retain updated full output and identify next failure.

## Verification
`FRIGOLITE_TEST=altermalloc2 go test . -run '^TestSQLiteSuite$' -count=1 -v`, then `go test ./...`.
