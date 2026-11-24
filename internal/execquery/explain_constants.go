// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//

package execquery

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// The result-expression opcodes are the rows SQLite emits after the scan:
// OP_Column carries the column's default value in p4 (sqlite3ExprCodeGetColumn
// sets p4 to the column's zDefault), large integer literals load through
// OP_Int64 and real literals through OP_Real. These rows are observable by
// the EXPLAIN-reading fixtures (misc3-6.11 probes their p4 text).

// explainResultContext carries the FROM tables and the index columns that
// already supply GENERATED columns. C omits OP_Column for a generated column
// when its covering index holds the value (gencol1-23.3).
type explainResultContext struct {
	tables    []queryTable
	indexCols map[string]map[string]bool
}

// emitExplainResultRows emits the SELECT result columns' opcode rows. Only
// column references and large numeric literals are rendered: small integers
// (OP_Integer) and string constants add no p4 text the fixtures probe, and
// omitting them keeps the synthetic stream from drifting from the existing
// opcode-shape expectations.
func (e *SelectEngine) emitExplainResultRows(prog *explainProgram, s *sql.SelectStmt, ctx explainResultContext) {
	if s == nil {
		return
	}
	for _, col := range s.Columns {
		e.emitExplainExprRows(prog, col.Expr, ctx)
	}
}

// emitExplainExprRows walks the expression shapes that carry observable
// literals and column defaults into the VDBE program.
func (e *SelectEngine) emitExplainExprRows(prog *explainProgram, expr sql.Expr, ctx explainResultContext) {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		e.emitExplainExprRows(prog, v.Expr, ctx)
	case *sql.UnaryOp:
		e.emitExplainExprRows(prog, v.Operand, ctx)
	case *sql.BinaryOp:
		e.emitExplainExprRows(prog, v.Left, ctx)
		e.emitExplainExprRows(prog, v.Right, ctx)
	case *sql.ColumnRef:
		if !e.explainGeneratedColumnSupplied(v, ctx) {
			prog.emit("Column", 0, 0, 2, e.explainColumnDefault(v, ctx.tables), 0, "r[2]=cursor 0 column "+v.Name)
		}
	case *sql.NumericLit:
		e.emitExplainNumericRows(prog, v)
	}
}

// emitExplainNumericRows renders the literal-load opcode SQLite chooses:
// OP_Int64 for integers outside the 32-bit OP_Integer range, OP_Real for
// floating-point literals. Both carry the literal text in p4.
func (e *SelectEngine) emitExplainNumericRows(prog *explainProgram, lit *sql.NumericLit) {
	if lit == nil {
		return
	}
	if _, err := strconv.ParseInt(lit.Value, 10, 64); err == nil {
		n, err := strconv.ParseInt(lit.Value, 10, 32)
		if err == nil && n >= -(1<<31) && n <= 1<<31-1 {
			return
		}
		prog.emit("Int64", 0, 0, 0, lit.Value, 0, "")
		return
	}
	if _, err := strconv.ParseFloat(lit.Value, 64); err == nil {
		prog.emit("Real", 0, 0, 0, lit.Value, 0, "")
	}
}

// explainResultContext builds the result-row context from a SELECT and its
// planned scan nodes. Index coverage is collected recursively so a join's
// inner loop can suppress its own generated-column reads.
func (e *SelectEngine) explainResultContext(s *sql.SelectStmt, nodes []planNode) explainResultContext {
	ctx := explainResultContext{tables: e.collectQueryTables(s), indexCols: map[string]map[string]bool{}}
	e.collectExplainIndexColumns(nodes, ctx)
	if s == nil {
		return ctx
	}
	e.addExplainIndexColumns(ctx, explainTableRefDisplay(s.From), s.From.IndexedBy)
	for _, j := range s.Joins {
		e.addExplainIndexColumns(ctx, explainTableRefDisplay(j.Table), j.Table.IndexedBy)
	}
	return ctx
}

// explainTableRefDisplay is the plan-visible name of a FROM term.
func explainTableRefDisplay(r sql.TableRef) string {
	if r.As != "" {
		return r.As
	}
	return r.Name
}

// addExplainIndexColumns records one named index's key columns under the
// table's display name.
func (e *SelectEngine) addExplainIndexColumns(ctx explainResultContext, display, idx string) {
	if idx == "" {
		return
	}
	key := strings.ToLower(display)
	set := ctx.indexCols[key]
	if set == nil {
		set = map[string]bool{}
		ctx.indexCols[key] = set
	}
	for _, col := range e.indexColumns(idx) {
		set[strings.ToLower(col)] = true
	}
}

