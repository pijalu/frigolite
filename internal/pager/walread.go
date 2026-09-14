package pager

// walread.go — the reader read-mark snapshot protocol (P7.WAL-G7 slice 3),
// ported from SQLite src/wal.c walTryBeginRead (L3000) and its wrapper
// sqlite3WalBeginReadTransaction (L3473).
//
// Every read transaction pins the connection's view of the write-ahead log:
//
//   - The connection's cached wal-index header (w.hdr) is refreshed ONCE at
//     pin time and then FROZEN: page lookups resolve frames only up to
//     hdr.mxFrame, so another connection's later commits are invisible until
//     the read transaction ends (repeatable reads).
//   - A shared lock is taken on WAL_READ_LOCK(mxI) (shm byte 123+mxI) for
//     the chosen read-mark slot. While held, no connection can change
//     aReadMark[mxI], a checkpointer cannot backfill past the mark, and the
//     log cannot be wrapped (walRestartLog needs exclusive READ_LOCK(1..4))
//     — the frames the frozen header references can never disappear.
//   - Read-mark selection (C: the aReadMark[] loop): the slot with the
//     largest mark ≤ mxFrame wins; when the largest mark is older than
//     mxFrame, an exclusive lock is taken on the first free slot, the mark
//     is bumped to mxFrame under that lock, and the lock is released before
//     re-taking it shared — readers that follow can share the bumped mark.
//   - When the log is fully backfilled (nBackfill == mxFrame) the reader
//     pins READ_LOCK(0) and IGNORES the WAL: the main database file is the
//     trustworthy snapshot (pager.c reads dbPage directly).
//   - minFrame is set to nBackfill+1 at pin time: frames at or below
//     nBackfill are already checkpointed into the main file, and the hash
//     tables may legally contain stale entries for them (a page rewritten
//     later in the log) — walIndexFind's minFrame rule skips them so a
//     reader never resurrects a pre-checkpoint page version.
//
// Locking shape: the header refresh (including the recovery dance) runs
// OUTSIDE the wal-index mutex (it takes the WRITER byte itself); each
// walTryBeginRead round's read-mark work runs inside one WriterSection via
// walTryBeginReadHeld, so mark reads, mark bumps and the pin verification
// are atomic with respect to the shared header. Retries use C's WAL_RETRY
// budget: >WAL_RETRY_PROTOCOL_LIMIT rounds degrade to SQLITE_PROTOCOL
// ("locking protocol").

import (
	"errors"
	"time"
)

// walBeginReadTxn pins this connection's WAL read snapshot — the
// sqlite3WalBeginReadTransaction / walTryBeginRead port. It reports whether
// the shared wal-index header moved since the connection's cached copy (the
// caller must drop its page cache when it did). A pin is not re-taken while
// one is held: the frozen snapshot IS the read transaction. Errors follow
// wal.c: BUSY while a recovery is running elsewhere is BUSY_RECOVERY
// ("database is locked"); exhausting the retry budget is SQLITE_PROTOCOL
// ("locking protocol").
//
// Snapshot anchoring (the SQLITE_ENABLE_SNAPSHOT branch of C's
// walBeginReadTransaction, wal.c L3357-3448): when a snapshot is armed, the
// whole open runs under the shared CKPT lock so no concurrent checkpointer
// can advance nBackfillAttempted mid-check; after the pin, the snapshot is
// verified against the refreshed live header — a changed salt (the WAL was
// wrapped) or nBackfillAttempted past the snapshot's mxFrame (a checkpoint
// attempted to write frames beyond it) fails the open with
// SQLITE_ERROR_SNAPSHOT and ends the just-taken transaction. On success the
// connection's header is OVERWRITTEN with the snapshot image (wal.c L3430)
// and pChanged reports whether the snapshot differed from the pre-open
// cached header (C overwrites, not ORs, at L3432 — when the snapshot equals
// the cached header the pager's caches are still valid). A snapshot reader
// never ignores the start of the log (minFrame=1, wal.c L3437): a
// checkpointer may have skipped frames below its own nBackfill that this
// snapshot still needs.
func (w *walWriter) walBeginReadTxn() (bool, error) {
	if w.readLock >= 0 {
		return false, nil // read transaction already open: snapshot frozen
	}
	var deadline time.Time
	if w.busyTimeout > 0 {
		deadline = time.Now().Add(w.busyTimeout)
	}
	changed := false
	// C L3366-3385: armed snapshot ⇒ the whole open runs under the shared
	// CKPT lock (blocking in C; plain F_SETLK here — a busy CKPT reports
	// "database is locked" immediately, snapshot.test 5.3's SQLITE_BUSY).
	releaseCkpt, snapChanged, err := w.walSnapshotPrologue(&changed)
	if err != nil {
		return changed, err
	}
	if releaseCkpt != nil {
		defer releaseCkpt()
	}
	return w.walReadTxnLoop(&changed, snapChanged, deadline)
}

