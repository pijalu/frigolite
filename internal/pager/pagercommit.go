// Package pager — commit path: truncate, flush and journal finalization.
//
// Truncate (pager.c pager_truncate_image parity), FlushWithContext (the
// sqlite3PagerCommitPhaseOne port: journal open, page-list write in
// flushOrderLocked order, journal finalize) and flushPage, the per-page
// writer with quota and header-growth bookkeeping.
package pager

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"

	"github.com/pijalu/frigolite/internal/quota"
)

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
	p.journalTruncateTailLocked(n)
	p.truncateCachePagesLocked(n)
	if n < p.numPages {
		p.numPages = n
	}
	if p.file != nil {
		if err := p.shrinkDatabaseFileLocked(n); err != nil {
			return err
		}
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
	p.truncateHeaderCountsLocked(n)
	if p.file != nil {
		if err := p.flushTruncateHeaderLocked(n); err != nil {
			return err
		}
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

// journalTruncateTailLocked captures the before-image of every truncated
// tail page while a rollback journal is open, so a ROLLBACK can restore both
// the pages' content and the file length (pager.c syncJournal's nTrunc field
// + pager_rollback playback). The on-disk image is still the before-image
// here — dirty pages flush only at COMMIT — and the journal must be appended
// BEFORE the file shrinks. Caller holds p.mu.
func (p *Pager) journalTruncateTailLocked(n uint32) {
	if p.journalFile == nil || n >= p.numPages {
		return
	}
	for pgno := n + 1; pgno <= p.numPages; pgno++ {
		p.journalPageBeforeLocked(pgno)
	}
}

// truncateCachePagesLocked evicts every cached page above the new page count
// (the cache half of pager.c pager_truncate_image). Caller holds p.mu.
func (p *Pager) truncateCachePagesLocked(n uint32) {
	for pgno := range p.pages {
		if pgno > n {
			delete(p.pages, pgno)
			delete(p.dirty, pgno)
		}
	}
}

// shrinkDatabaseFileLocked truncates the database file to n pages and mirrors
// the new size in the cache so FilePageCount() reflects the post-truncate
// size (P8.INCRVACUUM phase 4: integrity_check otherwise sees the
// pre-truncate size and reports "Page N: never used" for pages that no
// longer exist on disk). Caller holds p.mu.
func (p *Pager) shrinkDatabaseFileLocked(n uint32) error {
	newSize := int64(n) * int64(p.pageSize)
	if err := p.file.Truncate(newSize); err != nil {
		return fmt.Errorf("pager: truncate to %d pages: %w", n, err)
	}
	p.fileSize = newSize
	return nil
}

// truncateHeaderCountsLocked updates the truncate-sensitive header fields:
// the in-header database size (offset 28) and the largest-root meta[3] slot
// (offset 52), then mirrors the header into the cached page 1 so the next
// flush writes the new header bytes (FreePage/Truncate only update p.header;
// the page cache holds a separate copy of pg.Data).
//
// The meta[3] cap: the largest root btree page number is also the
// autovacuum-mode flag (a non-zero value at Open time enables autovacuum).
// An autovacuum truncate may leave the field stale (largest > n if the new
// file size is below the previous largest rootpage); ValidateHeader then
// reports "database disk image is malformed" on the next Open (autovacuum-
// 2.4.7 → 2.5.1, autovacuum-9.x after the DELETE-t4 + autovacuum step). Cap
// to `n` (not 0): capping to n means "the autovacuum-mode flag is still set,
// but the recorded largest rootpage is the current file size". The next Open
// reads autovacuum=on; the actual rootpage map is re-derived from the schema
// btree. (The previous implementation cleared largest=0 here, which silently
// disabled autovacuum for the rest of the connection's life and produced the
// autovacuum-9.x failure pattern where the file stayed at full size after
// DROP TABLE.)
//
// Caller holds p.mu.
func (p *Pager) truncateHeaderCountsLocked(n uint32) {
	if len(p.header) >= 32 {
		binary.BigEndian.PutUint32(p.header[28:32], n)
		p.dirty[1] = true
	}
	if len(p.header) >= 56 {
		largest := binary.BigEndian.Uint32(p.header[52:56])
		if largest > n {
			binary.BigEndian.PutUint32(p.header[52:56], n)
			p.dirty[1] = true
		}
	}
	if p.header != nil {
		if pg, ok := p.pages[1]; ok && pg != nil {
			copy(pg.Data[:HeaderSize], p.header)
		}
	}
}

// flushTruncateHeaderLocked writes the updated header directly to offset 0
// so the on-disk header matches the truncated file size before the next read
// (P8.INCRVACUUM phase 5 fix): the file was just truncated but the on-disk
// header still has the pre-truncate size at offset 28. The next statement's
// execDBFileChecks calls HeaderBeyondFile, which reads the on-disk header
// and compares its nPage against the file's page count. Without this write,
// the file is now N pages but the header says N+1 (or more), and every
// subsequent statement fails with "database disk image is malformed". The
// trunk page's chain pointer (if updated above) is also flushed so the
// freelist walker (checkFreelistCount / isFreelistPage) sees a consistent
// chain; only a trunk that still exists below the truncation point is
// written — a stale header.trunk above n is chain garbage that the
// autovacuum commit zeroing (ZeroFreelistChain) removes, and writing it here
// would persist a reference to a truncated page. The known file stamp is
// refreshed so CheckExternalFile does not mistake our own writes for
// external changes. Caller holds p.mu.
func (p *Pager) flushTruncateHeaderLocked(n uint32) error {
	if len(p.header) < HeaderSize {
		return nil
	}
	if _, err := p.file.WriteAt(p.header[:HeaderSize], 0); err != nil {
		return fmt.Errorf("pager: truncate: write header: %w", err)
	}
	trunk := binary.BigEndian.Uint32(p.header[32:36])
	if trunk > 0 && trunk <= n {
		if pg, ok := p.pages[trunk]; ok && pg != nil {
			off := int64(trunk-1) * int64(p.pageSize)
			if _, err := p.file.WriteAt(pg.Data, off); err != nil {
				return fmt.Errorf("pager: truncate: write trunk page %d: %w", trunk, err)
			}
		}
	}
	vers, size, _ := p.readFileStamp()
	p.knownFileVers = vers
	p.knownFileSize = size
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

// flushAll is the legacy single-DB flush entry point. New callers should
// use FlushWithContext to pass the multi-DB flag.
func (p *Pager) flushAll() error {
	return p.flushAllCtx(false)
}

// flushOrderLocked returns the dirty pages in commit-write order: all pages
// ascending, then page 1 LAST. sqlite3PagerCommitPhaseOne stamps page 1's
// change counter / in-header page count after the page-list write: a page
// allocated during this cycle extends the file mid-loop and
// growHeaderSizeLocked raises the in-header size and re-dirties page 1.
// Flushing page 1 first would write the pre-growth header (on-disk nPage 47
// with page 48 present — integrity_check then reports "invalid page number"
// on page_size=512 FTS4 builds) and the end-of-cycle dirty wipe would drop
// the re-dirty mark. The caller holds p.mu.
func (p *Pager) flushOrderLocked() []uint32 {
	order := make([]uint32, 0, len(p.dirty))
	for pageNum := range p.dirty {
		if pageNum != 1 {
			order = append(order, pageNum)
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	if p.dirty[1] {
		order = append(order, 1)
	}
	return order
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
		if err := p.flushFilePagesLocked(multiDB); err != nil {
			return err
		}
	}
	// Clear the dirty set in all cases (an in-memory pager has no file to
	// write, but COMMIT/autocommit must still release the "exclusive" lock
	// state that lock_status reports from HasDirtyPages).
	p.dirty = make(map[uint32]bool)
	return nil
}

// flushFilePagesLocked is the legacy direct-flush commit phase for a
// file-backed pager (the non-WAL tail of sqlite3PagerCommitPhaseOne): open
// the rollback journal, write every dirty page in flushOrderLocked order,
// finalise the journal, and re-baseline the external-change stamp. When the
// commit has no dirty pages only a PERSIST journal re-finalisation runs (to
// honour journal_size_limit). Caller holds p.mu.
func (p *Pager) flushFilePagesLocked(multiDB bool) error {
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
	// Flush page 1 LAST (see flushOrderLocked for the sqlite3PagerCommit
	// PhaseOne ordering rationale).
	for _, pageNum := range p.flushOrderLocked() {
		if err := p.flushPage(pageNum); err != nil {
			// pager.c: a failed commit phase-one rolls the
			// transaction back — journal playback restores the
			// before-images of the pages already written and
			// unlinks the journal. Without this, the journal fd
			// lingers open with a half-written main database.
			_ = p.rollbackFromJournalLocked()
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
	return nil
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
	if len(p.header) < 32 {
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
	if os.Getenv("QDBG3") != "" {
		fmt.Fprintf(os.Stderr, "QDBG3 flushPage page=%d fileSize=%d fileEnd=%d numPages=%d\n", pageNum, p.fileSize, fileEnd, p.numPages)
	}
	if p.fileSize < fileEnd {
		// Quota enforcement (test_quota.c quotaWrite): growing the file
		// past its tracked size goes through the quota layer, which
		// invokes the group callback (the callback may raise or zero the
		// limit) and refuses the growth with SQLITE_FULL otherwise.
		if err := quota.CheckDBFileGrowth(p.path, fileEnd); err != nil {
			return err
		}
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
	// Record the BEFORE image of this page in the rollback journal — once
	// per transaction (journalBeforeImageLocked's pInJournal parity); the
	// WritePage path already journalled every pre-existing page it dirtied,
	// so this is a no-op unless the page was dirtied without WritePage.
	// The pg.Data we have here is the AFTER image (the in-memory dirty
	// copy); the BEFORE image lives in the on-disk page. We must read it
	// from the file BEFORE we overwrite it. (pager.c pager_write_pagelist:
	// the BEFORE image is whatever is on disk; for a newly-allocated page
	// the BEFORE is zeros, which is also what an OpenFile of a
	// non-existent page would return.)
	if err := p.journalBeforeImageLocked(pageNum); err != nil {
		return err
	}

	if _, err := p.file.WriteAt(pg.Data, off); err != nil {
		return fmt.Errorf("pager: write page %d: %w", pageNum, err)
	}
	return nil
}
