package exec

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// statementFromTermCounter accumulates the FROM-term count of one statement
// (SQLite selects the read-statement limit's unit).
type statementFromTermCounter struct {
	count int
}

// countSelect counts one SELECT's FROM terms and recurses into CTEs, FROM
// and JOIN subqueries, expression subqueries, and UNION chains.
func (c *statementFromTermCounter) countSelect(s *sql.SelectStmt) {
	if s == nil {
		return
	}
	if s.From.Name != "" || s.From.Subquery != nil {
		c.count++
	}
	c.count += len(s.Joins)
	for _, cte := range s.CTEs {
		c.countSelect(cte.Select)
	}
	if s.From.Subquery != nil {
		c.countSelect(s.From.Subquery)
	}
	for i := range s.Joins {
		if s.Joins[i].Table.Subquery != nil {
			c.countSelect(s.Joins[i].Table.Subquery)
		}
	}
	for _, col := range s.Columns {
		countSelectExprSubqueries(col.Expr, c.countSelect)
	}
	countSelectExprSubqueries(s.Where, c.countSelect)
	for _, g := range s.GroupBy {
		countSelectExprSubqueries(g, c.countSelect)
	}
	countSelectExprSubqueries(s.Having, c.countSelect)
	for _, ob := range s.OrderBy {
		countSelectExprSubqueries(ob.Expr, c.countSelect)
	}
	if s.Union != nil {
		c.countSelect(s.Union)
	}
}

func countStatementFromTerms(stmt sql.Stmt) int {
	c := &statementFromTermCounter{}
	countSelectExprSubqueriesInStmt(stmt, c.countSelect)
	return c.count
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
