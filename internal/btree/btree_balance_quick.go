// Port of btree.c::balance_quick (line 7968). Handles the common
// special case where a single overflow cell is sitting at the
// right-end of the rightmost leaf. Instead of gathering 5 siblings
// and running full balance_nonroot, balance_quick just splits the
// overflow cell into a fresh sibling page and inserts a divider
// cell into the parent.
//
// Invoked only when ALL of these are true:
//   - pPage is an intkey leaf (table leaf, not index)
//   - pPage has exactly 1 overflow cell
//   - The overflow cell is the rightmost (cell idx == nCell)
//   - The parent is not the schema root (parent->pgno != 1)
//   - The parent has no other cells pointing past this leaf
//     (parent->nCell == iIdx — i.e. this is the parent's
//     rightmost-child).
//
// Reference: src/btree.c::balance_quick (line 7968).
//
// This is a fast path: in the common "rightmost insert" case it
// avoids a full 5-sibling gather. The pSpace buffer (a 13-byte
// scratch for the divider cell) is supplied by the caller.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// balanceQuickResult is the outcome of a successful balance_quick
// call. The caller (balance()) uses it to update its cursor stack.
type balanceQuickResult struct {
	// newPgno is the page number of the freshly allocated sibling
	// that received the overflow cell.
	newPgno uint32
	// parentPgno is the parent that received the new divider cell.
	parentPgno uint32
}

