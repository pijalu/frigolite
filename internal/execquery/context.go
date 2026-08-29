package execquery

import (
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/vtab"
)

// VtabScanOptions carries per-scan context into virtual-table
// materialization: the enclosing WHERE clause (hidden-constraint pushdown)
// and a row cap derived from the query's LIMIT/OFFSET (series.c LIMIT
// pushdown parity — an omitted STOP defaults to 4294967295, so generation
// must stop once the LIMIT is satisfied).
type VtabScanOptions struct {
	Where   sql.Expr
	MaxRows int64 // -1 = unlimited
	// Residual, when non-nil, is filled in by the materializer with the
	// WHERE clause that remains after constraints the virtual table consumed
	// are omitted (series.c argvConsumed/omit parity).
	Residual *sql.Expr
}

// SelectContext is the capability interface SELECT execution needs from the
// execution engine. The Engine in internal/exec implements it; the
// SelectEngine depends on this interface rather than on the concrete engine
// type (Dependency Inversion).
type SelectContext interface {
	// Engine resources.
	Functions() *function.Registry
	Schema() *schema.Manager
	Pager() *pager.Pager
	MainDB() *DatabaseContext
	VTables() *vtab.Registry
	FullColumnNames() bool
	FTSTables() map[string]*fts.FTS3Table
	Expr() *execexpr.Evaluator

	// SkipScanEnabled reports whether the skip-scan query optimization is on.
	// Mirrors SQLite's SQLITE_SkipScan optimization_control bit. Used by the
	// query planner's skip-scan detection.
	SkipScanEnabled() bool

	// Trigger/DML context for SELECT-time trigger validation.
	TriggerOldRow() Row
	TriggerNewRow() Row
	TriggerDepth() int
	CurrentDMLCtx() *DatabaseContext

	// Schema lookup.
	FindTable(name string) (*schema.Entry, *DatabaseContext, error)
	FindView(name string) (*schema.Entry, *DatabaseContext, error)
	ParseColumnDefs(tableName, createSQL string) []sql.ColumnDef
	TableConstraints(tableName, createSQL string) []sql.TableConstraint
	TableHasColumn(tableName, colName string) bool
	UniqueIndexColumns(tableName string) []UniqueIndexDef
	WithoutRowidPKColumns(tableName string, tableEntry *schema.Entry, colDefs []sql.ColumnDef, xinfo bool) []IndexPragmaColumn

	// FKChildTableNames returns the child table names whose FOREIGN KEY
	// constraints reference the given parent table (used by EXPLAIN QUERY
	// PLAN to model FK-check scans on parent DELETE/UPDATE).
	FKChildTableNames(tableName string) []string

	// Expression evaluation (delegates to the execexpr Evaluator).
	EvalExpr(expr sql.Expr, row Row) (interface{}, error)
	EvalFuncCall(f *sql.FuncCall, row Row) (interface{}, error)
	EvalBool(expr sql.Expr, row Row) (bool, error)
	EvalSubquery(v *sql.Subquery, row Row) (interface{}, error)
	CompareValuesCollate(a, b interface{}, collation string) int
	CompareValuesWithCollate(left, right interface{}) int
	CheckCollationString(name string) error

	// Table access.
	TableBTree(tableName string, schemaRoot uint32, isTable bool) *btree.BTree
	TableBTreeForName(tableName string, schemaRoot uint32, isTable bool) *btree.BTree
	TableBTreePg(pg *pager.Pager, tableName string, schemaRoot uint32, isTable bool) *btree.BTree
	VirtualTableRows(entry *schema.Entry, bound int64, input string, hasInput bool) ([][]interface{}, error)
	// MaterializeCorrelatedVTab materializes an input-constrained virtual table
	// per left row: for each left row, the value of leftColName becomes the
	// vtab's input constraint (fts3tokenize in a join — fts3tok1 1.13.2).
	// Returns the column defs, one row map per vtab row, and for each row map
	// the index of the left row that produced it.
	MaterializeCorrelatedVTab(entry *schema.Entry, leftColName string, leftRows []RowMap) ([]sql.ColumnDef, []RowMap, []int, error)
	ExecFTSSelect(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef) *Result

	// Materialized table-valued sources (PRAGMA functions, table-valued funcs).
	ExecPragmaTableValued(s *sql.SelectStmt) *Result
	MaterializePragmaTable(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error)
	MaterializeVtabTableFunc(ref sql.TableRef, opts VtabScanOptions) ([]sql.ColumnDef, [][]interface{}, []int64, error)
	// TryMaterializeEponymousVtab resolves a bare FROM reference to an
	// eponymous-only module's implicit table (FROM generate_series WHERE
	// start=...). handled is false when the name is not an eponymous module.
	TryMaterializeEponymousVtab(ref sql.TableRef, opts VtabScanOptions) (colDefs []sql.ColumnDef, rows [][]interface{}, rowids []int64, err error, handled bool)
	// MaterializeCreatedVTab materializes a CREATE VIRTUAL TABLE entry's
	// rows for SELECT (RootPage 0 + stored SQL naming a registered module,
	// e.g. csv). ok is false when the name is not such a table.
	MaterializeCreatedVTab(name string, opts VtabScanOptions) (colDefs []sql.ColumnDef, rows [][]interface{}, rowids []int64, err error, ok bool)
	// WithoutRowidVTab reports whether the named created virtual table's
	// stored schema declares WITHOUT ROWID (rowid references are errors).
	WithoutRowidVTab(name string) bool
	// MaterializeVtabTableFuncInRow materializes a table-valued vtab function
	// with its argument expressions evaluated against the given outer row
	// (correlated FROM-TVF inside a subquery, e.g.
	// EXISTS(SELECT 1 FROM json_each(t1.json,'$.items'))).
	MaterializeVtabTableFuncInRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error)
	// MaterializeCorrelatedVTabFunc materializes a table-valued vtab function
	// whose arguments reference left-side columns (e.g. json_each(t.json))
	// once per left row, returning the column defs, the concatenated right
	// rows, and the index of the left row for each right row. The join's
	// WHERE clause is supplied so conjuncts referencing only outer-side
	// columns gate materialization per row (sqlite WHERE pushdown parity:
	// json102-1011 relies on json_valid(user.phone) filtering rows BEFORE
	// json_each runs on them).
	MaterializeCorrelatedVTabFunc(ref sql.TableRef, leftRows []RowMap, where sql.Expr) ([]sql.ColumnDef, []RowMap, []int, error)
	MaterializeCorrelatedPragma(ref sql.TableRef, leftRows []RowMap) ([]sql.ColumnDef, []RowMap, []int, error)

	// View column helpers.
	ViewColumnDefs(viewEntry *schema.Entry) ([]sql.ColumnDef, error)
	ViewColumnDefsFromSelect(sel *sql.SelectStmt) []sql.ColumnDef
	ViewColumnNames(sel *sql.SelectStmt) []string

	// OR-index optimization.
	PlanOrIndexScan(where sql.Expr, tableName string, colDefs []sql.ColumnDef, ctx *DatabaseContext) ([]OrBranchPlan, bool)
	ExecSelectWithOrPlan(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, branches []OrBranchPlan) *Result

	// Validation helpers.
	ValidateIndexedBy(tableEntry *schema.Entry, indexName string, s *sql.SelectStmt) error
	ValidateNoRaiseOutsideTrigger(stmt sql.Stmt) error

	// Progress handler.
	CheckProgress() error

	// Index introspection.
	IndexColumnCount(idxName string) int
	ParseIndexColumns(sqlStr string) []string
	HasWithoutRowidKeyword(upperSQL string) bool
	CaseSensitiveLike() bool
	SchemaFunctionSafe(name string) bool

	// Statement settings.
	ReverseUnordered() bool
	ExprDepthLimit() int
	RecursiveCTELimit() int
}

