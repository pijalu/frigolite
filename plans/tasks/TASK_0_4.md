# Task 0.4 — Phase out JSON harness

> **Phase**: 0 — TCL to Go Test Pipeline
> **Status**: 🔲 Not started
> **Files**: `frigolite_harness_test.go`, `testdata/*.json`, `Makefile`
> **Prerequisite**: Task 0.3 (tcl2go produces ≥300 PASS)
> **Estimated**: 1 session

## Description

Once tcl2go covers all 1002 files with no regressions, deprecate the old JSON
harness. Move it to archive, update build targets.

## Steps

- [ ] Verify tcl2go covers all 1002 files with no regressions from harness baseline
- [ ] Move `frigolite_harness_test.go` to `plans/archive/`
- [ ] Archive `testdata/*.json` to `testdata/archive/`
- [ ] Update `Makefile` build targets (remove JSON harness dependencies)
- [ ] Remove old hand-written compat tests (`frigolite_agg_test.go`, etc.) that duplicate tcl2go coverage
- [ ] Verify: `go test ./testgen/... -count=1` runs all 1002 files
- [ ] **Commit** with message: `P0.4: phase out JSON harness, tcl2go is sole test approach`

## Verification

```bash
go test ./testgen/... -count=1
# All files run, no harness dependency
```

## Session notes

- Started:
- Completed:
- Files archived:
- Regressions found (if any):

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
