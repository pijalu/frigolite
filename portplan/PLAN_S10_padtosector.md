# PLAN S10 — Fix B: padToSectorBoundary (walvfs-5.3) + the devchar surface

## C contracts (wal.c / test_vfs.c / os_unix.c)

- walOpen (wal.c:1731-1734): `syncHeader=0` when the DB fd's device
  characteristics carry IOCAP_SEQUENTIAL; `padToSectorBoundary=0` when they
  carry IOCAP_POWERSAFE_OVERWRITE. Default (unix VFS): PSO **on**
  (os_unix.c: "This VFS is set up to return
  SQLITE_IOCAP_POWERSAFE_OVERWRITE by default").
- walFrames commit (wal.c:4172-4190): when isCommit && WAL_SYNC_FLAGS(sync
  _flags)!=0 && padToSectorBoundary — repeat the LAST frame (with its
  commit mark, nTruncate) until the file offset crosses the next
  sector-size boundary (sqlite3SectorSize(pWalFd), floor 512 / default
  SQLITE_DEFAULT_SECTOR_SIZE=4096, os.h:53). The padding frames are
  appended to the wal-index like real frames (mxFrame and aFrameCksum
  include them); the sync fires at the sync point (one xSync per commit
  either way).
- test_vfs.c devchar (CMD_DEVCHAR, tvfsDeviceCharacteristics:591):
  `p->iDevchar` starts at **-1 = passthrough** (the real file's
  characteristics — unix default PSO on). `tvfs devchar sequential`
  REPLACES the whole mask with SQLITE_IOCAP_SEQUENTIAL; `tvfs devchar
  default` resets to passthrough. tvfsSectorSize passes through to the
  real file (4096 on this Mac's APFS — the oracle's 24 = 22+2 padding).
- sqlite.h.in:671-686: SEQUENTIAL=0x400, POWERSAFE_OVERWRITE=0x1000,
  ATOMIC=0x1. The engine's IOCAPSequential=0x100 and the harness parse's
  0x100/0x800 are consistently wrong and must move to the real values
  together (they agree with each other today, which is why walvfs-1.3's
  sync_count=0 passes).

## Changes

1. **internal/pager/vfshooks.go**: `IOCAPSequential = 0x00000400`; add
   `IOCAPPowerSafeOverwrite = 0x00001000` (sqlite.h values).
2. **internal/pager/pager.go / pageropen.go**: initialize
   `deviceCharacteristics = IOCAPPowerSafeOverwrite` at Pager construction
   (the unix-VFS default). SetDeviceCharacteristics keeps REPLACING the
   mask (C's iDevchar semantics — the caller decides the mask).
3. **internal/pager/wal.go commitLocked**: after the pages loop, before the
   header publish — the padding block:
   `walCommitSyncBits(p.walSyncFlags)!=0 && p.DeviceCharacteristics()&PSO==0`
   → repeat `pages[len-1]` via appendFrame(commit=true, dbSize) +
   wi.appendLocked(nFrame, pLast.PageNum, pre-commit MxFrame) while
   `WalHdrSize + nFrame*frameSize < syncPoint` (syncPoint = next 4096
   boundary). *appended includes the padding frames (C's iCallback =
   iFrame total). Lazy devchar evaluation matches the existing
   walHeaderSyncAllowed style (the fixture installs hooks at open, before
   any commit).
4. **internal/exec/testvfs.go InstallVFSHook**: skip
   SetDeviceCharacteristics when devcharBits < 0 (the C passthrough state).
5. **frigolite_harness_test.go**: `testvfsFixture.devcharBits` starts at -1
   (passthrough); the `devchar` parse maps the aFlag[] names to the REAL
   sqlite.h bits (sequential 0x400, powersafe_overwrite 0x1000, atomic 0x1)
   and `default` resets to -1; every InstallVFSHook/SetDeviceCharacteristics
   call site passes the fixture's value (install skips < 0).

## Verification

- Native pin `TestWalPadToSectorBoundary`: a 1024-page-size WAL db under
  FULL sync — devchar without PSO (SetDeviceCharacteristics(0)) commits 22
  pages → 24 frames in the -wal; PSO on (default) → 22 frames; sync
  NORMAL/OFF → no padding either way (the sync-bits gate).
- walvfs-5.3 green ({24 100 100 100}); the other walvfs residue unchanged;
  wal sweep failure-name-set diff vs the S9 baseline: exactly walvfs-5.3
  removed, zero new.
