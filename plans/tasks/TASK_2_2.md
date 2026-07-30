# Task 2.2 — ALTER TABLE

> **Phase**: 2 — Full Feature Coverage
> **Status**: 🔲 Not started
> **Files**: `internal/rename/` (existing), `internal/exec/engine.go` (ALTER execution)
> **SQLite ref**: `src/alter.c`
> **Prerequisite**: Phase 1 complete
> **Estimated**: 2 sessions

## Description

Implement full ALTER TABLE support: token-level RENAME, ADD COLUMN, DROP
COLUMN with proper SQL rewriting for triggers, views, and indexes.

## Steps

- [ ] Remove `altercorrupt`, `altertab2`, `altertab3` from skip list
- [ ] Run tests: `FRIGOLITE_TEST=alter go test -run "^TestSQLiteSuite$" .` — capture baseline failures
- [ ] **Fix token-level RENAME**: use `internal/rename` package for robust identifier
      replacement in stored SQL (trigger bodies, view definitions, index SQL).
- [ ] **Fix ADD COLUMN**: support `ALTER TABLE t ADD COLUMN col type DEFAULT expr`.
      - Default must be constant expression. NOT NULL requires default.
      - Foreign key references allowed on ADD COLUMN.
- [ ] **Fix DROP COLUMN**: `ALTER TABLE t DROP COLUMN col`.
      - Reject if column is referenced by triggers, views, or FKs.
      - Rewrite stored CREATE TABLE SQL to omit the column.
- [ ] Verify: `FRIGOLITE_TEST=alter go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** with message: `P2.2: fix ALTER TABLE — RENAME, ADD COLUMN, DROP COLUMN`

## Verification

```bash
FRIGOLITE_TEST=alter go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 120s .
```

## Session notes

- Started:
- Completed:
- Features implemented:
- Baseline failures:
- Final failures:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
