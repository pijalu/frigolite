package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
)

// SELECT prepare-time validators added by FULL-SUITE-DRIFT T26-select, split
// out of select_agg_validate.go for the 1000-line file-size gate.

// validateCompoundTermLimit enforces SQLITE_LIMIT_COMPOUND_SELECT (default
// 500): a compound chain with more terms errors "too many terms in compound
// SELECT" at prepare (select7-1.x's 501-term UNION ALL probe). Pure-VALUES
// compounds are exempt: the grammar check (parse.y parserDoubleLinkSelect)
// tests `(p->selFlags & (SF_MultiValue|SF_Values))==0` on the chain head, so
// VALUES('a'),('b'),... rows never count against the limit even inside a
// scalar subquery (values-4.x: a 4-row VALUES subquery with limit 3 is legal).
func (e *SelectEngine) validateCompoundTermLimit(s *sql.SelectStmt) error {
	if s.Union == nil || s.ValuesChain {
		return nil
	}
	n := 1
	for cur := s.Union; cur != nil; cur = cur.Union {
		n++
	}
	if limit := e.ctx.CompoundSelectLimit(); limit > 0 && n > limit {
		return fmt.Errorf("too many terms in compound SELECT")
	}
	return nil
}

// validateAggregateStarArgs enforces aggregate argument-shape rules at
// prepare time (sqlite3WrongNumArgs): a `*` argument is only valid for a
// single-argument COUNT, and an aggregate whose argument count falls outside
// its registered arity errors "wrong number of arguments to function X()"
// with the name spelled exactly as in the SQL (select1-2.6 min(*), 2.9
// MAX(), 2.14 SUM()). Only aggregates are checked here — scalar arity keeps
// its established eval-time reporting.
func (e *SelectEngine) validateAggregateStarArgs(s *sql.SelectStmt) error {
	clauses := make([]sql.Expr, 0, len(s.Columns)+len(s.GroupBy)+len(s.OrderBy)+2)
	for _, col := range s.Columns {
		if col.Expr != nil {
			clauses = append(clauses, col.Expr)
		}
	}
	clauses = append(clauses, s.Where)
	clauses = append(clauses, s.Having)
	for _, g := range s.GroupBy {
		clauses = append(clauses, g)
	}
	for _, ob := range s.OrderBy {
		clauses = append(clauses, ob.Expr)
	}
	var firstErr error
	for _, expr := range clauses {
		if expr == nil {
			continue
		}
		WalkExprFull(expr, func(n sql.Expr) {
			if firstErr != nil {
				return
			}
			v, ok := n.(*sql.FuncCall)
			if !ok {
				return
			}
			reg, found := e.ctx.Functions().Find(v.Name)
			if !found || reg.Type != function.TypeAggregate {
				return
			}
			// Star argument: SQLite's grammar (parse.y `expr ::= idj LP STAR
			// RP` → sqlite3ExprFunction(pParse, 0, ...)) turns f(*) into a
			// ZERO-argument call. count is registered with both 0- and
			// 1-arg overloads (func.c WAGGREGATE count,0 / count,1), so
			// count(*) is legal, and the TCL fixture aggregate x_count is
			// registered with nArg 0 and 1 (test1.c test_create_aggregate)
			// so x_count(*) is legal too (aggerror-1.1). Any aggregate whose
			// registered MINIMUM arity is > 0 errors — min(*)/max(*)/sum(*)
			// keep "wrong number of arguments to function X()"
			// (select1-2.6/2.9/2.14).
			star := 0
			for _, a := range v.Args {
				if ref, ok := sql.UnwrapParenExpr(a).(*sql.ColumnRef); ok && ref.Name == "*" && ref.Table == "" {
					star++
				}
			}
			if star > 0 {
				if reg.MinArgs > 0 {
					firstErr = fmt.Errorf("wrong number of arguments to function %s()", v.Name)
				}
				return
			}
			if len(v.Args) < reg.MinArgs || (reg.MaxArgs >= 0 && len(v.Args) > reg.MaxArgs) {
				firstErr = fmt.Errorf("wrong number of arguments to function %s()", v.Name)
			}
		})
		if firstErr != nil {
			return firstErr
		}
	}
	return nil
}

// validateHavingAliasedAggregate rejects a HAVING aggregate whose argument
// references a SELECT alias whose own expression IS an aggregate (SQLite
// resolve.c: the alias reference expands to the aggregate expression, nesting
// one aggregate inside another — "misuse of aliased aggregate m",
// select1-7.x).
func (e *SelectEngine) validateHavingAliasedAggregate(s *sql.SelectStmt) error {
	if s.Having == nil {
		return nil
	}
	for _, col := range s.Columns {
		if col.As == "" || col.Expr == nil {
			continue
		}
		if expressionAggregateName(col.Expr, e.ctx.Functions()) == "" {
			continue
		}
		if havingAggregateReferencesAlias(s.Having, col.As, e.ctx.Functions()) {
			return fmt.Errorf("misuse of aliased aggregate %s", col.As)
		}
	}
	return nil
}

