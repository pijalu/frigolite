package execdml

import (
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// DML-side collation resolution (build.c parity).
//
// SQLite builds the collation KeyInfo of every index a DML statement
// maintains at prepare time (sqlite3LocateCollSeq), and resolves the
// collation of WHERE-comparison operands in the same pass. After a
// close/reopen that did not re-register a schema-declared collation, those
// statements fail with "no such collation sequence: NAME":
//   - INSERT into a table with an index on the collation fails
//     (collate3-3.1);
//   - UPDATE maintains an index only when it assigns one of the index's key
//     columns (or an expression/predicate column of the index), so
//     "SET c1 = ..." fails while "SET c2 = ..." succeeds (collate3-3.2/3.3);
//   - DELETE with a WHERE clause is row-by-row and maintains every index
//     (fails), while a WHERE-less DELETE truncates and needs no collation
//     (collate3-3.4/3.6);
//   - WHERE comparisons against a column with an unregistered declared
//     collation fail regardless of indexes (update.c/resolve.c prepare-time
//     expression collation resolution).

// validateIndexCollations verifies the key collations of every index this
// statement maintains. changed lists the assigned column names for UPDATE
// (nil = every index is maintained: INSERT, DELETE ... WHERE). Returns the
// first resolution error, "no such collation sequence: NAME".
func (e *DMLExecutor) validateIndexCollations(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changed map[string]bool) *Result {
	for _, def := range e.allTableIndexes(tableEntry.Name) {
		if changed != nil && !indexTouchesChangedCols(def, colDefs, changed) {
			continue
		}
		for _, name := range IndexKeyCollations(def.SQL, colDefs) {
			if err := e.checkCollationString(name); err != nil {
				return &Result{Error: err}
			}
		}
	}
	return nil
}

// indexTouchesChangedCols reports whether an index must be maintained when
// the given columns are assigned: a plain column key matches directly, an
// expression key or partial-index predicate matches when it references any
// changed column (SQLite update.c maintains an index only in that case).
func indexTouchesChangedCols(def indexDef, colDefs []sql.ColumnDef, changed map[string]bool) bool {
	for _, key := range def.Cols {
		if cd := colDefAt(colDefs, key); cd != nil {
			if changed[strings.ToLower(cd.Name)] {
				return true
			}
			continue
		}
		if exprTouchesChangedCols(key, colDefs, changed) {
			return true
		}
	}
	if def.Where != "" && exprTouchesChangedCols(def.Where, colDefs, changed) {
		return true
	}
	return false
}

// exprTouchesChangedCols reports whether the expression text references any
// changed column. An expression that fails to parse counts as touching
// (conservative: the index is maintained).
func exprTouchesChangedCols(exprSQL string, colDefs []sql.ColumnDef, changed map[string]bool) bool {
	expr := parseWhereExpr(exprSQL)
	if expr == nil {
		return true
	}
	found := false
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if ref, ok := n.(*sql.ColumnRef); ok && changed[strings.ToLower(ref.Name)] {
			found = true
		}
	})
	return found
}

// IndexKeyCollations parses a CREATE INDEX statement's key list and returns
// the collation name each key resolves to: an explicit COLLATE in the key
// wins, then the key column's declared collation; an expression key
// propagates like SQLite's expr.c sqlite3ExprCollSeq (COLLATE operator,
// first collation-bearing function argument, CASE branches, ||). "" means
// BINARY. Exported for the engine's integrity check, which resolves the same
// collations when it opens every index.
func IndexKeyCollations(indexSQL string, colDefs []sql.ColumnDef) []string {
	colText := indexColumnListText(indexSQL)
	if colText == "" {
		return nil
	}
	explicit := parseIndexKeyCollations(colText)
	keys := parseIndexKeyCols(colText)
	names := make([]string, 0, len(keys))
	for i, key := range keys {
		if i < len(explicit) && explicit[i] != "" {
			names = append(names, explicit[i])
			continue
		}
		if cd := colDefAt(colDefs, key); cd != nil {
			names = append(names, cd.Collate)
			continue
		}
		names = append(names, exprCollationName(parseWhereExpr(key), colDefs))
	}
	return names
}

