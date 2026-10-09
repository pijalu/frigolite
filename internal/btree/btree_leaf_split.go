// Leaf-page cell writing and the multi-page leaf split (splitLeafMulti):
// cells are re-sorted, partitioned by size across the original page plus as
// many newly allocated pages as needed, and the divider keys between the
// resulting siblings are computed.

package btree

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// errLeafFull reports that a leaf page cannot hold a pending cell even
// after freeblock reuse and defragmentation — the caller must split
// (btree.c balance's trigger: insertCell could not place the cell).
var errLeafFull = fmt.Errorf("btree: page is full")

// writeLeafCell inserts a cell at the correct position in a leaf page.
// Returns errLeafFull when the page cannot hold the cell (the caller
// splits). Space comes from allocateSpaceOnPage's btree.c precedence:
// freeblock slot, then (defragmented) content-area gap.
//
// The caller owns duplicate handling and position discovery: every call site
// has already removed any same-rowid cell (sqlite3BtreeInsert's dropCell
// before insertCell, src/btree.c:9458) and passes the insertion index —
// btree.c's append insert runs NO search at all (`idx = ++pCur->ix`,
// src/btree.c:9612, the USESEEKRESULT loc==-1 contract); insertIdx < 0 falls
// back to the leaf-wide binary walk for the non-append callers. With
// insertIdx >= 0 newCell is not consulted (its key already located the
// slot), so callers that only hold the encoded image may pass nil.
func (t *BTree) writeLeafCell(pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell, cellData []byte, coff int, insertIdx int) error {
	if page == nil {
		var err error
		page, err = storage.ParsePage(pg.Data, int(t.pageSize), coff)
		if err != nil {
			return err
		}
	}

	if insertIdx < 0 {
		if t.isTable {
			insertIdx = t.findInsertPositionTable(pg, page, newCell.RowID)
		} else {
			insertIdx = t.findInsertPositionIndex(pg, page, newCell.Payload)
		}
	}

	// Allocate the cell's bytes (freeblock reuse, defragment-on-demand, or
	// the content-area gap — allocateSpace parity).
	cellStart, ok, err := allocateSpaceOnPage(t.pager, pg, page, coff, len(cellData), t.usableSize)
	if err != nil {
		return err
	}
	if !ok {
		return errLeafFull
	}

	// Shift cell pointers
	shiftCellPtrsRight(pg.Data, coff+storage.CellPointerOffset, insertIdx, int(page.CellCount))

	// Write cell data and pointer
	copy(pg.Data[cellStart:], cellData)
	binary.BigEndian.PutUint16(pg.Data[coff+storage.CellPointerOffset+insertIdx*2:coff+storage.CellPointerOffset+insertIdx*2+2], uint16(cellStart))

	// Update header (the cell count only — allocateSpaceOnPage already
	// moved the content-start field to its post-allocation position).
	page.CellCount++
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)

	// Re-arm the pager's parse memo from this path's synced parsed header:
	// the next statement's seek of this leaf is then a memo hit instead of a
	// full re-parse (btree.c's live-MemPage discipline).
	pg.RefreshParsedBTree(int(t.pageSize), coff, page)
	return t.pager.WritePage(pg)
}

// tableLeafRowidAt returns the rowid stored in the table-leaf cell at cell
// pointer index idx.
func (t *BTree) tableLeafRowidAt(pg *pager.Page, coff, idx int) int64 {
	cellOff := int(storage.CellPointer(pg.Data, coff, idx, int(t.pageSize)))
	_, n := util.GetVarint(pg.Data[cellOff:])
	rowID, _ := util.GetVarint(pg.Data[cellOff+n:])
	return int64(rowID)
}

// shiftCellPtrsRight shifts the cell pointer slots [to..from) (index order)
// one slot to the right within the pointer array at ptrBase. Callers insert
// at `to`; every pointer from `to` up to (exclusive) `from` moves one slot up.
func shiftCellPtrsRight(data []byte, ptrBase, to, from int) {
	for i := from; i > to; i-- {
		src := ptrBase + (i-1)*2
		dst := ptrBase + i*2
		data[dst] = data[src]
		data[dst+1] = data[src+1]
	}
}

// splitEntry is a cell plus its encoded byte form and (for index b-trees)
// its full sort key, used during leaf splitting.
type splitEntry struct {
	cell     *storage.Cell
	cellData []byte
	key      []byte // sort key for index b-trees (full payload)
}

