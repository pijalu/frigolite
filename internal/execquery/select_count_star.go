// The ISimpleCount shortcut: answering `SELECT count(*) FROM <table>` from the
// b-tree's page headers instead of a row scan. Port of select.c:5557
// isSimpleCount + select.c:8854's OP_Count path (vdbe.c:3795 OP_Count →
// sqlite3BtreeCount).

package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// countStarShortCircuit answers the ISimpleCount shape
//
//	SELECT count(*) FROM <ordinary table>
//
// — exactly one result column, one FROM source, one aggregate and no
// WHERE/GROUP BY/HAVING/DISTINCT/compound — by counting the table b-tree's
// entries (btree.CountEntries, sqlite3BtreeCount). handled is false for every
// other shape, and for any page error, so the caller's scan stays the fallback
// (and keeps its error reporting).
func (e *SelectEngine) countStarShortCircuit(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef) (*Result, bool) {
	if !isSimpleCountShape(s, tableEntry) {
		return nil, false
	}
	tree := e.ctx.TableBTreePg(dbCtx.Pager, tableEntry.Name, tableEntry.RootPage, true)
	n, err := tree.CountEntries()
	if err != nil {
		return nil, false
	}
	columns := e.buildColumnNames(s.Columns, colDefs, s)
	return e.finalizeSelectResult(&Result{
		Columns: columns,
		Rows:    [][]interface{}{{n}},
	}, s, nil), true
}

// isSimpleCountShape mirrors the guards of select.c isSimpleCount (5557) for
// frigolite's AST: no WHERE, one result column, one non-subquery FROM source,
// no GROUP BY/HAVING, and the single output expression is a bare count(*)
// aggregate (not DISTINCT, not a window function, no FILTER/ORDER BY — the
// flags that keep sqlite off the OP_Count path, select.c:5584).
//
// The source must be an ordinary table (isSimpleCount rejects views and
// virtual tables); execRealTableSelect has already resolved the FROM to a real
// table entry, so the check is on the entry's schema type.
// ORDER BY/LIMIT and a compound chain are excluded here: they do not change
// the answer (a one-row result), but keeping them off this path avoids
// interacting with the compound merge, at the cost of falling back to the scan
// for those shapes.
func isSimpleCountShape(s *sql.SelectStmt, tableEntry *schema.Entry) bool {
	if s.Where != nil || s.Having != nil || len(s.GroupBy) > 0 || s.Distinct {
		return false
	}
	if s.Union != nil || len(s.OrderBy) > 0 || len(s.Joins) > 0 {
		return false
	}
	if len(s.Columns) != 1 || !plainFromTable(s) {
		return false
	}
	if tableEntry == nil || tableEntry.Type != schema.TypeTable {
		return false
	}
	return isBareCountStar(s.Columns[0].Expr)
}

// plainFromTable reports a single, non-subquery, non-TVF FROM source.
func plainFromTable(s *sql.SelectStmt) bool {
	return s.From.Name != "" && s.From.Subquery == nil && len(s.From.Args) == 0
}

// isBareCountStar reports whether expr is a bare count(*): no DISTINCT, no
// FILTER/ORDER BY, no OVER, exactly one "*" argument.
func isBareCountStar(expr sql.Expr) bool {
	call, ok := expr.(*sql.FuncCall)
	if !ok || !strings.EqualFold(call.Name, "count") {
		return false
	}
	if call.Distinct || call.Over != nil || call.Filter != nil || len(call.OrderBy) > 0 {
		return false
	}
	if len(call.Args) != 1 {
		return false
	}
	star, ok := call.Args[0].(*sql.ColumnRef)
	return ok && star.Name == "*"
}
