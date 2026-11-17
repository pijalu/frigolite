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
	"sort"
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
	writeLock    bool
	ckptLock     bool
	spillStarted bool
	spilled      map[uint32][]byte
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
	// snapshot is the armed sqlite3_snapshot (C's pWal->pSnapshot,
	// wal.c L548): when non-nil, the NEXT read transaction pins mxFrame at
	// the snapshot's (capped, wal.c L3158) and verifies the salt /
	// nBackfillAttempted under the shared CKPT lock (wal.c L3401-3441) —
	// SQLITE_ERROR_SNAPSHOT ("snapshot is out of date") when the WAL was
	// wrapped or checkpointed past the snapshot. Arming is scoped to one
	// read-transaction open (see walsnapshot.go).
	snapshot *Snapshot
	// bGetSnapshot is C's pWal->bGetSnapshot (wal.c L549), set while a read
	// transaction is opened on behalf of sqlite3_snapshot_get: the
	// fully-backfilled READ_LOCK(0) shortcut is skipped (unless mxFrame==0)
	// so the snapshot cannot be destroyed by a later WAL wrap while the
	// caller's transaction stays open (wal.c L3116).
	bGetSnapshot bool
	// exclusiveMode is locking_mode=EXCLUSIVE in effect: every shm lock call
	// becomes a no-op (wal.c walLockShared/walLockExclusive). The flag FLIPS
	// at the first write transaction under the pragma (wal.c
	// sqlite3WalExclusiveMode op=1 via pager.c:5937) and back at the first
	// transaction ending after locking_mode=normal (op=0 via pager.c:2159).
	exclusiveMode bool
	// exclusiveRequested is the PRAGMA locking_mode=EXCLUSIVE request
	// (pPager->exclusiveMode): no shm state changes at pragma time.
	exclusiveRequested bool
	// heapMemory is C's WAL_HEAPMEMORY_MODE: the WAL was opened while the
	// pager's locking mode was EXCLUSIVE, so the wal-index lives in heap
	// memory (wal.c sqlite3WalOpen's bNoShm ← pager.c:7596
	// pPager->exclusiveMode). sqlite3WalHeapMemory reports it, and
	// sqlite3PagerLockingMode REFUSES a locking-mode change while it holds
	// (pager1-7.1.6's `PRAGMA locking_mode = NORMAL` must return
	// "exclusive"). The flag dies with the WAL (leaving WAL mode tears the
	// heap wal-index down with it).
	heapMemory bool
	// busyTimeout is sqlite3_busy_timeout: contended shm lock acquisitions
	// retry until the deadline, then report "database is locked".
	busyTimeout time.Duration
	// busyHandler is the connection's sqlite3_busy_handler callback
	// (main.c sqlite3_busy_handler): invoked between contended checkpoint
	// lock attempts, it receives the number of previous invocations for the
	// current locked event and returns true to retry the attempt. It
	// replaces the busy timeout for the checkpoint locks (registering a
	// handler zeroes busy_timeout — main.c:1788).
	busyHandler func(count int) bool
}

// walBusySession is one busy-handler counting session for a checkpoint
// statement (main.c:2576 resets nBusy at each statement execution, so one
// checkpoint — writer lock, read marks, restart blocking — is ONE session:
// the handler receives the current count starting at 0 and the count
// advances only on a retry answer; a give-up disables the handler for the
// rest of the statement, like wal.c's local xBusy=0).
type walBusySession struct {
	handler func(count int) bool
	count   int
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
	// The WAL open maps the -shm region (C's xShmMap): a fixture callback
	// returning SQLITE_READONLY fails the access with the readonly-db
	// message (walvfs-5.5's db2 SELECT).
	if err := p.vfsCall("xShmMap", walPath+"-shm"); err != nil {
		return nil, translateVFSHookError(err)
	}
	// C's TEST io-error injector covers xOpen too — the wal file's open
	// counts against the pending counter.
	if injectIOErr() {
		return nil, ErrIOErr
	}
	f, err := os.OpenFile(walPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("pager: open wal %s: %w", walPath, err)
	}
	wi, err := acquireWALIndex(dbPath)
	if err != nil {
		f.Close()
		return nil, err
	}
	w := &walWriter{p: p, path: walPath, file: f, pageSize: pageSize, wi: wi, readLock: -1,
		heapMemory: p.walExclusiveLatched}
	if err := w.walInitHeader(); err != nil {
		wi.release()
		f.Close()
		return nil, err
	}
	return w, nil
}

