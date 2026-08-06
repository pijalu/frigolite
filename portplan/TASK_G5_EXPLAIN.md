# TASK G5.EXPLAIN — EXPLAIN / EXPLAIN QUERY PLAN

> **Phase**: G5 (advanced SQL).
> **Goal**: G5.EXPLAIN.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1+G2 (all query features produce a plan).
> **Current state: FAILING** — EXPLAIN QUERY PLAN output differs (seen in `where`).

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
- [ ] **G5.EXPLAIN.1** Baseline eqp package; record results. Commit:
      `G5.EXPLAIN.1: explain baseline`.
- [ ] **G5.EXPLAIN.2** Pre-test suite (EQP shapes). Commit: `G5.EXPLAIN.2: explain pre-test`.
- [ ] **G5.EXPLAIN.3** EQP emitter: SCAN/SEARCH/USE TEMP B-TREE/COMPOUND/
      CO-ROUTINE labels matching SQLite shape. Commit: `G5.EXPLAIN.3: EQP shapes`.
- [ ] **G5.EXPLAIN.4** Transpiler EQP normalization: match testgen regex patterns
      (e.g. collapse autoindex names). Commit: `G5.EXPLAIN.4: EQP normalization`.
- [ ] **G5.EXPLAIN.5** Triage exact-bytecode tests → N-A with evidence.
      Commit: `G5.EXPLAIN.5: explain bytecode N-A`.
- [ ] **G5.EXPLAIN.6** eqp green + EQP-only failures in where/selectN resolved.
      Commit: `G5.EXPLAIN.6: explain TCL green`.

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
