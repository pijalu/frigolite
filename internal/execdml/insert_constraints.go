package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

func (e *DMLExecutor) hasInsertConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	// Fast path: if there are no constraints at all, skip allocation entirely
	for _, cd := range colDefs {
		if cd.NotNull || cd.Check != nil || cd.PrimaryKey {
			return true
		}
	}
	// Quick scan for any constraint — including UNIQUE via unique indices
	for _, cd := range colDefs {
		if cd.Unique {
			return true
		}
	}
	// UNIQUE indexes (CREATE UNIQUE INDEX) also impose constraints even when
	// no column-level constraint exists on the table.
	if len(e.uniqueIndexColumns(tableEntry.Name)) > 0 {
		return true
	}
	// Table-level constraints (CONSTRAINT c1 CHECK(...) etc.) also impose
	// constraints even when no column-level constraint exists.
	return len(e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL)) > 0
}

// checkColumnConstraints validates per-column NOT NULL and CHECK constraints.

// checkColumnConstraints validates per-column NOT NULL and CHECK constraints.

// checkColumnConstraints validates per-column NOT NULL and CHECK constraints.
// checkColumnConstraints validates per-column NOT NULL and CHECK constraints.
func (e *DMLExecutor) checkColumnConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, row RowMap, withoutRowid bool) error {
	// In WITHOUT ROWID tables every PRIMARY KEY column is implicitly NOT NULL
	// (the PK is the storage key; no rowid auto-generation exists to fill it).
	var pkCols map[int]bool
	if withoutRowid {
		pkCols = e.primaryKeyColIndices(tableEntry.Name, tableEntry.SQL, colDefs)
	}
	for _, cd := range colDefs {
		val := columnValue(values, colDefs, cd.Name)

		// NOT NULL constraint — skip for INTEGER PRIMARY KEY columns of rowid
		// tables (they get their value from the auto-generated rowid) UNLESS
		// the column declares NOT NULL explicitly (e_createtable-4.5.5: "u
		// INT PRIMARY KEY NOT NULL" rejects NULL) or the table is STRICT
		// (e_createtable-4.5.6: a STRICT IPK column does not auto-fill NULL).
		if e.columnNotNullViolation(tableEntry, colDefs, cd, val, withoutRowid, pkCols) {
			return fmt.Errorf("NOT NULL constraint failed: %s.%s", tableEntry.Name, e.originalColumnName(tableEntry.SQL, cd.Name))
		}

		// CHECK constraint: only fails when result is explicitly false.
		// NULL (unknown) and true both pass. PRAGMA
		// ignore_check_constraints=ON skips enforcement (integrity_check
		// still reports violations later).
		if cd.Check != nil && !e.ctx.IgnoreCheckConstraints() {
			if err := e.checkColumnCheckExpr(tableEntry, colDefs, cd, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// columnNotNullViolation reports whether a column's NULL value violates its
// NOT NULL constraint. INTEGER PRIMARY KEY columns of rowid tables auto-fill
// NULL from the rowid and are exempt, UNLESS the column declares NOT NULL
// explicitly or the table is STRICT (where every PK column is implicitly NOT
// NULL — e_createtable-4.5.5/4.5.6/4.5.7).
func (e *DMLExecutor) columnNotNullViolation(tableEntry *schema.Entry, colDefs []sql.ColumnDef, cd sql.ColumnDef, val interface{}, withoutRowid bool, pkCols map[int]bool) bool {
	if val != nil {
		return false
	}
	// In STRICT tables every PRIMARY KEY column is implicitly NOT NULL
	// (SQLite: "every column of a PRIMARY KEY is implicitly NOT NULL"),
	// like WITHOUT ROWID tables.
	strictPKNotNull := isStrictTable(tableEntry.SQL) && cd.PrimaryKey
	implicitNotNull := cd.NotNull || strictPKNotNull || (withoutRowid && pkCols[cdIndex(colDefs, cd.Name)])
	if !implicitNotNull {
		return false
	}
	// An INTEGER PRIMARY KEY rowid-alias column in a rowid table (without
	// explicit NOT NULL, not STRICT) auto-fills NULL from the rowid.
	pkAutoRowID := isIPKRowidAliasCol(cd) && !withoutRowid && !cd.NotNull && !isStrictTable(tableEntry.SQL)
	return !pkAutoRowID
}

// checkColumnCheckExpr evaluates one column-level CHECK constraint.

// checkColumnCheckExpr evaluates one column-level CHECK constraint.

// checkColumnCheckExpr evaluates one column-level CHECK constraint.
// checkColumnCheckExpr evaluates one column-level CHECK constraint.
func (e *DMLExecutor) checkColumnCheckExpr(tableEntry *schema.Entry, colDefs []sql.ColumnDef, cd sql.ColumnDef, row RowMap) error {
	// trusted_schema=OFF blocks non-innocuous user functions in CHECK
	// constraints (trustschema1-1.230); temp-schema tables are always
	// trusted (1.240).
	if e.currentDMLCtx == nil || !e.currentDMLCtx.IsTemp {
		if name := e.unsafeSchemaFunc(cd.Check); name != "" {
			return fmt.Errorf("unsafe use of %s()", name)
		}
	}
	var checkVal interface{}
	var checkErr error
	function.WithPureContext("check", func() error {
		checkVal, checkErr = e.ctx.EvalExpr(cd.Check, row)
		return checkErr
	})
	if checkErr != nil {
		return checkErr
	}
	if checkVal != nil && !execexpr.ToBool(checkVal) {
		// Prefer the original CHECK expression text from the CREATE
		// TABLE SQL (SQLite reports the expression verbatim, e.g.
		// "rowid!=33" not the re-rendered "rowid <> 33").
		checkText := e.checkConstraintText(tableEntry.SQL, cd.Name, cd.Check)
		return fmt.Errorf("CHECK constraint failed: %s", checkText)
	}
	return nil
}

// checkTableLevelCheckConstraints validates table-level CHECK constraints.

// checkTableLevelCheckConstraints validates table-level CHECK constraints.

// checkTableLevelCheckConstraints validates table-level CHECK constraints.
// checkTableLevelCheckConstraints validates table-level CHECK constraints.
func (e *DMLExecutor) checkTableLevelCheckConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, row RowMap) error {
	// Table-level CHECK constraints (CONSTRAINT c1 CHECK(...)).
	tcs := e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL)
	for ti, tc := range tcs {
		if tc.Type != sql.ConstraintCheck || tc.Expr == nil {
			continue
		}
		if e.ctx.IgnoreCheckConstraints() {
			continue
		}
		// trusted_schema=OFF blocks non-innocuous user functions in CHECK
		// constraints; temp-schema tables are always trusted.
		if e.currentDMLCtx == nil || !e.currentDMLCtx.IsTemp {
			if name := e.unsafeSchemaFunc(tc.Expr); name != "" {
				return fmt.Errorf("unsafe use of %s()", name)
			}
		}
		var checkVal interface{}
		var checkErr error
		function.WithPureContext("check", func() error {
			checkVal, checkErr = e.ctx.EvalExpr(tc.Expr, row)
			return checkErr
		})
		if checkErr != nil {
			// A CHECK evaluation error (e.g. non-deterministic use of a date
			// function) propagates as-is, matching SQLite.
			return checkErr
		}
		if checkVal != nil && !execexpr.ToBool(checkVal) {
			name := tc.Name
			if name == "" {
				name = e.tableCheckConstraintText(tableEntry.SQL, ti, tcs)
			}
			return fmt.Errorf("CHECK constraint failed: %s", name)
		}
	}
	return nil
}

// checkUniqueConstraints validates UNIQUE and PRIMARY KEY constraints by scanning
// for existing rows with the same values on UNIQUE or PRIMARY KEY columns.

// maintainIndexesOnInsert writes the new row's entries into every index on
// the table. Partial-index predicates and expression keys are evaluated in a
// pure context, so a non-deterministic date/time function (e.g. date('now'))
// in an index expression raises SQLite's "non-deterministic use of %s() in an
// index" error, matching OP_PureFunc semantics.

// checkUniqueConstraints validates UNIQUE and PRIMARY KEY constraints by scanning
// for existing rows with the same values on UNIQUE or PRIMARY KEY columns.
// maintainIndexesOnInsert writes the new row's entries into every index on
// the table. Partial-index predicates and expression keys are evaluated in a
// pure context, so a non-deterministic date/time function (e.g. date('now'))
// in an index expression raises SQLite's "non-deterministic use of %s() in an
// index" error, matching OP_PureFunc semantics.

// indexRowIncluded evaluates a partial-index predicate for one row in a pure
// context. Indexes without a WHERE include every row.
// indexRowIncluded evaluates a partial-index predicate for one row in a pure
// context. Indexes without a WHERE include every row.
func (e *DMLExecutor) indexRowIncluded(def indexDef, row RowMap) (bool, error) {
	if strings.TrimSpace(def.Where) == "" {
		return true, nil
	}
	var inIndex bool
	var werr error
	function.WithPureContext("index", func() error {
		v, err := e.ctx.EvalExpr(parseWhereExpr(def.Where), row)
		if err != nil {
			werr = err
			return err
		}
		if v == nil {
			inIndex = false
		} else {
			inIndex = execexpr.ToBool(v)
		}
		return nil
	})
	return inIndex, werr
}

// indexKeyValuesForRow computes one index's key values for a row in a pure
// context, returning nil for expression keys that do not apply.

// indexKeyValuesForRow computes one index's key values for a row in a pure
// context, returning nil for expression keys that do not apply.

// indexKeyValuesForRow computes one index's key values for a row in a pure
// context, returning nil for expression keys that do not apply.
// indexKeyValuesForRow computes one index's key values for a row in a pure
// context, returning nil for expression keys that do not apply.
func (e *DMLExecutor) indexKeyValuesForRow(def indexDef, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, row RowMap) ([]interface{}, error) {
	indexValues := make([]interface{}, 0, len(def.Cols)+1)
	for _, cn := range def.Cols {
		var kv interface{}
		var kerr error
		function.WithPureContext("index", func() error {
			var ok bool
			kv, ok, kerr = e.indexKeyValueErr(cn, colDefs, colIndex, values, row)
			if kerr != nil {
				return kerr
			}
			if !ok {
				kv = nil
			}
			return nil
		})
		if kerr != nil {
			return nil, kerr
		}
		indexValues = append(indexValues, kv)
	}
	return indexValues, nil
}

// writeIndexCell encodes and inserts one index entry, tracking root page
// changes after splits.

// writeIndexCell encodes and inserts one index entry, tracking root page
// changes after splits.

// writeIndexCell encodes and inserts one index entry, tracking root page
// changes after splits.
// writeIndexCell encodes and inserts one index entry, tracking root page
// changes after splits.
func (e *DMLExecutor) writeIndexCell(def indexDef, indexValues []interface{}) error {
	payload, err := storage.EncodeRecord(indexValues)
	if err != nil {
		return err
	}
	idxCell := &storage.Cell{
		Type:    storage.CellIndexLeaf,
		Payload: payload,
	}
	idxTree := btree.NewBTree(def.Ctx.Pager, def.RootPage, false)
	if err := idxTree.InsertCell(idxCell); err != nil {
		return err
	}
	if idxTree.RootPage() != def.RootPage {
		e.updateIndexRootPage(def.Name, def.Ctx, idxTree.RootPage())
	}
	return nil
}

// parseWhereExpr parses a standalone expression string into a sql.Expr.

// checkUniqueIndex scans the table for a row whose values match the new row
// on all columns of a UNIQUE index. Returns a SQLite-style error on conflict.
// NULL values never conflict (SQL UNIQUE allows multiple NULLs).

// parseWhereExpr parses a standalone expression string into a sql.Expr.
// checkUniqueIndex scans the table for a row whose values match the new row
// on all columns of a UNIQUE index. Returns a SQLite-style error on conflict.
// NULL values never conflict (SQL UNIQUE allows multiple NULLs).

// uniqueIndexConflictError builds the SQLite-style UNIQUE conflict message for
// an index: expression keys report the index name, plain column keys list the
// columns.
// uniqueIndexConflictError builds the SQLite-style UNIQUE conflict message for
// an index: expression keys report the index name, plain column keys list the
// columns.
func uniqueIndexConflictError(tableEntry *schema.Entry, colIndex map[string]int, def uniqueIndexDef, idxCols []string) error {
	// Expression index keys report the index name (SQLite:
	// "UNIQUE constraint failed: index 'name'"); plain column keys
	// list the columns ("UNIQUE constraint failed: t.col").
	allPlain := true
	for _, cn := range idxCols {
		if _, ok := colIndex[cn]; !ok {
			allPlain = false
			break
		}
	}
	if !allPlain {
		return fmt.Errorf("UNIQUE constraint failed: index '%s'", def.Name)
	}
	parts := make([]string, len(idxCols))
	for i, cn := range idxCols {
		parts[i] = tableEntry.Name + "." + cn
	}
	return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(parts, ", "))
}

// findRowByIndexCols finds a row that matches the given values on every column
// of the named UNIQUE index. Returns its rowid, values, and true if found.

// replaceDeleteConflicts deletes every row that conflicts with the new values
// on a UNIQUE/PRIMARY KEY column, a UNIQUE index, or the explicit rowid
// (replaceRowID), firing BEFORE and AFTER DELETE triggers for each deleted row
// (SQLite REPLACE semantics).

// findRowByIndexCols finds a row that matches the given values on every column
// of the named UNIQUE index. Returns its rowid, values, and true if found.
// replaceDeleteConflicts deletes every row that conflicts with the new values
// on a UNIQUE/PRIMARY KEY column, a UNIQUE index, or the explicit rowid
// (replaceRowID), firing BEFORE and AFTER DELETE triggers for each deleted row
// (SQLite REPLACE semantics).

// collectReplaceConflicts gathers every row conflicting with the new values:
// the explicit rowid, UNIQUE/PRIMARY KEY columns, and UNIQUE indexes.
// collectReplaceConflicts gathers every row conflicting with the new values:
// the explicit rowid, UNIQUE/PRIMARY KEY columns, and UNIQUE indexes.
func (e *DMLExecutor) collectReplaceConflicts(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, replaceRowID int64) ([]int64, map[int64][]interface{}) {
	seen := make(map[int64]bool)
	var conflicts []int64
	var conflictValueMap map[int64][]interface{}
	for {
		foundID, foundVals, found := e.findNextReplaceConflict(pg, tableEntry, colDefs, colIndex, values, replaceRowID, seen)
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
	return conflicts, conflictValueMap
}

// findNextReplaceConflict locates one not-yet-seen row conflicting with the
// new values, checking the explicit rowid, UNIQUE columns, then UNIQUE indexes.
// UNIQUE columns are checked PER-COLUMN: INSERT OR REPLACE INTO t(a UNIQUE,
// b UNIQUE) VALUES('one','two') must delete BOTH the row with a='one' and the
// row with b='two' (each unique column independently).
func (e *DMLExecutor) findNextReplaceConflict(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, replaceRowID int64, seen map[int64]bool) (int64, []interface{}, bool) {
	// An explicit rowid (rowid/oid/_rowid_ in the INSERT list) conflicts
	// with the existing row at that rowid (SQLite OP_Delete on the rowid).
	if rid, rv, ok := e.replaceConflictAtRowID(pg, tableEntry, replaceRowID, seen); ok {
		return rid, rv, true
	}
	// Scan the table once, collecting the first conflicting row for each
	// UNIQUE/PK column. scanAllUniqueConflicts uses the DML target's context
	// pager (dmlTableBTree); the explicit-pager variant below reuses the
	// same scan tree so an ATTACHed table (currentDMLCtx pager) is scanned.
	uniqueCols := collectUniqueColsWithPK(colDefs, colIndex, values)
	if len(uniqueCols) > 0 {
		tree := e.uniqueScanTree(tableEntry.Name, tableEntry.RootPage)
		cursor, err := tree.OpenCursor()
		if err == nil {
			foundCols := make(map[int]bool, len(uniqueCols))
			for {
				cell, cerr := cursor.ReadCell()
				if cerr != nil || cell == nil {
					break
				}
				rec, derr := storage.DecodeRecord(cell.Payload)
				if derr != nil || rec == nil {
					break
				}
				for _, idx := range uniqueCols {
					if foundCols[idx] || seen[cell.RowID] {
						continue
					}
					if idx >= len(rec.Values) || idx >= len(values) {
						continue
					}
					if rec.Values[idx] == nil || values[idx] == nil {
						continue
					}
					if util.CompareValues(rec.Values[idx], values[idx]) == 0 {
						foundCols[idx] = true
						return cell.RowID, rec.Values, true
					}
				}
				hasNext, nerr := cursor.Next()
				if nerr != nil || !hasNext {
					break
				}
			}
		}
	}
	// UNIQUE indexes (CREATE UNIQUE INDEX ... ON t(c1, c2)): SQLite resolves
	// these conflicts first in a REPLACE (a row matched by a UNIQUE index is
	// deleted before a composite-PK conflict; hook2.test 2.1.5 expects the
	// index-conflict row's DELETE preupdate before the PK-conflict row's).
	for _, def := range e.uniqueIndexColumns(tableEntry.Name) {
		if rid, rv, ok := e.findRowByIndexCols(tableEntry, colDefs, values, def); ok && !seen[rid] {
			return rid, rv, true
		}
	}
	// Composite PRIMARY KEY / UNIQUE groups (e.g. PRIMARY KEY(b,c)): scan
	// for a row where ALL group columns match the new values (statement
	// REPLACE must delete it; per-column scans miss composite keys).
	for _, group := range e.compositeUniqueGroups(tableEntry.Name, tableEntry.SQL, colDefs) {
		if cell, rec, err := e.scanTableForMatch(tableEntry, func(rec *storage.Record, cell *storage.Cell) bool {
			return !seen[cell.RowID] && e.allMatch(colDefs, rec.Values, group, values)
		}); err == nil && cell != nil {
			return cell.RowID, rec.Values, true
		}
	}
	return 0, nil, false
}

// replaceConflictAtRowID returns the row at replaceRowID when it is a not-yet-
// seen conflict for a REPLACE insert.

// replaceConflictAtRowID returns the row at replaceRowID when it is a not-yet-
// seen conflict for a REPLACE insert.

// replaceConflictAtRowID returns the row at replaceRowID when it is a not-yet-
// seen conflict for a REPLACE insert.
// replaceConflictAtRowID returns the row at replaceRowID when it is a not-yet-
// seen conflict for a REPLACE insert.
func (e *DMLExecutor) replaceConflictAtRowID(pg *pager.Pager, tableEntry *schema.Entry, replaceRowID int64, seen map[int64]bool) (int64, []interface{}, bool) {
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return 0, nil, false
	}
	tree := e.ctx.TableBTreePg(pg, tableEntry.Name, tableEntry.RootPage, true)
	cell, cerr := e.ctx.ReadCellByRowID(tree, replaceRowID)
	if cerr != nil || cell == nil {
		return 0, nil, false
	}
	rec, derr := storage.DecodeRecord(cell.Payload)
	if derr != nil || rec == nil {
		return 0, nil, false
	}
	if seen[cell.RowID] {
		return 0, nil, false
	}
	return cell.RowID, rec.Values, true
}

// deleteReplaceConflictRow fires BEFORE/AFTER DELETE triggers, deletes the
// row, and applies foreign-key actions for one REPLACE conflict.

// deleteReplaceConflictRow fires BEFORE/AFTER DELETE triggers, deletes the
// row, and applies foreign-key actions for one REPLACE conflict.

// deleteReplaceConflictRow fires BEFORE/AFTER DELETE triggers, deletes the
// row, and applies foreign-key actions for one REPLACE conflict.
// deleteReplaceConflictRow fires BEFORE/AFTER DELETE triggers, deletes the
// row, and applies foreign-key actions for one REPLACE conflict.
func (e *DMLExecutor) deleteReplaceConflictRow(tree *btree.BTree, tableEntry *schema.Entry, colDefs []sql.ColumnDef, conflictRowID int64, conflictValues []interface{}, hasTriggers bool) *Result {
	// Read the row for trigger OLD values.
	oldRow := buildRowMapFromValues(conflictValues, colDefs, conflictRowID)
	if hasTriggers {
		if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
			// RAISE(IGNORE) in a BEFORE DELETE trigger skips this row's delete.
			if trigResult.Error == errRaiseIgnore {
				return nil
			}
			return trigResult
		}
	}
	if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == conflictRowID
	}); err != nil {
		return &Result{Error: err}
	}
	// Remove the conflicting row's index entries (REPLACE deletes the old
	// row; its index entries must go with it).
	if err := e.maintainIndexesOnDelete(tableEntry, colDefs, []RowMap{oldRow}); err != nil {
		return &Result{Error: err}
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
	// Fire the preupdate hook for the deleted conflicting row (REPLACE
	// deletes the old row, then the INSERT fires for the new one).
	delRowID := conflictRowID
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		delRowID = 0
	}
	if res := e.ctx.FirePreupdate(PreupdateEvent{
		Type:  "DELETE",
		DB:    e.schemaNameForPager(e.dmlPager(tableEntry.Name)),
		Table: tableEntry.Name,
		RowID: delRowID, RowID2: delRowID,
		RowidTable:   !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)),
		NoUpdateHook: true,
		Old:          conflictValues,
		New:          nil,
	}); res != nil {
		return res
	}
	if hasTriggers {
		if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
			return trigResult
		}
	}
	// Foreign key actions for the deleted conflicting row: CASCADE children
	// are deleted, SET NULL / SET DEFAULT children update their FK column.
	// NO ACTION / RESTRICT are deferred to after the new row is written
	// (SQLite: the REPLACE may re-insert the same key).
	if e.ctx.ForeignKeys() {
		if fkResult := e.ctx.FkParentDeleteReplace(tableEntry, colDefs, oldRow); fkResult.Error != nil {
			return fkResult
		}
	}
	return nil
}

