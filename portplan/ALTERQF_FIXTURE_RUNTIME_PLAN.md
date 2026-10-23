# alterqf Fixture Runtime Repair

## Scope
Repair only `testdata/alterqf.json` setup semantics omitted by conversion; no engine shortcuts or test exclusions.

## Evidence / contract
Full baseline after altermalloc2 repair retained at `/tmp/frigolite-suite-evidence/current2.{stdout,stderr,status}` (exit 1, real 340.32s); next failing family alterqf. Exact family command reproduces. SQLite `../sqlite/test/alterqf.test` enables internal functions at lines 27–28, creates t1, then foreach iterates eleven `{before after}` pairs and invokes `sqlite_rename_quotefix('main',$before)`; converter fixture omitted foreach's before values and test-control marker. Current harness runtime expands `$before` to empty; hidden internal function returns NULL.

## Changes
1. Add ordered fixture marker enabling internal functions immediately after t1 setup.
2. Preserve each loop case's source `before` SQL as per-test query_assign/runtime setup, rather than deleting SQL or weakening expectations.
3. Add regression pins for generated values and fixture order if conversion/native mechanisms allow.
4. Run exact alterqf; repair only further direct failure from this source sequence.

## Verification
`FRIGOLITE_TEST=alterqf go test . -run '^TestSQLiteSuite$' -count=1 -v`, JSON validation, diff check.
