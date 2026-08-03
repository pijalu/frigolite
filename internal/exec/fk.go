package exec

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// fkCascadeRef describes a column-level FOREIGN KEY with ON DELETE CASCADE:
// childTable.childCol references parentTable.parentCol.
type fkCascadeRef struct {
	childTable  string
	childCol    string
	parentTable string
	parentCol   string
}

// fkRefAction describes a column-level FOREIGN KEY with its ON DELETE and
// ON UPDATE actions ("" = NO ACTION).
type fkRefAction struct {
	childTable  string
	childCol    string
	parentTable string
	parentCol   string
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

// isDeferredFK reports whether a References string declares
// DEFERRABLE INITIALLY DEFERRED (the FK is checked at COMMIT, not per-statement).
func isDeferredFK(refs string) bool {
	upper := strings.ToUpper(refs)
	return strings.Contains(upper, "DEFERRABLE") && strings.Contains(upper, "INITIALLY DEFERRED")
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

// fkChildRefs returns all column-level FOREIGN KEY references whose parent is
// the given table, across all attached databases.
func (e *Engine) fkChildRefs(parentTable string) []fkRefAction {
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
			for _, cd := range colDefs {
				if cd.References == "" {
					continue
				}
				m := fkRefFullRe.FindStringSubmatch(cd.References)
				if m == nil {
					continue
				}
				if !strings.EqualFold(m[1], parentTable) {
					continue
				}
				parentCol := strings.TrimSpace(m[2])
				if parentCol == "" {
					if parentEntry, err := e.schema.FindTable(parentTable); err == nil {
						pcd := e.parseColumnDefs(parentEntry.Name, parentEntry.SQL)
						parentCol = e.fkParentPKColumn(parentEntry, pcd)
					}
				}
				refs = append(refs, fkRefAction{
					childTable:  ent.Name,
					childCol:    cd.Name,
					parentTable: m[1],
					parentCol:   parentCol,
					onDelete:    fkActionInRefs(m[3], "DELETE"),
					onUpdate:    fkActionInRefs(m[3], "UPDATE"),
					deferred:    strings.Contains(strings.ToUpper(m[4]), "INITIALLY DEFERRED"),
				})
			}
			// Table-level FOREIGN KEY constraints.
			for _, tc := range e.tableConstraints(ent.Name, ent.SQL) {
				if tc.Type != sql.ConstraintForeignKey || tc.RefTable == "" {
					continue
				}
				if !strings.EqualFold(tc.RefTable, parentTable) {
					continue
				}
				// Parent columns: explicit list, or the parent's PK columns
				// in order (a multi-column FK without an explicit list maps
				// child columns to the parent PK columns positionally).
				var parentCols []string
				if len(tc.RefCols) > 0 {
					parentCols = tc.RefCols
				} else if parentEntry, err := e.schema.FindTable(parentTable); err == nil {
					pcd := e.parseColumnDefs(parentEntry.Name, parentEntry.SQL)
					parentCols = e.fkParentPKColumns(parentEntry, pcd)
				}
				for i, childCol := range tc.Columns {
					parentCol := ""
					if i < len(parentCols) {
						parentCol = parentCols[i]
					}
					refs = append(refs, fkRefAction{
						childTable:  ent.Name,
						childCol:    childCol.Name,
						parentTable: tc.RefTable,
						parentCol:   parentCol,
						onDelete:    fkActionFromText(tc.RefAction, "DELETE"),
						onUpdate:    fkActionFromText(tc.RefAction, "UPDATE"),
						deferred:    tc.Deferred,
					})
				}
			}
		}
	}
	return refs
}

// fkParentDelete enforces FOREIGN KEY actions when a parent row is deleted:
// RESTRICT/NO ACTION children cause an error; CASCADE children are deleted;
// SET NULL / SET DEFAULT children update their FK column.
func (e *Engine) fkParentDelete(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	return e.fkParentAction(parentTable, parentColDefs, oldRow, nil, true, 0, true)
}

