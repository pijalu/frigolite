# PLAN S22 — the where-family sort-marker classes (the where census 78-leaf slice 1)

Session: 2026-11-11, feature/perf @ 965d510db. Baseline:
`/tmp/w_s22_leaves.txt` (the isolated where sweep at HEAD = exactly the S21
post state: where 26, where8 16, whereC 10, where9 8, where2 6, whereH 5,
wherelimit2 4, whereF 3).

## Diagnosis (verified this session with traces + the 3.54 oracle)

The 26 `where` leaves decompose:

- **B: 7.1-7.6/7.17-7.22 (8)** — `SELECT w FROM t1 WHERE x=3 [AND y>…]
  ORDER BY y [DESC] [LIMIT n]`. Our EQP renders the right plan
  ("SEARCH t1 USING INDEX i1xy") but the EXECUTION loop is a full scan +
  sorter. Root cause: `indexSeekChosen`'s stat1 regime applies a 10%
  selectivity cliff (`bestEstimate < nRow*0.10`). The where-6.1 ANALYZE
  gives stat1 "t1 i1xy 100 8 1"; the x=3 equality estimates ~15 rows = 15%
  → seek declined → kind=0 loop → `emitIndexOrderedRows` declines
  (loopOrderedEmission needs kind==loopIndex) → RecordSortOp.
  ORACLE (3.54, ANALYZE): SEARCH i1xy for every one of these shapes, AND at
  50% selectivity (`a=?` over "1000 500 1"), AND seek+TEMP-B-TREE when the
  index does not deliver the order (`a=? ORDER BY b` over (a)). C's rule:
  **seek wins whenever est < nRow; the tie (est == nRow) goes to SCAN**;
  the sorter never rescues the scan (scan+sort >= seek+sort on both axes).
  The no-stat1 branch (default model est = nRow/10) is unchanged.
- **C: 12.2/12.6/13.2/13.6 (4)** — t6/t7(a INTEGER PRIMARY KEY, b TEXT)
  + index on (b): `ORDER BY b, a` / `b DESC, a DESC`. Rows already match;
  the marker differs. C consumes the trailing rowid/PK term via the index's
  trailing rowid column (where.c nColumn = nKeyCol+1, XN_ROWID). Our walk
  HAS the rowid tail (walkColumnStep's isRowidCol) — verify why it
  declines (the term name `a` resolves to the IPK alias, the walk column
  is "rowid" — the matchWalkColumn must map the alias).
- **D: 14.1-14.4/14.7.2 + 6.15 (6)** — join ORDER BY delivery (per-loop
  ordering + the order-distinct mark-off for terms referencing only
  already-ordered loops). 14.3/14.7.2 ALSO differ in row order (C's inner
  loop scans y's covering UNIQUE index). Separate slice.
- **A: 6.7.1/6.7.2 (2)** — `WHERE b>0 ORDER BY a LIMIT n` over t3 + 3
  indexes + ANALYZE: C picks SCAN t3 USING COVERING INDEX t3acb (the
  a-order covering scan) — an ORDER-BY-aware planner choice among
  covering candidates. May fall out of the seek fix (t3bc est vs t3acb);
  re-triage after slice 1 lands.
- **E: 19.0 (1)** — EQP autoindex names (sqlite_autoindex_t191_1/2) —
  separate class.
- **F: 25.1/25.2/25.5 (3)** — corruption ("database disk image is
  malformed" on DELETE/INSERT) — separate class, likely the fixture
  corrupts the index; needs the integrity check at write time.

## Slice 1 (this unit): the seek threshold + the marker buckets B and C

1. `indexSeekChosen` (select_order_search.go): the stat1 branch becomes
   `bestEstimate < nRow` (strict — the tie is C's scan win, probed via
   "1000 1000 1" → SCAN t). Update the doc comment with the oracle probes.
2. Run the where family; name-set diff vs baseline. Expect bucket B
   green; check nothing else moved (preferOrderByServingIndex now engages
   on the previously-declined seeks; 6.7.x may flip).
3. Bucket C: if the walk's rowid tail still declines, fix the IPK-alias
   mapping in matchWalkColumn (col "a" vs walk column "rowid").
4. Native pin (frigolite_where_s22_test.go): the oracle EQP probes as
   engine assertions (seek at 15%/50% with stat1, tie→scan, seek+sorter
   when the index does not deliver).
5. Full-suite gate: no family worse (name-set diff vs
   /tmp/full_S15_fails.txt re-derived at HEAD).

## Verification protocol

- `FRIGOLITE_TEST=where go test -run "^TestSQLiteSuite$" -count=1` — the
  failure-name-set diff (Python set logic, not comm — macOS comm trap).
- Binary-verify: `go vet .` before trusting zeros; the run must COMPLETE
  (tail shows ok/FAIL + duration, never "panic: test timed out").
- Full-suite gate sequential, -timeout 25m.

## Out of scope (later slices)

- Slice 2: the join ORDER BY delivery (bucket D).
- Slice 3: 19.0 EQP names + 25.x corruption + where8/whereC/where9/where2/
  whereH/wherelimit2/whereF classes (the rest of the 78-leaf census).
