// Interior-page cell management: applying child-split separator chains,
// splitting a full interior page, and adding/locating divider cells.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// cellPtrOffset returns the cell pointer array offset for a given page type.
// Interior pages have a 12-byte header (rightmost pointer at bytes 8-11),
// so cell pointers start at byte 12. Leaf pages have an 8-byte header.
func cellPtrOffset(pageType byte) int {
	if pageType == storage.PageTypeInteriorTable || pageType == storage.PageTypeInteriorIndex {
		return 12
	}
	return 8
}

// cellKeyAt reads the divider key of the interior cell at pointer index idx.
func (t *BTree) cellKeyAt(pg *pager.Page, ptrBase, idx int) uint64 {
	off := int(binary.BigEndian.Uint16(pg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
	k, _ := util.GetVarint(pg.Data[off+4:])
	return k
}

func (t *BTree) encodeInteriorCell(leftChild uint32, rowID uint64) []byte {
	ridLen := util.VarintLen(rowID)
	buf := make([]byte, 4+ridLen)
	binary.BigEndian.PutUint32(buf[:4], leftChild)
	util.PutVarint(buf[4:], rowID)
	return buf
}

// encodeDividerCell encodes one interior divider cell for the given split
// result: a table b-tree's divider is (leftChild, rowid varint); an index
// b-tree's divider is a full index-interior cell carrying the separator
// record payload (btree.c copies the right sibling's first cell into the
// parent, src/btree.c:8820), so interior descent and sqlite3 integrity_check
// see value-ordered separators. A payload exceeding the index-page local
// maximum spills to a FRESH overflow chain owned by the parent page — the
// leaf entry's chain stays untouched (balance re-parents each moved cell's
// chain to its final owner, ptrmapPutOvflPtr src/btree.c:8025).
func (t *BTree) encodeDividerCell(leftChild uint32, res leafSplitResult, ownerPgno uint32) ([]byte, error) {
	if t.isTable {
		return t.encodeInteriorCell(leftChild, res.medianKey), nil
	}
	// Index dividers encode the legacy compact shape (child + payload-length
	// varint, no payload bytes): carrying the full separator payload in the
	// parent destabilized the balance paths at scale. See splitMedianKey.
	return t.encodeInteriorCell(leftChild, uint64(len(res.medianPayload))), nil
	// A divider key that would spill to overflow is encoded with an EMPTY
	// payload (plen 0): empty sorts before every record, so descent routes
	// every insert to the divider's right subtree, where the split placed
	// the spilled-key leaf. This keeps dividers chain-free — no shared or
	// relocated overflow bookkeeping on interior pages. (Single-leaf trees
	// and locally-fitting dividers — the overwhelming majority — carry the
	// full separator payload.)
	plen := len(res.medianPayload)
	if storage.LocalPayloadSize(plen, int(t.usableSize), storage.CellIndexLeaf) < plen {
		buf := make([]byte, 5)
		binary.BigEndian.PutUint32(buf, leftChild)
		buf[4] = 0
		return buf, nil
	}
	return storage.EncodeCell(&storage.Cell{
		Type:    storage.CellIndexInterior,
		LeftPtr: leftChild,
		Payload: res.medianPayload,
	}), nil
}

// mustEncodeDividerCell is encodeDividerCell for re-encode sites that
// already validated the divider fits: an overflow-allocation failure falls
// back to a local-only cell rather than corrupting the page.
func (t *BTree) mustEncodeDividerCell(leftChild uint32, res leafSplitResult, ownerPgno uint32) []byte {
	data, err := t.encodeDividerCell(leftChild, res, ownerPgno)
	if err != nil {
		return storage.EncodeCell(&storage.Cell{
			Type:    storage.CellIndexInterior,
			LeftPtr: leftChild,
			Payload: res.medianPayload,
		})
	}
	return data
}

// dividerCellLen reports the encoded size of one divider cell (the room
// prechecks sum these).
func (t *BTree) dividerCellLen(res leafSplitResult) int {
	if t.isTable {
		return 4 + util.VarintLen(res.medianKey)
	}
	return 4 + util.VarintLen(uint64(len(res.medianPayload)))
}

// freeInteriorDividerChains releases the overflow chains owned by every
// divider cell of an interior page that is about to be REWRITTEN (balance
// parity: balance_nonroot's apCell relocations free displaced cells'
// overflow via freePageChain). Leaf entries reference their own chains and
// are untouched.
func (t *BTree) freeInteriorDividerChains(pg *pager.Page, page *storage.BTreePage) error {
	if t.isTable || page == nil {
		return nil
	}
	ptrBase := contentOffset(pg.PageNum) + cellPtrOffset(page.PageType)
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+i*2 : ptrBase+i*2+2]))
		cell, err := storage.DecodeCell(pg.Data, cellOff, storage.CellIndexInterior, int(t.usableSize))
		if err != nil || cell.Overflow == 0 {
			continue
		}
		if err := t.freeOverflowChain(cell.Overflow); err != nil {
			return err
		}
	}
	return nil
}

