// Package exec implements query execution.
package exec

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- INSERT ---

func (e *Engine) execInsert(s *sql.InsertStmt) (ret *Result) {
	if err := e.authorize(auth.ActionInsert, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	tableEntry, dbCtx, err := e.findTable(s.Table)
	if err != nil {
		// Not a table — fall back to INSTEAD-OF-trigger view insert support.
		viewEntry, _, viewErr := e.findView(s.Table)
		if viewErr != nil {
			return &Result{Error: err}
		}
		return e.execInsertView(s, viewEntry)
	}

	// Protect system and pragma virtual tables from modification.
	if e.isNonModifiableTable(tableEntry) {
		return &Result{Error: fmt.Errorf("table %s may not be modified", tableEntry.Name)}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	if s.HasReturning {
		if err := e.validateReturning(s.Returning, colDefs, tableEntry.Name); err != nil {
			return &Result{Error: err}
		}
	}

	// Virtual tables without module-backed storage (rtree, echo, dbstat, ...)
	// accept INSERT as a no-op success; RETURNING projects NULLs for every
	// column. FTS tables are handled by their dedicated paths.
	if e.isStoragelessVirtualTable(tableEntry) {
		if s.HasReturning {
			row := make(RowMap, len(colDefs)+1)
			for _, cd := range colDefs {
				row[cd.Name] = nil
			}
			row["rowid"] = nil
			vals, err := e.evalReturningStrict(s.Returning, row, colDefs, tableEntry.Name)
			if err != nil {
				return &Result{Error: err}
			}
			columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
			return &Result{Columns: columns, Rows: [][]interface{}{vals}}
		}
		return &Result{Changes: 1, LastInsertRowID: 1}
	}

	if s.Select != nil {
		return e.execInsertSelect(tableEntry, colDefs, s)
	}
	if len(s.Values) == 0 {
		return e.execInsertDefault(tableEntry, colDefs, s)
	}

	// REPLACE deletes rows and may fire triggers before inserting; if anything
	// fails the whole statement must be rolled back (SQLite statement journal).
	if s.IsReplace {
		snap := dbCtx.Pager.Snapshot()
		defer func() {
			if ret != nil && ret.Error != nil {
				dbCtx.Pager.Restore(snap)
				// Rows whose rowids were computed for the aborted statement
				// are gone; the cached rowid counter must not survive.
				e.nextRowIDCache = make(map[uint32]int64)
			}
		}()
	}

	var totalChanges int64
	var lastRowID int64
	var returningRows [][]interface{}
	for _, tuple := range s.Values {
		values, evalErr := e.evalTuple(tuple, s.Columns, colDefs)
		if evalErr != nil {
			return &Result{Error: evalErr}
		}

		// Handle REPLACE (INSERT OR REPLACE): delete conflicting rows before
		// inserting. The new row's rowid is computed BEFORE the deletes
		// (SQLite keeps it through the REPLACE retry, so a trigger may grab it).
		var replaceRowID int64
		var haveReplaceRowID bool
		if s.IsReplace {
			replaceRowID = e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage)
			haveReplaceRowID = true
			if res := e.replaceDeleteConflicts(dbCtx.Pager, tableEntry, colDefs, values); res.Error != nil {
				return res
			}
		}

		// A trigger may have inserted a row with our rowid during the
		// REPLACE's delete; report it as a rowid UNIQUE conflict.
		if haveReplaceRowID {
			if e.rowIDExists(tableEntry.Name, tableEntry.RootPage, replaceRowID) {
				return &Result{Error: e.rowIDConflictError(tableEntry, colDefs)}
			}
		}

		// Check for ON CONFLICT (UPSERT)
		var writtenRow []interface{}
		if s.OnConflict != nil {
			res := e.execInsertOnConflict(dbCtx.Pager, tableEntry, colDefs, values, s)
			if res.Error != nil {
				return res
			}
			totalChanges += res.Changes
			if res.LastInsertRowID > 0 {
				lastRowID = res.LastInsertRowID
			}
			// For a DO UPDATE conflict the written row differs from the
			// attempted values; DO NOTHING skips the row entirely (Row nil).
			writtenRow = res.Row
		} else {
			var fixed *int64
			if haveReplaceRowID {
				fixed = &replaceRowID
			}
			res := e.insertRow(dbCtx.Pager, tableEntry, colDefs, values, fixed)
			if res.Error != nil {
				// INSERT OR IGNORE: silently skip UNIQUE conflicts.
				if s.OrIgnore && isUniqueConflictError(res.Error) {
					res = &Result{Changes: 0}
				} else {
					return res
				}
			}
			totalChanges += res.Changes
			lastRowID = res.LastInsertRowID
			// insertRow mutates values in place; it holds the written row.
			writtenRow = values
		}

		// Handle RETURNING clause — evaluate RETURNING expression against the
		// row that was actually written (upsert DO UPDATE writes a different
		// row than the attempted VALUES; DO NOTHING writes nothing).
		if s.HasReturning && writtenRow != nil {
			row := buildRowMapFromValues(writtenRow, colDefs, lastRowID)
			rowValues, err := e.evalReturningStrict(s.Returning, row, colDefs, tableEntry.Name)
			if err != nil {
				return &Result{Error: err}
			}
			returningRows = append(returningRows, rowValues)
		}
	}

	// If RETURNING clause was present, return result rows instead of change count
	if s.HasReturning {
		columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
		return &Result{Columns: columns, Rows: returningRows}
	}
	return &Result{Changes: totalChanges}
}

func (e *Engine) insertRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, fixedRowID *int64) *Result {
	// Route FTS virtual table inserts directly to the FTS table
	if ftsTable, ok := e.ftsTables[tableEntry.Name]; ok {
		nextRowID := ftsTable.Insert(values)
		e.lastRowID = nextRowID
		return &Result{Changes: 1, LastInsertRowID: nextRowID}
	}

	// Validate constraints before inserting
	// Determine rowID: if an INTEGER PRIMARY KEY column has an explicit non-nil
	// value, use that value as the rowid (the column IS the rowid). Otherwise
	// auto-assign the next available rowid. REPLACE passes a rowid computed
	// before its conflict deletes (SQLite keeps it through the retry).
	nextRowID := e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage)
	if fixedRowID != nil {
		nextRowID = *fixedRowID
	}
	e.lastRowID = nextRowID

	// If INTEGER PRIMARY KEY column value is nil, set it to the auto-assigned rowid.
	// SQLite behavior: inserting NULL into an INTEGER PRIMARY KEY column causes
	// the column to contain the auto-generated rowid.
	for i, cd := range colDefs {
		if cd.PrimaryKey && i < len(values) && values[i] == nil {
			values[i] = nextRowID
			break
		}
	}

	if err := e.checkConstraints(tableEntry, colDefs, values, nextRowID); err != nil {
		// Column-level ON CONFLICT IGNORE: silence UNIQUE constraint violations
		if isIgnoreableConflict(err, colDefs) {
			return &Result{Changes: 0}
		}
		return &Result{Error: err}
	}

	// Apply type affinity to each value based on column type.
	// Apply in-place to avoid allocating a separate affValues slice.
	for i, v := range values {
		if i < len(colDefs) {
			values[i] = util.ApplyColumnAffinity(v, colDefs[i].Type)
		}
	}

	// Compute any generated columns (b AS(expr)) that were not explicitly set.
	e.computeGeneratedValues(colDefs, values)

	// Enforce FOREIGN KEY constraints (only when PRAGMA foreign_keys is ON).
	if res := e.checkForeignKeyViolations(tableEntry, colDefs, values); res.Error != nil {
		return res
	}

	// Fire BEFORE INSERT triggers — the row is not in the table yet, so
	// only build the row map when triggers exist for this table.
	if e.hasTriggersForTable(tableEntry.Name) {
		newRow := make(RowMap)
		for i, v := range values {
			if i < len(colDefs) {
				newRow[colDefs[i].Name] = v
			}
		}
		if trigResult := e.fireBeforeInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
			return trigResult
		}
	}

	record, err := storage.EncodeRecord(values)
	if err != nil {
		return &Result{Error: err}
	}

	tree := e.tableBTreePg(pg, tableEntry.Name, tableEntry.RootPage, true)
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   nextRowID,
		Payload: record,
	}
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}
	// Track root page changes (after splits)
	if tree.RootPage() != e.rootPage(tableEntry.Name, tableEntry.RootPage) {
		e.updateRootPage(tableEntry.Name, tree.RootPage())
	}
	e.bumpRowIDCache(tableEntry.RootPage, nextRowID)

	// Fire AFTER INSERT triggers — but only if triggers exist for this table.
	// The trigger check is cheap (cached schema lookup) but building the RowMap
	// and calling fireTriggers is wasteful when no triggers are registered.
	if e.hasTriggersForTable(tableEntry.Name) {
		newRow := make(RowMap)
		for i, v := range values {
			if i < len(colDefs) {
				newRow[colDefs[i].Name] = v
			}
		}
		if trigResult := e.fireAfterInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
			return trigResult
		}
	}
	return &Result{Changes: 1, LastInsertRowID: nextRowID}
}

