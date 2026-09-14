package pager

// wal.go — SQLite WAL write path (src/wal.c port), P7.WAL-G7 slice 1:
// multi-connection routing through the process-wide WALIndex registry.
//
// Every connection owns a walWriter (C's Wal handle): its own "-wal" fd, its
// cached copy of the wal-index header (hdr) and the running frame-checksum
// chain. The wal-index itself (the "-shm" sidecar) is SHARED: one WALIndex
// per database path holds the double-buffered WalIndexHdr, the WalCptInfo
// checkpoint info and the pgno→frame hash tables, persisted to the -shm file
// (walindex.go / walindexregistry.go).
//
// Writer protocol (wal.c walFrames): under the WRITER lock (slice 1: the
// registry's WriterSection) re-read the shared header, append frames to the
// -wal continuing the checksum chain, append pgno→frame entries to the hash
// tables, then publish the new header double-buffered (iChange++ per commit).
// Reader protocol: pages are resolved through walIndexFind → frame → page
// bytes, falling back to the main database file (pager.c readDbPage).
//
// The frame/header byte layout and checksum chain follow walview.go's decoder
// (validated against oracle fixtures), so recovery can read what the writer
// produces and vice versa.

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
)

// walWriter implements the SQLite WAL write path for one connection.
type walWriter struct {
	p        *Pager
	path     string // "-wal" path
	file     *os.File
	pageSize uint32
	cksum1   uint32 // running frame-checksum chain (aFrameCksum seed)
	cksum2   uint32
	nFrame   int // number of frames in the log (1-based next index)
	// wi is the shared wal-index this connection attached to (one per
	// database path, refcounted by the WALIndexRegistry).
	wi *WALIndex
	// hdr is the connection's cached copy of the shared wal-index header
	// (C's pWal->hdr): mxFrame/nPage/iChange/aFrameCksum/aSalt/szPage. It is
	// ALSO the read-snapshot pin: sqlite3WalBeginWriteTransaction compares it
	// against the shared header under the WRITER lock and fails with
	// SQLITE_BUSY_SNAPSHOT when another connection committed since.
	hdr WalIndexHdr
	// nCkpt is the checkpoint sequence from the WAL file header.
	nCkpt uint32
	// writeLock/ckptLock mirror C's pWal->writeLock/ckptLock: which shm locks
	// this connection currently holds (released on Close).
	writeLock bool
	ckptLock  bool
	// readLock mirrors C's pWal->readLock: the shared WAL_READ_LOCK this
	// connection holds for its open read transaction (-1 none, 0..4 the
	// read-mark index; 0 pins the "ignore the WAL" state). Set by
	// walBeginReadTxn (walread.go, the walTryBeginRead port), released by
	// walEndReadTxn at the end of the read transaction or on Close.
	readLock int
	// minFrame mirrors C's pWal->minFrame: the first frame not yet
	// checkpointed at pin time (nBackfill+1). walIndexFind skips frames
	// below it — they are already in the main file and the hash tables may
	// hold stale entries for them.
	minFrame uint32
	// exclusiveMode is locking_mode=EXCLUSIVE: every shm lock call becomes a
	// no-op (wal.c walLockShared/walLockExclusive).
	exclusiveMode bool
	// busyTimeout is sqlite3_busy_timeout: contended shm lock acquisitions
	// retry until the deadline, then report "database is locked".
	busyTimeout time.Duration
}

// walMagicLE is the little-endian-checksum WAL magic (WalMagic); the LSB 0
// selects little-endian 32-bit word interpretation of the checksum data.
const walMagicLE = WalMagic // 0x377f0682

// openWal opens (or creates) the "-wal" file for dbPath and attaches to the
// shared wal-index. The shared header is read (walIndexReadHdr port, with the
// slice-2 lock dance: recovery runs under the WRITER lock and a contended
// recovery reports BUSY_RECOVERY / SQLITE_PROTOCOL); committed frames are NOT
// replayed into the page cache — reads resolve through the wal-index (see
// Pager.readPageLocked).
func openWal(p *Pager, dbPath string, pageSize uint32) (*walWriter, error) {
	walPath := dbPath + "-wal"
	f, err := os.OpenFile(walPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("pager: open wal %s: %w", walPath, err)
	}
	wi, err := acquireWALIndex(dbPath)
	if err != nil {
		f.Close()
		return nil, err
	}
	w := &walWriter{p: p, path: walPath, file: f, pageSize: pageSize, wi: wi, readLock: -1}
	if err := w.walInitHeader(); err != nil {
		wi.release()
		f.Close()
		return nil, err
	}
	return w, nil
}

