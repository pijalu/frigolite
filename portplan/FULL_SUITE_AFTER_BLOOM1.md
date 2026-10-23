# Full-suite continuation after bloom1 repair

`go test . -run '^TestSQLiteSuite$' -skip '^TestBtreeRebalanceFTSWipeChurn$' -count=1` rerun ended after 151.947s with exit 1. Previously documented `bloom1-4.3` failure is fixed; exact bloom1 suite passes. Requested `git diff --check && go test ./...` also rerun; diff check passed, but Go suite exceeded the 300s tool timeout.

Current earliest broad-harness failure: `btree01/btree01-2.1`. Isolated reproduction:

```sh
FRIGOLITE_TEST=btree01 go test . -run '^TestSQLiteSuite$' -count=1
```

fails expected row `198 99` from `t2 LEFT JOIN t1 ON y=a`, got `198 NULL`. Pure-Go query confirms row exists and has `(a,c,b length)=(198,99,100)` before `UPDATE t1 SET b=zeroblob(1000) WHERE a=198`; afterward row still reports length 100. Thus broad fixture's result mismatch is reproducible; further scratch checks show direct primary-key lookup and full table scan both return row 198, while the LEFT JOIN equi-join path does not match it. A separate minimal WITHOUT ROWID UPDATE confirms the updated BLOB reaches 100 bytes, so earlier attribution to an ineffective update was disproved. Likely mismatch lies in RowMap/hash join key normalization or row materialization under this overflow case. Current exact selector remains `FRIGOLITE_TEST=btree01 go test . -run '^TestSQLiteSuite$' -count=1`; inspect comparator source before changing.
