# TODO — the live restart index

> Details live in **[portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md](portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md)** —
> read it first. This file is the one-page index; update BOTH when checking
> items off.

## Goal

`close all gaps with sqlite — functional first, then performance.`

Current state (2026-11-05, feature/perf @ 935063d2d): S7d landed the
locking_mode=EXCLUSIVE flip (silent PRAGMA, the WAL flips at the first
write and back at the first normal-mode transaction end — the reader's
mark byte choreography) plus fixture-VFS routing to ATTACHed pagers and
the xWrite/xSync result-code mapping: **wal2 57 -> 51, e_walckpt 5 -> 4,
wal sweep 202 -> 201** (0 new throughout). S7c had landed the checkpoint
blocking choreography — the busy handler joins the WAL lock
protocol (writer lock, read marks, restart blocking, demote-to-PASSIVE on
give-up), the fixture-VFS callback dispatch drives live connections, and
the transpiler emits `db busy`. **e_walckpt 129 → 5** (4.2-4.5 are the
queued WAL→rollback header reset's downstream; 5.3.1 = IOERR injection on
an attached aux); wal sweep 306 → 214; wal3 residue = its pre-C3 set
(5, queued). Verification details + the exact remaining classes:
portplan/PLAN_e_walckpt_S7c.md §OUTCOME.

## Immediate next action

**Wal-group residue triage, post-S7c** — regenerate baselines
(/tmp baselines are gone), then: (1) wal2 exclusive-mode xShmLock traces
(wal2-6.4.5.2/6.4.8.2, unmasked by the S7c filter fix —
sqlite3WalExclusiveMode enter/exit choreography); (2) e_walckpt-4.2-4.5 +
5.3.1 (the WAL→rollback header reset's downstream — plan the
checkpoint/flush ORDER first, the naive port broke wal-24.x; and the
IOERR-on-aux injection). Fixture-regen + name-set-diff procedure in the
HANDOFF.

## Queue (in order)

1. Sidecar lifecycle (unblocks e_walckpt-4.2-4.5 + walbak-4.3.1/4.3.3):
   the WAL→rollback switch deletes -wal/-shm + resets the version bytes
   to 1/1 (C's walModeChange + BtreeSetVersion(pBt,1)). PLAN THE
   CHECKPOINT/FLUSH ORDER FIRST — the naive port broke wal-24.x.
2. Regen-drift follow-up: e_walauto/interrupt2/wal5 fixtures predate the
   sqlite3_wal_checkpoint handler — regenerate + battery (S6 procedure).
5. wal-18.2.x interrupt choreography (24).
5. Fix B: padToSectorBoundary + PSO/sector-size devchar surface
   (walvfs-5.3's 24-frame expectation; wal.c:4170-4188).
6. Census top non-WAL families: index 200, rtree 126, tkt 124, where 111,
   expr 109, pager1 101 — triage per family with the census methodology.
7. Performance phase (F1c/F3 lanes in GAP_CLOSURE_PLAN_2026-10-31.md) —
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
