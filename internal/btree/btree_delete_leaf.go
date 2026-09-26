// Per-leaf cell deletion: predicate-based bulk delete, index-entry delete by
// full payload, and the shared page-rewrite/overflow-free completion step.

package btree

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// leafCellType maps a leaf page type to its on-page cell encoding.
func leafCellType(pageType byte) storage.CellType {
	if pageType == storage.PageTypeLeafTable {
		return storage.CellTableLeaf
	}
	return storage.CellIndexLeaf
}

// DeleteIndexEntry removes the first index cell whose FULL payload (local
// bytes reassembled with its overflow chain) equals target. Index entries
// are unique per row (the rowid suffix is part of the record), so at most
// one cell matches. Returns true when an entry was removed. This is the
// delete-side counterpart of InsertCell for CellIndexLeaf entries written
// by execdml (maintainIndexesOnInsert), and keeps index btrees consistent
// after DELETE/REPLACE so stale entries cannot pin overflow pages (which
// stalled auto-vacuum truncation and corrupted integrity_check walks).
func (t *BTree) DeleteIndexEntry(target []byte) (bool, error) {
	n, err := t.DeleteIndexEntries([][]byte{target})
	return n > 0, err
}

// DeleteIndexEntries removes one index cell per target whose FULL payload
// (local bytes reassembled with its overflow chain) equals the target bytes.
// Index entries are unique per row (the rowid suffix is part of the record),
// so at most one cell matches each target. Returns the number of entries
// removed. This is the delete-side counterpart of InsertCell for
// CellIndexLeaf entries written by execdml, and keeps index btrees consistent
// after DELETE/REPLACE so stale entries cannot pin overflow pages. The walk
// visits every leaf once and matches ALL targets per leaf — the indexed-UPDATE
// maintenance path batches its per-row old-key deletions through here, so a
// statement's cost is O(index) instead of O(changes x index) (which thrashed
// the 10-page cache for minutes in temptable2 3.2).
func (t *BTree) DeleteIndexEntries(targets [][]byte) (int, error) {
	t.saveAllCursors() // btree.c saveAllCursors on the dropCell path
	var leaves []uint32
	if err := t.collectLeafPages(t.rootPage, &leaves, nil); err != nil {
		return 0, err
	}
	deleted := 0
	for _, leafNum := range leaves {
		// A leaf freed as a surplus empty sibling during an earlier
		// iteration must be skipped (its type byte is the freelist
		// chain pointer, not a page type).
		if pager.IsPageOnFreelist(t.pager, leafNum) {
			continue
		}
		found, err := t.deleteIndexEntryFromLeafBatch(leafNum, targets)
		if err != nil {
			return deleted, err
		}
		if found > 0 {
			deleted += found
			if os.Getenv("FRIGOLITE_IDX_DEBUG") != "" {
				fmt.Printf("REBAL leaf=%d found=%d\n", leafNum, found)
			}
			if err := t.maybeRebalanceAfterDelete(leafNum); err != nil {
				return deleted, err
			}
		}
	}
	// A fully-emptied index tree can leave its root an interior page with
	// 0 cells and a dead rightmost-child (removeEmptyIndexLeaf zeroed it
	// at the root level); rewrite the root as an empty leaf — the same
	// end state clearEmptyRootRightmost produces for table trees.
	if err := t.clearEmptyRootRightmost(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

// deleteIndexEntryFromLeafBatch removes the cells on one index leaf whose FULL
// payload equals any target. Unlike deleteAllMatchingFromLeaf's predicate
// callback (which deliberately receives LOCAL-only payloads for FTS
// performance), index-entry deletion must compare the complete record: an
// overflowing index cell's local bytes are a prefix of the target and would
// never match without reassembly (readOverflow). Returns the number of cells
// removed.
func (t *BTree) deleteIndexEntryFromLeafBatch(leafNum uint32, targets [][]byte) (int, error) {
	pg, err := t.pager.ReadPage(leafNum)
	if err != nil {
		return 0, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return 0, err
	}
	if page.PageType != storage.PageTypeLeafIndex {
		return 0, nil
	}
	encoded, decoded, err := t.decodeIndexLeafCells(pg, coff, page)
	if err != nil {
		return 0, err
	}
	keep, deletedIdx, err := t.classifyIndexMatches(encoded, decoded, targets)
	if err != nil {
		return 0, err
	}
	if len(deletedIdx) == 0 {
		return 0, nil
	}
	if _, err := t.finishLeafDelete(pg, page, encoded, keep, decoded, deletedIdx, int64(len(deletedIdx))); err != nil {
		return 0, err
	}
	return len(deletedIdx), nil
}

// decodeIndexLeafCells decodes and re-encodes every index cell of a leaf,
// preparing the byte-for-byte copies used by the rewrite pass.
func (t *BTree) decodeIndexLeafCells(pg *pager.Page, coff int, page *storage.BTreePage) ([][]byte, []storage.Cell, error) {
	encoded := make([][]byte, 0, int(page.CellCount))
	decoded := make([]storage.Cell, int(page.CellCount))
	for i := 0; i < int(page.CellCount); i++ {
		p := storage.CellPointer(pg.Data, coff, i, int(t.pageSize))
		c, derr := storage.DecodeCell(pg.Data, int(p), storage.CellIndexLeaf, int(t.usableSize))
		if derr != nil {
			return nil, nil, derr
		}
		decoded[i] = *c
		encoded = append(encoded, storage.EncodeCell(c))
	}
	return encoded, decoded, nil
}

// classifyIndexMatches splits the cells into survivors (keep) and matches
// (deletedIdx). Index entries are unique per row, so each target matches at
// most one cell.
func (t *BTree) classifyIndexMatches(encoded [][]byte, decoded []storage.Cell, targets [][]byte) ([]int, []int, error) {
	var keep []int
	var deletedIdx []int
	for i := 0; i < len(encoded); i++ {
		full, ferr := t.readOverflow(&decoded[i])
		if ferr != nil {
			return nil, nil, ferr
		}
		match := false
		for _, target := range targets {
			if bytes.Equal(full.Payload, target) {
				match = true
				break
			}
		}
		if match {
			deletedIdx = append(deletedIdx, i)
			continue
		}
		keep = append(keep, i)
	}
	return keep, deletedIdx, nil
}

// deleteAllMatchingFromLeaf removes every matching cell on the leaf page at
// leafNum in a single compaction pass (decode all cells once, keep the
// survivors, rebuild the page once). The previous per-cell delete rewrote all
// remaining cells each time — O(k^2) per leaf, which made DELETE FROM
// %_segments (thousands of 4KB blob rows) take ~30-40s (fts4merge4's
// between-scenario DELETE).
func (t *BTree) deleteAllMatchingFromLeaf(leafNum uint32, fn func(cell *storage.Cell) bool) (int64, error) {
	pg, err := t.pager.ReadPage(leafNum)
	if err != nil {
		return 0, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return 0, err
	}
	if page.PageType != storage.PageTypeLeafTable && page.PageType != storage.PageTypeLeafIndex {
		return 0, fmt.Errorf("btree: delete only supported on leaf pages")
	}
	cellType := leafCellType(page.PageType)
	// Decode every cell once.
	encoded, decoded, err := t.decodeAllLeafCells(pg, coff, page, cellType)
	if err != nil {
		return 0, err
	}
	// Keep the survivors, preserving order. Also collect the deleted cell
	// indices so their overflow-page chains can be freed.
	var keep []int
	deleted := int64(0)
	var deletedIdx []int
	for i := 0; i < len(encoded); i++ {
		if t.cellMatches(pg, page, i, fn) {
			deleted++
			deletedIdx = append(deletedIdx, i)
			continue
		}
		keep = append(keep, i)
	}
	if deleted == 0 {
		return 0, nil
	}
	return t.finishLeafDelete(pg, page, encoded, keep, decoded, deletedIdx, deleted)
}

// decodeAllLeafCells decodes and re-encodes every cell of a leaf, preparing
// the byte-for-byte copies used by the rewrite pass. A cell that fails to
// decode (corrupt cell pointer, garbage at the cell's offset, overlapping
// cells) is kept in the page as a raw byte slice. SQLite's btree.c
// clearDatabasePage treats such cells as "drop without decoding" — the bytes
// are preserved so the page stays valid for subsequent reads. We mirror that
// by encoding the raw bytes (re-validated on read).
func (t *BTree) decodeAllLeafCells(pg *pager.Page, coff int, page *storage.BTreePage, cellType storage.CellType) ([][]byte, []storage.Cell, error) {
	encoded := make([][]byte, 0, int(page.CellCount))
	decoded := make([]storage.Cell, int(page.CellCount))
	for i := 0; i < int(page.CellCount); i++ {
		p := storage.CellPointer(pg.Data, coff, i, int(t.pageSize))
		c, derr := storage.DecodeCell(pg.Data, int(p), cellType, int(t.usableSize))
		if derr != nil {
			raw := pg.Data[int(p):]
			// Bound the raw slice so we don't read past the page.
			end := len(raw)
			if end > int(t.usableSize)-int(p) {
				end = int(t.usableSize) - int(p)
			}
			encoded = append(encoded, append([]byte(nil), raw[:end]...))
			decoded[i] = storage.Cell{Type: cellType, RowID: 0, PayloadLen: 0, LocalLen: 0}
			continue
		}
		decoded[i] = *c
		encoded = append(encoded, storage.EncodeCell(c))
	}
	return encoded, decoded, nil
}

// finishLeafDelete completes a leaf-cell deletion: it frees the deleted
// cells' overflow-page chains, rewrites the surviving cells contiguously
// from the end of the usable area, and persists the page. Shared by
// deleteAllMatchingFromLeaf (predicate deletes) and
// deleteIndexEntryFromLeafBatch (full-payload index-entry deletes).
// `encoded` holds each cell's encoded bytes, `keep` the survivor indices,
// `decoded`/`deletedIdx` the decoded cells whose overflow chains must be
// freed, and `deleted` the running deletion count.
func (t *BTree) finishLeafDelete(pg *pager.Page, page *storage.BTreePage, encoded [][]byte, keep []int, decoded []storage.Cell, deletedIdx []int, deleted int64) (int64, error) {
	coff := contentOffset(pg.PageNum)
	// Free the overflow pages of the cells that were deleted. Each
	// leaf cell may carry a chain of overflow pages (4KB blobs
	// need several pages); when the cell is deleted the chain becomes
	// orphaned and must be returned to the freelist so the header count
	// tracks the freed space (corrupt2-14.2/14.3/14.5 depend on this).
	for _, di := range deletedIdx {
		dcell := &decoded[di]
		if dcell.Overflow != 0 {
			if err := t.freeOverflowChain(dcell.Overflow); err != nil {
				return deleted, err
			}
		}
	}
	// Rewrite the surviving cells contiguously from the end of the usable
	// area (cells grow downward; the first cell occupies the highest
	// addresses, ending at usableSize — btree.c defragmentPage packs from
	// cbrk=usableSize, src/btree.c:2205; no bytes are reserved at the page
	// end, so the flushed image leaves no untracked tail).
	start := int(t.usableSize)
	newPtrs := make([]uint16, len(keep))
	for pos, ci := range keep {
		start -= len(encoded[ci])
		copy(pg.Data[start:start+len(encoded[ci])], encoded[ci])
		newPtrs[pos] = uint16(start)
	}
	ptrBase := coff + storage.CellPointerOffset
	for i := 0; i < len(newPtrs); i++ {
		binary.BigEndian.PutUint16(pg.Data[ptrBase+i*2:ptrBase+i*2+2], newPtrs[i])
	}
	// Zero the remaining pointer slots.
	for i := len(newPtrs); i < int(page.CellCount); i++ {
		pg.Data[ptrBase+i*2] = 0
		pg.Data[ptrBase+i*2+1] = 0
	}
	page.CellCount = uint16(len(newPtrs))
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	if len(newPtrs) == 0 {
		// The page became empty: SQLite sets the cell content pointer to the
		// page's usable end for empty leaves (zeroPage: put2byte(&data[hdr+5],
		// pBt->usableSize)).
		page.CellContent = uint16(t.usableSize)
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(t.usableSize))
		pg.Data[coff+7] = 0
	} else {
		page.CellContent = uint16(start)
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(start))
		pg.Data[coff+7] = 0
	}
	if err := t.pager.WritePage(pg); err != nil {
		return deleted, err
	}
	return deleted, nil
}

