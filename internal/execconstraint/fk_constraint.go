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

// fkChildRefs returns the FOREIGN KEY references whose parent table is the
// given table, across all attached databases. A child's FK is included only
// when its parent reference resolves (in the child's own schema, matching
// SQLite's same-database parent lookup) to the given parent entry.
func (c *ConstraintEnforcer) ChildRefs(parentEntry *schema.Entry, parentCtx *DatabaseContext) []FKRefAction {
	var refs []FKRefAction
	for _, ctx := range c.ctx.Databases() {
		entries, err := ctx.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if ent.Name == "sqlite_schema" || ent.Name == "sqlite_master" {
				continue
			}
			refs = append(refs, c.fkChildRefsForEntry(ctx, ent, parentEntry, parentCtx)...)
		}
	}
	return refs
}

// fkChildRefsForEntry collects the FK references of one child table whose
// parent resolves to the given parent entry.
func (c *ConstraintEnforcer) fkChildRefsForEntry(ctx *DatabaseContext, ent *schema.Entry, parentEntry *schema.Entry, parentCtx *DatabaseContext) []FKRefAction {
	var refs []FKRefAction
	colDefs := c.ctx.ParseColumnDefs(ent.Name, ent.SQL)
	for _, fk := range c.TableFKConstraints(ent, colDefs) {
		// The parent must resolve (in the child's own schema) to the
		// parent being modified.
		pEntry, pCtx, err := c.fkResolveParent(ctx, fk.ParentRef)
		if err != nil || pCtx != parentCtx || !strings.EqualFold(pEntry.Name, parentEntry.Name) {
			continue
		}
		parentCols := fk.ParentCols
		if len(parentCols) == 0 {
			parentCols = c.fkParentPKColumns(parentEntry, c.ctx.ParseColumnDefs(parentEntry.Name, parentEntry.SQL))
		}
		refs = append(refs, FKRefAction{
			ChildTable:  ent.Name,
			ChildCtx:    ctx,
			ChildCols:   fk.ChildCols,
			ParentTable: fk.ParentRef,
			ParentCols:  parentCols,
			OnDelete:    fk.OnDelete,
			OnUpdate:    fk.OnUpdate,
			Deferred:    fk.Deferred,
		})
	}
	return refs
}

// fkParentActionRec is fkParentAction with a recursive CASCADE callback
// (cascadeRec, depth) used when a CASCADE delete removes a row that is itself
// a parent. The existing public entry points pass nil.
func (c *ConstraintEnforcer) fkParentActionRec(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, isDelete bool, skipRowID int64, checkTriggerReinsert, deferNoAction bool, cascadeRec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result, depth int) *Result {
	if !c.ctx.ForeignKeys() {
		return &Result{}
	}
	// The parent table's schema context: execDelete/execUpdate/execInsert set
	// currentDMLCtx to the modified table's database. Fall back to a schema
	// search when the context is nil (e.g. DROP TABLE).
	parentCtx := c.fkParentCtxFor(parentTable)
	refs := c.ChildRefs(parentTable, parentCtx)
	if len(refs) == 0 {
		return &Result{}
	}
	parentIndex := execdml.BuildColumnIndex(parentColDefs)

	for _, ref := range refs {
		if res := c.fkApplyParentRef(ref, parentTable, parentColDefs, oldRow, newRow, isDelete, skipRowID, checkTriggerReinsert, deferNoAction, parentIndex, cascadeRec, depth); res != nil {
			return res
		}
	}
	return &Result{}
}

// fkParentCtxFor resolves the schema context owning the parent table, falling
// back to a schema search when currentDMLCtx is nil (e.g. DROP TABLE).
func (c *ConstraintEnforcer) fkParentCtxFor(parentTable *schema.Entry) *DatabaseContext {
	if ctx := c.ctx.CurrentDMLCtx(); ctx != nil {
		return ctx
	}
	for _, ctx := range c.ctx.Databases() {
		if _, err := ctx.Schema.FindTable(parentTable.Name); err == nil {
			return ctx
		}
	}
	return nil
}

