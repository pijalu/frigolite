# TODO — the live restart index

> Details live in **[portplan/HANDOFF_2026-11-25_S55_FTS3QUERY.md](portplan/HANDOFF_2026-11-25_S55_FTS3QUERY.md)** —
> read it first (then the older WAL-residue handoff for the durable baseline
> methodology). This file is the one-page index; update BOTH when checking
> items off.

## Goal

`close all gaps with sqlite — functional first, then performance.`

## State (2026-11-25, feature/perf @ c02ff098a — S43 slices 1-6 + S44-S55 landed: bestindexE, bestindexF-1.4.1, distinctagg 5→0, trigger7 6→0, misc3 1→0, gencol1 6→5, indexexpr3 2→0, e_expr 2→0, indexexpr2 3→0, starschema1 8→1, expridx1 11→0, TCL binary-scan UDFs 8→0, FTS4 deferred matchinfo 6→0, deferred MATCH 14→0, ICU leaves 3→0, fts4umlaut 12→0, fts3malloc 5→0, fts3cov 2→0, fts3query 11→0; FULL gate NEW=0 vs t10; all other NEW=0)

Full-suite truth **1190 failing leaves** (the s35g gate baseline
portplan/baselines/s35g_gate_fails.txt minus the three walfault leaves
-11/-12/-15; the durable per-leaf baselines live in the repo — /tmp
baselines proved ephemeral across sessions). The arc: **e_wal 10→0
(S35e, 146a3c00e)**, **e_walhook 10→0 (S35f, cae6073ce)**, **walfault2
2→0 + walsetlk_recover superseded by the native TestWalSetlkRecoverEngine
(S35g slice 1, ae4ccd13ae)**, **walfault-12 GONE (S35g slice 2)** — the
trial-runner fixture threading (register_testvfs/testvfs_config + the
shm-seed step) plus the regen's runtime_expr/wal_checkpoint_rc trial
cases; the identity diff vs the s35g baseline across the 15-family gate
set (the FRIGOLITE_TEST=wal superset, 21 families + journal2 +
subjournal): GONE=exactly walfault-12, NEW=0. **walfault-15 GONE (S35g
slice 3)** — the fault-class -injectuninstall parity (the leaf-end
connection close for the shmerr/fullerr/cantopen classes; pagerfault
0-fail). **walfault-11 GONE (S35g slice 4)** — the shmerr class's
SHM-method ioerr counter (the fixture-level install/arm/stop; the file-I/O
injector had burned its budget on the checkpoint's backfill — the
512-trial cap, 737s → 5s). **The walfault family is GONE (walfault 3→0;
the whole family 0-fail verified fresh, pagerfault 0-fail,
go test ./internal/... green).** The GONE-also list (documented flakes):
resetdb-300 + where2-2.4; NEW=1: atof1-2.40 (the documented S32
random-dice).

