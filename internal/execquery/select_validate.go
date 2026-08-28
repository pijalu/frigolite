package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns SELECT clause validation, extracted from select.go for
// single-responsibility cohesion. It contains the 11 validation functions
// that check row-value usage, subquery arity, ORDER BY constraints, DML
// expression subqueries, trigger bodies, and compound-query ORDER BY terms.
//
// All functions here are in the same exec package and share the Engine
// receiver and helper functions that remain in select.go.

// ---------------------------------------------------------------------------
// ORDER BY length validation
// ---------------------------------------------------------------------------

// validateOrderByLength checks that no aggregate function's ORDER BY clause in
// expr exceeds limit terms. It recurses through all sub-expressions.
func validateOrderByLength(expr sql.Expr, limit int) error {
	if expr == nil {
		return nil
	}
	if fn, ok := expr.(*sql.FuncCall); ok {
		return validateOrderByLengthFuncCall(fn, limit)
	}
	for _, child := range orderByLengthChildren(expr) {
		if err := validateOrderByLength(child, limit); err != nil {
			return err
		}
	}
	return nil
}

// validateOrderByLengthFuncCall validates a function call's ORDER BY length and
// recurses into its arguments.
func validateOrderByLengthFuncCall(fn *sql.FuncCall, limit int) error {
	if len(fn.OrderBy) > limit {
		return fmt.Errorf("too many terms in ORDER BY clause")
	}
	for _, a := range fn.Args {
		if err := validateOrderByLength(a, limit); err != nil {
			return err
		}
	}
	return nil
}

// orderByLengthChildren returns the sub-expressions of expr that
// validateOrderByLength should recurse into (excluding *sql.FuncCall, which is
// handled by validateOrderByLengthFuncCall).
func orderByLengthChildren(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return []sql.Expr{v.Left, v.Right}
	case *sql.UnaryOp:
		return []sql.Expr{v.Operand}
	case *sql.IsNull:
		return []sql.Expr{v.Operand}
	case *sql.IsNotNull:
		return []sql.Expr{v.Operand}
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.InList:
		children := []sql.Expr{v.Operand}
		children = append(children, v.List...)
		return children
	case *sql.CaseExpr:
		children := []sql.Expr{}
		if v.Operand != nil {
			children = append(children, v.Operand)
		}
		for _, w := range v.Whens {
			children = append(children, w.When, w.Then)
		}
		if v.Else != nil {
			children = append(children, v.Else)
		}
		return children
	}
	return nil
}

// ---------------------------------------------------------------------------
// Row-value usage validation
// ---------------------------------------------------------------------------

// validateRowValueUse validates row-value usage in an expression. A row value
// at the top level is "row value misused"; nested under a comparison/IN it is
// legal. Parent context (BinaryOp, InList) decides legality.
func (e *SelectEngine) validateRowValueUse(expr sql.Expr, topLevel bool) error {
	if expr == nil {
		return nil
	}
	switch v := expr.(type) {
	case *sql.RowValue:
		return e.validateRowValueInRowValue(v, topLevel)
	case *sql.BinaryOp:
		return e.validateRowValueBinaryOp(v)
	case *sql.UnaryOp:
		return e.validateRowValueUse(v.Operand, false)
	case *sql.ParenExpr:
		return e.validateRowValueUse(v.Expr, topLevel)
	case *sql.InList:
		return e.validateRowValueInList(v)
	case *sql.Subquery:
		return nil
	case *sql.FuncCall:
		return validateRowValueInFuncCall(v)
	case *sql.CaseExpr:
		return e.validateRowValueInCase(v)
	default:
		return nil
	}
}

// validateRowValueInRowValue checks a RowValue node: top-level is misuse;
// nested it recurses into elements.
func (e *SelectEngine) validateRowValueInRowValue(v *sql.RowValue, topLevel bool) error {
	if topLevel {
		return fmt.Errorf("row value misused")
	}
	for _, sub := range v.Values {
		if err := e.validateRowValueUse(sub, false); err != nil {
			return err
		}
	}
	return nil
}

