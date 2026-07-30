# Task 0.2 — Run tcl2go across all input files

> **Phase**: 0 — TCL to Go Test Pipeline
> **Status**: 🔲 Not started
> **Files**: `tools/tcl2go/main.go`, `tools/tcl2go/gen.go`, `testgen/`
> **Estimated**: 30 min

## Description

Run the tcl2go generator across all 1002 TCL test files to produce Go test
files. Verify output count and no interpreter panics.

## Steps

- [ ] Run `go run ./tools/tcl2go/` — processes all 1002 `.test` files
- [ ] Count generated files: `ls testgen/*/*_test.go | wc -l` — expect ≥500
- [ ] Verify no TCL interpreter panics or timeouts for any file
- [ ] **Commit** with message: `P0.2: generate all 1002 test files via tcl2go`

## Verification

```bash
go run ./tools/tcl2go/
ls testgen/*/*_test.go | wc -l     # expect ≥500
go build ./testgen/...
```

## Session notes

- Started:
- Completed:
- Generated count:
- TCL interp errors (files with panics/timeouts):

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