// SelectEngine executes SELECT statements. It composes the SELECT sub-concerns
// (join execution, aggregate evaluation, clause validation, table scanning,
// query planning) and owns the query-state fields that the Engine previously
// carried on itself.
type SelectEngine struct {
	ctx SelectContext

	// Sub-executors composing this engine (SOLID-03..08). They share this
	// SelectEngine so inter-concern calls resolve through promoted methods.
	joins    JoinExecutor
	aggs     AggregateEvaluator
	validate SelectValidator
	scan     TableScanner
	planner  QueryPlanner

	// Query-state fields extracted from Engine (SOLID-14): correlated
	// subquery resolution, CTE scopes, alias resolution, view expansion.
	outerRow          Row                      // outer query row for correlated subquery resolution
	outerRowStack     []Row                    // stack of enclosing outer rows for multi-level correlation
	outerRows         []RowMap                 // all outer rows for correlated aggregate evaluation
	aliasStack        []map[string]sql.Expr    // output-column alias maps from enclosing SELECTs (innermost last)
	cteScopes         [][]sql.CTEDef           // CTE scopes from enclosing statements (innermost last)
	resolvingCTEs     map[*sql.SelectStmt]bool // CTE bodies currently being resolved (circular reference detection); keyed by the CTE body AST so a same-named inner WITH shadow is a different CTE
	currentScanTable  string                   // table name being scanned (for qualified column resolution)
	resolvingViews    map[string]bool          // tracks views currently being resolved (circular reference detection)
	schemaPin         *DatabaseContext         // view-body name resolution pin
	expandingTempView bool                     // expanding a TEMP-schema view body
	expandingView     bool                     // expanding any view body
	// derivedScope is set while executing the body of a parenthesized JOIN
	// group / derived table (parse.y "LP seltablist RP" → SF_NestedFrom).
	// Those subqueries are non-lateral: expressions inside — including TVF
	// arguments — cannot reference the enclosing query's tables
	// (tabfunc01-1420). Correlated EXISTS/scalar subqueries keep normal
	// outer-visibility rules.
	derivedScope bool
	// aggRowMaps, when non-nil, holds the row set an aggregate query is
	// evaluating over. Nested aggregate functions (e.g. round(avg(x),2))
	// resolve through it instead of evaluating per-row.
	aggRowMaps []RowMap
	// aggPendingErr captures an aggregate Step failure (e.g. zipfile's
	// "out of memory") raised while the aggregate was evaluated inside a
	// wrapping scalar expression, whose plumbing may not thread the error.
	// The enclosing SELECT finalization promotes it to the statement error.
	aggPendingErr error
	// windowGroupOutputs, when non-nil, holds the GROUP BY output column names
	// during the window pass over group rows. Window arguments that match an
	// output column (e.g. sum(sum(b)) OVER ... where the inner sum(b) is the
	// group aggregate) resolve from the row map value instead of being
	// re-aggregated, and nested-aggregate rejection is skipped.
	windowGroupOutputs []string
	// windowGroupCols mirrors the SELECT columns during the GROUP BY window
	// pass so window args resolve by alias (e.g. max(max(z)) OVER ... where
	// the inner max(z) is aliased m).
	windowGroupCols []sql.SelectColumn
	// inCompoundMember is true while executing a SELECT member of a compound
	// query (UNION/INTERSECT/EXCEPT), used by validation and aggregate checks.
	inCompoundMember bool
	// selectDepth is the current SELECT nesting depth (1 = top-level statement).
	selectDepth int
	// nestDepth is the current view/subquery nesting depth.
	nestDepth int
	// usingAutoIndex tracks whether an ephemeral index is being used (for EQP).
	usingAutoIndex bool
	// subqSeq is a monotonic counter for synthetic derived-table names (_subqN).
	subqSeq int
}

