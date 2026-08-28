// Package exec implements query execution.
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

// validateViewFromClause validates a view referenced in the FROM clause of an
// EXPLAIN QUERY PLAN statement. SQLite prepares the view definition, so
// compile errors in the body surface under EQP. Returns nil when the FROM is
// not a view or validation passes.
func (e *SelectEngine) validateViewFromClause(s *sql.SelectStmt) error {
	if s.From.Name == "" {
		return nil
	}
	if _, _, err := e.ctx.FindTable(s.From.Name); err == nil {
		return nil // real table, not a view
	}
	viewEntry, _, viewErr := e.ctx.FindView(s.From.Name)
	if viewErr != nil {
		return nil // not a view either; let normal planning handle it
	}
	return e.validateViewBody(viewEntry)
}

// planViewCompound expands a compound-SELECT view (UNION/UNION ALL/etc.) in
// the FROM clause, pushing the outer WHERE and ORDER BY into each branch.
// SQLite pushes predicates through UNION ALL so each branch can use its index
// (SEARCH instead of SCAN). Returns nil when the FROM is not a compound view
// (caller falls through to the normal single-table/join plan).
func (e *SelectEngine) planViewCompound(s *sql.SelectStmt) *Result {
	if s.From.Name == "" || len(s.Joins) > 0 {
		return nil
	}
	// Only when it is a view, not a real table.
	if _, _, err := e.ctx.FindTable(s.From.Name); err == nil {
		return nil
	}
	viewEntry, _, viewErr := e.ctx.FindView(s.From.Name)
	if viewErr != nil {
		return nil
	}
	bodySel := parseViewSelectBody(viewEntry.SQL)
	if bodySel == nil || bodySel.Union == nil {
		return nil // not a compound view
	}
	// Predicate pushdown: AND the outer WHERE into each branch and copy the
	// outer ORDER BY so each branch's plan reflects index usage.
	if s.Where != nil || len(s.OrderBy) > 0 {
		pushPredicatesIntoBranches(bodySel, s.Where, s.OrderBy)
	}
	return planTreeResult(e.planCompound(bodySel))
}

// pushPredicatesIntoBranches walks the compound SELECT chain and ANDs the
// outer WHERE into each branch, also setting each branch's ORDER BY. The
// parsed body is a fresh copy so mutation is safe.
func pushPredicatesIntoBranches(body *sql.SelectStmt, outerWhere sql.Expr, outerOrderBy []sql.OrderByTerm) {
	cur := body
	for cur != nil {
		if outerWhere != nil {
			if cur.Where != nil {
				cur.Where = &sql.BinaryOp{Left: cur.Where, Right: outerWhere, Operator: "AND"}
			} else {
				cur.Where = outerWhere
			}
		}
		if len(outerOrderBy) > 0 {
			cur.OrderBy = outerOrderBy
		}
		cur = cur.Union
	}
}

// parseViewSelectBody extracts and parses the SELECT body from a CREATE VIEW
// statement, returning the parsed SelectStmt (or nil on failure).
func parseViewSelectBody(viewSQL string) *sql.SelectStmt {
	upper := strings.ToUpper(viewSQL)
	idx := strings.Index(upper, " AS")
	if idx < 0 {
		return nil
	}
	selectSQL := strings.TrimSpace(viewSQL[idx+3:])
	stmts, err := parse.ParseSQL(selectSQL)
	if err != nil || len(stmts) == 0 {
		return nil
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok {
		return nil
	}
	return sel
}
