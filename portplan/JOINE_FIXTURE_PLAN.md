# joinE source-fixture reconciliation

## Evidence
- `FRIGOLITE_TEST=joinE go test . -run '^TestSQLiteSuite$' -count=1` fails 37 cases, all cascading `no such table: t1`.
- `../sqlite/test/joinE.test:31-37` top-level `db eval` creates and populates t1/t2 before tests.
- Existing JSON starts directly with joinE-1, lacking setup.

## Work completed
1. Regenerated only joinE from pinned TCL source.
2. Ordered fixture now carries top-level CREATE/INSERT setup and nullvalue token.
3. `FRIGOLITE_TEST=joinE go test . -run '^TestSQLiteSuite$' -count=1` passes; `git diff --check` passes. All 37 initial errors were setup cascades.
