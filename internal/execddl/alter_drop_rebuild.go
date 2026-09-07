package execddl

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// ALTER TABLE ... DROP COLUMN support: column filtering/validation and the
// row rebuild that removes the dropped column's slot from every stored
// record (declared-order for rowid tables, PK-first index-leaf records for
// WITHOUT ROWID tables).

// filterDroppedColumn returns the column definitions with the named column
// marked Dropped (PRIMARY KEY/UNIQUE columns cannot be dropped), or a non-nil
// Result describing why the column cannot be dropped.
func filterDroppedColumn(colDefs []sql.ColumnDef, columnName string) ([]sql.ColumnDef, *Result) {
	found := false
	var out []sql.ColumnDef
	for _, c := range colDefs {
		if c.Name != columnName {
			out = append(out, c)
			continue
		}
		// Cannot drop PRIMARY KEY columns.
		if c.PrimaryKey {
			return nil, &Result{Error: fmt.Errorf("cannot drop PRIMARY KEY column: %q", columnName)}
		}
		// Cannot drop UNIQUE columns.
		if c.Unique {
			return nil, &Result{Error: fmt.Errorf("cannot drop UNIQUE column: %q", columnName)}
		}
		found = true
		// Mark as dropped but keep in the list for correct record position mapping.
		c.Dropped = true
		out = append(out, c)
	}
	if !found {
		return nil, &Result{Error: fmt.Errorf("no such column: \"%s\"", columnName)}
	}
	return out, nil
}

// visibleColDefs returns the column definitions with Dropped entries removed.
func visibleColDefs(colDefs []sql.ColumnDef) []sql.ColumnDef {
	var out []sql.ColumnDef
	for _, c := range colDefs {
		if !c.Dropped {
			out = append(out, c)
		}
	}
	return out
}

// visibleCount returns how many column definitions are not Dropped.
func visibleCount(colDefs []sql.ColumnDef) int {
	n := 0
	for _, c := range colDefs {
		if !c.Dropped {
			n++
		}
	}
	return n
}

// rebuildRowsAfterDrop rewrites every row of a table after DROP COLUMN,
// removing the dropped column's value from each record. The dropped column is
// identified by its name (it is the only Dropped-flagged definition in
// colDefs).
// dropRewrite holds one record to re-insert after DROP COLUMN.
type dropRewrite struct {
	rowID  int64
	values []interface{}
}

func (e *DDLExecutor) rebuildRowsAfterDrop(tableEntry *schema.Entry, colDefs []sql.ColumnDef, droppedName, oldSQL string) {
	// Find the dropped column's index in the OLD record layout.
	dropIdx := findColDefIndex(colDefs, droppedName)
	if dropIdx < 0 {
		return
	}
	// WITHOUT ROWID tables live in an index btree whose cells are PK-first
	// storage-order records (index_xinfo iField layout; see execdml
	// wr_order.go). Their rewrite must decode in the OLD layout, drop the
	// declared slot, and re-encode in the NEW layout as CellIndexLeaf cells
	// keyed by PK — CellTableLeaf cells and RowID addressing corrupt the
	// tree (alterdropcol-7.3/9.3).
	withoutRowid := execdml.HasWithoutRowidKeyword(strings.ToUpper(oldSQL))
	newColDefs := visibleColDefs(colDefs)
	var oldOrder, newOrder, pkIdx []int
	if withoutRowid {
		oldOrder = execdml.WithoutRowidStorageOrder(oldSQL, colDefs)
		newOrder = execdml.WithoutRowidStorageOrder(tableEntry.SQL, newColDefs)
		pkIdx = execdml.WRPKIndices(oldSQL, colDefs)
	}
	// The scan and rewrite must use the schema-qualified table name so the
	// correct pager is used for an ATTACHed table. The engine stores
	// generated-column values in the record (both VIRTUAL and STORED are
	// computed at INSERT and written to the row), so dropping a generated
	// column must remove its slot from each record — leaving the slot in
	// place makes every later SELECT * misread columns to the right
	// (alterdropcol-4.x).
	tree := e.ctx.TableBTreeForName(tableEntry.Name, tableEntry.RootPage, !withoutRowid)
	if withoutRowid && len(newOrder) == len(newColDefs) {
		if npk := execdml.WRPKSlotCount(tableEntry.SQL, newColDefs); npk > 0 {
			tree.SetKeyCompare(execdml.WRRecordComparator(npk, newColDefs, newOrder))
		}
	}
	rewrites, rowIDs, wrKeys := e.collectDropRewrites(tree, colDefs, dropIdx, oldOrder, pkIdx, withoutRowid)
	e.applyDropRewrites(tree, tableEntry, colDefs, rewrites, rowIDs, wrKeys, oldOrder, newOrder, pkIdx, withoutRowid)
}