// havingAggregateReferencesAlias reports whether any aggregate call inside
// expr contains a bare column reference naming alias.
func havingAggregateReferencesAlias(expr sql.Expr, alias string, fns *function.Registry) bool {
	if expr == nil {
		return false
	}
	found := false
	var walk func(n sql.Expr, insideAgg bool)
	walk = func(n sql.Expr, insideAgg bool) {
		if found {
			return
		}
		switch v := n.(type) {
		case *sql.Subquery, *sql.ExistsExpr:
			return
		case *sql.FuncCall:
			reg, isAggFn := fns.Find(v.Name)
			aggHere := insideAgg || (isAggFn && reg.Type == function.TypeAggregate)
			for _, a := range v.Args {
				walk(a, aggHere)
			}
			return
		case *sql.ColumnRef:
			if insideAgg && v.Table == "" && strings.EqualFold(v.Name, alias) {
				found = true
			}
			return
		}
		for _, child := range aggValidateChildExprs(n) {
			walk(child, insideAgg)
		}
	}
	walk(expr, false)
	return found
}

// validateClauseFunctions rejects GROUP BY/HAVING terms that call an unknown
// function. The group-key evaluation swallows evaluation errors (a term that
// errors groups by NULL) and a HAVING reference to an unknown function
// otherwise slips past prepare, so the SQLite prepare-time name resolution
// ("no such function: z", select5-2.2/2.4) must happen up front. Window
// functions (OVER) are validated elsewhere.
func (e *SelectEngine) validateClauseFunctions(clauses []sql.Expr) error {
	for _, expr := range clauses {
		if expr == nil {
			continue
		}
		unknown := ""
		WalkExprFull(expr, func(n sql.Expr) {
			if unknown != "" {
				return
			}
			if fn, ok := n.(*sql.FuncCall); ok && fn.Over == nil {
				if _, found := e.ctx.Functions().Find(fn.Name); !found {
					unknown = fn.Name
				}
			}
		})
		if unknown != "" {
			return fmt.Errorf("no such function: %s", unknown)
		}
	}
	return nil
}

// checkOrderByAggMisuse rejects aggregate functions in ORDER BY when the SELECT
// is not an aggregate query (no GROUP BY, no aggregate in SELECT list).
// Compound queries skip this: a trailing ORDER BY on a compound member is the
// compound-level ORDER BY, where aggregates are permitted.
func (e *SelectEngine) checkOrderByAggMisuse(s *sql.SelectStmt) error {
	if len(s.OrderBy) == 0 || s.GroupBy != nil || e.inCompoundMember || s.Union != nil {
		return nil
	}
	isAgg := e.hasAggregates(s.Columns)
	for _, ob := range s.OrderBy {
		if e.exprHasAggregate(ob.Expr) && !isAgg {
			return fmt.Errorf("misuse of aggregate: %s()", e.aggregateName(ob.Expr))
		}
		// An aggregate inside a scalar subquery in ORDER BY that references
		// outer columns is a misuse (window1 61.4.3: ORDER BY (SELECT sum(a)
		// FROM t2) where a is t1's column). A subquery aggregate over its own
		// FROM is fine (61.4.4).
		if !isAgg {
			if name := e.orderBySubqueryOuterAgg(ob.Expr); name != "" {
				return fmt.Errorf("misuse of aggregate: %s()", name)
			}
		}
	}
	return nil
}

// orderBySubqueryOuterAgg returns the name of the first correlated aggregate
// (aggregate referencing columns outside the subquery's own FROM) inside a
// scalar subquery in an ORDER BY expression, or "".
func (e *SelectEngine) orderBySubqueryOuterAgg(expr sql.Expr) string {
	found := ""
	WalkExprFull(expr, func(en sql.Expr) {
		if found != "" {
			return
		}
		if sub, ok := en.(*sql.Subquery); ok && sub.Select != nil {
			if n := e.subqueryOuterAggRef(sub.Select); n != "" {
				found = n
			}
		}
	})
	return found
}

// nestedSubqueryPromotedAgg returns the name of an aggregate inside a FROM
// subquery of the given SELECT that references columns outside that inner
// level's own FROM (a promoted aggregate), or "".
func (e *SelectEngine) nestedSubqueryPromotedAgg(s *sql.SelectStmt) string {
	for cur := s; cur != nil; cur = cur.Union {
		if cur.From.Subquery != nil {
			if name := e.subqueryOuterAggRef(cur.From.Subquery); name != "" {
				return name
			}
			if name := e.nestedSubqueryPromotedAgg(cur.From.Subquery); name != "" {
				return name
			}
		}
	}
	return ""
}