// walReadTxnLoop is walBeginReadTxn's walTryBeginRead retry loop (C's
// `do{...}while(rc==WAL_RETRY)` with the WAL_RETRY_PROTOCOL_LIMIT budget).
func (w *walWriter) walReadTxnLoop(changed *bool, snapChanged bool, deadline time.Time) (bool, error) {
	for cnt := 0; ; cnt++ {
		// C step 1: refresh (and recover, if needed) the wal-index header.
		// Its busy conversion (BUSY_RECOVERY vs WAL_RETRY) is the wal.c
		// walIndexReadHdr port; a header error is terminal here.
		ch, err := w.walIndexReadHdr()
		*changed = *changed || ch
		if err != nil {
			return *changed, err
		}
		// C steps 3-9: one read-mark round under the wal-index mutex, with
		// the post-pin snapshot verification (C L3401-3441) in the same
		// section so its nBackfillAttempted read is atomic with the pin.
		rerr, stale := w.walReadRoundHeld(changed, snapChanged)
		if stale {
			w.walEndReadTxn() // wal.c L3439: end the read transaction
			return *changed, errWalSnapshotStale
		}
		if rerr == nil {
			return *changed, nil
		}
		if !errors.Is(rerr, errWalRetry) {
			return *changed, rerr
		}
		if cnt >= walRetryProtocolLimit {
			return *changed, errWalProtocol
		}
		w.walReadRetryPace(cnt, deadline)
	}
}

// walSnapshotPrologue ports walBeginReadTransaction's snapshot preamble
// (wal.c L3366-3385): remember whether the armed snapshot differs from the
// cached header (the *pChanged answer on the overwrite path), refresh the
// header once, and exclude checkpointer races with the shared CKPT lock.
// Returns the release callback for that lock (nil when no snapshot is
// armed) and the snapshot-vs-cached-header comparison. The refresh's
// changed signal feeds the same *pChanged answer C's loop produces
// (walIndexReadHdr writes it through): when the snapshot equals the live
// header the differ-branch is skipped, so this is the only notice that the
// cached header (a previously pinned snapshot) is stale and the caches must
// drop.
func (w *walWriter) walSnapshotPrologue(changed *bool) (release func(), snapChanged bool, err error) {
	if w.snapshot == nil {
		return nil, false, nil
	}
	snapChanged = !w.snapshot.hdrEqual(&w.hdr)
	var ch bool
	if ch, err = w.walIndexReadHdr(); err != nil {
		return nil, snapChanged, err
	}
	if ch {
		*changed = true
	}
	if err = w.walLockShared(walLockCkpt, 1); err != nil {
		return nil, snapChanged, err
	}
	return func() { w.walUnlockShared(walLockCkpt, 1) }, snapChanged, nil
}

