// Package exec implements query execution.
package exec

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- UPDATE ---

type updateChange struct {
	rowID     int64
	newRowID  *int64 // non-nil when the UPDATE sets rowid/_rowid_/oid to a new value
	values    []interface{}
	oldValues []interface{}
}

func (e *Engine) execUpdate(s *sql.UpdateStmt) *Result {
	if err := e.authorize(auth.ActionUpdate, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	tableEntry, dbCtx, err := e.findTable(s.Table)
	if err != nil {
		// Not a table — route through INSTEAD OF UPDATE triggers on a view.
		viewEntry, _, viewErr := e.findView(s.Table)
		if viewErr == nil {
			return e.execUpdateView(s, viewEntry)
		}
		return &Result{Error: err}
	}

	// Track the modified table's database context for trigger scoping.
	prevDMLCtx := e.currentDMLCtx
	e.currentDMLCtx = dbCtx
	defer func() { e.currentDMLCtx = prevDMLCtx }()

	// Protect system and pragma virtual tables from modification.
	if e.isNonModifiableTable(tableEntry) {
		return &Result{Error: fmt.Errorf("table %s may not be modified", tableEntry.Name)}
	}

	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	// Record which columns this UPDATE statement's SET clause assigns, so
	// UPDATE OF <cols> triggers fire only when a listed column is in the set.
	// Cleared on return (the engine is single-threaded per connection).
	prevSetCols := e.updateSetColumns
	e.updateSetColumns = nil
	for _, a := range s.Assignments {
		e.updateSetColumns = append(e.updateSetColumns, a.Column)
	}
	if len(s.SetParenColumns) > 0 {
		e.updateSetColumns = append(e.updateSetColumns, s.SetParenColumns...)
	}
	defer func() { e.updateSetColumns = prevSetCols }()

	if s.HasReturning {
		if err := e.validateReturning(s.Returning, colDefs, tableEntry.Name); err != nil {
			return &Result{Error: err}
		}
	}

	colIndex := buildColumnIndex(colDefs)

	changes, err := e.collectUpdateChanges(s.Table, tableEntry.RootPage, colIndex, colDefs, s)
	if err != nil {
		return &Result{Error: err}
	}

	// Enforce NOT NULL and CHECK constraints on the new values (SQLite checks
	// these per-row during UPDATE; a violation aborts the whole statement).
	// UPDATE OR IGNORE skips violating rows instead of aborting, so the
	// per-row check happens inside applyUpdateIgnore (below).
	if !strings.EqualFold(s.OnConflict, "IGNORE") {
		if res := e.checkUpdateConstraints(tableEntry, colDefs, changes); res.Error != nil {
			return res
		}
	}

	// Handle RETURNING clause — evaluate against updated rows before applying
	var returningRows [][]interface{}
	if s.HasReturning {
		for _, ch := range changes {
			row := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			values, err := e.evalReturningStrict(s.Returning, row, colDefs, tableEntry.Name)
			if err != nil {
				return &Result{Error: err}
			}
			returningRows = append(returningRows, values)
		}
	}

	// Enforce FOREIGN KEY constraints on the new values (PRAGMA foreign_keys).
	if e.foreignKeys {
		for _, ch := range changes {
			// Pass ch.rowID so a self-referential FK does not count the row's
			// own OLD key value as a valid parent for the NEW child value.
			if res := e.checkForeignKeyViolations(tableEntry, colDefs, ch.values, ch.rowID); res.Error != nil {
				return res
			}
		}
	}

	var result *Result
	if strings.EqualFold(s.OnConflict, "REPLACE") {
		// UPDATE OR REPLACE: apply each change incrementally, deleting
		// conflicting rows (with DELETE triggers) as we go.
		result = e.applyUpdateReplace(tableEntry, colDefs, changes)
	} else if strings.EqualFold(s.OnConflict, "IGNORE") {
		// UPDATE OR IGNORE: rows whose new values conflict with a
		// UNIQUE/PK constraint are skipped without error.
		result = e.applyUpdateIgnore(tableEntry, colDefs, changes)
	} else if e.hasTriggersForTable(tableEntry.Name) {
		// Plain UPDATE with triggers: SQLite fires BEFORE UPDATE triggers
		// per-row before the row is written. A BEFORE trigger may delete the
		// row being updated (or other rows); SQLite then skips writing rows
		// that no longer exist and checks UNIQUE/PK constraints against the
		// live table state. Process changes one row at a time.
		result = e.applyUpdateWithTriggers(tableEntry, colDefs, changes)
	} else {
		// Plain UPDATE: check UNIQUE/PK constraints on the new values
		// (SQLite errors on conflicts; there is no REPLACE resolution).
		if res := e.checkUpdateConflicts(tableEntry, colDefs, changes); res.Error != nil {
			return res
		}
		// Enforce FOREIGN KEY parent actions: children referencing the old
		// key values are restricted (error) or cascaded/updated.
		if e.foreignKeys {
			for _, ch := range changes {
				oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
				newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
				if res := e.fkParentUpdate(tableEntry, colDefs, oldRow, newRow, ch.rowID); res.Error != nil {
					return res
				}
			}
		}
		result = e.applyUpdateChanges(s.Table, tableEntry.RootPage, changes)
	}
	if result.Error != nil {
		return result
	}

	// Fire AFTER UPDATE triggers with the new and old row values. The
	// applyUpdateWithTriggers and applyUpdateIgnore paths fire AFTER
	// triggers themselves, so skip this block there.
	afterTriggersFired := false
	if strings.EqualFold(s.OnConflict, "REPLACE") {
		afterTriggersFired = false // applyUpdateReplace fires DELETE triggers, not UPDATE
	} else if e.hasTriggersForTable(tableEntry.Name) {
		afterTriggersFired = true // applyUpdateWithTriggers / applyUpdateIgnore fired them
	}
	if e.hasTriggersForTable(tableEntry.Name) && !afterTriggersFired {
		for _, ch := range changes {
			newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
			if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name, newRow, oldRow); trigResult.Error != nil {
				return trigResult
			}
		}
	}

	// Direct edits to sqlite_schema (PRAGMA writable_schema=ON) are schema
	// changes: re-read the schema btree on the next table lookup.
	if isSchemaTable(tableEntry.Name) {
		e.schema.InvalidateCache()
		e.tableCache = make(map[string]*cachedTableEntry)
	}

	// If RETURNING clause was present, return result rows instead of change count
	if s.HasReturning {
		columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
		return &Result{Columns: columns, Rows: returningRows}
	}

	return result
}

