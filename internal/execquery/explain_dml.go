package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// dmlSearchDetail renders the SEARCH node for a DELETE/UPDATE point lookup:
// "SEARCH <t> USING INTEGER PRIMARY KEY (rowid=?)" for a rowid equality, or
// "SEARCH <t> USING INDEX <idx> (<col>=?)" for an indexed leading-column
// equality. Returns "" when no seek applies (caller keeps SCAN). The gate
// mirrors the DML seek path (internal/execdml planDMLSeek); the plan label is
// advisory, so a divergence only affects the EQP text, never the rows
// touched.
func (e *SelectEngine) dmlSearchDetail(tableName string, where sql.Expr) string {
	if where == nil || tableName == "" {
		return ""
	}
	tableEntry, _, err := e.ctx.FindTable(tableName)
	if err != nil || tableEntry == nil {
		return ""
	}
	if e.ctx.HasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return ""
	}
	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	if RowHasRowIDColumn(colDefs) {
		return ""
	}
	isRowidTable := !e.ctx.HasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	for _, conj := range splitAnd(where) {
		bin, ok := conj.(*sql.BinaryOp)
		if !ok || bin.Operator != "=" {
			continue
		}
		if detail := e.dmlEqualitySeekDetail(bin, tableName, colDefs, isRowidTable); detail != "" {
			return detail
		}
	}
	return ""
}

// dmlEqualitySeekDetail renders the SEARCH plan detail for one equality
// conjunct, probing both operand orders. Rowid equality (rowid/_rowid_/oid;
// a declared column with such a name shadows the pseudo-column, checked by
// the caller) maps to INTEGER PRIMARY KEY; otherwise the seek narrows on an
// index whose LEADING key is the constrained column (mirrors the execdml
// seek gate; a non-leading key cannot drive the probe).
func (e *SelectEngine) dmlEqualitySeekDetail(bin *sql.BinaryOp, tableName string, colDefs []sql.ColumnDef, isRowidTable bool) string {
	for _, sides := range [2][2]sql.Expr{{bin.Left, bin.Right}, {bin.Right, bin.Left}} {
		if detail := e.dmlSearchSideDetail(sides[0], sides[1], tableName, colDefs, isRowidTable); detail != "" {
			return detail
		}
	}
	return ""
}

// dmlSearchSideDetail checks one equality side (column = literal).
func (e *SelectEngine) dmlSearchSideDetail(colExpr, litExpr sql.Expr, tableName string, colDefs []sql.ColumnDef, isRowidTable bool) string {
	ref, ok := colExpr.(*sql.ColumnRef)
	if !ok {
		return ""
	}
	if litExpr == nil || !isDMLSearchLiteral(litExpr) {
		return ""
	}
	name := strings.ToLower(ref.Name)
	qualified := ref.Table == "" || strings.EqualFold(ref.Table, tableName)
	if isRowidTable && qualified && (name == "rowid" || name == "_rowid_" || name == "oid") {
		return fmt.Sprintf("SEARCH %s USING INTEGER PRIMARY KEY (rowid=?)", tableName)
	}
	if !colDefsHasColumn(colDefs, ref.Name) || !qualified {
		return ""
	}
	return e.dmlColumnSeekDetail(ref.Name, tableName, colDefs, isRowidTable)
}

// dmlColumnSeekDetail renders the SEARCH detail for a constrained declared
// column: INTEGER PRIMARY KEY rowid alias, else a leading-column index.
func (e *SelectEngine) dmlColumnSeekDetail(colName, tableName string, colDefs []sql.ColumnDef, isRowidTable bool) string {
	if cd, ok := findColDefByName(colDefs, colName); ok && isIPKRowidAliasCol(cd) && isRowidTable {
		return fmt.Sprintf("SEARCH %s USING INTEGER PRIMARY KEY (rowid=?)", tableName)
	}
	if idx := e.findIndexOnLeadingColumn(tableName, colName); idx != "" {
		return fmt.Sprintf("SEARCH %s USING INDEX %s (%s=?)", tableName, idx, colName)
	}
	return ""
}

// isDMLSearchLiteral reports whether an expression is a plain literal (or a
// parenthesized/unary literal): the shapes the DML seek path pins on.
func isDMLSearchLiteral(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.NumericLit, *sql.StringLit, *sql.NullLit, *sql.BlobLit:
		return true
	case *sql.ParenExpr:
		return isDMLSearchLiteral(v.Expr)
	case *sql.UnaryOp:
		if _, ok := v.Operand.(*sql.NumericLit); ok {
			return true
		}
	}
	return false
}

// colDefsHasColumn reports whether colName resolves to a declared column.
func colDefsHasColumn(colDefs []sql.ColumnDef, colName string) bool {
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, colName) {
			return true
		}
	}
	return false
}

// rowidSeekPlanDetail renders the "SEARCH <t> USING INTEGER PRIMARY KEY (...)"
// node for a WHERE that pins the rowid by equality or by literal range
// (intpkey-1.12.2 "WHERE a==4"; where.c's integer-key range normalization).
// realName is the FROM table (lookups), display the rendered name (the alias
// when one was written), alias the FROM alias for reference qualification.
// ok reports whether a seek applies; eq reports the at-most-one-row equality
// form. The node shares analyzeRowidSeek with the seek executor, so the plan
// text and the executed plan cannot diverge.
func (e *SelectEngine) rowidSeekPlanDetail(t queryTable, s *sql.SelectStmt) (detail string, eq, ok bool) {
	if s.Where == nil || t.real == "" || IsSchemaTable(t.real) {
		return "", false, false
	}
	colDefs, rowidTable := e.ipkSeekColDefs(t.real)
	if !rowidTable {
		return "", false, false
	}
	a := analyzeRowidSeek(s.Where, t.real, s.From.As, colDefs)
	if a == nil || !a.planned {
		return "", false, false
	}
	return fmt.Sprintf("SEARCH %s USING INTEGER PRIMARY KEY %s", t.display, rowidSeekConstraints(a)), a.eq, true
}

// ipkSeekColDefs loads a rowid table's column definitions for the IPK seek
// check. ok is false for missing tables and WITHOUT ROWID tables.
func (e *SelectEngine) ipkSeekColDefs(tableName string) ([]sql.ColumnDef, bool) {
	tableEntry, _, err := e.ctx.FindTable(tableName)
	if err != nil || tableEntry == nil {
		return nil, false
	}
	if e.ctx.HasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return nil, false
	}
	return e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL), true
}

// isParameterExpr reports whether expr is a bound-parameter placeholder.
func isParameterExpr(expr sql.Expr) bool {
	_, ok := expr.(*sql.ParameterExpr)
	return ok
}
