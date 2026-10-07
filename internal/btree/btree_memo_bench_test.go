package btree

// Descent cost micro-benchmark (R11.BTREEMEMO lever 4): SeekToRowID's
// root-to-leaf walk after the parse-memo refresh — every level should be a
// read-only memo hit (or, right after a mutation of the target leaf, a
// served refresh) instead of a re-parse.

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

func benchDescentTree(b *testing.B, n int) (*BTree, *pager.Pager) {
	b.Helper()
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	pg.AllocatePage()
	rootPg, err := pg.ReadPage(1)
	if err != nil {
		b.Fatalf("ReadPage(1): %v", err)
	}
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	if err := pg.WritePage(rootPg); err != nil {
		b.Fatalf("WritePage: %v", err)
	}
	bt := NewBTree(pg, 1, true)
	for i := 1; i <= n; i++ {
		rec, _ := storage.EncodeRecord([]interface{}{int64(i), int64(i * 7 % 1000003)})
		if err := bt.InsertCell(&storage.Cell{Type: storage.CellTableLeaf, RowID: int64(i), Payload: rec}); err != nil {
			b.Fatalf("InsertCell(%d): %v", i, err)
		}
	}
	return bt, pg
}

// BenchmarkSeekToRowIDDescent measures point seeks over an n-row tree with
// the standard bench probe pattern ((i*7919)%n+1): the descent walks
// interior levels (memo hits) and the leaf, where the previous statement's
// write refreshed the memo.
func benchmarkSeekToRowIDDescent(b *testing.B, n int) {
	bt, _ := benchDescentTree(b, n)
	c, err := bt.OpenCursorAtRoot()
	if err != nil {
		b.Fatalf("OpenCursorAtRoot: %v", err)
	}
	b.ResetTimer()
	found := 0
	for i := 0; i < b.N; i++ {
		ok, serr := c.SeekToRowID(int64(i*7919)%int64(n) + 1)
		if serr != nil {
			b.Fatalf("SeekToRowID: %v", serr)
		}
		if ok {
			found++
		}
	}
	b.StopTimer()
	if found == 0 {
		b.Fatalf("no seeks matched")
	}
}

func BenchmarkSeekToRowIDDescent100k(b *testing.B) { benchmarkSeekToRowIDDescent(b, 100000) }
func BenchmarkSeekToRowIDDescent300k(b *testing.B) { benchmarkSeekToRowIDDescent(b, 300000) }

// BenchmarkSeekAfterRewriteDescent is the point-statement shape: every
// iteration rewrites the row's cell (dup-drop + writeLeafCell — header
// mutation), then the NEXT iteration's seek of a nearby rowid hits the
// just-mutated leaf. On main that seek re-parses; with the write-path memo
// refresh it is a served hit.
func benchmarkSeekAfterRewriteDescent(b *testing.B, n int) {
	bt, _ := benchDescentTree(b, n)
	c, err := bt.OpenCursorAtRoot()
	if err != nil {
		b.Fatalf("OpenCursorAtRoot: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rid := int64(i%n) + 1
		if _, serr := c.SeekToRowID(rid); serr != nil {
			b.Fatalf("SeekToRowID: %v", serr)
		}
		rec, _ := storage.EncodeRecord([]interface{}{rid, int64(i)})
		if ierr := bt.InsertCell(&storage.Cell{Type: storage.CellTableLeaf, RowID: rid, Payload: rec}); ierr != nil {
			b.Fatalf("InsertCell: %v", ierr)
		}
	}
}

func BenchmarkSeekAfterRewriteDescent100k(b *testing.B) { benchmarkSeekAfterRewriteDescent(b, 100000) }
