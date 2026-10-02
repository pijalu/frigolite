package btree

// Single-cell table-leaf delete: DeleteCellByRowID's fast path. The generic
// deleteAllMatchingFromLeaf rewrites a leaf by decoding AND re-encoding every
// cell of the page (defensible for predicate sweeps that remove many cells,
// wasteful by ~N decodes when exactly one known cell goes away). The fast
// path stages the survivors' RAW page bytes through a scratch arena and packs
// them exactly like finishLeafDelete does — same output bytes, no per-cell
// decode/re-encode. Any anomaly (parse failure, malformed cell, a duplicate
// rowid on the same leaf) declines the fast path and the caller re-runs the
// generic predicate delete, so observable behavior is identical in every
// case, including corrupt images.

import (
	"encoding/binary"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// delSpan is one survivor cell's raw byte range on the page.
type delSpan struct {
	off  int
	size int
}

// deleteSingleTableRowID removes the cell at idx (the row the cursor seeked
// to) from the table leaf at leafNum without decoding the surviving cells.
// handled=false declines the fast path (the caller falls back to
// deleteAllMatchingFromLeaf); a non-nil error is real and propagates.
func (t *BTree) deleteSingleTableRowID(leafNum uint32, idx int, rowID int64) (handled bool, n int64, err error) {
	pg, page, coff, delCell, ok := t.pointDeleteTarget(leafNum, idx, rowID)
	if !ok {
		return false, 0, nil
	}
	spans, ok := t.survivorSpans(pg, coff, page, idx)
	if !ok {
		return false, 0, nil
	}
	// Overflow-chain release happens before any page mutation; on failure the
	// generic path owns the same situation (finishLeafDelete frees chains
	// before the rewrite and propagates the error), so decline rather than
	// diverge.
	if delCell.Overflow != 0 {
		if ferr := t.freeOverflowChain(delCell.Overflow); ferr != nil {
			return false, 0, nil
		}
	}
	if err := t.rewriteLeafSpans(pg, page, coff, spans); err != nil {
		return false, 0, err
	}
	return true, 1, nil
}

// pointDeleteTarget resolves the fast path's preconditions on the target
// leaf: a parseable table leaf holding a well-formed cell at idx, with no
// duplicate of the same rowid after it (the generic predicate deletes ALL
// matches on the leaf; the cursor seek lands on the FIRST match, so checking
// the next pointer suffices). ok=false declines the fast path.
func (t *BTree) pointDeleteTarget(leafNum uint32, idx int, rowID int64) (*pager.Page, *storage.BTreePage, int, storage.Cell, bool) {
	var delCell storage.Cell
	pg, err := t.pager.ReadPage(leafNum)
	if err != nil {
		return nil, nil, 0, delCell, false // the generic path re-reads and surfaces the error
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return nil, nil, 0, delCell, false
	}
	if page.PageType != storage.PageTypeLeafTable {
		return nil, nil, 0, delCell, false
	}
	if idx < 0 || idx >= int(page.CellCount) {
		return nil, nil, 0, delCell, false
	}
	if idx+1 < int(page.CellCount) && t.tableLeafRowidAt(pg, coff, idx+1) == rowID {
		return nil, nil, 0, delCell, false
	}
	delOff := int(storage.CellPointer(pg.Data, coff, idx, int(t.pageSize)))
	if derr := storage.DecodeCellInto(pg.Data, delOff, storage.CellTableLeaf, int(t.usableSize), &delCell); derr != nil {
		// A malformed target cell is kept by the generic path (decode-failed
		// cells never match the predicate) — let it run.
		return nil, nil, 0, delCell, false
	}
	return pg, page, coff, delCell, true
}

// survivorSpans collects the raw byte range of every cell except skip, in
// cell-pointer order. ok=false declines the fast path when any survivor's
// size cannot be determined (the generic path handles such pages).
func (t *BTree) survivorSpans(pg *pager.Page, coff int, page *storage.BTreePage, skip int) ([]delSpan, bool) {
	spans := make([]delSpan, 0, int(page.CellCount)-1)
	for i := 0; i < int(page.CellCount); i++ {
		if i == skip {
			continue
		}
		p := int(storage.CellPointer(pg.Data, coff, i, int(t.pageSize)))
		sz, err := storage.TableLeafCellSizeAt(pg.Data, p, int(t.usableSize))
		if err != nil {
			return nil, false
		}
		spans = append(spans, delSpan{off: p, size: sz})
	}
	return spans, true
}

// rewriteLeafSpans repacks the page's cells to exactly the image
// finishLeafDelete produces for the same survivor set: survivor bytes in
// cell-pointer order, packed contiguously downward from usableSize, the
// pointer array rewritten in that order, no fragmented free bytes, content
// start at the lowest written byte. The bytes stage through t.delArena
// because source and destination ranges overlap on a fragmented page.
func (t *BTree) rewriteLeafSpans(pg *pager.Page, page *storage.BTreePage, coff int, spans []delSpan) error {
	total := 0
	for _, sp := range spans {
		total += sp.size
	}
	if cap(t.delArena) < total {
		t.delArena = make([]byte, max(total, 64))
	}
	arena := t.delArena[:total]
	pos := 0
	for _, sp := range spans {
		copy(arena[pos:pos+sp.size], pg.Data[sp.off:sp.off+sp.size])
		pos += sp.size
	}
	// Pack from the end of the usable area downward, in pointer-array order
	// (finishLeafDelete's layout: cell 0 ends at usableSize).
	start := int(t.usableSize)
	ptrBase := coff + storage.CellPointerOffset
	arenaPos := 0
	for i, sp := range spans {
		start -= sp.size
		copy(pg.Data[start:start+sp.size], arena[arenaPos:arenaPos+sp.size])
		arenaPos += sp.size
		binary.BigEndian.PutUint16(pg.Data[ptrBase+i*2:ptrBase+i*2+2], uint16(start))
	}
	// Zero the pointer slots past the survivor count.
	for i := len(spans); i < int(page.CellCount); i++ {
		pg.Data[ptrBase+i*2] = 0
		pg.Data[ptrBase+i*2+1] = 0
	}
	page.CellCount = uint16(len(spans))
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	if len(spans) == 0 {
		// The page became empty: SQLite sets the cell content pointer to the
		// usable end for empty leaves (zeroPage).
		page.CellContent = int(t.usableSize)
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(t.usableSize))
	} else {
		page.CellContent = start
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(start))
	}
	pg.Data[coff+7] = 0 // fragmented free bytes: compaction leaves none
	return t.pager.WritePage(pg)
}
