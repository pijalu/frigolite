package pager

// wallocks.go — the wal-index shm lock protocol (P7.WAL-G7 slice 2), ported
// from SQLite src/wal.c (walLockShared/walLockExclusive/walBusyLock,
// walTryBeginRead's busy/protocol conversion) and src/os_unix.c
// (unixShmSystemLock: POSIX advisory locks at UNIX_SHM_BASE = (22+8)*4 = 120,
// one byte per slot, F_RDLCK for shared / F_WRLCK for exclusive).
//
// The eight lock slots live at shm bytes 120..127 (WalCkptInfo.aLock):
//
//	idx 0  WAL_WRITE_LOCK    one writer at a time (exclusive)
//	idx 1  WAL_CKPT_LOCK     checkpointer (exclusive; recovery co-locks 1..2)
//	idx 2  WAL_RECOVER_LOCK  recovery in progress (shared probe ⇒ BUSY_RECOVERY)
//	idx 3..7 WAL_READ_LOCK(0..4)  reader read marks
//
// Two arbitration layers are co-managed, exactly like os_unix.c:
//
//   - syscall.FcntlFlock (F_SETLK, non-blocking) on the registry's ONE shm fd
//     arbitrates BETWEEN processes;
//   - the aLock[] shadow (a per-slot refcount array under the wal-index
//     mutex) arbitrates WITHIN the process — POSIX fcntl locks never conflict
//     for two open file descriptions of the same process (os_unix.c wraps its
//     posix advisory locks in pShmNode->pShmMutex for the same reason), so
//     the shadow is load-bearing, not optional.
//
// Because both connections of one process share the single shm fd, a failed
// in-process attempt or an unlock must RECONCILE the kernel locks with the
// remaining shadow holders (another goroutine of this process may still hold
// shared on a byte this call releases): reconcileFlockHeld re-asserts
// WRLCK/RDLCK/UNLCK per byte from the shadow state.
//
// Busy semantics follow wal.c: a plain lock attempt never blocks (F_SETLK);
// walBusyLock retries it while the connection's busy timeout allows (the
// sqlite3_busy_timeout handler); the read-side recovery loop retries up to
// WAL_RETRY_PROTOCOL_LIMIT rounds and then fails with SQLITE_PROTOCOL
// ("locking protocol"); BUSY with the RECOVER lock held is reported as
// SQLITE_BUSY_RECOVERY. All of them surface the primary-code text "database
// is locked" (src/main.c sqlite3ErrStr masks the extended code) except
// SQLITE_PROTOCOL's "locking protocol".

import (
	"errors"
	"io"
	"os"
	"syscall"
	"time"
)

// WAL lock error family. The texts are the oracle texts (sqlite3 3.51):
// SQLITE_BUSY/BUSY_RECOVERY/BUSY_SNAPSHOT all report "database is locked";
// SQLITE_PROTOCOL reports "locking protocol" (walprotocol-1.3/1.4).
var (
	// errWalBusy is SQLITE_BUSY: the lock byte is held by another connection.
	errWalBusy = errors.New("database is locked")
	// errWalBusyRecovery is SQLITE_BUSY_RECOVERY: another connection is
	// running wal-index recovery (RECOVER lock held).
	errWalBusyRecovery = errors.New("database is locked")
	// errWalBusySnapshot is SQLITE_BUSY_SNAPSHOT: the writer's snapshot is
	// stale (another connection committed after this connection's read
	// transaction pinned the wal-index header).
	errWalBusySnapshot = errors.New("database is locked")
	// errWalProtocol is SQLITE_PROTOCOL: the WAL retry protocol limit was
	// exhausted ("locking protocol", walprotocol-1.3/1.4).
	errWalProtocol = errors.New("locking protocol")
	// errWalRetry is the internal WAL_RETRY sentinel: the caller should sleep
	// briefly and retry the read transaction (wal.c WAL_RETRY).
	errWalRetry = errors.New("wal retry")
)

// walRetryProtocolLimit is WAL_RETRY_PROTOCOL_LIMIT (wal.c L486): the number
// of read-retry rounds before the protocol error.
const walRetryProtocolLimit = 100

