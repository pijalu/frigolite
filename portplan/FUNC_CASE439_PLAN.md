# func-4.39 Tcl wrapper fidelity

## Evidence
- `../sqlite/test/func.test:348-350`: test body is `string tolower [db eval {SELECT round(1e500), round(-1e500);}]`, expected `inf -inf`.
- `testdata/func.json` preserves only SQL query `SELECT round(...)`, with expected lower-case output. Harness formats SQLite real infinities as `Inf`/`-Inf`; mismatch is lost Tcl result postprocessing, not engine math.
- A distinct `func-38.100` uses direct `do_execsql_test` with infinities and uppercase expectations; do not conflate the cases.

## Plan
1. Inspect Tcl test-body capture and JSON schema for preserving a transformed result contract. Search adjacent converted fixtures/tests for equivalent output processing.
2. Add the smallest generic, semantics-preserving representation for supported Tcl postprocessing, with converter regression first.
3. Have the harness apply the transformation to actual query result text, never rewrite expected values.
4. Regenerate only `func.json`; run converter regressions and exact `func-4.39`.
5. Record lesson and update family plan; don't claim broad func family resolved.

## Verification
`go test ./tools/tclconvert/... -count=1`; run exact `func-4.39` via harness subtest path after confirming naming.
