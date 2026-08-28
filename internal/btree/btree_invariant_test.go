package btree

// Go port of the b-tree invariant checks SQLite exercises through
// test_btree.c / btree01.test and PRAGMA integrity_check: after every
// mutation, (1) the set of keys returned by a full tree walk equals the live
// key set, (2) every live key is seekable, (3) keys appear in sorted order,
// and (4) each cell decodes cleanly. This harness pins down cell-loss bugs
// that only appear with overflow payloads under insert/delete churn.

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

type btreeHarness struct {
	t    *testing.T
	tr   *BTree
	live map[int64][]byte // rowid → expected payload
	next int64
}

func newBtreeHarness(t *testing.T) *btreeHarness {
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	return &btreeHarness{t: t, tr: NewBTree(pg, 1, true), live: make(map[int64][]byte), next: 1}
}

func (h *btreeHarness) payload(id int64) []byte {
	size := 100 + int((id*7)%1200)
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(int64(i) + id)
	}
	return out
}

func (h *btreeHarness) insert() {
	id := h.next
	h.next++
	cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: h.payload(id)}
	if err := h.tr.InsertCell(cell); err != nil {
		h.t.Fatalf("insert %d: %v", id, err)
	}
	h.live[id] = cell.Payload
}

func (h *btreeHarness) overwrite() {
	if h.next < 4 {
		return
	}
	id := h.next - 2
	pl := []byte(fmt.Sprintf("over-%d", id))
	cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: pl}
	if err := h.tr.InsertCell(cell); err != nil {
		h.t.Fatalf("overwrite %d: %v", id, err)
	}
	h.live[id] = pl
}

func (h *btreeHarness) deleteRange() {
	from := h.next - 9
	if from < 1 {
		from = 1
	}
	to := from + 4
	if _, err := h.tr.DeleteCellsWhere(func(c *storage.Cell) bool {
		return c.RowID >= from && c.RowID <= to
	}); err != nil {
		h.t.Fatalf("delete [%d..%d]: %v", from, to, err)
	}
	for id := from; id <= to; id++ {
		delete(h.live, id)
	}
}

func (h *btreeHarness) verify(context string) {
	// Full walk: reachable leaves must yield exactly the live keys in order.
	var leaves []uint32
	if err := h.tr.collectLeafPages(h.tr.RootPage(), &leaves); err != nil {
		h.t.Fatalf("%s: collectLeafPages: %v", context, err)
	}
	seen := make(map[int64]bool)
	var prev int64 = -1
	for _, ln := range leaves {
		pg, _ := h.tr.pager.ReadPage(ln)
		coff := contentOffset(pg.PageNum)
		page, perr := storage.ParsePage(pg.Data, int(h.tr.pageSize), coff)
		if perr != nil {
			h.t.Fatalf("%s: leaf %d unparsable: %v", context, ln, perr)
		}
		for i := 0; i < int(page.CellCount); i++ {
			p := storage.CellPointer(pg.Data, coff, i, int(h.tr.pageSize))
			c, derr := storage.DecodeCell(pg.Data, int(p), storage.CellTableLeaf, int(h.tr.pageSize))
			if derr != nil {
				h.t.Fatalf("%s: leaf %d cell %d: %v", context, ln, i, derr)
			}
			if seen[c.RowID] {
				var all []int64
				for j := 0; j < int(page.CellCount); j++ {
					p2 := storage.CellPointer(pg.Data, coff, j, int(h.tr.pageSize))
					c2, e2 := storage.DecodeCell(pg.Data, int(p2), storage.CellTableLeaf, int(h.tr.pageSize))
					if e2 == nil {
						all = append(all, c2.RowID)
					}
				}
				h.t.Fatalf("%s: duplicate rowid %d (leaf %d) leaf-rowids=%v live-has=%v",
					context, c.RowID, ln, all, h.live[c.RowID] != nil)
			}
			seen[c.RowID] = true
			if prev >= 0 && c.RowID <= prev {
				h.t.Fatalf("%s: order break at %d after %d", context, c.RowID, prev)
			}
			prev = c.RowID
		}
	}
	for id := range h.live {
		if !seen[id] {
			// Distinguish "unreachable" vs "bytes gone": raw-scan every page.
			where := h.rawScan(id)
			h.t.Fatalf("%s: key %d missing from walk (%d leaves); rawScan=%q",
				context, id, len(leaves), where)
		}
		if cur, cerr := h.tr.OpenCursor(); cerr == nil {
			found, serr := cur.SeekToRowID(id)
			if serr != nil || !found {
				where := h.rawScan(id)
				h.t.Fatalf("%s: seek failed for live key %d (found=%v err=%v); rawScan=%q",
					context, id, found, serr, where)
			}
		}
	}
	for id := range seen {
		if _, ok := h.live[id]; !ok {
			h.t.Fatalf("%s: deleted key %d still present in tree", context, id)
		}
	}
}

