package exec

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// fkRefAction describes one FOREIGN KEY constraint with its ON DELETE and
// ON UPDATE actions ("" = NO ACTION). childCols and parentCols are parallel
// arrays (child column i maps to parent column i).
type fkRefAction struct {
	childTable  string
	childCtx    *DatabaseContext // schema owning the child table
	childCols   []string
	parentTable string
	parentCols  []string
	onDelete    string // "", "CASCADE", "SET NULL", "SET DEFAULT", "RESTRICT"
	onUpdate    string
	deferred    bool // DEFERRABLE INITIALLY DEFERRED (checked at COMMIT)
}

// fkRefFullRe parses "parentTable(parentCol) ON DELETE X ON UPDATE Y" (any
// subset) from a column's References string, with an optional trailing
// "DEFERRABLE INITIALLY DEFERRED" clause. A bare "parentTable" reference
// (no parent column) implicitly targets the parent's PRIMARY KEY.
var fkRefFullRe = regexp.MustCompile(`(?is)^\s*([^\s(]+)(?:\s*\(([^)]+)\))?((?:\s+ON\s+(?:DELETE|UPDATE)\s+(?:CASCADE|RESTRICT|SET\s+NULL|SET\s+DEFAULT|NO\s+ACTION))*(?:\s+MATCH\s+[^\s]+)?)((?:\s+(?:NOT\s+)?DEFERRABLE(?:\s+INITIALLY\s+(?:DEFERRED|IMMEDIATE))?)?)\s*$`)

// fkActionInRefs extracts the ON DELETE/ON UPDATE action from a References
// string's action segment.
func fkActionInRefs(segment, kind string) string {
	upper := strings.ToUpper(segment)
	marker := "ON " + kind + " "
	idx := strings.Index(upper, marker)
	if idx < 0 {
		return ""
	}
	rest := upper[idx+len(marker):]
	for _, action := range []string{"CASCADE", "RESTRICT", "SET NULL", "SET DEFAULT", "NO ACTION"} {
		if strings.HasPrefix(rest, action) {
			return action
		}
	}
	return ""
}

// fkActionFromText extracts the ON DELETE/ON UPDATE action from a table-level
// FK action string (e.g. "ON DELETE SET NULL ON UPDATE CASCADE").
func fkActionFromText(text, kind string) string {
	upper := strings.ToUpper(text)
	marker := "ON " + kind + " "
	idx := strings.Index(upper, marker)
	if idx < 0 {
		return ""
	}
	rest := upper[idx+len(marker):]
	for _, action := range []string{"CASCADE", "RESTRICT", "SET NULL", "SET DEFAULT", "NO ACTION"} {
		if strings.HasPrefix(rest, action) {
			return action
		}
	}
	return ""
}

// fkChildRefs returns the FOREIGN KEY references whose parent table is the
// given table, across all attached databases. A child's FK is included only
// when its parent reference resolves (in the child's own schema, matching
// SQLite's same-database parent lookup) to the given parent entry.
func (e *Engine) fkChildRefs(parentEntry *schema.Entry, parentCtx *DatabaseContext) []fkRefAction {
	var refs []fkRefAction
	for _, ctx := range e.databases {
		entries, err := ctx.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if ent.Name == "sqlite_schema" || ent.Name == "sqlite_master" {
				continue
			}
			colDefs := e.parseColumnDefs(ent.Name, ent.SQL)
			for _, fk := range e.tableFKConstraints(ent, colDefs) {
				// The parent must resolve (in the child's own schema) to the
				// parent being modified.
				pEntry, pCtx, err := e.fkResolveParent(ctx, fk.parentRef)
				if err != nil || pCtx != parentCtx || !strings.EqualFold(pEntry.Name, parentEntry.Name) {
					continue
				}
				parentCols := fk.parentCols
				if len(parentCols) == 0 {
					parentCols = e.fkParentPKColumns(parentEntry, e.parseColumnDefs(parentEntry.Name, parentEntry.SQL))
				}
				refs = append(refs, fkRefAction{
					childTable:  ent.Name,
					childCtx:    ctx,
					childCols:   fk.childCols,
					parentTable: fk.parentRef,
					parentCols:  parentCols,
					onDelete:    fk.onDelete,
					onUpdate:    fk.onUpdate,
					deferred:    fk.deferred,
				})
			}
		}
	}
	return refs
}

// fkParentDelete enforces FOREIGN KEY actions when a parent row is deleted:
// RESTRICT/NO ACTION children cause an error; CASCADE children are deleted
// (recursively, since a cascaded child may itself be a parent); SET NULL /
// SET DEFAULT children update their FK column.
func (e *Engine) fkParentDelete(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	const maxDepth = 1000
	var rec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result
	rec = func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result {
		if depth > maxDepth {
			return &Result{}
		}
		return e.fkParentActionRec(entry, colDefs, row, nil, true, 0, true, false, rec, depth)
	}
	return rec(parentTable, parentColDefs, oldRow, 0)
}

