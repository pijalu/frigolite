package pager

// walrecover.go — walIndexRecover port (src/wal.c L1384): rebuild the
// wal-index (shared header + pgno→frame hash tables) from the "-wal" file.
//
// Recovery runs under the WRITER shm lock (its C caller contract) and takes
// the exclusive CKPT+RECOVER byte range {1 2 lock exclusive} for the duration
// (slice 2: the real flock + aLock shadow pair) — a contended range reports
// BUSY, which the caller converts to WAL_RETRY / BUSY_RECOVERY
// (walprotocol-1.3: a persistent veto burns the retry budget into
// SQLITE_PROTOCOL "locking protocol"). The finishing block re-initializes
// read marks 1..4 under transient exclusive READ_LOCK(i) locks, tolerating
// BUSY on each (wal.c L1576: a reader holding a mark keeps its mark;
// walprotocol-1.5 succeeds anyway).
//
// The caller refreshes the shared header first; an unparsable header means
// the wal-index must be reconstructed — a crashed process's frames are
// re-indexed frame by frame, stopping at the first invalid frame or after
// the last commit record, exactly like wal.c's walk.

import (
	"encoding/binary"
	"fmt"
)

// walIndexRecoverLocked ports walIndexRecover. The caller holds the WRITER
// shm lock (w.writeLock) and the wal-index writer section. This function
// takes the exclusive CKPT+RECOVER range for its duration and releases it on
// every exit path (wal.c recovery_error's unlock).
func (w *walWriter) walIndexRecoverLocked() error {
	// iLock = WAL_ALL_BUT_WRITE + ckptLock = 1, n = WAL_READ_LOCK(0)-iLock = 2
	// (slice 4's snapshot_recover caller, which pre-holds CKPT, will widen
	// this to the C ckptLock form). The CALLER holds the wal-index writer
	// section for the whole recovery — the commit-path callers (a corrupt
	// header under the commit's section) need it as one section; the
	// read-phase-outside split lives in walRecoverWithWriterLock.
	if err := w.shmTryLockHeld(walLockCkpt, walLockRecover+1-walLockCkpt, true); err != nil {
		return err
	}
	parsed, rerr := w.walIndexRecoverRead()
	if rerr == nil {
		rerr = w.walIndexRecoverApplyLocked(parsed)
	}
	w.shmUnlockHeld(walLockCkpt, walLockRecover+1-walLockCkpt, true)
	return rerr
}

// walRecoverParsed is the recovery's read-phase result: everything the
// -wal file yields, parsed outside the wal-index writer section (C builds
// the hash tables in PRIVATE buffers while reading the log — the index
// mutex is not held across the file reads, so a slow recovering reader
// (walsetlk_recover's parked xRead) does not stall other connections'
// index probes).
type walRecoverParsed struct {
	// versionErr carries the CANTOPEN-family format-version mismatch.
	versionErr error
	frames     []uint32 // per-frame pgno, 1-based frame index implicit
	commits    []int    // indexes (0-based) of commit records (nTruncate != 0)
	truncates  []uint32 // the commit record's nTruncate, parallel to commits
	cksums     [][2]uint32
	endCksum   [2]uint32
	mxFrame    uint32
	nPage      uint32
	szPage     uint32
	bigEnd     bool
	salts      [2]uint32
	nCkpt      uint32
}

// walIndexRecoverRead is the recovery's READ phase: validate the WAL file
// header (magic, page size, checksum, version — a version mismatch is the
// SQLITE_CANTOPEN family error) and walk every frame advancing the
// cumulative checksum chain (walDecodeFrame validity rules), collecting the
// valid frames' pgno→frame mappings and commit records. NO wal-index state
// is touched: the caller holds the CKPT+RECOVER range and the WRITER lock,
// but the wal-index writer section is deliberately NOT held (C's private
// buffers) — a slow read here parks the recovery lock without stalling the
// index probes.
func (w *walWriter) walIndexRecoverRead() (*walRecoverParsed, error) {
	parsed := &walRecoverParsed{}
	info, err := w.file.Stat()
	if err != nil {
		return nil, err
	}
	nSize := info.Size()
	if nSize <= WalHdrSize {
		return parsed, nil
	}
	// The recovery's -wal reads are VFS xReads (C's sqlite3OsRead on the
	// wal fd) — walsetlk_recover's fixture parks its callback on the first
	// one, holding the recovery lock mid-recovery.
	if herr := w.p.vfsCall("xRead", w.path); herr != nil {
		return nil, herr
	}
	buf := make([]byte, nSize)
	if _, err := w.file.ReadAt(buf, 0); err != nil {
		return nil, err
	}
	// The magic VALUE's low bit selects the checksum byte order; the value
	// is stored big-endian, so the low byte is buf[3] (the old parse read
	// the whole uint32 and masked &1 — the same bit).
	bigEnd := buf[3]&1 != 0
	szPage := binary.BigEndian.Uint32(buf[8:])
	// An invalid magic or page size means the WAL contains no valid data:
	// recovery finishes with an empty wal-index (wal.c "finished").
	if binary.BigEndian.Uint32(buf[0:])&0xFFFFFFFE != WalMagic ||
		szPage&(szPage-1) != 0 || szPage > 65536 || szPage < 512 {
		return parsed, nil
	}
	// Verify the WAL header checksum; its value seeds the frame chain.
	ck1, ck2 := WalChecksumBytes(bigEnd, buf[:WalHdrSize-8], 0, 0)
	if ck1 != binary.BigEndian.Uint32(buf[24:]) ||
		ck2 != binary.BigEndian.Uint32(buf[28:]) {
		return parsed, nil
	}
	// Verify the WAL format version (wal.c: SQLITE_CANTOPEN on mismatch).
	if version := binary.BigEndian.Uint32(buf[4:]); version != WalMaxVersion {
		return nil, fmt.Errorf("pager: open wal %s: unable to open database file", w.path)
	}
	parsed.szPage = szPage
	parsed.bigEnd = bigEnd
	parsed.salts = [2]uint32{binary.BigEndian.Uint32(buf[16:]), binary.BigEndian.Uint32(buf[20:])}
	parsed.nCkpt = binary.BigEndian.Uint32(buf[12:])
	frameSize := int64(szPage) + WalFrameHdrSize
	iLastFrame := (nSize - WalHdrSize) / frameSize
	for iFrame := int64(1); iFrame <= iLastFrame; iFrame++ {
		off := walFrameOffset(int(iFrame), szPage)
		fh := buf[off : off+WalFrameHdrSize]
		data := buf[off+WalFrameHdrSize : off+frameSize]
		// walDecodeFrame: salts must match the WAL header and the
		// cumulative checksum must equal the frame's stored pair.
		ck1, ck2 = WalChecksumBytes(bigEnd, fh[:8], ck1, ck2)
		ck1, ck2 = WalChecksumBytes(bigEnd, data, ck1, ck2)
		if binary.BigEndian.Uint32(fh[8:]) != parsed.salts[0] ||
			binary.BigEndian.Uint32(fh[12:]) != parsed.salts[1] ||
			ck1 != binary.BigEndian.Uint32(fh[16:]) ||
			ck2 != binary.BigEndian.Uint32(fh[20:]) {
			break
		}
		parsed.frames = append(parsed.frames, binary.BigEndian.Uint32(fh[0:]))
		if nTruncate := binary.BigEndian.Uint32(fh[4:]); nTruncate != 0 {
			// A non-zero nTruncate marks the commit record (wal.c mxFrame).
			parsed.commits = append(parsed.commits, len(parsed.frames)-1)
			parsed.truncates = append(parsed.truncates, nTruncate)
			parsed.cksums = append(parsed.cksums, [2]uint32{ck1, ck2})
			parsed.mxFrame = uint32(iFrame)
			parsed.nPage = nTruncate
		}
	}
	parsed.endCksum = [2]uint32{ck1, ck2}
	return parsed, nil
}

