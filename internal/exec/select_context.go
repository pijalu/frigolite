package exec

import (
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

// This file implements the execquery.SelectContext capability interface on
// the Engine. The SELECT engine (internal/execquery) depends on this
// interface rather than on the concrete Engine type (Dependency Inversion).

// Compile-time probe: Engine implements execquery.SelectContext.
var _ execquery.SelectContext = (*Engine)(nil)

// FindTable resolves a table name to its schema entry (exported wrapper for
// the SelectContext interface).
func (e *Engine) FindTable(name string) (*schema.Entry, *execquery.DatabaseContext, error) {
	return e.findTable(name)
}

// FindView resolves a view name to its schema entry (exported wrapper for the
// SelectContext interface).
func (e *Engine) FindView(name string) (*schema.Entry, *execquery.DatabaseContext, error) {
	return e.findView(name)
}

// SubqueryColumnCount returns the output column count of a subquery SELECT
// (exported wrapper for the execexpr.ExprContext interface).
func (e *Engine) SubqueryColumnCount(s *sql.SelectStmt) int {
	return e.subqueryColumnCount(s)
}

func (e *Engine) Schema() *schema.Manager { return e.schema }

func (e *Engine) Pager() *pager.Pager { return e.pager }

func (e *Engine) MainDB() *execquery.DatabaseContext { return e.mainDB }

func (e *Engine) VTables() *vtab.Registry { return e.vtabs }

func (e *Engine) Expr() *execexpr.Evaluator { return e.expr }

func (e *Engine) CurrentDMLCtx() *execquery.DatabaseContext { return e.dml.CurrentDMLCtx() }

func (e *Engine) ParseColumnDefs(tableName, createSQL string) []sql.ColumnDef {
	return e.parseColumnDefs(tableName, createSQL)
}

func (e *Engine) TableConstraints(tableName, createSQL string) []sql.TableConstraint {
	return e.tableConstraints(tableName, createSQL)
}

func (e *Engine) TableHasColumn(tableName, colName string) bool {
	return e.tableHasColumn(tableName, colName)
}

func (e *Engine) UniqueIndexColumns(tableName string) []execquery.UniqueIndexDef {
	return e.uniqueIndexColumns(tableName)
}

func (e *Engine) WithoutRowidPKColumns(tableName string, tableEntry *schema.Entry, colDefs []sql.ColumnDef, xinfo bool) []execquery.IndexPragmaColumn {
	return e.withoutRowidPKColumns(tableName, tableEntry, colDefs, xinfo)
}

func (e *Engine) EvalExpr(expr sql.Expr, row Row) (interface{}, error) {
	return e.evalExpr(expr, row)
}

func (e *Engine) EvalFuncCall(f *sql.FuncCall, row Row) (interface{}, error) {
	return e.evalFuncCall(f, row)
}

func (e *Engine) EvalBool(expr sql.Expr, row Row) (bool, error) {
	return e.evalBool(expr, row)
}

func (e *Engine) EvalSubquery(v *sql.Subquery, row Row) (interface{}, error) {
	return e.evalSubquery(v, row)
}

func (e *Engine) CheckCollationString(name string) error {
	return e.checkCollationString(name)
}

func (e *Engine) TableBTree(tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return e.tableBTree(tableName, schemaRoot, isTable)
}

func (e *Engine) TableBTreeForName(tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return e.tableBTreeForName(tableName, schemaRoot, isTable)
}

func (e *Engine) TableBTreePg(pg *pager.Pager, tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return e.tableBTreePg(pg, tableName, schemaRoot, isTable)
}

func (e *Engine) VirtualTableRows(entry *schema.Entry, bound int64, input string, hasInput bool) ([][]interface{}, error) {
	return e.virtualTableRows(entry, bound, input, hasInput)
}

// MaterializeCorrelatedVTab materializes an input-constrained virtual table per
// left row (see execquery.DatabaseContext.MaterializeCorrelatedVTab).
func (e *Engine) MaterializeCorrelatedVTab(entry *schema.Entry, leftColName string, leftRows []RowMap) ([]sql.ColumnDef, []RowMap, []int, error) {
	return e.ddl.MaterializeCorrelatedVTab(entry, leftColName, leftRows)
}

func (e *Engine) ExecFTSSelect(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef) *Result {
	return e.execFTSSelect(s, tableEntry, ftsTable, colDefs)
}

func (e *Engine) ExecPragmaTableValued(s *sql.SelectStmt) *Result {
	return e.execPragmaTableValued(s)
}

func (e *Engine) MaterializePragmaTable(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	return e.materializePragmaTable(ref)
}

// TryMaterializeEponymousVtab resolves a bare FROM reference to an
// eponymous-only module's implicit table (see tryMaterializeEponymousVtab).
func (e *Engine) TryMaterializeEponymousVtab(ref sql.TableRef, opts execquery.VtabScanOptions) ([]sql.ColumnDef, [][]interface{}, []int64, error, bool) {
	return e.tryMaterializeEponymousVtab(ref, opts)
}

// MaterializeVtabTableFunc materializes a table-valued vtab reference with
// hidden-constraint pushdown from the enclosing WHERE clause.
func (e *Engine) MaterializeVtabTableFunc(ref sql.TableRef, opts execquery.VtabScanOptions) ([]sql.ColumnDef, [][]interface{}, []int64, error) {
	return e.materializeVtabTableFunc(ref, opts)
}

// MaterializeVtabTableFuncInRow materializes a table-valued vtab function
// with arguments evaluated against an outer row (see execquery.DatabaseContext).
func (e *Engine) MaterializeVtabTableFuncInRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	return e.materializeVtabTableFuncInRow(ref, row)
}

