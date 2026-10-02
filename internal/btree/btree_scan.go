package btree

import (
	"errors"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// Page-batch table-leaf scanning: a full scan visits a leaf page's cells in
// pointer order on ONE page, so the per-cell cursor machinery (restore
// checkpoint, page-cache hit, empty-leaf skip) collapses to a per-PAGE cost.
// ScanTableLeaves drives the cursor's own navigation (the same path stack and
// landing-leaf cache Next uses, one ReadPage + memoized parse per leaf) and
// hands the consumer a LeafBatch page view; Cell decodes a cell straight from
// the page bytes with the exact error behavior of the cursor path's
// ReadCellData.
//
// Save/restore fidelity (btree_cursor_save.go): a nested statement's write
// (an eval() UDF or trigger body running inside the consumer's row
// processing) saves the cursor's position as a key. The batch keeps that
// capture exact — Cell syncs the cursor's cellIdx to the cell it is about to
// decode, so saveCursorPosition records the cell currently being consumed —
// and detects the save BEFORE reading page bytes that the write may already
// have defragmented: Cell reports ErrScanSaved and the walker stops. The
// consumer then steps one Next() (restore re-seeks to the saved key, skipNext
// returns the next-larger entry) and finishes the scan on the cursor loop,
// reproducing the pure cursor path's resume semantics entry for entry.

// ErrScanSaved reports that a nested statement's write saved the cursor's
// position while a batch scan was decoding a page (btree.c
// saveAllCursors/CURSOR_REQUIRESEEK): the batch must not read page bytes the
// write may have moved, and the consumer falls back to the cursor loop.
var ErrScanSaved = errors.New("btree: cursor position saved by a nested write during batch scan")

// LeafBatch is a page-local view of one table-leaf page's cells for a batch
// scan. It borrows the cursor's cached page (read-only: the parsed header and
// the page bytes are never mutated — the ParsePage memo's shared-struct
// contract); Cell decodes cells from those bytes in place.
type LeafBatch struct {
	c    *Cursor
	pg   *pager.Page
	page *storage.BTreePage
	coff int
}

// CellCount returns the page's cell count.
func (b *LeafBatch) CellCount() int {
	return int(b.page.CellCount)
}

// Saved reports whether a nested statement's write saved the cursor's
// position while this page's cells were being consumed.
func (b *LeafBatch) Saved() bool {
	return b.c.state == cursorRequireSeek
}

// Cell decodes cell i's local payload and rowID straight from the page bytes
// (ReadCellData's table-leaf fast path without the per-cell cursor
// machinery), following the overflow chain when the payload spills. Cell i
// becomes the cursor's recorded position before its bytes are read, so a
// nested write's save captures it exactly. Errors match the cursor path:
// ErrScanSaved after a nested write's save, the identical corrupt-image texts
// otherwise.
func (b *LeafBatch) Cell(i int) (payload []byte, rowID int64, err error) {
	if b.c.state != cursorValid {
		return nil, 0, ErrScanSaved
	}
	// Record the position before decoding: a write running inside this cell's
	// row processing saves (page, i) — the same position the cursor loop's
	// read/Next window would capture.
	b.c.cellIdx = i
	cellOff := int(storage.CellPointer(b.pg.Data, b.coff, i, int(b.c.tx.pageSize)))

	// A corrupt cell pointer (outside the page buffer) must error, not panic
	// (SQLite reports "database disk image is malformed").
	if cellOff < 0 || cellOff >= len(b.pg.Data) {
		return nil, 0, fmt.Errorf("database disk image is malformed")
	}

	payload, rowID, pos, plen, localLen, err := tableLeafCellHeader(b.pg, cellOff, b.c.tx.usableSize)
	if err != nil {
		return nil, 0, err
	}

	// If the payload spills to overflow pages, follow the chain.
	if localLen < plen {
		full, err := b.c.readCellDataOverflow(b.pg, pos, int(plen), rowID, payload)
		if err != nil {
			return nil, 0, err
		}
		return full, rowID, nil
	}

	return payload, rowID, nil
}

// leafPageResult is one walk step's outcome: the page was consumed (continue
// the walk), the walk ends cleanly (a page the batch cannot decode, a
// swallowed extraction error, or fn's stop), or a nested write saved the
// position mid-page (the consumer must resume on the cursor loop).
type leafPageResult int8

const (
	leafPageNext  leafPageResult = iota // page consumed; continue the walk
	leafPageDone                        // walk ends cleanly
	leafPageSaved                       // nested write saved the position
)

// ScanTableLeaves walks the cursor's remaining table-leaf pages in order,
// calling fn once per page with a LeafBatch view. fn returns stop=true to end
// the walk (the cursor keeps the batch's saved/restore state untouched).
//
// The walk reuses the cursor's own page navigation (cachePage, the
// empty-leaf skip, navigateToNextChild over the path stack), so interior-page
// corruption surfaces at the same point of the scan it would on the cursor
// path. Errors the cursor loop swallows (ReadCellData treats every
// extraction error as clean EOF) end the walk with saved=false, err=nil;
// only fn's errors propagate.
//
// saved=true reports that a nested write saved the cursor's position during
// the walk (CURSOR_REQUIRESEEK): the consumer must step one Next() (which
// performs the re-seek and honors skipNext) and finish the scan on the
// cursor loop.
func (c *Cursor) ScanTableLeaves(fn func(b *LeafBatch) (stop bool, err error)) (saved bool, err error) {
	for !c.endOfBTree {
		if c.state != cursorValid {
			// A save landed before the walk started: decline to the consumer
			// (its cursor loop re-seeks through restoreIfNeeded).
			return true, nil
		}
		res, err := c.scanTableLeafPage(fn)
		if err != nil {
			return false, err
		}
		if res == leafPageNext {
			c.clearPageCache()
			c.navigateToNextChild()
			continue
		}
		return res == leafPageSaved, nil
	}
	return false, nil
}

// scanTableLeafPage primes the cursor's page cache, skips empty leaves, and
// hands fn one table-leaf page's batch view. Pages the batch cannot decode
// (non-table-leaf) and errors the cursor loop would swallow end the walk
// cleanly (leafPageDone) — the consumer's cursor loop reproduces their
// behavior exactly.
func (c *Cursor) scanTableLeafPage(fn func(b *LeafBatch) (stop bool, err error)) (leafPageResult, error) {
	if err := c.cachePage(); err != nil {
		return leafPageDone, nil // the cursor loop reports/ends identically
	}
	// Skip forward past empty leaf pages (the engine keeps them after
	// deletes; scans must not stop early).
	if err := c.skipEmptyLeaves(); err != nil {
		return leafPageDone, nil // "cursor at end": clean EOF on the cursor path
	}
	page := c.currentPage
	if page.PageType != storage.PageTypeLeafTable {
		// Not a rowid-table leaf (WITHOUT ROWID trees live in index leaves):
		// decline — the consumer decodes through the cursor path.
		return leafPageDone, nil
	}
	if page.CellCount == 0 {
		return leafPageNext, nil
	}
	b := LeafBatch{c: c, pg: c.currentPg, page: page, coff: contentOffset(c.currentPg.PageNum)}
	stop, err := fn(&b)
	if err != nil {
		return leafPageDone, err
	}
	if stop {
		return leafPageDone, nil
	}
	if c.state != cursorValid {
		return leafPageSaved, nil // a nested write saved the position mid-page
	}
	return leafPageNext, nil
}
