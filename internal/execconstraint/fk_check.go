package execconstraint

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// findFKViolations scans FK constraints and returns the violations in the same
// order SQLite reports them (child tables in schema order, rows in rowid
// order). When onlyTable is non-empty it is resolved like an ordinary table
// reference and only that child is scanned; when schemaName is non-empty only
// child tables in that schema are scanned. A missing parent table yields one
// violation per child row (with the referenced parent name); a parent key that
// is not a PRIMARY KEY or UNIQUE index yields a "foreign key mismatch" error
// that aborts the whole check, matching SQLite.
func (c *ConstraintEnforcer) FindFKViolations(onlyTable, schemaName string) ([]FKViolation, error) {
	if onlyTable != "" {
		return c.findFKViolationsSingle(onlyTable, schemaName)
	}
	var viols []FKViolation
	for _, ctx := range c.ctx.DBList() {
		v, err := c.FKViolationsInDB(ctx, schemaName)
		if err != nil {
			return nil, err
		}
		viols = append(viols, v...)
	}
	return viols, nil
}

// findFKViolationsSingle resolves onlyTable like an ordinary table reference
// and scans just that child table for violations.
func (c *ConstraintEnforcer) findFKViolationsSingle(onlyTable, schemaName string) ([]FKViolation, error) {
	entry, ctx, err := c.ctx.FindTable(onlyTable)
	if err != nil {
		if schemaName != "" {
			return nil, fmt.Errorf("no such table: %s.%s", schemaName, onlyTable)
		}
		return nil, err
	}
	if schemaName != "" && !strings.EqualFold(ctx.Name, schemaName) {
		return nil, fmt.Errorf("no such table: %s.%s", schemaName, onlyTable)
	}
	return c.fkCheckChildTable(entry, ctx, false)
}

// FKViolationsInDB scans the child tables of one database (optionally filtered
// by schemaName) for FK violations.
func (c *ConstraintEnforcer) FKViolationsInDB(ctx *DatabaseContext, schemaName string) ([]FKViolation, error) {
	if ctx == nil {
		return nil, nil
	}
	if schemaName != "" && !strings.EqualFold(ctx.Name, schemaName) {
		return nil, nil
	}
	entries, err := ctx.Schema.GetEntries(schema.TypeTable)
	if err != nil {
		return nil, nil
	}
	var viols []FKViolation
	for _, ent := range entries {
		if execquery.IsSchemaTable(ent.Name) {
			continue
		}
		v, err := c.fkCheckChildTable(ent, ctx, false)
		if err != nil {
			return nil, err
		}
		viols = append(viols, v...)
	}
	return viols, nil
}

// fkCheckChildTable scans one child table's FK constraints and returns its
// violations (rows in rowid order).
func (c *ConstraintEnforcer) fkCheckChildTable(entry *schema.Entry, ctx *DatabaseContext, onlyImmediate bool) ([]FKViolation, error) {
	colDefs := c.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	fks := c.TableFKConstraints(entry, colDefs)
	if len(fks) == 0 {
		return nil, nil
	}
	resolved, err := c.fkResolveChildFKs(ctx, fks, entry)
	if err != nil {
		return nil, err
	}
	// Build per-FK child column indices.
	childIndex := execdml.BuildColumnIndex(colDefs)
	fkChildIdx := buildFKChildIndices(childIndex, fks)
	tree := c.ctx.TableBTreeForName(entry.Name, entry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, nil
	}
	withoutRowid := execdml.HasWithoutRowidKeyword(strings.ToUpper(entry.SQL))
	var viols []FKViolation
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		v, ok := c.fkCheckChildRowViolations(entry, colDefs, cell, resolved, fkChildIdx, withoutRowid, onlyImmediate)
		if !ok {
			break
		}
		viols = append(viols, v...)
		if ok, err := cursor.Next(); err != nil || !ok {
			break
		}
	}
	return viols, nil
}

