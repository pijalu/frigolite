# Task 3.2 — Static analysis

> **Phase**: 3 — Quality & SOLID
> **Status**: 🔲 Not started
> **Files**: All packages
> **Prerequisite**: Phase 2 complete
> **Estimated**: 1 session

## Description

Run staticcheck on all packages and fix all warnings (unused code, incorrect
error handling, style issues, etc.).

## Steps

- [ ] Run `staticcheck ./...` on all packages
- [ ] Capture all warnings to a file
- [ ] Fix each warning:
  - [ ] Unused code (variables, functions, imports)
  - [ ] Incorrect error handling (unchecked errors, error strings)
  - [ ] Style issues (naming, formatting)
  - [ ] Other staticcheck categories
- [ ] Verify: `staticcheck ./...` clean, zero warnings
- [ ] **Commit** with message: `P3.2: staticcheck clean — fix all warnings`

## Verification

```bash
staticcheck ./... 2>&1
# Output should be empty (zero warnings)
```

## Session notes

- Started:
- Completed:
- Warnings found:
- Warnings fixed:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