// fkParentDeleteReplace is fkParentDelete for the implicit delete performed by
// INSERT OR REPLACE. NO ACTION / RESTRICT violations are not reported here:
// the REPLACE may re-insert the same key, so the constraint is checked after
// the new row is written (SQLite defers the REPLACE delete's NO ACTION check
// to statement end).
func (e *Engine) fkParentDeleteReplace(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	const maxDepth = 1000
	var rec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result
	rec = func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result {
		if depth > maxDepth {
			return &Result{}
		}
		return e.fkParentActionRec(entry, colDefs, row, nil, true, 0, true, true, rec, depth)
	}
	return rec(parentTable, parentColDefs, oldRow, 0)
}

// fkParentDropTable enforces FOREIGN KEY actions when a table is dropped.
// Unlike a DELETE statement, no trigger can re-insert the parent rows, so the
// trigger-reinsert check is disabled.
func (e *Engine) fkParentDropTable(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	return e.fkParentAction(parentTable, parentColDefs, oldRow, nil, true, 0, false, false)
}

// fkParentUpdate enforces FOREIGN KEY actions when a parent row's key changes:
// the old key value is checked against children (RESTRICT/NO ACTION error,
// CASCADE propagates the new value, SET NULL/SET DEFAULT update the column).
// skipRowID identifies the parent row being updated; when it is also a child
// row (self-referential FK) whose FK columns are updated consistently, it is
// not a conflict.
func (e *Engine) fkParentUpdate(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, skipRowID int64) *Result {
	return e.fkParentAction(parentTable, parentColDefs, oldRow, newRow, false, skipRowID, true, false)
}

// fkParentAction is the shared implementation for parent DELETE/UPDATE FK
// enforcement. newRow is non-nil for UPDATE (CASCADE propagates the new key).
// deferNoAction suppresses the immediate NO ACTION/RESTRICT error (used by
// INSERT OR REPLACE, whose implicit delete may be followed by a re-insert of
// the same key; the constraint is then checked after the new row is written).
func (e *Engine) fkParentAction(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, isDelete bool, skipRowID int64, checkTriggerReinsert, deferNoAction bool) *Result {
	return e.fkParentActionRec(parentTable, parentColDefs, oldRow, newRow, isDelete, skipRowID, checkTriggerReinsert, deferNoAction, nil, 0)
}