// hasTriggersForTable returns true if any AFTER INSERT/UPDATE/DELETE triggers
// exist for the given table across all databases. This is a fast check to avoid
// building trigger row maps when no triggers are registered.
func (e *Engine) hasTriggersForTable(tableName string) bool {
	// Check cache first
	if has, ok := e.hasTriggersCache[tableName]; ok {
		return has
	}
	has := false
	for _, ctx := range e.databases {
		triggers, err := ctx.Schema.FindTriggersForTable(tableName)
		if err == nil && len(triggers) > 0 {
			has = true
			break
		}
	}
	e.hasTriggersCache[tableName] = has
	return has
}

// checkConstraints validates NOT NULL, CHECK, UNIQUE, and PRIMARY KEY
// constraints for a row being inserted.
func (e *Engine) checkConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, rowID int64) error {
	// Fast path: if there are no constraints at all, skip allocation entirely
	hasConstraints := false
	for _, cd := range colDefs {
		if cd.NotNull || cd.Check != nil || cd.PrimaryKey {
			hasConstraints = true
			break
		}
	}

	// Check for UNIQUE constraints (defined separately from column defs)
	// by looking for indexes with unique=true for this table
	if !hasConstraints {
		// Quick scan for any constraint — including UNIQUE via unique indices
		for _, cd := range colDefs {
			if cd.Unique {
				hasConstraints = true
				break
			}
		}
	}

	// UNIQUE indexes (CREATE UNIQUE INDEX) also impose constraints even when
	// no column-level constraint exists on the table.
	if !hasConstraints && len(e.uniqueIndexColumns(tableEntry.Name)) > 0 {
		hasConstraints = true
	}

	// Table-level constraints (CONSTRAINT c1 CHECK(...) etc.) also impose
	// constraints even when no column-level constraint exists.
	if !hasConstraints && len(e.tableConstraints(tableEntry.Name, tableEntry.SQL)) > 0 {
		hasConstraints = true
	}

	if !hasConstraints {
		return nil
	}

	row := buildRowMapFromValues(values, colDefs, rowID)

	for _, cd := range colDefs {
		val := columnValue(values, colDefs, cd.Name)

		// NOT NULL constraint — skip for INTEGER PRIMARY KEY columns
		// since they get their value from the auto-generated rowid.
		if cd.NotNull && val == nil && !(cd.PrimaryKey && cd.Type == "INTEGER") {
			return fmt.Errorf("NOT NULL constraint failed: %s.%s", tableEntry.Name, cd.Name)
		}

		// CHECK constraint: only fails when result is explicitly false.
		// NULL (unknown) and true both pass.
		if cd.Check != nil {
			checkVal, err := e.evalExpr(cd.Check, row)
			if err == nil && checkVal != nil && !toBool(checkVal) {
				return fmt.Errorf("CHECK constraint failed: %s", sql.ExprString(cd.Check))
			}
		}
	}

	// UNIQUE and PRIMARY KEY uniqueness check
	if err := e.checkUniqueConstraints(tableEntry, colDefs, values); err != nil {
		return err
	}

	// Table-level CHECK constraints (CONSTRAINT c1 CHECK(...)).
	for _, tc := range e.tableConstraints(tableEntry.Name, tableEntry.SQL) {
		if tc.Type != sql.ConstraintCheck || tc.Expr == nil {
			continue
		}
		checkVal, err := e.evalExpr(tc.Expr, row)
		if err == nil && checkVal != nil && !toBool(checkVal) {
			name := tc.Name
			if name == "" {
				name = sql.ExprString(tc.Expr)
			}
			return fmt.Errorf("CHECK constraint failed: %s", name)
		}
	}

	return nil
}

// checkUniqueConstraints validates UNIQUE and PRIMARY KEY constraints by scanning
// for existing rows with the same values on UNIQUE or PRIMARY KEY columns.
func (e *Engine) checkUniqueConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) error {
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := gatherUniqueColIndices(colDefs, colIndex, values)
	for i, cd := range colDefs {
		if cd.PrimaryKey && !contains(uniqueCols, i) {
			if i < len(values) && values[i] != nil {
				uniqueCols = append(uniqueCols, i)
			}
		}
	}
	if len(uniqueCols) > 0 {
		_, _, conflictIdx, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)
		if found {
			if conflictIdx >= 0 && conflictIdx < len(colDefs) {
				return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableEntry.Name, colDefs[conflictIdx].Name)
			}
			return fmt.Errorf("UNIQUE constraint failed: %s", tableEntry.Name)
		}
	}

	// Check UNIQUE indexes (CREATE UNIQUE INDEX ... ON t(c1, c2)).
	for _, def := range e.uniqueIndexColumns(tableEntry.Name) {
		if err := e.checkUniqueIndex(tableEntry, colDefs, values, def); err != nil {
			return err
		}
	}
	return nil
}

// uniqueIndexDef describes a UNIQUE index on a table: the indexed columns and
// the optional partial-index WHERE clause (empty for a full index).
type uniqueIndexDef struct {
	Cols  []string
	Where string // partial index predicate ("" for full indexes)
}

// uniqueIndexColsRe matches "CREATE UNIQUE INDEX name ON tbl(col1, col2 ...)".
var uniqueIndexColsRe = regexp.MustCompile(`(?is)^\s*CREATE\s+UNIQUE\s+INDEX\b.*?\bON\b\s+[^\s(]+\((.*?)\)`)

// indexWhereRe captures the partial-index predicate after the column list.
var indexWhereRe = regexp.MustCompile(`(?is)\)\s*WHERE\s+(.+)$`)

