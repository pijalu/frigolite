# Plan — S55 slice 1: fts3query converter parity (2026-11-25)

## Problem

`fts3query` has 11 leaves in 3 classes:

1. `5.2.1`, `5.3.1`, `5.4.1`: fixture emits `exec` steps with expected text
   `matchinfo` for `do_select_tests -errorformat`. SQLite's tester expects a
   caught error (`1 {wrong number of arguments to function matchinfo()}` /
   `1 {illegal first argument to matchinfo}`). Frigolite already returns those
   exact error texts; the fixture expectation is wrong.
2. `fts3query-7.2` fails at the first `SMALLINT` insert: converter's TCL `expr`
   renders `0 + -9223372036854775808` as `-9.223372036854776e+18`, so all 100
   min-rowid inserts collide. The engine was never asked to insert the min
   rowid.
3. `7.2.1.1.1`, `7.2.1.2.1`, `7.2.6.1.1`, `7.2.6.2.1`, `7.3..2`, `7.3..4`
   are downstream row-shape comparisons against expectations built from the
   corrupted setup; they should disappear once the min rowid is emitted as an
   integer.

No engine change is indicated. This is a converter/fixture slice.

## Evidence

- `/tmp/t18_full.log`, `fts3query` block: engine errors for 5.2/5.3/5.4 match
  SQLite's `-errorformat` strings; the fixture records `exec`/`expect:
  matchinfo`.
- `testdata/fts3query.json`: `fts3query-7.2` step 100 is a 100x repeat of an
  insert with `-9.223372036854776e+18`; SQLite source uses
  `set SMALLINT -9223372036854775808` and `expr $i + $iFirst`.
- `tools/tclconvert/tcl/expr.go`: `parseUnary` negates `9223372036854775808`
  after `parseDecimalNumber` has already fallen back to float64 because the
  magnitude is one past `math.MaxInt64`.
- `tools/tclconvert/tcl/interp_cmds.go`: `commandDoSelectTests` recognizes
  `-error`, `-query`, `-tclquery` and `-repair`, but not `-errorformat`.

## Engine root cause found after regeneration

Regenerating `fts3query` exposed a second, real engine bug. Minimal pure-Go
pins reproduced it on both ordinary and FTS rowid tables: after inserting
`-9223372036854775808`, the next `-9223372036854775807` was stored as
`+9223372036854775807`. The in-memory FTS index itself was correct; the sign
was lost before `InsertWithID`.

Trace evidence: `rowidAliasColumnValue` read `lit=<nil>` for the first insert
but `lit=9223372036854775807` for the second. The template normalizer records
only the unsigned digit run, so `-9223372036854775808` (folded by the parser
into a signed `NumericLit`) and `-9223372036854775807` (a structural
`UnaryOp`) share one normalized shape. The template clone/slot path then
substituted the positive magnitude into the folded-literal slot, erasing the
unary minus.

Fix this class in the template layer, not in FTS:

- `internal/exec/template_slotpath.go`: refuse direct INSERT tuple slots whose
  `NumericLit` spelling is not a plain decimal integer/real (signed folded
  literals are not substitutable slots).
- `internal/exec/engine_core.go`: make `insertValue` apply the same shape gate
  to `NumericLit` slots before substituting int64/float64 values, so a clone
  refusal falls back to a fresh parse.

Keep the converter fixes above; both classes are needed for the 11 leaves.

## Exact changes

1. `tools/tclconvert/tcl/interp_cmds.go`
   - Treat `-errorformat` like `-error`: set `errorMode = true` and store the
     format string in `errorFormat`.
   - Include `-errorformat` in the explicit option-value skip list.
2. `tools/tclconvert/tcl/expr.go`
   - In `parseUnary`, detect the exact int64-min literal magnitude
     `9223372036854775808` before float fallback and return `math.MinInt64`.
   - Keep the existing int64 negation behavior for all other operands.
3. Converter unit tests (tracked files in the ignored converter subtree):
   - `tools/tclconvert/tcl/interp_sql_test.go`: pin `do_select_tests
     -errorformat` emission to a `catch` step with
     `1 {wrong number of arguments to function matchinfo()}`.
   - `tools/tclconvert/tcl/floatfmt_test.go`: pin `EvalExpr("0 +
     -9223372036854775808")` and `exprNumericLiteral` output to int64-min text.
4. `internal/exec/template_slotpath.go`
   - In `collectInsertTuples`, accept a direct `NumericLit` tuple slot only
     when its spelling is a plain decimal integer/real; signed folded literals
     keep the template COW-only.
5. `internal/exec/engine_core.go`
   - In `insertValue`, refuse int64/float64 substitution into a `NumericLit`
     slot whose spelling is not a plain decimal integer/real, and refuse the
     ambiguous 2^63 float; the clone then falls back to a fresh parse.
6. Tests:
   - `frigolite_fts3query_rowid_t19_pin_test.go`: pure-Go boundary sequence on
     a regular FTS4 table.
   - `internal/exec/template_clone_gate_test.go`: folded negative tuple slot
     refuses template substitution.
7. Regenerate only `testdata/fts3query.json` with the current converter:
   `go run ./tools/tclconvert -testdir ../sqlite/test -outdir testdata -only fts3query`.

## Verification

- Converter package raw failure-name set unchanged vs
  `/tmp/t17_tclconvert_all.log`; added tests green.
- Focused `fts3query` gate: raw leaf-fail set diff vs `/tmp/t18_full.log` has
  NEW=0 and all 11 target leaves GONE.
- Quality NEW=0 for staticcheck/gocognit/gocyclo.
- Detached full gate with `-timeout 30m`; raw-byte leaf fail-set diff vs
  `/tmp/t10_full2.log` and `/tmp/t18_full.log`, with documented flakes called
  out explicitly.
- Code commit, then docs/handoff/lessons/TODO commit.

## Execution record (2026-11-25)

- Code commit `c02ff098a`: converter `-errorformat` support, int64-min TCL
  expression parsing, regenerated `fts3query.json`, and template-slot shape
  gates for folded signed/hex numeric literals.
- Pure-Go pins: FTS4 boundary rowid sequence (root) and direct FTS index
  boundary docids (`internal/fts`) failed before the fix, passed after.
- Focused gate `/tmp/t19_focus2.log`: exit 0; all 11 `fts3query` leaves GONE
  vs `/tmp/t18_full.log`, NEW=0.
- Quality `/tmp/t19_quality3.log`: staticcheck/gocognit/gocyclo NEW=0 vs
  `/tmp/t18_quality.log`; only pre-existing file-size violations remain.
- Converter full package `/tmp/t19_tclconvert_all.log`: failure-name set
  NEW=0 vs `/tmp/t17_tclconvert_all.log`.
- Final full gate `/tmp/t19_full2.log` (1273 leaves): raw fail-set diff NEW=0
  vs both `/tmp/t18_full.log` and durable `/tmp/t10_full2.log`; 11
  `fts3query` leaves GONE. An earlier gate showed the documented
  `atof1-2.40` dice flake; focused `/tmp/t19_atof1_focus.log` was green.
- A refactor briefly registered non-literal tuple items (NULL) as slots;
  changed-package tests caught the `stashValues` panic and the helper was
  corrected before the final gate.

## Risks / non-goals

- No hand-editing JSON; regenerate only the one family.
- No engine changes unless a regenerated fixture failure produces a failing
  pure-Go pin.
- Do not broaden the converter to arbitrary bignums; the observed contract is
  the int64-min boundary used by SQLite's test suite.
- Full gates must use raw-byte leaf-name sets only, never count/line greps.
