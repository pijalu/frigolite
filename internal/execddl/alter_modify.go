// ALTER TABLE ADD/DROP/ALTER COLUMN and constraint manipulation: adding and
// dropping columns, adding/dropping named constraints, SET/DROP NOT NULL, and
// the CREATE TABLE SQL rewrites that back them.
package execddl

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

func (e *DDLExecutor) validateAddColumnConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, newCol sql.ColumnDef) *Result {
	// SQLite: "Cannot add a REFERENCES column with non-NULL default value" —
	// a column with a REFERENCES clause may not have a non-NULL constant
	// default (the FK would be ambiguous for existing rows).
	if res := e.validateAddColumnRefDefault(newCol); res != nil {
		return res
	}
	// STRICT tables: a DEFAULT whose value is incompatible with the declared
	// column type is rejected when the table already has rows (SQLite:
	// "type mismatch on DEFAULT"). An empty table accepts the column; the
	// mismatch surfaces on the first INSERT.
	if res := e.validateAddColumnStrictDefault(tableEntry, newCol); res != nil {
		return res
	}

	// Generated columns are validated before the Check/NotNull early-return:
	// SQLite re-parses the generated expression during ALTER ADD and rejects
	// prohibited constructs (subqueries) even for a column with no CHECK or
	// NOT NULL (vtabK-300). The expression is then evaluated for every
	// existing row and NOT NULL/CHECK are enforced per row (alter3-9.*).
	if newCol.Generated != nil {
		return e.validateGeneratedAddColumn(tableEntry, colDefs, newCol)
	}

	if newCol.Check == nil && !newCol.NotNull {
		return &Result{}
	}

	// Determine the default value for the new column.
	defVal, err := e.evalColumnExpr(newCol)
	if err != nil {
		return &Result{Error: err}
	}

	// NOT NULL without a non-NULL default is only allowed when the table
	// has no rows.
	if newCol.NotNull && defVal == nil && e.tableHasRows(tableEntry) {
		//lint:ignore ST1005 SQLite capitalizes this exact message.
		return &Result{Error: fmt.Errorf("Cannot add a NOT NULL column with default value NULL")}
	}
	if newCol.Check == nil {
		return &Result{}
	}
	return e.checkRowsAgainstCheck(tableEntry, colDefs, newCol, defVal)
}

// validateAddColumnRefDefault rejects a REFERENCES column with a non-NULL
// default value (the FK would be ambiguous for existing rows). DEFAULT NULL is
// allowed.
func (e *DDLExecutor) validateAddColumnRefDefault(newCol sql.ColumnDef) *Result {
	if newCol.References == "" || !e.ctx.ForeignKeys() || newCol.Default == nil {
		return nil
	}
	defVal, derr := e.evalColumnExpr(newCol)
	if derr != nil || defVal == nil {
		return nil
	}
	if s, ok := defVal.(string); ok && strings.EqualFold(strings.Trim(s, `'"`), "NULL") {
		return nil // NULL literal
	}
	//lint:ignore ST1005 SQLite error message is capitalized ("Cannot add...")
	return &Result{Error: fmt.Errorf("Cannot add a REFERENCES column with non-NULL default value")}
}

// validateAddColumnStrictDefault rejects a STRICT-table DEFAULT whose value
// mismatches the declared column type when the table already has rows.
func (e *DDLExecutor) validateAddColumnStrictDefault(tableEntry *schema.Entry, newCol sql.ColumnDef) *Result {
	if !execdml.IsStrictTable(tableEntry.SQL) || newCol.Default == nil || !e.tableHasRows(tableEntry) {
		return nil
	}
	defVal, derr := e.evalColumnExpr(newCol)
	if derr == nil && defVal != nil {
		if err := execdml.EnforceStrictType(tableEntry.Name, newCol.Name, newCol.Type, defVal); err != nil {
			return &Result{Error: fmt.Errorf("type mismatch on DEFAULT")}
		}
	}
	return nil
}

// checkRowsAgainstCheck evaluates the new column's CHECK expression for every
// existing row (with the column set to defVal), returning the first violation
// or resolve error. The expression is resolved once against an empty row first,
// matching SQLite's ADD COLUMN re-parse (unknown functions are reported even
// for empty tables, alter-22.2).
func (e *DDLExecutor) checkRowsAgainstCheck(tableEntry *schema.Entry, colDefs []sql.ColumnDef, newCol sql.ColumnDef, defVal interface{}) *Result {
	if _, verr := e.ctx.EvalExpr(newCol.Check, nil); verr != nil {
		return &Result{Error: fmt.Errorf("error in table %s after add column: %v", tableEntry.Name, verr)}
	}
	tree := e.ctx.TableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{}
	}
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		row := e.ctx.BuildRowMap(rec, colDefs, cell.RowID)
		if res := e.evalAddColumnCheck(tableEntry, colDefs, newCol, row, defVal); res != nil {
			return res
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return &Result{}
}

