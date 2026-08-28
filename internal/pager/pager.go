// Package pager manages reading and writing of database pages.
//
// File layout (SQLite compatible):
//
//	Page 1: bytes 0-99 = database header, bytes 100-(pageSize-1) = b-tree content (pageSize total)
//	Pages N>1: bytes 0-(pageSize-1) = b-tree content (pageSize total)
//
// The b-tree layer always sees Data of exactly pageSize bytes.
// For page 1, the first HeaderSize bytes are the database header (unused by b-tree).
// The pager handles the header transparently.
package pager

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/pijalu/frigolite/internal/storage"
)

const (
	// DefaultPageSize matches the SQLite test-build default used by the
	// transcribed TCL suite. Tests explicitly set page_size when needed.
	DefaultPageSize  = 1024
	DefaultCacheSize = 1000
	HeaderSize       = 100
)

type Pager struct {
	mu       sync.RWMutex
	pageSize uint32
	reserved uint32 // bytes reserved at page end per header byte 20
	file     *os.File
	pages    map[uint32]*Page
	dirty    map[uint32]bool
	readOnly bool
	numPages uint32
	header   []byte
	// fileSize caches the database file's size in bytes so flushPage can
	// decide whether a page write needs a Truncate without an Fstat syscall
	// per page (the dominant cost of per-commit flushes: 8000 FTS inserts
	// issue thousands of page writes, each previously Stat-ing the file).
	// Updated on Open, SetPageSize (truncate to 0), flushPage (grow), and
	// InvalidateCache (external modification). Only meaningful when file !=
	// nil.
	fileSize int64
	// autoVacuum enables pointer-map page reservation (btree.c
	// sqlite3BtreeSetAutoVacuum): page numbers at PTRMAP_PAGENO positions are
	// reserved as zeroed pointer-map pages, and callers of AllocatePage
	// receive the following page (btree.c allocateBtreePage's auto-vacuum
	// branch). Auto-vacuum and incremental-vacuum databases both carry the
	// pointer map.
	autoVacuum bool
	// knownFileVers/knownFileSize are the file stamp this connection last
	// observed (see external.go). openedEmpty records that the database was
	// 0 bytes when opened (pager.c lazy creation): opening it must not
	// materialize the file.
	openedEmpty bool
	// knownFileVers/knownFileSize are the file stamp this connection last
	// observed (pager.c Pager.dbFileVers plus the file size). Refreshed at
	// open and after every own flush; CheckExternalFile compares against it
	// per statement.
	knownFileVers [16]byte
	knownFileSize int64
}

type Page struct {
	Data    []byte
	PageNum uint32
}

// PagerState is a deep snapshot of a pager's in-memory state, used for
// statement-level rollback (e.g. a failed REPLACE that fired triggers).
type PagerState struct {
	pages    map[uint32]*Page
	dirty    map[uint32]bool
	numPages uint32
	header   []byte
}

// Snapshot captures the pager's current in-memory pages and header so they
// can be restored later with Restore.
func (p *Pager) Snapshot() *PagerState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s := &PagerState{
		pages:    make(map[uint32]*Page, len(p.pages)),
		dirty:    make(map[uint32]bool, len(p.dirty)),
		numPages: p.numPages,
	}
	if p.header != nil {
		s.header = append([]byte(nil), p.header...)
	}
	for n, pg := range p.pages {
		cp := &Page{PageNum: pg.PageNum, Data: append([]byte(nil), pg.Data...)}
		s.pages[n] = cp
		if p.dirty[n] {
			s.dirty[n] = true
		}
	}
	return s
}

// Restore replaces the pager's in-memory state with a snapshot taken earlier.
func (p *Pager) Restore(s *PagerState) {
	if s == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = make(map[uint32]*Page, len(s.pages))
	for n, pg := range s.pages {
		cp := &Page{PageNum: pg.PageNum, Data: append([]byte(nil), pg.Data...)}
		p.pages[n] = cp
	}
	p.dirty = make(map[uint32]bool, len(s.dirty))
	for n := range s.dirty {
		p.dirty[n] = true
	}
	p.numPages = s.numPages
	if s.header != nil {
		p.header = append([]byte(nil), s.header...)
	}
}

