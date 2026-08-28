// Package exec implements query execution.
package execdml

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"strings"
)

// --- INSERT ---
// execInsertRow writes one INSERT VALUES tuple: resolves the explicit rowid,
// handles REPLACE conflict deletes, dispatches to the ON CONFLICT (UPSERT) or
// insertRow path, and returns the write result plus the actually-written row
// (for RETURNING).
func (e *DMLExecutor) execInsertRow(dbCtx *DatabaseContext, tableEntry *schema.Entry, colDefs []sql.ColumnDef, tuple []sql.Expr, values []interface{}, s *sql.InsertStmt) (*Result, []interface{}) {
	explicitRowID, res := e.explicitRowIDFromColumns(tableEntry, s, tuple)
	if res != nil {
		return res, nil
	}
	// An ON CONFLICT (UPSERT) clause takes precedence over INSERT OR REPLACE:
	// SQLite applies the upsert action (DO NOTHING / DO UPDATE) instead of
	// the REPLACE conflict deletion when both are present.
	if s.OnConflict != nil {
		res := e.execInsertOnConflict(dbCtx.Pager, tableEntry, colDefs, values, s)
		if res.Error != nil {
			return res, nil
		}
		// For a DO UPDATE conflict the written row differs from the
		// attempted values; DO NOTHING skips the row entirely (Row nil).
		return res, res.Row
	}
	// Handle REPLACE (INSERT OR REPLACE): delete conflicting rows before
	// inserting. The new row's rowid is computed BEFORE the deletes
	// (SQLite keeps it through the REPLACE retry, so a trigger may grab it).
	// FTS virtual tables handle their own REPLACE conflict in insertFTSRow
	// (the docid PRIMARY KEY), so the btree-based pre-delete is skipped.
	var replaceRowID int64
	var haveReplaceRowID bool
	_, isFTS := e.ctx.FTSTables()[tableEntry.Name]
	if s.IsReplace && !isFTS {
		var rr *Result
		replaceRowID, haveReplaceRowID, rr = e.replaceRowIDAndDelete(dbCtx, tableEntry, colDefs, values, explicitRowID, s)
		if rr != nil {
			return rr, nil
		}
	}
	res = e.insertRow(dbCtx.Pager, tableEntry, colDefs, values, fixedRowIDFor(haveReplaceRowID, replaceRowID, explicitRowID), s.OrConflict)
	if res.Error != nil {
		return res, nil
	}
	// INSERT OR REPLACE: the implicit delete's NO ACTION / RESTRICT
	// constraints are checked at statement end, after the new row is
	// written (the new row may restore the deleted key).
	if s.IsReplace && e.ctx.ForeignKeys() {
		if fkResult := e.ctx.FkCheckReplaceChildren(tableEntry, dbCtx); fkResult.Error != nil {
			return fkResult, nil
		}
	}
	// insertRow mutates values in place; it holds the written row.
	return res, values
}

// explicitRowIDFromColumns scans the INSERT column list for a rowid alias
// column (rowid/_rowid_/oid, plus docid for FTS tables) and evaluates its
// tuple value.
func (e *DMLExecutor) explicitRowIDFromColumns(tableEntry *schema.Entry, s *sql.InsertStmt, tuple []sql.Expr) (*int64, *Result) {
	// An explicit rowid/_rowid_/oid column in the INSERT list sets the
	// new row's rowid (SQLite allows INSERT INTO t(rowid, ...) VALUES).
	// FTS virtual tables also accept docid as the rowid alias
	// (fts3DeclareVtab declares "docid HIDDEN"; e_fts3 1.2.1.2 inserts
	// INTO pages(docid, title, body) VALUES(53, ...)).
	_, isFTS := e.ctx.FTSTables()[tableEntry.Name]
	var explicitRowID *int64
	// fts3.c fts3UpdateMethod: supplying BOTH rowid and docid with
	// different values is an error ("SQL logic error"); equal values are
	// accepted (fts3b-4.8).
	var rowidAliasVal, docidVal *int64
	for i, col := range s.Columns {
		isRowIDCol := execquery.IsRowIDName(col) || (isFTS && strings.EqualFold(col, "docid"))
		if isRowIDCol && i < len(tuple) {
			v, err := e.ctx.EvalExpr(tuple[i], nil)
			if err != nil {
				return nil, &Result{Error: err}
			}
			if v != nil {
				if iv, ok := util.UnwrapColumnValue(v).(int64); ok {
					explicitRowID = &iv
					if execquery.IsRowIDName(col) {
						rowidAliasVal = &iv
					} else {
						docidVal = &iv
					}
				} else if isFTS {
					// FTS docid must be an integer: a non-numeric docid
					// (REPLACE INTO t(docid, x) VALUES('zero', ...)) is a
					// datatype mismatch (fts3.c fts3UpdateMethod:
					// "datatype mismatch").
					return nil, &Result{Error: fmt.Errorf("datatype mismatch")}
				}
			}
		}
	}
	if isFTS && rowidAliasVal != nil && docidVal != nil && *rowidAliasVal != *docidVal {
		return nil, &Result{Error: fmt.Errorf("SQL logic error")}
	}
	return explicitRowID, nil
}

