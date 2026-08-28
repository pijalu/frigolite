package execexpr

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"strings"
)

func (ev *Evaluator) evalExpr(expr sql.Expr, row Row) (interface{}, error) {
	if expr == nil {
		return nil, nil
	}
	switch v := expr.(type) {
	case *sql.NumericLit:
		return evalNumericLit(v)
	case *sql.StringLit:
		return v.Value, nil
	case *sql.BlobLit:
		return v.Value, nil
	case *sql.NullLit:
		return nil, nil
	case *sql.ParameterExpr:
		// Unbound parameter placeholders evaluate to NULL.
		return nil, nil
	case *sql.ParenExpr:
		return ev.evalExpr(v.Expr, row)
	case *sql.ColumnRef:
		return ev.evalColumnRef(v, row)
	case *sql.FuncCall:
		return ev.evalFuncCall(v, row)
	case *sql.RowValue:
		// Evaluate a row value (a,b,c) into a structured slice so comparison
		// operators and IN can implement SQLite's per-element lexicographic
		// row-value semantics (with arity checks). A bare row value in a
		// SELECT list projects its first element; that unwrapping happens at
		// the projection sites.
		return ev.evalRowValueExpr(v, row)
	default:
		return ev.evalComplexExpr(expr, row)
	}
}

// evalRowValueExpr evaluates each element of a row value into a slice.
func (ev *Evaluator) evalRowValueExpr(v *sql.RowValue, row Row) (interface{}, error) {
	var values []interface{}
	for _, val := range v.Values {
		ev, err := ev.evalExpr(val, row)
		if err != nil {
			return nil, err
		}
		values = append(values, ev)
	}
	return values, nil
}

// evalExprWithCollation evaluates an expression and applies the compile-time
// collation propagation (SQLite sqlite3ExprCollSeq): when the expression's
// subtree contains an explicit COLLATE that propagates up through a function
// call, CASE, or ||, the result is wrapped in an explicit CollatedValue so an
// enclosing comparison uses that collation (collate8 semantics).
func (ev *Evaluator) evalExprWithCollation(expr sql.Expr, row Row) (interface{}, error) {
	v, err := ev.evalExpr(expr, row)
	if err != nil || v == nil {
		return v, err
	}
	// Only wrap expressions whose compile-time collation is explicit: a
	// COLLATE operator at the top, or an explicit COLLATE propagating up from
	// a function argument / CASE branch / || operand. Column collations are
	// already carried by the runtime CollatedValue marker (explicit=false).
	coll, explicit := exprCollation(expr)
	if explicit && coll != "" {
		// A CASE expression's runtime value is the selected branch (which may
		// carry that branch's collation marker). The compile-time collation
		// wins over the runtime-selected branch (SQLite resolves CASE
		// collation from the source THEN/ELSE lists, not the taken branch), so
		// override an existing marker rather than skipping the wrap.
		if cv, ok := v.(*CollatedValue); ok {
			cv.Collation = coll
			cv.Explicit = true
			return v, nil
		}
		return &CollatedValue{Value: v, Collation: coll, Explicit: true}, nil
	}
	return v, nil
}

func (ev *Evaluator) evalComplexExpr(expr sql.Expr, row Row) (interface{}, error) {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		return ev.evalExpr(v.Expr, row)
	case *sql.BinaryOp:
		return ev.evalBinaryOp(v, row)
	case *sql.UnaryOp:
		return ev.evalUnaryOp(v, row)
	case *sql.IsNull, *sql.IsNotNull, *sql.IsTrue, *sql.IsFalse:
		return ev.evalUnaryPredicate(v, row)
	case *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		return ev.evalDistinctPredicate(v, row)
	case *sql.Between:
		return ev.evalBetween(v, row)
	case *sql.InList:
		return ev.evalInList(v, row)
	case *sql.Subquery, *sql.ExistsExpr:
		return ev.evalSubqueryOrExists(v, row)
	case *sql.CaseExpr:
		return ev.evalCaseExpr(v, row)
	case *sql.CastExpr:
		return ev.evalCastExpr(v, row)
	case *sql.RaiseExpr:
		return ev.evalRaiseExpr(v, row)
	default:
		return nil, fmt.Errorf("unknown expression type: %T", expr)
	}
}

