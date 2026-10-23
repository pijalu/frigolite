# vtab6 source-fixture reconciliation

## Evidence
- `FRIGOLITE_TEST=vtab6 go test . -run '^TestSQLiteSuite$' -count=1` fails 44 leaves; many say `no such table: t1/t2/t6`.
- `../sqlite/test/vtab6.test:27-48` registers echo, creates real_t* tables, and dynamically creates echo virtual tables before test cases.
- Current `testdata/vtab6.json` begins directly with vtab6-1.1 and lacks setup/registration. Therefore these failures are fixture divergence, not JOIN semantics.

## Work completed and verification
1. Regenerated only vtab6 from pinned source; ordered setup now captures echo registration, 14 backing tables and virtual tables.
2. Added captured `echo_ignore_usable` markers for source `set`/`unset` and harness dispatch to echo module test variable, preserving TCL toggle order.
3. Fixed star result-column labels to expose original unqualified column names (internal table prefixes remain only for row resolution); added native regression `TestSQLiteEchoVtabUsingMergedColumnNames` which was RED before fix.
4. Verification: converter marker unit test passes; `FRIGOLITE_TEST=vtab6 go test . -run '^TestSQLiteSuite$' -count=1` passes; native merged-column test passes; `git diff --check` passes.