// walReadRoundHeld runs one read-mark round and — when the pin succeeded
// with a snapshot armed — the post-pin verification of C L3401-3441 in the
// same wal-index section. stale marks the SQLITE_ERROR_SNAPSHOT exit, whose
// walEndReadTxn the caller must run OUTSIDE the section (it takes the shm
// locks itself).
func (w *walWriter) walReadRoundHeld(changed *bool, snapChanged bool) (rerr error, stale bool) {
	_ = w.wi.WriterSection(func() error {
		if rerr = w.walTryBeginReadHeld(true); rerr != nil {
			return nil
		}
		stale = w.walSnapshotPostPinHeld(changed, snapChanged)
		return nil
	})
	return rerr, stale
}

// walSnapshotPostPinHeld is the C L3401-3441 tail of a pinned read
// transaction with a snapshot armed: when the snapshot equals the refreshed
// live header there is nothing to verify (C skips the whole branch, keeping
// the loop's *pChanged). Otherwise the snapshot must be valid — the WAL not
// wrapped (salt unchanged) and no checkpoint attempted frames past it
// (mxFrame >= nBackfillAttempted) — and the connection's header is
// OVERWRITTEN with the snapshot image (C overwrites *pChanged with the
// pre-open comparison at L3432: when the snapshot equals the cached header
// the pager's caches are still valid) and minFrame resets to 1 (L3437 — a
// non-current reader may not ignore any log prefix). Returns stale for the
// ERROR_SNAPSHOT exit. Caller holds the wal-index writer section and the
// shared CKPT lock.
func (w *walWriter) walSnapshotPostPinHeld(changed *bool, snapChanged bool) (stale bool) {
	if w.snapshot == nil || w.snapshot.hdrEqual(&w.hdr) {
		return false
	}
	info := w.wi.ckptInfoLocked()
	if w.snapshot.ASalt == w.hdr.ASalt && w.snapshot.MxFrame >= info.NBackfillAttempted {
		w.hdr = WalIndexHdr(*w.snapshot) // wal.c L3430: read at the snapshot
		*changed = snapChanged
		w.minFrame = 1 // wal.c L3437
		return false
	}
	return true // wal.c L3433: SQLITE_ERROR_SNAPSHOT
}

// walReadRetryPace paces read-retry rounds (walTryBeginRead's entry sleep):
// a busy timeout takes precedence (the pager-level busy handler), then C's
// cnt>5 delay kicks in.
func (w *walWriter) walReadRetryPace(cnt int, deadline time.Time) {
	if w.busyTimeout > 0 && time.Now().Before(deadline) {
		time.Sleep(walBusySleep)
	} else if cnt >= walBusyEarlyRounds {
		time.Sleep(walRetrySleep)
	}
}

// Read-pin outcomes of tryLockZeroHeld.
const (
	rdPinNone  = iota // READ_LOCK(0) busy: fall through to mark selection (C)
	rdPinOK           // pinned: the reader ignores the log entirely
	rdPinRetry        // pinned but the header moved: unlocked, retry the round
)

// tryLockZeroHeld takes the READ_LOCK(0) "the log is fully backfilled" pin
// (wal.c L3122-3147): on success the reader ignores the WAL and reads the
// main database file. A moved shared header between the header read and the
// lock means frames were appended before the pin — reading the main file
// could serve a torn image, so the pin is dropped and the round retries
// (wal.c's retry comment at L3125). Caller holds the wal-index writer
// section.
func (w *walWriter) tryLockZeroHeld(info *WalCkptInfo) int {
	if w.markTryLock(0, 1, false) != nil {
		return rdPinNone // BUSY (a checkpointer holds it): C falls through
	}
	if w.walHdrMovedHeld() {
		w.markUnlock(0, 1, false)
		return rdPinRetry
	}
	w.readLock = 0
	w.minFrame = info.NBackfill + 1
	return rdPinOK
}

