# Session handoff — the WAL residue slice (2026-11-04, through 3dc008e81)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". The working tree is CLEAN at 3dc008e81 (feature/perf,
105+ commits ahead of origin). Everything below is verified state.

## Where the corpus stands

- **Residue census** (`portplan/RESIDUE_CENSUS_2026-11-04.md`, commits
  c7e506320 + e80befc4f): the full-suite root package fails ~2475 subtests
  in one run, but the TRUE family-level backlog is **4,162 isolated subtests
  across 254 families** (every failing family fails harder isolated; zero
  families pass isolated). The census table is the priority map. Known
  caveat recorded there: `FRIGOLITE_TEST` matches by SUBSTRING
  (strings.Contains) — "wal" runs the whole wal* group; per-family truth
  comes from parsing `TestSQLiteSuite/<family>/<case>` paths in a run log.
- Known pre-existing engine failure OUTSIDE the harness:
  `TestWalCheckpoint` (internal/pager/wal_test.go:347) — TEST DRIFT, carded,
  no engine change planned (it drives AllocatePage/WritePage/Flush
  directly, a path that no longer appends the commit frame).

## Landed this session (9 commits, oldest → newest)

| commit | slice | effect |
|---|---|---|
| 6b1436b1a | S5: anonymous-catch rc surface (`__catchrc_<msg>`) + open_named_db installs the step's NAMED `-vfs` fixture (129 hook-less opens fixed) | walvfs 12→10 |
| a633c3b5b | S5b: page-1 frame in WAL commits (C's allocateBtreePage append branch, btree.c:6737+6761 — WAL-mode-only, size-bytes-only into page 1) | walvfs 10→9; fresh-db WAL byte-exact [1..22] vs the C oracle |
| 0bb49bb37 | S5c: checkpoint busy triple {1 -1 -1} (pragma initializers survive a WRITER/CKPT lock veto) | walvfs 9→8 (7.1) |
| 2ec4b187f | S5d: second-connection WAL observation — pagerOpenWalIfPresent port (pager.c:3337) on every shared lock, BtreeSetVersion(2) before the -wal opens, the writer's READ_LOCK(0) pin gated on the LIVE shared header | wal-14 class + walbak-4.1/4.2 |
| 9afec942b | S6: stale WAL-fixture regen (13 fixtures predated the converter's testvfs capture), xOpen-handle surface (test_vfs.c:638 — the script's return names the connection, passed as callback arg 3), main-path delete_glob | wal3 112→5, walhook 11→0 |
| 6b6403735 | S7: sqlite3_wal_checkpoint{,_v2} command handlers → wal_checkpoint_v2/wal_checkpoint_rc steps; harness reuses the engine's WalCheckpoint pragma path; do_test promotion attaches expectations only to a DIRECTLY-trailing checkpoint call, the catch-all never stamps these steps | e_walckpt 139→136 |
| aa264ba33 | S7b: db-hash surface — commandCatch executes QUOTED bodies; catch-list rc refs resolve to the carrier actually stamped; WalCheckpoint mirrors main.c (unknown zDb → SQLITE_ERROR "unknown database: <name>", NULL zDb → every attached db, the triple reports the FIRST checkpointed db, BUSY accumulates, non-WAL leaves -1s); harness rejects a bad MODE with SQLITE_MISUSE "not an error" and composes the wrapper's [sqlite3_errcode] - msg prefix; save_db_hashes/compare_db_hashes emit purpose-built md5 steps | e_walckpt 136→129; 1.1.x/2.1.x green end-to-end |
| c7e506320/e80befc4f/cb5ac869b/d2de43985/1437fe2dd/c4a03b9ae/3dc008e81 | plan-doc + census + lessons records | — |

## S7d RECORD (2026-11-05, eec79677c + 935063d2d)

- eec79677c: the locking_mode flip — the PRAGMA records the request only
  (SetWALExclusiveRequested); the WAL flips to exclusive shm mode at the
  first write (WALBeginWrite releases the reader's mark byte through the
  PAGER hook channel — the wi-level global hook doesn't carry the
  fixture) and back at the first transaction ending under normal
  (WALEndRead re-acquires the byte first; a veto/conflict silently stays
  exclusive). locking_mode/wal_checkpoint pragmas pin no read mark
  (stmtSkipsWalReadPin).
- 935063d2d: ATTACHed pagers route through the parent's fixture
  (Engine.AttachedPagers + Pager.HasVFSHook); tvfsResultCode maps xWrite/
  xSync script returns (test_vfs.c:381/453); wal_checkpoint_v2 composes
  sqlite3ErrName " - " errmsg for bare VFS codes.
- Results: wal2 57 -> 51 (the whole 6.4 block + three 6.6 exit tests),
  e_walckpt 5 -> 4 (5.3.1 fixed), wal sweep 202 -> 201, 0 new throughout.

## S7c RECORD (2026-11-05, 834a5bef1) — e_walckpt 129 -> 5

Three commits: eb2dc2f0e (engine: CKPT-first order, the busy-handler
counting session per statement, demote-to-PASSIVE on writer-lock give-up
with the demoted triple {1, mxFrame, nBackfill}, per-phase handler loops,
mini WriterSections — nested sections self-deadlocked once, caught by the
goroutine dump), f91bd0c5f (harness: unset-filter = ALL methods per
test_vfs.c:1574; the tvfs interp builds lazily when the script binds after
the open; the db-file sync gated on mxSafeFrame==mxFrame), 834a5bef1 (the
register_busy_handler step + the callback-dispatch precedence +
WALWriterProbe BEGIN gate + page-deduped backfill + catch-list/mode-parse
fixes). The full story: portplan/PLAN_e_walckpt_S7c.md §OUTCOME.
Remaining e_walckpt: 4.2-4.5 (sidecar lifecycle downstream), 5.3.1
(IOERR-on-aux). NEW LESSONS: the empty-failure-set trap (timeout/build
kill) bit four times — see .agents/lessons_learned.md.

## THE PREVIOUS RESTART POINT — e_walckpt (129 isolated failures)

The next unit was JUST STARTED: the mode-sweep choreography class. Triage
state:
- **1.full/restart/truncate.x (no-such-table + COMMIT classes, ~45)**:
  e_walckpt-1.full.1.14 = copy test.db → abc.db, open db4 on abc.db,
  `SELECT * FROM t1 UNION ALL SELECT * FROM t2` → "no such table: t1" (the
  copied main file lacks the checkpointed content); 1.full.2.7/3.7 =
  `COMMIT` "expected error but got success" (a second connection's read
  snapshot should block the checkpoint/wrap). The C source region starts at
  e_walckpt.test's `foreach {tn mode}` loop (the full/restart/truncate
  sweeps) — the last command run before the pause was a malformed `sed -n`
  on that grep, re-run with `grep -n "foreach {tn mode}" ../sqlite/test/e_walckpt.test`
  and read the loop from there.
