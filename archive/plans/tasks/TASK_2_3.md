# Task 2.3 — ATTACH / DETACH

> **Phase**: 2 — Full Feature Coverage
> **Status**: 🔲 Not started
> **Files**: `internal/exec/engine.go` (multi-database dispatch), `internal/pager/`, `internal/schema/`
> **SQLite ref**: `src/attach.c`
> **Prerequisite**: Phase 1 complete
> **Estimated**: 4-5 sessions (deep architectural change)

## Description

Implement multi-database support for ATTACH/DETACH. Change Engine from
single schema+pager to an array of Database structs. Fix encoding checks,
schema-prefix dispatch, and cross-database queries.

## Steps

- [ ] **Multi-database engine**: change `Engine` from single `schema`+`pager` to
      `[]Database` where each Database has a `Name`, `*schema.Manager`, `*pager.Pager`.
      Index 0 = main, 1 = temp, 2+ = attached.
- [ ] **Fix encoding check false positives**: 47 false "encoding mismatch" errors.
      When attaching `:memory:`, use same encoding as main database (UTF-8).
- [ ] **Implement ATTACH**: Parse `ATTACH 'file' AS name`. Validate name (not main/temp).
      Open file/create in-memory. Load schema. Add to databases[].
- [ ] **Implement DETACH**: Find by name. Reject main/temp. Close pager. Remove.
- [ ] **Schema-prefix dispatch**: Update ALL table/view/trigger/index lookup functions
      to support `schema.name` prefix:
      - findTable, findView, findTrigger, findIndex
      - execSelect, execInsert, execUpdate, execDelete
      - PRAGMA handlers
- [ ] **sqlite_master per database**: `SELECT * FROM aux.sqlite_master` returns tables
      from the `aux` database. Without prefix, search `main` first.
- [ ] **Cross-database queries**: `SELECT * FROM main.t1, aux.t2 WHERE ...` — resolve
      each table to its database, execute JOIN across pagers.
- [ ] Verify: `FRIGOLITE_TEST=attach go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** with message: `P2.3: implement ATTACH/DETACH — multi-database dispatch`

## Verification

```bash
FRIGOLITE_TEST=attach go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 120s .
```

## Session notes

- Started:
- Completed:
- Architecture changes:
- Baseline failures:
- Final failures:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