// deleteCellOnPage removes the cell at cellIdx from the given leaf page,
// shifting the pointer array down and updating the cell count. The cell's
// overflow-page chain is returned to the freelist first — btree.c
// dropCell → sqlite3BtreeClearCell → clearCell → freePageChain
// (src/btree.c:7237/6893): an overwritten or deleted cell's chain is
// exclusively owned by that cell, so leaking it permanently orphans the
// pages ("Page N is never used") and breaks freelist/vacuum accounting.
func (t *BTree) deleteCellOnPage(pg *pager.Page, page *storage.BTreePage, cellIdx int) error {
	coff := contentOffset(pg.PageNum)
	if cellIdx < 0 || cellIdx >= int(page.CellCount) {
		return fmt.Errorf("btree: cell index %d out of range (count %d)", cellIdx, page.CellCount)
	}
	// Free the removed cell's overflow chain (clearCell parity). Decode
	// before any pointer shift so the cell's bytes are still in place.
	cellType := leafCellType(page.PageType)
	ptrBase := coff + storage.CellPointerOffset
	delOff := int(storage.CellPointer(pg.Data, coff, cellIdx, int(t.pageSize)))
	if delCell, derr := storage.DecodeCell(pg.Data, delOff, cellType, int(t.usableSize)); derr == nil && delCell.Overflow != 0 {
		if err := t.freeOverflowChain(delCell.Overflow); err != nil {
			return err
		}
	}
	for i := cellIdx; i < int(page.CellCount)-1; i++ {
		src := ptrBase + (i+1)*2
		dst := ptrBase + i*2
		pg.Data[dst] = pg.Data[src]
		pg.Data[dst+1] = pg.Data[src+1]
	}
	lastPtr := ptrBase + (int(page.CellCount)-1)*2
	pg.Data[lastPtr] = 0
	pg.Data[lastPtr+1] = 0
	page.CellCount--
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	if page.CellCount == 0 {
		// The page became empty: SQLite sets the cell content pointer to the
		// page's usable end for empty leaves (so the free-space accounting is
		// consistent; an empty page whose content pointer is 0 looks like a
		// crash-written page — "free space corruption"). Reset it to the
		// usable size so the next insert treats it as fresh.
		page.CellContent = uint16(t.usableSize)
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(t.usableSize))
		pg.Data[coff+7] = 0 // fragmented free bytes
		return t.pager.WritePage(pg)
	}
	// Compact the remaining cells down so the deleted cell's bytes are
	// reclaimed, then persist the mutation so a fresh cursor / pager read
	// sees the deletion (the pager cache returns the same buffer, but the
	// page must be marked dirty to be written back on flush and to keep
	// reads consistent).
	if err := t.compactLeafAfterDelete(pg, page, coff); err != nil {
		return err
	}
	return t.pager.WritePage(pg)
}

