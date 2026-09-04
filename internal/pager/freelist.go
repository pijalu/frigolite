// C-parity freelist machinery (P8.INCRVACUUM phase16).
//
// The on-disk freelist chain is the single source of truth, exactly as in
// btree.c: header[32:36] = first trunk page, header[36:40] = number of free
// pages (trunks + leaves), and each trunk page stores [0:4] = next trunk,
// [4:8] = leaf count, [8:8+4k] = leaf page numbers. There is deliberately
// NO in-memory shadow of the chain (no freePages set, no trunk/leaf maps):
// every membership query reads the chain or the pointer-map, mirroring
// allocateBtreePage / freePage2 / incrVacuumStep.
package pager

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/storage"
)

// maxTrunkLeaves returns the btree.c back-compat leaf cap: a trunk takes
// another leaf only while its leaf count is below usableSize/4 - 8
// (freePage2: "newer versions of SQLite still avoid using the last six
// entries in the freelist trunk page array"). usableSize == pageSize (the
// engine reserves no bytes).
func (p *Pager) maxTrunkLeaves() uint32 {
	return p.pageSize/4 - 8
}

// journalPageBeforeLocked appends the page's on-disk (pre-transaction)
// image to the rollback journal, mirroring sqlite3PagerWrite's
// before-image capture. Pages are flushed only at COMMIT, so the file
// image of a dirty page is still its transaction-start state — the image
// ROLLBACK must restore. Caller holds p.mu.
func (p *Pager) journalPageBeforeLocked(pgno uint32) {
	if p.journalFile == nil {
		return
	}
	off := int64(pgno-1) * int64(p.pageSize)
	before := make([]byte, p.pageSize)
	if _, err := p.file.ReadAt(before, off); err == nil {
		_ = p.appendRollbackRecordLocked(pgno, before)
	}
}

// mirrorHeaderToPage1Locked copies the header bytes into the cached page 1
// so the next flush writes them (page 1's Data and p.header are separate
// buffers). Caller holds p.mu.
func (p *Pager) mirrorHeaderToPage1Locked() {
	p.dirty[1] = true
	if pg, ok := p.pages[1]; ok && pg != nil {
		copy(pg.Data[:HeaderSize], p.header)
	}
}

// grabPageLocked returns the page for pgno, allocating a fresh zeroed
// buffer when it is not cached. A freelist pop hands the page out for
// immediate reuse (btreeGetUnusedPage + PAGER_GET_NOCONTENT semantics:
// free-page content is garbage and is not journaled). Caller holds p.mu.
func (p *Pager) grabPageLocked(pgno uint32) *Page {
	if pg, ok := p.pages[pgno]; ok && pg != nil {
		p.dirty[pgno] = true
		return pg
	}
	pg := &Page{Data: make([]byte, p.pageSize), PageNum: pgno}
	p.pages[pgno] = pg
	p.dirty[pgno] = true
	return pg
}

// freelistTrunk is one trunk page read from the chain.
type freelistTrunk struct {
	pgno   uint32
	next   uint32
	leaves []uint32
}

// readFreelistTrunkLocked reads trunk page pgno. Returns ok=false when the
// page is unreadable or not trunk-shaped (too small). Caller holds p.mu.
func (p *Pager) readFreelistTrunkLocked(pgno uint32) (freelistTrunk, bool) {
	if pgno < 2 || pgno > p.numPages {
		return freelistTrunk{}, false
	}
	pg, err := p.readPageLocked(pgno)
	if err != nil || len(pg.Data) < 8 {
		return freelistTrunk{}, false
	}
	t := freelistTrunk{pgno: pgno, next: binary.BigEndian.Uint32(pg.Data[0:4])}
	k := binary.BigEndian.Uint32(pg.Data[4:8])
	if k > p.maxTrunkLeaves()+2 {
		// Leaf count above the hard ceiling (usableSize/4 - 2) is
		// corruption in btree.c (allocateBtreePage) — treat the trunk
		// as unreadable rather than walking into the array.
		return freelistTrunk{}, false
	}
	for i := uint32(0); i < k; i++ {
		off := 8 + i*4
		if int(off)+4 > len(pg.Data) {
			break
		}
		t.leaves = append(t.leaves, binary.BigEndian.Uint32(pg.Data[off:off+4]))
	}
	return t, true
}

