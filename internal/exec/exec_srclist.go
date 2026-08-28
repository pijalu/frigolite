package exec

import (
	"github.com/pijalu/frigolite/internal/sql"
)

func countStatementFromTerms(stmt sql.Stmt) int {
	count := 0
	var countSelect func(*sql.SelectStmt)
	countSelect = func(s *sql.SelectStmt) {
		if s == nil {
			return
		}
		if s.From.Name != "" || s.From.Subquery != nil {
			count++
		}
		count += len(s.Joins)
		for _, cte := range s.CTEs {
			countSelect(cte.Select)
		}
		if s.From.Subquery != nil {
			countSelect(s.From.Subquery)
		}
		for i := range s.Joins {
			if s.Joins[i].Table.Subquery != nil {
				countSelect(s.Joins[i].Table.Subquery)
			}
		}
		for _, col := range s.Columns {
			countSelectExprSubqueries(col.Expr, countSelect)
		}
		countSelectExprSubqueries(s.Where, countSelect)
		for _, g := range s.GroupBy {
			countSelectExprSubqueries(g, countSelect)
		}
		countSelectExprSubqueries(s.Having, countSelect)
		for _, ob := range s.OrderBy {
			countSelectExprSubqueries(ob.Expr, countSelect)
		}
		if s.Union != nil {
			countSelect(s.Union)
		}
	}
	countSelectExprSubqueriesInStmt(stmt, countSelect)
	return count
}

// countSelectExprSubqueries walks an expression tree invoking countSelect for
// every nested SELECT subquery.
func countSelectExprSubqueries(expr sql.Expr, countSelect func(*sql.SelectStmt)) {
	if expr == nil {
		return
	}
	if sub, ok := expr.(*sql.Subquery); ok {
		countSelect(sub.Select)
		return
	}
	if ex, ok := expr.(*sql.ExistsExpr); ok {
		countSelect(ex.Select)
		return
	}
	for _, kid := range raiseChildExprs(expr) {
		countSelectExprSubqueries(kid, countSelect)
	}
}

// countSelectExprSubqueriesInStmt walks a statement's expression positions
// (DML assignments, WHERE, etc.) invoking countSelect for every nested SELECT.
func countSelectExprSubqueriesInStmt(stmt sql.Stmt, countSelect func(*sql.SelectStmt)) {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		countSelect(s)
	case *sql.InsertStmt:
		if s.Select != nil {
			countSelect(s.Select)
		}
	case *sql.UpdateStmt:
		for _, a := range s.Assignments {
			countSelectExprSubqueries(a.Value, countSelect)
		}
		countSelectExprSubqueries(s.Where, countSelect)
	case *sql.DeleteStmt:
		countSelectExprSubqueries(s.Where, countSelect)
	case *sql.CreateViewStmt:
		countSelect(s.Select)
	}
}
