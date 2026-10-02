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

import (
	"github.com/pijalu/frigolite/internal/quota"
)

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

// Before-image buffers recycle through the pager's imageFree list (see
// stmtImageBuf/putStmtImageBuf): capture and drop both hold p.mu, and each
// buffer is referenced exclusively by its stmtEntry between the two, so the
// pager can hand the same page-size buffers out statement after statement
// with no allocation in the steady state. (The sync.Pool this replaced boxed
// a fresh *[]byte on every Put — one heap allocation per captured page,
// several per point write on the in-memory path.)
//
// Lifetime discipline — the reason this is safe:
//
//   - A buffer is acquired ONLY in copyPageBytesLocked, written in full, and
//     from then on referenced exclusively by its stmtEntry (never aliased
//     into the page cache: rollback RESTORES by adopting the buffer as the
//     restored page's Data, transferring ownership, or by eviction — a
//     free-list buffer never becomes a page's bytes through a copy that
//     leaves the original shared).
//   - A buffer is returned to the free list ONLY at the point its entry is
//     dropped: EndStatement discards it (the parent already holds an older
//     image for the page, or there is no parent to splice into), or — never
//     — after a rollback (adopted buffers belong to the restored page; the
//     list refills through make() at the next capture).
//   - Every scope closes exactly once (the done flag makes the second of
//     EndStatement/RollbackStatement a no-op), so each buffer is returned at
//     most once, and no caller observes an entry after its scope closed.

// stmtImageFreeCap bounds the pager's before-image free list (16 page-size
// buffers: deeper statement footprints fall back to GC, as the sync.Pool
// did).
const stmtImageFreeCap = 16

// stmtImageBuf returns a page-size buffer for a before-image (recycled when
// one of the right size is available). Caller holds p.mu.
func (p *Pager) stmtImageBuf() []byte {
	for n := len(p.imageFree) - 1; n >= 0; n-- {
		b := p.imageFree[n]
		p.imageFree[n] = nil
		p.imageFree = p.imageFree[:n]
		if cap(b) >= int(p.pageSize) {
			return b[:p.pageSize]
		}
	}
	return make([]byte, p.pageSize)
}

// putStmtImageBuf returns a dead before-image buffer to the free list.
// Buffers of the wrong capacity (a pager whose page size changed) are
// dropped. Caller holds p.mu.
func (p *Pager) putStmtImageBuf(b []byte) {
	if cap(b) != int(p.pageSize) {
		return
	}
	if len(p.imageFree) < stmtImageFreeCap {
		p.imageFree = append(p.imageFree, b[:cap(b)])
	}
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
	// fullState marks a quota-layer scope: with the quota shim active a
	// flush can fail in the middle of writing pages to the FILE (after the
	// cache state the lazy journal models is already stale), so the scope
	// keeps a whole-state Snapshot and rollback reinstates it via Restore —
	// the pre-journal P5 semantics. Test-harness only; nil in production.
	fullState *PagerState
}

