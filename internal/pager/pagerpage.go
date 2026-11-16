// Package pager — page allocation, cache reads and cache invalidation.
//
// AllocatePage family (btree.c allocateBtreePage tail), ReadPage and its
// file/WAL sourcing (pager.c readDbPage), InvalidateCache and the WAL
// snapshot refresh, plus the write-path entry (sqlite3PagerWrite).
package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pijalu/frigolite/internal/storage"
)

// ErrCorruptPage is the pager's page-image corruption verdict (C's
// SQLITE_CORRUPT from getPageNormal's lock-byte-page fetch and the
// malformed-header guards). Scans distinguish it from cursor exhaustion so
// a corrupt page aborts the statement instead of ending the scan
// silently (pager.c's readDbPage/getPageNormal error propagation).
var ErrCorruptPage = errors.New("database disk image is malformed")

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
// from the user rootpage range.
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
	pg := p.allocateExtendLocked()
	if pg == nil {
		return nil
	}
	return pg
}

// AllocatePageForTree is the b-tree write-path allocation (btree.c
// allocateBtreePage called from a live b-tree operation): liveRoot is the
// calling tree's root page number. When the freelist pop hands back
// liveRoot itself, the popped page is still in use by the open tree —
// btreeGetUnusedPage's "page already in use" corruption check
// (sqlite3PagerPageRefcount>1 → SQLITE_CORRUPT_BKPT, src/btree.c:2457-2461),
// which fires on images with duplicated freelist entries (corrupt9): the
// duplicated leaf was consumed as this tree's root moments earlier, so the
// second pop returns a live page. The root of the allocating tree is the
// one page its own write path always holds, making it the faithful
// refcount>1 signal; a legitimate allocation can never return the tree's
// own root because a live root is never on the freelist.
func (p *Pager) AllocatePageForTree(liveRoot uint32) (*Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pgno := p.allocateFreelistLocked(); pgno != 0 {
		if liveRoot != 0 && pgno == liveRoot {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		return p.grabPageLocked(pgno), nil
	}
	pg := p.allocateExtendLocked()
	if pg == nil {
		return nil, fmt.Errorf("database or disk is full")
	}
	return pg, nil
}

// allocateExtendLocked grows the file by one page and returns it (the
// no-freelist tail of btree.c allocateBtreePage). Returns nil when
// max_page_count blocks the extend (caller reports SQLITE_FULL).
// Caller holds p.mu.
func (p *Pager) allocateExtendLocked() *Page {
	// P8.PRAGMA: PRAGMA max_page_count enforcement. pager.c::getPageNo
	// rejects writes beyond mxPgno with SQLITE_FULL. We mirror that here:
	// if the new page number would exceed maxPageCount, return nil so the
	// caller surfaces "database or disk is full" (the canonical SQLite
	// text).
	if p.numPages+1 > p.effectiveMaxPageCountLocked() {
		return nil
	}
	p.numPages++
	// btree.c:6740 — the PENDING_BYTE page is never a usable page number:
	// after every end-of-file increment, allocations jump past it. This
	// applies to a test-harness override too (sqlite3_test_control
	// TESTCTRL_PENDING_BYTE changes WHERE the lock byte lives, not WHETHER
	// the page is usable — autovacuum-2.4.5's expected root list excludes
	// the overridden pending-byte page 65 exactly like the ptrmap pages
	// 207/412). The page is materialized zeroed so the file size covers
	// the hole (SQLite's file_pages counts it via file size).
	pendingPage := p.pendingBytePageFor()
	if p.numPages == pendingPage {
		pending := &Page{
			Data:    make([]byte, p.pageSize),
			PageNum: p.numPages,
		}
		p.pages[pending.PageNum] = pending
		p.markDirtyLocked(pending.PageNum)
		p.numPages++
	}
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
		p.markDirtyLocked(ptr.PageNum)
		p.numPages++
		// btree.c:6758 — re-check the pending byte after the ptrmap skip.
		if p.numPages == pendingPage {
			p.numPages++
		}
	}
	pg := &Page{
		Data:    make([]byte, p.pageSize),
		PageNum: p.numPages,
	}
	// For page 1, pre-fill with header
	if p.numPages == 1 && p.header != nil {
		copy(pg.Data[:HeaderSize], p.header)
	}
	// btree.c allocateBtreePage's append branch (src/btree.c:6737+6761)
	// makes page 1 writable and writes ONLY the new nPage (bytes 28-31)
	// into its header image at every allocation-from-end: the commit's
	// frame list then carries a page-1 frame whenever the file grew
	// (walvfs-5.3/8.3's frame-count parity — C's WAL has one more frame
	// than a page list without it). WAL-mode commits only: the stamp must
	// not lead the physical file — C's allocateBtreePage never runs
	// outside a write transaction, while this pager's open-time
	// initialization allocates page 1 on a fresh (0-byte) database and a
	// stamp would survive cache resets with no commit to flush it
	// (HeaderBeyondFile would then report the healthy image as truncated).
	// Rollback-mode commits keep the flush-time header stamp (flushPage).
	// The write is size-bytes-only, in place on the cached image: a
	// full-header mirror here would clobber page 1's fresher in-progress
	// meta bytes (btree writes those directly into the page buffer).
	if p.wal != nil {
		p.markDirtyLocked(1)
		binary.BigEndian.PutUint32(p.header[28:32], p.numPages)
		if pg1, ok := p.pages[1]; ok && pg1 != nil && len(pg1.Data) >= 32 {
			binary.BigEndian.PutUint32(pg1.Data[28:32], p.numPages)
		}
	}
	p.pages[pg.PageNum] = pg
	p.markDirtyLocked(pg.PageNum)
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
	if len(p.header) >= 56 && p.autoVacuum {
		current := binary.BigEndian.Uint32(p.header[52:56])
		if pg.PageNum > current {
			binary.BigEndian.PutUint32(p.header[52:56], pg.PageNum)
			p.markDirtyLocked(1)
		}
	}
	p.mu.Unlock()
	return pg
}

