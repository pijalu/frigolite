// Package exec implements query execution.
package exec

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
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
				e.restorePager(dbCtx.Pager, snap)
				// Rows whose rowids were computed for the aborted statement
				// are gone; the cached rowid counter must not survive.
				e.nextRowIDCache = make(map[uint32]int64)
				e.autoIncSeq = make(map[uint32]int64)
			}
		}()
	}

	var totalChanges int64
	var lastRowID int64
	var returningRows [][]interface{}
	for _, tuple := range s.Values {
		values, evalErr := e.evalTuple(tableEntry.Name, tuple, s.Columns, colDefs)
		if evalErr != nil {
			return &Result{Error: evalErr}
		}
		// An explicit rowid/_rowid_/oid column in the INSERT list sets the
		// new row's rowid (SQLite allows INSERT INTO t(rowid, ...) VALUES).
		var explicitRowID *int64
		for i, col := range s.Columns {
			if isRowIDName(col) && i < len(tuple) {
				v, err := e.evalExpr(tuple[i], nil)
				if err != nil {
					return &Result{Error: err}
				}
				if v != nil {
					if iv, ok := util.UnwrapColumnValue(v).(int64); ok {
						explicitRowID = &iv
					}
				}
			}
		}

		// Handle REPLACE (INSERT OR REPLACE): delete conflicting rows before
		// inserting. The new row's rowid is computed BEFORE the deletes
		// (SQLite keeps it through the REPLACE retry, so a trigger may grab it).
		var replaceRowID int64
		var haveReplaceRowID bool
		if s.IsReplace {
			rr, err := e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage, hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)))
			if err != nil {
				return &Result{Error: err}
			}
			replaceRowID = rr
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
			} else if explicitRowID != nil {
				fixed = explicitRowID
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
	nextRowID, err := e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage, hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)))
	if err != nil {
		return &Result{Error: err}
	}
	if fixedRowID != nil {
		nextRowID = *fixedRowID
	}
	e.lastRowID = nextRowID

	// If INTEGER PRIMARY KEY column value is nil, set it to the auto-assigned rowid.
	// SQLite behavior: inserting NULL into an INTEGER PRIMARY KEY column causes
	// the column to contain the auto-generated rowid. This does NOT apply to
	// non-INTEGER PRIMARY KEY columns (e.g. "ANY PRIMARY KEY"), which may
	// legally contain NULL in rowid tables, nor to WITHOUT ROWID tables (whose
	// keys are never auto-generated).
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	for i, cd := range colDefs {
		if !withoutRowid && cd.PrimaryKey && strings.EqualFold(strings.TrimSpace(cd.Type), "INTEGER") &&
			i < len(values) && values[i] == nil {
			values[i] = nextRowID
			break
		}
	}

	if err := e.checkConstraints(tableEntry, colDefs, values, nextRowID); err != nil {
		// Column-level ON CONFLICT IGNORE: silence UNIQUE constraint violations
		if e.isIgnoreableConflict(err, tableEntry, colDefs) {
			return &Result{Changes: 0}
		}
		// Column-level ON CONFLICT REPLACE on a NOT NULL column: substitute the
		// column's DEFAULT value for the NULL and re-validate (SQLite conflate.c
		// OP_IsNull + ON CONFLICT REPLACE resolution). Without a DEFAULT the
		// constraint error stands.
		if cd := notNullReplaceColumn(err, colDefs); cd != nil && cd.Default != nil {
			dv, derr := e.evalExpr(cd.Default, nil)
			if derr != nil {
				return &Result{Error: derr}
			}
			idx := cdIndex(colDefs, cd.Name)
			if idx >= 0 && idx < len(values) {
				values[idx] = dv
				if err := e.checkConstraints(tableEntry, colDefs, values, nextRowID); err != nil {
					return &Result{Error: err}
				}
			}
		} else {
			return &Result{Error: err}
		}
	}

	// STRICT table enforcement: check each value against its column's declared
	// type BEFORE affinity is applied (affinity would convert the value to
	// match the column type, defeating the STRICT check). In STRICT tables,
	// only values compatible with the declared type are allowed.
	isStrict := isStrictTable(tableEntry.SQL)
	if isStrict {
		for i, v := range values {
			if i >= len(colDefs) {
				break
			}
			cd := colDefs[i]
			// Skip generated columns (computed separately)
			if cd.Generated != nil {
				continue
			}
			if err := enforceStrictType(tableEntry.Name, cd.Name, cd.Type, v); err != nil {
				return &Result{Error: err}
			}
		}
	}

	// Apply type affinity to each value based on column type.
	// Apply in-place to avoid allocating a separate affValues slice.
	for i, v := range values {
		if i < len(colDefs) {
			values[i] = util.ApplyColumnAffinity(v, colDefs[i].Type)
		}
	}

	// In STRICT mode, affinity may have converted the value — re-check that
	// the converted value still matches the declared type (e.g. integer '42'
	// was accepted as a string but affinity converted it to int64 42).
	if isStrict {
		for i, v := range values {
			if i >= len(colDefs) {
				break
			}
			cd := colDefs[i]
			if cd.Generated != nil {
				continue
			}
			if err := enforceStrictType(tableEntry.Name, cd.Name, cd.Type, v); err != nil {
				return &Result{Error: err}
			}
		}
	}

	// Compute any generated columns (b AS(expr)) that were not explicitly set.
	values = e.computeGeneratedValues(colDefs, values)

	// In STRICT mode, enforce type checking on generated column values too.
	// Generated columns compute values from expressions, and those values must
	// conform to the column's declared type (e.g., REAL column can't have TEXT).
	if isStrict {
		for i, v := range values {
			if i >= len(colDefs) {
				break
			}
			cd := colDefs[i]
			if cd.Generated == nil {
				continue // already checked above
			}
			if err := enforceStrictType(tableEntry.Name, cd.Name, cd.Type, v); err != nil {
				return &Result{Error: err}
			}
		}
	}

	// Enforce FOREIGN KEY constraints (only when PRAGMA foreign_keys is ON).
	if res := e.checkForeignKeyViolations(tableEntry, colDefs, values, 0); res.Error != nil {
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

	// Unwrap collation wrappers (a trigger body may pass a column value
	// wrapped with its collation) so only raw values are stored.
	for i := range values {
		if values[i] != nil {
			values[i] = unwrapCollatedValue(values[i])
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

	// Set the DML table context so table-qualified column references inside
	// CHECK/default expressions (e.g. CHECK (5 IN (false.false))) resolve
	// against this row's unqualified column keys.
	prevDML := e.currentDMLTable
	e.currentDMLTable = tableEntry.Name
	defer func() { e.currentDMLTable = prevDML }()

	row := buildRowMapFromValues(values, colDefs, rowID)

	// In WITHOUT ROWID tables every PRIMARY KEY column is implicitly NOT NULL
	// (the PK is the storage key; no rowid auto-generation exists to fill it).
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	var pkCols map[int]bool
	if withoutRowid {
		pkCols = e.primaryKeyColIndices(tableEntry.Name, tableEntry.SQL, colDefs)
	}

	for _, cd := range colDefs {
		val := columnValue(values, colDefs, cd.Name)

		// NOT NULL constraint — skip for INTEGER PRIMARY KEY columns of rowid
		// tables, since they get their value from the auto-generated rowid.
		pkAutoRowID := cd.PrimaryKey && strings.EqualFold(strings.TrimSpace(cd.Type), "INTEGER") && !withoutRowid
		implicitNotNull := cd.NotNull || (withoutRowid && pkCols[cdIndex(colDefs, cd.Name)])
		if implicitNotNull && val == nil && !pkAutoRowID {
			return fmt.Errorf("NOT NULL constraint failed: %s.%s", tableEntry.Name, e.originalColumnName(tableEntry.SQL, cd.Name))
		}

		// CHECK constraint: only fails when result is explicitly false.
		// NULL (unknown) and true both pass.
		if cd.Check != nil {
			checkVal, err := e.evalExpr(cd.Check, row)
			if err == nil && checkVal != nil && !toBool(checkVal) {
				// Prefer the original CHECK expression text from the CREATE
				// TABLE SQL (SQLite reports the expression verbatim, e.g.
				// "rowid!=33" not the re-rendered "rowid <> 33").
				checkText := e.checkConstraintText(tableEntry.SQL, cd.Name, cd.Check)
				return fmt.Errorf("CHECK constraint failed: %s", checkText)
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

	// Check table-level composite PRIMARY KEY / UNIQUE constraints
	// (e.g. PRIMARY KEY(a,b) or UNIQUE(a,b)). Each group is a set of column
	// indices that must be unique TOGETHER, not individually.
	for _, group := range e.compositeUniqueGroups(tableEntry.Name, tableEntry.SQL, colDefs) {
		if err := e.checkCompositeUnique(tableEntry, colDefs, values, group); err != nil {
			return err
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

// compositeUniqueGroups returns groups of column indices that have table-level
// PRIMARY KEY or UNIQUE constraints. Each group must be unique together.
// Single-column PRIMARY KEY (column-level) is excluded since it's handled
// separately by the column-level check.
func (e *Engine) compositeUniqueGroups(tableName, createSQL string, colDefs []sql.ColumnDef) [][]int {
	constraints := e.tableConstraints(tableName, createSQL)
	colIndex := buildColumnIndex(colDefs)
	var groups [][]int
	for _, tc := range constraints {
		switch tc.Type {
		case sql.ConstraintPrimaryKey, sql.ConstraintUnique:
			var indices []int
			for _, ic := range tc.Columns {
				if idx, ok := colIndex[ic.Name]; ok && idx >= 0 {
					indices = append(indices, idx)
				}
			}
			if len(indices) > 0 {
				groups = append(groups, indices)
			}
		}
	}
	return groups
}

// checkCompositeUnique scans for an existing row where ALL columns in the group
// match the new row's values (composite uniqueness). NULL values never conflict
// (NULL != NULL in SQL uniqueness semantics).
func (e *Engine) checkCompositeUnique(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, group []int) error {
	// Skip if any group value is NULL (composite key with NULL is never a conflict)
	for _, idx := range group {
		if idx >= len(values) || values[idx] == nil {
			return nil
		}
	}
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
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
		if allMatch(colDefs, rec.Values, group, values) {
			// Build error message with all constraint column names
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
		hasNext, err := cursor.Next()
		if err != nil || !hasNext {
			break
		}
	}
	return nil
}

// allMatch returns true if ALL columns in the group match between the existing
// record and the new values. NULL values never match (NULL != NULL). Each
// column's declared collation is applied to its comparison.
func allMatch(colDefs []sql.ColumnDef, recValues []interface{}, group []int, values []interface{}) bool {
	for _, idx := range group {
		if idx >= len(recValues) || idx >= len(values) {
			return false
		}
		if recValues[idx] == nil || values[idx] == nil {
			return false
		}
		coll := ""
		if idx < len(colDefs) {
			coll = colDefs[idx].Collate
		}
		if util.CompareValuesCollate(recValues[idx], values[idx], coll) != 0 {
			return false
		}
	}
	return true
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
	stmts, perr := parse.ParseSQL("SELECT " + whereSQL)
	if perr != nil || len(stmts) == 0 {
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

// indexKeyValue returns the value of one index column for a row. A plain
// column name resolves through colIndex; any other expression (e.g. "0 | c0")
// is parsed and evaluated against the row. The bool result is false when the
// value cannot be computed (NULL or evaluation error) — callers treat that as
// no-conflict (SQL UNIQUE allows multiple NULLs in an index key).
func (e *Engine) indexKeyValue(cn string, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, row RowMap) (interface{}, bool) {
	if idx, ok := colIndex[cn]; ok && idx >= 0 && idx < len(values) {
		if values[idx] == nil {
			return nil, false
		}
		return values[idx], true
	}
	// Expression index column: evaluate SELECT <expr> against the row.
	stmts, perr := parse.ParseSQL("SELECT " + cn)
	if perr != nil || len(stmts) == 0 {
		return nil, false
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || len(sel.Columns) == 0 {
		return nil, false
	}
	v, err := e.evalExpr(sel.Columns[0].Expr, row)
	if err != nil {
		return nil, false
	}
	if v == nil {
		return nil, false
	}
	// Unwrap column-affinity wrappers so comparisons use raw values.
	v = util.UnwrapColumnValue(v)
	return v, true
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
		kv, ok := e.indexKeyValue(cn, colDefs, colIndex, values, row)
		if !ok {
			return nil
		}
		key[i] = kv
	}
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
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
				kv, ok := e.indexKeyValue(cn, colDefs, colIndex, rec.Values, erow)
				if !ok || util.CompareValues(kv, key[i]) != 0 {
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
		kv, ok := e.indexKeyValue(cn, colDefs, colIndex, values, row)
		if !ok {
			return 0, nil, false
		}
		key[i] = kv
	}
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
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
				kv, ok := e.indexKeyValue(cn, colDefs, colIndex, rec.Values, erow)
				if !ok || util.CompareValues(kv, key[i]) != 0 {
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
	tree := e.tableBTreeForName(tableName, rootPage, true)
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
			// Wrap the value with its column's affinity so comparisons and
			// affinity-reporting functions (affinity()) see the declared
			// affinity, matching how scanned table rows are wrapped. Only wrap
			// when the column declares an explicit type (an empty type is the
			// generic no-affinity case, where the raw value is used).
			if colDefs[i].Type != "" {
				cv := &util.ColumnValue{Value: v, Affinity: util.Affinity(colDefs[i].Type)}
				if coll := colDefs[i].Collate; coll != "" && !strings.EqualFold(coll, "BINARY") && !strings.EqualFold(coll, "RTRIM") {
					row[colDefs[i].Name] = &collatedValue{value: cv, collation: strings.ToUpper(coll)}
				} else {
					row[colDefs[i].Name] = cv
				}
				continue
			}
			// Wrap values whose column declares a collation (e.g. NOCASE) so
			// comparisons against them use that collation (SQLite column
			// collation rules). Only non-BINARY collations are wrapped.
			if coll := colDefs[i].Collate; coll != "" && !strings.EqualFold(coll, "BINARY") && !strings.EqualFold(coll, "RTRIM") {
				row[colDefs[i].Name] = &collatedValue{value: v, collation: strings.ToUpper(coll)}
			} else {
				row[colDefs[i].Name] = v
			}
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
		res := e.applyUpsertUpdate(tableEntry, colDefs, colIndex, existingRowID, existingValues, values, oc)
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
func (e *Engine) applyUpsertUpdate(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, existingRowID int64, existingValues []interface{}, values []interface{}, oc *sql.OnConflictClause) *Result {
	updated := e.buildUpdatedRow(colDefs, colIndex, existingValues, values, oc)

	// Enforce FOREIGN KEY constraints on the updated row.
	if res := e.checkForeignKeyViolations(tableEntry, colDefs, updated, 0); res.Error != nil {
		return res
	}

	record, err := storage.EncodeRecord(updated)
	if err != nil {
		return &Result{Error: err}
	}

	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
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
// existing values and returns the updated row. values holds the attempted
// insert row; its columns are exposed to the SET expressions through the
// "excluded" pseudo-table (e.g. excluded.b).
func (e *Engine) buildUpdatedRow(colDefs []sql.ColumnDef, colIndex map[string]int, existingValues []interface{}, values []interface{}, oc *sql.OnConflictClause) []interface{} {
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
		// The excluded pseudo-table carries the row that would have been
		// inserted (values).
		if idx, ok := colIndex[col.Name]; ok && idx < len(values) {
			row["excluded."+col.Name] = values[idx]
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

	// Check table-level composite PRIMARY KEY / UNIQUE constraints for
	// REPLACE and UPSERT conflict detection. A composite key conflict occurs
	// when ALL columns in the group match simultaneously.
	if tableEnt, _, err := e.findTable(tableName); err == nil {
		for _, group := range e.compositeUniqueGroups(tableName, tableEnt.SQL, colDefs) {
			// Skip if any group value is NULL (NULL never conflicts)
			hasNull := false
			for _, idx := range group {
				if idx >= len(values) || values[idx] == nil {
					hasNull = true
					break
				}
			}
			if hasNull {
				continue
			}
			tree := e.tableBTreeForName(tableName, rootPage, true)
			cursor, err := tree.OpenCursor()
			if err != nil {
				continue
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
				if allMatch(colDefs, rec.Values, group, values) {
					return cell.RowID, rec.Values, group[0], true
				}
				hasNext, err := cursor.Next()
				if err != nil || !hasNext {
					break
				}
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
				tree := e.tableBTreeForName(tableName, rootPage, true)
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

	tree := e.tableBTreeForName(tableName, rootPage, true)
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
// due to a column-level ON CONFLICT IGNORE clause or a table-level constraint's
// ON CONFLICT IGNORE clause.
func (e *Engine) isIgnoreableConflict(err error, tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// A NOT NULL violation on a column with ON CONFLICT IGNORE is skipped.
	// The message is "NOT NULL constraint failed: <table>.<column>".
	if strings.Contains(errStr, "NOT NULL constraint failed") {
		for _, cd := range colDefs {
			if cd.OnConflict == "IGNORE" && strings.HasSuffix(errStr, "."+cd.Name) {
				return true
			}
		}
		return false
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
	for _, tc := range e.tableConstraints(tableEntry.Name, tableEntry.SQL) {
		if (tc.Type == sql.ConstraintUnique || tc.Type == sql.ConstraintPrimaryKey) && tc.OnConflict == "IGNORE" {
			return true
		}
	}
	return false
}

// notNullReplaceColumn returns the column (with ON CONFLICT REPLACE) whose NOT
// NULL constraint was violated, or nil. Used to substitute the column DEFAULT.
func notNullReplaceColumn(err error, colDefs []sql.ColumnDef) *sql.ColumnDef {
	if err == nil {
		return nil
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "NOT NULL constraint failed") {
		return nil
	}
	for i := range colDefs {
		cd := &colDefs[i]
		if cd.OnConflict == "REPLACE" && strings.HasSuffix(errStr, "."+cd.Name) {
			return cd
		}
	}
	return nil
}

// isReplaceableConflict checks if a UNIQUE/PRIMARY KEY constraint error should
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
	// The INSERT's WITH clause (CTEs) applies to its SELECT body. Push the
	// CTEs onto the scope stack so findCTE can resolve references like
	// "INSERT INTO t SELECT ... FROM c" where c is a WITH RECURSIVE CTE.
	if len(s.CTEs) > 0 {
		e.cteScopes = append(e.cteScopes, s.CTEs)
		defer func() { e.cteScopes = e.cteScopes[:len(e.cteScopes)-1] }()
	}
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
			e.restorePager(e.pager, snap)
			// The pager rollback can invalidate cached rowid counters (rows
			// whose rowids were computed for the aborted statement are gone).
			e.nextRowIDCache = make(map[uint32]int64)
			e.autoIncSeq = make(map[uint32]int64)
		}
	}()

	// Determine the effective number of columns we expect.
	// If specific columns are given in the INSERT, the SELECT
	// must produce that many values; otherwise it must produce
	// one per table column (excluding generated columns).
	expectedCount := len(colDefs)
	if len(columns) > 0 {
		expectedCount = len(columns)
	} else {
		// Exclude generated columns from expected count when no column list given
		for _, cd := range colDefs {
			if cd.Generated != nil {
				expectedCount--
			}
		}
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
		values = e.computeGeneratedValues(colDefs, values)

		// Handle REPLACE: delete conflicting rows before inserting. The new
		// row's rowid is computed BEFORE the deletes (SQLite keeps the rowid
		// through the REPLACE retry, so a trigger may grab it and conflict).
		var replaceRowID int64
		if isReplace {
			rr, err := e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage, hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)))
			if err != nil {
				return &Result{Error: err}
			}
			replaceRowID = rr
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
			var err error
			rowID, err = e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage, hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)))
			if err != nil {
				return &Result{Error: err}
			}
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
			if e.isIgnoreableConflict(err, tableEntry, colDefs) {
				continue
			}
			// Column-level ON CONFLICT REPLACE: delete the conflicting row and
			// fall through to insert the new one.
			if isReplaceableConflict(err, colDefs) {
				colIndex := buildColumnIndex(colDefs)
				conflictRowID, _, _, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)
				if found {
					tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
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
		tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
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
// that are still nil, and returns the (possibly extended) values slice.
// Generated expressions may reference other columns of the same row —
// including other generated columns — so evaluation iterates to a fixpoint:
// a VIRTUAL column defined before the STORED column it references (e.g.
// "a INT AS (b*2) VIRTUAL, b INT AS (c*2) STORED") computes on a later pass
// once b is filled. The slice may need to grow when an INSERT...SELECT maps
// fewer columns than the table has (the trailing generated columns are nil).
func (e *Engine) computeGeneratedValues(colDefs []sql.ColumnDef, values []interface{}) []interface{} {
	for pass := 0; pass < len(colDefs); pass++ {
		progress := false
		rowMap := make(RowMap)
		for j, v := range values {
			if j < len(colDefs) {
				rowMap[colDefs[j].Name] = v
			}
		}
		for i, cd := range colDefs {
			if cd.Generated == nil {
				continue
			}
			if i < len(values) && values[i] != nil {
				continue // explicit value provided
			}
			if v, err := e.evalExpr(cd.Generated, rowMap); err == nil {
				for len(values) <= i {
					values = append(values, nil)
				}
				values[i] = v
				rowMap[cd.Name] = v
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	return values
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
func (e *Engine) pkRowID(tableName string, colDefs []sql.ColumnDef, values []interface{}, rootPage uint32, withoutRowid bool) (int64, error) {
	for i, cd := range colDefs {
		if cd.PrimaryKey && i < len(values) && values[i] != nil {
			if !withoutRowid && strings.EqualFold(strings.TrimSpace(cd.Type), "INTEGER") {
				v := util.ApplyColumnAffinity(values[i], "NUMERIC")
				if iv, ok := v.(int64); ok {
					return iv, nil
				}
				return 0, fmt.Errorf("datatype mismatch")
			}
			if v, ok := values[i].(int64); ok {
				if withoutRowid {
					return v, nil
				}
			}
			break
		}
	}
	return e.findNextRowID(tableName, rootPage), nil
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
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
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
	// SQLite (recursive_triggers OFF by default) does not allow a trigger to
	// re-fire on a table that is already in the current trigger invocation
	// chain (that would be recursion). Chained triggers on OTHER tables fire
	// normally. With recursive_triggers ON, recursion is allowed.
	if e.triggerDepth > 0 && !e.recursiveTriggers {
		for _, t := range e.triggerTables {
			if t == tableName {
				return &Result{}
			}
		}
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
	e.triggerTables = append(e.triggerTables, tableName)
	defer func() { e.triggerTables = e.triggerTables[:len(e.triggerTables)-1] }()
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
	// Extract the declared timing and event from the trigger header. This is
	// whitespace-robust (the declaration can have arbitrary spaces between
	// the timing, event and ON keywords) unlike a naive " BEFORE INSERT ON "
	// substring match. Triggers without an explicit timing default to BEFORE.
	declTiming, declEvent := parseTriggerHeader(t.SQL)
	if declTiming == "" {
		declTiming = "BEFORE"
	}
	if declTiming != timing {
		return nil
	}
	if declEvent != event {
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
	upper := strings.ToUpper(t.SQL)
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
	stmts, perr := parse.ParseSQL(body)
	if perr != nil {
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
	stmts, perr := parse.ParseSQL("SELECT " + exprText)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || len(sel.Columns) == 0 {
		return nil
	}
	return sel.Columns[0].Expr
}

// parseTriggerHeader extracts the declared timing ("BEFORE", "AFTER",
// "INSTEAD OF") and event ("INSERT", "UPDATE", "DELETE") from a trigger's
// CREATE TRIGGER SQL text. It is whitespace-robust: the declaration may have
// any number of spaces/newlines between the timing, event and ON keywords.
// Returns ("", "") when the header cannot be parsed.
func parseTriggerHeader(triggerSQL string) (timing, event string) {
	upper := strings.ToUpper(triggerSQL)
	// Only look at the declaration header, before the body's BEGIN keyword.
	header := upper
	if beginIdx := strings.Index(upper, "BEGIN"); beginIdx >= 0 {
		header = upper[:beginIdx]
	}
	if strings.Contains(header, "INSTEAD OF") {
		timing = "BEFORE"
	} else if strings.Contains(header, "AFTER") {
		timing = "AFTER"
	} else if strings.Contains(header, "BEFORE") {
		timing = "BEFORE"
	}
	// The event is the first standalone INSERT/UPDATE/DELETE word in the
	// header (the table name appears after "ON", so the first event word is
	// always the declared event).
	for _, ev := range []string{"INSERT", "UPDATE", "DELETE"} {
		if regexp.MustCompile(`\b` + ev + `\b`).MatchString(header) {
			event = ev
			break
		}
	}
	return timing, event
}

// checkConstraintText extracts the original CHECK constraint expression text
// from a CREATE TABLE SQL for the given column. Falls back to the re-rendered
// expression when the raw text cannot be located.
func (e *Engine) checkConstraintText(createSQL, colName string, check sql.Expr) string {
	upper := strings.ToUpper(createSQL)
	start := strings.Index(upper, "(")
	end := strings.LastIndex(upper, ")")
	if start < 0 || end <= start {
		return sql.ExprString(check)
	}
	body := createSQL[start+1 : end]
	for _, part := range splitColumnDefs(body) {
		if !strings.HasPrefix(strings.TrimSpace(part), colName) {
			continue
		}
		pUpper := strings.ToUpper(part)
		ci := strings.Index(pUpper, "CHECK")
		if ci < 0 {
			continue
		}
		lp := strings.Index(part[ci:], "(")
		if lp < 0 {
			continue
		}
		lp += ci
		depth := 0
		for i := lp; i < len(part); i++ {
			switch part[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					return strings.TrimSpace(part[lp+1 : i])
				}
			}
		}
	}
	return sql.ExprString(check)
}

func (e *Engine) evalTuple(tableName string, tuple []sql.Expr, columns []string, colDefs []sql.ColumnDef) ([]interface{}, error) {
	values := make([]interface{}, len(tuple))
	for i, expr := range tuple {
		v, err := e.evalExpr(expr, nil)
		if err != nil {
			return nil, err
		}
		values[i] = v
	}
	if len(columns) > 0 {
		// The VALUES list must supply exactly one value per named column.
		if len(values) != len(columns) {
			return nil, fmt.Errorf("table %s has %d values for %d columns",
				tableName, len(values), len(columns))
		}
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
	} else if colDefs == nil {
		// No column definitions available (e.g. view INSERT): return the
		// values as-is; the caller maps them to the view's output columns.
		return values, nil
	} else {
		// Without a column list every table column must be supplied. Generated
		// columns are excluded from the count (SQLite computes them).
		expected := len(colDefs)
		for _, cd := range colDefs {
			if cd.Generated != nil {
				expected--
			}
		}
		if len(values) != expected {
			if len(values) < expected {
				return nil, fmt.Errorf("table %s has %d columns but %d values were supplied",
					tableName, expected, len(values))
			}
			return nil, fmt.Errorf("table %s has %d values for %d columns",
				tableName, len(values), expected)
		}
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
	stmts, perr := parse.ParseSQL(viewEntry.SQL)
	if perr != nil {
		return &Result{Error: perr}
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

	// Build the NEW row: map the INSERTed values to the view's output column
	// names so trigger bodies can reference NEW.col. Column names come from
	// the view's SELECT (aliases when present, else expression text).
	row := make(RowMap)
	row["rowid"] = nil
	viewCols := e.viewColumnNames(viewSelect)
	var values []interface{}
	if len(s.Values) > 0 {
		values, _ = e.evalTuple(viewEntry.Name, s.Values[0], s.Columns, nil)
	}
	if len(s.Columns) > 0 {
		for i, col := range s.Columns {
			if i < len(values) {
				row[col] = values[i]
			}
		}
	} else {
		for i, val := range values {
			if i < len(viewCols) {
				row[viewCols[i]] = val
			}
		}
	}

	// Fire INSTEAD OF INSERT triggers; their bodies replace the insert.
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

// viewColumnNames returns the output column names of a view's SELECT: the
// explicit alias when present, otherwise the column reference name or the
// expression text.
func (e *Engine) viewColumnNames(sel *sql.SelectStmt) []string {
	if sel == nil {
		return nil
	}
	var names []string
	for _, col := range sel.Columns {
		if col.As != "" {
			names = append(names, col.As)
			continue
		}
		if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			names = append(names, ref.Name)
			continue
		}
		names = append(names, e.exprName(col.Expr))
	}
	return names
}

// exprName returns a human-readable name for an expression (fallback for view
// columns without an alias).
func (e *Engine) exprName(expr sql.Expr) string {
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return v.Name
	case *sql.BinaryOp:
		return e.exprName(v.Left) + v.Operator + e.exprName(v.Right)
	case *sql.NumericLit:
		return v.Value
	case *sql.StringLit:
		return v.Value
	case *sql.FuncCall:
		return v.Name
	}
	return "col"
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
	return checkCollationString(name)
}

// checkCollationString verifies that a collation name is a known sequence.
func checkCollationString(name string) error {
	switch strings.ToUpper(name) {
	case "", "BINARY", "NOCASE", "RTRIM":
		return nil
	default:
		return fmt.Errorf("no such collation sequence: %s", name)
	}
}