// walInitHeader loads the shared wal-index header (recovering from the -wal
// through the WRITER-lock dance when it does not parse), rebuilds when the
// shared header outlives its -wal file, and adopts the WAL file's header
// state into the writer.
func (w *walWriter) walInitHeader() error {
	if _, err := w.walIndexReadHdr(); err != nil {
		return err
	}
	stale := false
	_ = w.wi.WriterSection(func() error {
		stale = w.hdr.MxFrame > 0 && !w.walFileHeaderSaltsMatchLocked()
		return nil
	})
	if stale {
		// The shared header outlived its WAL file (restarted/truncated by
		// another generation): the -wal is the authority — rebuild.
		if err := w.walRecoverWithWriterLock(); err != nil {
			return err
		}
	}
	return w.wi.WriterSection(w.initHeaderAdoptLocked)
}

// initHeaderAdoptLocked seeds the writer's derived state from the (parsed)
// shared header. Caller holds the wal-index writer section.
func (w *walWriter) initHeaderAdoptLocked() error {
	if w.hdr.IVersion != WalIndexMaxVersion {
		return fmt.Errorf("pager: open wal %s: unable to open database file", w.path)
	}
	if err := w.syncChainFromWalFileLocked(); err != nil {
		return err
	}
	w.nFrame = int(w.hdr.MxFrame)
	if w.hdr.MxFrame > 0 {
		w.cksum1, w.cksum2 = w.hdr.AFrameCksum[0], w.hdr.AFrameCksum[1]
	}
	return nil
}

// walIndexReadHdr ports walIndexReadHdr (wal.c L2640) + walTryBeginRead's
// busy conversion (wal.c L3043): try the lockless header read; when it does
// not parse, take the WRITER lock and run walIndexRecover. A contended WRITER
// (or recovery lock) converts per C: RECOVER held by someone ⇒ BUSY_RECOVERY
// ("database is locked"); RECOVER free ⇒ brief retry, up to
// WAL_RETRY_PROTOCOL_LIMIT rounds ⇒ SQLITE_PROTOCOL ("locking protocol").
// With a busy timeout set, the BUSY_RECOVERY round also waits (pager.c wraps
// the whole read in the busy handler). Reports whether the shared header
// changed since the connection's cached copy (the pChanged signal).
func (w *walWriter) walIndexReadHdr() (bool, error) {
	var deadline time.Time
	if w.busyTimeout > 0 {
		deadline = time.Now().Add(w.busyTimeout)
	}
	changed := false
	for cnt := 0; ; cnt++ {
		ch, ok := w.tryHeaderRefresh()
		changed = changed || ch
		if ok {
			return changed, nil
		}
		rc := w.walRecoverWithWriterLock()
		switch {
		case rc == nil:
			continue // recovered — the next round's parse succeeds
		case errors.Is(rc, errWalRetry):
			if cnt >= walRetryProtocolLimit {
				return changed, errWalProtocol
			}
			if cnt >= walBusyEarlyRounds {
				time.Sleep(walRetrySleep)
			}
		case errors.Is(rc, errWalBusyRecovery) && w.busyTimeout > 0 && time.Now().Before(deadline):
			time.Sleep(walBusySleep)
		default:
			return changed, rc
		}
	}
}

// tryHeaderRefresh runs one lockless walIndexTryHdr round and adopts the
// shared state into the connection's pin when it moved. Reports whether the
// header parsed and whether the pin moved. Caller must NOT hold the wal-index
// mutex.
func (w *walWriter) tryHeaderRefresh() (changed, ok bool) {
	_ = w.wi.WriterSection(func() error {
		ch, parsed := w.wi.tryRefreshLocked(&w.hdr)
		if parsed && ch {
			w.adoptHeaderLocked()
			changed = true
		}
		ok = parsed
		return nil
	})
	return changed, ok
}

// walRecoverWithWriterLock runs walIndexRecover under the WRITER lock (its C
// caller contract: the WRITER byte is held before the recovery locks). On a
// contended WRITER or a contended recovery lock it applies walTryBeginRead's
// conversion: probe the RECOVER lock shared — held ⇒ BUSY_RECOVERY (a
// recovery is running), free ⇒ WAL_RETRY (the holder will release; retry).
func (w *walWriter) walRecoverWithWriterLock() error {
	if err := w.walLockExclusive(walLockWrite, 1); err != nil {
		return w.busyToRetryOrRecovery(err)
	}
	w.writeLock = true
	err := w.wi.WriterSection(func() error {
		return w.walIndexRecoverLocked()
	})
	w.walUnlockExclusive(walLockWrite, 1)
	w.writeLock = false
	if err != nil && errors.Is(err, errWalBusy) {
		return w.busyToRetryOrRecovery(err)
	}
	return err
}