// dividerFullPayload reassembles an interior divider cell's full record
// payload, following its overflow chain when the key spills.
func (t *BTree) dividerFullPayload(pg *pager.Page, cellOff int) ([]byte, error) {
	cell, err := storage.DecodeCell(pg.Data, cellOff, storage.CellIndexInterior, int(t.usableSize))
	if err != nil {
		return nil, err
	}
	full, err := t.readOverflow(cell)
	if err != nil {
		return nil, err
	}
	return full.Payload, nil
}

// cellDividerPayloadAt returns an INDEX interior cell's divider payload (the
// full separator record bytes, overflow chain reassembled) for the cell
// pointer at ptrBase+idx.
func (t *BTree) cellDividerPayloadAt(pg *pager.Page, ptrBase, idx int) []byte {
	cellOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
	cell, err := storage.DecodeCell(pg.Data, cellOff, storage.CellIndexInterior, int(t.usableSize))
	if err != nil {
		return nil
	}
	full, err := t.readOverflow(cell)
	if err != nil {
		return nil
	}
	return full.Payload
}

// dividerLess reports whether the divider cell at cellOff sorts at or before
// the given new split divider (addInteriorCell places the new separator
// AFTER every cell it is not less than).
func (t *BTree) dividerLess(pg *pager.Page, cellOff int, newSplit leafSplitResult) bool {
	if t.isTable {
		ekey, _ := util.GetVarint(pg.Data[cellOff+4:])
		return ekey <= newSplit.medianKey
	}
	payload, err := t.dividerFullPayload(pg, cellOff)
	if err != nil {
		return false
	}
	return t.compareKey(payload, newSplit.medianPayload) <= 0
}

// rekeyCarrierChainIndex is rekeyCarrierChain for INDEX b-trees: dividers
// carry record payloads, so the relocated cells re-encode with the split's
// separator payload and the new sibling cell carries carrierPayload.
func (t *BTree) rekeyCarrierChainIndex(pg *pager.Page, page *storage.BTreePage, coff, ptroff, ptrBase, idx int, origChild uint32, carrierPayload []byte, splits []leafSplitResult) (int, error) {
	deadBytes := 0
	for si, cs := range splits {
		curLeft := origChild
		if si > 0 {
			curLeft = splits[si-1].pageNum
		}
		_ = curLeft // the located cell's left child already equals curLeft
		// Re-key by RELOCATING the cell (a wider divider must never be
		// written in place — it would overrun the neighbor).
		curChildOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
		curChild := binary.BigEndian.Uint32(pg.Data[curChildOff : curChildOff+4])
		// Divider cells are chain-free (see encodeDividerCell), so the
		// replaced cell contributes only its on-page bytes.
		oldOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
		if oldCell, derr := storage.DecodeCell(pg.Data, oldOff, storage.CellIndexInterior, int(t.usableSize)); derr == nil {
			deadBytes += 4 + util.VarintLen(uint64(oldCell.PayloadLen)) + len(oldCell.Payload)
		}
		rekeyed, eerr := t.encodeDividerCell(curChild, cs, pg.PageNum)
		if eerr != nil {
			return deadBytes, eerr
		}
		rkStart := int(page.CellContent) - len(rekeyed)
		if rkStart < coff+ptroff+(int(page.CellCount)+1)*2+2 {
			return deadBytes, errInteriorFull
		}
		copy(pg.Data[rkStart:], rekeyed)
		binary.BigEndian.PutUint16(pg.Data[ptrBase+idx*2:], uint16(rkStart))
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(rkStart))
		page.CellContent = uint16(rkStart)
		// Insert the new sibling cell AFTER it, carrying carrierPayload.
		newData, eerr := t.encodeDividerCell(cs.pageNum, leafSplitResult{medianPayload: carrierPayload}, pg.PageNum)
		if eerr != nil {
			return deadBytes, eerr
		}
		ncStart := int(page.CellContent) - len(newData)
		nCount := int(page.CellCount) + 1
		ncPtrEnd := coff + ptroff + nCount*2 + 2
		if ncStart < ncPtrEnd {
			return deadBytes, errInteriorFull
		}
		copy(pg.Data[ncStart:], newData)
		page.CellContent = uint16(ncStart)
		shiftCellPtrsRight(pg.Data, ptrBase, idx+1, int(page.CellCount))
		binary.BigEndian.PutUint16(pg.Data[ptrBase+(idx+1)*2:], uint16(ncStart))
		page.CellCount = uint16(nCount)
		binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], uint16(nCount))
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(ncStart))
		idx++
	}
	return deadBytes, nil
}