// execUpdateView routes UPDATE on a view through INSTEAD OF UPDATE triggers.
// The view's SELECT is executed (with the UPDATE's WHERE applied) to find
// matching rows; for each, the trigger fires with OLD.* and NEW.* values
// where NEW reflects the SET clause applied to the view's output columns.
func (e *Engine) execUpdateView(s *sql.UpdateStmt, viewEntry *schema.Entry) *Result {
	if !e.hasTriggersForTable(viewEntry.Name) {
		return &Result{Error: fmt.Errorf("cannot modify %s because it is a view", viewEntry.Name)}
	}
	// Qualified view column references (main.v5.b, v5.x) must resolve against
	// the view row during WHERE/SET evaluation.
	prevDML := e.currentDMLTable
	e.currentDMLTable = viewEntry.Name
	defer func() { e.currentDMLTable = prevDML }()
	viewResult := e.execSelectView(viewEntry)
	if viewResult.Error != nil {
		return viewResult
	}
	viewCols := viewResult.Columns
	// Apply the view's declared column list (CREATE VIEW v(a,b) AS ...) so
	// INSTEAD OF trigger OLD/NEW rows are keyed by the declared names even
	// when the SELECT produces expression columns without names.
	if decl := e.viewDeclaredColumns(viewEntry); len(decl) > 0 {
		viewCols = decl
	}
	if len(viewCols) == 0 {
		return &Result{}
	}
	// Convert each view row into a RowMap keyed by the view's column names.
	var changed int64
	colDefs := make([]sql.ColumnDef, len(viewCols))
	for i, c := range viewCols {
		colDefs[i] = sql.ColumnDef{Name: c}
	}
	for _, rowVals := range viewResult.Rows {
		oldRow := make(RowMap)
		for i, v := range rowVals {
			if i < len(viewCols) {
				oldRow[viewCols[i]] = v
			}
		}
		oldRow["rowid"] = nil
		// Apply the WHERE clause against the view row (joined with the UPDATE
		// FROM tables when present). The matched joined row supplies columns
		// for the SET expressions too.
		evalRow := oldRow
		if s.Where != nil {
			if s.From.Name != "" {
				joined, jerr := e.joinUpdateFromRows(s, oldRow)
				if jerr != nil {
					return &Result{Error: jerr}
				}
				matched := false
				for _, jrow := range joined {
					pass, err := e.evalBool(s.Where, jrow)
					if err == nil && pass {
						evalRow = jrow
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			} else {
				pass, err := e.evalBool(s.Where, oldRow)
				if err != nil || !pass {
					continue
				}
			}
		}
		// Build the NEW row by applying SET assignments to the old values.
		newRow := make(RowMap, len(oldRow))
		for k, v := range oldRow {
			newRow[k] = v
		}
		for _, a := range s.Assignments {
			v, err := e.evalExpr(a.Value, evalRow)
			if err != nil {
				return &Result{Error: fmt.Errorf("exec: failed to evaluate SET expression for %s: %w", a.Column, err)}
			}
			newRow[a.Column] = util.UnwrapColumnValue(v)
		}
		if res := e.fireTriggers(viewEntry.Name, "UPDATE", "BEFORE", newRow, oldRow); res != nil && res.Error != nil {
			return res
		}
		changed++
	}
	return &Result{Changes: changed}
}

func buildColumnIndex(colDefs []sql.ColumnDef) map[string]int {
	colIndex := make(map[string]int)
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}
	// rowid/_rowid_/oid map to the pseudo-rowid unless the table declares a
	// column with one of those names (which shadows the alias).
	if !rowHasRowIDColumn(colDefs) {
		colIndex["rowid"] = -1
	}
	return colIndex
}

// cdIndex returns the column index for name, or -1 if not found.
func cdIndex(colDefs []sql.ColumnDef, name string) int {
	for i, cd := range colDefs {
		if strings.EqualFold(cd.Name, name) {
			return i
		}
	}
	return -1
}

// originalColumnName returns the column name as spelled in the CREATE TABLE
// SQL (preserving original case for error messages), or the uppercased name
// if it cannot be found.
func (e *Engine) originalColumnName(createSQL, colName string) string {
	start := strings.IndexByte(createSQL, '(')
	end := strings.LastIndexByte(createSQL, ')')
	if start < 0 || end <= start {
		return colName
	}
	body := createSQL[start+1 : end]
	for _, part := range splitColumnDefs(body) {
		fields := strings.FieldsFunc(part, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\r'
		})
		if len(fields) == 0 {
			continue
		}
		first := strings.Trim(fields[0], "`\"[]")
		if strings.EqualFold(first, colName) {
			return first
		}
	}
	return colName
}

// splitColumnDefs splits a CREATE TABLE column list on top-level commas
// (ignoring commas inside parentheses such as CHECK(...) or DEFAULT(...)).
func splitColumnDefs(body string) []string {
	var parts []string
	depth := 0
	last := 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, body[last:])
	return parts
}

// primaryKeyColIndices returns the set of column indices that are PRIMARY KEY
// columns: column-level PRIMARY KEY declarations plus table-level PRIMARY KEY
// constraints (honoring integer column positions).
func (e *Engine) primaryKeyColIndices(tableName, createSQL string, colDefs []sql.ColumnDef) map[int]bool {
	idx := make(map[int]bool)
	colIndex := buildColumnIndex(colDefs)
	for i, cd := range colDefs {
		if cd.PrimaryKey {
			idx[i] = true
		}
	}
	for _, tc := range e.tableConstraints(tableName, createSQL) {
		if tc.Type != sql.ConstraintPrimaryKey {
			continue
		}
		for _, ic := range tc.Columns {
			if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
				idx[n-1] = true
				continue
			}
			if i, ok := colIndex[ic.Name]; ok {
				idx[i] = true
			}
		}
	}
	return idx
}