// ReadPage reads a page. Data is always pageSize bytes.
//
// The read path performs NO statement-journal work: a read hands out bytes
// the caller may only read; pages a caller intends to mutate must go
// through PrepareWrite first (pager.c sqlite3PagerWrite's contract), which
// captures the before-image pre-mutation. Capturing on read would byte-copy
// every interior page of every seek — pages the vast majority of statements
// never mutate.
func (p *Pager) ReadPage(pageNum uint32) (*Page, error) {
	if pageNum == 0 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	// C's pagerOpenWal refusal surfaces at the first page acquire (the lazy
	// sqlite3_open): a WAL db through an xShm-less VFS reports CANTOPEN
	// "unable to open database file" on every statement (pager1-21.1/21.2).
	if p.walOpenRefused {
		return nil, fmt.Errorf("unable to open database file")
	}
	// C's getPageNormal (pager.c:5577-5580): a content fetch of the lock-byte
	// page (the PENDING_BYTE page, PAGER_SJ_PGNO) is corruption. A database
	// BUILT before the pending byte moved holds data on that page; reading it
	// after `sqlite3_test_control(SQLITE_TESTCTRL_PENDING_BYTE, ...)` fails
	// "database disk image is malformed" (pager1-42.1). 0 override = the
	// production default (0x40000000, beyond any test file) — no check.
	if p.pendingByteOverride != 0 && pageNum == p.PendingBytePage() {
		return nil, fmt.Errorf("%w (lock-byte page %d)", ErrCorruptPage, pageNum)
	}
	p.mu.RLock()
	pg, ok := p.pages[pageNum]
	p.mu.RUnlock()
	if ok {
		p.auditReadTouch(pg)
		p.cacheStatHitN.Add(1)
		return pg, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	_, hit := p.pages[pageNum]
	if !hit {
		// Cache miss with content wanted (pager.c sqlite3PagerAcquire's
		// pcache-miss branch: PAGER_STAT_MISS increments right before
		// readDbPage). Only the content-acquire path counts — the open-time
		// header validation and external-modification checks read page 1
		// through readPageLocked directly, mirroring C's direct
		// sqlite3OsRead at open (no pcache acquire, no counter).
		if p.file != nil || p.wal != nil {
			p.cacheStatMissN.Add(1)
		}
	}
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
	if err := p.loadPageFromDiskLocked(pg, pageNum); err != nil {
		return nil, err
	}
	// For page 1, extract the header from the full page data (only when the
	// page was actually sourced from a file — memory pagers own their header
	// through storage.DefaultHeader).
	if pageNum == 1 && p.header == nil && (p.wal != nil || p.file != nil) {
		p.header = make([]byte, HeaderSize)
		copy(p.header, pg.Data[:HeaderSize])
		p.invalidateCookieCacheLocked()
	}
	p.pages[pageNum] = pg
	p.auditRecordReadLocked(pg)
	return pg, nil
}

// loadPageFromDiskLocked fills a fresh page from the WAL or the main file
// (memory pagers leave it zeroed). Caller holds p.mu.
func (p *Pager) loadPageFromDiskLocked(pg *Page, pageNum uint32) error {
	if p.wal != nil {
		// WAL mode (P7.WAL-G7): resolve the page through the shared
		// wal-index or the checkpointed main file (readPageWALLocked).
		return p.readPageWALLocked(pg, pageNum)
	}
	return p.readFilePageLocked(pg, pageNum)
}

// readPageWALLocked fills pg from the connection's WAL snapshot: the newest
// frame within the reader's frozen view (hdr.mxFrame) at or above minFrame,
// else the main database file. A reader pinned at READ_LOCK(0) (readLock==0
// — the log was fully backfilled at pin time) ignores the WAL entirely: the
// main database file is the snapshot (wal.c walFindFrame's early return).
// Frames below minFrame are already checkpointed and their hash entries may
// be stale, so walIndexFind skips them (the minFrame rule). Caller holds
// p.mu for writing.
func (p *Pager) readPageWALLocked(pg *Page, pageNum uint32) error {
	if p.wal.readLock == 0 {
		return p.readFilePageLocked(pg, pageNum)
	}
	if iFrame := p.wal.wi.FindFrame(pageNum, p.wal.hdr.MxFrame, p.wal.minFrame); iFrame > 0 {
		off := walFrameOffset(int(iFrame), p.pageSize) + WalFrameHdrSize
		if _, err := p.wal.file.ReadAt(pg.Data, off); err != nil {
			return fmt.Errorf("pager: read wal frame %d: %w", iFrame, err)
		}
		return nil
	}
	return p.readFilePageLocked(pg, pageNum)
}

// readFilePageLocked fills pg from the main database file (pager.c
// readDbPage). A short read (page beyond EOF — a truncated or still-empty
// main file under WAL mode) zero-fills the remainder and succeeds:
// SQLITE_IOERR_SHORT_READ is swallowed by readDbPage, and pages above
// dbSize come back zeroed without I/O in getPageNormal. The corruption
// detection happens in the btree/schema layers on the content, not in the
// I/O layer. Caller holds p.mu for writing.
func (p *Pager) readFilePageLocked(pg *Page, pageNum uint32) error {
	if p.file == nil {
		return nil
	}
	off := int64(pageNum-1) * int64(p.pageSize)
	n, err := p.file.ReadAt(pg.Data, off)
	if err == io.EOF || (err == nil && n < len(pg.Data)) {
		// Short read: zero the unread tail (ReadAt may deliver a partial
		// fill together with io.EOF, and a reused buffer holds stale bytes).
		for i := n; i < len(pg.Data); i++ {
			pg.Data[i] = 0
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("pager: read page %d: %w", pageNum, err)
	}
	return nil
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
	// External-modification detection compares file identities
	// (os.SameFile): a multiplexed file's identity is chunk 0's raw stat —
	// the group's aggregated FileInfo wrapper does not satisfy
	// os.SameFile's *os.fileStat assertion.
	if p.mux != nil {
		info, err := p.mux.Chunk0Stat()
		if err != nil {
			return nil, false
		}
		return info, true
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
// mode the cache is then rebuilt through the shared wal-index (reads resolve
// via walIndexFind → frame → page bytes), so a schema reload never loses
// uncheckpointed commits.
//
// Rollback mode also RE-READS the 100-byte header and adopts its page size
// and reserved-space byte when it still parses (btree.c lockBtree reloads
// page 1 on every new read transaction: another connection's VACUUM may have
// materialized a new reserve, and the usable size drives every cell parse —
// reservebytes-1.3.4's integrity_check on a second connection walks the
// rebuilt image with the NEW usable size). A header that fails to parse is
// left untouched: the headerCorrupt deferral survives (filefmt-1.2).
func (p *Pager) InvalidateCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = make(map[uint32]*Page)
	if p.wal != nil {
		// Refresh the shared wal-index header (recovering it when another
		// connection left it unparsable) and rebuild the header/page-count
		// state through the wal-index read path. The caller holds p.mu.
		p.walIndexRefreshLocked()
		return
	}
	if p.file != nil {
		buf := make([]byte, HeaderSize)
		if _, err := p.file.ReadAt(buf, 0); err == nil {
			if _, perr := storage.ParseHeader(buf); perr == nil {
				p.header = buf
				p.adoptHeaderPageSizeLocked(buf)
			}
		}
		if info, err := p.file.Stat(); err == nil {
			p.fileSize = info.Size()
			p.numPages = uint32(info.Size() / int64(p.pageSize))
			if p.numPages == 0 && info.Size() > 0 {
				p.numPages = 1
			}
		}
	}
	p.invalidateCookieCacheLocked()
}

// walIndexRefreshLocked opens the connection's WAL read transaction (the
// sqlite3WalBeginReadTransaction port of wal.c L3473): the wal-index header
// is refreshed (recovering it when another connection left it unparsable)
// and a shared read-mark lock pins the snapshot (walread.go). While a read
// transaction is open the header is FROZEN — repeated calls see the same
// frame universe (repeatable reads); it is released by WALEndRead at the
// end of the read transaction. On any refresh the shared state is adopted
// into this connection's cached view: page count from hdr.nPage, and — when
// the header moved — the page cache is dropped and page 1 re-read through
// the wal-index so the cached database header is the committed image. The
// returned bool is the pChanged signal (the caller must reset its caches);
// the error surfaces BUSY_RECOVERY / SQLITE_PROTOCOL per walTryBeginRead.
// Caller holds p.mu.
func (p *Pager) walIndexRefreshLocked() (bool, error) {
	if p.wal == nil {
		return false, nil
	}
	changed, err := p.wal.walBeginReadTxn()
	if err != nil {
		// An unparsable wal-index that cannot be recovered right now
		// (another connection holds it busy, or the retry budget burned):
		// surface the error to the statement (walTryBeginRead's contract).
		// pager.c pagerBeginReadTransaction (L3257-3261) drops the cache on
		// a FAILED read-transaction open as well as a changed one — a
		// failed snapshot open must not leave snapshot-era pages cached.
		p.pages = make(map[uint32]*Page)
		p.header = nil
		p.invalidateCookieCacheLocked()
		return false, err
	}
	// Adopt the shared state ONLY when the header moved (another
	// connection committed): the committed page count must never shrink a
	// live transaction's view — this transaction's own allocations grow
	// p.numPages past the frozen hdr.NPage, and a mid-transaction reset
	// would make allocateExtend re-issue page numbers already used by
	// dirty pages (torn btree: lost rows, cyclic overflow chains).
	if changed {
		if n := p.wal.hdr.NPage; n > 0 {
			p.numPages = n
		}
		// Another connection committed: drop the WHOLE page cache
		// (pager.c pager_reset on an external change) — every cached page
		// may have a newer frame in the wal-index. Then re-read page 1
		// through the wal-index read path so ValidateHeader and the schema
		// reload see the committed image (SQLite's shared lock re-reads
		// page 1 after walIndexReadHdr reports a change).
		p.pages = make(map[uint32]*Page)
		p.header = nil
		p.invalidateCookieCacheLocked()
		if p.numPages > 0 {
			if _, err := p.readPageLocked(1); err != nil {
				// The wal-index may reference frames the -wal lost to an
				// external truncation: leave the cache empty and let the
				// next read surface the error.
				delete(p.pages, 1)
			}
		}
	}
	return changed, nil
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
	// For page 1, ensure the header is preserved in Data[0:HeaderSize].
	// The statement journal must see the page's PRE-mirror bytes: capture
	// before the header copy, exactly as a PrepareWrite caller would.
	if pg.PageNum == 1 && p.header != nil {
		p.mu.Lock()
		p.stmtJournalPageLocked(1)
		p.mu.Unlock()
		copy(pg.Data[:HeaderSize], p.header)
	}
	p.mu.Lock()
	// Out-of-transaction write detection: an engine statement brackets every
	// attached pager with a statement scope (snapshotAllPagers) and opens the
	// WAL write transaction through the eager statement gate (WALBeginWrite),
	// so an in-statement write arrives with writeLock already held or a
	// statement scope open. A write with NEITHER is the schema bootstrap
	// (schema.Init at Open — C runs newDatabase inside the first statement's
	// transaction; frigolite bootstraps at Open): its WAL write transaction
	// must not outlive this call, or the WRITER shm lock it takes stays held
	// for the connection's lifetime and every OTHER connection's open (whose
	// walIndexReadHdr recovery needs the WRITER) fails with "database is
	// locked" (e_walckpt's cross-connection open cascade).
	walOrphan := p.wal != nil && !p.wal.writeLock && p.stmtTop == nil
	// Open the WAL write transaction on the first dirty page
	// (sqlite3WalBeginWriteTransaction parity): the WRITER shm lock is held
	// from the first write until COMMIT/ROLLBACK so the whole write phase
	// serializes against other connections — the b-tree edits themselves,
	// not just the frame appends. A contended or stale-snapshot write
	// reports "database is locked" (SQLITE_BUSY / SQLITE_BUSY_SNAPSHOT)
	// BEFORE any page is dirtied, so the failed statement aborts cleanly.
	if _, err := p.walBeginWriteLocked(false); err != nil {
		p.mu.Unlock()
		return err
	}
	p.pages[pg.PageNum] = pg
	p.markDirtyLocked(pg.PageNum)
	// Open the rollback journal eagerly on the first write so a ROLLBACK
	// before COMMIT can replay the BEFORE images. openRollbackJournalLocked
	// is a no-op for memory/off/wal modes and for pagers without a file.
	if err := p.openRollbackJournalLocked(); err != nil {
		p.mu.Unlock()
		return err
	}
	// Record the BEFORE image of this page (on disk) into the open journal
	// — at most once per page per transaction (pager.c sqlite3PagerWrite's
	// pInJournal bitvec; a page already journalled keeps its original
	// before-image, and pages above dbOrigSize are never journalled).
	if err := p.journalBeforeImageLocked(pg.PageNum); err != nil {
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	if walOrphan {
		// Complete the orphaned write as its own implicit autocommit
		// transaction: flushAllCtx routes the commit through wal.commit
		// (frames appended, wal-index header published) whose deferred
		// walEndWriteLocked releases the WRITER shm lock — the
		// walBeginWriteTransaction/walEndWriteTransaction pairing of wal.c.
		return p.flushAllCtx("")
	}
	return nil
}

// journalBeforeImageLocked records the BEFORE image of pageNum in the open
// rollback journal, at most once per transaction. A page already journalled
// this transaction keeps the first-recorded before-image, so replaying the
// journal restores the transaction-start state regardless of how many times
// the page was rewritten (pager.c pager_write + pagerJournalPage: the
// pInJournal bitvec skips re-journalling). Pages above journalDBOrigSize
// did not exist when the transaction began — they have no before-image and
// rollback restores them by truncation — so they are never recorded, but
// they are still marked done to avoid a per-write ReadAt probe into EOF.
// Caller holds p.mu.
func (p *Pager) journalBeforeImageLocked(pageNum uint32) error {
	if p.journalFile == nil || pageNum > p.journalDBOrigSize {
		return nil
	}
	if p.journalPagesDone[pageNum] {
		return nil
	}
	off := int64(pageNum-1) * int64(p.pageSize)
	before := make([]byte, p.pageSize)
	_, err := p.file.ReadAt(before, off)
	switch {
	case err == nil:
		if err := p.appendRollbackRecordLocked(pageNum, before); err != nil {
			return err
		}
		if p.journalPagesDone == nil {
			p.journalPagesDone = make(map[uint32]bool)
		}
		p.journalPagesDone[pageNum] = true
	case err == io.EOF:
		// No on-disk image (page allocated during this transaction): no
		// record needed — a short/missing read leaves `before` zeroed and
		// unappended, and rollback truncates back to journalDBOrigSize.
		if p.journalPagesDone == nil {
			p.journalPagesDone = make(map[uint32]bool)
		}
		p.journalPagesDone[pageNum] = true
	default:
		// Transient read error: leave unmarked so the next write retries
		// (matches the previous behavior of silently skipping the record).
	}
	return nil
}

// resetJournalPagesLocked clears the per-transaction journalled-page set.
// Called when a journal epoch ends (journal open, commit finalize, rollback
// playback, mode-switch close). Caller holds p.mu.
func (p *Pager) resetJournalPagesLocked() {
	p.journalPagesDone = nil
}
