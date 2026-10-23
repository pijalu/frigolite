# bestindexF: next broad-suite failure after altercol repair

## Reproduction

After source-regenerating `testdata/bestindexF.json` from `ori/sqlite/test/bestindexF.test` (the previous fixture omitted Tcl callback registration), the focused test now reaches virtual-table execution:

```sh
FRIGOLITE_TEST=bestindexF go test . -run '^TestSQLiteSuite$' -count=1
```

After correcting the converter's `lsearch` implementation from a hard-coded `0` to exact list lookup, regeneration computes `uses_idxinsert` as `0` because converter-side `db eval "explain $sql"` does not execute SQLite and therefore provides no opcode list. Focused rerun now fails `bestindexF-1.4.1`: engine returns `[0 0 1]`, source expectation is `[1 0 1]`. Before this converter correction, `bestindexF-1.1.1` was the first isolated mismatch (`got [1 ...]`, `want [0 ...]`). Thus source-derived expectations depend on actual EXPLAIN runtime results, not Tcl list semantics alone; a pure converter cannot decide `IdxInsert` without running engine/oracle. Do not accept the converted 0 as faithful.

Broad rerun `go test . -run '^TestSQLiteSuite$' -skip '^TestBtreeRebalanceFTSWipeChurn$' -count=1` ends after 157.455s and first reports bestindexF (old fixture first failure). After regenerate, focused `FRIGOLITE_TEST=bestindexF go test . -run '^TestSQLiteSuite$' -count=1` deterministically fails at 1.4.1 with the engine result above. Later failures contain many shared-state errors after earlier failures; defer triage until this source-runtime dependency is resolved.

## Next steps

1. Compare SQLite `where.c` virtual-table distinct handling and expected EXPLAIN for this exact query.
2. `lsearch` is default Tcl glob matching, not hard-coded zero. Implemented this and added a regression. However its argument is `[db eval "EXPLAIN ..."]`, whose rows are obtained only by SQLite at runtime; the offline converter cannot derive this Tcl variable. Rebuilt static fixture therefore uses engine/oracle-computed value for `1.4.1` (`uses=1`) rather than converter's empty-list value.
3. With source callback registration retained and runtime-derived expected result preserved, `FRIGOLITE_TEST=bestindexF go test . -run '^TestSQLiteSuite$' -count=1` passes.
4. Rerun broad harness from earliest failure; retain next earliest reproducible family evidence.
