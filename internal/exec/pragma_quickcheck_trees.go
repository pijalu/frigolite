// Corruption tree-check helpers for PRAGMA integrity_check. Split out
// from pragma_quickcheck.go to keep the main file under the per-file
// line cap. Mirrors btree.c::checkTree / checkTreePage.
package exec

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/storage"
)

// checkTreePage runs the per-page b-tree integrity check that mirrors
// btree.c::checkTree. For each b-tree (table or index) it walks every
// reachable page, tracks which pages are referenced, and emits one
// diagnostic per finding:
//
//	"Tree <rootPgno> page <pgno> cell <iCell>: 2nd reference to page <child>"
//	"Page <pgno>: never used"
//
// The duplicate-reference detection is ACROSS all b-trees: the first
// reference of each page is recorded globally (in `referenced`), and
// the second reference (in any tree) is reported naming the FIRST
// reference location. SQLite's checkTree / checkTreePage uses the same
// first-reference semantics: the diagnostic names the original
// pointer, not the duplicate (corrupt2-5.1 reports "Tree 2 page 2
// cell 0" — the t1 reference, not the corrupted t2 reference).
func (e *Engine) checkTreePage(emit func(string)) {
	for _, ctx := range e.dbList {
		if ctx == nil || ctx.Pager == nil {
			continue
		}
		findings, orphans := e.collectTreeFindings(ctx)
		if len(findings) == 0 && len(orphans) == 0 {
			continue
		}
		emit("*** in database main ***")
		emitAll(emit, findings)
		emitAll(emit, orphans)
	}
}

// collectTreeFindings runs the full tree-walk + orphan detection for one
// database context, returning the consolidated findings (cross-tree /
// within-tree duplicates) and the per-page orphan diagnostics.
func (e *Engine) collectTreeFindings(ctx *DatabaseContext) (findings, orphans []string) {
	referenced := make(map[uint32]firstRef)
	for _, te := range e.allRoots(ctx) {
		if te == nil || te.RootPage <= 1 {
			continue
		}
		findings = append(findings, e.checkOneTree(te.RootPage, ctx.Pager, referenced)...)
	}
	return findings, e.findOrphans(ctx, referenced)
}

// emitAll fans a list of strings through the emit callback.
func emitAll(emit func(string), lines []string) {
	for _, s := range lines {
		emit(s)
	}
}

// allRoots returns the combined list of table + index b-tree roots for a
// database context, used by checkTreePage to walk every b-tree once.
func (e *Engine) allRoots(ctx *DatabaseContext) []*schema.Entry {
	entries, _ := ctx.Schema.GetEntries(schema.TypeTable)
	idxEntries, _ := ctx.Schema.GetEntries(schema.TypeIndex)
	return append(append([]*schema.Entry{}, entries...), idxEntries...)
}

// findOrphans returns the "Page N: never used" diagnostics for every
// page in 2..nPages that is neither referenced by any b-tree nor owned
// by the freelist. The caller decides whether to emit the per-database
// banner based on whether this slice is empty.
func (e *Engine) findOrphans(ctx *DatabaseContext, referenced map[uint32]firstRef) []string {
	nPages := ctx.Pager.FilePageCount()
	if nPages == 0 {
		nPages = ctx.Pager.HeaderPageCount()
	}
	pageSize := ctx.Pager.PageSize()
	autoVacuum := ctx.Pager.AutoVacuum()
	var out []string
	for p := uint32(2); p <= nPages; p++ {
		if _, ok := referenced[p]; ok {
			continue
		}
		if isFreelistPage(ctx.Pager, p) {
			continue
		}
		// Auto-vacuum databases reserve pointer-map pages at fixed
		// intervals (page 2 and every usableSize/5+1 pages thereafter,
		// pending-byte skip); they are owned by the pointer-map machinery,
		// not by any b-tree, so they are not orphans. Without this skip a
		// pristine auto-vacuum database (one table, no deletes) reports
		// "Page 2: never used" for its reserved ptrmap page (autovacuum2-1.5,
		// incrvacuum3-1.x integrity_check).
		if autoVacuum && pager.IsPtrmapPageNo(p, pageSize) {
			continue
		}
		out = append(out, fmt.Sprintf("Page %d: never used", p))
	}
	return out
}