// evalUnaryPredicate evaluates a unary predicate (IS NULL, IS NOT NULL, IS
// TRUE, IS FALSE) by dispatching on its concrete type.
func (ev *Evaluator) evalUnaryPredicate(expr interface{}, row Row) (interface{}, error) {
	switch v := expr.(type) {
	case *sql.IsNull:
		return ev.evalIsNull(v, row)
	case *sql.IsNotNull:
		return ev.evalIsNotNull(v, row)
	case *sql.IsTrue:
		return ev.evalIsTrue(v, row)
	case *sql.IsFalse:
		return ev.evalIsFalse(v, row)
	}
	return nil, fmt.Errorf("unknown unary predicate type: %T", expr)
}

// evalDistinctPredicate evaluates an IS DISTINCT FROM / IS NOT DISTINCT FROM
// expression by dispatching on its concrete type.
func (ev *Evaluator) evalDistinctPredicate(expr interface{}, row Row) (interface{}, error) {
	switch v := expr.(type) {
	case *sql.IsDistinctFrom:
		return ev.evalIsDistinctFrom(v, row)
	case *sql.IsNotDistinctFrom:
		return ev.evalIsNotDistinctFrom(v, row)
	}
	return nil, fmt.Errorf("unknown distinct predicate type: %T", expr)
}

// evalSubqueryOrExists evaluates a subquery or EXISTS expression.
func (ev *Evaluator) evalSubqueryOrExists(expr interface{}, row Row) (interface{}, error) {
	switch v := expr.(type) {
	case *sql.Subquery:
		return ev.evalSubquery(v, row)
	case *sql.ExistsExpr:
		return ev.evalExists(v, row)
	}
	return nil, fmt.Errorf("unknown subquery expression type: %T", expr)
}

// ErrRaiseIgnore is the sentinel error returned when a trigger program executes
// RAISE(IGNORE). The statement that hit it is aborted without error and
// execution continues with the next statement in the trigger program. Packages
// that must recognize RAISE(IGNORE) (execdml's trigger execution) reference
// this single exported instance.
var ErrRaiseIgnore = fmt.Errorf("RAISE(IGNORE)")

// evalRaiseExpr evaluates the RAISE() special function. RAISE() is only valid
// inside a trigger program; outside one it is a syntax/semantic error. Within
// a trigger, RAISE(IGNORE) aborts the current statement (signaled via
// errRaiseIgnore) and the other kinds abort with the given error message.
func (ev *Evaluator) evalRaiseExpr(v *sql.RaiseExpr, row Row) (interface{}, error) {
	if ev.ctx.TriggerDepth() == 0 {
		return nil, fmt.Errorf("RAISE() may only be used within a trigger-program")
	}
	if strings.EqualFold(v.Kind, "IGNORE") {
		return nil, ErrRaiseIgnore
	}
	msg := ""
	if v.Message != nil {
		val, err := ev.evalExpr(v.Message, row)
		if err != nil {
			return nil, err
		}
		if val != nil {
			msg = fmt.Sprintf("%v", val)
		}
	}
	return nil, fmt.Errorf("%s", msg)
}