- **1.passive.1.6-8 + 1.3.2.2 (callback counters, ~10)**:
  busy_handler_counter got "" want 0; sync_counter got "0" want 1 (the
  checkpoint's WAL-header sync callback); write_count got "" want 2 (the
  checkpoint's copyback xWrite events) — the engine callback surfaces
  (busy-handler invocation counting, xSync at the header sync under the
  fixture VFS, xWrite during backfill) not wired to the harness's
  callback-recorded runtime vars.
- Failure-name-set baselines for diffing: /tmp/pb-before.txt (136-set),
  /tmp/pb11.txt (the current 129-set) — REGENERATE both (the /tmp files may
  not survive a restart): `FRIGOLITE_TEST=e_walckpt go test -run
  "^TestSQLiteSuite$" -count=1 . | grep -oE "FAIL: TestSQLiteSuite/e_walckpt/[^ ]+" | sort -u`.
- Verification protocol: fixture regen via
  `mkdir -p /tmp/x && go run ./tools/tclconvert -testdir ../sqlite/test
  -outdir /tmp/x -only e_walckpt && cp /tmp/x/e_walckpt.json testdata/`;
  go test rebuilds the binary itself, but for /tmp/frigolite.test-style
  runs `go test -c -o` FIRST and never trust an older binary (the
  strings-grep check saved this session twice).

## Queued units after e_walckpt (designs recorded in TESTVFS_P_C1_PLAN.md)

1. **e_walckpt remainder** (the two classes above).
2. **Regen-drift follow-up**: e_walauto/interrupt2/wal5 also call
   sqlite3_wal_checkpoint — their committed fixtures predate the S7
   handler; regenerate + battery per the S6 procedure.
3. **wal2 (61) + wal3 (5) xShmLock trace residue** — post-regen triage
   (the fixtures are now current; the residue is genuine).
4. **wal-18.2.x interrupt choreography (24)** — sqlite3_interrupt +
   sqlite3_memdebug_fail inside xWrite callbacks across close-time
   checkpoints.
5. **Fix B: padToSectorBoundary** — walvfs-5.3's 24 frames: `tvfs devchar
   sequential` (set in walvfs.test:61, never reset) DROPS
   POWERSAFE_OVERWRITE → walFrames repeats the last commit frame to the
   4096-byte sector boundary at synchronous=FULL (22 pages + 2 padding).
   Needs the PSO/sector-size devchar surface in vfshooks.go + the WAL
   writer's padding loop (wal.c:4170-4188) + the tvfsGetDeviceCharacteristics
   override semantics in the harness.
6. **Sidecar lifecycle**: the WAL→rollback switch must delete -wal/-shm +
   reset the version bytes to 1/1 (C's walModeChange + BtreeSetVersion(pBt,1)).
   The naive port broke wal-24.x reopens ("file is not a database") — plan
   the checkpoint/flush ORDER first. Unblocks walbak-4.3.1/4.3.3.
7. **Then the census's non-WAL top families**: index 200, rtree 126,
   tkt 124, where 111, expr 109, pager1 101 — triage each with the census
   methodology (failure-class bucketing by message, C-source contract,
   name-diff verification).
8. **Performance phase** (the objective's second half): untouched. The
   census gives the functional-completion gate; the perf lanes (F1c/F3 in
   GAP_CLOSURE_PLAN_2026-10-31.md) follow.

## Methodology (mandatory — violations cost hours this session)

- **Failure-name-set diffs, never counts**: `grep -oE "FAIL:
  TestSQLiteSuite/<fam>/[^ ]+" | sort -u` + comm both directions. Counts
  swing ±35 under load (pure HEAD reproduces it).
- **No stash roundtrips for baselines** — a pop takes stash@{0}, which
  after any push isn't your baseline; pops conflict and reorder. Commit
  each verified unit immediately; use a worktree for HEAD baselines.
- **Binary-verify instrumentation**: `go build ./...` does NOT compile
  _test.go files; a 0-failure grep can be a build error. `go test -c -o`
  + `strings <bin> | grep <trace-tag>` before trusting trace silence.
- **FRIGOLITE_TEST is Contains-based** — "wal" sweeps the whole group.
- **The stale-fixture signature**: got [""] runtime-var mismatches +
  cascading "file is not a database" reopens = regenerate the fixture
  first (`-only <fam>` to a temp dir, diff against committed) before
  root-causing engine behavior.
- **The oracle**: ../sqlite/testfixture runs via a dylib shim —
  `DYLD_LIBRARY_PATH=/tmp/tclshim` with libtcl9.0.dylib → 9.1 symlinked
  (rebuild the shim dir if /tmp was cleared). ../sqlite is 3.51.0;
  /usr/bin/sqlite3 is 3.54 — version-tag every oracle observation.
- Lessons file: .agents/lessons_learned.md (reviewed/extended every unit).
- Plan docs: portplan/TESTVFS_P_C1_PLAN.md (the WAL-slice log with per-slice
  records), portplan/GAP_CLOSURE_PLAN_2026-10-31.md (progress rows).

## S7e/S7f/S7g RECORDS (2026-11-06/07) — the WAL residue slice is CLOSED

- S7e (1ab97052c, b72123592, 5d9f97c3f): the sidecar lifecycle — the
  WAL→rollback switch (leaveWALModeLocked, ONE w.Close()), the close-time
  sidecar deletion (w.path IS the -wal path; the first attempt removed
  "t.db-wal-wal"), the 2/2-header reopen re-attach + the open-time
  garbage rule. wal-24.2, walbak-4.3.1/4.3.3 pass. Detail:
  PLAN_S7e_sidcar.md.
- S7f (47e9165de): the backup_file step (`db backup FILE`) +
  backup.c:430's destination journal-mode parity. walbak 23 → 9.
- bc722d011: the harness refs=2 connection leak — reopen_db /
  open_named_db now CLOSE the replaced engine (C's tester.tcl
  semantics). wal sweep 156 → 147 (wal-12.4 + the wal-24.2/setup_78-86
  class fixed). NEW residue: walfault-11 — the per-reopen close-time
  checkpoints multiply faultsim's fault-injection schedule → the
  512-trial cap (874.71s in the S7g sweep). Carded, same class as the
  wal-3 xShmLock surplus.
- S7g (f85b1f905): the sqlite3_backup command object (test_backup.c) —
  the backup_db step, the harness runner over Backup.Step with honest
  return-code expectations, Finish's BUSY guard (backup.c:599-603).
  **walbak 9 → 0** (96 subtests). Other dropped-step fixtures
  (backup*.test, pagerfault.test, multiplex3.test) can regenerate in
  their own units.

**Restart point**: continue TODO.md's queue from item 3 (regen-drift:
e_walauto/interrupt2/wal5 fixtures predate the sqlite3_wal_checkpoint
handler), then the census families. The wal-sweep failure baseline is
portplan/baseline_wal_sweep_2026-11-07_S7g.txt (138 names).

