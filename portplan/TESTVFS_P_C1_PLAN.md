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

- **S1 (walvfs)**: converter (`testvfs <name>`, `<name> script|filter|devchar
  <arg>`, `-vfs <name>` opens), harness VFS registry + the pagerFile
  decorator + DeviceCharacteristics, the WAL sync-event model above, and the
  passive-checkpoint log restart. Re-measure walvfs-1.2 ([0 4 4] vs our
  oracle-verified [0 2 2] — the tvfs environment may change the frame
  accounting; re-derive under the real machinery).
- **S2**: xShm callbacks + the `shm` subcommand (notify/wal observation
  families).
- **S3**: ioerr fault injection (pagerfault 58, e_walckpt 272, the
  *fault families) — test_vfs.c's ioerr counters + the harness fault steps.
- **S4**: fullerr/cantopenerr/sectorsize/delete + the remaining option
  surface.

## Ground already landed this round

- WAL commits never bump the file header change counter (pager.c:6496's WAL
  early return) — walvfs-1.0's 5 frames byte-verified (+37 leaves across the
  WAL families).
- The WAL frame decoder (walview.go) is the byte-exact verification tool.
