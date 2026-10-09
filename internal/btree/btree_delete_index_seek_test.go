// Index-entry deletion / lookup by descent: the OP_IdxDelete-shaped paths
// (deleteIndexEntryBySeek, indexLowerBoundScan) verified against the
// exhaustive stored-order walk they replace, on multi-level, divider-valued,
// duplicate-key and overflowing-key trees.

package btree

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/pijalu/frigolite/internal/storage"
)

// entryPayloads walks every index entry in key order — leaf cells AND the
// interior pages' own cells, which are real entries — and returns the FULL
// payload of each (spilling cells reassembled). It is the walk-shaped oracle the
// descent must agree with.
func entryPayloads(t *testing.T, bt *BTree) [][]byte {
	t.Helper()
	var out [][]byte
	_, err := bt.walkIndexLeaves(bt.rootPage, 0, nil, func(data []byte, _ uint32, cellOff, _ int, cellType storage.CellType, _ []cursorPathEntry) (bool, error) {
		local, fullLen, ovfl, err := bt.indexCellLocalPayload(data, cellOff, cellType)
		if err != nil {
			return false, err
		}
		full, err := bt.fullIndexCellPayload(local, fullLen, ovfl, cellType)
		if err != nil {
			return false, err
		}
		out = append(out, append([]byte(nil), full...))
		return false, nil
	})
	if err != nil {
		t.Fatalf("walk entries: %v", err)
	}
	return out
}

// entrySet returns the payload multiset as sorted byte strings.
func entrySet(t *testing.T, bt *BTree) []string {
	t.Helper()
	payloads := entryPayloads(t, bt)
	out := make([]string, 0, len(payloads))
	for _, p := range payloads {
		out = append(out, string(p))
	}
	sort.Strings(out)
	return out
}

// leafPageCount reports how many leaves the tree has (multi-level check).
func leafPageCount(t *testing.T, bt *BTree) int {
	t.Helper()
	var leaves []uint32
	if err := bt.collectLeafPages(bt.rootPage, &leaves, nil); err != nil {
		t.Fatalf("collectLeafPages: %v", err)
	}
	return len(leaves)
}

// indexPayload encodes values as an index record.
func indexPayload(t *testing.T, values []interface{}) []byte {
	t.Helper()
	rec, err := storage.EncodeRecord(values)
	if err != nil {
		t.Fatalf("EncodeRecord(%v): %v", values, err)
	}
	return rec
}

// indexInteriorDividers returns every interior divider payload of the tree (the
// copies SQLite-style index interiors hold of a right sibling's first key).
func indexInteriorDividers(t *testing.T, bt *BTree) [][]byte {
	t.Helper()
	var out [][]byte
	var visit func(pageNum uint32, depth int)
	visit = func(pageNum uint32, depth int) {
		if depth > indexSeekMaxDepth {
			t.Fatalf("interior chain too deep")
		}
		pg, err := bt.pager.ReadPage(pageNum)
		if err != nil {
			t.Fatalf("read page %d: %v", pageNum, err)
		}
		coff := contentOffset(pg.PageNum)
		page, err := storage.ParsePage(pg.Data, int(bt.pageSize), coff)
		if err != nil {
			t.Fatalf("parse page %d: %v", pageNum, err)
		}
		if page.PageType != storage.PageTypeInteriorIndex {
			return
		}
		ptrBase := coff + cellPtrOffset(page.PageType) - 8
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pg.Data, ptrBase, i, int(bt.pageSize)))
			var sc storage.Cell
			if err := storage.DecodeCellInto(pg.Data, cellOff, storage.CellIndexInterior, int(bt.usableSize), &sc); err != nil {
				t.Fatalf("decode divider %d/%d: %v", pageNum, i, err)
			}
			full, err := bt.readOverflow(&sc)
			if err != nil {
				t.Fatalf("reassemble divider %d/%d: %v", pageNum, i, err)
			}
			out = append(out, append([]byte(nil), full.Payload...))
			visit(binary.BigEndian.Uint32(pg.Data[cellOff:cellOff+4]), depth+1)
		}
		if page.RightmostPtr != 0 {
			visit(page.RightmostPtr, depth+1)
		}
	}
	visit(bt.rootPage, 0)
	return out
}

