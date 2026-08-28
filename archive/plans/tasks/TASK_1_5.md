# Task 1.5 — Fix constraint enforcement

> **Phase**: 1 — Fix Engine Bugs
> **Status**: 🔲 Not started
> **Files**: `internal/exec/engine.go` (INSERT/UPDATE execution)
> **SQLite ref**: `src/insert.c`, `src/vdbe.c` (constraint check opcodes)
> **Estimated**: 2 sessions

## Description

Fix UNIQUE, NOT NULL, CHECK, FOREIGN KEY, and PRIMARY KEY constraint
enforcement. Match SQLite error messages exactly. Handle multi-column
UNIQUE with NULL correctly.

## Steps

- [ ] **UNIQUE constraint error messages**: Match SQLite exactly —
      `"UNIQUE constraint failed: table.column"` not just `"UNIQUE constraint failed"`.
- [ ] **Multi-column UNIQUE**: `CREATE TABLE t(a,b,UNIQUE(a,b))` — enforce pairs.
      NULL handling: `NULL != NULL` in UNIQUE (two rows with NULL in column a are allowed).
- [ ] **NOT NULL constraint**: Error on INSERT of NULL to `col NOT NULL`.
      Error message: `"NOT NULL constraint failed: table.column"`.
- [ ] **CHECK constraint**: `CREATE TABLE t(a CHECK(a > 0))` — error on violation.
      Error message: `"CHECK constraint failed: table"`.
- [ ] **FOREIGN KEY**: `REFERENCES parent(col)` — enforce referential integrity.
      Default action: RESTRICT. Support ON DELETE/UPDATE CASCADE|SET NULL|SET DEFAULT.
- [ ] **PRIMARY KEY**: Synthesize UNIQUE index for PK columns. Auto-generate rowid.
- [ ] Verify: all constraint tests pass.
- [ ] **Commit** with message: `P1.5: fix constraint enforcement — UNIQUE, NOT NULL, CHECK, FK`

## Verification

```bash
go test ./testgen/conflict* ./testgen/unique* ./testgen/notnull* ./testgen/check* ./testgen/fkey* -count=1
# All should pass
```

## Session notes

- Started:
- Completed:
- Constraints fixed:
- Failing tests before:
- Failing tests after:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
