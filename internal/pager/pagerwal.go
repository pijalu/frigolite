// Package pager — WAL write transaction, read marks and checkpointing.
//
// The WRITER shm lock lifecycle (sqlite3WalBeginWriteTransaction /
// sqlite3WalEndWriteTransaction ports) and the PRAGMA wal_checkpoint
// entry points route through the walWriter in wal.go.
package pager

import (
	"errors"
	"time"
)

// walBeginWriteLocked acquires the WRITER shm lock for this connection's
// write transaction when not already held (C's pWal->writeLock). Under the
// lock the shared wal-index header is compared against this connection's
// pinned snapshot: another connection having committed in between fails the
// write with SQLITE_BUSY_SNAPSHOT ("database is locked",
// walprotocol2-2.2/2.3). Caller holds p.mu.
//
// cacheDroppable enables the stale-snapshot retry (busyTimeout > 0,
// walprotocol2-2.4/2.5: refresh the pin, drop the page cache, re-check).
// It is safe only BEFORE the statement's btree phase opened pages — the
// eager engine gate (WALBeginWrite); the WritePage path passes false,
// because C's retry re-runs the whole statement from sqlite3_reset and
// frigolite models that by failing the statement for the caller to retry.
func (p *Pager) walBeginWriteLocked(cacheDroppable bool) (bool, error) {
	w := p.wal
	if w == nil || w.writeLock {
		return false, nil
	}
	if err := w.walBusyLockExclusive(walLockWrite, 1); err != nil {
		return false, err
	}
	w.writeLock = true
	adopted := false
	var deadline time.Time
	if w.busyTimeout > 0 {
		deadline = time.Now().Add(w.busyTimeout)
	}
	for {
		err := w.wi.WriterSection(w.checkWriteSnapshot)
		if err == nil {
			// C's pagerSharedLock pins READ_LOCK(0) (the "ignore the WAL"
			// mark) before the write transaction: walFrames' walRestartLog
			// gate (wal.c:4056) requires the writer's readLock==0 at
			// frame-write time — without the pin a fully-backfilled log
			// never restarts and the next commit never rewrites the WAL
			// header (walvfs-1.1's sync count).
			if w.readLock == -1 {
				_ = w.wi.WriterSection(func() error {
					// READ_LOCK(0) is the "ignore the log" mark: legitimate
					// only when the shared log is empty or fully
					// backfilled. The gate reads the LIVE shared header —
					// the connection's cached w.hdr may still be zero on a
					// connection whose first refresh has not adopted (the
					// vacuous 0==0 pinned mark 0 beside a non-empty log and
					// the writer then read WAL-only pages from the main
					// file — wal-14's db2 insert saw zeroed page images).
					info := w.wi.ckptInfoLocked()
					pin := w.hdr
					liveMxFrame := uint32(0)
					if _, ok := w.wi.tryRefreshLocked(&pin); ok {
						liveMxFrame = pin.MxFrame
					} else {
						liveMxFrame = info.NBackfill // unparsable: fall back
					}
					if liveMxFrame != info.NBackfill {
						return nil // a non-empty, un-backfilled log: no mark-0 pin
					}
					if w.tryLockZeroHeld(&info) == rdPinRetry {
						info = w.wi.ckptInfoLocked()
						w.tryLockZeroHeld(&info)
					}
					return nil
				})
			}
			return adopted, nil
		}
		if errors.Is(err, errWalBusySnapshot) &&
			cacheDroppable && w.busyTimeout > 0 && time.Now().Before(deadline) {
			// walprotocol2-2.4/2.5: the busy handler fired; re-run from a
			// fresh read snapshot. The statement has not read or dirtied a
			// page yet, so dropping the cache is the pager_reset parity.
			// The caller must ALSO drop its schema/table caches: rowid
			// counters derived from the pre-retry snapshot are stale.
			adopted = w.refreshStalePinLocked() || adopted
			p.pages = make(map[uint32]*Page)
			p.header = nil
			p.invalidateCookieCacheLocked()
			continue
		}
		w.walUnlockExclusive(walLockWrite, 1)
		w.writeLock = false
		return adopted, err
	}
}

