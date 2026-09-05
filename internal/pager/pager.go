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
	// pendingByteOverride stores a non-default PENDING_BYTE offset installed
	// by the SQLite test harness via sqlite3_test_control_pending_byte
	// (src/test2.c::testPendingByte). 0 means production default.
	pendingByteOverride uint32
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
	pages    map[uint32]*Page
	dirty    map[uint32]bool
	numPages uint32
	header   []byte
	fileSize int64
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
	// Restore the file size so the on-disk image matches the snapshot's
	// page count in BOTH directions: a transaction may have appended
	// pages (AllocatePage extends the file) or SHRUNK it (in-transaction
	// incremental vacuum truncates the image), and the BEGIN image may
	// itself be an EMPTY file (fresh database — nothing flushed yet, so
	// s.fileSize == 0). C's sqlite3PagerRollback always truncates the
	// database file back to the size recorded in the journal header
	// (pager.c pager_rollback / pagerPlayback), including down to zero
	// pages; skipping the truncate for a zero-size snapshot left the
	// transaction's pages on disk and integrity_check reported every one
	// of them as "Page N: never used" after ROLLBACK.
	if p.file != nil && p.fileSize != s.fileSize {
		if err := p.file.Truncate(s.fileSize); err != nil {
			// Best-effort: if truncate fails, continue and let the
			// integrity check report the mismatch.
			_ = err
		} else {
			p.fileSize = s.fileSize
		}
	}
	// Restore the header SYMMETRICALLY: a snapshot taken before the
	// header was ever materialized (fresh database — page 1 never read
	// or flushed, so p.header was nil at BEGIN) has s.header == nil, and
	// the post-transaction header must then be DROPPED, not kept. Keeping
	// it left header[28] claiming the vacuumed page count after the page
	// cache itself had rolled back (integrity_check walked the stale
	// count and reported the transaction's pages as "never used").
	// pager.c restores page 1's before-image from the journal; for an
	// empty BEGIN image that means no header at all — the next page-1
	// read/flush re-materializes it (readPageLocked / AllocatePage).
	p.header = nil
	if s.header != nil {
		p.header = append([]byte(nil), s.header...)
	}
	// P8.INCRVACUUM.phase16: persist the restored header bytes too.
	// truncatePages writes the shrunken header directly to offset 0
	// mid-transaction (the next statement's HeaderBeyondFile check
	// reads the ON-DISK header), so a ROLLBACK that only restored the
	// in-memory copy would leave the file header claiming the
	// truncated size while the file itself is back at the snapshot
	// size — every subsequent statement fails with "database disk
	// image is malformed" (incrvacuum3 tn3: BEGIN / incremental_vacuum
	// / ROLLBACK). pager.c restores page 1's before-image (which
	// carries the header) from the journal; write it here.
	if p.file != nil && s.header != nil && len(s.header) >= HeaderSize {
		if _, err := p.file.WriteAt(s.header[:HeaderSize], 0); err != nil {
			_ = err
		}
	}
	// The restore rewrote the file behind the external-modification
	// detector's back; re-baseline the file stamp so our own writes are
	// not mistaken for another connection's (pager.c does the same after
	// journal playback — pagerPlayback ends with pager_unlock re-reading
	// the file state).
	if p.file != nil {
		p.refreshKnownFileStamp()
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
			// Restore the autovacuum mode from the file header. SQLite stores
			// the flag at offset 52, the same 4-byte slot the btree later
			// uses for BTREE_LARGEST_ROOT_PAGE (btree.c:2727 / 3537:
			// `pBt->autoVacuum = (get4byte(&zDbHeader[36+4*4])?1:0)`
			// and `put4byte(&data[36+4*4], pBt->autoVacuum)`). The
			// dual-use is intentional: a freshly-created autovacuum DB
			// has largest_root=1 (the schema btree lives at page 1), so
			// writing 1 there serves both purposes. A subsequent
			// btreeCreateTable raises the value to ≥3 once a user table
			// exists with the ptrmap page 2 reserved — still flagging
			// auto_vacuum=true. Without this restore, the engine reports
			// auto_vacuum=0 for a DB created with PRAGMA auto_vacuum=1
			// (incrvacuum-12.4 expects auto_vacuum=1 after close/reopen).
			if hdr.LargestBTreePage != 0 {
				pr.autoVacuum = true
			}
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
	// lockBtree (btree.c:3401): a header page count (offset 28, trusted
	// only when the change counter matches version-valid-for) that
	// exceeds the file's actual page count means the file was truncated
	// underneath the header — malformed. Corrupt2/incrvacuum suites load
	// images cut short while the header still advertises more pages.
	if p.HeaderBeyondFile() {
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

func (p *Pager) NumPages() uint32        { p.mu.RLock(); defer p.mu.RUnlock(); return p.numPages }
func (p *Pager) Pages() map[uint32]*Page { p.mu.RLock(); defer p.mu.RUnlock(); return p.pages }
func (p *Pager) Header() []byte          { p.mu.RLock(); defer p.mu.RUnlock(); return p.header }

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
//
// S6 P8.INCRVACUUM fix: when turning auto-vacuum ON, also stamp the
// header's LargestBTreePage (offset 52) to 1 — the schema btree
// lives at page 1 and is the largest b-tree page. SQLite does this
// in btree.c::newDatabase (line 3537: `put4byte(&data[36+4*4], pBt->autoVacuum)`).
// The Go engine splits that work: schema.Init allocates page 1 (in
// the pager cache) and writes a default header if none exists, but
// never wrote the LargestBTreePage slot. A fresh DB closed right
// after `PRAGMA auto_vacuum=1` therefore lost the auto-vacuum flag
// on reopen — incrvacuum-12.4 expected 1, got 0; 12.5 then read
// EOF on SELECT * FROM sqlite_master because the engine treated
// the empty file as not-a-database.
//
// Mirroring C: setting on=true writes 1 to header[52:56] (page 1
// is the largest b-tree page) and 0 to header[64:68] (the
// incremental-vacuum flag — FULL auto-vacuum, not INCREMENTAL).
// The on=false path leaves both fields at 0 (no auto-vacuum).
//
// The write happens whether the header is the in-memory default
// (file was opened empty) or the on-disk header was read at Open.
// Page 1 is marked dirty so the commit flushes the change. For
// files already past page 1 the LargestBTreePage slot is meaningful
// (tracks the maximum rootpage); the engine's PRAGMA gate
// (`NumPages() <= 1`) prevents reaching here with a larger file.
func (p *Pager) SetAutoVacuum(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.autoVacuum = on
	if !on {
		return
	}
	if p.header == nil || len(p.header) < HeaderSize {
		// File opened empty (header not yet allocated by schema.Init).
		// Synthesize a default header so the LargestBTreePage write
		// has somewhere to land. schema.Init's own SetHeader call will
		// overwrite this with a fresh default — but the auto-vacuum
		// flag we set here will be lost (schema.Init's default has
		// LargestBTreePage=0). The schema.init path is the right
		// place to fix that, NOT here. (See schema.Init: it copies
		// the pager's autoVacuum flag into the header when
		// allocating page 1.)
		//
		// The next call to schema.Init (e.g. by the first statement
		// that touches the btree) reads the autoVacuum flag and
		// stamps header[52:56]=1 alongside the page-1 allocation.
		// Returning here is correct: the in-memory flag is set, and
		// the schema init path will write the header.
		return
	}
	if binary.BigEndian.Uint32(p.header[52:56]) == 0 {
		binary.BigEndian.PutUint32(p.header[52:56], 1)
		p.dirty[1] = true
		if pg, ok := p.pages[1]; ok && pg != nil && len(pg.Data) >= HeaderSize {
			copy(pg.Data[:HeaderSize], p.header)
		}
	}
	if binary.BigEndian.Uint32(p.header[64:68]) != 0 {
		binary.BigEndian.PutUint32(p.header[64:68], 0)
		p.dirty[1] = true
		if pg, ok := p.pages[1]; ok && pg != nil && len(pg.Data) >= HeaderSize {
			copy(pg.Data[:HeaderSize], p.header)
		}
	}
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

// SetPendingByte overrides the PENDING_BYTE lock-byte offset for this
// pager. The SQLite C test harness installs a non-default value
// (typically 0x10000, page 65 at 1024-byte page size) so file-size
// checks in autovacuum-9.3 / 9.5 / corrupt2 / lock4 can observe a
// small expected value without creating a 1GB database. The override
// is consulted by AllocatePage / AllocatePageLE when deciding whether
// a candidate page lands on the reserved pending-byte slot. A value
// of 0 restores the production default (0x40000000).
func (p *Pager) SetPendingByte(byteOffset uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pendingByteOverride = byteOffset
}

// PendingBytePage returns the page number holding the PENDING_BYTE lock
// byte, honouring any SetPendingByte override.
func (p *Pager) PendingBytePage() uint32 {
	return p.pendingBytePageFor()
}

// SetNumPagesForTesting clamps the in-memory page count to n when n is
// smaller. Used by the btree autovacuum pipeline to resync from the
// on-disk file when a memory/file divergence is observed.
func (p *Pager) SetNumPagesForTesting(n uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n < p.numPages {
		p.numPages = n
	}
}

// ReadAutoVacuumFromHeader reports the auto-vacuum mode stored in the
// on-disk database header. SQLite encodes the largest root btree page
// number at header[52:56] in autovacuum mode; the mode is FULL when
// the field is non-zero. Returns 0 (NONE) when the header is missing
// or corrupted. Used by the engine on Open to restore the auto_vacuum
// mode across connection restarts.
func (p *Pager) ReadAutoVacuumFromHeader() int {
	p.mu.RLock()
	h := p.header
	p.mu.RUnlock()
	if len(h) < 56 {
		return 0
	}
	// meta[3] = header[52:56]. In FULL autovacuum mode, btreeCreateTable
	// writes the page number of each new table root here; a non-zero value
	// means autovacuum is on. INCREMENTAL mode is opt-in via
	// header[64:68] (meta[6]); the value 1..N maps to a free page count
	// threshold, but for "is autovacuum on?" we only need != 0.
	largest := binary.BigEndian.Uint32(h[52:56])
	if largest != 0 {
		return 1 // FULL
	}
	// meta[6] = header[64:68] is the incremental vacuum mode. If set,
	// return 2 (INCREMENTAL).
	if len(h) >= 68 {
		incr := binary.BigEndian.Uint32(h[64:68])
		if incr != 0 {
			return 2
		}
	}
	return 0
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
// PENDING_BYTE_PAGE). The PENDING_BYTE offset is fixed at 0x40000000
// (1073741824) by the SQLite source; for the test harness, see
// Pager.SetPendingByte / Pager.PendingBytePage.
func pendingBytePage(pageSize uint32) uint32 {
	return 1073741824/pageSize + 1
}

// pendingBytePageFor returns the page holding the PENDING_BYTE lock byte
// for the given pager, honouring a per-pager override set by the SQLite
// test harness via sqlite3_test_control_pending_byte. Without an
// override the value matches the production default.
//
// The caller MUST hold p.mu (RLock or Lock); the function does not
// re-acquire the lock because callers that already hold the write lock
// (AllocatePageMode, AllocatePageLE, pickNextFreePageLocked) would
// otherwise self-deadlock. The lock-free read of a uint32 is safe
// under the mutex.
func (p *Pager) pendingBytePageFor() uint32 {
	if p.pendingByteOverride != 0 {
		return p.pendingByteOverride/p.pageSize + 1
	}
	return pendingBytePage(p.pageSize)
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

// For page 1, the first HeaderSize bytes are reserved for the database header.
// With auto-vacuum enabled, page numbers at pointer-map positions are
// reserved as zeroed pointer-map pages and the caller receives the following
// page (btree.c allocateBtreePage reserves PTRMAP pages as they are crossed).
func (p *Pager) AllocatePage() *Page {
	return p.AllocatePageMode(false)
}

// AllocatePageSkipFreelist allocates a page by extending the file, never
// using a page from the freelist. Used by the schema btree so its
// pages don't take slots from the user-rootpage range
// (P8.INCRVACUUM.phase9).
func (p *Pager) AllocatePageSkipFreelist() *Page {
	return p.AllocatePageMode(true)
}

// AllocatePageMode allocates a page, optionally bypassing the freelist
// (always extending the file). P8.INCRVACUUM.phase9: the schema btree
// uses skipFreelist=true so the schema btree's pages don't take slots
// from the user-rootpage range.
func (p *Pager) AllocatePageMode(skipFreelist bool) *Page {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !skipFreelist {
		// btree.c allocateBtreePage: reuse a freelist page whenever the
		// on-disk chain has one (n = header[36] > 0), before extending
		// the file. The pop itself is the C algorithm — take the head
		// trunk when it has no leaves, else its first leaf (copying the
		// last leaf into the freed slot). No in-memory shadow state.
		if pgno := p.allocateFreelistLocked(); pgno != 0 {
			return p.grabPageLocked(pgno)
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
	// P8.INCRVACUUM.phase9.q: in autovacuum mode, btree.c's
	// allocateBTreePage skips the pending-byte page (PENDING_BYTE_PAGE)
	// so a btree page never lands on the lock-byte slot. Materialize
	// the pending-byte page as a free slot (zeroed, no btree header)
	// and increment numPages again so the caller gets a real page
	// past it. Without this, a CREATE TABLE whose next rootpage would
	// otherwise be the pending-byte page silently lands on that slot
	// and is later read as a sparse gap page, which reports
	// "database disk image is malformed".
	if p.autoVacuum && p.numPages == p.pendingBytePageFor() {
		pending := &Page{
			Data:    make([]byte, p.pageSize),
			PageNum: p.numPages,
		}
		p.pages[pending.PageNum] = pending
		p.dirty[pending.PageNum] = true
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

// AllocateRootpage is AllocatePage + a header[52:56] update with the new
// page number. In autovacuum mode, SQLite tracks the largest b-tree
// rootpage in header[52:56] (meta[3]) so a reopened connection can
// detect the mode without re-running PRAGMA auto_vacuum. The engine's
// ReadAutoVacuumFromHeader uses this to restore the mode at Open.
//
// Callers (CREATE TABLE / CREATE INDEX) must invoke this on the page
// they intend to use as a btree root, so the header reflects the actual
// rootpage list across restarts.
func (p *Pager) AllocateRootpage() *Page {
	pg := p.AllocatePage()
	if pg == nil {
		return nil
	}
	p.mu.Lock()
	if p.header != nil && len(p.header) >= 56 && p.autoVacuum {
		current := binary.BigEndian.Uint32(p.header[52:56])
		if pg.PageNum > current {
			binary.BigEndian.PutUint32(p.header[52:56], pg.PageNum)
			p.dirty[1] = true
		}
	}
	p.mu.Unlock()
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
	return p.truncatePages(n, true)
}

// TruncateNoFreelistAdjust is Truncate for the auto-vacuum/incremental
// vacuum paths (btree.c incrVacuumStep): the freelist count is owned
// exclusively by explicit chain operations — the BTALLOC_EXACT pop
// (TakePageFromFreelist) for a bCommit==0 trailing FREE page, and the
// full-drain zeroing (ZeroFreelistChain) at commit end — never by the
// truncation itself. A bCommit==1 trailing FREE page is truncated away
// WITHOUT being popped: its chain entry becomes intentional garbage
// ("it doesn't matter if it still contains some garbage entries",
// btree.c:4026-4028), so the header count must stay in lockstep with
// the chain length.
func (p *Pager) TruncateNoFreelistAdjust(n uint32) error {
	return p.truncatePages(n, false)
}

func (p *Pager) truncatePages(n uint32, adjustFreelistCount bool) error {
	if p.readOnly {
		return fmt.Errorf("pager: read-only")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// C-parity (P8.INCRVACUUM phase16): the freelist chain on disk is the
	// only bookkeeping. For the plain Truncate path, chain entries above
	// the truncation point are removed (and the count kept in lockstep) by
	// freelistPagesAboveLocked BEFORE numPages/file shrink — the splice
	// needs to read trunks above n for their next pointers. The
	// auto-vacuum drain path (TruncateNoFreelistAdjust) leaves the chain
	// untouched: bCommit=1 tolerates trailing "garbage entries"
	// (btree.c:4022-4028) and the count stays in lockstep with the chain.
	if adjustFreelistCount {
		p.freelistPagesAboveLocked(n)
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
	// Skipped for the auto-vacuum drain (adjustFreelistCount=false): the
	// chain still lists the truncated pages by design (bCommit=1 garbage
	// entries), so the count must stay in lockstep with the chain.
	// P8.INCRVACUUM.phase16 (C-parity): the freelist-count adjustment for
	// truncated chain entries lives entirely in freelistPagesAboveLocked
	// (called above, plain-Truncate path only). SQLite's pager truncate
	// does no freelist-chain surgery of its own (pager.c
	// pager_truncate_image only shrinks the file); the btree layer
	// maintains the chain exclusively via allocateBtreePage (pop) and
	// freePage2 (push) — src/btree.c:4022-4032 leaves a trailing FREE
	// page untouched at commit ("the free-list will be truncated to zero
	// after this function returns, so it doesn't matter if it still
	// contains some garbage entries"), and autoVacuumCommit zeroes
	// header.trunk/header.count when the drain completes
	// (src/btree.c:4249-4252, mirrored by ZeroFreelistChain).
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
	// P8.INCRVACUUM.phase9 follow-up: the largest root btree page
	// number (header[52:56] = meta[3]) is the autovacuum-mode flag
	// (a non-zero value at Open time enables autovacuum). It is set
	// to the new page number by AllocatePage for each new rootpage
	// (P8.INCRVACUUM.phase9 follow-up). The autovacuum's truncate
	// may leave the field stale (largest > n if the new file size
	// is below the previous largest rootpage); ValidateHeader then
	// reports "database disk image is malformed" on the next Open
	// (autovacuum-2.4.7 → 2.5.1, autovacuum-9.x after the
	// DELETE-t4 + autovacuum step).
	//
	// Cap largestRoot at the new file size so ValidateHeader
	// passes. Cap to `n` (not 0): the cap must preserve
	// autovacuum mode (largest != 0 enables autovacuum). Capping
	// to n means "the autovacuum-mode flag is still set, but the
	// recorded largest rootpage is the current file size". The
	// next Open reads autovacuum=on; the actual rootpage map is
	// re-derived from the schema btree. (The previous
	// implementation cleared largest=0 here, which silently
	// disabled autovacuum for the rest of the connection's life
	// and produced the autovacuum-9.x failure pattern where the
	// file stayed at full size after DROP TABLE.)
	if p.header != nil && len(p.header) >= 56 {
		largest := binary.BigEndian.Uint32(p.header[52:56])
		if largest > n {
			binary.BigEndian.PutUint32(p.header[52:56], n)
			p.dirty[1] = true
		}
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
		// freelist chain is consistent on disk. Only a trunk that still
		// exists below the truncation point is written; a stale
		// header.trunk above n is chain garbage that the autovacuum
		// commit zeroing (ZeroFreelistChain) removes — writing it here
		// would persist a reference to a truncated page.
		trunk := binary.BigEndian.Uint32(p.header[32:36])
		if trunk > 0 && trunk <= n {
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
	// P8.INCRVACUUM.T5: the pruneFreelistChain walk is GONE. SQLite's
	// truncate does no freelist-chain surgery (pager.c
	// pager_truncate_image only shrinks the file); the engine's prune
	// followed the chain into pages it could not verify as trunks and
	// zeroed their content (see the T5 note above). Chain consistency
	// below the truncation point is maintained by the pops
	// (TakePageFromFreelist / AllocatePageLE); above-the-truncation
	// garbage is removed by the autovacuum commit zeroing.
	return nil
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

// syncHeaderPage1Locked copies the authoritative in-memory header into the
// cached page-1 buffer (caller holds p.mu). Header consumers split between
// p.header (FreelistCount, HeaderPageCount) and the page cache
// (integrity_check parses page 1's bytes), so every header mutation must
// reach both or the two views diverge.
func (p *Pager) syncHeaderPage1Locked() {
	if pg1, ok := p.pages[1]; ok && pg1 != nil && len(p.header) >= HeaderSize && len(pg1.Data) >= HeaderSize {
		copy(pg1.Data[:HeaderSize], p.header)
	}
}

// growHeaderSizeLocked records a file growth in the in-header database size
// (offset 28), monotonically. flushAll iterates the dirty map in random
// order, so a non-monotonic write lets a lower-numbered page flushed late
// overwrite the size an earlier flush of a higher page already recorded
// (e.g. page 22 after page 24 → header says 22 while the file holds 24
// pages). A stale-SMALL header is legal for lockBtree (only
// nPage > nPageFile is corrupt, btree.c:3401) but it defeats the
// incrcorrupt-2.2 parity check after an external truncate and misleads
// HeaderPageCount readers. Only the commit paths (updateFileChangeCounter /
// Truncate) lower the value.
func (p *Pager) growHeaderSizeLocked(pageNum uint32) {
	if p.header == nil || len(p.header) < 32 {
		return
	}
	if cur := binary.BigEndian.Uint32(p.header[28:32]); pageNum <= cur {
		return
	}
	binary.BigEndian.PutUint32(p.header[28:32], pageNum)
	// Mirror the updated header into the cached page 1 so the
	// subsequent flushAll() writes the new header bytes; the page
	// cache holds a separate copy of pg.Data[0:100] from the
	// original Open() read (pager.c pager_write_changecounter
	// likewise mutates page 1's buffer in place at COMMIT).
	if pg1, ok := p.pages[1]; ok && pg1 != nil {
		copy(pg1.Data[:HeaderSize], p.header)
		p.dirty[1] = true
	}
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
	// Page 1 carries the 100-byte database header. p.header is the
	// authoritative in-memory copy (updated by Truncate, FreePage,
	// SetHeader, ...), but several of those writers only touch the cache
	// and mark page 1 dirty without copying the bytes into the cached
	// page buffer (ZeroFreelistChain and updateDBHeaderField do; the
	// Truncate freelist-count/size adjustments did not). Stamping the
	// live header here — right before the write — guarantees the flushed
	// page 1 never carries a stale freelist count or file size
	// (integrity_check parses page 1 from the page cache and compares it
	// against the chain walk; a stale count reports "Freelist: size is 9
	// but should be 5" after an auto-vacuum drain).
	if pageNum == 1 && len(pg.Data) >= HeaderSize && len(p.header) >= HeaderSize {
		copy(pg.Data[:HeaderSize], p.header)
	}
	if p.fileSize < fileEnd {
		if err := p.file.Truncate(fileEnd); err != nil {
			return fmt.Errorf("pager: truncate: %w", err)
		}
		p.fileSize = fileEnd
		// Mirror the new file size in the in-header database size (offset
		// 28). Without this, the on-disk header keeps the pre-extension
		// size even after the file grew, so the next statement's
		// HeaderBeyondFile check (src/btree.c lockBtree) compares a stale
		// header against the new file size and either fails with
		// "database disk image is malformed" (when the version-valid-for
		// check at offset 92 trusts the header) or lets autovacuum walk a
		// freelist chain that no longer matches the file (corrupt2 /
		// autovacuum-2.4.5, -2.5.1, -9.x, -10.1).
		p.growHeaderSizeLocked(pageNum)
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
