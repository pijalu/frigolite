# testvfs P-C1 — fault-injection VFS + WAL sync-event model (walvfs slice first)

Sized by triage: walvfs (12) + pagerfault (58) + e_walckpt (272). This file
records the testvfs.c survey and the WAL sync-event contract derived from
walvfs-1.x so slice S1 implements from a complete design.

## testvfs.c survey (src/test_vfs.c, 1697 lines)

- `testvfs <name>` creates a named VFS; the object command exposes:
  `shm`, `delete`, `filter`, `ioerr`, `fullerr`, `cantopenerr`, `script`,
  `devchar`, `sectorsize` (test_vfs.c:1093-1107).
- `script <proc>` sets THE callback script; `filter <method>`... restricts
  which x-methods invoke it. Callbacks receive (method, filename, args...).
- `devchar <chars>` overrides SQLITE_IOCAP_* device characteristics
  (SEQUENTIAL gates the WAL header sync; POWERSAFE_OVERWRITE matters to
  journaling).
- `ioerr <count>/<failures>` = the fault-injection engine (S3: pagerfault
  and e_walckpt's IOERR classes; test_vfs.c's ioerr panic/fail counters).
- Opens name the VFS: `sqlite3 db test.db -vfs tvfs`.

## Our seam

internal/pager's `pagerFile` interface (pager.go:39) is the VFS seam —
the multiplex port already builds pagers over wrapped files. S1 adds a
testvfs decorator (per open, carrying the file path) that invokes the
fixture script on filtered methods, plus a device-characteristics
capability (`DeviceCharacteristics() int`) consulted by the WAL writer.

## The WAL sync-event contract (wal.c, pinned against walvfs-1.x)

Flags (pager.c:3636-3650, wal.h:25-26):
- `syncFlags`: 0 (synchronous=OFF) / SQLITE_SYNC_NORMAL (0x02) / FULL (0x03)
  — the ROLLBACK-journal sync bits.
- `walSyncFlags = syncFlags<<2`, plus `| syncFlags` only when fullSync.
  synchronous=NORMAL ⇒ 0x08; FULL ⇒ 0x0F; OFF ⇒ 0.
- `WAL_SYNC_FLAGS(x) = x&0x03` (commit bits); `CKPT_SYNC_FLAGS(x) =
  (x>>2)&3` (checkpoint/header bits).

Events (our WAL writer must emulate each C sqlite3OsSync site as a
decorator xSync callback):
1. **wal-header sync** (wal.c:4099): when the first frame of a (re)started
   WAL is written (`iFrame==0` before the append) and `syncHeader` — false
   when the file's devchar has IOCAP_SEQUENTIAL (wal.c:1731) or
   synchronous=OFF. Uses CKPT_SYNC_FLAGS — fires at NORMAL and FULL.
2. **commit sync** (wal.c:4187): only when `WAL_SYNC_FLAGS(sync_flags)!=0`
   — synchronous=FULL only. NORMAL never syncs at commit.
3. **checkpoint syncs** (wal.c:2260 the WAL, 2314 the db): CKPT_SYNC_FLAGS —
   NORMAL and FULL.
4. `padToSectorBoundary`/iSyncPoint (wal.c:3922): FULL-only padding sync —
   S3 detail (sector-size interactions).

Why walvfs-1.1 counts ONE sync at synchronous=normal: 1.0's passive
checkpoint copied the whole log and RESTARTED it (sqlite3WalCheckpoint's
walRestartLog sets mxFrame=0 when fully backfilled) — 1.1's INSERT writes
the WAL header again and syncs it (CKPT bits run at NORMAL). 1.3's zero is
the SEQUENTIAL skip on syncHeader. Our checkpoint path must implement the
full-copy log restart to reproduce this.

## Slices