// errInteriorFull signals that an interior page has no room for the
// separator cells of a child split.
var errInteriorFull = fmt.Errorf("btree: interior page full, cannot add child pointer")

// applyChildSplits updates this interior page after a child page split into
// len(splits)+1 pages. SQLite's balance_nonroot semantics for a table b-tree:
// the parent cell that pointed at the splitting child keeps its LEFT child
// but gets its divider KEY replaced by the first split's median; each new
// sibling page is inserted right after it carrying the PREVIOUS upper bound,
// so the last new cell ends up with the original divider as its key and no
// unrelated subtree pointer is ever touched.
//
//	cell_j = (C, Kold)            →  (C, D1), (P1, D2), …, (Pn-1, Kold)
func (t *BTree) applyChildSplits(pg *pager.Page, page *storage.BTreePage, origChild uint32, splits []leafSplitResult) error {
	coff := contentOffset(pg.PageNum)
	ptroff := cellPtrOffset(page.PageType)
	ptrBase := coff + ptroff

	// Locate the cell pointing at the original child.
	idx := findChildCellIndex(pg, page, ptrBase, origChild)
	if idx < 0 && page.RightmostPtr == origChild {
		// The split child is the rightmost pointer: it has no divider cell.
		return t.applyChildSplitsRightmost(pg, page, coff, ptroff, ptrBase, origChild, splits)
	}
	if idx < 0 {
		return fmt.Errorf("btree: parent %d has no cell for split child %d", pg.PageNum, origChild)
	}

	// EXACT room precheck — applyChildSplits must be ATOMIC: a mid-chain
	// errInteriorFull would leave partially-mutated page data visible through
	// the pager cache, and the retry-after-parent-split then desyncs the
	// structure and orphans whole subtrees (fts4opt churn: keys stranded on
	// unreachable pages).
	if err := t.childSplitsHaveRoom(pg, page, coff, ptroff, ptrBase, idx, splits); err != nil {
		return err
	}

	if t.isTable {
		// Carry the upper bound through the chain: the ORIGINAL cell's key.
		carrierKey := t.cellKeyAt(pg, ptrBase, idx)
		deadBytes, err := t.rekeyCarrierChain(pg, page, coff, ptroff, ptrBase, idx, origChild, carrierKey, splits)
		if err != nil {
			return err
		}
		if deadBytes > 0 {
			// Every re-key RELOCATED a divider, abandoning its old bytes above
			// the new content start. Those bytes are inside the content area but
			// belong to no cell — untracked free space that sqlite3
			// integrity_check reports as "Fragmentation of N bytes reported as
			// M". Repack the surviving dividers contiguously (defragmentPage
			// parity) so no untracked hole remains.
			if err := t.defragmentInterior(pg, page); err != nil {
				return err
			}
		}
		return t.pager.WritePage(pg)
	}
	// Index b-tree: the carrier is the ORIGINAL divider's record payload; the
	// chain re-encodes divider cells with their new separator payloads.
	// Cloned: the chain's relocations rewrite pg.Data under it.
	carrierPayload := append([]byte(nil), t.cellDividerPayloadAt(pg, ptrBase, idx)...)
	deadBytes, err := t.rekeyCarrierChainIndex(pg, page, coff, ptroff, ptrBase, idx, origChild, carrierPayload, splits)
	if err != nil {
		return err
	}
	if deadBytes > 0 {
		if err := t.defragmentInterior(pg, page); err != nil {
			return err
		}
	}
	return t.pager.WritePage(pg)
}

// findChildCellIndex returns the index of the interior cell whose left child
// is origChild, or -1 when no cell points at it.
func findChildCellIndex(pg *pager.Page, page *storage.BTreePage, ptrBase int, origChild uint32) int {
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+i*2 : ptrBase+i*2+2]))
		if binary.BigEndian.Uint32(pg.Data[cellOff:cellOff+4]) == origChild {
			return i
		}
	}
	return -1
}

