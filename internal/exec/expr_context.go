package exec

import (
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
)

// Compile-time check that Engine implements the expression evaluation
// capability interface (Liskov Substitution: Engine is substitutable for
// ExprContext).
var _ execexpr.ExprContext = (*Engine)(nil)

// Functions returns the engine's SQL function registry.
func (e *Engine) Functions() *function.Registry {
	return e.funcs
}

// OverloadProbe implements ExprContext: reports whether LIKE/GLOB/REGEXP
// evaluations should also invoke user overrides for side effects (set while
// an opted-in OperatorOverloadCounter vtab feeds the statement).
func (e *Engine) OverloadProbe() bool { return e.overloadProbe }

// ExecSelectRows runs a SELECT statement and returns its raw result rows,
// or an error. It is the subquery-execution capability exposed to the
// expression evaluator (the evaluator never sees the full *Result).
func (e *Engine) ExecSelectRows(s *sql.SelectStmt) ([][]interface{}, error) {
	result := e.execSelect(s)
	if result.Error != nil {
		return nil, result.Error
	}
	return result.Rows, nil
}

// EvalExecSQL runs SQL text and returns every result cell of every row of
// every statement, joined by sep (ext/misc/eval.c eval()). NULL cells render
// as the empty string; non-SELECT statements contribute no cells. An error in
// any statement aborts with that error.
func (e *Engine) EvalExecSQL(sqlStr, sep string) (string, error) {
	stmts, err := e.Prepare(sqlStr)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, stmt := range stmts {
		res := e.Exec(stmt)
		if res.Error != nil {
			return "", res.Error
		}
		if _, ok := stmt.(*sql.SelectStmt); ok {
			for _, row := range res.Rows {
				for _, v := range row {
					parts = append(parts, function.ValueText(v))
				}
			}
		}
	}
	return strings.Join(parts, sep), nil
}

// FromSourceColumnDefs resolves column definitions for a table reference
// during view-body name resolution.
func (e *Engine) FromSourceColumnDefs(ref sql.TableRef, resolving map[string]bool) []sql.ColumnDef {
	return e.fromSourceColumnDefsGuard(ref, resolving)
}

// QuoteFixWithSchema rewrites a schema-qualified SQL string's identifier
// quoting (delegated to the DDL executor's rename machinery).
func (e *Engine) QuoteFixWithSchema(schemaName, sqlStr string) string {
	return e.ddl.QuoteFixWithSchema(schemaName, sqlStr)
}

// RowHasQualifiedKeys reports whether a row map contains any table-qualified
// keys ("t1.a"), indicating it came from a join result rather than a bare
// single-table scan.
func (e *Engine) RowHasQualifiedKeys(row Row) bool {
	if row == nil {
		return false
	}
	if rm, ok := row.(RowMap); ok {
		for k := range rm {
			if strings.Contains(k, ".") {
				return true
			}
		}
	}
	if sr, ok := row.(*structRow); ok && sr.Index != nil {
		for k := range sr.Index {
			if strings.Contains(k, ".") {
				return true
			}
		}
	}
	return false
}

// CurrentScanTable returns the table name currently being scanned (for
// qualified column resolution).
func (e *Engine) CurrentScanTable() string {
	return e.selectEngine.CurrentScanTable()
}

// CurrentDMLTable returns the table currently being INSERTed/UPDATEd (for
// qualified refs in CHECK/defaults).
func (e *Engine) CurrentDMLTable() string {
	return e.dml.CurrentDMLTable()
}

// TriggerNewRow returns the new-row values for trigger program execution.
func (e *Engine) TriggerNewRow() Row {
	return e.triggers.NewRow()
}

// TriggerOldRow returns the old-row values for trigger program execution.
func (e *Engine) TriggerOldRow() Row {
	return e.triggers.OldRow()
}

// TriggerDepth returns the current trigger execution depth.
func (e *Engine) TriggerDepth() int {
	return e.triggers.Depth()
}

// ReturningStrict reports whether RETURNING expression evaluation treats
// unknown columns as errors (SQLite semantics).
func (e *Engine) ReturningStrict() bool {
	return e.returning.strict
}

