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
