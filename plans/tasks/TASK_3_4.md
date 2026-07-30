# Task 3.4 — Remove all skip-list entries

> **Phase**: 3 — Quality & SOLID
> **Status**: 🔲 Not started
> **Files**: `frigolite_harness_test.go` (skip list), `Makefile`
> **Prerequisite**: Phase 2 complete, 1002/1002 green
> **Estimated**: 15 min

## Description

Remove all remaining entries from unsupportedTestFiles and slowTestFiles.
Final verification: 1002/1002 file PASS with zero skips.

## Steps

- [ ] Remove remaining entries from `unsupportedTestFiles` in `frigolite_harness_test.go`
- [ ] Remove remaining entries from `slowTestFiles`
- [ ] Verify: `go test ./testgen/... -count=1` — 1002/1002 file PASS
- [ ] **Commit** with message: `P3.4: zero skip-list entries, 1002/1002 green`

## Verification

```bash
go test ./testgen/... -count=1
# All 1002 files pass
```

## Session notes

- Started:
- Completed:
- Entries removed:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