// replaceRowIDAndDelete computes the REPLACE rowid and deletes conflicting
// rows, returning the rowid, whether it is set, and any failure result.
func (e *DMLExecutor) replaceRowIDAndDelete(dbCtx *DatabaseContext, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, explicitRowID *int64, s *sql.InsertStmt) (int64, bool, *Result) {
	rr, err := e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage, hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)))
	if err != nil {
		return 0, false, &Result{Error: err}
	}
	// An explicit rowid in the INSERT list sets the new row's rowid;
	// use it for conflict detection (a REPLACE of a specific rowid must
	// delete the existing row at that rowid).
	if explicitRowID != nil && !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		rr = *explicitRowID
	}
	if res := e.replaceDeleteConflicts(dbCtx.Pager, tableEntry, colDefs, values, rr); res.Error != nil {
		return 0, false, res
	}
	// A trigger may have inserted a row with our rowid during the
	// REPLACE's delete; report it as a rowid UNIQUE conflict.
	if e.rowIDExists(tableEntry.Name, tableEntry.RootPage, rr) {
		return 0, false, &Result{Error: e.rowIDConflictError(tableEntry, colDefs)}
	}
	return rr, true, nil
}

// fixedRowIDFor selects the fixed rowid for the plain insert path.
func fixedRowIDFor(haveReplaceRowID bool, replaceRowID int64, explicitRowID *int64) *int64 {
	if haveReplaceRowID {
		return &replaceRowID
	}
	return explicitRowID
}

// fireInsertRowBeforeTriggers fires BEFORE INSERT triggers for a row about to
// be written, re-allocating the rowid when the triggers consumed the
// pre-computed one. Returns a non-nil Result on trigger failure (errRowSkipped
// for RAISE(IGNORE)).
func (e *DMLExecutor) fireInsertRowBeforeTriggers(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID *int64, withoutRowid, ipkWasNil bool, ipkIndex int) *Result {
	newRow := buildBeforeTriggerRow(colDefs, values, ipkWasNil, ipkIndex, withoutRowid)
	if trigResult := e.fireBeforeInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
		// RAISE(IGNORE) in a BEFORE trigger aborts the insert (the row is
		// skipped, no error) — SQLite semantics.
		if trigResult.Error == errRaiseIgnore {
			return &Result{Error: errRowSkipped}
		}
		return trigResult
	}
	// A BEFORE trigger may have inserted rows into this same table,
	// consuming the rowid we pre-allocated. SQLite assigns the
	// statement's rowid after the BEFORE triggers run, so re-allocate
	// when the pre-computed rowid is no longer the next free one.
	if !withoutRowid {
		e.reallocRowIDAfterTrigger(tableEntry, colDefs, values, nextRowID)
	}
	return nil
}

// buildBeforeTriggerRow builds the new-row map visible to a BEFORE INSERT
// trigger, exposing an unassigned rowid (and IPK) as -1.
func buildBeforeTriggerRow(colDefs []sql.ColumnDef, values []interface{}, ipkWasNil bool, ipkIndex int, withoutRowid bool) RowMap {
	newRow := make(RowMap)
	for i, v := range values {
		if i < len(colDefs) {
			// An auto-assigned INTEGER PRIMARY KEY is not yet known to a
			// BEFORE INSERT trigger: SQLite exposes new.<ipk> as -1.
			if ipkWasNil && i == ipkIndex {
				newRow[colDefs[i].Name] = int64(-1)
			} else {
				newRow[colDefs[i].Name] = v
			}
		}
	}
	// SQLite exposes new.rowid as -1 inside a BEFORE INSERT trigger (the
	// rowid is not assigned until the row is written).
	if !withoutRowid && !execquery.RowHasRowIDColumn(colDefs) {
		newRow["rowid"] = int64(-1)
		newRow["_rowid_"] = int64(-1)
		newRow["oid"] = int64(-1)
	}
	return newRow
}