// uniqueIndexColumns returns the UNIQUE indexes defined on the given table
// (cached per table name).
func (e *Engine) uniqueIndexColumns(tableName string) []uniqueIndexDef {
	if e.uniqueIdxCache == nil {
		e.uniqueIdxCache = make(map[string][]uniqueIndexDef)
	}
	if defs, ok := e.uniqueIdxCache[tableName]; ok {
		return defs
	}
	var result []uniqueIndexDef
	for _, ctx := range e.databases {
		entries, err := ctx.Schema.GetEntries(schema.TypeIndex)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !strings.EqualFold(ent.TblName, tableName) {
				continue
			}
			m := uniqueIndexColsRe.FindStringSubmatch(ent.SQL)
			if m == nil {
				continue
			}
			var cols []string
			for _, part := range strings.Split(m[1], ",") {
				name := strings.TrimSpace(part)
				upper := strings.ToUpper(name)
				if idx := strings.Index(upper, " COLLATE"); idx >= 0 {
					name = strings.TrimSpace(name[:idx])
				} else if idx := strings.Index(upper, " DESC"); idx >= 0 {
					name = strings.TrimSpace(name[:idx])
				} else if idx := strings.Index(upper, " ASC"); idx >= 0 {
					name = strings.TrimSpace(name[:idx])
				}
				if name != "" {
					cols = append(cols, name)
				}
			}
			if len(cols) == 0 {
				continue
			}
			def := uniqueIndexDef{Cols: cols}
			if wm := indexWhereRe.FindStringSubmatch(ent.SQL); wm != nil {
				def.Where = strings.TrimSpace(wm[1])
			}
			result = append(result, def)
		}
	}
	e.uniqueIdxCache[tableName] = result
	return result
}

// evalIndexWhere evaluates a partial-index predicate against a row.
// A nil/empty predicate always matches.
func (e *Engine) evalIndexWhere(whereSQL string, row RowMap) (bool, error) {
	if strings.TrimSpace(whereSQL) == "" {
		return true, nil
	}
	p := sql.NewParser("SELECT " + whereSQL)
	stmts := p.Parse()
	if len(stmts) == 0 {
		return true, nil
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || len(sel.Columns) == 0 {
		return true, nil
	}
	v, err := e.evalExpr(sel.Columns[0].Expr, row)
	if err != nil {
		return true, nil
	}
	if v == nil {
		return false, nil
	}
	return toBool(v), nil
}

// checkUniqueIndex scans the table for a row whose values match the new row
// on all columns of a UNIQUE index. Returns a SQLite-style error on conflict.
// NULL values never conflict (SQL UNIQUE allows multiple NULLs).
func (e *Engine) checkUniqueIndex(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, def uniqueIndexDef) error {
	colIndex := buildColumnIndex(colDefs)
	// The new row must itself satisfy the partial-index predicate to be in
	// the index; otherwise it cannot conflict via this index.
	row := buildRowMapFromValues(values, colDefs, 0)
	if inIndex, _ := e.evalIndexWhere(def.Where, row); !inIndex {
		return nil
	}
	idxCols := def.Cols
	key := make([]interface{}, len(idxCols))
	for i, cn := range idxCols {
		idx, ok := colIndex[cn]
		if !ok || idx >= len(values) || values[idx] == nil {
			return nil
		}
		key[i] = values[idx]
	}
	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil
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
		// Only rows that satisfy the partial predicate are in the index.
		erow := buildRowMapFromValues(rec.Values, colDefs, cell.RowID)
		if inIndex, _ := e.evalIndexWhere(def.Where, erow); inIndex {
			match := true
			for i, cn := range idxCols {
				idx, ok := colIndex[cn]
				if !ok || idx >= len(rec.Values) || util.CompareValues(rec.Values[idx], key[i]) != 0 {
					match = false
					break
				}
			}
			if match {
				parts := make([]string, len(idxCols))
				for i, cn := range idxCols {
					parts[i] = tableEntry.Name + "." + cn
				}
				return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(parts, ", "))
			}
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return nil
}

// findRowByIndexCols finds a row that matches the given values on every column
// of the named UNIQUE index. Returns its rowid, values, and true if found.
func (e *Engine) findRowByIndexCols(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, def uniqueIndexDef) (int64, []interface{}, bool) {
	colIndex := buildColumnIndex(colDefs)
	// The new row must itself satisfy the partial-index predicate.
	row := buildRowMapFromValues(values, colDefs, 0)
	if inIndex, _ := e.evalIndexWhere(def.Where, row); !inIndex {
		return 0, nil, false
	}
	idxCols := def.Cols
	key := make([]interface{}, len(idxCols))
	for i, cn := range idxCols {
		idx, ok := colIndex[cn]
		if !ok || idx >= len(values) || values[idx] == nil {
			return 0, nil, false
		}
		key[i] = values[idx]
	}
	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0, nil, false
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
		// Only rows satisfying the partial predicate are in the index.
		erow := buildRowMapFromValues(rec.Values, colDefs, cell.RowID)
		if inIndex, _ := e.evalIndexWhere(def.Where, erow); inIndex {
			match := true
			for i, cn := range idxCols {
				idx, ok := colIndex[cn]
				if !ok || idx >= len(rec.Values) || util.CompareValues(rec.Values[idx], key[i]) != 0 {
					match = false
					break
				}
			}
			if match {
				return cell.RowID, rec.Values, true
			}
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return 0, nil, false
}

// rowIDConflictError builds the UNIQUE error for a rowid conflict. SQLite
// names the INTEGER PRIMARY KEY column when one exists, else "rowid".
func (e *Engine) rowIDConflictError(tableEntry *schema.Entry, colDefs []sql.ColumnDef) error {
	for _, cd := range colDefs {
		if cd.PrimaryKey && strings.EqualFold(strings.TrimSpace(cd.Type), "INTEGER") {
			return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableEntry.Name, cd.Name)
		}
	}
	return fmt.Errorf("UNIQUE constraint failed: %s.rowid", tableEntry.Name)
}

// rowIDExists reports whether the table already has a row with the given rowid.
func (e *Engine) rowIDExists(tableName string, rootPage uint32, rowID int64) bool {
	tree := e.tableBTree(tableName, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			return false
		}
		if cell.RowID == rowID {
			return true
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			return false
		}
	}
}

// replaceDeleteConflicts deletes every row that conflicts with the new values
// on a UNIQUE/PRIMARY KEY column or a UNIQUE index, firing BEFORE and AFTER
// DELETE triggers for each deleted row (SQLite REPLACE semantics).
func (e *Engine) replaceDeleteConflicts(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) *Result {
	colIndex := buildColumnIndex(colDefs)
	seen := make(map[int64]bool)
	// Collect ALL currently-conflicting rows BEFORE firing any triggers.
	// Rows inserted by triggers during the deletes are NOT re-deleted; if
	// they conflict with the new row, the subsequent INSERT reports the
	// UNIQUE/CHECK error (matching SQLite, which does not loop over
	// trigger-inserted rows).
	var conflicts []int64
	var conflictValueMap map[int64][]interface{}
	for {
		var foundID int64
		var foundVals []interface{}
		found := false
		if rid, rv, _, ok := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values); ok && !seen[rid] {
			foundID, foundVals, found = rid, rv, true
		}
		if !found {
			for _, def := range e.uniqueIndexColumns(tableEntry.Name) {
				if rid, rv, ok := e.findRowByIndexCols(tableEntry, colDefs, values, def); ok && !seen[rid] {
					foundID, foundVals, found = rid, rv, true
					break
				}
			}
		}
		if !found {
			break
		}
		seen[foundID] = true
		conflicts = append(conflicts, foundID)
		if conflictValueMap == nil {
			conflictValueMap = make(map[int64][]interface{})
		}
		conflictValueMap[foundID] = foundVals
	}

	hasTriggers := e.hasTriggersForTable(tableEntry.Name)
	tree := e.tableBTreePg(pg, tableEntry.Name, tableEntry.RootPage, true)
	for _, conflictRowID := range conflicts {
		// Read the row for trigger OLD values.
		conflictValues := conflictValueMap[conflictRowID]
		oldRow := buildRowMapFromValues(conflictValues, colDefs, conflictRowID)
		if hasTriggers {
			if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
				return trigResult
			}
		}
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == conflictRowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.invalidateRowIDCache(tableEntry.RootPage)
		if hasTriggers {
			if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
				return trigResult
			}
		}
		// Foreign key ON DELETE CASCADE: delete child rows that reference
		// this row's key values (firing their DELETE triggers).
		if cascadeResult := e.cascadeDelete(tableEntry, colDefs, conflictValues); cascadeResult.Error != nil {
			return cascadeResult
		}
	}
	return &Result{}
}

