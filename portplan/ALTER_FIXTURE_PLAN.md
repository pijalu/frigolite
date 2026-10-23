# alter.test source fixture reconciliation

## Evidence
- Exact isolated `FRIGOLITE_TEST=alter go test . -run '^TestSQLiteSuite$/^alter$' -count=1` has 27 failures.
- Source `alter.test:478-523` creates temp triggers/table in prior tests and uses a dynamic `::temp` substitution; current JSON has missing `tbl1` before alter-3.3.2. `alter-6.2` is also malformed (`CREATE TABLE (a,b,c)`) in current JSON; inspect original source expansion.
- Other errors involve test-only `SQLITE_RENAME_*` callbacks, not necessarily engine defects.

## Work completed / residuals
1. Regenerated only alter from source; emitted SQL now resolves `${tbl_name}` after global `set ::tbl_name` (added `getVar` fallback for bare lookup of global namespace).
2. Added converter regression `TestExecSQLSubstitutesGlobalVariableByBareName`; RED before fix, green after.
3. Isolated alter failures fell from 27 to 20. Remaining clusters include missing Tcl-derived `::oid` in dynamic SQL (alter-6.2 onward), stale table setup at alter-5.2/8.*, corrupt/incomplete rename placeholder SQL at 2.2/2.3, and SQLite test-only `SQLITE_RENAME_*` callback gaps.
4. Focused converter regression passes; exact alter still fails and needs separate targeted converter/runtime fixes.