// reallocRowIDAfterTrigger re-allocates the statement rowid when BEFORE
// triggers consumed the pre-computed one, updating an IPK column holding it.
func (e *DMLExecutor) reallocRowIDAfterTrigger(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID *int64) {
	if *nextRowID >= e.findNextRowID(tableEntry.Name, tableEntry.RootPage) {
		return
	}
	oldID := *nextRowID
	*nextRowID = e.findNextRowID(tableEntry.Name, tableEntry.RootPage)
	e.ctx.SetLastRowID(*nextRowID)
	// If an INTEGER PRIMARY KEY column holds the old rowid, update
	// it to the re-allocated value.
	for i, cd := range colDefs {
		if isIPKRowidAliasCol(cd) &&
			i < len(values) && values[i] != nil {
			if v, ok := values[i].(int64); ok && v == oldID {
				values[i] = *nextRowID
			}
		}
	}
}

// checkUniqueConstraints validates UNIQUE and PRIMARY KEY constraints by scanning
// for existing rows with the same values on UNIQUE or PRIMARY KEY columns.
func (e *DMLExecutor) checkUniqueConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) error {
	return e.checkUniqueConstraintsExcluding(tableEntry, colDefs, values, 0, false)
}

// checkUniqueConstraintsExcluding validates UNIQUE / PRIMARY KEY constraints
// for a row, optionally excluding the row with the given rowid (used by UPSERT
// DO UPDATE, where the updated row already exists and must not conflict with
// itself).
func (e *DMLExecutor) checkUniqueConstraintsExcluding(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, excludeRowID int64, haveExclude bool) error {
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := uniqueColIndicesWithPK(colDefs, values)
	if len(uniqueCols) > 0 {
		rowID, _, conflictIdx, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)
		if found && (!haveExclude || rowID != excludeRowID) {
			if conflictIdx >= 0 && conflictIdx < len(colDefs) {
				return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableEntry.Name, colDefs[conflictIdx].Name)
			}
			return fmt.Errorf("UNIQUE constraint failed: %s", tableEntry.Name)
		}
	}

	// Check table-level composite PRIMARY KEY / UNIQUE constraints
	// (e.g. PRIMARY KEY(a,b) or UNIQUE(a,b)). Each group is a set of column
	// indices that must be unique TOGETHER, not individually.
	for _, group := range e.compositeUniqueGroups(tableEntry.Name, tableEntry.SQL, colDefs) {
		if err := e.checkCompositeUniqueExcluding(tableEntry, colDefs, values, group, excludeRowID, haveExclude); err != nil {
			return err
		}
	}

	// Check UNIQUE indexes (CREATE UNIQUE INDEX ... ON t(c1, c2)).
	for _, def := range e.uniqueIndexColumns(tableEntry.Name) {
		if err := e.checkUniqueIndexExcluding(tableEntry, colDefs, values, def, excludeRowID, haveExclude); err != nil {
			return err
		}
	}
	return nil
}

// checkCompositeUniqueExcluding is checkCompositeUnique with an optional rowid
// to exclude from the conflict scan.
func (e *DMLExecutor) checkCompositeUniqueExcluding(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, group []int, excludeRowID int64, haveExclude bool) error {
	// Skip if any group value is NULL (composite key with NULL is never a conflict)
	for _, idx := range group {
		if idx >= len(values) || values[idx] == nil {
			return nil
		}
	}
	cell, _, err := e.scanTableForMatch(tableEntry, func(rec *storage.Record, cell *storage.Cell) bool {
		if haveExclude && cell.RowID == excludeRowID {
			return false
		}
		return e.allMatch(colDefs, rec.Values, group, values)
	})
	if err != nil || cell == nil {
		return nil
	}
	return compositeUniqueError(tableEntry, colDefs, group)
}

// uniqueColIndicesWithPK gathers the unique column indices, adding any
// single-column PRIMARY KEY columns with non-nil values.
func uniqueColIndicesWithPK(colDefs []sql.ColumnDef, values []interface{}) []int {
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := gatherUniqueColIndices(colDefs, colIndex, values)
	for i, cd := range colDefs {
		if cd.PrimaryKey && !contains(uniqueCols, i) {
			if i < len(values) && values[i] != nil {
				uniqueCols = append(uniqueCols, i)
			}
		}
	}
	return uniqueCols
}