// buildRowMapFromValues creates a column-name-to-value map from a values slice.
func buildRowMapFromValues(values []interface{}, colDefs []sql.ColumnDef, rowID int64) RowMap {
	row := make(RowMap)
	for i, v := range values {
		if i < len(colDefs) {
			row[colDefs[i].Name] = v
		}
	}
	row["rowid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
	return row
}

// columnValue returns the value for a named column from a values array.
func columnValue(values []interface{}, colDefs []sql.ColumnDef, name string) interface{} {
	for i, cd := range colDefs {
		if cd.Name == name && i < len(values) {
			return values[i]
		}
	}
	return nil
}

// contains returns true if the slice contains the value.
func contains(s []int, v int) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// execInsertOnConflict handles INSERT ... ON CONFLICT by attempting the
// insert and falling back to the conflict action when a conflict is detected.
func (e *Engine) execInsertOnConflict(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, s *sql.InsertStmt) *Result {
	oc := s.OnConflict

	// Build a map of column name → index for easy lookup
	colIndex := buildColumnIndex(colDefs)

	// Try to find an existing conflicting row by scanning for UNIQUE violations
	existingRowID, existingValues, _, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)

	// Also check conflicts via UNIQUE indexes (e.g. CREATE UNIQUE INDEX t ON t(a)).
	if !found {
		for _, def := range e.uniqueIndexColumns(tableEntry.Name) {
			if rid, rv, ok := e.findRowByIndexCols(tableEntry, colDefs, values, def); ok {
				existingRowID, existingValues, found = rid, rv, true
				break
			}
		}
	}

	if !found {
		res := e.insertRow(pg, tableEntry, colDefs, values, nil)
		if res.Error != nil {
			return res
		}
		// insertRow mutates values in place (rowid fill, affinity, generated
		// columns), so values holds the row that was actually written.
		res.Row = values
		return res
	}

	switch oc.Action {
	case sql.ConflictDoNothing:
		// The insert is skipped; RETURNING must not emit a row for it.
		return &Result{Changes: 0, Row: nil}
	case sql.ConflictDoUpdate:
		res := e.applyUpsertUpdate(tableEntry, colDefs, colIndex, existingRowID, existingValues, oc)
		if res.Error != nil {
			return res
		}
		// RETURNING projects against the updated row, not the attempted values.
		return res
	}
	return &Result{Changes: 0}
}

// applyUpsertUpdate applies DO UPDATE SET assignments to the existing row
// and writes the updated row back to the table.
func (e *Engine) applyUpsertUpdate(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, existingRowID int64, existingValues []interface{}, oc *sql.OnConflictClause) *Result {
	updated := e.buildUpdatedRow(colDefs, colIndex, existingValues, oc)

	// Enforce FOREIGN KEY constraints on the updated row.
	if res := e.checkForeignKeyViolations(tableEntry, colDefs, updated); res.Error != nil {
		return res
	}

	record, err := storage.EncodeRecord(updated)
	if err != nil {
		return &Result{Error: err}
	}

	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	deleted, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == existingRowID
	})
	if err != nil || deleted == 0 {
		return &Result{Error: fmt.Errorf("upsert: row not found for update")}
	}
	e.invalidateRowIDCache(tableEntry.RootPage)

	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   existingRowID,
		Payload: record,
	}
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}
	e.bumpRowIDCache(tableEntry.RootPage, existingRowID)

	if e.hasTriggersForTable(tableEntry.Name) {
		newRow := buildRowMapFromValues(updated, colDefs, existingRowID)
		oldRow := buildRowMapFromValues(existingValues, colDefs, existingRowID)
		if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name, newRow, oldRow); trigResult.Error != nil {
			return trigResult
		}
	}
	// Carry the updated row back so RETURNING can project against it.
	return &Result{Changes: 1, Row: updated}
}

// buildUpdatedRow applies ON CONFLICT DO UPDATE SET assignments to the
// existing values and returns the updated row.
func (e *Engine) buildUpdatedRow(colDefs []sql.ColumnDef, colIndex map[string]int, existingValues []interface{}, oc *sql.OnConflictClause) []interface{} {
	// Pad to the full column count: storage trims trailing NULLs from records,
	// so existingValues may be shorter than colDefs.
	n := len(existingValues)
	if len(colDefs) > n {
		n = len(colDefs)
	}
	updated := make([]interface{}, n)
	copy(updated, existingValues)

	row := make(RowMap)
	for _, col := range colDefs {
		if idx, ok := colIndex[col.Name]; ok && idx < len(existingValues) {
			row[col.Name] = existingValues[idx]
		}
	}

	for _, assign := range oc.Assignments {
		if idx, ok := colIndex[assign.Column]; ok {
			val, err := e.evalExpr(assign.Value, row)
			if err == nil && idx < len(updated) {
				updated[idx] = val
			}
		}
	}
	return updated
}

// findRowByUniqueCols searches for a row that conflicts with the given values
// on any UNIQUE column. Returns the RowID, existing values, and whether a
// conflict was found.
func (e *Engine) findRowByUniqueCols(tableName string, rootPage uint32, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) (int64, []interface{}, int, bool) {
	uniqueCols := gatherUniqueColIndices(colDefs, colIndex, values)
	// Also include PRIMARY KEY columns (they imply UNIQUE)
	for i, cd := range colDefs {
		if cd.PrimaryKey && !contains(uniqueCols, i) {
			if i < len(values) && values[i] != nil {
				uniqueCols = append(uniqueCols, i)
			}
		}
	}
	if len(uniqueCols) == 0 {
		return 0, nil, -1, false
	}

	// Fast path: when the only unique column is an INTEGER PRIMARY KEY, the
	// column value IS the rowid, so a conflict can be detected with a direct
	// rowid seek instead of a full-table scan. This matters for large tables
	// (e.g. delete3.test doubles a table via INSERT...SELECT 20 times) where
	// scanning per-row would be O(n²).
	if len(uniqueCols) == 1 {
		idx := uniqueCols[0]
		cd := colDefs[idx]
		if cd.PrimaryKey && strings.EqualFold(strings.TrimSpace(cd.Type), "INTEGER") {
			if v, ok := values[idx].(int64); ok {
				tree := e.tableBTree(tableName, rootPage, true)
				cursor, err := tree.OpenCursor()
				if err != nil {
					return 0, nil, -1, false
				}
				found, err := cursor.SeekToRowID(v)
				if err != nil || !found {
					return 0, nil, -1, false
				}
				cell, err := cursor.ReadCell()
				if err != nil || cell == nil {
					return 0, nil, -1, false
				}
				rec, err := storage.DecodeRecord(cell.Payload)
				if err != nil || rec == nil {
					return 0, nil, -1, false
				}
				return cell.RowID, rec.Values, idx, true
			}
		}
	}

	tree := e.tableBTree(tableName, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0, nil, -1, false
	}

	return scanForConflict(cursor, uniqueCols, values)
}

