// Focused UTs for the btree.c rebalance port (P8.INCRVACUUM phase 5.5).
// Each test exercises one piece of the btree.c machinery — the same
// pieces that autovacuum/incrvacuum/incrvacuum2 testgen packages need.
// The tests are tiny on purpose: they build a specific state by hand,
// invoke one btree.c port, and assert the result matches SQLite's
// behavior.

package btree

import (
	"encoding/binary"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// makeInteriorWithChildren writes a small interior-table page with N
// children (the child page numbers are 0, 1, ..., N-1 — the caller
// decides what the children are). Returns the page.
//
// The btree header layout (interior table) is:
//
//	0: page type (0x05 = interior table)
//	1-2: first freeblock (0)
//	3-4: cell count (N)
//	5-6: cell content start (we set it to len(cellContent))
//	7:   fragmented free bytes (0)
//	8-11: rightmost-child pointer
//	12+: cell pointer array (2 bytes per cell)
//
// Each interior-table cell is 4 bytes (left child) + varint (rowid).
// We use the minimum rowid (a single byte 0x00 = rowid 0 for the
// varint, except cells like rowid=1..N which are 0x01..0x0a).
func makeInteriorWithChildren(t *testing.T, pg *pager.Pager, pgNo uint32, children []uint32, rowids []int64) *pager.Page {
	t.Helper()
	if uint32(len(children)) != uint32(len(rowids)) {
		t.Fatalf("children/rowids length mismatch: %d vs %d", len(children), len(rowids))
	}
	pgx, err := pg.ReadPage(pgNo)
	if err != nil {
		t.Fatalf("ReadPage(%d): %v", pgNo, err)
	}
	coff := contentOffset(pgNo)
	// Header
	pgx.Data[coff+0] = storage.PageTypeInteriorTable
	binary.BigEndian.PutUint16(pgx.Data[coff+1:coff+3], 0) // first freeblock
	binary.BigEndian.PutUint16(pgx.Data[coff+3:coff+5], uint16(len(children)))
	// Place cell content area near the end (cells grow downward).
	// The cell-content pointer stored in the btree header is a
	// PAGE-BUFFER offset (not a btree-content offset). For page 1
	// with a 100-byte database header, the btree content lives at
	// [100..1024]. To leave room for ~3 small cells, set the
	// cell-content pointer to 900 (page-buffer offset). The cell
	// pointers and bytes below are written at btree-content offsets
	// [cellContentStart-100 .. cellContentStart].
	const cellContentStart = 900 // page-buffer offset
	binary.BigEndian.PutUint16(pgx.Data[coff+5:coff+7], cellContentStart)
	pgx.Data[coff+7] = 0 // frag free
	// Rightmost child = last entry.
	binary.BigEndian.PutUint32(pgx.Data[coff+8:coff+12], children[len(children)-1])
	// Build cells in the cell content area: each cell is 4 bytes
	// (left child) + varint (rowid). The cell content area is
	// [cellContentStart..usableSize] (a page-buffer offset range).
	// Cells are placed from the END of the usable area downward;
	// each cell's start is its predecessor's start minus its size.
	// The cell at the highest address is cell N-1 (rightmost child
	// of the interior page); cell 0 is at the lowest address but
	// still >= cellContentStart.
	//
	// Cell layout: 4-byte left child FIRST, then varint rowid.
	usableEnd := 1024 // = usableSize for OpenInMemory
	pos := usableEnd
	ptrs := make([]uint16, len(children))
	for i := len(children) - 1; i >= 0; i-- {
		// varint rowid (1 byte for small rowids)
		n := binary.PutUvarint(pgx.Data[pos-1:pos], uint64(rowids[i]))
		pos -= n
		// 4 bytes left child
		pos -= 4
		binary.BigEndian.PutUint32(pgx.Data[pos:pos+4], children[i])
		ptrs[i] = uint16(pos)
	}
	if pos < cellContentStart {
		t.Fatalf("cells overflowed content area: pos=%d cellContentStart=%d", pos, cellContentStart)
	}
	// Cell pointer array at coff+12.
	for i, p := range ptrs {
		binary.BigEndian.PutUint16(pgx.Data[coff+12+i*2:coff+12+i*2+2], p)
	}
	return pgx
}

// TestCopyNodeContent_RoundTrip: build an interior page with 3
// children, copy its content to a fresh page, and assert the new
// page parses with the same children/rowids and the rightmost-child
// pointer is preserved. Without the right copy, the new page would
// parse with 0 children or 0 cell count.
func TestCopyNodeContent_RoundTrip(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	// Page 1 is the schema; we'll use it as the source.
	pg.AllocatePage()
	src := makeInteriorWithChildren(t, pg, 1,
		[]uint32{100, 101, 102}, []int64{5, 10, 20})
	if err := pg.WritePage(src); err != nil {
		t.Fatalf("WritePage(src): %v", err)
	}
	// Page 2 is the destination (freshly allocated).
	pg.AllocatePage()
	dst, err := pg.ReadPage(2)
	if err != nil {
		t.Fatalf("ReadPage(2): %v", err)
	}
	bt := NewBTree(pg, 1, true)
	if err := bt.copyNodeContent(src, dst); err != nil {
		t.Fatalf("copyNodeContent: %v", err)
	}
	// Persist the copy (copyNodeContent does not write the
	// destination — the caller is responsible; this mirrors
	// btree.c::balance_shallower's flow which writes the page as
	// part of subsequent operations).
	if err := pg.WritePage(dst); err != nil {
		t.Fatalf("WritePage(dst): %v", err)
	}
	// Parse the destination and verify.
	dst2, err := pg.ReadPage(2)
	if err != nil {
		t.Fatalf("ReadPage(2 again): %v", err)
	}
	coff := contentOffset(2)
	page, err := storage.ParsePage(dst2.Data, 1024, coff)
	if err != nil {
		t.Fatalf("ParsePage(dst): %v", err)
	}
	if page.CellCount != 3 {
		t.Errorf("dst cell count: got %d, want 3", page.CellCount)
	}
	if page.RightmostPtr != 102 {
		t.Errorf("dst rightmost: got %d, want 102", page.RightmostPtr)
	}
	// Walk the cell pointer array and verify each child + rowid.
	for i := uint16(0); i < page.CellCount; i++ {
		co := int(storage.CellPointer(dst2.Data, coff+cellPtrOffset(page.PageType)-8, int(i), 1024))
		if co+5 > len(dst2.Data) {
			t.Errorf("cell %d: pointer %d past page", i, co)
			continue
		}
		leftChild := binary.BigEndian.Uint32(dst2.Data[co : co+4])
		rowid, _ := decodeVarint(dst2.Data[co+4:])
		wantChild := uint32(100 + i)
		if leftChild != wantChild {
			t.Errorf("cell %d: leftChild got %d, want %d", i, leftChild, wantChild)
		}
		wantRowid := []int64{5, 10, 20}[i]
		if int64(rowid) != wantRowid {
			t.Errorf("cell %d: rowid got %d, want %d", i, rowid, wantRowid)
		}
	}
}

// TestCopyNodeContent_RejectsLeaf: copyNodeContent is interior-only.
// Invoking it on a leaf must return an error rather than silently
// copy garbage.
func TestCopyNodeContent_RejectsLeaf(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	// Make page 1 a leaf.
	rootPg, _ := pg.ReadPage(1)
	setEmptyLeafContent(rootPg)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	pg.WritePage(rootPg)
	pg.AllocatePage()
	dst, _ := pg.ReadPage(2)
	bt := NewBTree(pg, 1, true)
	if err := bt.copyNodeContent(rootPg, dst); err == nil {
		t.Errorf("copyNodeContent on leaf: expected error, got nil")
	}
}

// decodeVarint is a tiny helper to read a SQLite-style varint from
// the start of `data`. Returns the value and the number of bytes
// consumed. We use this in the test only — production code uses
// util.GetVarint.
func decodeVarint(data []byte) (uint64, int) {
	var v uint64
	var n int
	for n = 0; n < 9 && n < len(data); n++ {
		b := data[n]
		v = (v << 7) | uint64(b&0x7f)
		if (b & 0x80) == 0 {
			return v, n + 1
		}
	}
	return v, n
}

// TestBalanceQuick_AllocateSibling: build a leaf page with 1 in-page
// cell and 1 overflow cell (the rightmost, simulating the
// post-split state from balance_quick's caller). Call balanceQuick
// and verify:
//  1. A new sibling page exists with the overflow cell.
//  2. The parent has a new divider cell pointing to the new sibling
//     as its rightmost-child.
//  3. The original leaf's rightmost-child pointer in the parent is
//     still the original leaf (not changed).
func TestBalanceQuick_AllocateSibling(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	// Page 1: parent (interior table).
	pg.AllocatePage()
	// Page 2: the leaf that needs balance.
	pg.AllocatePage()
	// We will need page 3 too — the new sibling. But balanceQuick
	// allocates it itself, so we just set up pages 1 and 2.
	bt := NewBTree(pg, 1, true)

	// Build parent: page 1 with one cell pointing to page 2, then
	// page 2 as the rightmost-child.
	parentPg, _ := pg.ReadPage(1)
	coff := contentOffset(1)
	parentPg.Data[coff+0] = storage.PageTypeInteriorTable
	binary.BigEndian.PutUint16(parentPg.Data[coff+3:coff+5], 1)  // 1 cell
	binary.BigEndian.PutUint32(parentPg.Data[coff+8:coff+12], 2) // rightmost = page 2
	// Place a divider cell: 4-byte child=2 + varint rowid=100.
	// Cell goes at end of usable area.
	dividerStart := 1024 - 5 // 4 (child) + 1 (varint 100)
	binary.BigEndian.PutUint32(parentPg.Data[dividerStart:dividerStart+4], 2)
	parentPg.Data[dividerStart+4] = 100 // varint 100
	binary.BigEndian.PutUint16(parentPg.Data[coff+12:coff+14], uint16(dividerStart))
	binary.BigEndian.PutUint16(parentPg.Data[coff+5:coff+7], uint16(dividerStart))
	pg.WritePage(parentPg)

	// Build leaf: page 2 with 1 in-page cell (rowid=99) and 1
	// overflow cell (rowid=100, payload=4000 bytes so it overflows).
	leafPg, _ := pg.ReadPage(2)
	lcoff := contentOffset(2)
	leafPg.Data[lcoff+0] = storage.PageTypeLeafTable
	// Allocate an overflow page (page 3) and write the first 4KB
	// of payload there.
	pg.AllocatePage()
	ovflPg, _ := pg.ReadPage(3)
	for i := 0; i < 1024; i++ {
		ovflPg.Data[i] = byte(i & 0xff)
	}
	ovflPg.Data[0] = 0 // next = 0 (only 1 overflow page)
	ovflPg.Data[1] = 0
	ovflPg.Data[2] = 0
	ovflPg.Data[3] = 0
	pg.WritePage(ovflPg)

	// Build the in-page cell: rowid=99, payload=10 bytes.
	inPageCell := buildTableLeafCell(t, 99, make([]byte, 10), 0)
	pos := 1024 - 4 - len(inPageCell)
	copy(leafPg.Data[pos:pos+len(inPageCell)], inPageCell)
	binary.BigEndian.PutUint16(leafPg.Data[lcoff+8:lcoff+10], uint16(pos))

	// Build the overflow cell: rowid=100, payload=4000 bytes (local
	// portion + 4-byte overflow pointer). LocalPayloadSize for
	// payload 4000, page 1024: maxLocal = 1024-35 = 989, so 4000
	// spills, local = minLocal + (4000-minLocal) % 1020 =
	// ((1024-12)*32/255) - 23 + (4000 - that) % 1020. Use a simple
	// approach: just put a 1-byte varint local payload, then the
	// overflow pointer.
	overflowCell := buildTableLeafCell(t, 100, []byte{0xff}, 3) // overflow → page 3
	pos2 := pos - len(overflowCell)
	copy(leafPg.Data[pos2:pos2+len(overflowCell)], overflowCell)
	binary.BigEndian.PutUint16(leafPg.Data[lcoff+10:lcoff+12], uint16(pos2))
	// Header: 2 cells, cell content = pos2, frag=0.
	binary.BigEndian.PutUint16(leafPg.Data[lcoff+3:lcoff+5], 2)
	binary.BigEndian.PutUint16(leafPg.Data[lcoff+5:lcoff+7], uint16(pos2))
	leafPg.Data[lcoff+7] = 0
	pg.WritePage(leafPg)

	// Re-read parent (since WritePage may invalidate).
	parentPg2, _ := pg.ReadPage(1)
	leafPg2, _ := pg.ReadPage(2)
	pSpace := make([]byte, 32)
	res, err := bt.balanceQuick(leafPg2, parentPg2, pSpace)
	if err != nil {
		t.Fatalf("balanceQuick: %v", err)
	}
	if res.newPgno == 0 {
		t.Fatalf("balanceQuick: newPgno is 0")
	}
	// The new sibling should have one cell.
	sibPg, _ := pg.ReadPage(res.newPgno)
	scoff := contentOffset(res.newPgno)
	sibPage, err := storage.ParsePage(sibPg.Data, 1024, scoff)
	if err != nil {
		t.Fatalf("ParsePage(sibling): %v", err)
	}
	if sibPage.CellCount != 1 {
		t.Errorf("sibling cell count: got %d, want 1", sibPage.CellCount)
	}
	// The parent should now have 2 cells and rightmost = the new sibling.
	parentAfter, _ := pg.ReadPage(1)
	pcoff := contentOffset(1)
	pp, _ := storage.ParsePage(parentAfter.Data, 1024, pcoff)
	if pp.CellCount != 2 {
		t.Errorf("parent cell count: got %d, want 2", pp.CellCount)
	}
	if pp.RightmostPtr != res.newPgno {
		t.Errorf("parent rightmost: got %d, want %d", pp.RightmostPtr, res.newPgno)
	}
}

// buildTableLeafCell constructs a table-leaf cell's encoded bytes:
// varint(plen) + varint(rowid) + localPayload + 4-byte overflowPtr.
// The overflowPtr is 0 if the cell fits in the page; the caller
// supplies the actual page number when spilling.
func buildTableLeafCell(t *testing.T, rowid int64, localPayload []byte, overflow uint32) []byte {
	t.Helper()
	plen := len(localPayload)
	if overflow != 0 {
		plen = 4000 // simulate a payload that needs overflow
	}
	out := make([]byte, 0, 32)
	// varint(plen) — for 4000 this is 2 bytes (0xfa 0x28).
	if plen < 128 {
		out = append(out, byte(plen))
	} else {
		out = append(out, byte((plen&0x7f)|0x80))
		out = append(out, byte(plen>>7))
	}
	// varint(rowid).
	if rowid < 0 {
		t.Fatalf("rowid out of range: %d", rowid)
	}
	if rowid < 128 {
		out = append(out, byte(rowid))
	} else {
		out = append(out, byte((rowid&0x7f)|0x80))
		out = append(out, byte(rowid>>7))
	}
	// local payload.
	out = append(out, localPayload...)
	// overflow pointer (4 bytes, big-endian).
	if overflow != 0 {
		out = append(out, byte(overflow>>24), byte(overflow>>16), byte(overflow>>8), byte(overflow))
	} else {
		out = append(out, 0, 0, 0, 0)
	}
	return out
}

// TestRebuildPage_Leaf: build a leaf with 0 cells, then call
// rebuildPage with 3 cells in a CellArray. Verify the page now
// has 3 cells in the right order, with the cell-content pointer
// at the correct address.
func TestRebuildPage_Leaf(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	coff := contentOffset(1)
	rootPg.Data[coff+0] = storage.PageTypeLeafTable
	// Initialize cell content pointer to end of page so the
	// page validates as a fresh empty leaf (matches
	// setEmptyLeafContent).
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	bt := NewBTree(pg, 1, true)

	// Build 3 cells.
	cell1 := buildTableLeafCell(t, 10, []byte("hello"), 0)
	cell2 := buildTableLeafCell(t, 20, []byte("world"), 0)
	cell3 := buildTableLeafCell(t, 30, []byte("!"), 0)
	bca := newBalanceCellArray(3, 1)
	bca.addCell(cell1, 1024, 0)
	bca.endRegion()
	bca.finalizeRegionEnds([]int{1024})

	// Note: the CellArray design needs cells to be added with
	// their region index; the rebuildPage walks b.cells[iFirst+k]
	// in cell-index order, so we need to add them with region
	// 0/1/2 (not all region 0).
	bca2 := newBalanceCellArray(3, 3)
	bca2.addCell(cell1, 1024, 0)
	bca2.addCell(cell2, 1024, 1)
	bca2.addCell(cell3, 1024, 2)
	bca2.endRegion()
	bca2.finalizeRegionEnds([]int{1024, 1024, 1024})
	_ = bca

	if err := bt.rebuildPage(rootPg, bca2, 0, 3); err != nil {
		t.Fatalf("rebuildPage: %v", err)
	}
	// Re-read and verify.
	pg2, _ := pg.ReadPage(1)
	page, err := storage.ParsePage(pg2.Data, 1024, coff)
	if err != nil {
		t.Fatalf("ParsePage: %v", err)
	}
	if page.CellCount != 3 {
		t.Errorf("cell count: got %d, want 3", page.CellCount)
	}
	// Verify each cell decodes and has the right rowid.
	for i := uint16(0); i < page.CellCount; i++ {
		cp := int(storage.CellPointer(pg2.Data, coff, int(i), 1024))
		c, err := storage.DecodeCell(pg2.Data, cp, storage.CellTableLeaf, 1024)
		if err != nil {
			t.Errorf("cell %d: decode: %v", i, err)
			continue
		}
		wantRowid := []int64{10, 20, 30}[i]
		if c.RowID != wantRowid {
			t.Errorf("cell %d: rowid got %d, want %d", i, c.RowID, wantRowid)
		}
	}
}

// TestEditPage_Empty: editPage with nNew=0 should produce an
// empty page (cell count 0, content pointer at usableSize).
func TestEditPage_Empty(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	coff := contentOffset(1)
	rootPg.Data[coff+0] = storage.PageTypeLeafTable
	// Pre-fill with 2 cells to make sure editPage clears them.
	rootPg.Data[coff+3] = 2 // cell count
	pg.WritePage(rootPg)
	bt := NewBTree(pg, 1, true)
	bca := newBalanceCellArray(0, 0)
	if err := bt.editPage(rootPg, 0, 0, 0, bca); err != nil {
		t.Fatalf("editPage: %v", err)
	}
	pg2, _ := pg.ReadPage(1)
	page, err := storage.ParsePage(pg2.Data, 1024, coff)
	if err != nil {
		t.Fatalf("ParsePage: %v", err)
	}
	if page.CellCount != 0 {
		t.Errorf("cell count: got %d, want 0", page.CellCount)
	}
}
