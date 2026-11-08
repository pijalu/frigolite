# TODO — the live restart index

> Details live in **[portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md](portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md)** —
> read it first. This file is the one-page index; update BOTH when checking
> items off.

## Goal

`close all gaps with sqlite — functional first, then performance.`

Current state (2026-11-08, feature/perf @ S16): the census's tkt slice
is largely a STALE-GENERATION artifact. Landed this session:
- **S9** (73bee149a): wal-18.2 pin + the open-failure wal-index ref leak.
- **S10** (b4d2817a5): padToSectorBoundary + IOCAP/devchar/sync defaults.
- **S11** (c96cf0782): PRAGMA wal_autocheckpoint. wal2 37 -> 23.
- **S12** (d084ca3af): the ORDER BY collation-aware index matcher +
  build.c REINDEX mode resolution. e_reindex 95 -> 0, index 2 -> 0.
- **S13** (74c173e27): the stale fts3-parentheses fixtures regenerated
  (tkt-bdc6bbbb38 16 -> 0; the set_fts3_parens machinery existed) + the
  query catch-capture path. fts3 sweep 121 -> 118.
- **S14** (7a37980ba): the tkt fixture generation refresh — 48
  underscore-named old-generation duplicate fixtures deleted (the same
  sources ran TWICE under two family names, the stale ones truncated:
  missing setups, missing UDFs). tkt census 124 -> 55.
- **S15** (ecfdf622c): the corpus-wide duplicate scan found 3 more
  (fts_9fd058691, fuzz_oss1, vacuum_into) — removed.
- **S16** (f81066ddf): the order-distinct mark-off honors UNIQUE-index
  NULL semantics (an IS NULL equality over a nullable unique-prefix
  column voids the mark-off — duplicate NULL keys). tkt3824 6 -> 0.
Full-suite gates: S12 2014 -> S15 1929 failing subtests, no family
worse at either gate. THE CENSUS NUMBERS WERE INFLATED by the duplicate
generation — re-derive family truth from the case paths before scoping.

## Immediate next action

The remaining tkt classes: **tkt-31338dca7e 8** (LEFT JOIN chains +
"unable to identify" DROP TABLE), tkt2820 4, tkt1644 3 (C-API finalize
surfaces), then the census families **rtree** (the per-unit class map in
HANDOFF's RTREE SWEEP TRIAGE), **where 111, expr 109, pager1 101**.
Baselines: /tmp/full_S15_fails.txt (the true full-run name set),
/tmp/tkt_after_fails.txt, portplan/baseline_wal_sweep_2026-11-07_S8.txt.

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
