// DML prepare-time name and function resolution (build.c/resolve.c parity):
// SQLite resolves every column reference and function name in an
// INSERT/UPDATE/DELETE statement at prepare time, long before any row is
// touched. The engine historically deferred these to row evaluation, which
// silently skipped WHERE terms that resolve to NULL (update/delete_pkg/
// triggerB/misc4/misc5 class).
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
)

// isLiteralOrDQSRef reports whether a bare column reference is actually a
// boolean literal (unquoted TRUE/FALSE, which the parser keeps as
// ColumnRefs) or a double-quoted identifier the DQS_DML evaluator may turn
// into a string literal — both are exempt from prepare-time column lookup.
func isLiteralOrDQSRef(v *sql.ColumnRef) bool {
	return v.Table == "" && (strings.EqualFold(v.Name, "true") ||
		strings.EqualFold(v.Name, "false") || v.Quoted)
}

// buildDMLColumnLookup returns a case-insensitive lookup of the target
// table's columns, plus the rowid pseudo-aliases when the table is a rowid
// table (a WITHOUT ROWID table has no rowid/_rowid_/oid).
func buildDMLColumnLookup(colDefs []sql.ColumnDef, hasRowid bool) map[string]bool {
	lookup := make(map[string]bool, len(colDefs)+3)
	for _, cd := range colDefs {
		lookup[strings.ToLower(cd.Name)] = true
	}
	if hasRowid {
		lookup["rowid"] = true
		lookup["_rowid_"] = true
		lookup["oid"] = true
	}
	return lookup
}

// dmlColumnLookup returns the case-insensitive target-column lookup,
// memoized per DML executor and guarded by the schema fingerprint (the
// lookup is a pure function of the column defs, so DDL invalidates it). The
// prepare-time validation of every UPDATE/DELETE builds this identical map
// once per statement; wide tables paid the full rebuild each time.
func (e *DMLExecutor) dmlColumnLookup(colDefs []sql.ColumnDef, hasRowid bool) map[string]bool {
	if len(colDefs) == 0 {
		return buildDMLColumnLookup(colDefs, hasRowid)
	}
	fp := e.schemaFingerprint()
	if e.lookupCache != nil && e.lookupFingerprint == fp && e.lookupDefs == &colDefs[0] &&
		e.lookupLen == len(colDefs) && e.lookupHasRowid == hasRowid {
		return e.lookupCache
	}
	m := buildDMLColumnLookup(colDefs, hasRowid)
	e.lookupFingerprint, e.lookupDefs, e.lookupLen, e.lookupHasRowid, e.lookupCache = fp, &colDefs[0], len(colDefs), hasRowid, m
	return m
}

// validateDMLExprs resolves every bare column reference and function name in
// the given expressions against the target table, mirroring resolve.c:
//   - an unknown column errors "no such column: NAME";
//   - an unknown function errors "no such function: NAME";
//   - a scalar aggregate is a misuse outside SELECT-list/HAVING context:
//     "misuse of aggregate: NAME()".
//
// qualifiers lists the valid table qualifiers (the table name plus a
// statement alias when present). Subquery/ExistsExpr are leaves — subquery
// bodies are not descended into, they resolve against their own scope.
//
// The name-resolution walk and the comparison-collation resolution
// (validateDMLComparisonCollations) used to be two closure walks per
// statement; they are fused into one allocation-free walk here with the
// exact output contract: the FIRST name-resolution error in walk order
// wins, and a collation error reports only when no resolution error exists
// anywhere in the statement's expressions (the resolution pass used to run
// to completion before the collation pass started).
func (e *DMLExecutor) validateDMLExprs(qualifiers []string, colDefs []sql.ColumnDef, hasRowid bool, exprs []sql.Expr) *Result {
	lookup := e.dmlColumnLookup(colDefs, hasRowid)
	var collErr error
	for _, ex := range exprs {
		if ex == nil {
			continue
		}
		if err := e.walkDMLExprValidation(ex, lookup, qualifiers, colDefs, &collErr); err != nil {
			return &Result{Error: err}
		}
	}
	if collErr != nil {
		return &Result{Error: collErr}
	}
	return nil
}

// dmlValidationWalker is the fused validation state for one statement.
type dmlValidationWalker struct {
	e       *DMLExecutor
	lookup  map[string]bool
	quals   []string
	colDefs []sql.ColumnDef
	collErr *error
}

// walkDMLExprValidation walks one expression pre-order (the WalkExprFull /
// ForEachExprChild order, subquery bodies excluded), resolving names and
// functions as it goes and recording the first comparison-collation error.
func (e *DMLExecutor) walkDMLExprValidation(ex sql.Expr, lookup map[string]bool, qualifiers []string, colDefs []sql.ColumnDef, collErr *error) error {
	w := dmlValidationWalker{e: e, lookup: lookup, quals: qualifiers, colDefs: colDefs, collErr: collErr}
	return w.expr(ex)
}

