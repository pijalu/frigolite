# Remaining independent alter.test fixture failures

## Verified evidence after runtime result-assignment repair
- Converter RED: `go test ./tools/tclconvert/tcl -run '^TestExecSQLResultAssignmentIsCapturedAsRuntimeStep$' -count=1` failed with 3 plain exec steps and `WHERE oid = `; passes after typed `query_assign` marker and deferred `$::oid` interpolation.
- Regenerated only alter fixture via `go run ./tools/tclconvert/ -testdir ../sqlite/test -outdir testdata alter.test`; generated cases 6.2–6.6 now carry query assignment and unresolved global reference.
- Converter suite passes: `go test ./tools/tclconvert/...`.
- Direct `sqlite3_exec` source `../sqlite/src/test1.c:421-459`: percent-decodes each `%HH` byte, calls sqlite3_exec with callback collecting column names/values, and returns `{rc result}`. Implemented converter `sqlite3_exec` typed step + harness callback-like result, plus `%HH` decoder. RED/green test uses `%c6%c6`.
- Re-converted only alter fixture. Exact family `/tmp/alter-sqlite3-exec3.log`: alter-11.1–11.10 no longer fail; 6.2–6.6 also remain clear. Remaining leaves now only setup_1 (unsupported `trigfunc` callback proc) and 9.1–9.3 (missing `SQLITE_TESTCTRL_INTERNAL_FUNCTIONS` controls/functions). Converter tests and `git diff --check` pass.

## Source-based plans
1. **TCL callback setup:** `alter.test:344-346` defines variadic trigfunc that assigns `::TRIGGER`; later trigger behavior may need this callback's runtime state. Inspect source invocation/assertion and current callback model before implementing; preserve returned Tcl argument list rather than register a no-op.
2. **Internal rename test controls:** `alter.test:687-700` enables `SQLITE_TESTCTRL_INTERNAL_FUNCTIONS` around 9.1/9.2, then disables it. Add ordered test-control state and internal-function behavior only after reading source `alter.c`/SQL function implementations; tests assert bad-input return/error semantics, not normal ALTER.
3. **`sqlite3_exec db` capture:** `alter.test:729+` calls C API helper directly to create `t11`, `t11b`, and later `t11c`; converter captures only `execsql` commands, so these schemas are absent. Add command handling that preserves selected connection, SQL execution, and Tcl `sqlite3_exec` result conventions; test conversion before fixture regeneration.

## Validation order
- Separate plans/regressions for each mechanism; do not mix into ALTER engine changes absent a native engine discrepancy.
- After each implementation regenerate only alter fixture and run the exact family; check full converter suite and `git diff --check`.