// fkApplyParentRef enforces one child reference's ON DELETE/ON UPDATE action
// against the parent row being modified. Returns a non-nil Result on failure.
func (c *ConstraintEnforcer) fkApplyParentRef(ref FKRefAction, parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, isDelete bool, skipRowID int64, checkTriggerReinsert, deferNoAction bool, parentIndex map[string]int, cascadeRec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result, depth int) *Result {
	action := ref.OnDelete
	if !isDelete {
		action = ref.OnUpdate
	}
	// DEFERRABLE INITIALLY DEFERRED constraints (and RESTRICT/NO ACTION
	// constraints while PRAGMA defer_foreign_keys is ON) are checked at
	// COMMIT, not per-statement. CASCADE / SET NULL / SET DEFAULT actions
	// still fire immediately (SQLite fkey.c: only the constraint CHECK is
	// deferred, not the ON action).
	if fkParentRefDeferred(ref, action, c.ctx.DeferForeignKeys()) {
		return nil
	}
	// When dropping a table, self-referential FKs (child == parent) are
	// dropped with the table and do not block the DROP (SQLite allows
	// DROP TABLE on a table with self-referencing rows).
	if !checkTriggerReinsert && isDelete && strings.EqualFold(ref.ChildTable, parentTable.Name) {
		return nil
	}
	// Resolve the child in the schema that owns it (a parent in one
	// database may be referenced by same-named tables in others; the
	// main-schema lookup would find the wrong one, e.g. main.c1 instead of
	// aux.c1).
	childEntry, err := c.fkResolveChildEntry(ref)
	if err != nil {
		return nil
	}
	childColDefs := c.ctx.ParseColumnDefs(childEntry.Name, childEntry.SQL)
	childIndex := execdml.BuildColumnIndex(childColDefs)
	childIdxs, parentIdxs, oldVals, keyChanged, allNonNull := fkParentKeyIndices(ref, oldRow, newRow, childIndex, parentIndex, parentColDefs, isDelete)
	if len(childIdxs) == 0 || len(parentIdxs) == 0 || !keyChanged || !allNonNull {
		return nil
	}
	// Apply the parent column's affinity to the old values for matching.
	applyParentAffinity(oldVals, parentColDefs, parentIdxs)
	// Find matching child rows: every child FK column equals its old parent
	// key value. Use the child's own schema pager (a child in an attached
	// database lives on the attached pager, not main's).
	tree := c.fkChildTree(ref, childEntry)
	matches := c.fkFindChildMatches(tree, childEntry, parentTable, skipRowID, childIdxs, oldVals, parentColDefs, parentIdxs)
	if len(matches) == 0 {
		return nil
	}
	// For DELETE statements, a trigger may have re-inserted the parent key
	// (e.g. an AFTER DELETE trigger restoring the row). SQLite still fires
	// the FK action at the point of the delete (before AFTER triggers), so
	// the re-insert does NOT suppress RESTRICT/NO ACTION errors. This does
	// not apply when the table itself is being dropped (fkParentDropTable).
	return c.fkParentRefAction(ref, action, matches, childEntry, childColDefs, childIdxs, parentIdxs, oldVals, newRow, oldRow, parentColDefs, isDelete, deferNoAction, tree, cascadeRec, depth)
}

// applyParentAffinity converts the old parent key values to the parent column
// affinity for matching against child FK columns.
func applyParentAffinity(oldVals []interface{}, parentColDefs []sql.ColumnDef, parentIdxs []int) {
	for i := range oldVals {
		if oldVals[i] != nil {
			oldVals[i] = util.ApplyColumnAffinity(oldVals[i], parentColDefs[parentIdxs[i]].Type)
		}
	}
}

