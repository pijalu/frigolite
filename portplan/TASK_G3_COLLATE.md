# TASK G3.COLLATE — COLLATE (BINARY/NOCASE/RTRIM, custom, in expressions)

> **Phase**: G3 (schema & constraints).
> **Goal**: G3.COLLATE.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.WHERE; G1.TYPES (affinity).
> **Current state: FAILING** — `collate` fails.

## Objective
Collation matches SQLite: BINARY (default, byte compare), NOCASE (case-insensitive
ASCII), RTRIM (ignore trailing spaces), column-level COLLATE declaration,
COLLATE in expressions (overrides column collation), COLLATE in ORDER BY / index
definitions, the collation precedence rules (expr `a COLLATE X = b COLLATE Y` →
X wins if explicit), and `SELECT ... = ... COLLATE NOCASE` in comparisons. Also
`LIKE` is always case-insensitive for ASCII regardless of column collation (but
NOCASE collation makes `=` case-insensitive).

## Scope — testgen packages
`collate`, `collateA`, `collateB`.

## Pre-test file
`frigolite_p3_collate_test.go` — `TestP3Collate_*`. Cases vs oracle:
- BINARY vs NOCASE vs RTRIM comparison results.
- Column-level COLLATE; COLLATE in ORDER BY; COLLATE in index.
- `a = b COLLATE NOCASE` overriding column BINARY.
- Precedence: explicit COLLATE wins over column; leftmost explicit wins.
- RTRIM: `'a' = 'a  '` (trailing spaces) true.
- LIKE always ASCII-case-insensitive; `=` with NOCASE column case-insensitive.

## SQLite source references
- `src/func.c` — `binCollFunc`, `nocaseCollatingFunc`, `rtrimCollFunction`.
- `src/expr.c` — collation assignment + precedence (`sqlite3ExprCollSeq`).
- `src/vdbe.c` — comparison opcodes honoring collation.

## Steps
- [ ] **G3.COLLATE.1** Pre-test suite. Commit: `G3.COLLATE.1: collate pre-test suite`.
- [ ] **G3.COLLATE.2** Triage `collate` failure via pure-Go test. Likely collation
      precedence or RTRIM. Fix `internal/value/` (comparison) + `internal/exec/`.
      Commit: `G3.COLLATE.2: collation precedence + RTRIM`.
- [ ] **G3.COLLATE.3** NOCASE for ASCII (not Unicode — match SQLite default).
      Commit: `G3.COLLATE.3: NOCASE ASCII`.
- [ ] **G3.COLLATE.4** COLLATE in ORDER BY + index definitions. Commit:
      `G3.COLLATE.4: COLLATE in ORDER BY/index`.
- [ ] **G3.COLLATE.5** collate/collateA/collateB green. Commit: `G3.COLLATE.5: collate TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/collate/ ./testgen/collateA/ ./testgen/collateB/ && \
go test -run 'TestP3Collate' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Collation matches SQLite: BINARY/NOCASE/RTRIM, column-level COLLATE, COLLATE in expressions/ORDER BY/index, collation precedence (explicit > column; leftmost explicit wins), LIKE always ASCII-case-insensitive, = case-insensitive under NOCASE. collate currently FAILS. See portplan/TASK_G3_COLLATE.md." \
  completionCriterion "testgen collate, collateA, collateB PASS and TestP3Collate pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/collate/ ./testgen/collateA/ ./testgen/collateB/ && go test -run TestP3Collate -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G3.COLLATE. collate FAILS. Comparison in internal/value/ (honors collation); precedence in
internal/exec/expression.go. BINARY default (byte); NOCASE ASCII-only; RTRIM trailing-space-insensitive.
Decisions: LIKE is always ASCII-case-insensitive regardless of collation.
Next: pre-tests, triage collate, then precedence + RTRIM + NOCASE.
Risks: collation precedence in compound expressions is subtle; coordinate with G1.WHERE.
Carried limits: verifyCommand above.
```