// compositeUniqueGroups returns groups of column indices that have table-level
// PRIMARY KEY or UNIQUE constraints. Each group must be unique together.
// Single-column PRIMARY KEY (column-level) is excluded since it's handled
// separately by the column-level check.

// compositeUniqueError builds the UNIQUE constraint failure message naming all
// columns in the composite group.
func compositeUniqueError(tableEntry *schema.Entry, colDefs []sql.ColumnDef, group []int) error {
	var names []string
	seen := make(map[string]bool)
	for _, idx := range group {
		if idx < len(colDefs) {
			n := tableEntry.Name + "." + colDefs[idx].Name
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(names, ", "))
}

// allMatch returns true if ALL columns in the group match between the existing
// record and the new values. NULL values never match (NULL != NULL). Each
// column's declared collation is applied to its comparison.

// uniqueIndexColumns returns the UNIQUE indexes defined on the given table
// (cached per table name).
func (e *DMLExecutor) uniqueIndexColumns(tableName string) []uniqueIndexDef {
	e.ctx.InitUniqueIdxCache()
	if defs, ok := e.ctx.CachedUniqueIdx(tableName); ok {
		return defs
	}
	var result []uniqueIndexDef
	for _, ctx := range e.ctx.Databases() {
		result = append(result, e.uniqueIndexDefsIn(ctx, tableName)...)
	}
	// SQLite checks UNIQUE indexes newest-first (its table index list is
	// prepended on creation), so the error text names the most recently
	// created conflicting index. Reverse to match the exact error message.
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	e.ctx.SetCachedUniqueIdx(tableName, result)
	return result
}

// uniqueIndexDefsIn returns the UNIQUE indexes for a table defined in one
// database context.
func (e *DMLExecutor) uniqueIndexDefsIn(ctx *DatabaseContext, tableName string) []uniqueIndexDef {
	var result []uniqueIndexDef
	entries, err := ctx.Schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return result
	}
	// The table entry provides CREATE TABLE SQL for deriving autoindex
	// columns (sqlite_autoindex_* entries store no SQL; their columns come
	// from the table's PRIMARY KEY / UNIQUE constraints).
	tableEntry, _ := ctx.Schema.FindTable(tableName)
	for _, ent := range entries {
		if !strings.EqualFold(ent.TblName, tableName) {
			continue
		}
		var cols []string
		var keyColl []string
		if uniqueIndexColsRe.MatchString(ent.SQL) {
			colText := indexColumnListText(ent.SQL)
			if colText == "" {
				continue
			}
			cols = parseIndexKeyCols(colText)
			keyColl = parseIndexKeyCollations(colText)
		} else if tableEntry != nil && strings.HasPrefix(strings.ToUpper(ent.Name), "SQLITE_AUTOINDEX_") {
			// An autoindex created for a table-level PRIMARY KEY or UNIQUE
			// constraint: its columns are the constraint's columns (SQLite
			// assigns sqlite_autoindex_<table>_<N> in creation order: PK
			// first when both exist).
			cols = autoindexConstraintColumns(tableEntry, e)
		}
		if len(cols) == 0 {
			continue
		}
		def := uniqueIndexDef{Name: ent.Name, Cols: cols, KeyColl: keyColl}
		if wm := indexWhereRe.FindStringSubmatch(ent.SQL); wm != nil {
			def.Where = strings.TrimSpace(wm[1])
		}
		result = append(result, def)
	}
	return result
}

// autoindexConstraintColumns returns the column list of a table's PRIMARY KEY
// or UNIQUE constraint for its autoindex (the first table-level constraint
// found; SQLite creates one autoindex per constraint in declaration order).
func autoindexConstraintColumns(tableEntry *schema.Entry, e *DMLExecutor) []string {
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if tc.Type != sql.ConstraintPrimaryKey && tc.Type != sql.ConstraintUnique {
			continue
		}
		var cols []string
		for _, ic := range tc.Columns {
			cols = append(cols, ic.Name)
		}
		if len(cols) > 0 {
			return cols
		}
	}
	return nil
}

// parseIndexKeyCols parses a CREATE INDEX key column-list into stripped key
// expressions (plain names or expression text), removing COLLATE/ASC/DESC
// suffixes where they are not part of an expression.

// allTableIndexes returns every index defined on the given table (unique and
// non-unique alike), with their key expressions, partial predicates, and root
// pages. This drives index maintenance on INSERT.
func (e *DMLExecutor) allTableIndexes(tableName string) []indexDef {
	var result []indexDef
	for _, ctx := range e.ctx.Databases() {
		result = append(result, e.indexDefsIn(ctx, tableName)...)
	}
	return result
}

