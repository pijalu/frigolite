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
	"fmt"
	"os"
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
	// (C's pWal->hdr): mxFrame/nPage/iChange/aFrameCksum/aSalt/szPage.
	hdr WalIndexHdr
	// nCkpt is the checkpoint sequence from the WAL file header.
	nCkpt uint32
}

// walMagicLE is the little-endian-checksum WAL magic (WalMagic); the LSB 0
// selects little-endian 32-bit word interpretation of the checksum data.
const walMagicLE = WalMagic // 0x377f0682

// openWal opens (or creates) the "-wal" file for dbPath and attaches to the
// shared wal-index. The shared header is read (walIndexReadHdr port); an
// unusable header triggers recovery from the -wal file (walIndexRecover
// port). Committed frames are NOT replayed into the page cache here — reads
// resolve through the wal-index (see Pager.readPageLocked).
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
	w := &walWriter{p: p, path: walPath, file: f, pageSize: pageSize, wi: wi}
	if err := wi.WriterSection(w.initHeaderLocked); err != nil {
		wi.release()
		f.Close()
		return nil, err
	}
	return w, nil
}

// initHeaderLocked loads the shared wal-index header (recovering from the
// -wal when it does not parse) and adopts the WAL file's header state.
// Caller holds the wal-index writer section.
func (w *walWriter) initHeaderLocked() error {
	_, ok := w.wi.tryRefreshLocked(&w.hdr)
	if ok && w.hdr.MxFrame > 0 && !w.walFileHeaderSaltsMatchLocked() {
		// The shared header outlived its WAL file (restarted/truncated by
		// another generation): the -wal is the authority — rebuild.
		ok = false
	}
	if !ok {
		if err := w.walIndexRecoverLocked(); err != nil {
			return err
		}
	}
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

// writerBeginLocked ports the shared-header synchronization of
// sqlite3WalBeginWriteTransaction / walFrames: re-read the shared header,
// recover when it does not parse, and adopt its frame state. (The
// SQLITE_BUSY_SNAPSHOT staleness error requires a pinned read snapshot —
// slice 3.) Caller holds the wal-index writer section.
func (w *walWriter) writerBeginLocked() error {
	changed, ok := w.wi.tryRefreshLocked(&w.hdr)
	if !ok {
		if err := w.walIndexRecoverLocked(); err != nil {
			return err
		}
		changed = true
	}
	if w.hdr.IVersion != WalIndexMaxVersion {
		return fmt.Errorf("pager: wal: unable to open database file")
	}
	if changed {
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
// p.numPages). Frames are recorded in the shared wal-index hash tables and
// the shared header is published double-buffered (mxFrame, nPage, iChange++,
// aFrameCksum) — wal.c walFrames' isCommit branch. The wal hook
// (sqlite3_wal_hook) fires after the commit with the number of frames
// appended.
func (w *walWriter) commit() (int, error) {
	p := w.p
	if len(p.dirty) == 0 {
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
	appended := 0
	err := w.wi.WriterSection(func() error {
		if err := w.writerBeginLocked(); err != nil {
			return err
		}
		// walRestartLog (wal.c L3852): when the log is fully backfilled
		// (nBackfill == mxFrame > 0) and no readers hold WAL read marks, the
		// new frames overwrite the log from frame 1.
		info := w.wi.ckptInfoLocked()
		if info.NBackfill > 0 && info.NBackfill == w.hdr.MxFrame {
			if err := w.walRestartHdrLocked(false); err != nil {
				return err
			}
		}
		dbSize := p.numPages
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
		appended = len(pages)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if p.walHook != nil {
		p.walHook(appended, 0)
	}
	return appended, nil
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

// checkpoint ports walCheckpoint (wal.c L2193) for the single-process
// registry model: backfill committed frames (nBackfill, mxSafeFrame] into
// the main database file, coordinate the aReadMark slots (unpinned in
// slice 1 — the loop is the slice-3 seam), and reset the log for
// RESTART/TRUNCATE. It returns the PRAGMA wal_checkpoint triple
// (busy, nLog, nCkpt) — walprotocol-2.1 expects {0 5 5} for PASSIVE.
func (w *walWriter) checkpoint(mode WalCheckpointMode) (busy, nLog, nCkpt int, err error) {
	err = w.wi.WriterSection(func() error {
		// sqlite3WalCheckpoint: exclusive CKPT lock always; WRITER for
		// non-PASSIVE modes (slice 1: aLock shadow, uncontended; the shadow
		// helpers run under the held wal-index writer section).
		if !w.wi.tryExclusiveLocked(walLockCkpt, 1) {
			busy = 1
			return nil
		}
		defer w.wi.releaseExclusiveLocked(walLockCkpt, 1)
		if mode != WalCkptPassive {
			if !w.wi.tryExclusiveLocked(walLockWrite, 1) {
				busy = 1
				return nil
			}
			defer w.wi.releaseExclusiveLocked(walLockWrite, 1)
		}
		if err := w.writerBeginLocked(); err != nil {
			return err
		}
		nBackfill0 := w.wi.ckptInfoLocked().NBackfill
		mxFrame := w.hdr.MxFrame
		// PASS1: compute mxSafeFrame, stepping over active readers' marks.
		mxSafeFrame := mxFrame
		info := w.wi.ckptInfoLocked()
		for i := 1; i < WalNReader; i++ {
			y := info.AReadMark[i]
			if mxSafeFrame > y && y != ReadmarkNotUsed {
				if w.wi.tryExclusiveLocked(walReadLockIdx(i), 1) {
					iMark := uint32(ReadmarkNotUsed)
					if i == 1 {
						iMark = mxSafeFrame
					}
					w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.AReadMark[i] = iMark })
					w.wi.releaseExclusiveLocked(walReadLockIdx(i), 1)
				} else {
					// BUSY reader: stop the backfill short of its mark
					// (and stop invoking the busy handler, wal.c xBusy=0).
					mxSafeFrame = y
				}
			}
		}
		if nBackfill0 < mxSafeFrame {
			// Backfill under exclusive READ_LOCK(0) (wal.c walBusyLock).
			if !w.wi.tryExclusiveLocked(walReadLockIdx(0), 1) {
				mxSafeFrame = nBackfill0
			} else {
				w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.NBackfillAttempted = mxSafeFrame })
				berr := w.backfillLocked(nBackfill0, mxSafeFrame, mxFrame)
				if berr == nil {
					w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) { ci.NBackfill = mxSafeFrame })
				}
				w.wi.releaseExclusiveLocked(walReadLockIdx(0), 1)
				if berr != nil {
					return berr
				}
			}
		}
		// PASS2 (eMode != PASSIVE): the log must be fully backfilled, else
		// report busy; RESTART/TRUNCATE reset the log under the read locks.
		if mode != WalCkptPassive {
			if w.wi.ckptInfoLocked().NBackfill < w.hdr.MxFrame {
				busy = 1
				return nil
			}
			if mode >= WalCkptRestart {
				if !w.wi.tryExclusiveLocked(walReadLockIdx(1), WalNReader-1) {
					busy = 1
					return nil
				}
				rerr := w.walRestartHdrLocked(true)
				w.wi.releaseExclusiveLocked(walReadLockIdx(1), WalNReader-1)
				if rerr != nil {
					return rerr
				}
			}
		}
		// Result triple from the final state (sqlite3WalCheckpoint tail):
		// nLog = mxFrame, nCkpt = nBackfill — both 0 after a RESTART reset.
		nLog = int(w.hdr.MxFrame)
		nCkpt = int(w.wi.ckptInfoLocked().NBackfill)
		return nil
	})
	return busy, nLog, nCkpt, err
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
// (the last detach closes the shm fd and drops the registry entry).
func (w *walWriter) Close() error {
	if w.file != nil {
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
