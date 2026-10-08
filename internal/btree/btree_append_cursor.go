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
	"sync"
	"sync/atomic"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// quickAppendSlot is one tree's append-cursor state: the rightmost leaf and
// the largest key in the tree (btree.c: a cursor parked on the last entry,
// with BTCF_ValidNKey publishing its cached nKey).
//
// The fields are ATOMICS and the slot is advisory state, not a lock: the
// quick path's real gate is verifyQuickLeaf's page-bytes check (the leaf's
// last cell rowid must equal maxKey). A reader that tears a store pair (a
// fresh maxKey under a stale leaf, or vice versa) fails that check and takes
// the generic path, which re-establishes the slot from the tree — so no
// synchronization beyond the atomics is required, and the insert hot path
// runs without cursorRegMu (btree.c reads BTCF_ValidNKey under the BtShared
// lock it already holds; the engine's funnel holds none here).
type quickAppendSlot struct {
	valid  atomic.Bool   // leaf is trusted (BTCF_ValidNKey)
	leaf   atomic.Uint32 // page holding the maximum key
	maxKey atomic.Int64  // largest key seen on this tree
}

// quickAppendReg holds one slot per (pager, rootPage) tree identity.
// Entries are created once and never deleted (the map holds at most one
// small struct per (pager, root) pair ever written), which is sync.Map's
// niche: the insert hot path's Load touches no lock, and slot creation from
// one tree cannot corrupt another tree's Load (a plain map would need
// cursorRegMu on every claim — the two mutex pairs this replaces).
var quickAppendReg sync.Map // cursorTreeKey -> *quickAppendSlot

// quickAppendHits counts quick-path engagements (test observability; the
// pin tests assert it moves, proving the fast path is taken).
var quickAppendHits atomic.Int64

// quickAppendHitsForTest returns the engagement counter (white-box tests).
func quickAppendHitsForTest() int64 { return quickAppendHits.Load() }

// quickAppendSlotFor returns the tree's slot, creating it when create is set.
func (t *BTree) quickAppendSlotFor(create bool) *quickAppendSlot {
	key := cursorTreeKey{pg: t.pager, root: t.rootPage}
	if !create {
		if slot, ok := quickAppendReg.Load(key); ok {
			return slot.(*quickAppendSlot)
		}
		return nil
	}
	slot, _ := quickAppendReg.LoadOrStore(key, &quickAppendSlot{})
	return slot.(*quickAppendSlot)
}

// invalidateAppendCursor clears the slot's leaf trust for this tree. Lock
// free: the slot fields are atomics, and an invalidation that races a claim
// is resolved by verifyQuickLeaf's page-bytes check (the generic path
// re-establishes the slot). The recorded maximum is kept: it only ever gates
// when the slot re-establishes (a conservative no-op when stale).
func (t *BTree) invalidateAppendCursor() {
	if slot := t.quickAppendSlotFor(false); slot != nil {
		slot.valid.Store(false)
	}
}

// InvalidateAppendCursor clears the append-cursor slot for a tree the caller
// mutated or dropped outside the btree package (the insert write-tree cache
// drop on a pager layout change).
func InvalidateAppendCursor(pg *pager.Pager, root uint32) {
	if pg == nil {
		return
	}
	if slot, ok := quickAppendReg.Load(cursorTreeKey{pg: pg, root: root}); ok {
		slot.(*quickAppendSlot).valid.Store(false)
	}
}

// dropQuickAppendSlot clears the slot's leaf trust for this tree (its
// recorded maximum is kept: it only gates re-establishment, and every
// engagement re-derives the truth from the page).
func (t *BTree) dropQuickAppendSlot() {
	t.invalidateAppendCursor()
}

// claimQuickAppend reads the slot for an ascending insert. ok=false means the
// generic path must run; a true non-append sighting clears the leaf trust on
// the way out (btree.c clears BTCF_ValidNKey on every non-append
// positioning).
func (t *BTree) claimQuickAppend(rowID int64) (*quickAppendSlot, bool) {
	slot := t.quickAppendSlotFor(false)
	if slot == nil || !slot.valid.Load() {
		return nil, false
	}
	if rowID <= slot.maxKey.Load() {
		slot.valid.Store(false)
		return nil, false
	}
	return slot, true
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
	slot, ok := t.claimQuickAppend(newCell.RowID)
	if !ok {
		return false, nil
	}
	leaf, maxKey := slot.leaf.Load(), slot.maxKey.Load()
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
	// dupAlreadyDropped is folded into the position argument: claimQuickAppend
	// proved newCell.RowID strictly beyond the leaf's last key (== maxKey,
	// re-verified by verifyQuickLeaf), so the cell appends at the END — the
	// position IS CellCount and both the duplicate probe and the leaf-wide
	// binary search inside writeLeafCell are provably dead work (btree.c's
	// `idx = ++pCur->ix` on the BTREE_APPEND path, src/btree.c:9612).
	if werr := t.writeLeafCell(pg, page, newCell, cellData, coff, int(page.CellCount)); werr != nil {
		t.recycleCellScratch(cellData)
		t.dropQuickAppendSlot()
		if werr == errLeafFull {
			return false, nil
		}
		return false, werr
	}
	t.recycleCellScratch(cellData)

	// Park the cursor on the appended leaf (lock-free: the slot is this
	// tree's own advisory state — see quickAppendSlot).
	slot.maxKey.Store(newCell.RowID)
	slot.leaf.Store(leaf)
	slot.valid.Store(true)
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
	slot := t.quickAppendSlotFor(true)
	if newCell.RowID <= slot.maxKey.Load() {
		return
	}
	leaf, err := t.rightmostTableLeaf()
	if err == nil && leaf != 0 {
		slot.leaf.Store(leaf)
		slot.maxKey.Store(newCell.RowID)
		slot.valid.Store(true)
	}
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
