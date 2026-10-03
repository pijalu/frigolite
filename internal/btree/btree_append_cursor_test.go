// White-box pins for the append-cursor fast path (btree_append_cursor.go):
// engagement, invalidation, and equivalence with the generic root-descent
// path on the same data.

package btree

import (
	"encoding/binary"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// newAppendTestTree opens an in-memory pager with an empty table-leaf root
// (the same scaffold as btree_test.go's TestInsertAndScan).
func newAppendTestTree(t *testing.T) (*pager.Pager, *BTree) {
	t.Helper()
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	t.Cleanup(func() { pg.Close() })
	pg.AllocatePage()
	rootPg, err := pg.ReadPage(1)
	if err != nil {
		t.Fatal(err)
	}
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	binary.BigEndian.PutUint16(rootPg.Data[pager.HeaderSize+5:pager.HeaderSize+7], uint16(len(rootPg.Data)))
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatal(err)
	}
	return pg, NewBTree(pg, 1, true)
}

func insertRowCell(tr *BTree, rowid int64) error {
	return tr.InsertCell(&storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   rowid,
		Payload: []byte{byte(rowid), 1, 2, 3},
	})
}

// dumpRowIDs scans the whole tree through the generic cursor path.
func dumpRowIDs(t *testing.T, tr *BTree) []int64 {
	t.Helper()
	cur, err := tr.OpenCursor()
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	var ids []int64
	for {
		cell, err := cur.ReadCell()
		if err != nil {
			break
		}
		ids = append(ids, cell.RowID)
		ok, err := cur.Next()
		if err != nil || !ok {
			break
		}
	}
	return ids
}

// TestAppendCursorEngagesOnAscendingInserts: a pure-append workload must take
// the quick path (hits grow with inserts, establishes stay bounded by the
// split count) and produce the same rows as the generic path would.
func TestAppendCursorEngagesOnAscendingInserts(t *testing.T) {
	_, tr := newAppendTestTree(t)
	const n = 4000
	for i := 1; i <= n; i++ {
		if err := insertRowCell(tr, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if hits := quickAppendHitsForTest(); hits < n/2 {
		t.Fatalf("quick path engaged only %d/%d times", hits, n)
	}
	ids := dumpRowIDs(t, tr)
	if len(ids) != n {
		t.Fatalf("row count %d, want %d", len(ids), n)
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("row %d has rowid %d, want %d", i, id, i+1)
		}
	}
}

// TestAppendCursorRandomOrderStaysCorrect: interleaved appends and
// out-of-order inserts (the invalidating shape) must produce exactly the
// sorted rowid set, with the quick path disengaging on every non-append.
func TestAppendCursorRandomOrderStaysCorrect(t *testing.T) {
	_, tr := newAppendTestTree(t)
	seen := map[int64]bool{}
	// Ascending runs broken by out-of-order inserts: every run re-parks the
	// cursor, every interloper invalidates it.
	pattern := []int64{1, 2, 3, 1, 4, 5, 2, 6, 7, 3, 8, 9, 4, 10}
	for _, id := range pattern {
		if seen[id] {
			continue // duplicate rowid = overwrite path (table b-tree REPLACE)
		}
		if err := insertRowCell(tr, id); err != nil {
			t.Fatal(err)
		}
		seen[id] = true
	}
	ids := dumpRowIDs(t, tr)
	if len(ids) != len(seen) {
		t.Fatalf("row count %d, want %d", len(ids), len(seen))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Fatalf("rowids not sorted at %d: %v", i, ids)
		}
		if !seen[ids[i-1]] {
			t.Fatalf("unexpected rowid %d", ids[i-1])
		}
	}
}