// splitStaging is the leaf-split scratch: the decoded cell structs, their
// re-encoded bytes, and the partition assignment. The buffers are pooled
// (splitStagingPool) and truncated per use — a sequential-insert workload
// splits a few hundred times, and fresh arena/slice pairs per split dominated
// the insert-phase allocation profile. Nothing in the staging escapes
// splitLeafMulti: the divider payload handed up to the parent is the split
// entry's separately cloned key bytes (readCellsForSplit), never a view into
// the pooled byte arena.
type splitStaging struct {
	cells []splitEntry   // decoded cells + encoded-byte views (truncated per use)
	arena []storage.Cell // backing for the Cell structs
	bytes []byte         // re-encoded cell bytes (cells' cellData views)
	parts [][]splitEntry // partition assignment
	cur   []splitEntry   // partition-in-progress
}

// splitStagingPool recycles the leaf-split scratch buffers. Buffers only —
// unlike wrappers (btree_pool.go) a staging has no identity or lifecycle:
// it is fully rebuilt from the page bytes on every use, so a Get always
// yields a functionally fresh scratch.
var splitStagingPool = sync.Pool{New: func() interface{} { return new(splitStaging) }}

func acquireSplitStaging() *splitStaging {
	return splitStagingPool.Get().(*splitStaging)
}

// releaseSplitStaging truncates every buffer (keeping capacity) and returns
// the staging to the pool. Callers must ensure no splitEntry view is still
// referenced — splitLeafMulti releases after writeSplitPartitions, whose
// results carry only median rowids and cloned key bytes.
func releaseSplitStaging(st *splitStaging) {
	st.cells = st.cells[:0]
	st.arena = st.arena[:0]
	st.bytes = st.bytes[:0]
	st.parts = st.parts[:0]
	st.cur = st.cur[:0]
	splitStagingPool.Put(st)
}

// splitLeafMulti splits a full leaf page's cells — plus the incoming new cell
// — across the original page and as many newly allocated pages as needed,
// distributing by size so every page fits (SQLite's balance_nonroot
// rebalances across multiple pages when no two-way split can hold the cells).
// Returns the new pages in order, each with the median key separating it from
// the previous page (the first cell's key of that page).
func (t *BTree) splitLeafMulti(pg *pager.Page, page *storage.BTreePage, parentPgno uint32, newCell *storage.Cell, newCellData []byte) ([]leafSplitResult, error) {
	coff := contentOffset(pg.PageNum)
	// Split children hang off the splitting page's parent (btree.c
	// balance_nonroot ptrmapPut PTRMAP_BTREE pParent->pgno, src/btree.c:8023);
	// when the splitting page IS the root, they hang off the root itself
	// (balance_deeper, src/btree.c:9028) — parentPgno==0 marks that case.
	ptrParent := parentPgno
	if ptrParent == 0 {
		ptrParent = pg.PageNum
	}

	cellType := storage.CellTableLeaf
	if !t.isTable {
		cellType = storage.CellIndexLeaf
	}

	st := acquireSplitStaging()
	defer releaseSplitStaging(st)
	cells, err := t.readCellsForSplit(st, pg, page, coff, cellType, newCell, newCellData)
	if err != nil {
		return nil, err
	}
	sortSplitCells(cells, t.isTable, t.compareKey)

	partitions, err := partitionSplitCells(st, cells, coff, int(t.usableSize))
	if err != nil {
		return nil, err
	}

	// Clear original leaf content (except page type)
	// Write-intent barrier: capture the original leaf's statement-journal
	// before-image before the clear wipes its bytes.
	t.pager.PrepareWrite(pg)
	for i := coff + 1; i < int(t.pageSize); i++ {
		pg.Data[i] = 0
	}

	// Write the first partition to the original leaf.
	if err := writeLeafHalf(pg, coff, partitions[0], int(t.usableSize)); err != nil {
		return nil, err
	}
	// Pre-allocate every new page, then write each partition. There is no
	// right-sibling chain pointer: btree pages carry no page-end trailer
	// (cells pack from usableSize) and traversal follows the parent's
	// child pointers.
	return t.writeSplitPartitions(pg, coff, partitions, ptrParent)
}

