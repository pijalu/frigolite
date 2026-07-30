# TDD Tier 2 — SQL Features

> **Goal**: JOINs, subqueries, ORDER BY, LIMIT/OFFSET, GROUP BY/HAVING,
> DISTINCT, UNION, indexes, views, triggers, transactions all pass.
> **Files**: ~80 files, ~2000 TODOs remaining.
> **Prerequisite**: Tier 1 complete.

## File Order

### Quick wins (1-5 TODOs)
| File | TODOs | Feature |
|------|-------|---------|
| `trigger1` | ~2 | basic triggers |
| `trigger2` | ~3 | trigger edge cases |
| `view1` | ~2 | basic views |
| `view2` | ~3 | view edge cases |
| `index1` | ~3 | basic indexes |
| `index2` | ~4 | index edge cases |
| `join1` | ~2 | basic JOIN |
| `join2` | ~3 | JOIN edge cases |
| `subquery1` | ~2 | basic subqueries |
| `union1` | ~2 | basic UNION |
| `distinct1` | ~2 | basic DISTINCT |
| `orderby1` | ~3 | basic ORDER BY |
| `limit1` | ~2 | basic LIMIT |
| `transaction1` | ~2 | basic transactions |

### Medium (6-20 TODOs)
| File | TODOs | Feature |
|------|-------|---------|
| `trigger3` | ~8 | trigger edge cases |
| `trigger4` | ~10 | complex triggers |
| `index3` | ~8 | composite indexes |
| `index4` | ~10 | index usage |
| `join3` | ~8 | LEFT JOIN |
| `join4` | ~10 | complex JOIN |
| `subquery2` | ~8 | EXISTS, IN |
| `union2` | ~6 | UNION ALL |
| `orderby2` | ~8 | ORDER BY edge cases |
| `groupby1` | ~8 | GROUP BY |
| `groupby2` | ~10 | HAVING |
| `transaction2` | ~6 | savepoints |

### Heavy (21+ TODOs)
| File | TODOs | Feature |
|------|-------|---------|
| `join5` | ~25 | complex JOIN patterns |
| `orderby3` | ~20 | ORDER BY with functions |
| `groupby3` | ~30 | GROUP BY edge cases |

## TDD Workflow

Same as Tier 1. One file at a time. Fix the engine, verify, commit.

```bash
# Run a specific feature file
go test ./testgen/join1/... -v -count=1

# Run all Tier 2 files
for f in trigger view index join subquery union distinct orderby limit transaction groupby; do
  go test ./testgen/$f*/... -count=1 2>&1 | grep -E "PASS|FAIL|TODO"
done
```

## Verification

```bash
# End of Tier 2 — must pass at minimum:
go test ./testgen/join1/... -v -count=1
go test ./testgen/trigger1/... -v -count=1
go test ./testgen/view1/... -v -count=1
go test ./testgen/index1/... -v -count=1
go test ./testgen/subquery1/... -v -count=1
go test ./testgen/union1/... -v -count=1
go test ./testgen/distinct1/... -v -count=1
go build ./...
go test -run TestSOLID_ -count=1
```
