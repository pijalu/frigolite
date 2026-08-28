package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// occurrence (SQLite ignores later duplicates).
// mapNamedTupleValues starts with each column's DEFAULT and overrides with the
// provided values by column name. Duplicate column names use the FIRST
// occurrence (SQLite ignores later duplicates).
func (e *DMLExecutor) mapNamedTupleValues(values []interface{}, columns []string, colDefs []sql.ColumnDef) ([]interface{}, error) {
	mapped := make([]interface{}, len(colDefs))
	mappedSet := make([]bool, len(colDefs))
	// First, mark which columns are explicitly provided in the column list.
	for i, col := range columns {
		for j, cd := range colDefs {
			if strings.EqualFold(cd.Name, col) && i < len(values) && !mappedSet[j] {
				mapped[j] = values[i]
				mappedSet[j] = true
				break
			}
		}
	}
	// Only evaluate DEFAULT for columns NOT explicitly provided (SQLite
	// defers DEFAULT evaluation to INSERT time and rejects aggregates;
	// a column with an explicit value must NOT trigger DEFAULT evaluation).
	for j, cd := range colDefs {
		if !mappedSet[j] && cd.Default != nil {
			dv, err := e.evalDefaultExpr(cd.Default, cd.Name)
			if err != nil {
				return nil, err
			}
			mapped[j] = dv
		}
	}
	return mapped, nil
}

// mapPositionalTupleValues maps a no-column-list VALUES tuple onto the
// non-generated columns in order, validating the supplied count.

// mapPositionalTupleValues maps a no-column-list VALUES tuple onto the
// non-generated columns in order, validating the supplied count.

// mapPositionalTupleValues maps a no-column-list VALUES tuple onto the
// non-generated columns in order, validating the supplied count.
// mapPositionalTupleValues maps a no-column-list VALUES tuple onto the
// non-generated columns in order, validating the supplied count.
func (e *DMLExecutor) mapPositionalTupleValues(tableName string, values []interface{}, colDefs []sql.ColumnDef) ([]interface{}, error) {
	// Without a column list every table column must be supplied. Generated
	// and hidden columns are excluded from the count (SQLite computes the
	// former and does not accept positional values for the latter).
	expected := 0
	for _, cd := range colDefs {
		if cd.Generated == nil && !execquery.IsHiddenColumnDef(cd) {
			expected++
		}
	}
	if len(values) != expected {
		return nil, fmt.Errorf("table %s has %d columns but %d values were supplied",
			tableName, expected, len(values))
	}
	// Re-map the values into the full column array: without a column list,
	// the VALUES map to the non-generated, non-hidden columns in order
	// (generated columns are computed separately; hidden columns are only
	// addressable by name).
	mapped := make([]interface{}, len(colDefs))
	vi := 0
	for j, cd := range colDefs {
		if cd.Generated != nil || execquery.IsHiddenColumnDef(cd) {
			continue
		}
		if vi < len(values) {
			mapped[j] = values[vi]
			vi++
		}
	}
	return mapped, nil
}

// evalReturningStrict evaluates RETURNING expressions with strict column
// resolution: unknown columns and invalid qualifiers produce "no such column"
// errors (SQLite semantics), and table-qualified wildcards are rejected.

// evalReturningExprs evaluates RETURNING expressions against a row and
// returns a flat list of values. It handles three cases:
//   - RETURNING * : expands to all column values
//   - RETURNING expr (single) : returns the single expression value
//   - RETURNING expr, ..., * , ... : multi-expression with * expanded inline

// evalReturningStrict evaluates RETURNING expressions with strict column
// resolution: unknown columns and invalid qualifiers produce "no such column"
// errors (SQLite semantics), and table-qualified wildcards are rejected.
// evalReturningExprs evaluates RETURNING expressions against a row and
// returns a flat list of values. It handles three cases:
//   - RETURNING * : expands to all column values
//   - RETURNING expr (single) : returns the single expression value
//   - RETURNING expr, ..., * , ... : multi-expression with * expanded inline