// balanceQuick implements btree.c::balance_quick. It expects
// pParent to already be a writable interior page, and pPage to be
// the rightmost child leaf of pParent. The overflow cell of pPage
// is the one and only cell in pPage's overflow array. The function:
//
//  1. Allocates a new btree-node page (sibling).
//  2. Writes the overflow cell into the new page as its only cell.
//  3. Writes a divider cell into pParent: 4-byte page number +
//     varint largest-key-on-pPage.
//  4. Updates pParent's rightmost-child to the new sibling's pgno.
//  5. Updates the auto-vacuum pointer map for the new page and any
//     overflow chain it now owns.
//  6. Returns the new sibling's page number (caller will writePage
//     both pParent and the new sibling).
func (t *BTree) balanceQuick(pPage, pParent *pager.Page, pSpace []byte) (*balanceQuickResult, error) {
	if len(pSpace) < 13 {
		return nil, fmt.Errorf("btree: balanceQuick: pSpace must be at least 13 bytes (got %d)", len(pSpace))
	}
	pageCo := contentOffset(pPage.PageNum)
	page, err := storage.ParsePage(pPage.Data, int(t.pageSize), pageCo)
	if err != nil {
		return nil, fmt.Errorf("btree: balanceQuick: parse pPage %d: %w", pPage.PageNum, err)
	}
	if page.PageType != storage.PageTypeLeafTable {
		return nil, fmt.Errorf("btree: balanceQuick: pPage %d is not a table leaf (type 0x%02x)", pPage.PageNum, page.PageType)
	}
	if page.CellCount == 0 {
		return nil, fmt.Errorf("btree: balanceQuick: pPage %d has zero cells (corrupt)", pPage.PageNum)
	}
	// Locate the overflow cell. We expect this to be the rightmost
	// cell of pPage, but the C code uses pPage->apOvfl[0]
	// (pointed to by pPage->aiOvfl[0], which equals nCell).
	overflowCell := t.overflowCellAt(pPage, page)
	if overflowCell == nil {
		return nil, fmt.Errorf("btree: balanceQuick: pPage %d has no overflow cell data", pPage.PageNum)
	}

	// Allocate a new sibling page. In SQLite this calls
	// allocateBtreePage which handles ptrmap entry; in our model
	// allocBtreeNode does the same.
	newPg, err := t.allocBtreeNode(pParent.PageNum)
	if err != nil {
		return nil, fmt.Errorf("btree: balanceQuick: alloc new sibling: %w", err)
	}
	// Initialize the new page as a table leaf (PTF_INTKEY | PTF_LEAFDATA | PTF_LEAF).
	// In our storage types: PageTypeLeafTable = 0x0d.
	if err := t.zeroPageAsLeafTable(newPg); err != nil {
		return nil, err
	}

	// Write the overflow cell into the new page at its only slot.
	// The C code calls rebuildPage with b.nCell=1, b.pRef=pPage,
	// b.apCell[0]=pCell, b.szCell[0]=szCell. rebuildPage writes the
	// cell at the end of the new page's usable area, updates the
	// cell pointer array, and computes the new cell-content pointer.
	if err := t.writeSingleCellAtEnd(newPg, overflowCell); err != nil {
		return nil, fmt.Errorf("btree: balanceQuick: write cell to new sibling: %w", err)
	}

	// Auto-vacuum: write ptrmap for the new page (parent = pParent)
	// and for any overflow page owned by the cell (already done by
	// allocBtreeNode above for the new page; the overflow chain
	// itself is on disk and was originally recorded when the cell
	// was inserted — the cell still references the same chain, so
	// the ptrmap is already correct).

	// Find the largest key on pPage. In SQLite this is read from the
	// rightmost cell of pPage (which is the cell at pPage->nCell-1,
	// the one before the overflow cell — overflow cells live AFTER
	// the in-page cells). The largest key in the btree subtree is
	// the rowid of the rightmost in-page cell.
	//
	// C code: pCell = findCell(pPage, pPage->nCell-1);
	//         pStop = &pCell[9];
	//         while ((*(pCell++)&0x80) && pCell<pStop); // skip payload-length varint
	//         pStop = &pCell[9];
	//         while (((*(pOut++) = *(pCell++))&0x80) && pCell<pStop); // copy rowid varint
	//
	// In Go we use storage.DecodeCell which already extracts RowID.
	//
	// In SQLite's model, overflow cells are kept in apOvfl[] (not in
	// the cell pointer array). pPage->nCell is the count of in-page
	// cells, so findCell(pPage, pPage->nCell-1) is the last in-page
	// cell. In our simplified model, overflow cells still appear in
	// the cell pointer array; the "last in-page cell" is therefore
	// at index page.CellCount - 1 - nOverflow.
	lastCell := int(page.CellCount) - 1
	var lastInPageRowID int64
	// Detect overflow cells by inspecting the cell pointer at
	// index `lastCell`. An overflow cell has a 4-byte overflow
	// pointer at its end (non-zero, pointing to a valid page).
	// We use a simpler heuristic: count overflow cells = number of
	// cells whose payload length exceeds the in-page local
	// capacity. For balance_quick, we know there's exactly 1
	// overflow cell (the rightmost one in our pointer array).
	// Walk backwards to find the last cell that does NOT have an
	// overflow pointer.
	for lastCell >= 0 {
		lastCellPtr := storage.CellPointer(pPage.Data, pageCo, lastCell, int(t.pageSize))
		c, derr := storage.DecodeCell(pPage.Data, int(lastCellPtr), storage.CellTableLeaf, int(t.usableSize))
		if derr == nil && c.Overflow == 0 {
			// Found the last in-page cell. Use its rowid.
			lastInPageRowID = c.RowID
			break
		}
		lastCell--
	}
	if lastCell < 0 {
		return nil, fmt.Errorf("balanceQuick: no in-page cell found on pPage %d", pPage.PageNum)
	}
	if lastInPageRowID < 0 {
		return nil, fmt.Errorf("balanceQuick: last in-page cell rowid is negative (%d)", lastInPageRowID)
	}

	// Build the divider cell into pSpace:
	//   pSpace[0..4]   = uint32 pPage->pgno  (4 bytes, big-endian)
	//   pSpace[4..]    = varint(lastRowID)   (1..9 bytes)
	// pOut starts at &pSpace[4] (the C code uses `&pSpace[4]` as the
	// write cursor for the rowid varint).
	binary.BigEndian.PutUint32(pSpace[0:4], pPage.PageNum)
	rowidLen := util.PutVarint(pSpace[4:], uint64(lastInPageRowID))
	dividerSize := 4 + rowidLen
	// pOut - pSpace = dividerSize (the size of the divider cell).

	// Insert the divider cell into pParent at position pParent->nCell
	// (the rightmost-child slot in pParent's cell array). For
	// interior-table pages, an interior cell is 4-byte left-child +
	// varint rowid — that's exactly the divider layout.
	parentCo := contentOffset(pParent.PageNum)
	parentPage, err := storage.ParsePage(pParent.Data, int(t.pageSize), parentCo)
	if err != nil {
		return nil, fmt.Errorf("btree: balanceQuick: parse pParent %d: %w", pParent.PageNum, err)
	}
	if parentPage.PageType != storage.PageTypeInteriorTable && parentPage.PageType != storage.PageTypeInteriorIndex {
		return nil, fmt.Errorf("btree: balanceQuick: pParent %d is not interior (type 0x%02x)", pParent.PageNum, parentPage.PageType)
	}
	// Place the divider cell at the end of pParent's content area.
	// We do NOT call insertCell here because that requires
	// page-level cell-insertion bookkeeping that hasn't been ported
	// yet (the full insertCell() lives in btree_insert.go and is
	// specific to leaf pages). For now, write the cell manually.
	usableStart := int(t.usableSize)
	// Cell content pointer is a page-buffer offset. Place the new
	// cell at usableStart-dividerSize (just below the existing
	// cell content area).
	dividerStart := usableStart - dividerSize
	if dividerStart < parentCo+cellPtrOffset(parentPage.PageType)+2*int(parentPage.CellCount)+2 {
		return nil, fmt.Errorf("btree: balanceQuick: not enough room for divider cell on pParent %d", pParent.PageNum)
	}
	copy(pParent.Data[dividerStart:dividerStart+dividerSize], pSpace[:dividerSize])
	// Cell pointer array: insert at position parentPage.CellCount.
	ptrBase := parentCo + cellPtrOffset(parentPage.PageType) - 8
	binary.BigEndian.PutUint16(pParent.Data[ptrBase+int(parentPage.CellCount)*2:ptrBase+int(parentPage.CellCount)*2+2], uint16(dividerStart))
	// Update parent header: cell count, cell content pointer.
	newCount := parentPage.CellCount + 1
	binary.BigEndian.PutUint16(pParent.Data[parentCo+3:parentCo+5], newCount)
	binary.BigEndian.PutUint16(pParent.Data[parentCo+5:parentCo+7], uint16(dividerStart))
	pParent.Data[parentCo+7] = 0 // frag free

	// Set pParent's rightmost-child to the new sibling's page
	// number. pParent's rightmost-child lives at parentCo+8..parentCo+12.
	binary.BigEndian.PutUint32(pParent.Data[parentCo+8:parentCo+12], newPg.PageNum)

	// Persist the new sibling and the parent.
	if err := t.pager.WritePage(newPg); err != nil {
		return nil, err
	}
	if err := t.pager.WritePage(pParent); err != nil {
		return nil, err
	}
	return &balanceQuickResult{
		newPgno:    newPg.PageNum,
		parentPgno: pParent.PageNum,
	}, nil
}

