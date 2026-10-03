package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
)

// The statement expression census: ONE walk per SELECT statement collects the
// node kinds the prepare-time validators key on, so the dozen-plus clause
// validators (select_validate_exprs.go and its split files) can skip their
// own walks when the kinds they police are provably absent — the plain
// point-SELECT shape (bare column projection, literal WHERE) pays a single
// pass instead of the full chain.
//
// Soundness contract: a validator may skip only when the census proves no
// node kind any of its error paths requires. WalkExprFull does not descend
// into subquery bodies, so any validator whose error paths inspect inside a
// subquery stays enabled while the census saw a subquery at this level (the
// subquery's own execution validates its body against its own scope).

// selectExprCensus carries the per-statement node-kind flags.
type selectExprCensus struct {
	funcCall  bool // any *sql.FuncCall
	aggFunc   bool // aggregate-registry FuncCall (no OVER)
	distinct  bool // FuncCall with DISTINCT
	filter    bool // FuncCall with a FILTER clause
	over      bool // FuncCall with OVER (window function)
	subquery  bool // Subquery / ExistsExpr node at this level
	rowValue  bool // RowValue node
	matchOp   bool // MATCH / NOT MATCH operator (FTS constraint)
	collateOp bool // explicit COLLATE operator
}

// visitExprs walks one expression with the census collector.
func (c *selectExprCensus) visitExprs(e *SelectEngine, expr sql.Expr) {
	if expr == nil {
		return
	}
	WalkExprFull(expr, func(n sql.Expr) {
		switch v := n.(type) {
		case *sql.FuncCall:
			c.funcCall = true
			if v.Distinct {
				c.distinct = true
			}
			if v.Filter != nil {
				c.filter = true
			}
			if v.Over != nil {
				c.over = true
			} else if !c.aggFunc {
				if reg, ok := e.ctx.Functions().Find(v.Name); ok && reg.Type == function.TypeAggregate {
					c.aggFunc = true
				}
			}
		case *sql.Subquery, *sql.ExistsExpr:
			c.subquery = true
		case *sql.RowValue:
			c.rowValue = true
		case *sql.BinaryOp:
			if v.Operator == "MATCH" || v.Operator == "NOT MATCH" {
				c.matchOp = true
			} else if strings.EqualFold(v.Operator, "COLLATE") {
				c.collateOp = true
			}
		}
	})
}

// censusSelectStmt collects the census over every expression clause of one
// SELECT level (compound members and FROM subqueries run their own statement
// execution, and with it their own census).
func censusSelectStmt(e *SelectEngine, s *sql.SelectStmt) selectExprCensus {
	var c selectExprCensus
	for i := range s.Columns {
		c.visitExprs(e, s.Columns[i].Expr)
	}
	c.visitExprs(e, s.Where)
	for _, g := range s.GroupBy {
		c.visitExprs(e, g)
	}
	c.visitExprs(e, s.Having)
	for i := range s.OrderBy {
		c.visitExprs(e, s.OrderBy[i].Expr)
	}
	c.visitExprs(e, s.Limit)
	c.visitExprs(e, s.Offset)
	for i := range s.Joins {
		c.visitExprs(e, s.Joins[i].On)
	}
	return c
}