// ReturningTable returns the table name in scope for RETURNING evaluation.
func (e *Engine) ReturningTable() string {
	return e.returning.table
}

// DQS_DML reports whether double-quoted strings are allowed in DML.
func (e *Engine) DQS_DML() bool {
	return e.settings.dqsDML
}

// TextEncoding returns the database text encoding name.
func (e *Engine) TextEncoding() string {
	return e.encoding
}

// LastRowID returns the rowid of the last inserted row.
func (e *Engine) LastRowID() int64 {
	return e.lastRowID
}

// LastChanges returns the change count of the last statement.
func (e *Engine) LastChanges() int64 {
	return e.lastChanges
}

// SetLastChanges overrides the changes() counter. The trigger manager uses it
// to save/restore the counter around trigger programs (SQLite restores
// sqlite3_changes() after a trigger program exits).
func (e *Engine) SetLastChanges(v int64) {
	e.lastChanges = v
}

// TotalChanges returns the total number of row changes since the connection
// opened (sqlite3_total_changes), including trigger-body and FK-action
// changes.
func (e *Engine) TotalChanges() int64 {
	return e.totalChanges
}

// ResetChangesCounters zeroes the changes()/total_changes() counters. Called
// when the TCL harness reopens the connection ("sqlite3 db test.db"), which
// creates a fresh sqlite3 handle with a zeroed total_changes counter.
func (e *Engine) ResetChangesCounters() {
	e.lastChanges = 0
	e.totalChanges = 0
	e.lastRowID = 0
}

// BumpTotalChanges adds n to the connection's total-changes counter. Called
// by FK actions (CASCADE/SET NULL/SET DEFAULT rewrites) that modify rows
// directly without going through a statement Exec.
func (e *Engine) BumpTotalChanges(n int64) {
	e.totalChanges += n
}

// CounterVal returns the test-only counter() backing value.
func (e *Engine) CounterVal() int64 {
	return e.testState.counterVal
}

// SetCounterVal sets the test-only counter() backing value.
func (e *Engine) SetCounterVal(v int64) {
	e.testState.counterVal = v
}

// NondeterVal returns the test-only nondeter() backing value.
func (e *Engine) NondeterVal() int64 {
	return e.testState.nondeterVal
}

// SetNondeterVal sets the test-only nondeter() backing value.
func (e *Engine) SetNondeterVal(v int64) {
	e.testState.nondeterVal = v
}

// CurrentFTSMatch returns the current FTS table for MATCH evaluation.
func (e *Engine) CurrentFTSMatch() string {
	return e.currentFTSMatch
}

// ftsMatchInfoCtx is the matchinfo() query context: the FTS table being
// selected and the phrase structure of its MATCH constraint. hasMatch is
// false when the SELECT has no MATCH constraint (matchinfo then returns an
// empty blob, fts3_snippet.c fts3GetMatchinfo with a NULL pExpr).
type ftsMatchInfoCtx struct {
	table    string
	hasMatch bool
	phrases  []fts.MatchPhrase
}

// SetFTSMatchInfo stores the matchinfo context for the current FTS SELECT.
func (e *Engine) SetFTSMatchInfo(table string, hasMatch bool, phrases []fts.MatchPhrase) {
	e.ftsMatchInfo = ftsMatchInfoCtx{table: table, hasMatch: hasMatch, phrases: phrases}
}

// ClearFTSMatchInfo resets the matchinfo context (statement end).
func (e *Engine) ClearFTSMatchInfo() {
	e.ftsMatchInfo = ftsMatchInfoCtx{}
}

// FTSMatchInfo returns the current matchinfo context.
func (e *Engine) FTSMatchInfo() (string, bool, []fts.MatchPhrase) {
	return e.ftsMatchInfo.table, e.ftsMatchInfo.hasMatch, e.ftsMatchInfo.phrases
}

// FTSTables returns the registered FTS3/4/5 tables (table name -> instance).
func (e *Engine) FTSTables() map[string]*fts.FTS3Table {
	return e.ftsTables
}

// AggRowMaps returns the aggregate row maps for aggregate function
// evaluation (e.g. round(avg(x),2) over the aggregate row set).
func (e *Engine) AggRowMaps() []RowMap {
	return e.selectEngine.AggRowMaps()
}
