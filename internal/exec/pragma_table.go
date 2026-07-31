// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package exec implements query execution.

package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/value"
)

// isPragmaTableFunc reports whether name refers to a table-valued pragma
// function, e.g. pragma_table_info. Schema qualifiers (temp.pragma_...) are
// stripped before the check.
func isPragmaTableFunc(name string) bool {
	lower := strings.ToLower(name)
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		lower = lower[dot+1:]
	}
	return strings.HasPrefix(lower, "pragma_")
}

// execPragmaTableValued executes a SELECT whose FROM clause is a table-valued
// pragma function. The pragma is materialized into column definitions and rows
// and the outer SELECT pipeline runs over them.
func (e *Engine) execPragmaTableValued(s *sql.SelectStmt) *Result {
	colDefs, rows, err := e.materializePragmaTable(s.From)
	if err != nil {
		return &Result{Error: err}
	}
	return e.execSelectOverMaterialized(s, colDefs, rows)
}

// materializePragmaTable converts a table-valued pragma reference into column
// definitions and rows, mirroring SQLite's pragma table-valued functions.
func (e *Engine) materializePragmaTable(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	lower := strings.ToLower(ref.Name)
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		lower = lower[dot+1:]
	}
	pragma := strings.TrimPrefix(lower, "pragma_")
	switch pragma {
	case "table_info", "table_xinfo":
		return e.materializeTableInfo(ref)
	default:
		return nil, nil, fmt.Errorf("no such table-valued pragma: %s", ref.Name)
	}
}

// materializeTableInfo builds the rows of pragma_table_info / pragma_table_xinfo
// for the table or view named by the first function argument. The result has
// columns (cid, name, type, notnull, dflt_value, pk), one row per column.
func (e *Engine) materializeTableInfo(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{
		{Name: "cid"},
		{Name: "name"},
		{Name: "type"},
		{Name: "notnull"},
		{Name: "dflt_value"},
		{Name: "pk"},
	}
	if len(ref.Args) == 0 {
		return nil, nil, fmt.Errorf("wrong number of arguments to function %s()", ref.Name)
	}
	argVal, err := e.evalExpr(ref.Args[0], nil)
	if err != nil {
		return nil, nil, err
	}
	tableName, ok := argVal.(string)
	if !ok {
		return nil, nil, fmt.Errorf("wrong type for argument of %s(): expected string", ref.Name)
	}

	var colDefs []sql.ColumnDef
	if te, _, err := e.findTable(tableName); err == nil {
		colDefs = e.parseColumnDefs(te.Name, te.SQL)
	} else if ve, _, err := e.findView(tableName); err == nil {
		colDefs, err = e.viewColumnDefs(ve)
		if err != nil {
			return nil, nil, err
		}
	} else {
		// Unknown table or view: pragma_table_info returns zero rows.
		return cols, nil, nil
	}

	rows := make([][]interface{}, 0, len(colDefs))
	for i, cd := range colDefs {
		notnull := int64(0)
		if cd.NotNull {
			notnull = 1
		}
		pk := int64(0)
		if cd.PrimaryKey {
			pk = 1
		}
		rows = append(rows, []interface{}{int64(i), cd.Name, cd.Type, notnull, nil, pk})
	}
	return cols, rows, nil
}

// viewColumnDefs derives the column names and declared types of a view from
// its stored SELECT statement, mirroring SQLite's view column typing
// (sqlite3SubqueryColumnTypes in select.c).
func (e *Engine) viewColumnDefs(viewEntry *schema.Entry) ([]sql.ColumnDef, error) {
	sqlStr := viewEntry.SQL
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " AS ")
	if idx < 0 {
		return nil, fmt.Errorf("exec: invalid view SQL: %s", sqlStr)
	}
	selectSQL := sqlStr[idx+4:]
	stmts, err := parse.ParseSQL(selectSQL)
	if err != nil || len(stmts) == 0 {
		return nil, fmt.Errorf("exec: view parse error: %v", err)
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok {
		return nil, fmt.Errorf("exec: view does not contain SELECT")
	}
	return e.viewColumnDefsFromSelect(sel), nil
}

// viewColumnDefsFromSelect computes column definitions for a view's SELECT.
// Column names come from aliases or column references; declared types follow
// SQLite's expression-based type inference.
func (e *Engine) viewColumnDefsFromSelect(sel *sql.SelectStmt) []sql.ColumnDef {
	// Resolve the FROM table once so column references can inherit its types.
	var srcDefs []sql.ColumnDef
	if sel.From.Name != "" {
		if te, _, err := e.findTable(sel.From.Name); err == nil {
			srcDefs = e.parseColumnDefs(te.Name, te.SQL)
		}
	}
	defs := make([]sql.ColumnDef, len(sel.Columns))
	for i, col := range sel.Columns {
		name := col.As
		if name == "" {
			if ref, ok := col.Expr.(*sql.ColumnRef); ok {
				name = ref.Name
			}
		}
		defs[i] = sql.ColumnDef{Name: name, Type: e.viewColumnType(sel, i, srcDefs)}
	}
	return defs
}