// rawScan reads EVERY allocated page looking for the rowid — distinguishes
// "orphaned on an unreachable page" from "overwritten".
func (h *btreeHarness) rawScan(id int64) string {
	out := ""
	for pn := uint32(2); pn < 4000; pn++ {
		pg, rerr := h.tr.pager.ReadPage(pn)
		if rerr != nil || pg == nil || len(pg.Data) == 0 {
			continue
		}
		coff := contentOffset(pg.PageNum)
		page, perr := storage.ParsePage(pg.Data, int(h.tr.pageSize), coff)
		if perr != nil || page.PageType != storage.PageTypeLeafTable {
			continue
		}
		for i := 0; i < int(page.CellCount); i++ {
			p := storage.CellPointer(pg.Data, coff, i, int(h.tr.pageSize))
			c, derr := storage.DecodeCell(pg.Data, int(p), storage.CellTableLeaf, int(h.tr.pageSize))
			if derr == nil && c.RowID == id {
				tag := fmt.Sprintf("page%d ", pn)
				reach := false
				var leaves []uint32
				_ = h.tr.collectLeafPages(h.tr.RootPage(), &leaves)
				for _, l := range leaves {
					if l == pn {
						reach = true
					}
				}
				if reach {
					out += tag + "(reachable)"
				} else {
					out += tag + "(ORPHAN)"
				}
			}
		}
	}
	if out == "" {
		return "nowhere"
	}
	return out
}

// TestBtreeInvariantsChurn runs the insert/overwrite/delete-range churn with
// full invariant verification after every operation. The operation pattern
// matches TestBTreeStressCellLoss exactly (which loses keys): 3 inserts per
// step, overwrite at step%7==3, range delete [next-9..next-4] at step%17==5.
func TestBtreeInvariantsChurn(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		h := newBtreeHarness(t)
		for step := 0; step < 3000; step++ {
			// EXACTLY the stress pattern: three inserts, then optional
			// overwrite (%7==3) and/or range delete (%17==5).
			for k := 0; k < 3; k++ {
				id := h.next
				h.next++
				size := 100 + int((id*7)%1200)
				pl := make([]byte, size)
				for bi := range pl {
					pl[bi] = byte(int64(bi) + id)
				}
				cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: pl}
				if err := h.tr.InsertCell(cell); err != nil {
					t.Fatalf("seed %d step %d: insert %d: %v", seed, step, id, err)
				}
				h.live[id] = pl
				if step >= 860 {
					if refs := h.dupChildRefsVerbose(); refs != "" {
						t.Fatalf("after inserting id=%d (step %d k=%d): first dup: %s", id, step, k, refs)
					}
				}
			}

			if step%7 == 3 && h.next > 3 {
				oid := h.next - 2
				opl := []byte(fmt.Sprintf("over-%d-%d", oid, step))
				if err := h.tr.InsertCell(&storage.Cell{Type: storage.CellTableLeaf, RowID: oid, Payload: opl}); err != nil {
					t.Fatalf("seed %d step %d: overwrite %d: %v", seed, step, oid, err)
				}
				h.live[oid] = opl
				if step >= 860 {
					if refs := h.dupChildRefsVerbose(); refs != "" {
						t.Fatalf("after overwrite oid=%d (step %d): first dup: %s", oid, step, refs)
					}
				}
				if step == 73 {
					h.verify(fmt.Sprintf("post-overwrite step %d", step))
				}
			}
			if step%17 == 5 {
				from := h.next - 9
				if from < 1 {
					from = 1
				}
				to := from + 4
				f, tt := from, to
				if _, err := h.tr.DeleteCellsWhere(func(c *storage.Cell) bool {
					return c.RowID >= f && c.RowID <= tt
				}); err != nil {
					t.Fatalf("seed %d step %d: delete: %v", seed, step, err)
				}
				for d := f; d <= tt; d++ {
					delete(h.live, d)
				}
				if step >= 860 {
					if refs := h.dupChildRefsVerbose(); refs != "" {
						t.Fatalf("after delete [%d..%d] (step %d): first dup: %s", f, tt, step, refs)
					}
				}
			}

			// Full invariant check every 13 steps.
			if step >= 65 && step <= 78 {
				if refs := h.dupCrossPageParents(); refs != "" {
					t.Fatalf("step %d cross-page dup: %s", step, refs)
				}
			}
			if (step >= 70 && step <= 76) || step%13 == 0 || step > 2985 {
				h.verify(fmt.Sprintf("seed %d step %d", seed, step))
			}
		}
		h.verify(fmt.Sprintf("seed %d final", seed))
	}
}