// validateRowValueInFuncCall checks that no function argument is a row value.
func validateRowValueInFuncCall(v *sql.FuncCall) error {
	for _, arg := range v.Args {
		if isRowValueExpr(arg) {
			return fmt.Errorf("row value misused")
		}
	}
	return nil
}

// validateRowValueInCase recurses into a CASE expression's sub-expressions for
// row-value validation.
func (e *SelectEngine) validateRowValueInCase(v *sql.CaseExpr) error {
	if err := e.validateRowValueUse(v.Operand, false); err != nil {
		return err
	}
	for _, w := range v.Whens {
		if err := e.validateRowValueUse(w.When, false); err != nil {
			return err
		}
		if err := e.validateRowValueUse(w.Then, false); err != nil {
			return err
		}
	}
	return e.validateRowValueUse(v.Else, false)
}

// validateRowValueBinaryOp validates a binary operation's row-value usage:
// COLLATE collation names, row-vs-scalar comparisons, and scalar-vs-subquery
// arity.
func (e *SelectEngine) validateRowValueBinaryOp(v *sql.BinaryOp) error {
	if err := validateCollateClause(v, e); err != nil {
		return err
	}
	leftIsRow := isRowValueExpr(v.Left)
	rightIsRow := isRowValueExpr(v.Right)
	leftIsSub := isSubqueryExpr(v.Left)
	rightIsSub := isSubqueryExpr(v.Right)
	if err := checkRowScalarMismatch(leftIsRow, rightIsRow, leftIsSub, rightIsSub); err != nil {
		return err
	}
	if err := checkScalarSubqueryArity(leftIsRow, leftIsSub, rightIsSub, v.Right, e); err != nil {
		return err
	}
	if err := checkScalarSubqueryArity(rightIsRow, rightIsSub, leftIsSub, v.Left, e); err != nil {
		return err
	}
	if err := e.validateRowValueUse(v.Left, false); err != nil {
		return err
	}
	return e.validateRowValueUse(v.Right, false)
}

// validateCollateClause validates the collation name of an explicit COLLATE
// binary operation.
func validateCollateClause(v *sql.BinaryOp, e *SelectEngine) error {
	if !strings.EqualFold(v.Operator, "COLLATE") {
		return nil
	}
	if name := getCollationName(v.Right); name != "" {
		return e.ctx.CheckCollationString(name)
	}
	return nil
}

// checkRowScalarMismatch returns "row value misused" if a row value is compared
// against a non-subquery scalar.
func checkRowScalarMismatch(leftIsRow, rightIsRow, leftIsSub, rightIsSub bool) error {
	if leftIsRow != rightIsRow {
		if leftIsRow && !rightIsSub {
			return fmt.Errorf("row value misused")
		}
		if rightIsRow && !leftIsSub {
			return fmt.Errorf("row value misused")
		}
	}
	return nil
}