// partitionSplitCells greedily partitions the sorted cells into pages: each
// page takes the longest prefix that fits. Every page holds at least one cell
// (a single cell whose local payload is oversized goes alone — prepareCell
// caps the local payload so this is defensive).
//
// The fit test runs per cell against a RUNNING byte total (leafCellsFit's
// only variable inputs are Σ len(cellData) and the cell count), so no probe
// copy of the current partition is built. Each partition is a CONTIGUOUS RUN
// of the sorted cells slice, so the partitions are recorded as sub-slices of
// `cells` (backed by the staging's pooled array) instead of copies — the
// naive append-per-partition form allocated a fresh splitEntry array per
// page and dominated the insert-phase allocation profile.
func partitionSplitCells(st *splitStaging, cells []splitEntry, coff, usableSize int) ([][]splitEntry, error) {
	partitions := st.parts[:0]
	start, n, bytes := 0, 0, 0 // current run: cells[start:start+n], Σ len(cellData)
	for i, c := range cells {
		sz := len(c.cellData)
		if n > 0 {
			// leafCellsFit(run+[c]) inlined: contentEnd - total >= ptrEnd.
			if usableSize-bytes-sz < coff+storage.CellPointerOffset+(n+1)*2+2 {
				partitions = append(partitions, cells[start:start+n])
				start, n, bytes = i, 0, 0
			}
		}
		n++
		bytes += sz
	}
	if n > 0 {
		partitions = append(partitions, cells[start:start+n])
	}
	if len(partitions) == 0 {
		return nil, fmt.Errorf("btree: split failed: cannot balance leaf pages")
	}
	return partitions, nil
}

// writeSplitPartitions pre-allocates len(partitions)-1 new leaf pages of the
// same type as the original, persists the rewritten original page (pg), then
// writes each remaining partition to its new page. Returns the new pages in
// order with the median key separating each from its left neighbor.
func (t *BTree) writeSplitPartitions(pg *pager.Page, coff int, partitions [][]splitEntry, ptrParent uint32) ([]leafSplitResult, error) {
	nNew := len(partitions) - 1
	newPages := make([]*pager.Page, 0, nNew)
	for i := 0; i < nNew; i++ {
		np, aerr := t.allocBtreeNode(ptrParent)
		if aerr != nil {
			return nil, aerr
		}
		newPages = append(newPages, np)
	}
	if err := t.pager.WritePage(pg); err != nil {
		return nil, err
	}
	results := make([]leafSplitResult, 0, nNew)
	for pi := 1; pi < len(partitions); pi++ {
		newPg := newPages[pi-1]
		newCoff := contentOffset(newPg.PageNum)
		newPg.Data[newCoff] = pg.Data[coff] // same page type
		if err := writeLeafHalf(newPg, newCoff, partitions[pi], int(t.usableSize)); err != nil {
			return nil, err
		}
		if err := t.reparentSplitOverflowChains(partitions[pi], newPg); err != nil {
			return nil, err
		}
		// No page-end chain pointer (btree.c pages carry no trailer): the
		// cell content area runs to usableSize.
		if err := t.pager.WritePage(newPg); err != nil {
			return nil, err
		}
		key, payload := t.splitMedianKey(partitions, pi)
		results = append(results, leafSplitResult{pageNum: newPg.PageNum, medianKey: key, medianPayload: payload})
	}
	return results, nil
}

// reparentSplitOverflowChains re-parents the overflow chains of the cells
// that moved to this new page: each chain's FIRST page follows its cell's new
// owner (btree.c balance_nonroot ptrmapPutOvflPtr, src/btree.c:8025/8783).
// Later chain pages keep their OVFL2 parent (the previous overflow page).
func (t *BTree) reparentSplitOverflowChains(partition []splitEntry, newPg *pager.Page) error {
	if !t.ptrmapEnabled() {
		return nil
	}
	for _, e := range partition {
		if e.cell.Overflow != 0 {
			if werr := t.pager.WritePtrmap(e.cell.Overflow, storage.PtrmapOverflow1, newPg.PageNum); werr != nil {
				return werr
			}
		}
	}
	return nil
}

