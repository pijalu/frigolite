// Append-cursor fast path for table-btree inserts (PERF.INSQUICK).
//
// SQLite's insert loop reuses a cursor parked on the rightmost leaf across
// successive intkey inserts: BTCF_ValidNKey lets sqlite3BtreeInsert skip the
// moveto entirely for an ascending append (src/btree.c:9448, the comment at
// src/btree.c:9613 on "leave the cursor pointing to the last entry"), and
// balance_quick keeps right-end splits O(1). This file ports the same
// discipline to the BTree wrapper's InsertCell entry: a per-(pager, root)
// slot remembers the rightmost leaf and its largest key, so an insert whose
// key exceeds that maximum appends to the saved leaf WITHOUT descending from
// the root.
//
// frigolite builds a fresh wrapper per statement over the same (pager,
// rootPage) pair — that pair is the BtShared identity — so the slot lives in
// a registry keyed on it (the same key the cross-statement cursor registry
// uses, and the same mutex guards both maps). The executor's cached insert
// write tree (execdml insertWriteTree) keeps ONE wrapper alive across
// statements, so the slot survives across statements inside a transaction,
// which is exactly the shape that pays.
//
// Invalidation discipline (mirrors btree.c cursor invalidation):
//   - any insert that is not a rightmost append clears the slot (btree.c
//     clears BTCF_ValidNKey on every non-append positioning);
//   - every delete entry point clears the slot (btreeFdpCursorClear /
//     dropCell paths invalidate the cursor's key validity);
//   - Clear (sqlite3BtreeClearTable) clears the slot;
//   - BTree.Close clears the slot for its tree: statement teardown drops
//     function-local wrappers after DELETE/UPDATE/VACUUM statements, so any
//     mutation made through a DIFFERENT wrapper invalidates the insert
//     wrapper's slot at that statement's boundary (the btree.c property that
//     every statement's writes go through saveAllCursors);
//   - a page-layout replacement (VACUUM/backup) drops the cached wrapper via
//     execdml's layout hook, and Close clears the slot with it.
//
// Because a journal ROLLBACK or SAVEPOINT rollback rewrites page images in
// place WITHOUT running btree code, the quick path additionally re-verifies
// the saved leaf on every engagement: the page must still be a table leaf,
// and its last cell's rowid must equal the slot's recorded maximum. Any
// mutation the slot did not observe (a rollback, a page reallocated by a
// DROP/CREATE cycle, a corrupted image) fails the check and falls back to
// the generic root-descent insert, which re-establishes the slot when the
// insert lands at the right edge.

package btree