// checkScalarSubqueryArity validates that a scalar compared against a
// multi-column subquery is "row value misused".
func checkScalarSubqueryArity(sideIsRow, sideIsSub, otherIsSub bool, other sql.Expr, e *SelectEngine) error {
	if sideIsRow || sideIsSub || !otherIsSub {
		return nil
	}
	if err := e.validateSubqueryArity(other, 1); err != nil {
		return fmt.Errorf("row value misused")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Expression subquery validation
// ---------------------------------------------------------------------------

// validateExprSubqueries walks an expression tree looking for subqueries and
// checking them for invalid patterns like aggregates inside UNION ALL.
func (e *SelectEngine) validateExprSubqueries(expr sql.Expr) error {
	return e.validateExprSubqueriesCtx(expr, false)
}

func (e *SelectEngine) validateExprSubqueriesCtx(expr sql.Expr, rowValueOK bool) error {
	return e.validateExprSubqueriesCtxMode(expr, rowValueOK, false)
}

// validateExprSubqueriesCtxDML is like validateExprSubqueriesCtx but reports
// the scalar-subquery arity error for a multi-column subquery in DML contexts.
func (e *SelectEngine) validateExprSubqueriesCtxDML(expr sql.Expr) error {
	return e.validateExprSubqueriesCtxMode(expr, false, true)
}

// validateExprSubqueriesCtxMode is the core subquery validation dispatcher. It
// checks scalar-subquery arity (one column unless rowValueOK), validates nested
// SELECT statements, and recurses into all sub-expressions.
func (e *SelectEngine) validateExprSubqueriesCtxMode(expr sql.Expr, rowValueOK, dmlArity bool) error {
	switch v := expr.(type) {
	case *sql.Subquery:
		return e.validateSubqueryNode(v, rowValueOK)
	case *sql.ExistsExpr:
		return e.validateExistsNode(v)
	case *sql.FuncCall:
		return e.validateSubqFuncCall(v, dmlArity)
	case *sql.BinaryOp:
		return e.validateExprSubqueriesBinaryOp(v, dmlArity)
	case *sql.UnaryOp:
		return e.validateExprSubqueriesCtxMode(v.Operand, false, dmlArity)
	case *sql.CaseExpr:
		return e.validateExprSubqueriesCase(v, dmlArity)
	case *sql.Between:
		return e.validateSubqBetween(v, dmlArity)
	case *sql.InList:
		return e.validateExprSubqueriesInList(v, dmlArity)
	case *sql.IsNull, *sql.IsNotNull:
		return e.validateExprSubqueriesCtxMode(isNullExprOperand(v), false, dmlArity)
	case *sql.IsDistinctFrom:
		return e.validateSubqIsExpr(v.Left, v.Right, dmlArity)
	case *sql.IsNotDistinctFrom:
		return e.validateSubqIsExpr(v.Left, v.Right, dmlArity)
	}
	return nil
}

// isNullExprOperand extracts the operand from an IsNull or IsNotNull expression.
func isNullExprOperand(expr sql.Expr) sql.Expr {
	if is, ok := expr.(*sql.IsNull); ok {
		return is.Operand
	}
	return expr.(*sql.IsNotNull).Operand
}

// validateSubqueryInnerSelect validates a subquery's inner SELECT after pushing
// the subquery's own CTEs onto the CTE-scope stack. This mirrors execSelect's
// CTE-scope handling so that nested subqueries inside the inner SELECT can
// resolve CTEs the subquery declares (with2 10.1: WITH t1(a) AS (...) SELECT
// (SELECT ... FROM t1)). Several validation entry points (validateExprOrderBy,
// validateExistsNode, IN-list items) previously called validateSelectExprs on the
// inner SELECT directly, bypassing the push and leaving the inner CTE invisible
// to nested FROM references, which surfaced as "no such table: NAME" at
// validation time. Routing them through this helper keeps their existing
// validation semantics while establishing the CTE scope.
func (e *SelectEngine) validateSubqueryInnerSelect(subq *sql.Subquery) error {
	if subq == nil || subq.Select == nil {
		return nil
	}
	if len(subq.Select.CTEs) > 0 {
		e.cteScopes = append(e.cteScopes, subq.Select.CTEs)
		defer func() { e.cteScopes = e.cteScopes[:len(e.cteScopes)-1] }()
	}
	return e.validateSelectExprs(subq.Select)
}

// validateSubqueryNode validates a scalar subquery's column count and nested
// SELECT.
func (e *SelectEngine) validateSubqueryNode(v *sql.Subquery, rowValueOK bool) error {
	if v.Select == nil {
		return nil
	}
	if len(v.Select.CTEs) > 0 {
		e.cteScopes = append(e.cteScopes, v.Select.CTEs)
		defer func() { e.cteScopes = e.cteScopes[:len(e.cteScopes)-1] }()
	}
	// Resolve the subquery's FROM table at prepare time so a missing table
	// surfaces here (SQLite resolves names during prepare; window1 67.1
	// expects "no such table: v1" from a subquery nested in a compound
	// ORDER BY). CTEs and views are valid FROM sources.
	if v.Select.From.Name != "" {
		if _, _, err := e.ctx.FindTable(v.Select.From.Name); err != nil {
			if _, _, verr := e.ctx.FindView(v.Select.From.Name); verr != nil {
				if _, ok := e.findCTE(v.Select, v.Select.From.Name); !ok {
					// Eponymous vtab modules (generate_series, carray, ...)
					// are valid implicit FROM sources without a schema entry.
					if !e.eponymousModuleResolvable(v.Select.From.Name) {
						return err
					}
				}
			}
		}
	}
	if !rowValueOK {
		if n := e.subqueryColumnCount(v.Select); n > 1 {
			return fmt.Errorf("sub-select returns %d columns - expected 1", n)
		}
	}
	if err := e.validateSelectExprs(v.Select); err != nil {
		return err
	}
	// A correlated aggregate in a scalar subquery (e.g. SELECT (SELECT avg(a))
	// FROM t2 — avg over the outer rows) is valid and handled by
	// evalAggOverOuterRows. The IN-subquery misuse case is validated in
	// validateInListSubqueryItem.
	return nil
}

// validateExistsNode validates an EXISTS subquery's nested SELECT. Its CTEs
// (if any) are pushed onto the scope stack first so nested references resolve.
func (e *SelectEngine) validateExistsNode(v *sql.ExistsExpr) error {
	if v.Select == nil {
		return nil
	}
	return e.validateSubqueryInnerSelect(&sql.Subquery{Select: v.Select})
}

// validateSubqFuncCall recurses into a function call's arguments for subquery
// validation.
func (e *SelectEngine) validateSubqFuncCall(v *sql.FuncCall, dmlArity bool) error {
	// An aggregate function whose argument is a scalar subquery containing a
	// CORRELATED aggregate is a misuse: the inner aggregate would need to
	// evaluate in the outer context but is nested inside the outer aggregate's
	// argument (SQLite resolve.c: the aggregate clears NC_AllowAgg while
	// walking its arguments; window1 75.1: SELECT count((SELECT count(a)))
	// FROM t → "misuse of aggregate: count()").
	if fn, ok := e.ctx.Functions().Find(v.Name); ok && fn.Type == function.TypeAggregate {
		for _, arg := range v.Args {
			if sub, ok := arg.(*sql.Subquery); ok && sub.Select != nil {
				if name := e.subqueryOuterAggRef(sub.Select); name != "" {
					return fmt.Errorf("misuse of aggregate: %s()", strings.ToLower(v.Name))
				}
			}
		}
	}
	for _, arg := range v.Args {
		if err := e.validateExprSubqueriesCtxMode(arg, false, dmlArity); err != nil {
			return err
		}
	}
	return nil
}

// validateSubqBetween validates BETWEEN operands, allowing multi-column
// subqueries (row-value BETWEEN).
func (e *SelectEngine) validateSubqBetween(v *sql.Between, dmlArity bool) error {
	if err := e.validateExprSubqueriesCtxMode(v.Operand, true, dmlArity); err != nil {
		return err
	}
	if err := e.validateExprSubqueriesCtxMode(v.Low, true, dmlArity); err != nil {
		return err
	}
	return e.validateExprSubqueriesCtxMode(v.High, true, dmlArity)
}

// validateSubqIsExpr validates the two sides of an IS / IS NOT DISTINCT FROM
// expression for subquery correctness.
func (e *SelectEngine) validateSubqIsExpr(left, right sql.Expr, dmlArity bool) error {
	if err := e.validateExprSubqueriesCtxMode(left, false, dmlArity); err != nil {
		return err
	}
	return e.validateExprSubqueriesCtxMode(right, false, dmlArity)
}

// validateExprSubqueriesBinaryOp validates a binary-operation expression's
// subqueries, handling row-value comparisons and scalar-vs-multi-column misuse.
func (e *SelectEngine) validateExprSubqueriesBinaryOp(v *sql.BinaryOp, dmlArity bool) error {
	_, leftRow := v.Left.(*sql.RowValue)
	_, rightRow := v.Right.(*sql.RowValue)
	_, leftSub := v.Left.(*sql.Subquery)
	_, rightSub := v.Right.(*sql.Subquery)
	// Row-value comparisons: one side is a row, the other is a subquery.
	if ok, err := e.validateRowSubqComparison(leftSub, rightRow, v.Left, true, dmlArity, v.Right, false); ok {
		return err
	}
	if ok, err := e.validateRowSubqComparison(rightSub, leftRow, v.Right, true, dmlArity, v.Left, false); ok {
		return err
	}
	// Two subqueries compared against each other: both may be multi-column.
	if leftSub && rightSub {
		return e.validateTwoSubqueries(v.Left, v.Right, dmlArity)
	}
	if err := e.checkSubqMisuse(leftSub, v.Left, rightRow, rightSub, false); err != nil {
		return err
	}
	if err := e.checkSubqMisuseDML(rightSub, v.Right, leftRow, leftSub, dmlArity); err != nil {
		return err
	}
	if err := e.validateExprSubqueriesCtxMode(v.Left, false, dmlArity); err != nil {
		return err
	}
	return e.validateExprSubqueriesCtxMode(v.Right, false, dmlArity)
}

// validateRowSubqComparison checks the pattern: a subquery on one side, a row
// value on the other. Returns (true, err) if the pattern matched and was
// handled; (false, nil) otherwise.
func (e *SelectEngine) validateRowSubqComparison(subOK, rowOK bool, subExpr sql.Expr, subRowValOK bool, dmlArity bool, rowExpr sql.Expr, rowRowValOK bool) (bool, error) {
	if !subOK || !rowOK {
		return false, nil
	}
	if err := e.validateExprSubqueriesCtxMode(subExpr, subRowValOK, dmlArity); err != nil {
		return true, err
	}
	return true, e.validateExprSubqueriesCtxMode(rowExpr, rowRowValOK, dmlArity)
}

// validateTwoSubqueries validates two subquery expressions, allowing
// multi-column subqueries.
func (e *SelectEngine) validateTwoSubqueries(left, right sql.Expr, dmlArity bool) error {
	if err := e.validateExprSubqueriesCtxMode(left, true, dmlArity); err != nil {
		return err
	}
	return e.validateExprSubqueriesCtxMode(right, true, dmlArity)
}

// checkSubqMisuse checks for a multi-column subquery misused as a scalar on the
// LEFT side of a comparison.
func (e *SelectEngine) checkSubqMisuse(leftSub bool, left sql.Expr, rightRow, rightSub bool, _ bool) error {
	if !leftSub || rightRow || rightSub {
		return nil
	}
	sq, ok := left.(*sql.Subquery)
	if !ok {
		return nil
	}
	if n := e.subqueryColumnCount(sq.Select); n > 1 {
		return fmt.Errorf("row value misused")
	}
	return nil
}

// checkSubqMisuseDML checks for a multi-column subquery misused as a scalar on
// the RIGHT side of a comparison; the error type differs between SELECT and DML
// contexts.
func (e *SelectEngine) checkSubqMisuseDML(rightSub bool, right sql.Expr, leftRow, leftSub, dmlArity bool) error {
	if !rightSub || leftRow || leftSub {
		return nil
	}
	sq, ok := right.(*sql.Subquery)
	if !ok {
		return nil
	}
	n := e.subqueryColumnCount(sq.Select)
	if n <= 1 {
		return nil
	}
	if dmlArity {
		return fmt.Errorf("sub-select returns %d columns - expected 1", n)
	}
	return fmt.Errorf("row value misused")
}

// validateExprSubqueriesCase validates a CASE expression's subqueries: a
// row-value CASE operand may be a multi-column subquery.
func (e *SelectEngine) validateExprSubqueriesCase(v *sql.CaseExpr, dmlArity bool) error {
	if v.Operand != nil {
		if err := e.validateExprSubqueriesCtxMode(v.Operand, true, dmlArity); err != nil {
			return err
		}
	}
	for _, w := range v.Whens {
		if err := e.validateExprSubqueriesCtxMode(w.When, false, dmlArity); err != nil {
			return err
		}
		if err := e.validateExprSubqueriesCtxMode(w.Then, false, dmlArity); err != nil {
			return err
		}
	}
	if v.Else != nil {
		return e.validateExprSubqueriesCtxMode(v.Else, false, dmlArity)
	}
	return nil
}

// validateExprSubqueriesInList validates an IN-list expression's subqueries:
// the operand may be a row-value subquery, and list subqueries may return
// multiple columns when the operand is a row value.
func (e *SelectEngine) validateExprSubqueriesInList(v *sql.InList, dmlArity bool) error {
	if err := e.validateExprSubqueriesCtxMode(v.Operand, true, dmlArity); err != nil {
		return err
	}
	_, opIsRow := v.Operand.(*sql.RowValue)
	_, opIsSub := v.Operand.(*sql.Subquery)
	for _, val := range v.List {
		if err := e.validateInListSubqueryItem(val, opIsRow, opIsSub, dmlArity); err != nil {
			return err
		}
	}
	return nil
}

// validateInListSubqueryItem validates a single IN-list item, handling
// subquery items specially.
func (e *SelectEngine) validateInListSubqueryItem(val sql.Expr, opIsRow, opIsSub, dmlArity bool) error {
	subq, ok := val.(*sql.Subquery)
	if !ok || subq.Select == nil {
		return e.validateExprSubqueriesCtxMode(val, false, dmlArity)
	}
	if !opIsRow && !opIsSub {
		if n := e.subqueryColumnCount(subq.Select); n > 1 {
			return fmt.Errorf("sub-select returns %d columns - expected 1", n)
		}
	}
	if err := e.validateSubqueryInnerSelect(subq); err != nil {
		return err
	}
	// A correlated aggregate in an IN-subquery is valid in SQLite (window1
	// 44.3.2: (0,0) IN (SELECT MIN(c0), NTILE(1) OVER()) FROM t0 → 0).
	return nil
}

// ---------------------------------------------------------------------------
// DML subquery validation
// ---------------------------------------------------------------------------

// validateDMLSubqueries validates the subquery arity of expressions inside
// INSERT/UPDATE/DELETE statements.
func (e *SelectEngine) validateDMLSubqueries(stmt sql.Stmt) error {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		if err := e.validateInsertSubqueries(s); err != nil {
			return err
		}
	case *sql.UpdateStmt:
		if err := e.validateUpdateSubqueries(s); err != nil {
			return err
		}
	case *sql.DeleteStmt:
		if s.Where != nil {
			if err := e.validateExprSubqueriesCtxDML(s.Where); err != nil {
				return err
			}
		}
	}
	return e.validateTriggerBodiesForDML(stmt)
}

// validateInsertSubqueries validates subqueries in INSERT VALUES tuples.
func (e *SelectEngine) validateInsertSubqueries(s *sql.InsertStmt) error {
	for _, tuple := range s.Values {
		for _, expr := range tuple {
			if err := e.validateExprSubqueriesCtxDML(expr); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateUpdateSubqueries validates subqueries in UPDATE assignments and WHERE.
func (e *SelectEngine) validateUpdateSubqueries(s *sql.UpdateStmt) error {
	for _, a := range s.Assignments {
		if err := e.validateExprSubqueriesCtxDML(a.Value); err != nil {
			return err
		}
	}
	if s.Where != nil {
		return e.validateExprSubqueriesCtxDML(s.Where)
	}
	return nil
}

// validateTriggerBodiesForDML validates the expression-level content of the
// trigger bodies that the DML statement could fire, mirroring SQLite's trigger
// program compilation at statement prepare.
func (e *SelectEngine) validateTriggerBodiesForDML(stmt sql.Stmt) error {
	tableName, event, ok := dmlTableEvent(stmt)
	if !ok {
		return nil
	}
	ctx := e.dmlSchemaContext(tableName)
	triggers, err := ctx.Schema.FindTriggersForTable(tableName)
	if err != nil {
		return nil
	}
	for _, t := range triggers {
		if err := e.validateMatchingTrigger(t, event); err != nil {
			return err
		}
	}
	return nil
}

// dmlTableEvent extracts the table name and event type from a DML statement.
// Returns ok=false for non-DML statements.
func dmlTableEvent(stmt sql.Stmt) (tableName, event string, ok bool) {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		return s.Table, "INSERT", true
	case *sql.UpdateStmt:
		return s.Table, "UPDATE", true
	case *sql.DeleteStmt:
		return s.Table, "DELETE", true
	}
	return "", "", false
}

// dmlSchemaContext returns the schema context to use for trigger lookup for
// the given table.
func (e *SelectEngine) dmlSchemaContext(tableName string) *DatabaseContext {
	if e.ctx.CurrentDMLCtx() != nil {
		return e.ctx.CurrentDMLCtx()
	}
	if _, c, err := e.ctx.FindTable(tableName); err == nil && c != nil {
		return c
	}
	return e.ctx.MainDB()
}

// validateMatchingTrigger validates a single trigger if its event matches.
func (e *SelectEngine) validateMatchingTrigger(t *schema.Entry, event string) error {
	if t == nil {
		return nil
	}
	_, declEvent := parseTriggerHeader(t.SQL)
	if declEvent != event {
		return nil
	}
	stmts, perr := parse.ParseSQL(t.SQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	trig := findTriggerStmt(stmts)
	if trig == nil {
		return nil
	}
	for _, bodyStmt := range trig.Statements {
		if sel, ok := bodyStmt.(*sql.SelectStmt); ok {
			if err := e.validateSelectExprs(sel); err != nil {
				return err
			}
		}
	}
	return nil
}

// findTriggerStmt finds the first CreateTriggerStmt in a parsed statement list.
func findTriggerStmt(stmts []sql.Stmt) *sql.CreateTriggerStmt {
	for _, st := range stmts {
		if c, ok := st.(*sql.CreateTriggerStmt); ok {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// No-FROM column reference validation
// ---------------------------------------------------------------------------

// validateNoFromColumnRefs rejects column references in a FROM-less SELECT.
// SQLite's name resolver has no table to resolve against, so any column ref
// (qualified or not) is an error. TRUE/FALSE and time literals are exempt.
func (e *SelectEngine) validateNoFromColumnRefs(s *sql.SelectStmt) error {
	v := &noFromRefValidator{engine: e}
	for _, col := range s.Columns {
		// A bare * or alias.* in a FROM-less SELECT is an error: SQLite has
		// no table to expand it against ("no tables specified").
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			return fmt.Errorf("no tables specified")
		}
		v.checkExpr(col.Expr)
	}
	aliasNames := make(map[string]bool)
	for _, col := range s.Columns {
		if col.As != "" {
			aliasNames[strings.ToLower(col.As)] = true
		}
	}
	v.checkWhere(s.Where, aliasNames)
	return v.err
}

// noFromRefValidator carries the first error found while checking column
// references in a FROM-less SELECT.
type noFromRefValidator struct {
	engine *SelectEngine
	err    error
}

// checkExpr walks expr looking for invalid column references.
func (v *noFromRefValidator) checkExpr(expr sql.Expr) {
	if v.err != nil || expr == nil {
		return
	}
	WalkExprFull(expr, v.visitRef)
}

// visitRef is the walkExprFull callback for checkExpr.
func (v *noFromRefValidator) visitRef(e2 sql.Expr) {
	if v.err != nil {
		return
	}
	ref, ok := e2.(*sql.ColumnRef)
	if !ok {
		return
	}
	v.err = v.engine.checkNoFromRef(ref)
}

// checkWhere validates the WHERE clause, allowing references to output aliases.
func (v *noFromRefValidator) checkWhere(where sql.Expr, aliasNames map[string]bool) {
	if where == nil {
		return
	}
	WalkExprFull(where, func(e2 sql.Expr) {
		if v.err != nil {
			return
		}
		ref, ok := e2.(*sql.ColumnRef)
		if !ok {
			return
		}
		if aliasNames[strings.ToLower(ref.Name)] {
			return
		}
		v.err = v.engine.checkNoFromRef(ref)
	})
}

// checkNoFromRef checks a single ColumnRef for no-FROM validity, returning an
// error if it's not an exempt reference (TRUE/FALSE, time literal, trigger
// NEW/OLD, DQS).
func (e *SelectEngine) checkNoFromRef(ref *sql.ColumnRef) error {
	if ref == nil || ref.Name == "*" {
		return nil
	}
	if e.isTriggerRowRef(ref) {
		return nil
	}
	// An unqualified reference may name an output-column alias of an
	// ENCLOSING SELECT (e.g. a FROM-less subquery's SELECT column
	// referencing the outer query's "SELECT expr AS x"); such references
	// resolve through the alias stack at evaluation time (resolver01-7.1:
	// SELECT 2 AS x WHERE (SELECT x AS y WHERE 3>y)).
	if ref.Table == "" {
		if _, found := e.resolveAliasRef(ref.Name); found {
			return nil
		}
	}
	if ref.Table != "" {
		return fmt.Errorf("no such column: %s.%s", ref.Table, ref.Name)
	}
	if isExemptColumnName(ref.Name) {
		return nil
	}
	if ref.Quoted {
		return nil
	}
	return fmt.Errorf("no such column: %s", ref.Name)
}

// isTriggerRowRef checks if ref is a valid NEW.col / OLD.col reference inside a
// trigger body.
func (e *SelectEngine) isTriggerRowRef(ref *sql.ColumnRef) bool {
	if ref.Table == "" {
		return false
	}
	if strings.EqualFold(ref.Table, "new") && e.ctx.TriggerNewRow() != nil {
		if _, ok := e.ctx.TriggerNewRow().Get(ref.Name); ok {
			return true
		}
	}
	if strings.EqualFold(ref.Table, "old") && e.ctx.TriggerOldRow() != nil {
		if _, ok := e.ctx.TriggerOldRow().Get(ref.Name); ok {
			return true
		}
	}
	return false
}

// isExemptColumnName reports whether an unqualified column name is a boolean or
// time literal that should not be rejected as a no-FROM column reference.
func isExemptColumnName(name string) bool {
	upper := strings.ToUpper(name)
	switch upper {
	case "TRUE", "FALSE":
		return true
	case "CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Expression ORDER BY validation
// ---------------------------------------------------------------------------

// validateExprOrderBy validates that ORDER BY is only used inside aggregate
// functions, and that nested aggregates are not misused. It recurses through
// all sub-expressions.
func (e *SelectEngine) validateExprOrderBy(expr sql.Expr) error {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return e.validateOrderByFuncExpr(v)
	case *sql.BinaryOp:
		return e.validateBinaryOrderBy(v)
	case *sql.UnaryOp:
		return e.validateExprOrderBy(v.Operand)
	case *sql.CaseExpr:
		return e.validateCaseOrderBy(v)
	case *sql.Subquery:
		return e.validateSubqueryInnerSelect(v)
	case *sql.ExistsExpr:
		return e.validateSubqueryInnerSelect(&sql.Subquery{Select: v.Select})
	}
	return nil
}

// validateOrderByFuncExpr validates a function call's ORDER BY usage: only
// aggregate functions may have ORDER BY, and nested aggregates are checked.
func (e *SelectEngine) validateOrderByFuncExpr(v *sql.FuncCall) error {
	if len(v.OrderBy) > 0 {
		fn, ok := e.ctx.Functions().Find(v.Name)
		if ok && fn.Type != function.TypeAggregate {
			return fmt.Errorf("ORDER BY may not be used with non-aggregate %s()", v.Name)
		}
		for _, ob := range v.OrderBy {
			if nested := findNestedAggregate(ob.Expr, e.ctx.Functions()); nested != "" {
				return fmt.Errorf("misuse of aggregate function %s()", nested)
			}
		}
	}
	for _, arg := range v.Args {
		if err := e.validateExprOrderBy(arg); err != nil {
			return err
		}
	}
	return nil
}

// validateBinaryOrderBy validates both sides of a binary operation for ORDER BY
// usage.
func (e *SelectEngine) validateBinaryOrderBy(v *sql.BinaryOp) error {
	if err := e.validateExprOrderBy(v.Left); err != nil {
		return err
	}
	return e.validateExprOrderBy(v.Right)
}

// validateCaseOrderBy validates a CASE expression's sub-expressions for ORDER BY
// usage.