// fkParentActionRec is fkParentAction with a recursive CASCADE callback
// (cascadeRec, depth) used when a CASCADE delete removes a row that is itself
// a parent. The existing public entry points pass nil.
func (e *Engine) fkParentActionRec(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, isDelete bool, skipRowID int64, checkTriggerReinsert, deferNoAction bool, cascadeRec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result, depth int) *Result {
	if !e.foreignKeys {
		return &Result{}
	}
	// The parent table's schema context: execDelete/execUpdate/execInsert set
	// currentDMLCtx to the modified table's database. Fall back to a schema
	// search when the context is nil (e.g. DROP TABLE).
	parentCtx := e.currentDMLCtx
	if parentCtx == nil {
		for _, ctx := range e.databases {
			if _, err := ctx.Schema.FindTable(parentTable.Name); err == nil {
				parentCtx = ctx
				break
			}
		}
	}
	refs := e.fkChildRefs(parentTable, parentCtx)
	if len(refs) == 0 {
		return &Result{}
	}
	parentIndex := buildColumnIndex(parentColDefs)

	for _, ref := range refs {
		action := ref.onDelete
		if !isDelete {
			action = ref.onUpdate
		}
		// DEFERRABLE INITIALLY DEFERRED constraints (and RESTRICT/NO ACTION
		// constraints while PRAGMA defer_foreign_keys is ON) are checked at
		// COMMIT, not per-statement. CASCADE / SET NULL / SET DEFAULT actions
		// still fire immediately (SQLite fkey.c: only the constraint CHECK is
		// deferred, not the ON action).
		if ref.deferred || (e.deferForeignKeys && (action == "" || action == "NO ACTION" || action == "RESTRICT")) {
			continue
		}
		// When dropping a table, self-referential FKs (child == parent) are
		// dropped with the table and do not block the DROP (SQLite allows
		// DROP TABLE on a table with self-referencing rows).
		if !checkTriggerReinsert && isDelete && strings.EqualFold(ref.childTable, parentTable.Name) {
			continue
		}
		// Resolve the child in the schema that owns it (a parent in one
		// database may be referenced by same-named tables in others; the
		// main-schema lookup would find the wrong one, e.g. main.c1 instead of
		// aux.c1).
		childEntry, err := e.schema.FindTable(ref.childTable)
		if ref.childCtx != nil {
			childEntry, err = ref.childCtx.Schema.FindTable(ref.childTable)
		}
		if err != nil {
			continue
		}
		childColDefs := e.parseColumnDefs(childEntry.Name, childEntry.SQL)
		childIndex := buildColumnIndex(childColDefs)
		var childIdxs []int
		var parentIdxs []int
		var oldVals []interface{}
		keyChanged := isDelete
		allNonNull := true
		for i, childCol := range ref.childCols {
			childIdx, ok := childIndex[childCol]
			if !ok {
				childIdxs = nil
				break
			}
			parentCol := ""
			if i < len(ref.parentCols) {
				parentCol = ref.parentCols[i]
			}
			parentIdx, ok := parentIndex[parentCol]
			if !ok {
				parentIdxs = nil
				break
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
			if !isDelete && newRow != nil {
				newValRaw, _ := newRow.Get(parentCol)
				newVal := unwrapRowValue(newValRaw)
				if newVal == nil || util.CompareValues(newVal, oldVal) != 0 {
					keyChanged = true
				}
			}
		}
		if len(childIdxs) == 0 || len(parentIdxs) == 0 {
			continue
		}
		if !keyChanged {
			continue
		}
		if !allNonNull {
			continue
		}
		// Apply the parent column's affinity to the old values for matching.
		for i := range oldVals {
			if oldVals[i] != nil {
				oldVals[i] = util.ApplyColumnAffinity(oldVals[i], parentColDefs[parentIdxs[i]].Type)
			}
		}
		// Find matching child rows: every child FK column equals its old
		// parent key value. Use the child's own schema pager (a child in an
		// attached database lives on the attached pager, not main's).
		tree := e.tableBTreeForName(childEntry.Name, childEntry.RootPage, true)
		if ref.childCtx != nil && ref.childCtx.Pager != nil {
			tree = e.tableBTreePg(ref.childCtx.Pager, childEntry.Name, childEntry.RootPage, true)
		}
		cursor, err := tree.OpenCursor()
		if err != nil {
			continue
		}
		type match struct {
			rowID  int64
			values []interface{}
		}
		var matches []match
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			// skipRowID identifies the parent row being updated/deleted. It is
			// only meaningful for self-referential FKs (child == parent table),
			// where that row appears in the child scan. For normal FKs the
			// parent rowid may coincide with a child rowid (both start at 1,
			// especially on WITHOUT ROWID tables), so it must not skip child
			// rows.
			if strings.EqualFold(childEntry.Name, parentTable.Name) && cell.RowID == skipRowID {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			allMatch := true
			for i, cidx := range childIdxs {
				if cidx >= len(rec.Values) || rec.Values[cidx] == nil || oldVals[i] == nil ||
					e.compareValuesCollate(rec.Values[cidx], oldVals[i], parentColDefs[parentIdxs[i]].Collate) != 0 {
					allMatch = false
					break
				}
			}
			if allMatch {
				matches = append(matches, match{cell.RowID, rec.Values})
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
		if len(matches) == 0 {
			continue
		}
		// For DELETE statements, a trigger may have re-inserted the parent key
		// (e.g. an AFTER DELETE trigger restoring the row). SQLite still fires
		// the FK action at the point of the delete (before AFTER triggers), so
		// the re-insert does NOT suppress RESTRICT/NO ACTION errors. This does
		// not apply when the table itself is being dropped (fkParentDropTable).
		switch action {
		case "", "NO ACTION", "RESTRICT":
			// Default (NO ACTION) and RESTRICT reject the parent operation.
			// For REPLACE's implicit delete the error is deferred: the new row
			// may restore the key, and the statement-end/COMMIT check decides.
			if !deferNoAction {
				return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
			}
		case "CASCADE":
			for _, m := range matches {
				if isDelete {
					oldChildRow := buildRowMapFromValues(m.values, childColDefs, m.rowID)
					hasTriggers := e.hasTriggersForTable(childEntry.Name)
					if hasTriggers {
						if trigResult := e.fireBeforeDeleteTriggers(childEntry.Name, oldChildRow); trigResult.Error != nil {
							return trigResult
						}
					}
					if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
						return cell.RowID == m.rowID
					}); err != nil {
						return &Result{Error: err}
					}
					e.invalidateRowIDCache(e.tablePager(childEntry.Name), childEntry.RootPage)
					// Recursively enforce FK actions for the deleted child: it
					// may itself be a parent (self-referential FK chains). The
					// depth guard breaks cycles (SQLite allows CASCADE cycles
					// only when the chain terminates).
					if cascadeRec != nil {
						if res := cascadeRec(childEntry, childColDefs, oldChildRow, depth+1); res.Error != nil {
							return res
						}
					}
					if hasTriggers {
						if trigResult := e.fireAfterDeleteTriggers(childEntry.Name, oldChildRow); trigResult.Error != nil {
							return trigResult
						}
					}
				} else {
					// CASCADE UPDATE: propagate the parent's new key values.
					vals := make([]interface{}, len(m.values))
					copy(vals, m.values)
					for i, cidx := range childIdxs {
						if i < len(ref.parentCols) && cidx < len(vals) {
							newValRaw, _ := newRow.Get(ref.parentCols[i])
							newVal := unwrapRowValue(newValRaw)
							vals[cidx] = util.ApplyColumnAffinity(newVal, parentColDefs[parentIdxs[i]].Type)
						}
					}
					if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
						return cell.RowID == m.rowID
					}); err != nil {
						return &Result{Error: err}
					}
					e.invalidateRowIDCache(e.tablePager(childEntry.Name), childEntry.RootPage)
					newRecord, err := storage.EncodeRecord(vals)
					if err != nil {
						return &Result{Error: err}
					}
					newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: m.rowID, Payload: newRecord}
					if err := tree.InsertCell(newCell); err != nil {
						return &Result{Error: err}
					}
					e.bumpRowIDCache(e.tablePager(childEntry.Name), childEntry.RootPage, m.rowID)
				}
			}
		case "SET NULL":
			for _, m := range matches {
				vals := make([]interface{}, len(m.values))
				copy(vals, m.values)
				for _, cidx := range childIdxs {
					if cidx < len(vals) {
						vals[cidx] = nil
					}
				}
				if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
					return cell.RowID == m.rowID
				}); err != nil {
					return &Result{Error: err}
				}
				e.invalidateRowIDCache(e.tablePager(childEntry.Name), childEntry.RootPage)
				newRecord, err := storage.EncodeRecord(vals)
				if err != nil {
					return &Result{Error: err}
				}
				newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: m.rowID, Payload: newRecord}
				if err := tree.InsertCell(newCell); err != nil {
					return &Result{Error: err}
				}
				e.bumpRowIDCache(e.tablePager(childEntry.Name), childEntry.RootPage, m.rowID)
			}
		case "SET DEFAULT":
			for _, m := range matches {
				vals := make([]interface{}, len(m.values))
				copy(vals, m.values)
				for _, cidx := range childIdxs {
					if cidx < len(vals) {
						if childColDefs[cidx].Default != nil {
							if dv, err := e.evalExpr(childColDefs[cidx].Default, nil); err == nil {
								vals[cidx] = dv
							}
						} else {
							vals[cidx] = nil
						}
					}
				}
				if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
					return cell.RowID == m.rowID
				}); err != nil {
					return &Result{Error: err}
				}
				e.invalidateRowIDCache(e.tablePager(childEntry.Name), childEntry.RootPage)
				newRecord, err := storage.EncodeRecord(vals)
				if err != nil {
					return &Result{Error: err}
				}
				newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: m.rowID, Payload: newRecord}
				if err := tree.InsertCell(newCell); err != nil {
					return &Result{Error: err}
				}
				e.bumpRowIDCache(e.tablePager(childEntry.Name), childEntry.RootPage, m.rowID)
			}
		}
	}
	return &Result{}
}

