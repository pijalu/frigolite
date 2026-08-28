package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// tvfScopeItem is one flattened FROM operand visible to TVF argument
// expressions.
type tvfScopeItem struct {
	name  string // alias when present, else relation name (lower-case)
	outer bool   // operand is joined via RIGHT or FULL (an OUTER join)
	tvf   bool   // operand carries table-function arguments
	ref   sql.TableRef
}

// validateTVFArgScope enforces SQLite's name-visibility rules for
// table-valued function arguments (resolve.c name resolution plus the
// select.c sqlite3SelectCheckOnClauses bFuncArg walk):
//
//   - an argument expression may only reference FROM items of its own
//     SELECT scope. A parenthesized JOIN group parses as a nested-from
//     subquery (parse.y "LP seltablist RP"), so it shields outer names:
//     "FROM t2 JOIN (t1 RIGHT JOIN generate_series(t2.y,5))" must report
//     "no such column: t2.y" (tabfunc01-1420).
//
//   - when the function sits on the RHS of a RIGHT or FULL join, its
//     arguments must not reference tables to its right: "table-function
//     argument references tables to its right" (tabfunc01-1410,
//     carray01-201).
//
// Unqualified argument columns are not checked here; they resolve against
// the row context at evaluation time.
func (e *SelectEngine) validateTVFArgScope(s *sql.SelectStmt) error {
	items := make([]tvfScopeItem, 0, len(s.Joins)+1)
	add := func(ref sql.TableRef, outer bool) {
		n := ref.Name
		if ref.As != "" {
			n = ref.As
		}
		items = append(items, tvfScopeItem{
			name:  strings.ToLower(n),
			outer: outer,
			tvf:   len(ref.Args) > 0 && ref.Subquery == nil,
			ref:   ref,
		})
	}
	add(s.From, false)
	for _, j := range s.Joins {
		outer := joinTypeHas(j.JoinType, "RIGHT") || joinTypeHas(j.JoinType, "FULL")
		add(j.Table, outer)
	}
	for i, it := range items {
		if !it.tvf {
			continue
		}
		for _, arg := range it.ref.Args {
			err := checkTVFArgTables(arg, items, i, it.outer, e.derivedScope)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// checkTVFArgTables walks one argument expression and validates every
// qualified column reference against the ordered FROM scope. itemIdx is the
// position of the table function itself; nonLateral enables the strict
// out-of-scope rejection for derived-table bodies.
func checkTVFArgTables(arg sql.Expr, items []tvfScopeItem, itemIdx int, outer, nonLateral bool) error {
	var firstErr error
	WalkExprFull(arg, func(ex sql.Expr) {
		if firstErr != nil {
			return
		}
		cr, ok := ex.(*sql.ColumnRef)
		if !ok || cr.Table == "" {
			return
		}
		t := strings.ToLower(cr.Table)
		if dot := strings.LastIndex(t, "."); dot >= 0 {
			t = t[dot+1:] // strip a schema qualifier (main.t2 -> t2)
		}
		for j, cand := range items {
			if cand.name != t {
				continue
			}
			if j > itemIdx && outer {
				firstErr = fmt.Errorf("table-function argument references tables to its right")
			}
			return
		}
		// Outside this SELECT scope: a parenthesized JOIN group is a
		// non-lateral subquery whose arguments never see past its own FROM
		// clause (tabfunc01-1420); correlated subqueries keep outer visibility,
		// so their unresolved references are left to evaluation time.
		if nonLateral {
			firstErr = fmt.Errorf("no such column: %s.%s", cr.Table, cr.Name)
		}
	})
	return firstErr
}
