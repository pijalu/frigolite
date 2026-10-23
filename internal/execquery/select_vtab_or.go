package execquery

import "github.com/pijalu/frigolite/internal/sql"

// execCreatedVTabOrBranches executes one created-vtab materialization per
// top-level OR branch. SQLite's where.c emits branch rows in OR term order and
// suppresses rowids already emitted by earlier branches.
func (e *SelectEngine) execCreatedVTabOrBranches(stmt *sql.SelectStmt, tableName string, colDefs []sql.ColumnDef) (*Result, bool) {
	branches, ok := createdVTabOrBranches(stmt.Where)
	if !ok || !e.ctx.MayScanCreatedVTab(tableName) {
		return nil, false
	}
	var defs []sql.ColumnDef
	var rows [][]interface{}
	var rowids []int64
	seen := make(map[int64]struct{})
	for _, where := range branches {
		branchStmt := *stmt
		branchStmt.Where = where
		opts := e.vtabScanOptions(&branchStmt)
		// LIMIT/OFFSET apply after the OR union, never separately per branch.
		opts.Limit, opts.Offset, opts.MaxRows = nil, nil, -1
		residual := opts.Where
		opts.Residual = &residual
		branchDefs, branchRows, branchIDs, err, handled := e.ctx.MaterializeCreatedVTab(tableName, opts)
		if !handled {
			return nil, false
		}
		if err != nil {
			return &Result{Error: err}, true
		}
		if len(branchRows) != len(branchIDs) {
			return nil, false
		}
		if defs == nil {
			defs = branchDefs
		}
		branchMaps := buildMaterializedRowMaps(&branchStmt, branchDefs, branchRows, branchIDs)
		for i, id := range branchIDs {
			passes, filterErr := e.rowPassesWhere(where, branchMaps[i], nil)
			if filterErr != nil {
				return &Result{Error: filterErr}, true
			}
			if !passes {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			rows = append(rows, branchRows[i])
			rowids = append(rowids, id)
		}
	}
	if defs == nil {
		defs = colDefs
	}
	return e.execSelectOverMaterializedRowids(stmt, defs, rows, rowids), true
}

// createdVTabOrBranches expands exactly one OR-bearing top-level AND term,
// preserving sibling AND constraints in each branch. Multiple independent OR
// terms are left to the ordinary full scan until their Cartesian expansion is
// implemented.
func createdVTabOrBranches(where sql.Expr) ([]sql.Expr, bool) {
	conjuncts := splitCreatedVTabAnd(where)
	orIndex := -1
	for i, term := range conjuncts {
		if hasRootOr(term) {
			if orIndex >= 0 {
				return nil, false
			}
			orIndex = i
		}
	}
	if orIndex < 0 {
		return nil, false
	}
	terms := flattenCreatedVTabOr(conjuncts[orIndex])
	if len(terms) < 2 || len(terms) > 32 {
		return nil, false
	}
	branches := make([]sql.Expr, len(terms))
	for i, term := range terms {
		branchTerms := append([]sql.Expr(nil), conjuncts...)
		branchTerms[orIndex] = term
		branches[i] = joinCreatedVTabAnd(branchTerms)
	}
	return branches, true
}

func splitCreatedVTabAnd(expr sql.Expr) []sql.Expr {
	bin, ok := expr.(*sql.BinaryOp)
	if !ok || bin.Operator != "AND" {
		return []sql.Expr{expr}
	}
	return append(splitCreatedVTabAnd(bin.Left), splitCreatedVTabAnd(bin.Right)...)
}

func hasRootOr(expr sql.Expr) bool {
	bin, ok := expr.(*sql.BinaryOp)
	return ok && bin.Operator == "OR"
}

func flattenCreatedVTabOr(expr sql.Expr) []sql.Expr {
	bin, ok := expr.(*sql.BinaryOp)
	if !ok || bin.Operator != "OR" {
		return []sql.Expr{expr}
	}
	return append(flattenCreatedVTabOr(bin.Left), flattenCreatedVTabOr(bin.Right)...)
}

func joinCreatedVTabAnd(terms []sql.Expr) sql.Expr {
	var result sql.Expr
	for _, term := range terms {
		if result == nil {
			result = term
		} else {
			result = &sql.BinaryOp{Left: result, Right: term, Operator: "AND"}
		}
	}
	return result
}