// AggRowMaps returns the aggregate row maps for aggregate function
// evaluation (e.g. round(avg(x),2) over the aggregate row set).
func (e *SelectEngine) AggRowMaps() []RowMap {
	return e.aggRowMaps
}

// PushOuterRow pushes a correlated outer row onto the outer-row stack.
func (e *SelectEngine) PushOuterRow(row Row) {
	e.outerRowStack = append(e.outerRowStack, e.outerRow)
	e.outerRow = row
}

// PopOuterRow pops the most recent correlated outer row.
func (e *SelectEngine) PopOuterRow() {
	n := len(e.outerRowStack)
	if n == 0 {
		e.outerRow = nil
		return
	}
	e.outerRow = e.outerRowStack[n-1]
	e.outerRowStack = e.outerRowStack[:n-1]
}

// OuterRowsForResolution returns the stack of enclosing outer rows for
// multi-level correlation resolution (innermost last).
func (e *SelectEngine) OuterRowsForResolution() []Row {
	rows := make([]Row, 0, len(e.outerRowStack)+1)
	if e.outerRow != nil {
		rows = append(rows, e.outerRow)
	}
	for i := len(e.outerRowStack) - 1; i >= 0; i-- {
		rows = append(rows, e.outerRowStack[i])
	}
	return rows
}