// fkParentDropTable enforces FOREIGN KEY actions when a table is dropped.
// Unlike a DELETE statement, no trigger can re-insert the parent rows, so the
// trigger-reinsert check is disabled.
func (e *Engine) fkParentDropTable(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	return e.fkParentAction(parentTable, parentColDefs, oldRow, nil, true, 0, false)
}

// fkParentUpdate enforces FOREIGN KEY actions when a parent row's key changes:
// the old key value is checked against children (RESTRICT/NO ACTION error,
// CASCADE propagates the new value, SET NULL/SET DEFAULT update the column).
// skipRowID identifies the parent row being updated; when it is also a child
// row (self-referential FK) whose FK columns are updated consistently, it is
// not a conflict.
func (e *Engine) fkParentUpdate(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, skipRowID int64) *Result {
	return e.fkParentAction(parentTable, parentColDefs, oldRow, newRow, false, skipRowID, true)
}

// fkParentAction is the shared implementation for parent DELETE/UPDATE FK
// enforcement. newRow is non-nil for UPDATE (CASCADE propagates the new key).
func (e *Engine) fkParentAction(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, isDelete bool, skipRowID int64, checkTriggerReinsert bool) *Result {
	if !e.foreignKeys {
		return &Result{}
	}
	refs := e.fkChildRefs(parentTable.Name)
	if len(refs) == 0 {
		return &Result{}
	}
	parentIndex := buildColumnIndex(parentColDefs)

	for _, ref := range refs {
		// DEFERRABLE INITIALLY DEFERRED constraints are checked at COMMIT,
		// not per-statement; skip them in the immediate check.
		if ref.deferred {
			continue
		}
		// When dropping a table, self-referential FKs (child == parent) are
		// dropped with the table and do not block the DROP (SQLite allows
		// DROP TABLE on a table with self-referencing rows).
		if !checkTriggerReinsert && isDelete && strings.EqualFold(ref.childTable, parentTable.Name) {
			continue
		}
		action := ref.onDelete
		if !isDelete {
			action = ref.onUpdate
		}
		parentIdx, ok := parentIndex[ref.parentCol]
		if !ok {
			continue
		}
		// The parent column value being changed/deleted.
		oldValRaw, _ := oldRow.Get(ref.parentCol)
		oldVal := unwrapRowValue(oldValRaw)
		if parentIdx < 0 {
			parentIdx = cdIndex(parentColDefs, ref.parentCol)
		}
		if parentIdx < 0 || parentIdx >= len(parentColDefs) {
			continue
		}
		if oldVal == nil {
			continue
		}
		// For UPDATE, if the parent key value did not actually change there
		// is no FK impact (children still reference the same key).
		if !isDelete && newRow != nil {
			newValRaw, _ := newRow.Get(ref.parentCol)
			newVal := unwrapRowValue(newValRaw)
			if newVal != nil && util.CompareValues(newVal, oldVal) == 0 {
				continue
			}
		}
		childEntry, err := e.schema.FindTable(ref.childTable)
		if err != nil {
			continue
		}
		childColDefs := e.parseColumnDefs(childEntry.Name, childEntry.SQL)
		childIndex := buildColumnIndex(childColDefs)
		childIdx, ok := childIndex[ref.childCol]
		if !ok {
			continue
		}
		parentColDef := parentColDefs[parentIdx]
		childColDef := childColDefs[childIdx]
		// Match children using the parent column's collation and affinity.
		oldVal = util.ApplyColumnAffinity(oldVal, parentColDef.Type)

		tree := e.tableBTreeForName(childEntry.Name, childEntry.RootPage, true)
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
			if cell.RowID == skipRowID {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			if childIdx < len(rec.Values) && rec.Values[childIdx] != nil &&
				util.CompareValuesCollate(rec.Values[childIdx], oldVal, parentColDef.Collate) == 0 {
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
		// (e.g. an AFTER DELETE trigger restoring the row), making the
		// references valid again — then no action is needed. This does not
		// apply when the table itself is being dropped (fkParentDropTable).
		if checkTriggerReinsert && isDelete {
			parentTree := e.tableBTreeForName(parentTable.Name, parentTable.RootPage, true)
			if e.tableHasValue(parentTree, parentIdx, oldVal, parentColDef.Collate) {
				continue
			}
		}
		switch action {
		case "", "NO ACTION", "RESTRICT":
			// Default (NO ACTION) and RESTRICT reject the parent operation.
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
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
					e.invalidateRowIDCache(childEntry.RootPage)
					if hasTriggers {
						if trigResult := e.fireAfterDeleteTriggers(childEntry.Name, oldChildRow); trigResult.Error != nil {
							return trigResult
						}
					}
				} else {
					// CASCADE UPDATE: propagate the parent's new key value.
					newValRaw, _ := newRow.Get(ref.parentCol)
					newVal := unwrapRowValue(newValRaw)
					vals := make([]interface{}, len(m.values))
					copy(vals, m.values)
					if childIdx < len(vals) {
						vals[childIdx] = util.ApplyColumnAffinity(newVal, parentColDef.Type)
					}
					if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
						return cell.RowID == m.rowID
					}); err != nil {
						return &Result{Error: err}
					}
					e.invalidateRowIDCache(childEntry.RootPage)
					newRecord, err := storage.EncodeRecord(vals)
					if err != nil {
						return &Result{Error: err}
					}
					newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: m.rowID, Payload: newRecord}
					if err := tree.InsertCell(newCell); err != nil {
						return &Result{Error: err}
					}
					e.bumpRowIDCache(childEntry.RootPage, m.rowID)
				}
			}
		case "SET NULL":
			for _, m := range matches {
				vals := make([]interface{}, len(m.values))
				copy(vals, m.values)
				if childIdx < len(vals) {
					vals[childIdx] = nil
				}
				if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
					return cell.RowID == m.rowID
				}); err != nil {
					return &Result{Error: err}
				}
				e.invalidateRowIDCache(childEntry.RootPage)
				newRecord, err := storage.EncodeRecord(vals)
				if err != nil {
					return &Result{Error: err}
				}
				newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: m.rowID, Payload: newRecord}
				if err := tree.InsertCell(newCell); err != nil {
					return &Result{Error: err}
				}
				e.bumpRowIDCache(childEntry.RootPage, m.rowID)
			}
		case "SET DEFAULT":
			for _, m := range matches {
				vals := make([]interface{}, len(m.values))
				copy(vals, m.values)
				if childIdx < len(vals) {
					if childColDef.Default != nil {
						if dv, err := e.evalExpr(childColDef.Default, nil); err == nil {
							vals[childIdx] = dv
						}
					} else {
						vals[childIdx] = nil
					}
				}
				if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
					return cell.RowID == m.rowID
				}); err != nil {
					return &Result{Error: err}
				}
				e.invalidateRowIDCache(childEntry.RootPage)
				newRecord, err := storage.EncodeRecord(vals)
				if err != nil {
					return &Result{Error: err}
				}
				newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: m.rowID, Payload: newRecord}
				if err := tree.InsertCell(newCell); err != nil {
					return &Result{Error: err}
				}
				e.bumpRowIDCache(childEntry.RootPage, m.rowID)
			}
		}
	}
	return &Result{}
}

