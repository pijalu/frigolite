# Task 0.5 — Set new baseline

> **Phase**: 0 — TCL to Go Test Pipeline
> **Status**: 🔲 Not started
> **Files**: `plans/PLAN.md` (this file)
> **Prerequisite**: Task 0.4 (JSON harness phased out)
> **Estimated**: 15 min

## Description

Record the first tcl2go baseline metrics. Update PLAN.md with current PASS
counts and commit.

## Steps

- [ ] Record file PASS from `go test ./testgen/... -count=1`
- [ ] Record sub-test PASS/FAIL counts
- [ ] Update this document's metrics table
- [ ] **Commit** with message: `P0.5: set new tcl2go baseline`

## Verification

```bash
go test ./testgen/... -count=1 2>&1 | grep -E "PASS:|FAIL:" | sort
```

## Session notes

- Started:
- Completed:
- File PASS:
- Sub-test PASS:
- Sub-test FAIL:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
