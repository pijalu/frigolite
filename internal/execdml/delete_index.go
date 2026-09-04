package execdml

import (
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// maintainIndexesOnDelete removes every deleted row's entries from all
// indexes on the table — the delete-side mirror of maintainIndexesOnInsert.
// Without this, stale index entries (and their overflow-page chains) survive
// DELETE/REPLACE: scans still LOOK correct because index hits are rowid-joined
// against the table, but auto-vacuum cannot reclaim the pinned overflow
// pages and integrity_check flags the freed-page references. Partial-index
// predicates and expression keys are evaluated in a pure context, matching
// the insert-side semantics (OP_PureFunc).
func (e *DMLExecutor) maintainIndexesOnDelete(tableEntry *schema.Entry, colDefs []sql.ColumnDef, deletedRows []RowMap) error {
	defs := e.allTableIndexes(tableEntry.Name)
	if len(defs) == 0 {
		return nil
	}
	colIndex := buildColumnIndex(colDefs)
	for _, row := range deletedRows {
		rowID, _ := util.UnwrapColumnValue(row["rowid"]).(int64)
		// The deleted row's stored values (decoded from the table cell)
		// reproduce the exact index payload that maintainIndexesOnInsert
		// wrote for this rowid.
		values := e.rowMapColumnValues(row, colDefs)
		for _, def := range defs {
			inIndex, werr := e.indexRowIncluded(def, row)
			if werr != nil {
				return werr
			}
			if !inIndex {
				continue
			}
			indexValues, kerr := e.indexKeyValuesForRow(def, colDefs, colIndex, values, row)
			if kerr != nil {
				return kerr
			}
			if err := e.deleteIndexCell(def, append(indexValues, rowID)); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteIndexCell removes one index entry (the record of indexValues),
// the delete-side mirror of writeIndexCell. The root page cannot move on
// delete (clearEmptyRootRightmost rewrites the root in place), so no root
// tracking is needed. A missing entry is tolerated (no error): the index
// may predate this engine's index maintenance.
func (e *DMLExecutor) deleteIndexCell(def indexDef, indexValues []interface{}) error {
	payload, err := storage.EncodeRecord(indexValues)
	if err != nil {
		return err
	}
	idxTree := btree.NewBTree(def.Ctx.Pager, def.RootPage, false)
	if _, err := idxTree.DeleteIndexEntry(payload); err != nil {
		return err
	}
	return nil
}
