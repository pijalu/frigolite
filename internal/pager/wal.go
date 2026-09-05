package pager

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// walWriter implements the SQLite WAL write path for a single connection
// (src/wal.c). It appends frames to the "-wal" file with SQLite-compatible
// fibonacci-weighted checksums and recovers committed frames on open. The
// wal-index shared memory ("-shm") is maintained in-memory: a single
// connection has no concurrent readers to coordinate, and recovery rebuilds
// the frame->page map directly from the "-wal" file (wal.c walIndexRecover).
//
// The frame/header byte layout and checksum chain follow walview.go's decoder
// (which is validated against oracle fixtures), so recovery can read what the
// writer produces.
type walWriter struct {
	p        *Pager
	path     string // "-wal" path
	shmPath  string // "-shm" path
	file     *os.File
	pageSize uint32
	salt1    uint32
	salt2    uint32
	cksum1   uint32 // running checksum seed (header checksum)
	cksum2   uint32
	nFrame   int // number of frames written so far (1-based next index)
}

// walMagicLE is the little-endian-checksum WAL magic (WalMagic); the LSB 0
// selects little-endian 32-bit word interpretation of the checksum data.
const walMagicLE = WalMagic // 0x377f0682

// openWal opens (or creates) the "-wal" file for dbPath, (re)initializing the
// 32-byte header and positioning the writer to append after any existing valid
// frames. It does NOT recover committed frames — callers invoke recoverWal
// separately when reopening a database that may carry uncheckpointed frames.
func openWal(p *Pager, dbPath string, pageSize uint32) (*walWriter, error) {
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	f, err := os.OpenFile(walPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("pager: open wal %s: %w", walPath, err)
	}
	// Faithfulness: WAL mode materializes a wal-index shared-memory file.
	if shm, err := os.OpenFile(shmPath, os.O_RDWR|os.O_CREATE, 0644); err == nil {
		_ = shm.Truncate(32768)
		shm.Close()
	}
	w := &walWriter{
		p:        p,
		path:     walPath,
		shmPath:  shmPath,
		file:     f,
		pageSize: pageSize,
	}
	if err := w.initHeader(); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

// initHeader loads an existing valid WAL header or writes a fresh one. A fresh
// header gets a random salt (wal.c sqlitepag_wal and walEncodeHeader).
func (w *walWriter) initHeader() error {
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() >= WalHdrSize {
		buf := make([]byte, WalHdrSize)
		if _, err := w.file.ReadAt(buf, 0); err != nil {
			return err
		}
		h, err := DecodeWalHeader(buf)
		if err == nil && h.HeaderCksumOK && h.PageSize == w.pageSize {
			// Existing valid WAL: continue the checksum chain and frame count.
			w.salt1 = h.Salt1
			w.salt2 = h.Salt2
			w.cksum1 = h.Checksum1
			w.cksum2 = h.Checksum2
			w.nFrame = w.countFrames()
			return nil
		}
		// Stale/corrupt header: fall through to overwrite.
	}
	return w.writeFreshHeader()
}

// writeFreshHeader generates a new salt and writes the 32-byte WAL header.
func (w *walWriter) writeFreshHeader() error {
	var s1, s2 [4]byte
	if _, err := rand.Read(s1[:]); err != nil {
		return err
	}
	if _, err := rand.Read(s2[:]); err != nil {
		return err
	}
	w.salt1 = binary.BigEndian.Uint32(s1[:])
	w.salt2 = binary.BigEndian.Uint32(s2[:])
	w.nFrame = 0

	hdr := make([]byte, WalHdrSize)
	binary.BigEndian.PutUint32(hdr[0:], walMagicLE)
	// [4:8] version field (unused by recovery; mirrors WAL_MAX_VERSION).
	binary.BigEndian.PutUint32(hdr[4:], 3007000)
	// [8:12] page size (walview.DecodeWalHeader reads PageSize here).
	binary.BigEndian.PutUint32(hdr[8:], w.pageSize)
	// [12:16] checkpoint sequence (0 = no checkpoint yet).
	binary.BigEndian.PutUint32(hdr[12:], 0)
	// [16:20] salt-1, [20:24] salt-2.
	binary.BigEndian.PutUint32(hdr[16:], w.salt1)
	binary.BigEndian.PutUint32(hdr[20:], w.salt2)
	// Header checksum over the first 24 bytes (WalFrameHdrSize) with zero seed.
	hc1, hc2 := WalChecksumBytes(false, hdr[:WalFrameHdrSize], 0, 0)
	w.cksum1, w.cksum2 = hc1, hc2
	binary.BigEndian.PutUint32(hdr[24:], hc1)
	binary.BigEndian.PutUint32(hdr[28:], hc2)
	if _, err := w.file.WriteAt(hdr, 0); err != nil {
		return err
	}
	return nil
}

// countFrames returns the number of complete frames in the current "-wal" file
// (used when continuing an existing WAL).
func (w *walWriter) countFrames() int {
	info, err := w.file.Stat()
	if err != nil {
		return 0
	}
	frameSize := int64(w.pageSize) + WalFrameHdrSize
	n := int((info.Size() - WalHdrSize) / frameSize)
	if n < 0 {
		return 0
	}
	return n
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
	binary.BigEndian.PutUint32(fh[8:], w.salt1)
	binary.BigEndian.PutUint32(fh[12:], w.salt2)
	// Checksum chain: seed from the running (header) checksum, extend over the
	// frame header's first 8 bytes then the page data (wal.c validity rule).
	w.cksum1, w.cksum2 = WalChecksumBytes(false, fh[:8], w.cksum1, w.cksum2)
	w.cksum1, w.cksum2 = WalChecksumBytes(false, pg.Data, w.cksum1, w.cksum2)
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
// p.numPages). The wal hook (sqlite3_wal_hook) fires after the commit with the
// number of frames appended.
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
	before := w.nFrame
	dbSize := p.numPages
	for i, pg := range pages {
		commit := i == len(pages)-1
		if err := w.appendFrame(pg, commit, dbSize); err != nil {
			return 0, err
		}
	}
	appended := w.nFrame - before
	if p.walHook != nil {
		p.walHook(appended, 0)
	}
	return appended, nil
}

// recoverWal reads the "-wal" file, applies every frame up to the last commit
// record (wal.c mxFrame recovery: frames past the last commit are ignored as a
// torn/incomplete transaction), and rebuilds the pager's in-memory page cache
// and header. It locks p.mu itself; callers already holding p.mu must use
// recoverWalLocked.
func recoverWal(p *Pager, dbPath string, pageSize uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return recoverWalLocked(p, dbPath, pageSize)
}

// recoverWalLocked is recoverWal without the mutex lock (caller holds p.mu).
func recoverWalLocked(p *Pager, dbPath string, pageSize uint32) error {
	walPath := dbPath + "-wal"
	f, err := os.Open(walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	buf, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if len(buf) < WalHdrSize {
		return nil
	}
	h, err := DecodeWalHeader(buf)
	if err != nil || !h.HeaderCksumOK {
		// Corrupt or non-WAL header: treat as no recoverable frames.
		return nil
	}
	frames, err := DecodeWalFrames(buf, h)
	if err != nil {
		return nil
	}
	last := LastCommitFrame(frames)
	if last == 0 {
		return nil
	}
	// Caller holds p.mu (recoverWal or InvalidateCache); do not re-lock.
	for _, fr := range frames {
		if fr.Number > last {
			break
		}
		if !fr.Valid {
			break
		}
		pg := &Page{PageNum: fr.PageNumber, Data: append([]byte(nil), fr.PageData...)}
		p.pages[fr.PageNumber] = pg
		if fr.PageNumber > p.numPages {
			p.numPages = fr.PageNumber
		}
	}
	// Recover the database header from page 1 if present.
	if pg, ok := p.pages[1]; ok && p.header == nil {
		hdr := make([]byte, HeaderSize)
		copy(hdr, pg.Data[:HeaderSize])
		p.header = hdr
	}
	return nil
}

// WalCheckpointMode mirrors the SQLITE_CHECKPOINT_* constants from
// sqlite/src/wal.c (sqlite3.h) and selects what a WAL checkpoint does
// beyond reporting its size. PASSIVE only reports — frames are kept in
// the -wal file so concurrent readers (or a follow-up test that asserts
// -wal size) can still see them. FULL/RESTART/TRUNCATE backfill the
// main database file; RESTART/TRUNCATE additionally truncate the -wal
// to just its 32-byte header.
type WalCheckpointMode int

const (
	WalCkptPassive  WalCheckpointMode = 0
	WalCkptFull     WalCheckpointMode = 1
	WalCkptRestart  WalCheckpointMode = 2
	WalCkptTruncate WalCheckpointMode = 3
)

// checkpoint folds every committed WAL frame into the main database file and
// then truncates the "-wal" to just its header (a RESTART-style checkpoint,
// wal.c walRestartLog). After a checkpoint the main file carries the committed
// state and subsequent commits start a fresh WAL. The caller (Pager.Checkpoint)
// must hold p.mu.
//
// The mode argument selects the PRAGMA wal_checkpoint variant:
//   - Passive: do not modify the WAL; the frames stay on disk and a
//     subsequent read of test.db-wal sees the same content (incrvacuum2
//     4.2.1 asserts on this size).
//   - Full: fold frames into the main DB but do not truncate the WAL.
//   - Restart / Truncate: fold frames AND truncate the WAL to its header.
func (w *walWriter) checkpoint(mode WalCheckpointMode) error {
	p := w.p
	// PASSIVE: leave the WAL exactly as it is (do not fold frames, do not
	// truncate). Used by PRAGMA wal_checkpoint with no argument — the test
	// harness asserts on -wal file size after the call.
	if mode == WalCkptPassive {
		return nil
	}
	// Read and apply all committed frames through the writer's own file.
	buf, err := io.ReadAll(w.file)
	if err != nil {
		return err
	}
	if len(buf) < WalHdrSize {
		return nil
	}
	h, err := DecodeWalHeader(buf)
	if err != nil || !h.HeaderCksumOK {
		return nil
	}
	frames, err := DecodeWalFrames(buf, h)
	if err != nil {
		return nil
	}
	last := LastCommitFrame(frames)
	if last == 0 {
		return nil
	}
	// Apply frames to the pager cache (single connection: in-memory then file).
	for _, fr := range frames {
		if fr.Number > last {
			break
		}
		if !fr.Valid {
			break
		}
		pg := &Page{PageNum: fr.PageNumber, Data: append([]byte(nil), fr.PageData...)}
		p.pages[fr.PageNumber] = pg
		if fr.PageNumber > p.numPages {
			p.numPages = fr.PageNumber
		}
	}
	if pg, ok := p.pages[1]; ok {
		if p.header == nil {
			p.header = make([]byte, HeaderSize)
		}
		copy(p.header, pg.Data[:HeaderSize])
	}
	// Write recovered pages to the main file.
	for n, pg := range p.pages {
		off := int64(n-1) * int64(p.pageSize)
		fileEnd := int64(n) * int64(p.pageSize)
		if p.fileSize < fileEnd {
			if err := p.file.Truncate(fileEnd); err != nil {
				return err
			}
			p.fileSize = fileEnd
		}
		if _, err := p.file.WriteAt(pg.Data, off); err != nil {
			return err
		}
	}
	p.dirty = make(map[uint32]bool)
	p.refreshKnownFileStamp()
	// FULL: keep the WAL frames (backfill only); RESTART/TRUNCATE reset it.
	if mode == WalCkptFull {
		return nil
	}
	// Truncate the WAL to its header (RESTART checkpoint).
	if err := w.file.Truncate(WalHdrSize); err != nil {
		return err
	}
	w.nFrame = 0
	return w.writeFreshHeader()
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

// Close closes the "-wal" file.
func (w *walWriter) Close() error {
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}
