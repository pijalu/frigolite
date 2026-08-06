# TASK G5.ATTACH — ATTACH / DETACH databases, schema-qualified names

> **Phase**: G5 (advanced SQL).
> **Goal**: G5.ATTACH.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.CREATE; G3 (schema operations consistent with schema prefix).
> **Current state: PARTIAL** — ATTACH currently a no-op (HANDOVER §7).

## Objective
ATTACH/DETACH match SQLite for correctness: `ATTACH DATABASE 'file' AS schema`,
`DETACH DATABASE schema`, the `main`/`temp`/attached schemas, schema-qualified
table names (`aux.t1`), cross-schema queries/joins, `pragma database_list`, the
attached-db limit (`SQLITE_MAX_ATTACHED`), and that schema operations
(FindTable/RenameEntry/RemoveEntry) consistently handle the schema prefix.

> **Scope:** frigolite already makes ATTACH/DETACH/SAVEPOINT no-ops to avoid
> cascade failures (HANDOVER §7). This task makes them *real enough* for the
> `attach` testgen package: an attached database is a second pager opened on the
> given file, queryable under its schema alias. Concurrency/locking across
> attached DBs is deferred (single-connection model is fine for these tests).

## Scope — testgen packages
`attach` (`attachmalloc` → N-A).

## Pre-test file
`frigolite_p5_attach_test.go` — `TestP5Attach_*`. Cases vs oracle:
- ATTACH a second .db file; query `aux.t1`.
- Cross-schema join `main.t1 JOIN aux.t2`.
- DETACH; error detaching main/temp/in-use.
- `pragma database_list` shows attached schemas.
- ATTACH limit reached error.
- CREATE TABLE aux.t1; INSERT/SELECT/UPDATE/DELETE on it.

## SQLite source references
- `src/attach.c` — `sqlite3AttachDatabase`, `sqlite3Detach`, schema array.
- `src/build.c` — schema-qualified name resolution.

## Steps
- [ ] **G5.ATTACH.1** Pre-test suite. Commit: `G5.ATTACH.1: attach pre-test suite`.
- [ ] **G5.ATTACH.2** Make ATTACH open a real second pager on the file, registered
      under the schema alias; DETACH closes it. Fix `internal/pager/` + engine.
      Commit: `G5.ATTACH.2: real ATTACH/DETACH`.
- [ ] **G5.ATTACH.3** Schema-qualified resolution (`aux.t1`) consistent across
      Find/Rename/Remove. Commit: `G5.ATTACH.3: schema-qualified ops`.
- [ ] **G5.ATTACH.4** Cross-schema queries/joins. Commit: `G5.ATTACH.4: cross-schema query`.
- [ ] **G5.ATTACH.5** pragma database_list + attach limit. Commit:
      `G5.ATTACH.5: database_list + limit`.
- [ ] **G5.ATTACH.6** attach green. Commit: `G5.ATTACH.6: attach TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/attach/ && \
go test -run 'TestP5Attach' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "ATTACH/DETACH match SQLite: ATTACH DATABASE 'file' AS schema opens a second pager under the alias; DETACH closes it; schema-qualified names (aux.t1); cross-schema query/join; pragma database_list; attach limit. Currently a no-op. Single-connection model (no cross-DB locking). See portplan/TASK_G5_ATTACH.md." \
  completionCriterion "testgen attach PASS and TestP5Attach pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/attach/ && go test -run TestP5Attach -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G5.ATTACH. Currently no-op. Make it open a real second pager (internal/pager/) under the schema alias.
Schema ops must handle the prefix consistently (HANDOVER §7 fixed Find/Rename/Remove).
Decisions: single-connection model; no cross-DB locking (deferred with WAL).
Next: pre-tests, real ATTACH/DETACH pager, schema-qualified ops, cross-schema query.
Risks: transaction semantics across attached DBs (commit atomicity) — match SQLite best-effort for tests.
Carried limits: verifyCommand above.
```