// MaterializeCorrelatedVTabFunc materializes a table-valued vtab function per
// left row (see execquery.DatabaseContext.MaterializeCorrelatedVTabFunc).
func (e *Engine) MaterializeCorrelatedVTabFunc(ref sql.TableRef, leftRows []RowMap, where sql.Expr) ([]sql.ColumnDef, []RowMap, []int, error) {
	return e.materializeCorrelatedVTabFunc(ref, leftRows, where)
}

func (e *Engine) MaterializeCorrelatedPragma(ref sql.TableRef, leftRows []RowMap) ([]sql.ColumnDef, []RowMap, []int, error) {
	return e.materializeCorrelatedPragma(ref, leftRows)
}

func (e *Engine) ViewColumnDefs(viewEntry *schema.Entry) ([]sql.ColumnDef, error) {
	return e.viewColumnDefs(viewEntry)
}

func (e *Engine) ViewColumnDefsFromSelect(sel *sql.SelectStmt) []sql.ColumnDef {
	return e.viewColumnDefsFromSelect(sel)
}

func (e *Engine) ViewColumnNames(sel *sql.SelectStmt) []string {
	return e.viewColumnNames(sel)
}

func (e *Engine) PlanOrIndexScan(where sql.Expr, tableName string, colDefs []sql.ColumnDef, ctx *execquery.DatabaseContext) ([]execquery.OrBranchPlan, bool) {
	return e.planOrIndexScan(where, tableName, colDefs, ctx)
}

func (e *Engine) ExecSelectWithOrPlan(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *execquery.DatabaseContext, colDefs []sql.ColumnDef, branches []execquery.OrBranchPlan) *Result {
	return e.execSelectWithOrPlan(s, tableEntry, dbCtx, colDefs, branches)
}

func (e *Engine) ValidateIndexedBy(tableEntry *schema.Entry, indexName string, s *sql.SelectStmt) error {
	return e.validateIndexedBy(tableEntry, indexName, s)
}

func (e *Engine) ValidateNoRaiseOutsideTrigger(stmt sql.Stmt) error {
	return e.validateNoRaiseOutsideTrigger(stmt)
}

func (e *Engine) CheckProgress() error {
	return e.checkProgress()
}

func (e *Engine) IndexColumnCount(idxName string) int {
	return e.indexColumnCount(idxName)
}

func (e *Engine) ParseIndexColumns(sqlStr string) []string {
	return parseIndexColumns(sqlStr)
}

func (e *Engine) HasWithoutRowidKeyword(upperSQL string) bool {
	return hasWithoutRowidKeyword(upperSQL)
}

func (e *Engine) ReverseUnordered() bool { return e.settings.reverseUnordered }

func (e *Engine) ExprDepthLimit() int { return e.settings.exprDepthLimit }

// CompoundColumnAffinity returns the storage affinity of column i of a
// compound SELECT result (delegated to the SELECT engine).
func (e *Engine) CompoundColumnAffinity(s *sql.SelectStmt, i int) rune {
	return e.selectEngine.CompoundColumnAffinity(s, i)
}

// FindCTE resolves a CTE by name in the current scope (delegated).
func (e *Engine) FindCTE(s *sql.SelectStmt, name string) (sql.CTEDef, bool) {
	return e.selectEngine.FindCTE(s, name)
}

// FindCTEByName resolves a CTE by name in the current scope without a SELECT
// statement (used by UPDATE ... FROM / DELETE ... FROM to resolve a CTE as a
// FROM table).
func (e *Engine) FindCTEByName(name string) (sql.CTEDef, bool) {
	return e.selectEngine.FindCTEByScope(name)
}

// ResolveAliasRef resolves an output-column alias reference (delegated).
func (e *Engine) ResolveAliasRef(name string) (sql.Expr, bool) {
	return e.selectEngine.ResolveAliasRef(name)
}

// EvalAggFuncCall evaluates an aggregate function call over row maps
// (delegated to the SELECT engine's aggregate evaluator).
func (e *Engine) EvalAggFuncCall(v *sql.FuncCall, rowMaps []RowMap) (interface{}, error) {
	return e.selectEngine.EvalAggFuncCall(v, rowMaps)
}