// indexDefsIn returns the indexes for a table defined in one database
// context.
func (e *DMLExecutor) indexDefsIn(ctx *DatabaseContext, tableName string) []indexDef {
	var result []indexDef
	entries, err := ctx.Schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return result
	}
	for _, ent := range entries {
		if !strings.EqualFold(ent.TblName, tableName) {
			continue
		}
		colText := indexColumnListText(ent.SQL)
		if colText == "" {
			continue
		}
		cols := parseIndexKeyCols(colText)
		if len(cols) == 0 {
			continue
		}
		def := indexDef{Name: ent.Name, Cols: cols, RootPage: ent.RootPage, Ctx: ctx}
		if wm := indexWhereRe.FindStringSubmatch(ent.SQL); wm != nil {
			def.Where = strings.TrimSpace(wm[1])
		}
		result = append(result, def)
	}
	return result
}

// indexDef describes any (unique or non-unique) index for index maintenance.

// findRowByIndexCols finds a row that matches the given values on every column
// of the named UNIQUE index. Returns its rowid, values, and true if found.
func (e *DMLExecutor) findRowByIndexCols(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, def uniqueIndexDef) (int64, []interface{}, bool) {
	colIndex := buildColumnIndex(colDefs)
	// The new row must itself satisfy the partial-index predicate.
	row := buildRowMapFromValues(values, colDefs, 0)
	if inIndex, _ := e.evalIndexWhere(def.Where, row); !inIndex {
		return 0, nil, false
	}
	idxCols := def.Cols
	key := make([]interface{}, len(idxCols))
	for i, cn := range idxCols {
		kv, ok := e.indexKeyValue(cn, colDefs, colIndex, values, row)
		if !ok {
			return 0, nil, false
		}
		key[i] = kv
	}
	cell, rec, _ := e.scanTableForMatch(tableEntry, func(rc *storage.Record, cl *storage.Cell) bool {
		return e.rowMatchesIndexKey(rc, cl, colDefs, colIndex, idxCols, key, def)
	})
	if cell == nil {
		return 0, nil, false
	}
	return cell.RowID, rec.Values, true
}

// rowMatchesIndexKey reports whether an existing row belongs to the partial
// index and matches the new row's key on every index column.
func (e *DMLExecutor) rowMatchesIndexKey(rc *storage.Record, cl *storage.Cell, colDefs []sql.ColumnDef, colIndex map[string]int, idxCols []string, key []interface{}, def uniqueIndexDef) bool {
	// Only rows satisfying the partial predicate are in the index.
	erow := buildRowMapFromValues(rc.Values, colDefs, cl.RowID)
	if inIndex, _ := e.evalIndexWhere(def.Where, erow); !inIndex {
		return false
	}
	for i, cn := range idxCols {
		kv, ok := e.indexKeyValue(cn, colDefs, colIndex, rc.Values, erow)
		if !ok {
			return false
		}
		// Apply the column's declared-type affinity to both sides so an
		// int literal key matches the TEXT-stored value (b TEXT, key 2).
		cd := colDefAt(colDefs, cn)
		typ := ""
		if cd != nil {
			typ = cd.Type
		}
		if e.ctx.CompareValuesWithCollate(util.ApplyColumnAffinity(kv, typ), util.ApplyColumnAffinity(key[i], typ)) != 0 {
			return false
		}
	}
	return true
}

// isIPKRowidAliasCol reports whether a column is an INTEGER PRIMARY KEY
// rowid-alias candidate: PRIMARY KEY, declared type exactly INTEGER
// (case-insensitive), and NOT PRIMARY KEY DESC. SQLite treats INTEGER
// PRIMARY KEY DESC as an ordinary (non-rowid) column (build.c
// sqlite3AddPrimaryKey checks pCol->sortOrder), so DESC columns get a
// separate autoindex and their own rowid.