// busyToRetryOrRecovery converts a contended lock per walTryBeginRead: when
// the RECOVER lock is held by another connection the result is
// BUSY_RECOVERY; otherwise the contention is transient (WAL_RETRY).
func (w *walWriter) busyToRetryOrRecovery(busy error) error {
	if err := w.walLockShared(walLockRecover, 1); err != nil {
		if errors.Is(err, errWalBusy) {
			return errWalBusyRecovery
		}
		return err
	}
	w.walUnlockShared(walLockRecover, 1)
	_ = busy
	return errWalRetry
}

// walFileHeaderSaltsMatchLocked reports whether the -wal file carries a valid
// header whose salts match the shared wal-index header. Caller holds the
// wal-index writer section.
func (w *walWriter) walFileHeaderSaltsMatchLocked() bool {
	buf := make([]byte, WalHdrSize)
	n, err := w.file.ReadAt(buf, 0)
	if err != nil || n < WalHdrSize {
		return false
	}
	h, derr := DecodeWalHeader(buf)
	return derr == nil && h.HeaderCksumOK &&
		h.Salt1 == w.hdr.ASalt[0] && h.Salt2 == w.hdr.ASalt[1]
}

// syncChainFromWalFileLocked seeds the writer's frame-checksum chain from the
// -wal file header (the chain seed of frame 1 is the WAL header checksum,
// wal.c walFrames' iFrame==0 branch) and re-writes the file header when it is
// missing, corrupt or inconsistent with the shared wal-index header. Caller
// holds the wal-index writer section.
func (w *walWriter) syncChainFromWalFileLocked() error {
	buf := make([]byte, WalHdrSize)
	n, err := w.file.ReadAt(buf, 0)
	if err == nil && n == WalHdrSize {
		if h, derr := DecodeWalHeader(buf); derr == nil && h.HeaderCksumOK &&
			h.Salt1 == w.hdr.ASalt[0] && h.Salt2 == w.hdr.ASalt[1] &&
			h.PageSize == w.pageSize {
			w.nCkpt = h.CheckpointSeq
			w.hdr.BigEndCksum = h.BigEndCksum
			if w.hdr.MxFrame == 0 {
				w.cksum1, w.cksum2 = h.Checksum1, h.Checksum2
			}
			return nil
		}
	}
	// No usable on-disk header: (re)write one from the shared header state.
	if w.hdr.ASalt[0] == 0 && w.hdr.ASalt[1] == 0 {
		// Fresh WAL: random salts (wal.c walFrames writes a fresh header with
		// sqlite3_randomness salts when nCkpt==0).
		var s [8]byte
		if _, err := rand.Read(s[:]); err != nil {
			return err
		}
		w.hdr.ASalt = [2]uint32{
			binary.BigEndian.Uint32(s[:4]),
			binary.BigEndian.Uint32(s[4:]),
		}
		w.nCkpt = 0
	}
	return w.writeWalFileHeaderLocked()
}

// writeWalFileHeaderLocked writes the 32-byte WAL file header from the shared
// header's identity (salts, page size, checkpoint sequence) and seeds the
// frame-checksum chain from it. Caller holds the wal-index writer section.
func (w *walWriter) writeWalFileHeaderLocked() error {
	hdr := make([]byte, WalHdrSize)
	binary.BigEndian.PutUint32(hdr[0:], walMagicLE)
	binary.BigEndian.PutUint32(hdr[4:], WalMaxVersion)
	binary.BigEndian.PutUint32(hdr[8:], w.pageSize)
	binary.BigEndian.PutUint32(hdr[12:], w.nCkpt)
	binary.BigEndian.PutUint32(hdr[16:], w.hdr.ASalt[0])
	binary.BigEndian.PutUint32(hdr[20:], w.hdr.ASalt[1])
	hc1, hc2 := WalChecksumBytes(false, hdr[:WalFrameHdrSize], 0, 0)
	binary.BigEndian.PutUint32(hdr[24:], hc1)
	binary.BigEndian.PutUint32(hdr[28:], hc2)
	// I/O fault injection (test_syscall equivalent): abort before writing.
	if w.p.walFault != nil {
		if fe := w.p.walFault("write"); fe != nil {
			return fe
		}
	}
	if _, err := w.file.WriteAt(hdr, 0); err != nil {
		return fmt.Errorf("pager: write wal header: %w", err)
	}
	w.cksum1, w.cksum2 = hc1, hc2
	w.hdr.BigEndCksum = false
	setPageSizeForHdr(&w.hdr, w.pageSize)
	if w.hdr.MxFrame == 0 {
		// A frame-less WAL owns the shared header identity.
		w.wi.writeHdrLocked(&w.hdr)
	}
	return nil
}