// buildRowMapFromValues creates a column-name-to-value map from a values slice.

// execInsertOnConflict handles INSERT ... ON CONFLICT by attempting the
// insert and falling back to the conflict action when a conflict is detected.

// buildRowMapFromValues creates a column-name-to-value map from a values slice.
// execInsertOnConflict handles INSERT ... ON CONFLICT by attempting the
// insert and falling back to the conflict action when a conflict is detected.

// validateOnConflictTarget validates every ON CONFLICT clause in the chain
// (statement order) against the table's PRIMARY KEY / UNIQUE constraints and
// indexes. The target must name existing columns whose set exactly matches a
// UNIQUE index or PK/UNIQUE constraint (order-insensitive, matching SQLite's
// upsert target analysis), or be an expression whose text exactly matches an
// expression index key. COLLATE and partial-index WHERE predicates must also
// match. Missing columns raise "no such column"; a target that matches no
// constraint raises the SQLite "does not match" error.
func (e *DMLExecutor) validateOnConflictTarget(oc *sql.OnConflictClause, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	for c := oc; c != nil; c = c.Next {
		if res := e.validateOneConflictTarget(c, tableEntry, colDefs); res != nil {
			return res
		}
	}
	return nil
}

// validateOneConflictTarget validates a single ON CONFLICT clause target.
func (e *DMLExecutor) validateOneConflictTarget(oc *sql.OnConflictClause, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	if len(oc.ConflictColumn) == 0 && len(oc.TargetExpr) == 0 {
		// Bare ON CONFLICT (no target): always valid.
		return nil
	}
	target := oc.ConflictColumn
	// A single-column target naming the rowid is always valid.
	if len(target) == 1 && strings.EqualFold(target[0], "rowid") {
		return nil
	}
	if res := e.validateTargetColumns(target, colDefs); res != nil {
		return res
	}
	// Resolve expression target terms (subqueries / column refs) so a
	// missing table or column in a subquery is reported (SQLite resolves
	// the conflict-target expression at prepare time).
	if res := e.resolveTargetExpressions(oc.TargetExpr); res != nil {
		return res
	}

	// Validate the conflict-target WHERE (partial-index predicate): every
	// unqualified column reference must exist in the table (SQLite resolves
	// it at prepare time — "no such column: y").
	if oc.TargetWhere != nil {
		if bad := e.firstUnknownColumn(oc.TargetWhere, tableEntry, colDefs); bad != "" {
			return &Result{Error: fmt.Errorf("no such column: %s", bad)}
		}
	}

	if e.conflictTargetMatchesUnique(tableEntry, oc, colDefs) {
		return nil
	}
	return &Result{Error: fmt.Errorf("ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint")}
}

