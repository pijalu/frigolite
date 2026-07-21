package btree

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

func TestInsertAndScan(t *testing.T) {
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	pg.WritePage(rootPg)
	bt := NewBTree(pg, 1, true)

	// Insert a cell
	rec, _ := storage.EncodeRecord([]interface{}{int64(1), "hello"})
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   1,
		Payload: rec,
	}
	if err := bt.InsertCell(cell); err != nil {
		t.Fatalf("InsertCell: %v", err)
	}

	// Scan back
	c, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	got, err := c.ReadCell()
	if err != nil {
		t.Fatalf("ReadCell: %v", err)
	}
	if got.RowID != 1 {
		t.Errorf("expected rowid 1, got %d", got.RowID)
	}
}

func TestSplit(t *testing.T) {
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	pg.WritePage(rootPg)
	bt := NewBTree(pg, 1, true)

	// Insert enough cells to trigger a split
	for i := int64(1); i <= 500; i++ {
		rec, _ := storage.EncodeRecord([]interface{}{i, i * 10})
		cell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   i,
			Payload: rec,
		}
		if err := bt.InsertCell(cell); err != nil {
			t.Fatalf("InsertCell at %d: %v", i, err)
		}
	}

	// Verify all cells are accessible
	c, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	count := 0
	for {
		_, err := c.ReadCell()
		if err != nil {
			break
		}
		count++
		ok, err := c.Next()
		if err != nil || !ok {
			break
		}
	}
	if count != 500 {
		t.Errorf("expected 500 cells, got %d", count)
	}
}
