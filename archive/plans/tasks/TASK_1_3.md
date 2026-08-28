# Task 1.3 — Implement missing SQL functions

> **Phase**: 1 — Fix Engine Bugs
> **Status**: 🔲 Not started
> **Files**: `internal/function/` (existing + new `json.go`)
> **SQLite ref**: `src/func.c`, `ext/misc/json.c`, `src/date.c`
> **Estimated**: 3-4 sessions (complex)

## Description

Eliminate all "unknown function" errors (~300 occurrences) by implementing
missing JSON functions, fixing date/time functions, and adding other misc
functions.

## Steps

- [ ] **Audit**: Extract all "unknown function" errors across all generated tests.
      Build complete list: function name, file it's used in, count.
- [ ] **JSON functions** (new `internal/function/json.go`):
  - [ ] `json_valid(X)` — returns 1 if X is valid JSON, 0 otherwise
  - [ ] `json_type(X)` — returns the type of the root value
  - [ ] `json_type(X,P)` — returns the type at path P
  - [ ] `json_extract(X,P1,P2,...)` — extracts values at paths
  - [ ] `json_remove(X,P1,P2,...)` — removes values at paths
  - [ ] `json_quote(X)` — quotes a value as JSON
  - [ ] `json_array_length(X)` — returns array length
  - [ ] `json_array_length(X,P)` — returns array length at path
  - [ ] `json_array_insert(X,P,V)` — inserts into array at path
  - [ ] `json_patch(X,P)` — applies JSON patch
  - [ ] `json_each(X)` — table-valued function
  - [ ] `json_tree(X)` — table-valued function
  - [ ] Reference: `/Users/muaddib/dev/sqlite/ext/misc/json.c`
  - [ ] Verify: `go test ./testgen/json* -count=1` — zero unknown function errors
- [ ] **Date/time functions** (fix `internal/function/datetime.go`):
  - [ ] `strftime(format, timestring, mod...)`
  - [ ] `date(timestring, mod...)`, `time(timestring, mod...)`
  - [ ] `datetime(timestring, mod...)`, `julianday(timestring, mod...)`
  - [ ] Handle modifiers: `+N days`, `start of month`, `localtime`, `utc`
  - [ ] Reference: `/Users/muaddib/dev/sqlite/src/date.c`
  - [ ] Verify: `go test ./testgen/date* ./testgen/strftime* -count=1`
- [ ] **Other missing functions:**
  - [ ] `changes()` — number of rows changed by last statement
  - [ ] `total_changes()` — total rows changed since connection opened
  - [ ] `octet_length(X)` — byte length of string
  - [ ] `intreal(X)` — convert integer to real
  - [ ] `sqlite_compileoption_used(X)` — compile option check
- [ ] **Verify**: zero "unknown function" errors across all generated tests.
- [ ] **Commit** with message: `P1.3: implement missing SQL functions — JSON, date, misc`

## Verification

```bash
go test ./testgen/... -count=1 2>&1 | grep -c "unknown function"
# Should be 0 after completion
```

## Session notes

- Started:
- Completed:
- Functions implemented:
- Unknown function count before:
- Unknown function count after:
- Reference files consulted:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
