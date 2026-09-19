// Regression pin for FULL-SUITE-DRIFT.T28-regressC: the root-absorption
// path of cascadeChildless (balance_shallower, btree.c:8918-8943) must copy
// the child's cells using the child's OWN cell-pointer-array offset.
//
// The absorbed child may be an INTERIOR page (a 3-level tree collapsing
// during a mass delete). Interior pages carry a 12-byte header — the cell
// pointer array starts at byte 12, not byte 8 — so reading the array at the
// leaf offset served the rightmost-pointer bytes as cell 0's pointer and
// copied garbage divider cells (leftChild 0x05000000) into the root. The
// next insert then descended into page 0 ("database disk image is
// malformed"): tkt_6bfb98dfc0, update, alterdropcol and fts4aa all failed
// this way. C is safe by construction: copyNodeContent (btree.c:8124)
// copies the defragmented child's header + pointer array + content
// wholesale, so the child's own layout is preserved.
//
// The shape below is tkt-6bfb98dfc0.100 at the btree level: 512-byte pages,
// ~400-byte payloads (one row per leaf), 74 rows — enough for a 3-level
// tree whose root, after the delete sweep husks its interior children one
// by one, absorbs a live INTERIOR child through cascadeChildless. The
// second fixed defect (surplus siblings freed before the parent update,
// btree.c:8952) is exercised by the same sweep: with fix one alone the
// re-insert loses row 2 to a double-freed page handed out twice.

package btree

import (
	"bytes"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// shallowerOpen builds the 512-byte-page table btree used by the pin.
func shallowerOpen(t *testing.T) *BTree {
	t.Helper()
	pg := pager.OpenInMemory(512)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	return NewBTree(pg, 1, true)
}

// shallowerInsert inserts one fixed ~400-byte payload row (one row per
// 512-byte leaf).
func shallowerInsert(t *testing.T, tr *BTree, id int64, payload []byte) {
	t.Helper()
	cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: payload}
	if err := tr.InsertCell(cell); err != nil {
		t.Fatalf("insert %d: %v", id, err)
	}
}

// shallowerDeleteAll empties the tree through the bulk sweep.
func shallowerDeleteAll(t *testing.T, tr *BTree) {
	t.Helper()
	if _, err := tr.DeleteCellsWhere(func(c *storage.Cell) bool {
		return true
	}); err != nil {
		t.Fatalf("delete all: %v", err)
	}
}

// shallowerVerify re-reads rows 1..n and checks rowid order and payload.
func shallowerVerify(t *testing.T, tr *BTree, payload []byte, n int64) {
	t.Helper()
	cur, err := tr.OpenCursor()
	if err != nil {
		t.Fatalf("open cursor: %v", err)
	}
	for id := int64(1); id <= n; id++ {
		found, serr := cur.SeekToRowID(id)
		if serr != nil {
			t.Fatalf("seek %d: %v", id, serr)
		}
		if !found {
			t.Fatalf("seek %d: not found", id)
		}
		cell, rerr := cur.ReadCell()
		if rerr != nil {
			t.Fatalf("read %d: %v", id, rerr)
		}
		if !bytes.Equal(cell.Payload, payload) {
			t.Fatalf("row %d payload corrupted", id)
		}
	}
}

// TestShallowerRootAbsorbInteriorChild drives the exact shape that broke:
// 3-level table btree (512-byte pages, one ~400-byte row per leaf), mass
// delete all rows, then re-insert. The absorb must leave a walkable,
// seekable tree whose every cell decodes. With the pointer-offset bug the
// first re-insert returned "database disk image is malformed"
// (tkt-6bfb98dfc0.100); with only the free-order unfixed the re-insert
// silently lost row 2 (page handed out twice).
func TestShallowerRootAbsorbInteriorChild(t *testing.T) {
	tr := shallowerOpen(t)
	payload := make([]byte, 400)
	for i := range payload {
		payload[i] = byte(i)
	}
	const n = int64(74)
	for id := int64(1); id <= n; id++ {
		shallowerInsert(t, tr, id, payload)
	}
	shallowerDeleteAll(t, tr)
	for id := int64(1); id <= 8; id++ {
		shallowerInsert(t, tr, id, payload)
	}
	shallowerVerify(t, tr, payload, 8)
}
