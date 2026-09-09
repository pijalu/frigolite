package btree

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// TestSeekToRowIDAfterChompRanges reproduces the fts4opt merge=5,2 [SEG3]
// class: a %_segments-like table btree built by sequential appends (blockids
// 1..N), then interleaved chomp-range deletions (deleteFTSBlocks'
// BETWEEN start AND end). After the deletions, EVERY surviving rowid must
// still be found by SeekToRowID — the SQL scan finds them, so a seek miss
// is a routing divergence.
func TestSeekToRowIDAfterChompRanges(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	bt := NewBTree(pg, 1, true)

	const N = 800
	insert := func(rid int64) {
		rec, _ := storage.EncodeRecord([]interface{}{int64(rid), fmt.Sprintf("blk-%d", rid)})
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: rid, Payload: rec}
		if err := bt.InsertCell(cell); err != nil {
			t.Fatalf("Insert %d: %v", rid, err)
		}
	}
	for rid := int64(1); rid <= N; rid++ {
		insert(rid)
	}

	// Interleaved chomp ranges (mimic the cascade's deleteFTSBlocks calls):
	// [1030..1031]-style pairs — here scaled to the 1..800 space: delete
	// [50..51], [150..151], ... every 100.
	delRange := func(lo, hi int64) {
		if _, err := bt.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID >= lo && cell.RowID <= hi
		}); err != nil {
			t.Fatalf("delete [%d..%d]: %v", lo, hi, err)
		}
	}
	for base := int64(50); base+1 <= N; base += 100 {
		delRange(base, base+1)
	}

	// Scan: collect surviving rowids.
	cur, err := bt.OpenCursor()
	if err != nil {
		t.Fatal(err)
	}
	surviving := map[int64]bool{}
	for {
		cell, rerr := cur.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		surviving[cell.RowID] = true
		if ok, nerr := cur.Next(); nerr != nil || !ok {
			break
		}
	}
	if len(surviving) == 0 {
		t.Fatal("scan found no rows")
	}

	// Seek every surviving rowid: each must be found.
	cur2, err := bt.OpenCursor()
	if err != nil {
		t.Fatal(err)
	}
	missed := 0
	for rid := range surviving {
		found, serr := cur2.SeekToRowID(rid)
		if serr != nil {
			t.Errorf("SeekToRowID(%d): seek error %v", rid, serr)
			missed++
		} else if !found {
			t.Errorf("SeekToRowID(%d): not found (scan sees it)", rid)
			missed++
		}
	}
	if missed > 0 {
		t.Fatalf("%d surviving rowids missed by SeekToRowID", missed)
	}
}