// Open opens a database file. If the file already exists, the page size is
// read from the database header (SQLite stores the actual page size in bytes
// 16-17 of the header); the pageSize argument is only a default for new files.
func Open(path string, pageSize uint32) (*Pager, error) {
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	// SQLite canonicalizes the filename through the VFS xFullPathname hook
	// (os_unix.c unixFullPathname -> appendOnePathElement) before open(2):
	// ".", ".." and duplicate/trailing slashes are resolved lexically, so a
	// path like "./a//b/../c//" opens "a/c" (lock3-1.1). filepath.Clean is
	// the lexical equivalent (no symlink resolution, matching SQLite's
	// no-readlink fallback).
	cleanPath := filepath.Clean(path)
	f, err := os.OpenFile(cleanPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("pager: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("pager: stat %s: %w", path, err)
	}

	pr := &Pager{
		pageSize: pageSize,
		file:     f,
		pages:    make(map[uint32]*Page),
		dirty:    make(map[uint32]bool),
		fileSize: info.Size(),
		// pager.c lazy creation: a database opened empty (0 bytes) must
		// stay untouched on disk until the first real write — opening and
		// closing it never materializes the file.
		openedEmpty: info.Size() == 0,
	}

	if info.Size() > 0 {
		// Read the 100-byte header first: it contains the real page size.
		headerBuf := make([]byte, HeaderSize)
		if _, err := f.ReadAt(headerBuf, 0); err != nil {
			f.Close()
			return nil, fmt.Errorf("pager: read header: %w", err)
		}
		hdr, err := storage.ParseHeader(headerBuf)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("pager: parse header: %w", err)
		}
		pr.pageSize = hdr.PageSize
		// Header byte 20: bytes reserved at the end of every page (used by
		// e.g. codec/checksum extensions). Payload distribution math must use
		// the USABLE size (pageSize - reserved), not the raw page size —
		// SQLite files written with reserved > 0 are otherwise unreadable.
		pr.reserved = uint32(hdr.ReservedSpace)
		// Read full page 1 into a temporary buffer
		fullPage := make([]byte, pr.pageSize)
		if _, err := f.ReadAt(fullPage, 0); err != nil && err != io.EOF {
			f.Close()
			return nil, fmt.Errorf("pager: read page 1: %w", err)
		}
		pr.header = make([]byte, HeaderSize)
		copy(pr.header, fullPage[:HeaderSize])
		pr.numPages = uint32(info.Size() / int64(pr.pageSize))
		if pr.numPages == 0 && info.Size() > 0 {
			pr.numPages = 1
		}
	}
	// Baseline the external-change stamp on what we just opened (pager.c
	// records Pager.dbFileVers when page 1 is first read).
	pr.refreshKnownFileStamp()

	return pr, nil
}

// OpenInMemory creates an in-memory pager.
func OpenInMemory(pageSize uint32) *Pager {
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	dh := storage.DefaultHeader(pageSize)
	return &Pager{
		pageSize: pageSize,
		file:     nil,
		pages:    make(map[uint32]*Page),
		dirty:    make(map[uint32]bool),
		numPages: 0,
		header:   dh.Encode(),
	}
}

func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.flushAll(); err != nil {
		return err
	}
	if p.file != nil {
		return p.file.Close()
	}
	return nil
}

func (p *Pager) PageSize() uint32 { return p.pageSize }

// UsableSize returns the number of usable bytes per page (page size minus
// the reserved-space count from header byte 20). SQLite's payload
// distribution formulas are defined over this value, not the raw page size.
func (p *Pager) UsableSize() uint32 { return p.pageSize - p.reserved }

// ValidateHeader checks the database header's freelist and root-page fields
// against the actual page count. A freelist trunk page, freelist count, or
// largest root btree page beyond the file's page count indicates a corrupt
// database (SQLite reports "database disk image is malformed"; the altercorrupt
// suite loads images whose header advertises a freelist far beyond the file).
func (p *Pager) ValidateHeader() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.header == nil || p.numPages == 0 {
		return nil
	}
	freelistTrunk := binary.BigEndian.Uint32(p.header[32:36])
	freelistCount := binary.BigEndian.Uint32(p.header[36:40])
	largestRoot := binary.BigEndian.Uint32(p.header[52:56])
	// A nonzero freelist trunk/count or largest-root page that exceeds the
	// file's page count is malformed. Zero freelist fields are valid (no
	// free pages).
	if freelistTrunk > p.numPages || freelistCount > p.numPages || largestRoot > p.numPages {
		return fmt.Errorf("database disk image is malformed")
	}
	return nil
}

