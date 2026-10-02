package execquery

import (
	"errors"

	"github.com/pijalu/frigolite/internal/btree"
)

// Batch table scanning (btree.ScanTableLeaves): a full scan reads a leaf
// page's cells in pointer order on one page, so the per-cell cursor
// machinery (restore checkpoint, page-cache hit, empty-leaf skip) collapses
// to a per-page cost and the loop body runs page-locally. The row pipeline
// below the cell extraction (decodeAndFilterRow → WHERE → feed/output) is
// the cursor path's own scanRow, so values, error texts and their order are
// unchanged; only the cell-extraction mechanics differ.
//
// Fallbacks preserve cursor-path behavior exactly:
//   - a page the batch cannot decode (WITHOUT ROWID index leaves) declines
//     to the cursor loop from the current position;
//   - a nested statement's write that saved the cursor mid-scan (an eval()
//     UDF or trigger inside WHERE/output evaluation) stops the batch; the
//     resume steps one Next() — restore re-seeks the saved key and skipNext
//     returns the next-larger entry — then finishes on the cursor loop,
//     the same save/restore dance the pure cursor loop performs;
//   - cell-extraction errors end the scan silently, exactly like the cursor
//     loop's break on ReadCellData errors (record-level decode errors still
//     propagate through scanRow).

// runScanBatch drives the scan through the btree page-batch walker. saved
// reports a mid-scan position save (the caller resumes on the cursor loop).
func (st *scanState) runScanBatch(cursor *btree.Cursor) (saved bool, err error) {
	return cursor.ScanTableLeaves(func(b *btree.LeafBatch) (stop bool, err error) {
		n := b.CellCount()
		for i := 0; i < n; i++ {
			payload, rowID, err := b.Cell(i)
			if err != nil {
				if errors.Is(err, btree.ErrScanSaved) {
					return true, nil // decline: resume on the cursor loop
				}
				return false, nil // clean EOF (the cursor loop breaks silently)
			}
			if err := st.scanRow(payload, rowID); err != nil {
				return false, err
			}
		}
		return false, nil
	})
}
