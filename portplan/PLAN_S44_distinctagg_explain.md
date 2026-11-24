# PLAN — S44 slice 1: distinctagg aggregate-DISTINCT EXPLAIN rows (2026-11-24)

## State

At commit `700b45148`, the S41 residual `distinctagg` family has exactly
five failing leaves: 3.3.1, 3.8.1, 4.2.1, 4.3.1, 4.5.1. Each is the same
assertion shape:

```
query_assign EXPLAIN <SELECT count(DISTINCT ...) ...> -> prg (resultFlat)
runtime_expr [lsearch $prg OpenEphemeral]>=0 -> idx
runtime_var idx expect 1
```

Current engine yields `idx=0`: the synthetic EXPLAIN row stream has no
`OpenEphemeral` for an aggregate function's DISTINCT set.

## C contract

`../sqlite/src/select.c:6788-6807` (`resetAccumulator`) emits one
`OP_OpenEphemeral` per aggregate with `iDistinct>=0`, with the DISTINCT
argument key info; `select.c:7027-7035` (`codeDistinct`) then emits the
Found/MakeRecord/IdxInsert sequence inside the aggregate step. C also
reports `USE TEMP B-TREE FOR <func>(DISTINCT)` in EQP from the same site.
Our tree-walking executor has no literal opcode stream; `explain_rows.go`
derives one from the resolved plan, so the aggregate-DISTINCT cluster must
be added there.

## Implementation

`internal/execquery/explain_rows.go` now emits one `OpenEphemeral` per
DISTINCT aggregate whose argument is not delivered in order. The criterion
mirrors C's `WHERE_DISTINCTBY`/`isDistinctRedundant`: a rowid argument, an
argument pinned to a constant by WHERE, or a plain column immediately after
the index's WHERE-equality and GROUP BY prefix needs no b-tree; expression
arguments and columns not so served do. Focused distinctagg, index9,
bestindexF, indexexpr2, select, where and explain sweeps are baseline-clean
(known carded/flaky leaves only).

## Ordered steps

1. Add a pure-Go pin test over the public API that creates the fixture's
   tables and asserts `EXPLAIN SELECT count(DISTINCT c) FROM t1` contains
   `OpenEphemeral` (and, where C emits it, `IdxInsert`).
2. Add a small collector for aggregate `DISTINCT` calls in the SELECT's
   expressions (including GROUP BY/HAVING), and emit one `OpenEphemeral`
   row per call from `emitExplainRows` before the plan-node clusters.
3. Verify the five leaves and the whole distinctagg family; keep the S41
   hand-pinned EXPLAIN families green (index9, bestindexF, indexexpr2).
4. DONE — full-suite failure-name-set gate, then commit with
   TODO/lessons/handoff updates.

## Gate result

`/tmp/s44_full.log` (1226.7s, raw-byte failure-name scan): **1164
leaves**. Versus the prior gate, GONE = the five distinctagg leaves, NEW =
only the documented temptable2 flake. The native pin test and the full
distinctagg family are green.

## Carded

- trigger7-2.x trigger sub-program rows and misc3-6.11 remain queued after
  this unit.