// writeFreelistTrunkLocked writes the trunk's next/leaves fields back to
// its page (journal first). Caller holds p.mu.
func (p *Pager) writeFreelistTrunkLocked(t freelistTrunk) {
	p.journalPageBeforeLocked(t.pgno)
	pg, err := p.readPageLocked(t.pgno)
	if err != nil {
		return
	}
	binary.BigEndian.PutUint32(pg.Data[0:4], t.next)
	binary.BigEndian.PutUint32(pg.Data[4:8], uint32(len(t.leaves)))
	for i, leaf := range t.leaves {
		off := 8 + i*4
		if int(off)+4 > len(pg.Data) {
			break
		}
		binary.BigEndian.PutUint32(pg.Data[off:off+4], leaf)
	}
	p.dirty[t.pgno] = true
}

// unlinkTrunkLocked splices trunk pgno out of the chain: the previous
// link (header[32] when prev==0, else prev trunk's next) is pointed at
// t.next. Caller holds p.mu.
func (p *Pager) unlinkTrunkLocked(prev uint32, t freelistTrunk) {
	if prev == 0 {
		p.journalPageBeforeLocked(1)
		binary.BigEndian.PutUint32(p.header[32:36], t.next)
		p.mirrorHeaderToPage1Locked()
		return
	}
	pt, ok := p.readFreelistTrunkLocked(prev)
	if !ok {
		return
	}
	pt.next = t.next
	p.writeFreelistTrunkLocked(pt)
}

// allocateFreelistLocked implements btree.c allocateBtreePage's freelist
// branch for the default allocation (closest = 0, no list search):
// decrement the count, then take the head trunk itself when it has no
// leaves, else take its FIRST leaf, copying the last leaf into the freed
// slot. Returns 0 when the chain is empty or unusable (caller extends the
// file instead). Caller holds p.mu.
func (p *Pager) allocateFreelistLocked() uint32 {
	if p.header == nil || len(p.header) < 40 {
		return 0
	}
	n := binary.BigEndian.Uint32(p.header[36:40])
	if n == 0 {
		return 0
	}
	head := binary.BigEndian.Uint32(p.header[32:36])
	t, ok := p.readFreelistTrunkLocked(head)
	if !ok {
		return 0
	}
	// Count is decremented as soon as the pop is known to succeed
	// (allocateBtreePage: put4byte(&pPage1->aData[36], n-1)).
	p.journalPageBeforeLocked(1)
	binary.BigEndian.PutUint32(p.header[36:40], n-1)
	p.mirrorHeaderToPage1Locked()
	if len(t.leaves) == 0 {
		// k == 0 && !searchList: extract the trunk page itself.
		p.journalPageBeforeLocked(t.pgno)
		binary.BigEndian.PutUint32(p.header[32:36], t.next)
		p.mirrorHeaderToPage1Locked()
		return t.pgno
	}
	// k > 0: extract leaf[closest=0]; the LAST leaf moves into slot 0
	// (allocateBtreePage: memcpy(&aData[8], &aData[4+k*4], 4)).
	popped := t.leaves[0]
	if len(t.leaves) > 1 {
		t.leaves[0] = t.leaves[len(t.leaves)-1]
	}
	t.leaves = t.leaves[:len(t.leaves)-1]
	p.writeFreelistTrunkLocked(t)
	return popped
}

