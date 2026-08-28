package exec

import (
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// Shared regexes moved with the DML sub-package.
var indexWhereRe = execdml.IndexWhereRe
var uniqueIndexColsRe = execdml.UniqueIndexColsRe

func parseSchemaName(name string) (schema string, object string) {
	return execdml.ParseSchemaName(name)
}

// This file forwards package-level helpers and Engine methods that moved to
// internal/execdml. The execution engine keeps thin same-named wrappers so
// existing call sites (DDL, FK checks, ALTER, PRAGMA) stay unchanged while
// the implementation lives in the DML sub-package.

// --- Package-level helper forwarders (defined in internal/execdml) ---

func hasStrictKeyword(upperSQL string) bool {
	return execdml.HasStrictKeyword(upperSQL)
}

func hasWithoutRowidKeyword(upperSQL string) bool {
	return execdml.HasWithoutRowidKeyword(upperSQL)
}

func parseIndexColumns(sqlStr string) []string {
	return execdml.ParseIndexColumns(sqlStr)
}

func buildColumnIndex(colDefs []sql.ColumnDef) map[string]int {
	return execdml.BuildColumnIndex(colDefs)
}

func buildRowMapFromValues(values []interface{}, colDefs []sql.ColumnDef, rowID int64) RowMap {
	return execdml.BuildRowMapFromValues(values, colDefs, rowID)
}

func parseWhereExpr(whereSQL string) sql.Expr {
	return execdml.ParseWhereExpr(whereSQL)
}

func stripHiddenToken(typ string) (string, bool) {
	return execdml.StripHiddenToken(typ)
}

// --- Engine method forwarders (defined in internal/execdml) ---

func (e *Engine) viewColumnNames(sel *sql.SelectStmt) []string {
	return e.dml.ViewColumnNames(sel)
}

func (e *Engine) planOrIndexScan(where sql.Expr, tableName string, colDefs []sql.ColumnDef, ctx *DatabaseContext) ([]orBranchPlan, bool) {
	return e.dml.PlanOrIndexScan(where, tableName, colDefs, ctx)
}

func (e *Engine) execSelectWithOrPlan(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, branches []orBranchPlan) *Result {
	return e.dml.ExecSelectWithOrPlan(s, tableEntry, dbCtx, colDefs, branches)
}

func (e *Engine) fireBeforeDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.dml.FireBeforeDeleteTriggers(tableName, oldRow)
}

func (e *Engine) fireAfterDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.dml.FireAfterDeleteTriggers(tableName, oldRow)
}

func (e *Engine) hasTriggersForTable(tableName string) bool {
	return e.dml.HasTriggersForTable(tableName)
}

func (e *Engine) checkCollationString(name string) error {
	return e.dml.CheckCollationString(name)
}

func (e *Engine) validateLoadedTriggers() error {
	return e.dml.ValidateLoadedTriggers()
}

func (e *Engine) uniqueIndexColumns(tableName string) []uniqueIndexDef {
	return e.dml.UniqueIndexColumns(tableName)
}

func (e *Engine) insertRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, fixedRowID *int64, orConflict string) *Result {
	return e.dml.InsertRow(pg, tableEntry, colDefs, values, fixedRowID, orConflict)
}
