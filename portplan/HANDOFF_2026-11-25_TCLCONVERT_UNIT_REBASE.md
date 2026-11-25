# Session handoff — tclconvert unit re-baseline (2026-11-25, landed)

Converter and fixture code unchanged. Only nine stale unit expectations were
re-baselined against the current `runtime_expr` capture stream.

## Landed

Rebased tests (all green):

- `tools/tclconvert/main_test.go`
  - `TestConvertPreservesCustomCollationLifecycle`: constructor counter setup
    now appears as `setup_0`'s `runtime_expr caseless_del=0`; both `runtime_var`
    probes (`0` after registration, `1` after deletion) are pinned too.
- `tools/tclconvert/tcl/interp_sql_test.go`
  - `TestExecSQLResultAssignmentIsCapturedAsRuntimeStep`: query assignment is
    followed by a `runtime_expr` alias for `oid` before the dynamic SQL.
  - `TestChanges2CAPIEventsAreCapturedInSourceOrder`: prepared statement handle
    publishes `stmt=stmt_1` between prepare and step.
  - `TestNestedArrayVariableSubstitution`: file-level `C(column)` and `cols(1)`
    seeds precede the substituted insert.
  - `TestExecSQLSubstitutesGlobalVariableByBareName` and
    `TestExecSQLSubstitutesGlobalVariableInQuotedSQL`: runtime `table_name=T`
    precedes the generated CREATE.
  - `TestEchoIgnoreUsableSetIsCaptured`: echo marker plus runtime `1`.
- `tools/tclconvert/tcl/runtime_value_test.go`
  - `TestDBStatusBareStyleAndSearchCount`: `sqlite_search_count=0`
    `runtime_expr` sits between the two `where8` query assertions.

No assertion was deleted: the original collation markers, dynamic SQL, array
substitution, status style/search flags, and event ordering are all still
checked. The new runtime steps are asserted explicitly rather than filtered.

## Verification

- `go test ./tools/tclconvert/... -count=1` — green (all three packages).
- `go test -run TestSOLID_ ./...` — green.
- `git diff -- testdata/` empty: no fixture regeneration, so the harness
  leaf-name set is unchanged from the t30 baseline
  (`/tmp/t30_full_gate.log`). No new leaves were introduced.
- Converter/interpreter source (`interp*.go`) unchanged.

## Scope and risk

The extra steps seed harness runtime variables: callback counters, prepared
statement handles, query-assigned values, file-scope arrays, and counter
resets. Treating them as noise would silently drop runtime state. Do not
"fix" these tests by filtering `runtime_expr`; if the runtime stream changes,
re-derive each step's harness purpose before updating the expectations.

No engine behavior changed; no SQLite oracle comparison was needed. Restart
point: none — the converter unit baseline is clean.
