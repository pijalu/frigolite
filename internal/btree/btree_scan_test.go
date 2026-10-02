package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// scanBatchTree builds a table btree with n rows (rowid i, payload "v<i>") so
// multi-page walks can be exercised.
func scanBatchTree(t *testing.T, n int) (*BTree, []int) {
	t.Helper()
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	pg.AllocatePage()
	rootPg, err := pg.ReadPage(1)
	if err != nil {
		t.Fatal(err)
	}
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatal(err)
	}
	tr := NewBTree(pg, 1, true)
	want := make([]int, 0, n)
	for i := 1; i <= n; i++ {
		payload := []byte(fmt.Sprintf("v%d", i))
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: int64(i), Payload: payload}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatal(err)
		}
		want = append(want, i)
	}
	return tr, want
}

// TestScanTableLeavesOrdersCells walks a multi-page tree and verifies the
// batch yields every cell in rowid order with the cursor path's payloads.
func TestScanTableLeavesOrdersCells(t *testing.T) {
	for _, n := range []int{1, 10, 500, 4000} { // 4000 rows forces interior pages
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			tr, want := scanBatchTree(t, n)
			c, err := tr.OpenCursor()
			if err != nil {
				t.Fatal(err)
			}
			defer tr.Close()
			var gotRowIDs []int64
			var pages int
			saved, err := c.ScanTableLeaves(func(b *LeafBatch) (bool, error) {
				pages++
				for i := 0; i < b.CellCount(); i++ {
					payload, rowID, err := b.Cell(i)
					if err != nil {
						return false, err
					}
					if !bytes.Equal(payload, []byte(fmt.Sprintf("v%d", rowID))) {
						t.Fatalf("page %d cell %d: payload %q", b.pg.PageNum, i, payload)
					}
					gotRowIDs = append(gotRowIDs, rowID)
				}
				return false, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if saved {
				t.Fatal("unexpected save without a nested write")
			}
			if pages < 1 {
				t.Fatal("no pages walked")
			}
			if len(gotRowIDs) != len(want) {
				t.Fatalf("got %d cells, want %d", len(gotRowIDs), len(want))
			}
			for i, id := range gotRowIDs {
				if id != int64(want[i]) {
					t.Fatalf("cell %d: rowid %d, want %d", i, id, want[i])
				}
			}
		})
	}
}

// TestScanTableLeavesOverflowCells verifies a spilled (overflow-chain) payload
// comes back reassembled, like the cursor path's ReadCellData.
func TestScanTableLeavesOverflowCells(t *testing.T) {
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	pg.AllocatePage()
	rootPg, err := pg.ReadPage(1)
	if err != nil {
		t.Fatal(err)
	}
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatal(err)
	}
	tr := NewBTree(pg, 1, true)
	big := bytes.Repeat([]byte("Z"), 9000)
	for i := 1; i <= 20; i++ {
		payload := []byte(fmt.Sprintf("v%d", i))
		if i == 10 {
			payload = big
		}
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: int64(i), Payload: payload}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatal(err)
		}
	}
	c, err := tr.OpenCursor()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	count := 0
	if _, err := c.ScanTableLeaves(func(b *LeafBatch) (bool, error) {
		for i := 0; i < b.CellCount(); i++ {
			payload, rowID, err := b.Cell(i)
			if err != nil {
				return false, err
			}
			if rowID == 10 {
				if !bytes.Equal(payload, big) {
					t.Fatalf("overflow payload len %d, want %d", len(payload), len(big))
				}
			} else if !bytes.Equal(payload, []byte(fmt.Sprintf("v%d", rowID))) {
				t.Fatalf("payload %q", payload)
			}
			count++
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 20 {
		t.Fatalf("got %d cells, want 20", count)
	}
}

// TestScanTableLeavesEmptyLeaves verifies the walk skips empty leaves (kept
// in the tree after deletes) exactly like the cursor path's skipEmptyLeaves.
func TestScanTableLeavesEmptyLeaves(t *testing.T) {
	tr, want := scanBatchTree(t, 3000)
	// Delete enough rows to empty at least one leaf (the engine keeps the
	// empty page in the tree).
	for i := 1; i <= 200; i++ {
		if _, err := tr.DeleteCellByRowID(int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	want = want[200:]
	c, err := tr.OpenCursor()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	var got []int64
	if _, err := c.ScanTableLeaves(func(b *LeafBatch) (bool, error) {
		for i := 0; i < b.CellCount(); i++ {
			_, rowID, err := b.Cell(i)
			if err != nil {
				return false, err
			}
			got = append(got, rowID)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d cells, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != int64(want[i]) {
			t.Fatalf("cell %d: rowid %d, want %d", i, got[i], want[i])
		}
	}
}

// TestScanTableLeavesSavedDeclines verifies a cursor whose position was saved
// (nested-write simulation) before the walk reports saved=true immediately,
// and that the pure cursor loop still serves the rest of the scan after the
// restore dance.
func TestScanTableLeavesSavedDeclines(t *testing.T) {
	tr, want := scanBatchTree(t, 100)
	c, err := tr.OpenCursor()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	// Simulate a nested write's save: mark the position require-seek.
	c.savedRowID = 5
	c.savedKey = nil
	c.skipNext = 0
	c.state = cursorRequireSeek
	calls := 0
	saved, err := c.ScanTableLeaves(func(b *LeafBatch) (bool, error) {
		calls++
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !saved {
		t.Fatal("want saved=true for a saved cursor")
	}
	if calls != 0 {
		t.Fatalf("fn called %d times, want 0", calls)
	}
	// The cursor loop's resume: one Next steps off the saved cell (restore
	// re-seeks to rowid 5, which still exists — skipNext stays 0 — and the
	// Next advances past it).
	if ok, err := c.Next(); err != nil || !ok {
		t.Fatalf("step-off Next: ok=%v err=%v", ok, err)
	}
	count := 0
	for {
		_, rowID, err := c.ReadCellData()
		if err != nil {
			break
		}
		if rowID <= 5 {
			t.Fatalf("rowid %d re-served after the step-off Next", rowID)
		}
		count++
		if ok, err := c.Next(); err != nil || !ok {
			break
		}
	}
	// Rowids 1..5 were consumed before/with the step-off Next (the saved
	// position is 5 and the Next steps off it); 6..n remain.
	if count != len(want)-5 {
		t.Fatalf("cursor loop got %d cells, want %d", count, len(want)-5)
	}
}
