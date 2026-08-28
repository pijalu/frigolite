# Task 2.6 — Query planner & ANALYZE

> **Phase**: 2 — Full Feature Coverage
> **Status**: 🔲 Not started
> **Files**: `internal/exec/plan.go` (new), `internal/exec/engine.go` (index selection)
> **SQLite ref**: `src/analyze.c`, `src/where.c`
> **Prerequisite**: Phase 1 complete
> **Estimated**: 3 sessions

## Description

Implement ANALYZE to gather statistics, cost-based index selection, EXPLAIN
QUERY PLAN output, and auto-index for joins.

## Steps

- [ ] Remove `analyze*`, `eqp*` from skip list
- [ ] **Implement ANALYZE**: `ANALYZE` / `ANALYZE table` / `ANALYZE schema.table`.
      Create `sqlite_stat1` table storing `(tbl, idx, stat)` where `stat` is
      `"N K1 K2 ..."` format (N = rows, K1 = distinct prefix[1], etc.).
      Populate by scanning indexes, counting distinct prefix lengths.
- [ ] **Cost-based index selection**: Use `sqlite_stat1` to estimate row count for
      each query plan, select cheapest. Fallback to full scan when no stats.
      Estimate: `estRows = ceil(N / Ki)` for equality, `N/3` for range, etc.
- [ ] **EXPLAIN QUERY PLAN**: output `SEARCH table USING INDEX idx` / `SCAN table`.
      Format: `id parent detail` hierarchy (like SQLite's tab-indented output).
- [ ] **Auto-index for joins**: When joining two tables without usable index,
      automatically build a transient index on the join key.
- [ ] Verify: `FRIGOLITE_TEST=analyze go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** with message: `P2.6: implement query planner — ANALYZE, cost-based, EQP, auto-index`

## Verification

```bash
FRIGOLITE_TEST=analyze go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 120s .
```

## Session notes

- Started:
- Completed:
- ANALYZE status:
- Index selection status:
- EQP output status:
- Baseline failures:
- Final failures:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
