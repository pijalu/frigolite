// External-change detection of pager.c sqlite3PagerSharedLock: at the start
// of every statement the pager compares the file's 16-byte change version
// (header offset 24..39) and its size against the state this connection last
// observed; on any difference the page cache is dropped (pager_reset) and
// lockBtree's header-vs-file checks run (a header page count beyond the
// file's pages is corruption — "database disk image is malformed").
package pager

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/pijalu/frigolite/internal/storage"
)

// fileVersLen is the size of the change-detection window pager.c compares on
// every shared-lock acquisition: 16 bytes at offset 24 (the file change
// counter plus the freelist fields the TCL corruption tests patch with
// hexio_write).
const fileVersLen = 16

// CheckExternalFile compares the file's change version and size against the
// baseline this connection last observed (set at open and refreshed after
// every own flush). On a difference — another connection committed, or an
// external tool patched/truncated the file — the page cache and cached
// header are invalidated and the new state becomes the baseline (pager.c
// pager_reset). Reports whether an external change was seen. In-memory
// pagers have no file and never change externally.
func (p *Pager) CheckExternalFile() bool {
	if p.file == nil {
		return false
	}
	vers, size, ok := p.readFileStamp()
	if !ok {
		return false
	}
	p.mu.Lock()
	changed := vers != p.knownFileVers || size != p.knownFileSize
	if changed {
		p.knownFileVers = vers
		p.knownFileSize = size
	}
	p.mu.Unlock()
	if changed {
		p.InvalidateCache()
	}
	return changed
}

// HeaderBeyondFile reports the lockBtree corruption check (btree.c): the
// header's database size in pages (offset 28) is trusted only when the
// change counter matches the version-valid-for copy (offset 92); a trusted
// count that exceeds the file's actual page count means the file was
// truncated underneath the connection and is malformed.
func (p *Pager) HeaderBeyondFile() bool {
	if p.file == nil {
		return false
	}
	h := p.currentHeader()
	if len(h) < 96 {
		return false
	}
	// In WAL mode the main database file is updated only by Checkpoint, so
	// its on-disk size lags the committed page count: every WAL frame is
	// recovered into the in-memory page cache at Open / InvalidateCache, but
	// the file is not rewritten until a checkpoint. Comparing the header's
	// page count against the physical file size would therefore mis-report a
	// healthy WAL database as truncated/corrupt. lockBtree validates against
	// the pager's page count (p.numPages), which already includes the
	// WAL-recovered pages, so use that in WAL mode instead of FilePageCount.
	filePages := p.FilePageCount()
	if p.wal != nil {
		filePages = p.NumPages()
	}
	nPage := binary.BigEndian.Uint32(h[28:32])
	if nPage == 0 || string(h[24:28]) != string(h[92:96]) {
		nPage = filePages
	}
	return nPage > filePages
}

// FilePageCount returns the number of complete pages the file currently
// holds (pager.c pagerPagecount derives Pager.dbSize from the file size).
func (p *Pager) FilePageCount() uint32 {
	p.mu.RLock()
	size := p.fileSize
	p.mu.RUnlock()
	return uint32(size / int64(p.pageSize))
}

// HeaderPageCount returns the database size in pages recorded in the header
// (offset 28) — SQLite's pBt->nPage, set by lockBtree from page 1. Falls
// back to the file's page count when no header is cached.
func (p *Pager) HeaderPageCount() uint32 {
	h := p.currentHeader()
	if len(h) >= 32 {
		if n := binary.BigEndian.Uint32(h[28:32]); n != 0 {
			return n
		}
	}
	return p.FilePageCount()
}

// FreelistCount returns the header's total freelist page count (offset 36),
// read fresh from the file so externally patched headers are observed.
func (p *Pager) FreelistCount() uint32 {
	h := p.currentHeader()
	if len(h) < 40 {
		return 0
	}
	return binary.BigEndian.Uint32(h[36:40])
}

// DecrementFreelistCount decrements the on-disk freelist count by n, capping
// at zero (no underflow). This is the header-side bookkeeping for one step of
// `PRAGMA incremental_vacuum` / autoVacuumCommit: each call consumes one
// free page from the count. The free page itself is NOT relocated/truncated
// here — full file shrinkage requires the deeper pager port (sqlite3PagerMovepage
// + pBt->bDoTruncate + truncate in btree.c autoVacuumCommit, P8.INCRVACUUM
// follow-up). Used to drive `db eval {PRAGMA incremental_vacuum}` callback
// row counts so testgen loops terminate.
func (p *Pager) DecrementFreelistCount(n uint32) {
	if n == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.header
	if len(h) < 40 {
		h = p.currentHeader()
		if len(h) < 40 {
			return
		}
		p.header = h
	}
	cur := binary.BigEndian.Uint32(h[36:40])
	if n > cur {
		n = cur
	}
	binary.BigEndian.PutUint32(h[36:40], cur-n)
	p.dirty[1] = true
}