The WAL small-slices arc (S35a-e) is COMPLETE and merged:

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
- **S35f** (cae6073ce): the wal-hook surface — C's doWalCallbacks position
  pinned (vdbeapi.c:723: the hook fires at the committing statement's
  completion, after the commit + the write-lock release; per-db
  read-and-clear of the log's TOTAL frame count, wal.c:4237/4415; the
  raw-code return fails the statement while the commit stands). Engine:
  the statement-tail dispatch (fireWalCallbacks + walHookErrStr), the hook
  signature at C's shape (dbName, nEntry), wal_autocheckpoint=N replaces
  the script hook (main.c:2496). Converter: the register_wal_hook step +
  re-registration on hook-proc redefinitions (C's TCL late binding) + the
  top-level global literal-set emission. Harness: the hook interp's
  sqlite3 opener (2.1's in-hook db2 read), the runtime_var expect
  evaluation (wal_frame_count), the lifecycle promotion scan for hook
  procs opening test.db. **e_walhook 10→0.** Latent pager unit breaks
  fixed (the WAL-mode AllocatePage nil-header panic; TestWalCheckpoint's
  premise + the RESTART observables; the checkpoint seam's read-mark
  deadlock; TestWALCallbackEntry pins read-and-clear).
- **S35g slice 1** (ae4ccd13ae): walfault2 2→0 (the stale JSON
  regenerated; the faultsim runner gained close_db + the URI reopen —
  the first cut leaked the reopen to the process CWD and pagerfault-36
  caught it), the walsetlk_recover native port (TestWalSetlkRecoverEngine —
  the testfixture_nb subprocess class), the recovery restructure (the
  -wal reads outside the index mutex — C's private-buffer shape; the
  endianness bit: the magic's checksum-order flag is the value's low
  byte, buf[3]), PRAGMA busy_timeout implemented + wired to the WAL layer,
  the xSleep/xRead hook emissions. Remaining carded: walfault-11 (the
  trial-cap), -15 (the exclusive-INSERT lock interplay — passes in isolation).
- **S35g slice 2** (walfault-12): the trial-runner fixture threading —
  runFaultsimTest/runFaultsimStep carry the fixture registry +
  defaultVFS; the trial-scope register_testvfs/testvfs_config cases (the
  shm-seed step seeds the wal-index header via CorruptWalIndexWith on
  every open connection matching the resolved FILE); the regen's new
  trial emissions handled (runtime_expr — the prepared-handle binding +
  the evalHarnessRuntimeExpr fallback; wal_checkpoint_rc/v2 —
  test1.c:7609's t1ErrorName shape: the success pair is {0 {}} (the C
  body's `if` false-result), non-OK → {1 {errmsg}} matching faultsim's
  tolerated errlist); evalFaultsimTestScript threads the trial's
  runtimeVars into the -test interp (walfault-10's `sqlite3_finalize
  $::stmt`). Converter: decodeBackslashEscape octal escapes + the
  ShmSeed capture/mapping chain (b0c131023). **walfault 3→2** (only the
  carded -11 trial-cap + -15 JOIN remain).
- **S35g slice 3** (walfault-15): the fault-class lifecycle — C's
  do_faultsim_test runs the class's -injectuninstall at the leaf end
  (shmerr/fullerr/cantopen: `catch {db close}; catch {db2 close};
  shmfault delete`; the ioerr classes define none). The harness's
  runFaultsimTest now closes the main + named connections for the
  uninstall classes at EVERY exit: walfault-11's last trial leaves db2
  open, its WAL-mode file-SHARED hold (lockreg WALFileShared —
  pager.c:2158's unlock-skip) blocked -15's locking_mode=exclusive write
  three leaves later. Verified: the walfault family → only the carded
  -11 fails; pagerfault (the cantopen user, pagerfault-6.2) 0-fail;
  go test ./internal/... green. **walfault 2→1.**
- **S35g slice 4** (walfault-11): the shmerr class's real injector —
  C's ioerr counter runs over the fixture's FILTERED SHM METHODS
  (test_vfs's `tvfs ioerr nFail persist`: per-VFS, method-entry counting,
  script or no script); the harness's pager file-I/O injector burned its
  budget on the checkpoint's ~8000-frame backfill (nfail never cleared →
  the 512-trial cap, ~737s). The testvfsFixture grew the ioerr counter
  (ioerrStart/ioerrStop + the hook-entry check), the leaf-start
  -injectinstall parity (register shmfault -default with the
  {xShmOpen,xShmMap,xShmLock} filter preset), and the per-trial
  arm/stop switch (prep-before-arm, C's do_one_faultsim_test order).
  **walfault-11 5s (was 737s) — walfault 1→0: the family is GONE.**
- **S35e** (146a3c00e): the no-shm WAL class — the C choreography pinned
  from source (PLAN_S35e_noshm.md §UPDATE): lockBtree's version-2/2 WAL
  entry at the first schema-needing statement's parse (btree.c:3337), the
  CANTOPEN source sqlite3PagerOpenWal (pager.c:7641), the EXCLUSIVE
  main-file lock taken+held under exclusiveMode (pagerOpenWal 7588 /
  sqlite3PagerBegin 5928), the NORMAL-WAL persistent file-SHARED quirk
  (pager.c:2158's unlock skip — wal.c:4474's op=0 rc=0), the heap-index
  locking_mode no-op. Engine: the per-statement refusal gate
  (WALAttachRefusal, pinWAL-gated), the heap conversion at the latch
  (walConvertHeapEligibleLocked, windowed on walDataTouched with
  SuppressAccessMark for the open's schema load + the harness reopen
  prime), lockreg.walFileShared + e.walExclusiveFileHeld (the file-lock
  hold surviving the journal_mode=DELETE exit). **e_wal 10→0**; the
  27-family worktree diff leaf-identical; wal2-6.6's real-page-1-SELECT
  discriminator carded in lessons.

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
  stream), bestindexF 2.3.0/2.4.0/
  2.5.0 vtab eDistinct handshake + xBestIndex globals carded to S43.
  **S46 closed misc3**: runtime regexp probes over the flat EXPLAIN
  capture + Int64/Real/default-p4/DESC-keyinfo rows; a GENERATED-column
  index guard kept gencol1-23.3 green and made gencol1-23.4 GONE.
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

Census now (verified at the walfault-11 unit, 2026-11-19): **walckptnoop
5** (the density card 1.1-1.5), **walvfs 6**, **wal2 25** (the xShmLock
trace class), **wal3 4**, **wal6 1**, **walfault 0 + walfault2 0** (the
whole faultsim family green), **journal2 5** (re-verified fresh), **e_wal
0** (S35e complete), **e_walhook 0** (S35f), walsetlk_recover 2, wal-23.4,
**where 1** (the 2.4 flake), **index9 0**, **bestindexF 17**,
**indexexpr2 0**, distinctagg 0, trigger7 0, misc3 0, gencol1 5,
indexexpr3 0, e_expr 0, starschema1 1 (1.2), without_rowid3 16
+ without_rowid7 4, tkt 14, rtree 126, the fts3* residue, expr residue.
(The old census's grouped counts — e_wal 14, tkt 33, without_rowid 27 —
mixed substring filters; the per-leaf truth is
portplan/baselines/s35g_gate_fails.txt.)

## Immediate next action

1. **S35g COMPLETE** (e_wal, e_walhook, walfault2, walfault all 0). The
   sqlite3_interrupt/memdebug_fail surfaces stay carded until a family
   needs them.
1b. **The btree page-density unit: the LEAF-SPILL SLICE is GATED
   identity-clean (FULL suite REAL NEW=0, /tmp/dens_gate3.log) and
   committed** — read portplan/PLAN_density_walckptnoop.md FIRST (the
   2026-11-21 handovers: the root cause = the parent re-key's room
   check ran after the page writes; the fix = the dividerCellLen-
   predicted precheck hoisted BEFORE any write, spill unit in
   btree_leaf_spill.go). Density: ours 134 i1-leaves/87.6%/312 frames
   (C: 129/91.6%/298; pre-slice 145/79.7%/345); walckptnoop mxFrame
   451→348 vs C 298. The two gate NEWs are verified flakes (resetdb-300
   flap; temptable2-4.1.1 — the engine's randomblob is UNSEEDED, index
   shapes flip per run; see lessons). NEXT: slice 2, the interior
   window spill (the SLICE 2 PLAN section in the plan doc) — the
   interior tail-split husks (31 vs C's 22) hold the remaining ~50-
   frame delta.
2. **The btree page-density unit: slices 1-2 LANDED (the leaf spill +
   the interior window spill, identity-clean FULL gates — ours
   308 pages/136 i1-leaves/25 interiors vs C 299/129/22; walckptnoop
   mxFrame 451→337 vs C 298). SLICE 3 (the 3-old-sibling window)
   ATTEMPTED AND REVERTED** — both extensions measured worse (the
   redistribution's cut keys perturb the downstream leaf landscape; the
   leaf draft also corrupted — the right-sibling slot and exact-run
   bugs found and recorded; the residue thread open in the plan doc's
   2026-11-22 (2) handover). The remaining delta stays CARDED with the
   resumable lever; the unit's core (the spills, the husk elimination)
   is banked.
3. **S42/S43** (TRIAGE_index_callback): index9 = 0; S43 slices 1-6
   DELIVERED (the vtab callback stack; the selective regen + the seeding
   machinery; the WHERE_GROUPBY consumption with the streaming aggregate;
   the WHERE_DISTINCTBY offering + the DISTINCT suppression + the
   ephemeral grouping; slice 5 = the LEFT JOIN xBestIndex prepare
   contract + the compound WHERE push-down (select.c pushDownWhereTerms
   parity) + the eponymous tcl module; slice 6 = the bestindexF-1.4.1
   fixture correction + the native EXPLAIN pin — the engine was already
   green). **bestindexE 6→0; bestindexF-1.4.1 GONE**; the bestindex residue
   = 7 leaves (F: 2.8.0/2.8.1/2.9.0/2.9.1/2.10.0, the converter's
   machinery-placement class + bestindex1-3.4/3.5, both carded).
3b. **S44/S45** (S41 residuals): S44 slice 1 DELIVERED — distinctagg
   5→0 via aggregate-DISTINCT `OpenEphemeral` EXPLAIN rows
   (WHERE_DISTINCTBY / isDistinctRedundant parity). S45 slice 1
   DELIVERED — trigger7-2.x 6→0 via firing UPDATE OF trigger sub-program
   rows in EXPLAIN + a runtime `string match` converter/harness step.
   S46 slice 1 DELIVERED — misc3-6.11-utf8 1→0 via runtime regexp
   probes, Int64/Real/default-p4 rows, DESC keyinfo, and a generated-column
   index guard (gencol1 6→5). S47 slice 1 DELIVERED — indexexpr3 2→0
   via scalar OP_Function result rows + the Function-count fixture
   correction. S48 slice 1 DELIVERED — e_expr-32.2.8 1→0 via the
   in-tree 3.51 REAL-text expectation (3.54's 17-digit spelling was
   version drift). S49 slice 1 DELIVERED — e_expr-26.1.6 1→0 via the
   callback-counter reset step (converter already emits it; checked-in
   fixture predated it). e_expr family now 0. S50 slice 1 DELIVERED —
   indexexpr2-4.120/4.130 2→0 via the `cnt` reset steps.
   S51 slice 2 DELIVERED — indexexpr2 3→0: expression-index WHERE seeks
   (canonical key + declared collation + expression affinity), expression-
   index ORDER BY matching, and C's correlated-aggregate context rule
   (argument with an inner-column reference stays inner). The balanced
   CREATE INDEX key parser also fixed starschema1-1.3..1.9 (8→1).
   The fts3defer2 deferred MATCH residue was closed in 3f below.
3c. **S52 slice 1 DELIVERED — expridx1 11→0**: imposter rootpage rewrite in
   the converter/fixture, canonical expression seeks with correlated outer
   bounds, WITHOUT ROWID secondary autoindex maintenance + covering seeks,
   imposter-only stale-key delete fallback, imprecise-float integrity
   diagnostics, and pure-Go engine pins. FULL gate NEW=0 vs t10.
3d. **S53 slice 1 DELIVERED — TCL binary-scan matchinfo UDFs 8→0**: the
   converter interpreter now exposes `tcl_platform(byteOrder)` and
   `binary scan i*/I*`, so fts3defer2/fts3first `mit` UDFs render the FTS4
   matchinfo blob as tclsqlite does. FULL gate NEW=0 vs t10; `fts3first`
   (7 leaves) and `fts3query-3.3` GONE.
3e. **S53 slice 2 DELIVERED — FTS4 deferred matchinfo 6→0**: C's
   `sqlite3Fts3EvalPhraseStats` rule for overflow-sized tokens — an all-deferred
   phrase in an AND cluster reports nDoc for global occurrences/rows; a mixed
   phrase counts through its non-deferred tokens. Full gate NEW=0 vs t10,
   GONE=6 fts3defer2 leaves; pure-Go pin added.
3f. **S53 slice 3 DELIVERED — deferred MATCH 14→0**: one prepared,
   deferred-marked query tree now drives row filtering, matchinfo, offsets and
   snippet; deferred tokens rescan stored `%_content` positions after segment
   loss, with text-only document restore (no postings) on reload. Defer
   decisions and content-size estimates are cached per index revision and
   invalidated on every index mutation, so MATCH preparing once per row stays
   linear. FULL gate `/tmp/t14_full2.log` (1171.345s, 1311 leaves): NEW=0 vs
   t10, GONE=40; fts3defer2 focused 0 leaves; pins and quality clean.
3g. **S54 slice 1 DELIVERED — ICU tokenizer 3→0**: `TOKENIZE icu en_US`
   now resolves to a pure-Go ICU word-boundary tokenizer (case folding,
   mid-word apostrophes, original byte spans), so the FTS4 `a b` deferral
   test runs instead of failing at CREATE. FULL gate `/tmp/t15_full.log`
   (1154.984s, 1307 leaves): NEW=0 vs t10, GONE=4; focused fts3defer raw
   diff NEW=0/GONE=4; pins and quality clean. `fts3atoken` remains an
   unsupported family at HEAD.
3h. **S54 slice 2 DELIVERED — fts4umlaut 12→0**: the JSON fixture had literal
   `u1ed9`/`u006f`/`u0302` where SQLite TCL uses `\uXXXX`. The pure-Go port
   passed, so `tclconvert`'s quoted-list unescaper now reuses TCL's
   backslash decoder; `testdata/fts4umlaut.json` was regenerated. FULL gate
   `/tmp/t16_full.log` (1183.085s, 1291 leaves): NEW=0 vs t10, GONE=60;
   focused family 0 leaves, converter failure-name set unchanged vs HEAD.
3i. **S54 slice 3 DELIVERED — fts3malloc 5→0**: `malloc_common.tcl`'s
   `do_write_test`/`do_error_test` wrappers now materialize as exec/catch
   steps in `tclconvert`, so the CREATEs that `fts3_malloc-2.0` drops are
   present. FULL gate `/tmp/t17_full.log` (1183.785s, 1287 leaves): NEW=0
   vs t10, GONE=64; focused family 0 leaves, converter failure-name set
   unchanged vs HEAD. `resetdb-300` is the documented flake (present in
   t10 and a focused re-run).
3j. **S54 slice 4 DELIVERED — fts3cov 2→0**: regenerating the family with the
   malloc-common wrappers exposed the dropped `read_fts3varint` helper; the
   converter now emits it as a runtime_expr and the harness can evaluate
   `string range` plus FTS3 varint decoding. FULL gate `/tmp/t18_full.log`
   (1172.895s, 1286 leaves): GONE=2 vs t17 for fts3cov; the only NEW leaves
   vs t10 are the documented `atof1-2.40` random-dice flake (focused run
   green). Converter failure-name set unchanged vs HEAD; quality NEW=0.
3k. **S55 slice 1 DELIVERED — fts3query 11→0**: `do_select_tests -errorformat`
   now emits catch expectations and TCL `expr` folds the int64-min boundary.
   Regenerating the fixture exposed a real template-cache bug: a folded
   `-9223372036854775808` slot served a positive magnitude on the next
   same-shape INSERT, flipping boundary rowids. Signed/hex numeric slots now
   refuse template substitution and force a fresh parse. FULL gate
   `/tmp/t19_full2.log` (1273 leaves): NEW=0 vs t18 and durable t10; 11
   fts3query leaves GONE. Converter failure-name set NEW=0; quality NEW=0.
4. **Engine residue cards**: fts3* residue ~12 (fts3query/fts3defer2/ICU/
   fts4umlaut/fts3malloc/fts3cov now 0; e_fts3 8, fts3auto 1, fts3defer3 1
   next); `binary format`/file-write substrate (the format-c and binary-scan
   i*/I* pieces landed in S35d/S53), the interleaved row-callback step.
5. **The performance phase** (F1c/F3 lanes in
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
3. **S42-S46**: the S41 residual surfaces are DELIVERED (trigger7,
   misc3, distinctagg); the tcl-module completion (~34: callback
   global push/pull, %N% xFilter substitution, vtab IN-constraint loop,
   bestindexF 2.3.0/2.4.0/2.5.0 eDistinct handshake + xBestIndex
   globals).
4. **Engine residue cards**: fts3* residue ~39 (fts3defer2 MATCH
   evaluation first); `binary format`/file-write substrate
   (the format-c and binary-scan i*/I* pieces landed in S35d/S53), the
   interleaved row-callback step.
5. **Engine residue cards**: pager1 4.4.2 directory-hierarchy machinery,
   S34's two journal cards, temptable2-9.1.x/9.2.x (pre-existing
   small-cache stress dithers, watchlist), where2-2.4 random() flake,
   the resetdb load flap (watchlist).
6. **Performance phase** (F1c/F3 lanes in GAP_CLOSURE_PLAN_2026-10-31.md)
   — after functional gaps close.