// fkParentRefDeferred reports whether a reference's constraint check is
// deferred to COMMIT (DEFERRABLE INITIALLY DEFERRED with a NO ACTION / default
// action, or RESTRICT/NO ACTION while PRAGMA defer_foreign_keys is ON).
// RESTRICT is NEVER deferred — SQLite enforces RESTRICT immediately even for
// a DEFERRABLE INITIALLY DEFERRED constraint (R-24179-60523). The ON action
// still fires now.
func fkParentRefDeferred(ref FKRefAction, action string, deferForeignKeys bool) bool {
	if ref.Deferred {
		return action != "RESTRICT"
	}
	return deferForeignKeys && (action == "" || action == "NO ACTION" || action == "RESTRICT")
}

// fkResolveChildEntry resolves the child table in the schema that owns it.
func (c *ConstraintEnforcer) fkResolveChildEntry(ref FKRefAction) (*schema.Entry, error) {
	if ref.ChildCtx != nil {
		return ref.ChildCtx.Schema.FindTable(ref.ChildTable)
	}
	return c.ctx.Schema().FindTable(ref.ChildTable)
}

// fkChildTree returns the child table's BTree, preferring the child's own
// schema pager (a child in an attached database lives on the attached pager).
func (c *ConstraintEnforcer) fkChildTree(ref FKRefAction, childEntry *schema.Entry) *btree.BTree {
	tree := c.ctx.TableBTreeForName(childEntry.Name, childEntry.RootPage, true)
	if ref.ChildCtx != nil && ref.ChildCtx.Pager != nil {
		tree = c.ctx.TableBTreePg(ref.ChildCtx.Pager, childEntry.Name, childEntry.RootPage, true)
	}
	return tree
}

// fkParentKeyIndices resolves the child/parent column index pairs for a FK
// reference, extracts the old parent key values, and reports whether the key
// changed (for UPDATE) and whether all key values are non-NULL.
func fkParentKeyIndices(ref FKRefAction, oldRow, newRow RowMap, childIndex, parentIndex map[string]int, parentColDefs []sql.ColumnDef, isDelete bool) (childIdxs, parentIdxs []int, oldVals []interface{}, keyChanged, allNonNull bool) {
	keyChanged = isDelete
	allNonNull = true
	for i, childCol := range ref.ChildCols {
		childIdx, parentIdx, parentCol, ok := fkColumnPairIndices(ref, i, childCol, childIndex, parentIndex)
		if !ok {
			return nil, nil, nil, keyChanged, allNonNull
		}
		childIdxs = append(childIdxs, childIdx)
		parentIdxs = append(parentIdxs, parentIdx)
		oldValRaw, _ := oldRow.Get(parentCol)
		oldVal := unwrapRowValue(oldValRaw)
		oldVals = append(oldVals, oldVal)
		if oldVal == nil {
			// A NULL parent key value cannot be referenced by children;
			// the whole key must be non-NULL to have matching children.
			allNonNull = false
		}
		if !isDelete && fkParentKeyChanged(newRow, parentCol, oldVal) {
			keyChanged = true
		}
	}
	return childIdxs, parentIdxs, oldVals, keyChanged, allNonNull
}

// fkColumnPairIndices resolves one FK child/parent column pair to its column
// indices, returning the parent column name too. ok is false when either side
// does not resolve.
func fkColumnPairIndices(ref FKRefAction, i int, childCol string, childIndex, parentIndex map[string]int) (childIdx, parentIdx int, parentCol string, ok bool) {
	childIdx, ok = colIndexLookup(childIndex, childCol)
	if !ok {
		return 0, 0, "", false
	}
	if i < len(ref.ParentCols) {
		parentCol = ref.ParentCols[i]
	}
	parentIdx, ok = colIndexLookup(parentIndex, parentCol)
	if !ok {
		return 0, 0, "", false
	}
	return childIdx, parentIdx, parentCol, true
}

// fkParentKeyChanged reports whether an UPDATE changed the parent key value.
func fkParentKeyChanged(newRow RowMap, parentCol string, oldVal interface{}) bool {
	if newRow == nil {
		return false
	}
	newValRaw, _ := newRow.Get(parentCol)
	newVal := unwrapRowValue(newValRaw)
	return newVal == nil || util.CompareValues(newVal, oldVal) != 0
}

