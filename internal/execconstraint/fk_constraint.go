package execconstraint

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
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
func (c *ConstraintEnforcer) fkParentActionRec(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, isDelete bool, skipRowID int64, checkTriggerReinsert, deferNoAction bool, cascadeRec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result, depth int, updRec fkUpdateRec) *Result {
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

	// FK actions run as per-row trigger programs in SQLite; each program
	// invocation fires the trace callback with the top-level statement SQL
	// (fkey1-5.2.1). One fire per parent-row action entry, matching one
	// program per row.
	c.ctx.FireFKProgramTrace()
	for _, ref := range refs {
		if res := c.fkApplyParentRef(ref, parentTable, parentColDefs, oldRow, newRow, isDelete, skipRowID, checkTriggerReinsert, deferNoAction, parentIndex, cascadeRec, depth, updRec); res != nil {
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
func (c *ConstraintEnforcer) fkApplyParentRef(ref FKRefAction, parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, isDelete bool, skipRowID int64, checkTriggerReinsert, deferNoAction bool, parentIndex map[string]int, cascadeRec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result, depth int, updRec fkUpdateRec) *Result {
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
	ex := c.fkBuildRowExcluder(ref, childEntry, parentTable, oldRow, skipRowID)
	tree := c.fkChildTree(ref, childEntry)
	matches := c.fkFindChildMatches(tree, childEntry, parentTable, ex, childIdxs, oldVals, parentColDefs, parentIdxs)
	if len(matches) == 0 {
		return nil
	}
	// For DELETE statements, a trigger may have re-inserted the parent key
	// (e.g. an AFTER DELETE trigger restoring the row). SQLite still fires
	// the FK action at the point of the delete (before AFTER triggers), so
	// the re-insert does NOT suppress RESTRICT/NO ACTION errors. This does
	// not apply when the table itself is being dropped (fkParentDropTable).
	return c.fkParentRefAction(ref, action, matches, childEntry, childColDefs, childIdxs, parentIdxs, oldVals, newRow, oldRow, parentColDefs, isDelete, deferNoAction, tree, cascadeRec, depth, updRec)
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

// fkBuildRowExcluder builds the self-referential row excluder for a FK child
// scan: rowid exclusion for rowid tables, PK-value exclusion for WITHOUT
// ROWID tables (whose cells all share synthetic RowID 0). Inactive for
// non-self-referential FKs.
func (c *ConstraintEnforcer) fkBuildRowExcluder(ref FKRefAction, childEntry *schema.Entry, parentTable *schema.Entry, oldRow RowMap, skipRowID int64) fkRowExcluder {
	ex := fkRowExcluder{selfRef: strings.EqualFold(ref.ChildTable, parentTable.Name), rowID: skipRowID}
	if !ex.selfRef {
		return ex
	}
	if !execdml.HasWithoutRowidKeyword(strings.ToUpper(childEntry.SQL)) {
		return ex
	}
	colDefs := c.ctx.ParseColumnDefs(childEntry.Name, childEntry.SQL)
	order := execdml.WithoutRowidStorageOrder(childEntry.SQL, colDefs)
	if len(order) != len(colDefs) {
		return fkNoRowExclude()
	}
	pkIdx := execdml.WRPKIndices(childEntry.SQL, colDefs)
	key := make([]interface{}, len(pkIdx))
	for k, ci := range pkIdx {
		if v, ok := oldRow.Get(colDefs[ci].Name); ok {
			key[k] = util.UnwrapColumnValue(v)
		}
	}
	ex.wr = true
	ex.wrPKIdx = pkIdx
	ex.wrKey = key
	ex.wrOrder = order
	ex.colDefs = colDefs
	return ex
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
func (c *ConstraintEnforcer) fkFindChildMatches(tree *btree.BTree, childEntry *schema.Entry, parentTable *schema.Entry, ex fkRowExcluder, childIdxs []int, oldVals []interface{}, parentColDefs []sql.ColumnDef, parentIdxs []int) []fkChildMatch {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil
	}
	// The excluder identifies the parent row being updated/deleted. It is
	// only active for self-referential FKs (child == parent table), where
	// that row appears in the child scan. For normal FKs the parent rowid
	// may coincide with a child rowid (both start at 1, especially on
	// WITHOUT ROWID tables), so it must not skip child rows.
	var matches []fkChildMatch
	fkScanCells(cursor, ex, func(cell *storage.Cell, rec *storage.Record) bool {
		remapWRRecord(c, childEntry, nil, rec)
		if fkChildRowMatchesParent(c, rec, childIdxs, oldVals, parentColDefs, parentIdxs) {
			matches = append(matches, fkChildMatch{cell.RowID, rec.Values})
		}
		return false
	})
	return matches
}

// fkRowExcluder identifies the parent row being modified so a self-
// referential FK scan can skip it. Rowid tables exclude by rowid; WITHOUT
// ROWID tables exclude by the excluded row's declared PRIMARY KEY values
// (every WR cell shares the synthetic RowID 0, so rowid exclusion would
// skip the whole table).
type fkRowExcluder struct {
	selfRef bool
	rowID   int64
	wr      bool
	wrPKIdx []int
	wrKey   []interface{}
	wrOrder []int
	colDefs []sql.ColumnDef
}

// fkNoRowExclude returns an inactive excluder (nothing is skipped).
func fkNoRowExclude() fkRowExcluder {
	return fkRowExcluder{}
}

// skip reports whether the cell is the excluded parent row.
func (ex *fkRowExcluder) skip(cell *storage.Cell, rec *storage.Record) bool {
	if !ex.selfRef {
		return false
	}
	if ex.wr {
		return execdml.WRCellMatchesPKKeys(cell, [][]interface{}{ex.wrKey}, ex.wrOrder, ex.wrPKIdx, ex.colDefs)
	}
	return cell.RowID == ex.rowID
}

// fkScanCells iterates a cursor's cells, skipping the self-referential parent
// row, and calls match for each decoded record. Returns true when match
// returned true (early exit), false when the scan completed or hit an error.
func fkScanCells(cursor *btree.Cursor, ex fkRowExcluder, match func(cell *storage.Cell, rec *storage.Record) bool) bool {
	for {
		cell, rec, ok := fkNextCell(cursor)
		if !ok {
			return false
		}
		if ex.skip(cell, rec) {
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

// remapWRRecord permutes a WITHOUT ROWID table's PK-first storage-order
// record to declared column order in place (no-op for rowid tables), so
// positional FK column lookups see declared slots.
func remapWRRecord(c *ConstraintEnforcer, entry *schema.Entry, colDefs []sql.ColumnDef, rec *storage.Record) {
	if rec == nil || entry == nil || !execdml.HasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
		return
	}
	if colDefs == nil {
		colDefs = c.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	}
	full := colDefs
	if len(full) != len(rec.Values) {
		// A partial def list (e.g. only the FK's referenced parent columns)
		// cannot express the storage layout: parse the full table defs.
		full = c.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	}
	order := execdml.WithoutRowidStorageOrder(entry.SQL, full)
	if len(order) == len(full) && len(order) == len(rec.Values) {
		rec.Values = execdml.ReorderToDeclared(rec.Values, order)
	}
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
func (c *ConstraintEnforcer) fkParentRefAction(ref FKRefAction, action string, matches []fkChildMatch, childEntry *schema.Entry, childColDefs []sql.ColumnDef, childIdxs, parentIdxs []int, oldVals []interface{}, newRow, oldRow RowMap, parentColDefs []sql.ColumnDef, isDelete, deferNoAction bool, tree *btree.BTree, cascadeRec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result, depth int, updRec fkUpdateRec) *Result {
	switch action {
	case "RESTRICT":
		// RESTRICT is always immediate — it cannot be deferred (SQLite
		// fkey.c: RESTRICT is checked as soon as the parent key changes,
		// even inside a deferred constraint). The deferNoAction flag only
		// defers NO ACTION (REPLACE's implicit delete, statement-end
		// checks).
		return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed [SITE 0]")}
	case "", "NO ACTION":
		// Default (NO ACTION) rejects the parent operation. For REPLACE's
		// implicit delete the error is deferred: the new row may restore the
		// key, and the statement-end/COMMIT check decides.
		if !deferNoAction {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed [SITE1]")}
		}
	case "CASCADE":
		return c.fkCascadeMatches(ref, matches, childEntry, childColDefs, childIdxs, parentIdxs, newRow, parentColDefs, isDelete, tree, cascadeRec, depth, updRec)
	case "SET NULL":
		return c.fkSetNullMatches(matches, childEntry, childColDefs, childIdxs, tree)
	case "SET DEFAULT":
		return c.fkSetDefaultMatches(matches, childEntry, childIdxs, childColDefs, tree)
	}
	return nil
}

// fkCascadeMatches applies CASCADE to every matched child row (recursive delete
// or cascaded update).
func (c *ConstraintEnforcer) fkCascadeMatches(ref FKRefAction, matches []fkChildMatch, childEntry *schema.Entry, childColDefs []sql.ColumnDef, childIdxs, parentIdxs []int, newRow RowMap, parentColDefs []sql.ColumnDef, isDelete bool, tree *btree.BTree, cascadeRec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result, depth int, updRec fkUpdateRec) *Result {
	for _, m := range matches {
		if isDelete {
			if res := c.fkCascadeDelete(m, childEntry, childColDefs, tree, cascadeRec, depth); res != nil {
				return res
			}
		} else {
			if res := c.fkCascadeUpdate(m, ref, childEntry, childColDefs, childIdxs, parentIdxs, newRow, parentColDefs, tree, updRec, depth); res != nil {
				return res
			}
		}
	}
	return nil
}

// fkSetNullMatches sets every matched child row's FK columns to NULL.
func (c *ConstraintEnforcer) fkSetNullMatches(matches []fkChildMatch, childEntry *schema.Entry, childColDefs []sql.ColumnDef, childIdxs []int, tree *btree.BTree) *Result {
	for _, m := range matches {
		if res := c.fkSetNull(m, childEntry, childColDefs, childIdxs, tree); res != nil {
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
// plain columns) matches the parent key columns, with every key's collation
// equal to the parent column's declared default collation (fkey.c
// sqlite3FkLocateIndex; e_fkey-19.x: an index on (f COLLATE nocase) cannot
// serve a parent key over the BINARY-default column f).
func (c *ConstraintEnforcer) fkParentKeyUniqueIndexes(parentCtx *DatabaseContext, parentEntry *schema.Entry, parentCols []string) bool {
	entries, err := parentCtx.Schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return false
	}
	for _, ent := range entries {
		if !strings.EqualFold(ent.TblName, parentEntry.Name) {
			continue
		}
		cols, colls, ok := fkUniqueIndexCandidate(ent)
		if !ok || !fkSameColumnSet(cols, parentCols) {
			continue
		}
		// Each index key's collation must equal the referenced column's
		// default collation (an index without an explicit COLLATE uses the
		// column's default, so only explicit mismatches disqualify it).
		if c.fkIndexCollationDisqualified(cols, colls, parentEntry) {
			continue
		}
		return true
	}
	return false
}

// fkUniqueIndexCandidate reports whether an index entry is a full UNIQUE
// index on plain columns (not partial, no expression keys), returning its
// column names and each key's explicit collation.
func fkUniqueIndexCandidate(ent *schema.Entry) (cols, colls []string, ok bool) {
	if !execdml.UniqueIndexColsRe.MatchString(ent.SQL) {
		return nil, nil, false
	}
	// Partial indexes never qualify as parent keys.
	if execdml.IndexWhereRe.MatchString(ent.SQL) {
		return nil, nil, false
	}
	colText := execdml.IndexColumnListText(ent.SQL)
	if colText == "" {
		return nil, nil, false
	}
	return fkIndexPlainCols(colText)
}

// fkIndexCollationDisqualified reports whether any index key's explicit
// collation differs from the referenced parent column's declared default
// collation.
func (c *ConstraintEnforcer) fkIndexCollationDisqualified(cols, colls []string, parentEntry *schema.Entry) bool {
	parentColDefs := c.ctx.ParseColumnDefs(parentEntry.Name, parentEntry.SQL)
	for i, col := range cols {
		if defColl := fkColumnDefaultCollation(parentColDefs, col); colls[i] != "" && !strings.EqualFold(colls[i], defColl) {
			return true
		}
	}
	return false
}

// fkColumnDefaultCollation returns the declared COLLATE of one parent column
// ("" when the column does not resolve or has no explicit collation).
func fkColumnDefaultCollation(colDefs []sql.ColumnDef, col string) string {
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, col) {
			return cd.Collate
		}
	}
	return ""
}