// collectDropRewrites scans the table and returns the records whose dropped
// column slot must be removed (each with its rowID) plus the set of rowIDs to
// delete. Short records written before ADD COLUMN are left unchanged. For a
// WITHOUT ROWID table the records are PK-first storage order: each is mapped
// to declared order (dropping the slot and normalizing short records to full
// width — SQLite's rebuild writes complete records), and the OLD PK key is
// collected for the key-addressed delete phase.
func (e *DDLExecutor) collectDropRewrites(tree *btree.BTree, colDefs []sql.ColumnDef, dropIdx int, oldOrder, pkIdx []int, withoutRowid bool) ([]dropRewrite, map[int64]bool, [][]interface{}) {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, nil, nil
	}
	var rewrites []dropRewrite
	var rowIDs map[int64]bool
	var wrKeys [][]interface{}
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		if withoutRowid {
			rewrites, wrKeys = addWRDropRewrite(rewrites, wrKeys, rec, oldOrder, pkIdx, dropIdx)
		} else {
			rewrites, rowIDs = addDropRewrite(rewrites, rowIDs, cell.RowID, collectDropRowValues(rec, dropIdx))
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return rewrites, rowIDs, wrKeys
}

// addWRDropRewrite appends one WITHOUT ROWID drop rewrite: decode in the OLD
// storage layout, project to declared order, drop the column's slot, and
// collect the OLD PK key for the key-addressed delete phase. Short records
// (dropIdx beyond the declared width) are skipped.
func addWRDropRewrite(rewrites []dropRewrite, wrKeys [][]interface{}, rec *storage.Record, oldOrder, pkIdx []int, dropIdx int) ([]dropRewrite, [][]interface{}) {
	declared := execdml.ReorderToDeclared(rec.Values, oldOrder)
	if dropIdx >= len(declared) {
		return rewrites, wrKeys
	}
	values := make([]interface{}, 0, len(declared)-1)
	values = append(values, declared[:dropIdx]...)
	values = append(values, declared[dropIdx+1:]...)
	rewrites = append(rewrites, dropRewrite{values: values})
	key := make([]interface{}, len(pkIdx))
	for k, ci := range pkIdx {
		if ci < len(declared) {
			key[k] = declared[ci]
		}
	}
	return rewrites, append(wrKeys, key)
}

// addDropRewrite appends a drop rewrite (skipping nil values — short records
// written before ADD COLUMN have no dropped slot) and returns the updated
// collections.
func addDropRewrite(rewrites []dropRewrite, rowIDs map[int64]bool, rowID int64, values []interface{}) ([]dropRewrite, map[int64]bool) {
	if values == nil {
		return rewrites, rowIDs
	}
	rewrites = append(rewrites, dropRewrite{rowID: rowID, values: values})
	if rowIDs == nil {
		rowIDs = make(map[int64]bool)
	}
	rowIDs[rowID] = true
	return rewrites, rowIDs
}

// collectDropRowValues returns the record values with the dropped column's
// slot removed, or nil when the short record has no such slot.
func collectDropRowValues(rec *storage.Record, dropIdx int) []interface{} {
	if dropIdx >= len(rec.Values) {
		return nil
	}
	values := make([]interface{}, 0, len(rec.Values)-1)
	values = append(values, rec.Values[:dropIdx]...)
	values = append(values, rec.Values[dropIdx+1:]...)
	return values
}

// applyDropRewrites deletes the original records and re-inserts them without
// the dropped column's slot (a single delete pass avoids the O(n²) per-row
// delete+insert that made DROP COLUMN on large tables take minutes,
// alterdropcol-9.x: 50000 rows).
func (e *DDLExecutor) applyDropRewrites(tree *btree.BTree, tableEntry *schema.Entry, colDefs []sql.ColumnDef, rewrites []dropRewrite, rowIDs map[int64]bool, wrKeys [][]interface{}, oldOrder, newOrder, pkIdx []int, withoutRowid bool) {
	if len(rewrites) == 0 {
		return
	}
	if withoutRowid {
		// WITHOUT ROWID rows are PK-keyed index cells: delete by OLD PK key
		// match (cell.RowID is a synthetic 0 shared by every row), re-insert
		// CellIndexLeaf records in the NEW storage layout. Rowid-cache
		// bookkeeping does not apply (WR rows have no rowid).
		if _, err := tree.DeleteCellsWhere(func(c *storage.Cell) bool {
			return execdml.WRCellMatchesPKKeys(c, wrKeys, oldOrder, pkIdx, colDefs)
		}); err != nil {
			return
		}
		for _, rw := range rewrites {
			newRecord, err := storage.EncodeRecord(execdml.ReorderToStorage(rw.values, newOrder))
			if err != nil {
				continue
			}
			_ = tree.InsertCell(&storage.Cell{Type: storage.CellIndexLeaf, Payload: newRecord})
		}
		return
	}
	if _, err := tree.DeleteCellsWhere(func(c *storage.Cell) bool {
		return rowIDs[c.RowID]
	}); err != nil {
		return
	}
	e.ctx.InvalidateRowIDCache(e.ctx.TablePager(tableEntry.Name), tableEntry.RootPage)
	for _, rw := range rewrites {
		newRecord, err := storage.EncodeRecord(rw.values)
		if err != nil {
			continue
		}
		newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: rw.rowID, Payload: newRecord}
		_ = tree.InsertCell(newCell)
		e.ctx.BumpRowIDCache(e.ctx.TablePager(tableEntry.Name), tableEntry.RootPage, rw.rowID)
	}
}