// scanForConflict iterates through all rows and looks for a value match
// on any of the given UNIQUE column indices. It returns the conflicting row's
// rowid, its values, and the column index that conflicted.
func scanForConflict(cursor *btree.Cursor, uniqueCols []int, values []interface{}) (int64, []interface{}, int, bool) {
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}

		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}

		if idx := hasConflictAt(rec.Values, uniqueCols, values); idx >= 0 {
			return cell.RowID, rec.Values, idx, true
		}

		hasNext, err := cursor.Next()
		if err != nil || !hasNext {
			break
		}
	}
	return 0, nil, -1, false
}

// hasConflictAt returns true if any of the UNIQUE column values match.
// Per SQL standard, NULL != NULL for UNIQUE constraint purposes.
// hasConflictAt returns the first UNIQUE column index whose value matches the
// new row (or -1 if the row does not conflict).
func hasConflictAt(recValues []interface{}, uniqueCols []int, values []interface{}) int {
	for _, idx := range uniqueCols {
		if idx < len(recValues) && idx < len(values) {
			// NULL != NULL — two NULLs never violate a UNIQUE constraint
			if recValues[idx] == nil || values[idx] == nil {
				continue
			}
			if util.CompareValues(recValues[idx], values[idx]) == 0 {
				return idx
			}
		}
	}
	return -1
}

// isIgnoreableConflict checks if a constraint error should be silently ignored
// due to a column-level ON CONFLICT IGNORE clause.
func isIgnoreableConflict(err error, colDefs []sql.ColumnDef) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "UNIQUE constraint failed") {
		return false
	}
	for _, cd := range colDefs {
		if cd.OnConflict == "IGNORE" {
			return true
		}
	}
	return false
}

// isReplaceableConflict checks if a UNIQUE/PRIMARY KEY constraint error should
// be resolved by deleting the conflicting row (column-level ON CONFLICT REPLACE).
func isReplaceableConflict(err error, colDefs []sql.ColumnDef) bool {
	if err == nil {
		return false
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return false
	}
	for _, cd := range colDefs {
		if cd.OnConflict == "REPLACE" {
			return true
		}
	}
	return false
}

// isUniqueConflictError reports whether the error is a UNIQUE/PRIMARY KEY
// constraint violation (used for INSERT OR IGNORE).
func isUniqueConflictError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// gatherUniqueColIndices returns the column indices that have UNIQUE constraints
// and are present in both the column definitions and the provided values.
func gatherUniqueColIndices(colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) []int {
	var uniqueCols []int
	for _, cd := range colDefs {
		if cd.Unique {
			if idx, ok := colIndex[cd.Name]; ok && idx < len(values) {
				uniqueCols = append(uniqueCols, idx)
			}
		}
	}
	return uniqueCols
}

func (e *Engine) execInsertSelect(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt) (ret *Result) {
	selectStmt := s.Select
	columns := s.Columns
	isReplace := s.IsReplace
	orIgnore := s.OrIgnore
	selectResult := e.execSelect(selectStmt)
	if selectResult.Error != nil {
		return selectResult
	}

	// Statement atomicity: REPLACE deletes rows and may fire triggers, and any
	// row may fail a constraint (e.g. CHECK) after earlier rows were already
	// written. If anything fails the whole statement must be rolled back
	// (SQLite statement journal), so snapshot the pager up front.
	snap := e.pager.Snapshot()
	defer func() {
		if ret != nil && ret.Error != nil {
			e.pager.Restore(snap)
			// The pager rollback can invalidate cached rowid counters (rows
			// whose rowids were computed for the aborted statement are gone).
			e.nextRowIDCache = make(map[uint32]int64)
		}
	}()

	// Determine the effective number of columns we expect.
	// If specific columns are given in the INSERT, the SELECT
	// must produce that many values; otherwise it must produce
	// one per table column.
	expectedCount := len(colDefs)
	if len(columns) > 0 {
		expectedCount = len(columns)
	}
	numSelectCols := len(selectResult.Columns)
	if expectedCount != numSelectCols {
		return &Result{Error: fmt.Errorf("table %s has %d columns but %d values were supplied",
			tableEntry.Name, expectedCount, numSelectCols)}
	}

	// Route FTS virtual table inserts directly to the FTS table (same as
	// insertRow): the SELECT result rows become FTS documents.
	if ftsTable, ok := e.ftsTables[tableEntry.Name]; ok {
		var changes int64
		for _, row := range selectResult.Rows {
			nextRowID := ftsTable.Insert(row)
			e.lastRowID = nextRowID
			changes++
		}
		return &Result{Changes: changes, LastInsertRowID: e.lastRowID}
	}

	// Build a column mapping for the INSERT column list.
	// Handle _rowid_ specially (maps to the implicit rowid, not a table column).
	// Handle duplicate column names by only using the first occurrence.
	var colMapping []int // maps SELECT column index → colDefs index (-1 for _rowid_)
	if len(columns) > 0 {
		colMapping = make([]int, len(columns))
		seen := make(map[string]bool)
		for i, col := range columns {
			if strings.EqualFold(col, "_rowid_") || strings.EqualFold(col, "rowid") {
				colMapping[i] = -1 // _rowid_ marker
			} else {
				if seen[col] {
					colMapping[i] = -2 // duplicate, skip
				} else {
					seen[col] = true
					found := false
					for j, cd := range colDefs {
						if cd.Name == col {
							colMapping[i] = j
							found = true
							break
						}
					}
					if !found {
						colMapping[i] = -3 // column not found in table
					}
				}
			}
		}
	}

	var changes int64
	var returningRows [][]interface{}
	for _, row := range selectResult.Rows {
		var values []interface{}
		var explicitRowID int64
		hasExplicitRowID := false

		if len(columns) > 0 {
			values = make([]interface{}, len(colDefs))
			// First, apply default values for all columns
			for j, cd := range colDefs {
				if cd.Default != nil {
					// Evaluate the default expression to get the default value
					if dv, err := e.evalExpr(cd.Default, nil); err == nil {
						values[j] = dv
					}
				}
			}
			// Then map SELECT values to column positions
			for i, colIdx := range colMapping {
				if colIdx >= 0 && i < len(row) {
					values[colIdx] = row[i]
				} else if colIdx == -1 && i < len(row) {
					// _rowid_ column
					if v, ok := row[i].(int64); ok {
						explicitRowID = v
						hasExplicitRowID = true
					}
				}
				// colIdx == -2: duplicate column, skip
				// colIdx == -3: column not found, skip (will be caught by validation)
			}
		} else {
			values = row
		}

		// Apply type affinity to each value based on column type
		for i, v := range values {
			if i < len(colDefs) {
				values[i] = util.ApplyColumnAffinity(v, colDefs[i].Type)
			}
		}

		// Compute any generated columns (b AS(expr)) that were not explicitly set.
		e.computeGeneratedValues(colDefs, values)

		// Handle REPLACE: delete conflicting rows before inserting. The new
		// row's rowid is computed BEFORE the deletes (SQLite keeps the rowid
		// through the REPLACE retry, so a trigger may grab it and conflict).
		var replaceRowID int64
		if isReplace {
			replaceRowID = e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage)
			if res := e.replaceDeleteConflicts(e.pager, tableEntry, colDefs, values); res.Error != nil {
				return res
			}
		}

		// Determine rowID BEFORE constraint checks (CHECK(rowid<=5) needs it)
		var rowID int64
		if hasExplicitRowID {
			rowID = explicitRowID
		} else if isReplace {
			rowID = replaceRowID
			// If INTEGER PRIMARY KEY column is nil, set it to the assigned rowid
			for i, cd := range colDefs {
				if cd.PrimaryKey && i < len(values) && values[i] == nil {
					values[i] = rowID
					break
				}
			}
		} else {
			rowID = e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage)
			// If INTEGER PRIMARY KEY column is nil, set it to the auto-assigned rowid
			for i, cd := range colDefs {
				if cd.PrimaryKey && i < len(values) && values[i] == nil {
					values[i] = rowID
					break
				}
			}
		}

		// Validate constraints before inserting
		if err := e.checkConstraints(tableEntry, colDefs, values, rowID); err != nil {
			// Statement-level INSERT OR IGNORE: silently skip UNIQUE conflicts.
			if orIgnore && isUniqueConflictError(err) {
				continue
			}
			// Column-level ON CONFLICT IGNORE: silence UNIQUE constraint violations
			if isIgnoreableConflict(err, colDefs) {
				continue
			}
			// Column-level ON CONFLICT REPLACE: delete the conflicting row and
			// fall through to insert the new one.
			if isReplaceableConflict(err, colDefs) {
				colIndex := buildColumnIndex(colDefs)
				conflictRowID, _, _, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)
				if found {
					tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
					if _, derr := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
						return cell.RowID == conflictRowID
					}); derr != nil {
						return &Result{Error: derr}
					}
					e.invalidateRowIDCache(tableEntry.RootPage)
				} else {
					return &Result{Error: err}
				}
			} else {
				return &Result{Error: err}
			}
		}

		// A trigger may have inserted a row with our rowid during the
		// REPLACE's delete; report it as a rowid UNIQUE conflict.
		if isReplace && !hasExplicitRowID {
			if e.rowIDExists(tableEntry.Name, tableEntry.RootPage, rowID) {
				return &Result{Error: e.rowIDConflictError(tableEntry, colDefs)}
			}
		}

		record, err := storage.EncodeRecord(values)
		if err != nil {
			return &Result{Error: err}
		}
		cell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   rowID,
			Payload: record,
		}
		tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
		if err := tree.InsertCell(cell); err != nil {
			return &Result{Error: err}
		}
		// Track root page changes (after splits).
		if tree.RootPage() != e.rootPage(tableEntry.Name, tableEntry.RootPage) {
			e.updateRootPage(tableEntry.Name, tree.RootPage())
		}
		e.bumpRowIDCache(tableEntry.RootPage, rowID)
		changes++
		e.lastRowID = rowID

		// Handle RETURNING clause — evaluate against the row that was written.
		if s.HasReturning {
			rrow := buildRowMapFromValues(values, colDefs, rowID)
			rv, err := e.evalReturningStrict(s.Returning, rrow, colDefs, tableEntry.Name)
			if err != nil {
				return &Result{Error: err}
			}
			returningRows = append(returningRows, rv)
		}
	}
	if s.HasReturning {
		columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
		return &Result{Columns: columns, Rows: returningRows}
	}
	return &Result{Changes: changes, LastInsertRowID: e.lastRowID}
}

