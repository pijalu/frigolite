# TODO — the live restart index

> Details live in **[portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md](portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md)** —
> read it first. This file is the one-page index; update BOTH when checking
> items off.

## Goal

`close all gaps with sqlite — functional first, then performance.`

Current state (2026-11-08, feature/perf @ S19): *S18** (215dd7ae1): the same-connection active-statement DDL lock
  (Engine.RegisterStmtReadTables; a started statement's cursors block
  DROP with SQLITE_LOCKED) + the C-API step/finalize code surfaces
  (__FIN_ expansion; the schema-change finalize reports SQLITE_SCHEMA).
  tkt2820 4 -> 0, tkt1644 3 -> 0; tkt 41 -> 33.
- **S19 slice 1** (42ec0f866): the rtree geometry REGISTRATION surface
  (the engine machinery existed; the converter dropped the
  register_circle_geom/register_box_geom commands). The box callback
  ported with the OnCell observer hook. rtreedoc2 29 -> 11 (the fixture
  refreshed to 46 cases).
the census's tkt slice
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

- **S18** (215dd7ae1): the same-connection active-statement DDL lock
  (Engine.RegisterStmtReadTables) + the C-API step/finalize code
  surfaces (__FIN_ expansion; the schema-change finalize reports
  SQLITE_SCHEMA). tkt2820 4 -> 0, tkt1644 3 -> 0; tkt 41 -> 33.
- **S19 slice 1** (42ec0f866): the rtree geometry REGISTRATION surface
  (the engine machinery existed; the converter dropped the
  register_circle_geom/register_box_geom commands). The box callback
  ported with the OnCell observer hook. rtreedoc2 29 -> 11 (the
  fixture refreshed to 46 cases).
- **S19 slice 2** (ffd102815): the box_geom trace recorder — the
  fixture-proc interpreter wired to the box callback's OnCell observer
  (the per-case `proc box_geom` redefinitions rebind; the C
  invokeTclGeomCb argument shape; the zero/user/user_is_zero result
  protocol; the query-end finalizer; the runtime-var sync). **rtreedoc2
  0 failures** — the whole 46-case family green.

- **S20** (680e725fd): the search-count surfaces — whereD's
  do_searchcount_test helper (the labeled SearchStyle), the unique-scan
  plan shape (a unique fully-bound equality scan has NO Next loop in
  C's bytecode), and the OR-plan's covering-aware movetos. The where
  census 102 -> 94 leaves, zero new. The remaining where classes:
  subquery-in-WHERE search accounting (whereD 3.4.2-3.5.x,
  6.3.2-6.6.4), where8-3.x's db-status-step counter, the whereD
  index-cycle shapes.
- **S21**: the OR-plan's per-arm mechanics — the whereD census residue
  (3.4.2-3.5.2, 6.3.2-6.6.4, 16 leaves) closed with C-pinned changes in
  execSelectWithOrPlan: PER-BRANCH movetos (C's alt-map is per-arm), a
  TWO-PHASE at-most-one-per-candidate fetch (phase 1 = residual reads,
  pre-RowSetTest — rejected and duplicate candidates count, where8-1.10;
  phase 2 = output reads, post-RowSetTest — emitted rows only, 1.4a's
  duplicate; oracle decompositions 7/10/5/3), ROWID arms (eq =
  OP_SeekRowid: 0 seek/0 Next/0 moveto, pseudo-aliases on rowid tables;
  range = counted seek + Nexts), the ONCE-BOUND SUBQUERY arm (C's
  consumed/residual split — a bound subquery runs once), the residual
  fold covering every constraint CLASS (unconsumed GLOB/LIKE/range subs
  stay residual — dropping them leaked +3078 leaves), the RowSetTest
  dedup AFTER the arm's residual (a rejected row stays available to
  later arms), and NULL-constant comparisons never binding as index
  terms (`col = NULL` is never true; rowvalue2-4.IDX2). whereD 16 -> 0;
  where7/rowvalue2 green; where8's set == baseline; the full-suite gate
  strictly better than S15 (temptable2-4.1.1 = timing flake, the rest
  of its leaves = the carded temp-spill class, pre-existing at HEAD).

## S22 RECORD (2026-11-12, LANDED — gate3 verified)

- **ENGINE** (internal/execquery): the single-table ORDER BY delivery
  slice. (1) `indexSeekChosen`'s stat1 branch: est<nRow replaces the
  invented 10% cliff (C has no 10% cliff — probed 3.54; tie→scan).
  (2) The ORDER-BY resolution walks the ROWID TAIL (findIndexForOrderTerms
  refactored into forEachOrderServingIndex): where-12.2's ORDER BY b, a
  over (b). (3) The COVERING ORDER-BY-serving walk beats the seek+sorter
  when C's cost says so: internal/execquery/wherecost.go ports
  wherePathSolver's comparison verbatim (LogEst/LogEstAdd/estLog, the
  seek rRun, whereSortingCost with the mixed-domain LIMIT cap, +3/-2).
  Pins: frigolite_where_s22_test.go (TestS22_*).