func (e *Engine) collectUpdateChanges(tableName string, rootPage uint32, colIndex map[string]int, colDefs []sql.ColumnDef, s *sql.UpdateStmt) ([]updateChange, error) {
	tree := e.tableBTreeForName(tableName, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, fmt.Errorf("exec: cursor error: %w", err)
	}

	// Set the current scan table so table-qualified column references
	// ("t1.a") in the WHERE clause resolve to the row map.
	prevScan := e.currentScanTable
	e.currentScanTable = tableName
	defer func() { e.currentScanTable = prevScan }()

	var changes []updateChange
	var rowMaps []RowMap
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}

		row := e.buildRowMap(rec, colDefs, cell.RowID)
		// UPDATE ... FROM <tables>: the target row joins with the FROM tables;
		// each join combination is a candidate (the WHERE and SET evaluate
		// against the combined row). A row is updated once per matching join
		// row (SQLite uses the first match's SET values for the row).
		if s.From.Name != "" {
			joined, err := e.joinUpdateFromRows(s, row)
			if err != nil {
				return nil, err
			}
			for _, jrow := range joined {
				match, err := e.rowMatchesWhere(s.Where, jrow)
				if err != nil {
					return nil, err
				}
				if match {
					ch, err := e.buildUpdateChange(cell, rec, colIndex, colDefs, s, jrow)
					if err != nil {
						return nil, err
					}
					changes = append(changes, *ch)
					rowMaps = append(rowMaps, jrow)
					break // one update per target row
				}
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
			continue
		}
		match, err := e.rowMatchesWhere(s.Where, row)
		if err != nil {
			return nil, err
		}
		if match {
			ch, err := e.buildUpdateChange(cell, rec, colIndex, colDefs, s, row)
			if err != nil {
				return nil, err
			}
			changes = append(changes, *ch)
			rowMaps = append(rowMaps, row)
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}

	// Apply UPDATE ... ORDER BY ... LIMIT (a SQLite extension): sort the
	// matching rows by the ORDER BY expressions, then keep only the LIMIT
	// window. Without ORDER BY the rows are processed in rowid order; LIMIT
	// alone applies to that natural order (SQLite: "LIMIT ... on UPDATE
	// statements ... are not supported unless the UPDATE is on a single
	// table").
	if len(s.OrderBy) > 0 {
		e.sortUpdateChanges(changes, rowMaps, s.OrderBy)
	}
	if s.Limit != nil {
		changes = e.limitUpdateChanges(changes, s)
	}
	return changes, nil
}

// sortUpdateChanges sorts updateChange entries by the ORDER BY expressions
// evaluated against each row's original values.
func (e *Engine) sortUpdateChanges(changes []updateChange, rowMaps []RowMap, orderBy []sql.OrderByTerm) {
	if len(changes) <= 1 {
		return
	}
	type pair struct {
		ch  updateChange
		row RowMap
	}
	pairs := make([]pair, len(changes))
	for i := range changes {
		pairs[i] = pair{ch: changes[i], row: rowMaps[i]}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		for _, ob := range orderBy {
			left, _ := e.evalExpr(ob.Expr, pairs[i].row)
			right, _ := e.evalExpr(ob.Expr, pairs[j].row)
			cmp := compareOrderByValues(left, right, ob)
			if cmp < 0 {
				return true
			} else if cmp > 0 {
				return false
			}
		}
		return false
	})
	for i := range pairs {
		changes[i] = pairs[i].ch
	}
}

// limitUpdateChanges applies UPDATE ... LIMIT n [OFFSET m] to the change list,
// keeping the first n entries after skipping m (SQLite semantics for UPDATE
// LIMIT: the first N rows matched by the scan order are updated).
func (e *Engine) limitUpdateChanges(changes []updateChange, s *sql.UpdateStmt) []updateChange {
	limit, err := e.evalConstInt(s.Limit)
	if err != nil || limit < 0 {
		return changes
	}
	offset := int64(0)
	if s.Offset != nil {
		if v, err := e.evalConstInt(s.Offset); err == nil && v > 0 {
			offset = v
		}
	}
	if offset >= int64(len(changes)) {
		return nil
	}
	end := offset + limit
	if end > int64(len(changes)) {
		end = int64(len(changes))
	}
	return changes[offset:end]
}

// evalConstInt evaluates an expression that must be a constant integer,
// returning -1 on error or non-integer values.
func (e *Engine) evalConstInt(expr sql.Expr) (int64, error) {
	v, err := e.evalExpr(expr, nil)
	if err != nil {
		return -1, err
	}
	v = util.UnwrapColumnValue(v)
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	}
	return -1, fmt.Errorf("exec: LIMIT expression is not an integer")
}

// joinUpdateFromRows reads the UPDATE ... FROM tables and returns combined
// row maps (target row + each FROM table row) for the target row. The
// combined row keys include both the target table's columns and the FROM
// tables' columns (qualified and unqualified).
func (e *Engine) joinUpdateFromRows(s *sql.UpdateStmt, targetRow RowMap) ([]RowMap, error) {
	// Read all rows of each FROM table.
	var tables [][]RowMap
	for _, ref := range append([]sql.TableRef{s.From}, s.FromJoins...) {
		if ref.Name == "" {
			continue
		}
		var entry *schema.Entry
		var err error
		// Resolve the FROM table in the modified table's context first (a
		// trigger body's unqualified references resolve in the trigger's
		// schema, e.g. an aux trigger's FROM mmm → aux.mmm), then fall back
		// to the normal temp/main-first resolution.
		if e.currentDMLCtx != nil && !strings.Contains(ref.Name, ".") {
			if ent, cerr := e.currentDMLCtx.Schema.FindTable(ref.Name); cerr == nil {
				entry = ent
			}
		}
		if entry == nil {
			entry, _, err = e.findTable(ref.Name)
		}
		if err != nil {
			return nil, err
		}
		colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
		var fromCtx *DatabaseContext
		if !strings.Contains(ref.Name, ".") && e.currentDMLCtx != nil {
			fromCtx = e.currentDMLCtx
		} else {
			_, fc, ferr := e.findTable(ref.Name)
			if ferr == nil {
				fromCtx = fc
			}
		}
		var tree *btree.BTree
		if fromCtx != nil && fromCtx.Pager != nil {
			tree = e.tableBTreePg(fromCtx.Pager, entry.Name, entry.RootPage, true)
		} else {
			tree = e.tableBTreeForName(entry.Name, entry.RootPage, true)
		}
		cursor, err := tree.OpenCursor()
		if err != nil {
			return nil, err
		}
		var rows []RowMap
		for {
			cell, rerr := cursor.ReadCell()
			if rerr != nil || cell == nil {
				break
			}
			rec, derr := storage.DecodeRecord(cell.Payload)
			if derr != nil || rec == nil {
				break
			}
			fm := e.buildRowMap(rec, colDefs, cell.RowID)
			// Add qualified keys (map.k, map.v) so SET/WHERE can reference
			// them by table name.
			alias := ref.As
			if alias == "" {
				alias = entry.Name
			}
			for k, v := range fm {
				if k == "rowid" {
					continue
				}
				fm[alias+"."+k] = v
			}
			rows = append(rows, fm)
			ok, nerr := cursor.Next()
			if nerr != nil || !ok {
				break
			}
		}
		tables = append(tables, rows)
	}
	// Cross-product of the target row with all FROM rows.
	result := []RowMap{targetRow}
	for _, tblRows := range tables {
		if len(tblRows) == 0 {
			return nil, nil
		}
		var next []RowMap
		for _, base := range result {
			for _, fr := range tblRows {
				merged := make(RowMap, len(base)+len(fr))
				for k, v := range base {
					merged[k] = v
				}
				for k, v := range fr {
					if _, ok := merged[k]; !ok {
						merged[k] = v
					}
				}
				next = append(next, merged)
			}
		}
		result = next
	}
	return result, nil
}

