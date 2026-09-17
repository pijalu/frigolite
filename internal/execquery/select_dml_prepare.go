// Prepare-time FROM-relation resolution for subqueries (resolve.c parity):
// SQLite resolves every table reference in a statement — including the FROM
// clauses of expression subqueries — before any row is read. The engine
// historically resolved subquery FROM tables lazily at subquery execution,
// which silently ignored a missing table when the enclosing DML scan had no
// rows to evaluate the WHERE against (in3-5.2: DELETE FROM Folders WHERE
// folderid IN (SELECT ... FROM Folder) on an empty Folders must still report
// "no such table: Folder").
package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// validateFromRelations checks that every relation a SELECT's FROM clause
// names (its own term, its JOIN terms, and its set-operation chain members)
// resolves to a CTE, table or view, recursing into derived tables and into
// the expression trees for nested subqueries. Table-valued FROM functions
// (pragma/vtab modules) resolve at execution and are skipped, as is the
// quoted empty table name.
func (e *SelectEngine) validateFromRelations(s *sql.SelectStmt) error {
	for m := s; m != nil; m = m.Union {
		if err := e.validateTableRefRelations(m, m.From); err != nil {
			return err
		}
		for _, j := range m.Joins {
			if err := e.validateTableRefRelations(m, j.Table); err != nil {
				return err
			}
		}
		// CTE bodies are resolved as part of the statement too; make the
		// CTE's own name visible inside its body (recursive CTEs reference
		// themselves) while validating it.
		for i := range m.CTEs {
			e.cteScopes = append(e.cteScopes, []sql.CTEDef{m.CTEs[i]})
			err := e.validateFromRelations(m.CTEs[i].Select)
			e.cteScopes = e.cteScopes[:len(e.cteScopes)-1]
			if err != nil {
				return err
			}
		}
		if m.Union == nil {
			break
		}
	}
	return nil
}

// validateTableRefRelations validates one FROM term of the given SELECT.
func (e *SelectEngine) validateTableRefRelations(s *sql.SelectStmt, ref sql.TableRef) error {
	if ref.Subquery != nil {
		return e.validateFromRelations(ref.Subquery)
	}
	if ref.Name == "" || ref.EmptyName || ref.IsTabFunc {
		return nil
	}
	if e.relationExists(s, ref.Name) {
		return nil
	}
	// Eponymous-only virtual-table modules (dbstat, dbpage, ...) resolve
	// through the module registry, not the schema — a FROM term naming one
	// is a valid relation (dbstat.test's scalar subqueries over dbstat).
	if _, isModule := e.ctx.VTables().Find(strings.ToLower(ref.Name)); isModule {
		return nil
	}
	return fmt.Errorf("no such table: %s", ref.Name)
}

// validateLimitExpr resolves the names and functions of one LIMIT/OFFSET
// expression: column references are always "no such column" (no source row),
// unknown functions are "no such function", and builtin arity mismatches are
// "wrong number of arguments to function F()" — all prepare-time errors.
func (e *SelectEngine) validateLimitExpr(expr sql.Expr) error {
	if expr == nil {
		return nil
	}
	switch v := expr.(type) {
	case *sql.ColumnRef:
		if v.Table != "" {
			return fmt.Errorf("no such column: %s.%s", v.Table, v.Name)
		}
		return fmt.Errorf("no such column: %s", v.Name)
	case *sql.ParenExpr:
		return e.validateLimitExpr(v.Expr)
	case *sql.UnaryOp:
		return e.validateLimitExpr(v.Operand)
	case *sql.BinaryOp:
		if err := e.validateLimitExpr(v.Left); err != nil {
			return err
		}
		return e.validateLimitExpr(v.Right)
	case *sql.FuncCall:
		fn, ok := e.ctx.Functions().Find(v.Name)
		if !ok {
			return fmt.Errorf("no such function: %s", v.Name)
		}
		n := len(v.Args)
		if n < fn.MinArgs || (fn.MaxArgs > 0 && n > fn.MaxArgs) {
			return fmt.Errorf("wrong number of arguments to function %s()", v.Name)
		}
		for _, a := range v.Args {
			if err := e.validateLimitExpr(a); err != nil {
				return err
			}
		}
		return nil
	case *sql.CastExpr:
		return e.validateLimitExpr(v.Operand)
	}
	return nil
}