// writerRefreshLocked ports the shared-header synchronization of walFrames:
// re-read the shared header and adopt its frame state when it moved. Unlike
// the read path it does not recover a corrupt header (the WRITER lock that
// recovery requires is not held here — callers took only the CKPT lock);
// a checkpoint on an unparsable header proceeds with the connection's cached
// snapshot, exactly as C reads pWal->hdr without re-validating.
// Caller holds the wal-index writer section.
func (w *walWriter) writerRefreshLocked() error {
	changed, ok := w.wi.tryRefreshLocked(&w.hdr)
	if ok && changed {
		w.adoptHeaderLocked()
	}
	return nil
}

// adoptHeaderLocked syncs the writer's derived state (next frame index,
// checksum chain, salts) from the cached shared header. Caller holds the
// wal-index writer section.
func (w *walWriter) adoptHeaderLocked() {
	if w.hdr.MxFrame > 0 {
		w.nFrame = int(w.hdr.MxFrame)
		w.cksum1, w.cksum2 = w.hdr.AFrameCksum[0], w.hdr.AFrameCksum[1]
		return
	}
	// Frame-less log: the chain seed is the WAL file header's checksum.
	w.nFrame = 0
	if w.hdr.ASalt[0] == 0 && w.hdr.ASalt[1] == 0 {
		var s [8]byte
		if _, err := rand.Read(s[:]); err != nil {
			w.hdr.ASalt = [2]uint32{0xa5a5a5a5, 0x5a5a5a5a}
		} else {
			w.hdr.ASalt = [2]uint32{
				binary.BigEndian.Uint32(s[:4]),
				binary.BigEndian.Uint32(s[4:]),
			}
		}
	}
	if err := w.syncChainFromWalFileLocked(); err != nil {
		// Best-effort: the chain falls back to the cached values.
		_ = err
	}
}

// appendFrame writes one WAL frame for page pg. When commit is true the frame
// records the post-transaction database size (commitDBSize) and is the
// transaction's commit record (wal.c mxFrame). The cumulative checksum chain
// is extended and stored in the frame header.
func (w *walWriter) appendFrame(pg *Page, commit bool, dbSize uint32) error {
	frameSize := int64(w.pageSize) + WalFrameHdrSize
	off := WalHdrSize + int64(w.nFrame)*frameSize
	fh := make([]byte, frameSize)
	binary.BigEndian.PutUint32(fh[0:], pg.PageNum)
	if commit {
		binary.BigEndian.PutUint32(fh[4:], dbSize)
	}
	binary.BigEndian.PutUint32(fh[8:], w.hdr.ASalt[0])
	binary.BigEndian.PutUint32(fh[12:], w.hdr.ASalt[1])
	// Checksum chain: seed from the running (header) checksum, extend over the
	// frame header's first 8 bytes then the page data (wal.c validity rule).
	w.cksum1, w.cksum2 = WalChecksumBytes(w.hdr.BigEndCksum, fh[:8], w.cksum1, w.cksum2)
	w.cksum1, w.cksum2 = WalChecksumBytes(w.hdr.BigEndCksum, pg.Data, w.cksum1, w.cksum2)
	binary.BigEndian.PutUint32(fh[16:], w.cksum1)
	binary.BigEndian.PutUint32(fh[20:], w.cksum2)
	copy(fh[WalFrameHdrSize:], pg.Data)
	// I/O fault injection (test_syscall equivalent): abort before writing.
	if w.p.walFault != nil {
		if fe := w.p.walFault("write"); fe != nil {
			return fe
		}
	}
	if _, err := w.file.WriteAt(fh, off); err != nil {
		return fmt.Errorf("pager: write wal frame: %w", err)
	}
	w.nFrame++
	return nil
}

