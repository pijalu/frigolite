# Handoff 2026-11-26 — SUM(BLOB) coercion (goal fair.swan)

## Fix

SQLite `func.c sumStep` calls `sqlite3_value_numeric_type` on every input.
For BLOB that type stays BLOB, so sumStep takes its non-integer approx branch
and calls `sqlite3_value_double`. `vdbemem.c sqlite3VdbeRealValue` →
`memRealValue` runs `sqlite3AtoF` over the blob BYTES (longest numeric prefix,
no INTEGER classification): `sum(x'31')` = 1.0 REAL, `sum(x'312e35')` = 1.5,
`sum(x'4142')` = 0.0.

Frigolite's `sumNumericArg` previously handled only TEXT, leaving BLOB as an
opaque value that `stepApprox` accumulated as 0.0. General fix in
`internal/function/function_aggregate.go`: unwrap `*util.ColumnValue`, then
for `[]byte` return `float64(value.RealValuePrefix(string(b)))` — the shared
`internal/value` sqlite3AtoF scanner already used by comparison/CAST. No
query- or table-specific branching; AVG and TOTAL inherit the fix because
they delegate to `sumAgg.Step`.

## Pin

`frigolite_sum_blob_test.go` `TestSumBlobCoercion` (pure Go, written BEFORE
the fix and observed RED): scalar BLOB literals (numeric prefix, fractional,
sign, exponent, embedded NUL, empty, non-numeric), AVG/TOTAL/COUNT over a
BLOB literal, and a BLOB column with 10 rows accumulating 3004.5. All expected
values verified against `/usr/bin/sqlite3` 3.54.0.

Stale pin corrected: `frigolite_aggfeed_test.go` `TestAggFeedValueDynamics`
expected `SUM(b)=0` for a BLOB column containing `x'31'`, `x'32332e35'` (`23.5`)
and `x'2d31'` (`-1`); oracle 3.54 gives 23.5. The old expectation encoded the
bug, not a behavior contract — corrected to 23.5.

## Separate gap found (NOT fixed here)

`abs(x'31')` and `round(x'31')` also read BLOB bytes as numeric in SQLite
(`func.c absFunc`/`roundFunc` → `sqlite3_value_double`), while Frigolite's
`numericArg` returns false for BLOB. That is a core-function coercion path,
not the SUM family; needs its own unit. Math-extension functions (`exp`,
`sin`, …) are different: `math1Func` returns NULL for BLOB, matching
Frigolite.

## Gates (run by completing session)

- `go test -count=1 ./internal/function/... ./internal/execquery/...
  ./internal/util/... ./internal/value/...` PASS.
- `TestSumBlobCoercion|TestAggFeed.*|TestScanAggTypedFeed|TestAgg.*` PASS
  after stale-pin correction.
- Full 12-shard harness `bash tools/run_json_shards.sh /tmp/swan_testbin 12`
  → `/tmp/swan_shards.out`; leaf FAIL set NEW=0 vs `/tmp/t30_full_gate.log`.
- `go test -run TestSOLID_ ./...`; cross-engine matrix (verify command).

## Commit plan

1. `fix(func): coerce BLOB inputs to REAL in SUM family` (engine + pin + pin correction).
2. docs: TODO.md, `.agents/lessons_learned.md`, this handoff.
