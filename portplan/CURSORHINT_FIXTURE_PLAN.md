# cursorhint fixture regeneration plan

## Evidence
- Latest full suite `/tmp/frigolite-current-full-suite5.log` first reports stale cursorhint cases 5.0/7.1, with table collisions. Source `../sqlite/test/cursorhint.test` lacks these names.
- Regenerated `testdata/cursorhint.json` from current source. Old collisions gone. Remaining cursorhint-6.0 pure query result mismatch (`[]` vs `[abc 2 uvw 2]`) needs engine reproduction; cursorhint2-3.0 table-exists likely stale source-order fixture.

## Steps
1. Regenerate cursorhint2 source into temporary JSON and run both isolated families.
2. Create native test for cursorhint-6.0 exact SQL and compare SQLite oracle; plan any engine change before editing.
3. Verify focused files, attach2, diff check; full suite again to find new earliest remaining failure.