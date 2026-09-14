package pager

// walsnapshot.go — the sqlite3_snapshot C-API layer (P7.WAL-G7 slice 4),
// ported from SQLite src/wal.c SQLITE_ENABLE_SNAPSHOT block (L4496-4602):
// sqlite3WalSnapshotGet (L4501), sqlite3WalSnapshotOpen (L4523),
// sqlite3_snapshot_cmp (L4552), sqlite3WalSnapshotCheck (L4573),
// sqlite3WalSnapshotUnlock (L4595), sqlite3WalSnapshotRecover (L3322) +
// walSnapshotRecover (L3261), and the pSnapshot branch of
// walBeginReadTransaction (L3357-3448, wired in walread.go).
//
// A snapshot is a frozen copy of the connection's cached wal-index header
// (the 48-byte WalIndexHdr image — sqlite3_snapshot's opaque 48 bytes).
// Opening a read transaction on a snapshot caps the pinned mxFrame at the
// snapshot's, verifies — under the shared CKPT lock, so no concurrent
// checkpointer can advance nBackfillAttempted mid-check — that the WAL was
// neither wrapped (salt changed) nor checkpointed past the snapshot
// (mxFrame < nBackfillAttempted), and then runs the read transaction with
// pWal->hdr OVERWRITTEN by the snapshot image (wal.c L3430).
//
// Contract violations report SQLITE_ERROR ("SQL logic error" — the
// sqlite3_errmsg text for a bare SQLITE_ERROR return, main.c
// sqlite3_snapshot_get/open/recover initialize rc=SQLITE_ERROR and return
// it without a message); a stale snapshot reports
// errWalSnapshotStale (SQLITE_ERROR_SNAPSHOT — C defines no message text,
// only the code; the Go text is frigolite's carrier, the code name is
// main.c L1531 sqlite3ErrName's "SQLITE_ERROR_SNAPSHOT").

import (
	"bytes"
	"errors"
)

// Snapshot is a frozen wal-index header image (sqlite3_snapshot). Its
// content is opaque outside the wal layer; salt[0] and mxFrame drive
// SnapshotCmp and the read-transaction open.
type Snapshot WalIndexHdr

// hdrEqual reports whether the snapshot image equals a wal-index header
// byte-for-byte (C's memcmp(pSnapshot, &pWal->hdr, sizeof(WalIndexHdr))).
func (s *Snapshot) hdrEqual(h *WalIndexHdr) bool {
	return *s == Snapshot(*h)
}

// SnapshotSize is the byte size of a snapshot blob
// (sizeof(sqlite3_snapshot) == sizeof(WalIndexHdr) == 48).
const SnapshotSize = 48

// Snapshot error family. errWalSnapshotContract is the bare SQLITE_ERROR the
// C API returns for every contract violation (autocommit on, temp schema,
// write transaction, non-WAL database, empty WAL); errWalSnapshotStale is
// SQLITE_ERROR_SNAPSHOT; errSnapshotBlobFormat is test1.c's "bad SNAPSHOT"
// for a blob whose length is not sizeof(sqlite3_snapshot).
var (
	errWalSnapshotContract = errors.New("SQL logic error")
	errWalSnapshotStale    = errors.New("snapshot is out of date")
	errSnapshotBlobFormat  = errors.New("bad SNAPSHOT")
)

// SnapshotCmp ports sqlite3_snapshot_cmp (wal.c L4552): a positive value if
// snapshot a is newer than b, negative if older, zero when equal. aSalt[0]
// (the WAL header salt, incremented on every WAL restart) dominates, then
// mxFrame.
func SnapshotCmp(a, b *Snapshot) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	if a.ASalt[0] != b.ASalt[0] {
		if a.ASalt[0] < b.ASalt[0] {
			return -1
		}
		return 1
	}
	if a.MxFrame != b.MxFrame {
		if a.MxFrame < b.MxFrame {
			return -1
		}
		return 1
	}
	return 0
}