// Timing used by the retry loops. C sleeps 1us..323ms across the 100 retry
// rounds (≈10s total) and lets the busy handler pace the busy-retry loop;
// frigolite uses fixed small delays so the protocol limit (100 rounds) costs
// ~20ms and a busy timeout is honored to its deadline.
const (
	walRetrySleep      = 200 * time.Microsecond
	walBusySleep       = 2 * time.Millisecond
	walBusyEarlyRounds = 5 // rounds before any sleeping starts (wal.c *pCnt>5)
)

// defaultShmLockHook is the process-wide xShmLock observer/veto (the testvfs
// "T filter xShmLock; T script lock_callback" equivalent). It fires for every
// shm lock operation with (first slot, slot count, "lock"|"unlock", exclusive)
// BEFORE the lock is taken; a non-nil return vetoes the operation and surfaces
// as SQLITE_BUSY. Nil in production. Mirror of SetDefaultJournalFileOpHook.
var defaultShmLockHook func(idx, n int, op string, excl bool) error

// SetShmLockHook installs (or clears, when fn is nil) the process-wide
// xShmLock hook. The hook observes the exact lock sequence of the wal-index
// protocol — recovery fires {0 1 lock exclusive}, {1 2 lock exclusive}, the
// read-mark pairs {4..7 1 lock/unlock exclusive}, then the unlocks
// (walprotocol-1.1/1.2); a commit wraps its frames in {0 1 lock/unlock
// exclusive}. Returning an error vetoes the lock (testvfs lock_callback
// returning SQLITE_BUSY). The hook must be installed before the connections
// under test are opened and must not itself block.
func SetShmLockHook(fn func(idx, n int, op string, excl bool) error) {
	defaultShmLockHook = fn
}

// shmLockHookFn returns the effective hook (nil when unset).
func shmLockHookFn() func(idx, n int, op string, excl bool) error {
	return defaultShmLockHook
}

// shmFlockTry applies one non-blocking POSIX advisory lock operation to the
// shm fd at byte WalIndexLockOffset+idx (os_unix.c unixShmSystemLock:
// F_SETLK with l_start = ofst, l_len = n). EINTR is retried; EWOULDBLOCK
// (another process holds a conflicting lock) reports busy.
func shmFlockTry(fd *os.File, idx, n int, excl, unlock bool) error {
	typ := int16(syscall.F_RDLCK)
	if excl {
		typ = syscall.F_WRLCK
	}
	if unlock {
		typ = syscall.F_UNLCK
	}
	for {
		err := syscall.FcntlFlock(fd.Fd(), syscall.F_SETLK, &syscall.Flock_t{
			Type:   typ,
			Whence: int16(io.SeekStart),
			Start:  int64(WalIndexLockOffset + idx),
			Len:    int64(n),
		})
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return errWalBusy
		}
		return err
	}
}

// shmTryLockHeld takes shm locks on slots [idx, idx+n) — the caller holds the
// wal-index mutex (i.e. runs inside WriterSection or another w.mu holder).
// Sequence: hook (may veto) → kernel flock (non-blocking, cross-process) →
// aLock shadow (in-process). On an in-process shadow conflict the kernel
// locks are reconciled back to the shadow's state (the failed attempt's
// transient F_WRLCK must not mask a concurrent in-process shared holder —
// see the file comment) and the result is SQLITE_BUSY.
//
// The hook fires UNDER the wal-index mutex here: these are the protocol's
// internal lock ops (recovery read marks, checkpoint coordination), whose
// hooks are veto-only. The WRITER/CKPT attempts made from public paths fire
// their hooks outside the mutex (shmTryLock below), so a hook that re-enters
// the engine (walprotocol2's sabotaging second connection) cannot deadlock.
func (w *WALIndex) shmTryLockHeld(idx, n int, excl bool) error {
	if h := shmLockHookFn(); h != nil {
		if err := h(idx, n, "lock", excl); err != nil {
			return errWalBusy
		}
	}
	return w.shmTryLockHeldNoHook(idx, n, excl)
}

// shmTryLockHeldNoHook is shmTryLockHeld without firing the hook.
func (w *WALIndex) shmTryLockHeldNoHook(idx, n int, excl bool) error {
	if w.shmFd != nil {
		if err := shmFlockTry(w.shmFd, idx, n, excl, false); err != nil {
			return err
		}
	}
	if !w.lockSlotsLocked(idx, n, excl) {
		// In-process conflict (R1): undo the kernel state this attempt set.
		w.reconcileFlockHeld(idx, n)
		return errWalBusy
	}
	return nil
}

