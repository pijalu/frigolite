//go:build scratch

package btree

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// reachable walks the tree from the root using production parsing rules
// (interior pages have a 12-byte header) and returns every rowid.
func reachable(pg *pager.Pager, root uint32) map[int64]bool {
	out := map[int64]bool{}
	var walk func(p uint32)
	walk = func(p uint32) {
		if p == 0 || p > pg.NumPages() {
			fmt.Printf("WALK: bad page ref %d\n", p)
			return
		}
		pgd, _ := pg.ReadPage(p)
		coff := contentOffset(p)
		page, err := storage.ParsePage(pgd.Data, int(pager.DefaultPageSize), coff)
		if err != nil {
			fmt.Printf("WALK: page %d parse error %v\n", p, err)
			return
		}
		switch page.PageType {
		case storage.PageTypeLeafTable:
			for i := uint16(0); i < page.CellCount; i++ {
				off := int(storage.CellPointer(pgd.Data, coff, int(i), int(pager.DefaultPageSize)))
				c, err := storage.DecodeCell(pgd.Data, off, storage.CellTableLeaf, int(pager.DefaultPageSize))
				if err != nil {
					fmt.Printf("WALK: page %d cell %d decode error %v\n", p, i, err)
					continue
				}
				out[c.RowID] = true
			}
		case storage.PageTypeInteriorTable:
			ptrBase := coff + 12
			for i := uint16(0); i < page.CellCount; i++ {
				off := int(storage.CellPointer(pgd.Data, coff+4, int(i), int(pager.DefaultPageSize)))
				left := uint32(pgd.Data[off])<<24 | uint32(pgd.Data[off+1])<<16 | uint32(pgd.Data[off+2])<<8 | uint32(pgd.Data[off+3])
				walk(left)
			}
			walk(page.RightmostPtr)
			_ = ptrBase
		}
	}
	walk(root)
	return out
}

// physical collects rowids from EVERY page that parses as a table leaf.
func physical(pg *pager.Pager) map[int64]bool {
	out := map[int64]bool{}
	for p := uint32(1); p <= pg.NumPages(); p++ {
		pgd, _ := pg.ReadPage(p)
		page, err := storage.ParsePage(pgd.Data, int(pager.DefaultPageSize), contentOffset(p))
		if err != nil || page.PageType != storage.PageTypeLeafTable {
			continue
		}
		for i := uint16(0); i < page.CellCount; i++ {
			off := int(storage.CellPointer(pgd.Data, coffFor(p), int(i), int(pager.DefaultPageSize)))
			c, err := storage.DecodeCell(pgd.Data, off, storage.CellTableLeaf, int(pager.DefaultPageSize))
			if err != nil {
				continue
			}
			out[c.RowID] = true
		}
	}
	return out
}

func coffFor(p uint32) int {
	if p == 1 {
		return 100
	}
	return 0
}

func TestScratchOverflowStress(t *testing.T) {
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	bt := NewBTree(pg, 1, true)

	live := map[int64]bool{}
	next := int64(1)

	check := func(when string) {
		reach := reachable(pg, bt.RootPage())
		phys := physical(pg)
		var lostInReach []int64
		var phantomReach []int64
		for id := range live {
			if !reach[id] {
				lostInReach = append(lostInReach, id)
			}
		}
		for id := range reach {
			if !live[id] {
				phantomReach = append(phantomReach, id)
			}
		}
		if len(lostInReach) > 0 || len(phantomReach) > 0 {
			orphans := []int64{}
			for id := range phys {
				if !reach[id] && live[id] {
					orphans = append(orphans, id)
				}
			}
			fmt.Printf("DUMP for %s\n", when)
			var walk func(p uint32, depth int)
			walk = func(p uint32, depth int) {
				pgd, _ := pg.ReadPage(p)
				page, err := storage.ParsePage(pgd.Data, int(pager.DefaultPageSize), coffFor(p))
				if err != nil {
					return
				}
				fmt.Printf("%*spage %d type=%d cells=%d right=%d\n", depth*2, "", p, page.PageType, page.CellCount, page.RightmostPtr)
				if p == 781 {
					for i := uint16(0); i < page.CellCount; i++ {
						o := int(storage.CellPointer(pgd.Data, coffFor(p)+4, int(i), int(pager.DefaultPageSize)))
						l := uint32(pgd.Data[o])<<24 | uint32(pgd.Data[o+1])<<16 | uint32(pgd.Data[o+2])<<8 | uint32(pgd.Data[o+3])
						k, _ := storage.DecodeCell(pgd.Data, o, storage.CellTableInterior, int(pager.DefaultPageSize))
						fmt.Printf("%*s ROOT slot%d off=%d left=%d key=%d\n", depth*2+2, "", i, o, l, k.RowID)
					}
				}
				if page.PageType == storage.PageTypeInteriorTable {
					for i := uint16(0); i < page.CellCount; i++ {
						off := int(storage.CellPointer(pgd.Data, coffFor(p)+4, int(i), int(pager.DefaultPageSize)))
						left := uint32(pgd.Data[off])<<24 | uint32(pgd.Data[off+1])<<16 | uint32(pgd.Data[off+2])<<8 | uint32(pgd.Data[off+3])
						key, _ := storage.DecodeCell(pgd.Data, off, storage.CellTableInterior, int(pager.DefaultPageSize))
						fmt.Printf("%*s child=%d sep<%d\n", depth*2+2, "", left, key.RowID)
						walk(left, depth+1)
					}
					walk(page.RightmostPtr, depth+1)
				} else if false {
				} else if page.PageType == storage.PageTypeLeafTable {
					var lo, hi int64 = -1, -1
					for i := uint16(0); i < page.CellCount; i++ {
						off := int(storage.CellPointer(pgd.Data, coffFor(p), int(i), int(pager.DefaultPageSize)))
						c, _ := storage.DecodeCell(pgd.Data, off, storage.CellTableLeaf, int(pager.DefaultPageSize))
						if lo < 0 || c.RowID < lo {
							lo = c.RowID
						}
						if c.RowID > hi {
							hi = c.RowID
						}
					}
					fmt.Printf("%*s rows [%d..%d]\n", depth*2+2, "", lo, hi)
				}
			}
			walk(bt.RootPage(), 0)
			t.Fatalf("%s: unreachable=%v phantom=%d orphanedButPresent=%v",
				when, lostInReach, len(phantomReach), orphans)
		}
	}

	for round := 0; round < 40; round++ {
		for i := 0; i < 35; i++ {
			payload := strings.Repeat("x", 50+int(next*7)%900)
			rec, _ := storage.EncodeRecord([]interface{}{next, payload})
			cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: next, Payload: rec}
			if err := bt.InsertCell(cell); err != nil {
				t.Fatalf("InsertCell %d: %v", next, err)
			}
			live[next] = true
			fmt.Printf("OP-INS %d\n", next)
			next++
			check(fmt.Sprintf("round %d after insert %d", round, next-1))
		}
		lo, hi := next-30, next-10
		if _, err := bt.DeleteCellsWhere(func(c *storage.Cell) bool { return c.RowID >= lo && c.RowID <= hi }); err != nil {
			t.Fatalf("delete: %v", err)
		}
		for id := lo; id <= hi; id++ {
			delete(live, id)
		}
		check(fmt.Sprintf("round %d after delete [%d,%d]", round, lo, hi))
	}
	fmt.Println("btree stress OK")
}