## S8 RECORD (2026-11-07, 351936d39) — the regen-drift battery + the copyback interrupt check

- The regen-drift battery (queue item 3): interrupt2.json regenerated
  (the converter captures the testvfs surface) and UNSKIPPED — 35
  subtests, 8 failures, all one engine root cause. e_walauto carded
  unsupported (its core reads the -shm bytes via read_nbackfill/
  read_mxframe binary scans — not expressible as engine steps; the old
  fixture was internally broken). wal5: the converter captures nothing
  (lock_common primitives) — the old green fixture stands.
- ENGINE (wal.c:2289-2292): the checkpoint copyback checks the
  connection's interrupt flag PER PAGE and aborts SQLITE_INTERRUPT with
  the log's frames standing. Pager.interruptProbe (closure, like the
  busy handler) bound at NewEngine + AppendDBList;
  ErrCheckpointInterrupted → the pragma's "interrupted"; the harness's
  checkpoint_v2 composition renders "SQLITE_INTERRUPT - interrupted".
  Native pin: TestCheckpointInterruptPin.
- Converter: db_save/db_save_and_close/db_restore/db_restore_and_reopen
  alias the faultsim snapshot pair (malloc_common.tcl:169-172).
- Results: interrupt2 36/37 (residue 2.1: the per-row callback that
  arms an interrupt and interleaves a checkpoint mid-scan — needs an
  interleaved row-callback step surface); walvfs = the exact S7g
  baseline set; wal sweep 136 = 138 − 2 (e_walauto's card), zero new.
  Baseline: portplan/baseline_wal_sweep_2026-11-07_S8.txt.

**Restart point**: TODO.md's queue item 6 (the census families) or the
wal2/wal3 xShmLock trace residue (§Queued item 3). Detail:
portplan/PLAN_S8_interrupt.md.

## S9-S12 RECORDS (2026-11-07/08) — the census's index slice opens

- S9 (73bee149a): wal-18.2.x's failures were the converter's missing
  binary-format crafting surface, NOT interrupt choreography. Native pin
  TestWalLogPageSizeValidation + 24 skips; the ENGINE fix the pin
  exposed: openPager's error path dropped the attached WAL without
  closing it — the wal-index ref leak pinned a stale shared header over
  every later open of the same path. wal 136 → 112, zero new.
- S10: walFrames padToSectorBoundary (wal.c:4172-4190) — the padding
  frames are real frames (mxFrame/aFrameCksum include them); the
  unix-VFS PSO default; the REAL sqlite.h IOCAP values (0x400/0x1000);
  the pager's WAL sync-flag default FULL (SQLITE_DEFAULT_SYNCHRONOUS=2
  is PRAGMA-numbering). walvfs-5.3 green; sweep 112 → 111, zero new.
  Detail: portplan/PLAN_S10_padtosector.md.