// checkOneTree walks a single b-tree depth-first, populating the
// cross-tree `referenced` map with the first reference site of each
// child page. Two duplicate-detection paths:
//
//   - Within-tree duplicate (cell points to a page already in the
//     per-tree `seen` set): reported with the CURRENT tree's
//     (rootPgno, pgno, cell) location, matching SQLite's
//     checkTreePage behaviour for self-references.
//   - Cross-tree duplicate (cell points to a page already referenced
//     in `referenced` by an earlier b-tree): reported with the
//     FIRST-reference location (the earlier tree's pointer), matching
//     SQLite's corrupt2-5.1 output format ("Tree 2 page 2 cell 0" names
//     the t1 reference, not the t2 reference that was overwritten).
func (e *Engine) checkOneTree(rootPgno uint32, pg *pager.Pager, referenced map[uint32]firstRef) []string {
	seen := map[uint32]bool{rootPgno: true}
	referenced[rootPgno] = firstRef{tree: rootPgno, page: rootPgno, cell: -1}
	pageSize := int(pg.PageSize())
	walked := e.walkChildren(rootPgno, pg, referenced, seen, pageSize)
	e.markLeafOverflow(walked, pg, referenced, rootPgno, pageSize)
	return walked.finds
}

// treeWalk is the accumulated state of a single tree-walk: the set of
// pages visited (for the second-pass overflow walk) and any duplicate-
// reference diagnostics found.
type treeWalk struct {
	seen  map[uint32]bool
	finds []string
}

// walkChildren performs the depth-first interior-page walk for
// checkOneTree, returning the set of pages visited and any duplicate
// references discovered.
func (e *Engine) walkChildren(rootPgno uint32, pg *pager.Pager, referenced map[uint32]firstRef, seen map[uint32]bool, pageSize int) treeWalk {
	var finds []string
	pages := []uint32{rootPgno}
	for len(pages) > 0 {
		pgno := pages[0]
		pages = pages[1:]
		next, more := e.processInteriorPage(rootPgno, pgno, pg, referenced, seen, pageSize)
		finds = append(finds, next...)
		pages = append(pages, more...)
	}
	return treeWalk{seen: seen, finds: finds}
}

// processInteriorPage handles one interior page: walks each cell's left
// pointer plus the rightmost pointer, returning (findings, new pages
// to descend into).
func (e *Engine) processInteriorPage(rootPgno, pgno uint32, pg *pager.Pager, referenced map[uint32]firstRef, seen map[uint32]bool, pageSize int) ([]string, []uint32) {
	page, ok := e.readInteriorPage(pg, pgno, pageSize)
	if !ok {
		return nil, nil
	}
	var finds []string
	var next []uint32
	for i, ci := range page.cells {
		if ci.leftPtr == 0 {
			continue
		}
		if f, desc := e.recordReference(rootPgno, pgno, i, ci.leftPtr, referenced, seen); desc {
			finds = append(finds, f)
		} else {
			next = append(next, ci.leftPtr)
		}
	}
	if page.rightmost == 0 {
		return finds, next
	}
	cellIdx := len(page.cells)
	if f, desc := e.recordReference(rootPgno, pgno, cellIdx, page.rightmost, referenced, seen); desc {
		finds = append(finds, f)
	} else {
		next = append(next, page.rightmost)
	}
	return finds, next
}