// fkCheckChildRowViolations checks one child table cell against every resolved
// FK constraint of the table. ok is false when the cell's record could not be
// decoded and the scan must stop (no violations contributed).
func (c *ConstraintEnforcer) fkCheckChildRowViolations(entry *schema.Entry, colDefs []sql.ColumnDef, cell *storage.Cell, resolved []resolvedFK, fkChildIdx [][]int, withoutRowid, onlyImmediate bool) (viols []FKViolation, ok bool) {
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return nil, false
	}
	remapWRRecord(c, entry, colDefs, rec)
	// Rowid-alias convention: the child's IPK column reads back NULL;
	// its value IS the rowid. foreign_key_check must treat that value
	// as the child key, not as an exempt NULL.
	if !withoutRowid {
		fillRowidAliasSlots(colDefs, rec, cell.RowID)
	}
	rowID := interface{}(cell.RowID)
	if withoutRowid {
		rowID = nil
	}
	return c.fkCheckChildRowSet(resolved, fkChildIdx, rec, rowID, entry.Name, onlyImmediate), true
}

// fkCheckChildRowSet checks one child row against every resolved FK constraint,
// returning the row's violations (in FK order). When onlyImmediate is true,
// constraints deferred to COMMIT (DEFERRABLE INITIALLY DEFERRED, or immediate
// while PRAGMA defer_foreign_keys is ON) are skipped.
func (c *ConstraintEnforcer) fkCheckChildRowSet(resolved []resolvedFK, fkChildIdx [][]int, rec *storage.Record, rowID interface{}, childName string, onlyImmediate bool) []FKViolation {
	var viols []FKViolation
	for fi, rfk := range resolved {
		if onlyImmediate && fkDeferredToCommit(rfk.fk, c.ctx.DeferForeignKeys()) {
			continue
		}
		if v := c.fkCheckChildRow(rfk, fkChildIdx[fi], rec, rowID, childName, fi); v != nil {
			viols = append(viols, *v)
		}
	}
	return viols
}

// fkDeferredToCommit reports whether an FK constraint is deferred to COMMIT:
// DEFERRABLE INITIALLY DEFERRED, or any constraint while PRAGMA
// defer_foreign_keys is ON (which defers even immediate constraints).
func fkDeferredToCommit(fk FKConstraint, deferForeignKeys bool) bool {
	return fk.Deferred || deferForeignKeys
}

// buildFKChildIndices resolves each FK's child column names to column indices.
func buildFKChildIndices(childIndex map[string]int, fks []FKConstraint) [][]int {
	fkChildIdx := make([][]int, len(fks))
	for i, fk := range fks {
		for _, c := range fk.ChildCols {
			if ci, ok := colIndexLookup(childIndex, c); ok {
				fkChildIdx[i] = append(fkChildIdx[i], ci)
			}
		}
	}
	return fkChildIdx
}

// fkCheckChildRow checks one child row against one resolved FK constraint,
// returning a violation when the parent key is missing. NULL FK values are
// valid and yield no violation.
func (c *ConstraintEnforcer) fkCheckChildRow(rfk resolvedFK, idxs []int, rec *storage.Record, rowID interface{}, childName string, fi int) *FKViolation {
	// Skip if any child FK value is NULL (NULL FK values are valid).
	if fkRowKeyHasNull(idxs, rec) {
		return nil
	}
	if rfk.parentEntry == nil {
		return &FKViolation{ChildTable: childName, RowID: rowID, ParentTable: rfk.fk.ParentRef, FKID: fi}
	}
	// Apply parent column affinity to child values and scan the parent.
	childKey := make([]interface{}, len(idxs))
	for ci, cidx := range idxs {
		childKey[ci] = util.ApplyColumnAffinity(rec.Values[cidx], rfk.parentDefs[ci].Type)
	}
	found, ok := c.fkParentRowInTable(rfk, childKey)
	if !ok {
		return nil
	}
	if !found {
		return &FKViolation{ChildTable: childName, RowID: rowID, ParentTable: rfk.parentEntry.Name, FKID: fi}
	}
	return nil
}

