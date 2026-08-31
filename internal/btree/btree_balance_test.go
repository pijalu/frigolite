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
//   0: page type (0x05 = interior table)
//   1-2: first freeblock (0)
//   3-4: cell count (N)
//   5-6: cell content start (we set it to len(cellContent))
//   7:   fragmented free bytes (0)
//   8-11: rightmost-child pointer
//   12+: cell pointer array (2 bytes per cell)
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