// computeGeneratedValues fills in values for generated columns (b AS(expr))
// that are still nil. Generated expressions may reference other columns of
// the same row, so evaluation uses a RowMap of the values built so far.
func (e *Engine) computeGeneratedValues(colDefs []sql.ColumnDef, values []interface{}) {
	var rowMap RowMap
	for i, cd := range colDefs {
		if cd.Generated == nil {
			continue
		}
		if i >= len(values) || values[i] != nil {
			continue // explicit value provided (or out of range)
		}
		if rowMap == nil {
			rowMap = make(RowMap)
			for j, v := range values {
				if j < len(colDefs) {
					rowMap[colDefs[j].Name] = v
				}
			}
		}
		if v, err := e.evalExpr(cd.Generated, rowMap); err == nil {
			values[i] = v
		}
	}
}

// pkRowID returns the rowid for a new row, using the INTEGER PRIMARY KEY value
// if one is explicitly provided, or auto-assigning the next available rowid.
func (e *Engine) pkRowID(tableName string, colDefs []sql.ColumnDef, values []interface{}, rootPage uint32) int64 {
	for i, cd := range colDefs {
		if cd.PrimaryKey && i < len(values) && values[i] != nil {
			if v, ok := values[i].(int64); ok {
				return v
			}
			break
		}
	}
	return e.findNextRowID(tableName, rootPage)
}

func (e *Engine) execInsertDefault(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt) *Result {
	// DEFAULT VALUES: every column takes its default value (NULL if none),
	// and an INTEGER PRIMARY KEY column receives the auto-assigned rowid.
	values := make([]interface{}, len(colDefs))
	for i, cd := range colDefs {
		if cd.Default != nil {
			if dv, err := e.evalExpr(cd.Default, nil); err == nil {
				values[i] = dv
			}
		}
	}
	nextRowID := e.findNextRowID(tableEntry.Name, tableEntry.RootPage)
	for i, cd := range colDefs {
		if cd.PrimaryKey && values[i] == nil {
			values[i] = nextRowID
			break
		}
	}

	// Fire BEFORE INSERT triggers — the row is not in the table yet.
	if e.hasTriggersForTable(tableEntry.Name) {
		newRow := make(RowMap)
		for i, v := range values {
			if i < len(colDefs) {
				newRow[colDefs[i].Name] = v
			}
		}
		if trigResult := e.fireBeforeInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
			return trigResult
		}
	}

	record, err := storage.EncodeRecord(values)
	if err != nil {
		return &Result{Error: err}
	}
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   nextRowID,
		Payload: record,
	}
	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}
	e.bumpRowIDCache(tableEntry.RootPage, nextRowID)
	e.lastRowID = nextRowID

	// Fire AFTER INSERT triggers.
	if e.hasTriggersForTable(tableEntry.Name) {
		newRow := make(RowMap)
		for i, v := range values {
			if i < len(colDefs) {
				newRow[colDefs[i].Name] = v
			}
		}
		if trigResult := e.fireAfterInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
			return trigResult
		}
	}

	// Handle RETURNING clause — evaluate against the actual written row.
	if s.HasReturning {
		row := buildRowMapFromValues(values, colDefs, nextRowID)
		vals, err := e.evalReturningStrict(s.Returning, row, colDefs, tableEntry.Name)
		if err != nil {
			return &Result{Error: err}
		}
		columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
		return &Result{Columns: columns, Rows: [][]interface{}{vals}}
	}

	return &Result{Changes: 1, LastInsertRowID: nextRowID}
}

// fireAfterInsertTriggers fires AFTER INSERT triggers for the given table.
func (e *Engine) fireAfterInsertTriggers(tableName string, newRow RowMap) *Result {
	return e.fireTriggers(tableName, "INSERT", "AFTER", newRow, nil)
}

// fireBeforeInsertTriggers fires BEFORE INSERT triggers for the given table.
func (e *Engine) fireBeforeInsertTriggers(tableName string, newRow RowMap) *Result {
	return e.fireTriggers(tableName, "INSERT", "BEFORE", newRow, nil)
}

// fireAfterUpdateTriggers fires AFTER UPDATE triggers for the given table.
func (e *Engine) fireAfterUpdateTriggers(tableName string, newRow, oldRow RowMap) *Result {
	return e.fireTriggers(tableName, "UPDATE", "AFTER", newRow, oldRow)
}

// fireBeforeUpdateTriggers fires BEFORE UPDATE triggers for the given table.
func (e *Engine) fireBeforeUpdateTriggers(tableName string, newRow, oldRow RowMap) *Result {
	return e.fireTriggers(tableName, "UPDATE", "BEFORE", newRow, oldRow)
}

// fireAfterDeleteTriggers fires AFTER DELETE triggers for the given table.
func (e *Engine) fireAfterDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.fireTriggers(tableName, "DELETE", "AFTER", nil, oldRow)
}

// fireBeforeDeleteTriggers fires BEFORE DELETE triggers for the given table.
func (e *Engine) fireBeforeDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.fireTriggers(tableName, "DELETE", "BEFORE", nil, oldRow)
}