// validateTargetColumns checks that every plain-column target term names a
// table column.
func (e *DMLExecutor) validateTargetColumns(target []string, colDefs []sql.ColumnDef) *Result {
	for _, name := range target {
		if name == "" {
			// Expression target term (not a plain column reference);
			// resolution happens during index matching.
			continue
		}
		found := false
		for _, cd := range colDefs {
			if strings.EqualFold(cd.Name, name) {
				found = true
				break
			}
		}
		if !found {
			return &Result{Error: fmt.Errorf("no such column: %s", name)}
		}
	}
	return nil
}

// resolveTargetExpressions surfaces name-resolution errors (no such table /
// no such column) in conflict-target expression terms containing subqueries.
func (e *DMLExecutor) resolveTargetExpressions(targetExprs []string) *Result {
	for _, te := range targetExprs {
		if te == "" {
			continue
		}
		base, _ := splitCollateExpr(te)
		if isColumnName(base) {
			// COLLATE-qualified plain column: no subquery resolution needed.
			continue
		}
		if !e.conflictExprHasSubquery(te) {
			continue
		}
		if err := e.resolveConflictExpr(te); err != nil {
			msg := err.Error()
			if strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column") {
				return &Result{Error: err}
			}
		}
	}
	return nil
}

// firstUnknownColumn returns the name of the first unqualified column reference
// in expr that does not exist in the table's columns (or "" when all resolve).
func (e *DMLExecutor) firstUnknownColumn(expr sql.Expr, tableEntry *schema.Entry, colDefs []sql.ColumnDef) string {
	if expr == nil {
		return ""
	}
	bad := ""
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if bad != "" {
			return
		}
		ref, ok := n.(*sql.ColumnRef)
		if !ok || ref.Table != "" {
			return
		}
		if strings.EqualFold(ref.Name, "rowid") || strings.EqualFold(ref.Name, "_rowid_") || strings.EqualFold(ref.Name, "oid") {
			return
		}
		found := false
		for _, cd := range colDefs {
			if strings.EqualFold(cd.Name, ref.Name) {
				found = true
				break
			}
		}
		if !found {
			bad = ref.Name
		}
	})
	return bad
}

