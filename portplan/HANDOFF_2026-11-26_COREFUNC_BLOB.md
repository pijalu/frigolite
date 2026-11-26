# Handoff 2026-11-26 — core-function BLOB coercion (goal sparky.orca)

## Fix

SQLite `func.c absFunc` (default arm) and `roundFunc` call
`sqlite3_value_double` directly on a non-NULL argument. `vdbemem.c`
`sqlite3VdbeRealValue`/`memRealValue` runs `sqlite3AtoF` on TEXT/BLOB bytes,
so BLOB numeric text becomes REAL: `abs(x'31')`=1.0, `abs(x'312e35')`=1.5,
`round(x'312e35')`=2.0, embedded NUL/non-numeric/empty=0.0.

Frigolite's `toFloat64` had no BLOB case, so abs returned 0.0 and round
returned the raw blob. General fix:
- `internal/function/function_numeric.go`: `sqliteValueDouble(v)` unwraps
  `*util.ColumnValue`, maps `[]byte` through the shared
  `value.RealValuePrefix` scanner, and delegates all other types to the
  existing `toFloat64`. Used ONLY by functions whose SQLite source calls
  `sqlite3_value_double` unconditionally (abs/round).

Scope guard: `sqlite3_value_numeric_type`-gated functions must stay NULL on
BLOB. Oracle 3.54: `exp/sign/radians/pow/mod/log(x'31')` all NULL — Frigolite
already matches; the pin asserts this explicitly. Math extensions and
`sign`/`radians` (`numericArg`), `pow`/`mod`/`log` (`toFloat64`) untouched.

## Pin

`frigolite_corefunc_blob_test.go` `TestCoreFuncBlobCoercion`, written BEFORE
the fix and observed RED: scalar abs/round cases (numeric prefix, fraction,
sign, garbage, embedded NUL, empty, precision arg), a BLOB-column WHERE case,
and NULL expectations for the numeric_type-gated functions. All expected
values verified against `/usr/bin/sqlite3` 3.54.0.

## Separate gap found (queued goal, NOT fixed here)

TEXT numeric-prefix conversion for abs/round: SQLite `abs('31abc')`=31.0,
`round('31abc')`=31.0 (sqlite3AtoF longest prefix); Frigolite's `toFloat64`
parses the whole string with strconv and yields 0.0 / raw text. Scheduled as
goal `core TEXT numeric-prefix coercion` (queued). The helper doc explicitly
states TEXT delegation is a separate gap to prevent accidental scope creep.

## Gates

- `go test -count=1 ./internal/function/... ./internal/execquery/...` PASS.
- `TestCoreFuncBlobCoercion|TestSumBlobCoercion|TestAggFeed.*|
  TestScanAggTypedFeed|TestW5.*|TestFunc.*` PASS.
- Full 12-shard harness `bash tools/run_json_shards.sh /tmp/orca_testbin 12`
  → `/tmp/orca_shards.out`, run dir `/var/folders/.../frigolite-shards.uaHedn`;
  leaf FAIL set 1016 vs t30 1028, NEW=0, GONE=12. The single difference from
  the previous run's GONE=13 is the documented `resetdb-300` flake.
- `go test -run TestSOLID_ ./...`; cross-engine matrix (verify command).

## Commit plan

1. `fix(func): coerce BLOB in abs/round via shared sqlite double helper`
   (engine + pin).
2. docs: TODO.md, `.agents/lessons_learned.md`, this handoff.
