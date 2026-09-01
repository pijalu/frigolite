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
	"strings"
	"sync"
	"time"

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
		// headerCorrupt records that Open was given a file whose 100-byte header
		// did not parse (bad magic, short header, etc.). SQLite defers header
		// errors to the first statement; frigolite mirrors that so tests like
		// corrupt2-1.2 can run their expected error-producing query against the
		// open connection rather than seeing Open itself fail. The flag is
		// read by the schema-init path which produces "file is not a database"
		// on the first SELECT * FROM sqlite_master.
		headerCorrupt bool
	// path is the canonical database file path (filepath.Clean'd), used to
	// derive the "-wal"/"-shm" companion files in WAL mode.
	path string
	// journalMode is the active journal mode ("" or "delete" = legacy
	// rollback-journal path; "wal" = WAL write path). Only "wal" routes
	// commits through the WAL writer; the default path is untouched.
	journalMode string
	// journalSizeLimit is the per-database cap (bytes) applied to a PERSIST
	// journal file after a successful commit (PRAGMA journal_size_limit). A
	// negative value means unlimited (the journal keeps its full content); 0
	// truncates the journal to zero; a positive value truncates it down to that
	// many bytes. SQLite's default is 32768.
	journalSizeLimit int64
	// pendingJournalMode holds a journal-mode change requested while a
	// transaction was open; pager.c defers the switch until the transaction
	// ends (sqlite3BtreeSetJournalMode / btreeEndTransaction). Empty means no
	// pending change.
	pendingJournalMode string
	// freePages tracks pages returned to the freelist by FreePage (P8.INCRVACUUM
	// phase 1). The on-disk SQLite-format freelist (header.trunk + count) is
	// also updated, but AllocatePage uses freePages for fast O(1) pop without
	// having to read the freed page's chain pointer. The freed page's on-disk
	// content is left as-is (it was a valid b-tree leaf, and zeroing it would
	// break the b-tree reader's integrity-check walk).
	freePages map[uint32]bool
	// P8.INCRVACUUM.phase8: trunkPages / leafToTrunk are the in-memory
	// mirror of the on-disk freelist chain topology. They let AllocatePage
	// (in-memory branch) advance header.trunk when popping a trunk page and
	// zero a leaf slot when popping a leaf, so the on-disk chain stays in
	// sync with the in-memory freePages set. Without these, popping a leaf
	// leaves the trunk's leaves list referencing the popped page; the next
	// FreePage of the same page (after re-alloc + re-free) creates a
	// duplicate, which checkFreelistCount reports as "Page X: never used"
	// and btreeStructureOK as a cycle.
	trunkPages  map[uint32]bool
	leafToTrunk map[uint32]uint32
	// P8.INCRVACUUM.phase7: set by the exec engine at BEGIN, cleared at
	// COMMIT/ROLLBACK. While true, AllocatePage skips chain consumption
	// (the chain pages are not popped; the file is extended instead) so a
	// ROLLBACK does not produce "Page N: never used" orphans.
	inTransaction bool
	// Rollback-journal file machinery (P7.WAL-E — see journal.go).
	// journalFile is the open "test.db-journal" sidecar for the current
	// in-flight non-WAL transaction. Nil when no transaction is open or
	// when the mode is memory/off/wal.
	journalFile *os.File
	// journalSectorSize is the sector size used to size the journal header
	// (the header always occupies exactly one sector; 512 on most
	// platforms).
	journalSectorSize uint32
	// journalCksum1/2 are the random seeds the running-checksum chain
	// (over the journal records) starts from; written into the journal
	// header at open time.
	journalCksum1 uint32
	journalCksum2 uint32
	// journalDBOrigSize is the database's page count at journal-open
	// time; written into the journal header for recovery to detect a
	// stale journal (a different dbOrigSize means the journal belongs to
	// a different database file).
	journalDBOrigSize uint32
	// journalRecC1/C2 are the running checksum state of the records
	// appended so far; initialised from journalCksum1/2 after the
	// header is written, advanced by journalChecksumUpdate on every
	// appended record.
	journalRecC1 uint32
	journalRecC2 uint32
	// wal is non-nil while the pager is in WAL mode.
	wal *walWriter
	// walHook is the sqlite3_wal_hook callback, fired after each WAL commit.
	walHook func(nLog, nCkpt int) int
	// walFault injects I/O faults into WAL writes when non-nil (mirrors
	// SQLite's test_syscall faultsim). The writer calls it before each write;
	// a non-nil return aborts the write with that error. Nil in production.
	walFault func(op string) error
	// journalFileOpHook fires for xOpen/xClose/xDelete events on the
	// "test.db-journal" sidecar (testvfs equivalent). Used by the journal2
	// TCL test suite, which asserts on the OS-level sequence of file
	// operations on the journal sidecar; frigolite does not have a full
	// VFS plugin system, so this hook is the narrow path through which
	// those events are observable. Nil in production (no overhead).
	journalFileOpHook func(op, path string)
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
	pages     map[uint32]*Page
	dirty     map[uint32]bool
	numPages  uint32
	header    []byte
	freePages map[uint32]bool
	fileSize  int64
}