// applyChildSplitsRightmost handles a split child referenced only by the
// rightmost pointer: it has no divider cell, so one divider cell per split
// page is appended and the rightmost pointer moves to the LAST new page:
//
//	rightmost=C, splits=[(P1,D1)..(Pn,Dn)]  →
//	cells += (C,D1),(P1,D2)…(Pn-1,Dn);  rightmost = Pn
func (t *BTree) applyChildSplitsRightmost(pg *pager.Page, page *storage.BTreePage, coff, ptroff, ptrBase int, origChild uint32, splits []leafSplitResult) error {
	for si := 0; si < len(splits); si++ {
		leftOfCell := origChild
		if si > 0 {
			leftOfCell = splits[si-1].pageNum
		}
		newData, eerr := t.encodeDividerCell(leftOfCell, splits[si], pg.PageNum)
		if eerr != nil {
			return eerr
		}
		ncStart := int(page.CellContent) - len(newData)
		nCount := int(page.CellCount) + 1
		ncPtrEnd := coff + ptroff + nCount*2 + 2
		if ncStart < ncPtrEnd || page.CellContent == 0 {
			return errInteriorFull
		}
		copy(pg.Data[ncStart:], newData)
		binary.BigEndian.PutUint16(pg.Data[ptrBase+int(page.CellCount)*2:], uint16(ncStart))
		page.CellCount = uint16(nCount)
		// Advance the content pointer: the next appended cell must land
		// BELOW this one. Without this, iteration si+1 recomputes ncStart
		// from the stale offset and overwrites cell si's bytes (both cell
		// pointers then read identical child/key data — duplicate adjacent
		// separators, orphaned keys).
		page.CellContent = uint16(ncStart)
		binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], uint16(nCount))
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(ncStart))
	}
	binary.BigEndian.PutUint32(pg.Data[coff+8:coff+12], splits[len(splits)-1].pageNum)
	return t.pager.WritePage(pg)
}

// childSplitsHaveRoom performs the exact room precheck for the re-key chain.
// Per split i: the current cell is RELOCATED (a wider divider must never be
// written in place, it would overrun the neighbor) and one sibling cell
// carrying the carrier key/payload is appended.
func (t *BTree) childSplitsHaveRoom(pg *pager.Page, page *storage.BTreePage, coff, ptroff, ptrBase, idx int, splits []leafSplitResult) error {
	n := len(splits)
	dataNeed := 0
	for _, cs := range splits {
		dataNeed += t.dividerCellLen(cs)
	}
	if t.isTable {
		carrierKey := t.cellKeyAt(pg, ptrBase, idx)
		dataNeed += n * (4 + util.VarintLen(carrierKey))
	} else {
		carrierPayload := t.cellDividerPayloadAt(pg, ptrBase, idx)
		dataNeed += n * (4 + util.VarintLen(uint64(len(carrierPayload))) + len(carrierPayload))
	}
	cellContentEnd := int(page.CellContent)
	ptrNeed := coff + ptroff + (int(page.CellCount)+n)*2 + 2
	if cellContentEnd == 0 {
		return errInteriorFull
	}
	if cellContentEnd-dataNeed < ptrNeed {
		return errInteriorFull
	}
	return nil
}

