# PLAN — S49 slice 1: e_expr-26.1.6 callback counter reset (2026-11-24)

## State

`e_expr-26.1.6` expects `evalcount == 5`, but the harness reports 8. The
Tcl source resets `::evalcount` to 0 between the 26.1.5 and 26.1.6 probes.
The converter records `set ::triggers_fired [list]`-style empty resets for
tracked callback globals, but a numeric reset (`set ::evalcount 0`) is not
emitted at all, so the runtime counter accumulates 3 + 5.

## Approach

1. Converter check: `cmdSet` already emits a `runtime_expr` with
   `Variable=evalcount, SQL=0` for a numeric global reset when a later
   probe reads it. No converter code change is needed; pin that shape with
   a unit test.
2. The checked-in fixture predates that emission and has no reset step,
   so add the `runtime_expr` reset at the start of e_expr-26.1.6.
3. Verify focused `e_expr` family → 0 FAIL; full-suite failure-name-set
   gate; commit with TODO/lessons/handoff.

## Verification

- `FRIGOLITE_TEST=e_expr`: 0 FAIL.
- Converter unit for numeric tracked-var reset: green.
- Full gate NEW=0 vs S48 apart from documented flakes.
