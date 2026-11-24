# PLAN — S45 slice 1: trigger7-2.x (2026-11-24)

## State

`trigger7` has six failing leaves, all in section 2 (`UPDATE OF` trigger
code generation). The fixture's do_test body is:

```
set txt [db eval {EXPLAIN UPDATE t1 SET x=5}]
string match *___update_t1.x___* $txt
```

Expected results: x-update EXPLAIN contains the `r1` marker but not `r2`;
y-update the reverse; rowid-update neither. Current fixture steps are a
single `query_assign` with `expect: 1/0`, so the harness compares the whole
flattened EXPLAIN row list against `1`/`0` and fails all six.

Two gaps:

1. Engine: `explain_rows.go`'s `emitUpdateRows` renders no trigger
   sub-program rows, so the markers are absent from EXPLAIN.
2. Converter/harness: a trailing `string match` over a runtime-captured
   variable is evaluated at CONVERSION time (over a runtime marker), and
   the harness runtime-expression command layer has no `string` command.

## C contract

- `trigger.c` compiles one VDBE sub-program per firing trigger; EXPLAIN
  renders those opcodes after the main UPDATE program. `UPDATE OF <cols>`
  only compiles triggers whose column list intersects the SET clause
  (trigger7.test:45-81); rowid-only updates match neither `OF x` nor `OF y`.
- Tcl `string match` is a glob match returning 1/0; the converter's
  `tools/tclconvert/tcl.StringMatch` already implements Tcl's matcher.

## Implementation

- `internal/execquery/explain_trigger_rows.go` emits firing UPDATE trigger
  sub-program rows; an `UPDATE OF` trigger fires only when its column list
  intersects the statement's SET columns. SELECT literals in the body are
  carried as `String8` p4 values, the opcode C uses for trigger constants.
- The synthetic EXPLAIN file was split: `explain_aggregate_distinct.go`
  now owns the distinctagg helpers, keeping `explain_rows.go` under the
  1000-line hard gate.
- `tools/tclconvert` emits a `runtime_expr` step for a trailing
  `string match <pattern> $var`, and the harness runtime command layer
  dispatches `string match` through `tcl.StringMatch`. The unrelated
  `tools/tclconvert/tcl` unit failures reproduce at HEAD (scratch worktree)
  and are pre-existing.
- Focused gates: trigger7 0, distinctagg 0, index9 0, where 0, select and
  indexexpr2 baseline-identical, bestindexF carded residue only.

## Ordered steps

1. Add a pure-Go pin test: create t1 + the two UPDATE OF triggers, assert
   EXPLAIN UPDATE x/y/rowid contains exactly the expected marker.
2. Engine: in `emitUpdateRows`, render a small sub-program cluster for
   each firing UPDATE trigger whose body SELECT carries a string literal;
   the `String8` p4 carries the literal so the flattened EXPLAIN contains
   the marker. Reuse the local `UPDATE OF` column parser and schema
   `FindTriggersForTable`.
3. Converter: emit a `runtime_expr` step for a trailing `string match`
   that reads a runtime variable (`string match <pattern> <var>`), instead
   of stamping the conversion-time expectation on the query_assign.
4. Harness: add `string` to the runtime command head words and dispatch
   `string match` through `tcl.StringMatch`.
5. DONE — full-suite gate, commit with TODO/lessons/handoff updates.

## Gate result

`/tmp/s45_full.log` (1186.2s, raw-byte failure-name scan): **1158
leaves**. Versus the prior gate, GONE = the six trigger7 leaves, NEW = 0.
The native pins, trigger7 family, and focused EXPLAIN sweeps are green.
Converter unit failures elsewhere in `tools/tclconvert/tcl` reproduce at
HEAD and are out of scope.

## Carded

- trigger7-99.1's malformed-schema lifecycle passes today; do not disturb it.
- misc3-6.11-utf8 remains the next S41 residual after this unit.