- **S1 (walvfs) — LANDED (commit "P-C1 S1")**: converter (`testvfs <name>`,
  `<name> script|filter|devchar|delete <arg>`, `-vfs <name>` opens, file
  tail/dirname/extension, the late-proc post-pass, callback-target seeding),
  the pager hook layer (vfshooks.go: the observational xSync hook + devchar
  + the WAL sync-flag level), the sync events at C's four sites, and the
  writer's READ_LOCK(0) pin (walFrames' walRestartLog gate — without it the
  fully-backfilled log never restarts and the header sync never fires).
  walvfs 1.0/1.1/1.3 pass; e_walckpt 272->253 ('tvfs delete' = xDelete is
  the section reset); wal 482->463. OPEN: walvfs-1.2 ([0 2 2] matches the
  system-sqlite oracle; the fixture's [0 4 4] is a tvfs-environment frame
  contract), 3.x/4.x (synchronous=off/psow sync points), 5.5/5.6 = S2's
  xShm surface, 6.x-9.x = checkpoint/shm/protocol contracts.
- **S2 — LANDED (commits "P-C1 S2" + "P-C1 S2b")**: the xShmMap/xShmLock
  callback surface — hook returns map to pager.VFSHookError and translate to
  C's errstr (SQLITE_READONLY -> "attempt to write a readonly database");
  the pending post-install xShmMap check (C maps the -shm lazily at the
  first transaction, our pager attaches at OPEN); 'tvfs filter {}' resets;
  xWrite events at the copyback/frame writes + the fixture's
  sqlite3_interrupt; the whole-source PreScanCallbackTargets (file-level
  resets precede both the binding and the definition); the interp's
  if-without-else returns "". walvfs 12->5 (5.5/5.6/6.2/7.1/8.3/9.1 pass).
  REMAINING walvfs: 1.2 (tvfs frame contract), 3.1-4.1 (the interrupt
  counter choreography spans the close-time checkpoint of the PREVIOUS
  connection — the fixture's ::cnt carries across connections; needs the
  S3 fault machinery's connection-lifecycle events). The `shm` subcommand +
  vfs_set_readmark (test_vfs.c:1637 — aShm[25+slot] read/write) remain for
  the readmark-dependent leaves.
- **S3 — DESIGN COMPLETE (survey 2026-11-03d)**: pagerfault does NOT use
  testvfs for ioerr — it uses malloc_common.tcl's `do_faultsim_test`
  framework over the GLOBAL `::sqlite_io_error_*` counters (os_unix.c's
  TEST block), plus tester.tcl's file-snapshot pairs.
  - **ioerr injector**: process-global counters — `ioerr_injectstart(persist,
    iFail)` sets ::sqlite_io_error_persist and ::sqlite_io_error_pending=iFail;
    EVERY OS-layer I/O (read/write/sync/truncate/open) decrements pending and
    fails with SQLITE_IOERR ("disk I/O error") when it reaches 0 — once
    (transient) or until reset (persistent). Port: a package-level injector
    in internal/pager consulted by every pagerFile I/O site.
  - **The faultsim trial loop** (malloc_common.tcl:326-390): for iFail=1,2,…
    — prep → injectstart → body → injectstop (nfail = the hit count) → the
    harness asserts `{rc res} == {0 ok}` (the engine must swallow each fault
    via retry/recovery) — **stop at the first trial whose nfail==0**.
  - **faultsim_save/restore** (tester.tcl:2463): file copies of test.db*
    (all sidecars) ↔ sv_test.db* taken in -prep and before each trial.
  - **The -test script** (faultsim_test_result_int): the (rc, result) pair
    must be in the -injecterrlist patterns; nfail==0 requires the CLEAN
    result.
  - **Port shape — IMPLEMENTED (commit 2a6d4f03a)**: converter
    `do_faultsim_test` → one dynamic step {type: "faultsim_test", Faults,
    Prep []TestStep, Body []TestStep, Test patterns}; the harness's
    runFaultsimTest owns the loop; faultsimFileOp does the snapshot pairs.
  - **The next session's identified fixes** (from the engine-pointer
    tracing): (1) the driver's reopens (faultsim_restore_reopen, reopen_db
    inside runFaultsimStep) and the reset path must install the DEFAULT
    fixture — the naive threading of installDefaultVFS into the driver took
    pagerfault 73→103 (the hook's events disturbed other trial steps) and
    was reverted; the correct shape: the driver's executor must install the
    default fixture ONLY on the connection the -default fixture governed at
    the recording point (the fixture's per-connection choreography needs a
    recorded install-context, not a global default). (2) pagerfault-2's
    "no such table: abc": the trial's prep restores the snapshot saved by
    the -pre1's xSyncCb (the cb fires on the journal-mode writes of the
    -default connection) — verify the save fires on the RESET connection
    (the reset_db install was wired but the traced dbsyncs still showed
    hook=false on the trial's connection). (3) the pattern expressions
    (`[list 0 $answer]`) need per-trial evaluation against the body's
    runtime variables (the interp machinery exists via runNotifyScript's
    pattern).
  *fault families) — test_vfs.c's ioerr counters + the harness fault steps.
