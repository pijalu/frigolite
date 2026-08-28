# Frigolite — TDD Master Plan

> **Status**: Phase 0 complete (tcl2go transpiler works, 1190 files generated).
> **Remaining**: 14,551 TODO markers across 757 files need to be addressed.
> **Approach**: TDD — make tests pass, one feature at a time, by priority.
> **Test command**: `go test ./testgen/... -count=1`

## Priority Tiers

Tests are ordered by functional impact. Each tier must be green before
moving to the next. Within a tier, files with fewer TODOs are fixed first
(low-hanging fruit) to build confidence.

| Tier | Area | Files | TODOs | Impact |
|------|------|-------|-------|--------|
| 1 | **Core SQL** — CREATE, INSERT, SELECT, DELETE, UPDATE, WHERE, expressions, types | ~50 files | ~800 | Highest — basic DML/DDL must work |
| 2 | **SQL Features** — JOINs, subqueries, ORDER BY, GROUP BY, DISTINCT, UNION, indexes, views, triggers | ~80 files | ~2000 | High — most query patterns |
| 3 | **Functions** — string, numeric, date/time, aggregate, window, CASE | ~60 files | ~2500 | Medium — expressions in queries |
| 4 | **Schema** — ALTER TABLE, constraints, defaults, generated columns | ~40 files | ~500 | Medium — schema changes |
| 5 | **Advanced** — FTS, virtual tables, ATTACH, ANALYZE, VACUUM, PRAGMA | ~100 files | ~4000 | Low — SQLite-specific extensions |
| 6 | **Edge Cases** — corruption, concurrency, C API tests, shell, file format | ~400 files | ~4700 | Lowest — SQLite infrastructure tests |

## TDD Workflow Per Task

```
1. Pick a test file → run it → capture failures
2. Analyze first failure pattern → find root cause in engine
3. Fix the engine — smallest change that resolves the failure
4. Re-run the test file → verify fix + no regressions
5. Check SOLID: go test -run TestSOLID_ -count=1
6. Commit with P<phase>.<task>: <description>
7. Update progress in task file
```

## Key Facts

- **14,551 TODO markers** remain. Each TODO is a `t.Errorf("TODO: ...")`
  call that causes a test failure when the corresponding TCL command has
  no frigolite equivalent.
- **158 files have only 1 TODO** — these are the easiest wins.
- **20 files have 100+ TODOs** — these are complex infrastructure tests
  (where7: 2020, printf: 1193, fts3corrupt4: 577, expr: 475, date: 422).
- The transpiler handles ALL TCL structures (loops, variables, conditionals,
  list/string operations). TODOs represent missing **engine features**,
  not missing transpiler features.

## How To Work

```bash
# Generate latest test files
go run ./tools/tcl2go/

# Run a single test file
go test ./testgen/select1/... -v -count=1

# Run all generated tests
go test ./testgen/... -count=1

# Check architecture rules
go test -run TestSOLID_ -count=1

# Quick build check
go build ./...
```

## Initiation

To start Tier 1:
```bash
go run ./tools/tcl2go/              # generate all tests
go test ./testgen/... -count=1      # see current state
go test ./testgen/select1/... -v    # focus on first file
```