// fkFindChildMatches scans the child table for rows whose FK columns equal the
// old parent key values, returning the matched rowids and decoded values. The
// parent row being updated/deleted (skipRowID) is skipped only for
// self-referential FKs.
func (c *ConstraintEnforcer) fkFindChildMatches(tree *btree.BTree, childEntry *schema.Entry, parentTable *schema.Entry, skipRowID int64, childIdxs []int, oldVals []interface{}, parentColDefs []sql.ColumnDef, parentIdxs []int) []fkChildMatch {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil
	}
	// skipRowID identifies the parent row being updated/deleted. It is only
	// meaningful for self-referential FKs (child == parent table), where that
	// row appears in the child scan. For normal FKs the parent rowid may
	// coincide with a child rowid (both start at 1, especially on WITHOUT
	// ROWID tables), so it must not skip child rows.
	selfRef := strings.EqualFold(childEntry.Name, parentTable.Name)
	var matches []fkChildMatch
	fkScanCells(cursor, selfRef, skipRowID, func(cell *storage.Cell, rec *storage.Record) bool {
		if fkChildRowMatchesParent(c, rec, childIdxs, oldVals, parentColDefs, parentIdxs) {
			matches = append(matches, fkChildMatch{cell.RowID, rec.Values})
		}
		return false
	})
	return matches
}

// fkScanCells iterates a cursor's cells, skipping the self-referential parent
// row, and calls match for each decoded record. Returns true when match
// returned true (early exit), false when the scan completed or hit an error.
func fkScanCells(cursor *btree.Cursor, selfRef bool, skipRowID int64, match func(cell *storage.Cell, rec *storage.Record) bool) bool {
	for {
		cell, rec, ok := fkNextCell(cursor)
		if !ok {
			return false
		}
		if selfRef && cell.RowID == skipRowID {
			// Skip the parent row being updated; end the scan if we cannot
			// advance past it.
			if !fkAdvance(cursor) {
				return false
			}
			continue
		}
		if match(cell, rec) {
			return true
		}
		if !fkAdvance(cursor) {
			return false
		}
	}
}

// fkNextCell reads and decodes the next cell, returning ok=false at the end of
// the table or on error.
func fkNextCell(cursor *btree.Cursor) (*storage.Cell, *storage.Record, bool) {
	cell, err := cursor.ReadCell()
	if err != nil || cell == nil {
		return nil, nil, false
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return nil, nil, false
	}
	return cell, rec, true
}

// fkAdvance moves the cursor to the next cell, reporting whether it succeeded.
func fkAdvance(cursor *btree.Cursor) bool {
	ok, err := cursor.Next()
	return ok && err == nil
}

// fkChildRowMatchesParent reports whether a child record's FK columns equal the
// old parent key values.
func fkChildRowMatchesParent(c *ConstraintEnforcer, rec *storage.Record, childIdxs []int, oldVals []interface{}, parentColDefs []sql.ColumnDef, parentIdxs []int) bool {
	for i, cidx := range childIdxs {
		if cidx >= len(rec.Values) || rec.Values[cidx] == nil || oldVals[i] == nil ||
			c.ctx.CompareValuesCollate(rec.Values[cidx], oldVals[i], parentColDefs[parentIdxs[i]].Collate) != 0 {
			return false
		}
	}
	return true
}

