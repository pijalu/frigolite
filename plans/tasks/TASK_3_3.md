# Task 3.3 — SOLID compliance

> **Phase**: 3 — Quality & SOLID
> **Status**: 🔲 Not started
> **Files**: All `internal/` packages
> **Prerequisite**: Phase 2 complete
> **Estimated**: 1-2 sessions

## Description

Enforce SOLID architecture: clean import boundaries, single-responsibility
packages, and full GoDoc documentation for all exported symbols.

## Steps

- [ ] Run SOLID import boundary check: `go test -run TestSOLID_ImportBoundaries -count=1 ./...`
      — no upward or circular deps between packages
- [ ] Fix any import boundary violations (move code, extract interfaces)
- [ ] Verify each package has focused scope (single responsibility)
- [ ] Check all exported symbols have GoDoc comments:
      ```bash
      grep -rn "^func \|^type \|^const \|^var " internal/ | grep -v "_test.go" | grep -v "^//"
      ```
- [ ] Add GoDoc for any undocumented symbols
- [ ] Verify: `go test -run TestSOLID_ -count=1 ./...` passes
- [ ] **Commit** with message: `P3.3: SOLID compliance — clean deps, full GoDoc`

## Verification

```bash
go test -run TestSOLID_ -count=1 ./...
# All SOLID checks pass
```

## Session notes

- Started:
- Completed:
- Boundary violations fixed:
- Packages with added GoDoc:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
