package pager

import (
	"errors"
	"strconv"
)

// testvfs hook layer: the fixture VFS's observable I/O surface. C's testvfs
// wraps the VFS and invokes the fixture script on the x-methods; our pager
// emits the same events through hooks at the identical call sites. The
// events are observational (S3's ioerr layer adds fault injection).

// SQLITE_IOCAP_* bits (sqlite.h) the fixture's devchar option models.
const (
	IOCAPSequential = 0x00000100
)

// theShmIDSeq mints the per-connection shm identifiers (test_vfs.c's
// pShmId: a stable integer per shm instance).
var theShmIDSeq uint64

// walShmRegionBytes is the wal-index region size the C readmark command
// probes through xShmMap (test_vfs.c test_vfs_set_readmark's
// xShmMap(pFd, 0, 32*1024, 0, &pShm)).
const walShmRegionBytes = "32768"

// walSyncFlagBits are the WAL-mode sync flag levels (pager.c:3636-3650):
// synchronous=OFF → 0, NORMAL → 0x08 (NORMAL<<2, commit bits zero), FULL →
// 0x0F (NORMAL<<2|FULL). WAL_SYNC_FLAGS(x) = x&0x03 (the commit sync bits),
// CKPT_SYNC_FLAGS(x) = (x>>2)&3 (the checkpoint/header sync bits).
const (
	// WALSyncOff/WALSyncNormal/WALSyncFull are the synchronous levels' WAL
	// sync-flag values (exported for the engine's PRAGMA synchronous push).
	WALSyncOff    uint8 = 0x00
	WALSyncNormal uint8 = 0x08
	WALSyncFull   uint8 = 0x0F
)

// deprecated internal aliases (the wal.go sites read these).
const (
	walSyncOff    = WALSyncOff
	walSyncNormal = WALSyncNormal
	walSyncFull   = WALSyncFull
)

// walCommitSyncBits reports the commit-sync bits of the WAL sync flags
// (wal.h WAL_SYNC_FLAGS).
func walCommitSyncBits(flags uint8) uint8 { return flags & 0x03 }

// walCkptSyncBits reports the checkpoint/header sync bits (wal.h
// CKPT_SYNC_FLAGS).
func walCkptSyncBits(flags uint8) uint8 { return flags >> 2 & 0x03 }

// VFSHookError is a fixture callback's result code: the testvfs script's
// return ("SQLITE_READONLY", "SQLITE_BUSY", ...) mapped to an error. The
// pager's hook sites translate it into the C-equivalent failure
// (sqlite3ErrStr's message for the code).
type VFSHookError struct{ Code string }

// Error implements error.
func (e *VFSHookError) Error() string { return e.Code }

// SetVFSHook installs the fixture callback hook: op is the VFS method name
// ("xSync", "xShmMap", "xShmLock", "xWrite"), path the file path the method
// ran on, details the method-specific trailing arguments C's testvfs passes
// (test_vfs.c tvfsExecTcl): the xSync sync-flag string ("normal"/"full"/
// "normal|dataonly"/...), the xShmMap region size, the xWrite byte count,
// the xShmLock "<ofst> <n> lock|unlock shared|exclusive" string. The
// callback is always invoked with (op, path, shmID, details) — four
// arguments — regardless of the method. xSync is observational (S1); the
// shm methods' returns are the method RESULT (S2) — VFSHookError values
// propagate as the mapped failure.
func (p *Pager) SetVFSHook(fn func(op, path string, details []string) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.vfsHook = fn
	// The pager attached the WAL (and mapped the -shm) at OPEN, before the
	// fixture installed: the next WAL access performs the callback's
	// xShmMap check (C maps lazily at the first transaction).
	p.vfsShmMapPending = true
}

// translateVFSHookError maps a fixture callback's result code to the C
// error message the caller would see (sqlite3ErrStr). All the
// SQLITE_READONLY_* extended codes share the base code's message
// (main.c:1547's switch falls through to the primary name's errstr).
func translateVFSHookError(err error) error {
	if he, ok := err.(*VFSHookError); ok {
		switch he.Code {
		case "SQLITE_READONLY", "SQLITE_READONLY_CANTINIT", "SQLITE_READONLY_DIRECTORY",
			"SQLITE_READONLY_RECOVERY", "SQLITE_READONLY_ROLLBACK", "SQLITE_READONLY_DBMOVED":
			return errors.New("attempt to write a readonly database")
		case "SQLITE_BUSY":
			return errors.New("database is locked")
		}
		return errors.New(he.Code)
	}
	return err
}

