// DML prepare-time name and function resolution (build.c/resolve.c parity):
// SQLite resolves every column reference and function name in an
// INSERT/UPDATE/DELETE statement at prepare time, long before any row is
// touched. The engine historically deferred these to row evaluation, which
// silently skipped WHERE terms that resolve to NULL (update/delete_pkg/
// triggerB/misc4/misc5 class).
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
)

// buildDMLColumnLookup returns a case-insensitive lookup of the target
// table's columns, plus the rowid pseudo-aliases when the table is a rowid
// table (a WITHOUT ROWID table has no rowid/_rowid_/oid).
func buildDMLColumnLookup(colDefs []sql.ColumnDef, hasRowid bool) map[string]bool {
	lookup := make(map[string]bool, len(colDefs)+3)
	for _, cd := range colDefs {
		lookup[strings.ToLower(cd.Name)] = true
	}
	if hasRowid {
		lookup["rowid"] = true
		lookup["_rowid_"] = true
		lookup["oid"] = true
	}
	return lookup
}

// validateDMLExprs resolves every bare column reference and function name in
// the given expressions against the target table, mirroring resolve.c:
//   - an unknown column errors "no such column: NAME";
//   - an unknown function errors "no such function: NAME";
//   - a scalar aggregate is a misuse outside SELECT-list/HAVING context:
//     "misuse of aggregate: NAME()".
//
// qualifiers lists the valid table qualifiers (the table name plus a
// statement alias when present). WalkExprFull treats Subquery/ExistsExpr as
// leaves, so subquery bodies are not descended into — they resolve against
// their own scope.
func (e *DMLExecutor) validateDMLExprs(qualifiers []string, colDefs []sql.ColumnDef, hasRowid bool, exprs []sql.Expr) *Result {
	lookup := buildDMLColumnLookup(colDefs, hasRowid)
	validQual := func(q string) bool {
		// Trigger-body NEW./OLD. row pseudo-aliases are always valid
		// qualifiers (the T18 alias-masking rule only rejects the ORIGINAL
		// table name).
		if strings.EqualFold(q, "new") || strings.EqualFold(q, "old") {
			return true
		}
		for _, v := range qualifiers {
			if strings.EqualFold(q, v) {
				return true
			}
		}
		return false
	}
	for _, ex := range exprs {
		if ex == nil {
			continue
		}
		var err error
		execquery.WalkExprFull(ex, func(n sql.Expr) {
			if err != nil {
				return
			}
			switch v := n.(type) {
			case *sql.Subquery, *sql.ExistsExpr:
				return
			case *sql.ColumnRef:
				if v.Table != "" {
					// NEW./OLD. pseudo-rows (trigger bodies) resolve their
					// columns against the fired row — which may carry columns
					// the target table lacks (a view's column list), so the
					// bare-name lookup does not apply to them.
					if strings.EqualFold(v.Table, "new") || strings.EqualFold(v.Table, "old") {
						return
					}
					if !validQual(v.Table) {
						err = fmt.Errorf("no such column: %s.%s", v.Table, v.Name)
						return
					}
					if !lookup[strings.ToLower(v.Name)] {
						err = fmt.Errorf("no such column: %s.%s", v.Table, v.Name)
					}
					return
				}
				if !lookup[strings.ToLower(v.Name)] {
					err = fmt.Errorf("no such column: %s", v.Name)
				}
			case *sql.FuncCall:
				isAgg, exists := e.ctx.LookupFunction(v.Name)
				if !exists {
					err = fmt.Errorf("no such function: %s", v.Name)
					return
				}
				if isAgg {
					err = fmt.Errorf("misuse of aggregate: %s()", strings.ToLower(v.Name))
				}
			}
		})
		if err != nil {
			return &Result{Error: err}
		}
	}
	// resolve.c also prepares each expression's collation: comparison
	// operands resolve the compared columns' declared collations and COLLATE
	// operators name registered sequences, else "no such collation
	// sequence: NAME" fires at prepare time (collate3-2.3/3.12 analogs in
	// DML WHERE clauses).
	if err := e.validateDMLComparisonCollations(colDefs, exprs); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// validateInsertColumnList checks a named INSERT column list against the
// target table (build.c sqlite3AddColumnToList): an unknown name errors
// "table %s has no column named %s".
func validateInsertColumnList(tableName string, columns []string, colDefs []sql.ColumnDef) *Result {
	lookup := buildDMLColumnLookup(colDefs, true)
	for _, col := range columns {
		if !lookup[strings.ToLower(col)] {
			return &Result{Error: fmt.Errorf("table %s has no column named %s", tableName, col)}
		}
	}
	return nil
}