// SetPageSize changes the pager's page size. It is only valid before any
// user data pages exist (PRAGMA page_size on an empty database); the caller
// is responsible for enforcing that. Page 1's Data buffer is resized so the
// header/payload layout stays consistent. For a file-backed database the
// file is truncated to zero so the next flush writes page 1 at the new size
// (a previously-written page 1 at the old size would otherwise leave a
// stale larger file, since flushPage only ever grows the file).
func (p *Pager) SetPageSize(ps uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pageSize = ps
	if p.file != nil {
		_ = p.file.Truncate(0)
		p.fileSize = 0
		// Page 1 (the schema page) already exists from Init; keep it as the
		// only page so the next AllocatePage starts at page 2. Resetting to 0
		// would make AllocatePage overwrite the schema page with a new table
		// page.
		if _, ok := p.pages[1]; ok {
			p.numPages = 1
		} else {
			p.numPages = 0
		}
	}
	if pg, ok := p.pages[1]; ok {
		newData := make([]byte, ps)
		copy(newData, pg.Data)
		pg.Data = newData
		// Reset page 1's b-tree header: page type leaf, empty (cell content
		// pointer at the usable end, as SQLite writes empty leaves). The old
		// header carried the previous page size's cell-content pointer, which
		// a free-space/cell-area check would reject at the new size.
		coff := 100 // page 1: content starts after the 100-byte header
		if len(newData) > coff+8 {
			newData[coff] = storage.PageTypeLeafTable
			for i := coff + 1; i < coff+5; i++ {
				newData[i] = 0
			}
			binary.BigEndian.PutUint16(newData[coff+5:coff+7], uint16(ps))
			newData[coff+7] = 0
		}
		p.dirty[1] = true
	}
}

// ResetToEmpty rewrites the database as a fresh, empty single-page database
// at the given page size (backup.c sqlite3BtreeNewDb → btree.c newDatabase,
// reached from backup_step's nSrcPage==0 branch): all cached pages are
// dropped, a canonical default header replaces the old one (both in the
// pager's header cache and inside page 1's first 100 bytes, so the on-disk
// image is self-consistent), page 1 is recreated as an empty schema leaf,
// and the file is truncated to exactly one page so the next Flush
// materializes precisely pageSize bytes.
func (p *Pager) ResetToEmpty(pageSize uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	p.pageSize = pageSize
	p.pages = make(map[uint32]*Page)
	p.dirty = make(map[uint32]bool)
	hdr := storage.DefaultHeader(pageSize).Encode()
	p.header = hdr
	p.numPages = 1
	pg := &Page{PageNum: 1, Data: make([]byte, pageSize)}
	copy(pg.Data[:HeaderSize], hdr)
	// Empty leaf-table b-tree: type byte at the content offset and the
	// cell-content pointer at the usable end (an empty leaf whose pointer
	// is 0 looks crash-written — "free space corruption").
	coff := HeaderSize
	pg.Data[coff] = storage.PageTypeLeafTable
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(pageSize))
	p.pages[1] = pg
	p.dirty[1] = true
	if p.file != nil {
		end := int64(pageSize)
		if err := p.file.Truncate(end); err == nil {
			p.fileSize = end
		}
	}
}

// MarkClean drops all dirty flags and re-baselines the external-change
// stamp WITHOUT writing anything. Used after opening an empty database:
// schema.Init allocates page 1 in memory (so the connection is usable), but
// pager.c lazy creation means opening — even followed by close — must leave
// a 0-byte file untouched until the first real write.
func (p *Pager) MarkClean() {
	p.mu.Lock()
	p.dirty = make(map[uint32]bool)
	p.mu.Unlock()
	p.refreshKnownFileStamp()
}

// OpenedEmpty reports whether the database file was 0 bytes when opened.
func (p *Pager) OpenedEmpty() bool { return p.openedEmpty }

func (p *Pager) NumPages() uint32 { p.mu.RLock(); defer p.mu.RUnlock(); return p.numPages }
func (p *Pager) Header() []byte   { p.mu.RLock(); defer p.mu.RUnlock(); return p.header }

// SetAutoVacuum toggles pointer-map page reservation for subsequent
// AllocatePage calls (btree.c sqlite3BtreeSetAutoVacuum). SQLite applies a
// mode change immediately only while the database is still empty; callers
// are responsible for that check.
func (p *Pager) SetAutoVacuum(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.autoVacuum = on
}