// evalAddColumnCheck evaluates the new column's CHECK expression for one row
// (with the column set to defVal), returning a failure Result on violation or
// resolve error.
func (e *DDLExecutor) evalAddColumnCheck(tableEntry *schema.Entry, colDefs []sql.ColumnDef, newCol sql.ColumnDef, row RowMap, defVal interface{}) *Result {
	row[newCol.Name] = defVal
	checkVal, verr := e.ctx.EvalExpr(newCol.Check, row)
	if verr != nil {
		// SQLite re-parses the CHECK expression during ADD COLUMN and
		// reports a resolve error (e.g. an unknown function) as
		// "error in table %s after add column: %s" (alter-22.2).
		return &Result{Error: fmt.Errorf("error in table %s after add column: %v", tableEntry.Name, verr)}
	}
	if checkVal != nil && !execexpr.ToBool(checkVal) {
		checkText := e.ctx.CheckConstraintText(tableEntry.SQL, newCol.Name, newCol.Check)
		return &Result{Error: fmt.Errorf("CHECK constraint failed: %s", checkText)}
	}
	return nil
}

// validateGeneratedAddColumn enforces NOT NULL and CHECK constraints for a
// generated column added via ALTER TABLE ADD COLUMN. The generated
// expression is evaluated for every existing row (SQLite evaluates the
// constraints per row, in row order).
func (e *DDLExecutor) validateGeneratedAddColumn(tableEntry *schema.Entry, colDefs []sql.ColumnDef, newCol sql.ColumnDef) *Result {
	// SQLite re-parses the added column's generated expression during ALTER
	// (sqlite3AlterAddColumn → sqlite3ExprCheck with NC_GenCol), rejecting
	// subqueries with the "error in table %s after add column: %s" wrapper
	// (vtabK-300).
	if err := validateGeneratedExpr(newCol.Generated); err != nil {
		return &Result{Error: fmt.Errorf("error in table %s after add column: %v", tableEntry.Name, err)}
	}
	tree := e.ctx.TableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{}
	}
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		row := e.ctx.BuildRowMap(rec, colDefs, cell.RowID)
		if res := e.evalGeneratedRow(tableEntry, newCol, row); res != nil {
			return res
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return &Result{}
}

// evalGeneratedRow evaluates newCol.Generated for one row and enforces its
// CHECK and NOT NULL constraints. A nil return means the row satisfied all
// constraints (or the generated expression could not be evaluated — such rows
// are skipped, matching SQLite). A non-nil return is a constraint failure.
func (e *DDLExecutor) evalGeneratedRow(tableEntry *schema.Entry, newCol sql.ColumnDef, row RowMap) *Result {
	genVal, gerr := e.ctx.EvalExpr(newCol.Generated, row)
	if gerr != nil {
		return nil
	}
	row[newCol.Name] = genVal
	// CHECK is evaluated before NOT NULL for each row, matching SQLite.
	if newCol.Check != nil {
		checkVal, verr := e.ctx.EvalExpr(newCol.Check, row)
		if verr == nil && checkVal != nil && !execexpr.ToBool(checkVal) {
			checkText := e.ctx.CheckConstraintText(tableEntry.SQL, newCol.Name, newCol.Check)
			return &Result{Error: fmt.Errorf("CHECK constraint failed: %s", checkText)}
		}
	}
	if newCol.NotNull && genVal == nil {
		return &Result{Error: fmt.Errorf("NOT NULL constraint failed: %s", newCol.Name)}
	}
	return nil
}