// evalRaiseFuncCall handles RAISE() when it reaches expression evaluation as
// a regular function call (the legacy parser represents RAISE(IGNORE) as a
// FuncCall whose first argument is a column reference). The LALR parser
// produces a *sql.RaiseExpr instead, handled by evalRaiseExpr.
func (ev *Evaluator) evalRaiseFuncCall(f *sql.FuncCall, row Row) (interface{}, error) {
	if ev.ctx.TriggerDepth() == 0 {
		return nil, fmt.Errorf("RAISE() may only be used within a trigger-program")
	}
	if len(f.Args) == 0 {
		return nil, fmt.Errorf("RAISE() requires an argument")
	}
	kind := ""
	if col, ok := f.Args[0].(*sql.ColumnRef); ok {
		kind = strings.ToUpper(col.Name)
	}
	if kind == "" {
		if s, ok := f.Args[0].(*sql.StringLit); ok {
			kind = strings.ToUpper(s.Value)
		}
	}
	if kind == "IGNORE" {
		return nil, ErrRaiseIgnore
	}
	msg := ""
	if len(f.Args) > 1 {
		val, err := ev.evalExpr(f.Args[1], row)
		if err != nil {
			return nil, err
		}
		if val != nil {
			msg = fmt.Sprintf("%v", val)
		}
	}
	return nil, fmt.Errorf("%s", msg)
}

func (ev *Evaluator) evalSubquery(v *sql.Subquery, row Row) (interface{}, error) {
	// Save and restore outerRow for correlated subquery support. Push the
	// current outer row onto a stack so nested subqueries can resolve
	// multi-level correlated references (outer → grandparent).
	ev.ctx.PushOuterRow(row)
	defer ev.ctx.PopOuterRow()

	rows, err := ev.ctx.ExecSelectRows(v.Select)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// Return first column of first row. The subquery's output column
	// affinity is preserved so the outer comparison applies it (SQLite: a
	// TEXT = (SELECT y FROM d2) with d2.y BLOB does not coerce — rowvalue9
	// 4.x.6). The raw row value from execSelect has the wrapper stripped.
	if len(rows[0]) > 0 {
		val := rows[0][0]
		if aff := ev.subqueryOutputAffinity(v.Select, 0); aff != 0 {
			if _, isCV := val.(*util.ColumnValue); !isCV {
				return &util.ColumnValue{Value: util.UnwrapColumnValue(val), Affinity: aff}, nil
			}
		}
		return val, nil
	}
	return nil, nil
}

// subqueryRowWithAffinity wraps each element of a subquery result row in a
// ColumnValue carrying the subquery output column's affinity, so an outer
// row-value comparison applies it (SQLite: (c, a) = (SELECT x, y FROM d2)
// with d2.x/d2.y BLOB does not coerce — rowvalue9 4.x.4). A column-derived
// output (a column reference or unary-plus-of-column, e.g. SELECT +bb) also
// keeps its column-ness (and non-BINARY column collation) so an outer
// row-value element comparison resolves the collation from the LEFT side and
// does not fall back to the right operand's collation (rowvalue 23.110).
func (ev *Evaluator) subqueryRowWithAffinity(subq *sql.Subquery, row []interface{}) []interface{} {
	out := make([]interface{}, len(row))
	for i, val := range row {
		if val == nil {
			out[i] = nil
			continue
		}
		if aff := ev.subqueryOutputAffinity(subq.Select, i); aff != 0 {
			out[i] = &util.ColumnValue{Value: util.UnwrapColumnValue(val), Affinity: aff}
			continue
		}
		if coll := ev.subqueryOutputCollation(subq.Select, i); coll != "" {
			cv := &util.ColumnValue{Value: util.UnwrapColumnValue(val), Affinity: 0}
			if !strings.EqualFold(coll, "BINARY") {
				out[i] = &CollatedValue{Value: cv, Collation: strings.ToUpper(coll)}
			} else {
				out[i] = cv
			}
			continue
		}
		if ev.subqueryOutputColumnish(subq.Select, i) {
			out[i] = &util.ColumnValue{Value: util.UnwrapColumnValue(val), Affinity: 0}
			continue
		}
		out[i] = val
	}
	return out
}

// subqueryOutputCollation returns the collation of subquery output column i
// ("" for BINARY), resolved through the FROM source's column definitions for
// column references and unary-plus-of-column expressions.
func (ev *Evaluator) subqueryOutputCollation(sel *sql.SelectStmt, i int) string {
	if sel == nil || i >= len(sel.Columns) {
		return ""
	}
	return ev.exprOutputCollation(sel, sel.Columns[i].Expr, i)
}

