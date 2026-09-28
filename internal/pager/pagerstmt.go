// Package pager — statement-scoped before-image journal.
//
// This file ports SQLite's statement journal (pager.c sub-journal,
// sqlite3PagerStmtBegin/StmtEnd + pager_playback for the statement scope):
// a writing statement opens a scope with BeginStatement; the FIRST time any
// page is modified inside the scope the pager captures that page's
// before-image exactly once (pager.c pagerAddPageToSubjournal); a statement
// failure replays the captured images (sqlite3VdbeAbort's statement
// rollback), restoring ONLY the pages the statement actually touched — a
// statement that modifies nothing rolls back nothing and costs nothing
// beyond the O(1) scope open.
//
// Capture is ordered before mutation through two coordinated paths:
//
//   - markDirtyLocked (the single choke point every dirty-flag set goes
//     through) captures the before-image of a page at its first dirtying
//     inside the scope. The btree layer mutates page bytes through handles
//     obtained from ReadPage/AllocatePage and flushes them afterwards via
//     WritePage, so capture there is ordered after mutation; it is still
//     correct for pages that were CLEAN at statement start because their
//     pre-statement image lives on the disk/WAL (pages reach the file only
//     at commit), and is captured lazily by re-read at rollback time
//     (stmtEntFromFile entries restore by cache eviction, not by byte copy).
//   - stmtReadTouch (ReadPage / readPageLocked) captures IN MEMORY before
//     the caller can mutate: pages that were already dirty at statement
//     start (an earlier statement of the same transaction modified them, so
//     the disk image is stale) and pages of memory pagers (no disk to fall
//     back to) are byte-copied at first read inside the scope.
//
// Nested scopes compose like SQLite's nested statement transactions
// (trigger bodies firing inside an outer statement): each scope journals
// into its own map; EndStatement splices a committing inner scope's entries
// into its parent (pager.c sub-journal spliced into the transaction
// journal at statement COMMIT — the parent keeps the oldest before-image);
// RollbackStatement replays and discards only the innermost scope's entries,
// leaving earlier statements' writes intact.
package pager

// stmtEntryKind classifies a statement-journal before-image.
type stmtEntryKind uint8

const (
	// stmtEntMemory holds the pre-statement page bytes; rollback reinstates
	// them (the page's disk image does not match its statement-start state:
	// it was already dirty when the statement began, or the pager has no
	// file to recover from).
	stmtEntMemory stmtEntryKind = iota
	// stmtEntFromFile records that the page was CLEAN at statement start, so
	// its statement-start image is the transaction-start image still held by
	// the database file / WAL. Rollback restores it by EVICTING the cache
	// entry — the next read re-fetches the disk image — which avoids a byte
	// copy per written page entirely.
	stmtEntFromFile
	// stmtEntAbsent records a page ALLOCATED during the statement (its number
	// exceeds the scope's starting page count): no before-image exists and
	// rollback deletes the cache entry (pager.c truncates such pages away).
	stmtEntAbsent
)

// stmtEntry is one statement-journal record: the before-image kind and, for
// stmtEntMemory, the captured page bytes.
type stmtEntry struct {
	kind stmtEntryKind
	data []byte
}

// StmtJournal is a statement-scoped rollback scope handed out by
// BeginStatement. Callers close it with exactly one of EndStatement
// (statement succeeded — entries splice into the parent scope) or
// RollbackStatement (statement failed — entries replay). Both are no-ops on
// an already-closed scope, so double-restore paths (a DML-level rollback
// followed by the engine-level rollback of the same statement) stay safe.
type StmtJournal struct {
	p      *Pager
	parent *StmtJournal
	// entries maps page number to its before-image, captured at the page's
	// first modification inside the scope (pager.c pagerAddPageToSubjournal's
	// once-per-page rule: a page rewritten N times keeps its FIRST image).
	entries map[uint32]stmtEntry
	// begin metadata: page count, file size, header bytes and the deferred
	// file-shrink flag as they stood at BeginStatement (pager.c nStmtSize /
	// stmtSize bookkeeping). Rollback reinstates them.
	numPages            uint32
	fileSize            int64
	header              []byte
	pendingFileTruncate bool
	// beginDirtyStamp is the pager's dirtyStamp at scope open; a page whose
	// dirty stamp is at or below it was already dirty when the statement
	// began and must be restored from memory, not from the stale disk image.
	beginDirtyStamp uint64
	done            bool
}