// walTryBeginReadHeld runs one walTryBeginRead round with the wal-index
// mutex held: the fully-backfilled READ_LOCK(0) shortcut (skipped when
// lock0Allowed is false — C's useWal flag), read-mark selection, mark
// bumping, the shared pin and its mark/header verification. Returns
// errWalRetry to signal the caller to run another round.
// Caller holds the wal-index writer section.
func (w *walWriter) walTryBeginReadHeld(lock0Allowed bool) error {
	info := w.wi.ckptInfoLocked()
	if lock0Allowed {
		done, err := w.tryLockZeroShortcutHeld(&info)
		if done {
			return err
		}
	}
	// C wal.c L3157-3160: an armed snapshot caps the frame universe — no
	// mark may exceed the snapshot's mxFrame.
	mxFrame := w.hdr.MxFrame
	if w.snapshot != nil && w.snapshot.MxFrame < mxFrame {
		mxFrame = w.snapshot.MxFrame
	}
	mxI, mxReadMark := w.walBumpOrSelectHeld(&info, mxFrame)
	if mxI == 0 {
		// Every mark is above mxFrame and every slot is pinned: retry
		// (C returns WAL_RETRY on SQLITE_BUSY here).
		return errWalRetry
	}
	if w.markTryLock(mxI, 1, false) != nil {
		return errWalRetry // BUSY on the shared pin: transient, retry
	}
	// With the pin held, re-read the checkpoint info: minFrame becomes the
	// first frame this reader may need from the log (everything at or below
	// nBackfill is checkpointed), and neither the mark nor the shared
	// header may have moved (wal.c L3239-3245).
	info = w.wi.ckptInfoLocked()
	w.minFrame = info.NBackfill + 1
	if info.AReadMark[mxI] != mxReadMark || w.walHdrMovedHeld() {
		w.markUnlock(mxI, 1, false)
		return errWalRetry
	}
	w.readLock = mxI
	return nil
}

// tryLockZeroShortcutHeld decides and takes the READ_LOCK(0) "ignore the
// log" pin (wal.c L3114-3147): it applies only when the log is fully
// backfilled (nBackfill == mxFrame) AND no snapshot is in play — the
// transaction-open path for sqlite3_snapshot_get sets bGetSnapshot, and an
// armed pSnapshot both exclude it (L3114-3117: taking mark 0 would let a
// later writer wrap the log and destroy the snapshot while the transaction
// holds) unless mxFrame==0, where there is nothing to preserve. done=true
// means the round is decided (err nil = pinned; errWalRetry = header moved
// under the pin); done=false falls through to read-mark selection. Caller
// holds the wal-index writer section.
func (w *walWriter) tryLockZeroShortcutHeld(info *WalCkptInfo) (done bool, err error) {
	if (w.snapshot != nil || w.bGetSnapshot) && w.hdr.MxFrame != 0 {
		return false, nil
	}
	if info.NBackfill != w.hdr.MxFrame {
		return false, nil
	}
	switch w.tryLockZeroHeld(info) {
	case rdPinOK:
		return true, nil
	case rdPinRetry:
		return true, errWalRetry
	}
	return false, nil
}

// walBumpOrSelectHeld selects the read-mark slot to pin (the largest mark ≤
// mxFrame) and, when the newest mark is older than mxFrame, bumps the first
// free mark up to mxFrame under a transient exclusive lock (wal.c L3162-
// 3185). Returns mxI == 0 when no slot qualifies. Caller holds the wal-index
// writer section.
func (w *walWriter) walBumpOrSelectHeld(info *WalCkptInfo, mxFrame uint32) (mxI int, mxReadMark uint32) {
	mxI, mxReadMark = walSelectReadMarkHeld(info, mxFrame)
	if mxI != 0 && mxReadMark >= mxFrame {
		return mxI, mxReadMark
	}
	for i := 1; i < WalNReader; i++ {
		if w.markTryLock(i, 1, true) != nil {
			continue // BUSY: that mark is pinned by an active reader
		}
		w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.AReadMark[i] = mxFrame })
		mxReadMark, mxI = mxFrame, i
		w.markUnlock(i, 1, true)
		return mxI, mxReadMark
	}
	return mxI, mxReadMark
}