// SnapshotBlob encodes s into buf[0:SnapshotSize] — little-endian
// WalIndexHdr image (the sqlite3_snapshot_get_blob form, test1.c L2747:
// Tcl_NewByteArrayObj((unsigned char*)pSnapshot, sizeof(sqlite3_snapshot))).
func (s *Snapshot) SnapshotBlob(buf []byte) {
	EncodeWalIndexHdr((*WalIndexHdr)(s), buf[:SnapshotSize])
}

// SnapshotDecode parses a snapshot blob produced by SnapshotBlob. A blob of
// any other length is test1.c's "bad SNAPSHOT".
func SnapshotDecode(blob []byte) (*Snapshot, error) {
	if len(blob) != SnapshotSize {
		return nil, errSnapshotBlobFormat
	}
	s := Snapshot(DecodeWalIndexHdr(blob))
	return &s, nil
}

// ---------------------------------------------------------------------------
// walWriter-level primitives.
// ---------------------------------------------------------------------------

// walSnapshotVerify ports sqlite3WalSnapshotCheck's check body (wal.c
// L4577-4586): the snapshot is stale when its salt differs from the
// connection's cached header (the WAL was wrapped) or its mxFrame is below
// the shared nBackfillAttempted (a checkpoint wrote frames past it — the
// checkpoint need not have completed, wal.c L3405-3424). Caller holds the
// shared CKPT lock (the nBackfillAttempted read must not race a
// checkpointer's PASS1 update).
func (w *walWriter) walSnapshotVerify(s *Snapshot) error {
	var info WalCkptInfo
	_ = w.wi.WriterSection(func() error { info = w.wi.ckptInfoLocked(); return nil })
	if s.ASalt != w.hdr.ASalt || s.MxFrame < info.NBackfillAttempted {
		return errWalSnapshotStale
	}
	return nil
}

// walSnapshotRecoverLocked ports walSnapshotRecover (wal.c L3261-3301):
// reduce nBackfillAttempted so older snapshots become openable again. It
// walks frames from nBackfillAttempted down to nBackfill+1; for each frame
// it compares the WAL page image against the main-file page — on the first
// match (or a page that lies beyond the database file) the frame was
// already checkpointed and the scan stops; otherwise the checkpointer that
// "attempted" it did not get that far and the counter drops. It is not an
// error when nBackfillAttempted cannot be decreased at all. Callers hold
// the exclusive CKPT lock (sqlite3WalSnapshotRecover, wal.c L3322) — which
// excludes other checkpointers, not writers — and an open read transaction
// (the caller contract of main.c sqlite3_snapshot_recover).
func (w *walWriter) walSnapshotRecoverLocked() error {
	info := w.wi.ckptInfoLocked()
	szPage := int64(w.pageSize)
	var dbSize int64
	if st, err := w.p.file.Stat(); err != nil {
		return err
	} else {
		dbSize = st.Size()
	}
	buf1 := make([]byte, szPage) // the WAL frame image (C pBuf1)
	buf2 := make([]byte, szPage) // the database page image (C pBuf2)
	for i := info.NBackfillAttempted; i > info.NBackfill; i-- {
		pgno := w.wi.pgnoAtFrameLocked(i)
		if pgno == 0 {
			// No hash entry for this frame: a corrupt wal-index cannot be
			// probed further (C would read the db at a negative offset and
			// break on the I/O error — same stop, without the bogus read).
			return errWalCorrupt
		}
		match, err := w.walFrameMatchesDbLocked(i, pgno, dbSize, buf1, buf2)
		if err != nil {
			return err
		}
		if match {
			break // frame i is checkpointed content: scan done
		}
		attempted := i - 1
		w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.NBackfillAttempted = attempted })
	}
	return nil
}

// walFrameMatchesDbLocked compares the WAL frame at position i against the
// main-file page it belongs to (wal.c L3281-3294): a page beyond the
// database file was never written there (not checkpointed — C skips the
// compare and keeps decrementing), and any read error propagates (C breaks
// with rc set). Caller holds the exclusive CKPT lock and the wal-index
// writer section preconditions of walSnapshotRecoverLocked.
func (w *walWriter) walFrameMatchesDbLocked(i, pgno uint32, dbSize int64, buf1, buf2 []byte) (bool, error) {
	szPage := int64(w.pageSize)
	iDbOff := int64(pgno-1) * szPage
	if iDbOff+szPage > dbSize {
		return false, nil
	}
	iWalOff := walFrameOffset(int(i), w.pageSize) + WalFrameHdrSize
	if _, err := w.file.ReadAt(buf1, iWalOff); err != nil {
		return false, err
	}
	if _, err := w.p.file.ReadAt(buf2, iDbOff); err != nil {
		return false, err
	}
	return bytes.Equal(buf1, buf2), nil
}