func (e *Engine) buildUpdateChange(cell *storage.Cell, rec *storage.Record, colIndex map[string]int, colDefs []sql.ColumnDef, s *sql.UpdateStmt, row Row) (*updateChange, error) {
	// Allocate values array large enough to hold all columns,
	// not just those present in the current record.
	maxIdx := len(rec.Values)
	for _, idx := range colIndex {
		if idx+1 > maxIdx {
			maxIdx = idx + 1
		}
	}
	values := make([]interface{}, maxIdx)
	copy(values, rec.Values)

	oldValues := make([]interface{}, len(rec.Values))
	copy(oldValues, rec.Values)

	// Detect an explicit rowid assignment (SET rowid = N / _rowid_ / oid):
	// the row is deleted at the old rowid and re-inserted at the new one.
	var newRowID *int64

	for _, a := range s.Assignments {
		idx, ok := colIndex[a.Column]
		if !ok {
			// Column not in schema - this happens when SQLite tests dynamically
			// add columns via PRAGMA writable_schema. Extend values array.
			idx = len(values)
			values = append(values, nil)
			colIndex[a.Column] = idx
		}
		if isRowIDName(a.Column) && !rowHasRowIDColumn(colDefs) {
			// SET rowid = N changes the cell's rowid, not a column value (only
			// when the table has no column named rowid/oid/_rowid_; a declared
			// rowid column is a normal column assignment).
			v, err := e.evalExpr(a.Value, row)
			if err != nil {
				return nil, fmt.Errorf("exec: failed to evaluate SET expression for %s: %w", a.Column, err)
			}
			nv := util.UnwrapColumnValue(v)
			if nv != nil {
				n, ok := toInt64(nv)
				if !ok {
					return nil, fmt.Errorf("datatype mismatch")
				}
				newRowID = &n
				// An INTEGER PRIMARY KEY column is the rowid: changing the
				// rowid changes the IPK column value too (and fires FK parent
				// actions on it).
				for i, cd := range colDefs {
					if isIPKRowidAliasCol(cd) &&
						i < len(values) {
						values[i] = n
					}
				}
			}
			continue
		}
		v, err := e.evalExpr(a.Value, row)
		if err != nil {
			return nil, fmt.Errorf("exec: failed to evaluate SET expression for %s: %w", a.Column, err)
		}
		// Unwrap ColumnValue to avoid storing internal wrapper types
		// in the record — only raw values should be serialized.
		v = util.UnwrapColumnValue(v)
		// Apply the column's type affinity (e.g. a REAL column stores 1 as 1.0).
		if idx >= 0 && idx < len(values) {
			if idx < len(colDefs) {
				v = util.ApplyColumnAffinity(v, colDefs[idx].Type)
			}
			values[idx] = v
		}
	}
	return &updateChange{cell.RowID, newRowID, values, oldValues}, nil
}

func (e *Engine) rowMatchesWhere(where sql.Expr, row Row) (bool, error) {
	if where == nil {
		return true, nil
	}
	match, err := e.evalBool(where, row)
	if err != nil {
		return false, err
	}
	return match, nil
}

func (e *Engine) applyUpdateChanges(tableName string, rootPage uint32, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}

	// Build a set of rowIDs to update
	type rowIDSet map[int64]bool
	toUpdate := make(rowIDSet, len(changes))
	for _, c := range changes {
		toUpdate[c.rowID] = true
	}

	tree := e.tableBTreeForName(tableName, rootPage, true)

	// Step 1: Delete all existing rows in a single pass
	_, delErr := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return toUpdate[cell.RowID]
	})
	if delErr != nil {
		return &Result{Error: delErr}
	}
	e.invalidateRowIDCache(rootPage)

	// Step 2: Insert all new rows
	for _, c := range changes {
		newRecord, err := storage.EncodeRecord(c.values)
		if err != nil {
			return &Result{Error: err}
		}
		writeRowID := c.rowID
		if c.newRowID != nil {
			writeRowID = *c.newRowID
		}
		newCell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   writeRowID,
			Payload: newRecord,
		}
		if err := tree.InsertCell(newCell); err != nil {
			return &Result{Error: err}
		}
		e.bumpRowIDCache(rootPage, writeRowID)
	}

	return &Result{Changes: int64(len(changes))}
}

// applyUpdateWithTriggers processes a plain UPDATE with triggers, matching
// SQLite's ordering: BEFORE UPDATE triggers fire per-row before the row is
// written, and a BEFORE trigger may delete the row being updated — SQLite then
// skips writing rows that no longer exist and checks UNIQUE/PK constraints
// against the live table state. AFTER UPDATE triggers fire phase-based after
// the writes (matching the engine's existing behavior for non-trigger rows).
// tableBTreeForDML builds the btree for a table being modified, using the
// modified table's context pager when known (a table in an ATTACHed database
// lives on the attached pager even when a same-named table exists in main).
func (e *Engine) tableBTreeForDML(tableEntry *schema.Entry, rootPage uint32) *btree.BTree {
	if e.currentDMLCtx != nil && e.currentDMLCtx.Pager != nil {
		return e.tableBTreePg(e.currentDMLCtx.Pager, tableEntry.Name, rootPage, true)
	}
	return e.tableBTree(tableEntry.Name, rootPage, true)
}

