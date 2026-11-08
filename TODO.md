# TODO — the live restart index

> Details live in **[portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md](portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md)** —
> read it first. This file is the one-page index; update BOTH when checking
> items off.

## Goal

`close all gaps with sqlite — functional first, then performance.`

Current state (2026-11-08, feature/perf @ S12): the wal group's residue
is down to the callback/byte-manipulation classes and the census's
index-sweep lost its two biggest families. Landed this session:
- **S9** (73bee149a): the wal-18.2 page-size-validation pin (the TODO's
  old "interrupt choreography" label was stale — the failures were the
  converter's missing `binary format` crafting surface) + the
  open-failure wal-index ref leak (a failed open pinned a stale shared
  header over every later open of the same path). wal 136 → 112.
- **S10**: walFrames padToSectorBoundary (wal.c:4172-4190) + the real
  sqlite.h IOCAP values + testvfs iDevchar passthrough + the FULL
  sync-flag default. walvfs-5.3 green; wal sweep 112 → 111.
- **S11** (c96cf0782): PRAGMA wal_autocheckpoint (pragma.c:2409's row
  surface). wal2 37 → 23.
- **S12**: where.c's ORDER BY collation-aware index matcher + build.c's
  REINDEX mode resolution (collation precedence R-35892-30289,
  single-object table/index resolution, per-schema emission) + the
  collation shape fixes in the harness (sort_by_length/sort_by_value
  bodies) + the e_reindex skip removals. **e_reindex 95 → 0, index 2 →
  0**; the index census sweep 200 → 101, zero new. The full-suite run
  gates the commit.

## Immediate next action

Verify the S12 full-suite run (per-family failure counts vs the census —
a count INCREASE in any family is a regression), then commit S12. After
that, continue the census families in size order: **rtree 126, tkt 124,
where 111, expr 109, pager1 101**, then bestindexF 35 / index9 20 /
indexexpr2 10 in the index sweep. Baselines: /tmp/index_fails.txt (the
pre-S12 index sweep), portplan/baseline_wal_sweep_2026-11-07_S8.txt, the
S10/S12 sweep logs in /tmp (regenerate as needed).

## Queue (in order)

1. Sidecar lifecycle COMPLETE (pieces 1/2/3: 1ab97052c, b72123592,
   5d9f97c3f) — wal-24.2 passes, walbak-4.3.1/4.3.3 pass, e_walckpt 0.
2. Backup unit COMPLETE: 47e9165de + the S7g sqlite3_backup command
   object (f85b1f905): **walbak 0 failures** (was 23).
3. Regen-drift follow-up DONE (S8, 351936d39): interrupt2 36/37;
   e_walauto carded unsupported (-shm byte introspection); wal5 carded
   (lock_common primitives). Residue: interrupt2-2.1 (the per-row
   callback interleaving a checkpoint — needs an interleaved
   row-callback step surface).
4. wal-18.2.x DONE (S9): native pin + skips; the binary-format
   converter class (43 files) carded below.
5. Fix B padToSectorBoundary DONE (S10): walvfs-5.3 green.
6. Census top non-WAL families: ~~index (the true family: 2, S12)~~,
   ~~e_reindex 95 (S12)~~, **rtree 126, tkt 124, where 111, expr 109,
   pager1 101** — triage per family with the census methodology. In the
   index sweep the remaining residue is the callback/byte-manipulation
   classes: bestindexF 35, index9 20 (EXPLAIN-row callbacks),
   indexexpr2 10, bestindexE 9.
7. WAL residue (carded, not engine gaps): the xShmLock trace /
   -shm byte introspection class (wal2 23: 1.x/2.x incr_tvfs_hdr/
   set_tvfs_hdr, wal_set_walhdr, -noshm; wal3 5), walvfs 7 (error-
   injection surfaces: 1.2, 3.1/3.2/3.2.2, 4.2, 6.2, 9.1), walfault-11
   (the faultsim trial-cap), interrupt2-2.1. The converter classes:
   `binary format`/`binary scan` + file-write (43 files), the EXPLAIN-
   row callback surface, the interleaved row-callback step.
8. Performance phase (F1c/F3 lanes in GAP_CLOSURE_PLAN_2026-10-31.md) —
   after functional gaps close.

## Working rules (violations cost hours — see .agents/lessons_learned.md)

- Verify by **failure-name-set diff** (`comm` both directions), never counts
  (counts swing ±35 under load; pure HEAD reproduces it).
- **Never** build baselines via stash roundtrips — commit each verified unit
  immediately; use a worktree for HEAD baselines.
- `go build ./...` does NOT compile _test.go files — binary-verify
  instrumented builds (`go test -c -o` + `strings | grep <trace-tag>`).
- `FRIGOLITE_TEST` matches by SUBSTRING — "wal" sweeps the whole wal* group,
  "index" sweeps bestindex*/index9/e_reindex. Derive family truth from the
  case paths.
- A fixture family with got [""] runtime-var mismatches + cascading
  "file is not a database" reopens = STALE FIXTURE: regenerate first.
- A whole-case skip that starves later cases' setup (the descidx2 class —
  e_reindex-2.0's exec carried the whole section-2 setup) must run the
  steps instead (harnessStaleAssertSubtests) or be removed.
- Oracle: `DYLD_LIBRARY_PATH=/tmp/tclshim ../sqlite/testfixture` (rebuild the
  shim per HANDOFF); ../sqlite is 3.51.0, /usr/bin/sqlite3 is 3.54 — tag
  every observation.
- Plans before change, lessons after every unit (plan/GUIDELINES.md §1b).