// walInitHeader loads the shared wal-index header (recovering from the -wal
// through the WRITER-lock dance when it does not parse) and adopts the WAL
// file's header state into the writer.
//
// The two C paths split exactly:
//   - The REAL-SHM path trusts a parsed shared header (isInit + the header's
//     own checksums) with NO file re-check — walIndexTryHdr (wal.c:2580-2625).
//     wal6-4.4.2's zeroed-WAL scenario: db2's reopen adopts db's live index,
//     t1's checkpointed page is served from the main db (minFrame =
//     nBackfill+1) and t2's un-checkpointed frame reads back the zeroed
//     image the btree rejects ("database disk image is malformed"). A gate
//     here (salts/capacity vs the -wal file) voided the log on every reopen
//     beside a zeroed/garbage -wal header and never matched C.
//   - The HEAP-MEMORY path re-validates the header against the "-wal" file
//     (wal.c begin_unreliable_shm: "some writer has come in and changed the
//     WAL file out from under us, then disconnected" — the salt check at
//     wal.c:2849-2861 returns WAL_RETRY, rebuilding the heap index from the
//     file). The heap index outlives file resets it did not author —
//     walfault-15's faultsim trials: a faulted exclusive transaction leaves
//     the heap header at mxFrame 15482 beside a reset 32-byte log, and the
//     next restore+reopen must rebuild from the file, not read frame 15475
//     of a 32-byte log.
func (w *walWriter) walInitHeader() error {
	if _, err := w.walIndexReadHdr(); err != nil {
		return err
	}
	if w.heapMemory {
		stale := false
		_ = w.wi.WriterSection(func() error {
			stale = w.hdr.MxFrame > 0 && !w.walFileHeaderSaltsMatchLocked()
			return nil
		})
		if stale {
			if err := w.walRecoverWithWriterLock(); err != nil {
				return err
			}
		}
	}
	return w.wi.WriterSection(w.initHeaderAdoptLocked)
}