// AutoVacuum reports whether pointer-map pages are being reserved.
func (p *Pager) AutoVacuum() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.autoVacuum
}

// PtrmapPageNo returns the pointer-map page number covering pgno (btree.c
// ptrmapPageno): usableSize/5+1 pages are mapped per pointer-map page, the
// first being page 2. Returns 0 for pgno < 2.
func PtrmapPageNo(pgno, pageSize uint32) uint32 {
	if pgno < 2 {
		return 0
	}
	nPer := pageSize/5 + 1
	ret := ((pgno-2)/nPer)*nPer + 2
	// btree.c: a pointer-map page never lands on the pending-byte page.
	if ret == pendingBytePage(pageSize) {
		ret++
	}
	return ret
}

// pendingBytePage is the page holding the PENDING_BYTE lock byte
// (1073741824), which SQLite reserves and never uses (btree.c
// PENDING_BYTE_PAGE).
func pendingBytePage(pageSize uint32) uint32 {
	return 1073741824/pageSize + 1
}

// isPtrmapPageNo reports whether pgno itself is a pointer-map page.
func isPtrmapPageNo(pgno, pageSize uint32) bool {
	return pgno >= 2 && PtrmapPageNo(pgno, pageSize) == pgno
}

func (p *Pager) SetHeader(h []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.header = append([]byte(nil), h...)
}

// AllocatePage creates a new page. Data is always pageSize bytes.
// For page 1, the first HeaderSize bytes are reserved for the database header.
// With auto-vacuum enabled, page numbers at pointer-map positions are
// reserved as zeroed pointer-map pages and the caller receives the following
// page (btree.c allocateBtreePage reserves PTRMAP pages as they are crossed).
func (p *Pager) AllocatePage() *Page {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.numPages++
	// btree.c allocateBtreePage (auto-vacuum branch): when the next page is
	// a pointer-map page, zero it out (no b-tree header — its content is a
	// flat array of 5-byte entries maintained by ptrmapPut, unused until
	// pages are relocated by vacuuming) and extend the file once more so the
	// caller gets a normal page.
	if p.autoVacuum && isPtrmapPageNo(p.numPages, p.pageSize) {
		ptr := &Page{
			Data:    make([]byte, p.pageSize),
			PageNum: p.numPages,
		}
		p.pages[ptr.PageNum] = ptr
		p.dirty[ptr.PageNum] = true
		p.numPages++
	}
	pg := &Page{
		Data:    make([]byte, p.pageSize),
		PageNum: p.numPages,
	}
	// For page 1, pre-fill with header
	if p.numPages == 1 && p.header != nil {
		copy(pg.Data[:HeaderSize], p.header)
	}
	p.pages[pg.PageNum] = pg
	p.dirty[pg.PageNum] = true
	return pg
}

// ReadPage reads a page. Data is always pageSize bytes.
func (p *Pager) ReadPage(pageNum uint32) (*Page, error) {
	if pageNum == 0 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	p.mu.RLock()
	if pg, ok := p.pages[pageNum]; ok {
		p.mu.RUnlock()
		return pg, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if pg, ok := p.pages[pageNum]; ok {
		return pg, nil
	}
	if pageNum > p.numPages {
		return nil, fmt.Errorf("database disk image is malformed")
	}

	pg := &Page{
		Data:    make([]byte, p.pageSize),
		PageNum: pageNum,
	}
	if p.file != nil {
		off := int64(pageNum-1) * int64(p.pageSize)
		_, err := p.file.ReadAt(pg.Data, off)
		if err != nil {
			return nil, fmt.Errorf("pager: read page %d: %w", pageNum, err)
		}
		// For page 1, extract the header from the full page data
		if pageNum == 1 && p.header == nil {
			p.header = make([]byte, HeaderSize)
			copy(p.header, pg.Data[:HeaderSize])
		}
	}
	p.pages[pageNum] = pg
	return pg, nil
}

// IsMemory reports whether the pager is backed by memory (no file).
func (p *Pager) IsMemory() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.file == nil
}

// FileInfo returns the underlying file's info (nil, false for in-memory
// pagers). Used to detect external modification of attached database files.
func (p *Pager) FileInfo() (os.FileInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.file == nil {
		return nil, false
	}
	info, err := p.file.Stat()
	if err != nil {
		return nil, false
	}
	return info, true
}