func (e *Engine) applyUpdateWithTriggers(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}
	colIndex := buildColumnIndex(colDefs)
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	rootPage := tableEntry.RootPage
	tableName := tableEntry.Name
	tree := e.tableBTreeForDML(tableEntry, rootPage)
	var changesMade int64
	var applied []updateChange

	for _, ch := range changes {
		newRowID := ch.rowID
		if ch.newRowID != nil {
			newRowID = *ch.newRowID
		}
		newRow := buildRowMapFromValues(ch.values, colDefs, newRowID)
		oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
		if trigResult := e.fireBeforeUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
			// RAISE(IGNORE) in a BEFORE UPDATE trigger skips this row's update
			// (no error); other rows continue.
			if trigResult.Error == errRaiseIgnore {
				continue
			}
			return trigResult
		}
		// If a BEFORE trigger deleted the row being updated, skip the write.
		stillExists, err := e.rowExists(tableName, rootPage, ch.rowID)
		if err != nil {
			return &Result{Error: err}
		}
		if !stillExists {
			continue
		}
		// Check UNIQUE/PK conflicts against the live table state. The row
		// being updated is not a conflict; every other live row (including
		// rows already written by earlier changes, which now hold their new
		// values) is.
		cursor, err := tree.OpenCursor()
		if err != nil {
			return &Result{Error: err}
		}
		conflict := false
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			if cell.RowID == ch.rowID {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			if e.valuesConflict(rec.Values, ch.values, colDefs, colIndex, uniqueCols, idxColsList) {
				conflict = true
				break
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
		if conflict {
			return &Result{Error: e.uniqueConflictError(tableName, colDefs, colIndex, nil, ch.values, uniqueCols, idxColsList)}
		}
		// Enforce FOREIGN KEY parent actions: children referencing the old key
		// values are restricted (error) or cascaded/updated. This runs for the
		// plain UPDATE path here too (SQLite fires FK actions regardless of
		// whether the table has triggers).
		if e.foreignKeys {
			oldFKRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
			newFKRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			if res := e.fkParentUpdate(tableEntry, colDefs, oldFKRow, newFKRow, ch.rowID); res.Error != nil {
				return res
			}
		}
		// Write the row: delete the old cell and insert the new record. SQLite
		// computes the NEW row values from the SET expressions BEFORE firing
		// BEFORE triggers, then writes only the SET columns after the triggers
		// run — so a BEFORE trigger that modifies a non-SET column (e.g.
		// UPDATE t SET b=1000 WHERE a=old.a) keeps its change, while the outer
		// SET columns get the pre-computed values. Re-read the current
		// (post-trigger) row first, overlay the SET columns' computed values,
		// then replace the row with the merged result.
		finalValues, err := e.mergeTriggerModifiedRow(tableName, rootPage, colDefs, ch)
		if err != nil {
			return &Result{Error: err}
		}
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == ch.rowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.invalidateRowIDCache(rootPage)
		newRecord, err := storage.EncodeRecord(finalValues)
		if err != nil {
			return &Result{Error: err}
		}
		writeRowID := ch.rowID
		if ch.newRowID != nil {
			writeRowID = *ch.newRowID
		}
		newCell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   writeRowID,
			Payload: newRecord,
		}
		if err := tree.InsertCell(newCell); err != nil {
			return &Result{Error: err}
		}
		e.bumpRowIDCache(rootPage, writeRowID)
		changesMade++
		applied = append(applied, ch)
	}

	// Fire AFTER UPDATE triggers phase-based (after all writes), matching the
	// engine's behavior for UPDATEs without this per-row path.
	for _, ch := range applied {
		newRowID := ch.rowID
		if ch.newRowID != nil {
			newRowID = *ch.newRowID
		}
		newRow := buildRowMapFromValues(ch.values, colDefs, newRowID)
		oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
		if trigResult := e.fireAfterUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
			return trigResult
		}
	}
	return &Result{Changes: changesMade}
}