// Snapshot captures the pager's current in-memory pages and header so they
// can be restored later with Restore.
func (p *Pager) Snapshot() *PagerState {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Load every on-disk page into the cache before snapshotting. Without
	// this, pages that are not in the cache at BEGIN are missing from the
	// snapshot. After Restore, those pages are still missing from p.pages,
	// so the next read fetches them from disk — which holds the
	// transaction's modified state (rebalance, vacuum, etc.), not the
	// BEGIN state. PRAGMA integrity_check after ROLLBACK then walks a
	// half-restored btree and reports "database disk image is malformed".
	// The cost is O(numPages) per BEGIN, which is acceptable for the
	// small databases used in the testgen suites and matches SQLite's
	// pager semantics where the cache is warmed by the first read of
	// every page during the transaction.
	if p.file != nil {
		for n := uint32(2); n <= p.numPages; n++ {
			if _, ok := p.pages[n]; !ok {
				_, _ = p.readPageLocked(n)
			}
		}
	}
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
	if len(p.freePages) > 0 {
		s.freePages = make(map[uint32]bool, len(p.freePages))
		for n := range p.freePages {
			s.freePages[n] = true
		}
	}
	s.fileSize = p.fileSize
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
	// Restore the freelist so pages freed during the transaction are
	// re-marked as free, and pages allocated from the freelist during the
	// transaction are removed from the free set. Without this, a ROLLBACK
	// after a DELETE+rebalance (which calls pager.FreePage) leaves the
	// freed pages on the in-memory freelist while the btree still
	// references them via the parent cell, and PRAGMA integrity_check
	// reports "database disk image is malformed".
	if s.freePages != nil {
		p.freePages = make(map[uint32]bool, len(s.freePages))
		for n := range s.freePages {
			p.freePages[n] = true
		}
	} else {
		p.freePages = nil
	}
	// Restore the file size so pages that were appended during the
	// transaction (AllocatePage grew the file) are removed from the
	// integrity-check scan. Without this, the file still has the
	// post-transaction size while the in-memory pages map holds the
	// BEGIN state, and PRAGMA integrity_check sees the appended pages
	// as "never used" (neither on the freelist nor referenced by the
	// btree). Truncate the file to the BEGIN size to match.
	if s.fileSize > 0 && p.file != nil && p.fileSize > s.fileSize {
		if err := p.file.Truncate(s.fileSize); err != nil {
			// Best-effort: if truncate fails, continue and let the
			// integrity check report the mismatch.
			_ = err
		} else {
			p.fileSize = s.fileSize
		}
	} else {
		_ = s.fileSize
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
		path:        cleanPath,
		// SQLite's default PRAGMA journal_size_limit cap is 32768 bytes
		// (pragma.c journalSizeLimit). A PERSIST journal is truncated to this
		// many bytes after a commit; negative means unlimited, 0 means zero.
		journalSizeLimit: 32768,
	}

	if info.Size() > 0 {
			// Read the 100-byte header first: it contains the real page size.
			headerBuf := make([]byte, HeaderSize)
			n, err := f.ReadAt(headerBuf, 0)
			if err != nil && n < HeaderSize {
			// Short read (file smaller than the 100-byte header) — SQLite
			// does not fail on Open; it defers to the first statement that
			// touches the page (which then reports "file is not a database"
			// via btreeOpenTableCursor). Mirror that: keep the Pager open,
			// default pageSize to DefaultPageSize, and let the schema-init
			// path report the error. (corrupt2.test 1.2/1.3/1.5 and
			// incrvacuum.test-14.1 depend on this deferral.)
			pr.pageSize = DefaultPageSize
			pr.headerCorrupt = true
			} else if err != nil {
			f.Close()
			return nil, fmt.Errorf("pager: read header: %w", err)
			} else if hdr, perr := storage.ParseHeader(headerBuf); perr != nil {
			// SQLite's sqlite3PagerOpen does NOT fail on bad header parse: the
			// error is surfaced by the first statement that touches the page
			// (sqlite_master scan reports "file is not a database" via
			// btreeOpenTableCursor's locked-table flag and schema init's
			// SQLITE_NOTADB error path). Mirroring that: keep the Pager
			// open, default pageSize to DefaultPageSize, and let subsequent
			// reads detect the corruption. corrupt2.test 1.2/1.3/1.5
			// (corrupt2-1.2 expects `file is not a database` on the FIRST
			// statement, not on Open) and many other crash-recovery tests
			// require this deferral.
			pr.pageSize = DefaultPageSize
			pr.headerCorrupt = true
			} else {
			pr.pageSize = hdr.PageSize
			// Header byte 20: bytes reserved at the end of every page (used by
			// e.g. codec/checksum extensions). Payload distribution math must use
			// the USABLE size (pageSize - reserved), not the raw page size —
			// SQLite files written with reserved > 0 are otherwise unreadable.
			pr.reserved = uint32(hdr.ReservedSpace)
			}
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

	// WAL crash recovery / WAL-mode detection: SQLite auto-detects WAL from the
	// presence of a valid "-wal" file. When one accompanies the main database,
	// recover its committed frames into the page cache before the first read
	// (wal.c walIndexRecover on open) and place the connection in WAL mode so
	// the WAL write path and WAL-aware header validation are active. A WAL
	// database whose main file is still empty (uncheckpointed) carries its page
	// size only in the "-wal" header, so prefer that when the main file did not
	// yield a size.
	if _, err := os.Stat(cleanPath + "-wal"); err == nil {
		if pr.pageSize == 0 {
			if wps, ok := readWalPageSize(cleanPath + "-wal"); ok {
				pr.pageSize = wps
			}
		}
		if err := recoverWal(pr, cleanPath, pr.pageSize); err != nil {
			f.Close()
			return nil, err
		}
		if w, werr := openWal(pr, cleanPath, pr.pageSize); werr == nil {
			pr.wal = w
			pr.journalMode = "wal"
		}
	}

	return pr, nil
}

// readWalPageSize returns the page size recorded in a "-wal" file's header, if
// the header is present and valid (used to size a WAL database whose main file
// is still empty).
func readWalPageSize(walPath string) (uint32, bool) {
	f, err := os.Open(walPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	buf := make([]byte, WalHdrSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return 0, false
	}
	h, err := DecodeWalHeader(buf)
	if err != nil || !h.HeaderCksumOK {
		return 0, false
	}
	if h.PageSize == 0 {
		return 0, false
	}
	return h.PageSize, true
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

// deriveCksumInit produces a non-zero random-ish uint32 used as the
// rollback-journal header's cksumInit seed (pager.c uses
// sqlite3_randomness for this). It is not a security boundary; it only
// needs to be (a) different across connections to the same file, and
// (b) non-zero so a torn/zero-padded journal is detectable on recovery.
//
// We derive it from the journal file's current time + the file's path
// (so concurrent pagers with different db files get different seeds).
// The exact algorithm is not part of the SQLite wire format — the
// header only carries the seed verbatim; the journal-recovery code
// re-reads the seed and re-checksums the records.
func (p *Pager) deriveCksumInit() uint32 {
	var s uint32
	// Mix in the path bytes (different per database file).
	for i := 0; i < len(p.path); i++ {
		s = s*16777619 + uint32(p.path[i])
	}
	// Mix in the current file size (different per write).
	s ^= uint32(p.fileSize)
	// Mix in a high-resolution timestamp (different per call).
	now := time.Now().UnixNano()
	s ^= uint32(now)
	s ^= uint32(now >> 32)
	// Force non-zero: a zero seed is the canonical "no journal" marker.
	if s == 0 {
		s = 0xa5a5a5a5
	}
	return s
}

func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.flushAll(); err != nil {
		return err
	}
	if p.journalFile != nil {
		// Close the open rollback-journal sidecar (PERSIST/TRUNCATE
		// modes keep it open across commits; Close is the only path
		// that releases the FD). Fire xClose via the hook so the
		// journal2 test sees a balanced sequence when the connection
		// ends.
		jpath := p.journalFile.Name()
		_ = p.journalFile.Close()
		p.journalFile = nil
		if h := p.journalFileOpHookFn(); h != nil {
			h("xClose", jpath)
		}
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
	// If Open observed a header that did not parse (bad magic / short header),
	// mirror SQLite's deferral: surface "file is not a database" on the first
	// statement that actually reads the schema btree. corrupt2.test 1.2/1.3/1.5
		// rely on this (Open succeeds, the next SELECT * FROM sqlite_master
		// returns the error).
		if p.headerCorrupt {
			return fmt.Errorf("database disk image is malformed")
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
func (p *Pager) Pages() map[uint32]*Page { p.mu.RLock(); defer p.mu.RUnlock(); return p.pages }
func (p *Pager) Header() []byte   { p.mu.RLock(); defer p.mu.RUnlock(); return p.header }

// IsHeaderCorrupt reports whether Open observed a 100-byte header that did
// not parse (bad magic, short header, etc.). The schema-init path reads
// this to surface "file is not a database" on the first SELECT * FROM
// sqlite_master rather than failing at Open time.
func (p *Pager) IsHeaderCorrupt() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.headerCorrupt
}

// SetAutoVacuum toggles pointer-map page reservation for subsequent
// AllocatePage calls (btree.c sqlite3BtreeSetAutoVacuum). SQLite applies a
// mode change immediately only while the database is still empty; callers
// are responsible for that check.
func (p *Pager) SetAutoVacuum(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.autoVacuum = on
}

// SetInTransaction toggles the transaction flag (P8.INCRVACUUM.phase7).
// While true, AllocatePage skips chain consumption (extending the file
// instead) so that a ROLLBACK does not leave popped pages without an
// owner ("Page N: never used" orphans). The exec engine calls this at
// BEGIN (true) and at COMMIT/ROLLBACK (false).
func (p *Pager) SetInTransaction(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inTransaction = on
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

// IsPtrmapPageNo reports whether pgno itself is a pointer-map page.
func IsPtrmapPageNo(pgno, pageSize uint32) bool {
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
	// P8.INCRVACUUM.phase7: inside an active transaction, do not
	// consume freelist pages (the in-memory branch and the on-disk
	// branch both pop from p.freePages and decrements header.count).
	// Popping a chain page inside a transaction would create
	// "Page N: never used" orphans on ROLLBACK: the btree state
	// rolls back so it no longer references the popped page, but
	// the chain no longer lists it either, leaving the page with
	// no owner. Skipping the chain and extending the file is safe
	// — the extra pages are simply wasted at ROLLBACK, and the
	// pending vacuum step (IncrVacuumStep) on COMMIT drains the
	// freelist in a single file-shrink pass.
	if !p.inTransaction {
		// P8.INCRVACUUM phase 1: pop from in-memory freePages set first
		// (O(1), avoids reading the freed page's chain pointer). The
		// header's count is decremented to keep the on-disk SQLite format
		// consistent. The on-disk chain is also updated so the popped
		// page is no longer referenced by any trunk's leaves list (which
		// is what the P8.INCRVACUUM.phase8 trunkPages / leafToTrunk
		// maps track).
		if len(p.freePages) > 0 {
			var trunk uint32
			for pg := range p.freePages {
				trunk = pg
				break
			}
			delete(p.freePages, trunk)
			// P8.INCRVACUUM.phase8: maintain the on-disk chain.
			// If `trunk` is itself a freelist trunk (it was the
			// page holding Data[0:4] = nextTrunk, Data[4:8] =
			// leafCount), advance header.trunk to its nextTrunk.
			// If it's a leaf of some trunk, zero the slot in
			// that trunk's leaves list and decrement the trunk's
			// leafCount. If it's neither (orphan), just
			// decrement header.count.
			if p.header != nil && len(p.header) >= 40 {
				if p.trunkPages[trunk] {
					// Trunk pop: read nextTrunk from the
					// cached page data BEFORE we overwrite it.
					// The page is in p.pages because FreePage
					// read it (line 1409) before adding to
					// p.trunkPages. If it's not in p.pages
					// (was evicted), re-read it.
					trunkPg, terr := p.readPageLocked(trunk)
					if terr == nil && len(trunkPg.Data) >= 4 {
						nextTrunk := binary.BigEndian.Uint32(trunkPg.Data[0:4])
						binary.BigEndian.PutUint32(p.header[32:36], nextTrunk)
						p.dirty[1] = true
					}
					delete(p.trunkPages, trunk)
				} else if trunkT, ok := p.leafToTrunk[trunk]; ok {
					// Leaf pop: find the leaf slot in the
					// trunk's data and zero it. The trunk is
					// still in p.pages from the FreePage that
					// added this leaf; if it was evicted,
					// re-read it.
					trunkPg, terr := p.readPageLocked(trunkT)
					if terr == nil && len(trunkPg.Data) >= 8 {
						lc := binary.BigEndian.Uint32(trunkPg.Data[4:8])
						// Walk the leaves list to find
						// the slot for `trunk`. O(lc) per
						// pop; matches btree.c
						// allocateBtreePage's slot scan
						// (lines 6680-6700).
						for i := uint32(0); i < lc; i++ {
							off := 8 + i*4
							if int(off)+4 > len(trunkPg.Data) {
								break
							}
							leaf := binary.BigEndian.Uint32(trunkPg.Data[off : off+4])
							if leaf == trunk {
								// Found it:
								// shift the last
								// slot into this
								// one (btree.c
								// lines 6697-6700)
								// and decrement
								// leafCount.
								if i < lc-1 {
									copy(trunkPg.Data[off:off+4], trunkPg.Data[off+4:off+8])
								}
								// Zero the
								// now-unused
								// last slot.
								lastOff := 8 + (lc-1)*4
								if int(lastOff)+4 <= len(trunkPg.Data) {
									binary.BigEndian.PutUint32(trunkPg.Data[lastOff:lastOff+4], 0)
								}
								binary.BigEndian.PutUint32(trunkPg.Data[4:8], lc-1)
								p.dirty[trunkT] = true
								// ROLLBACK fidelity: journal
								// the trunk's BEFORE
								// image so ROLLBACK
								// can restore the
								// leaf slot and count.
								if p.journalFile != nil {
									trunkOff := int64(trunkT-1) * int64(p.pageSize)
									trunkBefore := make([]byte, p.pageSize)
									if _, err := p.file.ReadAt(trunkBefore, trunkOff); err == nil {
										if err := p.appendRollbackRecordLocked(trunkT, trunkBefore); err != nil {
											return nil
										}
									}
								}
								break
							}
						}
					}
					delete(p.leafToTrunk, trunk)
				}
				// Decrement header count.
				count := binary.BigEndian.Uint32(p.header[36:40])
				if count > 0 {
					binary.BigEndian.PutUint32(p.header[36:40], count-1)
				}
			}
			p.dirty[1] = true
			pg := &Page{Data: make([]byte, p.pageSize), PageNum: trunk}
			p.pages[trunk] = pg
			p.dirty[trunk] = true
			return pg
		}
		// If the on-disk freelist has pages (header byte 32..36 = trunk, 36..40 =
		// count), recycle one instead of extending the file. SQLite's
		// allocateBtreePage branch in btree.c consumes freelist pages before
		// extending the database. Without this, DELETE/DROP never recycle pages
		// (vacuum.go documents that), and PRAGMA integrity_check can't see a
		// meaningful freelist count. corrupt2-14.2/14.3/14.5 depend on the
		// freelist count surviving a DELETE and being mismatched by a hex patch.
		if p.header != nil && len(p.header) >= 40 {
			trunk := binary.BigEndian.Uint32(p.header[32:36])
			count := binary.BigEndian.Uint32(p.header[36:40])
			if trunk != 0 && count > 0 && trunk <= p.numPages {
				trunkPg, terr := p.readPageLocked(trunk)
				if terr == nil && len(trunkPg.Data) >= 8 {
					// The trunk page is a freelist trunk: first 4 bytes = next trunk
					// (or 0 if last), bytes 4..6 = leaf count, bytes 8+ = leaves. Pop
					// the trunk itself (subtract 1 from count) and clear the leaf
					// pages too so checkFreelistCount doesn't count them as still
					// reachable. (The leaf pages were the original overflow pages of
					// deleted cells — they are no longer valid freelist entries once
					// the trunk itself is consumed.)
					nextTrunk := binary.BigEndian.Uint32(trunkPg.Data[0:4])
					// SQLite freelist trunk format (btree.c:10701): leaf count is
					// a 4-byte big-endian integer at offset 4, NOT 2 bytes. Reading
					// only 2 bytes also pulls bytes 6..7 which are the high two bytes
					// of the first leaf page number, yielding a garbage count that
					// confuses the integrity_check walker. pragma_quickcheck.go reads
					// 4 bytes for the same reason.
					leafCount := binary.BigEndian.Uint32(trunkPg.Data[4:8])
					clearedLeaves := uint32(0)
					for i := 0; i < int(leafCount); i++ {
						off := 8 + i*4
						if off+4 > len(trunkPg.Data) {
							break
						}
						leafPg := binary.BigEndian.Uint32(trunkPg.Data[off : off+4])
						if leafPg != 0 && leafPg <= p.numPages {
							if lp, lerr := p.readPageLocked(leafPg); lerr == nil {
								for j := range lp.Data {
									lp.Data[j] = 0
								}
								p.dirty[leafPg] = true
								clearedLeaves++
								// P8.INCRVACUUM.phase8: the leaf
								// is being consumed by the on-disk
								// branch. Remove from leafToTrunk
								// (the entry pointing to this
								// trunk).
								if p.leafToTrunk != nil {
									if existingT, ok := p.leafToTrunk[leafPg]; ok && existingT == trunk {
										delete(p.leafToTrunk, leafPg)
									}
								}
							}
						}
					}
					binary.BigEndian.PutUint32(p.header[32:36], nextTrunk)
					// P8.INCRVACUUM.phase8: the trunk itself is
					// being consumed; remove from trunkPages.
					// The chain's new head is nextTrunk; if it's
					// non-zero, it must also be in trunkPages
					// (it was already there from the FreePage
					// that chained it).
					if p.trunkPages != nil {
						delete(p.trunkPages, trunk)
					}
					// Decrement count by 1 (for the trunk) plus the number of leaves
					// we cleared (each leaf was a free page in the chain). Without
					// this, after AllocatePage consumes a trunk with leaves, the
					// header's count stays high while checkFreelistCount no longer
					// sees those pages (we zeroed them), so integrity_check reports
					// a "size is N but should be M" mismatch (corrupt2-14.5).
					totalFreed := uint32(1) + clearedLeaves
					if totalFreed > count {
						totalFreed = count
					}
					binary.BigEndian.PutUint32(p.header[36:40], count-totalFreed)
					p.dirty[trunk] = true
					delete(p.pages, trunk)
					pg := &Page{
						Data:    make([]byte, p.pageSize),
						PageNum: trunk,
					}
					p.pages[trunk] = pg
					p.dirty[trunk] = true
					return pg
				}
			}
		}
	}
	p.numPages++
	// btree.c allocateBtreePage (auto-vacuum branch): when the next page is
	// a pointer-map page, zero it out (no b-tree header — its content is a
	// flat array of 5-byte entries maintained by ptrmapPut, unused until
	// pages are relocated by vacuuming) and extend the file once more so the
	// caller gets a normal page.
	if p.autoVacuum && IsPtrmapPageNo(p.numPages, p.pageSize) {
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
	return p.readPageLocked(pageNum)
}

// readPageLocked reads a page; the caller must hold p.mu for writing.
func (p *Pager) readPageLocked(pageNum uint32) (*Page, error) {
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
		if pg.Data[0] == 0 && pg.Data[1] == 0 && pg.Data[2] == 0 && pg.Data[3] == 0 {
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
// the database file (schema reload after an ATTACHed file changes). In WAL
// mode the cache is then rebuilt from the "-wal" (wal.c reads pages through
// the WAL index), so a schema reload never loses uncheckpointed commits.
func (p *Pager) InvalidateCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = make(map[uint32]*Page)
	p.header = nil
	if p.wal != nil {
		// Rebuild the cache from the WAL's committed frames; this restores
		// numPages/header that the stale main file no longer reflects. The
		// caller holds p.mu, so use the lock-free variant.
		_ = recoverWalLocked(p, p.path, p.pageSize)
		return
	}
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

// WritePage marks a page as dirty. The first write under a non-memory/non-off
// transaction also opens the rollback journal sidecar (test.db-journal) so
// the BEFORE image of every subsequently-dirtied page is recorded; a ROLLBACK
// (or a fault during COMMIT) replays the journal to restore the original
// pages (pager.c sqlite3PagerWrite — the journal is opened on the first
// write of a transaction, not deferred to COMMIT). For autocommit writes the
// journal is opened and finalised in the same flush cycle (pager.c
// pager_end_transaction).
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
	// Open the rollback journal eagerly on the first write so a ROLLBACK
	// before COMMIT can replay the BEFORE images. openRollbackJournalLocked
	// is a no-op for memory/off/wal modes and for pagers without a file.
	if err := p.openRollbackJournalLocked(); err != nil {
		return err
	}
	// Record the BEFORE image of this page (on disk) into the open journal
	// (only for the FIRST write — subsequent writes during the same
	// transaction overwrite the BEFORE image; SQLite's journal only stores
	// the most-recent before-image per page, and the journal's cksum
	// chain covers the latest write).
	if p.journalFile != nil {
		off := int64(pg.PageNum-1) * int64(p.pageSize)
		before := make([]byte, p.pageSize)
		if _, err := p.file.ReadAt(before, off); err == nil {
			if err := p.appendRollbackRecordLocked(pg.PageNum, before); err != nil {
				return err
			}
		}
	}
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
	// Update the on-disk freelist count: pages being truncated that
	// are on the freelist (in p.freePages) reduce the header's freelist
	// count by one each. The pager is the only writer of header.count;
	// the integrity check (checkFreelistCount) compares this count
	// against the actual chain length.
	truncatedFree := uint32(0)
	for pgno := p.numPages; pgno > n; pgno-- {
		if p.freePages[pgno] {
			truncatedFree++
			// Drop truncated free pages from p.freePages too — the page
			// no longer exists, so it must not be popped by AllocatePage.
			delete(p.freePages, pgno)
		}
	}
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
		newSize := int64(n) * int64(p.pageSize)
		if err := p.file.Truncate(newSize); err != nil {
			return fmt.Errorf("pager: truncate to %d pages: %w", n, err)
		}
		// Mirror the file size in the cache so FilePageCount() reflects
		// the post-truncate size (P8.INCRVACUUM phase 4: integrity_check
		// otherwise sees the pre-truncate size and reports "Page N: never
		// used" for pages that no longer exist on disk).
		p.fileSize = newSize
	}
	// Adjust the on-disk freelist count for the truncated free pages.
	if truncatedFree > 0 && p.header != nil && len(p.header) >= 40 {
		oldCount := binary.BigEndian.Uint32(p.header[36:40])
		if oldCount >= truncatedFree {
			binary.BigEndian.PutUint32(p.header[36:40], oldCount-truncatedFree)
			p.dirty[1] = true
		}
		// If the trunk was one of the truncated pages, advance the trunk
		// to the next free page still in the file. Without this, the
		// isFreelistPage walker (src/btree.c checkTree) starts at a
		// page that no longer exists and reports every remaining free
		// page as "Page N: never used" → integrity_check fails.
		trunk := binary.BigEndian.Uint32(p.header[32:36])
		if trunk > n {
			// Find the highest free page that survived the truncate.
			var newTrunk uint32
			for pgno := n; pgno >= 2; pgno-- {
				if p.freePages[pgno] {
					newTrunk = pgno
					break
				}
			}
			binary.BigEndian.PutUint32(p.header[32:36], newTrunk)
			p.dirty[1] = true
			// Break the chain at the new trunk: set its first 4 bytes
			// to the next free page below it (or 0 if newTrunk is the
			// lowest free page). The original FreePage chain was a
			// monotonic sequence (e.g. 8→7→6→5→4) so the next page
			// below newTrunk is the one that was originally chained to
			// newTrunk.
			if newTrunk > 0 {
				if pg, ok := p.pages[newTrunk]; ok && pg != nil {
					var nextFree uint32
					for pgno := newTrunk - 1; pgno >= 2; pgno-- {
						if p.freePages[pgno] {
							nextFree = pgno
							break
						}
					}
					binary.BigEndian.PutUint32(pg.Data[0:4], nextFree)
					p.dirty[newTrunk] = true
				}
			}
		}
	}
	// Update the in-header database size (offset 28) so the next
	// HeaderBeyondFile check (src/btree.c lockBtree) reports the new
	// file page count instead of the pre-truncate size. SQLite sets
	// this every time the file shrinks, otherwise the in-header count
	// would exceed the file's actual page count and every subsequent
	// statement would fail with "database disk image is malformed".
	if p.header != nil && len(p.header) >= 32 {
		binary.BigEndian.PutUint32(p.header[28:32], n)
		p.dirty[1] = true
	}
	// Mirror the updated header into the cached page 1 so the next
	// flush writes the new header bytes (FreePage/Truncate only update
	// p.header; the page cache holds a separate copy of pg.Data).
	if p.header != nil {
		if pg, ok := p.pages[1]; ok && pg != nil {
			copy(pg.Data[:HeaderSize], p.header)
		}
	}
	// P8.INCRVACUUM phase 5 fix: the file was just truncated but the
	// on-disk header still has the pre-truncate size at offset 28. The
	// next statement's execDBFileChecks calls HeaderBeyondFile, which
	// reads the on-disk header and compares its nPage against the file's
	// page count. Without this write, the file is now N pages but the
	// header says N+1 (or more), and every subsequent statement fails
	// with "database disk image is malformed". Write the updated header
	// directly to offset 0 so the on-disk header matches the truncated
	// file size before the next read. The trunk page's chain pointer
	// (if updated above) is also flushed so the freelist walker
	// (checkFreelistCount / isFreelistPage) sees a consistent chain.
	if p.file != nil && p.header != nil && len(p.header) >= HeaderSize {
		if _, err := p.file.WriteAt(p.header[:HeaderSize], 0); err != nil {
			return fmt.Errorf("pager: truncate: write header: %w", err)
		}
		// Flush the trunk page's updated chain pointer (if any) so the
		// freelist chain is consistent on disk.
		trunk := binary.BigEndian.Uint32(p.header[32:36])
		if trunk > 0 {
			if pg, ok := p.pages[trunk]; ok && pg != nil {
				off := int64(trunk-1) * int64(p.pageSize)
				if _, err := p.file.WriteAt(pg.Data, off); err != nil {
					return fmt.Errorf("pager: truncate: write trunk page %d: %w", trunk, err)
				}
			}
		}
		// Refresh the known file stamp so CheckExternalFile doesn't
		// think the file changed externally and invalidate our cache.
		vers, size, _ := p.readFileStamp()
		p.knownFileVers = vers
		p.knownFileSize = size
	}
	// P8.INCRVACUUM phase 5 multi-trunk fix: after truncating the file
	// to n pages, walk the on-disk freelist chain and prune any trunk or
	// leaf that references a page > n. Without this, integrity_check
	// reports "Freelist: size is N but should be M" because the chain
	// walk sees pages that no longer exist on disk. We also fix up
	// the next-trunk pointers so the chain is still well-formed
	// (no dangling references to truncated pages).
	if err := p.pruneFreelistChain(n); err != nil {
		return fmt.Errorf("pager: truncate: prune chain: %w", err)
	}
	return nil
}

// pruneFreelistChain walks the on-disk freelist chain starting at
// header.trunk and drops any trunk or leaf page that is past the file
// size `n`. The remaining chain is rewired (next-trunk pointers of the
// kept trunks) so the chain is well-formed. This is the counterpart of
// the multi-trunk FreePage fix: FreePage can build chains with pages in
// arbitrary order, and Truncate needs to keep the chain consistent when
// the file shrinks.
//
// Returns nil if no chain exists (header.trunk == 0) or if all entries
// survive. If all trunks are dropped, header.trunk and header.count are
// both zeroed.
func (p *Pager) pruneFreelistChain(n uint32) error {
	if p.header == nil || len(p.header) < 40 {
		return nil
	}
	// Pass 1: walk the chain, collect surviving (trunk, [leaves]) pairs.
	type trunkRec struct {
		pgno       uint32
		leaves     []uint32
		nextTrunk  uint32
	}
	var survivors []trunkRec
	trunk := binary.BigEndian.Uint32(p.header[32:36])
	const maxIter = 100000
	for iter := 0; trunk != 0 && iter < maxIter; iter++ {
		if trunk > n {
			// Trunk itself is past the truncation point; skip and
			// follow its next-trunk pointer (the read may be
			// impossible since the page no longer exists on disk,
			// but if the page is still in the cache, we can read it).
			if pg, ok := p.pages[trunk]; ok && pg != nil && len(pg.Data) >= 8 {
				trunk = binary.BigEndian.Uint32(pg.Data[0:4])
			} else {
				// Page is gone and not in cache; bail.
				break
			}
			continue
		}
		pg, err := p.readPageLocked(trunk)
		if err != nil || len(pg.Data) < 8 {
			break
		}
		coff := 0
		if trunk == 1 {
			coff = 100
		}
		next := binary.BigEndian.Uint32(pg.Data[coff : coff+4])
		lc := binary.BigEndian.Uint32(pg.Data[coff+4 : coff+8])
		// Filter leaves: keep only those <= n. SQLite's truncate
		// leaves the rest of the leaf array alone (count is already
		// decremented by the caller); we follow the same convention
		// and zero out removed leaves while keeping the array length.
		var keptLeaves []uint32
		newLC := uint32(0)
		for i := uint32(0); i < lc; i++ {
			off := coff + 8 + int(i)*4
			if off+4 > len(pg.Data) {
				break
			}
			leaf := binary.BigEndian.Uint32(pg.Data[off : off+4])
			if leaf != 0 && leaf <= n {
				keptLeaves = append(keptLeaves, leaf)
				newLC++
			} else if leaf > n {
				// Mark this slot as free (zero).
				binary.BigEndian.PutUint32(pg.Data[off:off+4], 0)
			}
		}
		// Update the trunk's leaf count to match survivors.
		if newLC != lc {
			binary.BigEndian.PutUint32(pg.Data[coff+4:coff+8], newLC)
		}
		// If a leaf was zeroed, mark the trunk dirty so the next
		// flush writes the updated array.
		if newLC < lc {
			p.dirty[trunk] = true
		}
		if newLC > 0 {
			survivors = append(survivors, trunkRec{pgno: trunk, leaves: keptLeaves, nextTrunk: next})
		}
		trunk = next
	}
	// Pass 2: rewire the chain. For each survivor in order, set its
	// next-trunk pointer to the NEXT survivor's trunk page (or 0 if
	// last). This drops the truncated-trunk entries and any
	// survivor-trunks that became empty after the leaf prune.
	if len(survivors) == 0 {
		// Whole chain is gone.
		binary.BigEndian.PutUint32(p.header[32:36], 0)
		binary.BigEndian.PutUint32(p.header[36:40], 0)
		p.dirty[1] = true
		if pg, ok := p.pages[1]; ok && pg != nil {
			copy(pg.Data[:HeaderSize], p.header)
		}
		return nil
	}
	for i := range survivors {
		var nextTrunk uint32
		if i+1 < len(survivors) {
			nextTrunk = survivors[i+1].pgno
		}
		if survivors[i].nextTrunk != nextTrunk {
			pg, err := p.readPageLocked(survivors[i].pgno)
			if err == nil && len(pg.Data) >= 4 {
				coff := 0
				if survivors[i].pgno == 1 {
					coff = 100
				}
				binary.BigEndian.PutUint32(pg.Data[coff:coff+4], nextTrunk)
				p.dirty[survivors[i].pgno] = true
			}
		}
	}
	// Update header.trunk to the first survivor.
	binary.BigEndian.PutUint32(p.header[32:36], survivors[0].pgno)
	// Recompute header.count from survivors' leaf counts + 1 per
	// survivor-trunk (the trunk itself is also on the freelist).
	newCount := uint32(len(survivors))
	for _, s := range survivors {
		newCount += uint32(len(s.leaves))
	}
	binary.BigEndian.PutUint32(p.header[36:40], newCount)
	p.dirty[1] = true
	if pg, ok := p.pages[1]; ok && pg != nil {
		copy(pg.Data[:HeaderSize], p.header)
	}
	return nil
}

// FreePage returns a page number to the on-disk freelist (P8.INCRVACUUM
// phase 1 + multi-trunk fix). The freed page's on-disk content is
// left as-is (it was a valid b-tree page before FreePage was called,
// and zeroing it would break the b-tree reader's integrity-check walk
// on a freed leaf). The freelist is tracked in two places:
//   - p.freePages: in-memory set for O(1) pop in AllocatePage.
//   - header.trunk + header.count: on-disk SQLite format (kept in sync
//     for corrupt2-14.x tests + integrity_check).
//
// The on-disk freelist chain mirrors SQLite btree.c::freePage2 (lines
// 6797-6930): if the current trunk's leafCount < (pageSize-8)/4 - 8,
// the freed page is added as a leaf of the current trunk; otherwise
// the freed page becomes a new trunk and the previous trunk is linked
// as its next-trunk. (The previous code always made the new page a
// 0-leaf trunk, which produced a chain of empty trunks after > 254
// frees and left the actual data pages as "Page X: never used" in
// integrity_check.)
func (p *Pager) FreePage(pageNum uint32) error {
	if pageNum <= 1 {
		return fmt.Errorf("pager: cannot free page %d", pageNum)
	}
	if p.readOnly {
		return fmt.Errorf("pager: read-only")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if pageNum > p.numPages {
		return nil
	}
	if p.header == nil || len(p.header) < 40 {
		// Build a minimal header so we can update the freelist fields.
		p.header = make([]byte, HeaderSize)
		copy(p.header, storage.DefaultHeader(p.pageSize).Encode())
	}
	// SQLite's btree.c::freePage calls sqlite3PagerWrite on page 1 and on
	// the freed page BEFORE modifying them, so the rollback journal
	// captures the BEFORE images. ROLLBACK then restores both the header
	// (freelist trunk/count) and the freed page's bytes. Without this,
	// ROLLBACK leaves the freed page on the freelist while the btree still
	// references it, and PRAGMA integrity_check fails. Mirror that
	// behavior here.
	if err := p.openRollbackJournalLocked(); err != nil {
		return err
	}
	// Journal the freed page's BEFORE image (its current first 4 bytes
	// may be the previous chain pointer; restore them on rollback).
	if pg, ok := p.pages[pageNum]; ok && pg != nil {
		if p.journalFile != nil {
			off := int64(pageNum-1) * int64(p.pageSize)
			before := make([]byte, p.pageSize)
			if _, err := p.file.ReadAt(before, off); err == nil {
				if err := p.appendRollbackRecordLocked(pageNum, before); err != nil {
					return err
				}
			}
		}
	}
	// Track the freed page in p.freePages (O(1) pop in AllocatePage).
	if p.freePages == nil {
		p.freePages = make(map[uint32]bool)
	}
	p.freePages[pageNum] = true
	// Update header for on-disk SQLite format compatibility.
	oldCount := binary.BigEndian.Uint32(p.header[36:40])
	binary.BigEndian.PutUint32(p.header[36:40], oldCount+1)
	// SQLite btree.c::freePage2 algorithm: if the current trunk has room
	// for another leaf, append the freed page as a leaf; otherwise make
	// the freed page a new trunk and chain the previous trunk as its
	// next-trunk. The (pageSize-8)/4 - 8 cap matches SQLite's
	// back-compat margin (newer SQLite versions reserve the last 6
	// leaf slots so older readers can still read newer files).
	oldTrunk := binary.BigEndian.Uint32(p.header[32:36])
	leafAdded := false
	if oldTrunk != 0 && oldTrunk != pageNum {
		trunkPg, terr := p.readPageLocked(oldTrunk)
		if terr == nil && len(trunkPg.Data) >= 8 {
			// SQLite freelist trunk format: bytes 0-3 = next-trunk
			// (4 bytes), bytes 4-7 = leaf count (4 bytes), bytes 8+
			// = leaf page numbers (4 bytes each).
			lc := binary.BigEndian.Uint32(trunkPg.Data[4:8])
			// Back-compat cap: (pageSize - 8) / 4 - 8.
			maxLeaves := int(p.pageSize)/4 - 10
			if int(lc) < maxLeaves {
				// Add this page as a leaf of the current trunk.
				off := 8 + int(lc)*4
				if off+4 <= len(trunkPg.Data) {
					binary.BigEndian.PutUint32(trunkPg.Data[off:off+4], pageNum)
					binary.BigEndian.PutUint32(trunkPg.Data[4:8], lc+1)
					p.dirty[oldTrunk] = true
					// P8.INCRVACUUM.phase8: track the leaf→trunk
					// mapping so AllocatePage's in-memory branch can
					// zero this leaf slot when the leaf is later
					// popped. Without this, popping a leaf leaves the
					// trunk's leaves list referencing the popped page,
					// and a subsequent FreePage of the same page (after
					// re-alloc + re-free) creates a duplicate, which
					// checkFreelistCount reports as "Page X: never
					// used".
					if p.leafToTrunk == nil {
						p.leafToTrunk = make(map[uint32]uint32)
					}
					p.leafToTrunk[pageNum] = oldTrunk
					// ROLLBACK fidelity: the trunk's BEFORE image is
					// needed too. Without it, ROLLBACK restores the
					// trunk to its prior leafCount but the freed page
					// is also restored — leaving an inconsistent state.
					if p.journalFile != nil {
						trunkOff := int64(oldTrunk-1) * int64(p.pageSize)
						trunkBefore := make([]byte, p.pageSize)
						if _, err := p.file.ReadAt(trunkBefore, trunkOff); err == nil {
							if err := p.appendRollbackRecordLocked(oldTrunk, trunkBefore); err != nil {
								return err
							}
						}
					}
					leafAdded = true
				}
			}
		}
	}
	if !leafAdded {
		// Either freelist was empty, or current trunk is full (or
		// readPageLocked failed): this page becomes the new trunk.
		// The previous trunk's leaf data is preserved (SQLite only
		// updates the new trunk's bytes 0-7).
		binary.BigEndian.PutUint32(p.header[32:36], pageNum)
		if pg, ok := p.pages[pageNum]; ok && pg != nil {
			binary.BigEndian.PutUint32(pg.Data[0:4], oldTrunk) // next_trunk = old trunk
			binary.BigEndian.PutUint32(pg.Data[4:8], 0)         // leaf count = 0
			p.dirty[pageNum] = true
		} else {
			// The page isn't in the cache (was dropped before being
			// freed). Re-read it from disk so the new-trunk bytes
			// are written back at flush time.
			pg2, err := p.readPageLocked(pageNum)
			if err == nil {
				binary.BigEndian.PutUint32(pg2.Data[0:4], oldTrunk)
				binary.BigEndian.PutUint32(pg2.Data[4:8], 0)
				p.dirty[pageNum] = true
			}
		}
		// P8.INCRVACUUM.phase8: mark the page as a trunk so
		// AllocatePage's in-memory branch can advance header.trunk
		// when this page is later popped. The previous trunk
		// (oldTrunk, if non-zero) is already in trunkPages from a
		// prior FreePage call; we leave it there because it remains
		// reachable via the chain's next_trunk pointer (AllocatePage
		// may still consume it on a future pop if the chain's
		// current trunk is consumed first).
		if p.trunkPages == nil {
			p.trunkPages = make(map[uint32]bool)
		}
		p.trunkPages[pageNum] = true
	}
	// Journal page 1's BEFORE image so ROLLBACK restores the header's
	// freelist trunk/count. The freed-page BEFORE image was already
	// journaled above. Without this, ROLLBACK leaves the header with
	// the new trunk/count but the freed page is still on the freelist
	// in the file — actually no, the freed page's BEFORE image restore
	// already handles that. The header restore is the missing piece:
	// ROLLBACK must also clear the freelist header fields, otherwise
	// the freelist count is inflated.
	if p.journalFile != nil {
		off := int64(0)
		before := make([]byte, p.pageSize)
		if _, err := p.file.ReadAt(before, off); err == nil {
			if err := p.appendRollbackRecordLocked(1, before); err != nil {
				return err
			}
		}
	}
	p.dirty[1] = true
	// Mirror the header bytes into the cached page 1 data so the next
	// flush writes the updated header (otherwise flushPage writes
	// pg.Data which is a separate copy from p.header).
	if pg, ok := p.pages[1]; ok && pg != nil {
		copy(pg.Data[:HeaderSize], p.header)
	}
	return nil
}

// AllocatePageLE is the page-swap target allocator (P8.INCRVACUUM phase 3).
// Like AllocatePage, it returns a free page from p.freePages, but it
// picks the LOWEST free page (for the page-swap step: the last page of
// the file is moved to a free page near the front, so we want a
// low-numbered free page to keep the file contiguous). If no free page
// is available, returns nil and an SQLITE_FULL error.
func (p *Pager) AllocatePageLE() (*Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.freePages) == 0 {
		return nil, fmt.Errorf("database or disk full")
	}
	var lowest uint32 = 0xFFFFFFFF
	for pg := range p.freePages {
		if pg < lowest {
			lowest = pg
		}
	}
	delete(p.freePages, lowest)
	// Decrement header count.
	if p.header != nil && len(p.header) >= 40 {
		count := binary.BigEndian.Uint32(p.header[36:40])
		if count > 0 {
			binary.BigEndian.PutUint32(p.header[36:40], count-1)
		}
	}
	p.dirty[1] = true
	pg := &Page{Data: make([]byte, p.pageSize), PageNum: lowest}
	p.pages[lowest] = pg
	p.dirty[lowest] = true
	return pg, nil
}

// Flush is the public flush entry point. See flushAll for the actual work.
func (p *Pager) Flush() error {
	return p.FlushWithContext(false)
}

// FlushWithContext flushes the pager. multiDB is true when this flush is
// part of a multi-database COMMIT (one or more ATTACH'd databases are
// committing together); it controls PERSIST-mode finalization (the
// super-journal path in pager.c forces the per-database journal file to
// 0 bytes when the commit is multi-DB).
func (p *Pager) FlushWithContext(multiDB bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.flushAllCtx(multiDB)
}

// SetJournalMode switches the pager's journal mode. "wal" enables the WAL
// write path: it creates the "-wal"/"-shm" companions, writes a WAL header,
// and routes future commits through the WAL writer (the main file is then
// only updated by an explicit Checkpoint). Any other mode (the default)
// keeps the legacy direct-flush path unchanged.
func (p *Pager) SetJournalMode(mode string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "wal":
		if p.wal != nil {
			return nil // already in WAL mode
		}
		if p.file == nil {
			return fmt.Errorf("pager: cannot enable WAL on in-memory pager")
		}
		// Switching from PERSIST/TRUNCATE to WAL: close and unlink the
		// existing rollback-journal file (it is no longer the active
		// sidecar). Fire xClose + xDelete via the testvfs hook.
		prev := p.journalMode
		if p.journalFile != nil && (prev == "persist" || prev == "truncate") {
			jpath := p.journalFile.Name()
			_ = p.journalFile.Close()
			p.journalFile = nil
			if h := p.journalFileOpHookFn(); h != nil {
				h("xClose", jpath)
			}
			if h := p.journalFileOpHookFn(); h != nil {
				h("xDelete", jpath)
			}
			_ = os.Remove(jpath)
		}
		w, err := openWal(p, p.path, p.pageSize)
		if err != nil {
			return err
		}
		p.wal = w
		p.journalMode = "wal"
		return nil
	case "delete", "truncate", "persist", "memory", "off", "wal2":
		// Legacy rollback-journal modes. The mode is recorded so that
		// PRAGMA journal_mode reports it on read-back; the commit path
		// honours it when materialising / disposing of the rollback
		// journal (see the transaction commit/rollback handlers). "delete"
		// is the SQLite default and keeps the legacy direct-flush path.
		if p.wal != nil {
			p.wal.Close()
			p.wal = nil
		}
		// Switching journal modes may need to close + unlink an
		// already-open journal file (the previous mode opened it under
		// its own policy, but the new mode may want to start fresh or
		// handle it differently). We close + unlink in every
		// cross-mode transition (not just PERSIST/TRUNCATE → *) so a
		// DELETE-mode implicit open (the engine opens one on the first
		// write of an empty database) does not leak into a subsequent
		// PERSIST/TRUNCATE session — otherwise the new mode's first
		// transaction sees the stale DELETE-mode file already open and
		// skips xOpen (journal2.test 2.2 — PRAGMA persist; CREATE TABLE
		// → expected xOpen, but the file is already open).
		prev := p.journalMode
		if p.journalFile != nil && m != prev {
			jpath := p.journalFile.Name()
			_ = p.journalFile.Close()
			p.journalFile = nil
			if h := p.journalFileOpHookFn(); h != nil {
				h("xClose", jpath)
			}
			if h := p.journalFileOpHookFn(); h != nil {
				h("xDelete", jpath)
			}
			_ = os.Remove(jpath)
		}
		p.journalMode = m
		return nil
	default:
		// SQLite treats an unrecognised journal mode token as a no-op:
		// the current mode is left unchanged and the statement returns
		// the current mode without an error (test/journal.c jrnlmode-1.8).
		return nil
	}
}

// JournalMode reports the active journal mode ("wal", "delete", "truncate",
// "persist", "memory", "off", or "" which the caller maps to "delete").
func (p *Pager) JournalMode() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.journalMode
}

// SetJournalSizeLimit records the PRAGMA journal_size_limit cap (bytes) for
// this database. A negative value means unlimited (the journal keeps its full
// content after a PERSIST commit); 0 truncates the journal to zero; a positive
// value truncates it down to that many bytes. SQLite's default is 32768
// (pragma.c journalSizeLimit). The value is stored verbatim so the getter can
// echo it.
func (p *Pager) SetJournalSizeLimit(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.journalSizeLimit = n
}

// JournalSizeLimit reports the recorded PRAGMA journal_size_limit cap.
func (p *Pager) JournalSizeLimit() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.journalSizeLimit
}

// SetPendingJournalMode records a journal-mode change requested while a
// transaction was open. pager.c defers the actual switch until the transaction
// ends (sqlite3BtreeSetJournalMode sets pBt->pendingJournalMode and
// btreeEndTransaction applies it). An unrecognised mode is ignored (no-op),
// matching the setter's behaviour outside a transaction.
func (p *Pager) SetPendingJournalMode(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "delete", "truncate", "persist", "memory", "off", "wal", "wal2":
		p.pendingJournalMode = m
	default:
		// unrecognised token: leave the pending change unset
	}
}

// ApplyPendingJournalMode commits a deferred journal-mode change (recorded by
// SetPendingJournalMode) at transaction end. It is a no-op when no change is
// pending. Called from the engine's COMMIT/ROLLBACK path for every database.
func (p *Pager) ApplyPendingJournalMode() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pendingJournalMode == "" {
		return
	}
	if p.wal != nil && p.pendingJournalMode != "wal" {
		p.wal.Close()
		p.wal = nil
	}
	p.journalMode = p.pendingJournalMode
	p.pendingJournalMode = ""
}

// SetWalHook registers the sqlite3_wal_hook callback, invoked after each WAL
// commit with (frames appended, frames checkpointed).
func (p *Pager) SetWalHook(fn func(nLog, nCkpt int) int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.walHook = fn
}

// SetWalFault installs an I/O fault injector for WAL writes (test_syscall
// equivalent). When fn returns a non-nil error for a given operation, the WAL
// writer aborts that write with it. Pass nil to clear.
func (p *Pager) SetWalFault(fn func(op string) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.walFault = fn
}

// SetJournalFileOpHook installs a callback that fires for xOpen/xClose/xDelete
// events on the "test.db-journal" sidecar. The hook is the testvfs equivalent
// for the journal file: frigolite does not have a full VFS plugin system, but
// the journal2 TCL test suite needs to observe the OS-level sequence of file
// operations on the journal sidecar. Pass nil to clear.
//
// The hook is called with the operation name ("xOpen", "xClose", "xDelete")
// and the absolute path of the journal file. It fires synchronously under
// p.mu, so the hook should be lightweight (e.g. appending to a string).
func (p *Pager) SetJournalFileOpHook(fn func(op, path string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.journalFileOpHook = fn
}

// Checkpoint folds the WAL into the main database file and resets the WAL
// (RESTART-style). It is a no-op when not in WAL mode.
func (p *Pager) Checkpoint() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wal == nil {
		return nil
	}
	return p.wal.checkpoint()
}

// WalFileSize reports the current "-wal" file size in bytes (0 when not in
// WAL mode). Used by tests to simulate a crash at a frame boundary.
func (p *Pager) WalFileSize() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.wal == nil {
		return 0
	}
	return p.wal.FileSize()
}

