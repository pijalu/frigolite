# TODO — the live restart index

> Details live in **[portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md](portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md)** —
> read it first. This file is the one-page index; update BOTH when checking
> items off.

## Goal

`close all gaps with sqlite — functional first, then performance.`

## State (2026-11-16, feature/perf @ e8675d3de)

Full-suite truth **1317 failing leaves** (verified combined gate
/tmp/full_s33s34_merge_gate.log: **NEW=0** vs the true baseline
/tmp/full_s33_true_fails.txt 1430, every one of the 113 GONEs accounted
for by commit evidence).

The parallel-where + pager arc is COMPLETE:

- **S22-S30** (the parallel-where phase): where census 78 → 3, fts3expr3
  52 → 0, e_expr 21 → 2, wherelimit2/where8/where9/whereH/rowvalue2 0,
  the DML corrupt contract (25.1/25.2) + 9 bonus DML fixes. Full record:
  portplan/HANDOFF_2026-11-12_PARALLEL_WHERE.md.
- **S31** (s31-pager-engine, merged 51a09c00b): pager1 U5-U7 — the
  failed-COMMIT abort path (S31.6), the hot-journal substrate (S31.7:
  recovery at the per-statement shared-lock point, C's three-way
  classification, file-healing playback), the super-journal engine +
  faultsim/testvfs harness machinery (S31.8). **pager1 89 → 0** (957
  leaves green). It also really fixed alter2 (13), analyze-5.99,
  fkey6-6.3+setup_1, index3-99.1, trigger7-99.1, main-2.0#01,
  tkt-f3e5abed55-2.5 via the select_scan rewrite (verified: those fail at
  dd045611d, pass at HEAD, consistent across two gates).
- **S34** (s34-regress, merged): the one real S31 regression —
  hotJournalProbe classified stale journals HOT because the nPage==0
  guard read the in-memory page count (schema.Init materializes page 1;
  C's pagerPagecount reads the FILE size). pragma2-5.1/5.3,
  sharedA-1.1/1.2 green; the sweep 46→42 name-set clean. Two cards
  (rollback/close journal-file residue; close-mid-txn flushes instead of
  rolling back) in portplan/PLAN_S34_regress.md §Carded.
- **S33** (s33-wr-index, merged e8675d3de): the WR index payload format —
  every write site carries C's [indexed cols..., PK cols...] tail (dup
  elision, tail collations, DESC), comparators extended, the where-25.5
  corrupt recheck ENABLED (**where-25.5 green**, where 5 → 3), integrity
  un-exempts WR, probe parity vs oracle 3.54 byte-verified. Beyond-plan
  find: WR table trees are index-shaped — three scan sites opened them
  isTable=true and skipped one row per divider (fixed a5ea11125).
  **where-25.4 stays green.**

Census now: **where 3** (where2-2.5/2.5b the random()-EXPLAIN class +
the where2-2.4 probabilistic flake), **pager1 0**, fts3expr3 0, e_expr 2,
where8/9/H 0, without_rowid 27, tkt 33, rtree 126, expr residue, the WAL
residue classes.

## Immediate next action

1. **The WAL residue classes** (HANDOFF_2026-11-04_WAL_RESIDUE.md) — the
   next queue head.
2. **Carded converter classes**: expridx1 fossil 10, indexexpr2/3 11,
   e_expr regen drift/callbackVars, A2/A9 engine slices, the fts3*
   residue ~53, where2-12.1 RESOLVED, expr S5 residual 2.
3. **The performance phase** (F1c/F3 lanes in
   GAP_CLOSURE_PLAN_2026-10-31.md) after functional gaps close.

## Working rules (violations cost hours — see .agents/lessons_learned.md)

- Verify by **failure-name-set diff** — python raw byte scan
  (`re.findall(rb'--- FAIL: TestSQLiteSuite/([^\s(]+)')`, leaf-only) —
  NEVER `strings`/line-grep (faultsim NUL corruption silently eats lines:
  the 1355 baseline lost 75; re-derived 1430 from the same log). Never
  counts (swing ±35 under load).
- **Never** build baselines via stash roundtrips — commit each verified unit
  immediately; use a worktree for HEAD baselines.
- `go build ./...` does NOT compile _test.go files — binary-verify
  instrumented builds (`go test -c -o` + `strings | grep <trace-tag>`).
- `FRIGOLITE_TEST` matches by SUBSTRING — "wal" sweeps the whole wal* group,
  "index" sweeps bestindex*/index9/e_reindex. Derive family truth from the
  case paths. Leaf-level filters SKIP setup steps — verify in FAMILY context.
- A fixture family with got [""] runtime-var mismatches + cascading
  "file is not a database" reopens = STALE FIXTURE: regenerate first.
- A whole-case skip that starves later cases' setup must run the steps
  instead (harnessStaleAssertSubtests) or be removed.
- Gate health: -timeout mandatory (60m when sharing CPU), -v required,
  gates need exclusive CPU (concurrent agent sweeps starved walfault-11),
  faultsim logs corrupt in place, bounded waits + ps-CPU-progress checks.
- Suspicious GONE sets: bisect at the pre-unit base commit (a scratch
  worktree) before crediting a fix; a "fix" no commit explains is an
  extraction loss (e_dropview's 14 leaves were eaten from the baseline).
- Agent sweep lists must cover the FULL family universe — a 31-family
  sweep missed both the S31 regressions (pragma2/sharedA) and the phantoms.
  The full-suite gate is the only complete net.
- Oracle: `DYLD_LIBRARY_PATH=/tmp/tclshim ../sqlite/testfixture` (rebuild the
  shim per HANDOFF); ../sqlite is 3.51.0, /usr/bin/sqlite3 is 3.54 — tag
  every observation.
- Plans before change, lessons after every unit (plan/GUIDELINES.md §1b).

## Baselines

- **/tmp/full_s33s34_merge_fails.txt (1317, the CURRENT diff target)**
  from /tmp/full_s33s34_merge_gate.log (NEW=0 verified).
- /tmp/full_s33_true_fails.txt (1430) — the pre-S31 truth (python
  raw-scan of /tmp/full_s33_gate.log; supersedes the UNDERCOUNTED
  full_s33_gate_fails.txt (1355) — keep both files for provenance).
- /tmp/full_s31_merge_fails.txt (1322 + 4 regressions fixed by S34).
- Older logs purged — rebuild fail sets from logs, never trust remembered
  counts.

## Queue (in order)

1. **WAL residue** (carded, not engine gaps — see the HANDOFF): the
   xShmLock trace / -shm byte introspection class (wal2 1.x/2.x
   incr_tvfs_hdr/set_tvfs_hdr, wal_set_walhdr, -noshm; wal3 5), walvfs 7
   (error-injection surfaces), walfault-11 (the faultsim trial-cap),
   interrupt2-2.1. Converter classes: `binary format`/`binary scan` +
   file-write (43 files), the EXPLAIN-row callback surface, the
   interleaved row-callback step.
2. **Carded converter classes**: expridx1 fossil 10, indexexpr2/3 11,
   index9 20 + bestindexE/F 44 (EXPLAIN-row callbacks), e_expr regen
   drift/callbackVars, A2/A9 engine slices, fts3* residue ~53.
3. **Engine residue cards**: pager1 4.4.2 directory-hierarchy machinery,
   S34's two journal cards, temptable2-9.1.x/9.2.x (pre-existing
   small-cache stress dithers, watchlist), where2-2.4/2.5 random().
4. **Performance phase** (F1c/F3 lanes in GAP_CLOSURE_PLAN_2026-10-31.md)
   — after functional gaps close.