// walIndexRecoverApplyLocked applies the parsed recovery inside the
// wal-index writer section: zero the hash pages, append each frame's
// pgno→frame mapping, publish the recovered header and reset the
// checkpoint info. Caller holds the CKPT+RECOVER range and the wal-index
// writer section.
func (w *walWriter) walIndexRecoverApplyLocked(parsed *walRecoverParsed) error {
	// memset(&pWal->hdr, 0, sizeof(WalIndexHdr))
	w.hdr = WalIndexHdr{}
	if parsed.versionErr != nil {
		return parsed.versionErr
	}
	w.hdr.BigEndCksum = parsed.bigEnd
	w.nCkpt = parsed.nCkpt
	w.hdr.ASalt = parsed.salts
	iLastFrame := len(parsed.frames)
	for iPg := 0; iPg <= walFramePageOf(uint32(iLastFrame)); iPg++ {
		loc := newWalHashLoc(w.wi.pageLocked(iPg), iPg)
		loc.zeroMapping()
		w.wi.markDirtyLocked(iPg)
	}
	for i, pgno := range parsed.frames {
		if err := w.wi.appendLocked(uint32(i+1), pgno, 0); err != nil {
			return err
		}
	}
	if len(parsed.commits) > 0 {
		// The last commit record's checksum chain seeds the header.
		w.hdr.MxFrame = uint32(parsed.commits[len(parsed.commits)-1] + 1)
		w.hdr.NPage = parsed.truncates[len(parsed.truncates)-1]
		setPageSizeForHdr(&w.hdr, parsed.szPage)
		return w.finishRecoveryLocked(parsed.cksums[len(parsed.cksums)-1])
	}
	return w.finishRecoveryLocked([2]uint32{})
}

// finishRecoveryLocked publishes the recovered header and resets the
// checkpoint info (wal.c's "finished:" block L1562-1596): read marks 1..4
// are re-initialized each under a transient exclusive READ_LOCK(i) — a BUSY
// mark (an active reader) keeps its old value and does not fail recovery.
// Caller holds the CKPT+RECOVER range and the wal-index writer section.
func (w *walWriter) finishRecoveryLocked(aFrameCksum [2]uint32) error {
	w.hdr.AFrameCksum = aFrameCksum
	w.wi.writeHdrLocked(&w.hdr)
	w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) {
		ci.NBackfill = 0
		ci.NBackfillAttempted = w.hdr.MxFrame
		ci.AReadMark[0] = 0
	})
	for i := 1; i < WalNReader; i++ {
		if w.shmTryLockHeld(walReadLockIdx(i), 1, true) == nil {
			w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) {
				if i == 1 && w.hdr.MxFrame != 0 {
					ci.AReadMark[i] = w.hdr.MxFrame
				} else {
					ci.AReadMark[i] = ReadmarkNotUsed
				}
			})
			w.shmUnlockHeld(walReadLockIdx(i), 1, true)
		}
		// BUSY on the mark: leave the reader's value untouched (wal.c tolerates).
	}
	// Adopt the recovered state into the writer (next frame index, chain).
	w.nFrame = int(w.hdr.MxFrame)
	if w.hdr.MxFrame > 0 {
		w.cksum1, w.cksum2 = w.hdr.AFrameCksum[0], w.hdr.AFrameCksum[1]
	}
	return nil
}
