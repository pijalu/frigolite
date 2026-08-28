# TASK G5.EXPLAIN — EXPLAIN / EXPLAIN QUERY PLAN

> **Phase**: G5 (advanced SQL).
> **Goal**: G5.EXPLAIN.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1+G2 (all query features produce a plan).
> **Current state: COMPLETE** — EQP emits SQLite-shaped labels (SCAN/SEARCH/
> USE TEMP B-TREE/COMPOUND QUERY/CO-ROUTINE/SCALAR|LIST SUBQUERY); plain
> EXPLAIN emits the opcode-dump column shape. Testgen eqp passes, pre-tests
> pass, EQP-shape failures in where/selectN resolved. (Committed as
> `ff0c03807` "G5.EXPLAIN.2-3: SQLite-shape EQP emitter + pre-tests".)

## Objective
`EXPLAIN` (bytecode-style op dump) and `EXPLAIN QUERY PLAN` (EQP: SCAN/SEARCH/
USE labels) produce output the TCL tests accept. The testgen tests often match
EQP output with regex patterns, so frigolite's EQP must emit the *shape* SQLite
does (`SCAN <table>`, `SEARCH <table> USING ...`, `USE TEMP B-TREE FOR ...`,
`COMPOUND QUERY`, `CO-ROUTINE`, etc.). This is delicate — many testgen tests
filter/normalize EQP; ensure the transpiler's normalization matches.

> **Note:** Frigolite is not a bytecode VDBE, so full `EXPLAIN` opcodes won't
> match SQLite's. Tests asserting exact bytecode are N/A; tests asserting EQP
> *shape* are in scope. Triage each: if a test pins exact opcode numbers/text →
> N-A with evidence; if it matches EQP shape via regex → make it match.

## Scope — testgen packages
`eqp`, `explain` (if present). EQP assertions are *scattered* across many
packages (where, selectN, join*) — this task defines the EQP emitter + the
transpiler's EQP normalization, then those packages' EQP-only failures resolve.

## Pre-test file
`frigolite_p5_explain_test.go` — `TestP5Explain_*`. Cases vs oracle:
- EQP for a full scan: `SCAN t`.
- EQP for an index search: `SEARCH t USING INDEX ...`.
- EQP for a join: two SCAN/SEARCH lines + `USE TEMP B-TREE` if a sort.
- `EXPLAIN` (plain) shape: column names `addr opcode p1 p2 ...`.
- COMPOUND / CO-ROUTINE / TEMP B-TREE labels.

## SQLite source references
- `src/build.c`, `src/select.c` — EQP generation (`sqlite3VdbeAppendP4` EXPLAIN).
- `src/shell.c` — `.explain` output formatting (column layout).
- `internal/exec/explain.go` — frigolite's EXPLAIN.

## Steps
- [x] **G5.EXPLAIN.1** Baseline eqp package; record results. — `eqp` testgen
      passed at baseline (asserts only error-free execution, not EQP text).
- [x] **G5.EXPLAIN.2** Pre-test suite (EQP shapes). — `frigolite_p5_explain_test.go`
      (`TestP5Explain_*`: ScanSearch, TempBTree, Compound, Subqueries, CoRoutine,
      PlainColumns) — all 6 pass vs sqlite3 3.51 oracle.
- [x] **G5.EXPLAIN.3** EQP emitter: SCAN/SEARCH/USE TEMP B-TREE/COMPOUND/
      CO-ROUTINE labels matching SQLite shape. — `internal/exec/explain.go`
      rebuilt around a `planNode` tree rendered with the sqlite3 CLI prefixes
      (|-- / `--, 3-space indent); covering-index ORDER BY/GROUP BY/DISTINCT,
      COMPOUND QUERY trees, nested subquery plans, CO-ROUTINE for materialized
      FROM subqueries. Verified byte-for-byte vs sqlite3 3.51.
- [x] **G5.EXPLAIN.4** Transpiler EQP normalization — already present: tcl2go
      converts TCL `\y` word boundaries to Go `\b` (gen.go ~1537), and the
      transpiled eqp tests assert only error-free runs, so no further
      normalization is needed for the in-scope packages.
- [x] **G5.EXPLAIN.5** Triage exact-bytecode tests → N-A with evidence. —
      where2-2.5/2.5b/2.6/2.6b, whereF-7.3, distinctagg-*.1 remain in the
      tcl2go unsupported map as "EXPLAIN VDBE opcode output not implemented
      (G5.EXPLAIN)"; documented N-A in the pre-test header (frigolite is not
      a bytecode VDBE).
- [x] **G5.EXPLAIN.6** eqp green + EQP-only failures in where/selectN resolved.
      — `testgen/eqp` PASS; selectD-4.1 EQP failure (SEARCH x2 USING
      AUTOMATIC COVERING INDEX) fixed via joinNodeFor joinRef-first ordering;
      remaining where/selectD/select1/select6 failures are non-EQP engine
      bugs (numeric overflow, nested-join execution, ORDER BY/ambiguous-
      column resolution) tracked by their own phases.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/eqp/ && \
go test -run 'TestP5Explain' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "EXPLAIN QUERY PLAN emits SQLite-shape output (SCAN/SEARCH/USE TEMP B-TREE/COMPOUND/CO-ROUTINE); EXPLAIN plain emits the opcode-dump column shape; transpiler EQP normalization matches testgen regex patterns. Exact-bytecode tests documented N-A. EQP currently differs (seen in where). See portplan/TASK_G5_EXPLAIN.md." \
  completionCriterion "testgen eqp PASS and TestP5Explain pre-tests PASS; EQP-shape failures in where/selectN resolved." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/eqp/ && go test -run TestP5Explain -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G5.EXPLAIN. EQP differs from SQLite (seen in where testgen). Frigolite is NOT a bytecode VDBE —
exact opcode tests are N-A; EQP-shape tests are in scope. Emitter in internal/exec/explain.go.
Decisions: emit the shape SQLite does; tests using regex EQP patterns must match after transpiler normalization.
Next: baseline, pre-tests, EQP shapes, transpiler normalization, N-A bytecode.
Risks: EQP is asserted in many packages — fixing the emitter may flip many tests; re-run G1/G2 verify after.
Carried limits: verifyCommand above.
```