// unwrapRowValue extracts the raw value from a ColumnValue wrapper.
func unwrapRowValue(v interface{}) interface{} {
	return util.UnwrapColumnValue(v)
}

// tableHasValue reports whether any row in the table has the given value in
// the column at colIdx (using the given collation for comparison).
func (e *Engine) tableHasValue(tree *btree.BTree, colIdx int, val interface{}, collation string) bool {
	if tree == nil {
		return false
	}
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			return false
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			return false
		}
		if colIdx < len(rec.Values) && rec.Values[colIdx] != nil &&
			util.CompareValuesCollate(rec.Values[colIdx], val, collation) == 0 {
			return true
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			return false
		}
	}
}

// fkRefRe parses "parentTable(parentCol) ON DELETE CASCADE" from a column's
// References string.
var fkRefRe = regexp.MustCompile(`(?is)^\s*([^\s(]+)\(([^)]+)\)\s+ON\s+DELETE\s+CASCADE`)

// fkCascadeRefs returns the FK CASCADE references whose parent is the given
// table (cached per parent table; invalidated by invalidateTableCaches).
func (e *Engine) fkCascadeRefs(parentTable string) []fkCascadeRef {
	if e.fkCache == nil {
		e.fkCache = make(map[string][]fkCascadeRef)
	}
	if refs, ok := e.fkCache[parentTable]; ok {
		return refs
	}
	var refs []fkCascadeRef
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
			for _, cd := range colDefs {
				m := fkRefRe.FindStringSubmatch(cd.References)
				if m == nil {
					continue
				}
				if strings.EqualFold(m[1], parentTable) {
					refs = append(refs, fkCascadeRef{
						childTable:  ent.Name,
						childCol:    cd.Name,
						parentTable: m[1],
						parentCol:   strings.TrimSpace(m[2]),
					})
				}
			}
		}
	}
	e.fkCache[parentTable] = refs
	return refs
}