// exprOutputCollation resolves the collation of a subquery output column
// expression. Column references and unary plus of a column resolve through the
// FROM source; anything else has no collation.
func (ev *Evaluator) exprOutputCollation(sel *sql.SelectStmt, expr sql.Expr, i int) string {
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return ev.exprRefColumnCollation(sel, v, i)
	case *sql.UnaryOp:
		if v.Operator == "+" {
			if ref, ok := v.Operand.(*sql.ColumnRef); ok {
				return ev.exprRefColumnCollation(sel, ref, i)
			}
		}
	}
	return ""
}

// exprRefColumnCollation looks up the declared collation of a column reference
// against the FROM source's column definitions.
func (ev *Evaluator) exprRefColumnCollation(sel *sql.SelectStmt, v *sql.ColumnRef, i int) string {
	defs := ev.ctx.FromSourceColumnDefs(sel.From, nil)
	for _, cd := range defs {
		if cd.Name == v.Name || (v.Table != "" && cd.Name == v.Table+"."+v.Name) {
			if cd.Collate != "" && !strings.EqualFold(cd.Collate, "BINARY") {
				return strings.ToUpper(cd.Collate)
			}
			return ""
		}
	}
	return ""
}

// subqueryOutputColumnish reports whether subquery output column i is
// column-derived (a column reference or unary-plus-of-column), so its result
// keeps column-ness when used in a row-value comparison.
func (ev *Evaluator) subqueryOutputColumnish(sel *sql.SelectStmt, i int) bool {
	if sel == nil || i >= len(sel.Columns) {
		return false
	}
	switch v := sel.Columns[i].Expr.(type) {
	case *sql.ColumnRef:
		return true
	case *sql.UnaryOp:
		if v.Operator == "+" {
			_, ok := v.Operand.(*sql.ColumnRef)
			return ok
		}
	}
	return false
}

func (ev *Evaluator) evalExists(v *sql.ExistsExpr, row Row) (interface{}, error) {
	// Propagate outerRow for correlated subquery references
	ev.ctx.PushOuterRow(row)
	defer ev.ctx.PopOuterRow()

	rows, err := ev.ctx.ExecSelectRows(v.Select)
	if err != nil {
		return nil, err
	}
	exists := len(rows) > 0
	if v.Negated {
		exists = !exists
	}
	return boolToInt(exists), nil
}

func (ev *Evaluator) evalCaseExpr(v *sql.CaseExpr, row Row) (interface{}, error) {
	if v.Operand != nil {
		return ev.evalCaseWithOperand(v, row)
	}
	for _, w := range v.Whens {
		when, err := ev.evalExpr(w.When, row)
		if err != nil {
			return nil, err
		}
		if ToBool(when) {
			return ev.evalExpr(w.Then, row)
		}
	}
	return ev.evalCaseElse(v, row)
}

func (ev *Evaluator) evalCaseWithOperand(v *sql.CaseExpr, row Row) (interface{}, error) {
	// A subquery operand in a row-value CASE (CASE (SELECT a,b ...) WHEN
	// (1,2) ...) is evaluated as its full row, not just the first column.
	if subq, ok := v.Operand.(*sql.Subquery); ok {
		rows, err := ev.evalSubqueryRows(subq, row)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 || len(rows[0]) == 0 {
			return ev.evalCaseElse(v, row)
		}
		if len(rows[0]) > 1 {
			opRow := make([]interface{}, len(rows[0]))
			for i := range rows[0] {
				opRow[i] = util.UnwrapColumnValue(rows[0][i])
			}
			return ev.evalRowValueCaseWhens(v, opRow, row)
		}
		operand := util.UnwrapColumnValue(rows[0][0])
		return ev.evalScalarCaseWhens(v, operand, row)
	}
	operand, err := ev.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if operand != nil {
		if opRow, ok := operand.([]interface{}); ok {
			return ev.evalRowValueCaseWhens(v, opRow, row)
		}
		return ev.evalScalarCaseWhens(v, operand, row)
	}
	return ev.evalCaseElse(v, row)
}