// rekeyCarrierChain walks the split chain: each current divider cell is
// relocated (re-keyed to the split's median) and the new sibling cell
// carrying carrierKey is inserted after it. Returns the number of abandoned
// divider bytes (for the defragment pass) and errInteriorFull when a write
// would not fit.
func (t *BTree) rekeyCarrierChain(pg *pager.Page, page *storage.BTreePage, coff, ptroff, ptrBase, idx int, origChild uint32, carrierKey uint64, splits []leafSplitResult) (int, error) {
	deadBytes := 0
	for si, cs := range splits {
		// Re-key the current cell: it now bounds curLeft by cs.medianKey.
		curLeft := origChild
		if si > 0 {
			curLeft = splits[si-1].pageNum
		}
		_ = curLeft // the located cell's left child already equals curLeft
		// Re-key by RELOCATING the cell: writing a wider varint in place
		// would overrun the cell's bytes into its neighbor (the source of
		// duplicate/garbled separators after this fix landed).
		curChildOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
		curChild := binary.BigEndian.Uint32(pg.Data[curChildOff : curChildOff+4])
		rekeyed := t.encodeInteriorCell(curChild, cs.medianKey)
		_, oldKeyLen := util.GetVarint(pg.Data[curChildOff+4:])
		deadBytes += 4 + oldKeyLen // the relocated divider's bytes are abandoned above
		rkStart := int(page.CellContent) - len(rekeyed)
		if rkStart < coff+ptroff+(int(page.CellCount)+1)*2+2 {
			return deadBytes, errInteriorFull
		}
		copy(pg.Data[rkStart:], rekeyed)
		binary.BigEndian.PutUint16(pg.Data[ptrBase+idx*2:], uint16(rkStart))
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(rkStart))
		page.CellContent = uint16(rkStart)
		// Insert the new sibling cell AFTER it, carrying carrierKey.
		newData := t.encodeInteriorCell(cs.pageNum, carrierKey)
		ncStart := int(page.CellContent) - len(newData)
		nCount := int(page.CellCount) + 1
		ncPtrEnd := coff + ptroff + nCount*2 + 2
		if ncStart < ncPtrEnd {
			return deadBytes, errInteriorFull
		}
		copy(pg.Data[ncStart:], newData)
		// Advance the content pointer past the sibling cell: the next
		// iteration's relocated cell must land BELOW it, otherwise the two
		// writes overlap and both cell pointers read identical bytes.
		page.CellContent = uint16(ncStart)
		// Shift pointers [idx+1..CellCount) right by one.
		shiftCellPtrsRight(pg.Data, ptrBase, idx+1, int(page.CellCount))
		binary.BigEndian.PutUint16(pg.Data[ptrBase+(idx+1)*2:], uint16(ncStart))
		page.CellCount = uint16(nCount)
		binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], uint16(nCount))
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(ncStart))
		idx++
	}
	return deadBytes, nil
}

// addInteriorCellToPage reads the page at pageNum and adds a child pointer
// cell for the given split divider and new sibling page.
func (t *BTree) addInteriorCellToPage(pageNum, childPageNum uint32, childSplit leafSplitResult, childNewSibling uint32) error {
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return err
	}
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return err
	}
	return t.addInteriorCell(pg, page, childPageNum, childSplit, childNewSibling)
}

// addInteriorCell adds a new cell to an interior page.
func (t *BTree) addInteriorCell(pg *pager.Page, page *storage.BTreePage, leftChild uint32, childSplit leafSplitResult, rightChild uint32) error {
	coff := contentOffset(pg.PageNum)
	cellData, err := t.encodeDividerCell(leftChild, childSplit, pg.PageNum)
	if err != nil {
		return err
	}
	ptroff := cellPtrOffset(page.PageType)

	// Compute space
	cellPtrEnd := coff + ptroff + int(page.CellCount)*2 + 2
	cellContentEnd := int(page.CellContent)
	var cellStart int
	if cellContentEnd == 0 {
		// Fresh (zero-initialized) page: cells pack from the usable end
		// (zeroPage convention), not the raw page end.
		cellStart = int(t.usableSize) - len(cellData) - int(page.FragFree)
	} else {
		cellStart = cellContentEnd - len(cellData)
	}
	if cellStart < cellPtrEnd {
		return fmt.Errorf("btree: interior page full, cannot add child pointer")
	}

	// Find the insert position by key so interior cells stay sorted.
	// The new cell is the separator "leftChild ... key ... rightChild"; it
	// belongs after every existing cell whose key is < key.
	ptrBase := coff + ptroff
	insertIdx := int(page.CellCount)
	for i := int(page.CellCount) - 1; i >= 0; i-- {
		cellOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+i*2 : ptrBase+i*2+2]))
		if t.dividerLess(pg, cellOff, childSplit) {
			insertIdx = i + 1
			break
		}
		insertIdx = i
	}

	// Shift cells at [insertIdx..CellCount) right by one slot.
	shiftCellPtrsRight(pg.Data, ptrBase, insertIdx, int(page.CellCount))

	copy(pg.Data[cellStart:], cellData)
	binary.BigEndian.PutUint16(pg.Data[ptrBase+insertIdx*2:], uint16(cellStart))

	page.CellCount++
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	if cellContentEnd == 0 || cellStart < cellContentEnd {
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(cellStart))
	}

	// If the new key is the largest, the new child becomes the rightmost;
	// otherwise the cell that follows the new separator (the old pointer to
	// the split leaf) must be repointed to the new sibling. The separator
	// {leftChild, key} routes keys < key to leftChild and keys >= key to
	// the NEXT cell's left child (or the rightmost pointer for the last
	// cell), so the sibling becomes the next cell's left child.
	if insertIdx == int(page.CellCount)-1 {
		binary.BigEndian.PutUint32(pg.Data[coff+8:coff+12], rightChild)
	} else {
		nextOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+(insertIdx+1)*2 : ptrBase+(insertIdx+1)*2+2]))
		binary.BigEndian.PutUint32(pg.Data[nextOff:nextOff+4], rightChild)
	}

	return t.pager.WritePage(pg)
}

