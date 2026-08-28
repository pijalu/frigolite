package execquery

import (
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// JoinExecutor executes JOIN operations (SOLID-03). It is composed by the
// SelectEngine and delegates to the join execution methods on the shared
// engine, keeping the join concern's public surface focused.
type JoinExecutor struct {
	engine *SelectEngine
}

// AggregateEvaluator evaluates aggregate queries (SOLID-04). It is composed
// by the SelectEngine and exposes the aggregate evaluation entry points.
type AggregateEvaluator struct {
	engine *SelectEngine
}

// SelectValidator performs SELECT clause validation (SOLID-05). It is
// composed by the SelectEngine and exposes the validation entry points.
type SelectValidator struct {
	engine *SelectEngine
}

// TableScanner scans base tables and builds result rows (SOLID-06). It is
// composed by the SelectEngine and exposes the scanning entry points.
type TableScanner struct {
	engine *SelectEngine
}

// QueryPlanner builds EXPLAIN QUERY PLAN output (SOLID-08). It is composed by
// the SelectEngine and exposes the planning entry points.
type QueryPlanner struct {
	engine *SelectEngine
}

// --- JoinExecutor public surface (SOLID-03) ---

// ExecFrom executes the FROM clause (single table, join, subquery, view) and
// returns the result plus whether it was handled.
func (j *JoinExecutor) ExecFrom(s *sql.SelectStmt) (*Result, bool) {
	return j.engine.execSelectFrom(s)
}

// --- AggregateEvaluator public surface (SOLID-04) ---

// EvalAggregates evaluates aggregate queries over the scanned row maps.
func (a *AggregateEvaluator) EvalAggregates(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	return a.engine.evalAggregates(s, rowMaps, colDefs)
}

// --- SelectValidator public surface (SOLID-05) ---

// ValidateExprs validates SELECT clause expressions.
func (v *SelectValidator) ValidateExprs(s *sql.SelectStmt) error {
	return v.engine.validateSelectExprs(s)
}

// --- TableScanner public surface (SOLID-06) ---

// ScanTable scans a base table's rows (fast StructRow path) returning the
// flat rows, the per-row maps, and an error. colDefs must be the resolved
// column definitions for the table; tableEntry the schema entry; cursor the
// open B-tree cursor positioned for the scan.
func (t *TableScanner) ScanTable(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, cursor *btree.Cursor) ([][]interface{}, []RowMap, error) {
	return t.engine.execSelectScanPhase(s, cursor, colDefs, tableEntry)
}

// --- QueryPlanner public surface (SOLID-08) ---

// PlanSelect returns the EXPLAIN QUERY PLAN result for a SELECT.
func (p *QueryPlanner) PlanSelect(s *sql.SelectStmt) *Result {
	return p.engine.explainQueryPlanSelect(s)
}
