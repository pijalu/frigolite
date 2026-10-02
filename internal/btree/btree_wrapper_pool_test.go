package btree

import (
	"fmt"
	"sync"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// buildWrapperPoolTree creates a table btree at a fresh root with n rowid
// rows (same shape as buildPoolTestTree).
func buildWrapperPoolTree(t *testing.T, n int) (*pager.Pager, uint32) {
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

// TestWrapperPoolConcurrentLifecycle is the -race stress test for wrapper
// recycling: many goroutines run the statement lifecycle (NewBTree -> open
// cursor -> seek/scan -> Close) over and over, so wrappers churn through the
// pool concurrently. Every cycle must observe exactly its own rows; a
// double-Put (two owners for one wrapper), a missed re-arm (previous
// tenant's keyCompare/cursors/pager leaking), or a use-after-pool shows up
// as a wrong row set or a panic the race detector flags.
func TestWrapperPoolConcurrentLifecycle(t *testing.T) {
	pg, root := buildWrapperPoolTree(t, 200)
	const goroutines = 8
	const rounds = 300
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				// Close is called twice per generation on purpose: it must be
				// idempotent and must not re-Put the wrapper (a second Put
				// would hand one wrapper to two owners — the race detector
				// sees the interleaved field writes as a data race).
				tree := NewBTree(pg, root, true)
				cur, err := tree.OpenCursor()
				if err != nil {
					errs <- fmt.Errorf("g%d r%d: OpenCursor: %w", id, r, err)
					return
				}
				count := 0
				var lastRowID int64
				for {
					_, rowID, err := cur.ReadCellData()
					if err != nil {
						break
					}
					if rowID <= lastRowID {
						errs <- fmt.Errorf("g%d r%d: rowid order broken: %d after %d", id, r, rowID, lastRowID)
						return
					}
					lastRowID = rowID
					count++
					ok, err := cur.Next()
					if err != nil || !ok {
						break
					}
				}
				if count != 200 {
					errs <- fmt.Errorf("g%d r%d: saw %d rows, want 200", id, r, count)
					return
				}
				tree.Close()
				tree.Close()
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestWrapperPoolReArmFresh pins the re-arm contract: a recycled wrapper
// must be indistinguishable from a fresh one — keyCompare from the previous
// tenant must not leak, the cursors slice must be empty, and cellScratch /
// delArena staging must be cleared.
func TestWrapperPoolReArmFresh(t *testing.T) {
	pg, root := buildWrapperPoolTree(t, 10)
	// Tenant 1: customize everything initFrom must clear.
	t1 := NewBTree(pg, root, true)
	t1.SetKeyCompare(func(a, b []byte) int { return 1 })
	if c, err := t1.OpenCursor(); err != nil {
		t.Fatalf("OpenCursor: %v", err)
	} else {
		if _, err := c.SeekToRowID(3); err != nil {
			t.Fatalf("SeekToRowID: %v", err)
		}
	}
	t1.cellScratch = make([]byte, 64)
	t1.delArena = make([]byte, 64)
	t1.Close()
	// Tenant 2: recycle through the pool.
	t2 := NewBTree(pg, root, true)
	if t2.keyCompare != nil {
		t.Fatal("recycled wrapper carried the previous tenant's keyCompare")
	}
	if len(t2.cursors) != 0 {
		t.Fatalf("recycled wrapper carries %d cursors, want 0", len(t2.cursors))
	}
	if t2.cellScratch != nil || t2.delArena != nil {
		t.Fatal("recycled wrapper carried staging buffers")
	}
	if t2.closed {
		t.Fatal("recycled wrapper is still marked closed")
	}
	// Behavioral: the default (raw byte) comparator must be in effect —
	// a leaked "always 1" comparator corrupts index ordering.
	cur, err := t2.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor t2: %v", err)
	}
	count := 0
	for {
		_, rowID, err := cur.ReadCellData()
		if err != nil {
			break
		}
		count++
		_ = rowID
		ok, err := cur.Next()
		if err != nil || !ok {
			break
		}
	}
	if count != 10 {
		t.Fatalf("recycled wrapper scan saw %d rows, want 10", count)
	}
	t2.Close()
}

// TestWrapperPoolDoubleCloseNoDoublePut pins the once-only pool return: a
// double Close must not hand the same wrapper to two owners.
func TestWrapperPoolDoubleCloseNoDoublePut(t *testing.T) {
	pg, root := buildWrapperPoolTree(t, 5)
	t1 := NewBTree(pg, root, true)
	t1.Close()
	t1.Close() // idempotent, no second Put
	// Drain: the pool must hold at most ONE copy of t1. Take three wrappers;
	// no two may be the same object.
	seen := map[*BTree]bool{}
	for i := 0; i < 3; i++ {
		bt := NewBTree(pg, root, true)
		if seen[bt] {
			t.Fatal("double Close handed one wrapper to two owners")
		}
		seen[bt] = true
		defer bt.Close()
	}
}