- S11 (c96cf0782): PRAGMA wal_autocheckpoint (pragma.c:2409 — the row
  surface; the default-hook/custom-hook gate). wal2 37 → 23.
- S12: the ORDER BY collation-aware index matcher (where.c azColl name
  rule; the stale stored order delivers — e_reindex 2.3.x) + build.c's
  REINDEX mode resolution (collation precedence R-35892-30289; the
  single-object table/index resolution temp→main→attached; per-schema
  emission b-trees) + the harness collation shape fixes
  (sort_by_length/sort_by_value) + the e_reindex-2.0/2.6.0 skip removals
  (the skips starved the whole section-2 setup) + two hand-pinned
  index.json cases. **e_reindex 95 → 0, index 2 → 0; the index census
  sweep 200 → 101, zero new.** Detail:
  portplan/PLAN_S12_orderby_collation_index.md.

**Restart point**: verify the S12 full-suite run (per-family counts vs
the census — an increase is a regression), commit S12, then the census
families rtree 126 / tkt 124 / where 111 / expr 109 / pager1 101. The
WAL residue is the carded callback/byte-manipulation classes (see
TODO.md's queue item 7).

## RTREE SWEEP TRIAGE (2026-11-08, post-S12, /tmp/rtree_run.log)

The rtree census family (126 isolated) decomposes into per-unit classes
(the sources live in ../sqlite/ext/rtree/*.test — NOT ../sqlite/test):

- **rtreedoc2 (28)**: `box(...)` is a `sqlite3_rtree_geometry_callback`
  registered by the C-API command `register_box_geom` (test_rtreedoc.c)
  — the MATCH-geometry surface (the SQL function returns the opaque
  rtree-geometry handle; xQueryFunc filters). Unit: the geometry-
  callback registry in the rtree MATCH path + the harness step. Also
  the callback-state assertions (the ::box_geom trace).
- **rtree1 (26)**: the 12.x class — column-level UNIQUE on rtree aux
  columns + UPDATE/INSERT OR ROLLBACK|ABORT|IGNORE|FAIL|REPLACE
  semantics over vtables + the `sql_uses_stmt` C-API assertions
  (sqlite3_stmt_readonly class). Two table flavors per case (#01
  copies); the second flavor's UNIQUE didn't fire in ours.
- **rtreedoc (15), rtreeA (15), rtreeJ (11)**: row-order/insert classes
  needing individual triage (rtreeA-2.x "got: [lob]" looks like a
  quoting bug; rtreeJ-2.x row sets).
- **rtreefuzz001 (8)**: "no such table: t1/geo1" — converter setup
  drops (stale-fixture class; the source is ext/rtree/rtreefuzz001.test).
- **rtreecheck (5), rtree8 (5)**: assorted; rtreecheck's swap_int32
  UPDATEs corrupt the shadow tables directly.

## S13-S16 RECORDS (2026-11-08, the tkt census slice) — the duplicate-generation discovery

- S13 (74c173e27): tkt-bdc6bbbb38's 16 got-[] failures were a stale
  fixture missing its `set sqlite_fts3_enable_parentheses 1` step (the
  converter's set_fts3_parens special-case + the harness's TclVarSet
  bridge + fts3ParenthesesEnabled all existed). 13 flag-setting fixtures
  regenerated; the harness query arm's error path now stamps the catch
  carriers (a catch-wrapped probe query whose query errors IS the
  captured result). fts3 sweep 121 -> 118, zero new.
- S14 (7a37980ba): **48 underscore-named tkt fixtures were stale-
  generation duplicates of the dash-named siblings** — the same sources
  ran twice, the stale ones truncated (missing setups -> "no such
  table" cascades, missing UDF registrations). Deleted; every tkt-*
  source regenerated with the current converter. tkt census 124 -> 55.
- S15 (ecfdf622c): the corpus-wide duplicate scan found 3 more
  (fts_9fd058691, fuzz_oss1, vacuum_into) — removed.
- S16 (f81066ddf): the order-distinct mark-off honors UNIQUE-index NULL
  semantics — an IS NULL equality over a nullable unique-prefix column
  voids the mark-off (duplicate NULL keys); the remaining ORDER BY term
  keeps its sorter. tkt3824 6 -> 0; where 111 / index 101 isolated runs
  match their baselines exactly.
- Full-suite gates: S12 2014 -> S15 1929 failing subtests, no family
  worse. THE CENSUS NUMBERS WERE INFLATED by the duplicates — re-derive
  family truth from the case paths before scoping any unit.

**Restart point**: the remaining tkt classes (tkt-31338dca7e 8 — LEFT
JOIN chains, tkt2820 4, tkt1644 3 — C-API finalize surfaces), then the
rtree class map (the previous record), where 111, expr 109, pager1 101.

## S18-S19 RECORDS (2026-11-08, continued)

- S18 (215dd7ae1): the same-connection active-statement DDL lock —
  Engine.RegisterStmtReadTables tracks each started statement's read
  tables (registered at the first Step, dropped at Reset/DONE/finalize —
  the C cursor window) and execDropStmt fails SQLITE_LOCKED "database
  table is locked" when sqlite_master or the target is being read
  (tkt2820). The C-API emulation records step/finalize RESULT CODES
  (__STEP_/__FIN_ expansion) — a step error is observable state, not a
  harness failure — and a statement whose step failed after the
  connection's SchemaMutationStamp moved reports SQLITE_SCHEMA from
  finalize (tkt1644). tkt 41 -> 33.
- S19 slice 1 (42ec0f866): the engine's geometry-callback machinery
  (rtree_geom.go) ALREADY existed — the converter dropped the
  register_circle_geom/register_box_geom commands. The box callback
  ported (rtreeBoxGeom: (min,max) pair overlap; the OnCell observer runs
  even on the nParam!=nCoord error path), the converter emits
  register_rtgeom steps, the harness installs the bundles. rtreedoc2
  29 -> 11 (the fixture refreshed to 46 cases).
- **S19 slice 2 (carded)**: the 11 remaining rtreedoc2 failures are the
  ::box_geom/::ptr_list trace assertions — the fixture-proc interpreter
  must drive the box callback's OnCell observer (per-case `proc
  box_geom` redefinitions rebind; the recorder executes the CURRENT
  proc: lappend the trace / error / return "zero"|"user" per
  test_rtreedoc.c's result protocol), and the trace elements render
  floats through the TCL formatting.

## S19 SLICE 2 RECORD (2026-11-08) — rtreedoc2 fully green

- The recorder (rtgeomBoxFixture) evaluates the CURRENT `proc box_geom`
  body through a tcl.Interp per engine cell visit, with the C
  invokeTclGeomCb argument shape ("box", ctxPtr, {params}, {coords},
  geomPtr — the floats through the TCL double rendering, the marker's
  pointer as the same-struct identity). The result protocol: "zero"
  adjusts aParam, "user" arms the query-end finalizer (the per-step
  finishQuery hook), "user_is_zero" errors once armed, a proc error
  aborts the MATCH as "SQL logic error".
- The callback interpreter's globals sync into the runtime set after
  every invocation; the runtime_var_reset/runtime_expr assignments reach
  the interpreter too (the counter resets between cases).
- ENGINE: the box callback's per-call order matches C — the
  nParam!=nCoord check uses the PRE-observation count (the "zero"
  adjustment lands on the NEXT call's check — C's 3.3 trace has FOUR
  elements with the fourth recorded post-adjustment), and the overlap
  reads the pre-call parameters when the observer changed the count
  mid-call (C reads the stale tail of the original aParam allocation).
- Converter: rtgeom_proc update steps for every later proc definition
  after a box registration (the callback's own name and the procs it
  calls); the box bundle's write targets seeded at registration, at
  each redefinition, AND in the whole-source pre-scan (the
  `set ::box_geom_calls 0` resets precede their proc definitions).
- The 6.3 case's run-length compression hand-pinned as a tcl_expr step
  (the harness evaluates plain-TCL do_test bodies through the recorder's
  interpreter; the $variable expansion must be SKIPPED for it — textual
  inlining corrupts the parse).

**rtreedoc2: 0 failures** (the whole 46-case family).

**Restart point**: the census families where 111 / expr 109 / pager1
101, then the WAL residue classes.

## S20 RECORD (2026-11-08) — the search-count surfaces (where 102 -> 94)

- The whereD do_searchcount_test helper: the converter emits the query
  with SearchCount + the labeled SearchStyle (" search N"; where8's
  execsql_status2 stays bare " N").
- The unique-scan plan shape: a UNIQUE index fully bound by equality has
  NO Next loop in C's bytecode — the seek path's Next counting is
  suppressed for that shape (whereD-3.4.1 = seek + row fetch = 2).
- The OR-plan's movetos are covering-aware: orPlanCovered checks every
  column the SELECT references against the branches' key columns; a
  covered query counts seeks + arm Nexts only (whereD-3.1 = 4),
  uncovered adds one moveto per row (3.2 = 6).
- C source pins: vdbe.c:4973 (the seek family), vdbe.c:6530
  (OP_Next/OP_Prev's next_tail), vdbeaux.c:3809 (sqlite3VdbeFinishMoveto
  — the deferred row fetch DOES count).

**Restart point**: the remaining where classes (subquery-in-WHERE
search accounting whereD 3.4.2-3.5.x/6.3.2-6.6.4; where8-3.x's
db-status-step counter), then expr 109 / pager1 101.