// BeginStatement opens a statement rollback scope (pager.c
// sqlite3PagerStmtBegin). Scopes nest: a trigger body's statement opens a
// scope inside the outer statement's scope. The call is O(1) — no pages are
// copied; before-images are captured lazily as pages are first modified.
func (p *Pager) BeginStatement() *StmtJournal {
	p.mu.Lock()
	defer p.mu.Unlock()
	j := &StmtJournal{
		p:                   p,
		parent:              p.stmtTop,
		entries:             make(map[uint32]stmtEntry),
		numPages:            p.numPages,
		fileSize:            p.fileSize,
		pendingFileTruncate: p.pendingFileTruncate,
		beginDirtyStamp:     p.dirtyStamp,
	}
	if p.header != nil {
		j.header = append([]byte(nil), p.header...)
	}
	p.stmtTop = j
	return j
}

// EndStatement closes a succeeded statement's scope: its journal entries
// splice into the parent scope (keeping the parent's OLDER before-image for
// pages the parent already journalled — pager.c appends the sub-journal to
// the transaction journal at statement COMMIT so the outer scope's rollback
// stays able to undo the page), and the scope's own bookkeeping is dropped.
// A scope opened at the outermost level simply discards its entries: the
// pages it wrote stay dirty for the flush/commit path.
func (p *Pager) EndStatement(j *StmtJournal) {
	if j == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if j.done {
		return
	}
	j.done = true
	p.unlinkStmtLocked(j)
	if j.parent != nil && !j.parent.done {
		for pgno, e := range j.entries {
			if _, ok := j.parent.entries[pgno]; !ok {
				j.parent.entries[pgno] = e
			}
		}
	}
}

// RollbackStatement replays a failed statement's journal (pager.c
// pager_rollback over the statement journal): every journalled page is
// restored to its before-image, pages the statement allocated are dropped,
// and the page count / file size / header / deferred-truncate bookkeeping
// return to the scope's begin state. Pages other statements of the same
// transaction wrote — and did this statement not touch — survive untouched.
func (p *Pager) RollbackStatement(j *StmtJournal) {
	if j == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if j.done {
		return
	}
	j.done = true
	p.unlinkStmtLocked(j)
	// Replay the before-images (pager.c pagerPlayback over the statement
	// journal). Each entry is independent, so map order is irrelevant.
	for pgno, e := range j.entries {
		switch e.kind {
		case stmtEntMemory:
			p.pages[pgno] = &Page{PageNum: pgno, Data: append([]byte(nil), e.data...)}
			p.dirty[pgno] = true
		case stmtEntFromFile, stmtEntAbsent:
			// from-file images restore by eviction (the disk/WAL still holds
			// the statement-start image); allocated pages never had one.
			delete(p.pages, pgno)
			delete(p.dirty, pgno)
		}
	}
	// Drop cache pages above the restored count: allocations the statement
	// made (and any bookkeeping pages created after its begin) are undone by
	// the page-count restore, mirroring Restore's eviction of unknown pages.
	for pgno := range p.pages {
		if pgno > j.numPages {
			delete(p.pages, pgno)
			delete(p.dirty, pgno)
		}
	}
	p.numPages = j.numPages
	p.pendingFileTruncate = false
	// ROLLBACK ends the write transaction: drop the WRITER shm lock
	// (sqlite3WalEndWriteTransaction parity, as in Restore; the next write
	// re-acquires it).
	p.walEndWriteLocked()
	// In WAL mode the main database file is updated only by Checkpoint, so
	// the in-memory restore above is the whole rollback (see Restore): only
	// the header and cached file size return to the begin state. The legacy
	// journal path delegates the file/header bookkeeping to
	// restoreFileImageLocked — truncate in BOTH directions (a statement may
	// have shrunk the image mid-flight, and the file must grow back),
	// header reinstatement (nil-aware), header persist, external-stamp
	// re-baselining — the same contracts full Restore documents, applied to
	// the statement's begin state. It reads s.fileSize, so p.fileSize must
	// NOT be pre-assigned here or the size-mismatch truncate is skipped.
	if p.wal == nil {
		p.restoreFileImageLocked(&PagerState{header: j.header, fileSize: j.fileSize})
	} else {
		p.header = nil
		if j.header != nil {
			p.header = append([]byte(nil), j.header...)
		}
		p.fileSize = j.fileSize
	}
}

// stmtJournalPageLocked captures pgno's before-image into the innermost open
// statement scope at its first modification inside the scope. This is the
// single choke point for page dirtying: every dirty-flag set in the pager
// goes through markDirtyLocked, which calls here first. Caller holds p.mu.
//
// Callers that mutate a page's bytes BEFORE marking it dirty must invoke
// stmtJournalPageLocked themselves before the mutation (the read path does
// this for every page it hands out, which covers the btree layer).
func (p *Pager) stmtJournalPageLocked(pgno uint32) {
	top := p.stmtTop
	if top == nil {
		return
	}
	if _, ok := top.entries[pgno]; ok {
		return
	}
	top.entries[pgno] = p.stmtCaptureEntryLocked(pgno, top)
}

