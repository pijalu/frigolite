# altercol dynamic expectation conversion repair

## Source trace

`../sqlite/test/altercol.test` defines `do_rename_column_test` (around lines 364–389). For each index it calls `drop_all_tables_and_views db`, builds `lSorted` by executing each `$lSchema` SQL entry and appending `[string trim $sql]`, sorts that Tcl list with `lsort`, then passes `$lSorted` unbraced to `do_execsql_test`. The final expectation is produced with `[string map [list $old $new] $lSorted]`.

The Tcl interpreter correctly expands the unbraced expectation to a Tcl list. Before repair, `commandSet` tagged the most recent `execsql` as `query_assign` whenever `isQueryStmt` found `SELECT` anywhere in captured SQL. A `CREATE TRIGGER ... BEGIN SELECT ... END` therefore became a result assignment and overwrote `lSorted`. In addition, `curTest` leaked after helper test commands, causing later loop setup SQL to be emitted as duplicate prior tests instead of setup groups.

## Repair

- Classify query assignments by the first SQL statement's verb, not the final statement or embedded trigger body; keep true `SELECT` result assignment behavior.
- Clear current test context after `do_execsql_test` command capture so following Tcl setup/reset execution is grouped in source order.
- Regenerate altercol fixture from source Tcl execution; preserve each iteration's setup, checks, rename, close/reopen, and mapped expectations.
- Add Tcl interpreter regression ensuring CREATE TRIGGER stays `exec` while following SELECT assignment remains `query_assign`.

## Verification

- `go test ./tools/tclconvert/tcl -run 'TestExecSQLAssignmentDoesNotClassifyTriggerBodyAsQuery|TestExecSQLResultAssignmentIsCapturedAsRuntimeStep|TestDoExecSQLResolvesUnbracedLocalExpectation' -count=1` passes.
- `go run ./tools/tclconvert/ -testdir ../sqlite/test -outdir /tmp/altercol-convert altercol.test` emits source-derived grouped test cases and expectations.
- `FRIGOLITE_TEST=altercol go test . -run '^TestSQLiteSuite$' -count=1` passes.
- `FRIGOLITE_TEST=attach2 go test . -run '^TestSQLiteSuite$' -count=1` passes.
- `go test ./tools/tclconvert/... -count=1` passes, including source-driven converter regression for Tcl list and mapped expectations.
- `git diff --check` passes.
- `FRIGOLITE_TEST=atof1 go test . -run '^TestSQLiteSuite$' -count=1` passes; older broad-run artifact's earliest `atof1` failure is no longer reproducible.
- Latest `go test ./...` attempt exceeded 300-second tool timeout without a final status. Not verified; broad suite should be rerun with a longer session/background allowance.