// cascadeDelete deletes child rows whose FK column matches the deleted parent
// row's referenced column value (ON DELETE CASCADE), firing BEFORE/AFTER
// DELETE triggers for each child row.
func (e *Engine) cascadeDelete(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, parentValues []interface{}) *Result {
	refs := e.fkCascadeRefs(parentTable.Name)
	if len(refs) == 0 {
		return &Result{}
	}
	parentIndex := buildColumnIndex(parentColDefs)
	for _, ref := range refs {
		parentIdx, ok := parentIndex[ref.parentCol]
		if !ok || parentIdx >= len(parentValues) || parentValues[parentIdx] == nil {
			continue
		}
		parentVal := parentValues[parentIdx]
		childEntry, err := e.schema.FindTable(ref.childTable)
		if err != nil {
			continue
		}
		childColDefs := e.parseColumnDefs(childEntry.Name, childEntry.SQL)
		childIndex := buildColumnIndex(childColDefs)
		childIdx, ok := childIndex[ref.childCol]
		if !ok {
			continue
		}
		tree := e.tableBTreeForName(childEntry.Name, childEntry.RootPage, true)
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
			if childIdx < len(rec.Values) && rec.Values[childIdx] != nil && util.CompareValues(rec.Values[childIdx], parentVal) == 0 {
				matches = append(matches, match{cell.RowID, rec.Values})
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
		hasTriggers := e.hasTriggersForTable(childEntry.Name)
		for _, m := range matches {
			oldRow := buildRowMapFromValues(m.values, childColDefs, m.rowID)
			if hasTriggers {
				if trigResult := e.fireBeforeDeleteTriggers(childEntry.Name, oldRow); trigResult.Error != nil {
					return trigResult
				}
			}
			if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
				return cell.RowID == m.rowID
			}); err != nil {
				return &Result{Error: err}
			}
			e.invalidateRowIDCache(childEntry.RootPage)
			if hasTriggers {
				if trigResult := e.fireAfterDeleteTriggers(childEntry.Name, oldRow); trigResult.Error != nil {
					return trigResult
				}
			}
		}
	}
	return &Result{}
}

// fkRefAnyRe parses "parentTable(parentCol)" (with optional ON DELETE/UPDATE
// actions) from a column's References string. A bare "parentTable" reference
// (no parent column) implicitly targets the parent's PRIMARY KEY.
var fkRefAnyRe = regexp.MustCompile(`(?is)^\s*([^\s(]+)(?:\s*\(([^)]+)\))?`)