// validateUpsertExpressions checks the DO UPDATE SET assignments and WHERE
// condition of every chained ON CONFLICT clause: a table-qualified column
// reference must name the target table or its alias ("excluded" is always
// valid). SQLite rejects a qualifier naming any other table at prepare time
// with "no such column: qual.col".
func (e *DMLExecutor) validateUpsertExpressions(tableName, alias string, oc *sql.OnConflictClause) *Result {
	for c := oc; c != nil; c = c.Next {
		if c.Action != sql.ConflictDoUpdate {
			continue
		}
		validQual := validUpsertQualifier(tableName, alias)
		for _, assign := range c.Assignments {
			if res := checkUpsertQualified(assign.Value, validQual); res != nil {
				return res
			}
		}
		if res := checkUpsertQualified(c.Where, validQual); res != nil {
			return res
		}
	}
	return nil
}

// validUpsertQualifier returns a predicate accepting a table-qualified column
// reference's qualifier in a DO UPDATE expression. "excluded" is always valid;
// with an alias the original table name is NOT a valid qualifier.
func validUpsertQualifier(tableName, alias string) func(string) bool {
	return func(q string) bool {
		if strings.EqualFold(q, "excluded") {
			return true
		}
		if alias != "" {
			// When an alias is present, the original table name is
			// NOT a valid qualifier (SQLite rejects t1.c when the
			// statement aliases the table as t2).
			return strings.EqualFold(q, alias)
		}
		return strings.EqualFold(q, tableName)
	}
}