// viewColumnType computes the declared type string of view column i, following
// sqlite3SubqueryColumnTypes: affinity of the expression is refined across
// compound (UNION / multi-row VALUES) members, then mapped to a type name.
func (e *Engine) viewColumnType(sel *sql.SelectStmt, i int, srcDefs []sql.ColumnDef) string {
	first := sel
	aff := e.exprAffinity(first.Columns[i].Expr, srcDefs)
	pS2 := first
	m := 0
	// Walk the compound chain while the current member has no affinity,
	// remembering the datatypes of the members skipped.
	for aff == 0 && pS2.Union != nil {
		m |= exprDataType(pS2.Columns[i].Expr)
		pS2 = pS2.Union
		aff = e.exprAffinity(pS2.Columns[i].Expr, srcDefs)
	}
	if aff == 0 {
		aff = 'B' // default view affinity: SQLITE_AFF_BLOB
	}
	// Compound queries refine the affinity using the datatypes of later members.
	if isTextOrNumericAff(aff) && (pS2.Union != nil || pS2 != first) {
		for p := pS2.Union; p != nil; p = p.Union {
			m |= exprDataType(p.Columns[i].Expr)
		}
		if aff == 'T' && (m&0x01) != 0 {
			aff = 'B'
		} else if isNumericAff(aff) && (m&0x02) != 0 {
			aff = 'B'
		}
		if isNumericAff(aff) && isCastExpr(first.Columns[i].Expr) {
			aff = 'F' // FLEXNUM: CAST over a compound numeric column
		}
	}
	zType := e.exprColumnType(first.Columns[i].Expr, srcDefs)
	if zType == "" || value.Affinity(zType) != aff {
		if aff == 'N' || aff == 'F' {
			return "NUM"
		}
		for _, st := range viewStdTypes {
			if st.aff == aff {
				return st.name
			}
		}
		return ""
	}
	return zType
}

// viewStdTypes maps an affinity to SQLite's standard type names
// (sqlite3StdType[1..] in 3.53: BLOB, INT, INTEGER, REAL, TEXT). The first
// match wins, so INTEGER affinity yields "INT".
var viewStdTypes = []struct {
	aff  rune
	name string
}{
	{'B', "BLOB"},
	{'I', "INT"},
	{'I', "INTEGER"},
	{'R', "REAL"},
	{'T', "TEXT"},
}

// exprAffinity returns the affinity of a view column expression: CAST targets
// carry the affinity of their type name, column references inherit the
// affinity of the source column's declared type, and anything else has none.
func (e *Engine) exprAffinity(expr sql.Expr, srcDefs []sql.ColumnDef) rune {
	switch x := expr.(type) {
	case *sql.CastExpr:
		return value.Affinity(x.AsType)
	case *sql.ColumnRef:
		for _, cd := range srcDefs {
			if cd.Name == x.Name {
				return value.Affinity(cd.Type)
			}
		}
		return 0
	default:
		return 0
	}
}

// exprColumnType returns the declared type of a view column expression, or ""
// if the expression has none. Column references inherit the source column's
// declared type; CAST and other expressions have none (mirroring SQLite's
// columnType(), which only reports types for column and subselect expressions).
func (e *Engine) exprColumnType(expr sql.Expr, srcDefs []sql.ColumnDef) string {
	switch x := expr.(type) {
	case *sql.ColumnRef:
		for _, cd := range srcDefs {
			if cd.Name == x.Name {
				return cd.Type
			}
		}
		return ""
	default:
		return ""
	}
}

// exprDataType returns a bitmask of possible result datatypes for an
// expression, mirroring sqlite3ExprDataType: 0x01 numeric, 0x02 text, 0x04 blob.
func exprDataType(expr sql.Expr) int {
	switch x := expr.(type) {
	case *sql.NumericLit:
		return 0x01
	case *sql.StringLit:
		return 0x02
	case *sql.BlobLit:
		return 0x04
	case *sql.CastExpr:
		return exprDataType(x.Operand)
	default:
		return 0
	}
}

// isCastExpr reports whether expr is a CAST expression.
func isCastExpr(expr sql.Expr) bool {
	_, ok := expr.(*sql.CastExpr)
	return ok
}

// isTextOrNumericAff reports whether aff is TEXT or one of the numeric
// affinities (NUMERIC, INTEGER, REAL) — SQLite's "affinity >= SQLITE_AFF_TEXT".
func isTextOrNumericAff(aff rune) bool {
	return aff == 'T' || aff == 'N' || aff == 'I' || aff == 'R'
}

// isNumericAff reports whether aff is a numeric affinity
// (NUMERIC, INTEGER, REAL) — SQLite's "affinity >= SQLITE_AFF_NUMERIC".
func isNumericAff(aff rune) bool {
	return aff == 'N' || aff == 'I' || aff == 'R'
}