// ---------------------------------------------------------------------------
// Pager-level API (the engine's snapshot entry points; native tests).
// ---------------------------------------------------------------------------

// WALSnapshotGet ports sqlite3PagerSnapshotGet + sqlite3WalSnapshotGet
// (pager.c L7736, wal.c L4501) for a connection inside a transaction: it
// opens the connection's read transaction if it is not already open (main.c
// sqlite3_snapshot_get L4998 runs sqlite3BtreeBeginTrans(0) first, arming
// the bGetSnapshot flag so the open skips the fully-backfilled READ_LOCK(0)
// pin — a later WAL wrap must not destroy the snapshot behind the caller's
// open transaction) and returns a frozen copy of the pinned wal-index
// header. The first bool is the read-txn-open pChanged signal (the engine
// must drop its derived caches when set). Errors (all bare SQLITE_ERROR
// contract violations per main.c L4976): a write transaction is open
// (SQLITE_TXN_WRITE), the database is not in WAL mode (pPager->pWal == 0),
// or the WAL is empty (aFrameCksum+salt all zero — wal.c L4509).
func (p *Pager) WALSnapshotGet() (bool, *Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil {
		return false, nil, errWalSnapshotContract
	}
	w := p.wal
	if w.writeLock {
		return false, nil, errWalSnapshotContract // C TXN_WRITE, main.c L4997
	}
	w.bGetSnapshot = true // sqlite3PagerSnapshotOpen(pPager, &dummy) — iVersion==0
	changed, err := p.walIndexRefreshLocked()
	w.bGetSnapshot = false // sqlite3PagerSnapshotOpen(pPager, 0)
	if err != nil {
		return false, nil, err
	}
	// wal.c L4508-4511: a zeroed aFrameCksum+aSalt means the WAL was never
	// written — there is nothing to snapshot (memcmp against aZero[4], 16
	// bytes spanning aFrameCksum[2] and aSalt[2]).
	if w.hdr.AFrameCksum == [2]uint32{} && w.hdr.ASalt == [2]uint32{} {
		return false, nil, errWalSnapshotContract
	}
	snap := Snapshot(w.hdr)
	return changed, &snap, nil
}

// WALSnapshotOpen ports sqlite3_snapshot_open's pager dance (main.c L5016 +
// pager.c L7749): the NEXT (or current, see below) read transaction on this
// connection is opened on the snapshot — pinned at the snapshot's mxFrame
// and verified against the live wal-index (SQLITE_ERROR_SNAPSHOT /
// errWalSnapshotStale when the WAL was wrapped or checkpointed past the
// snapshot; "database is locked" when the CKPT lock is busy). The bool is
// the pChanged signal (the engine must drop its derived caches when set).
//
//	main.c contract mirrored here and by the engine caller:
//	- the connection must be inside a transaction (autocommit off),
//	- no write transaction may be open (SQLITE_TXN_WRITE),
//	- the database must be in WAL mode (pPager->pWal == 0 → SQLITE_ERROR).
//
// When a read transaction is ALREADY open on this connection (main.c
// L5034-5046: TXN_READ, no active statements), the snapshot is verified
// under the shared CKPT lock, the open transaction is ended
// (sqlite3BtreeCommit) and a new one opened on the snapshot; the shared
// CKPT lock is held across the whole dance (bUnlock, main.c L5039-5057).
// A nil snapshot opens a plain read transaction (C arms nothing and
// sqlite3BtreeBeginTrans runs unanchored).
func (p *Pager) WALSnapshotOpen(s *Snapshot) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil {
		return false, errWalSnapshotContract
	}
	w := p.wal
	if w.writeLock {
		return false, errWalSnapshotContract // C TXN_WRITE, main.c L5032
	}
	if s == nil {
		return p.walIndexRefreshLocked()
	}
	if w.readLock >= 0 {
		// Read transaction already open: verify-then-reopen under the CKPT
		// lock (wal.c walBeginReadTransaction takes the same lock inside the
		// reopen; shared locks coexist).
		if err := w.walLockShared(walLockCkpt, 1); err != nil {
			return false, err // SQLITE_BUSY: a checkpointer holds the slot
		}
		if err := w.walSnapshotVerify(s); err != nil {
			w.walUnlockShared(walLockCkpt, 1)
			return false, err
		}
		w.walEndReadTxn() // sqlite3BtreeCommit ends the stale read txn
		w.snapshot = s    // sqlite3WalSnapshotOpen (wal.c L4540)
		changed, err := p.walIndexRefreshLocked()
		w.snapshot = nil // sqlite3PagerSnapshotOpen(pPager, 0)
		w.walUnlockShared(walLockCkpt, 1) // sqlite3PagerSnapshotUnlock
		return changed, err
	}
	w.snapshot = s
	changed, err := p.walIndexRefreshLocked()
	w.snapshot = nil
	return changed, err
}