// evalScalarCaseWhens compares a scalar CASE operand against each WHEN with
// simple-CASE = semantics (NULL operand never matches).
func (ev *Evaluator) evalScalarCaseWhens(v *sql.CaseExpr, operand interface{}, row Row) (interface{}, error) {
	if operand == nil {
		return ev.evalCaseElse(v, row)
	}
	for _, w := range v.Whens {
		when, err := ev.evalExpr(w.When, row)
		if err != nil {
			return nil, err
		}
		if ev.ctx.CompareValuesWithCollate(operand, when) == 0 {
			return ev.evalExpr(w.Then, row)
		}
	}
	return ev.evalCaseElse(v, row)
}

func (ev *Evaluator) evalCaseElse(v *sql.CaseExpr, row Row) (interface{}, error) {
	if v.Else != nil {
		return ev.evalExpr(v.Else, row)
	}
	return nil, nil
}

// isTextAffinityType reports whether a CAST target type name has TEXT
// affinity (SQLite affinity rules): VARCHAR, CHARACTER, CLOB, and any type
// containing those substrings (e.g. VARCHAR(50), CHARACTER(20)).
func isTextAffinityType(typeName string) bool {
	upper := strings.ToUpper(typeName)
	for _, kw := range []string{"CHAR", "CLOB", "TEXT"} {
		if strings.Contains(upper, kw) {
			return true
		}
	}
	return false
}

// isHexLiteral reports whether s is a hexadecimal integer literal
// (optionally with a leading + or - sign), e.g. "0x1A" or "-0xFF".
func isHexLiteral(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	return i+2 <= len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') && i+2 < len(s)
}
func (ev *Evaluator) evalColumnRef(v *sql.ColumnRef, row Row) (interface{}, error) {
	// CURRENT_TIME / CURRENT_DATE / CURRENT_TIMESTAMP are SQL keywords
	// evaluated as the statement-cached current time (SQLite evaluates them
	// once per statement via sqlite3StmtCurrentTime). They are equivalent to
	// time('now'), date('now'), and datetime('now') respectively.
	if v.Table == "" {
		if val, ok := evalCurrentTimeKeyword(v.Name); ok {
			return val, nil
		}
		// TRUE/FALSE keywords are boolean literals (1/0), not column references.
		// The parser represents them as ColumnRef{Name:"TRUE"} / {Name:"FALSE"}.
		if strings.EqualFold(v.Name, "TRUE") {
			return int64(1), nil
		}
		if strings.EqualFold(v.Name, "FALSE") {
			return int64(0), nil
		}
	}
	if v.Name == "*" {
		// RETURNING rejects table-qualified wildcards ("t1.*").
		if ev.ctx.ReturningStrict() && v.Table != "" {
			return nil, fmt.Errorf("RETURNING may not use \"%s.*\" wildcards", v.Table)
		}
		return "*", nil
	}
	// Qualified column reference: the qualified path always completes (a
	// non-resolving qualified ref returns nil, matching the original).
	if v.Table != "" {
		return ev.evalQualifiedColumnRef(v, row)
	}
	// Unqualified: check short name
	return ev.evalUnqualifiedColumnRef(v, row)
}

// evalCurrentTimeKeyword evaluates the CURRENT_TIME / CURRENT_DATE /
// CURRENT_TIMESTAMP keyword literals, returning ok=false for other names.
func evalCurrentTimeKeyword(name string) (interface{}, bool) {
	switch strings.ToUpper(name) {
	case "CURRENT_TIME":
		v, _ := function.FnTimeNow()
		return v, true
	case "CURRENT_DATE":
		v, _ := function.FnDateNow()
		return v, true
	case "CURRENT_TIMESTAMP":
		v, _ := function.FnDateTimeNow()
		return v, true
	}
	return nil, false
}

// rowHasQualifiedKeys reports whether a row map contains any table-qualified
// keys ("t1.a"), indicating it came from a join result rather than a bare
// single-table scan.