// markTryLock takes shm locks on READ_LOCK(mark) for the read-mark
// protocol; idx is the read-mark index 0..4 (the shm slot is 3+idx).
// locking_mode=EXCLUSIVE short-circuits every shm lock to a no-op (wal.c
// walLockShared/walLockExclusive) — the held lock helpers must honor it the
// same way.
func (w *walWriter) markTryLock(mark, n int, excl bool) error {
	if w.exclusiveMode {
		return nil
	}
	return w.wi.shmTryLockHeld(walReadLockIdx(mark), n, excl)
}

// markUnlock is markTryLock's release.
func (w *walWriter) markUnlock(mark, n int, excl bool) {
	if w.exclusiveMode {
		return
	}
	w.wi.shmUnlockHeld(walReadLockIdx(mark), n, excl)
}

// walSelectReadMarkHeld ports walTryBeginRead's aReadMark[] scan: the index
// (1..4) of the largest mark not exceeding mxFrame, and that mark's value
// (0 when no slot qualifies — READMARK_NOT_USED entries never fit). Caller
// holds the wal-index writer section.
func walSelectReadMarkHeld(info *WalCkptInfo, mxFrame uint32) (mxI int, mxReadMark uint32) {
	for i := 1; i < WalNReader; i++ {
		if thisMark := info.AReadMark[i]; mxReadMark <= thisMark && thisMark <= mxFrame {
			mxReadMark = thisMark
			mxI = i
		}
	}
	return mxI, mxReadMark
}

// walHdrMovedHeld compares the live shared wal-index header against the
// connection's cached copy (C's memcmp(walIndexHdr(pWal), &pWal->hdr)). A
// live read that does not parse counts as moved. Caller holds the wal-index
// writer section.
func (w *walWriter) walHdrMovedHeld() bool {
	pin := w.hdr
	changed, ok := w.wi.tryRefreshLocked(&pin)
	return !ok || changed
}

// walEndReadTxn releases the shared read-mark lock at the end of the read
// transaction — the sqlite3WalEndReadTransaction port (wal.c L3486). The
// next read transaction re-runs walBeginReadTxn and observes every commit
// that landed in between. Idempotent.
func (w *walWriter) walEndReadTxn() {
	if w.readLock < 0 {
		return
	}
	if !w.exclusiveMode {
		w.wi.shmUnlock(walReadLockIdx(w.readLock), 1, false)
	}
	w.readLock = -1
	w.minFrame = 0
}

// walRestartLogHeld ports walRestartLog (wal.c L3852) for a writer whose
// read transaction pinned READ_LOCK(0): the log is fully backfilled, so the
// new transaction's frames may overwrite it from frame 1. The restart runs
// under exclusive READ_LOCK(1..4) when no other reader holds a mark (a BUSY
// keeps appending at the log end); READ_LOCK(0) is then released and a real
// read mark (1..4) is re-selected with C's useWal=1 retry loop, because a
// writer appending frames must not keep the "ignore the WAL" pin. Caller
// holds the WRITER shm lock and the wal-index writer section.
func (w *walWriter) walRestartLogHeld() error {
	info := w.wi.ckptInfoLocked()
	if info.NBackfill > 0 && info.NBackfill == w.hdr.MxFrame {
		if w.markTryLock(1, WalNReader-1, true) == nil {
			if err := w.walRestartHdrLocked(false); err != nil {
				return err
			}
			w.markUnlock(1, WalNReader-1, true)
		}
		// BUSY: readers still use the log — append at its end (C keeps rc).
	}
	// Release the "ignore the WAL" pin and re-select a read mark.
	w.markUnlock(0, 1, false)
	w.readLock = -1
	for i := 0; i < walRetryProtocolLimit; i++ {
		if err := w.walTryBeginReadHeld(false); !errors.Is(err, errWalRetry) {
			// The re-selection cannot fail the commit: a BUSY storm leaves
			// the writer mark-less, which only widens what a concurrent
			// checkpointer may backfill (the frames are already committed
			// content — never a torn read).
			return nil
		}
	}
	return nil
}