// commit writes all currently-dirty pages of the pager as WAL frames, marking
// the final frame as the commit record (post-transaction database size =
// p.numPages). It is the sqlite3WalBeginWriteTransaction + walFrames port:
// the WRITER shm lock is taken first (busy-handler aware — a contended
// writer reports "database is locked" once the busy timeout expires), then
// the snapshot-consistency check runs (another connection committing since
// this connection's read snapshot pins the header ⇒ SQLITE_BUSY_SNAPSHOT,
// "database is locked", walprotocol2-2.2/2.3 — retried while the busy
// timeout allows, 2.4/2.5), and the frames are appended under the lock.
// Frames are recorded in the shared wal-index hash tables and the shared
// header is published double-buffered (mxFrame, nPage, iChange++,
// aFrameCksum). The wal hook (sqlite3_wal_hook) fires after the commit with
// the number of frames appended.
func (w *walWriter) commit() (int, error) {
	p := w.p
	if len(p.dirty) == 0 {
		// Nothing to commit; still end the write transaction the eager
		// statement gate may have opened (a write-class statement that
		// affected no pages — UPDATE matching nothing and friends).
		p.walEndWriteLocked()
		return 0, nil
	}
	// Deterministic order: sort dirty page numbers ascending.
	pages := make([]*Page, 0, len(p.dirty))
	for n := range p.dirty {
		if pg, ok := p.pages[n]; ok {
			pages = append(pages, pg)
		}
	}
	// Stable insertion sort (small N; avoids importing sort into hot path).
	for i := 1; i < len(pages); i++ {
		for j := i; j > 0 && pages[j].PageNum < pages[j-1].PageNum; j-- {
			pages[j], pages[j-1] = pages[j-1], pages[j]
		}
	}
	// The WRITER shm lock is already held for an open write transaction
	// (walBeginWriteLocked, taken at the first dirty page). Transactions
	// that dirtied pages through allocation-only paths acquire it here as a
	// fallback.
	if !w.writeLock {
		if err := w.walBusyLockExclusive(walLockWrite, 1); err != nil {
			return 0, err
		}
		w.writeLock = true
	}
	appended := 0
	var commitErr error
	func() {
		defer p.walEndWriteLocked()
		if err := w.wi.WriterSection(func() error { return w.commitLocked(pages, &appended) }); err != nil {
			commitErr = err
		}
	}()
	if commitErr != nil {
		return 0, commitErr
	}
	if p.walHook != nil {
		p.walHook(appended, 0)
	}
	return appended, nil
}

// commitPrepareLocked synchronizes the writer with the shared wal-index
// before frames are appended: the snapshot-consistency check
// (sqlite3WalBeginWriteTransaction's memcmp — the WRITER lock has been held
// since the first dirty page, so the shared header cannot have moved since
// the pin was checked there), walRestartLog (wal.c L3852: when the log is
// fully backfilled and no readers hold read marks, the new frames overwrite
// the log from frame 1; contended read locks skip the restart — the BUSY
// branch keeps appending at the log end) and the frame-less-log file header
// (walFrames' iFrame==0 branch — e.g. right after a TRUNCATE checkpoint
// zeroed the file). Caller holds the WRITER lock and the wal-index writer
// section.
func (w *walWriter) commitPrepareLocked() error {
	pin := w.hdr
	changed, ok := w.wi.tryRefreshLocked(&pin)
	if !ok {
		// Header corrupted under the held WRITER: recover (its caller
		// contract) — the rebuilt state becomes the pin.
		if err := w.walIndexRecoverLocked(); err != nil {
			return err
		}
		return nil
	}
	if changed && !pin.Equal(&w.hdr) {
		return errWalBusySnapshot
	}
	if changed {
		w.adoptHeaderLocked()
	}
	if w.readLock == 0 {
		// walRestartLog (wal.c L3852): this writer's read transaction pins
		// READ_LOCK(0) — the log is fully backfilled and may be overwritten
		// from frame 1 when no other reader holds a mark; the pin is then
		// re-selected as a real read mark.
		if err := w.walRestartLogHeld(); err != nil {
			return err
		}
	}
	if w.nFrame == 0 {
		return w.syncChainFromWalFileLocked()
	}
	return nil
}

// commitLocked appends the transaction's frames and publishes the header.
// Caller holds the WRITER shm lock and the wal-index writer section.
func (w *walWriter) commitLocked(pages []*Page, appended *int) error {
	if err := w.commitPrepareLocked(); err != nil {
		return err
	}
	dbSize := w.p.numPages
	for i, pg := range pages {
		commit := i == len(pages)-1
		if err := w.appendFrame(pg, commit, dbSize); err != nil {
			return err
		}
		// walIndexAppend: record pgno→frame in the shared hash tables
		// (the mxFrame cleanup bound is the pre-commit value).
		if err := w.wi.appendLocked(uint32(w.nFrame), pg.PageNum, w.hdr.MxFrame); err != nil {
			return err
		}
	}
	// Publish the new header (wal.c walFrames isCommit branch).
	w.hdr.MxFrame = uint32(w.nFrame)
	w.hdr.NPage = dbSize
	w.hdr.IChange++
	w.hdr.AFrameCksum = [2]uint32{w.cksum1, w.cksum2}
	setPageSizeForHdr(&w.hdr, w.pageSize)
	w.wi.writeHdrLocked(&w.hdr)
	*appended = len(pages)
	return nil
}