// validateAddConstraint evaluates a table-level constraint added by
// ALTER TABLE ... ADD CONSTRAINT against the table's existing rows. SQLite
// rejects the ALTER if any existing row violates the new constraint.
func (e *DDLExecutor) validateAddConstraint(tableName string, tableEntry *schema.Entry, tc *sql.TableConstraint) *Result {
	if !isCheckConstraint(tc) {
		return &Result{}
	}
	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	tree := e.ctx.TableBTreeForName(tableName, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{}
	}
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		row := e.ctx.BuildRowMap(rec, colDefs, cell.RowID)
		if res := e.checkConstraintRow(tableEntry, tc, row); res != nil {
			return res
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return &Result{}
}

// isCheckConstraint reports whether tc is a CHECK table constraint with an
// expression.
func isCheckConstraint(tc *sql.TableConstraint) bool {
	return tc != nil && tc.Type == sql.ConstraintCheck && tc.Expr != nil
}

// checkConstraintRow evaluates a CHECK table constraint for one row, returning
// a failure Result when the row violates it.
func (e *DDLExecutor) checkConstraintRow(tableEntry *schema.Entry, tc *sql.TableConstraint, row RowMap) *Result {
	checkVal, verr := e.ctx.EvalExpr(tc.Expr, row)
	if verr != nil {
		// A resolve error (e.g. an unknown function) is reported as-is
		// (altercons-10.2: ADD CONSTRAINT CHECK(sqlite_drop_column(...))
		// fails with "no such function: sqlite_drop_column").
		return &Result{Error: verr}
	}
	if checkVal != nil && !execexpr.ToBool(checkVal) {
		name := tc.Name
		if name == "" {
			name = sql.ExprString(tc.Expr)
		}
		return &Result{Error: fmt.Errorf("CHECK constraint failed: %s", name)}
	}
	return nil
}
func (e *DDLExecutor) columnHasNull(tableName string, entry *schema.Entry, colDefs []sql.ColumnDef, colName string) bool {
	tree := e.ctx.TableBTreeForName(tableName, entry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false
	}
	idx := findColDefIndex(colDefs, colName)
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		if idx >= 0 && (idx >= len(rec.Values) || rec.Values[idx] == nil) {
			return true
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return false
}

// findColDefIndex returns the index of the named column definition, or -1.
func findColDefIndex(colDefs []sql.ColumnDef, name string) int {
	for i, cd := range colDefs {
		if cd.Name == name {
			return i
		}
	}
	return -1
}
func (e *DDLExecutor) execAlterTableAdd(s *sql.AlterTableStmt) *Result {
	// ALTER TABLE ... ADD [COLUMN] column_def
	tableName := s.Table
	tableEntry, ctx, err := e.ctx.FindTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	// SQLite: virtual tables may not be altered (ALTER TABLE ADD COLUMN on a
	// virtual table reports "virtual tables may not be altered").
	if e.isVirtualTable(tableEntry) {
		return &Result{Error: fmt.Errorf("virtual tables may not be altered")}
	}

	// ALTER TABLE ... ADD [CONSTRAINT nm] CHECK(expr): append a table-level
	// constraint to the stored CREATE TABLE SQL and invalidate caches.
	if s.NewConstraint != nil {
		return e.addTableConstraint(tableName, tableEntry, ctx, s.NewConstraint)
	}
	return e.addTableColumn(tableName, tableEntry, ctx, s.ColDef)
}

// addTableConstraint appends a table-level constraint to the stored CREATE
// TABLE SQL after validating it against existing rows.
func (e *DDLExecutor) addTableConstraint(tableName string, tableEntry *schema.Entry, ctx *DatabaseContext, tc *sql.TableConstraint) *Result {
	// Validate the constraint against existing rows before committing.
	if vres := e.validateAddConstraint(tableName, tableEntry, tc); vres.Error != nil {
		return vres
	}
	newSQL := addConstraintToCreateTableSQL(tableEntry.SQL, tc)
	if newSQL == "" || newSQL == tableEntry.SQL {
		return &Result{}
	}
	tableEntry.SQL = newSQL
	e.ctx.DeleteTcCacheTable(tableName)
	if res := e.commitAlterTableEntry(tableName, ctx.Schema, tableEntry); res != nil {
		return res
	}
	return &Result{}
}

// addTableColumn appends a column definition to the stored CREATE TABLE SQL
// after validating its constraints against existing rows.
func (e *DDLExecutor) addTableColumn(tableName string, tableEntry *schema.Entry, ctx *DatabaseContext, colDef sql.ColumnDef) *Result {
	if colDef.Name == "" {
		return &Result{}
	}
	colDefs := e.ctx.ColCache()[tableName]
	if colDefs == nil {
		colDefs = e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}
	if hasColumnDefFold(colDefs, colDef.Name) {
		return &Result{Error: fmt.Errorf("duplicate column name: %q", colDef.Name)}
	}
	// STRICT tables only allow the standard datatypes (INT, INTEGER, REAL,
	// TEXT, BLOB, ANY). Reject custom or missing datatypes with SQLite's
	// "error in table ... after add column:" message.
	if res := validateStrictAddColumn(tableEntry, colDef); res != nil {
		return res
	}
	// Validate CHECK/NOT NULL against existing rows before committing.
	if vres := e.validateAddColumnConstraints(tableEntry, colDefs, colDef); vres.Error != nil {
		return vres
	}
	colDefs = append(colDefs, colDef)
	e.ctx.ColCache()[tableName] = colDefs

	// Update the stored CREATE TABLE SQL to include the new column.
	newSQL := addColumnToCreateTableSQL(tableEntry.SQL, colDef)
	if newSQL == "" || newSQL == tableEntry.SQL {
		return &Result{}
	}
	tableEntry.SQL = newSQL
	if res := e.commitAlterTableEntry(tableName, ctx.Schema, tableEntry); res != nil {
		return res
	}
	return &Result{}
}

// hasColumnDefFold reports whether a column definition with the given name
// (case-insensitive) exists.
func hasColumnDefFold(colDefs []sql.ColumnDef, name string) bool {
	for _, c := range colDefs {
		if strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}

// validateStrictAddColumn rejects custom or missing datatypes on STRICT tables
// with SQLite's "error in table ... after add column:" message.
func validateStrictAddColumn(tableEntry *schema.Entry, colDef sql.ColumnDef) *Result {
	if !execdml.HasStrictKeyword(strings.ToUpper(tableEntry.SQL)) {
		return nil
	}
	switch strings.ToUpper(strings.TrimSpace(colDef.Type)) {
	case "INT", "INTEGER", "REAL", "TEXT", "BLOB", "ANY":
		return nil
	case "":
		return &Result{Error: fmt.Errorf("error in table %s after add column: missing datatype for %s.%s",
			tableEntry.Name, tableEntry.Name, colDef.Name)}
	default:
		return &Result{Error: fmt.Errorf("error in table %s after add column: unknown datatype for %s.%s: %q",
			tableEntry.Name, tableEntry.Name, colDef.Name, colDef.Type)}
	}
}

// commitAlterTableEntry replaces a modified table entry in its schema manager
// and invalidates the engine's table cache. Returns a non-nil Result when the
// entry cannot be persisted.
func (e *DDLExecutor) commitAlterTableEntry(tableName string, schemaMgr *schema.Manager, tableEntry *schema.Entry) *Result {
	e.ctx.DeleteTableCache(tableName)
	_ = schemaMgr.RemoveEntry(tableEntry.Name)
	if err := schemaMgr.AddEntry(tableEntry); err != nil {
		return &Result{Error: fmt.Errorf("failed to re-add entry after DDL: %w", err)}
	}
	// Verify the entry was re-added.
	if _, err := schemaMgr.FindTable(tableEntry.Name); err != nil {
		if retryErr := schemaMgr.AddEntry(tableEntry); retryErr != nil {
			return &Result{Error: fmt.Errorf("schema consistency check failed: entry %s lost after DDL", tableEntry.Name)}
		}
	}
	return nil
}
func (e *DDLExecutor) execAlterTableDrop(s *sql.AlterTableStmt) *Result {
	tableName := s.Table

	// Handle DROP CONSTRAINT - remove named constraint from schema SQL
	if s.Column == "CONSTRAINT" {
		return e.dropTableConstraint(tableName, s.NewName)
	}
	return e.dropTableColumn(tableName, s.Column)
}

// dropTableConstraint removes a named constraint from the table's stored
// CREATE TABLE SQL.
func (e *DDLExecutor) dropTableConstraint(tableName, constraintName string) *Result {
	if constraintName == "" {
		return &Result{}
	}
	tableEntry, tableCtx, err := e.ctx.FindTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}
	schemaMgr := e.ctx.Schema()
	if tableCtx != nil && tableCtx.Schema != nil {
		schemaMgr = tableCtx.Schema
	}
	// Remove the named constraint from the CREATE TABLE SQL.
	if !sqlHasConstraintName(tableEntry.SQL, constraintName) {
		return &Result{Error: fmt.Errorf("no such constraint: %s", constraintName)}
	}
	newSQL := removeConstraintFromSQL(tableEntry.SQL, constraintName)
	if newSQL == tableEntry.SQL {
		return &Result{}
	}
	tableEntry.SQL = newSQL
	// Invalidate cached column/constraint info for this table.
	delete(e.ctx.ColCache(), tableEntry.Name)
	e.ctx.DeleteTcCacheTable(tableEntry.Name)
	if res := e.commitAlterTableEntry(tableName, schemaMgr, tableEntry); res != nil {
		return res
	}
	return &Result{}
}

// dropTableColumn removes a column from a table: it validates dependencies,
// rebuilds the CREATE TABLE SQL, and rewrites the table's rows.
func (e *DDLExecutor) dropTableColumn(tableName, columnName string) *Result {
	tableEntry, err := e.ctx.Schema().FindTable(tableName)
	if err != nil {
		// Check if it's a view.
		if viewEntry, viewErr := e.ctx.Schema().FindView(tableName); viewErr == nil && viewEntry != nil {
			return &Result{Error: fmt.Errorf("cannot drop column from view %q", tableName)}
		}
		// Return the table not found error.
		return &Result{Error: err}
	}
	// Check if it's a virtual table (has "USING" in SQL or uses a known module).
	if strings.Contains(tableEntry.SQL, "USING") || e.isVirtualTable(tableEntry) {
		return &Result{Error: fmt.Errorf("cannot drop column from virtual table %q", tableName)}
	}
	// Check if the table's SQL is malformed (doesn't look like a CREATE TABLE).
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(tableEntry.SQL)), "CREATE TABLE") {
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}
	if depResult := e.checkDropColumnDependencies(tableName, tableEntry, columnName); depResult != nil {
		return depResult
	}
	// Check if it's the sqlite_master system table.
	if isSchemaTableName(tableName) {
		return &Result{Error: fmt.Errorf("table sqlite_master may not be altered")}
	}
	return e.removeDroppedColumn(tableName, tableEntry, columnName)
}

