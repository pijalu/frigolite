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
