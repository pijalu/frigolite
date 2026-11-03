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
  - **Port shape**: converter `do_faultsim_test` → one dynamic step
    {type: "faultsim_test", Faults, Prep []TestStep, Body []TestStep, Test
    patterns} (the trial count is a RUNTIME property — the harness owns the
    loop); harness faultsim_save/restore via file copies; the injector as a
    global pager hook (like the walFault precedent) with the same counter
    semantics. e_walckpt's per-fault sections layer the same injector over
    the checkpoint modes.
  *fault families) — test_vfs.c's ioerr counters + the harness fault steps.
- **S4**: fullerr/cantopenerr/sectorsize/delete + the remaining option
  surface.

## Ground already landed this round

- WAL commits never bump the file header change counter (pager.c:6496's WAL
  early return) — walvfs-1.0's 5 frames byte-verified (+37 leaves across the
  WAL families).
- The WAL frame decoder (walview.go) is the byte-exact verification tool.
