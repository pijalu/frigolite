package btree

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// buildPoolTestTree creates a table btree at a fresh root with n rowid rows.
func buildPoolTestTree(t *testing.T, n int) (*pager.Pager, uint32) {
	t.Helper()
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	rootPg := pg.AllocatePage()
	root := rootPg.PageNum
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatalf("WritePage: %v", err)
	}
	bt := NewBTree(pg, root, true)
	for i := int64(1); i <= int64(n); i++ {
		rec, _ := storage.EncodeRecord([]interface{}{i, fmt.Sprintf("x%d", i)})
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: i, Payload: rec}
		if err := bt.InsertCell(cell); err != nil {
			t.Fatalf("InsertCell %d: %v", i, err)
		}
	}
	bt.Close()
	return pg, root
}

// TestPooledSeekLifecycle replays the statement lifecycle — fresh wrapper,
// open cursor, seek, read, close — over and over, and asserts every seek
// finds its row. A pooling reset bug (a stale cursor position, a retained
// page cache, a wrapper re-armed with the previous tenant's state) shows up
// as a missed seek.
func TestPooledSeekLifecycle(t *testing.T) {
	pg, root := buildPoolTestTree(t, 1000)
	for round := 0; round < 50; round++ {
		tree := NewBTree(pg, root, true)
		cur, err := tree.OpenCursor()
		if err != nil {
			t.Fatalf("round %d: OpenCursor: %v", round, err)
		}
		// Full scan sees every row exactly once.
		seen := make(map[int64]bool)
		for {
			payload, rowID, err := cur.ReadCellData()
			if err != nil {
				break
			}
			if seen[rowID] {
				t.Fatalf("round %d: row %d seen twice", round, rowID)
			}
			seen[rowID] = true
			if len(payload) == 0 {
				t.Fatalf("round %d: row %d empty payload", round, rowID)
			}
			ok, err := cur.Next()
			if err != nil || !ok {
				break
			}
		}
		if len(seen) != 1000 {
			t.Fatalf("round %d: scan saw %d rows, want 1000", round, len(seen))
		}
		tree.Close()

		// Point seeks on a fresh wrapper per round (the per-statement shape).
		for _, want := range []int64{1, 2, 500, 999, 1000} {
			tree2 := NewBTree(pg, root, true)
			cur2, err := tree2.OpenCursor()
			if err != nil {
				t.Fatalf("round %d seek %d: OpenCursor: %v", round, want, err)
			}
			found, err := cur2.SeekToRowID(want)
			if err != nil {
				t.Fatalf("round %d seek %d: %v", round, want, err)
			}
			if !found {
				t.Errorf("round %d: SeekToRowID(%d) not found", round, want)
			} else {
				payload, gotRowID, err := cur2.ReadCellData()
				if err != nil {
					t.Errorf("round %d seek %d: ReadCellData: %v", round, want, err)
				} else if gotRowID != want {
					t.Errorf("round %d: seek %d landed on %d", round, want, gotRowID)
				} else if len(payload) == 0 {
					t.Errorf("round %d: seek %d empty payload", round, want)
				}
			}
			tree2.Close()
		}
	}
}

// TestPooledScanAcrossWrappers mixes wrapper identities within one
// statement-shaped unit: scan with wrapper A while probing seeks with
// wrappers B and C, then close all — the registry-based save/restore must
// stay keyed on (pager, root), not wrapper identity.
func TestPooledScanAcrossWrappers(t *testing.T) {
	pg, root := buildPoolTestTree(t, 500)
	for round := 0; round < 20; round++ {
		a := NewBTree(pg, root, true)
		b := NewBTree(pg, root, true)
		curA, err := a.OpenCursor()
		if err != nil {
			t.Fatalf("round %d: OpenCursor A: %v", round, err)
		}
		n := 0
		for {
			if _, _, err := curA.ReadCellData(); err != nil {
				break
			}
			n++
			// A nested statement's write pattern: probe via another wrapper.
			curB, err := b.OpenCursor()
			if err != nil {
				t.Fatalf("round %d: OpenCursor B: %v", round, err)
			}
			if _, err := curB.SeekToRowID(int64(n)); err != nil {
				t.Fatalf("round %d: seek B: %v", round, err)
			}
			ok, err := curA.Next()
			if err != nil || !ok {
				break
			}
		}
		if n != 500 {
			t.Fatalf("round %d: scan saw %d rows, want 500", round, n)
		}
		a.Close()
		b.Close()
	}
}