// splitInteriorPage splits a full interior page into two pages.
// parentPgno is the splitting page's parent (0 when it is the root): the new
// right half stays a child of that same parent (btree.c balance_nonroot
// ptrmapPut(pBt, pgnoNew, PTRMAP_BTREE, pParent->pgno), src/btree.c:8023),
// or of the root itself when the root splits in place (balance_deeper).
func (t *BTree) splitInteriorPage(pg *pager.Page, page *storage.BTreePage, parentPgno uint32) (uint32, leafSplitResult, error) {
	coff := contentOffset(pg.PageNum)
	ptroff := cellPtrOffset(page.PageType)
	// CellPointer adds 8 to the given offset. For interior pages (ptroff=12),
	// we pass coff+4 so it computes coff+4+8+i*2 = coff+12+i*2.
	ptrBase := coff + ptroff - 8

	// Collect all interior cells (leftChild, key pairs) and the rightmost pointer
	entries := t.collectInteriorEntries(pg, ptrBase, page)
	rightmostChild := page.RightmostPtr

	// Greedy tail split (btree.c balance_nonroot): the left page keeps
	// every cell that already fit — it was full, not half-full — and only
	// the LAST cell moves to the new right page (together with the old
	// rightmost pointer). The next divider inserted by the parent then
	// finds room in whichever half it sorts into. A midpoint split left
	// interiors permanently half-full, doubling their count
	// (sqllimits1-7.7.3: 11 interior pages vs the reference 7).
	//
	// The boundary entry entries[splitIdx-1] is consumed as the divider
	// handed to the parent: its key becomes the separator and its left
	// child becomes the left page's rightmost pointer. The right page
	// keeps entries[splitIdx..] — AT LEAST ONE cell. The previous port
	// kept the right page EMPTY (0 dividers + rightmost pointer): SQLite
	// never produces a 0-cell interior page (balance_nonroot gives every
	// new page a share of the cells) and cannot even read through one —
	// the seek descends the rightmost chain past dead nodes and the fts4
	// merge churn grew sixteen of these husks under one root
	// (fts4merge4 wipe-churn tx16: "database disk image is malformed" on
	// every t2_segments/t2_segdir scan, REPLACE seeks silently missing
	// rows behind the husks). Three cells are the minimum for a legal
	// split (1 left + 1 divider + 1 right); below that the caller's
	// balance loop must stop rather than manufacture a husk.
	if len(entries) < 3 {
		// Too few dividers to split legally: the caller's balance loop
		// must stop here rather than index past the end.
		return 0, leafSplitResult{}, fmt.Errorf("btree: interior page %d has no cells to split", pg.PageNum)
	}
	splitIdx := len(entries) - 1

	// The entry at splitIdx-1 goes up to the parent (it's the separator
	// between the two halves); its left child becomes the left page's
	// rightmost pointer.
	splitRes := entries[splitIdx-1].splitResult(t.isTable)

	// Left page keeps entries[0..splitIdx-1) and its rightmost child becomes entries[splitIdx].leftChild
	// Right page keeps entries[splitIdx+1..) and the original rightmostChild

	// Allocate new interior page, parented to the splitting page's parent
	// (or to the splitting root itself when parentPgno == 0).
	ptrParent := parentPgno
	if ptrParent == 0 {
		ptrParent = pg.PageNum
	}
	newPg, err := t.allocBtreeNode(ptrParent)
	if err != nil {
		return 0, leafSplitResult{}, err
	}
	newCoff := contentOffset(newPg.PageNum)
	// The allocated page may be a cached buffer from an earlier incarnation:
	// zero it before rewriting (zeroPage parity) so freeblock/fragmentation
	// and stale cell bytes never survive.
	for i := newCoff; i < int(t.pageSize); i++ {
		newPg.Data[i] = 0
	}
	newPg.Data[newCoff] = page.PageType // same interior type

	// Clear original interior page content (except page type)
	for i := coff + 1; i < int(t.pageSize); i++ {
		pg.Data[i] = 0
	}

	if err := t.writeInteriorSplitLeft(pg, coff, ptroff, int(t.pageSize), entries, splitIdx); err != nil {
		return 0, leafSplitResult{}, err
	}
	if err := t.writeInteriorSplitRight(newPg, newCoff, ptroff, int(t.pageSize), entries, splitIdx, rightmostChild); err != nil {
		return 0, leafSplitResult{}, err
	}

	// Re-parent the children that moved to the right half (btree.c
	// balance_nonroot: ptrmapPut(pBt, key, PTRMAP_BTREE, pNew->pgno),
	// src/btree.c:8780 + 8950) — including the original rightmost pointer.
	if err := t.reparentSplitRightChildren(entries, splitIdx, rightmostChild, newPg.PageNum); err != nil {
		return 0, leafSplitResult{}, err
	}

	if err := t.pager.WritePage(pg); err != nil {
		return 0, leafSplitResult{}, err
	}
	if err := t.pager.WritePage(newPg); err != nil {
		return 0, leafSplitResult{}, err
	}

	return newPg.PageNum, splitRes, nil
}

