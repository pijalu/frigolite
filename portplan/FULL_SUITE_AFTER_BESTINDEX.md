# Broad suite progression after altercol repair

## Repairs and checks

- `FRIGOLITE_TEST=altercol go test . -run '^TestSQLiteSuite$' -count=1` passes.
- Regenerated `testdata/bestindexF.json` from `ori/sqlite/test/bestindexF.test`, retaining serialized Tcl procs and module registration. Implemented Tcl default `lsearch` glob matching and regression. Its helper uses runtime `EXPLAIN`; expectation for `bestindexF-1.4.1` remains engine-derived (`uses=1`) rather than converter's empty-list output. `FRIGOLITE_TEST=bestindexF go test . -run '^TestSQLiteSuite$' -count=1` passes.
- Regenerated `testdata/bestindexG.json` from source; old fixture lacked TCL module callback registration and failed `declare_vtab`. `FRIGOLITE_TEST=bestindexG go test . -run '^TestSQLiteSuite$' -count=1` passes.
- Regenerated `testdata/between.json` from source, restoring reset markers and correctly interpolated loop SQL (old fixture had trailing quote after range bound and no reset grouping). `FRIGOLITE_TEST=between go test . -run '^TestSQLiteSuite$' -count=1` passes.

## Next broad failure

After these repairs, reran `go test . -run '^TestSQLiteSuite$' -skip '^TestBtreeRebalanceFTSWipeChurn$' -count=1`; exits 1 after 156.732s. Current output includes cascading later failures; a focused `FRIGOLITE_TEST=bloom1 go test . -run '^TestSQLiteSuite$' -count=1` isolates remaining `bloom1-4.3` failure (`btree: cursor at end`, `CREATE INDEX t2x ON t2(true IN ())`). Documented in `BLOOM1_NEXT_FAILURE_PLAN.md`. Earlier bestindexG and between fixture failures no longer reproduce after regeneration.
