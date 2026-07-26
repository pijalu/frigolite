# Frigolite — Implementation TODO

## Plan

All implementation plans are in [`.goa/plans/`](.goa/plans/).
Start with [`.goa/plans/MASTER.md`](.goa/plans/MASTER.md) for the dependency
graph and implementation order.

## Phases

| Phase | File | Scope | Tests |
|-------|------|-------|-------|
| 1 | [01-parsing-type-system.md](.goa/plans/01-parsing-type-system.md) | atof1/2, affinity2/3 | 12 |
| 2 | [02-explain-query-planner.md](.goa/plans/02-explain-query-planner.md) | analyzeE/C/7, autoindex3 | 81 |
| 3 | [03-alter-table-constraints.md](.goa/plans/03-alter-table-constraints.md) | altercons2/3 | 19 |
| 4 | [04-alter-rename-triggers.md](.goa/plans/04-alter-rename-triggers.md) | altertab3, alterlegacy, altertab2 | 78 |
| 5 | [05-alter-rename-views.md](.goa/plans/05-alter-rename-views.md) | altercorrupt, altermalloc2 | 7 |
| 6 | [06-attach-database.md](.goa/plans/06-attach-database.md) | attach3 | 20 |
| 7 | [07-pragma-schema-analysis.md](.goa/plans/07-pragma-schema-analysis.md) | autoanalyze1, analyze8/D | 14 |
| 8 | [08-virtual-tables-indexes.md](.goa/plans/08-virtual-tables-indexes.md) | amatch1, autoindex4 | 7 |
| 9 | [09-auth-transactions.md](.goa/plans/09-auth-transactions.md) | alterauth, atomic2 | 7 |
| 10 | [10-final-triage.md](.goa/plans/10-final-triage.md) | Cleanup | 0 |
| 11 | [11-fts-port.md](.goa/plans/11-fts-port.md) | FTS3/4/5 (Go-native) | ~50 files |
| 12 | [12-profiling-optimization.md](.goa/plans/12-profiling-optimization.md) | Performance optimization | all |

## Core Principle

**Use Go stdlib first.** Before writing custom data structures, check:
- `container/heap`, `container/list` — data structures
- `encoding/binary` — binary encoding
- `sort` — sorting, binary search
- `strings`, `bytes`, `unicode` — text processing
- `math`, `sync`, `compress/*` — utilities

Reference at `/Users/muaddib/dev/go` for complete Go standard library source.

## Validation

```bash
# Run a specific phase's tests
FRIGOLITE_TEST=<suite_name> go test -run 'TestSQLiteSuite$' ./

# Run all compat tests
go test -v -run 'TestSQLiteSuite$' ./

# Quick regression
go test -run 'TestOpenClose|TestExec|TestEmpty|TestParse|TestFile|TestDump' ./

# SOLID architecture
go test -run TestSOLID ./...
```
