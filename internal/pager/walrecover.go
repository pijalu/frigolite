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
	// this to the C ckptLock form).
	if err := w.wi.shmTryLockHeld(walLockCkpt, walLockRecover+1-walLockCkpt, true); err != nil {
		return err
	}
	err := w.walIndexRecoverBodyLocked()
	w.wi.shmUnlockHeld(walLockCkpt, walLockRecover+1-walLockCkpt, true)
	return err
}

// walIndexRecoverBodyLocked is the recovery proper (wal.c L1404-1565): it
// validates the WAL file header (magic, page size, checksum, version — a
// version mismatch is the SQLITE_CANTOPEN family error), walks every frame
// updating the cumulative checksum chain (walDecodeFrame validity rules),
// appends each frame's pgno→frame mapping to the shared hash tables, and
// finishes by publishing the recovered header and resetting the checkpoint
// info (nBackfill=0, read marks per wal.c L1546-1560). Caller holds the
// CKPT+RECOVER range and the wal-index writer section.
func (w *walWriter) walIndexRecoverBodyLocked() error {
	// memset(&pWal->hdr, 0, sizeof(WalIndexHdr))
	w.hdr = WalIndexHdr{}
	var aFrameCksum [2]uint32

	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	nSize := info.Size()
	if nSize > WalHdrSize {
		buf := make([]byte, WalHdrSize)
		if _, err := w.file.ReadAt(buf, 0); err != nil {
			return err
		}
		magic := binary.BigEndian.Uint32(buf[0:])
		szPage := binary.BigEndian.Uint32(buf[8:])
		// An invalid magic or page size means the WAL contains no valid
		// data: recovery finishes with an empty wal-index (wal.c "finished").
		if magic&0xFFFFFFFE != WalMagic || szPage&(szPage-1) != 0 ||
			szPage > 65536 || szPage < 512 {
			return w.finishRecoveryLocked(aFrameCksum)
		}
		w.hdr.BigEndCksum = magic&1 != 0
		w.nCkpt = binary.BigEndian.Uint32(buf[12:])
		w.hdr.ASalt = [2]uint32{
			binary.BigEndian.Uint32(buf[16:]),
			binary.BigEndian.Uint32(buf[20:]),
		}
		// Verify the WAL header checksum; its value seeds the frame chain.
		ck1, ck2 := WalChecksumBytes(w.hdr.BigEndCksum, buf[:WalHdrSize-8], 0, 0)
		if ck1 != binary.BigEndian.Uint32(buf[24:]) ||
			ck2 != binary.BigEndian.Uint32(buf[28:]) {
			return w.finishRecoveryLocked(aFrameCksum)
		}
		// Verify the WAL format version (wal.c: SQLITE_CANTOPEN on mismatch).
		if version := binary.BigEndian.Uint32(buf[4:]); version != WalMaxVersion {
			return fmt.Errorf("pager: open wal %s: unable to open database file", w.path)
		}

		// Walk the frames. C builds the hash tables in a private zeroed
		// buffer and memcpy's page-by-page into the wal-index; slice 1 runs
		// under the registry writer section, so the (pre-zeroed) shared
		// buffers are populated directly.
		frameSize := int64(szPage) + WalFrameHdrSize
		iLastFrame := (nSize - WalHdrSize) / frameSize
		for iPg := 0; iPg <= walFramePageOf(uint32(iLastFrame)); iPg++ {
			loc := newWalHashLoc(w.wi.pageLocked(iPg), iPg)
			loc.zeroMapping()
			w.wi.markDirtyLocked(iPg)
		}
		for iFrame := int64(1); iFrame <= iLastFrame; iFrame++ {
			off := walFrameOffset(int(iFrame), szPage)
			fh := make([]byte, WalFrameHdrSize)
			if _, err := w.file.ReadAt(fh, off); err != nil {
				break // short read: torn frame
			}
			data := make([]byte, szPage)
			if _, err := w.file.ReadAt(data, off+WalFrameHdrSize); err != nil {
				break
			}
			// walDecodeFrame: salts must match the WAL header and the
			// cumulative checksum must equal the frame's stored pair.
			ck1, ck2 = WalChecksumBytes(w.hdr.BigEndCksum, fh[:8], ck1, ck2)
			ck1, ck2 = WalChecksumBytes(w.hdr.BigEndCksum, data, ck1, ck2)
			if binary.BigEndian.Uint32(fh[8:]) != w.hdr.ASalt[0] ||
				binary.BigEndian.Uint32(fh[12:]) != w.hdr.ASalt[1] ||
				ck1 != binary.BigEndian.Uint32(fh[16:]) ||
				ck2 != binary.BigEndian.Uint32(fh[20:]) {
				break
			}
			pgno := binary.BigEndian.Uint32(fh[0:])
			if err := w.wi.appendLocked(uint32(iFrame), pgno, 0); err != nil {
				return err
			}
			// A non-zero nTruncate marks the commit record (wal.c mxFrame).
			if nTruncate := binary.BigEndian.Uint32(fh[4:]); nTruncate != 0 {
				w.hdr.MxFrame = uint32(iFrame)
				w.hdr.NPage = nTruncate
				setPageSizeForHdr(&w.hdr, szPage)
				aFrameCksum = [2]uint32{ck1, ck2}
			}
		}
	}
	return w.finishRecoveryLocked(aFrameCksum)
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
		if w.wi.shmTryLockHeld(walReadLockIdx(i), 1, true) == nil {
			w.wi.setCkptInfoLocked(func(ci *WalCkptInfo) {
				if i == 1 && w.hdr.MxFrame != 0 {
					ci.AReadMark[i] = w.hdr.MxFrame
				} else {
					ci.AReadMark[i] = ReadmarkNotUsed
				}
			})
			w.wi.shmUnlockHeld(walReadLockIdx(i), 1, true)
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