// SetCurrentScanTable sets the table name being scanned (for qualified column
// resolution during DML and SELECT execution).
func (e *SelectEngine) SetCurrentScanTable(name string) {
	e.currentScanTable = name
}

// CurrentScanTable returns the table name being scanned.
func (e *SelectEngine) CurrentScanTable() string {
	return e.currentScanTable
}

// PushCTEScope pushes a CTE scope onto the CTE stack.
func (e *SelectEngine) PushCTEScope(ctes []sql.CTEDef) {
	e.cteScopes = append(e.cteScopes, ctes)
}

// PopCTEScope pops the most recent CTE scope.
func (e *SelectEngine) PopCTEScope() {
	e.cteScopes = e.cteScopes[:len(e.cteScopes)-1]
}

// SetSchemaPin pins unqualified table/view resolution to a single schema
// (view-body name resolution, SQLite sqlite3FixSrcList semantics).
func (e *SelectEngine) SetSchemaPin(pin *DatabaseContext) {
	e.schemaPin = pin
}

// SchemaPin returns the current schema resolution pin (nil when unpinned).
func (e *SelectEngine) SchemaPin() *DatabaseContext {
	return e.schemaPin
}

// NewSelectEngine builds a SELECT engine over the given context.
func NewSelectEngine(ctx SelectContext) *SelectEngine {
	e := &SelectEngine{
		ctx:            ctx,
		resolvingCTEs:  make(map[*sql.SelectStmt]bool),
		resolvingViews: make(map[string]bool),
	}
	e.joins = JoinExecutor{engine: e}
	e.aggs = AggregateEvaluator{engine: e}
	e.validate = SelectValidator{engine: e}
	e.scan = TableScanner{engine: e}
	e.planner = QueryPlanner{engine: e}
	return e
}

// joinExecutor is the join-execution capability (SOLID-03). SelectEngine
// implements it; JoinExecutor is a thin composed facade over it.
type joinExecutor interface {
	execSelectFrom(s *sql.SelectStmt) (*Result, bool)
}

