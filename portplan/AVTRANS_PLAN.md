# avtrans execsql2 Source-Faithfulness Plan

## Reproduction / corrected cause
- Isolated avtrans fails 6.7/6.16 (COMMIT executed after omitted setup cases) and 6.20 (table exists from stale state).
- Previous conclusion of DDL transaction defect is premature and incorrect: source `avtrans.test:435-472` includes 6.1–6.6 using `execsql2`, but current and fresh generated JSON both omit those cases. `tools/tclconvert/tcl/interp_cmds.go` registers `execsql` but not `execsql2`; unknown commands silently no-op, so `do_test` bodies containing execsql2 produce no captured statement. Source-faithful conversion is root fix, not transaction behavior.
- Test source expectations show execsql2 returns alternating column-name/value pairs (`{a 1 b 2 c 3}`), unlike normal row-only execsql.

## Plan before edits
1. Add converter regression asserting `do_test {... execsql2 {SELECT ...}}` emits a dedicated query2 statement with expected value.
2. Implement execsql2 capture; support query2 in harness by flattening each row into column-name/value pairs matching SQLite Tcl helper, preserving existing query behavior.
3. Regenerate only `testdata/avtrans.json` from `../sqlite/test/avtrans.test`; compare test counts, ordering, and restored 6.1–6.6 setup.

## Implemented and verified
- Test-first `TestExecSQL2IsCapturedWithNamedRows` failed before registration (`[]tcl.Stmt(nil)`), then passed after adding `execsql2` capture as `query2` and harness rendering of alternating column/value pairs.
- Regenerated only avtrans fixture. Test count 255→267; ordered source now includes `avtrans-6.1` through `6.6`, with `6.6` represented as `query2` and expected `a 4 b 5 c 6`.
- Isolated full avtrans run no longer reports 6.7/6.16/6.20; remaining failures are 9.x reset/reopen/live database-state cases (e.g. reopen marker path and missing t3), separate converter/harness contracts. `git diff --check`, converter regression, native aggregate pin, and analyze5 isolated suite pass.
4. Run converter tests and isolated avtrans; triage any remaining source-vs-engine differences without modifying expected values.
