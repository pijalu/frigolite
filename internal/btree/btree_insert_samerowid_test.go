package btree

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// TestInsertSameRowidNoDuplicate covers SQLite's overwrite semantics on a
// FULL leaf page (btree.c sqlite3BtreeInsert: loc==0 drops the old cell
// BEFORE insertCellFast, so balance() never sees two equal keys). The
// engine's split path previously redistributed the old and new cell,
// leaving the rowid twice in the tree.
func TestInsertSameRowidNoDuplicate(t *testing.T) {
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	bt := NewBTree(pg, 1, true)

	// Fill several pages worth of cells so leaves are full.
	const n = 2000
	for i := int64(1); i <= n; i++ {
		rec, _ := storage.EncodeRecord([]interface{}{i, "original"})
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: i, Payload: rec}
		if err := bt.InsertCell(cell); err != nil {
			t.Fatalf("InsertCell %d: %v", i, err)
		}
	}

	// Re-insert existing rowids (including ones on full leaves) — each must
	// REPLACE, never duplicate.
	for _, rid := range []int64{1, 250, 500, 999, 1500, n} {
		rec, _ := storage.EncodeRecord([]interface{}{rid, "replaced"})
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: rid, Payload: rec}
		if err := bt.InsertCell(cell); err != nil {
			t.Fatalf("re-InsertCell %d: %v", rid, err)
		}
	}

	// Full scan: every rowid exactly once, payload is the replacement.
	counts := map[int64]int{}
	c, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	total := 0
	for {
		cell, rerr := c.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		counts[cell.RowID]++
		total++
		ok, nerr := c.Next()
		if nerr != nil || !ok {
			break
		}
	}
	if total != n {
		t.Fatalf("tree holds %d cells, want %d (duplicate or lost rows)", total, n)
	}
	for rid, cnt := range counts {
		if cnt != 1 {
			t.Errorf("rowid %d appears %d times, want 1", rid, cnt)
		}
	}
	// The replaced payload must be the surviving one for a replaced rowid.
	if got := readRowPayload(t, bt, 500); got != "replaced" {
		t.Errorf("rowid 500 payload = %q, want \"replaced\"", got)
	}
}

func readRowPayload(t *testing.T, bt *BTree, rowID int64) string {
	t.Helper()
	c, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	for {
		cell, rerr := c.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		if cell.RowID == rowID {
			rec, derr := storage.DecodeRecord(cell.Payload)
			if derr != nil {
				t.Fatalf("DecodeRecord: %v", derr)
			}
			if s, ok := rec.Values[len(rec.Values)-1].(string); ok {
				return s
			}
		}
		ok, nerr := c.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return ""
}