// fkRowKeyHasNull reports whether any of the row's FK key columns is NULL or
// out of range.
func fkRowKeyHasNull(idxs []int, rec *storage.Record) bool {
	for _, cidx := range idxs {
		if cidx < 0 || cidx >= len(rec.Values) || rec.Values[cidx] == nil {
			return true
		}
	}
	return false
}

// fkParentRowInTable scans the parent table for a row whose key columns match
// the child key values. ok is false when the parent table cannot be opened (the
// caller skips the check, matching SQLite's tolerate-a-broken-parent behavior).
func (c *ConstraintEnforcer) fkParentRowInTable(rfk resolvedFK, childKey []interface{}) (found, ok bool) {
	parentTree := c.ctx.TableBTreePg(rfk.parentCtx.Pager, rfk.parentEntry.Name, rfk.parentEntry.RootPage, true)
	pCursor, err := parentTree.OpenCursor()
	if err != nil {
		return false, false
	}
	parentIndex := execdml.BuildColumnIndex(rfk.parentDefs)
	for {
		pCell, err := pCursor.ReadCell()
		if err != nil || pCell == nil {
			break
		}
		pRec, err := storage.DecodeRecord(pCell.Payload)
		if err != nil || pRec == nil {
			break
		}
		remapWRRecord(c, rfk.parentEntry, rfk.parentDefs, pRec)
		// Rowid-alias convention: the parent's IPK column reads back NULL
		// from the record; its value IS the rowid, so the parent-key
		// comparison must see the rowid (an INSERT of a valid child key
		// would otherwise be reported as a violation).
		fillRowidAliasSlots(rfk.parentDefs, pRec, pCell.RowID)
		allMatch := c.fkRecordMatchesParent(pRec, rfk, parentIndex, childKey)
		if allMatch {
			return true, true
		}
		ok, err := pCursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return false, true
}

// fkRecordMatchesParent reports whether a parent record's key columns equal the
// child key values.
func (c *ConstraintEnforcer) fkRecordMatchesParent(pRec *storage.Record, rfk resolvedFK, parentIndex map[string]int, childKey []interface{}) bool {
	allMatch := true
	for ci, pcol := range rfk.parentCols {
		pidx, _ := colIndexLookup(parentIndex, pcol)
		if pidx < 0 || pidx >= len(pRec.Values) || pRec.Values[pidx] == nil ||
			c.ctx.CompareValuesCollate(pRec.Values[pidx], childKey[ci], rfk.parentDefs[pidx].Collate) != 0 {
			allMatch = false
			break
		}
	}
	return allMatch
}

// checkForeignKeyViolations verifies that every non-NULL column value with a
// FOREIGN KEY clause references an existing parent row. It is only enforced
// when PRAGMA foreign_keys is ON. Returns an error describing the first
// violation. excludeRowID is the rowid of the row being updated (for
// self-referential FKs the row's OLD key value would otherwise falsely
// satisfy the parent lookup); pass 0 for INSERT.
func (c *ConstraintEnforcer) checkForeignKeyViolations(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, excludeRowID int64) *Result {
	if !c.ctx.ForeignKeys() {
		return &Result{}
	}
	childCtx := c.ctx.CurrentDMLCtx()
	fks := c.TableFKConstraints(tableEntry, colDefs)
	childIndex := execdml.BuildColumnIndex(colDefs)
	for _, fk := range fks {
		parentEntry, parentCtx, parentCols, parentColDefs, errRes := c.fkResolveParentForCheck(tableEntry, fk, childCtx)
		if errRes != nil {
			return errRes
		}
		// DEFERRABLE INITIALLY DEFERRED constraints (and all constraints
		// while PRAGMA defer_foreign_keys is ON) are checked at COMMIT, not
		// per-statement. The parent resolution above has already reported
		// schema-level errors, so only the row-existence check is skipped.
		if fk.Deferred || c.ctx.DeferForeignKeys() {
			continue
		}
		parentIndex := execdml.BuildColumnIndex(parentColDefs)
		childKey, parentIdx, parentDefs, hasNull, errRes := c.fkBuildChildKey(tableEntry, fk, childIndex, parentIndex, parentCols, parentColDefs, values)
		if errRes != nil {
			return errRes
		}
		if hasNull {
			continue
		}
		// A self-referential FK (REFERENCES the same table) may be satisfied
		// by the row being inserted itself: if the row's parent-key columns
		// equal its FK values, the reference is valid even before the row is
		// written (e.g. INSERT INTO t1 VALUES(10000, 10000) where c1
		// REFERENCES t1(c2) makes the row its own parent).
		if fkSelfRefSatisfied(fk, tableEntry.Name, values, childIndex, parentIndex, parentCols) {
			continue
		}
		if !c.fkParentRowExistsForValues(parentCtx, parentEntry, excludeRowID, fk.ParentRef, tableEntry.Name, parentIdx, childKey, parentDefs) {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed [SITE2]")}
		}
	}
	return &Result{}
}

// fkSelfRefSatisfied reports whether a self-referential FK is satisfied by the
// row being inserted itself (the row is its own parent).
func fkSelfRefSatisfied(fk FKConstraint, tableName string, values []interface{}, childIndex, parentIndex map[string]int, parentCols []string) bool {
	if !strings.EqualFold(fk.ParentRef, tableName) {
		return false
	}
	return fkSelfReferentialOK(values, fk, childIndex, parentIndex, parentCols)
}

// fkResolveParentForCheck resolves an FK's parent table within the child's own
// schema (SQLite sqlite3LocateTable) and validates the parent key. A missing
// parent or an invalid parent key is reported at statement time even for
// deferred constraints (the parent key resolution is part of statement
// preparation, not the deferred row check). Returns a non-nil errRes on error.
func (c *ConstraintEnforcer) fkResolveParentForCheck(tableEntry *schema.Entry, fk FKConstraint, childCtx *DatabaseContext) (parentEntry *schema.Entry, parentCtx *DatabaseContext, parentCols []string, parentColDefs []sql.ColumnDef, errRes *Result) {
	parentEntry, parentCtx, err := c.fkResolveParent(childCtx, fk.ParentRef)
	if err != nil {
		return nil, nil, nil, nil, &Result{Error: fmt.Errorf("no such table: main.%s", fk.ParentRef)}
	}
	parentColDefs = c.ctx.ParseColumnDefs(parentEntry.Name, parentEntry.SQL)
	parentCols = fk.ParentCols
	if len(parentCols) == 0 {
		parentCols = c.fkParentPKColumns(parentEntry, parentColDefs)
	}
	if len(parentCols) != len(fk.ChildCols) ||
		!c.fkParentKeyValid(parentCtx, parentEntry, parentColDefs, parentCols) {
		return nil, nil, nil, nil, &Result{Error: fmt.Errorf("foreign key mismatch - %q referencing %q", tableEntry.Name, fk.ParentRef)}
	}
	return parentEntry, parentCtx, parentCols, parentColDefs, nil
}

// fkBuildChildKey builds the child key columns for an FK and resolves the
// parallel parent indices/definitions. hasNull is true when any child FK value
// is NULL (valid, no check needed). Returns a non-nil errRes on error.
func (c *ConstraintEnforcer) fkBuildChildKey(tableEntry *schema.Entry, fk FKConstraint, childIndex, parentIndex map[string]int, parentCols []string, parentColDefs []sql.ColumnDef, values []interface{}) (childKey []interface{}, parentIdx []int, parentDefs []sql.ColumnDef, hasNull bool, errRes *Result) {
	childKey = make([]interface{}, len(fk.ChildCols))
	parentIdx = make([]int, len(fk.ChildCols))
	parentDefs = make([]sql.ColumnDef, len(fk.ChildCols))
	for i, childCol := range fk.ChildCols {
		cidx, ok := colIndexLookup(childIndex, childCol)
		if !ok || cidx >= len(values) {
			return nil, nil, nil, false, &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
		}
		if values[cidx] == nil {
			return childKey, parentIdx, parentDefs, true, nil
		}
		childKey[i] = values[cidx]
		pcol := parentCols[i]
		pi, ok := colIndexLookup(parentIndex, pcol)
		if !ok || pi < 0 || pi >= len(parentColDefs) {
			return nil, nil, nil, false, &Result{Error: fmt.Errorf("foreign key mismatch - %q referencing %q", tableEntry.Name, fk.ParentRef)}
		}
		parentIdx[i] = pi
		parentDefs[i] = parentColDefs[pi]
	}
	return childKey, parentIdx, parentDefs, false, nil
}

// fkParentRowExistsForValues applies parent affinity to the child key and
// scans the parent table for a matching row.
func (c *ConstraintEnforcer) fkParentRowExistsForValues(parentCtx *DatabaseContext, parentEntry *schema.Entry, excludeRowID int64, parentRef, tableName string, parentIdx []int, childKey []interface{}, parentDefs []sql.ColumnDef) bool {
	// Apply parent column affinity to child values (e.g. '35.0' matches an
	// INTEGER parent key 35).
	for i := range childKey {
		childKey[i] = util.ApplyColumnAffinity(childKey[i], parentDefs[i].Type)
	}
	tree := c.ctx.TableBTreePg(parentCtx.Pager, parentEntry.Name, parentEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false
	}
	return c.fkParentRowExists(cursor, parentEntry, excludeRowID, parentRef, tableName, parentIdx, childKey, parentDefs)
}

// fkParentRowExists scans the parent table for a row whose key columns match
// the child key values. The excludeRowID skip applies only to self-referential
// FKs (child == parent table).
func (c *ConstraintEnforcer) fkParentRowExists(cursor *btree.Cursor, parentEntry *schema.Entry, excludeRowID int64, parentRef, tableName string, parentIdx []int, childKey []interface{}, parentDefs []sql.ColumnDef) bool {
	// The excludeRowID skip applies only to self-referential FKs (child ==
	// parent table): the row being updated must not satisfy its own parent
	// lookup. For a normal FK the parent row may coincidentally share a rowid
	// with the child row being updated (both start at 1), and must not be
	// skipped.
	selfRef := strings.EqualFold(parentRef, tableName) && !execdml.HasWithoutRowidKeyword(strings.ToUpper(parentEntry.SQL))
	ex := fkRowExcluder{selfRef: selfRef, rowID: excludeRowID}
	// parentDefs is the per-FK-column subset (parallel to childKey/parentIdx);
	// the record-body adaptations below need the FULL declared column list so
	// slots align with rec.Values positions.
	fullDefs := c.ctx.ParseColumnDefs(parentEntry.Name, parentEntry.SQL)
	return fkScanCells(cursor, ex, func(cell *storage.Cell, rec *storage.Record) bool {
		remapWRRecord(c, parentEntry, fullDefs, rec)
		// Rowid-alias convention: the parent's IPK column reads back NULL
		// from the record; its value IS the rowid.
		fillRowidAliasSlots(fullDefs, rec, cell.RowID)
		return fkParentRecordMatches(c, rec, parentIdx, childKey, parentDefs)
	})
}

// fkParentRecordMatches reports whether a parent record's key columns equal the
// child key values.
func fkParentRecordMatches(c *ConstraintEnforcer, rec *storage.Record, parentIdx []int, childKey []interface{}, parentDefs []sql.ColumnDef) bool {
	for i, pi := range parentIdx {
		if pi >= len(rec.Values) || rec.Values[pi] == nil ||
			c.ctx.CompareValuesCollate(rec.Values[pi], childKey[i], parentDefs[i].Collate) != 0 {
			return false
		}
	}
	return true
}