// buildRowMapFromValues creates a column-name-to-value map from a values slice.
func buildRowMapFromValues(values []interface{}, colDefs []sql.ColumnDef, rowID int64) RowMap {
	row := make(RowMap)
	for i, v := range values {
		if i < len(colDefs) {
			row[colDefs[i].Name] = wrapValueForRowMap(v, colDefs[i])
		}
	}
	// A table may declare columns named rowid/oid/_rowid_; those shadow the
	// pseudo-rowid for name resolution (see rowHasRowIDColumn).
	if !execquery.RowHasRowIDColumn(colDefs) {
		row["rowid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
		row["_rowid_"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
		row["oid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
	}
	return row
}

// columnValue returns the value for a named column from a values array.

// isIgnoreableConflict checks if a constraint error should be silently ignored
// due to a column-level ON CONFLICT IGNORE clause or a table-level constraint's
// ON CONFLICT IGNORE clause.
func (e *DMLExecutor) isIgnoreableConflict(err error, tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// A NOT NULL violation on a column with ON CONFLICT IGNORE is skipped.
	// The message is "NOT NULL constraint failed: <table>.<column>".
	if notNullIgnoreable(errStr, colDefs) {
		return true
	}
	if !strings.Contains(errStr, "UNIQUE constraint failed") {
		return false
	}
	for _, cd := range colDefs {
		if cd.OnConflict == "IGNORE" {
			return true
		}
	}
	// Table-level UNIQUE/PRIMARY KEY constraints may carry their own
	// ON CONFLICT IGNORE clause (e.g. UNIQUE(b,c) ON CONFLICT IGNORE).
	return e.uniqueIgnoreableByTableConstraint(tableEntry)
}

// notNullIgnoreable reports whether a NOT NULL violation belongs to a column
// with ON CONFLICT IGNORE.
func notNullIgnoreable(errStr string, colDefs []sql.ColumnDef) bool {
	if !strings.Contains(errStr, "NOT NULL constraint failed") {
		return false
	}
	for _, cd := range colDefs {
		if cd.OnConflict == "IGNORE" && strings.HasSuffix(errStr, "."+cd.Name) {
			return true
		}
	}
	return false
}

// uniqueIgnoreableByTableConstraint reports whether a table-level UNIQUE or
// PRIMARY KEY constraint carries ON CONFLICT IGNORE.
func (e *DMLExecutor) uniqueIgnoreableByTableConstraint(tableEntry *schema.Entry) bool {
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if (tc.Type == sql.ConstraintUnique || tc.Type == sql.ConstraintPrimaryKey) && tc.OnConflict == "IGNORE" {
			return true
		}
	}
	return false
}

// notNullReplaceColumn returns the column (with ON CONFLICT REPLACE) whose NOT
// NULL constraint was violated, or nil. Used to substitute the column DEFAULT.
// When stmtReplace is true (statement-level INSERT OR REPLACE), any NOT NULL
// column with a DEFAULT qualifies (SQLite substitutes the default for the NULL
// on REPLACE).

// buildInsertSelectValues maps one SELECT result row into the target table's
// column values, applying the INSERT column list (with defaults) or the
// positional/partial mapping. Returns the values, any explicit _rowid_ value,
// and whether an explicit rowid was supplied.
func (e *DMLExecutor) buildInsertSelectValues(row []interface{}, columns []string, colMapping []int, colDefs []sql.ColumnDef) ([]interface{}, int64, bool) {
	if len(columns) > 0 {
		// Fill defaults ONLY for columns not provided by the SELECT (SQLite
		// evaluates a DEFAULT expression exactly once per inserted row; a
		// non-deterministic default like nextint() must not run for a column
		// the SELECT already supplies — e_createtable-3.7.4).
		values := e.fillMissingColumnDefaults(colDefs, colMapping)
		explicitRowID, hasExplicitRowID := mapSelectValues(row, colMapping, values)
		return values, explicitRowID, hasExplicitRowID
	}
	if len(row) == len(colDefs) {
		// Full positional mapping: the SELECT provides one value per table
		// column, including generated columns. Generated columns absorb
		// the SELECT value (SQLite ignores it and recomputes) — clear
		// them before computeGeneratedValues runs (gencol: INSERT INTO
		// t1 SELECT * FROM t0 where both tables have generated columns).
		return rowValuesWithoutGenerated(row, colDefs), 0, false
	}
	// Partial mapping: the SELECT provides one value per non-generated
	// column, in order. Generated columns receive no value here (they
	// are recomputed below) — strict1-8.1: a 2-column SELECT into
	// (debit, credit, amount GENERATED).
	return partialRowValues(row, colDefs), 0, false
}

// fillMissingColumnDefaults builds a values slice applying each column's
// DEFAULT, but only for columns the INSERT column list does not provide
// (colMapping[col] >= 0 marks a provided column; -1 rowid; -2/-3 skip).
func (e *DMLExecutor) fillMissingColumnDefaults(colDefs []sql.ColumnDef, colMapping []int) []interface{} {
	values := make([]interface{}, len(colDefs))
	provided := make(map[int]bool, len(colMapping))
	for _, colIdx := range colMapping {
		if colIdx >= 0 {
			provided[colIdx] = true
		}
	}
	for j, cd := range colDefs {
		if cd.Default != nil && !provided[j] {
			// Evaluate the default expression to get the default value
			if dv, err := e.ctx.EvalExpr(cd.Default, nil); err == nil {
				values[j] = dv
			}
		}
	}
	return values
}

// mapSelectValues maps SELECT values onto column positions, returning any
// explicit _rowid_ value.
func mapSelectValues(row []interface{}, colMapping []int, values []interface{}) (int64, bool) {
	var explicitRowID int64
	hasExplicitRowID := false
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
	return explicitRowID, hasExplicitRowID
}

// rowValuesWithoutGenerated clears generated columns from a full positional
// row so they are recomputed.
func rowValuesWithoutGenerated(row []interface{}, colDefs []sql.ColumnDef) []interface{} {
	values := row
	for gi, gcd := range colDefs {
		if gcd.Generated != nil && gi < len(values) {
			values[gi] = nil
		}
	}
	return values
}

// partialRowValues maps a shorter SELECT row onto non-generated columns.
func partialRowValues(row []interface{}, colDefs []sql.ColumnDef) []interface{} {
	values := make([]interface{}, len(colDefs))
	vidx := 0
	for gi, gcd := range colDefs {
		if gcd.Generated != nil {
			continue
		}
		// Hidden columns (e.g. an FTS table's table-name and docid columns)
		// are not addressable by positional values; a shorter SELECT row maps
		// onto the visible non-generated columns only (mapPositionalTupleValues
		// applies the same rule for VALUES tuples). Without this an
		// INSERT INTO t1 SELECT * FROM t1 on an FTS table would pad the row
		// with nil hidden-column values, corrupting the stored document
		// (fts3prefix2 1.1: the doubling insert then leaves docid NULL).
		if execquery.IsHiddenColumnDef(gcd) {
			continue
		}
		if vidx < len(row) {
			values[gi] = row[vidx]
		}
		vidx++
	}
	return values
}

// assignIPKRowID sets a nil INTEGER PRIMARY KEY column to the assigned
// rowid, returning whether it was nil and its index (a BEFORE INSERT trigger
// sees new.<ipk> as -1).

// execInsertSelectConflict validates constraints for an INSERT ... SELECT row
// and handles the ON CONFLICT resolution (IGNORE skips the row via
// errRowSkipped; REPLACE deletes the conflicting row).
func (e *DMLExecutor) execInsertSelectConflict(s *sql.InsertStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, rowID int64, orIgnore bool) *Result {
	if err := e.checkConstraints(tableEntry, colDefs, values, rowID); err != nil {
		// Statement-level INSERT OR IGNORE: silently skip UNIQUE conflicts.
		if orIgnore && isUniqueConflictError(err) {
			return &Result{Error: errRowSkipped}
		}
		// Per-constraint ON CONFLICT only applies when the statement has no
		// explicit OR clause (a statement OR overrides per-constraint).
		if s.OrConflict != "" {
			return &Result{Error: err}
		}
		// Column-level ON CONFLICT IGNORE: silence UNIQUE constraint violations
		if e.isIgnoreableConflict(err, tableEntry, colDefs) {
			return &Result{Error: errRowSkipped}
		}
		// Column-level ON CONFLICT REPLACE: delete the conflicting row and
		// fall through to insert the new one.
		if isReplaceableConflict(err, colDefs) {
			return e.deleteReplaceConflict(tableEntry, colDefs, values, err)
		}
		// Table-level ON CONFLICT REPLACE (UNIQUE(...) ON CONFLICT REPLACE,
		// PRIMARY KEY(...) ON CONFLICT REPLACE): delete every conflicting row
		// (composite keys may match several rows) and fall through.
		if e.uniqueReplaceableConflict(err, tableEntry, colDefs) {
			if res := e.replaceDeleteConflicts(e.ctx.Pager(), tableEntry, colDefs, values, rowID); res.Error != nil {
				return res
			}
			return nil
		}
		return &Result{Error: err}
	}
	return nil
}

// deleteReplaceConflict removes the row conflicting with the new values
// (column-level ON CONFLICT REPLACE).
func (e *DMLExecutor) deleteReplaceConflict(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, origErr error) *Result {
	colIndex := buildColumnIndex(colDefs)
	conflictRowID, _, _, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)
	if !found {
		return &Result{Error: origErr}
	}
	tree := e.uniqueScanTree(tableEntry.Name, tableEntry.RootPage)
	if _, derr := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == conflictRowID
	}); derr != nil {
		return &Result{Error: derr}
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
	return nil
}