// mergeTriggerModifiedRow reads the current (post-BEFORE-trigger) values of
// the row being updated and overlays the UPDATE statement's SET columns with
// their pre-computed values. SQLite computes NEW values from the SET
// expressions before BEFORE triggers run, then after the triggers the outer
// UPDATE writes only the SET columns — so a BEFORE trigger's changes to
// non-SET columns survive, while the SET columns get the computed values.
// Returns the full column-value slice to encode for the final row.
func (e *Engine) mergeTriggerModifiedRow(tableName string, rootPage uint32, colDefs []sql.ColumnDef, ch updateChange) ([]interface{}, error) {
	// Re-read the current row (post-trigger state) from the btree.
	var curTree *btree.BTree
	if e.currentDMLCtx != nil && e.currentDMLCtx.Pager != nil {
		curTree = e.tableBTreePg(e.currentDMLCtx.Pager, tableName, rootPage, true)
	} else {
		curTree = e.tableBTreeForName(tableName, rootPage, true)
	}
	cursor, err := curTree.OpenCursor()
	if err != nil {
		return nil, err
	}
	var current []interface{}
	found := false
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		if cell.RowID == ch.rowID {
			rec, derr := storage.DecodeRecord(cell.Payload)
			if derr != nil || rec == nil {
				break
			}
			current = rec.Values
			found = true
			break
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	if !found {
		// The row vanished during trigger execution (a BEFORE trigger deleted
		// it); the caller skips the write when rowExists reports false, so this
		// path should not normally be reached. Fall back to the computed NEW
		// values.
		return ch.values, nil
	}
	// Overlay the SET columns with the pre-computed values. Columns not in
	// the SET clause keep the post-trigger (possibly trigger-modified) values.
	if len(e.updateSetColumns) == 0 {
		return ch.values, nil
	}
	merged := make([]interface{}, len(current))
	copy(merged, current)
	for i, cd := range colDefs {
		for _, setCol := range e.updateSetColumns {
			if strings.EqualFold(cd.Name, setCol) && i < len(ch.values) {
				merged[i] = ch.values[i]
				break
			}
		}
	}
	return merged, nil
}

// applyUpdateIgnore implements UPDATE OR IGNORE: each change is applied only
// if its new values do not conflict with a UNIQUE/PK constraint or UNIQUE
// index. Conflicting rows are skipped without error. BEFORE/AFTER UPDATE
// triggers fire per row for the rows that are written.
func (e *Engine) applyUpdateIgnore(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}
	colIndex := buildColumnIndex(colDefs)
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	rootPage := tableEntry.RootPage
	tableName := tableEntry.Name
	tree := e.tableBTreeForDML(tableEntry, rootPage)
	hasTriggers := e.hasTriggersForTable(tableName)
	var changesMade int64

	for _, ch := range changes {
		if hasTriggers {
			newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
			if trigResult := e.fireBeforeUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
				// RAISE(IGNORE) in a BEFORE UPDATE trigger skips this row.
				if trigResult.Error == errRaiseIgnore {
					continue
				}
				return trigResult
			}
			stillExists, err := e.rowExists(tableName, rootPage, ch.rowID)
			if err != nil {
				return &Result{Error: err}
			}
			if !stillExists {
				continue
			}
		}
		// Check conflicts against the live table state; skip on conflict.
		cursor, err := tree.OpenCursor()
		if err != nil {
			return &Result{Error: err}
		}
		conflict := false
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			if cell.RowID == ch.rowID {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			if e.valuesConflict(rec.Values, ch.values, colDefs, colIndex, uniqueCols, idxColsList) {
				conflict = true
				break
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
		if conflict {
			continue
		}
		// OR IGNORE also skips rows whose NEW values violate a NOT NULL or
		// CHECK constraint (SQLite's OR IGNORE applies to every constraint
		// type, so a violating row is silently left unchanged).
		if res := e.checkUpdateConstraints(tableEntry, colDefs, []updateChange{ch}); res.Error != nil {
			continue
		}
		// OR IGNORE also skips rows that would violate a FOREIGN KEY
		// constraint (child direction: the new child value has no parent;
		// parent direction: a child references the old key value).
		if e.foreignKeys {
			if res := e.checkForeignKeyViolations(tableEntry, colDefs, ch.values, ch.rowID); res.Error != nil {
				continue
			}
			oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
			newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			if res := e.fkParentUpdate(tableEntry, colDefs, oldRow, newRow, ch.rowID); res.Error != nil {
				continue
			}
		}
		// Write the row.
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == ch.rowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.invalidateRowIDCache(rootPage)
		newRecord, err := storage.EncodeRecord(ch.values)
		if err != nil {
			return &Result{Error: err}
		}
		newCell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   ch.rowID,
			Payload: newRecord,
		}
		if err := tree.InsertCell(newCell); err != nil {
			return &Result{Error: err}
		}
		e.bumpRowIDCache(rootPage, ch.rowID)
		changesMade++
		if hasTriggers {
			newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
			if trigResult := e.fireAfterUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
				return trigResult
			}
		}
	}
	return &Result{Changes: changesMade}
}

// rowExists reports whether a table contains a cell with the given rowID.
func (e *Engine) rowExists(tableName string, rootPage uint32, rowID int64) (bool, error) {
	// The table may share its short name with a table in another schema
	// (main.t1 vs aux.t1); use the modified table's context pager when known
	// AND the name carries a schema prefix (an unqualified name resolves
	// temp/main-first, which the fallback handles correctly).
	var pg *pager.Pager
	if e.currentDMLCtx != nil && e.currentDMLCtx.Pager != nil && e.currentDMLCtx != e.mainDB {
		pg = e.currentDMLCtx.Pager
	} else {
		pg = e.tablePager(tableName)
	}
	tree := e.tableBTreePg(pg, tableName, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false, err
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			return false, nil
		}
		if cell.RowID == rowID {
			return true, nil
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			return false, nil
		}
	}
}

// resolveUpdateConflicts implements UPDATE OR REPLACE: for each row being
// updated, delete other rows whose values conflict with the new values on a
// UNIQUE/PRIMARY KEY column or UNIQUE index, firing BEFORE/AFTER DELETE
// triggers with the deleted row's OLD values.
func (e *Engine) resolveUpdateConflicts(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	colIndex := buildColumnIndex(colDefs)
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)

	// Scan the table, collecting rows that conflict with any change.
	type conflictInfo struct {
		values []interface{}
	}
	conflicts := make(map[int64]conflictInfo)
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		for _, c := range changes {
			if cell.RowID == c.rowID {
				continue // the row being updated is not a conflict
			}
			conflict := false
			for _, idx := range uniqueCols {
				if idx < len(rec.Values) && idx < len(c.values) {
					if rec.Values[idx] == nil || c.values[idx] == nil {
						continue
					}
					if util.CompareValues(rec.Values[idx], c.values[idx]) == 0 {
						conflict = true
						break
					}
				}
			}
			if !conflict {
				for _, def := range idxColsList {
					match := true
					// The new row must satisfy the partial predicate too.
					nrow := buildRowMapFromValues(c.values, colDefs, c.rowID)
					if inIndex, _ := e.evalIndexWhere(def.Where, nrow); !inIndex {
						continue
					}
					orow := buildRowMapFromValues(rec.Values, colDefs, cell.RowID)
					if inIndex, _ := e.evalIndexWhere(def.Where, orow); !inIndex {
						continue
					}
					for _, cn := range def.Cols {
						rkv, rok := e.indexKeyValue(cn, colDefs, colIndex, rec.Values, orow)
						ckv, cok := e.indexKeyValue(cn, colDefs, colIndex, c.values, nrow)
						if !rok || !cok || util.CompareValues(rkv, ckv) != 0 {
							match = false
							break
						}
					}
					if match {
						conflict = true
						break
					}
				}
			}
			if conflict {
				conflicts[cell.RowID] = conflictInfo{values: rec.Values}
				break
			}
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	hasTriggers := e.hasTriggersForTable(tableEntry.Name)
	for rowID, ci := range conflicts {
		oldRow := buildRowMapFromValues(ci.values, colDefs, rowID)
		if hasTriggers {
			if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
				if trigResult.Error == errRaiseIgnore {
					continue
				}
				return trigResult
			}
		}
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == rowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.invalidateRowIDCache(tableEntry.RootPage)
		if hasTriggers {
			if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
				return trigResult
			}
		}
	}
	return &Result{}
}