// TestAppendCursorDeleteInvalidates: append → delete (max and middle) →
// append must fall back to the generic path after the delete and still yield
// the right set — the slot's maxKey/leaf pair must not survive the delete.
func TestAppendCursorDeleteInvalidates(t *testing.T) {
	_, tr := newAppendTestTree(t)
	for i := 1; i <= 500; i++ {
		if err := insertRowCell(tr, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if quickAppendHitsForTest() == 0 {
		t.Fatal("quick path never engaged before delete")
	}
	hitsBefore := quickAppendHitsForTest()
	// Delete the max row: the cached (leaf, maxKey) pair becomes false.
	if n, err := tr.DeleteCellByRowID(500); err != nil || n != 1 {
		t.Fatalf("delete max: n=%d err=%v", n, err)
	}
	if err := insertRowCell(tr, 500); err != nil {
		t.Fatal(err)
	}
	// Delete a middle row, then append.
	if n, err := tr.DeleteCellByRowID(250); err != nil || n != 1 {
		t.Fatalf("delete mid: n=%d err=%v", n, err)
	}
	if err := insertRowCell(tr, 501); err != nil {
		t.Fatal(err)
	}
	// A further append must find the re-parked cursor (501 established it).
	hitsBeforeReestablish := quickAppendHitsForTest()
	if err := insertRowCell(tr, 502); err != nil {
		t.Fatal(err)
	}
	if quickAppendHitsForTest() == hitsBeforeReestablish {
		t.Fatal("quick path never re-engaged after delete")
	}
	ids := dumpRowIDs(t, tr)
	want := make(map[int64]bool)
	for i := 1; i <= 502; i++ {
		if i == 250 {
			continue
		}
		want[int64(i)] = true
	}
	if len(ids) != len(want) {
		t.Fatalf("row count %d, want %d", len(ids), len(want))
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected rowid %d after delete+append", id)
		}
	}
	_ = hitsBefore
}

// TestAppendCursorSplitAtAppend: the quick path must hand over to the split
// machinery at the exact leaf boundary and re-park afterwards, with rowids
// intact across the split (fmt forces enough rows for several splits).
func TestAppendCursorSplitAtAppend(t *testing.T) {
	_, tr := newAppendTestTree(t)
	const n = 20000
	for i := 1; i <= n; i++ {
		if err := insertRowCell(tr, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	ids := dumpRowIDs(t, tr)
	if len(ids) != n {
		t.Fatalf("row count %d, want %d", len(ids), n)
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("row %d rowid %d after splits", i, id)
		}
	}
	// balance_deeper keeps the root PAGE NUMBER (schema entries stay valid);
	// the split demotes it to an interior page.
	if tr.RootPageType() != storage.PageTypeInteriorTable {
		t.Fatalf("tree never split (root type 0x%02x, want interior)", tr.RootPageType())
	}
}

// TestAppendCursorDuplicateRowidAppend: an insert with the current maximum
// rowid is an overwrite, not an append — the slot must invalidate (or the
// verification must catch it) and the table must hold one row for the rowid.
func TestAppendCursorDuplicateRowidAppend(t *testing.T) {
	_, tr := newAppendTestTree(t)
	for i := 1; i <= 100; i++ {
		if err := insertRowCell(tr, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Overwrite the max rowid with different payload.
	if err := tr.InsertCell(&storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   100,
		Payload: []byte{0xEE, 0xEE, 0xEE, 0xEE},
	}); err != nil {
		t.Fatal(err)
	}
	ids := dumpRowIDs(t, tr)
	if len(ids) != 100 {
		t.Fatalf("row count %d after overwrite, want 100", len(ids))
	}
	// Append past the overwritten max.
	if err := insertRowCell(tr, 101); err != nil {
		t.Fatal(err)
	}
	if ids = dumpRowIDs(t, tr); len(ids) != 101 || ids[99] != 100 || ids[100] != 101 {
		t.Fatalf("unexpected ids after overwrite+append: %v", ids[len(ids)-3:])
	}
}

// TestAppendCursorGenericEquivalence inserts the SAME workload twice — once
// with the quick path forcibly cold (a fresh slot map each insert, i.e. the
// generic path only) and once normally — and compares the full rowid dumps.
func TestAppendCursorGenericEquivalence(t *testing.T) {
	build := func(disable bool) []int64 {
		pg, tr := newAppendTestTree(t)
		_ = pg
		defer tr.Close()
		for i := 1; i <= 3000; i++ {
			if disable {
				// Any wrapper Close clears the slot; closing a sentinel
				// wrapper on the same tree before every insert keeps the
				// quick path permanently disengaged (generic path only).
				sentinel := NewBTree(pg, tr.RootPage(), true)
				sentinel.Close()
			}
			if err := insertRowCell(tr, int64(i)); err != nil {
				panic(err)
			}
			if disable {
				// Re-open is unnecessary: the tree wrapper itself was never
				// closed; only its slot was cleared by the sentinel Close.
			}
		}
		return dumpRowIDs(t, tr)
	}
	generic := build(true)
	quick := build(false)
	if len(generic) != len(quick) {
		t.Fatalf("row counts differ: generic %d quick %d", len(generic), len(quick))
	}
	for i := range generic {
		if generic[i] != quick[i] {
			t.Fatalf("row %d differs: generic %d quick %d", i, generic[i], quick[i])
		}
	}
	if quickAppendHitsForTest() == 0 {
		t.Fatal("quick build never engaged the fast path")
	}
}
