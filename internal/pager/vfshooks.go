package pager

// testvfs hook layer: the fixture VFS's observable I/O surface. C's testvfs
// wraps the VFS and invokes the fixture script on the x-methods; our pager
// emits the same events through hooks at the identical call sites. The
// events are observational (S3's ioerr layer adds fault injection).

// SQLITE_IOCAP_* bits (sqlite.h) the fixture's devchar option models.
const (
	IOCAPSequential = 0x00000100
)

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

// SetVFSHook installs the fixture callback hook: op is the VFS method name
// ("xSync"), path the file path the method ran on. The hook is observational
// in S1 — its error is reported back to the caller but the I/O already
// succeeded (C's xSync failure surfaces as SQLITE_IOERR; S3 wires that).
func (p *Pager) SetVFSHook(fn func(op, path string) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.vfsHook = fn
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
// so). Called from INSIDE the pager's locked sections (the wal writer runs
// under p.mu): the hook field is read without locking — SetVFSHook installs
// it once at connection setup, before any I/O — and the fixture hook must
// not re-enter the pager.
func (p *Pager) vfsSync(path string, bits uint8) {
	if bits == 0 || p.vfsHook == nil {
		return
	}
	_ = p.vfsHook("xSync", path)
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
