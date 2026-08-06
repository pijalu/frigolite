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
- [x] **G1.UPDATE.1** Pre-test suite (`frigolite_p1_update_test.go` — SET, WHERE,
  ORDER BY/LIMIT, RETURNING, conflicts, constraints, WITHOUT ROWID, FK ON UPDATE,
  index maintenance; all PASS vs oracle).
- [x] **G1.UPDATE.2** Triage + fixes in `internal/exec/update.go`:
  NOT NULL/CHECK enforcement (checkUpdateConstraints), one-pass UNIQUE conflict
  semantics (checkUpdateConflicts), lazy FTS table re-init after reopen
  (ensureFTSForTable in findTable), shared parseVTabSQL helper.
- [x] **G1.UPDATE.3** Conflict clauses (OR IGNORE/REPLACE/FAIL/ABORT/ROLLBACK)
  verified against oracle; OR IGNORE multi-row + OR REPLACE covered in pre-tests.
- [x] **G1.UPDATE.4** UPDATE ... ORDER BY ... LIMIT (SQLite extension): parser
  rewrite in `internal/parse/parser.go` (top-level token scan strips ORDER
  BY/LIMIT, re-attached to UpdateStmt) + exec sort/limit in
  collectUpdateChanges; ORDER BY requires LIMIT (SQLite behavior).
- [x] **G1.UPDATE.5** RETURNING (shared path; coordinate with INSERT/DELETE):
  `testgen/returning` green — fixed fts5 INSERT RETURNING after reopen
  (lazy FTS) and reset_db nullvalue (tcl2go transpiler emits
  `tcl_nullvalue = "{}"` after reset_db to match per-connection semantics).
- [x] **G1.UPDATE.6** Index maintenance on updated columns (updated column
  reflected in index; UNIQUE index conflict errors); WITHOUT ROWID PK updates
  (verified in pre-tests + probe).
- [x] **G1.UPDATE.7** FK ON UPDATE actions (CASCADE/SET NULL/RESTRICT) verified
  with PRAGMA foreign_keys=ON in pre-tests.
- [x] **G1.UPDATE.8** testgen update + returning green; full verify command
  passes (see below).

## Current state: DONE
Verify output:
```
$ go test -tags testgen -count=1 ./testgen/update/ ./testgen/returning/
ok  github.com/pijalu/frigolite/testgen/update
ok  github.com/pijalu/frigolite/testgen/returning
$ go test -run TestP1Update -count=1 .
ok  github.com/pijalu/frigolite
$ go build ./...
BUILD OK
```
No new root-package failures (baseline: TestDoubleCreateTable + TestDropTable
pre-existing). testgen conflict/notnull/upfrom/trigger failures are also
pre-existing (verified via git stash baseline).

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
