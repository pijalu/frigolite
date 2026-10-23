# Analyze8 Tcl Integer Division Repair Plan

## Evidence

Latest full suite retained in `portplan/FULL_SUITE_AFTER_ANALYZE.*` (exit 1, 377 s). First remaining family is `analyze8`, failing 3.0: expected count 50 for `b BETWEEN 30 AND 34`, got 41. Canonical `../sqlite/test/analyze8.test:39-44` computes `set b [expr {$i/10}]` for integer loop variable i. Regenerated `testdata/analyze8.json` currently contains e.g. `INSERT ... VALUES(100,0.1,...)`; Tcl integer division of two integer operands yields integer values (0 for i=1), so converter's expression arithmetic is wrong and fixture input diverges.

## Steps

1. Inspect `tools/tclconvert/tcl/expr.go` arithmetic semantics and existing expression tests; isolate integer `/` versus floating division.
2. Add converter regression for integer/integer and float/integer expression division, matching Tcl `expr`.
3. Implement smallest Tcl-faithful division behavior, regenerate only analyze8.json, inspect representative generated inserts and exact counts.
4. Run converter tests, exact analyze8 family; retain full-suite evidence and proceed to next failure.