// checkWriteSnapshot is the WRITER-lock snapshot-consistency check run under
// the wal-index writer section (sqlite3WalBeginWriteTransaction's memcmp):
// the shared header must still match this connection's pin. An unparsable
// header under the WRITER lock is recovered (its caller contract) and the
// rebuilt state becomes the pin; a header moved past the pin reports
// errWalBusySnapshot.
func (w *walWriter) checkWriteSnapshot() error {
	pin := w.hdr
	changed, ok := w.wi.tryRefreshLocked(&pin)
	if !ok {
		// Corrupt header under the WRITER lock: recover (its caller
		// contract) — the rebuilt state becomes the pin.
		return w.walIndexRecoverLocked()
	}
	if changed && !pin.Equal(&w.hdr) {
		return errWalBusySnapshot
	}
	if changed {
		w.adoptHeaderLocked()
	}
	return nil
}

// refreshStalePinLocked re-runs the wal-index refresh under the WRITER lock
// (the stale-snapshot retry path): a moved shared header is adopted into
// this connection's cached view. Reports whether the header had changed
// (the caller's "adopted" signal). Never fails: a failed refresh leaves the
// pin as-is and the next iteration re-runs the consistency check.
func (w *walWriter) refreshStalePinLocked() bool {
	adopted := false
	_ = w.wi.WriterSection(func() error {
		if ch, ok := w.wi.tryRefreshLocked(&w.hdr); ok && ch {
			w.adoptHeaderLocked()
			adopted = true
		}
		return nil
	})
	return adopted
}

// WALBeginWrite opens the WAL write transaction eagerly for a writing
// statement — the engine calls this from its per-statement gate BEFORE the
// statement's btree phase reads any page (sqlite3WalBeginWriteTransaction
// parity: C takes the WRITER lock before the btree cursor work, so the
// statement's page images build on a snapshot the WRITER lock freezes).
// The statement's read snapshot is pinned first (walIndexRefreshLocked):
// in autocommit that is a fresh read transaction whose stale-snapshot
// write retry may drop the page cache; inside an explicit transaction the
// snapshot is already frozen and the retry is disabled (C re-runs from the
// SAME snapshot only, else repeatable reads would break). Reports whether
// the shared wal-index header had changed since the connection's cached
// view (the caller must invalidate its schema/table caches, the
// engine-side pager_reset — the plain CheckExternalFile path would no
// longer see the change, the refresh having been consumed here). A failed
// write gate ends a read snapshot THIS call created (C's autocommit
// statement failure closes the transaction); a snapshot pinned before the
// call (explicit transaction) stays open.
func (p *Pager) WALBeginWrite() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil {
		return false, nil
	}
	wasPinned := p.wal.readLock >= 0
	// Refresh the pin first: this IS the statement's read-transaction open
	// (walIndexReadHdr), so the snapshot check compares against fresh state.
	changed, err := p.walIndexRefreshLocked()
	if err != nil {
		if !wasPinned {
			p.wal.walEndReadTxn()
		}
		return changed, err
	}
	retryChanged, err := p.walBeginWriteLocked(!wasPinned)
	if err != nil && !wasPinned {
		p.wal.walEndReadTxn()
	}
	return changed || retryChanged, err
}

// WALEndRead releases the connection's pinned WAL read snapshot (the
// sqlite3WalEndReadTransaction parity): the shared read-mark lock is
// dropped so checkpoints may backfill past the mark and the writer may wrap
// the log. The next statement re-pins a fresh snapshot and observes every
// commit that landed in between. No-op when not in WAL mode or when no read
// transaction is open.
func (p *Pager) WALEndRead() {
	p.mu.Lock()
	if w := p.wal; w != nil {
		w.walEndReadTxn()
	}
	p.mu.Unlock()
}