// vfsCall invokes the hook for op and returns its error unchanged (the
// caller translates VFSHookError codes). The hook field is read without
// the mutex (installed once at connection setup; the fixture callback must
// not re-enter the pager).
func (p *Pager) vfsCall(op, path string, details ...string) error {
	if p.vfsHook == nil {
		return nil
	}
	return p.vfsHook(op, path, details)
}

// ShmID renders the connection's shm identifier (test_vfs.c's pShmId — a
// stable per-connection integer the callbacks receive as their third
// argument).
func (p *Pager) ShmID() string { return strconv.FormatUint(p.shmID, 10) }

// vfsShmLock fires the fixture's xShmLock callback (test_vfs.c tvfsShmLock):
// details "<ofst> <n> lock|unlock shared|exclusive". The callback's return
// code is the method result; unlock results are ignored (wal.c discards
// them).
func (p *Pager) vfsShmLock(idx, n int, op string, excl bool) error {
	if p.vfsHook == nil {
		return nil
	}
	details := strconv.Itoa(idx) + " " + strconv.Itoa(n) + " " + op + " shared"
	if excl {
		details = strconv.Itoa(idx) + " " + strconv.Itoa(n) + " " + op + " exclusive"
	}
	err := p.vfsHook("xShmLock", p.path, []string{details})
	if err == nil || op != "lock" {
		return nil
	}
	// wal.c's walLock* propagate the VFS result verbatim: BUSY drives the
	// lock protocol's busy handling; other codes flow to the caller's
	// mapping (walIndexRecover's READONLY_RECOVERY et al).
	if he, ok := err.(*VFSHookError); ok && he.Code == "SQLITE_BUSY" {
		return errWalBusy
	}
	return err
}

// SetDeviceCharacteristics overrides the IOCAP bits the fixture's devchar
// option sets (test_vfs.c CMD_DEVCHAR). The WAL writer consults them where
// wal.c does (IOCAP_SEQUENTIAL gates the WAL header sync).
func (p *Pager) SetDeviceCharacteristics(bits int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deviceCharacteristics = bits
}

// DeviceCharacteristics reports the effective IOCAP bits.
func (p *Pager) DeviceCharacteristics() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.deviceCharacteristics
}

// SetWALSyncFlags stores the synchronous level's WAL sync bits. The engine
// derives them from PRAGMA synchronous (see walSync* constants).
func (p *Pager) SetWALSyncFlags(flags uint8) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.walSyncFlags = flags
}

// vfsSync emits one xSync event through the hook when the sync's bit group
// is non-zero (C's sqlite3OsSync(walFd, flags) runs only when the flags say
// so). The callback's details carry the sync-flag string test_vfs.c renders
// ("normal", "full", "normal|dataonly", ...). Called from INSIDE the pager's
// locked sections (the wal writer runs under p.mu): the hook field is read
// without locking — SetVFSHook installs it once at connection setup, before
// any I/O — and the fixture hook must not re-enter the pager.
func (p *Pager) vfsSync(path string, bits uint8) {
	if bits == 0 || p.vfsHook == nil {
		return
	}
	// test_vfs.c tvfsSync's zFlags rendering (SQLITE_SYNC_NORMAL=0x02,
	// SQLITE_SYNC_FULL=0x03, SQLITE_SYNC_DATAONLY=0x10 — the FULL
	// discriminator is the low bit).
	zFlags := "normal"
	if bits&0x01 != 0 {
		zFlags = "full"
	}
	if bits&0x10 != 0 {
		zFlags += "|dataonly"
	}
	_ = p.vfsHook("xSync", path, []string{zFlags})
}

// walHeaderSyncAllowed reports whether the WAL header sync may run: false
// when the device characteristics carry IOCAP_SEQUENTIAL (wal.c:1731 —
// syncHeader=0) or all syncing is off (synchronous=OFF, walSyncFlags==0).
func (p *Pager) walHeaderSyncAllowed() bool {
	if p.deviceCharacteristics&IOCAPSequential != 0 {
		return false
	}
	return p.walSyncFlags != walSyncOff
}
