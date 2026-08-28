# TDD Tier 3 — Functions & Expressions

> **Goal**: String functions, numeric functions, date/time functions,
> aggregate functions, window functions, CASE expressions all work.
> **Files**: ~60 files, ~2500 TODOs remaining.
> **Prerequisite**: Tier 2 complete.

## Key Files

### String Functions
| File | TODOs | Functions |
|------|-------|-----------|
| `like1` | ~5 | LIKE, GLOB |
| `like2` | ~8 | LIKE edge cases |
| `like3` | ~10 | LIKE with ESCAPE |
| `substr1` | ~3 | SUBSTR |
| `substr2` | ~5 | SUBSTR edge cases |
| `length1` | ~2 | LENGTH |
| `trim1` | ~3 | TRIM, LTRIM, RTRIM |
| `replace1` | ~2 | REPLACE |
| `instr1` | ~2 | INSTR |
| `printf` | 1193 | PRINTF (very heavy — C API test) |

### Numeric Functions
| File | TODOs | Functions |
|------|-------|-----------|
| `abs1` | ~2 | ABS |
| `round1` | ~3 | ROUND |
| `ceil1` | ~2 | CEIL, FLOOR |
| `random1` | ~2 | RANDOM |
| `total1` | ~2 | TOTAL |

### Date/Time Functions
| File | TODOs | Functions |
|------|-------|-----------|
| `date1` | ~10 | DATE, TIME, DATETIME, STRFTIME |
| `date2` | ~15 | date modifiers |
| `date3` | ~20 | date edge cases |

### Aggregate Functions
| File | TODOs | Functions |
|------|-------|-----------|
| `agg1` | ~5 | COUNT, SUM, AVG, MIN, MAX |
| `agg2` | ~8 | GROUP_CONCAT |
| `agg3` | ~10 | aggregate edge cases |
| `total1` | ~2 | TOTAL |

### Window Functions
| File | TODOs | Functions |
|------|-------|-----------|
| `window1` | ~15 | ROW_NUMBER, RANK |
| `window2` | ~20 | window frames |
| `window3` | ~30 | complex window |

### CASE
| File | TODOs | Notes |
|------|-------|-------|
| `case1` | ~3 | basic CASE |
| `case2` | ~5 | CASE with subqueries |

## Strategy

The `printf` file (1193 TODOs) is a special case — it tests `sqlite3_mprintf`
which is a C API function, not the SQL `PRINTF()` function. Most of these
TODOs come from `sqlite3_mprintf_double` and `sqlite3_mprintf_int` calls.
These are C-level tests that don't apply to frigolite.

**Approach for `printf`**: Add `sqlite3_mprintf_*` to the C API comment handler.
These are NOT SQL-level tests — they test C string formatting. A single
`t.Log("TODO: sqlite3_mprintf tests are C API — not applicable to frigolite")`
at the top of the file would handle most of them.

## Verification

```bash
go test ./testgen/like1/... -v -count=1
go test ./testgen/substr1/... -v -count=1
go test ./testgen/abs1/... -v -count=1
go test ./testgen/round1/... -v -count=1
go test ./testgen/agg1/... -v -count=1
go test ./testgen/window1/... -v -count=1
go build ./...
go test -run TestSOLID_ -count=1
```