// flushAll is the legacy single-DB flush entry point. New callers should
// use FlushWithContext to pass the multi-DB flag.
func (p *Pager) flushAll() error {
	return p.flushAllCtx(false)
}

// flushAllCtx is called under p.mu. The multiDB flag is true when this
// flush is part of a COMMIT that includes one or more ATTACH'd databases
// (the "super-journal" path in pager.c, which forces PERSIST-mode
// journals to truncate to 0 instead of honouring journal_size_limit).
func (p *Pager) flushAllCtx(multiDB bool) error {
	// WAL mode: route the commit through the WAL writer (frames go to the
	// "-wal" file; the main database is updated only by Checkpoint). The
	// legacy direct-flush path below is used for every other journal mode.
	if p.wal != nil {
		_, err := p.wal.commit()
		if err != nil {
			return err
		}
		p.dirty = make(map[uint32]bool)
		return nil
	}
	if p.file != nil {
		if len(p.dirty) == 0 {
			// No dirty pages — nothing to write to the main database, and
			// nothing to record in the journal. The journal file may still
			// be open from a previous flush cycle (PERSIST/TRUNCATE keep
			// the file open across COMMITs). Only PERSIST needs
			// re-finalisation here to honour journal_size_limit; TRUNCATE
			// and DELETE already finalised at COMMIT (DELETE closed +
			// unlinked, TRUNCATE left an open zero-length file that does
			// not need re-truncation).
			if p.journalFile != nil && p.journalMode == "persist" {
				if err := p.finalizeRollbackJournalLockedMulti(multiDB); err != nil {
					return err
				}
			}
			return nil
		}
		// Open the rollback journal (test.db-journal) for this COMMIT. For
		// modes that don't use a file (memory/off/wal) the helper is a
		// no-op. The journal captures the BEFORE image of every dirty
		// page written below, so a ROLLBACK can restore them. (P7.WAL-E
		// rollback journal machinery — see journal.go.)
		if err := p.openRollbackJournalLocked(); err != nil {
			return err
		}
		for pageNum := range p.dirty {
			if err := p.flushPage(pageNum); err != nil {
				return err
			}
		}
		// Finalise the journal after every dirty page is on disk: DELETE
		// unlinks, TRUNCATE zeroes, PERSIST truncates to journal_size_limit
		// (or 0 in the super-journal / multi-DB case), MEMORY/OFF are
		// no-ops (no file was created). This mirrors pager.c
		// pager_end_transaction / sqlite3PagerCommitPhaseOne.
		if err := p.finalizeRollbackJournalLockedMulti(multiDB); err != nil {
			return err
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
	// Record the BEFORE image of this page in the rollback journal. The
	// pg.Data we have here is the AFTER image (the in-memory dirty copy);
	// the BEFORE image lives in the on-disk page. We must read it from
	// the file BEFORE we overwrite it. (pager.c pager_write_pagelist /
	// sqlite3PagerWrite: the BEFORE image is whatever is on disk; for a
	// newly-allocated page the BEFORE is zeros, which is also what an
	// OpenFile of a non-existent page would return.)
	if p.journalFile != nil {
		before := make([]byte, p.pageSize)
		if _, err := p.file.ReadAt(before, off); err == nil {
			// The on-disk byte may be short (file smaller than the
			// page offset) — that means the page was never written
			// before, so the BEFORE is all zeros (already the case).
			if err := p.appendRollbackRecordLocked(pageNum, before); err != nil {
				return err
			}
		}
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