// walFileHeaderSaltsMatchLocked reports whether the -wal file carries a valid
// header whose salts match the shared wal-index header AND holds every frame
// the shared header maps (wal.c's walIndexRecover is the only writer of
// MxFrame and it never maps frames the file lacks — a heap header claiming
// frames beyond the file's capacity belongs to a reset/restarted log). The
// -wal file is the authority: rebuild. Caller holds the wal-index writer
// section. Consulted ONLY on the heap-memory path (see walInitHeader).
func (w *walWriter) walFileHeaderSaltsMatchLocked() bool {
	buf := make([]byte, WalHdrSize)
	n, err := w.file.ReadAt(buf, 0)
	if err != nil || n < WalHdrSize {
		return false
	}
	h, derr := DecodeWalHeader(buf)
	if derr == nil && h.HeaderCksumOK &&
		h.Salt1 == w.hdr.ASalt[0] && h.Salt2 == w.hdr.ASalt[1] {
		if info, serr := w.file.Stat(); serr == nil {
			frameSize := int64(w.pageSize) + WalFrameHdrSize
			if (info.Size()-WalHdrSize)/frameSize < int64(w.hdr.MxFrame) {
				return false
			}
		}
		return true
	}
	return false
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
	// wal.c:4098 — sync the just-written WAL header unless IOCAP_SEQUENTIAL
	// or synchronous=OFF (CKPT_SYNC_FLAGS run at NORMAL and FULL).
	if w.p.walHeaderSyncAllowed() {
		w.p.vfsSync(w.path, walCkptSyncBits(w.p.walSyncFlags))
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

// spill writes dirty pager pages as non-commit WAL frames. The published index header is untouched,
// so readers cannot observe these frames until the transaction commits.
func (w *walWriter) spill(pages []*Page) error {
	if len(pages) == 0 {
		return nil
	}
	if !w.writeLock {
		return fmt.Errorf("pager: WAL spill without writer lock")
	}
	if w.spilled == nil {
		w.spilled = make(map[uint32][]byte)
	}
	return w.wi.WriterSection(func() error {
		if !w.spillStarted && w.readLock == 0 {
			if err := w.walRestartLogHeld(); err != nil {
				return err
			}
		}
		for _, pg := range pages {
			if prior, ok := w.spilled[pg.PageNum]; ok && string(prior) == string(pg.Data) {
				continue
			}
			if err := w.appendFrame(pg, false, 0); err != nil {
				return err
			}
			if err := w.wi.appendLocked(uint32(w.nFrame), pg.PageNum, w.hdr.MxFrame); err != nil {
				return err
			}
			w.spilled[pg.PageNum] = append(w.spilled[pg.PageNum][:0], pg.Data...)
		}
		w.spillStarted = true
		return nil
	})
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
	// wal.c:4187 — the transaction-commit sync runs only when the commit
	// sync bits are non-zero (synchronous=FULL; NORMAL's walSyncFlags carry
	// only the checkpoint bits).
	p.vfsSync(w.path, walCommitSyncBits(p.walSyncFlags))
	if p.walHook != nil {
		p.walHook(appended, 0)
	}
	return appended, nil
}

// pagerWriteVersionNumber is the SQLite version number stamped into page-1
// image bytes 96-99 by the WAL commit's change-counter rider (C's
// SQLITE_VERSION_NUMBER). The engine's open-time header materialization
// (internal/exec pragma_header.go) carries the same value.
const pagerWriteVersionNumber uint32 = 3045000

// pagerWriteChangeCounterLocked ports pager_write_changecounter
// (pager.c:3082-3097): bump the change counter read from the connection's
// last-seen disk stamp (dbFileVers — knownFileVers' first word, the page-1
// header bytes 24-27) and write it into the page-1 IMAGE at bytes 24, mirror
// it at 92 (version-valid-for) and stamp the version number at 96. No
// pager-stamp mutation: C's dbFileVers stays stale across consecutive
// page-1 WAL commits. Caller holds p.mu (and the wal-index writer section).
func (p *Pager) pagerWriteChangeCounterLocked(pg1 *Page) {
	if pg1 == nil || len(pg1.Data) < 100 {
		return
	}
	counter := binary.BigEndian.Uint32(p.knownFileVers[:4]) + 1
	binary.BigEndian.PutUint32(pg1.Data[24:28], counter)
	binary.BigEndian.PutUint32(pg1.Data[92:96], counter)
	binary.BigEndian.PutUint32(pg1.Data[96:100], pagerWriteVersionNumber)
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
	if os.Getenv("FRIGOLITE_WALTRACE") != "" {
		info := w.wi.ckptInfoLocked()
		fmt.Fprintf(os.Stderr, "TRACE prepare: readLock=%d spill=%v nbackfill=%d mxframe=%d\n", w.readLock, w.spillStarted, info.NBackfill, w.hdr.MxFrame)
	}
	if w.readLock == 0 && !w.spillStarted {
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
	// pager.c:3216 (pagerWalFrames) → pager_write_changecounter
	// (pager.c:3082-3097): when page 1 leads the commit list, bump the
	// change counter in the page-1 IMAGE (bytes 24), mirror it at 92
	// (version-valid-for) and stamp the version number at 96. C's dbFileVers
	// is the last-seen DISK copy — it updates only at page-1 disk fetches —
	// so consecutive page-1 WAL commits stamp the SAME counter (the
	// walvfs-1.2 oracle dump: both page-1 frames carry counter 3).
	if len(pages) > 0 && pages[0].PageNum == 1 {
		w.p.pagerWriteChangeCounterLocked(pages[0])
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
	// wal.c:4172-4190 — pad the transaction out to the next sector boundary
	// when the device lacks POWERSAFE_OVERWRITE and the commit sync bits are
	// non-zero (synchronous=FULL): the last frame is repeated (with its
	// commit mark) until the file offset crosses the sync point. The
	// padding frames join the wal-index like real frames — mxFrame and
	// aFrameCksum include them; the commit's single xSync still fires once
	// (C's walWriteToLog syncs at the sync point).
	extra := 0
	if walCommitSyncBits(w.p.walSyncFlags) != 0 &&
		w.p.deviceCharacteristics&IOCAPPowerSafeOverwrite == 0 {
		const sectorSize = 4096 // SQLITE_DEFAULT_SECTOR_SIZE (os.h:53)
		frameSize := int64(w.pageSize) + WalFrameHdrSize
		off := WalHdrSize + int64(w.nFrame)*frameSize
		syncPoint := (off + sectorSize - 1) / sectorSize * sectorSize
		pLast := pages[len(pages)-1]
		for off < syncPoint {
			if err := w.appendFrame(pLast, true, dbSize); err != nil {
				return err
			}
			if err := w.wi.appendLocked(uint32(w.nFrame), pLast.PageNum, w.hdr.MxFrame); err != nil {
				return err
			}
			off += frameSize
			extra++
		}
	}
	// Publish the new header (wal.c walFrames isCommit branch).
	w.hdr.MxFrame = uint32(w.nFrame)
	w.hdr.NPage = dbSize
	w.hdr.IChange++
	w.hdr.AFrameCksum = [2]uint32{w.cksum1, w.cksum2}
	setPageSizeForHdr(&w.hdr, w.pageSize)
	w.wi.writeHdrLocked(&w.hdr)
	*appended = len(pages) + extra
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
	// WalCkptNoop is SQLITE_CHECKPOINT_NOOP (sqlite.h.in:10060): do no work
	// at all — report {0, mxFrame, nBackfill} from the refreshed wal-index
	// header without taking the CKPT lock or running the passes. C's
	// ordering NOOP(-1) < PASSIVE(0) is load-bearing (eMode<=PASSIVE passes
	// xBusyHandler=0, pager.c sqlite3PagerCheckpoint).
	WalCkptNoop     WalCheckpointMode = -1
	WalCkptPassive  WalCheckpointMode = 0
	WalCkptFull     WalCheckpointMode = 1
	WalCkptRestart  WalCheckpointMode = 2
	WalCkptTruncate WalCheckpointMode = 3
)

// checkpoint ports sqlite3WalCheckpoint + walCheckpoint (wal.c L4290, L2193)
// for the single-process registry model. Lock acquisition follows C: every
// mode takes the exclusive CKPT lock first — directly, the busy handler is
// never invoked for it (R-53820-33897, and a veto there keeps the pragma's
// {1 -1 -1} initializers: wal.c's outer if(rc==SQLITE_OK) guard skips the
// triple assignment). Non-PASSIVE modes then take the WRITER lock via
// walBusyLock (R-60642-04082: the busy handler is invoked between attempts
// until it returns 0 or the lock is obtained). SQLITE_BUSY there DEMOTES
// the checkpoint to PASSIVE (wal.c:4339 eMode2=PASSIVE, xBusy2=0): the
// backfill still runs below the readers' marks and the whole call reports
// busy (eMode != eMode2) with the demoted run's triple. PASS1 computes
// mxSafeFrame stepping over active readers' read marks, backfills
// (nBackfill, mxSafeFrame] under exclusive READ_LOCK(0) and updates
// nBackfill; PASS2 (eMode != PASSIVE) reports busy when the log is not
// fully backfilled, and RESTART/TRUNCATE reset the log under exclusive
// READ_LOCK(1..4) — TRUNCATE truncates the -wal to ZERO bytes
// (R-44699-57140). The result is the PRAGMA wal_checkpoint triple
// (busy, nLog, nCkpt) — walprotocol-2.1 expects {0 5 5} for PASSIVE.
func (w *walWriter) checkpoint(mode WalCheckpointMode) (busy, nLog, nCkpt int, err error) {
	// One busy-handler count session per checkpoint statement. PASSIVE
	// checkpoints carry NO busy handler at all: the pager passes xBusy=0
	// for eMode<=PASSIVE (pager.c sqlite3PagerCheckpoint) and the handler
	// is never invoked in passive mode (wal.c EVIDENCE-OF R-62920-47450 —
	// e_walckpt-1.passive.1.6's busy_handler_counter stays 0).
	session := &walBusySession{handler: w.busyHandler}
	if mode == WalCkptPassive {
		session.handler = nil
	}
	// wal.c:4318 — NOOP skips the CKPT lock entirely (and with it the
	// WRITER lock and the passes); only the header refresh and the report
	// run.
	mode2 := mode
	if mode != WalCkptNoop {
		if !w.ckptLock {
			// wal.c:4319 — the CKPT lock is taken directly.
			if berr := w.walLockExclusive(walLockCkpt, 1); berr != nil {
				if errors.Is(berr, errWalBusy) {
					return 1, -1, -1, nil
				}
				return 0, 0, 0, berr
			}
			w.ckptLock = true
			defer func() {
				if w.ckptLock {
					w.walUnlockExclusive(walLockCkpt, 1)
					w.ckptLock = false
				}
			}()
		}
		// wal.c:4334-4343 — the WRITER lock for non-PASSIVE modes, the busy
		// handler invoked between contended attempts. SQLITE_BUSY demotes the
		// whole run to PASSIVE with the handler disabled.
		if mode != WalCkptPassive && !w.writeLock {
			if berr := w.walBusyLockHandler(walLockWrite, 1, session); berr != nil {
				if errors.Is(berr, errWalBusy) {
					mode2 = WalCkptPassive
					session.handler = nil
				} else {
					return 0, 0, 0, berr
				}
			} else {
				w.writeLock = true
				defer func() {
					if w.writeLock {
						w.walUnlockExclusive(walLockWrite, 1)
						w.writeLock = false
					}
				}()
			}
		}
	}
	// The wal-index header refresh and every pass state mutation run in
	// their own WriterSections (the checkpoint's lock attempts may invoke
	// the busy handler between them — its script drives other connections'
	// transactions, which need the mutex too; C's walBusyLock likewise
	// never runs inside a wal-index critical section).
	if err := w.wi.WriterSection(func() error {
		return w.writerRefreshLocked()
	}); err != nil {
		return busy, nLog, nCkpt, err
	}
	if mode != WalCkptNoop {
		busy, err = w.checkpointPasses(mode2, session)
	}
	err2 := w.wi.WriterSection(func() error {
		// Result triple from the final state (sqlite3WalCheckpoint tail):
		// nLog = mxFrame, nCkpt = nBackfill — both 0 after a RESTART reset.
		nLog = int(w.hdr.MxFrame)
		nCkpt = int(w.wi.ckptInfoLocked().NBackfill)
		return nil
	})
	if err == nil {
		err = err2
	}
	if mode2 != mode {
		// wal.c:4407 — the demoted run still reports SQLITE_BUSY.
		busy = 1
	}
	return busy, nLog, nCkpt, err
}

// walBusyLockHandler ports walBusyLock (wal.c L2101) with the checkpoint's
// busy-handler session: try the exclusive lock; while busy and the handler
// says retry, invoke it (main.c sqlite3InvokeBusyHandler counting: the
// handler sees the current count, the count advances only on retry) and
// retry. With no live handler this falls back to the busy-timeout loop
// (a single attempt when no timeout is configured), matching a C build
// whose only handler is the sqlite3_busy_timeout callback.
func (w *walWriter) walBusyLockHandler(idx, n int, session *walBusySession) error {
	if session == nil || session.handler == nil {
		return w.walBusyLockExclusive(idx, n)
	}
	for {
		err := w.walLockExclusive(idx, n)
		if err == nil || !errors.Is(err, errWalBusy) {
			return err
		}
		if !session.handler(session.count) {
			return err
		}
		session.count++
	}
}

// checkpointPasses runs walCheckpoint's two passes. PASS1 computes
// mxSafeFrame stepping over active readers' read marks and backfills
// (nBackfill, mxSafeFrame] under exclusive READ_LOCK(0); PASS2 (eMode !=
// PASSIVE) reports busy when the log is not fully backfilled, and
// RESTART/TRUNCATE reset the log under exclusive READ_LOCK(1..4). Caller
// holds the WRITER (non-PASSIVE) and CKPT locks. Lock attempts that may
// invoke the busy handler use the non-held shm primitives (the hook and
// the handler run OUTSIDE the wal-index mutex — the handler's script
// drives other connections' transactions which need the mutex themselves;
// C's walBusyLock likewise runs outside any wal-index critical section),
// with mini WriterSections around the state mutations.
func (w *walWriter) checkpointPasses(mode WalCheckpointMode, session *walBusySession) (busy int, err error) {
	// PASS1: compute mxSafeFrame, stepping over active readers' marks, and
	// backfill (nBackfill, mxSafeFrame] under exclusive READ_LOCK(0).
	mxSafeFrame, err := w.ckptPass1ReaderMarks(w.hdr.MxFrame, session)
	if err != nil {
		return busy, err
	}
	if err := w.ckptBackfillPass(w.hdr.MxFrame, mxSafeFrame, session); err != nil {
		return busy, err
	}
	return w.ckptPass2(mode, session)
}

// ckptPass1ReaderMarks ports walCheckpoint's PASS1 reader-mark loop: try an
// exclusive READ_LOCK(i) under every mark below mxSafeFrame, re-initializing
// the mark when granted; a BUSY reader (a mark left in place) stops the
// backfill short of it and disables the busy handler (wal.c xBusy=0).
func (w *walWriter) ckptPass1ReaderMarks(mxFrame uint32, session *walBusySession) (uint32, error) {
	mxSafeFrame := mxFrame
	var marks [WalNReader]uint32
	err := w.wi.WriterSection(func() error {
		info := w.wi.ckptInfoLocked()
		copy(marks[:], info.AReadMark[:])
		return nil
	})
	if err != nil {
		return mxSafeFrame, err
	}
	for i := 1; i < WalNReader; i++ {
		y := marks[i]
		if mxSafeFrame <= y || y == ReadmarkNotUsed {
			continue
		}
		// wal.c:2233 — walBusyLock(WAL_READ_LOCK(i), 1): the busy handler
		// is invoked between contended attempts.
		if err := w.walBusyShmLock(walReadLockIdx(i), 1, session); err == nil {
			iMark := uint32(ReadmarkNotUsed)
			if i == 1 {
				iMark = mxSafeFrame
			}
			w.wi.WriterSection(func() error {
				w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.AReadMark[i] = iMark })
				return nil
			})
			// The unlock's xShmLock hook fires OUTSIDE the wal-index mutex
			// (shmUnlock takes it for the slot state): the fixture script
			// may re-enter the engine (wal3-6.2.x drives a connection from
			// the xShmLock callback).
			w.wi.shmUnlock(walReadLockIdx(i), 1, true)
		} else if errors.Is(err, errWalBusy) {
			// BUSY reader: stop the backfill short of its mark
			// (and stop invoking the busy handler, wal.c xBusy=0).
			mxSafeFrame = y
			session = walBusySessionNone(session)
		} else {
			return mxSafeFrame, err
		}
	}
	return mxSafeFrame, nil
}

// walBusyShmLock ports walBusyLock for a shm slot range: try the exclusive
// lock (the xShmLock hook fires before the wal-index mutex is taken);
// while busy and the session's handler says retry, invoke it and retry.
func (w *walWriter) walBusyShmLock(idx, n int, session *walBusySession) error {
	if session == nil || session.handler == nil {
		return w.wi.shmTryLock(idx, n, true)
	}
	for {
		err := w.wi.shmTryLock(idx, n, true)
		if err == nil || !errors.Is(err, errWalBusy) {
			return err
		}
		if !session.handler(session.count) {
			return err
		}
		session.count++
	}
}

// walBusySessionNone disables the session's handler (wal.c's local
// xBusy=0 after a contended read mark).
func walBusySessionNone(session *walBusySession) *walBusySession {
	if session != nil {
		session.handler = nil
	}
	return session
}

// ckptBackfillPass backfills (nBackfill, mxSafeFrame] under exclusive
// READ_LOCK(0) (wal.c walBusyLock) and stores nBackfillAttempted/nBackfill.
// On a contended lock the backfill window collapses to nBackfill (the
// caller's fallback); an I/O error aborts the checkpoint.
func (w *walWriter) ckptBackfillPass(mxFrame, mxSafeFrame uint32, session *walBusySession) error {
	var nBackfill0 uint32
	err := w.wi.WriterSection(func() error {
		nBackfill0 = w.wi.ckptInfoLocked().NBackfill
		return nil
	})
	if err != nil {
		return err
	}
	if nBackfill0 >= mxSafeFrame {
		return nil
	}
	// Backfill under exclusive READ_LOCK(0) (wal.c walBusyLock).
	if err := w.walBusyShmLock(walReadLockIdx(0), 1, session); err != nil {
		if errors.Is(err, errWalBusy) {
			return nil
		}
		return err
	}
	// wal.c:2260 — sync the WAL before copying its frames into the db
	// (CKPT_SYNC_FLAGS run at synchronous=NORMAL and FULL).
	w.p.vfsSync(w.path, walCkptSyncBits(w.p.walSyncFlags))
	// wal.c:2257 — nBackfillAttempted before the copyback.
	w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.NBackfillAttempted = mxSafeFrame })
	// The copyback itself holds NO wal-index writer section: C's backfill
	// runs under the READ_LOCK(0) shm byte only, and its xWrite/xSync
	// callbacks re-enter OTHER connections (e_walckpt's tvfs_callback
	// BEGINs db6's reader and probes db5's write mid-copyback) — they need
	// the wal-index mutex themselves. The pager state the loop mutates is
	// guarded by p.mu (held by CheckpointMode throughout).
	berr := w.backfillLocked(nBackfill0, mxSafeFrame, mxFrame)
	w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) {
		if berr == nil {
			ci.NBackfill = mxSafeFrame
		}
	})
	if berr == nil && mxSafeFrame == mxFrame {
		// wal.c:2309-2316 — the database-file truncate and sync happen only
		// when the backfill reached the log's end (a partial checkpoint —
		// readers pinning mxSafeFrame below mxFrame — leaves the db file
		// unsynced: e_walckpt-1.passive.1.5's sync_counter stays 0).
		w.p.vfsSync(w.p.path, walCkptSyncBits(w.p.walSyncFlags))
	}
	w.wi.shmUnlock(walReadLockIdx(0), 1, true)
	return berr
}

