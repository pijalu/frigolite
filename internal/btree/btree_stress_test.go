package btree

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// TestBTreeStressCellLoss hammers a table b-tree with sequential blob inserts
// interleaved with predicate deletes (the %_segments churn pattern: blockid
// rows written by FTS flushes, deleted in ranges by chomp truncations). Any
// lost cell breaks SeekToRowID for an existing key.
// seekHas reports whether SeekToRowID finds id.
func seekHas(tr *BTree, id int64) bool {
	cur, cerr := tr.OpenCursor()
	if cerr != nil {
		return false
	}
	found, serr := cur.SeekToRowID(id)
	return serr == nil && found
}

// walkHas reports whether rowid id is reachable from the root.
func walkHas(tr *BTree, id int64) (bool, bool) {
	var leaves []uint32
	if err := tr.collectLeafPages(tr.RootPage(), &leaves); err != nil {
		return false, false
	}
	for _, ln := range leaves {
		pg2, _ := tr.pager.ReadPage(ln)
		coff2 := contentOffset(pg2.PageNum)
		page2, perr := storage.ParsePage(pg2.Data, int(tr.pageSize), coff2)
		if perr != nil {
			continue
		}
		for j := 0; j < int(page2.CellCount); j++ {
			p2 := storage.CellPointer(pg2.Data, coff2, j, int(tr.pageSize))
			c2, e2 := storage.DecodeCell(pg2.Data, int(p2), storage.CellTableLeaf, int(tr.pageSize))
			if e2 == nil && c2.RowID == id {
				return true, true
			}
		}
	}
	return false, true
}