// checkDropColumnDependencies runs the index/constraint/view/trigger dependency
// checks for DROP COLUMN.
func (e *DDLExecutor) checkDropColumnDependencies(tableName string, tableEntry *schema.Entry, columnName string) *Result {
	if depResult := e.checkIndexDependencies(tableName, columnName); depResult != nil {
		return depResult
	}
	if depResult := e.checkTableConstraintDependencies(tableEntry.SQL, tableName, columnName); depResult != nil {
		return depResult
	}
	if depResult := e.checkViewDependencies(tableName, columnName); depResult != nil {
		return depResult
	}
	if depResult := e.checkTriggerDependencies(tableName, columnName); depResult != nil {
		return depResult
	}
	return e.checkViewDropDependencies(tableName, columnName)
}

// isSchemaTableName reports whether tableName is one of the sqlite_schema
// system tables that DROP COLUMN may not alter.
func isSchemaTableName(tableName string) bool {
	return strings.EqualFold(tableName, "sqlite_master") ||
		strings.EqualFold(tableName, "sqlite_temp_master") ||
		strings.EqualFold(tableName, "sqlite_schema")
}

// removeDroppedColumn removes a column from the table's cached definitions,
// stored SQL, and on-disk rows.
func (e *DDLExecutor) removeDroppedColumn(tableName string, tableEntry *schema.Entry, columnName string) *Result {
	colDefs := e.ctx.ColCache()[tableName]
	if colDefs == nil {
		colDefs = e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}
	newColDefs, res := filterDroppedColumn(colDefs, columnName)
	if res != nil {
		return res
	}
	// Cannot drop the last remaining visible column.
	if visibleCount(newColDefs) == 0 {
		e.ctx.ColCache()[tableName] = colDefs // restore original column list
		return &Result{Error: fmt.Errorf("cannot drop column %q: no other columns exist", columnName)}
	}
	e.ctx.ColCache()[tableName] = newColDefs

	// Update the table's stored SQL to reflect the dropped column, using a
	// filtered list without dropped columns.
	sqlColDefs := visibleColDefs(newColDefs)
	updateSQL := rebuildCreateTableSQL(tableEntry.SQL, sqlColDefs)
	if updateSQL != "" {
		tableEntry.SQL = updateSQL
		if res := e.commitAlterTableEntry(tableName, e.ctx.Schema(), tableEntry); res != nil {
			return res
		}
	}

	// Rebuild the table's rows: SQLite's DROP COLUMN copies every row into a
	// fresh table without the dropped column, so the on-disk records no longer
	// contain a slot for it. Rewriting the records here (rather than relying on
	// the Dropped-flag position mapping) keeps reads correct even after the
	// colCache is invalidated by a later statement (e.g. PRAGMA page_count,
	// altertab3-31.2).
	e.rebuildRowsAfterDrop(tableEntry, newColDefs, columnName)

	// The rebuild removed the dropped column's slot from every record, so the
	// colCache must hold the visible definitions (no Dropped flag).
	e.ctx.ColCache()[tableName] = sqlColDefs

	return &Result{}
}

