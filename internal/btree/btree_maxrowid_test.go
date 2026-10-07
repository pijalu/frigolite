package btree

// R12.BTREE pin: BTree.MaxRowID (the O(depth) rightmost-leaf descent,
// sqlite3BtreeLast + sqlite3BtreeIntegerKey) must answer exactly what the
// retired scanMaxRowID full table scan answered, for every tree shape the
// engine's rowid allocator can meet: empty, single row, ascending /
// descending / random fills, after deletes of the maximum rowid, after
// deletes of middle rows, negative-only fills, and the emptied-rightmost-leaf
// shapes per-row deletes leave behind. The WITHOUT ROWID (index b-tree)
// exclusion is pinned too: MaxRowID reports not-found there, matching the
// scan's rowid-0 answer.

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// fullScanMaxRowID is the RETIRED scanMaxRowID algorithm (execdml rowid.go,
// pre-R12): a cursor walk decoding every cell and taking the max. It is the
// oracle the replacement must be byte-equal to.
func fullScanMaxRowID(t *testing.T, tr *BTree) int64 {
	t.Helper()
	cursor, err := tr.OpenCursor()
	if err != nil {
		return 0
	}
	defer cursor.Close()
	var maxID int64
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		if cell.RowID > maxID {
			maxID = cell.RowID
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return maxID
}

func newMemTree(t *testing.T, isTable bool) *BTree {
	t.Helper()
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	rootPg, err := pg.ReadPage(1)
	if err != nil {
		t.Fatalf("ReadPage(1): %v", err)
	}
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	if !isTable {
		rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafIndex
	}
	setEmptyLeafContent(rootPg)
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatalf("WritePage: %v", err)
	}
	tr := NewBTree(pg, 1, isTable)
	t.Cleanup(func() { tr.Close() })
	return tr
}

func insertRowIDs(t *testing.T, tr *BTree, ids []int64) {
	t.Helper()
	for _, id := range ids {
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: []byte{0x01, id_byte(id)}}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatalf("InsertCell(%d): %v", id, err)
		}
	}
}

func id_byte(id int64) byte { return byte(id & 0xff) }

func deleteRowID(t *testing.T, tr *BTree, id int64) {
	t.Helper()
	n, err := tr.DeleteCellByRowID(id)
	if err != nil {
		t.Fatalf("DeleteCellByRowID(%d): %v", id, err)
	}
	if n != 1 {
		t.Fatalf("DeleteCellByRowID(%d) removed %d cells", id, n)
	}
}

// scanMaxRowIDFloor maps (MaxRowID id, found) to the retired scanMaxRowID's
// answer: the old cursor-scan accumulator started at 0 and only ever moved
// up, so empty trees AND all-negative-rowid trees both answered 0. The
// execdml wrapper (internal/execdml/rowid.go) applies this exact floor.
func scanMaxRowIDFloor(id int64, ok bool) int64 {
	if ok && id > 0 {
		return id
	}
	return 0
}

func checkMaxRowID(t *testing.T, tr *BTree, label string) {
	t.Helper()
	want := fullScanMaxRowID(t, tr)
	got, ok := tr.MaxRowID()
	if scanMaxRowIDFloor(got, ok) != want {
		t.Fatalf("%s: MaxRowID rightmost descent = %d (found=%v), full scan = %d", label, got, ok, want)
	}
	// LastRowID keeps its (id, error) contract: the TRUE maximum (not the
	// old scan's 0 floor), or 0 for an empty tree.
	lid, err := tr.LastRowID()
	if err != nil {
		t.Fatalf("%s: LastRowID: %v", label, err)
	}
	trueMax := got
	if !ok {
		trueMax = 0
	}
	if lid != trueMax {
		t.Fatalf("%s: LastRowID = %d, true max = %d", label, lid, trueMax)
	}
}

func TestMaxRowIDMatchesFullScan(t *testing.T) {
	asc := func(n int64) []int64 {
		ids := make([]int64, n)
		for i := range ids {
			ids[i] = int64(i) + 1
		}
		return ids
	}
	desc := func(n int64) []int64 {
		ids := make([]int64, n)
		for i := range ids {
			ids[i] = n - int64(i)
		}
		return ids
	}
	// Deterministic pseudo-random fill (xorshift), interleaved positive.
	rnd := func(n int64, seed uint32) []int64 {
		ids := make([]int64, 0, n)
		s := seed
		for i := int64(0); i < n; i++ {
			s ^= s << 13
			s ^= s >> 17
			s ^= s << 5
			ids = append(ids, int64(s%1_000_000)+1)
		}
		return ids
	}

	shapes := []struct {
		name string
		ids  []int64
	}{
		{"ascending4000", asc(4000)},
		{"descending4000", desc(4000)},
		{"random3000", rnd(3000, 0x12345678)},
		{"single", []int64{7}},
		{"negative_only", []int64{-9, -3, -100, -1, -55}},
		{"negative_mixed", []int64{-5, 3, -2, 10, 0, 7}},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			tr := newMemTree(t, true)
			insertRowIDs(t, tr, sh.ids)
			checkMaxRowID(t, tr, sh.name)

			// Delete the maximum rowid, then middle rows, re-checking each
			// step (each delete can empty the rightmost leaf and force the
			// walk's leftward fallback).
			want := fullScanMaxRowID(t, tr)
			if want != 0 {
				deleteRowID(t, tr, want)
				checkMaxRowID(t, tr, sh.name+" after delete max")
			}
			ids := append([]int64(nil), sh.ids...)
			deleted := map[int64]bool{want: true}
			removed := 0
			for _, id := range ids {
				if deleted[id] {
					continue
				}
				if removed++; removed%2 == 0 {
					deleteRowID(t, tr, id)
					deleted[id] = true
					checkMaxRowID(t, tr, sh.name+" after delete middle")
				}
			}
			// Drain to empty in random-ish order and re-check (empty tree).
			for _, id := range ids {
				if !deleted[id] {
					deleteRowID(t, tr, id)
					deleted[id] = true
				}
			}
			checkMaxRowID(t, tr, sh.name+" drained")
		})
	}
}

func TestMaxRowIDEmptyTree(t *testing.T) {
	tr := newMemTree(t, true)
	if id, ok := tr.MaxRowID(); ok || id != 0 {
		t.Fatalf("empty tree MaxRowID = (%d, %v), want (0, false)", id, ok)
	}
	if id, err := tr.LastRowID(); err != nil || id != 0 {
		t.Fatalf("empty tree LastRowID = (%d, %v), want (0, nil)", id, err)
	}
}

func TestMaxRowIDIndexTreeNotFound(t *testing.T) {
	// A WITHOUT ROWID table is an index b-tree: cells carry no rowid, the
	// scan answered 0, so MaxRowID must report not-found (0).
	tr := newMemTree(t, false)
	for i := 0; i < 50; i++ {
		cell := &storage.Cell{Type: storage.CellIndexLeaf, Payload: []byte{byte(i), 0x42}}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatalf("InsertCell(%d): %v", i, err)
		}
	}
	if id, ok := tr.MaxRowID(); ok || id != 0 {
		t.Fatalf("index tree MaxRowID = (%d, %v), want (0, false)", id, ok)
	}
}