// fkParentRefAction applies one foreign-key ON action (RESTRICT / NO ACTION,
// CASCADE, SET NULL, SET DEFAULT) to the matched child rows. Returns a
// non-nil Result when the action failed.
func (c *ConstraintEnforcer) fkParentRefAction(ref FKRefAction, action string, matches []fkChildMatch, childEntry *schema.Entry, childColDefs []sql.ColumnDef, childIdxs, parentIdxs []int, oldVals []interface{}, newRow, oldRow RowMap, parentColDefs []sql.ColumnDef, isDelete, deferNoAction bool, tree *btree.BTree, cascadeRec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result, depth int) *Result {
	switch action {
	case "RESTRICT":
		// RESTRICT is always immediate — it cannot be deferred (SQLite
		// fkey.c: RESTRICT is checked as soon as the parent key changes,
		// even inside a deferred constraint). The deferNoAction flag only
		// defers NO ACTION (REPLACE's implicit delete, statement-end
		// checks).
		return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
	case "", "NO ACTION":
		// Default (NO ACTION) rejects the parent operation. For REPLACE's
		// implicit delete the error is deferred: the new row may restore the
		// key, and the statement-end/COMMIT check decides.
		if !deferNoAction {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
		}
	case "CASCADE":
		return c.fkCascadeMatches(ref, matches, childEntry, childColDefs, childIdxs, parentIdxs, newRow, parentColDefs, isDelete, tree, cascadeRec, depth)
	case "SET NULL":
		return c.fkSetNullMatches(matches, childEntry, childIdxs, tree)
	case "SET DEFAULT":
		return c.fkSetDefaultMatches(matches, childEntry, childIdxs, childColDefs, tree)
	}
	return nil
}

// fkCascadeMatches applies CASCADE to every matched child row (recursive delete
// or cascaded update).
func (c *ConstraintEnforcer) fkCascadeMatches(ref FKRefAction, matches []fkChildMatch, childEntry *schema.Entry, childColDefs []sql.ColumnDef, childIdxs, parentIdxs []int, newRow RowMap, parentColDefs []sql.ColumnDef, isDelete bool, tree *btree.BTree, cascadeRec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result, depth int) *Result {
	for _, m := range matches {
		if isDelete {
			if res := c.fkCascadeDelete(m, childEntry, childColDefs, tree, cascadeRec, depth); res != nil {
				return res
			}
		} else {
			if res := c.fkCascadeUpdate(m, ref, childEntry, childIdxs, parentIdxs, newRow, parentColDefs, tree); res != nil {
				return res
			}
		}
	}
	return nil
}

// fkSetNullMatches sets every matched child row's FK columns to NULL.
func (c *ConstraintEnforcer) fkSetNullMatches(matches []fkChildMatch, childEntry *schema.Entry, childIdxs []int, tree *btree.BTree) *Result {
	for _, m := range matches {
		if res := c.fkSetNull(m, childEntry, childIdxs, tree); res != nil {
			return res
		}
	}
	return nil
}

// fkSetDefaultMatches sets every matched child row's FK columns to their
// column defaults.
func (c *ConstraintEnforcer) fkSetDefaultMatches(matches []fkChildMatch, childEntry *schema.Entry, childIdxs []int, childColDefs []sql.ColumnDef, tree *btree.BTree) *Result {
	for _, m := range matches {
		if res := c.fkSetDefault(m, childEntry, childIdxs, childColDefs, tree); res != nil {
			return res
		}
	}
	return nil
}

// fkParentKeyValid reports whether parentCols form a valid parent key for the
// parent table: the PRIMARY KEY, a UNIQUE column/constraint, or a full
// (non-partial, default-collation, non-expression) UNIQUE index — mirroring
// sqlite3FkLocateIndex in fkey.c. parentCols must already be resolved (explicit
// list or the parent's PK for implicit references).
func (c *ConstraintEnforcer) fkParentKeyValid(parentCtx *DatabaseContext, parentEntry *schema.Entry, parentColDefs []sql.ColumnDef, parentCols []string) bool {
	if len(parentCols) == 0 {
		return false
	}
	// Single-column FK mapping to the INTEGER PRIMARY KEY is always valid.
	if fkParentKeyIPK(parentColDefs, parentCols) {
		return true
	}
	// The PRIMARY KEY columns (column-level or table-level constraint).
	if pkCols := c.fkParentPKColumns(parentEntry, parentColDefs); len(pkCols) > 0 && fkSameColumnSet(pkCols, parentCols) {
		return true
	}
	// Column-level UNIQUE constraints (each is a single-column unique index).
	if fkColumnLevelUnique(parentColDefs, parentCols) {
		return true
	}
	// Table-level UNIQUE constraints.
	if c.fkTableUniqueConstraints(parentEntry, parentCols) {
		return true
	}
	// Explicit full UNIQUE indexes (non-partial, plain columns, default
	// collation).
	return c.fkParentKeyUniqueIndexes(parentCtx, parentEntry, parentCols)
}

