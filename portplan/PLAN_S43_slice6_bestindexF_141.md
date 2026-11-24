# PLAN — S43 slice 6: bestindexF-1.4.1 (2026-11-22)

## State

At commit `435ca14d0`, bestindexE is 0 and the bestindex residue is eight
leaves: bestindex1-3.4/3.5, bestindexF-1.4.1, and the carded 2.8/2.9/2.10
`.0` fixture-placement trio (plus their `.1` companions). The next engine
slice is `bestindexF-1.4.1`.

The fixture's helper `do_idxinsert_test` computes
`set uses [uses_idxinsert $sql]` where `uses_idxinsert` is
`lsearch [db eval "explain $sql"] IdxInsert`, then emits
`SELECT $uses ; $sql`. The failure got `[0 0 1]`, want `[1 0 1]`: our
EXPLAIN row stream has no `IdxInsert` for

```sql
SELECT DISTINCT t0.c0 FROM t1, t0 ORDER BY t1.a;
```

`t1` is the tcl virtual table whose xBestIndex consumes `ORDER BY a`;
`t0(c0)` is an ordinary table.

## Diagnosis (revised by the pin test)

The pure-Go pin test (`frigolite_bestindexF_141_test.go`) drives the real
engine with the same tcl module and shows the EXPLAIN stream ALREADY carries
`OpenEphemeral Found MakeRecord IdxInsert`, and EQP already reports
`USE TEMP B-TREE FOR DISTINCT`. No engine change is needed.

The fixture is stale in a converter-specific way: `uses_idxinsert` evaluates
`lsearch [db eval "explain $sql"] IdxInsert` at CONVERSION time, but
`tools/tclconvert` emits `db eval` as a runtime `query` step and returns a
runtime marker. `lsearch` over the marker yields 0, so the converter baked
`SELECT 0 ; ...` into bestindexF-1.4.1. Regenerating the family reproduces
the same artifact (and would also reorder the hand-curated callback procs),
so the installed fixture needs a one-line correction to `SELECT 1 ; ...`.
The pin test keeps the engine contract honest.

## Ordered steps

1. DONE — add the pure-Go pin test driving the public API with the
   bestindexF tcl module; it proved the engine contract is correct.
2. DONE — regenerate bestindexF into /tmp; the same `SELECT 0 ;` artifact
   appears, confirming a converter/runtime-marker limitation, not engine.
3. DONE — correct the installed fixture's baked value to `SELECT 1 ;` and
   verify bestindexF-1.4.1 plus the pin test; the carded 2.8/2.9/2.10 `.0`
   fixture-placement trio is the only remaining bestindexF residue.
4. DONE — full-suite failure-name-set gate, then commit with
   TODO/lessons/handoff updates.

## Gate result

`/tmp/s43_slice8_full.log` (1177.3s, raw-byte failure-name scan): **1168
leaves**. Versus gate7: GONE = bestindexF-1.4.1 + the documented temptable2
flake, NEW = 0. The native pin test and the bestindexF family are green
apart from the carded fixture-placement class.

## Carded

- bestindexF 2.8/2.9/2.10 `.0` fixture-placement class (converter).
- bestindex1-3.4/3.5 vtab IN-loop / join-flow class.