// stmtCaptureEntryLocked classifies pgno's before-image for the scope.
// Caller holds p.mu.
func (p *Pager) stmtCaptureEntryLocked(pgno uint32, top *StmtJournal) stmtEntry {
	// A page above the scope's starting count was allocated inside the
	// statement — it has no before-image (pager.c never journals pages above
	// stmtSize; rollback truncates them away).
	if pgno > top.numPages {
		return stmtEntry{kind: stmtEntAbsent}
	}
	// A page already dirty at statement start carries earlier statements'
	// uncommitted writes, so the disk image is STALE for it — its statement-
	// start state exists only in memory, and it must be byte-copied (before
	// this statement's first mutation, which is the read path's job).
	// Memory pagers have no disk to fall back to either: every image is
	// captured from memory at first access.
	if p.file == nil || (p.dirty[pgno] && p.dirtyMark[pgno] <= top.beginDirtyStamp) {
		return stmtEntry{kind: stmtEntMemory, data: p.copyPageBytesLocked(pgno)}
	}
	// A clean page of a file-backed pager: its statement-start image is the
	// transaction-start image the file (or WAL) still holds — restore by
	// eviction, no bytes needed now.
	return stmtEntry{kind: stmtEntFromFile}
}

// copyPageBytesLocked snapshots a cached page's bytes (zeroed when the page
// is not cached — a fresh allocation's content). Caller holds p.mu.
func (p *Pager) copyPageBytesLocked(pgno uint32) []byte {
	if pg, ok := p.pages[pgno]; ok && pg != nil {
		return append([]byte(nil), pg.Data...)
	}
	return make([]byte, p.pageSize)
}

// stmtReadTouch captures the before-image of a page being handed out by the
// read path, BEFORE the caller can mutate it. Only pages whose statement-
// start image is memory-only need the copy: pages already dirty at scope
// begin, and every page of a memory pager. Clean pages of file-backed
// pagers skip the copy — their first dirtying journals a from-file entry at
// the markDirty choke point instead.
func (p *Pager) stmtReadTouch(pgno uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stmtReadTouchLocked(pgno)
}

// stmtReadTouchLocked is stmtReadTouch for callers already holding p.mu.
// Caller holds p.mu.
func (p *Pager) stmtReadTouchLocked(pgno uint32) {
	top := p.stmtTop
	if top == nil {
		return
	}
	if _, ok := top.entries[pgno]; ok {
		return
	}
	if p.file != nil && !p.dirty[pgno] {
		// File-backed and clean: the disk image is the before-image; the
		// dirty-mark path records the from-file entry when (and if) the page
		// is actually written.
		return
	}
	top.entries[pgno] = p.stmtCaptureEntryLocked(pgno, top)
}

// unlinkStmtLocked removes j from the open-scope chain. Caller holds p.mu.
func (p *Pager) unlinkStmtLocked(j *StmtJournal) {
	if p.stmtTop == j {
		p.stmtTop = j.parent
	} else {
		for cur := p.stmtTop; cur != nil; cur = cur.parent {
			if cur.parent == j {
				cur.parent = j.parent
				break
			}
		}
	}
}

// markDirtyLocked flags pgno as modified and journals its before-image at
// the first dirtying inside an open statement scope. Every dirty-flag set
// in the pager goes through this method — it is the statement journal's
// capture choke point. Caller holds p.mu.
func (p *Pager) markDirtyLocked(pgno uint32) {
	// Fast path: no open scope and no stale dirty stamps (dirtyMark is nil
	// outside transactions — clearDirtySetLocked drops it at every commit
	// boundary) — the statement journal needs nothing here, so keep this as
	// cheap as the raw store it replaces (write-heavy workloads dirty
	// pages millions of times).
	if p.stmtTop == nil && p.dirtyMark == nil {
		p.dirty[pgno] = true
		return
	}
	if !p.dirty[pgno] {
		p.dirtyStamp++
	}
	if p.dirtyMark == nil {
		p.dirtyMark = make(map[uint32]uint64)
	}
	p.dirtyMark[pgno] = p.dirtyStamp
	p.stmtJournalPageLocked(pgno)
	p.dirty[pgno] = true
}

// clearDirtySetLocked resets the dirty set at commit/rollback boundaries,
// dropping the per-page dirty stamps with it.
func (p *Pager) clearDirtySetLocked() {
	p.dirty = make(map[uint32]bool)
	p.dirtyMark = make(map[uint32]uint64)
}