// walRestartHdrLocked ports walRestartHdr (wal.c L2146) + the WAL file header
// rewrite of walFrames' iFrame==0 branch: reset the log so new frames start
// at frame 1 (salt1 incremented, salt2 randomized, checkpoint sequence
// bumped). When truncate is set the -wal file is truncated to its header
// (the frigolite RESTART/TRUNCATE checkpoint contract); the writer-side
// walRestartLog path keeps the file and overwrites frames in place (C).
// Caller holds the wal-index writer section.
func (w *walWriter) walRestartHdrLocked(truncate bool) error {
	w.nCkpt++
	w.hdr.MxFrame = 0
	var s [4]byte
	if _, err := rand.Read(s[:]); err != nil {
		s[0] = 0x5a
	}
	w.hdr.ASalt = [2]uint32{w.hdr.ASalt[0] + 1, binary.BigEndian.Uint32(s[:])}
	w.wi.writeHdrLocked(&w.hdr)
	w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) {
		ci.NBackfill = 0
		ci.NBackfillAttempted = 0
		ci.AReadMark[1] = 0
		for i := 2; i < WalNReader; i++ {
			ci.AReadMark[i] = ReadmarkNotUsed
		}
	})
	if truncate {
		if err := w.file.Truncate(0); err != nil {
			return fmt.Errorf("pager: truncate wal: %w", err)
		}
	}
	w.nFrame = 0
	return w.writeWalFileHeaderLocked()
}

// WalCheckpointMode mirrors the SQLITE_CHECKPOINT_* constants from
// sqlite/src/wal.c (sqlite3.h) and selects what a WAL checkpoint does
// beyond reporting its size. PASSIVE backfills frames into the main file
// but keeps the -wal so concurrent readers can still see them; FULL is
// PASSIVE plus the guarantee that it completed; RESTART/TRUNCATE
// additionally reset the log (salt evolution, mxFrame=0) and truncate the
// -wal to its 32-byte header.
type WalCheckpointMode int

const (
	WalCkptPassive  WalCheckpointMode = 0
	WalCkptFull     WalCheckpointMode = 1
	WalCkptRestart  WalCheckpointMode = 2
	WalCkptTruncate WalCheckpointMode = 3
)

// checkpoint ports sqlite3WalCheckpoint + walCheckpoint (wal.c L2193) for the
// single-process registry model. Lock acquisition follows C: non-PASSIVE
// modes take the WRITER lock first (busy handler honored), then every mode
// takes the exclusive CKPT lock (PASSIVE never invokes the busy handler —
// EVIDENCE-OF R-62920-47450). PASS1 computes mxSafeFrame stepping over active
// readers' read marks, backfills (nBackfill, mxSafeFrame] under exclusive
// READ_LOCK(0) and updates nBackfill; PASS2 (eMode != PASSIVE) reports busy
// when the log is not fully backfilled, and RESTART/TRUNCATE reset the log
// under exclusive READ_LOCK(1..4) — TRUNCATE truncates the -wal to ZERO
// bytes (R-44699-57140). The result is the PRAGMA wal_checkpoint triple
// (busy, nLog, nCkpt) — walprotocol-2.1 expects {0 5 5} for PASSIVE.
func (w *walWriter) checkpoint(mode WalCheckpointMode) (busy, nLog, nCkpt int, err error) {
	if mode != WalCkptPassive && !w.writeLock {
		if berr := w.walBusyLockExclusive(walLockWrite, 1); berr != nil {
			return 1, 0, 0, nil
		}
		w.writeLock = true
		defer func() {
			if w.writeLock {
				w.walUnlockExclusive(walLockWrite, 1)
				w.writeLock = false
			}
		}()
	}
	if !w.ckptLock {
		if berr := w.walBusyLockCkpt(mode != WalCkptPassive); berr != nil {
			return 1, 0, 0, nil
		}
		w.ckptLock = true
		defer func() {
			if w.ckptLock {
				w.walUnlockExclusive(walLockCkpt, 1)
				w.ckptLock = false
			}
		}()
	}
	err = w.wi.WriterSection(func() error {
		if err := w.writerRefreshLocked(); err != nil {
			return err
		}
		busy, err = w.checkpointPasses(mode)
		// Result triple from the final state (sqlite3WalCheckpoint tail):
		// nLog = mxFrame, nCkpt = nBackfill — both 0 after a RESTART reset.
		nLog = int(w.hdr.MxFrame)
		nCkpt = int(w.wi.ckptInfoLocked().NBackfill)
		return err
	})
	return busy, nLog, nCkpt, err
}