// applyUpdateReplace implements UPDATE OR REPLACE by processing each change
// incrementally (matching SQLite's row-by-row semantics): for each row, delete
// other rows whose values conflict with the row's NEW values on a UNIQUE/
// PRIMARY KEY column or UNIQUE index (firing BEFORE/AFTER DELETE triggers),
// then delete the row itself and insert its new version. Processing in order
// lets later changes see earlier applied rows, so conflicts between updated
// rows are resolved too (e.g. UPDATE OR REPLACE SET x=1 on two NULL rows).
func (e *Engine) applyUpdateReplace(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	colIndex := buildColumnIndex(colDefs)
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	hasTriggers := e.hasTriggersForTable(tableEntry.Name)
	changesMade := int64(0)
	// Snapshot so a FOREIGN KEY violation mid-statement rolls back any
	// conflict rows already deleted.
	snap := e.pager.Snapshot()

	for _, c := range changes {
		type conflictInfo struct {
			rowID  int64
			values []interface{}
		}
		var conflicts []conflictInfo
		cursor, err := tree.OpenCursor()
		if err != nil {
			return &Result{Error: err}
		}
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			if cell.RowID == c.rowID {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			conflict := false
			for _, idx := range uniqueCols {
				if idx < len(rec.Values) && idx < len(c.values) {
					if rec.Values[idx] == nil || c.values[idx] == nil {
						continue
					}
					if util.CompareValues(rec.Values[idx], c.values[idx]) == 0 {
						conflict = true
						break
					}
				}
			}
			if !conflict {
				for _, def := range idxColsList {
					nrow := buildRowMapFromValues(c.values, colDefs, c.rowID)
					if inIndex, _ := e.evalIndexWhere(def.Where, nrow); !inIndex {
						continue
					}
					orow := buildRowMapFromValues(rec.Values, colDefs, cell.RowID)
					if inIndex, _ := e.evalIndexWhere(def.Where, orow); !inIndex {
						continue
					}
					match := true
					for _, cn := range def.Cols {
						rkv, rok := e.indexKeyValue(cn, colDefs, colIndex, rec.Values, orow)
						ckv, cok := e.indexKeyValue(cn, colDefs, colIndex, c.values, nrow)
						if !rok || !cok || util.CompareValues(rkv, ckv) != 0 {
							match = false
							break
						}
					}
					if match {
						conflict = true
						break
					}
				}
			}
			if conflict {
				conflicts = append(conflicts, conflictInfo{cell.RowID, rec.Values})
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}

		for _, cf := range conflicts {
			oldRow := buildRowMapFromValues(cf.values, colDefs, cf.rowID)
			if hasTriggers {
				if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
					if trigResult.Error == errRaiseIgnore {
						continue
					}
					return trigResult
				}
			}
			if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
				return cell.RowID == cf.rowID
			}); err != nil {
				return &Result{Error: err}
			}
			e.invalidateRowIDCache(tableEntry.RootPage)
			if hasTriggers {
				if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
					return trigResult
				}
			}
		}

		// UPDATE OR REPLACE still enforces FOREIGN KEY constraints: a new
		// value that orphans a child (or a child value with no parent) is an
		// error, matching SQLite.
		if e.foreignKeys {
			if res := e.checkForeignKeyViolations(tableEntry, colDefs, c.values, c.rowID); res.Error != nil {
				e.restorePager(e.pager, snap)
				e.invalidateRowIDCache(tableEntry.RootPage)
				return res
			}
			oldRow := buildRowMapFromValues(c.oldValues, colDefs, c.rowID)
			newRow := buildRowMapFromValues(c.values, colDefs, c.rowID)
			if res := e.fkParentUpdate(tableEntry, colDefs, oldRow, newRow, c.rowID); res.Error != nil {
				e.restorePager(e.pager, snap)
				e.invalidateRowIDCache(tableEntry.RootPage)
				return res
			}
		}

		// Delete the row being updated (no DELETE trigger: this is the UPDATE
		// itself, not a conflict-replacement) and insert its new version.
		// If a conflict-resolution delete's trigger removed the row being
		// updated too (e.g. a recursive DELETE FROM t0 inside an AFTER
		// DELETE trigger), SQLite aborts the statement with the generic
		// "constraint failed" error and rolls it back.
		if !e.rowIDExists(tableEntry.Name, tableEntry.RootPage, c.rowID) {
			// The row being updated was deleted by a trigger fired during
			// this row's conflict resolution; abort like SQLite and roll
			// back the whole statement (conflict rows already deleted).
			e.restorePager(e.pager, snap)
			e.invalidateRowIDCache(tableEntry.RootPage)
			return &Result{Error: fmt.Errorf("constraint failed")}
		}
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == c.rowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.invalidateRowIDCache(tableEntry.RootPage)
		newRecord, err := storage.EncodeRecord(c.values)
		if err != nil {
			return &Result{Error: err}
		}
		newCell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   c.rowID,
			Payload: newRecord,
		}
		if err := tree.InsertCell(newCell); err != nil {
			return &Result{Error: err}
		}
		e.bumpRowIDCache(tableEntry.RootPage, c.rowID)
		changesMade++
	}
	return &Result{Changes: changesMade}
}

