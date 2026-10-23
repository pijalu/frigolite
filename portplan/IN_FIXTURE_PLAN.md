# in.test loop fixture audit

## Evidence
- Isolated `TestSQLiteSuite/in` has missing-row failures in `in-1.*`.
- `../sqlite/test/in.test:21-33` creates t1 and populates ten rows with Tcl `for` loop before later queries.
- Existing JSON `in-1.0` contains only BEGIN/CREATE; loop insert steps and COMMIT are absent.

## Work completed
1. Regenerated only `in` from source; in-1.0 now has ten ordered INSERT steps plus COMMIT/SELECT.
2. `FRIGOLITE_TEST=in go test . -run '^TestSQLiteSuite$/^in$' -count=1` passes. (An attempted `-run '^TestSQLiteSuite/in($|/)'` did not filter sibling files because harness filters on env string; exact Go subtest regex is required.)