// expr validates one node and recurses into its children in traversal order.
// Single-child kinds dispatch here; the multi-child containers recurse
// through the multi/walk helpers below so each function stays under the
// complexity gate.
func (w *dmlValidationWalker) expr(ex sql.Expr) error {
	if ex == nil {
		return nil
	}
	switch v := ex.(type) {
	case *sql.Subquery, *sql.ExistsExpr:
		return nil
	case *sql.ColumnRef:
		return w.e.dmlColumnRefError(v, w.lookup, w.quals)
	case *sql.FuncCall:
		if err := w.e.dmlFuncCallError(v); err != nil {
			return err
		}
		return w.multi(ex)
	case *sql.BinaryOp:
		return w.binary(ex, v.Left, v.Right)
	case *sql.IsDistinctFrom:
		return w.pair(v.Left, v.Right)
	case *sql.IsNotDistinctFrom:
		return w.pair(v.Left, v.Right)
	case *sql.Between, *sql.InList, *sql.RowValue, *sql.CaseExpr:
		return w.multi(ex)
	default:
		return w.single(ex)
	}
}

// single walks the single-child expression nodes (parentheses, unary and
// cast operands, the IS NULL / truth-value predicates, RAISE's message).
func (w *dmlValidationWalker) single(ex sql.Expr) error {
	switch v := ex.(type) {
	case *sql.ParenExpr:
		return w.expr(v.Expr)
	case *sql.RaiseExpr:
		return w.expr(v.Message)
	case *sql.UnaryOp:
		return w.expr(v.Operand)
	case *sql.CastExpr:
		return w.expr(v.Operand)
	case *sql.IsNull:
		return w.expr(v.Operand)
	case *sql.IsNotNull:
		return w.expr(v.Operand)
	case *sql.IsTrue:
		return w.expr(v.Operand)
	case *sql.IsFalse:
		return w.expr(v.Operand)
	}
	return nil
}

// binary validates a binary operator node: the COLLATE/comparison
// collation resolution is recorded (deferred — a name-resolution error
// anywhere in the statement outranks it), then both operands walk.
func (w *dmlValidationWalker) binary(ex sql.Expr, left, right sql.Expr) error {
	if err := w.e.comparisonCollationError(ex, w.colDefs); err != nil && *w.collErr == nil {
		*w.collErr = err
	}
	return w.pair(left, right)
}

// pair walks two child expressions in order.
func (w *dmlValidationWalker) pair(left, right sql.Expr) error {
	if err := w.expr(left); err != nil {
		return err
	}
	return w.expr(right)
}

// multi walks a multi-child container's children in the ForEachExprChild
// traversal order (BETWEEN operand/low/high, IN operand+list, row values,
// function args+FILTER, CASE operand/whens/else).
func (w *dmlValidationWalker) multi(ex sql.Expr) error {
	switch v := ex.(type) {
	case *sql.Between:
		return w.triple(v.Operand, v.Low, v.High)
	case *sql.InList:
		if err := w.expr(v.Operand); err != nil {
			return err
		}
		return w.list(v.List)
	case *sql.RowValue:
		return w.list(v.Values)
	case *sql.FuncCall:
		// ForEachExprChild walks a function call's arguments only (FILTER
		// and OVER bodies are not descended into by the validation walk —
		// parity with the closure walkers this fuses).
		return w.list(v.Args)
	case *sql.CaseExpr:
		if err := w.expr(v.Operand); err != nil {
			return err
		}
		for i := range v.Whens {
			if err := w.expr(v.Whens[i].When); err != nil {
				return err
			}
			if err := w.expr(v.Whens[i].Then); err != nil {
				return err
			}
		}
		return w.expr(v.Else)
	}
	return nil
}

// triple walks three child expressions in order.
func (w *dmlValidationWalker) triple(a, b, c sql.Expr) error {
	if err := w.expr(a); err != nil {
		return err
	}
	if err := w.expr(b); err != nil {
		return err
	}
	return w.expr(c)
}

// list walks an expression list in order.
func (w *dmlValidationWalker) list(items []sql.Expr) error {
	for _, item := range items {
		if err := w.expr(item); err != nil {
			return err
		}
	}
	return nil
}

// validDMLQualifier reports whether q is a valid table qualifier for the
// statement: the target table (plus a statement alias), or the NEW./OLD.
// trigger-body row pseudo-aliases (the T18 alias-masking rule only rejects
// the ORIGINAL table name).
func validDMLQualifier(qualifiers []string, q string) bool {
	if strings.EqualFold(q, "new") || strings.EqualFold(q, "old") {
		return true
	}
	for _, v := range qualifiers {
		if strings.EqualFold(q, v) {
			return true
		}
	}
	return false
}