- **S3 — LANDED (2026-11-04)**: pagerfault 73→**0** (all 36 sections), the
  driver's `-test` script evaluation, and the four structural fixes the
  trial loop needed. e_walckpt 253→141, aggregate wal 456→351 (the ioerr
  classes unlock); walvfs steady at 5 (S1/S2 residue unchanged); pagerfault2/
  pagerfault3 green; pager* aggregate 303→104. Battery regression-free
  (journal2 5, delete 9, insert 2, conflict 2, where 102, select1 1 — all
  identical to the pre-session HEAD baseline). The landed pieces:
  (a) converter `do_faultsim_test` records TestScript; the harness
  `evalFaultsimTestScript` runs the C faultsim_test_proc per trial
  (testrc/testresult/testnfail bound; `db`/named-connection adapters with
  eval/one/close; catchsql; a REAL sqlite3_finalize over the trial's
  prepared statements — a no-op finalize left the read transaction
  registered and blocked every later write with "database is locked").
  (b) the driver's `runFaultsimStep` handles reset_db (glob delete),
  register_function, open_empty_db, open_named_db/close_named_db (with
  TCL's replace-closes-the-old-command semantics), delete_file, delete_glob,
  prepare_stmt/stmt_step (per-trial scope, deferred finalize), and
  named-connection routing for exec/catch/query (step.Database).
  (c) C-order restore: db_restore_and_reopen closes the connection FIRST
  (tester.tcl:2482) — restoring under a live connection let its Close flush
  stale dirty pages over the fresh snapshot (the freelist-corruption seed
  behind the incremental-vacuum hang).
  (d) the snapshot pair is the test.db* GLOB (tester.tcl db_save/db_restore):
  ATTACH'd siblings (test.db2) travel with snapshots; db_restore
  force-deletes the whole glob first — hardcoded 4-suffix lists missed both.
  (e) the __RESET_DB__/reset_db handlers delete the whole test.db* glob
  (tester.tcl:2490) — a surviving stale -wal was recovered over the fresh
  file and poisoned the reopened header.
  (f) engine: readFilePageLocked zero-fills short reads (C readDbPage's
  SQLITE_IOERR_SHORT_READ swallow; the WAL-mode 0-byte-main path), and
  walFileHeaderSaltsMatchLocked also treats "MxFrame beyond the WAL file's
  frame capacity" as stale → rebuild from the -wal (a restored wal-index
  beside a restarted WAL claimed frames the file lacks).
  (g) converter: the foreach-glob-forcedelete idiom (`foreach f [glob
  -nocomplain test.db*] { forcedelete $f }` → delete_glob step); the trial
  exec paths route relative ATTACH operands through the fixture directory.
  (h) the trial loop propagates the last trial's connection back to the
  fixture (`db` follows the reopen) and the "no test patterns" check defers
  to the TestScript path (dynamic pattern cases).
  **REMAINING (S4)**: e_walckpt's 141 = the tvfs callback-counter classes
  (runtime_var mismatches from the xShm/xSync/xWrite observation surface),
  the `shm` subcommand + vfs_set_readmark (test_vfs.c:1637), walvfs 1.2/
  3.x-4.1 (the interrupt counter choreography across the close-time
  checkpoint of the previous connection), and the open_named_db shared-cache
  URI case inside trials (pagerfault-36's db2 — passes via the plain-URI
  path today). The pre-existing TestWalCheckpoint unit failure (internal/
  pager/wal_test.go:347, main file 0 bytes after checkpoint) is TEST DRIFT:
  it drives AllocatePage/WritePage/Flush directly, a path that no longer
  appends the commit frame; the engine's real commit path is exercised green
  by the WAL families — card queued, no engine change.