// allocateFreelistNearLocked implements the searchList branch of
// allocateBtreePage (BTALLOC_EXACT / BTALLOC_LE): walk the chain looking
// for nearby itself or the first leaf satisfying the mode, allocating the
// matching trunk (with first-leaf promotion when it has leaves) or leaf.
// le selects BTALLOC_LE semantics (leaf <= nearby); otherwise only an
// exact leaf/trunk match is taken. Returns 0 when nothing matches.
// Caller holds p.mu.
func (p *Pager) allocateFreelistNearLocked(nearby uint32, le bool) uint32 {
	if p.header == nil || len(p.header) < 40 {
		return 0
	}
	n := binary.BigEndian.Uint32(p.header[36:40])
	if n == 0 {
		return 0
	}
	// The count is decremented for ANY successful allocation from the
	// list; C does it before the walk (put4byte(n-1)) and restores via
	// corruption aborts only. Mirror the pre-decrement.
	head := binary.BigEndian.Uint32(p.header[32:36])
	var prev uint32
	cur := head
	for cur != 0 {
		t, ok := p.readFreelistTrunkLocked(cur)
		if !ok {
			return 0
		}
		if cur == nearby || (le && cur < nearby) {
			// The trunk page itself is the allocation target. Source: btree.c
			// allocateBtreePage src/btree.c:6430-6490 (k==0 path unlinks the
			// trunk; k>0 path promotes leaf[0] to a new trunk). Mirror the
			// C behavior exactly: the unlink for k==0 advances the chain to
			// the next trunk; the k>0 path REPLACES the chain head with the
			// new trunk, not with t.next (which would orphan the new trunk
			// when the old one was the only trunk — P8.INCRVACUUM.S6
			// regression: the chain header was set to t.next=0 instead of
			// the promoted leaf, leaving 16 free pages reachable only by a
			// dead-on-disk trunk).
			p.journalPageBeforeLocked(1)
			binary.BigEndian.PutUint32(p.header[36:40], n-1)
			p.mirrorHeaderToPage1Locked()
			if len(t.leaves) == 0 {
				// k==0: the trunk page is the allocation target. Splice
				// the chain at prev: prev.next (or header[32] when no prev)
				// becomes t.next. The popped trunk page is now reused.
				p.unlinkTrunkLocked(prev, t)
			} else {
				// k>0: promote leaf[0] to a new trunk carrying the
				// remaining k-1 leaves. Then update the chain head
				// (header[32] when !prev, or prev.next) to point at the
				// NEW trunk, not at t.next.
				first := t.leaves[0]
				rest := t.leaves[1:]
				if first < 2 || first > p.numPages {
					// C-parity: rollback the count decrement (the
					// pre-decrement is undone on a corruption abort —
					// see btree.c allocateBtreePage's `goto end_allocate_page`
					// branch with a *pPgno=0 result).
					p.journalPageBeforeLocked(1)
					binary.BigEndian.PutUint32(p.header[36:40], n)
					p.mirrorHeaderToPage1Locked()
					return 0
				}
				p.journalPageBeforeLocked(first)
				nt := freelistTrunk{pgno: first, next: t.next, leaves: rest}
				p.writeFreelistTrunkLocked(nt)
				// Update the chain link (header[32] or prev.next) to
				// the new trunk. The old trunk is the alloc target.
				if prev == 0 {
					p.journalPageBeforeLocked(1)
					binary.BigEndian.PutUint32(p.header[32:36], first)
					p.mirrorHeaderToPage1Locked()
				} else {
					pt, pok := p.readFreelistTrunkLocked(prev)
					if !pok {
						return 0
					}
					pt.next = first
					p.writeFreelistTrunkLocked(pt)
				}
			}
			return cur
		}
		for i, leaf := range t.leaves {
			if leaf == nearby || (le && leaf <= nearby) {
				p.journalPageBeforeLocked(1)
				binary.BigEndian.PutUint32(p.header[36:40], n-1)
				p.mirrorHeaderToPage1Locked()
				// Copy the LAST leaf into the freed slot
				// (memcpy(&aData[8+closest*4], &aData[4+k*4], 4)).
				t.leaves[i] = t.leaves[len(t.leaves)-1]
				t.leaves = t.leaves[:len(t.leaves)-1]
				p.writeFreelistTrunkLocked(t)
				return leaf
			}
		}
		prev = cur
		cur = t.next
	}
	return 0
}