// WALSnapshotCheck ports sqlite3WalSnapshotCheck (wal.c L4573) for a
// connection with a read transaction open: it takes the shared CKPT lock,
// verifies the snapshot is still available (salt unchanged, mxFrame not
// below nBackfillAttempted) and — ONLY on that failure — releases the lock
// again before returning errWalSnapshotStale. On success the lock stays
// held; the caller releases it with WALSnapshotUnlock (pager.c L7788
// contract: SQLITE_BUSY when the checkpointer lock cannot be obtained, the
// lock always released on any error).
func (p *Pager) WALSnapshotCheck(s *Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil {
		return errWalSnapshotContract
	}
	if err := p.wal.walLockShared(walLockCkpt, 1); err != nil {
		return err
	}
	if err := p.wal.walSnapshotVerify(s); err != nil {
		p.wal.walUnlockShared(walLockCkpt, 1)
		return err
	}
	return nil
}

// WALSnapshotUnlock releases the shared CKPT lock taken by a successful
// WALSnapshotCheck (sqlite3WalSnapshotUnlock, wal.c L4595).
func (p *Pager) WALSnapshotUnlock() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal != nil {
		p.wal.walUnlockShared(walLockCkpt, 1)
	}
}

// WALSnapshotRecover ports sqlite3_snapshot_recover (main.c L5072) +
// sqlite3WalSnapshotRecover (wal.c L3322): with no transaction open on this
// connection (SQLITE_TXN_NONE — main.c L5085; a bare deferred BEGIN counts
// as none, a transaction with a pinned read or an open write does not), it
// opens a read transaction, takes the EXCLUSIVE CKPT lock and walks back
// nBackfillAttempted as far as the main-file content proves safe (see
// walSnapshotRecoverLocked), then ends the read transaction (main.c
// L5088's sqlite3BtreeCommit). A non-WAL database is a bare SQLITE_ERROR
// (pager.c L7766).
func (p *Pager) WALSnapshotRecover() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil {
		return errWalSnapshotContract
	}
	w := p.wal
	if w.readLock >= 0 || w.writeLock {
		return errWalSnapshotContract // C SQLITE_TXN_NONE check, main.c L5085
	}
	if _, err := p.walIndexRefreshLocked(); err != nil { // sqlite3BtreeBeginTrans(0)
		return err
	}
	// sqlite3WalSnapshotRecover (wal.c L3322): exclusive CKPT lock; plain
	// F_SETLK semantics — busy reports immediately (no busy-handler loop in
	// C here either).
	err := w.walLockExclusive(walLockCkpt, 1)
	if err == nil {
		err = w.walSnapshotRecoverLocked()
		w.walUnlockExclusive(walLockCkpt, 1)
	}
	w.walEndReadTxn() // sqlite3BtreeCommit(pBt), main.c L5088
	return err
}