// overflowCellAt returns the encoded bytes of the rightmost cell on
// pPage. In our model, overflow cells live in the cell pointer
// array (we don't keep a separate apOvfl[] like SQLite's C code).
// The overflow cell is the rightmost cell — i.e. the cell at
// page.CellCount-1.
//
// For balanceQuick's purposes, the rightmost cell is the one that
// has overflowed: it's the cell that was inserted and didn't fit on
// the page. In a normal overflow scenario, the cell pointer array
// holds the in-page cells and the overflow cell lives past the
// in-page cells in apOvfl[]. We collapse that distinction by
// reading the cell at page.CellCount-1, which is the rightmost
// cell in the array.
func (t *BTree) overflowCellAt(pPage *pager.Page, page *storage.BTreePage) []byte {
	coff := contentOffset(pPage.PageNum)
	if page.CellCount == 0 {
		return nil
	}
	idx := int(page.CellCount) - 1
	ptr := storage.CellPointer(pPage.Data, coff, idx, int(t.pageSize))
	if int(ptr)+9 > len(pPage.Data) {
		return nil
	}
	// The cell is a table-leaf cell: varint(payload-len) + varint(rowid)
	// + local payload + 4-byte overflow-pointer.
	plen, n1 := util.GetVarint(pPage.Data[int(ptr):])
	rowid, n2 := util.GetVarint(pPage.Data[int(ptr)+n1:])
	_ = rowid
	local := storage.LocalPayloadSize(int(plen), int(t.usableSize), storage.CellTableLeaf)
	cellStart := int(ptr)
	cellEnd := cellStart + n1 + n2 + local + 4 // +4 for overflow ptr
	if cellEnd > len(pPage.Data) {
		cellEnd = len(pPage.Data)
	}
	out := make([]byte, cellEnd-cellStart)
	copy(out, pPage.Data[cellStart:cellEnd])
	return out
}

