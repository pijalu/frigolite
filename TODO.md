# TODO — the live restart index

> Details live in **[portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md](portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md)** —
> read it first. This file is the one-page index; update BOTH when checking
> items off.

## Goal

`close all gaps with sqlite — functional first, then performance.`

Current state (2026-11-04, feature/perf @ 8499ebbd2): the residue census
([portplan/RESIDUE_CENSUS_2026-11-04.md](portplan/RESIDUE_CENSUS_2026-11-04.md))
puts the functional backlog at **4,162 isolated subtests across 254
families**; the WAL slice (S5–S7b, 9 commits) landed walvfs 12→8, wal3
112→5, e_walckpt 139→129.

## Immediate next action

**e_walckpt (129)** — the mode-sweep choreography + callback counters.
Triage state, failing-case shapes, C source anchors, and the fixture-regen
procedure are in HANDOFF §"THE IMMEDIATE RESTART POINT". Verify by
failure-name-set diff against regenerated baselines (procedure in HANDOFF).

## Queue (in order)

1. e_walckpt remainder: mode-sweep choreography (1.full/restart/truncate.x
   no-such-table/COMMIT — checkpoint-busy vs a second connection) + the
   busy-handler/xWrite/sync callback counters (1.passive.1.6-8, 1.3.2.2).
2. Regen-drift follow-up: e_walauto/interrupt2/wal5 fixtures predate the
   sqlite3_wal_checkpoint handler — regenerate + battery (S6 procedure).
3. wal2 (61) / wal3 (5) xShmLock trace residue — post-regen triage.
4. wal-18.2.x interrupt choreography (24).
5. Fix B: padToSectorBoundary + PSO/sector-size devchar surface
   (walvfs-5.3's 24-frame expectation; wal.c:4170-4188).
6. Sidecar lifecycle: the WAL→rollback switch deletes -wal/-shm + resets
   the version bytes to 1/1 (unblocks walbak-4.3.1/4.3.3; plan the
   checkpoint/flush ORDER first — the naive port broke wal-24.x).
7. Census top non-WAL families: index 200, rtree 126, tkt 124, where 111,
   expr 109, pager1 101 — triage per family with the census methodology.
8. Performance phase (F1c/F3 lanes in GAP_CLOSURE_PLAN_2026-10-31.md) —
   after functional gaps close.

## Working rules (violations cost hours — see .agents/lessons_learned.md)

- Verify by **failure-name-set diff** (`comm` both directions), never counts
  (counts swing ±35 under load; pure HEAD reproduces it).
- **Never** build baselines via stash roundtrips — commit each verified unit
  immediately; use a worktree for HEAD baselines.
- `go build ./...` does NOT compile _test.go files — binary-verify
  instrumented builds (`go test -c -o` + `strings | grep <trace-tag>`).
- `FRIGOLITE_TEST` matches by SUBSTRING — "wal" sweeps the whole wal* group.
- A fixture family with got [""] runtime-var mismatches + cascading
  "file is not a database" reopens = STALE FIXTURE: regenerate first.
- Oracle: `DYLD_LIBRARY_PATH=/tmp/tclshim ../sqlite/testfixture` (rebuild the
  shim per HANDOFF); ../sqlite is 3.51.0, /usr/bin/sqlite3 is 3.54 — tag
  every observation.
- Plans before change, lessons after every unit (plan/GUIDELINES.md §1b).
