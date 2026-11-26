# Handoff 2026-11-26 — core TEXT numeric-prefix coercion (goal crimson.nyala)

## Fix

SQLite `abs`/`round` call `sqlite3_value_double` unconditionally (func.c
`absFunc` default arm, `roundFunc`); `vdbemem.c memRealValue` runs
`sqlite3AtoF` over TEXT bytes, consuming the longest numeric prefix:
`abs('31abc')`=31.0, `abs(' 31 ')`=31.0, `abs('31e')`=31.0 (dangling
exponent ignored), `abs('abc')`=0.0, `abs(char(0)||'31')`=0.0.

`internal/function/function_numeric.go`:
- `sqliteValueDouble` — shared by abs/round — now maps both `[]byte` and
  `string` through `value.RealValuePrefix`, the shared sqlite3AtoF scanner;
  INTEGER/REAL still use `toFloat64`. This is the general core-double path,
  not query-specific code.
- `fnPOW` switched from `toFloat64` to the `numericArg` gate. SQLite `pow` is
  a `math2Func`: both arguments gate on `sqlite3_value_numeric_type`, so
  non-numeric TEXT/BLOB return NULL. The new TEXT-prefix pin exposed
  `pow('31abc',2)` incorrectly returning 0; this corrects that gated path.

Untouched (still correct): `exp`/`sign`/`radians` (`numericArg`),
`mod`/`log` (`toFloat64`, whose whole-string parsing is correct for their
oracle semantics here), math extensions. `toFloat64` itself is unchanged.

## Pin

`frigolite_corefunc_text_test.go` `TestCoreFuncTextPrefix`, written BEFORE
the fix and observed RED. Covers prefix, leading/trailing whitespace, sign,
exponent, dangling exponent, embedded NUL, non-numeric/empty, overflow to
+Inf, previously-passing int/real/whole-numeric shapes, and gated-function
NULLs (`exp`/`sign`/`pow`). All expected values from `/usr/bin/sqlite3` 3.54.

## Gates

- `go test -count=1 ./internal/function/... ./internal/execquery/...` PASS.
- `TestCoreFunc.*|TestSumBlobCoercion|TestAggFeed.*|TestScanAggTypedFeed|
  TestW5.*|TestFunc.*|TestMath.*` PASS.
- Full 12-shard harness `bash tools/run_json_shards.sh /tmp/nyala_testbin 12`
  → `/tmp/nyala_shards.out`, run dir `/var/folders/.../frigolite-shards.WaPfl3`;
  leaf FAIL set 1016 vs t30 1028, NEW=0, GONE=12 (`resetdb-300` flake).
- `go test -run TestSOLID_ ./...`; cross-engine matrix (verify command).

## Commit plan

1. `fix(func): use sqlite3AtoF prefix for abs/round TEXT, gate pow` (engine + pin).
2. docs: TODO.md, `.agents/lessons_learned.md`, this handoff.