// unwrapRowValue extracts the raw value from a ColumnValue wrapper.
func unwrapRowValue(v interface{}) interface{} {
	return util.UnwrapColumnValue(v)
}

// fkParentPKColumns returns the parent's PRIMARY KEY column names in order.
func (e *Engine) fkParentPKColumns(parentEntry *schema.Entry, parentColDefs []sql.ColumnDef) []string {
	var cols []string
	for _, cd := range parentColDefs {
		if cd.PrimaryKey {
			cols = append(cols, cd.Name)
		}
	}
	if len(cols) > 0 {
		return cols
	}
	for _, c := range e.tableConstraints(parentEntry.Name, parentEntry.SQL) {
		if c.Type == sql.ConstraintPrimaryKey {
			for _, ic := range c.Columns {
				cols = append(cols, ic.Name)
			}
			return cols
		}
	}
	return nil
}

// fkConstraint describes one FOREIGN KEY constraint of a child table, in
// fkid order (column-level FKs in column order, then table-level FKs in
// constraint order, matching SQLite's FKey list).
type fkConstraint struct {
	childCols  []string // child column names (positional)
	parentRef  string   // parent table reference (may be schema-qualified)
	parentCols []string // explicit parent columns (nil = implicit parent PK)
	onDelete   string   // "", "CASCADE", "SET NULL", "SET DEFAULT", "RESTRICT"
	onUpdate   string
	deferred   bool // DEFERRABLE INITIALLY DEFERRED
}

// tableFKConstraints returns the table's FOREIGN KEY constraints (column-level
// then table-level) in fkid order.
func (e *Engine) tableFKConstraints(entry *schema.Entry, colDefs []sql.ColumnDef) []fkConstraint {
	var fks []fkConstraint
	for _, cd := range colDefs {
		if cd.References == "" {
			continue
		}
		m := fkRefFullRe.FindStringSubmatch(cd.References)
		if m == nil {
			continue
		}
		var parentCols []string
		if pc := strings.TrimSpace(m[2]); pc != "" {
			parentCols = []string{pc}
		}
		fks = append(fks, fkConstraint{
			childCols:  []string{cd.Name},
			parentRef:  strings.TrimSpace(m[1]),
			parentCols: parentCols,
			onDelete:   fkActionInRefs(m[3], "DELETE"),
			onUpdate:   fkActionInRefs(m[3], "UPDATE"),
			deferred:   strings.Contains(strings.ToUpper(m[4]), "INITIALLY DEFERRED"),
		})
	}
	for _, tc := range e.tableConstraints(entry.Name, entry.SQL) {
		if tc.Type != sql.ConstraintForeignKey || tc.RefTable == "" {
			continue
		}
		var cols []string
		for _, ic := range tc.Columns {
			cols = append(cols, ic.Name)
		}
		fks = append(fks, fkConstraint{
			childCols:  cols,
			parentRef:  tc.RefTable,
			parentCols: tc.RefCols,
			onDelete:   fkActionFromText(tc.RefAction, "DELETE"),
			onUpdate:   fkActionFromText(tc.RefAction, "UPDATE"),
			deferred:   tc.Deferred,
		})
	}
	return fks
}