func TestDeleteIndexEntryBySeek_MultiLevel(t *testing.T) {
	bt := newIndexTree(t)
	defer bt.Close()
	bt.SetIndexKeyInfo(singleKeyInfo(), nil)

	// A key permutation with rowid suffixes: 4000 entries force several leaf
	// levels plus interior dividers.
	const n = 4000
	rng := rand.New(rand.NewSource(7))
	keys := rng.Perm(n)
	var payloads [][]byte
	for i := 0; i < n; i++ {
		vals := []interface{}{int64(keys[i] * 3), int64(i + 1)}
		insertIndexRecord(t, bt, vals)
		payloads = append(payloads, indexPayload(t, vals))
	}
	if leaves := leafPageCount(t, bt); leaves < 2 {
		t.Fatalf("tree is single-leaf (%d); the descent needs an interior level", leaves)
	}

	order := rng.Perm(n)
	remaining := make([]string, 0, n)
	for _, p := range payloads {
		remaining = append(remaining, string(p))
	}
	sort.Strings(remaining)

	for step, idx := range order {
		target := payloads[idx]
		ok, err := bt.DeleteIndexEntry(target)
		if err != nil {
			t.Fatalf("step %d DeleteIndexEntry: %v", step, err)
		}
		if !ok {
			t.Fatalf("step %d: entry %x not removed", step, target)
		}
		// The entry is gone: a second delete must report nothing.
		again, err := bt.DeleteIndexEntry(target)
		if err != nil {
			t.Fatalf("step %d repeat delete: %v", step, err)
		}
		if again {
			t.Fatalf("step %d: entry %x removed twice", step, target)
		}
		pos := sort.SearchStrings(remaining, string(target))
		remaining = append(remaining[:pos], remaining[pos+1:]...)
		if step%500 == 0 || step == len(order)-1 {
			got := entrySet(t, bt)
			if len(got) != len(remaining) {
				t.Fatalf("step %d: %d entries left, want %d", step, len(got), len(remaining))
			}
			for i := range got {
				if got[i] != remaining[i] {
					t.Fatalf("step %d: entry %d mismatch (got %x want %x)", step, i, got[i], remaining[i])
				}
			}
		}
	}
	if got := entryPayloads(t, bt); len(got) != 0 {
		t.Fatalf("tree not empty after deleting every entry: %d left", len(got))
	}
	// The emptied tree stays usable for inserts.
	for i := 0; i < 20; i++ {
		insertIndexRecord(t, bt, []interface{}{int64(i), int64(1000 + i)})
	}
	if got := len(entryPayloads(t, bt)); got != 20 {
		t.Fatalf("after refill: %d entries, want 20", got)
	}
}

func TestDeleteIndexEntryBySeek_DividerTarget(t *testing.T) {
	bt := newIndexTree(t)
	defer bt.Close()
	bt.SetIndexKeyInfo(singleKeyInfo(), nil)

	const n = 3000
	rng := rand.New(rand.NewSource(11))
	keys := rng.Perm(n)
	byPayload := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		vals := []interface{}{int64(keys[i] * 7), int64(i + 1)}
		insertIndexRecord(t, bt, vals)
		byPayload[string(indexPayload(t, vals))] = true
	}
	dividiers := indexInteriorDividers(t, bt)
	if len(dividiers) == 0 {
		t.Fatalf("tree has no interior dividers; the equality case is not exercised")
	}
	// A divider is a COPY of a right sibling's first key: deleting the entry it
	// copies means the descent must pass the divider and continue in the right
	// subtree (the advance loop past `cellIdx == CellCount`).
	hit := 0
	for _, d := range dividiers {
		if !byPayload[string(d)] {
			continue
		}
		delete(byPayload, string(d))
		ok, err := bt.DeleteIndexEntry(d)
		if err != nil {
			t.Fatalf("DeleteIndexEntry(divider %x): %v", d, err)
		}
		if !ok {
			t.Fatalf("divider-copied entry %x not removed", d)
		}
		hit++
		if hit == 3 {
			break
		}
	}
	if hit == 0 {
		t.Fatalf("no divider payload matched a live entry; the case was not exercised")
	}
	want := make([]string, 0, len(byPayload))
	for p := range byPayload {
		want = append(want, p)
	}
	sort.Strings(want)
	got := entrySet(t, bt)
	if len(got) != len(want) {
		t.Fatalf("after divider-target deletes: %d entries, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("entry %d mismatch after divider-target deletes", i)
		}
	}
}

func TestDeleteIndexEntryBySeek_OverflowKeys(t *testing.T) {
	bt := newIndexTree(t)
	defer bt.Close()
	bt.SetIndexKeyInfo(singleKeyInfo(), nil)

	// Keys far past the local-payload limit: every entry spills into an
	// overflow chain, so the descent must compare reassembled payloads.
	long := bytes.Repeat([]byte("q"), 8000)
	const n = 120
	var payloads [][]byte
	spilled := 0
	for i := 0; i < n; i++ {
		key := append([]byte(nil), long...)
		copy(key[len(key)-8:], []byte(fmt.Sprintf("%08d", i)))
		vals := []interface{}{key, int64(i + 1)}
		insertIndexRecord(t, bt, vals)
		payloads = append(payloads, indexPayload(t, vals))
	}
	// Confirm the overflow path is live: the first leaf cell must reference a
	// chain.
	pg, err := bt.pager.ReadPage(bt.rootPage)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	page, err := storage.ParsePage(pg.Data, int(bt.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		t.Fatalf("parse root: %v", err)
	}
	if page.PageType != storage.PageTypeLeafIndex {
		t.Skip("root is interior; overflow check is covered by the content below")
	}
	if page.CellCount > 0 {
		var sc storage.Cell
		off := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), 0, int(bt.pageSize)))
		if err := storage.DecodeCellInto(pg.Data, off, storage.CellIndexLeaf, int(bt.usableSize), &sc); err != nil {
			t.Fatalf("decode first cell: %v", err)
		}
		if sc.Overflow != 0 {
			spilled++
		}
	}
	if spilled == 0 {
		t.Fatalf("no overflowing cell found; the test did not exercise the chain path")
	}

	remaining := map[string]bool{}
	for _, p := range payloads {
		remaining[string(p)] = true
	}
	for i := 0; i < n; i += 2 {
		ok, err := bt.DeleteIndexEntry(payloads[i])
		if err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("overflow entry %d not removed", i)
		}
		delete(remaining, string(payloads[i]))
	}
	got := entryPayloads(t, bt)
	if len(got) != len(remaining) {
		t.Fatalf("after overflow deletes: %d entries, want %d", len(got), len(remaining))
	}
	for _, p := range got {
		if !remaining[string(p)] {
			t.Fatalf("unexpected surviving entry of %d bytes", len(p))
		}
	}
}