// filterDroppedColumn returns the column definitions with the named column
// marked Dropped (PRIMARY KEY/UNIQUE columns cannot be dropped), or a non-nil
// Result describing why the column cannot be dropped.
func filterDroppedColumn(colDefs []sql.ColumnDef, columnName string) ([]sql.ColumnDef, *Result) {
	found := false
	var out []sql.ColumnDef
	for _, c := range colDefs {
		if c.Name != columnName {
			out = append(out, c)
			continue
		}
		// Cannot drop PRIMARY KEY columns.
		if c.PrimaryKey {
			return nil, &Result{Error: fmt.Errorf("cannot drop PRIMARY KEY column: %q", columnName)}
		}
		// Cannot drop UNIQUE columns.
		if c.Unique {
			return nil, &Result{Error: fmt.Errorf("cannot drop UNIQUE column: %q", columnName)}
		}
		found = true
		// Mark as dropped but keep in the list for correct record position mapping.
		c.Dropped = true
		out = append(out, c)
	}
	if !found {
		return nil, &Result{Error: fmt.Errorf("no such column: \"%s\"", columnName)}
	}
	return out, nil
}

// visibleColDefs returns the column definitions with Dropped entries removed.
func visibleColDefs(colDefs []sql.ColumnDef) []sql.ColumnDef {
	var out []sql.ColumnDef
	for _, c := range colDefs {
		if !c.Dropped {
			out = append(out, c)
		}
	}
	return out
}

// visibleCount returns how many column definitions are not Dropped.
func visibleCount(colDefs []sql.ColumnDef) int {
	n := 0
	for _, c := range colDefs {
		if !c.Dropped {
			n++
		}
	}
	return n
}

// rebuildRowsAfterDrop rewrites every row of a table after DROP COLUMN,
// removing the dropped column's value from each record. The dropped column is
// identified by its name (it is the only Dropped-flagged definition in
// colDefs).
// dropRewrite holds one record to re-insert after DROP COLUMN.
type dropRewrite struct {
	rowID  int64
	values []interface{}
}

