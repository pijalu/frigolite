# PLAN-PF0-AGGREGATE.md — Aggregate Function Fixes (COMPLETE)

## Status: ✅ COMPLETE

All aggregate tests pass:
- 7 manual aggregate tests ✅
- aggnested ✅ (0 FAIL)
- aggorderby ✅ (0 FAIL)
- aggfault ✅ (0 FAIL)

## What Was Fixed

1. **Aggregates returning struct objects** — Fixed `Final()` path so all aggregator structs resolve through their `Final()` method instead of being returned raw
2. **GROUP_CONCAT with ORDER BY** — Fixed separator handling; correct value ordering before `Step()`
3. **Aggregate empty-set handling** — SUM→NULL, AVG→NULL, MIN/MAX→NULL, COUNT→0, TOTAL→0.0
4. **Non-aggregate column evaluation** — Proper row source for bare columns in aggregate queries
5. **Nested aggregate detection** — `findNestedAggregate()` detects illegal nested aggregates and returns error

## Key Changes

- `engine.go`: `evalAggFuncCall`, `evalAggregates`, `evalAggregatesEmpty`, `findNestedAggregate`
- `engine.go`: float formatting with `formatSQLiteValue()` for whole-number floats

## Verification

```bash
go test -v -run "TestAggregate" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/aggnested" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/aggorderby" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```
