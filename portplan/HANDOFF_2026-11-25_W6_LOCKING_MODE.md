# HANDOFF — W6 locking_mode schema-qualified reporting (2026-11-25)

## State

COMPLETE. `TestW6_LockingMode` passes all assertions; the 7 original wrong
normal/exclusive combinations are fixed by one query-branch change.

## Root cause

`internal/exec/pragma_state.go:LockingMode` handled value-less invocations with
`e.currentLockingMode("")` unconditionally. In C (`../sqlite/src/pragma.c:687-724`,
`PragTyp_LOCKING_MODE`) only the `pId2->n==0 && eMode==QUERY` case substitutes
`db->dfltLockMode`; every schema-qualified query calls
`sqlite3PagerLockingMode(pPager, QUERY)`. Our helper `schemaLockingMode` already
models that pager mode (TEMP pinned `exclusive`, per-schema map for MAIN and
attached databases, connection default fallback for databases attached after the
last bare set), so the fix was to pass `s.Schema` on the query branch.

## Changes

- `internal/exec/pragma_state.go`: value-less branch returns
  `e.currentLockingMode(schema)`; comment notes the C distinction.
- `frigolite_w6_kernel_pin_test.go`: 1.6b/1.7b pins — an invalid value on a
  QUALIFIED pragma is a query of that schema (C's `getLockingMode` maps junk to
  `PAGER_LOCKINGMODE_QUERY`). Oracle sqlite3 3.54: after a default-normal set,
  `pragma main.locking_mode=invalid; pragma temp.locking_mode=invalid;` ->
  `normal`, `exclusive`; after a bare exclusive set both -> `exclusive`.
- `portplan/PLAN_2026-11-25_W6_LOCKING_MODE.md` — pre-change plan.
- `TODO.md` item 4 -> DONE with evidence.

## Verification

- `go test -run '^TestW6_LockingMode$' -count=1 .`: PASS (previous output had
  7 `t.Errorf` lines at 144/158/165/180/188/195/202).
- `go test -run '^TestW6_' -count=1 .`: PASS (all 10 W6 native pins).
- `go test -run TestSOLID_ ./...`: PASS.
- `go test ./internal/...`: PASS.
- `go vet ./internal/exec/...`: clean.
- `bash tools/quality_gate.sh internal/exec/pragma_state.go`: only pre-existing
  file-level debt (`lockingModeSetAll/One` complexity, 1166-line file) plus
  repo-wide staticcheck findings; `LockingMode` itself introduces none.
- Full harness, 12 shards (`tools/run_json_shards.sh` on a freshly compiled
  test binary, `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/frigolite-shards.o9fAJc`):
  leaf FAIL-name set vs `/tmp/t30_full_gate.log` — **NEW=0**, GONE=6
  (`resetdb-100`, `resetdb-300`, `temptable2` family-level,
  `temptable2-4.1.4`, `temptable2-5.1.2`, `vacuum-into-500`).
- First full run (`.../frigolite-shards.AFFEP2`) showed 15 NEW
  `temptable2-9.2.*` leaves. A focused `FRIGOLITE_TEST=temptable2 go test
  -run '^TestSQLiteSuite$' -count=1 .` exited 0 with zero leaves, and the second
  full run reproduced NEW=0. Per existing lessons these are the unseeded
  `temptable2` randomblob/index-shape flakes, not a regression.

## Residual risk

None for this contract. Repo-wide quality-gate debt stays open (TODO item 10).