// checkpointPasses runs walCheckpoint's two passes. PASS1 computes
// mxSafeFrame stepping over active readers' read marks and backfills
// (nBackfill, mxSafeFrame] under exclusive READ_LOCK(0); PASS2 (eMode !=
// PASSIVE) reports busy when the log is not fully backfilled, and
// RESTART/TRUNCATE reset the log under exclusive READ_LOCK(1..4). Caller
// holds the WRITER (non-PASSIVE) and CKPT locks and the wal-index writer
// section.
func (w *walWriter) checkpointPasses(mode WalCheckpointMode) (busy int, err error) {
	// PASS1: compute mxSafeFrame, stepping over active readers' marks, and
	// backfill (nBackfill, mxSafeFrame] under exclusive READ_LOCK(0).
	if _, err := w.ckptPass1ReaderMarks(w.hdr.MxFrame); err != nil {
		return busy, err
	}
	return w.ckptPass2(mode)
}

// ckptPass1ReaderMarks ports walCheckpoint's PASS1 reader-mark loop: try an
// exclusive READ_LOCK(i) under every mark below mxSafeFrame, re-initializing
// the mark when granted; a BUSY reader (a mark left in place) stops the
// backfill short of it and disables the busy handler (wal.c xBusy=0).
// Caller holds the wal-index writer section.
func (w *walWriter) ckptPass1ReaderMarks(mxFrame uint32) (uint32, error) {
	mxSafeFrame := mxFrame
	info := w.wi.ckptInfoLocked()
	for i := 1; i < WalNReader; i++ {
		y := info.AReadMark[i]
		if mxSafeFrame <= y || y == ReadmarkNotUsed {
			continue
		}
		if w.wi.shmTryLockHeld(walReadLockIdx(i), 1, true) == nil {
			iMark := uint32(ReadmarkNotUsed)
			if i == 1 {
				iMark = mxSafeFrame
			}
			w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.AReadMark[i] = iMark })
			w.wi.shmUnlockHeld(walReadLockIdx(i), 1, true)
		} else {
			// BUSY reader: stop the backfill short of its mark
			// (and stop invoking the busy handler, wal.c xBusy=0).
			mxSafeFrame = y
		}
	}
	return w.ckptBackfillPass(mxFrame, mxSafeFrame)
}

// ckptBackfillPass backfills (nBackfill, mxSafeFrame] under exclusive
// READ_LOCK(0) (wal.c walBusyLock) and stores nBackfillAttempted/nBackfill.
// On a contended lock the backfill window collapses to nBackfill (the
// caller's fallback); an I/O error aborts the checkpoint.
func (w *walWriter) ckptBackfillPass(mxFrame, mxSafeFrame uint32) (uint32, error) {
	nBackfill0 := w.wi.ckptInfoLocked().NBackfill
	if nBackfill0 >= mxSafeFrame {
		return mxSafeFrame, nil
	}
	// Backfill under exclusive READ_LOCK(0) (wal.c walBusyLock).
	if w.wi.shmTryLockHeld(walReadLockIdx(0), 1, true) != nil {
		return nBackfill0, nil
	}
	w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.NBackfillAttempted = mxSafeFrame })
	berr := w.backfillLocked(nBackfill0, mxSafeFrame, mxFrame)
	if berr == nil {
		w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.NBackfill = mxSafeFrame })
	}
	w.wi.shmUnlockHeld(walReadLockIdx(0), 1, true)
	if berr != nil {
		return mxSafeFrame, berr
	}
	return mxSafeFrame, nil
}

// ckptPass2 ports walCheckpoint's PASS2 (eMode != PASSIVE): the log must be
// fully backfilled, else report busy; RESTART/TRUNCATE reset the log under
// exclusive READ_LOCK(1..4) — TRUNCATE truncates the -wal to ZERO bytes
// (R-44699-57140). Caller holds the wal-index writer section.
func (w *walWriter) ckptPass2(mode WalCheckpointMode) (int, error) {
	busy := 0
	if mode == WalCkptPassive {
		return busy, nil
	}
	if w.wi.ckptInfoLocked().NBackfill < w.hdr.MxFrame {
		return 1, nil
	}
	if mode < WalCkptRestart {
		return busy, nil
	}
	if w.wi.shmTryLockHeld(walReadLockIdx(1), WalNReader-1, true) != nil {
		return 1, nil
	}
	rerr := w.walRestartHdrLocked(true)
	w.wi.shmUnlockHeld(walReadLockIdx(1), WalNReader-1, true)
	if rerr != nil {
		return busy, rerr
	}
	if mode == WalCkptTruncate {
		// R-44699-57140: TRUNCATE truncates the log file to ZERO bytes
		// prior to a successful return; the next writer rewrites the
		// 32-byte header before frame 1 (commitLocked's nFrame==0 branch).
		if terr := w.file.Truncate(0); terr != nil {
			return busy, fmt.Errorf("pager: truncate wal: %w", terr)
		}
	}
	return busy, nil
}

