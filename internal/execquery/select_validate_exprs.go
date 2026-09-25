package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
)

// SELECT expression validation (split from select_agg_validate.go for
// file-size hygiene): prepare-time clause checks — aggregate misuse in
// WHERE/GROUP BY/HAVING/ORDER BY, FILTER-clause rules, DISTINCT aggregate
// arity, subquery resolution, aliased window-function misuse, and the
// ORDER BY / result-shaping validations.

// ---------------------------------------------------------------------------
// SELECT expression validation
// ---------------------------------------------------------------------------

// orderByWindowAliasRef returns the name of a SELECT-list window-function
// alias referenced from a scalar subquery in the ORDER BY clause (SQLite
// rejects ORDER BY (SELECT m) on SELECT count() OVER() AS m with "misuse of
// aliased window function m"). Returns "" when no such reference exists.
func (e *SelectEngine) orderByWindowAliasRef(s *sql.SelectStmt) string {
	// Map SELECT-list aliases to window-function expressions.
	winAliases := make(map[string]bool)
	for _, col := range s.Columns {
		if col.As == "" || !e.exprHasWindowFunc(col.Expr) {
			continue
		}
		winAliases[strings.ToLower(col.As)] = true
	}
	if len(winAliases) == 0 {
		return ""
	}
	for _, ob := range s.OrderBy {
		if name := subqueryWindowAliasRef(ob.Expr, winAliases); name != "" {
			return name
		}
	}
	return ""
}

// subqueryWindowAliasRef returns the first SELECT-list window-function alias
// referenced bare inside a scalar subquery of expr, or "".
func subqueryWindowAliasRef(expr sql.Expr, winAliases map[string]bool) string {
	found := ""
	WalkExprFull(expr, func(en sql.Expr) {
		if found != "" {
			return
		}
		sub, ok := en.(*sql.Subquery)
		if !ok || sub.Select == nil {
			return
		}
		for _, col := range sub.Select.Columns {
			if name := bareColWindowAlias(col.Expr, winAliases); name != "" {
				found = name
				return
			}
		}
	})
	return found
}

// bareColWindowAlias returns the first bare column reference in expr whose
// name matches a window-function alias, or "".
func bareColWindowAlias(expr sql.Expr, winAliases map[string]bool) string {
	found := ""
	WalkExprFull(expr, func(en sql.Expr) {
		if found != "" {
			return
		}
		if ref, ok := en.(*sql.ColumnRef); ok && ref.Table == "" {
			if winAliases[strings.ToLower(ref.Name)] {
				found = ref.Name
			}
		}
	})
	return found
}

// validateSelectExprs validates aggregate misuse, DISTINCT aggregate arity,
// subquery validity, ORDER BY length limits, row-value misuse, and UNION
// subquery aggregates across a SELECT's clauses.
func (e *SelectEngine) validateSelectExprs(s *sql.SelectStmt) error {
	if err := e.validateSelectExprsClauses(s); err != nil {
		return err
	}
	return e.validateSelectExprsOrdering(s)
}

// validateSelectExprsClauses validates the SELECT list and each auxiliary
// clause (GROUP BY / HAVING / WHERE / LIMIT / OFFSET / ORDER BY terms).
func (e *SelectEngine) validateSelectExprsClauses(s *sql.SelectStmt) error {
	if err := e.validateMultipleFTSMatch(s); err != nil {
		return err
	}
	if err := e.checkOrderByAggMisuse(s); err != nil {
		return err
	}
	if err := e.validateAggregateStarArgs(s); err != nil {
		return err
	}
	if err := e.validateSelectColumnList(s); err != nil {
		return err
	}
	if err := e.validateWindowFunctions(s); err != nil {
		return err
	}
	if err := e.validateGroupByClauses(s); err != nil {
		return err
	}
	// LIMIT/OFFSET expressions are name-resolved at prepare time like other
	// clauses: a subquery naming a missing table fails the statement
	// ("no such table: blah", misc5-3.2), it is not silently un-evaluable.
	if err := e.validateLimitOffsetSubqueries(s); err != nil {
		return err
	}
	if err := e.validateHavingExprs(s); err != nil {
		return err
	}
	if err := e.validateHavingAliasedAggregate(s); err != nil {
		return err
	}
	if err := e.validateWhereExprs(s); err != nil {
		return err
	}
	return e.validateOrderByTerms(s)
}

// validateLimitOffsetSubqueries validates scalar subqueries inside the
// LIMIT/OFFSET expressions.
func (e *SelectEngine) validateLimitOffsetSubqueries(s *sql.SelectStmt) error {
	for _, limExpr := range []sql.Expr{s.Limit, s.Offset} {
		if limExpr == nil {
			continue
		}
		if err := e.validateExprSubqueries(limExpr); err != nil {
			return err
		}
	}
	return nil
}

