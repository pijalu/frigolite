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
	"os"
	"sync"
	"time"

	"github.com/pijalu/frigolite/internal/quota"
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
	// requestedReserve holds a SQLITE_FCNTL_RESERVE_BYTES request that has
	// not been materialized in the file yet (applied by the next VACUUM).
	requestedReserve uint32
	file             *os.File
	pages            map[uint32]*Page
	dirty            map[uint32]bool
	readOnly         bool
	numPages         uint32
	// pendingFileTruncate records a pager_truncate_image shrink that must
	// reach the database FILE at COMMIT: mid-transaction the file keeps the
	// pre-truncate images (the savepoint-snapshot restore re-reads evicted
	// pages from disk), so the physical shrink is deferred to the commit
	// path (src/pager.c pager_truncate_image is in-memory only; the file
	// is cut to nPage during commit).
	pendingFileTruncate bool
	// deferFileShrink is set for the duration of a TruncateDeferFile call
	// (the savepoint-restorable dbpage path); truncatePages consults it to
	// decide between the deferred and the immediate file shrink.
	deferFileShrink bool
	header          []byte
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
	// maxPageCount is the PRAGMA max_page_count cap (pager.c::mxPgno). When
	// numPages would exceed this value, AllocatePage returns nil and the
	// caller surfaces "database or disk is full" (SQLite behavior mirrored
	// from pager.c::getPageNo / sqlite3BtreeSetMaxPageCount). 0 means
	// unlimited (the production default: SQLITE_MAX_PAGE_COUNT = 0x7fffffff).
	maxPageCount uint32
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
	// journalPagesDone tracks the pages whose BEFORE image has already
	// been recorded (or found absent — pages beyond the on-disk file)
	// in the open rollback journal for the current transaction.
	// pager.c parity: sqlite3PagerWrite journals each page once per
	// transaction (pInJournal bitvec) and skips pages above dbOrigSize
	// entirely — a page that did not exist when the transaction began
	// has no before-image to protect (rollback truncates back to
	// journalDBOrigSize). Reset whenever the journal epoch ends
	// (open / finalize / rollback / close).
	journalPagesDone map[uint32]bool
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
	// freeSet memoizes the on-disk freelist chain's membership (trunk +
	// leaf pages) for bulk "is this page on the freelist?" queries
	// (btree_tail's leaf sweeps, findParentInBtree's BFS). Walking the
	// chain per query is O(pages × chain) and stalls mass UPDATE/DELETE on
	// long chains. nil means stale — rebuilt from the chain on next use by
	// freelistSetLocked; every chain mutator nils it.
	freeSet map[uint32]struct{}
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
	flushErr := p.flushAll()
	// Release the WAL writer: close the "-wal" fd and drop this connection's
	// shared wal-index reference (the last detach closes the shm fd and
	// removes the registry entry — the next attach re-runs the DMS truncate
	// + recovery from the "-wal").
	if p.wal != nil {
		walErr := p.wal.Close()
		p.wal = nil
		if walErr != nil && flushErr == nil {
			flushErr = walErr
		}
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
	// Quota layer (quotaClose): release the file's group reference even
	// when the final flush failed — a leaked reference would keep the
	// quota group alive forever (quota.test 4.1.x group removal).
	quota.UnregisterDBFile(p.path)
	if p.file != nil {
		err := p.file.Close()
		if flushErr != nil {
			return flushErr
		}
		return err
	}
	return flushErr
}

func (p *Pager) PageSize() uint32 { return p.pageSize }

// UsableSize returns the number of usable bytes per page (page size minus
// the reserved-space count from header byte 20). SQLite's payload
// distribution formulas are defined over this value, not the raw page size.
func (p *Pager) UsableSize() uint32 { return p.pageSize - p.reserved }

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

// ReadOnly reports whether the pager opened its file read-only (the
// read-write open failed with a permission error, os_unix.c unixOpen
// EACCES fallback). Writes against a read-only pager fail with
// SQLITE_READONLY, "attempt to write a readonly database".
func (p *Pager) ReadOnly() bool {
	return p.readOnly
}

// databaseFileMoved reports whether the file at p.path is no longer the file
// p.file was opened on (pager.c databaseIsUnmoved, surfaced through the
// SQLITE_FCNTL_HAS_MOVED file control): the path has been deleted, or its
// device/inode identity now differs (renamed and recreated). Memory pagers
// and temp files (no path) are never "moved".
func (p *Pager) databaseFileMoved() bool {
	if p.file == nil || p.path == "" {
		return false
	}
	pi, err := os.Stat(p.path)
	if err != nil {
		return true
	}
	fi, err := p.file.Stat()
	if err != nil {
		return false
	}
	return !os.SameFile(pi, fi)
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
func (p *Pager) SetHeader(h []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.header = append([]byte(nil), h...)
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

// DirtyPageCount reports the number of unflushed dirty pages. The engine
// uses it for PRAGMA lock_status: SQLite escalates the transaction lock
// from RESERVED to EXCLUSIVE when the pager spills dirty pages to the
// database file (pager.c WRITER_CACHEMOD→WRITER_DBMOD), which happens
// when the dirty count exceeds the spill threshold (pcache.c szSpill).
func (p *Pager) DirtyPageCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.dirty)
}

// FileChangeCounter reads the database file's change counter (header offset
// 24) directly from the file, bypassing the page cache (so commits by other
// connections are observed even before a cache invalidation). It reports
// whether a counter is available (false for in-memory pagers).
// SchemaCookie returns the database header's schema cookie (offset 40):
// SQLite's schema-version counter, incremented whenever the schema changes
// (btree.c OP_SetCookie semantics). The in-memory image participates in
// Snapshot/Restore, so the cookie reverts when a DDL transaction rolls back —
// cookie-keyed caches stay consistent across restores.
func (p *Pager) SchemaCookie() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.header) < 44 {
		return 0
	}
	return binary.BigEndian.Uint32(p.header[40:44])
}

// BumpSchemaCookie increments the schema cookie and marks page 1 dirty so the
// new value reaches the file (flushPage stamps p.header into the page-1
// buffer right before the write). Schema mutations call this so cookie-keyed
// caches see DDL.
func (p *Pager) BumpSchemaCookie() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.header) < 44 {
		return
	}
	c := binary.BigEndian.Uint32(p.header[40:44]) + 1
	binary.BigEndian.PutUint32(p.header[40:44], c)
	if pg, ok := p.pages[1]; ok && pg != nil && len(pg.Data) >= HeaderSize {
		copy(pg.Data[:HeaderSize], p.header)
	}
	p.dirty[1] = true
}

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
