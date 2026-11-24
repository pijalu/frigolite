# PLAN — S50 slice 1: indexexpr2-4.120/4.130 counter resets (2026-11-24)

## State

Both leaves expect `cnt` after resetting it immediately before the UPDATE.
The checked-in fixture has no reset steps, so the runtime counter carries
the previous probe's value: 1+2 = 3 for 4.120 and 3+0 = 3 for 4.130.

The converter's pending-file-set emission already produces a `runtime_expr`
with `Variable=cnt, SQL=0` for `set ::cnt 0`; the fixture predates it.

## Approach

1. Converter unit: pin the numeric global reset shape for `cnt` with a
   `global cnt` callback proc (same path as `::evalcount` in S49).
2. Add the two missing `runtime_expr` reset steps before the UPDATEs in
   `testdata/indexexpr2.json`. Do not change the expected counts.
3. Verify focused family (leaves 4.120/4.130 → 0); full-suite failure-name-set
   gate; commit with TODO/lessons/handoff.

## Verification

- `FRIGOLITE_TEST=indexexpr2`: only the three engine/EQP leaves remain.
- Converter unit green.
- Full gate NEW=0 vs S49 apart from documented flakes.