// exprCollationName propagates the compile-time collation of an expression
// through its operands, resolving column references' declared collations.
// Returns "" for BINARY/undefined.
func exprCollationName(expr sql.Expr, colDefs []sql.ColumnDef) string {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return binaryExprCollationName(v, colDefs)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if c := exprCollationName(arg, colDefs); c != "" {
				return c
			}
		}
		return ""
	case *sql.CaseExpr:
		return caseExprCollationName(v, colDefs)
	case *sql.UnaryOp:
		return exprCollationName(v.Operand, colDefs)
	case *sql.ColumnRef:
		if cd := colDefAt(colDefs, v.Name); cd != nil {
			return cd.Collate
		}
		return ""
	}
	return ""
}

// binaryExprCollationName resolves a binary operator's collation: COLLATE
// yields its named collation; "||" takes the right operand's collation,
// else the left's; other operators have none.
func binaryExprCollationName(v *sql.BinaryOp, colDefs []sql.ColumnDef) string {
	if strings.EqualFold(v.Operator, "COLLATE") {
		if lit, ok := v.Right.(*sql.StringLit); ok {
			return lit.Value
		}
		return ""
	}
	if strings.EqualFold(v.Operator, "||") {
		if rc := exprCollationName(v.Right, colDefs); rc != "" {
			return rc
		}
		return exprCollationName(v.Left, colDefs)
	}
	return ""
}

// caseExprCollationName resolves a CASE expression's collation: the first
// THEN branch with a collation, else the ELSE branch's.
func caseExprCollationName(v *sql.CaseExpr, colDefs []sql.ColumnDef) string {
	for _, w := range v.Whens {
		if c := exprCollationName(w.Then, colDefs); c != "" {
			return c
		}
	}
	return exprCollationName(v.Else, colDefs)
}

// validateDMLComparisonCollations resolves the collation of comparison
// operands and explicit COLLATE operators inside DML expressions against the
// target table's declared column collations (resolve.c prepares WHERE/SET
// expressions with the same collation resolution a SELECT uses).
func (e *DMLExecutor) validateDMLComparisonCollations(colDefs []sql.ColumnDef, exprs []sql.Expr) error {
	for _, ex := range exprs {
		if ex == nil {
			continue
		}
		if err := e.validateExprComparisonCollations(ex, colDefs); err != nil {
			return err
		}
	}
	return nil
}

// validateExprComparisonCollations checks one expression tree.
func (e *DMLExecutor) validateExprComparisonCollations(expr sql.Expr, colDefs []sql.ColumnDef) error {
	var err error
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if err != nil {
			return
		}
		err = e.comparisonCollationError(n, colDefs)
	})
	return err
}

// comparisonCollationError returns the resolution error for one expression
// node: a COLLATE operator's name, or a comparison operand's declared column
// collation.
func (e *DMLExecutor) comparisonCollationError(n sql.Expr, colDefs []sql.ColumnDef) error {
	bop, ok := n.(*sql.BinaryOp)
	if !ok {
		return nil
	}
	if strings.EqualFold(bop.Operator, "COLLATE") {
		if lit, ok := bop.Right.(*sql.StringLit); ok {
			return e.checkCollationString(lit.Value)
		}
		return nil
	}
	if !isDMLComparisonOp(bop.Operator) {
		return nil
	}
	return e.checkComparisonSideCollations(bop, colDefs)
}

// checkComparisonSideCollations verifies both operands' declared collations.
func (e *DMLExecutor) checkComparisonSideCollations(bop *sql.BinaryOp, colDefs []sql.ColumnDef) error {
	for _, side := range []sql.Expr{bop.Left, bop.Right} {
		ref, ok := side.(*sql.ColumnRef)
		if !ok {
			continue
		}
		if cd := colDefAt(colDefs, ref.Name); cd != nil && cd.Collate != "" {
			if err := e.checkCollationString(cd.Collate); err != nil {
				return err
			}
		}
	}
	return nil
}

// isDMLComparisonOp reports whether op is a comparison whose operand
// collations resolve at prepare time.
func isDMLComparisonOp(op string) bool {
	switch strings.ToUpper(op) {
	case "=", "<>", "!=", "<", ">", "<=", ">=":
		return true
	}
	return false
}