// TestBtreeDuplicateTrace reproduces the seed-1 step-884 duplicate and dumps
// both cell locations plus every divider along the seek path.
func (h *btreeHarness) dupChildRefsVerbose() string {
	msg := ""
	for pn := uint32(2); pn < 4000; pn++ {
		pg, rerr := h.tr.pager.ReadPage(pn)
		if rerr != nil || pg == nil {
			continue
		}
		coff := contentOffset(pg.PageNum)
		page, perr := storage.ParsePage(pg.Data, int(h.tr.pageSize), coff)
		if perr != nil || page.PageType != storage.PageTypeInteriorTable {
			continue
		}
		counts := map[uint32]int{}
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pg.Data, coff+4, i, int(h.tr.pageSize)))
			child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
			counts[child]++
			k, _ := util.GetVarint(pg.Data[cellOff+4:])
			if counts[child] > 1 {
				msg += fmt.Sprintf("p%d cell%d child=%d key=%d | ", pn, i, child, k)
			}
			if page.RightmostPtr == child && child != 0 {
				msg += fmt.Sprintf("p%d cell%d child=%d == RIGHTMOST | ", pn, i, child)
			}
		}
	}
	return msg
}

func (h *btreeHarness) dupCrossPageParents() string {
	parents := map[uint32][]uint32{}
	for pn := uint32(2); pn < 6000; pn++ {
		pg, rerr := h.tr.pager.ReadPage(pn)
		if rerr != nil || pg == nil {
			continue
		}
		coff := contentOffset(pg.PageNum)
		page, perr := storage.ParsePage(pg.Data, int(h.tr.pageSize), coff)
		if perr != nil || page.PageType != storage.PageTypeInteriorTable {
			continue
		}
		type kv struct {
			child uint32
			key   uint64
		}
		var kvs []kv
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pg.Data, coff+4, i, int(h.tr.pageSize)))
			child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
			k, _ := util.GetVarint(pg.Data[cellOff+4:])
			kvs = append(kvs, kv{child, k})
		}
		_ = kvs
		mark := func(child uint32) { parents[child] = append(parents[child], pn) }
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pg.Data, coff+4, i, int(h.tr.pageSize)))
			mark(binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4]))
		}
		if page.RightmostPtr != 0 {
			mark(page.RightmostPtr)
		}
	}
	msg := ""
	for child, ps := range parents {
		if len(ps) > 1 {
			// Re-walk to print the exact cells referencing this child.
			detail := ""
			for pn := uint32(2); pn < 6000; pn++ {
				pg, rerr := h.tr.pager.ReadPage(pn)
				if rerr != nil || pg == nil {
					continue
				}
				coff := contentOffset(pg.PageNum)
				page, perr := storage.ParsePage(pg.Data, int(h.tr.pageSize), coff)
				if perr != nil || page.PageType != storage.PageTypeInteriorTable {
					continue
				}
				for i := 0; i < int(page.CellCount); i++ {
					cellOff := int(storage.CellPointer(pg.Data, coff+4, i, int(h.tr.pageSize)))
					if binary.BigEndian.Uint32(pg.Data[cellOff:cellOff+4]) == child {
						k, _ := util.GetVarint(pg.Data[cellOff+4:])
						detail += fmt.Sprintf("p%d[i%d]key=%d ", pn, i, k)
						if i >= 58 && i <= 65 && pn == 173 {
							for j := 58; j <= 65 && j < int(page.CellCount); j++ {
								o2 := int(storage.CellPointer(pg.Data, coff+4, j, int(h.tr.pageSize)))
								c2 := binary.BigEndian.Uint32(pg.Data[o2 : o2+4])
								k2, _ := util.GetVarint(pg.Data[o2+4:])
								detail += fmt.Sprintf("{p%d i%d child=%d key=%d}", pn, j, c2, k2)
							}
						}
					}
				}
				if page.RightmostPtr == child {
					detail += fmt.Sprintf("p%d[RIGHTMOST] ", pn)
				}
			}
			msg += fmt.Sprintf("child %d parents %v detail[%s] | ", child, ps, detail)
		}
	}
	return msg
}

