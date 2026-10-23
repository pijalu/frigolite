# bloom1: next reproducible broad-suite failure

The broad suite after altercol, bestindexF/G, and between repairs ended after 152.279s. First failed family in that run was bloom1. `testdata/bloom1.json` was stale: it lacked reset markers between independently initialized subtests. Regenerated from `ori/sqlite/test/bloom1.test`; most earlier duplicate-table failures disappear.

Current focused command:

```sh
FRIGOLITE_TEST=bloom1 go test . -run '^TestSQLiteSuite$' -count=1
```

Source-faithful diagnosis: `bloom1-4.3` failed with `btree: cursor at end` at `ANALYZE`. A statement-by-statement pure-Go reproduction localized failure to STAT4 scan of empty partial index `t0x ON t0(a IS FALSE) WHERE false`. SQLite `src/analyze.c:1082–1090` branches on empty `Rewind` and initializes zero-count stats. Added `TestAnalyzeEmptyPartialIndex`; it failed before repair (`ANALYZE: btree: cursor at end`). `scanStat4Index` now treats cursor-at-end before any decoded records as clean empty scan. Regression and exact `FRIGOLITE_TEST=bloom1 go test . -run '^TestSQLiteSuite$' -count=1` pass.