// splitMedianKey computes the divider key between partition pi-1 and
// partition pi, together with the index divider payload (empty for tables).
func (t *BTree) splitMedianKey(partitions [][]splitEntry, pi int) (uint64, []byte) {
	if t.isTable {
		// SQLite's leafData separator convention (btree.c:8813): the
		// divider cell between two sibling leaves carries the LAST
		// rowid of the LEFT sibling (left subtree holds keys <= key,
		// right subtree holds keys > key — sqlite3BtreeTableMoveto
		// descends into the separator's left child on key==rowid).
		// Using MIN(right) instead (the engine's pre-fix convention)
		// made the right subtree overlap the boundary row, breaking
		// sqlite3 integrity_check on every table btree split
		// (incrvacuum2 4.1: doubling leaves produced "right child
		// Rowid N out of order" starting at iter 1).
		left := partitions[pi-1]
		return uint64(left[len(left)-1].cell.RowID), nil
	}
	// Index btrees (non-leafData): the divider is the FIRST cell of
	// the RIGHT sibling (btree.c:8820, pCell -= 4 branch) — the left
	// subtree holds keys < medianKey and the right subtree holds
	// keys >= medianKey (sqlite3BtreeIndexMoveto: equal keys go
	// right). The divider cell carries that cell's full record
	// payload (balance_nonroot copies the cell into the interior
	// page), so interior descent and sqlite3 integrity_check see
	// value-ordered separators. (The payload is the split entry's
	// key — already a full-payload clone that survives this page's
	// cell-area rewrite, see readCellsForSplit.)
	return 0, partitions[pi][0].key
}

// leafSplitResult is one new page produced by a split: the page number and
// the divider separating it from the previous page — medianKey (the last
// rowid of the left sibling) for table b-trees, medianPayload (the right
// sibling's first cell's full record payload, balance_nonroot's copied
// separator cell) for index b-trees.
type leafSplitResult struct {
	pageNum       uint32
	medianKey     uint64
	medianPayload []byte
}

// readCellsForSplit decodes the existing cells on a leaf page plus the new
// cell into a unified split-entry list, ready for redistribution. The list is
// built in the caller's pooled staging (see splitStaging): the decoded Cell
// structs and their encoded bytes land in two batched buffers (one backing
// array, one growing arena) instead of two allocations per cell — a splitting
// page holds hundreds of cells, so this is the difference between 2
// allocations per SPLIT and 2 per CELL. The bytes are byte-identical to
// storage.EncodeCell (AppendEncodedCell delegates to the same wire writer).
// Cell payloads remain views into pg.Data exactly as storage.DecodeCell
// returned them.
func (t *BTree) readCellsForSplit(st *splitStaging, pg *pager.Page, page *storage.BTreePage, coff int, cellType storage.CellType, newCell *storage.Cell, newCellData []byte) ([]splitEntry, error) {
	cells := st.cells[:0]
	arena := st.arena[:0]
	bytes := st.bytes[:0]
	for i := uint16(0); i < page.CellCount; i++ {
		cellOff := int(storage.CellPointer(pg.Data, coff, int(i), int(t.pageSize)))
		arena = append(arena, storage.Cell{Type: cellType})
		c := &arena[len(arena)-1]
		if err := storage.DecodeCellInto(pg.Data, cellOff, cellType, int(t.usableSize), c); err != nil {
			st.cells, st.arena, st.bytes = cells, arena, bytes
			return nil, err
		}

		start := len(bytes)
		bytes = storage.AppendEncodedCell(bytes, c)
		e := splitEntry{c, bytes[start:], nil}
		if !t.isTable {
			full, err := t.readOverflow(c)
			if err != nil {
				st.cells, st.arena, st.bytes = cells, arena, bytes
				return nil, err
			}
			// Clone the sort key: it aliases pg.Data, and splitLeafMulti
			// zeroes this page's cell area while redistributing — the
			// divider payload handed to the parent must stay valid.
			e.key = append([]byte(nil), full.Payload...)
		}
		cells = append(cells, e)
	}
	// Include the new cell in the redistribution — REPLACING any existing
	// cell with the same key (SQLite's sqlite3BtreeInsert overwrites in
	// place; the non-split path dedupes in writeLeafCell, and the split
	// path must too, otherwise overwriting a full page's boundary rowid
	// writes the rowid twice — duplicate rowids across/within partitions,
	// fts4opt churn: rowid 229 duplicated on leaf 171).
	// (The new cell's payload is engine-owned, not a page view.)
	for i := 0; i < len(cells); i++ {
		if t.isTable && cells[i].cell.RowID == newCell.RowID {
			cells = append(cells[:i], cells[i+1:]...)
			i--
		}
	}
	cells = append(cells, splitEntry{newCell, newCellData, newCell.Payload})
	st.cells, st.arena, st.bytes = cells, arena, bytes
	return cells, nil
}

