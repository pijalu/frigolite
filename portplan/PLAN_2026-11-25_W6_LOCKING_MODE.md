# PLAN — W6 locking_mode schema-qualified reporting (2026-11-25)

## Evidence

- `TestW6_LockingMode` fails 7 assertions: `frigolite_w6_kernel_pin_test.go:144,158,165,180,188,195,202`.
- Pure-Go probe (`/tmp/lmprobe`, `frigolite.Open/Query`) reproduces against the engine:
  `PRAGMA main.locking_mode=exclusive` records `settings.lockingModes[MAIN]=exclusive`
  but the following `PRAGMA main.locking_mode` still reports `normal`; `PRAGMA
  temp.locking_mode` also reports `normal` at open.
- sqlite3 3.54 oracle: `PRAGMA temp.locking_mode` -> `exclusive`; a
  schema-qualified query reports THAT schema's mode; only the bare query reports
  `db->dfltLockMode`.
- C source: `../sqlite/src/pragma.c:687-724` (`PragTyp_LOCKING_MODE`): the
  `pId2->n==0 && eMode==QUERY` branch is the ONLY case that substitutes
  `db->dfltLockMode`; all other query cases call
  `sqlite3PagerLockingMode(pPager, QUERY)`.

## Root cause

`internal/exec/pragma_state.go:LockingMode` returns
`e.currentLockingMode("")` for every value-less invocation, ignoring
`s.Schema`. The schema-qualified query therefore reads the connection default
instead of the addressed database's pager mode.

## Fix

1. In `LockingMode`'s query branch, pass `s.Schema` to `currentLockingMode` so
   a schema-qualified query resolves through `schemaLockingMode` (TEMP pinned
   exclusive, per-schema map for MAIN/attached, default fallback for a database
   attached after the last bare set). The bare query keeps passing `""`.
2. Update the function comment to state the query-branch distinction explicitly.
3. Add native assertions for qualified invalid-token queries (C's
   `getLockingMode` maps junk to QUERY, so a qualified query must report the
   schema mode), without relaxing any existing assertion.
4. Remove the temporary internal/exec probe test.

## Verification

- `go test -run '^TestW6_LockingMode$' -count=1 .`
- `go test -run TestSOLID_ ./...`
- `go test ./internal/...` and `go test -run '^TestSQLiteSuite$/exclusive' .` (focused harness family).
- W6 harness family failure-name set vs `/tmp/t30_full_gate.log` (`NEW=0`).
- `tools/quality_gate.sh internal/exec/pragma_state.go`.
- Commit with TODO handoff update.

## Resolution (2026-11-25)

All steps landed. `TestW6_LockingMode`, all `TestW6_*` pins, SOLID and
`go test ./internal/...` pass. Full 12-shard harness leaf-name diff vs
`/tmp/t30_full_gate.log`: NEW=0, GONE=6 documented flakes. The first full run's
15 `temptable2-9.2.*` leaves vanished in both a focused rerun (exit 0) and the
second full run — the documented unseeded-randomblob flake. Details:
`portplan/HANDOFF_2026-11-25_W6_LOCKING_MODE.md`.
