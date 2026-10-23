package exec

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// statementFromTermCounter accumulates FROM-term count across CTE expansions.
type statementFromTermCounter struct {
	count  int
	active map[*sql.CTEDef]bool
}

type fromTermCTEScope struct {
	parent *fromTermCTEScope
	defs   map[string]*fromTermCTEDef
}

type fromTermCTEDef struct {
	ast   *sql.CTEDef
	scope *fromTermCTEScope
}

// countSelect counts a SELECT's FROM terms, expanding referenced CTE bodies as SQLite does.
func (c *statementFromTermCounter) countSelect(s *sql.SelectStmt) {
	c.countSelectScoped(s, nil)
}

func (c *statementFromTermCounter) countSelectScoped(s *sql.SelectStmt, parent *fromTermCTEScope) {
	if s == nil || c.count >= 200 {
		return
	}
	scope := &fromTermCTEScope{parent: parent, defs: make(map[string]*fromTermCTEDef, len(s.CTEs))}
	for i := range s.CTEs {
		def := &s.CTEs[i]
		scope.defs[strings.ToLower(def.Name)] = &fromTermCTEDef{ast: def, scope: scope}
	}
	if s.From.Name != "" || s.From.Subquery != nil {
		c.count++
		c.countCTEReference(s.From, scope)
	}
	c.count += len(s.Joins)
	for i := range s.Joins {
		c.countCTEReference(s.Joins[i].Table, scope)
	}
	if s.From.Subquery != nil {
		c.countSelectScoped(s.From.Subquery, scope)
	}
	for i := range s.Joins {
		if s.Joins[i].Table.Subquery != nil {
			c.countSelectScoped(s.Joins[i].Table.Subquery, scope)
		}
	}
	countExpr := func(expr sql.Expr) {
		countSelectExprSubqueries(expr, func(child *sql.SelectStmt) {
			c.countSelectScoped(child, scope)
		})
	}
	for _, col := range s.Columns {
		countExpr(col.Expr)
	}
	countExpr(s.Where)
	for _, group := range s.GroupBy {
		countExpr(group)
	}
	countExpr(s.Having)
	for _, order := range s.OrderBy {
		countExpr(order.Expr)
	}
	if s.Union != nil {
		c.countSelectScoped(s.Union, scope)
	}
}

func (c *statementFromTermCounter) countCTEReference(ref sql.TableRef, scope *fromTermCTEScope) {
	if ref.Name == "" || ref.IsTabFunc {
		return
	}
	name := strings.ToLower(ref.Name)
	for current := scope; current != nil; current = current.parent {
		def := current.defs[name]
		if def == nil {
			continue
		}
		if c.active == nil {
			c.active = make(map[*sql.CTEDef]bool)
		}
		if c.active[def.ast] {
			return
		}
		c.active[def.ast] = true
		c.countSelectScoped(def.ast.Select, def.scope)
		delete(c.active, def.ast)
		return
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
	// One closure per walk (not per node) — no child slices on the
	// per-statement subquery-count walk.
	var rec func(sql.Expr)
	rec = func(kid sql.Expr) {
		countSelectExprSubqueriesCb(kid, countSelect, rec)
	}
	countSelectExprSubqueriesCb(expr, countSelect, rec)
}

// countSelectExprSubqueriesCb is countSelectExprSubqueries with the child
// walk injected (the recursion re-enters through next).
func countSelectExprSubqueriesCb(expr sql.Expr, countSelect func(*sql.SelectStmt), next func(sql.Expr)) {
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
	forEachRaiseChild(expr, next)
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