// recordReference handles the duplicate-detection + first-reference
// recording for one child pointer. Returns (finding, duplicate):
// when duplicate is true the caller must NOT descend into the child
// (it is already known); when false the child is new and the caller
// should add it to the worklist.
func (e *Engine) recordReference(rootPgno, pgno uint32, cellIdx int, child uint32, referenced map[uint32]firstRef, seen map[uint32]bool) (string, bool) {
	if first, dup := referenced[child]; dup && first.tree != rootPgno {
		return fmt.Sprintf("Tree %d page %d cell %d: 2nd reference to page %d",
			first.tree, first.page, first.cell, child), true
	}
	if seen[child] {
		return fmt.Sprintf("Tree %d page %d cell %d: 2nd reference to page %d",
			rootPgno, pgno, cellIdx, child), true
	}
	seen[child] = true
	referenced[child] = firstRef{tree: rootPgno, page: pgno, cell: cellIdx}
	return "", false
}

// interiorCell holds the child-page pointer extracted from one interior-
// table/index cell pointer slot.
type interiorCell struct {
	leftPtr uint32
}

// interiorPageResult bundles one interior page's child pointers + the
// rightmost child pointer, read in a single page fetch.
type interiorPageResult struct {
	cells     []interiorCell
	rightmost uint32
}

// readInteriorPage reads the cell-pointer array AND the rightmost-child
// pointer of an interior page in a single page fetch. The boolean is
// false when the page cannot be read, has the wrong type, or is too
// small to contain a valid header.
func (e *Engine) readInteriorPage(pg *pager.Pager, pgno uint32, pageSize int) (interiorPageResult, bool) {
	page, err := pg.ReadPage(pgno)
	if err != nil {
		return interiorPageResult{}, false
	}
	coff := 0
	if pgno == 1 {
		coff = 100
	}
	if coff+8 > len(page.Data) {
		return interiorPageResult{}, false
	}
	ptype := page.Data[coff]
	cellType := storage.CellTableInterior
	switch ptype {
	case storage.PageTypeInteriorTable:
		// already set
	case storage.PageTypeInteriorIndex:
		cellType = storage.CellIndexInterior
	default:
		return interiorPageResult{}, false
	}
	bp, err := storage.ParsePage(page.Data, pageSize, coff)
	if err != nil {
		return interiorPageResult{}, false
	}
	cells := make([]interiorCell, 0, int(bp.CellCount))
	for i := 0; i < int(bp.CellCount); i++ {
		ptrOff := coff + 12 + i*2
		if ptrOff+2 > len(page.Data) {
			break
		}
		off := int(binary.BigEndian.Uint16(page.Data[ptrOff : ptrOff+2]))
		cell, derr := storage.DecodeCell(page.Data, off, cellType, pageSize)
		if derr != nil || cell == nil {
			cells = append(cells, interiorCell{leftPtr: 0})
			continue
		}
		cells = append(cells, interiorCell{leftPtr: cell.LeftPtr})
	}
	return interiorPageResult{cells: cells, rightmost: bp.RightmostPtr}, true
}

// markLeafOverflow walks every page in `walked.seen` and marks each
// leaf-cell overflow chain into `referenced`. The first walk only
// followed interior child pointers; leaf payloads may also spill to
// overflow pages. Mirrors btree.c::checkTreePage / btreeIntegrityCheckpoint.
func (e *Engine) markLeafOverflow(walked treeWalk, pg *pager.Pager, referenced map[uint32]firstRef, rootPgno uint32, pageSize int) {
	for pgno := range walked.seen {
		overflows, ok := e.leafOverflows(pg, pgno, pageSize)
		if !ok {
			continue
		}
		for _, o := range overflows {
			if o != 0 {
				markOverflowChain(o, pg, referenced, rootPgno, pgno)
			}
		}
	}
}

