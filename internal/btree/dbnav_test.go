package btree

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

var nextChildCalls int64

func init() {
	// hook
}

func TestDbgNavNext(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	tr := NewBTree(pg, 1, true)
	for j := 0; j < 30; j++ {
		pl := make([]byte, 800)
		for k := range pl { pl[k] = byte(k) }
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: int64(j+1), Payload: pl}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatalf("insert %d: %v", j+1, err)
		}
	}
	// Delete all
	_, err := tr.DeleteCellsWhere(func(c *storage.Cell) bool { return true })
	if err != nil { t.Fatal(err) }
	fmt.Printf("After delete: freelist=%d, pages=%d\n", pg.FreelistCount(), pg.NumPages())

	// Try to open a cursor
	c, err := tr.OpenCursor()
	if err != nil { t.Fatal(err) }
	fmt.Printf("OpenCursor OK\n")

	count := 0
	atomic.StoreInt64(&nextChildCalls, 0)
	for {
		ok, err := c.Next()
		if err != nil { fmt.Printf("Next err: %v\n", err); break }
		if !ok { break }
		count++
		if count > 100 { fmt.Printf("OVER 100 - stopping\n"); break }
	}
	fmt.Printf("count=%d navCalls=%d\n", count, atomic.LoadInt64(&nextChildCalls))
}