// fireTriggers fires triggers matching the given event and timing for the table.
func (e *Engine) fireTriggers(tableName, event, timing string, newRow, oldRow RowMap) *Result {
	// Prevent recursive trigger firing unless PRAGMA recursive_triggers is
	// enabled (matches SQLite's default of OFF).
	if e.triggerDepth > 0 && !e.recursiveTriggers {
		return &Result{}
	}

	// Search for triggers across all databases. TEMP may alias MAIN, so
	// dedupe by context pointer to avoid firing each trigger twice.
	var triggers []*schema.Entry
	seen := make(map[*DatabaseContext]bool)
	for _, ctx := range e.databases {
		if seen[ctx] {
			continue
		}
		seen[ctx] = true
		t, err := ctx.Schema.FindTriggersForTable(tableName)
		if err == nil && len(t) > 0 {
			triggers = append(triggers, t...)
		}
	}

	if len(triggers) == 0 {
		return &Result{}
	}
	for _, t := range triggers {
		if res := e.fireTrigger(t, event, timing, newRow, oldRow); res != nil {
			return res
		}
	}
	return &Result{}
}

// fireTrigger fires a single trigger matching the given event and timing.
// Returns a Result with an error if execution fails, or nil on success
// (including when the trigger does not match or its WHEN clause is false).
func (e *Engine) fireTrigger(t *schema.Entry, event, timing string, newRow, oldRow RowMap) *Result {
	upper := strings.ToUpper(t.SQL)
	// Check the trigger's declared timing. Triggers without an explicit
	// timing default to BEFORE.
	hasBefore := strings.Contains(upper, " BEFORE ")
	hasAfter := strings.Contains(upper, " AFTER ")
	if hasBefore && timing != "BEFORE" {
		return nil
	}
	if hasAfter && timing != "AFTER" {
		return nil
	}
	if !hasBefore && !hasAfter && timing != "BEFORE" {
		return nil
	}
	// Check event matches: the declaration is "BEFORE|AFTER <event> ON <table>"
	// (or "<event> ON <table>" which defaults to BEFORE). Match against the
	// declaration only — the trigger BODY may contain the event keyword too
	// (e.g. an AFTER DELETE trigger whose body says "INSERT INTO ...").
	patterns := []string{
		" " + timing + " " + event + " ON ",
	}
	if timing == "BEFORE" {
		// Triggers with no explicit timing default to BEFORE.
		patterns = append(patterns, " "+event+" ON ")
	}
	matched := false
	for _, p := range patterns {
		if strings.Contains(upper, p) {
			matched = true
			break
		}
	}
	if !matched {
		return nil
	}

	// Increment trigger depth to prevent recursive trigger firing
	e.triggerDepth++
	defer func() { e.triggerDepth-- }()

	// Set NEW and OLD row values for trigger body execution
	prevNewRow := e.triggerNewRow
	prevOldRow := e.triggerOldRow
	e.triggerNewRow = newRow
	e.triggerOldRow = oldRow
	defer func() {
		e.triggerNewRow = prevNewRow
		e.triggerOldRow = prevOldRow
	}()

	// Evaluate the WHEN clause if present. The clause sits between the
	// "ON <table>" header and the BEGIN keyword.
	if whenExpr := e.parseTriggerWhen(t.SQL); whenExpr != nil {
		val, err := e.evalExpr(whenExpr, nil)
		if err != nil {
			return &Result{Error: err}
		}
		if val == nil || !toBool(val) {
			return nil
		}
	}

	// Extract statements between BEGIN and END
	beginIdx := strings.Index(upper, "BEGIN")
	if beginIdx < 0 {
		return nil
	}
	endIdx := strings.LastIndex(upper, "END")
	if endIdx < 0 {
		return nil
	}
	body := t.SQL[beginIdx+5 : endIdx]
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	parser := sql.NewParser(body)
	stmts := parser.Parse()
	if parser.Err() != nil {
		return nil
	}

	for _, stmt := range stmts {
		res := e.Exec(stmt)
		if res.Error != nil {
			// RAISE(IGNORE) aborts the current statement without error and
			// execution continues with the next statement in the trigger
			// program (SQLite semantics).
			if res.Error == errRaiseIgnore {
				continue
			}
			// Add "main." schema prefix for table-not-found errors during trigger execution,
			// matching SQLite's behavior where trigger execution errors include the default schema.
			errMsg := res.Error.Error()
			if strings.Contains(errMsg, "no such table:") {
				// Extract the table name and add "main." prefix if not already qualified
				if parts := strings.SplitN(errMsg, "no such table: ", 2); len(parts) == 2 {
					tableName := parts[1]
					if !strings.Contains(tableName, ".") {
						res.Error = fmt.Errorf("no such table: main.%s", tableName)
					}
				}
			}
			return res
		}
	}
	return nil
}