// walBusyLockCkpt takes the exclusive CKPT lock. It honors the busy timeout
// only for the non-PASSIVE modes (the PASSIVE checkpoint's busy handler is
// never invoked — wal.c EVIDENCE-OF R-62920-47450).
func (w *walWriter) walBusyLockCkpt(retry bool) error {
	if !retry {
		return w.walLockExclusive(walLockCkpt, 1)
	}
	return w.walBusyLockExclusive(walLockCkpt, 1)
}

// backfillLocked copies frames (nFrom, nTo] of the -wal into the main
// database file (walCheckpoint's iterator loop), updating the pager's page
// cache, header and page count. When nTo == mxFrame the main file is
// truncated to nPage*pageSize (wal.c's szDb truncate). Caller holds p.mu
// (via Pager.CheckpointMode) and the wal-index writer section.
func (w *walWriter) backfillLocked(nFrom, nTo, mxFrame uint32) error {
	p := w.p
	if nTo <= nFrom {
		return nil
	}
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < WalHdrSize {
		return nil
	}
	buf := make([]byte, info.Size())
	if _, err := w.file.ReadAt(buf, 0); err != nil {
		return err
	}
	h, derr := DecodeWalHeader(buf)
	if derr != nil || !h.HeaderCksumOK {
		return nil
	}
	frames, derr := DecodeWalFrames(buf, h)
	if derr != nil {
		return nil
	}
	nPage := w.hdr.NPage
	for _, fr := range frames {
		if fr.Number > int(nTo) || fr.Number <= int(nFrom) {
			continue
		}
		if !fr.Valid || fr.Number > int(mxFrame) {
			break
		}
		pgno := fr.PageNumber
		pg := &Page{PageNum: pgno, Data: append([]byte(nil), fr.PageData...)}
		p.pages[pgno] = pg
		if pgno > nPage {
			// A frame beyond the recorded nPage: the commit record's size
			// governs, but never write past the pages we have.
			nPage = pgno
		}
		off := int64(pgno-1) * int64(p.pageSize)
		fileEnd := int64(pgno) * int64(p.pageSize)
		if p.fileSize < fileEnd {
			if err := p.file.Truncate(fileEnd); err != nil {
				return err
			}
			p.fileSize = fileEnd
		}
		if _, err := p.file.WriteAt(pg.Data, off); err != nil {
			return fmt.Errorf("pager: checkpoint write page %d: %w", pgno, err)
		}
	}
	if nPage > p.numPages {
		p.numPages = nPage
	}
	// Recover the database header from page 1 if present.
	if pg, ok := p.pages[1]; ok {
		if p.header == nil {
			p.header = make([]byte, HeaderSize)
		}
		copy(p.header, pg.Data[:HeaderSize])
	}
	// When the whole log was covered, truncate the main file to the committed
	// page count (wal.c: szDb truncate under mxSafeFrame == mxFrame).
	if nTo == mxFrame {
		szDb := int64(w.hdr.NPage) * int64(p.pageSize)
		if szDb > 0 && p.fileSize != szDb {
			if err := p.file.Truncate(szDb); err != nil {
				return err
			}
			p.fileSize = szDb
		}
	}
	p.dirty = make(map[uint32]bool)
	p.refreshKnownFileStamp()
	return nil
}

// FileSize returns the current "-wal" file size in bytes (used by tests to
// simulate a crash at a frame boundary).
func (w *walWriter) FileSize() int64 {
	if w.file == nil {
		return 0
	}
	if info, err := w.file.Stat(); err == nil {
		return info.Size()
	}
	return 0
}

// Close closes the "-wal" file and releases the shared wal-index reference
// (the last detach closes the shm fd and drops the registry entry). Any shm
// locks this connection still holds are released first (C's connection close
// drops its locks — close(2) semantics on the POSIX locks).
func (w *walWriter) Close() error {
	if w.file != nil {
		if w.writeLock {
			w.walUnlockExclusive(walLockWrite, 1)
			w.writeLock = false
		}
		if w.ckptLock {
			w.walUnlockExclusive(walLockCkpt, 1)
			w.ckptLock = false
		}
		// A connection closed mid-read-transaction drops its shared
		// read-mark lock (close(2) semantics on the POSIX locks — C's
		// pWal->readLock dies with the handle).
		if w.readLock >= 0 {
			w.walEndReadTxn()
		}
		err := w.file.Close()
		w.file = nil
		if w.wi != nil {
			w.wi.release()
			w.wi = nil
		}
		return err
	}
	return nil
}