// checkUpsertQualified walks expr and returns an error for the first
// table-qualified column reference whose qualifier is not accepted.
func checkUpsertQualified(expr sql.Expr, validQual func(string) bool) *Result {
	if expr == nil {
		return nil
	}
	var bad *Result
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if bad != nil {
			return
		}
		ref, ok := n.(*sql.ColumnRef)
		if !ok || ref.Table == "" {
			return
		}
		if !validQual(ref.Table) {
			bad = &Result{Error: fmt.Errorf("no such column: %s.%s", ref.Table, ref.Name)}
		}
	})
	return bad
}

// resolveConflictExpr parses and resolves one conflict-target expression by
// executing it as a scalar SELECT. Name-resolution failures (missing table or
// column) propagate; other evaluation errors are swallowed.
func (e *DMLExecutor) resolveConflictExpr(exprSQL string) error {
	stmts, perr := parse.ParseSQL("SELECT " + exprSQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok {
		return nil
	}
	r := e.ctx.ExecSelect(sel)
	if r == nil {
		return nil
	}
	return r.Error
}

// conflictExprHasSubquery reports whether the expression text contains a
// subquery (the only target-expression form SQLite resolves at prepare time;
// a bare column/arithmetic expression is matched textually against indexes).
func (e *DMLExecutor) conflictExprHasSubquery(exprSQL string) bool {
	stmts, perr := parse.ParseSQL("SELECT " + exprSQL)
	if perr != nil || len(stmts) == 0 {
		return false
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok {
		return false
	}
	has := false
	if len(sel.Columns) > 0 {
		execquery.WalkExprFull(sel.Columns[0].Expr, func(n sql.Expr) {
			if _, ok := n.(*sql.Subquery); ok {
				has = true
			}
			if _, ok := n.(*sql.ExistsExpr); ok {
				has = true
			}
		})
	}
	return has
}

// conflictTargetMatchesUnique reports whether the conflict target of one clause
// exactly matches (order-insensitively) the keys of the table's PRIMARY KEY, a
// UNIQUE constraint, a UNIQUE index, or an expression index. A plain column
// target term matches an index key on the same column with any collation; a
// COLLATE-qualified term matches only a key with the same effective collation;
// an expression term matches an expression key with the same text. TargetWhere
// must match the index's partial predicate (conflictWhereMatches).
func (e *DMLExecutor) conflictTargetMatchesUnique(tableEntry *schema.Entry, oc *sql.OnConflictClause, colDefs []sql.ColumnDef) bool {
	terms := targetKeyTerms(oc)
	if terms == nil {
		return false
	}
	if e.singleColumnPKMatch(terms, colDefs, oc) {
		return true
	}
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if tc.Type != sql.ConstraintPrimaryKey && tc.Type != sql.ConstraintUnique {
			continue
		}
		if e.termsMatchIndexKeys(terms, tableConstraintKeys(tc, colDefs)) &&
			e.conflictWhereMatches(oc, "") {
			return true
		}
	}
	for _, def := range e.uniqueIndexColumns(tableEntry.Name) {
		if e.termsMatchIndexKeys(terms, indexKeyTerms(def, colDefs)) &&
			e.conflictWhereMatches(oc, def.Where) {
			return true
		}
	}
	return false
}

// singleColumnPKMatch reports whether a single-column target matches a
// column-level PRIMARY KEY or UNIQUE constraint.