func (e *DDLExecutor) rebuildRowsAfterDrop(tableEntry *schema.Entry, colDefs []sql.ColumnDef, droppedName string) {
	// Find the dropped column's index in the OLD record layout.
	dropIdx := findColDefIndex(colDefs, droppedName)
	if dropIdx < 0 {
		return
	}
	// The engine stores generated-column values in the record (both VIRTUAL
	// and STORED are computed at INSERT and written to the row), so dropping a
	// generated column must remove its slot from each record — the early
	// return below would otherwise leave the slot in place and every later
	// SELECT * misreads columns to the right (alterdropcol-4.x). The scan and
	// rewrite must use the schema-qualified table name so the correct pager is
	// used for an ATTACHed table.
	tree := e.ctx.TableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	rewrites, rowIDs := e.collectDropRewrites(tree, colDefs, dropIdx)
	e.applyDropRewrites(tree, tableEntry, rewrites, rowIDs)
}

// collectDropRewrites scans the table and returns the records whose dropped
// column slot must be removed (each with its rowID) plus the set of rowIDs to
// delete. Short records written before ADD COLUMN are left unchanged.
func (e *DDLExecutor) collectDropRewrites(tree *btree.BTree, colDefs []sql.ColumnDef, dropIdx int) ([]dropRewrite, map[int64]bool) {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, nil
	}
	var rewrites []dropRewrite
	var rowIDs map[int64]bool
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		rewrites, rowIDs = addDropRewrite(rewrites, rowIDs, cell.RowID, collectDropRowValues(rec, dropIdx))
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return rewrites, rowIDs
}

// addDropRewrite appends a drop rewrite (skipping nil values — short records
// written before ADD COLUMN have no dropped slot) and returns the updated
// collections.
func addDropRewrite(rewrites []dropRewrite, rowIDs map[int64]bool, rowID int64, values []interface{}) ([]dropRewrite, map[int64]bool) {
	if values == nil {
		return rewrites, rowIDs
	}
	rewrites = append(rewrites, dropRewrite{rowID: rowID, values: values})
	if rowIDs == nil {
		rowIDs = make(map[int64]bool)
	}
	rowIDs[rowID] = true
	return rewrites, rowIDs
}

// collectDropRowValues returns the record values with the dropped column's
// slot removed, or nil when the short record has no such slot.
func collectDropRowValues(rec *storage.Record, dropIdx int) []interface{} {
	if dropIdx >= len(rec.Values) {
		return nil
	}
	values := make([]interface{}, 0, len(rec.Values)-1)
	values = append(values, rec.Values[:dropIdx]...)
	values = append(values, rec.Values[dropIdx+1:]...)
	return values
}

// applyDropRewrites deletes the original records and re-inserts them without
// the dropped column's slot (a single delete pass avoids the O(n²) per-row
// delete+insert that made DROP COLUMN on large tables take minutes,
// alterdropcol-9.x: 50000 rows).
func (e *DDLExecutor) applyDropRewrites(tree *btree.BTree, tableEntry *schema.Entry, rewrites []dropRewrite, rowIDs map[int64]bool) {
	if len(rewrites) == 0 {
		return
	}
	if _, err := tree.DeleteCellsWhere(func(c *storage.Cell) bool {
		return rowIDs[c.RowID]
	}); err != nil {
		return
	}
	e.ctx.InvalidateRowIDCache(e.ctx.TablePager(tableEntry.Name), tableEntry.RootPage)
	for _, rw := range rewrites {
		newRecord, err := storage.EncodeRecord(rw.values)
		if err != nil {
			continue
		}
		newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: rw.rowID, Payload: newRecord}
		_ = tree.InsertCell(newCell)
		e.ctx.BumpRowIDCache(e.ctx.TablePager(tableEntry.Name), tableEntry.RootPage, rw.rowID)
	}
}
func (e *DDLExecutor) execAlterTableAlter(s *sql.AlterTableStmt) *Result {
	// ALTER TABLE ... ALTER COLUMN SET NOT NULL / DROP NOT NULL
	if s.AlterColAction == "" {
		return &Result{}
	}
	tableName := s.Table
	// SQLite protects its internal tables ("table sqlite_schema may not be
	// altered") and rejects ALTER COLUMN on a view ("cannot edit constraints
	// of view").
	if isProtectedSystemTable(tableName) {
		return &Result{Error: fmt.Errorf("table %s may not be altered", tableName)}
	}
	if _, _, vErr := e.ctx.FindView(tableName); vErr == nil {
		return &Result{Error: fmt.Errorf("cannot edit constraints of view %q", tableName)}
	}
	tableEntry, tableCtx, err := e.ctx.FindTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	// Use the cached column definitions (from the original CREATE) when
	// available; a writable_schema edit may corrupt the stored SQL but the
	// in-memory table definition stays valid. Only report malformed when the
	// SQL is fundamentally broken AND no cached definition exists.
	colDefs := e.ctx.ColCache()[tableEntry.Name]
	if colDefs == nil {
		colDefs = e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}
	if isMalformedColumnDefs(colDefs, tableEntry.SQL) {
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}

	// Find and update the column.
	found, alterErr := applyAlterColumnAction(e, tableName, tableEntry, colDefs, s.Column, s.AlterColAction)
	if alterErr != nil {
		return &Result{Error: alterErr}
	}
	if !found {
		return &Result{Error: fmt.Errorf("no such column: \"%s\"", s.Column)}
	}
	e.ctx.ColCache()[tableEntry.Name] = colDefs

	// Rebuild the CREATE TABLE SQL with updated column definitions,
	// filtering out dropped columns.
	sqlColDefs := visibleColDefs(colDefs)
	updateSQL := rebuildCreateTableSQL(tableEntry.SQL, sqlColDefs)
	if updateSQL != "" {
		tableEntry.SQL = updateSQL
		schemaMgr := alterSchemaManager(e, tableCtx)
		if res := e.commitAlterTableEntry(tableName, schemaMgr, tableEntry); res != nil {
			return res
		}
	}

	return &Result{}
}

