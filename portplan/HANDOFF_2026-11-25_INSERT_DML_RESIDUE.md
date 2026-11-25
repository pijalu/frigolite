# HANDOFF — insert DML parity residue (2026-11-25)

## State

COMPLETE. Both remaining result-visible `insert` leaves are fixed:
`insert-17.15` reports the same UNIQUE column as sqlite3 3.54 and
`insert-5.3` sees `OpenEphemeral` in its EXPLAIN stream. Focused family gate,
native pins, full 12-shard harness, SOLID, and internal packages are green.

## Root cause and fix

- `insert-17.15`: `buildTriggerRowMap` passed trigger OLD/NEW values through
  `wrapValueForRowMap`, which already saw `*util.ColumnValue`/
  `*collatedValue` values and wrapped them again. The nested affinity layer
  made `t3bpi`'s partial predicate `c<=d` evaluate `3<=2` as TRUE, producing
  a spurious REPLACE cascade and the IPK-alias error `t3.a`. The fix keeps an
  existing wrapper and wraps only raw scalars
  (`internal/execdml/insert_select.go`).
- `insert-5.3`: the EXPLAIN renderer emitted the driving SELECT plan and the
  write cursors but not the temp-table spool C generates for `INSERT ...
  SELECT` reading its own destination (`../sqlite/src/insert.c:1151-1189`).
  `emitInsertRows` now emits one `OpenEphemeral` row when a source table
  matches the target, after the plan rows and before the write opens
  (`internal/execquery/explain_rows.go`). This is display-only; DML execution
  is untouched.
- The fixture had used `resultAll`, but the TCL body is `set x [execsql
  {...}]`; testfixture's `execsql` is `$db eval`, whose result is a flat list
  of all columns. `testdata/insert.json` now uses `resultFlat` so `lsearch $x
  OpenEphemeral` probes individual values, as in the original test. The
  assertion is unchanged, not skipped or weakened.

## Evidence

- Oracle `/usr/bin/sqlite3` 3.54: the 17.15 trigger sequence errors with
  `UNIQUE constraint failed: t3.d`; `EXPLAIN INSERT INTO t4 SELECT x+2 FROM
  t4` has `OpenEphemeral 1 1 0 ... nColumn=1` at addr 9.
- Native pins: `go test -run 'TestInsert17_15UniqueColumnName|TestInsert53ExplainOpenEphemeral' -count=1 .` — PASS.
- Focused gate: `FRIGOLITE_TEST=insert go test -run '^TestSQLiteSuite$' -count=1 .` — PASS.
- Verify command: `FRIGOLITE_TEST=insert go test -run '^TestSQLiteSuite$' -count=1 . && go test -run TestSOLID_ ./...` — PASS.
- `go test ./internal/... -count=1` — PASS; `go vet` on touched packages clean.
- `bash tools/quality_gate.sh internal/execdml/insert_select.go internal/execquery/explain_rows.go`:
  only the baseline repo-wide staticcheck findings, the pre-existing
  `checkLoadedTableRefCtx` gocognit 16, and soft file-size warnings; no new
  finding from either changed function.
- Full harness: freshly compiled binary, 12 shards via
  `tools/run_json_shards.sh`, all 12 status files present at
  `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/frigolite-shards.78Os0U`.
  Leaf-name set vs `/tmp/t30_full_gate.log`: **NEW=0**, GONE=7
  (`insert-17.15`, `insert-5.3`, `resetdb-100`, `resetdb-300`,
  `temptable2-4.1.4`, `temptable2-5.1.2`, `vacuum-into-500`). Shard 11 spent
  about 10 minutes in the known-slow `vacuummem/1.0` family at high CPU and
  then completed; no shard was killed.
- Pre-change plan: `portplan/PLAN_2026-11-25_INSERT_DML_RESIDUE.md`.

## Residual risk

None for this contract. The wrapping guard has one caller (trigger row maps)
and leaves raw-scalar wrapping unchanged. The EXPLAIN addition cannot change
query or DML results. Repo-wide quality-gate debt remains open in TODO.md.
