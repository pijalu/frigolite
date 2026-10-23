# analyzeE STAT4 Range Estimate Plan

## Evidence

Exact `FRIGOLITE_TEST=analyzeE go test . -run '^TestSQLiteSuite$' -count=1` fails first at source test analyzeE-3.1. Canonical `../sqlite/test/analyzeE.test:136-148` creates 1001 rows with constant leading index column c=123 and varying a=1000..2000, creates `(c,a)`, runs ANALYZE, then expects a broad a-range (500..2500) to scan. Current planner picks the index and estimates combined equality/range too selectively; similarly broad `a>1100` and `a<1900` expected scans. This is STAT4 range selectivity behavior, not fixture loss. Prior full-suite output also has analyzeE 6.x table-exists issue; exact source reset boundaries need separate handling after first engine contract.

## Steps

1. Inspect `../sqlite/src/analyze.c` STAT4 sample construction and `where.c` range estimate use; inspect existing STAT4 estimator implementation.
2. Add native regression for same `(c,a)` dataset, stat4 analysis and broad/selective index plan decisions; verify RED.
3. Implement smallest complete source-faithful range estimate using existing STAT4 samples; avoid heuristic/test-specific thresholds.


## Additional source-faithful fixture repair

`analyzeE-5.0`, `6.0`, and `7.0` are each preceded by canonical `reset_db` calls (`../sqlite/test/analyzeE.test:244,256,282`). Current JSON lacks these markers, explaining t1-already-exists errors and stale state across sections. After implementing/focusedly testing STAT4 planning, regenerate only `testdata/analyzeE.json`, inspect expected rows unchanged and resets inserted, then rerun exact family.