- **S4 — LANDED (2026-11-04b)**: the testvfs callback argument surface —
  every fixture callback now receives (method, file, shmId, details)
  exactly as C's tvfsExecTcl passes them (test_vfs.c:950-975): xSync's
  sync-flag string, xShmMap's region size, xWrite's byte count, xShmLock's
  "<ofst> <n> lock|unlock shared|exclusive"; a per-connection pShmId mints
  the third argument. The xShmLock events route through the CONNECTION's
  pager hook (every walWriter lock op fires it; vetoes propagate verbatim —
  BUSY drives the protocol, other codes flow to the caller's mapping).
  Filter semantics follow C's mask: only listed methods fire. The
  SQLITE_READONLY_* extended codes map to the base errstr (main.c:1547).
  The S2-era walvfs passes of 5.4-5.6/6.2/7.1/8.3/9.1 were ARTIFACTS of the
  xShmLock scripts never firing — the C-faithful firing replaces them with
  the real choreography gaps.
- **S4b — LANDED (2026-11-04c)**: vfs_set_readmark end to end
  (test_vfs.c:1637) — the pager ReadMarkSlot/WriteMarkSlot answer through
  the wal-index ckpt-info region (aShm[25+iSlot]), firing the fixture's
  xShmMap probe first; the converter captures both the statement form and
  the `list [vfs_set_readmark ...]` body-value form; the do_test promotion
  attaches the body expectation to the marked probe. walvfs-5.2 green.
  **REMAINING walvfs (12, engine units)**: the frame-count divergence (5.3:
  our 21 frames vs C's 24 for the same CTE insert — a multi-row INSERT's
  per-page frame accounting), the secondary-connection xShmMap veto (5.5's
  readonly-shm contract: db2's FIRST xShmMap must fail), the interrupt
  counter choreography (1.2/3.x — spans the close-time checkpoint of the
  previous connection), and the tvfs frame contract (1.2). **e_walckpt
  residue (139)**: the `checkpoint db <mode>` USER-PROC calls inside do_test
  bodies are dropped by the converter (proc capture + invocation for
  body-level helper procs — the checkpoint procs eval
  wal_checkpoint_v2/PRAGMA wal_checkpoint); the lock-counter classes then
  follow. **Corpus drift card**: a full `tclconvert` regen showed the
  committed corpus predates the converter state (null→[] serialization +
  real new faultsim cases in aggfault et al, ~663 files) — reverted; the
  regen + full battery is its own unit.
- **S4**: fullerr/cantopenerr/sectorsize/delete + the remaining option
  surface.

## Ground already landed this round

- WAL commits never bump the file header change counter (pager.c:6496's WAL
  early return) — walvfs-1.0's 5 frames byte-verified (+37 leaves across the
  WAL families).
- The WAL frame decoder (walview.go) is the byte-exact verification tool.
- **S5 — LANDED (2026-11-04d, commits 6b1436b1a/a633c3b5b/0bb49bb37/2ec4b187f)**:
  four walvfs-unit commits. (a) The anonymous-catch runtime surface:
  commandCatch stamps the emitted statement with a deterministic
  __catchrc_<msg> variable and the runtime_list's `[catch {...} var]`
  element references it brace-aware — the harness's existing CatchResult
  capture records rc + message (5.5/5.6). (b) open_named_db installs the
  fixture the step NAMES (-vfs tvfs) — the global default was installed for
  every open, leaving 129 vfs-carrying opens hook-less. (c) The page-1
  frame in WAL commits: C's allocateBtreePage append branch makes page 1
  writable and writes nPage into its image at every allocation-from-end —
  the commit frame list gains the page-1 frame (scratch WAL byte-page-exact
  [1..22] vs the C oracle; 8.3's {0 5 5}). WAL-mode only (C's
  allocateBtreePage never runs outside a write transaction — the open-time
  init allocates page 1 on a 0-byte db and the stamp would lead the file;
  HeaderBeyondFile would report the healthy image as truncated).
  Size-bytes-only into the cached image (a full-header mirror clobbers
  page 1's fresher meta bytes). (d) The checkpoint busy triple {1 -1 -1}:
  the pragma's nLog/nCkpt initializers stay when walCheckpoint never ran
  (a WRITER/CKPT lock veto precedes it — 7.1's WAL_CKPT_LOCK veto).
  (e) The second-connection WAL observation (wal-14 class): the
  pagerOpenWalIfPresent port runs on every shared lock (attach beside a
  non-empty main file, delete the -wal beside an empty one);
  OP_JournalMode's BtreeSetVersion(pBt, 2) stamps the version bytes into
  page 1 through the ROLLBACK path before the -wal opens (the main file
  becomes non-empty — what the attach check reads); the writer's
  READ_LOCK(0) pin gates on the LIVE shared header (nBackfill == mxFrame)
  — the eager pin's vacuous 0==0 on a zero cached header pinned
  "ignore the log" beside a non-empty log and the writer read zeroed
  WAL-only pages. walvfs 12→8; wal-14 + walbak-4.1.x/4.2.x green.
  **REMAINING walvfs (8)**: 1.2 (tvfs frame contract), 3.1/3.2/3.2.2
  (interrupt choreography), 4.2 (readonly-shm + lock counter), 5.3 (the
  frame-count 24: `tvfs devchar sequential` persists from section 1.2,
  DROPS POWERSAFE_OVERWRITE, and padToSectorBoundary repeats the last
  commit frame to the 4096-byte sector boundary at synchronous=FULL —
  22 page frames + 2 padding = 24; the S4 option surface needs
  powersafe_overwrite/sector-size handling), 6.2 (locking protocol),
  9.1 (CANTINIT class). **CARD**: the WAL→rollback switch leaves the
  "-wal"/"-shm" sidecars and the 2/2 version bytes (C's walModeChange
  deletes them; BtreeSetVersion(pBt, 1) resets them) — the naive port
  broke the wal-24.x reopens; needs its own planned checkpoint/flush
  unit (walbak-4.3.1/4.3.3).
- **S7 — LANDED (2026-11-04f, commit 6b6403735)**: the
  sqlite3_wal_checkpoint{,_v2} command surface — the checkpoint procs
  execute. e_walckpt's three foreach-script checkpoint procs inlined
  except their tails: `eval wal_checkpoint_v2 ...` /
  `eval sqlite3_wal_checkpoint ...` hit unknown commands and the whole
  checkpoint call dropped — no checkpoint ran, the xShmLock callback
  recorders never saw the CKPT lock (86x seen_checkpoint_lock got 0 want
  1), and the mode-sweep db copies held no checkpointed content. The
  converter gains the two command handlers (test1.c:7615's
  sqlite3_wal_checkpoint_v2 — the {busy nLog nCkpt} triple; the simple
  form — the result-code name; the schema rides FileTarget because the
  catch wrapper stamps Variable), emitting wal_checkpoint_v2/
  wal_checkpoint_rc steps; the harness cases reuse the engine's
  WalCheckpoint pragma path (the same {0 -1 -1} non-WAL contract the
  passing e_walckpt-2.2.b exercises). The do_test promotion attaches an
  expectation ONLY to a DIRECTLY-trailing checkpoint call, and the
  catch-all never stamps these steps — a trailing compare_db_hashes
  leaves the checkpoint's placeholder in the anonymous variable and its
  value belongs to the db-hash surface. 47 checkpoint steps execute in
  the regenerated fixture. e_walckpt 139→136, 0 new failure names,
  wal -2. **CARD (Phase B)**: the db-hash body-value surface —
  save_db_hashes/compare_db_hashes (md5file) and the
  `list [catch {...} msg] $msg` shapes own the 1.x/2.1.x/3.1.x/5.x
  expectations. **Follow-up**: e_walauto/interrupt2/wal5 also call
  sqlite3_wal_checkpoint — their committed fixtures predate the handler
  and regenerate under the same drift card when triaged.
