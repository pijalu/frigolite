# aggnested-1.2 Mixed Aggregate Ownership Plan

## Evidence
- Latest serialized full run: `/tmp/frigolite-progress1.log`, exit 1, elapsed 335s (`.exit`, `.elapsed`). 367 top-level failing fixture families remain; `analyze5` absent from failure names after fixture correction.
- Isolated source harness: `/tmp/aggnested-current.log`; `FRIGOLITE_TEST=aggnested go test . -run '^TestSQLiteSuite$' -count=1` fails 1.2 with `1x1-4y5` vs `1x2x3-4y5`; unrelated tests 10.1, 11.2, 11.3 are validation failures.
- Native direct regression in `frigolite_aggnested_pin_test.go` fails identically (`TestAggnestedPin`, got `1x1-4y5`). Oracle source `../sqlite/test/aggnested.test:20-34` expects `1x2x3-4y5`.
- SQLite `../sqlite/src/select.c:6623-6650` analyzes each aggregate call and its arguments. In Frigolite, `evalAggOverOuterRowsWithInner` chooses one aggregate row set per output expression (`select_agg.go:496-529`), while `Evaluator.evalAggWrapperCall` evaluates every nested aggregate with global `AggRowMaps()` (`execexpr/expression_eval.go:366-376`). Mixed outer/inner calls require per-call aggregate row ownership.
- A prior broad descendant-walker experiment changed result to NULL and was reverted. It did not model per-function ownership.
- Implemented per-call aggregate row ownership: `SelectEngine` stores row maps keyed by `*sql.FuncCall`; correlated evaluation assigns each aggregate call rows based on its argument references; aggregate wrapper dispatch retrieves by call node. Direct native pin now passes; isolated `aggnested` has only independent validation failures 10.1, 11.2, 11.3. Affected `internal/execquery`, `internal/execexpr`, and `internal/exec` tests pass.

## Ordered implementation plan
1. Add a context API for a registered aggregate call's chosen row set, retaining existing global fallback for callers without overrides. Keep interface narrow and document ownership/lifetime.
2. In correlated aggregate evaluation, walk all expression nodes; for each registered aggregate call choose outer versus combined inner stepping rows using existing `aggregateHasOnlyOuterRefs`; associate rows with that call node, then evaluate output expression.
3. Update aggregate wrapper dispatch to request row set for current function node. Confirm nested call paths and cleanup on every return.
4. Run `TestAggnestedPin` (must go RED before edit baseline; already RED) and add cases 1.1/1.2/1.3 parity; run all affected aggregate/native pins and isolated aggnested suite.
5. Re-run full suite; retain elapsed/profile/output and derive next failure family from full evidence.