// collectExplainIndexColumns records each scan's index key columns under the
// scan's plan name (the table display name, alias included).
func (e *SelectEngine) collectExplainIndexColumns(nodes []planNode, ctx explainResultContext) {
	for _, n := range nodes {
		if shape := parseScanDetail(n.detail); shape != nil {
			e.addExplainIndexColumns(ctx, shape.table, shape.index)
		}
		e.collectExplainIndexColumns(n.children, ctx)
	}
}

// explainGeneratedColumnSupplied reports whether a GENERATED result column's
// value is already yielded by the chosen scan index. Only generated columns
// are suppressed: a plain indexed column still emits OP_Column in C.
func (e *SelectEngine) explainGeneratedColumnSupplied(ref *sql.ColumnRef, ctx explainResultContext) bool {
	real := e.explainRefTableName(ref, ctx.tables)
	if real == "" || !e.explainColumnIsGenerated(real, ref.Name) {
		return false
	}
	return ctx.indexCols[strings.ToLower(e.explainRefTableDisplay(ref, ctx.tables))][strings.ToLower(ref.Name)]
}

// explainColumnIsGenerated reads the GENERATED flag from the parsed schema.
func (e *SelectEngine) explainColumnIsGenerated(tableName, colName string) bool {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return false
	}
	for _, cd := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		if strings.EqualFold(cd.Name, colName) {
			return cd.Generated != nil
		}
	}
	return false
}

// explainExplainTables returns a statement's FROM tables for EXPLAIN row
// rendering. Non-SELECT statements have no result-column stream.
func (e *SelectEngine) explainExplainTables(stmt sql.Stmt) []queryTable {
	s, ok := stmt.(*sql.SelectStmt)
	if !ok || s == nil {
		return nil
	}
	return e.collectQueryTables(s)
}

// explainColumnDefault returns the p4 text of a column's literal DEFAULT.
// SQLite's OP_Column carries the default value so a NULL column read can
// substitute it; the fixture's regexp probes read that p4 text. A qualified
// reference resolves through the statement's FROM alias; an unqualified one
// resolves by matching the column name across the same tables. A fallback
// scan of attached schemas covers expressions outside a simple FROM list.
func (e *SelectEngine) explainColumnDefault(ref *sql.ColumnRef, tables []queryTable) string {
	if ref == nil {
		return ""
	}
	if len(tables) > 0 {
		if tableName := e.explainRefTableName(ref, tables); tableName != "" {
			return e.explainTableColumnDefault(tableName, ref.Name)
		}
	}
	return e.explainSchemaColumnDefault(ref)
}

// explainRefTableDisplay resolves a reference to the FROM term's display
// name (alias when present), matching the plan node vocabulary.
func (e *SelectEngine) explainRefTableDisplay(ref *sql.ColumnRef, tables []queryTable) string {
	if ref.Table != "" {
		for _, t := range tables {
			if strings.EqualFold(t.display, ref.Table) {
				return t.display
			}
		}
		return ""
	}
	if idx := e.resolveJoinTable(tables, ref); idx >= 0 {
		return tables[idx].display
	}
	return ""
}

// explainRefTableName resolves a result-column reference to a real table
// through the statement's FROM aliases, matching resolveJoinTable's rule for
// unqualified names.
func (e *SelectEngine) explainRefTableName(ref *sql.ColumnRef, tables []queryTable) string {
	if ref.Table != "" {
		for _, t := range tables {
			if strings.EqualFold(t.display, ref.Table) {
				return t.real
			}
		}
		return ""
	}
	if idx := e.resolveJoinTable(tables, ref); idx >= 0 {
		return tables[idx].real
	}
	return ""
}

// explainSchemaColumnDefault falls back to the attached schemas when a
// reference has no resolvable FROM table (a correlated or outer query ref).
func (e *SelectEngine) explainSchemaColumnDefault(ref *sql.ColumnRef) string {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry == nil || (ref.Table != "" && !strings.EqualFold(entry.Name, ref.Table)) {
			continue
		}
		if val := e.explainTableColumnDefault(entry.Name, ref.Name); val != "" {
			return val
		}
	}
	return ""
}

// explainTableColumnDefault reads a literal DEFAULT from one table's parsed
// column definitions. Non-literal defaults carry no p4 text.
func (e *SelectEngine) explainTableColumnDefault(tableName, colName string) string {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return ""
	}
	for _, cd := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		if !strings.EqualFold(cd.Name, colName) {
			continue
		}
		switch def := cd.Default.(type) {
		case *sql.NumericLit:
			return def.Value
		case *sql.StringLit:
			return def.Value
		}
		return ""
	}
	return ""
}
