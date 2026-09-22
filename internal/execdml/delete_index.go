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
		if err := e.deleteRowFromIndexes(defs, colDefs, colIndex, row, values, rowID); err != nil {
			return err
		}
	}
	return nil
}

// deleteRowFromIndexes removes one deleted row's entries from every index it
// participates in (partial-index membership evaluated per index).
func (e *DMLExecutor) deleteRowFromIndexes(defs []indexDef, colDefs []sql.ColumnDef, colIndex map[string]int, row RowMap, values []interface{}, rowID int64) error {
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
	return nil
}

// deleteIndexCellsBatch removes the index entries keyed by rowid in targets
// (each value is the full [key..., rowid] record) with ONE b-tree walk —
// the batch form of deleteIndexCell for statements touching many rows. A
// missing entry is tolerated (no error): the index may predate this engine's
// index maintenance.
func (e *DMLExecutor) deleteIndexCellsBatch(def indexDef, targets map[int64][]interface{}) error {
	if len(targets) == 0 {
		return nil
	}
	encoded := make([][]byte, 0, len(targets))
	for _, indexValues := range targets {
		// Mirror deleteIndexCell's storage normalization: the delete payload
		// must byte-match the entry the insert side wrote.
		indexStorageValues(indexValues)
		payload, err := storage.EncodeRecord(indexValues)
		if err != nil {
			return err
		}
		encoded = append(encoded, payload)
	}
	idxTree := btree.NewBTree(def.Ctx.Pager, def.RootPage, false)
	if _, err := idxTree.DeleteIndexEntries(encoded); err != nil {
		return err
	}
	return nil
}

// deleteIndexCell removes one index entry (the record of indexValues),
// the delete-side mirror of writeIndexCell. The root page cannot move on
// delete (clearEmptyRootRightmost rewrites the root in place), so no root
// tracking is needed. A missing entry is tolerated (no error): the index
// may predate this engine's index maintenance.
func (e *DMLExecutor) deleteIndexCell(def indexDef, indexValues []interface{}) error {
	// Mirror writeIndexCell's storage normalization: the delete payload must
	// byte-match the entry the insert side wrote.
	indexStorageValues(indexValues)
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