// fkResolveParent resolves an FK's parent table. SQLite looks up unqualified
// FK parents only in the schema containing the child table
// (sqlite3LocateTable(db, 0, zTo, zDb) in fkey.c); a schema-qualified parent
// reference is looked up in the named schema.
func (e *Engine) fkResolveParent(childCtx *DatabaseContext, ref string) (*schema.Entry, *DatabaseContext, error) {
	schemaName, objName := parseSchemaName(ref)
	if schemaName != "" {
		ctx := e.getDB(schemaName)
		if ctx == nil {
			return nil, nil, fmt.Errorf("no such table: %s", ref)
		}
		ent, err := ctx.Schema.FindTable(objName)
		if err != nil {
			return nil, nil, err
		}
		return ent, ctx, nil
	}
	if childCtx != nil {
		ent, err := childCtx.Schema.FindTable(objName)
		if err != nil {
			return nil, nil, err
		}
		return ent, childCtx, nil
	}
	return e.findTable(objName)
}

// fkParentKeyValid reports whether parentCols form a valid parent key for the
// parent table: the PRIMARY KEY, a UNIQUE column/constraint, or a full
// (non-partial, default-collation, non-expression) UNIQUE index — mirroring
// sqlite3FkLocateIndex in fkey.c. parentCols must already be resolved (explicit
// list or the parent's PK for implicit references).
func (e *Engine) fkParentKeyValid(parentCtx *DatabaseContext, parentEntry *schema.Entry, parentColDefs []sql.ColumnDef, parentCols []string) bool {
	if len(parentCols) == 0 {
		return false
	}
	// Single-column FK mapping to the INTEGER PRIMARY KEY is always valid.
	if len(parentCols) == 1 {
		for _, cd := range parentColDefs {
			if isIPKRowidAliasCol(cd) &&
				strings.EqualFold(cd.Name, parentCols[0]) {
				return true
			}
		}
	}
	// The PRIMARY KEY columns (column-level or table-level constraint).
	if pkCols := e.fkParentPKColumns(parentEntry, parentColDefs); len(pkCols) > 0 && fkSameColumnSet(pkCols, parentCols) {
		return true
	}
	// Column-level UNIQUE constraints (each is a single-column unique index).
	for _, cd := range parentColDefs {
		if cd.Unique && len(parentCols) == 1 && strings.EqualFold(cd.Name, parentCols[0]) {
			return true
		}
	}
	// Table-level UNIQUE constraints.
	for _, tc := range e.tableConstraints(parentEntry.Name, parentEntry.SQL) {
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
	// Explicit full UNIQUE indexes (non-partial, plain columns, default
	// collation).
	entries, err := parentCtx.Schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return false
	}
	for _, ent := range entries {
		if !strings.EqualFold(ent.TblName, parentEntry.Name) {
			continue
		}
		if !uniqueIndexColsRe.MatchString(ent.SQL) {
			continue
		}
		// Partial indexes never qualify as parent keys.
		if indexWhereRe.MatchString(ent.SQL) {
			continue
		}
		colText := indexColumnListText(ent.SQL)
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

// fkSameColumnSet reports whether two column-name lists contain the same names
// (case-insensitive, order-independent — fkey.c matches the parent key columns
// as a set and maps values positionally).
func fkSameColumnSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, x := range a {
		found := false
		for j, y := range b {
			if !used[j] && strings.EqualFold(x, y) {
				used[j] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// fkIndexPlainCols parses an index key column list. It returns the plain
// column names and false when any key is an expression or uses an explicit
// non-default COLLATE (expression keys and non-default collations make the
// index unusable as an FK parent key, matching fkey.c's checks).
func fkIndexPlainCols(colText string) ([]string, bool) {
	var cols []string
	for _, part := range splitIndexCols(colText) {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, false
		}
		upper := strings.ToUpper(name)
		// Expression keys are not usable.
		if strings.ContainsAny(name, "()") {
			return nil, false
		}
		// Explicit COLLATE must be the column's default (BINARY) to qualify;
		// any other explicit collation disqualifies the index. We only
		// recognize COLLATE BINARY / no COLLATE as usable.
		if ci := strings.Index(upper, " COLLATE"); ci >= 0 {
			coll := strings.TrimSpace(name[ci+len(" COLLATE"):])
			if !strings.EqualFold(coll, "BINARY") {
				return nil, false
			}
			name = strings.TrimSpace(name[:ci])
		}
		// Strip ASC/DESC.
		if di := strings.Index(upper, " DESC"); di >= 0 {
			name = strings.TrimSpace(name[:di])
		} else if ai := strings.Index(upper, " ASC"); ai >= 0 {
			name = strings.TrimSpace(name[:ai])
		}
		if name == "" {
			return nil, false
		}
		cols = append(cols, name)
	}
	return cols, true
}

// fkViolation is one row of PRAGMA foreign_key_check output: the child table,
// the child rowid (nil for WITHOUT ROWID child tables), the parent table, and
// the FK constraint id.
type fkViolation struct {
	childTable  string
	rowID       interface{}
	parentTable string
	fkID        int
}

// findFKViolations scans FK constraints and returns the violations in the same
// order SQLite reports them (child tables in schema order, rows in rowid
// order). When onlyTable is non-empty it is resolved like an ordinary table
// reference and only that child is scanned; when schemaName is non-empty only
// child tables in that schema are scanned. A missing parent table yields one
// violation per child row (with the referenced parent name); a parent key that
// is not a PRIMARY KEY or UNIQUE index yields a "foreign key mismatch" error
// that aborts the whole check, matching SQLite.
func (e *Engine) findFKViolations(onlyTable, schemaName string) ([]fkViolation, error) {
	var viols []fkViolation
	if onlyTable != "" {
		entry, ctx, err := e.findTable(onlyTable)
		if err != nil {
			if schemaName != "" {
				return nil, fmt.Errorf("no such table: %s.%s", schemaName, onlyTable)
			}
			return nil, err
		}
		if schemaName != "" && !strings.EqualFold(ctx.Name, schemaName) {
			return nil, fmt.Errorf("no such table: %s.%s", schemaName, onlyTable)
		}
		v, err := e.fkCheckChildTable(entry, ctx)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	for _, ctx := range e.dbList {
		if ctx == nil {
			continue
		}
		if schemaName != "" && !strings.EqualFold(ctx.Name, schemaName) {
			continue
		}
		entries, err := ctx.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if isSchemaTable(ent.Name) {
				continue
			}
			v, err := e.fkCheckChildTable(ent, ctx)
			if err != nil {
				return nil, err
			}
			viols = append(viols, v...)
		}
	}
	return viols, nil
}

// fkCheckChildTable scans one child table's FK constraints and returns its
// violations (rows in rowid order).
func (e *Engine) fkCheckChildTable(entry *schema.Entry, ctx *DatabaseContext) ([]fkViolation, error) {
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	fks := e.tableFKConstraints(entry, colDefs)
	if len(fks) == 0 {
		return nil, nil
	}
	// Resolve each FK's parent and validate the parent key up front (a
	// mismatch aborts the whole check). Parent tables missing from the child's
	// schema are reported as violations below, not errors.
	type resolvedFK struct {
		fk          fkConstraint
		parentEntry *schema.Entry
		parentCtx   *DatabaseContext
		parentCols  []string
		parentDefs  []sql.ColumnDef
	}
	resolved := make([]resolvedFK, 0, len(fks))
	for _, fk := range fks {
		parentEntry, parentCtx, err := e.fkResolveParent(ctx, fk.parentRef)
		if err != nil {
			// Missing parent table: every non-NULL-keyed row is a violation.
			resolved = append(resolved, resolvedFK{fk: fk, parentEntry: nil})
			continue
		}
		parentColDefs := e.parseColumnDefs(parentEntry.Name, parentEntry.SQL)
		parentCols := fk.parentCols
		if len(parentCols) == 0 {
			parentCols = e.fkParentPKColumns(parentEntry, parentColDefs)
		}
		if len(parentCols) != len(fk.childCols) ||
			!e.fkParentKeyValid(parentCtx, parentEntry, parentColDefs, parentCols) {
			return nil, fmt.Errorf("foreign key mismatch - %q referencing %q", entry.Name, fk.parentRef)
		}
		resolved = append(resolved, resolvedFK{
			fk:          fk,
			parentEntry: parentEntry,
			parentCtx:   parentCtx,
			parentCols:  parentCols,
			parentDefs:  parentColDefs,
		})
	}
	// Build per-FK child column indices.
	childIndex := buildColumnIndex(colDefs)
	fkChildIdx := make([][]int, len(fks))
	for i, fk := range fks {
		for _, c := range fk.childCols {
			fkChildIdx[i] = append(fkChildIdx[i], childIndex[c])
		}
	}
	tree := e.tableBTreeForName(entry.Name, entry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, nil
	}
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(entry.SQL))
	var viols []fkViolation
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		for fi, rfk := range resolved {
			idxs := fkChildIdx[fi]
			// Skip if any child FK value is NULL (NULL FK values are valid).
			hasNull := false
			for _, cidx := range idxs {
				if cidx < 0 || cidx >= len(rec.Values) || rec.Values[cidx] == nil {
					hasNull = true
					break
				}
			}
			if hasNull {
				continue
			}
			rowID := interface{}(cell.RowID)
			if withoutRowid {
				rowID = nil
			}
			if rfk.parentEntry == nil {
				viols = append(viols, fkViolation{childTable: entry.Name, rowID: rowID, parentTable: rfk.fk.parentRef, fkID: fi})
				continue
			}
			// Apply parent column affinity to child values and scan the parent.
			childKey := make([]interface{}, len(idxs))
			for ci, cidx := range idxs {
				childKey[ci] = util.ApplyColumnAffinity(rec.Values[cidx], rfk.parentDefs[ci].Type)
			}
			parentTree := e.tableBTreePg(rfk.parentCtx.Pager, rfk.parentEntry.Name, rfk.parentEntry.RootPage, true)
			pCursor, err := parentTree.OpenCursor()
			if err != nil {
				continue
			}
			parentIndex := buildColumnIndex(rfk.parentDefs)
			found := false
			for {
				pCell, err := pCursor.ReadCell()
				if err != nil || pCell == nil {
					break
				}
				pRec, err := storage.DecodeRecord(pCell.Payload)
				if err != nil || pRec == nil {
					break
				}
				allMatch := true
				for ci, pcol := range rfk.parentCols {
					pidx := parentIndex[pcol]
					if pidx < 0 || pidx >= len(pRec.Values) || pRec.Values[pidx] == nil ||
						e.compareValuesCollate(pRec.Values[pidx], childKey[ci], rfk.parentDefs[pidx].Collate) != 0 {
						allMatch = false
						break
					}
				}
				if allMatch {
					found = true
					break
				}
				ok, err := pCursor.Next()
				if err != nil || !ok {
					break
				}
			}
			if !found {
				viols = append(viols, fkViolation{childTable: entry.Name, rowID: rowID, parentTable: rfk.parentEntry.Name, fkID: fi})
			}
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return viols, nil
}

// fkDirtyKey identifies a table whose FK relationships changed during the
// current transaction/statement (schema context + table name, so main.c1 and
// aux.c1 are distinct).
type fkDirtyKey struct {
	ctx  *DatabaseContext
	name string
}

// markFKDirty records that a table's rows changed; its FK relationships (as
// child or parent) must be re-validated at COMMIT / statement end.
func (e *Engine) markFKDirty(entry *schema.Entry, ctx *DatabaseContext) {
	if !e.foreignKeys || entry == nil {
		return
	}
	if e.fkDirty == nil {
		e.fkDirty = make(map[fkDirtyKey]bool)
	}
	e.fkDirty[fkDirtyKey{ctx: ctx, name: entry.Name}] = true
}

// resetFKDirty clears the dirty-table set (at BEGIN, COMMIT, ROLLBACK, and
// after a statement-end check).
func (e *Engine) resetFKDirty() {
	e.fkDirty = nil
}

// checkDeferredFK re-validates the FK relationships of every table modified in
// the current transaction/statement and returns "FOREIGN KEY constraint failed"
// when any violation exists. It is called at COMMIT (and at statement end in
// autocommit mode) for deferred constraints and when PRAGMA
// defer_foreign_keys is ON. Only tables whose rows changed (or whose children
// reference a changed parent) are checked, mirroring SQLite's incremental
// deferred-FK counters — pre-existing violations in unrelated tables do not
// fail a COMMIT.
func (e *Engine) checkDeferredFK() error {
	if len(e.fkDirty) == 0 {
		return nil
	}
	var viols []fkViolation
	for key := range e.fkDirty {
		entry, err := key.ctx.Schema.FindTable(key.name)
		if err != nil {
			// The table was dropped during the transaction; its FKs are gone.
			continue
		}
		// The table's own FK constraints (as a child).
		v, err := e.fkCheckChildTable(entry, key.ctx)
		if err != nil {
			return err
		}
		viols = append(viols, v...)
		// Children of this table (as a parent): a parent row deleted/updated
		// may leave children referencing a missing key.
		for _, ref := range e.fkChildRefs(entry, key.ctx) {
			childEntry, cerr := ref.childCtx.Schema.FindTable(ref.childTable)
			if cerr != nil {
				continue
			}
			cv, cerr := e.fkCheckChildTable(childEntry, ref.childCtx)
			if cerr != nil {
				return cerr
			}
			viols = append(viols, cv...)
		}
	}
	if len(viols) > 0 {
		return fmt.Errorf("FOREIGN KEY constraint failed")
	}
	return nil
}

// dmlTargetTable resolves the table modified by a DML statement (INSERT,
// UPDATE, DELETE) for FK dirty tracking.
func (e *Engine) dmlTargetTable(stmt sql.Stmt) (*schema.Entry, *DatabaseContext, error) {
	var name string
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		name = s.Table
	case *sql.UpdateStmt:
		name = s.Table
	case *sql.DeleteStmt:
		name = s.Table
	default:
		return nil, nil, fmt.Errorf("not DML")
	}
	return e.findTable(name)
}

// fkCheckReplaceChildren verifies that the children of a table replaced by
// INSERT OR REPLACE still reference an existing parent key after the new row
// is written (SQLite checks the implicit delete's NO ACTION constraint at
// statement end, not at COMMIT).
func (e *Engine) fkCheckReplaceChildren(parentEntry *schema.Entry, parentCtx *DatabaseContext) *Result {
	for _, ref := range e.fkChildRefs(parentEntry, parentCtx) {
		childEntry, err := ref.childCtx.Schema.FindTable(ref.childTable)
		if err != nil {
			continue
		}
		viols, err := e.fkCheckChildTable(childEntry, ref.childCtx)
		if err != nil {
			return &Result{Error: err}
		}
		if len(viols) > 0 {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
		}
	}
	return &Result{}
}

// checkForeignKeyViolations verifies that every non-NULL column value with a
// FOREIGN KEY clause references an existing parent row. It is only enforced
// when PRAGMA foreign_keys is ON. Returns an error describing the first
// violation. excludeRowID is the rowid of the row being updated (for
// self-referential FKs the row's OLD key value would otherwise falsely
// satisfy the parent lookup); pass 0 for INSERT.
func (e *Engine) checkForeignKeyViolations(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, excludeRowID int64) *Result {
	if !e.foreignKeys {
		return &Result{}
	}
	childCtx := e.currentDMLCtx
	fks := e.tableFKConstraints(tableEntry, colDefs)
	childIndex := buildColumnIndex(colDefs)
	for _, fk := range fks {
		// Resolve the parent table within the child's own schema (SQLite
		// sqlite3LocateTable(db, 0, zTo, zDb)); a missing parent or an
		// invalid parent key is reported at statement time even for deferred
		// constraints (the parent key resolution is part of statement
		// preparation, not the deferred row check).
		parentEntry, parentCtx, err := e.fkResolveParent(childCtx, fk.parentRef)
		if err != nil {
			return &Result{Error: fmt.Errorf("no such table: main.%s", fk.parentRef)}
		}
		parentColDefs := e.parseColumnDefs(parentEntry.Name, parentEntry.SQL)
		parentCols := fk.parentCols
		if len(parentCols) == 0 {
			parentCols = e.fkParentPKColumns(parentEntry, parentColDefs)
		}
		if len(parentCols) != len(fk.childCols) ||
			!e.fkParentKeyValid(parentCtx, parentEntry, parentColDefs, parentCols) {
			return &Result{Error: fmt.Errorf("foreign key mismatch - %q referencing %q", tableEntry.Name, fk.parentRef)}
		}
		// DEFERRABLE INITIALLY DEFERRED constraints (and all constraints
		// while PRAGMA defer_foreign_keys is ON) are checked at COMMIT, not
		// per-statement. The parent resolution above has already reported
		// schema-level errors, so only the row-existence check is skipped.
		if fk.deferred || e.deferForeignKeys {
			continue
		}
		// Build the child key; skip if any FK value is NULL.
		childKey := make([]interface{}, len(fk.childCols))
		hasNull := false
		parentIdx := make([]int, len(fk.childCols))
		parentDefs := make([]sql.ColumnDef, len(fk.childCols))
		parentIndex := buildColumnIndex(parentColDefs)
		for i, childCol := range fk.childCols {
			cidx, ok := childIndex[childCol]
			if !ok || cidx >= len(values) {
				return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
			}
			if values[cidx] == nil {
				hasNull = true
				break
			}
			childKey[i] = values[cidx]
			pcol := parentCols[i]
			pi, ok := parentIndex[pcol]
			if !ok || pi < 0 || pi >= len(parentColDefs) {
				return &Result{Error: fmt.Errorf("foreign key mismatch - %q referencing %q", tableEntry.Name, fk.parentRef)}
			}
			parentIdx[i] = pi
			parentDefs[i] = parentColDefs[pi]
		}
		if hasNull {
			continue
		}
		// A self-referential FK (REFERENCES the same table) may be satisfied
		// by the row being inserted itself: if the row's parent-key columns
		// equal its FK values, the reference is valid even before the row is
		// written (e.g. INSERT INTO t1 VALUES(10000, 10000) where c1
		// REFERENCES t1(c2) makes the row its own parent).
		if strings.EqualFold(fk.parentRef, tableEntry.Name) {
			selfOK := true
			for i := range fk.childCols {
				cidx := childIndex[fk.childCols[i]]
				pidx := parentIndex[parentCols[i]]
				if cidx < 0 || cidx >= len(values) || pidx < 0 || pidx >= len(values) ||
					values[cidx] == nil || values[pidx] == nil ||
					util.CompareValues(values[cidx], values[pidx]) != 0 {
					selfOK = false
					break
				}
			}
			if selfOK {
				continue
			}
		}
		// Apply parent column affinity to child values (e.g. '35.0' matches
		// an INTEGER parent key 35).
		for i := range childKey {
			childKey[i] = util.ApplyColumnAffinity(childKey[i], parentDefs[i].Type)
		}
		tree := e.tableBTreePg(parentCtx.Pager, parentEntry.Name, parentEntry.RootPage, true)
		cursor, err := tree.OpenCursor()
		if err != nil {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
		}
		found := false
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			// The excludeRowID skip applies only to self-referential FKs (child
			// == parent table): the row being updated must not satisfy its own
			// parent lookup. For a normal FK the parent row may coincidentally
			// share a rowid with the child row being updated (both start at 1),
			// and must not be skipped.
			if strings.EqualFold(fk.parentRef, tableEntry.Name) && cell.RowID == excludeRowID {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			allMatch := true
			for i, pi := range parentIdx {
				if pi >= len(rec.Values) || rec.Values[pi] == nil ||
					e.compareValuesCollate(rec.Values[pi], childKey[i], parentDefs[i].Collate) != 0 {
					allMatch = false
					break
				}
			}
			if allMatch {
				found = true
				break
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
		if !found {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
		}
	}
	return &Result{}
}