// isMalformedColumnDefs reports whether no column definitions could be parsed
// from a malformed CREATE TABLE SQL.
func isMalformedColumnDefs(colDefs []sql.ColumnDef, createSQL string) bool {
	return len(colDefs) == 0 && isMalformedCreateTableSQL(createSQL)
}

// alterSchemaManager returns the schema manager for an ALTER TABLE operation
// (the attached schema when present, otherwise the main schema).
func alterSchemaManager(e *DDLExecutor, tableCtx *DatabaseContext) *schema.Manager {
	if tableCtx != nil && tableCtx.Schema != nil {
		return tableCtx.Schema
	}
	return e.ctx.Schema()
}

// applyAlterColumnAction applies SET/DROP NOT NULL to the matching column
// definition, returning false when no column matches. SET NOT NULL fails when
// any existing row has NULL in the column.
func applyAlterColumnAction(e *DDLExecutor, tableName string, tableEntry *schema.Entry, colDefs []sql.ColumnDef, columnName, action string) (bool, error) {
	for i, c := range colDefs {
		if c.Name != columnName {
			continue
		}
		switch action {
		case "SET NOT NULL":
			// SQLite: SET NOT NULL fails with "constraint failed" if any
			// existing row has NULL in this column.
			if e.columnHasNull(tableName, tableEntry, colDefs, c.Name) {
				return true, fmt.Errorf("constraint failed")
			}
			colDefs[i].NotNull = true
		case "DROP NOT NULL":
			colDefs[i].NotNull = false
		}
		return true, nil
	}
	return false, nil
}

// removeLeadingConstraintClause removes the first CONSTRAINT <name> clause
// from a constraint-chain fragment and returns what follows (e.g. from
// "abc CONSTRAINT one CHECK(a!=b) CONSTRAINT three" it returns
// "CONSTRAINT one CHECK(a!=b) CONSTRAINT three").
func removeLeadingConstraintClause(rest, constraintName, quotedName, upperName, upperQuotedName string) string {
	tailUpper := strings.ToUpper(rest)
	nameEnd := 0
	if strings.HasPrefix(tailUpper, upperQuotedName) {
		nameEnd = len(quotedName)
	} else if strings.HasPrefix(tailUpper, upperName) {
		nameEnd = len(constraintName)
	}
	// Skip whitespace and comments after the name (e.g.
	// "CONSTRAINT abc /* hello */ CHECK(...)").
	i := skipSQLWhitespaceAndComments(rest, nameEnd)
	// If the next token is CONSTRAINT, the removed clause had no type keyword
	// (bare "CONSTRAINT abc" in a chain) — the remainder starts here.
	if strings.HasPrefix(strings.ToUpper(rest[i:]), "CONSTRAINT") {
		return strings.TrimSpace(rest[i:])
	}
	// The next token is the constraint type keyword (CHECK, UNIQUE, ...).
	i, kwUpper := skipConstraintKeyword(rest, i)
	i = skipSQLWhitespaceAndComments(rest, i)
	i = skipParenGroup(rest, i)
	// FOREIGN KEY (cols) REFERENCES <table>(cols): skip the REFERENCES target
	// too so the whole clause is removed (altercons3-4.2/4.3).
	if kwUpper == "FOREIGN" {
		i = skipFKConstraintTail(rest, i)
	}
	// Everything after the removed clause is the remainder (a following
	// CONSTRAINT <name> ... chain or end of the part).
	return strings.TrimSpace(rest[i:])
}

