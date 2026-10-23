# bestindex1 Tcl virtual-table callback plan

## Diagnosis (reproduced)
Command `FRIGOLITE_TEST=bestindex1 go test . -run '^TestSQLiteSuite$' -count=1` exits 1. Initial failures: `CREATE VIRTUAL TABLE ... USING tcl(vtab_command)` → `declare_vtab: syntax error`; tests 1.1/1.2 subsequently have no x1. Current `internal/vtab/tclcmd.go` only reads `TclVarGet` and returns an empty cursor. Converter currently emits empty `register_tcl_module` markers and drops the fixture's Tcl proc definitions. Engine already implements generic planner `IndexInfo` and `PlanFilterer` but the tcl module does not use those interfaces.

## Detailed plan before edits
1. Converter representation: add proc snapshot fields to `tools/tclconvert/tcl.Stmt` and `tools/tclconvert/main.go` JSON `TestStep`; `commandRegisterTCLModule` serializes callback procs known at that source point. Preserve module registration location and arguments/connection name. Add converter unit test proving proc body and module args survive JSON conversion.
2. Runtime ownership: harness creates one fixture-scoped Tcl callback runtime from marker data and passes it explicitly into a new `TclCommandModule` adapter. Avoid package-global callback state and keep interpreter state isolated per fixture/connection. Implement only Tcl syntax needed by `bestindex1.test`, using existing interpreter evaluator where reusable; no package import cycle.


### Source contract and adapter boundary
`../sqlite/src/test_bestindex.c` confirms argument order and protocol: xConnect gets no further args; xBestIndex gets Tcl-list `constraints orderby colUsed`, with per-constraint fields `op`, `column`, `usable`; plan return list keys `orderby`, `cost`, `rows`, repeated `use`/`omit` indices, `idxnum`, `idxstr`; xFilter gets `idxNum idxStr argv-list`, response `sql <statement>` and query result supplies rowid then declared columns. Production `internal/vtab` must not import converter package `tools/tclconvert/tcl`; provide a narrow `TclCallbackRuntime` interface from harness, with proc invocation, SQL query function, and Tcl-list parsing/formatting kept on harness side. Runtime and SQL execution must be fixture/DB scoped, no package globals.
4. Native tests first for adapter semantics and table rows/plan values; then converter/harness regression. Add per-fixture isolation and Tcl-list quoting tests.
5. Read SQLite `test/bestindex1.test`/tcl module source; run callback tests, exact bestindex1 command, `git diff --check`, then full `go test ./...`; capture earliest failure and fix by root cause.

## Files/functions
- `tools/tclconvert/tcl/interp.go`: proc snapshot access
- `tools/tclconvert/tcl/interp_cmds.go:commandRegisterTCLModule`
- `tools/tclconvert/main.go`: JSON marker serialization
- `frigolite_harness_test.go`: marker decoding and per-fixture runtime wiring
- `internal/vtab/tclcmd.go`: callback adapter, xConnect/xBestIndex/xFilter
- `internal/vtab/indexinfo.go`: existing planner contract, extend only if source requires
- native `frigolite_*_test.go` and converter tests

## Constraints
## Validation evidence
- Exact target passed: `FRIGOLITE_TEST=bestindex1 go test . -run '^TestSQLiteSuite$' -count=1`.
- Native multi-scan/join regression passed: `go test . -run TestTclCallbackVirtualTableJoinRows -count=1`.
- Focused package checks passed: `go test ./internal/execquery ./internal/vtab ./tools/tclconvert/...`; `git diff --check` passed.
- Full `go test ./...` completed and exited 1 (captured `/tmp/frigolite-go-test-final.log`, exit `/tmp/frigolite-go-test-final.exit`). Earliest reproducible failure from that run: `TestSQLiteSuite/alterlegacy/alterlegacy-6.0` executes `CREATE VIRTUAL TABLE x1 USING tcl(tcl_command);` and errors `invalid command name "tcl_command"`; exact narrowed command `FRIGOLITE_TEST=alterlegacy go test . -run '^TestSQLiteSuite$' -count=1` reproduces. `alterlegacy-6.1` then cascades from absent x1. This needs additional source-faithful legacy callback/setup support and is outside bestindex1 callback contract; also the full run records independent bestindex3/5/6/7/A/E family and other repository failures, so full suite remains red.
- Expanded regenerated callback-family fixtures independently expose known limits (OR row ordering, plan SQL construction, IN handling, legacy callback fixtures). No tests were skipped or expectations weakened.
- Follow-up repair regenerated only stale Tcl registration metadata for `alterlegacy` and `altertab`; both exact family checks pass. Full `go test ./...` was rerun after these changes and exited 1 (captured `/tmp/frigolite-go-test-resumed.log`, exit `/tmp/frigolite-go-test-resumed.exit`). Earliest remaining full-suite failure: `TestSQLiteSuite/bestindex3/bestindex3-1.6.0.3`; reproduced by `FRIGOLITE_TEST=bestindex go test . -run '^TestSQLiteSuite$' -count=1`. For `SELECT rowid FROM t1 WHERE c='three' OR c LIKE 'o%'`, engine returns `[1 3 4 6]`, source expected `[1 6 3 4]`. Root category is generic multi-index OR branch result ordering, not Tcl callback invocation; further independent failures include bestindex5,6,7, A, E and are shown in that log.

Preserve existing workspace modifications. No fixture skipping or weakened assertions. Tcl callback runtime must be scoped and concurrency-safe. Determine hard feasibility boundaries from source and tests before implementing broad Tcl semantics.
