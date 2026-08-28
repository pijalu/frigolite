package execdml

import (
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// This file exposes the DML sub-package's helpers and DMLExecutor methods to
// internal/exec, which keeps thin forwarders so existing call sites (DDL, FK
// checks, ALTER, PRAGMA) stay unchanged.

// --- Package-level helper exports (forwarded from internal/exec) ---

// EnforceStrictType enforces a STRICT table column type on a stored value.
func EnforceStrictType(tableName, colName, declaredType string, v interface{}) error {
	return enforceStrictType(tableName, colName, declaredType, v)
}

// HasStrictKeyword checks if "STRICT" appears after the CREATE TABLE closing
// parenthesis.
func HasStrictKeyword(upperSQL string) bool {
	return hasStrictKeyword(upperSQL)
}

// HasWithoutRowidKeyword checks if "WITHOUT ROWID" appears after the closing
// parenthesis in the CREATE TABLE SQL.
func HasWithoutRowidKeyword(upperSQL string) bool {
	return hasWithoutRowidKeyword(upperSQL)
}

// IsStrictTable returns true if the table's CREATE SQL specifies STRICT.
func IsStrictTable(createSQL string) bool {
	return isStrictTable(createSQL)
}

// StripCTASSelect returns the CREATE TABLE text up to (but not including) an
// "AS SELECT" clause.
func StripCTASSelect(createSQL string) string {
	return stripCTASSelect(createSQL)
}

// ParseIndexColumns extracts indexed column names from a CREATE INDEX SQL.
func ParseIndexColumns(sqlStr string) []string {
	return parseIndexColumns(sqlStr)
}

// ParseSchemaName splits a possibly schema-qualified name into its schema
// prefix and object name.
func ParseSchemaName(name string) (schema string, object string) {
	return parseSchemaName(name)
}

// BuildColumnIndex builds a column-name-to-position index.
func BuildColumnIndex(colDefs []sql.ColumnDef) map[string]int {
	return buildColumnIndex(colDefs)
}

// BuildRowMapFromValues creates a column-name-to-value map from a values
// slice.
func BuildRowMapFromValues(values []interface{}, colDefs []sql.ColumnDef, rowID int64) RowMap {
	return buildRowMapFromValues(values, colDefs, rowID)
}

// SplitIndexCols splits a CREATE INDEX column-list text into its parts.
func SplitIndexCols(colText string) []string {
	return splitIndexCols(colText)
}

// CdIndex returns the index of the named column in the column definitions
// (-1 when absent).
func CdIndex(colDefs []sql.ColumnDef, name string) int {
	return cdIndex(colDefs, name)
}

// IsIPKRowidAliasCol reports whether a column is an INTEGER PRIMARY KEY
// rowid-alias candidate.
func IsIPKRowidAliasCol(cd sql.ColumnDef) bool {
	return isIPKRowidAliasCol(cd)
}

// IndexColumnListText extracts the column-list text of a CREATE INDEX.
func IndexColumnListText(entSQL string) string {
	return indexColumnListText(entSQL)
}

// ParseWhereExpr parses a WHERE SQL fragment into an expression.
func ParseWhereExpr(whereSQL string) sql.Expr {
	return parseWhereExpr(whereSQL)
}

// StripHiddenToken removes a standalone "hidden" word from a column type
// string and reports whether one was found.
func StripHiddenToken(typ string) (string, bool) {
	return stripHiddenToken(typ)
}

// --- DMLExecutor method exports (forwarded from internal/exec) ---

// CheckConstraintText formats a column-level CHECK constraint's SQL text.
func (e *DMLExecutor) CheckConstraintText(createSQL, colName string, check sql.Expr) string {
	return e.checkConstraintText(createSQL, colName, check)
}

// FireBeforeDeleteTriggers fires BEFORE DELETE triggers for a deleted row.
func (e *DMLExecutor) FireBeforeDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.fireBeforeDeleteTriggers(tableName, oldRow)
}

// FireAfterDeleteTriggers fires AFTER DELETE triggers for a deleted row.
func (e *DMLExecutor) FireAfterDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.fireAfterDeleteTriggers(tableName, oldRow)
}

// HasTriggersForTable returns true if any triggers exist for the table.
func (e *DMLExecutor) HasTriggersForTable(tableName string) bool {
	return e.hasTriggersForTable(tableName)
}

// EvalConstInt evaluates a constant integer expression.
func (e *DMLExecutor) EvalConstInt(expr sql.Expr) (int64, error) {
	return e.evalConstInt(expr)
}

// CheckCollationString validates a collation name.
func (e *DMLExecutor) CheckCollationString(name string) error {
	return e.checkCollationString(name)
}

// ValidateLoadedTriggers checks every trigger loaded from sqlite_master for
// schema references that no longer resolve.
func (e *DMLExecutor) ValidateLoadedTriggers() error {
	return e.validateLoadedTriggers()
}

// UniqueIndexColumns returns the UNIQUE indexes defined on the given table.
func (e *DMLExecutor) UniqueIndexColumns(tableName string) []uniqueIndexDef {
	return e.uniqueIndexColumns(tableName)
}

// InsertRow inserts one row into a table (used by DDL/PRAGMA write paths).
func (e *DMLExecutor) InsertRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, fixedRowID *int64, orConflict string) *Result {
	return e.insertRow(pg, tableEntry, colDefs, values, fixedRowID, orConflict)
}

// TableBTreeForDML builds a BTree for DML row access using the tracked root
// page.
func (e *DMLExecutor) TableBTreeForDML(tableEntry *schema.Entry, rootPage uint32) *btree.BTree {
	return e.tableBTreeForDML(tableEntry, rootPage)
}

// ViewColumnNames returns the output column names of a SELECT (delegated to
// the SELECT engine; kept here for the Engine's SelectContext implementation).
func (e *DMLExecutor) ViewColumnNames(sel *sql.SelectStmt) []string {
	return e.viewColumnNames(sel)
}

// PlanOrIndexScan plans an OR-index scan for a WHERE predicate (delegated).
func (e *DMLExecutor) PlanOrIndexScan(where sql.Expr, tableName string, colDefs []sql.ColumnDef, ctx *DatabaseContext) ([]orBranchPlan, bool) {
	return e.planOrIndexScan(where, tableName, colDefs, ctx)
}

// ExecSelectWithOrPlan executes a single-table SELECT whose WHERE was planned
// by PlanOrIndexScan (delegated).
func (e *DMLExecutor) ExecSelectWithOrPlan(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, branches []orBranchPlan) *Result {
	return e.execSelectWithOrPlan(s, tableEntry, dbCtx, colDefs, branches)
}