// WalReadMarks returns the shared wal-index checkpoint info (nBackfill,
// nBackfillAttempted, aReadMark[5]) and the connection's pinned read-lock
// index (-1 none, 0..4): the observability seam for the read-mark protocol
// (native tests; a future pragma). False when the pager is not in WAL mode.
func (p *Pager) WalReadMarks() (info WalCkptInfo, readLock int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil || p.wal.wi == nil {
		return WalCkptInfo{}, -1, false
	}
	return p.wal.wi.CkptInfo(), p.wal.readLock, true
}

// walEndWriteLocked releases the WRITER shm lock at the end of the write
// transaction (sqlite3WalEndWriteTransaction parity: COMMIT, ROLLBACK and
// Close all drop it). Caller holds p.mu.
func (p *Pager) walEndWriteLocked() {
	if w := p.wal; w != nil && w.writeLock {
		w.walUnlockExclusive(walLockWrite, 1)
		w.writeLock = false
		w.spillStarted = false
		clear(w.spilled)
	}
}

// Checkpoint folds the WAL into the main database file and resets the WAL
// (RESTART-style). It is a no-op when not in WAL mode. Kept as a no-arg
// convenience wrapper; new code should call CheckpointMode with the desired
// PRAGMA wal_checkpoint mode.
func (p *Pager) Checkpoint() error {
	_, _, _, err := p.CheckpointMode(WalCkptRestart)
	return err
}

// CheckpointMode performs a WAL checkpoint in the given mode and returns the
// PRAGMA wal_checkpoint result triple (busy, nLog, nCkpt) — nLog is the
// number of frames in the log and nCkpt the number backfilled into the main
// file (sqlite/src/wal.c walCheckpoint; walprotocol-2.1 expects {0 5 5} for
// a PASSIVE checkpoint over 5 frames). PASSIVE backfills the main DB but
// keeps the -wal; FULL is PASSIVE with completion guaranteed; RESTART/
// TRUNCATE backfill and reset the -wal to its 32-byte header.
func (p *Pager) CheckpointMode(mode WalCheckpointMode) (busy, nLog, nCkpt int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil {
		return 0, 0, 0, nil
	}
	return p.wal.checkpoint(mode)
}

// ReadMarkSlot reads wal-index read-mark slot (0-based; C's aShm[25+iSlot],
// the vfs_set_readmark reader form). ok=false when no WAL is attached. The
// fixture's xShmMap callback fires first exactly as the C command's
// xShmMap(0, 32*1024, 0) probe does — a veto surfaces as its error.
func (p *Pager) ReadMarkSlot(slot int) (value uint32, ok bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil {
		return 0, false, nil
	}
	if err := p.vfsCall("xShmMap", p.path+"-shm", walShmRegionBytes); err != nil {
		return 0, true, err
	}
	p.wal.wi.WriterSection(func() error {
		value = p.wal.wi.ckptInfoLocked().AReadMark[slot]
		return nil
	})
	return value, true, nil
}

// WriteMarkSlot writes wal-index read-mark slot (0-based) and reports the
// value now stored (test_vfs.c's vfs_set_readmark writer form — the command
// returns aShm[iOff] either way). ok=false when no WAL is attached.
func (p *Pager) WriteMarkSlot(slot int, v uint32) (uint32, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil {
		return 0, false, nil
	}
	if err := p.vfsCall("xShmMap", p.path+"-shm", walShmRegionBytes); err != nil {
		return 0, true, err
	}
	p.wal.wi.WriterSection(func() error {
		p.wal.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.AReadMark[slot] = v })
		return nil
	})
	return v, true, nil
}

// WalFileSize reports the current "-wal" file size in bytes (0 when not in
// WAL mode). Used by tests to simulate a crash at a frame boundary.
func (p *Pager) WalFileSize() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.wal == nil {
		return 0
	}
	return p.wal.FileSize()
}
