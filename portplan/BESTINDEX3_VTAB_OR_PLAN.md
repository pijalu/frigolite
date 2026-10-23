# Created virtual-table OR branch execution plan

## Reproduction
`FRIGOLITE_TEST=bestindex3 go test . -run '^TestSQLiteSuite$' -count=1` fails tests `1.6.0.3` and `1.6.1.3`: `SELECT rowid FROM t1 WHERE c='three' OR c LIKE 'o%'` returns `1 3 4 6`, expected `1 6 3 4` per `../sqlite/test/bestindex3.test`. SQLite's callback supports `eq`/`like`, and xFilter emits filtered SQL results branch-by-branch with rowid dedup.

## Detailed plan before edits
1. Trace `IndexInfo`/`VtabScanOptions` and materializer to confirm branch-specific WHERE reaches xBestIndex and consumed/omitted terms remain correct. OR evaluator must split only OR nodes in top-level WHERE conjuncts and retain sibling AND terms in each branch.
2. Add pure-Go engine regression using a Tcl callback and fixture values. Assert callback branch count/order, rowid dedup, output order, and residual filtering. Run RED before implementation.
3. Add a created-vtab-only OR materialization path: build a branch-local shallow SelectStmt/WHERE, materialize through existing `MaterializeCreatedVTab` independently per branch so each invokes xBestIndex/xFilter with branch constraints, union candidates by rowid in branch-emission order, then pass combined materialized rows through full original SELECT pipeline. Prevent branch-local LIMIT/OFFSET pushdown; preserve original ORDER BY/final projection.
4. Verify source-faithful omit: each branch materializer removes only branch constraints marked Omit; full SELECT predicate remains final safety filter. If callback plans don't meet this condition, reject optimization and use normal scan rather than change semantics.
5. Run native RED/GREEN, exact bestindex3 and bestindex1, attach2, focused packages. Then full `go test ./...`; repair next earliest failure if feasible or capture/reproduce exact blocker.

## Root cause evidence
`internal/execquery/select_exec.go:479–480` returns `RootPage==0` entries via `execSelectVtab` before ordinary OR optimization at lines 482–489. Created-vtab materialization in `internal/exec/select.go` passes full WHERE to `MaterializeCreatedVTab`; `internal/exec/vtab_materialize.go` invokes `PlanBestIndexer` once for that whole predicate. Existing `BuildVtabIndexInfo` enumerates top-level AND terms, so Tcl callback sees no separate OR loop plans. Ordinary btree OR executor in `internal/execdml/or.go` is index-specific and must not be reused directly.

## Files likely involved
- `internal/execquery/select.go`: created-vtab OR branch discovery/materialized row union.
- `internal/execquery/select_exec.go`: preserve no-join and RootPage==0 eligibility.
- `frigolite_tcl_vtab_callback_test.go`: native fixture runtime test.
- `frigolite_tcl_vtab_callback_test.go` or a focused new engine test: direct callback semantics.

## Risks
- `VtabScanOptions.Where` drives xBestIndex and residual calculation; don't substitute branch predicates after callback planning.
- Dedup must use vtab rowid, not projected values; real OR optimization deduplicates by rowid.
- Never alter ordinary btree OR or FTS special paths.
