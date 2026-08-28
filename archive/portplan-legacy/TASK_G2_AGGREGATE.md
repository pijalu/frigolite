# TASK G2.AGGREGATE — GROUP BY, HAVING, aggregates, DISTINCT

> **Phase**: G2 (query features).
> **Goal**: G2.AGGREGATE.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1 stable; G2.ORDERBY (ordering of group output).
> **Current state: PASSING** — all six verify testgen packages (count, having, distinct,
> distinctagg, aggorderby, aggerror) plus the TestP2Aggregate pre-tests pass.

## Objective
Aggregation matches SQLite: `GROUP BY` (column + expression keys), `HAVING`
(row-group filter, can reference aggregates + grouping cols), aggregate
functions (`COUNT(*)`, `COUNT(x)` ignoring NULL, `SUM/AVG/MIN/MAX/TOTAL`,
`GROUP_CONCAT` with separator), `DISTINCT` inside aggregates (`COUNT(DISTINCT
x)`), `SELECT DISTINCT` (coordinate with G1.SELECT), bare-column-with-aggregate
rules (must be in GROUP BY or error), NULL handling in aggregates, and the
output ordering of grouped queries (SQLite groups are not sorted unless ORDER BY
— match scan order).

## Scope — testgen packages
`count`, `having`, `distinct`, `distinctagg`, `aggorderby`, `aggerror`.
(`aggfault` is fault-injection → N/A.)

## Pre-test file
`frigolite_p2_aggregate_test.go` — `TestP2Aggregate_*`. Cases vs oracle:
- `COUNT(*)` vs `COUNT(col)` (NULL skipped) vs `COUNT(DISTINCT col)`.
- `SUM`/`AVG`/`MIN`/`MAX`/`TOTAL` with NULLs and empty groups.
- `GROUP_CONCAT(x)` and `GROUP_CONCAT(x, sep)` ordering.
- GROUP BY one + multiple keys; GROUP BY expression.
- HAVING with aggregate and with grouping column.
- `SELECT DISTINCT` vs `DISTINCT` in aggregate.
- Bare column not in GROUP BY → error ("column ... not in GROUP BY" or implicit —
  match SQLite).
- Empty table: `SELECT COUNT(*) FROM empty` → 0 (one row); `SELECT SUM(x)` → NULL.

## SQLite source references
- `src/select.c` — `sqlite3Select`, aggregate accumulator, GROUP BY/HAVING.
- `src/func.c` — `SUM/AVG/MIN/MAX/COUNT/GROUP_CONCAT` implementations.

## Steps
- [x] **G2.AGGREGATE.1** Pre-test suite (`frigolite_p2_aggregate_test.go`).
- [x] **G2.AGGREGATE.2** NULL-aware aggregates + HAVING: COUNT(col) skips NULL, HAVING
  with aggregates/grouping cols, DISTINCT-args validation, misuse-of-aggregate in IN
  subqueries, test-only `nondeter()` (having), qualified `t1._rowid_` resolution.
- [x] **G2.AGGREGATE.3** `COUNT(DISTINCT x)`, DISTINCT aggregates: exactly-one-arg
  rule, DISTINCT-in-agg collation/ordering.
- [x] **G2.AGGREGATE.4** GROUP_CONCAT with separator + ordering: function-call ORDER BY
  recovered in subqueries, COLLATE in aggregate ORDER BY, too-many-terms limit.
- [x] **G2.AGGREGATE.5** GROUP BY expressions: GROUP BY 1 reuses the group key for
  non-deterministic exprs (random()); SELECT DISTINCT covering-index ordering + nocase.
- [x] **G2.AGGREGATE.6** Empty-table semantics (COUNT→0, SUM→NULL, TOTAL→0).
- [x] **G2.AGGREGATE.7** testgen count/having/distinct/distinctagg/aggorderby green;
  distinctagg EXPLAIN-opcode checks and aggorderby json_group_array tests are
  documented skips (G5.EXPLAIN / JSON1 out of scope); distinct2-120 forward-ref join
  is a documented skip (G3.INDEX join order).

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/count/ ./testgen/having/ ./testgen/distinct/ ./testgen/distinctagg/ ./testgen/aggorderby/ ./testgen/aggerror/ && \
go test -run 'TestP2Aggregate' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Aggregation matches SQLite: GROUP BY (col+expr keys), HAVING (aggregate + grouping col), COUNT/SUM/AVG/MIN/MAX/TOTAL/GROUP_CONCAT with NULL handling, DISTINCT in aggregates and SELECT DISTINCT, bare-column-in-GROUP-BY rule, empty-group semantics. count/having/distinct currently FAIL. See portplan/TASK_G2_AGGREGATE.md." \
  completionCriterion "testgen count, having, distinct, distinctagg, aggorderby, aggerror PASS and TestP2Aggregate pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/count/ ./testgen/having/ ./testgen/distinct/ ./testgen/distinctagg/ ./testgen/aggorderby/ ./testgen/aggerror/ && go test -run TestP2Aggregate -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G2.AGGREGATE. count/having/distinct FAIL. Group phase in internal/exec/select.go;
aggregate funcs in internal/function/. COUNT(col) skips NULL; COUNT(*) doesn't.
Decisions: grouped output not sorted unless ORDER BY (match scan order).
Next: pre-tests, triage NULL-aware aggregates + HAVING, then DISTINCT-in-agg, GROUP_CONCAT.
Risks: GROUP BY over expressions + collation interplay.
Carried limits: verifyCommand above.
```