// fireBeforeInsertTriggersForRow fires BEFORE INSERT triggers for an inserted
// row. Returns a non-nil Result when a trigger failed, and true when the row
// was skipped (RAISE IGNORE).

// computeGeneratedValues fills in values for generated columns (b AS(expr))
// that are still nil, and returns the (possibly extended) values slice.
// Generated expressions may reference other columns of the same row —
// including other generated columns — so evaluation iterates to a fixpoint:
// a VIRTUAL column defined before the STORED column it references (e.g.
// "a INT AS (b*2) VIRTUAL, b INT AS (c*2) STORED") computes on a later pass
// once b is filled. The slice may need to grow when an INSERT...SELECT maps
// fewer columns than the table has (the trailing generated columns are nil).
func (e *DMLExecutor) computeGeneratedValues(colDefs []sql.ColumnDef, values []interface{}) error {
	for pass := 0; pass < len(colDefs); pass++ {
		progress := false
		rowMap := generatedRowMap(colDefs, values)
		for i, cd := range colDefs {
			if cd.Generated == nil {
				continue
			}
			var err error
			var done bool
			values, done, err = e.computeOneGenerated(cd, i, values, rowMap)
			if err != nil {
				return err
			}
			if done {
				rowMap[cd.Name] = values[i]
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	return nil
}

// generatedRowMap builds a column-name-to-value map for generated expression
// evaluation.
func generatedRowMap(colDefs []sql.ColumnDef, values []interface{}) RowMap {
	rowMap := make(RowMap)
	for j, v := range values {
		if j < len(colDefs) {
			rowMap[colDefs[j].Name] = v
		}
	}
	return rowMap
}

// computeOneGenerated evaluates one generated column, growing values when
// needed. done reports whether the column was filled in this call.
func (e *DMLExecutor) computeOneGenerated(cd sql.ColumnDef, i int, values []interface{}, rowMap RowMap) ([]interface{}, bool, error) {
	if i < len(values) && values[i] != nil {
		return values, false, nil // explicit value provided
	}
	var v interface{}
	var gerr error
	function.WithPureContext("gencol", func() error {
		v, gerr = e.ctx.EvalExpr(cd.Generated, rowMap)
		return gerr
	})
	if gerr != nil {
		return values, false, gerr
	}
	for len(values) <= i {
		values = append(values, nil)
	}
	// Apply the generated column's affinity (e.g. c0 REAL AS(1)
	// stores 1.0, not integer 1).
	values[i] = util.ApplyColumnAffinity(v, cd.Type)
	return values, true, nil
}

// pkRowID determines the cell rowid for an insert. In SQLite, only an
// INTEGER PRIMARY KEY column in a rowid table is a rowid alias (its value IS
// the rowid); other PRIMARY KEY columns are ordinary columns and get an
// auto-assigned rowid. WITHOUT ROWID tables are emulated with rowid-based
// storage, so their PRIMARY KEY integer value is used as the rowid to keep
// PK-ordered storage.
//
// For an INTEGER PRIMARY KEY column with an explicit non-NULL value, SQLite
// applies NUMERIC affinity (OP_MustBeInt): a value that converts to an exact
// integer (integer, integer-valued real like 3.0, or numeric text like '12')
// is used as the rowid; anything else (non-integer real like 3.5, or
// non-numeric text) fails with "datatype mismatch".

// pkRowID determines the cell rowid for an insert. In SQLite, only an
// INTEGER PRIMARY KEY column in a rowid table is a rowid alias (its value IS
// the rowid); other PRIMARY KEY columns are ordinary columns and get an
// auto-assigned rowid. WITHOUT ROWID tables are emulated with rowid-based
// storage, so their PRIMARY KEY integer value is used as the rowid to keep
// PK-ordered storage.
//
// For an INTEGER PRIMARY KEY column with an explicit non-NULL value, SQLite
// applies NUMERIC affinity (OP_MustBeInt): a value that converts to an exact
// integer (integer, integer-valued real like 3.0, or numeric text like '12')
// is used as the rowid; anything else (non-integer real like 3.5, or
// non-numeric text) fails with "datatype mismatch".
