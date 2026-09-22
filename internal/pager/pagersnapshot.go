// Package pager — statement-level state snapshot/restore.
//
// PagerState is a deep snapshot of the pager's in-memory pages and header,
// used for statement-level rollback (e.g. a failed REPLACE that fired
// triggers): Snapshot captures, Restore reinstates (pager.c pager_rollback
// via the journal playback path).
package pager

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
	// Pages NOT in the cache are deliberately not warmed here: the snapshot
	// records only cached pages, and Restore evicts cache entries absent
	// from the snapshot so those pages are re-read from disk (which still
	// holds the pre-statement image until the next commit). Warming every
	// page made each statement snapshot O(numPages) with a full byte copy,
	// which made trigger cascades under rollback protection quadratic
	// (sqllimits1-7.5: thousands of nested DML statements x a 1000-page
	// database).
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
	// ROLLBACK ends the write transaction: drop the WRITER shm lock
	// (sqlite3WalEndWriteTransaction parity; a savepoint rollback that still
	// leaves dirty pages re-acquires it at the next write).
	p.walEndWriteLocked()
	// Evict pages the snapshot does not know about: they were loaded (and
	// possibly modified) after the snapshot was taken, so their cached
	// content is post-statement state. Dropping them sends the next read
	// to disk, which still holds the snapshot-time image (pages reach the
	// file only at commit; a mid-transaction spill is covered by the
	// rollback journal).
	for n, pg := range p.pages {
		if _, ok := s.pages[n]; !ok {
			delete(p.pages, n)
			delete(p.dirty, n)
			_ = pg
		}
	}
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
	p.restoreFileImageLocked(s)
	// The rollback reinstated the pre-truncate page count, so a deferred
	// file shrink recorded by truncatePages no longer applies.
	p.pendingFileTruncate = false
}

// restoreFileImageLocked aligns the database FILE with the snapshot image:
// the file is truncated back to the snapshot size in both directions, the
// restored header bytes are persisted to offset 0, and the external-change
// stamp is re-baselined. Caller holds p.mu.
func (p *Pager) restoreFileImageLocked(s *PagerState) {
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
	// The header is only written when the database file has actually been
	// materialized (fileSize > 0). A transaction that never flushed — the
	// engine's read transaction on a zero-byte database — must end with the
	// file still absent (pager.c lazy creation: sqlite3PagerOpen leaves an
	// empty database untouched until the first page is actually written;
	// journal2.test 2.1 asserts no journal/file events from open+rollback
	// alone). Writing the snapshot header unconditionally materialized a
	// 100-byte empty database behind a concurrent writer's transaction.
	if p.file != nil && s.header != nil && len(s.header) >= HeaderSize && p.fileSize > 0 {
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