// selfFKMatch reports whether a row satisfies a self-referential table-level
// FOREIGN KEY constraint on its own: the row's parent-key column values equal
// its FK column values (so the reference is valid even before the row is
// inserted).
func (e *Engine) selfFKMatch(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, tc sql.TableConstraint) bool {
	colIndex := buildColumnIndex(colDefs)
	var parentCols []string
	if len(tc.RefCols) > 0 {
		parentCols = tc.RefCols
	} else if parentEntry, err := e.schema.FindTable(tc.RefTable); err == nil {
		pcd := e.parseColumnDefs(parentEntry.Name, parentEntry.SQL)
		parentCols = e.fkParentPKColumns(parentEntry, pcd)
	}
	for i, ic := range tc.Columns {
		childIdx, ok := colIndex[ic.Name]
		if !ok || childIdx >= len(values) {
			return false
		}
		parentCol := ""
		if i < len(parentCols) {
			parentCol = parentCols[i]
		}
		parentIdx, ok := colIndex[parentCol]
		if !ok || parentIdx >= len(values) {
			return false
		}
		if values[childIdx] == nil || values[parentIdx] == nil ||
			util.CompareValues(values[childIdx], values[parentIdx]) != 0 {
			return false
		}
	}
	return true
}

// fkParentPKColumn returns the parent's PRIMARY KEY column name (the implicit
// target of a "REFERENCES parent" clause that names no column).
func (e *Engine) fkParentPKColumn(parentEntry *schema.Entry, parentColDefs []sql.ColumnDef) string {
	cols := e.fkParentPKColumns(parentEntry, parentColDefs)
	if len(cols) > 0 {
		return cols[0]
	}
	return ""
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

// checkForeignKeyViolations verifies that every non-NULL column value with a
// FOREIGN KEY clause references an existing parent row. It is only enforced
// when PRAGMA foreign_keys is ON. Returns an error describing the first
// violation.
func (e *Engine) checkForeignKeyViolations(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) *Result {
	if !e.foreignKeys {
		return &Result{}
	}
	for i, cd := range colDefs {
		if cd.References == "" {
			continue
		}
		// DEFERRABLE INITIALLY DEFERRED constraints are checked at COMMIT,
		// not per-statement.
		if isDeferredFK(cd.References) {
			continue
		}
		m := fkRefAnyRe.FindStringSubmatch(cd.References)
		if m == nil {
			continue
		}
		parentTableName := m[1]
		parentCol := strings.TrimSpace(m[2])
		if parentCol == "" {
			// A bare "REFERENCES parent" clause targets the parent's PRIMARY KEY.
			// Search all databases (the parent may be in an ATTACHed database).
			if parentEntry, _, err := e.findTable(parentTableName); err == nil {
				pcd := e.parseColumnDefs(parentEntry.Name, parentEntry.SQL)
				parentCol = e.fkParentPKColumn(parentEntry, pcd)
			}
		}
		if i >= len(values) || values[i] == nil {
			continue
		}
		// A self-referential column FK (REFERENCES the same table) may be
		// satisfied by the row itself: the row's parent-key column equals its
		// FK value (e.g. INSERT INTO self VALUES(13, 13) where b REFERENCES
		// self(a)).
		if strings.EqualFold(parentTableName, tableEntry.Name) && parentCol != "" {
			childIdx, ok := buildColumnIndex(colDefs)[parentCol]
			if ok && childIdx < len(values) && values[childIdx] != nil &&
				util.CompareValues(values[childIdx], values[i]) == 0 {
				continue
			}
		}
		val := values[i]
		if parentCol == "" {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
		}

		parentEntry, _, err := e.findTable(parentTableName)
		if err != nil {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
		}
		parentColDefs := e.parseColumnDefs(parentEntry.Name, parentEntry.SQL)
		parentIndex := buildColumnIndex(parentColDefs)
		parentIdx, ok := parentIndex[parentCol]
		if !ok {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
		}
		// The FK comparison applies the parent column's affinity to the child
		// value (e.g. '35.0' matches an INTEGER parent key 35) and compares
		// using the parent column's collation (SQLite foreign-key rules).
		parentColDef := parentColDefs[parentIdx]
		val = util.ApplyColumnAffinity(val, parentColDef.Type)
		tree := e.tableBTreeForName(parentEntry.Name, parentEntry.RootPage, true)
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
			if parentIdx < len(rec.Values) && rec.Values[parentIdx] != nil && util.CompareValuesCollate(rec.Values[parentIdx], val, parentColDef.Collate) == 0 {
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
	// Table-level FOREIGN KEY constraints (FOREIGN KEY (cols) REFERENCES p).
	for _, tc := range e.tableConstraints(tableEntry.Name, tableEntry.SQL) {
		if tc.Type != sql.ConstraintForeignKey || tc.RefTable == "" {
			continue
		}
		// DEFERRABLE INITIALLY DEFERRED constraints are checked at COMMIT.
		if tc.Deferred {
			continue
		}
		// A self-referential FK (REFERENCES the same table) may be satisfied
		// by the row being inserted itself: if the row's parent-key columns
		// equal its FK values, the reference is valid.
		if strings.EqualFold(tc.RefTable, tableEntry.Name) {
			if selfRef := e.selfFKMatch(tableEntry, colDefs, values, tc); selfRef {
				continue
			}
		}
		// Parent columns: explicit list, or the parent's PK columns in order.
		var parentCols []string
		if len(tc.RefCols) > 0 {
			parentCols = tc.RefCols
		} else if parentEntry, err := e.schema.FindTable(tc.RefTable); err == nil {
			pcd := e.parseColumnDefs(parentEntry.Name, parentEntry.SQL)
			parentCols = e.fkParentPKColumns(parentEntry, pcd)
		}
		parentEntry, err := e.schema.FindTable(tc.RefTable)
		if err != nil {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
		}
		parentColDefs := e.parseColumnDefs(parentEntry.Name, parentEntry.SQL)
		parentIndex := buildColumnIndex(parentColDefs)
		colIndex := buildColumnIndex(colDefs)
		// Build the child key values; skip if any is NULL (NULL FK values
		// are always valid).
		var childKey []interface{}
		hasNull := false
		parentDefs := make([]sql.ColumnDef, 0, len(tc.Columns))
		for i, ic := range tc.Columns {
			childIdx, ok := colIndex[ic.Name]
			if !ok || childIdx >= len(values) {
				return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
			}
			if values[childIdx] == nil {
				hasNull = true
				break
			}
			childKey = append(childKey, values[childIdx])
			parentCol := ""
			if i < len(parentCols) {
				parentCol = parentCols[i]
			}
			parentIdx, ok := parentIndex[parentCol]
			if !ok || parentIdx < 0 || parentIdx >= len(parentColDefs) {
				return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
			}
			parentDefs = append(parentDefs, parentColDefs[parentIdx])
		}
		if hasNull {
			continue
		}
		// Apply parent column affinity to child values and scan the parent.
		for i := range childKey {
			childKey[i] = util.ApplyColumnAffinity(childKey[i], parentDefs[i].Type)
		}
		tree := e.tableBTreeForName(parentEntry.Name, parentEntry.RootPage, true)
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
			allMatch := true
			for i, pd := range parentDefs {
				pIdx, ok := parentIndex[parentCols[i]]
				if !ok || pIdx >= len(rec.Values) || rec.Values[pIdx] == nil ||
					util.CompareValuesCollate(rec.Values[pIdx], childKey[i], pd.Collate) != 0 {
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
