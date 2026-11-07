# TODO — the live restart index

> Details live in **[portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md](portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md)** —
> read it first. This file is the one-page index; update BOTH when checking
> items off.

## Goal

`close all gaps with sqlite — functional first, then performance.`

Current state (2026-11-07, feature/perf @ 351936d39): S8 landed the
checkpoint copyback interrupt check (wal.c:2289-2292 — the per-page
flag check aborts the backfill, frames stay standing) + the regen-drift
battery: interrupt2 UNSKIPPED and 36/37 (the whole 1.x interrupt-during-
checkpoint block green; residue 2.1 = the per-row callback that
interleaves a checkpoint mid-scan needs a row-callback surface), the
snapshot aliases (db_save/db_restore ≡ faultsim) landed, e_walauto
carded unsupported (-shm byte introspection), wal5 carded
(lock_common primitives). wal sweep 136 (baseline S8). Earlier: S7g
landed the sqlite3_backup command object (walbak 9 → 0) and bc722d011
the harness refs=2 leak (wal sweep 156 → 147). S7d landed the
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

**Continue the queue below item 3** — the regen-drift battery (item 3)
is DONE (S8: interrupt2 unblocked 36/37, e_walauto/wal5 carded; residue
interrupt2-2.1 needs an interleaved row-callback step surface). Next:
the census families (item 6) or the wal2/wal3 xShmLock trace residue
(item 4 in HANDOFF §Queued). Baselines:
portplan/baseline_wal_sweep_2026-11-07_S8.txt (136 names).

## Queue (in order)

1. Sidecar lifecycle COMPLETE (pieces 1/2/3: 1ab97052c, b72123592,
   5d9f97c3f) — wal-24.2 passes, walbak-4.3.1/4.3.3 pass, e_walckpt 0.
   The root cause of the first attempt's cascade: a PATH BUG (w.path IS
   the -wal path; the first attempt removed "t.db-wal-wal" — a no-op).
   Residue RESOLVED: wal-12.4 (the refs=2 leak — bc722d011 closes the
   replaced connection in reopen_db/open_named_db).
2. Backup unit COMPLETE: 47e9165de (the backup_file step + the
   destination-mode parity) + the S7g sqlite3_backup command object
   (backup_db step; PLAN_S7g_backupdb.md): **walbak 0 failures** (was 23).
   The converter now supports both backup constructs; other dropped-step
   fixtures (backup*.test, pagerfault.test, multiplex3.test) can be
   regenerated in their own units.
3. Regen-drift follow-up DONE (S8, 351936d39): interrupt2 unskipped +
   36/37 (the copyback interrupt check, wal.c:2289-2292); e_walauto
   carded unsupported (-shm byte introspection); wal5 carded
   (lock_common primitives). Residue: interrupt2-2.1 (the per-row
   callback interleaving a checkpoint — needs a row-callback surface).
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