// validateGroupByClauses rejects aggregates in GROUP BY and HAVING.
func (e *SelectEngine) validateGroupByClauses(s *sql.SelectStmt) error {
	if err := e.validateGroupByExprs(s); err != nil {
		return err
	}
	if err := e.validateClauseFunctions(s.GroupBy); err != nil {
		return err
	}
	return e.validateClauseFunctions([]sql.Expr{s.Having})
}

// validateOrderByTerms validates each ORDER BY term: DISTINCT aggregate arity
// and scalar-subquery resolution. SQLite resolves ORDER BY term
// names/subqueries even when the term does not match a result column
// (window1 67.1: a nested (SELECT 1 FROM v1) inside a window's ORDER BY must
// raise "no such table: v1").
func (e *SelectEngine) validateOrderByTerms(s *sql.SelectStmt) error {
	for _, ob := range s.OrderBy {
		if err := validateDistinctAggArgs(ob.Expr); err != nil {
			return err
		}
		if err := e.validateExprSubqueries(ob.Expr); err != nil {
			return err
		}
	}
	return nil
}

// validateSelectExprsOrdering runs the ORDER BY / result-shaping validations:
// aliased window-function references, row values, compound subquery
// aggregates, nested aggregates, and schema collation registration.
func (e *SelectEngine) validateSelectExprsOrdering(s *sql.SelectStmt) error {
	// A scalar subquery in ORDER BY that references a SELECT-list alias of a
	// window function is a misuse (window1 43.x: ORDER BY (SELECT m) on
	// SELECT count() OVER() AS m).
	if name := e.orderByWindowAliasRef(s); name != "" {
		return fmt.Errorf("misuse of aliased window function %s", name)
	}
	if err := e.validateSelectRowValues(s); err != nil {
		return err
	}
	if s.From.Subquery != nil {
		if err := validateUnionSubqueryNoAggs(s.From.Subquery); err != nil {
			return err
		}
	}
	if err := e.checkOrderByNestedAgg(s); err != nil {
		return err
	}
	// Schema-declared collations resolve at prepare time: ORDER BY/GROUP BY
	// sort keys, DISTINCT dedup, and compound set-op/ORDER BY column
	// collations must be registered (build.c sqlite3LocateCollSeq; a
	// close/reopen without re-registering a schema collation fails these
	// with "no such collation sequence: NAME" — collate3-2.x).
	if err := e.validateSchemaCollations(s); err != nil {
		return err
	}
	return e.validateCompoundTermLimit(s)
}

// validateSelectColumnList validates each SELECT column expression for ORDER BY
// terms, subquery validity, ORDER BY length, DISTINCT aggregate arity, and
// FILTER clause misuse (FILTER only on aggregates, no window/aggregate inside
// FILTER).
func (e *SelectEngine) validateSelectColumnList(s *sql.SelectStmt) error {
	for _, col := range s.Columns {
		if err := e.validateExprOrderBy(col.Expr); err != nil {
			return err
		}
		if err := validateOrderByLength(col.Expr, 1000); err != nil {
			return err
		}
		if err := e.validateFilterClause(col.Expr); err != nil {
			return err
		}
		if err := e.validateExprSubqueries(col.Expr); err != nil {
			return err
		}
		if err := validateDistinctAggArgs(col.Expr); err != nil {
			return err
		}
	}
	return nil
}

// validateFilterClause validates FILTER clauses on aggregate functions:
// FILTER may only be used with aggregates, and FILTER expressions must not
// contain window functions or aggregate functions.
func (e *SelectEngine) validateFilterClause(expr sql.Expr) error {
	var firstErr error
	WalkExprFull(expr, func(en sql.Expr) {
		if firstErr != nil {
			return
		}
		if fn, ok := en.(*sql.FuncCall); ok && fn.Filter != nil {
			firstErr = e.filterClauseError(fn)
		}
	})
	return firstErr
}

// filterClauseError validates one function call carrying a FILTER clause.
// SQLite reports a different message for a FILTER on a window function (which
// is not an aggregate) vs a plain non-aggregate scalar function
// (src/window.c:691 vs src/resolve.c:1282).
func (e *SelectEngine) filterClauseError(fn *sql.FuncCall) error {
	reg, found := e.ctx.Functions().Find(fn.Name)
	if !found || reg.Type != function.TypeAggregate {
		if fn.Over != nil {
			return fmt.Errorf("FILTER clause may only be used with aggregate window functions")
		}
		return fmt.Errorf("FILTER may not be used with non-aggregate %s()", fn.Name)
	}
	if nested := FindAggregateInExpr(fn.Filter); nested != "" {
		return fmt.Errorf("misuse of aggregate function %s()", nested)
	}
	if nested := e.windowFuncInExpr(fn.Filter); nested != "" {
		return fmt.Errorf("misuse of window function %s()", nested)
	}
	return nil
}