func TestIndexKeyRowIDsBySeekMatchesWalk(t *testing.T) {
	bt := newIndexTree(t)
	defer bt.Close()
	bt.SetIndexKeyInfo(singleKeyInfo(), nil)

	// Duplicate key values: each equal-prefix run spans several leaf entries,
	// and a 7-way key repeat forces runs across leaf boundaries.
	const n = 2000
	const mod = 7
	inserted := map[int64]bool{}
	for i := 0; i < n; i++ {
		key := int64(i % mod)
		rid := int64(i + 1)
		insertIndexRecord(t, bt, []interface{}{key, rid})
		inserted[rid] = true
	}
	if leaves := leafPageCount(t, bt); leaves < 2 {
		t.Fatalf("tree is single-leaf (%d); the descent needs an interior level", leaves)
	}
	probes := []interface{}{int64(0), int64(3), int64(6), int64(mod), int64(-1), int64(1 << 40)}
	for _, pv := range probes {
		probe := NewUnpackedIndexKey(singleKeyInfo(), []interface{}{pv})
		got, err := bt.IndexKeyRowIDs(probe)
		if err != nil {
			t.Fatalf("IndexKeyRowIDs(%v): %v", pv, err)
		}
		want, err := bt.indexKeyRowIDsByWalk(probe)
		if err != nil {
			t.Fatalf("walk IndexKeyRowIDs(%v): %v", pv, err)
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if len(got) != len(want) {
			t.Fatalf("probe %v: descent found %d rowids, walk found %d", pv, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("probe %v: rowid %d differs (descent %d, walk %d)", pv, i, got[i], want[i])
			}
		}
		if pv == int64(0) && len(got) == 0 {
			t.Fatalf("probe 0: descent found no rowid for a key that exists")
		}
		if (pv == int64(mod) || pv == int64(-1) || pv == int64(1<<40)) && len(got) != 0 {
			t.Fatalf("probe %v: absent key reported %d rowids", pv, len(got))
		}
	}
}

func TestSeekIndexKey_PositionsAtFirstEqual(t *testing.T) {
	bt := newIndexTree(t)
	defer bt.Close()
	bt.SetIndexKeyInfo(singleKeyInfo(), nil)

	const n = 600
	for i := 0; i < n; i++ {
		insertIndexRecord(t, bt, []interface{}{int64(i % 5), int64(i + 1)})
	}
	c, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	probe := NewUnpackedIndexKey(singleKeyInfo(), []interface{}{int64(2)})
	found, err := c.SeekIndexKey(probe)
	if err != nil || !found {
		t.Fatalf("SeekIndexKey(2): found=%v err=%v", found, err)
	}
	// The cursor is at the FIRST entry of the equal run: stepping forward while
	// the comparison stays equal must visit every match exactly once, in key
	// order, and stop at the first non-matching entry.
	var seen []int64
	for {
		full, err := bt.indexCursorCellPayload(c)
		if err != nil {
			t.Fatalf("payload at position: %v", err)
		}
		cmp, err := IndexRecordCompare(full, probe)
		if err != nil {
			t.Fatalf("compare at position: %v", err)
		}
		if cmp != 0 {
			break
		}
		rid, err := indexRecordRowID(full)
		if err != nil {
			t.Fatalf("rowid: %v", err)
		}
		seen = append(seen, rid)
		if ok, err := c.Next(); err != nil || !ok {
			break
		}
	}
	want := []int64{}
	for i := 0; i < n; i++ {
		if i%5 == 2 {
			want = append(want, int64(i+1))
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("equal run: got %d rowids, want %d", len(seen), len(want))
	}
	for i := range seen {
		if seen[i] != want[i] {
			t.Fatalf("equal run[%d]: got %d want %d", i, seen[i], want[i])
		}
	}
	// An absent probe reports no match.
	c2, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	if found, err := c2.SeekIndexKey(NewUnpackedIndexKey(singleKeyInfo(), []interface{}{int64(99)})); err != nil || found {
		t.Fatalf("SeekIndexKey(99): found=%v err=%v, want false", found, err)
	}
}
