# PLAN S35f — the wal-hook surface (e_walhook, 10 leaves)

## C contract (pinned from source)

- **doWalCallbacks** (vdbeapi.c:723-742): runs at the END of sqlite3Step
  when `rc==SQLITE_DONE && db->autoCommit`. Per btree:
  `nEntry = sqlite3PagerWalCallback(pager)` (pager.c:7537 →
  sqlite3WalCallback wal.c:4415 — **read-and-clear** of pWal->iCallback);
  `if( nEntry>0 && xWalCallback && rc==OK ) rc = xWalCallback(...)`;
  the hook's rc becomes the statement's error (rc=SQLITE_ERROR,
  p->rc=the raw code → sqlite3ErrStr's text). The commit itself stands
  (R-14034-00929).
- **iCallback** (wal.c:4237): set at commit to `iFrame` — the TOTAL frames
  in the log after the commit (R-18913-19355: nEntry = the pages currently
  in the -wal, including those just committed). NOT the per-commit delta.
- The hook runs after the commit + the write-lock release
  (R-65366-15139): 2.1's in-hook db2 read must see the committed row.
- Explicit-transaction statements never dispatch (C's autoCommit guard);
  the COMMIT statement's own tail does (autoCommit is true again by then —
  e_walhook-1.4's +1 at COMMIT).
- **wal_autocheckpoint=N** (main.c:2496): sqlite3_wal_hook(db,
  sqlite3WalDefaultHook, n) — REPLACES any script hook; the GET reports N
  only when the current hook IS the default (pragma.c:2414), 0 under a
  script hook. Registering a script hook disables the autocheckpoint.

## Delivery items

1. **pager**: remove wal.go's direct `p.walHook(appended, 0)` (wrong
   position + the rc discarded); set `p.walCallbackEntry` = the total
   mxFrame at commit; add `TakeWALCallbackEntry() int` (read+clear).
   Drop the pager-level walHook field/SetWalHook (the engine-level hook
   becomes the only path).
2. **exec**: `fireWalCallbacks` at Engine.Exec's success tail (C's
   doWalCallbacks): autocommit-only, per-db read-and-clear, the hook's rc
   → the statement error via a sqlite3ErrStr subset (1→"SQL logic error",
   5→"database is locked", 14→"unable to open database file", ...).
   Change the hook signature to C's shape: `func(dbName string, nEntry
   int) int` (the Go API + TestWalHookEngine move with it).
3. **pragma**: WalAutoCheckpoint SET also clears the script hook
   (e.SetWalHook(nil)) — the default-hook replacement.
4. **converter**: `db wal_hook NAME` → a `register_wal_hook` step
   carrying the CURRENT def of NAME (i.procs — the corpus redefines
   my_wal_hook between registrations); plus a runtime_expr emission for
   top-level `set ::NAME <literal>` globals (the 1.1.x pre-set — plain
   sets currently emit nothing).
5. **harness**: the register_wal_hook runtime handler (the
   register_busy_handler pattern): build the fixture interp, sync the
   NotifyScriptVarTargets vars in/out around each invocation, call the
   proc with (dbName, nEntry), parse the integer result (default 0).
6. **regenerate** testdata/e_walhook.json from the pinned
   ori/sqlite/test/e_walhook.test.

## Verify

- FRIGOLITE_TEST=e_walhook — 10→0 (raw-scan name-set diff).
- TestWalHookEngine updated with the signature; go vet . (test files).
- Identity: wal2/walvfs/walckptnoop/wal/e_walckpt/journal2/walmode +
  the full gate at the unit merge (the statement-tail dispatch touches
  every statement — the FULL gate is mandatory).

## STATUS (2026-11-18): DELIVERED — e_walhook 10→0

- **The engine dispatch** (vdbeapi.c doWalCallbacks): Engine.Exec's success
  tail → fireWalCallbacks (autocommit-only, per-db read-and-clear of the
  pager's walCallbackEntry = the log's TOTAL frame count — wal.c:4237/4415),
  the hook's rc → the statement error via walHookErrStr (the sqlite3ErrStr
  subset). The hook signature moved to C's shape: func(dbName string, nEntry
  int) int. The old direct call in walWriter.commit (wrong position: the
  hook must fire AFTER the commit + the write-lock release, R-65366-15139)
  is gone.
- **wal_autocheckpoint=N** replaces the script hook (main.c:2496) —
  WalAutoCheckpoint now also SetWalHook(nil).
- **The converter**: `db wal_hook NAME` emits register_wal_hook (the CURRENT
  def snapshot; the main.go step-filter list gained the type) + a
  re-registration on later `proc NAME` redefinitions (C holds the proc NAME —
  tclsqlite.c rebinds at call time; i.walHookProcs tracks) + top-level
  `set ::NAME <literal>` emits a runtime_expr (the 1.1.x pre-set).
- **The harness**: the register_wal_hook handler (the register_busy_handler
  pattern) + the hook interp's `sqlite3 NAME FILE` opener (e_walhook-2.1's
  in-hook db2 read) + the runtime_var expect evaluation through the runtime
  command layer (`[list aux [wal_frame_count test.db2-wal 1024]]` —
  wal_frame_count = (size-32)/(pgsz+24), wal_common.tcl) + the
  lifecycle-file promotion scan (a hook proc body opening test.db —
  harnessBodyTouchesLifecycleDB — e_walhook's main must be the file).
- **Latent pager unit breaks fixed** (pre-dating this unit, unexercised
  since the WAL slice): the WAL-mode AllocatePage header-stamp on a nil
  cached header (a panic — the length guard); TestWalCheckpoint's
  creation-flush-relative premise + the RESTART observables (the log resets
  — the file truncates in the frigolite RESTART contract — the next commit
  writes frame 1 again); the pager-level Checkpoint seam now ends the
  caller's read snapshot first (C's checkpoint runs outside any read
  transaction — wal.c:2347's exclusive read-lock take would deadlock on the
  caller's own mark); TestWALCallbackEntry pins the read-and-clear contract.

---

# S35g slice 1 (2026-11-18, in flight on the same arc)

- **walfault2 2→0**: the committed testdata/walfault2.json was a STALE
  conversion (the setup leaves only — the faultsim_save/trial steps were
  absent). Regenerated from ori (the source copied from the 3.51 tree —
  the pinned corpus lacked it): setup_1/setup_2's faultsim_save_close
  steps trigger the lifecycle promotion (the file-backed main — the
  setups' journal_mode=wal now answers "wal"), the trial leaves run the
  real faultsim grid. Harness: runFaultsimStep gained close_db (the trial
  prep's `catch { db close }`) and the URI-form reopen
  (file:test.db?psow=0 — normalizeURIPath + Open's URI handling).
- **walsetlk_recover superseded by the native port**
  (TestWalSetlkRecoverEngine): the testfixture_nb subprocess class (the
  background recovery against a delayed VFS read) is not transpilable.
  The native test drives the same engine-visible contract: a concurrent
  recovery-in-progress holds the WRITER+RECOVER locks, a second
  connection's read reports "database is locked" after its busy timeout
  (1.2), the busy path sleeps through xSleep (1.5.2's sleep_count>0), the
  read succeeds post-recovery (1.4). Listed in unsupportedTestFiles with
  the pointer.
- **C-shaped plumbing this forced**:
  - The recovery SPLIT (walrecover.go): the read phase
    (walIndexRecoverRead — the -wal file reads, the xRead hook sites)
    runs OUTSIDE the wal-index writer section; the apply
    (walIndexRecoverApplyLocked — the hash populate + the header publish)
    inside. C builds the hash in private buffers — the old
    section-enclosed recovery let a parked fixture xRead deadlock every
    other connection's index probe (the native test exposed it). The
    commit-path callers keep the section-enclosed form
    (walIndexRecoverLocked).
  - The xRead emissions at the recovery's -wal reads (C's sqlite3OsRead
    on the wal fd).
  - The xSleep emissions at the WAL busy-sleep sites (C's sqlite3OsSleep
    under the busy path).
  - PRAGMA busy_timeout implemented (PragTyp_BUSY_TIMEOUT — it had NO
    handler!) and wired to the WAL layer (Engine.SetBusyTimeout pushes to
    the pagers; the shm lock loops honor the deadline).
- **Remaining carded**: walfault-11 (the trial-cap — 789s per run),
  walfault-12 (the trial prep's register_testvfs/testvfs_config — the
  fixture registry threading into runFaultsimStep), walfault-15 (the
  signature shifted to "prep failed: database is locked" — the exclusive
  INSERT's lock interplay with the trial state; the JOIN-flap card
  stands).
