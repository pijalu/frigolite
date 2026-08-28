# Task 0.3 — Fix common generator patterns

> **Phase**: 0 — TCL to Go Test Pipeline
> **Status**: 🔲 Not started
> **Files**: `tools/tcl2go/gen.go`, `tools/tclconvert/tcl/interp.go`
> **Estimated**: 2-3 sessions (complex)

## Description

Run the generated tests, categorize failures, and fix the most common patterns
in the generator and TCL interpreter. Iterate until ≥300 file-level PASS.

## Steps

- [ ] Run `go test ./testgen/... -count=1`, capture all failures to a file
- [ ] Categorize by pattern: result mismatch, missing SQL, syntax in SQL, etc.
- [ ] Fix `gen.go` for top-3 failure patterns:
  - [ ] Multi-statement SQL splitting (`splitSQL` edge cases)
  - [ ] Expected value formatting (braced vs quoted, TCL list flattening)
  - [ ] C API test detection (skip files with `sqlite3_prepare`, `do_malloc_test`, etc.)
- [ ] Fix TCL interpreter for missed SQL patterns:
  - [ ] `db transaction { ... }` blocks
  - [ ] Multi-connection tests (`sqlite3 db2 test.db`)
  - [ ] `db eval { SQL } SCRIPT` (per-row callback form — capture SQL, skip script)
- [ ] Iterate until `go test ./testgen/... -count=1` produces ≥300 file PASS
- [ ] **Commit** with message: `P0.3: fix tcl2go generator patterns, fix TCL interp gaps`

## Verification

```bash
go test ./testgen/... -count=1 2>&1 | grep -E "PASS:|FAIL:" | wc -l
# Measure file-level PASS count
```

## Session notes

- Started:
- Completed:
- Failure patterns found:
- Generator fixes applied:
- TCL interp fixes applied:
- Final PASS count:
- Blockers encountered:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
