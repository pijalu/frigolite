# Handoff 2026-11-25 — TCL whole-reference expansion

## Why

Review item in `TODO.md`: `TestExpandTCLRuntimeVarsWholeReferences` failed at
`frigolite_harness_expand_test.go:24`.
`expandTCLRuntimeVars("SELECT $t$tcl FROM x", {t:"null", tcl:"'abc'"})`
returned `SELECT null'abc' FROM x`, splicing a quoted SQL literal into one
`$name` parameter. Harness/fixture-fidelity only; no engine behavior changed.

## Root cause

S32 (commit `9a8391251`) fixed the dead adjacent-placeholder split in
`expandTCLVarRefs` (`frigolite_harness_test.go`) so the converter's frozen
`[list "${as}${bs}"]` expect value `$as$bs` expands to `helloworld12345`.
The split fired whenever the first segment of a scanned key resolved:
`$t$tcl` scanned as ONE reference (`$` is a valid SQL identifier byte), so
the first segment `t` split off and the unrelated `tcl` value was spliced in.
The whole-name boundary scan itself (`scanTCLParamRef`) was already correct:
`$tcl`, `$tcl(array)`, `$tclx`, `$n:123`, `$n::123` and `$::n` all scan and
resolve by the full name.

## Fix

`frigolite_harness_test.go`:

- `expandTCLVarRefs`' split branch now requires
  `isAdjacentTCLRefBare(key, values)` in addition to a resolvable first
  segment.
- `isAdjacentTCLRefBare` walks every `$`-separated segment. Unknown segments
  stay verbatim (S32 behavior for `$as$missing`); a resolved segment whose
  value is not a bare SQL token (`'abc'` is quoted) blocks the split, so
  `$t$tcl` stays a single unexpanded parameter reference.
- Single references (no inner `$`) are untouched; `sqlSafeRuntimeValue` and
  legitimate bare-token substitutions (`$as$bs`, `$bs$as`, `$as$bs$as`)
  keep their existing rendering.

## Verification

- Repro before fix: `/tmp/review_root_repro.log` —
  `got "SELECT null'abc' FROM x", want "SELECT $t$tcl FROM x"`.
- Pins after fix: `TestExpandTCLRuntimeVarsWholeReferences`,
  `TestExpandTCLRuntimeVarsWithoutTclVarKeepsParameter`, and the S32
  `TestExpandTCLVarRefsAdjacentPlaceholders` all pass (`-count=1`).
- `FRIGOLITE_TEST=e_expr go test -run '^TestSQLiteSuite$' -count=1 .`:
  0 fail leaves (`/tmp/expand_post_e_expr.log`), NEW=0 vs
  `/tmp/t30_baseline_leaves_clean.txt` (which has 0 `e_expr/` leaves).
- `go test -run TestSOLID_ ./...`: green.
- Recorded verify command: green.

## Residual risk

The rule uses `isBareSQLToken` on raw runtime values. A future converter shape
that concatenates two non-bare (quoted) runtime values into one Tcl string
will stay whole instead of expanding; that shape has no fixture today. The
guard is deliberately harness-only — SQL execution and fixtures are unchanged.