// BeginStatement opens a statement rollback scope (pager.c
// sqlite3PagerStmtBegin). Scopes nest: a trigger body's statement opens a
// scope inside the outer statement's scope. The call is O(1) — no pages are
// copied; before-images are captured lazily as pages are first modified.
//
// Scope objects recycle through the pager's stmtFree list: a closed scope is
// pushed back by its closer, and the next BeginStatement pops it. This is
// safe because scope tokens are never touched after their close — the engine
// closes every scope it opened before the next statement begins on the same
// pager (defers run at statement end), and a stale second close lands on the
// still-done object (a no-op) in the window before reuse.
func (p *Pager) BeginStatement() *StmtJournal {
	if quota.Active() {
		// Quota layer (test_quota.c shim): a flush can fail mid-way through
		// writing pages to the FILE — quota refusal happens per page write,
		// so the file, the journal sidecar and the cache are in a state the
		// lazily-captured journal does not model. Keep the legacy whole-state
		// statement snapshot there (Restore reinstates it wholesale).
		return &StmtJournal{p: p, done: false, fullState: p.Snapshot()}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var j *StmtJournal
	if n := len(p.stmtFree); n > 0 {
		j = p.stmtFree[n-1]
		p.stmtFree[n-1] = nil
		p.stmtFree = p.stmtFree[:n-1]
		clear(j.entries)
	} else {
		j = &StmtJournal{entries: make(map[uint32]stmtEntry)}
	}
	j.p = p
	j.parent = p.stmtTop
	j.numPages = p.numPages
	j.fileSize = p.fileSize
	j.pendingFileTruncate = p.pendingFileTruncate
	j.beginDirtyStamp = p.dirtyStamp
	j.done = false
	j.fullState = nil
	if p.header != nil {
		// Reuse the recycled scope's header buffer (the close path keeps its
		// capacity) — one page-header copy, no fresh allocation.
		j.header = append(j.header[:0], p.header...)
	} else {
		j.header = nil
	}
	p.stmtTop = j
	return j
}

// recycleStmtLocked returns a just-closed scope's object to the free list
// (bounded: deeper recycling falls back to GC). The entries map and header
// buffer keep their storage for the next BeginStatement. Caller holds p.mu.
func (p *Pager) recycleStmtLocked(j *StmtJournal) {
	if len(p.stmtFree) < 8 {
		p.stmtFree = append(p.stmtFree, j)
	}
}

// EndStatement closes a succeeded statement's scope: its journal entries
// splice into the parent scope (keeping the parent's OLDER before-image for
// pages the parent already journalled — pager.c appends the sub-journal to
// the transaction journal at statement COMMIT so the outer scope's rollback
// stays able to undo the page), and the scope's own bookkeeping is dropped.
// A scope opened at the outermost level simply discards its entries: the
// pages it wrote stay dirty for the flush/commit path. Discarded memory
// images go back to the buffer pool; spliced entries move (the parent owns
// them now).
func (p *Pager) EndStatement(j *StmtJournal) {
	if j == nil {
		return
	}
	if j.fullState != nil {
		j.done = true
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if j.done {
		return
	}
	j.done = true
	p.unlinkStmtLocked(j)
	defer p.recycleStmtLocked(j)
	if j.parent != nil && !j.parent.done {
		for pgno, e := range j.entries {
			if _, ok := j.parent.entries[pgno]; !ok {
				j.parent.entries[pgno] = e
				continue
			}
			// The parent keeps its older image; this one is dead.
			p.dropStmtEntry(e)
		}
		return
	}
	// No parent to splice into (outermost scope, or the parent already
	// closed): every entry dies here.
	for _, e := range j.entries {
		p.dropStmtEntry(e)
	}
}

// dropStmtEntry releases a dead statement-journal entry: memory before-images
// return their buffers to the pool. Caller holds p.mu.
func (p *Pager) dropStmtEntry(e stmtEntry) {
	if e.kind == stmtEntMemory {
		p.putStmtImageBuf(e.data)
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
	if j.fullState != nil {
		j.done = true
		p.Restore(j.fullState)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if j.done {
		return
	}
	j.done = true
	p.unlinkStmtLocked(j)
	p.replayStmtEntriesLocked(j)
	p.recycleStmtLocked(j)
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

// replayStmtEntriesLocked replays a failed statement's before-images
// (pager.c pagerPlayback over the statement journal). Each entry is
// independent, so map order is irrelevant. Memory images are restored by
// ADOPTING the captured buffer as the restored page's data: the entry (its
// only remaining reference) dies with this scope, so ownership transfers
// cleanly and no copy is needed. The evicted page object's bytes are NOT
// returned to the pool — their lifetime is ambiguous (handles taken from
// ReadPage before the statement may outlive the scope) — GC reclaims them.
// Caller holds p.mu.
func (p *Pager) replayStmtEntriesLocked(j *StmtJournal) {
	for pgno, e := range j.entries {
		switch e.kind {
		case stmtEntMemory:
			if cap(e.data) >= int(p.pageSize) && len(e.data) >= int(p.pageSize) {
				p.pages[pgno] = &Page{PageNum: pgno, Data: e.data[:p.pageSize]}
			} else {
				// Capture always stores exactly page-size images; this is
				// the defensive fallback for any shape that slipped past
				// the invariant.
				p.pages[pgno] = &Page{PageNum: pgno, Data: append([]byte(nil), e.data...)}
			}
			p.dirty[pgno] = true
		case stmtEntFromFile, stmtEntAbsent:
			// from-file images restore by eviction (the disk/WAL still holds
			// the statement-start image); allocated pages never had one.
			delete(p.pages, pgno)
			delete(p.dirty, pgno)
		}
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
// is not cached — a fresh allocation's content) into a pooled before-image
// buffer. Caller holds p.mu.
func (p *Pager) copyPageBytesLocked(pgno uint32) []byte {
	buf := p.stmtImageBuf()
	if pg, ok := p.pages[pgno]; ok && pg != nil {
		n := copy(buf, pg.Data)
		if n < len(buf) {
			clear(buf[n:]) // short page data: the tail is the zeroed fresh page
		}
		return buf
	}
	clear(buf)
	return buf
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
	// Fast path: no open scope and no stale dirty stamps (dirtyMark is
	// empty outside transactions — clearDirtySetLocked clears it at every
	// commit boundary) — the statement journal needs nothing here, so keep
	// this as cheap as the raw store it replaces (write-heavy workloads
	// dirty pages millions of times).
	if p.stmtTop == nil && len(p.dirtyMark) == 0 {
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
//
// Both maps are reused in place (small sets: clear() keeps the buckets); a
// huge transaction's bucket arrays are traded in. The dirty-stamp map reads
// as empty afterwards, which is what markDirtyLocked's fast path requires —
// the maps were freshly allocated per boundary before, which made two map
// allocations the pager's per-statement floor on read-mostly workloads
// (every autocommit SELECT ends in a flush).
func (p *Pager) clearDirtySetLocked() {
	if len(p.dirty) > 1024 {
		p.dirty = make(map[uint32]bool)
	} else {
		clear(p.dirty)
	}
	if len(p.dirtyMark) > 1024 {
		p.dirtyMark = make(map[uint32]uint64)
	} else {
		clear(p.dirtyMark)
	}
}