// InvalidateCache drops the in-memory page cache and page-count so the next
// read re-reads the file. Used when an external connection may have modified
// the database file (schema reload after an ATTACHed file changes).
func (p *Pager) InvalidateCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = make(map[uint32]*Page)
	p.header = nil
	if p.file != nil {
		if info, err := p.file.Stat(); err == nil {
			p.fileSize = info.Size()
			p.numPages = uint32(info.Size() / int64(p.pageSize))
			if p.numPages == 0 && info.Size() > 0 {
				p.numPages = 1
			}
		}
	}
}

// WritePage marks a page as dirty.
func (p *Pager) WritePage(pg *Page) error {
	if p.readOnly {
		return fmt.Errorf("pager: read-only")
	}
	// For page 1, ensure the header is preserved in Data[0:HeaderSize]
	if pg.PageNum == 1 && p.header != nil {
		copy(pg.Data[:HeaderSize], p.header)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages[pg.PageNum] = pg
	p.dirty[pg.PageNum] = true
	return nil
}

// Truncate drops all pages after n, shrinking the in-memory cache and the
// database file to n pages (src/dbpage.c INSERT with NULL data truncates via
// sqlite3PagerTruncateImage).
func (p *Pager) Truncate(n uint32) error {
	if p.readOnly {
		return fmt.Errorf("pager: read-only")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for pgno := range p.pages {
		if pgno > n {
			delete(p.pages, pgno)
			delete(p.dirty, pgno)
		}
	}
	if n < p.numPages {
		p.numPages = n
	}
	if p.file != nil {
		if err := p.file.Truncate(int64(n) * int64(p.pageSize)); err != nil {
			return fmt.Errorf("pager: truncate to %d pages: %w", n, err)
		}
	}
	return nil
}

func (p *Pager) Flush() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.flushAll()
}

// flushAll is called under p.mu.
func (p *Pager) flushAll() error {
	if p.file != nil {
		for pageNum := range p.dirty {
			if err := p.flushPage(pageNum); err != nil {
				return err
			}
		}
		// Own writes just hit the file: refresh the external-change baseline
		// (pager.c readDbPage restores Pager.dbFileVers from page 1) so the
		// next per-statement check does not mistake them for external changes.
		p.refreshKnownFileStamp()
	}
	// Clear the dirty set in all cases (an in-memory pager has no file to
	// write, but COMMIT/autocommit must still release the "exclusive" lock
	// state that lock_status reports from HasDirtyPages).
	p.dirty = make(map[uint32]bool)
	return nil
}

// flushPage writes one dirty page to the file, truncating the file first when
// the page extends past the current end (a newly allocated page). The file
// size is cached (p.fileSize) so a page write does not need an Fstat syscall
// per page; the cache is updated whenever the file grows or is truncated.
func (p *Pager) flushPage(pageNum uint32) error {
	pg, ok := p.pages[pageNum]
	if !ok {
		return nil
	}
	off := int64(pageNum-1) * int64(p.pageSize)
	fileEnd := int64(pageNum) * int64(p.pageSize)
	if p.fileSize < fileEnd {
		if err := p.file.Truncate(fileEnd); err != nil {
			return fmt.Errorf("pager: truncate: %w", err)
		}
		p.fileSize = fileEnd
	}
	if _, err := p.file.WriteAt(pg.Data, off); err != nil {
		return fmt.Errorf("pager: write page %d: %w", pageNum, err)
	}
	return nil
}

func (p *Pager) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.file != nil {
		return p.file.Sync()
	}
	return nil
}

// HasDirtyPages reports whether the pager has unflushed dirty pages. The
// engine uses it at COMMIT to decide whether the transaction wrote data (and
// therefore whether the file change counter should be bumped).
func (p *Pager) HasDirtyPages() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.dirty) > 0
}

// FileChangeCounter reads the database file's change counter (header offset
// 24) directly from the file, bypassing the page cache (so commits by other
// connections are observed even before a cache invalidation). It reports
// whether a counter is available (false for in-memory pagers).
func (p *Pager) FileChangeCounter() (uint32, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.file == nil {
		// In-memory pager: fall back to the cached header.
		if len(p.header) < 28 {
			return 0, false
		}
		return binary.BigEndian.Uint32(p.header[24:28]), true
	}
	var buf [4]byte
	if _, err := p.file.ReadAt(buf[:], 24); err != nil {
		return 0, false
	}
	return binary.BigEndian.Uint32(buf[:]), true
}
