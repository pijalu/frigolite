package exec

import (
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// This file delegates SELECT-family execution to the internal/execquery
// package. The Engine keeps thin forwarding methods so the rest of the
// execution engine (DML, DDL, RETURNING, triggers, PRAGMA) can continue to
// call the SELECT entry points without depending on the query sub-package.

// execSelect executes a SELECT statement (delegated to execquery).
func (e *Engine) execSelect(s *sql.SelectStmt) *Result {
	// A read inside an open transaction takes a transaction-level SHARED lock
	// (held until COMMIT/ROLLBACK) so another connection's COMMIT cannot upgrade
	// to EXCLUSIVE — lock2 PENDING model. Auto-commit reads take no mark.
	e.registerSharedTx(selectSchema(s))
	return e.selectEngine.ExecSelect(s)
}

// execSelectView executes a SELECT over a view body (delegated to execquery).
func (e *Engine) execSelectView(viewEntry *schema.Entry) *Result {
	return e.selectEngine.ExecSelectView(viewEntry)
}

// execSelectOverMaterialized executes a SELECT over materialized rows.
func (e *Engine) execSelectOverMaterialized(s *sql.SelectStmt, colDefs []sql.ColumnDef, rows [][]interface{}) *Result {
	return e.selectEngine.ExecSelectOverMaterialized(s, colDefs, rows)
}

// execExplain executes an EXPLAIN statement (delegated to execquery).
func (e *Engine) execExplain(s *sql.ExplainStmt) *Result {
	return e.selectEngine.ExecExplain(s)
}

// handleSelectAggregates runs aggregate processing for a SELECT.
func (e *Engine) handleSelectAggregates(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	return e.selectEngine.HandleSelectAggregates(s, rowMaps, colDefs)
}

// finalizeSelectResult applies ORDER BY/LIMIT and final column naming.
func (e *Engine) finalizeSelectResult(result *Result, s *sql.SelectStmt, rowMaps []RowMap) *Result {
	return e.selectEngine.FinalizeSelectResult(result, s, rowMaps)
}

// buildColumnNames builds the result column names for a SELECT.
func (e *Engine) buildColumnNames(columns []sql.SelectColumn, colDefs []sql.ColumnDef, sel *sql.SelectStmt) []string {
	return e.selectEngine.BuildColumnNames(columns, colDefs, sel)
}

// buildRowMap builds a RowMap from a stored record.
func (e *Engine) buildRowMap(rec *storage.Record, colDefs []sql.ColumnDef, rowID int64) RowMap {
	return e.selectEngine.BuildRowMap(rec, colDefs, rowID)
}

// buildOutputRow builds one output row from an expression list. Used by the
// DML/DDL RETURNING path via BuildOutputRow (errors render NULL); the
// execquery SELECT path calls BuildOutputRowWithErr for error propagation.
func (e *Engine) buildOutputRow(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row Row) ([]interface{}, error) {
	return e.selectEngine.BuildOutputRow(columns, colDefs, row)
}

// BuildOutputRowWithErr is the error-propagating variant used by the
// execquery SELECT scan path.
func (e *Engine) BuildOutputRowWithErr(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row Row) ([]interface{}, error) {
	return e.selectEngine.BuildOutputRow(columns, colDefs, row)
}

// rowPassesWhere evaluates a WHERE predicate against a row.
func (e *Engine) rowPassesWhere(where sql.Expr, row Row, cursor *btree.Cursor) (bool, error) {
	return e.selectEngine.RowPassesWhere(where, row, cursor)
}

// tableColumnNames returns the declared column names of a table.
func (e *Engine) tableColumnNames(tableName string) ([]string, error) {
	return e.selectEngine.TableColumnNames(tableName)
}

// subqueryColumnCount returns the output column count of a subquery SELECT.
func (e *Engine) subqueryColumnCount(s *sql.SelectStmt) int {
	return e.selectEngine.SubqueryColumnCount(s)
}

// validateRowValueUse validates row-value usage in an expression.
func (e *Engine) validateRowValueUse(expr sql.Expr, topLevel bool) error {
	return e.selectEngine.ValidateRowValueUse(expr, topLevel)
}

// validateDMLSubqueries validates subqueries inside DML statements.
func (e *Engine) validateDMLSubqueries(stmt sql.Stmt) error {
	return e.selectEngine.ValidateDMLSubqueries(stmt)
}

// compareOrderByValues compares two values under an ORDER BY term.
func (e *Engine) compareOrderByValues(left, right interface{}, ob sql.OrderByTerm) int {
	return e.selectEngine.CompareOrderByValues(left, right, ob)
}