// compactLeafAfterDelete rebuilds a leaf's surviving cells contiguously from
// the end of the usable area after a cell removal. Without this, repeated
// create/drop on the schema btree fragments the content area and eventually
// corrupts cells (stale dropped-table ghosts, overlapping new cells). Cells
// grow downward — defragmentPage packs from cbrk=usableSize, no page-end
// reservation; CellContent is updated to the new lowest start and there is
// no fragmented free space after compaction.
func (t *BTree) compactLeafAfterDelete(pg *pager.Page, page *storage.BTreePage, coff int) error {
	cellType := leafCellType(page.PageType)
	cells := make([][]byte, int(page.CellCount))
	for i := 0; i < int(page.CellCount); i++ {
		p := int(storage.CellPointer(pg.Data, coff, i, int(t.pageSize)))
		// Read the cell's encoded length: for table cells the payload
		// length varint precedes the rowid; the encoded length is the
		// number of bytes the cell occupies on the page.
		c, err := storage.DecodeCell(pg.Data, p, cellType, int(t.usableSize))
		if err != nil {
			return err
		}
		cells[i] = storage.EncodeCell(c)
	}
	// Rewrite cells contiguously: the first cell (index 0) ends at
	// usableSize (cells grow downward — defragmentPage packs from
	// cbrk=usableSize, no page-end reservation). Compute each cell's
	// start.
	ptrBase := coff + storage.CellPointerOffset
	start := int(t.usableSize)
	for i := 0; i < len(cells); i++ {
		start -= len(cells[i])
		copy(pg.Data[start:start+len(cells[i])], cells[i])
		binary.BigEndian.PutUint16(pg.Data[ptrBase+i*2:ptrBase+i*2+2], uint16(start))
	}

	page.CellContent = uint16(start)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(start))
	// After compaction there is no fragmented free space.
	pg.Data[coff+7] = 0
	return nil
}

func (t *BTree) cellMatches(pg *pager.Page, page *storage.BTreePage, idx int, fn func(cell *storage.Cell) bool) bool {
	coff := contentOffset(pg.PageNum)
	cellOff := int(storage.CellPointer(pg.Data, coff, idx, int(t.pageSize)))
	cellType := leafCellType(page.PageType)
	cell, err := storage.DecodeCell(pg.Data, cellOff, cellType, int(t.usableSize))
	if err != nil {
		return false
	}
	// Decode only the cell's local portion — every DeleteCellsWhere caller
	// (DELETE/UPDATE/FK rowid matching) predicates on cell.RowID, which lives
	// in the cell header. Reading the full overflow chain here made a bulk
	// delete of large-blob rows (e.g. DELETE FROM %_segments with 4KB blocks)
	// read every blob once per candidate cell, O(n × blob) — the
	// between-scenario DELETE in fts4merge4 took ~40s.
	return fn(cell)
}
