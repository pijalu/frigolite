# TASK G3.CONSTRAINTS — NOT NULL, CHECK, UNIQUE, conflict resolution

> **Phase**: G3 (schema & constraints).
> **Goal**: G3.CONSTRAINTS.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.INSERT/UPDATE; G3.INDEX (UNIQUE).
> **Current state: FAILING** — `check`, `notnull`, `conflict` fail.

## Objective
Column/table constraints match SQLite: NOT NULL (incl. NULL from a NULL-yielding
expression), CHECK (expression evaluated per row; `CHECK constraint failed:
<expr-text>` with verbatim expression text), UNIQUE (column + table +
multi-column), PRIMARY KEY (rowid alias for INTEGER PK; composite PK; WITHOUT
ROWID), conflict-resolution clauses (`ON CONFLICT` per constraint + statement
`OR ...`), and exact error text + statement-level rollback on violation.

## Scope — testgen packages
`check`, `notnull`, `conflict`, `trans`, `transitive`.
(`unique` shared with G3.INDEX — coordinate.)

## Pre-test file
`frigolite_p3_constraints_test.go` — `TestP3Constraints_*`. Cases vs oracle:
- NOT NULL: literal NULL, NULL from expression, NULL via DEFAULT.
- CHECK: verbatim expression text in error; CHECK referencing multiple columns;
  CHECK with subquery (allowed? match SQLite).
- UNIQUE: single, multi-column, NULL handling (multiple NULLs allowed in non-PK
  UNIQUE; NOT NULL + UNIQUE).
- PRIMARY KEY: INTEGER PK = rowid alias; composite PK (not rowid alias);
  `PRIMARY KEY DESC` quirks.
- Conflict clauses: `ON CONFLICT ROLLBACK/ABORT/FAIL/IGNORE/REPLACE` per
  constraint + statement `INSERT OR ...`.
- Statement rollback on failure (partial changes undone).

## SQLite source references
- `src/insert.c`, `update.c` — constraint checks, conflict resolution.
- `src/build.c` — constraint attachment, `ON CONFLICT` parsing.
- `src/resolve.c` — CHECK expression resolution.

## Steps
- [ ] **G3.CONSTRAINTS.1** Pre-test suite. Commit: `G3.CONSTRAINTS.1: constraints pre-test suite`.
- [ ] **G3.CONSTRAINTS.2** Triage check/notnull/conflict via pure-Go tests.
      Commit per fix: `G3.CONSTRAINTS.2.<n>: <fix>`.
- [ ] **G3.CONSTRAINTS.3** CHECK verbatim error text (`CHECK constraint failed:
      <expr>`). Commit: `G3.CONSTRAINTS.3: CHECK error text`.
- [ ] **G3.CONSTRAINTS.4** Conflict resolution per constraint + statement OR.
      Commit: `G3.CONSTRAINTS.4: conflict resolution`.
- [ ] **G3.CONSTRAINTS.5** UNIQUE NULL handling + composite PRIMARY KEY.
      Commit: `G3.CONSTRAINTS.5: UNIQUE/PK NULL semantics`.
- [ ] **G3.CONSTRAINTS.6** Statement-level rollback on violation (pager snapshot
      + restore). Coordinate with G1.INSERT rollback work.
      Commit: `G3.CONSTRAINTS.6: statement rollback`.
- [ ] **G3.CONSTRAINTS.7** check/notnull/conflict/trans/transitive green.
      Commit: `G3.CONSTRAINTS.7: constraints TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/check/ ./testgen/notnull/ ./testgen/conflict/ ./testgen/trans/ ./testgen/transitive/ && \
go test -run 'TestP3Constraints' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Column/table constraints match SQLite: NOT NULL (incl NULL from expressions), CHECK (verbatim expr text in error), UNIQUE (single/multi-col/NULL handling), PRIMARY KEY (rowid alias / composite / WITHOUT ROWID), conflict clauses (ON CONFLICT per constraint + statement OR ROLLBACK/ABORT/FAIL/IGNORE/REPLACE), exact error text + statement rollback. check/notnull/conflict currently FAIL. See portplan/TASK_G3_CONSTRAINTS.md." \
  completionCriterion "testgen check, notnull, conflict, trans, transitive PASS and TestP3Constraints pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/check/ ./testgen/notnull/ ./testgen/conflict/ ./testgen/trans/ ./testgen/transitive/ && go test -run TestP3Constraints -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G3.CONSTRAINTS. check/notnull/conflict FAIL. Statement rollback snapshots pagers (internal/exec/engine.go).
CHECK error text = stored expression verbatim. Conflict resolution shared with INSERT.
Decisions: ON CONFLICT per-constraint overrides statement OR.
Next: pre-tests, triage the three packages, then CHECK text + conflict resolution.
Risks: multiple constraints on same column firing order; rollback interaction with triggers.
Carried limits: verifyCommand above.
```