- **CONVERTER**: `lsort -dec` (Tcl unique-prefix abbreviations) resolved
  via lsortOption() — whereC's 10 `.3` expectations were baked ASCENDING.
  whereC.json regenerated; whereF.json regenerated minus the 3 vmstep
  cases (SQLITE_STMTSTATUS_VM_STEP counts real VDBE opcodes — not
  expressible in the tree-walking executor; carded with e_walauto's
  -shm introspection); where2-7.2/7.3 hand-pinned (stale `stmtStatus`
  flag → sortMarker; a full where2 regen DROPS 19 passing cases — never
  install wholesale).
- **Census: where 78 → 47** (where 26→10, whereC 10→0, whereF 3→0,
  where2 6→4). Zero new at every step.
- Full gate 1 hit the 25m timeout on the CARDED walfault-11 trial cap
  (the S21 gate ran 1965s) — full gates need -timeout 40m. **Gate 3
  VERIFIED** (/tmp/full_s22_gate3.log, 1907s): 33 FIXED / 0
  REGRESSIONS vs /tmp/full_s21_fails.txt (1466→1433 names); the
  where-family census is exactly 47 (where 10, where2 4, where8 16,
  where9 8, whereH 5, wherelimit2 4). Gate health rules added to
  lessons (gate health section): -timeout mandatory, -v preferred,
  ps-CPU-progress check before waiting, bounded waits only.

## Immediate next action

1. **The parallel-where phase is COMPLETE** (S22 → S26 → S24 → S23-cont
   → S29 → S30, all merged, every merge gate name-set-verified; the
   full record lives in portplan/HANDOFF_2026-11-12_PARALLEL_WHERE.md):
   - Where census **78 → 3** (where-25.5 carded; where2-2.5/2.5b the
     random-EXPLAIN class; the where2-2.4 probabilistic flake).
   - pager1 **99 → 89** (U1-U4 landed; U3's readonly leaves await U6/U7).
   - fts3expr3 52 → 0; e_expr -9; where8/where9/whereH 0; the DML
     corrupt contract (25.1/25.2) + 9 bonus DML fixes.
   - The seek-collation gate (96b624803) fixed the existsexpr-4.4.2
     regression; temptable2-9.2.x bisected PRE-EXISTING (a small-cache
     stress dither, watchlist).
   - Verified baseline: /tmp/full_s30_gate_fails.txt (1366, clean run;
     the only "new" = the documented temptable2-4.1.1 timing flake).
2. **Next units (in order)**:
   - **pager1 U5-U7** (85 leaves: B7 misc 11 → B4 hot-journal 13 → B5
     super-journal+faultsim 61; land U6's journal substrate before U7;
     U3's 4 readonly leaves ride U7).
   - **expr S5+A1+A8+D1** (11 subtests, the converter batch) + the
     fts3* residue 54 (B1-style converter classes).
   - **where-25.5**: the WR index PK-tail format unit
     (portplan/PLAN_S26_dml_corrupt.md's card).
   - **The WAL residue classes** (HANDOFF_2026-11-04_WAL_RESIDUE.md).
   - Then the carded converter classes and the performance phase
     (F1c/F3 lanes in GAP_CLOSURE_PLAN_2026-10-31.md).
3. Standing rules: ≤2 agents while a gate runs (≤4 without); gates need
   the box's CPU (budget -timeout 60m when sharing); verify in FAMILY
   context (the leaf filter skips setup steps); faultsim logs corrupt
   in place — never trust a suspiciously-clean extraction.
Baselines: /tmp/full_s30_gate_fails.txt (1366, the verified diff
target), /tmp/full_s32_gate_fails.txt (1388 incl. the stress dither),
/tmp/full_s29_gate_fails.txt (1375). Older logs purged — rebuild fail
sets from logs, never trust remembered counts.

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
