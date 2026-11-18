# TODO — the live restart index

> Details live in **[portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md](portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md)** —
> read it first. This file is the one-page index; update BOTH when checking
> items off.

## Goal

`close all gaps with sqlite — functional first, then performance.`

## State (2026-11-18, feature/perf @ 4bb2a2911)

Full-suite truth **1218 failing leaves** (verified gate
/tmp/full_s35_merge_gate.log; vs the 1268 baseline: 52 GONE — 50 real,
each commit-attributed below, +2 resetdb flaps that re-run red — and
NEW=2, both documented dithers: temptable2-4.1.1 the random-sequence
dither, walfault-15 the cross-case JOIN flap).

The WAL small-slices arc (S35a-d) is COMPLETE and merged:

- **S35a** (s35-wal-small): the lifecycle-for-named-db pre-scan flip
  (open_named_db on test.db joins the reopen lifecycle) — wal9-1.1/1.7,
  walcrash2-1.3 + 24 same-class "no such table" harness artifacts;
  pager4-1.6 oracle-adjudicated as a fixture defect, fixed by the
  rename_file harness step (**pager4 5→0**).
- **S35b + S35c.1/.3** (one commit, 15c7e8789): wal3-3.3 (WALSyncFull
  0x0F→0x0A — the sync STRING is load-bearing), walmode-4.9 (the BUSY
  swallow deleted — C's OP_JournalMode fails the statement), wal-12.1
  (file_size step + the SetPageSize double-refusal: same-size no-op +
  BTS_PAGESIZE_FIXED), subjournal-2.1 (backup pin); S35c.1 the
  large-sector write rule (sectorSize + pagerWriteLargeSectorLocked +
  the changecounter rider) and S35c.3 the missing-main rule
  (os.IsNotExist joins the 0-byte garbage rule). **Plus 31 adjudicated
  ride-alongs** across 11 non-wal families (exclusive ×7, snapshot3 ×8,
  snapshot4 ×2, schema/schema2 ×4+4, parser1 ×2, e_changes ×2,
  e_blobclose, multiplex, tkt-d82e3-2.1) — all verified fixed at
  15c7e8789 by pre-commit probes.
- **S35c.2** (08897585f): the two-path wal-index header split — real-shm
  trusts a parsed header with NO file re-check (walIndexTryHdr);
  heap-memory (locking_mode=EXCLUSIVE, C's bNoShm) re-validates salts+
  capacity vs the -wal file and rebuilds. wal-universe sweeps
  byte-identical (74=74).
- **S35d** (15d293152/a9e1d2b3f): the NOOP checkpoint mode (WalCkptNoop=-1,
  locks+passes skipped, report {0 mxFrame nBackfill}), the btree.c in-txn
  SQLITE_LOCKED veto, register_function's fixture-proc interpreter round
  trip (S25 pattern), binary format c* in the interpreter, the
  converter's braced-body proc collection. **walckptnoop 7→1** (1.0+1.7
  green) + tkt-f7772-1.2 (stable 2/2). Carded: **1.1-1.5 expose a NEW
  btree page-density divergence** (our bulk insert's WAL mxFrame 451 vs
  C's 298 — the leaves hold ~2/3 of C's rows at this cell size); the
  faultsim lifecycle cards (walfault-15's cross-case JOIN,
  walfault-11's trial-cap) for S35g.

The pager + authorizer + EXPLAIN-substrate arcs (S22-S37+S41):

- **S37** (s37-auth-events, merged): authorizer/update-hook event
  collection — compile-time FK events, per-column SQLITE_READ + ROWID,
  schema-name arg3, harness authorizer global sync via the S25 push/pull
  pattern, converter update_hook branch. **15 leaves**: savepoint-9.x (3),
  without_rowid3-18.x (5), tkt-f7b4edec (2), fkey2-18.x (5 bonus).
  Cards: SQLITE_IGNORE on FK parent-key reads, RESTRICT synthetic-SELECT.
- **S41** (s41-explain-rows, merged afe2e21a6): the EXPLAIN-row substrate
  — Stage 1 the engine row stream (explain_rows.go; UPDATE dispatch ported
  from update.c:564-585 incl. indexColumnIsBeingUpdated), Stage 2 the
  explain() TVF (eponymous module + the real-object shadow guard),
  Stage 3 harness surfaces (index9 19→0 hand-pinned, bestindexF 34→17,
  vacuum2 4→2, indexexpr2 9→5), Stage 4 distinctagg hand-pins (13).
  **where2-2.5/2.5b GONE at Stage 1** (where → 1: the 2.4 flake only).
  61 GONE total (44 S41 + 17 riding the S37 merge, incl. tpch01-1.1b/1.1c).
  Residual cards (the 12 NEW): distinctagg ×5 (DISTINCT-ordered-input
  optimization), trigger7-2.x ×6 (trigger sub-program rows in the
  stream), misc3-6.11-utf8 (constant-load rows); bestindexF 2.3.0/2.4.0/
  2.5.0 vtab eDistinct handshake + xBestIndex globals carded to S43.
The parallel-where + pager arc (S22-S34, S33):

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

Census now: **walckptnoop 1** (the 1.1-1.5 density card), **walvfs 6**,
**wal2 25** (the xShmLock trace class), **wal3 4**, **wal6 1**,
**walfault 3 + walfault2 2**, e_wal 14, e_walhook 13, walsetlk_recover 2,
wal-23.4, **where 1** (the 2.4 flake), **index9 0**, **bestindexF 17**,
**indexexpr2 5**, distinctagg 5, trigger7 6, misc3 1, without_rowid 27,
tkt 33, rtree 126, the fts3* residue, expr residue.

## Immediate next action

1. **S35e** — the no-shm WAL class (B1, 14 leaves, ~250-400 LOC):
   converter `testvfs X -iversion/-noshm` + the engine rules (journal_mode
   refusal on shm-unavailable, CANTOPEN without shm, EXCLUSIVE heap
   wal-index, the stuck-EXCLUSIVE no-op, db2 BUSY) per
   TRIAGE_wal_residue_2026-11-16.md §S35e.
2. **S35f** — the wal-hook surface (B10, 10 leaves): db wal_hook converter
   + the commit-end hook with rc propagation + the wal_autocheckpoint
   overwrite (reuse TestWalHookEngine's substrate).
3. **S35g** — error-injection surfaces + the faultsim converter (B6/B3):
   sqlite3_interrupt/memdebug_fail/xShmMap scripts, the restart/exit lock
   choreography, walfault2's do_faultsim_test emission; plus the
   walfault-15 cross-case JOIN card and walfault-11's trial-cap.
4. **The btree page-density unit** (NEW card): bulk-insert WAL frame
   accounting (ours 451 vs C's 298 on the walckptnoop workload — probe:
   isolate leaf/interior/index counts; the splitter's balance point).
5. **S42/S43** (TRIAGE_index_callback): the S41 residual surfaces
   (trigger7-2.x sub-program rows, misc3-6.11, distinctagg 5), the
   tcl-module completion (~34, bestindexF remainder).
6. **Carded converter classes**: expridx1 fossil 10, indexexpr2/3 11,
   e_expr regen drift/callbackVars, A2/A9 engine slices, the fts3*
   residue ~53.
7. **The performance phase** (F1c/F3 lanes in
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

- **/tmp/full_s35_merge_fails.txt (1218, the CURRENT diff target)** from
  /tmp/full_s35_merge_gate.log. Note: resetdb ×2 in its GONE-vs-1268
  delta were a LOAD FLAP (re-run red) — the next gate's resetdb "NEWs"
  are expected; the 1218 set keeps the gate's raw truth.
- /tmp/full_s41_merge_fails.txt (1268) — composed from the S41 gate raw
  scan (/tmp/full_s41_gate.log, 1281) with the three families re-verified
  fresh at the merged HEAD (distinctagg 5, trigger7 6, misc3 1 — the 13
  hand-pin greens folded in).
- /tmp/full_s33s34_merge_fails.txt (1317) from
  /tmp/full_s33s34_merge_gate.log (NEW=0 verified).
- /tmp/full_s33_true_fails.txt (1430) — the pre-S31 truth (python
  raw-scan of /tmp/full_s33_gate.log; supersedes the UNDERCOUNTED
  full_s33_gate_fails.txt (1355) — keep both files for provenance).
- /tmp/full_s31_merge_fails.txt (1322 + 4 regressions fixed by S34).
- Older logs purged — rebuild fail sets from logs, never trust remembered
  counts.

## Queue (in order)

1. **S35e/f/g** (see Immediate next action — the remaining WAL residue
   classes; the triage's B2 xShmLock-trace/-shm byte class (24) stays
   carded behind them).
2. **The btree page-density unit** (the walckptnoop 1.1-1.5 card).
3. **S42/S43**: the S41 residual surfaces (trigger7-2.x trigger
   sub-program rows, misc3-6.11-utf8, distinctagg ×5
   DISTINCT-ordered-input), the tcl-module completion (~34: callback
   global push/pull, %N% xFilter substitution, vtab IN-constraint loop,
   bestindexF 2.3.0/2.4.0/2.5.0 eDistinct handshake + xBestIndex
   globals).
4. **Carded converter classes**: expridx1 fossil 10, indexexpr3 2,
   e_expr regen drift/callbackVars, A2/A9 engine slices, fts3* residue
   ~53; `binary format`/`binary scan` + file-write (43 files — the
   format-c piece landed in S35d), the interleaved row-callback step.
5. **Engine residue cards**: pager1 4.4.2 directory-hierarchy machinery,
   S34's two journal cards, temptable2-9.1.x/9.2.x (pre-existing
   small-cache stress dithers, watchlist), where2-2.4 random() flake,
   the resetdb load flap (watchlist).
6. **Performance phase** (F1c/F3 lanes in GAP_CLOSURE_PLAN_2026-10-31.md)
   — after functional gaps close.