// validateGroupByExprs rejects aggregate functions inside GROUP BY
// expressions. SQLite: "aggregate functions are not allowed in the GROUP BY
// clause". Numeric ordinals resolve to their SELECT-column expressions at
// execution (resolveGroupByOrdinals), where out-of-range and aggregate
// ordinals surface the same way (misc4-4.1/4.2).
func (e *SelectEngine) validateGroupByExprs(s *sql.SelectStmt) error {
	for _, gb := range s.GroupBy {
		if nested := FindAggregateInExpr(gb); nested != "" {
			return fmt.Errorf("aggregate functions are not allowed in the GROUP BY clause")
		}
	}
	// Numeric ordinals resolve to their SELECT-column expressions first
	// (resolve.c maps GROUP BY N to the Nth result column before the
	// no-aggregate check), so "GROUP BY 1, 2" over a list holding max(Value)
	// is rejected too (misc4-4.1/4.2). colDefs only matter for the SELECT *
	// mapping, which can never contain an aggregate.
	resolved, err := resolveGroupByOrdinals(s, nil)
	if err != nil {
		return err
	}
	for _, gb := range resolved {
		if nested := FindAggregateInExpr(gb); nested != "" {
			return fmt.Errorf("aggregate functions are not allowed in the GROUP BY clause")
		}
	}
	return nil
}

// validateHavingExprs validates the HAVING clause for ORDER BY terms, subqueries,
// and DISTINCT aggregate arity.
func (e *SelectEngine) validateHavingExprs(s *sql.SelectStmt) error {
	if s.Having == nil {
		return nil
	}
	if err := e.validateExprOrderBy(s.Having); err != nil {
		return err
	}
	if err := e.validateExprSubqueries(s.Having); err != nil {
		return err
	}
	return validateDistinctAggArgs(s.Having)
}

// validateWhereExprs validates the WHERE clause for subquery validity and
// DISTINCT aggregate arity. A correlated aggregate in a WHERE scalar subquery
// is a misuse (SQLite: "misuse of aggregate: X()").
func (e *SelectEngine) validateWhereExprs(s *sql.SelectStmt) error {
	if s.Where == nil {
		return nil
	}
	if err := e.validateExprSubqueries(s.Where); err != nil {
		return err
	}
	// A row-value comparison whose subquery operand contains a correlated
	// aggregate collapses the subquery's vector to a single value at VDBE
	// codegen (SQLite: "N columns assigned 1 values"; window1 71.0 with
	// WHERE (a,1)=(SELECT 2,2 UNION SELECT sum(b),max(b) OVER(ORDER BY b))).
	if n := e.whereRowValueCorrelatedAggCollapse(s.Where); n > 0 {
		return fmt.Errorf("%d columns assigned 1 values", n)
	}
	if name := e.whereSubqueryOuterAggRef(s.Where); name != "" {
		return fmt.Errorf("misuse of aggregate: %s()", name)
	}
	if name := e.whereInSubqOuterAggRef(s.Where); name != "" {
		return fmt.Errorf("misuse of aggregate: %s()", name)
	}
	// A scalar aggregate used DIRECTLY in this level's WHERE is always a
	// misuse (resolve.c: sqlite3ResolveExprNames clears NC_AllowAgg for the
	// WHERE subtree — "misuse of aggregate: max()", tkt1514/tkt3508). The
	// walk does not descend into subqueries: their WHERE clauses are
	// validated against their own scope by the nested validateWhereExprs.
	// resolve.c:1960 exception: when this SELECT is itself an aggregate query
	// (result-set aggregates or GROUP BY), WHERE resolution keeps NC_AllowAgg,
	// and the resolve.c:1332 context walk transfers an aggregate whose
	// arguments reference no column of this SELECT's own FROM to an outer
	// aggregate context (aggnested-3.11: WHERE value2=max(value1)).
	if name := e.whereDirectAggregateScoped(s); name != "" {
		return fmt.Errorf("misuse of aggregate: %s()", name)
	}
	// A WHERE reference to a SELECT alias whose expression IS an aggregate
	// resolves to that aggregate and is the same misuse (resolve.c name
	// resolution falls through to the output alias; tkt3508: "where c > 1"
	// with count(x) AS c).
	for _, col := range s.Columns {
		if col.As == "" || col.Expr == nil {
			continue
		}
		if agg := expressionAggregateName(col.Expr, e.ctx.Functions()); agg != "" {
			if whereReferencesBareName(s.Where, col.As) {
				return fmt.Errorf("misuse of aggregate: %s()", agg)
			}
		}
	}
	return validateDistinctAggArgs(s.Where)
}

// expressionAggregateName returns the lowercased name of the first scalar
// aggregate directly contained in the expression (no subquery descent).
func expressionAggregateName(expr sql.Expr, fns *function.Registry) string {
	if expr == nil {
		return ""
	}
	found := ""
	WalkExprFull(expr, func(n sql.Expr) {
		if found != "" {
			return
		}
		if fn, ok := n.(*sql.FuncCall); ok {
			if reg, found2 := fns.Find(fn.Name); found2 && reg.Type == function.TypeAggregate {
				found = strings.ToLower(fn.Name)
			}
		}
	})
	return found
}
