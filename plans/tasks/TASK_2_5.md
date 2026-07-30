# Task 2.5 — Virtual tables

> **Phase**: 2 — Full Feature Coverage
> **Status**: 🔲 Not started
> **Files**: `internal/vtab/`, `internal/exec/engine.go` (vtab dispatch)
> **SQLite ref**: `src/vtab.c`
> **Prerequisite**: Phase 1 complete
> **Estimated**: 2-3 sessions

## Description

Fix xBestIndex for cost-based plan selection, implement WITHOUT ROWID tables,
and add missing vtab modules.

## Steps

- [ ] Remove `vtab*`, `bestindex*` from skip list
- [ ] **Fix xBestIndex**: implement cost-based plan selection for vtab queries.
      Virtual table reports cost per plan; planner selects cheapest.
      Handle constraints (`col = ?`, `col IN (...)`, `col > ?`, etc.) and order-by.
- [ ] **WITHOUT ROWID tables**: `CREATE TABLE t(a, b, PRIMARY KEY(a)) WITHOUT ROWID`.
      Use the PK as the row key in b-tree (no separate rowid column).
      Clustered index: data stored in PK order. References from FKs on rowid tables.
- [ ] **Missing vtab modules**: `dbstat` (page-level DB stats), `pragma_*` tables,
      `generate_series` (improve existing), `json_each`/`json_tree` (eponymous).
- [ ] Verify: `FRIGOLITE_TEST=vtab go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** with message: `P2.5: fix virtual tables — xBestIndex, WITHOUT ROWID, modules`

## Verification

```bash
FRIGOLITE_TEST=vtab go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 120s .
```

## Session notes

- Started:
- Completed:
- xBestIndex fixes:
- WITHOUT ROWID status:
- Modules added:
- Baseline failures:
- Final failures:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