// zeroPageAsLeafTable initializes a freshly allocated page as a
// table leaf (PTF_INTKEY | PTF_LEAFDATA | PTF_LEAF). Mirrors
// btree.c::zeroPage.
func (t *BTree) zeroPageAsLeafTable(pg *pager.Page) error {
	coff := contentOffset(pg.PageNum)
	// Zero the entire page first (the cell pointer area, cell
	// content area, free blocks, etc.). SQLite's zeroPage zeroes
	// only the btree content area, but a fresh page is already
	// zeroed by AllocatePage.
	for i := coff; i < len(pg.Data); i++ {
		pg.Data[i] = 0
	}
	pg.Data[coff] = storage.PageTypeLeafTable
	// Cell pointer array: 0 cells.
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 0)
	// Cell content pointer: end of usable area. For interior pages
	// the btree content area extends to the end of the page; for
	// page 1 it's pageSize (the btree content ends at pageSize,
	// i.e. the end of the page buffer minus 0 reserved bytes).
	usableStart := int(t.usableSize)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(usableStart))
	pg.Data[coff+7] = 0 // frag free
	return nil
}

// writeSingleCellAtEnd writes a single cell at the end of pg's cell
// content area, updates the cell pointer array, and recomputes the
// cell content pointer. This is the bare-bones equivalent of
// btree.c::rebuildPage for the nCell=1 case.
//
// pg must already be a leaf (the btree content area is set up by
// zeroPageAsLeafTable). The cell bytes are written to the highest
// available addresses in the cell content area; the cell pointer
// array gets one entry pointing at the cell.
func (t *BTree) writeSingleCellAtEnd(pg *pager.Page, cell []byte) error {
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}
	if page.CellCount != 0 {
		return fmt.Errorf("btree: writeSingleCellAtEnd: page already has %d cells", page.CellCount)
	}
	// The cell content area extends from cellContent..usableSize.
	// SQLite's convention: cells grow downward; the cell pointer
	// at index 0 points to the cell at the HIGHEST address, with
	// each subsequent cell pointer at a lower address. For nCell=1
	// the cell goes at usableSize-len(cell).
	usableStart := int(t.usableSize)
	// SQLite reserves the last 4 bytes of the page for the
	// right-child pointer (a leaf page's "right child" is used by
	// overflow chains). Use the bytes just before that reserved
	// area.
	cellStart := usableStart - 4 - len(cell)
	if cellStart < coff+8+2*int(page.CellCount)+2 {
		return fmt.Errorf("btree: writeSingleCellAtEnd: cell too large for page")
	}
	copy(pg.Data[cellStart:cellStart+len(cell)], cell)
	// Cell pointer array at coff+8 (leaf): write pointer.
	binary.BigEndian.PutUint16(pg.Data[coff+8:coff+10], uint16(cellStart))
	// Update header.
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 1)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(cellStart))
	pg.Data[coff+7] = 0 // frag free
	return nil
}