// shmUnlockHeld releases shm locks on slots [idx, idx+n). Caller holds the
// wal-index mutex. The shadow is cleared first, then the kernel locks are
// reconciled: bytes another in-process holder still keeps shared stay
// RDLCK-locked (POSIX locks are per-process — an F_UNLCK would drop the
// co-holder's kernel lock too).
func (w *WALIndex) shmUnlockHeld(idx, n int, excl bool) {
	if h := shmLockHookFn(); h != nil {
		_ = h(idx, n, "unlock", excl) // unlock results are ignored (wal.c)
	}
	w.unlockSlotsLocked(idx, n, excl)
	w.reconcileFlockHeld(idx, n)
}

// reconcileFlockHeld re-asserts the kernel locks implied by the shadow state
// of slots [idx, idx+n): exclusive → F_WRLCK, shared → F_RDLCK, free →
// F_UNLCK. Caller holds the wal-index mutex.
func (w *WALIndex) reconcileFlockHeld(idx, n int) {
	if w.shmFd == nil {
		return
	}
	for i := idx; i < idx+n; i++ {
		if i < 0 || i >= len(w.aLock) {
			continue
		}
		switch {
		case w.aLock[i] < 0:
			_ = shmFlockTry(w.shmFd, i, 1, true, false)
		case w.aLock[i] > 0:
			_ = shmFlockTry(w.shmFd, i, 1, false, false)
		default:
			_ = shmFlockTry(w.shmFd, i, 1, false, true)
		}
	}
}