// aggEvaluator is the aggregate-evaluation capability (SOLID-04).
type aggEvaluator interface {
	evalAggregates(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result
}

// selectValidator is the SELECT-clause validation capability (SOLID-05).
type selectValidator interface {
	validateSelectExprs(s *sql.SelectStmt) error
}

// tableScanner is the table-scanning capability (SOLID-06).
type tableScanner interface {
	execSelectScanPhase(s *sql.SelectStmt, cursor *btree.Cursor, colDefs []sql.ColumnDef, tableEntry *schema.Entry) ([][]interface{}, []RowMap, error)
}

// queryPlanner is the query-planning capability (SOLID-08).
type queryPlanner interface {
	explainQueryPlanSelect(s *sql.SelectStmt) *Result
}

// Compile-time probes: SelectEngine implements each sub-executor capability,
// and the sub-executor facades expose their concern's public surface (LSP).
var (
	_ joinExecutor    = (*SelectEngine)(nil)
	_ aggEvaluator    = (*SelectEngine)(nil)
	_ selectValidator = (*SelectEngine)(nil)
	_ tableScanner    = (*SelectEngine)(nil)
	_ queryPlanner    = (*SelectEngine)(nil)
)

// ExecSelect executes a SELECT statement and returns its result.
func (e *SelectEngine) ExecSelect(s *sql.SelectStmt) *Result {
	return e.execSelect(s)
}

// ExecSelectView executes a SELECT over a view body and returns its result.
func (e *SelectEngine) ExecSelectView(entry *schema.Entry) *Result {
	return e.execSelectView(entry)
}

// ExecSelectOverMaterialized executes a SELECT over pre-materialized rows.
func (e *SelectEngine) ExecSelectOverMaterialized(s *sql.SelectStmt, colDefs []sql.ColumnDef, rows [][]interface{}) *Result {
	return e.execSelectOverMaterialized(s, colDefs, rows)
}

// ExecExplain executes an EXPLAIN statement.
func (e *SelectEngine) ExecExplain(s *sql.ExplainStmt) *Result {
	return e.execExplain(s)
}

// HandleSelectAggregates runs aggregate processing for a SELECT.
func (e *SelectEngine) HandleSelectAggregates(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	return e.handleSelectAggregates(s, rowMaps, colDefs)
}

// FinalizeSelectResult applies ORDER BY/LIMIT and final column naming.
func (e *SelectEngine) FinalizeSelectResult(result *Result, s *sql.SelectStmt, rowMaps []RowMap) *Result {
	return e.finalizeSelectResult(result, s, rowMaps)
}

// BuildColumnNames builds the result column names for a SELECT.
func (e *SelectEngine) BuildColumnNames(columns []sql.SelectColumn, colDefs []sql.ColumnDef, sel *sql.SelectStmt) []string {
	return e.buildColumnNames(columns, colDefs, sel)
}

// BuildRowMap builds a RowMap from a stored record.
func (e *SelectEngine) BuildRowMap(rec *storage.Record, colDefs []sql.ColumnDef, rowID int64) RowMap {
	return e.buildRowMap(rec, colDefs, rowID)
}

// BuildOutputRow builds one output row from an expression list.
func (e *SelectEngine) BuildOutputRow(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row Row) ([]interface{}, error) {
	return e.buildOutputRow(columns, colDefs, row)
}

// RowPassesWhere evaluates a WHERE predicate against a row.
func (e *SelectEngine) RowPassesWhere(where sql.Expr, row Row, cursor *btree.Cursor) (bool, error) {
	return e.rowPassesWhere(where, row, cursor)
}

// TableColumnNames returns the declared column names of a table.
func (e *SelectEngine) TableColumnNames(tableName string) ([]string, error) {
	return e.tableColumnNames(tableName)
}

// ValidateRowValueUse validates row-value usage in an expression.
func (e *SelectEngine) ValidateRowValueUse(expr sql.Expr, topLevel bool) error {
	return e.validateRowValueUse(expr, topLevel)
}

// ValidateDMLSubqueries validates subqueries inside DML statements.
func (e *SelectEngine) ValidateDMLSubqueries(stmt sql.Stmt) error {
	return e.validateDMLSubqueries(stmt)
}

// CompareOrderByValues compares two values under an ORDER BY term.
func (e *SelectEngine) CompareOrderByValues(left, right interface{}, ob sql.OrderByTerm) int {
	return e.compareOrderByValues(left, right, ob)
}