// leafOverflows reads the cell-pointer array of a leaf page and returns
// the overflow page number for each cell (0 if the cell has no overflow).
// A nil/zero-length result with ok=false means the page is not a leaf.
func (e *Engine) leafOverflows(pg *pager.Pager, pgno uint32, pageSize int) ([]uint32, bool) {
	page, err := pg.ReadPage(pgno)
	if err != nil {
		return nil, false
	}
	coff := 0
	if pgno == 1 {
		coff = 100
	}
	if coff+8 > len(page.Data) {
		return nil, false
	}
	ptype := page.Data[coff]
	var cellType storage.CellType
	switch ptype {
	case storage.PageTypeLeafTable, storage.PageTypeInteriorTable:
		cellType = storage.CellTableLeaf
	case storage.PageTypeLeafIndex, storage.PageTypeInteriorIndex:
		cellType = storage.CellIndexLeaf
	default:
		return nil, false
	}
	bp, err := storage.ParsePage(page.Data, pageSize, coff)
	if err != nil {
		return nil, false
	}
	out := make([]uint32, 0, int(bp.CellCount))
	for i := 0; i < int(bp.CellCount); i++ {
		ptrOff := coff + 8 + i*2
		if ptrOff+2 > len(page.Data) {
			break
		}
		off := int(binary.BigEndian.Uint16(page.Data[ptrOff : ptrOff+2]))
		cell, derr := storage.DecodeCell(page.Data, off, cellType, pageSize)
		if derr != nil || cell == nil {
			out = append(out, 0)
			continue
		}
		out = append(out, cell.Overflow)
	}
	return out, true
}

// markOverflowChain follows an overflow chain starting at `head`,
// marking each page in `out` as referenced. Each overflow page stores
// the next-overflow page number in its first 4 bytes (big-endian), or
// 0 if it is the last. Cycles are guarded by `out` membership; a cycle
// returns silently.
func markOverflowChain(head uint32, pg *pager.Pager, out map[uint32]firstRef, rootPgno, srcPage uint32) {
	const maxIter = 100000
	for cur := head; cur != 0; {
		if _, ok := out[cur]; ok {
			return
		}
		out[cur] = firstRef{tree: rootPgno, page: srcPage, cell: -1}
		page, err := pg.ReadPage(cur)
		if err != nil {
			return
		}
		if len(page.Data) < 4 {
			return
		}
		next := binary.BigEndian.Uint32(page.Data[:4])
		if next == cur {
			return
		}
		cur = next
		_ = maxIter
	}
}

// isFreelistPage reports whether pgno is part of the on-disk freelist
// chain (header-declared trunk + leaf pages). Mirrors
// btree.c::checkTree freelist walk: pages reachable from hdr[32..36]
// are owned by the freelist, not the b-trees, and are not orphans.
func isFreelistPage(pg *pager.Pager, pgno uint32) bool {
	hdr := pg.Header()
	if len(hdr) < 40 {
		return false
	}
	trunk := binary.BigEndian.Uint32(hdr[32:36])
	if trunk == 0 {
		return false
	}
	seen := map[uint32]bool{}
	const maxIter = 100000
	for iter := 0; trunk != 0 && iter < maxIter; iter++ {
		if seen[trunk] {
			return false
		}
		seen[trunk] = true
		if trunk == pgno {
			return true
		}
		page, err := pg.ReadPage(trunk)
		if err != nil {
			return false
		}
		coff := 0
		if trunk == 1 {
			coff = 100
		}
		data := page.Data
		if coff+4 > len(data) {
			return false
		}
		nextTrunk := binary.BigEndian.Uint32(data[coff : coff+4])
		pageSize := int(pg.PageSize())
		for off := coff + 4; off+4 <= len(data) && off+4 <= pageSize; off += 4 {
			leaf := binary.BigEndian.Uint32(data[off : off+4])
			if leaf == 0 {
				break
			}
			if seen[leaf] {
				return false
			}
			seen[leaf] = true
			if leaf == pgno {
				return true
			}
		}
		trunk = nextTrunk
	}
	return false
}
