# autoindex1 Next-Failure Plan

## Evidence
Exact harness initially failed 299, 310, 800, 801, 901, 1211. Added native regressions for statistics-driven real-table autoindexing, LEFT JOIN autoindexing, and a view joined through a non-leading composite WITHOUT ROWID primary-key column. SQLite `where.c` (`constructAutomaticIndex`, around lines 1047–1237) builds covering automatic indexes from eligible inner-loop terms. Planner now resolves view output columns, restricts WITHOUT ROWID PK lookup eligibility to leading PK column, and models autoindex loop candidates on real tables/views. `autoindex1-300` generated JSON had omitted source Tcl's per-row callback UPDATE effects; restored eight updates so `autoindex1-310` state is source-faithful. Exact autoindex1 passes; native autoindex and P5 tests pass; `git diff --check` passes.

Latest `go test ./...` completed exit 1 after 155.142s. `autoindex1` no longer fails. Earliest remaining harness family: `autoindex4`, case `autoindex4-1.0`, reproducibly isolated by `FRIGOLITE_TEST=autoindex4 go test . -run '^TestSQLiteSuite$' -count=1`. Divergence: duplicate `x=987` rows in `ORDER BY +b` ties come in insertion order (`zyx,rqp`) instead of SQLite's automatic partial-covering-index traversal (`rqp,zyx`). SQLite CLI confirms with automatic_index OFF order is insertion order and ON order is reverse for duplicate key.

## Plan
1. Trace autoindex1 source/failures, establish RED native regressions. DONE.
2. Implement real inner-loop autoindex EQP, view and PK resolution; add focused regressions. DONE.
3. Restore omitted autoindex1-300 callback mutations in generated fixture. DONE.
4. Diagnose autoindex4: plain comma joins now prefilter RHS by independent equality predicates and traverse candidate RHS rows in covering-index key order; added native regression. Exact autoindex4 family passes.
5. Run exact autoindex1, autoindex4, native planner/P5 tests, `git diff --check`, then post-fix `go test ./...`; capture status and earliest remaining independent family. DONE. `go test ./...` exit 1 (155.142s), retained in `/tmp/autoindex1-finalall/full.{stdout,stderr,status}`. `autoindex1` and `autoindex4` absent from suite failures. Earliest remaining family is `bestindex1`; exact rerun fails `bestindex1-1.0` at `CREATE VIRTUAL TABLE x1 USING tcl(vtab_command)` with `declare_vtab: syntax error`. Source requires invoking Tcl `xConnect` / `xBestIndex` procedures; current `TclCommandModule` obtains schema only from shared Tcl variable and exposes an empty cursor (`internal/vtab/tclcmd.go:9-13,45-76`), so this family is blocked on a separate Tcl callback runtime, unrelated to autoindex planner work. Current full-suite run also shows existing independent failures in `JoinHRightUsingSubqueryDuplicateNameUsesFirstColumn` and `P6_NestedJoinSameName`; first harness family is `bestindex1`.

## Constraints
Preserve all unrelated workspace changes. No test skips or weakened assertions.