// sortSplitCells orders cells by key (rowid for tables, full payload for
// indexes). A leaf page's cells are ALREADY in key order (the b-tree
// invariant the insert walk maintains); the split only interleaves the one
// incoming cell. SQLite's balance_leaf exploits that order and never
// re-sorts (src/btree.c distributes cells in stored order); the historical
// unconditional bubble sort here paid O(n²) comparisons per split —
// ~60k compares for a 350-cell leaf — with ZERO swaps on the common
// sequential-load/append shape (the new cell is the largest key). One
// linear sortedness probe (n compares) now bypasses the sort; a probe miss
// falls back to the stable bubble unchanged.
func sortSplitCells(cells []splitEntry, isTable bool, cmp func(a, b []byte) int) {
	if splitCellsSorted(cells, isTable, cmp) {
		return
	}
	if isTable {
		bubbleSortSplitCells(cells, func(a, b splitEntry) bool {
			return a.cell.RowID > b.cell.RowID
		})
		return
	}
	bubbleSortSplitCells(cells, func(a, b splitEntry) bool {
		return cmp(a.key, b.key) > 0
	})
}

// splitCellsSorted reports whether cells are already in ascending key order
// (rowids for table b-trees, key bytes for index b-trees).
func splitCellsSorted(cells []splitEntry, isTable bool, cmp func(a, b []byte) int) bool {
	for i := 1; i < len(cells); i++ {
		if isTable {
			if cells[i-1].cell.RowID > cells[i].cell.RowID {
				return false
			}
			continue
		}
		if cmp(cells[i-1].key, cells[i].key) > 0 {
			return false
		}
	}
	return true
}

// bubbleSortSplitCells sorts cells in place using a "greater than"
// comparison, keeping entries with equal keys in their original order.
func bubbleSortSplitCells(cells []splitEntry, greater func(a, b splitEntry) bool) {
	for i := 0; i < len(cells); i++ {
		for j := i + 1; j < len(cells); j++ {
			if greater(cells[i], cells[j]) {
				cells[i], cells[j] = cells[j], cells[i]
			}
		}
	}
}

// writeLeafHalf writes a slice of split entries to a leaf page starting at
// the given content offset, appending cells from the end of the usable area
// downward, and updates the page header's cell count and content end.
func writeLeafHalf(pg *pager.Page, coff int, half []splitEntry, usableSize int) error {
	var count uint16
	end := usableSize // cells pack from the usable end (zeroPage convention)
	for i := range half {
		d := half[i].cellData
		start := end - len(d)
		ptrOff := coff + storage.CellPointerOffset + int(count)*2
		if start < ptrOff+2 {
			return fmt.Errorf("btree: split failed: leaf half full")
		}
		copy(pg.Data[start:], d)
		binary.BigEndian.PutUint16(pg.Data[ptrOff:], uint16(start))
		count++
		end = start
	}
	// Full header rewrite (zeroPage parity): the page may be a cached
	// buffer from an earlier incarnation (freed leaf/overflow/trunk page
	// handed out again by the freelist pop), so freeblock and fragmentation
	// must be reset explicitly — stale bytes there read as free-space
	// corruption ("Fragmentation of N bytes reported as M").
	binary.BigEndian.PutUint16(pg.Data[coff+1:coff+3], 0) // first freeblock
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], count)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(end))
	pg.Data[coff+7] = 0 // fragmented free bytes
	return nil
}

// findInsertPositionTable returns the index of the first table-leaf cell with
// rowid >= rowID (binary search over the cell pointer array).
func (t *BTree) findInsertPositionTable(pg *pager.Page, page *storage.BTreePage, rowID int64) int {
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid, int(t.pageSize)))
		_, n := util.GetVarint(pg.Data[cellOff:])
		cellOff += n
		midRowID, _ := util.GetVarint(pg.Data[cellOff:])
		if int64(midRowID) < rowID {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// findInsertPositionIndex returns the index of the first index-leaf cell whose
// full payload is >= key (binary search over the cell pointer array).
func (t *BTree) findInsertPositionIndex(pg *pager.Page, page *storage.BTreePage, key []byte) int {
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cell, err := storage.DecodeCell(pg.Data, int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid, int(t.pageSize))), storage.CellIndexLeaf, int(t.usableSize))
		if err != nil {
			return lo
		}
		full, err := t.readOverflow(cell)
		if err != nil {
			return lo
		}
		if t.compareKey(full.Payload, key) < 0 {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return lo
}