import (
	"fmt"
	"sync/atomic"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// quickAppendSlot is one tree's append-cursor state: the rightmost leaf and
// the largest key in the tree (btree.c: a cursor parked on the last entry,
// with BTCF_ValidNKey publishing its cached nKey).
type quickAppendSlot struct {
	valid  bool   // leaf is trusted (BTCF_ValidNKey)
	leaf   uint32 // page holding the maximum key
	maxKey int64  // largest key seen on this tree
}

// quickAppendReg holds one slot per (pager, rootPage) tree identity. Guarded
// by cursorRegMu (shared with the cursor registry: the insert hot path takes
// this mutex once for the slot check, and saveAllCursors — also required per
// insert — takes it again; one lock domain keeps the ordering trivial).
// Entries are never deleted, only flagged: the map holds at most one small
// struct per (pager, root) pair ever written.
var quickAppendReg = map[cursorTreeKey]*quickAppendSlot{}

// quickAppendHits counts quick-path engagements (test observability; the
// pin tests assert it moves, proving the fast path is taken).
var quickAppendHits atomic.Int64

// quickAppendHitsForTest returns the engagement counter (white-box tests).
func quickAppendHitsForTest() int64 { return quickAppendHits.Load() }

// quickSlotLocked returns the tree's slot, creating it when create is set.
// Caller must hold cursorRegMu.
func (t *BTree) quickSlotLocked(create bool) *quickAppendSlot {
	key := cursorTreeKey{pg: t.pager, root: t.rootPage}
	slot := quickAppendReg[key]
	if slot == nil && create {
		slot = &quickAppendSlot{}
		quickAppendReg[key] = slot
	}
	return slot
}

// invalidateAppendCursorLocked clears the slot's leaf trust for this tree.
// Caller must hold cursorRegMu. The recorded maximum is kept: it only ever
// gates when the slot re-establishes (a conservative no-op when stale, and
// the quick path's verification re-derives the truth from the page anyway).
func (t *BTree) invalidateAppendCursorLocked() {
	if slot := quickAppendReg[cursorTreeKey{pg: t.pager, root: t.rootPage}]; slot != nil {
		slot.valid = false
	}
}

// InvalidateAppendCursor clears the append-cursor slot for a tree the caller
// mutated or dropped outside the btree package (the insert write-tree cache
// drop on a pager layout change).
func InvalidateAppendCursor(pg *pager.Pager, root uint32) {
	if pg == nil {
		return
	}
	cursorRegMu.Lock()
	if slot := quickAppendReg[cursorTreeKey{pg: pg, root: root}]; slot != nil {
		slot.valid = false
	}
	cursorRegMu.Unlock()
}

// dropQuickAppendSlot clears the slot's leaf trust for this tree (its
// recorded maximum is kept: it only gates re-establishment, and every
// engagement re-derives the truth from the page).
func (t *BTree) dropQuickAppendSlot() {
	cursorRegMu.Lock()
	t.invalidateAppendCursorLocked()
	cursorRegMu.Unlock()
}

// claimQuickAppend reads the slot for an ascending insert. ok=false means the
// generic path must run; a true non-append sighting clears the leaf trust on
// the way out (btree.c clears BTCF_ValidNKey on every non-append
// positioning).
func (t *BTree) claimQuickAppend(rowID int64) (leaf uint32, maxKey int64, ok bool) {
	cursorRegMu.Lock()
	defer cursorRegMu.Unlock()
	slot := quickAppendReg[cursorTreeKey{pg: t.pager, root: t.rootPage}]
	if slot == nil || !slot.valid {
		return 0, 0, false
	}
	if rowID <= slot.maxKey {
		slot.valid = false
		return 0, 0, false
	}
	return slot.leaf, slot.maxKey, true
}

// verifyQuickLeaf re-validates the saved leaf against the slot's claim and
// returns its page, parsed header and content offset. This is the guard that
// stands in for btree.c's cursor state machine: a journal/savepoint rollback,
// a page reallocated by a DROP/CREATE cycle, or any mutation the slot did not
// observe breaks one of these conditions and the insert falls back to the
// generic path.
func (t *BTree) verifyQuickLeaf(leaf uint32, maxKey int64) (*pager.Page, *storage.BTreePage, int, bool) {
	pg, err := t.pager.ReadPage(leaf)
	if err != nil {
		return nil, nil, 0, false
	}
	coff := contentOffset(pg.PageNum)
	page := &t.quickPageScratch
	if _, perr := storage.ParsePageInto(pg.Data, int(t.pageSize), coff, page); perr != nil {
		return nil, nil, 0, false
	}
	if pg.PageNum != leaf || page.PageType != storage.PageTypeLeafTable ||
		page.CellCount == 0 ||
		t.tableLeafRowidAt(pg, coff, int(page.CellCount)-1) != maxKey {
		return nil, nil, 0, false
	}
	return pg, page, coff, true
}

// insertQuickAppend attempts the O(1) rightmost-leaf append. It returns
// handled=true when the cell was written (the caller is done), and
// handled=false when the generic root-descent path must run — always after
// clearing the slot when it held state. A non-nil error is a real I/O or
// corruption error the caller must propagate (the generic path would hit the
// same page).
func (t *BTree) insertQuickAppend(newCell *storage.Cell) (bool, error) {
	if newCell.Type != storage.CellTableLeaf || newCell.RowID < 0 {
		return false, nil
	}
	leaf, maxKey, ok := t.claimQuickAppend(newCell.RowID)
	if !ok {
		return false, nil
	}
	// Spilling cells need overflow allocation and usually the split
	// machinery: bail BEFORE preparing the cell (prepareCell is a no-op for
	// a local cell, but the bail must not leave a half-allocated chain).
	if storage.LocalPayloadSize(len(newCell.Payload), int(t.usableSize), storage.CellTableLeaf) < len(newCell.Payload) {
		t.dropQuickAppendSlot()
		return false, nil
	}
	pg, page, coff, ok := t.verifyQuickLeaf(leaf, maxKey)
	if !ok {
		t.dropQuickAppendSlot()
		return false, nil
	}
	if perr := t.prepareCell(newCell, leaf); perr != nil {
		return false, perr
	}
	cellData := t.encodeCellScratch(newCell)
	if !leafHasRoom(pg, page, cellData, coff, t.usableSize) {
		t.recycleCellScratch(cellData)
		// The leaf filled up: the split belongs to the generic path
		// (balance machinery), which re-establishes the slot afterwards.
		t.dropQuickAppendSlot()
		return false, nil
	}

	// In-place page mutation can move cells (defragment-on-demand): save
	// open cursors first, exactly like the generic entry does.
	t.saveAllCursors()
	if werr := t.writeLeafCell(pg, page, newCell, cellData, coff); werr != nil {
		t.recycleCellScratch(cellData)
		t.dropQuickAppendSlot()
		if werr == errLeafFull {
			return false, nil
		}
		return false, werr
	}
	t.recycleCellScratch(cellData)

	cursorRegMu.Lock()
	if slot := quickAppendReg[cursorTreeKey{pg: t.pager, root: t.rootPage}]; slot != nil {
		slot.maxKey = newCell.RowID
		slot.leaf = leaf
		slot.valid = true
	}
	cursorRegMu.Unlock()
	quickAppendHits.Add(1)
	return true, nil
}

// noteAppendInsert maintains the slot after a GENERIC-path insert. A cell
// that becomes the new maximum landed on the rightmost leaf: descend there
// once and park the append cursor on it, so every subsequent rightmost
// insert skips the root descent the quick path exists for. Out-of-order
// inserts only refresh the recorded maximum bound (keeping the slot
// untrusted).
func (t *BTree) noteAppendInsert(newCell *storage.Cell) {
	if newCell.Type != storage.CellTableLeaf || newCell.RowID < 0 {
		return
	}
	cursorRegMu.Lock()
	slot := t.quickSlotLocked(true)
	if newCell.RowID <= slot.maxKey {
		cursorRegMu.Unlock()
		return
	}
	slot.maxKey = newCell.RowID
	cursorRegMu.Unlock()

	leaf, err := t.rightmostTableLeaf()
	cursorRegMu.Lock()
	if err == nil && leaf != 0 {
		slot.leaf = leaf
		slot.maxKey = newCell.RowID
		slot.valid = true
	}
	cursorRegMu.Unlock()
}

// rightmostTableLeaf descends from the root along rightmost-child pointers
// to the leaf holding the tree's maximum key.
func (t *BTree) rightmostTableLeaf() (uint32, error) {
	pageNum := t.rootPage
	var sp storage.BTreePage
	for {
		pg, err := t.pager.ReadPage(pageNum)
		if err != nil {
			return 0, err
		}
		page, err := storage.ParsePageInto(pg.Data, int(t.pageSize), contentOffset(pg.PageNum), &sp)
		if err != nil {
			return 0, err
		}
		if page.PageType != storage.PageTypeInteriorTable {
			return pageNum, nil
		}
		if page.RightmostPtr == 0 {
			return 0, fmt.Errorf("btree: interior page %d has no rightmost child", pageNum)
		}
		pageNum = page.RightmostPtr
	}
}
