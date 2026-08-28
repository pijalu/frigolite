// Package execexpr implements SQL expression evaluation.
//
// This package owns the expression evaluation engine: the evalExpr dispatch
// tree, scalar/row-value/IN/BETWEEN/CASE/CAST/function-call evaluation, and
// the value-comparison and arithmetic helpers those evaluations depend on.
//
// The Evaluator depends on a minimal ExprContext capability interface (the
// Engine implements it) rather than on the concrete Engine type: Dependency
// Inversion. Expression evaluation is isolated from query orchestration
// (Single Responsibility), and the context exposes only the capabilities
// evaluation actually needs (Interface Segregation).
package execexpr

import (
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
)

// Row provides column value lookup for expression evaluation.
// Implementations avoid per-row map allocation by using index-based access.
type Row interface {
	// Get returns the value for a named column and whether it was found.
	Get(name string) (interface{}, bool)
}

// RowMap implements Row for map-backed row stores.
type RowMap map[string]interface{}

// Get implements Row.
func (m RowMap) Get(name string) (interface{}, bool) {
	if v, ok := m[name]; ok {
		return v, ok
	}
	// SQLite column names are case-insensitive: a query may reference a
	// column (or table-qualified column) in a different case than the row
	// map key (built from CREATE TABLE column names). Fall back to a
	// case-insensitive scan of the small map.
	if len(m) > 0 {
		for k, v := range m {
			if strings.EqualFold(k, name) {
				return v, true
			}
		}
	}
	return nil, false
}

// CollatedValue wraps a value with a collation name for COLLATE support.
type CollatedValue struct {
	Value     interface{}
	Collation string
	// Explicit is true when the collation came from an explicit COLLATE
	// operator in the SQL text (expr COLLATE name), as opposed to a column's
	// declared COLLATE clause. SQLite's collation resolution gives an
	// explicit COLLATE on either operand precedence over any column
	// collation.
	Explicit bool
}

// ExprContext is the capability interface expression evaluation needs from
// the execution engine. The Engine implements it; the Evaluator depends on
// this interface rather than on the concrete engine type (DIP).
type ExprContext interface {
	// Collation and value comparison.
	LookupCollation(name string) func(a, b string) int
	CompareValuesCollate(a, b interface{}, collation string) int
	CompareValuesWithCollate(left, right interface{}) int

	// Function registry.
	Functions() *function.Registry
	// OverloadProbe reports whether the current statement's scan context
	// should probe overridden like()/glob()/regexp() functions per TRUE
	// operator evaluation (vtab.OperatorOverloadCounter modules).
	OverloadProbe() bool
	// LengthLimit reports the SQLITE_LIMIT_LENGTH setting (used by
	// base64/base85 to reject outputs that would exceed the limit).
	LengthLimit() int
	// EvalExecSQL runs SQL text and returns all result cells space/sep-joined,
	// mirroring ext/misc/eval.c's eval() function (used by misc8.test).
	EvalExecSQL(sqlStr, sep string) (string, error)

	// Subquery / SELECT execution.
	ExecSelectRows(s *sql.SelectStmt) ([][]interface{}, error)
	FindCTE(s *sql.SelectStmt, name string) (sql.CTEDef, bool)
	SubqueryColumnCount(s *sql.SelectStmt) int
	CompoundColumnAffinity(s *sql.SelectStmt, i int) rune
	// FromSourceColumnDefsGuard resolves column definitions for a table
	// reference during view-body name resolution.
	FromSourceColumnDefs(ref sql.TableRef, resolving map[string]bool) []sql.ColumnDef
	EvalAggFuncCall(v *sql.FuncCall, rowMaps []RowMap) (interface{}, error)
	QuoteFixWithSchema(schemaName, sqlStr string) string

	// Alias resolution.
	ResolveAliasRef(name string) (sql.Expr, bool)

	// RowHasQualifiedKeys reports whether a row map contains any
	// table-qualified keys ("t1.a"), indicating it came from a join result
	// rather than a bare single-table scan.
	RowHasQualifiedKeys(row Row) bool

	// Execution state.
	CurrentScanTable() string
	CurrentDMLTable() string
	TriggerNewRow() Row
	TriggerOldRow() Row
	TriggerDepth() int
	ReturningStrict() bool
	ReturningTable() string
	DQS_DML() bool
	CaseSensitiveLike() bool
	TextEncoding() string
	LastRowID() int64
	LastChanges() int64
	SetLastChanges(v int64)
	TotalChanges() int64
	CounterVal() int64
	SetCounterVal(v int64)
	NondeterVal() int64
	SetNondeterVal(v int64)

	// Outer-row scope (Engine-owned execution state for correlated
	// subquery resolution).
	PushOuterRow(row Row)
	PopOuterRow()
	OuterRowsForResolution() []Row

	// FTS MATCH support.
	CurrentFTSMatch() string
	FTSTables() map[string]*fts.FTS3Table
	// FTS matchinfo() context (the current FTS SELECT's MATCH phrases).
	FTSMatchInfo() (string, bool, []fts.MatchPhrase)
	// FTSShadowBlob reads a value BLOB from an FTS4 shadow table for
	// matchinfo 'l'/'a' (kind "docsize" = %_docsize row for docID,
	// "doctotal" = %_stat row id=0). A missing row or non-BLOB value errors
	// "database disk image is malformed" (fts3_write.c
	// sqlite3Fts3SelectDocsize / sqlite3Fts3SelectDoctotal).
	FTSShadowBlob(tableName, kind string, docID int64) ([]byte, error)

	// Aggregate evaluation state.
	AggRowMaps() []RowMap
}

// Evaluator evaluates SQL expressions. It owns the entire evaluation tree
// and reaches back into the engine only through the ExprContext interface.
type Evaluator struct {
	ctx ExprContext
	// aliasResolving tracks alias names currently being resolved (recursion
	// guard for SELECT output-column aliases).
	aliasResolving map[string]bool
}

// New creates an Evaluator bound to the given engine context.
func New(ctx ExprContext) *Evaluator {
	return &Evaluator{ctx: ctx}
}
