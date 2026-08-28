# Task 3.1 — Complexity gate

> **Phase**: 3 — Quality & SOLID
> **Status**: 🔲 Not started
> **Files**: `Makefile` (quality targets), all `internal/` packages (refactoring)
> **Prerequisite**: Phase 2 complete (all 1002 files green)
> **Estimated**: 2-3 sessions

## Description

Lower the cognitive and cyclomatic complexity gates to 15. Refactor each
offender by splitting functions, extracting helpers, and using guard clauses.

## Steps

- [ ] Set gocognit threshold to 15 in Makefile (currently 90)
- [ ] Set gocyclo threshold to 15 in Makefile (currently 40)
- [ ] Run: `make quality` — capture all offenders
- [ ] For each offender (highest complexity first):
  - [ ] Review function — understand what it does
  - [ ] Split into smaller functions (<15 gocognit)
  - [ ] Extract helper functions for repeated logic
  - [ ] Use guard clauses to reduce nesting
  - [ ] Verify: tests still pass after each refactor
- [ ] Verify: `make quality` passes at threshold 15
- [ ] **Commit** with message: `P3.1: lower complexity gate to 15, refactor offenders`

## Verification

```bash
make quality
# Must pass with gocognit ≤15, gocyclo ≤15
```

## Session notes

- Started:
- Completed:
- Offenders found:
- Offenders refactored:
- Remaining blockers:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