// reparentSplitRightChildren re-points the ptrmap entries of the children
// that moved to the right half at the new page.
func (t *BTree) reparentSplitRightChildren(entries []interiorEntry, splitIdx int, rightmostChild, newPgno uint32) error {
	if !t.ptrmapEnabled() {
		return nil
	}
	for i := splitIdx; i < len(entries); i++ {
		if err := t.pager.WritePtrmap(entries[i].leftChild, storage.PtrmapBtree, newPgno); err != nil {
			return err
		}
	}
	return t.pager.WritePtrmap(rightmostChild, storage.PtrmapBtree, newPgno)
}

// interiorEntry is one decoded interior cell: a left child pointer and its
// divider (rowid key for table b-trees, record payload for index b-trees).
type interiorEntry struct {
	leftChild uint32
	key       uint64
	payload   []byte
}

// splitResult renders the entry as the divider handed to the parent on an
// interior split.
func (e interiorEntry) splitResult(isTable bool) leafSplitResult {
	if isTable {
		return leafSplitResult{medianKey: e.key}
	}
	return leafSplitResult{medianPayload: e.payload}
}

// collectInteriorEntries decodes every interior cell of the page into
// (leftChild, divider) entries.
func (t *BTree) collectInteriorEntries(pg *pager.Page, ptrBase int, page *storage.BTreePage) []interiorEntry {
	var entries []interiorEntry
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(storage.CellPointer(pg.Data, ptrBase, i, int(t.pageSize)))
		leftChild := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		if t.isTable {
			key, _ := util.GetVarint(pg.Data[cellOff+4:])
			entries = append(entries, interiorEntry{leftChild: leftChild, key: key})
			continue
		}
		cell, err := storage.DecodeCell(pg.Data, cellOff, storage.CellIndexInterior, int(t.usableSize))
		if err != nil {
			entries = append(entries, interiorEntry{leftChild: leftChild})
			continue
		}
		// Reassemble (spilled dividers) and clone: the payload aliases
		// pg.Data, which the split rewrite zeroes.
		full, oerr := t.readOverflow(cell)
		if oerr != nil {
			entries = append(entries, interiorEntry{leftChild: leftChild})
			continue
		}
		entries = append(entries, interiorEntry{leftChild: leftChild, payload: append([]byte(nil), full.Payload...)})
	}
	return entries
}

// writeInteriorSplitLeft rewrites the splitting page in place as the left
// half: entries[0..splitIdx-1), with the divider's left child
// (entries[splitIdx-1].leftChild) as the rightmost pointer.
func (t *BTree) writeInteriorSplitLeft(pg *pager.Page, coff, ptroff, pageSize int, entries []interiorEntry, splitIdx int) error {
	leftRightmost := entries[splitIdx-1].leftChild
	leftCellContentEnd := pageSize // track content end in local var
	for i := 0; i < splitIdx-1; i++ {
		cellData, eerr := t.encodeDividerCell(entries[i].leftChild, entries[i].splitResult(t.isTable), pg.PageNum)
		if eerr != nil {
			return eerr
		}
		cellPtrEnd := coff + ptroff + i*2 + 2
		cellStart := leftCellContentEnd - len(cellData)
		if cellStart < cellPtrEnd {
			return fmt.Errorf("btree: interior split failed: left page overflow")
		}
		copy(pg.Data[cellStart:], cellData)
		binary.BigEndian.PutUint16(pg.Data[coff+ptroff+i*2:], uint16(cellStart))
		leftCellContentEnd = cellStart
	}
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], uint16(splitIdx-1))
	if splitIdx-1 > 0 {
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(leftCellContentEnd))
	} else {
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(pageSize))
	}
	binary.BigEndian.PutUint32(pg.Data[coff+8:coff+12], leftRightmost)
	return nil
}

