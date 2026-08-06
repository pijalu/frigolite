# TASK G1.UPDATE — UPDATE (SET, WHERE, RETURNING)

> **Phase**: G1 (CRUD core — critical path).
> **Goal**: G1.UPDATE.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G0.GRAMMAR; G1.WHERE; G1.EXPR.
> **Current state: FAILING** (`update` testgen package fails).

## Objective
UPDATE matches SQLite: multi-column SET, SET with expressions referencing the
current row + other columns, WHERE, ORDER BY+LIMIT on UPDATE (SQLite allows
`UPDATE ... ORDER BY ... LIMIT n`), RETURNING, conflict resolution
(`UPDATE OR IGNORE/REPLACE/FAIL/ABORT/ROLLBACK`), constraint enforcement, FK
cascade updates, statement-level rollback, and updating columns used in indexes.

## Scope — testgen packages
`update`, `returning` (RETURNING is shared with INSERT/DELETE — coordinate).

## Pre-test file
`frigolite_p1_update_test.go` — `TestP1Update_*`. Cases vs oracle:
- Single + multi-column SET; `SET a=a+1` self-reference; `SET a=b, b=a`.
- Expression RHS referencing columns and functions.
- WHERE selection (relies on G1.WHERE); UPDATE all rows (no WHERE).
- UPDATE ... ORDER BY ... LIMIT n (rowid order if no ORDER BY).
- RETURNING `*`, explicit cols, expressions, WHERE on returned rows.
- `UPDATE OR IGNORE/REPLACE/FAIL/ABORT/ROLLBACK` on constraint conflict.
- NOT NULL / CHECK / UNIQUE violation → exact error + statement rollback.
- UPDATE on WITHOUT ROWID table; updating PK column.
- FK ON UPDATE CASCADE/SET NULL/RESTRICT.

## SQLite source references
- `src/update.c` — `sqlite3Update`, one-pass vs two-pass, RETURNING.
- `src/insert.c` — constraint resolution shared with INSERT.

## Steps
- [ ] **G1.UPDATE.1** Pre-test suite. Commit: `G1.UPDATE.1: UPDATE pre-test suite`.
- [ ] **G1.UPDATE.2** Diagnose current `update` testgen failure via pure-Go test
  first (triage). Likely: SET expression evaluation against the row context, or
  WHERE eval error swallowing. Fix `internal/exec/update.go`.
  Commit: `G1.UPDATE.2: fix UPDATE SET/WHERE eval`.
- [ ] **G1.UPDATE.3** Conflict clauses (OR IGNORE/REPLACE/FAIL/ABORT/ROLLBACK).
  Commit: `G1.UPDATE.3: UPDATE conflict resolution`.
- [ ] **G1.UPDATE.4** UPDATE ... ORDER BY ... LIMIT (non-standard but real).
  Commit: `G1.UPDATE.4: UPDATE ORDER BY/LIMIT`.
- [ ] **G1.UPDATE.5** RETURNING (shared path; coordinate with INSERT/DELETE).
  Commit: `G1.UPDATE.5: UPDATE RETURNING`.
- [ ] **G1.UPDATE.6** Index maintenance on updated columns; WITHOUT ROWID PK
  updates. Commit: `G1.UPDATE.6: index + WITHOUT ROWID updates`.
- [ ] **G1.UPDATE.7** FK ON UPDATE actions (coordinate with G3.FKEY).
  Commit: `G1.UPDATE.7: FK ON UPDATE`.
- [ ] **G1.UPDATE.8** testgen update + returning green. Commit: `G1.UPDATE.8: UPDATE TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/update/ ./testgen/returning/ && \
go test -run 'TestP1Update' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "UPDATE matches SQLite: multi-column SET with row-self-reference, WHERE, ORDER BY+LIMIT, RETURNING, conflict clauses (OR IGNORE/REPLACE/FAIL/ABORT/ROLLBACK), constraint enforcement with statement rollback, FK ON UPDATE, index maintenance. update testgen currently FAILS. See portplan/TASK_G1_UPDATE.md." \
  completionCriterion "testgen update, returning PASS and TestP1Update pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/update/ ./testgen/returning/ && go test -run TestP1Update -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G1.UPDATE. [done + outputs]. RETURNING path shared with INSERT/DELETE.
Conflict resolution shared with INSERT (internal/exec/insert.go + update.go).
Decisions: UPDATE ORDER BY/LIMIT supported (non-standard but SQLite-real).
Next: triage current update failure with pure-Go test, then conflict clauses.
Risks: one-pass optimization vs FK cascade ordering.
Carried limits: verifyCommand above.
```