// TakePageFromFreelist pops a specific page off the freelist without
// handing its content to a caller — btree.c incrVacuumStep's
// allocateBtreePage(pBt, &pFreePg, &iFreePg, iLastPg, BTALLOC_EXACT)
// (src/btree.c:4025-4032): the whole chain is searched for iLastPg
// (ptrmapGet confirms it is a free page) and it is removed from wherever
// it sits.
func (p *Pager) TakePageFromFreelist(pgno uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.header == nil || len(p.header) < 40 || pgno < 2 {
		return
	}
	if p.autoVacuum {
		// BTALLOC_EXACT consults the pointer-map first: a page that is
		// not marked PTRMAP_FREEPAGE is not on the freelist.
		if eType, _, err := p.ReadPtrmapLocked(pgno); err != nil || eType != storage.PtrmapFreelist {
			return
		}
	}
	p.allocateFreelistNearLocked(pgno, false)
}

// FreePage returns a page to the freelist — btree.c freePage2 (lines
// 6797-6930): the count is incremented first, then the page is added as
// a leaf of the head trunk while the trunk has room (leaf count below
// usableSize/4 - 8); otherwise the page becomes the NEW head trunk whose
// next pointer is the previous head trunk. The freed page's content is
// left as-is (free pages hold garbage). Before-images of page 1, the
// head trunk, and the freed page are journaled for ROLLBACK.
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
		p.header = make([]byte, HeaderSize)
		copy(p.header, storage.DefaultHeader(p.pageSize).Encode())
	}
	if err := p.openRollbackJournalLocked(); err != nil {
		return err
	}
	// freePage2: nFree read, then put4byte(nFree+1) on page 1 — journal
	// page 1 and the freed page before touching them (sqlite3PagerWrite).
	p.journalPageBeforeLocked(1)
	p.journalPageBeforeLocked(pageNum)
	nFree := binary.BigEndian.Uint32(p.header[36:40])
	binary.BigEndian.PutUint32(p.header[36:40], nFree+1)
	if nFree != 0 {
		// A head trunk exists: leaf-add while it has room.
		head := binary.BigEndian.Uint32(p.header[32:36])
		if t, ok := p.readFreelistTrunkLocked(head); ok && head != pageNum &&
			uint32(len(t.leaves)) < p.maxTrunkLeaves() {
			t.leaves = append(t.leaves, pageNum)
			p.writeFreelistTrunkLocked(t)
			p.mirrorHeaderToPage1Locked()
			return nil
		}
	}
	// New head trunk: [0:4] = previous head, [4:8] = 0 leaves,
	// header[32] = the freed page (freePage2's tail block).
	pg := p.grabPageLocked(pageNum)
	binary.BigEndian.PutUint32(pg.Data[0:4], binary.BigEndian.Uint32(p.header[32:36]))
	binary.BigEndian.PutUint32(pg.Data[4:8], 0)
	p.journalPageBeforeLocked(1)
	binary.BigEndian.PutUint32(p.header[32:36], pageNum)
	p.mirrorHeaderToPage1Locked()
	return nil
}

// AllocatePageLE is the page-swap target allocator — btree.c
// allocateBtreePage(BTALLOC_LE, nearby=nFin) used by incrVacuumStep
// (src/btree.c:4051-4055): the freelist chain is searched for the first
// page at or below nearby (a trunk at or below nearby is taken whole,
// promoting its first leaf to trunk when it has leaves). Returns
// SQLITE_FULL ("database or disk full") when nothing qualifies.
func (p *Pager) AllocatePageLE(nearby uint32) (*Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pgno := p.allocateFreelistNearLocked(nearby, true); pgno != 0 {
		return p.grabPageLocked(pgno), nil
	}
	return nil, fmt.Errorf("database or disk full")
}

