# TASK G2.SETOPS — Compound queries (UNION / UNION ALL / INTERSECT / EXCEPT)

> **Phase**: G2 (query features).
> **Goal**: G2.SETOPS.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.SELECT; G2.ORDERBY (ORDER BY applies to the whole compound).
> **Current state: DONE** — unionall testgen green, TestP2Setops pre-tests pass.

## Objective
Compound SELECT matches SQLite: `UNION`, `UNION ALL`, `INTERSECT`, `EXCEPT`
(and chains like `A UNION B INTERSECT C`). Column-count must match across arms
(error otherwise). Result-column names come from the first arm. `ORDER BY` /
`LIMIT` apply to the whole compound, not a single arm. UNION/INTERSECT/EXCEPT
dedupe with the same rules as DISTINCT (NULLs equal). Operator precedence
(left-to-right for set ops; parentheses not supported by SQLite — verify).

## Scope — testgen packages
`unionall`, plus compound-query cases inside select2–selectG (coordinate with
G1.SELECT). `unionallfault`, `unionvtab` are fault/vtab → triage.

## Pre-test file
`frigolite_p2_setops_test.go` — `TestP2Setops_*`. Cases vs oracle:
- UNION (dedup) vs UNION ALL (no dedup); INTERSECT; EXCEPT.
- Column-count mismatch error.
- Result names from first arm; ORDER BY/LIMIT over whole.
- NULL dedup in UNION/INTERSECT/EXCEPT.
- Chain of 3+ arms; mixed operators.
- UNION of different-affinity columns (coercion).

## SQLite source references
- `src/select.c` — `multiSelect`, compound query materialization + dedup.
- `src/expr.c` — comparison affinity for dedup.

## Steps
- [x] **G2.SETOPS.1** Pre-test suite. Commit: `G2.SETOPS.1: setops pre-test suite`.
- [x] **G2.SETOPS.2** Triage `unionall` failure via pure-Go test. Likely result
  merge / column-count check / dedup. Fix `internal/exec/select.go`.
  Commit: `G2.SETOPS.2: compound query merge + dedup`.
- [x] **G2.SETOPS.3** INTERSECT / EXCEPT semantics. Commit: `G2.SETOPS.3: INTERSECT/EXCEPT`.
- [x] **G2.SETOPS.4** Column-count check + result names from first arm.
  Commit: `G2.SETOPS.4: compound arity + naming`.
- [x] **G2.SETOPS.5** ORDER BY/LIMIT over the whole compound. Commit:
  `G2.SETOPS.5: compound ORDER BY/LIMIT`.
- [x] **G2.SETOPS.6** testgen unionall green (+ compound cases in selectN).
  Commit: `G2.SETOPS.6: setops TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/unionall/ && \
go test -run 'TestP2Setops' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Compound SELECT matches SQLite: UNION/UNION ALL/INTERSECT/EXCEPT, chains, column-count check, result names from first arm, NULL dedup (DISTINCT rules), ORDER BY/LIMIT over whole compound, cross-affinity coercion. unionall currently FAILS. See portplan/TASK_G2_SETOPS.md." \
  completionCriterion "testgen unionall PASS, compound cases in select2-selectG still green, and TestP2Setops pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/unionall/ && go test -run TestP2Setops -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G2.SETOPS. unionall FAILS. Compound merge in internal/exec/select.go (multiSelect-equivalent).
UNION/INTERSECT/EXCEPT dedup like DISTINCT (NULLs equal). Names from first arm.
Decisions: ORDER BY/LIMIT apply to whole compound, not an arm.
Next: pre-tests, triage unionall, then INTERSECT/EXCEPT + arity/naming.
Risks: large compound arms may be slow without materialization optimization.
Carried limits: verifyCommand above.
```