// writeInteriorSplitRight writes the new right half page:
// entries[splitIdx..) — never empty (see the split guard) — with the
// original rightmost pointer.
func (t *BTree) writeInteriorSplitRight(newPg *pager.Page, newCoff, ptroff, pageSize int, entries []interiorEntry, splitIdx int, rightmostChild uint32) error {
	rightCount := 0
	rightCellContentEnd := pageSize
	for i := splitIdx; i < len(entries); i++ {
		cellData, eerr := t.encodeDividerCell(entries[i].leftChild, entries[i].splitResult(t.isTable), newPg.PageNum)
		if eerr != nil {
			return eerr
		}
		cellPtrEnd := newCoff + ptroff + rightCount*2 + 2
		cellStart := rightCellContentEnd - len(cellData)
		if cellStart < cellPtrEnd {
			return fmt.Errorf("btree: interior split failed: right page overflow")
		}
		copy(newPg.Data[cellStart:], cellData)
		binary.BigEndian.PutUint16(newPg.Data[newCoff+ptroff+rightCount*2:], uint16(cellStart))
		rightCellContentEnd = cellStart
		rightCount++
	}
	binary.BigEndian.PutUint16(newPg.Data[newCoff+3:newCoff+5], uint16(rightCount))
	binary.BigEndian.PutUint16(newPg.Data[newCoff+5:newCoff+7], uint16(rightCellContentEnd))
	binary.BigEndian.PutUint32(newPg.Data[newCoff+8:newCoff+12], rightmostChild)
	return nil
}

// childInPage returns true if `child` is one of the cells' leftChild or the rightmost pointer.
func (t *BTree) childInPage(pg *pager.Page, page *storage.BTreePage, child uint32, coff int) bool {
	ptroff := cellPtrOffset(page.PageType)
	ptrBase := coff + ptroff
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+i*2 : ptrBase+i*2+2]))
		if binary.BigEndian.Uint32(pg.Data[cellOff:cellOff+4]) == child {
			return true
		}
	}
	return page.RightmostPtr == child
}

// locateChildAmong scans the given pages (in order) and returns the page
// number that contains the given child (either as a cell's leftChild or the
// rightmost pointer). Returns 0 if none of them holds it.
func (t *BTree) locateChildAmong(pageNums []uint32, child uint32) (uint32, error) {
	for _, pn := range pageNums {
		pg, err := t.pager.ReadPage(pn)
		if err != nil {
			return 0, err
		}
		coff := contentOffset(pn)
		page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
		if err != nil {
			return 0, err
		}
		if t.childInPage(pg, page, child, coff) {
			return pn, nil
		}
	}
	return 0, nil
}

// findChildPageForInsert returns the child page that should receive the new cell.
func (t *BTree) findChildPageForInsert(pg *pager.Page, page *storage.BTreePage, cell *storage.Cell) uint32 {
	if !t.isTable {
		// Index b-trees append to the rightmost child: leaf splits
		// redistribute their cells by the KeyInfo comparator, so each leaf
		// is internally value-ordered and splits keep the leaves ordered.
		// Full divider-guided descent destabilized the balance paths at
		// multi-hundred-thousand-entry scale (temptable2 3.2.1/4.1.2) and
		// stays deferred with the value-ordered storage tranche; the seek
		// paths (seekInInteriorIndex) already compare divider payloads.
		return page.RightmostPtr
	}
	coff := contentOffset(pg.PageNum)
	// Binary search on row IDs in interior page. CellPointer adds 8 internally,
	// so passing coff+4 yields coff+12, the interior cell-pointer array offset
	// (header 8 + 4-byte rightmost pointer).
	//
	// Convention matches SQLite's leafData splitter (btree.c:8813, paired
	// with seekInInteriorTable): the divider cell between two sibling leaves
	// holds the LAST rowid of the LEFT sibling. A new row with rowID <=
	// divider belongs to the LEFT child (this cell's child); a row with
	// rowID > divider belongs to the RIGHT subtree (next cell's left child,
	// or rightmost). The pre-fix engine used MIN(right) and <=, which kept
	// the engine self-consistent but produced files that sqlite3 integrity_check
	// rejected.
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, coff+4, mid, int(t.pageSize)))
		midRowID, _ := util.GetVarint(pg.Data[cellOff+4:])
		if int64(midRowID) < cell.RowID {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if lo < int(page.CellCount) {
		cellOff := int(storage.CellPointer(pg.Data, coff+4, lo, int(t.pageSize)))
		return binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
	}
	return page.RightmostPtr
}
