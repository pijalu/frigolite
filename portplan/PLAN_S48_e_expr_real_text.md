# PLAN — S48 slice 1: e_expr-32.2.8 real-text version drift (2026-11-24)

## State

`e_expr-32.2.8` expects SQLite 3.54's 17-significant-digit rendering
(`9.2233720368547758e+18`) for four CAST-to-NUMERIC results. The checked-in
fixture was generated from that newer CLI. The project's in-tree oracle
(`../sqlite`, 3.51.0) renders the same values as
`9.22337203685478e+18`, which is also what Frigolite's
`util.FormatSQLiteReal` emits. The engine value is correct; only the fixture
expectation is version-drifted.

## Approach

1. Pure-Go pin: run the fixture SQL and assert Cast-to-NUMERIC results render
   exactly as the in-tree SQLite 3.51 oracle (15 significant digits) and
   that the integer/text results are unchanged.
2. Correct the four long REAL strings in the fixture expectation to the
   3.51 spelling. Keep the row count and integer results untouched.
3. Verify focused family; full-suite failure-name-set gate; commit with
   TODO/lessons/handoff.

## Verification

- `FRIGOLITE_TEST=e_expr` leaves only `e_expr-26.1.6` (separate card).
- Pure-Go pin green.
- Full gate NEW=0 vs S47 apart from documented flakes.