// currentHeader returns the 100-byte database header: the cached copy when
// present, else a fresh read from the file (filling the cache as a side
// effect, mirroring readDbPage restoring Pager.dbFileVers from page 1).
func (p *Pager) currentHeader() []byte {
	p.mu.RLock()
	h := p.header
	p.mu.RUnlock()
	if len(h) >= 100 || p.file == nil {
		return h
	}
	buf := make([]byte, HeaderSize)
	if _, err := p.file.ReadAt(buf, 0); err != nil && err != io.EOF {
		return p.header
	}
	p.SetHeader(buf)
	return buf
}

// readFileStamp reads the file's change-version window and size.
func (p *Pager) readFileStamp() ([fileVersLen]byte, int64, bool) {
	var vers [fileVersLen]byte
	info, err := p.file.Stat()
	if err != nil {
		return vers, 0, false
	}
	if _, err := p.file.ReadAt(vers[:], 24); err != nil && err != io.EOF {
		return vers, 0, false
	}
	return vers, info.Size(), true
}

// refreshKnownFileStamp records the file's current change version and size
// as this connection's baseline (called after open and after the pager's own
// flushes, so own writes are not mistaken for external changes). The caller
// must hold p.mu or otherwise guarantee exclusive access.
func (p *Pager) refreshKnownFileStamp() {
	if p.file == nil {
		return
	}
	if vers, size, ok := p.readFileStamp(); ok {
		p.knownFileVers = vers
		p.knownFileSize = size
	}
}

// ReadPtrmap reads the pointer-map entry for pgno (P8.INCRVACUUM
// phase 2). Returns (parentType, parentPgno, err). The pointer-map
// page is located via storage.PtrmapPageNo; if it's in the cache
// we use the cached copy, otherwise we read it from the file. An
// uninitialized entry (type=0) returns (0, 0, nil) per SQLite
// semantics. If pgno is itself a pointer-map page, the call errors
// (ptrmap pages have no entry).
func (p *Pager) ReadPtrmap(pgno uint32) (parentType byte, parentPgno uint32, err error) {
	if pgno < 2 {
		return 0, 0, fmt.Errorf("pager: ReadPtrmap: pgno %d < 2", pgno)
	}
	ptrmapPg := storage.PtrmapPageNo(pgno, p.pageSize)
	if ptrmapPg == pgno {
		return 0, 0, fmt.Errorf("pager: ReadPtrmap: pgno %d is a pointer-map page", pgno)
	}
	pg, err := p.ReadPage(ptrmapPg)
	if err != nil {
		return 0, 0, err
	}
	return storage.PtrmapEntry(pg.Data, pgno, p.pageSize)
}

// WritePtrmap writes a pointer-map entry for pgno (P8.INCRVACUUM
// phase 2). The entry is (parentType, parentPgno). The pointer-map
// page is read into the cache, the entry is written at the correct
// offset, and the page is marked dirty. Returns an error if pgno
// is invalid for a pointer-map entry.
func (p *Pager) WritePtrmap(pgno uint32, parentType byte, parentPgno uint32) error {
	if pgno < 2 {
		return fmt.Errorf("pager: WritePtrmap: pgno %d < 2", pgno)
	}
	ptrmapPg := storage.PtrmapPageNo(pgno, p.pageSize)
	if ptrmapPg == pgno {
		return fmt.Errorf("pager: WritePtrmap: pgno %d is a pointer-map page", pgno)
	}
	pg, err := p.ReadPage(ptrmapPg)
	if err != nil {
		return err
	}
	if _, err := storage.WritePtrmapEntry(pg.Data, pgno, p.pageSize, parentType, parentPgno); err != nil {
		return err
	}
	p.mu.Lock()
	p.dirty[ptrmapPg] = true
	p.mu.Unlock()
	return nil
}

// IsPageOnFreelist reports whether pgno is currently on the in-memory
// freelist (P8.INCRVACUUM phase 3). Used by IncrVacuumStep to decide
// whether the last page of the file can be truncated directly (it's on
// the freelist) or needs a page-swap (it's in use).
func IsPageOnFreelist(p *Pager, pgno uint32) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.freePages[pgno]
}

// MarkPageDirtyForVacuum marks pgno as dirty so its in-memory content
// gets written back on the next flush (P8.INCRVACUUM phase 3). Used by
// RelocatePage (which copies page content directly into pg.Data and
// needs the page to be flushed).
func MarkPageDirtyForVacuum(p *Pager, pgno uint32) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.dirty[pgno] = true
	p.mu.Unlock()
}