// evalReturningSingle evaluates one RETURNING expression and unwraps its value.
// evalReturningSingle evaluates one RETURNING expression and unwraps its value.
func (e *DMLExecutor) evalReturningSingle(expr sql.Expr, row Row) ([]interface{}, error) {
	val, err := e.ctx.EvalExpr(expr, row)
	if err != nil {
		return nil, err
	}
	return []interface{}{util.UnwrapColumnValue(val)}, nil
}

// returningAllColumnValues expands RETURNING * into all column values.

// returningAllColumnValues expands RETURNING * into all column values.

// returningAllColumnValues expands RETURNING * into all column values.
// returningAllColumnValues expands RETURNING * into all column values.
func returningAllColumnValues(row Row, colDefs []sql.ColumnDef) []interface{} {
	var values []interface{}
	for _, cd := range colDefs {
		// Hidden virtual-table columns (e.g. FTS's per-table match column and
		// docid) are excluded from star expansion (SQLite: RETURNING * and
		// SELECT * both hide them; returning1 24.3: INSERT INTO fts5_table
		// VALUES('hello world') RETURNING * returns one column).
		if cd.Dropped || execquery.IsHiddenColumnDef(cd) {
			continue
		}
		if v, ok := row.Get(cd.Name); ok {
			values = append(values, util.UnwrapColumnValue(v))
		}
	}
	return values
}

// viewDeclaredColumns returns the explicit column list from a CREATE VIEW
// declaration (CREATE VIEW v(a,b) AS ...). Returns nil when the view has no
// declared column list.

// execInsertView handles INSERT statements whose target is a view. SQLite
// routes such statements through INSTEAD OF triggers; resolving the view's
// columns (which validates collations in its SELECT) happens first.

// viewColumnNames returns the output column names of a view's SELECT: the
// explicit alias when present, otherwise the column reference name or the
// expression text. A bare "*" is expanded through the FROM source (SQLite
// resolves view output columns the same way as the result columns of a plain
// SELECT). For a compound SELECT the head member determines the output names.
// validateCollationsInExpr verifies COLLATE operators in an expression tree.

// validateCollationsInWrapper handles the remaining expression types: unary
// wrappers, pairwise operators, and expression lists.
// validateCollationsInWrapper handles the remaining expression types: unary
// wrappers, pairwise operators, and expression lists.
func (e *DMLExecutor) validateCollationsInWrapper(expr sql.Expr) error {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return e.validateCollationsInExprs(v.Args)
	case *sql.RowValue:
		return e.validateCollationsInExprs(v.Values)
	case *sql.IsDistinctFrom:
		return e.validateCollationsIn(v.Left, v.Right)
	case *sql.IsNotDistinctFrom:
		return e.validateCollationsIn(v.Left, v.Right)
	case *sql.UnaryOp:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.ParenExpr:
		return e.validateCollationsInExpr(v.Expr)
	case *sql.CastExpr:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.IsNull:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.IsNotNull:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.IsTrue:
		return e.validateCollationsInExpr(v.Operand)
	case *sql.IsFalse:
		return e.validateCollationsInExpr(v.Operand)
	}
	return nil
}

// validateCollationsIn validates COLLATE operators across one or more
// expressions.

// validateCollationsIn validates COLLATE operators across one or more
// expressions.

// validateCollationsIn validates COLLATE operators across one or more
// expressions.
// validateCollationsIn validates COLLATE operators across one or more
// expressions.
func (e *DMLExecutor) validateCollationsIn(exprs ...sql.Expr) error {
	for _, x := range exprs {
		if err := e.validateCollationsInExpr(x); err != nil {
			return err
		}
	}
	return nil
}

// validateCollationsInExprs validates COLLATE operators across an expression
// slice.

// validateCollationsInExprs validates COLLATE operators across an expression
// slice.

// validateCollationsInExprs validates COLLATE operators across an expression
// slice.
// validateCollationsInExprs validates COLLATE operators across an expression
// slice.
func (e *DMLExecutor) validateCollationsInExprs(exprs []sql.Expr) error {
	for _, x := range exprs {
		if err := e.validateCollationsInExpr(x); err != nil {
			return err
		}
	}
	return nil
}

// checkCollationName verifies that a COLLATE operand names a known collation.
