# TASK G1.DELETE — DELETE (WHERE, RETURNING, triggers)

> **Phase**: G1 (CRUD core — critical path).
> **Goal**: G1.DELETE.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G0.GRAMMAR; G1.WHERE.
> **Current state: PASSING** (`delete_` passes) — verify and extend.

## Objective
DELETE matches SQLite: WHERE selection, `DELETE FROM t` (all rows), ORDER BY +
LIMIT, RETURNING, qualified column references in WHERE during the scan, DELETE
triggers (BEFORE/AFTER DELETE for OLD row), FK ON DELETE CASCADE/SET NULL/SET
DEFAULT/RESTRICT, and statement-level rollback. Trigger firing belongs mostly to
G3.TRIGGER; this task covers the DELETE scan + RETURNING + the OLD-row context.

## Scope — testgen packages
`delete_`, `delete2`, `delete3`, `delete4`, `delete_pkg`.

## Pre-test file
`frigolite_p1_delete_test.go` — `TestP1Delete_*`. Cases vs oracle:
- DELETE all rows; DELETE with WHERE; DELETE with WHERE referencing qualified
  columns (`DELETE FROM t WHERE t.x = 1`).
- DELETE ... ORDER BY ... LIMIT n (rowid order default).
- RETURNING `*` / explicit cols.
- DELETE triggers: capture OLD.* values (coordinate with G3.TRIGGER).
- FK ON DELETE actions (coordinate with G3.FKEY).
- Statement rollback on trigger/constraint error mid-delete.

## SQLite source references
- `src/delete.c` — `sqlite3Delete`, rowid scan, RETURNING, trigger integration.
- `src/update.c` — RETURNING shared machinery.

## Steps
- [ ] **G1.DELETE.1** Pre-test suite. Commit: `G1.DELETE.1: DELETE pre-test suite`.
- [ ] **G1.DELETE.2** Qualified-column WHERE during DELETE scan
  (`e.currentScanTable` set so `t6.x` resolves). Commit: `G1.DELETE.2: DELETE qualified WHERE`.
- [ ] **G1.DELETE.3** DELETE ... ORDER BY ... LIMIT. Commit: `G1.DELETE.3: DELETE ORDER BY/LIMIT`.
- [ ] **G1.DELETE.4** RETURNING (shared path). Commit: `G1.DELETE.4: DELETE RETURNING`.
- [ ] **G1.DELETE.5** OLD-row context for DELETE triggers (coordinate G3.TRIGGER).
  Commit: `G1.DELETE.5: DELETE trigger OLD context`.
- [ ] **G1.DELETE.6** testgen delete_–delete4 green. Commit: `G1.DELETE.6: DELETE TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/delete_/ ./testgen/delete2/ ./testgen/delete3/ ./testgen/delete4/ ./testgen/delete_pkg/ && \
go test -run 'TestP1Delete' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "DELETE matches SQLite: WHERE (incl. qualified columns during scan), all-rows, ORDER BY+LIMIT, RETURNING, OLD-row trigger context, FK ON DELETE actions, statement rollback. delete_ currently PASSES — extend to delete2-4. See portplan/TASK_G1_DELETE.md." \
  completionCriterion "testgen delete_, delete2, delete3, delete4 PASS and TestP1Delete pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/delete_/ ./testgen/delete2/ ./testgen/delete3/ ./testgen/delete4/ ./testgen/delete_pkg/ && go test -run TestP1Delete -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G1.DELETE. delete_ already PASSING. RETURNING path shared with INSERT/UPDATE.
Decisions: DELETE ORDER BY/LIMIT supported; OLD context set during trigger scan.
Next: pre-tests, then qualified WHERE + RETURNING, then delete2-4.
Risks: trigger + FK cascade interaction (coordinate G3.TRIGGER/G3.FKEY).
Carried limits: verifyCommand above.
```
