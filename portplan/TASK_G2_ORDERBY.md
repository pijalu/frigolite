# TASK G2.ORDERBY — ORDER BY, LIMIT, sort, MIN/MAX as aggregates

> **Phase**: G2 (query features).
> **Goal**: G2.ORDERBY.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.SELECT.
> **Current state: PASSING** — all 6 target testgen packages green.
> **Note**: `limit` already passed when this goal started; the work landed on
> min/max bare-column semantics, cross-type aggregate comparison, view
> qualified-column resolution, compound ORDER BY validation, and out-of-scope
> skips (EQP/G3.INDEX, FTS3, CTE, sorter internals).

## Objective
Sorting matches SQLite: ORDER BY over columns/expressions/aliases/ordinals,
ASC/DESC, NULLS FIRST/LAST, COLLATE per key, mixed-type sort ordering
(SQLite's well-defined cross-type sort order: NULL < INTEGER/REAL < TEXT < BLOB),
LIMIT/OFFSET incl. negative limit, `ORDER BY RANDOM()`, MIN/MAX as scalar
aggregates (`SELECT MIN(x) FROM t` with no GROUP BY), and tie-breaking stability
matching SQLite (rowid order for ties).

## Scope — testgen packages
`orderby`, `orderbyA`, `orderbyB`, `limit`, `minmax`, `sort`, `sorterref`,
`starschema` (the last two may need index/cooperation — triage).

## Pre-test file
`frigolite_p2_orderby_test.go` — `TestP2Orderby_*`. Cases vs oracle:
- ORDER BY column/expr/alias/ordinal; ASC/DESC; multi-key.
- NULLS FIRST/LAST (default depends on ASC/DESC).
- COLLATE per key overriding affinity.
- Cross-type sort: NULL, numbers, text, blob ordering.
- LIMIT n; OFFSET m; LIMIT -1 (no limit); LIMIT 0.
- `ORDER BY RANDOM()` reproducibility caveat (non-deterministic seed — compare
  set, not order, or skip with a documented reason).
- `SELECT MIN(x), MAX(x)` scalar aggregate form.
- Tie-break = rowid order.

## SQLite source references
- `src/select.c` — sorter, `sqlite3Select` ORDER BY/LIMIT.
- `src/vdbesort.c` — sort comparison (collation, type order).
- `src/func.c` — `min/max` (scalar vs aggregate context).

## Steps
- [x] **G2.ORDERBY.1** Pre-test suite (`frigolite_p2_orderby_test.go`).
- [x] **G2.ORDERBY.2** LIMIT/OFFSET — `limit` already passed; edge cases covered by
  pre-tests (LIMIT -1, LIMIT 0, LIMIT/OFFSET).
- [x] **G2.ORDERBY.3** Cross-type min/max aggregate comparison (`util.CompareValues`
  replaces the simplified `less`); sort ordering verified by pre-tests.
- [x] **G2.ORDERBY.4** COLLATE per key — pre-test covers ORDER BY ... COLLATE NOCASE.
- [x] **G2.ORDERBY.5** MIN/MAX scalar aggregate bare-column semantics (bare columns
  take values from the last min/max aggregate's row, per group and globally);
  compound ORDER BY validation; view qualified-column keys.
- [x] **G2.ORDERBY.6** testgen orderby/orderbyA/B/limit/minmax/sort green.
  Out-of-scope tests skipped via tcl2go skipTests/skipTestFiles:
  - orderby1/2/5 EXPLAIN QUERY PLAN ORDER BY plans (G3.INDEX)
  - orderby7 FTS3 MATCH (FTS not supported)
  - sort 15.x CTE, sort-16.2 UNIQUE index, sort-18.2 EQP
  - sort3 cksum + CTE + sorter mmap test control
  - sort4 VDBE sorter internals (do_sorter_test), sort5 1.0 fixed via
    PRAGMA mmap_size returning 0 (mmap-disabled SQLite behavior)
  - sorterref/starschema remain failing at HEAD (query planner, G3.INDEX/G5).

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/orderby/ ./testgen/orderbyA/ ./testgen/orderbyB/ ./testgen/limit/ ./testgen/minmax/ ./testgen/sort/ && \
go test -run 'TestP2Orderby' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Sorting matches SQLite: ORDER BY (col/expr/alias/ordinal), ASC/DESC, NULLS FIRST/LAST, COLLATE per key, cross-type sort order (NULL<numb<text<blob), LIMIT/OFFSET (incl negative), MIN/MAX scalar aggregate, rowid tie-break. limit currently FAILS. See portplan/TASK_G2_ORDERBY.md." \
  completionCriterion "testgen orderby, orderbyA, orderbyB, limit, minmax, sort PASS and TestP2Orderby pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/orderby/ ./testgen/orderbyA/ ./testgen/orderbyB/ ./testgen/limit/ ./testgen/minmax/ ./testgen/sort/ && go test -run TestP2Orderby -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G2.ORDERBY. limit FAILS. Sort in internal/exec/select.go. Cross-type order must match
SQLite (NULL < INTEGER/REAL < TEXT < BLOB). Tie-break by rowid.
Decisions: RANDOM() order is non-deterministic — compare as a set, document.
Next: pre-tests, triage limit, then cross-type sort + COLLATE, then minmax/sort.
Risks: sorterref/starschema may need the query planner (G5) — move if so.
Carried limits: verifyCommand above.
```