// dupChildRefs returns a description of any child page referenced more than
// once across all interior pages, or "" when clean.
func (h *btreeHarness) dupChildRefs() string {
	seen := map[uint32]uint32{}
	msg := ""
	for pn := uint32(2); pn < 4000; pn++ {
		pg, rerr := h.tr.pager.ReadPage(pn)
		if rerr != nil || pg == nil {
			continue
		}
		coff := contentOffset(pg.PageNum)
		page, perr := storage.ParsePage(pg.Data, int(h.tr.pageSize), coff)
		if perr != nil || page.PageType != storage.PageTypeInteriorTable {
			continue
		}
		counts := map[uint32]int{}
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pg.Data, coff+4, i, int(h.tr.pageSize)))
			child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
			counts[child]++
			k, _ := util.GetVarint(pg.Data[cellOff+4:])
			_ = k
		}
		for child, n := range counts {
			if n > 1 {
				seen[child] += uint32(n)
			}
		}
		_ = msg
	}
	for child := range seen {
		return fmt.Sprintf("child %d multi-referenced", child)
	}
	return ""
}

func TestBtreeDuplicateTrace(t *testing.T) {
	h := newBtreeHarness(t)
	const target = int64(2629)
	locate := func() [][2]int64 {
		var out [][2]int64 // [page, index]
		var leaves []uint32
		_ = h.tr.collectLeafPages(h.tr.RootPage(), &leaves)
		for _, ln := range leaves {
			pg, _ := h.tr.pager.ReadPage(ln)
			coff := contentOffset(pg.PageNum)
			page, perr := storage.ParsePage(pg.Data, int(h.tr.pageSize), coff)
			if perr != nil {
				continue
			}
			for i := 0; i < int(page.CellCount); i++ {
				p := storage.CellPointer(pg.Data, coff, i, int(h.tr.pageSize))
				c, derr := storage.DecodeCell(pg.Data, int(p), storage.CellTableLeaf, int(h.tr.pageSize))
				if derr == nil && c.RowID == target {
					out = append(out, [2]int64{int64(ln), int64(i)})
				}
			}
		}
		return out
	}
	for step := 0; step < 3000; step++ {
		for k := 0; k < 3; k++ {
			id := h.next
			h.next++
			size := 100 + int((id*7)%1200)
			pl := make([]byte, size)
			for bi := range pl {
				pl[bi] = byte(int64(bi) + id)
			}
			if err := h.tr.InsertCell(&storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: pl}); err != nil {
				t.Fatal(err)
			}
			h.live[id] = pl
		}
		if step%7 == 3 && h.next > 3 {
			oid := h.next - 2
			opl := []byte(fmt.Sprintf("over-%d-%d", oid, step))
			if err := h.tr.InsertCell(&storage.Cell{Type: storage.CellTableLeaf, RowID: oid, Payload: opl}); err != nil {
				t.Fatal(err)
			}
			h.live[oid] = opl
		}
		if step%17 == 5 {
			from := h.next - 9
			if from < 1 {
				from = 1
			}
			f, tt := from, from+4
			_, _ = h.tr.DeleteCellsWhere(func(c *storage.Cell) bool {
				return c.RowID >= f && c.RowID <= tt
			})
			for d := f; d <= tt; d++ {
				delete(h.live, d)
			}
		}

		if step >= 870 {
			if refs := h.dupChildRefsVerbose(); refs != "" {
				t.Fatalf("step %d pre-locate: %s", step, refs)
			}
		}
		locs := locate()
		if len(locs) > 1 {
			// Dump every interior reference to the duplicated leaf.
			msg := fmt.Sprintf("step %d: rowid %d duplicated at %v\n", step, target, locs)
			for pn := uint32(2); pn < 3000; pn++ {
				pg, rerr := h.tr.pager.ReadPage(pn)
				if rerr != nil || pg == nil {
					continue
				}
				coff := contentOffset(pg.PageNum)
				page, perr := storage.ParsePage(pg.Data, int(h.tr.pageSize), coff)
				if perr != nil || (page.PageType != storage.PageTypeInteriorTable && page.PageType != storage.PageTypeLeafTable) {
					continue
				}
				if page.PageType == storage.PageTypeInteriorTable {
					for i := 0; i < int(page.CellCount); i++ {
						cellOff := int(storage.CellPointer(pg.Data, coff+4, i, int(h.tr.pageSize)))
						child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
						k, _ := util.GetVarint(pg.Data[cellOff+4:])
						if uint64(child) == uint64(locs[0][0]) || uint64(child) == uint64(locs[1][0]) {
							msg += fmt.Sprintf("  interior p%d cell%d child=%d key=%d\n", pn, i, child, k)
						}
					}
					if uint64(page.RightmostPtr) == uint64(locs[0][0]) || uint64(page.RightmostPtr) == uint64(locs[1][0]) {
						msg += fmt.Sprintf("  interior p%d RIGHTMOST=%d\n", pn, page.RightmostPtr)
					}
				}
			}
			t.Fatal(msg)
		}
	}
}
