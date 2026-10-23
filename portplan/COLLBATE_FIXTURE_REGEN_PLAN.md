# collateB fixture fidelity plan

## Evidence
- Clean `go test ./... -count=1` in `/tmp/frigolite-current-full-suite.log` failed `TestSQLiteSuite/collateB` (wrong row values in 1.5/1.15); isolated family now errors `no such table: t2`.
- Authoritative `../sqlite/test/collateB.test:18-29` has `do_execsql_test collateB-1.1` creating three tables and expecting empty SELECT, then `collateB-1.2` inserts rows and checks SELECT.
- Current `testdata/collateB.json` encodes the DDL setup SQL as a `query` step instead of an exec step. It does not faithfully capture source command semantics; broader run's apparent values also contradicted SQLite oracle.

## Ordered repair
1. Generate only collateB JSON into `/tmp`, compare generated lifecycle, step types, SQL and expected results to authoritative source.
2. Build pure-Go regression for exact setup and both query predicates, compare to Python SQLite oracle. Inspect post-join positional map construction.
3. Clone current joined positional values before appending right values; preserve input maps and all other workspace changes.
4. Run isolated native regression, collateB, attach2, `git diff --check`; then full `go test ./... -count=1`.