// dmlColumnRefError validates one column reference in a DML expression
// against the target table's column lookup and valid qualifiers.
func (e *DMLExecutor) dmlColumnRefError(v *sql.ColumnRef, lookup map[string]bool, qualifiers []string) error {
	if v.Table != "" {
		// NEW./OLD. pseudo-rows (trigger bodies) resolve their
		// columns against the fired row — which may carry columns
		// the target table lacks (a view's column list), so the
		// bare-name lookup does not apply to them.
		if strings.EqualFold(v.Table, "new") || strings.EqualFold(v.Table, "old") {
			return nil
		}
		if !validDMLQualifier(qualifiers, v.Table) {
			return fmt.Errorf("no such column: %s.%s", v.Table, v.Name)
		}
		if !lookup[strings.ToLower(v.Name)] {
			return fmt.Errorf("no such column: %s.%s", v.Table, v.Name)
		}
		return nil
	}
	// Unquoted TRUE/FALSE are boolean literals (parser keeps
	// them as ColumnRefs); a double-quoted identifier falls to
	// the DQS_DML evaluator, which converts it to a string
	// literal when the legacy DQS setting is on
	// (indexexpr1-2110: WHERE (SELECT 'y') GLOB "y").
	if isLiteralOrDQSRef(v) {
		return nil
	}
	if !lookup[strings.ToLower(v.Name)] {
		return fmt.Errorf("no such column: %s", v.Name)
	}
	return nil
}

// dmlFuncCallError validates one function reference in a DML expression: an
// unknown function errors "no such function: NAME" and a scalar aggregate is
// a misuse outside SELECT-list/HAVING context ("misuse of aggregate: NAME()").
func (e *DMLExecutor) dmlFuncCallError(v *sql.FuncCall) error {
	isAgg, exists := e.ctx.LookupFunction(v.Name)
	if !exists {
		return fmt.Errorf("no such function: %s", v.Name)
	}
	if isAgg {
		return fmt.Errorf("misuse of aggregate: %s()", strings.ToLower(v.Name))
	}
	return nil
}

// validateInsertValuesExprs rejects column references inside INSERT VALUES
// tuples: a VALUES row has no source row to read columns from (resolve.c
// reports "no such column: X" for bare and qualified references alike).
// Trigger-body NEW./OLD. row references stay valid, and subqueries (with
// their own scope) are not descended into.
func (e *DMLExecutor) validateInsertValuesExprs(s *sql.InsertStmt) *Result {
	for _, tuple := range s.Values {
		for _, ex := range tuple {
			if ex == nil {
				continue
			}
			if bad := insertValuesBadRef(ex); bad != "" {
				return &Result{Error: fmt.Errorf("no such column: %s", bad)}
			}
		}
	}
	return nil
}

// insertValuesBadRef finds the first rejected column reference in a VALUES
// tuple expression ("name" or "table.name"); "" when every reference is
// allowed.
func insertValuesBadRef(ex sql.Expr) string {
	var bad string
	execquery.WalkExprFull(ex, func(n sql.Expr) {
		if bad != "" {
			return
		}
		if v, ok := n.(*sql.ColumnRef); ok {
			bad = insertValuesRefName(v)
		}
	})
	return bad
}

// insertValuesRefName reports the rejected reference text for one column
// reference inside a VALUES tuple, or "" when the reference is allowed.
func insertValuesRefName(v *sql.ColumnRef) string {
	if strings.EqualFold(v.Table, "new") || strings.EqualFold(v.Table, "old") {
		return ""
	}
	if v.Table == "" && (strings.EqualFold(v.Name, "true") || strings.EqualFold(v.Name, "false")) {
		return ""
	}
	// DQS (resolve.c): a double-quoted identifier that fails
	// column resolution becomes a string literal when DQS_DML
	// is enabled (the legacy default). The evaluator owns the
	// final decision, so the prepare pass tolerates quoted
	// refs it cannot resolve (indexexpr1-2110: WHERE
	// (SELECT 'y') GLOB "y").
	if v.Quoted {
		return ""
	}
	if v.Table != "" {
		return v.Table + "." + v.Name
	}
	return v.Name
}

// validateInsertColumnList checks a named INSERT column list against the
// target table (build.c sqlite3AddColumnToList): an unknown name errors
// "table %s has no column named %s".
func validateInsertColumnList(tableName string, columns []string, colDefs []sql.ColumnDef) *Result {
	lookup := buildDMLColumnLookup(colDefs, true)
	for _, col := range columns {
		if !lookup[strings.ToLower(col)] {
			return &Result{Error: fmt.Errorf("table %s has no column named %s", tableName, col)}
		}
	}
	return nil
}