func TestBTreeStressCellLoss(t *testing.T) {
	// (skip removed after fix — see lessons_learned.md)
	for seed := int64(1); seed <= 3; seed++ {
		pg := pager.OpenInMemory(1024)
		pg.AllocatePage()
		rootPg, _ := pg.ReadPage(1)
		rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
		setEmptyLeafContent(rootPg)
		pg.WritePage(rootPg)
		tr := NewBTree(pg, 1, true)
		live := make(map[int64]bool)
		next := int64(1)
		for step := 0; step < 3000; step++ {
			for k := 0; k < 3; k++ {
				id := next
				next++
				// Blob sizes straddle the overflow threshold (page 1024).
				size := 100 + int((id*7)%1200)
				payload := make([]byte, size)
				for bi := range payload {
					payload[bi] = byte(id + int64(bi))
				}
				cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: payload}
				if err := tr.InsertCell(cell); err != nil {
					t.Fatalf("seed %d step %d: insert %d: %v", seed, step, id, err)
				}
				live[id] = true
				if step >= 74 && step <= 78 && next > 228 && !seekHas(tr, 228) {
					// Dump the descent path and every divider along the way.
					path := "root"
					pn := tr.RootPage()
					for depth := 0; depth < 8; depth++ {
						pg2, _ := tr.pager.ReadPage(pn)
						coff2 := contentOffset(pg2.PageNum)
						page2, perr := storage.ParsePage(pg2.Data, int(tr.pageSize), coff2)
						if perr != nil {
							break
						}
						if page2.PageType != storage.PageTypeInteriorTable {
							first, last := int64(-1), int64(-1)
							for j := 0; j < int(page2.CellCount); j++ {
								p2 := storage.CellPointer(pg2.Data, coff2, j, int(tr.pageSize))
								c2, e2 := storage.DecodeCell(pg2.Data, int(p2), storage.CellTableLeaf, int(tr.pageSize))
								if e2 == nil {
									if first < 0 {
										first = c2.RowID
									}
									last = c2.RowID
									if c2.RowID == 228 {
										last = -999
									}
								}
							}
							path += fmt.Sprintf(" -> leaf%d[cells=%d first=%d last=%d has228=%v]", pn, page2.CellCount, first, last, last == -999)
							break
						}
						chosen, div := uint32(0), int64(-1)
						lo, hi := 0, int(page2.CellCount)-1
						for lo <= hi {
							mid := (lo + hi) / 2
							cellOff := int(storage.CellPointer(pg2.Data, coff2+4, mid, int(tr.pageSize)))
							mk, _ := util.GetVarint(pg2.Data[cellOff+4:])
							if int64(mk) <= 228 {
								lo = mid + 1
							} else {
								hi = mid - 1
							}
						}
						if lo < int(page2.CellCount) {
							cellOff := int(storage.CellPointer(pg2.Data, coff2+4, lo, int(tr.pageSize)))
							chosen = binary.BigEndian.Uint32(pg2.Data[cellOff : cellOff+4])
							var dv uint64
							dv, _ = util.GetVarint(pg2.Data[cellOff+4:])
							div = int64(dv)
						} else {
							chosen = page2.RightmostPtr
						}
						path += fmt.Sprintf(" -> int%d[cell%d child=%d div=%d]", pn, lo, chosen, div)
						pn = chosen
					}
					t.Fatalf("seed %d step %d k=%d: inserting id=%d broke seek of 228; path=%s", seed, step, k, id, path)
				}
			}
			// Same-rowid overwrite (the merge continuation's reuse path).
			if step%7 == 3 && next > 3 {
				id := next - 2
				cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id,
					Payload: []byte(fmt.Sprintf("over-%d-%d", id, step))}
				if err := tr.InsertCell(cell); err != nil {
					t.Fatalf("seed %d step %d: overwrite %d: %v", seed, step, id, err)
				}
			}
			if step%17 == 5 {
				from := next - 9
				if from < 1 {
					from = 1
				}
				to := from + 4
				if _, derr := tr.DeleteCellsWhere(func(c *storage.Cell) bool {
					return c.RowID >= from && c.RowID <= to
				}); derr != nil {
					t.Fatalf("seed %d step %d: delete range: %v", seed, step, derr)
				}
				for id := from; id <= to; id++ {
					delete(live, id)
				}
			}
			if next > 228 && !seekHas(tr, 228) {
				t.Fatalf("seed %d step %d: key 228 detached from tree", seed, step)
			}
			if step >= 55 && step <= 95 {
				// early-detach tracker for key 228
				if _, ok := walkHas(tr, 228); !ok {
					t.Fatalf("step %d: key 228 detached from tree", step)
				}
			}
			if step%13 == 0 && next > 2 {
				probe := next - 1
				if live[probe] {
					cur, cerr := tr.OpenCursor()
					if cerr != nil {
						t.Fatal(cerr)
					}
					found, serr := cur.SeekToRowID(probe)
					if serr != nil || !found {
						t.Fatalf("seed %d step %d: key %d lost (found=%v err=%v)", seed, step, probe, found, serr)
					}
				}
			}
		}
		missing := 0
		for id := int64(1); id < next; id++ {
			if !live[id] {
				continue
			}
			cur, _ := tr.OpenCursor()
			found, serr := cur.SeekToRowID(id)
			if serr != nil || !found {
				// classify: present in a leaf walk?
				var leaves []uint32
				_ = tr.collectLeafPages(tr.RootPage(), &leaves)
				inWalk, inScan := false, ""
				for _, ln := range leaves {
					pg2, _ := tr.pager.ReadPage(ln)
					coff2 := contentOffset(pg2.PageNum)
					page2, perr := storage.ParsePage(pg2.Data, int(tr.pageSize), coff2)
					if perr != nil {
						continue
					}
					for j := 0; j < int(page2.CellCount); j++ {
						p2 := storage.CellPointer(pg2.Data, coff2, j, int(tr.pageSize))
						c2, e2 := storage.DecodeCell(pg2.Data, int(p2), storage.CellTableLeaf, int(tr.pageSize))
						if e2 == nil && c2.RowID == id {
							inWalk = true
						}
					}
				}
				for pn2 := uint32(2); pn2 < 12000; pn2++ {
					pg3, rerr := tr.pager.ReadPage(pn2)
					if rerr != nil || pg3 == nil {
						break
					}
					coff3 := contentOffset(pg3.PageNum)
					page3, perr := storage.ParsePage(pg3.Data, int(tr.pageSize), coff3)
					if perr != nil || page3.PageType != storage.PageTypeLeafTable {
						continue
					}
					for j := 0; j < int(page3.CellCount); j++ {
						p3 := storage.CellPointer(pg3.Data, coff3, j, int(tr.pageSize))
						c3, e3 := storage.DecodeCell(pg3.Data, int(p3), storage.CellTableLeaf, int(tr.pageSize))
						if e3 == nil && c3.RowID == id {
							inScan += fmt.Sprintf(" page%d", pn2)
						}
					}
				}
				_ = inWalk
				missing++
				if missing <= 3 {
					t.Errorf("seed %d: key %d VERIFY FAIL found=%v serr=%v nleaves=%d inWalk=%v rawScan=[%s]", seed, id, found, serr, len(leaves), inWalk, inScan)
				}
			}
		}
		if missing > 0 {
			t.Errorf("seed %d: %d keys missing total (live=%d)", seed, missing, len(live))
		}
	}
}
