package execdml

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/storage"
)

// rowidsToUpdate builds the set of rowids the changes rewrite.
func rowidsToUpdate(changes []updateChange) map[int64]bool {
	toUpdate := make(map[int64]bool, len(changes))
	for _, c := range changes {
		toUpdate[c.rowID] = true
	}
	return toUpdate
}

// deleteUpdatedTableRows removes the rows applyUpdateChanges Step 0 could not
// rewrite in place. rowid-addressed rows go through a positioned
// DeleteCellByRowID per row — btree.c's single-row delete path: one
// root-to-leaf seek, one single-cell repack, one hinted rebalance — which is
// O(rows x log n) instead of DeleteCellsWhere's O(rows x pages). The
// O(rows x log n) form matters most here because Step 2 re-inserts every
// deleted row immediately after, so an UPDATE that changes a cell's size
// still costs one seek and one page ripple per row rather than a full-table
// rewrite (measured on a 50k-row indexed table: 3,583 us/op -> ~1.8 us/op).
//
// WITHOUT ROWID tables are not rowid-addressable (every row's cell.RowID is a
// synthetic 0), so they keep the old-key predicate sweep, matched on the
// snapshot old PK payloads.
//
// A physical duplicate rowid (corrupt or legacy file) may span more than one
// leaf, so the positioned deletes can miss a copy. When what they removed
// disagrees with the number of rows the changes named, finish with the same
// generic sweep — anomalous files only, so the fast path stays sweep-free.
func (e *DMLExecutor) deleteUpdatedTableRows(tree *btree.BTree, toUpdate, inPlace map[int64]bool, changes []updateChange, wrEntry *schema.Entry, wrOldKeys [][]interface{}) *Result {
	if wrEntry != nil {
		colDefs := e.ctx.ParseColumnDefs(wrEntry.Name, wrEntry.SQL)
		order := WithoutRowidStorageOrder(wrEntry.SQL, colDefs)
		targets := make([][]byte, 0, len(changes))
		for _, change := range changes {
			payload, err := storage.EncodeRecord(ReorderToStorage(change.oldValues, order))
			if err != nil {
				return &Result{Error: err}
			}
			targets = append(targets, payload)
		}
		deleted, err := tree.DeleteIndexEntries(targets)
		if err != nil {
			return &Result{Error: err}
		}
		if deleted != len(targets) {
			return &Result{Error: fmt.Errorf("exec: WITHOUT ROWID UPDATE deleted %d old rows, expected %d", deleted, len(targets))}
		}
		return nil
	}
	var deleted int64
	for i := range changes {
		rowID := changes[i].rowID
		if inPlace[rowID] {
			continue
		}
		n, err := tree.DeleteCellByRowID(rowID)
		if err != nil {
			return &Result{Error: err}
		}
		deleted += n
	}
	if want := int64(len(toUpdate) - len(inPlace)); deleted != want {
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return toUpdate[cell.RowID] && !inPlace[cell.RowID]
		}); err != nil {
			return &Result{Error: err}
		}
	}
	return nil
}
