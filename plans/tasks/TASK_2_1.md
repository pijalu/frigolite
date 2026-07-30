# Task 2.1 — Window functions

> **Phase**: 2 — Full Feature Coverage
> **Status**: 🔲 Not started
> **Files**: `internal/exec/window.go` (new), `internal/sql/parser.go` (OVER clause), `internal/function/` (window funcs)
> **SQLite ref**: `src/window.c`
> **Prerequisite**: Phase 1 complete (engine bugs fixed)
> **Estimated**: 3-4 sessions

## Description

Implement window function support: partition/sort/frame pipeline, built-in
window functions (ROW_NUMBER, RANK, LAG, LEAD, etc.), and aggregate
functions over windows.

## Steps

- [ ] Remove `window*` from `unsupportedTestFiles` in harness
- [ ] Run tests: `FRIGOLITE_TEST=window go test -run "^TestSQLiteSuite$" .` — capture baseline failures
- [ ] **Window pipeline** (partition/sort/frame/evaluate):
  - [ ] Partition input rows by `PARTITION BY` expressions
  - [ ] Sort each partition by `ORDER BY` expressions
  - [ ] Apply frame specification (`ROWS`/`RANGE`/`GROUPS` between)
  - [ ] Evaluate window function for each row in frame
- [ ] **Built-in window functions:**
  - [ ] `ROW_NUMBER()` — sequential row number within partition
  - [ ] `RANK()` — row number with gaps for ties
  - [ ] `DENSE_RANK()` — row number without gaps for ties
  - [ ] `LAG(expr, offset, default)` — previous row value
  - [ ] `LEAD(expr, offset, default)` — next row value
  - [ ] `FIRST_VALUE(expr)` — first value in window frame
  - [ ] `LAST_VALUE(expr)` — last value in window frame
  - [ ] `NTH_VALUE(expr, N)` — Nth value in window frame
- [ ] **Aggregate functions over windows:**
  - [ ] `SUM(expr) OVER (...)`, `AVG(expr) OVER (...)`, `COUNT(*) OVER (...)`
  - [ ] `MIN(expr) OVER (...)`, `MAX(expr) OVER (...)`, `TOTAL(expr) OVER (...)`
- [ ] Verify: `FRIGOLITE_TEST=window go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** with message: `P2.1: implement window functions — ROW_NUMBER, RANK, frame`

## Verification

```bash
FRIGOLITE_TEST=window go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 120s .
```

## Session notes

- Started:
- Completed:
- Pipeline implemented:
- Functions implemented:
- Baseline failures:
- Final failures:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