// fkParentKeyIPK reports whether a single-column FK maps to the INTEGER
// PRIMARY KEY (always a valid parent key).
func fkParentKeyIPK(parentColDefs []sql.ColumnDef, parentCols []string) bool {
	if len(parentCols) != 1 {
		return false
	}
	for _, cd := range parentColDefs {
		if execdml.IsIPKRowidAliasCol(cd) && strings.EqualFold(cd.Name, parentCols[0]) {
			return true
		}
	}
	return false
}

// fkColumnLevelUnique reports whether a single-column FK matches a column-level
// UNIQUE constraint.
func fkColumnLevelUnique(parentColDefs []sql.ColumnDef, parentCols []string) bool {
	if len(parentCols) != 1 {
		return false
	}
	for _, cd := range parentColDefs {
		if cd.Unique && strings.EqualFold(cd.Name, parentCols[0]) {
			return true
		}
	}
	return false
}

// fkTableUniqueConstraints reports whether a table-level UNIQUE constraint
// matches the parent key columns.
func (c *ConstraintEnforcer) fkTableUniqueConstraints(parentEntry *schema.Entry, parentCols []string) bool {
	for _, tc := range c.ctx.TableConstraints(parentEntry.Name, parentEntry.SQL) {
		if tc.Type != sql.ConstraintUnique {
			continue
		}
		var cols []string
		for _, ic := range tc.Columns {
			cols = append(cols, ic.Name)
		}
		if fkSameColumnSet(cols, parentCols) {
			return true
		}
	}
	return false
}

// fkParentKeyUniqueIndexes reports whether a full UNIQUE index (non-partial,
// plain columns, default collation) matches the parent key columns.
func (c *ConstraintEnforcer) fkParentKeyUniqueIndexes(parentCtx *DatabaseContext, parentEntry *schema.Entry, parentCols []string) bool {
	entries, err := parentCtx.Schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return false
	}
	for _, ent := range entries {
		if !strings.EqualFold(ent.TblName, parentEntry.Name) {
			continue
		}
		if !execdml.UniqueIndexColsRe.MatchString(ent.SQL) {
			continue
		}
		// Partial indexes never qualify as parent keys.
		if execdml.IndexWhereRe.MatchString(ent.SQL) {
			continue
		}
		colText := execdml.IndexColumnListText(ent.SQL)
		if colText == "" {
			continue
		}
		cols, ok := fkIndexPlainCols(colText)
		if !ok {
			continue
		}
		if fkSameColumnSet(cols, parentCols) {
			return true
		}
	}
	return false
}

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
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		rowID := interface{}(cell.RowID)
		if withoutRowid {
			rowID = nil
		}
		viols = append(viols, c.fkCheckChildRowSet(resolved, fkChildIdx, rec, rowID, entry.Name, onlyImmediate)...)
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return viols, nil
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
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
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
	return c.fkParentRowExists(cursor, excludeRowID, parentRef, tableName, parentIdx, childKey, parentDefs)
}

// fkParentRowExists scans the parent table for a row whose key columns match
// the child key values. The excludeRowID skip applies only to self-referential
// FKs (child == parent table).
func (c *ConstraintEnforcer) fkParentRowExists(cursor *btree.Cursor, excludeRowID int64, parentRef, tableName string, parentIdx []int, childKey []interface{}, parentDefs []sql.ColumnDef) bool {
	// The excludeRowID skip applies only to self-referential FKs (child ==
	// parent table): the row being updated must not satisfy its own parent
	// lookup. For a normal FK the parent row may coincidentally share a rowid
	// with the child row being updated (both start at 1), and must not be
	// skipped.
	selfRef := strings.EqualFold(parentRef, tableName)
	return fkScanCells(cursor, selfRef, excludeRowID, func(_ *storage.Cell, rec *storage.Record) bool {
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