// removeColumnLevelConstraint detects a column-level "colName CONSTRAINT name ..."
// clause in a column definition and returns the column part with the named
// CONSTRAINT clause removed (keeping any clauses that follow it), or ok=false
// when the part has no matching column-level constraint.
func removeColumnLevelConstraint(part, upperPart, upperName, upperQuotedName, quotedName, constraintName string) (string, bool) {
	// Find " CONSTRAINT " within the part and check if the following name matches.
	conIdx := strings.Index(upperPart, " CONSTRAINT ")
	if conIdx < 0 {
		return part, false
	}
	tail := strings.TrimSpace(part[conIdx+11:]) // after " CONSTRAINT "
	tailUpper := strings.ToUpper(tail)
	if !strings.HasPrefix(tailUpper, upperName) && !strings.HasPrefix(tailUpper, upperQuotedName) {
		return part, false
	}
	// Skip the constraint name, then the constraint type keyword and its
	// parenthesized expression. clauseStart points at the next clause keyword
	// or end of part.
	nameEnd := 0
	if strings.HasPrefix(tailUpper, upperQuotedName) {
		nameEnd = len(quotedName)
	} else if strings.HasPrefix(tailUpper, upperName) {
		nameEnd = len(constraintName)
	}
	clauseStart := skipColumnConstraintTail(tail, nameEnd)
	part = strings.TrimSpace(part[:conIdx])
	if rest2 := strings.TrimSpace(tail[clauseStart:]); rest2 != "" {
		part += " " + rest2
	}
	return part, true
}

// removeConstraintFromSQL removes a named constraint from a CREATE TABLE SQL string.
func removeConstraintFromSQL(origSQL, constraintName string) string {
	if !strings.Contains(strings.ToUpper(origSQL), "CREATE TABLE") {
		return origSQL
	}
	parenStart, parenEnd := findOuterParens(origSQL)
	if parenStart < 0 {
		return origSQL
	}
	if parenEnd < 0 {
		// No closing paren — treat end of string as the virtual closing paren.
		parenEnd = len(origSQL)
	}
	trailingSQL := ""
	if parenEnd+1 < len(origSQL) {
		trailingSQL = strings.TrimSpace(origSQL[parenEnd+1:])
	}
	upperName := strings.ToUpper(constraintName)
	quotedName := `"` + constraintName + `"`
	upperQuotedName := strings.ToUpper(quotedName)
	keptParts := filterDroppedConstraintParts(splitTopLevelParts(origSQL[parenStart+1:parenEnd]),
		constraintName, upperName, quotedName, upperQuotedName)

	// Rebuild the SQL.
	var buf strings.Builder
	buf.WriteString(origSQL[:parenStart+1])
	for i, part := range keptParts {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(part)
	}
	buf.WriteString(")")
	if trailingSQL != "" {
		buf.WriteString(" ")
		buf.WriteString(trailingSQL)
	}
	return buf.String()
}

// addConstraintToCreateTableSQL appends a table-level constraint (e.g.
// CONSTRAINT nm CHECK(expr)) to the stored CREATE TABLE SQL, inserting it
// before the closing parenthesis of the column list.
func addConstraintToCreateTableSQL(origSQL string, tc *sql.TableConstraint) string {
	if tc == nil {
		return origSQL
	}
	parenStart, parenEnd := findOuterParens(origSQL)
	if parenStart < 0 || parenEnd < 0 {
		return origSQL
	}
	trailingSQL := ""
	if parenEnd+1 < len(origSQL) {
		trailingSQL = strings.TrimSpace(origSQL[parenEnd+1:])
	}

	var buf strings.Builder
	buf.WriteString(origSQL[:parenEnd])
	if parenEnd > parenStart && !strings.HasSuffix(strings.TrimRight(origSQL[:parenEnd], " \t\n"), ",") {
		buf.WriteString(",")
	}
	buf.WriteString(" ")
	writeConstraintClause(&buf, tc)
	buf.WriteString(")")
	if trailingSQL != "" {
		buf.WriteString(" ")
		buf.WriteString(trailingSQL)
	}
	return buf.String()
}

// addColumnToCreateTableSQL adds a new column definition to a CREATE TABLE SQL string.
func addColumnToCreateTableSQL(origSQL string, colDef sql.ColumnDef) string {
	upper := strings.ToUpper(strings.TrimSpace(origSQL))
	if !strings.HasPrefix(upper, "CREATE TABLE") && !strings.HasPrefix(upper, "CREATE TEMP TABLE") && !strings.HasPrefix(upper, "CREATE TEMPORARY TABLE") {
		return ""
	}
	parenStart, parenEnd := findOuterParens(origSQL)
	if parenStart < 0 || parenEnd < 0 {
		return ""
	}

	// Build the column definition text.
	var colBuf strings.Builder
	formatColumnDef(&colBuf, colDef)
	colText := colBuf.String()
	if colText == "" {
		return origSQL
	}

	insertAt := findConstraintInsertPoint(origSQL, parenStart, parenEnd)
	return origSQL[:insertAt] + ", " + colText + origSQL[insertAt:]
}