// shmTryLock is shmTryLockHeld for callers outside the wal-index mutex. The
// hook fires BEFORE the mutex is taken, so hooks may re-enter the engine
// (walprotocol2's lock_callback drives a second connection's commit from the
// WRITER-attempt event).
func (w *WALIndex) shmTryLock(idx, n int, excl bool) error {
	if h := shmLockHookFn(); h != nil {
		if err := h(idx, n, "lock", excl); err != nil {
			return errWalBusy
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.shmTryLockHeldNoHook(idx, n, excl)
}

// shmUnlock is shmUnlockHeld for callers outside the wal-index mutex, with
// the hook fired after the mutex is released.
func (w *WALIndex) shmUnlock(idx, n int, excl bool) {
	w.mu.Lock()
	w.unlockSlotsLocked(idx, n, excl)
	w.reconcileFlockHeld(idx, n)
	w.mu.Unlock()
	if h := shmLockHookFn(); h != nil {
		_ = h(idx, n, "unlock", excl)
	}
}

// ---------------------------------------------------------------------------
// walWriter-level lock calls (wal.c walLockShared / walLockExclusive /
// walUnlockShared / walUnlockExclusive). In locking_mode=EXCLUSIVE
// (exclusiveMode) they are all no-ops, exactly as in C.
// ---------------------------------------------------------------------------

// walLockExclusive takes exclusive shm locks on slots [idx, idx+n).
func (w *walWriter) walLockExclusive(idx, n int) error {
	if w.exclusiveMode {
		return nil
	}
	return w.wi.shmTryLock(idx, n, true)
}

// walUnlockExclusive releases exclusive shm locks on slots [idx, idx+n).
func (w *walWriter) walUnlockExclusive(idx, n int) {
	if w.exclusiveMode {
		return
	}
	w.wi.shmUnlock(idx, n, true)
}

// walLockShared takes shared shm locks on slots [idx, idx+n).
func (w *walWriter) walLockShared(idx, n int) error {
	if w.exclusiveMode {
		return nil
	}
	return w.wi.shmTryLock(idx, n, false)
}

// walUnlockShared releases shared shm locks on slots [idx, idx+n).
func (w *walWriter) walUnlockShared(idx, n int) {
	if w.exclusiveMode {
		return
	}
	w.wi.shmUnlock(idx, n, false)
}

// walBusyLock ports walBusyLock (wal.c L2101): try the exclusive lock; while
// it reports busy and the connection's busy timeout allows, sleep and retry
// (the sqlite3_busy_timeout handler). With no timeout a contended lock fails
// immediately — a single F_SETLK attempt, like a C build without a handler.
func (w *walWriter) walBusyLockExclusive(idx, n int) error {
	var deadline time.Time
	if w.busyTimeout > 0 {
		deadline = time.Now().Add(w.busyTimeout)
	}
	for {
		err := w.walLockExclusive(idx, n)
		if err == nil || !errors.Is(err, errWalBusy) {
			return err
		}
		if w.busyTimeout <= 0 || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(walBusySleep)
	}
}

// ---------------------------------------------------------------------------
// Pager-level seams (native-test surface; the vfs_shmlock test-command and
// sqlite3_busy_timeout parity).
// ---------------------------------------------------------------------------

// SetBusyTimeout sets this connection's busy timeout (sqlite3_busy_timeout
// parity): contended WAL shm lock acquisitions retry with a small sleep until
// the timeout expires, then fail with "database is locked". A zero timeout
// (the default) fails immediately.
func (p *Pager) SetBusyTimeout(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal != nil {
		p.wal.busyTimeout = d
	}
}

// WALMode reports whether the pager runs in WAL mode (a walWriter is
// attached). The engine consults it for the cross-connection lock matrix:
// in WAL mode readers never block writers and writers never block readers
// (only the WRITER shm byte serializes writers), unlike the rollback-journal
// RESERVED/EXCLUSIVE upgrade model.
func (p *Pager) WALMode() bool { return p.walRef() }

// SetWALExclusiveMode toggles locking_mode=EXCLUSIVE for the WAL writer
// (wal.c: in exclusive mode every walLockShared/walLockExclusive is a no-op —
// the shm locks are never taken, assuming single-process access).
func (p *Pager) SetWALExclusiveMode(excl bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal != nil {
		p.wal.exclusiveMode = excl
	}
}

// WALIndexLock exercises the wal-index lock protocol directly — the parity
// seam for shmlock.test's vfs_shmlock test command: acquire (lock=true) or
// release (lock=false) shm locks on slots [idx, idx+n) of this pager's shared
// wal-index, shared (excl=false) or exclusive (excl=true). Reports whether
// the acquisition succeeded (false = SQLITE_BUSY). Releases always succeed.
func (p *Pager) WALIndexLock(idx, n int, excl, lock bool) bool {
	p.mu.Lock()
	w := p.wal
	p.mu.Unlock()
	if w == nil || w.wi == nil {
		return false
	}
	if lock {
		return w.wi.shmTryLock(idx, n, excl) == nil
	}
	w.wi.shmUnlock(idx, n, excl)
	return true
}

// CheckExternalFileErr is CheckExternalFile with the WAL-mode header-refresh
// error surfaced: a wal-index that cannot be parsed and cannot be recovered
// right now reports SQLITE_BUSY_RECOVERY ("database is locked", another
// connection is mid-recovery) or SQLITE_PROTOCOL ("locking protocol"). The
// plain CheckExternalFile keeps the no-error signature for existing callers.
//
// pinWAL selects whether a WAL-mode pager OPENS ITS READ TRANSACTION (the
// read-mark pin — sqlite3WalBeginReadTransaction parity) as part of the
// check. Statements that never read pages through the WAL (transaction
// control, PRAGMA wal_checkpoint) pass false: pinning there would park a
// read mark across the statement and cap/FAIL a checkpoint that C runs
// outside any read transaction. Caller: the engine's per-statement gate.
func (p *Pager) CheckExternalFileErr(pinWAL bool) (bool, error) {
	if p.file == nil {
		return false, nil
	}
	if p.walRef() {
		if !pinWAL {
			return false, nil
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.dirty) > 0 {
			return false, nil
		}
		return p.walIndexRefreshLocked()
	}
	return p.CheckExternalFile(), nil
}

// CorruptWalIndex clobbers the shared wal-index header — the walsetlk-1.2
// harness parity ("set fd [open test.db-shm r+]; puts $fd blahblahblah"):
// the next header read fails to parse and walIndexRecover rebuilds the index
// from the -wal file. No-op when not in WAL mode.
func (p *Pager) CorruptWalIndex() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil || p.wal.wi == nil {
		return
	}
	_ = p.wal.wi.WriterSection(func() error {
		page0 := p.wal.wi.pageLocked(0)
		copy(page0[:48], []byte("blahblahblahblahblahblahblahblahblahblahblahblah"))
		p.wal.wi.markDirtyLocked(0)
		p.wal.wi.persistAllLocked()
		return nil
	})
}