// ckptPass2 ports walCheckpoint's PASS2 (eMode != PASSIVE): the log must be
// fully backfilled, else report busy; RESTART/TRUNCATE reset the log under
// exclusive READ_LOCK(1..4) — TRUNCATE truncates the -wal to ZERO bytes
// (R-44699-57140).
func (w *walWriter) ckptPass2(mode WalCheckpointMode, session *walBusySession) (int, error) {
	busy := 0
	if mode == WalCkptPassive {
		return busy, nil
	}
	var nBackfill, mxFrame uint32
	err := w.wi.WriterSection(func() error {
		nBackfill = w.wi.ckptInfoLocked().NBackfill
		mxFrame = w.hdr.MxFrame
		return nil
	})
	if err != nil {
		return busy, err
	}
	if nBackfill < mxFrame {
		return 1, nil
	}
	if mode < WalCkptRestart {
		return busy, nil
	}
	// wal.c:2347 — block until all readers are off the WAL (the busy
	// handler is invoked between contended attempts).
	if err := w.walBusyShmLock(walReadLockIdx(1), WalNReader-1, session); err != nil {
		if errors.Is(err, errWalBusy) {
			return 1, nil
		}
		return busy, err
	}
	rerr := w.wi.WriterSection(func() error {
		return w.walRestartHdrLocked(true)
	})
	w.wi.shmUnlock(walReadLockIdx(1), WalNReader-1, true)
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

// backfillLocked copies frames (nFrom, nTo] of the -wal into the main
// database file (walCheckpoint's iterator loop), updating the pager's page
// cache, header and page count. When nTo == mxFrame the main file is
// truncated to nPage*pageSize (wal.c's szDb truncate). Caller holds p.mu
// (via Pager.CheckpointMode) — NO wal-index writer section: the copyback's
// VFS callbacks re-enter other connections (e_walckpt's copyback probes).
func (w *walWriter) backfillLocked(nFrom, nTo, mxFrame uint32) error {
	p := w.p
	if nTo <= nFrom {
		return nil
	}
	frames, ok, err := w.readCheckpointFrames()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	// C reads every backfilled frame back from the "-wal" file
	// (walCheckpoint's sqlite3OsRead, wal.c L2309): a frame the wal-index
	// claims but the file does not hold (crash truncate, torn write) fails
	// the checkpoint with SQLITE_IOERR_SHORT_READ — nBackfill and the szDb
	// truncate are skipped (wal.c L2313-2322). Mirror that: the decoded log
	// must cover the window's final commit frame.
	if last := LastCommitFrame(frames); uint32(last) < nTo {
		return fmt.Errorf("pager: checkpoint: wal frame %d missing (short read)", nTo)
	}
	nPage, err := p.backfillFrameRangeLocked(frames, nFrom, nTo, mxFrame, w.hdr.NPage)
	if err != nil {
		return err
	}
	if nPage > p.numPages {
		p.numPages = nPage
	}
	return w.finishBackfillLocked(nTo, mxFrame)
}

// readCheckpointFrames reads and decodes the -wal frames for a checkpoint.
// ok=false when the WAL holds nothing trustworthy to backfill (smaller than
// its header, unreadable, or a header/frame decode failure). A Stat or read
// I/O error is returned. Caller holds p.mu and the wal-index writer section.
func (w *walWriter) readCheckpointFrames() ([]WalFrame, bool, error) {
	info, err := w.file.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Size() < WalHdrSize {
		return nil, false, nil
	}
	buf := make([]byte, info.Size())
	if _, err := w.file.ReadAt(buf, 0); err != nil {
		return nil, false, err
	}
	h, derr := DecodeWalHeader(buf)
	if derr != nil || !h.HeaderCksumOK {
		return nil, false, nil
	}
	frames, derr := DecodeWalFrames(buf, h)
	if derr != nil {
		return nil, false, nil
	}
	return frames, true, nil
}

// backfillFrameRangeLocked copies the frames in (nFrom, nTo] into the main
// database file, extending the file page by page and updating the pager's
// page cache. nPage starts at the recorded commit size and tracks the highest
// page seen. C's walIterator (wal.c walIteratorBuild/walIteratorNext) walks
// ONE entry per DISTINCT page — the frame carrying its latest content — in
// ascending page order, so a page committed several times is written ONCE
// (e_walckpt-1.3.2.2's write_count counts the checkpoint's xWrite events:
// two pages, two writes). Caller holds p.mu and the wal-index writer section.
func (p *Pager) backfillFrameRangeLocked(frames []WalFrame, nFrom, nTo, mxFrame, nPage uint32) (uint32, error) {
	// Latest frame per page (a later frame's image wins).
	byPage := make(map[uint32]int)
	for i, fr := range frames {
		if fr.Number > int(nTo) || fr.Number <= int(nFrom) {
			continue
		}
		if !fr.Valid || fr.Number > int(mxFrame) {
			break
		}
		byPage[fr.PageNumber] = i
	}
	pgnos := make([]int, 0, len(byPage))
	for pgno := range byPage {
		pgnos = append(pgnos, int(pgno))
	}
	sort.Ints(pgnos)
	for _, pgnoInt := range pgnos {
		pgno := uint32(pgnoInt)
		// wal.c:2289-2292: the copyback checks the connection's interrupt
		// flag per page and breaks with SQLITE_INTERRUPT — the szDb
		// truncate and the nBackfill advance are skipped (the SQLITE_OK
		// gate at wal.c:2308 covers both), so the interrupted checkpoint
		// leaves the log's frames standing for the next attempt.
		if p.interruptFired() {
			return nPage, ErrCheckpointInterrupted
		}
		// testvfs's xWrite fires per main-file page write (walvfs-3.1's
		// interrupt counter counts the checkpoint's copyback writes).
		if err := p.vfsCall("xWrite", p.path); err != nil {
			return nPage, err
		}
		fr := frames[byPage[pgno]]
		pg := &Page{PageNum: pgno, Data: append([]byte(nil), fr.PageData...)}
		p.pages[pgno] = pg
		if pgno > nPage {
			// A frame beyond the recorded nPage: the commit record's size
			// governs, but never write past the pages we have.
			nPage = pgno
		}
		if err := p.writeBackfillPageLocked(pg); err != nil {
			return nPage, err
		}
	}
	return nPage, nil
}

// writeBackfillPageLocked writes one backfilled frame's page image to the
// main database file, extending the file first when the page lands past the
// current end. Caller holds p.mu and the wal-index writer section.
func (p *Pager) writeBackfillPageLocked(pg *Page) error {
	pgno := pg.PageNum
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
	return nil
}

// finishBackfillLocked completes the backfill: the recovered database header
// is taken from the cached page 1, and when the whole log was covered
// (nTo == mxFrame) the main file is truncated to the committed page count
// (wal.c: szDb truncate under mxSafeFrame == mxFrame). The dirty set is
// cleared — every page is now clean in the main file — and the
// external-change stamp re-baselined. Caller holds p.mu and the wal-index
// writer section.
func (w *walWriter) finishBackfillLocked(nTo, mxFrame uint32) error {
	p := w.p
	// Recover the database header from page 1 if present.
	if pg, ok := p.pages[1]; ok {
		if p.header == nil {
			p.header = make([]byte, HeaderSize)
		}
		copy(p.header, pg.Data[:HeaderSize])
		p.invalidateCookieCacheLocked()
	}
	if nTo == mxFrame {
		szDb := int64(w.hdr.NPage) * int64(p.pageSize)
		if szDb > 0 && p.fileSize != szDb {
			if err := p.file.Truncate(szDb); err != nil {
				return err
			}
			p.fileSize = szDb
		}
	}
	p.clearDirtySetLocked()
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
