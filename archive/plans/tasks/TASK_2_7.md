# Task 2.7 — Corruption & edge cases

> **Phase**: 2 — Full Feature Coverage
> **Status**: 🔲 Not started
> **Files**: `internal/storage/` (page/cell validation), `internal/btree/` (B-tree integrity)
> **Prerequisite**: Phase 1 complete
> **Estimated**: 4-5 sessions

## Description

Handle corruption errors matching SQLite's exact messages (~14 corrupt* files),
fix ticket tests (~73 tkt-* files, each a specific SQLite bug fix), and handle
large data edge cases (bigfile*, bigrow*).

## Steps

- [ ] Remove `corrupt*`, `tkt*`, `bigfile*` from skip list
- [ ] **Corruption handling**: match SQLite error messages exactly:
      `"malformed database schema - malformed database encoding (ma)"` etc.
      Validate page headers, cell pointers, b-tree structure on read.
- [ ] **Ticket tests** (~73 `tkt-*` files): each tests a specific bug fix in SQLite.
      Debug each failure by reading `tkt-XXXX.test` and implementing the edge case.
- [ ] **Large data**: `bigfile*`, `bigrow*` — test large BLOBs, large row counts,
      file growth. May need pager optimization for >4KB pages.
- [ ] Verify: remaining tests pass progressively
- [ ] **Commit** with message: `P2.7: fix corruption handling, edge cases, large data tests`

## Verification

```bash
go test ./testgen/... -count=1
# Measure remaining failures
```

## Session notes

- Started:
- Completed:
- Corrupt files fixed:
- Ticket tests fixed:
- Large data fixed:
- Total failing tests before:
- Total failing tests after:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
