# Task 1.2 — Fix missing setup SQL (no-such-table)

> **Phase**: 1 — Fix Engine Bugs
> **Status**: 🔲 Not started
> **Files**: `tools/tclconvert/tcl/interp.go`
> **Estimated**: 1-2 sessions

## Description

Fix remaining TCL patterns that the interpreter doesn't capture, causing
"no such table" errors in generated tests (~2000+ occurrences).

## Steps

- [ ] Run failure baseline: capture all "no such table" errors
      ```bash
      go test -run "^TestSQLiteSuite$" -count=1 . 2>&1 | grep "no such table" | sort | uniq -c | sort -rn
      ```
- [ ] For each TCL pattern producing errors:
  - [ ] `db eval { SQL } SCRIPT` — capture SQL, ignore per-row callback script
  - [ ] `db onecolumn { SQL }` — capture as query step
  - [ ] `db transaction { ... }` — execute body commands, capture their SQL
  - [ ] Multi-connection: `sqlite3 db2 test.db; db2 eval { SQL }` — note as multi-conn
  - [ ] `for`/`foreach` loops: execute loop body, unroll INSERT patterns
- [ ] Regenerate: `go run ./tools/tcl2go/`
- [ ] Verify: no-such-table errors drop by ≥80%
- [ ] **Commit** with message: `P1.2: fix TCL interp for missed SQL patterns`

## Verification

```bash
go run ./tools/tcl2go/
go test ./testgen/... -count=1 2>&1 | grep -c "no such table"
# Compare count before vs after fix
```

## Session notes

- Started:
- Completed:
- TCL patterns fixed:
- No-such-table count before:
- No-such-table count after:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