// valuesConflict reports whether two value sets conflict on any UNIQUE/PRIMARY
// KEY column or UNIQUE index (partial-index predicates evaluated).
func (e *Engine) valuesConflict(a, b []interface{}, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef) bool {
	for _, idx := range uniqueCols {
		if idx < len(a) && idx < len(b) {
			if a[idx] == nil || b[idx] == nil {
				continue
			}
			if util.CompareValues(a[idx], b[idx]) == 0 {
				return true
			}
		}
	}
	for _, def := range idxColsList {
		arow := buildRowMapFromValues(a, colDefs, 0)
		if inIndex, _ := e.evalIndexWhere(def.Where, arow); !inIndex {
			continue
		}
		brow := buildRowMapFromValues(b, colDefs, 0)
		if inIndex, _ := e.evalIndexWhere(def.Where, brow); !inIndex {
			continue
		}
		match := true
		for _, cn := range def.Cols {
			akv, aok := e.indexKeyValue(cn, colDefs, colIndex, a, arow)
			bkv, bok := e.indexKeyValue(cn, colDefs, colIndex, b, brow)
			if !aok || !bok || util.CompareValues(akv, bkv) != 0 {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// checkUpdateConflicts validates that an UPDATE's new values do not violate
// UNIQUE/PRIMARY KEY constraints, matching SQLite's one-pass semantics: rows
// are processed in rowid order, and each row's new values are checked against
// the CURRENT state of every other row — rows processed earlier hold their NEW
// values, rows not yet processed hold their ORIGINAL values.
func (e *Engine) checkUpdateConflicts(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	colIndex := buildColumnIndex(colDefs)
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	if len(uniqueCols) == 0 && len(idxColsList) == 0 {
		return &Result{}
	}

	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	for i := range changes {
		c := changes[i]
		// Check the new values against every other row's CURRENT value: for
		// changes processed before this one (j < i) use their NEW values (they
		// have already been written); for all other rows (unchanged rows and
		// changes processed later) use the row's ORIGINAL value from the table
		// (they have not been written yet).
		for j := 0; j < i; j++ {
			if e.valuesConflict(changes[j].values, c.values, colDefs, colIndex, uniqueCols, idxColsList) {
				return &Result{Error: e.uniqueConflictError(tableEntry.Name, colDefs, colIndex, changes[j].values, c.values, uniqueCols, idxColsList)}
			}
		}
		cursor, err := tree.OpenCursor()
		if err != nil {
			return &Result{Error: err}
		}
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			if cell.RowID == c.rowID {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			// Skip rows that were already processed (j < i): their current
			// value is the NEW value (checked pairwise above), and the table
			// still holds their ORIGINAL value.
			skip := false
			for j := 0; j < i; j++ {
				if changes[j].rowID == cell.RowID {
					skip = true
					break
				}
			}
			if skip {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			// A later change (j > i) still holds its ORIGINAL values (not yet
			// written), which the table scan sees.
			if e.valuesConflict(rec.Values, c.values, colDefs, colIndex, uniqueCols, idxColsList) {
				return &Result{Error: e.uniqueConflictError(tableEntry.Name, colDefs, colIndex, rec.Values, c.values, uniqueCols, idxColsList)}
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
	}
	return &Result{}
}

// checkUpdateConstraints validates NOT NULL and CHECK constraints on the new
// values of an UPDATE. SQLite checks these per-row during UPDATE (before
// applying); a violation aborts the whole statement with no partial writes.
// UNIQUE/PRIMARY KEY conflicts are handled separately by checkUpdateConflicts.
func (e *Engine) checkUpdateConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	hasNotNullOrCheck := false
	for _, cd := range colDefs {
		if cd.NotNull || cd.Check != nil {
			hasNotNullOrCheck = true
			break
		}
	}
	if !hasNotNullOrCheck && len(e.tableConstraints(tableEntry.Name, tableEntry.SQL)) == 0 {
		return &Result{}
	}
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	var pkCols map[int]bool
	if withoutRowid {
		pkCols = e.primaryKeyColIndices(tableEntry.Name, tableEntry.SQL, colDefs)
	}
	// Set the DML table context so table-qualified column references inside
	// CHECK expressions (e.g. CHECK(Table0.Col0 NOT NULL)) resolve against
	// the row's unqualified column keys.
	prevDML := e.currentDMLTable
	e.currentDMLTable = tableEntry.Name
	defer func() { e.currentDMLTable = prevDML }()
	for _, ch := range changes {
		row := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
		for _, cd := range colDefs {
			val := columnValue(ch.values, colDefs, cd.Name)
			// NOT NULL constraint — skip for INTEGER PRIMARY KEY columns of
			// rowid tables (their value derives from the rowid, which is
			// unchanged by an UPDATE).
			pkAutoRowID := isIPKRowidAliasCol(cd) && !withoutRowid
			implicitNotNull := cd.NotNull || (withoutRowid && pkCols[cdIndex(colDefs, cd.Name)])
			if implicitNotNull && val == nil && !pkAutoRowID {
				return &Result{Error: fmt.Errorf("NOT NULL constraint failed: %s.%s", tableEntry.Name, e.originalColumnName(tableEntry.SQL, cd.Name))}
			}
			// CHECK constraint: only fails when the result is explicitly false.
			// PRAGMA ignore_check_constraints=ON skips enforcement.
			if cd.Check != nil && !e.ignoreCheckConstraints {
				checkVal, err := e.evalExpr(cd.Check, row)
				if err == nil && checkVal != nil && !toBool(checkVal) {
					checkText := e.checkConstraintText(tableEntry.SQL, cd.Name, cd.Check)
					return &Result{Error: fmt.Errorf("CHECK constraint failed: %s", checkText)}
				}
			}
		}
		// Table-level CHECK constraints.
		tcs := e.tableConstraints(tableEntry.Name, tableEntry.SQL)
		for ti, tc := range tcs {
			if tc.Type != sql.ConstraintCheck || tc.Expr == nil {
				continue
			}
			if e.ignoreCheckConstraints {
				continue
			}
			checkVal, err := e.evalExpr(tc.Expr, row)
			if err == nil && checkVal != nil && !toBool(checkVal) {
				name := tc.Name
				if name == "" {
					name = e.tableCheckConstraintText(tableEntry.SQL, ti, tcs)
				}
				return &Result{Error: fmt.Errorf("CHECK constraint failed: %s", name)}
			}
		}
	}
	return &Result{}
}

// uniqueConflictError builds a SQLite-style UNIQUE constraint error for the
// first conflicting column.
func (e *Engine) uniqueConflictError(tableName string, colDefs []sql.ColumnDef, colIndex map[string]int, a, b []interface{}, uniqueCols []int, idxColsList []uniqueIndexDef) error {
	for _, idx := range uniqueCols {
		if idx < len(a) && idx < len(b) && a[idx] != nil && b[idx] != nil && util.CompareValues(a[idx], b[idx]) == 0 {
			return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableName, colDefs[idx].Name)
		}
	}
	for _, def := range idxColsList {
		if e.valuesConflict(a, b, colDefs, colIndex, nil, []uniqueIndexDef{def}) {
			parts := make([]string, len(def.Cols))
			for i, cn := range def.Cols {
				parts[i] = tableName + "." + cn
			}
			return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(parts, ", "))
		}
	}
	return fmt.Errorf("UNIQUE constraint failed: %s", tableName)
}