// IsPageOnFreelist reports whether pgno is on the freelist. C answers this
// from the pointer-map (incrVacuumStep: ptrmapGet(iLastPg) ==
// PTRMAP_FREEPAGE); non-autovacuum databases fall back to a chain walk.
func IsPageOnFreelist(p *Pager, pgno uint32) bool {
	if p == nil || pgno < 2 {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.autoVacuum {
		eType, _, err := p.ReadPtrmapLocked(pgno)
		return err == nil && eType == storage.PtrmapFreelist
	}
	return p.chainContainsLocked(pgno)
}

// ReadPtrmapLocked is ReadPtrmap for callers already holding p.mu (RLock).
func (p *Pager) ReadPtrmapLocked(pgno uint32) (byte, uint32, error) {
	ptrmapPg := storage.PtrmapPageNo(pgno, p.pageSize)
	if ptrmapPg == pgno {
		return 0, 0, fmt.Errorf("pager: pgno %d is a pointer-map page", pgno)
	}
	pg, err := p.readPageLocked(ptrmapPg)
	if err != nil {
		return 0, 0, err
	}
	return storage.PtrmapEntry(pg.Data, pgno, p.pageSize)
}

// chainContainsLocked walks the on-disk chain looking for pgno (trunk or
// leaf). Caller holds p.mu (read lock).
func (p *Pager) chainContainsLocked(pgno uint32) bool {
	if p.header == nil || len(p.header) < 40 {
		return false
	}
	n := binary.BigEndian.Uint32(p.header[36:40])
	cur := binary.BigEndian.Uint32(p.header[32:36])
	for visited := uint32(0); cur != 0 && visited <= n; visited++ {
		t, ok := p.readFreelistTrunkLocked(cur)
		if !ok {
			return false
		}
		if cur == pgno {
			return true
		}
		for _, leaf := range t.leaves {
			if leaf == pgno {
				return true
			}
		}
		cur = t.next
	}
	return false
}

// freelistPagesAboveLocked counts chain entries (trunks and leaves) whose
// page number is above n and removes them from the chain, keeping the
// count in lockstep. Used by truncatePages: pages above the truncation
// point no longer exist, so chain entries referencing them must be
// dropped, exactly like the pre-phase16 in-memory bookkeeping did.
// Caller holds p.mu.
func (p *Pager) freelistPagesAboveLocked(n uint32) uint32 {
	if p.header == nil || len(p.header) < 40 {
		return 0
	}
	total := binary.BigEndian.Uint32(p.header[36:40])
	head := binary.BigEndian.Uint32(p.header[32:36])
	removed := uint32(0)
	var prev uint32
	cur := head
	for cur != 0 {
		t, ok := p.readFreelistTrunkLocked(cur)
		if !ok {
			break
		}
		kept := t.leaves[:0]
		for _, leaf := range t.leaves {
			if leaf > n {
				removed++
			} else {
				kept = append(kept, leaf)
			}
		}
		if cur > n {
			// The trunk page itself is truncated: splice the chain at
			// prev; the kept leaves above n are gone with the file.
			removed += uint32(len(kept)) + 1
			if prev == 0 {
				p.journalPageBeforeLocked(1)
				binary.BigEndian.PutUint32(p.header[32:36], t.next)
				p.mirrorHeaderToPage1Locked()
			} else {
				pt, pok := p.readFreelistTrunkLocked(prev)
				if pok {
					pt.next = t.next
					p.writeFreelistTrunkLocked(pt)
				}
			}
			cur = t.next
			continue
		}
		if uint32(len(kept)) != uint32(len(t.leaves)) {
			t.leaves = kept
			p.writeFreelistTrunkLocked(t)
		}
		prev = cur
		cur = t.next
	}
	if removed > total {
		removed = total
	}
	if removed > 0 {
		p.journalPageBeforeLocked(1)
		binary.BigEndian.PutUint32(p.header[36:40], total-removed)
		p.mirrorHeaderToPage1Locked()
	}
	return removed
}
