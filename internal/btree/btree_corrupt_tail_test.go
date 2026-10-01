package btree

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// The memofix tail-guard contract: a crafted cell pointer aimed at the page
// tail must surface as "database disk image is malformed" (or a clean walk
// stop) on every read path — never a slice-bounds panic. Go slices panic on
// the out-of-page tails where SQLite's masked in-page addressing
// (btreeInitPage bounds nCell; get2byte & maskPage) stays in-bounds by
// construction.

// corruptTailTree builds a two-level TABLE b-tree over 1024-byte pages:
// page 2 = interior root with `cellCount` cells (cell i: left child = leaf 3,
// rowid i+1), rightmost = page 4; page 3 = leaf with rowid 1; page 4 = an
// empty leaf. The returned root page handle lets a test re-aim a cell-pointer
// array entry in place (the pager serves the same object to the btree).
func corruptTailTree(t *testing.T, cellCount int) (*BTree, *pager.Page) {
	t.Helper()
	const ps = 1024
	pg := pager.OpenInMemory(ps)
	pg.AllocatePage() // page 1: schema header
	root := pg.AllocatePage()
	leaf := pg.AllocatePage()
	empty := pg.AllocatePage()

	// Leaf 3: one table-leaf cell (payload len 1, rowid 1, payload "x").
	leaf.Data[0] = storage.PageTypeLeafTable
	lpos := ps - 3
	leaf.Data[lpos] = 1
	leaf.Data[lpos+1] = 1
	leaf.Data[lpos+2] = 'x'
	binary.BigEndian.PutUint16(leaf.Data[3:5], 1)
	binary.BigEndian.PutUint16(leaf.Data[5:7], uint16(lpos))
	binary.BigEndian.PutUint16(leaf.Data[8:10], uint16(lpos))

	// Empty leaf 4 (the rightmost subtree reports no rows).
	empty.Data[0] = storage.PageTypeLeafTable
	binary.BigEndian.PutUint16(empty.Data[5:7], uint16(ps))

	// Interior root 2: cell i = (left child, rowid i+1) packed downward from
	// the page end; pointer array at 12 (8-byte header + 4-byte rightmost).
	root.Data[0] = storage.PageTypeInteriorTable
	binary.BigEndian.PutUint32(root.Data[8:12], empty.PageNum)
	cpos := ps
	for i := 0; i < cellCount; i++ {
		cpos -= 5
		binary.BigEndian.PutUint32(root.Data[cpos:cpos+4], leaf.PageNum)
		root.Data[cpos+4] = byte(i + 1) // rowid varint (1..cellCount)
		binary.BigEndian.PutUint16(root.Data[12+i*2:14+i*2], uint16(cpos))
	}
	binary.BigEndian.PutUint16(root.Data[3:5], uint16(cellCount))
	binary.BigEndian.PutUint16(root.Data[5:7], uint16(cpos))
	for _, p := range []*pager.Page{root, leaf, empty} {
		if err := pg.WritePage(p); err != nil {
			t.Fatalf("WritePage: %v", err)
		}
	}
	return NewBTree(pg, root.PageNum, true), root
}

// aimCellPointerAtTail points cell index i's pointer-array entry at the last
// byte of the page: the canonical crafted-tail cell pointer (reading the
// 4-byte child there runs two bytes past the buffer). The mutation is an
// in-place byte write on the cached page object — the same shape as any
// crash-torn image the btree layer re-reads.
func aimCellPointerAtTail(t *testing.T, root *pager.Page, i int) {
	t.Helper()
	binary.BigEndian.PutUint16(root.Data[12+i*2:14+i*2], uint16(len(root.Data)-1))
}

// TestCorruptTailStepDownNoPanic: OpenCursor's descent reads cell 0's child
// through stepDownLeftmost; a tail-aimed pointer must land on a clean EOF,
// not a panic.
func TestCorruptTailStepDownNoPanic(t *testing.T) {
	bt, root := corruptTailTree(t, 1)
	aimCellPointerAtTail(t, root, 0)
	c, err := bt.OpenCursor() // descends through the corrupt cell 0
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	if c.AtEnd() {
		return // clean EOF: the guard reported no-child
	}
	if _, err := c.ReadCell(); err != nil {
		return // corruption surfaced as an error
	}
}

// TestCorruptTailNavigateNextNoPanic: navigating past leaf 3 routes through
// root cell 1's pointer; a tail-aimed entry must stop the walk (endOfBTree),
// not panic (guarded navigateToNextChild).
func TestCorruptTailNavigateNextNoPanic(t *testing.T) {
	bt, root := corruptTailTree(t, 2)
	aimCellPointerAtTail(t, root, 1)
	c, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	if _, _, err := c.ReadCellData(); err != nil {
		t.Fatalf("landing read: %v", err)
	}
	next, nerr := c.Next() // crosses the corrupt pointer
	if nerr != nil {
		t.Fatalf("Next must not error: %v", nerr)
	}
	if next {
		t.Fatalf("Next crossed a corrupt interior cell")
	}
}

// TestCorruptTailLastRowIDReportsMalformed: LastRowID falls into the interior
// cell walk once the rightmost subtree reports empty; a tail-aimed pointer
// must return the malformed-image error, not panic (guarded lastRowIDFrom*
// paths).
func TestCorruptTailLastRowIDReportsMalformed(t *testing.T) {
	bt, root := corruptTailTree(t, 1)
	aimCellPointerAtTail(t, root, 0)
	_, err := bt.LastRowID()
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("LastRowID err = %v, want malformed-image error", err)
	}
}

// TestCorruptTailSeekToRowIDReportsMalformed: the seek's interior routing
// reads the separator rowid past the child pointer; a tail-aimed pointer
// must error (guarded routeInteriorTable), not panic.
func TestCorruptTailSeekToRowIDReportsMalformed(t *testing.T) {
	bt, root := corruptTailTree(t, 1)
	aimCellPointerAtTail(t, root, 0)
	c, err := bt.OpenCursor()
	if err != nil {
		// The descent itself may already surface the corruption.
		if strings.Contains(err.Error(), "malformed") {
			return
		}
		t.Fatalf("OpenCursor: %v", err)
	}
	found, err := c.SeekToRowID(1)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("SeekToRowID err = %v (found %v), want malformed-image error", err, found)
	}
}
