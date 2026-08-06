# TASK G2.ORDERBY — ORDER BY, LIMIT, sort, MIN/MAX as aggregates

> **Phase**: G2 (query features).
> **Goal**: G2.ORDERBY.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.SELECT.
> **Current state: FAILING** — `limit` fails.

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
- [ ] **G2.ORDERBY.1** Pre-test suite. Commit: `G2.ORDERBY.1: orderby pre-test suite`.
- [ ] **G2.ORDERBY.2** Triage `limit` failure via pure-Go test. Likely OFFSET
  arithmetic or negative-limit handling. Fix `internal/exec/select.go`.
  Commit: `G2.ORDERBY.2: LIMIT/OFFSET edge cases`.
- [ ] **G2.ORDERBY.3** Cross-type sort order (NULL<numb<text<blob) + ASC/DESC +
  NULLS FIRST/LAST. Commit: `G2.ORDERBY.3: sort ordering`.
- [ ] **G2.ORDERBY.4** COLLATE per key. Commit: `G2.ORDERBY.4: ORDER BY COLLATE`.
- [ ] **G2.ORDERBY.5** Tie-break by rowid; MIN/MAX scalar aggregate.
  Commit: `G2.ORDERBY.5: tie-break + scalar MIN/MAX`.
- [ ] **G2.ORDERBY.6** testgen orderby/orderbyA/B/limit/minmax/sort green (triage
  sorterref/starschema — may be index-planner, move to G3/G5 if so).
  Commit: `G2.ORDERBY.6: orderby TCL green`.

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