// parseTriggerWhen extracts and parses the WHEN expression of a trigger's
// CREATE TRIGGER SQL text. Returns nil when the trigger has no WHEN clause.
func (e *Engine) parseTriggerWhen(triggerSQL string) sql.Expr {
	upper := strings.ToUpper(triggerSQL)
	whenIdx := strings.Index(upper, " WHEN ")
	if whenIdx < 0 {
		return nil
	}
	beginIdx := strings.Index(upper[whenIdx:], " BEGIN")
	if beginIdx < 0 {
		return nil
	}
	exprText := triggerSQL[whenIdx+len(" WHEN ") : whenIdx+beginIdx]
	exprText = strings.TrimSpace(exprText)
	if exprText == "" {
		return nil
	}
	parser := sql.NewParser("SELECT " + exprText)
	stmts := parser.Parse()
	if parser.Err() != nil || len(stmts) == 0 {
		return nil
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || len(sel.Columns) == 0 {
		return nil
	}
	return sel.Columns[0].Expr
}

func (e *Engine) evalTuple(tuple []sql.Expr, columns []string, colDefs []sql.ColumnDef) ([]interface{}, error) {
	values := make([]interface{}, len(tuple))
	for i, expr := range tuple {
		v, err := e.evalExpr(expr, nil)
		if err != nil {
			return nil, err
		}
		values[i] = v
	}
	if len(columns) > 0 {
		// Start with default values for all columns, then override with provided values
		mapped := make([]interface{}, len(colDefs))
		for j, cd := range colDefs {
			if cd.Default != nil {
				if dv, err := e.evalExpr(cd.Default, nil); err == nil {
					mapped[j] = dv
				}
			}
		}
		for i, col := range columns {
			for j, cd := range colDefs {
				if cd.Name == col && i < len(values) {
					mapped[j] = values[i]
					break
				}
			}
		}
		values = mapped
	} else if len(values) < len(colDefs) {
		// Pad with default values for any missing trailing columns
		padded := make([]interface{}, len(colDefs))
		copy(padded, values)
		for j := len(values); j < len(colDefs); j++ {
			if colDefs[j].Default != nil {
				if dv, err := e.evalExpr(colDefs[j].Default, nil); err == nil {
					padded[j] = dv
				}
			}
		}
		values = padded
	}
	return values, nil
}

// evalReturningStrict evaluates RETURNING expressions with strict column
// resolution: unknown columns and invalid qualifiers produce "no such column"
// errors (SQLite semantics), and table-qualified wildcards are rejected.
func (e *Engine) evalReturningStrict(ret sql.SelectColumn, row Row, colDefs []sql.ColumnDef, tableName string) ([]interface{}, error) {
	prevStrict, prevTable := e.returningStrict, e.returningTable
	e.returningStrict, e.returningTable = true, tableName
	defer func() { e.returningStrict, e.returningTable = prevStrict, prevTable }()
	return e.evalReturningExprs(ret, row, colDefs)
}

// evalReturningExprs evaluates RETURNING expressions against a row and
// returns a flat list of values. It handles three cases:
//   - RETURNING * : expands to all column values
//   - RETURNING expr (single) : returns the single expression value
//   - RETURNING expr, ..., * , ... : multi-expression with * expanded inline
func (e *Engine) evalReturningExprs(ret sql.SelectColumn, row Row, colDefs []sql.ColumnDef) ([]interface{}, error) {
	switch expr := ret.Expr.(type) {
	case *sql.ColumnRef:
		if expr.Name == "*" && expr.Table == "" {
			// RETURNING * — expand to all column values
			var values []interface{}
			for _, cd := range colDefs {
				if cd.Dropped {
					continue
				}
				if v, ok := row.Get(cd.Name); ok {
					values = append(values, util.UnwrapColumnValue(v))
				}
			}
			return values, nil
		}
		// Single column reference
		val, err := e.evalExpr(expr, row)
		if err != nil {
			return nil, err
		}
		return []interface{}{util.UnwrapColumnValue(val)}, nil

	case *sql.RowValue:
		// Multi-expression RETURNING — evaluate each sub-expression
		var values []interface{}
		for _, subExpr := range expr.Values {
			if ref, ok := subExpr.(*sql.ColumnRef); ok && ref.Name == "*" && ref.Table == "" {
				// Expand * to all column values inline
				for _, cd := range colDefs {
					if cd.Dropped {
						continue
					}
					if v, ok := row.Get(cd.Name); ok {
						values = append(values, util.UnwrapColumnValue(v))
					}
				}
			} else {
				val, err := e.evalExpr(subExpr, row)
				if err != nil {
					return nil, err
				}
				values = append(values, util.UnwrapColumnValue(val))
			}
		}
		return values, nil

	default:
		// Single expression not * and not a row value
		val, err := e.evalExpr(ret.Expr, row)
		if err != nil {
			return nil, err
		}
		return []interface{}{util.UnwrapColumnValue(val)}, nil
	}
}

// execInsertView handles INSERT statements whose target is a view. SQLite
// routes such statements through INSTEAD OF triggers; resolving the view's
// columns (which validates collations in its SELECT) happens first.
func (e *Engine) execInsertView(s *sql.InsertStmt, viewEntry *schema.Entry) *Result {
	// Resolve the view definition: parse and validate its SELECT expressions.
	// This surfaces errors like "no such collation sequence: X" at insert time.
	parser := sql.NewParser(viewEntry.SQL)
	stmts := parser.Parse()
	if parser.Err() != nil {
		return &Result{Error: parser.Err()}
	}
	var viewSelect *sql.SelectStmt
	for _, st := range stmts {
		if c, ok := st.(*sql.CreateViewStmt); ok {
			viewSelect = c.Select
			break
		}
	}
	if viewSelect != nil {
		if err := e.validateCollationsInSelect(viewSelect); err != nil {
			return &Result{Error: err}
		}
	}

	// Views only accept INSERT when an INSTEAD OF INSERT trigger exists.
	if !e.hasTriggersForTable(viewEntry.Name) {
		return &Result{Error: fmt.Errorf("cannot modify %s because it is a view", viewEntry.Name)}
	}

	// Fire INSTEAD OF INSERT triggers; their bodies replace the insert.
	row := make(RowMap)
	row["rowid"] = nil
	if res := e.fireTriggers(viewEntry.Name, "INSERT", "BEFORE", row, nil); res != nil && res.Error != nil {
		return res
	}
	if !s.HasReturning {
		return &Result{Changes: 1}
	}
	vals, err := e.evalReturningStrict(s.Returning, row, nil, viewEntry.Name)
	if err != nil {
		return &Result{Error: err}
	}
	return &Result{Rows: [][]interface{}{vals}}
}

// validateCollationsInSelect walks every expression in a SELECT statement and
// verifies that each COLLATE operator names a known collation sequence.
func (e *Engine) validateCollationsInSelect(s *sql.SelectStmt) error {
	if s == nil {
		return nil
	}
	for _, col := range s.Columns {
		if err := e.validateCollationsInExpr(col.Expr); err != nil {
			return err
		}
	}
	if err := e.validateCollationsInExpr(s.Where); err != nil {
		return err
	}
	if err := e.validateCollationsInExpr(s.Having); err != nil {
		return err
	}
	for _, g := range s.GroupBy {
		if err := e.validateCollationsInExpr(g); err != nil {
			return err
		}
	}
	for _, o := range s.OrderBy {
		if err := e.validateCollationsInExpr(o.Expr); err != nil {
			return err
		}
	}
	if s.Union != nil {
		return e.validateCollationsInSelect(s.Union)
	}
	return nil
}

// validateCollationsInExpr verifies COLLATE operators in an expression tree.
func (e *Engine) validateCollationsInExpr(expr sql.Expr) error {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		if strings.EqualFold(v.Operator, "COLLATE") {
			return e.checkCollationName(v.Right)
		}
		if err := e.validateCollationsInExpr(v.Left); err != nil {
			return err
		}
		return e.validateCollationsInExpr(v.Right)
	case *sql.UnaryOp:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.ParenExpr:
		return e.validateCollationsInExpr(v.Expr)
	case *sql.FuncCall:
		for _, a := range v.Args {
			if err := e.validateCollationsInExpr(a); err != nil {
				return err
			}
		}
		return nil
	case *sql.CastExpr:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.CaseExpr:
		if err := e.validateCollationsInExpr(v.Operand); err != nil {
			return err
		}
		for _, w := range v.Whens {
			if err := e.validateCollationsInExpr(w.When); err != nil {
				return err
			}
			if err := e.validateCollationsInExpr(w.Then); err != nil {
				return err
			}
		}
		return e.validateCollationsInExpr(v.Else)
	case *sql.Between:
		if err := e.validateCollationsInExpr(v.Operand); err != nil {
			return err
		}
		if err := e.validateCollationsInExpr(v.Low); err != nil {
			return err
		}
		return e.validateCollationsInExpr(v.High)
	case *sql.InList:
		if err := e.validateCollationsInExpr(v.Operand); err != nil {
			return err
		}
		for _, item := range v.List {
			if err := e.validateCollationsInExpr(item); err != nil {
				return err
			}
		}
		return nil
	case *sql.IsDistinctFrom:
		if err := e.validateCollationsInExpr(v.Left); err != nil {
			return err
		}
		return e.validateCollationsInExpr(v.Right)
	case *sql.IsNotDistinctFrom:
		if err := e.validateCollationsInExpr(v.Left); err != nil {
			return err
		}
		return e.validateCollationsInExpr(v.Right)
	case *sql.IsNull:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.IsNotNull:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.IsTrue:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.IsFalse:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.Subquery:
		return e.validateCollationsInSelect(v.Select)
	case *sql.ExistsExpr:
		return e.validateCollationsInSelect(v.Select)
	case *sql.RowValue:
		for _, sub := range v.Values {
			if err := e.validateCollationsInExpr(sub); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

// checkCollationName verifies that a COLLATE operand names a known collation.
func (e *Engine) checkCollationName(expr sql.Expr) error {
	var name string
	switch v := expr.(type) {
	case *sql.StringLit:
		name = v.Value
	case *sql.ColumnRef:
		name = v.Name
	default:
		return nil
	}
	switch strings.ToUpper(name) {
	case "", "BINARY", "NOCASE", "RTRIM":
		return nil
	default:
		return fmt.Errorf("no such collation sequence: %s", name)
	}
}
