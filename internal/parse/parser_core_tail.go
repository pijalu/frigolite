// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar. This replaces the hand-written
// recursive-descent parser in internal/sql/parser.go.

package parse

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

func getFrameBound(v interface{}) *sql.FrameBound {
	if v == nil {
		return nil
	}
	if fb, ok := v.(*sql.FrameBound); ok {
		return fb
	}
	return nil
}

// getFrameOptSpec extracts the frame spec text from a frame_opt value, which
// is either a plain string (empty frame_opt, rule 325) or a *sql.WindowFrame
// (rules 326/327). The spec text preserves the original SQL for view
// serialization round-trips.
func getFrameOptSpec(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if f := getWindowFrame(v); f != nil {
		return frameFromStruct(f)
	}
	return getString(v)
}

// getFrameOptFrame extracts the structured frame from a frame_opt value, or
// nil when the value is an empty frame_opt string.
func getFrameOptFrame(v interface{}) *sql.WindowFrame {
	return getWindowFrame(v)
}

// frameFromStruct renders a *sql.WindowFrame back to its SQL frame text.
func frameFromStruct(f *sql.WindowFrame) string {
	spec := f.Type
	if spec != "" {
		if f.Between {
			spec += " BETWEEN " + boundString(f.Start) + " AND " + boundString(f.End)
		} else {
			spec += " " + boundString(f.Start)
		}
	}
	if f.Exclude != "" {
		spec += " EXCLUDE " + f.Exclude
	}
	return strings.TrimSpace(spec)
}

// boundString renders a FrameBound back to its SQL text (used by the
// frame_opt-only window shape so FrameSpec round-trips).
func boundString(b sql.FrameBound) string {
	switch b.Kind {
	case "PRECEDING", "FOLLOWING":
		if b.Expr != nil {
			return sql.ExprString(b.Expr) + " " + b.Kind
		}
		return b.Kind
	default:
		return b.Kind
	}
}

// getWindowDefList extracts a []sql.WindowDef from a parser stack value.
func getWindowDefList(v interface{}) []sql.WindowDef {
	if v == nil {
		return nil
	}
	if list, ok := v.([]sql.WindowDef); ok {
		return list
	}
	return nil
}

// frameSpecFromParts joins frame clause parts into a single frame spec
// string, skipping empty optional parts.
func frameSpecFromParts(parts ...string) string {
	var sb strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(p)
	}
	return sb.String()
}

// boolLitName returns "TRUE" or "FALSE" if the expression is a boolean
// literal column reference (the LALR parser represents TRUE/FALSE keywords as
// ColumnRef{Name:"TRUE"} / ColumnRef{Name:"FALSE"}), and whether it matched.
func boolLitName(e sql.Expr) (string, bool) {
	ref, ok := e.(*sql.ColumnRef)
	if !ok {
		return "", false
	}
	if ref.Name == "TRUE" || ref.Name == "FALSE" {
		return ref.Name, true
	}
	return "", false
}

// getAssignments extracts a []sql.Assignment from a stack value.
func getAssignments(v interface{}) []sql.Assignment {
	if v == nil {
		return nil
	}
	if a, ok := v.([]sql.Assignment); ok {
		return a
	}
	return nil
}

// getStringList extracts a []string from a stack value.
func getStringList(v interface{}) []string {
	if v == nil {
		return nil
	}
	if list, ok := v.([]string); ok {
		return list
	}
	return nil
}

func getSetOp(v interface{}) sql.SetOp {
	if v == nil {
		return sql.SetNone
	}
	if s, ok := v.(setOpResult); ok {
		return s.Op
	}
	return sql.SetNone
}

func getWhenClauses(v interface{}) []sql.WhenClause {
	if v == nil {
		return nil
	}
	if w, ok := v.([]sql.WhenClause); ok {
		return w
	}
	return nil
}

// stripSQLComments removes SQL block and line comments, preserving strings.

type createTableArgs struct {
	columns      []sql.ColumnDef
	constraints  []sql.TableConstraint
	withoutRowid bool
	strict       bool
}

// whereRet carries the WHERE expression and optional RETURNING projection for
// DELETE and UPDATE statements. The where_opt_ret nonterminal (rules 155-158)
// produces this value, and the DELETE/UPDATE cmd rules (152, 159) consume it.
// RETURNING columns are folded into a single sql.SelectColumn (multi-expression
// RETURNING becomes a RowValue), matching the AST's single-Returning field.
type whereRet struct {
	where     sql.Expr
	returning []sql.SelectColumn
}

// upsertVal carries the ON CONFLICT clause and/or RETURNING projection that an
// INSERT ... upsert nonterminal (rules 166-171) produces. INSERT statements can
// have both ON CONFLICT ... DO ... and RETURNING (e.g.
// "INSERT ... ON CONFLICT DO UPDATE SET ... RETURNING *"), so the upsert value
// carries both into rule 164's cmd handler.
type upsertVal struct {
	onConflict *sql.OnConflictClause
	returning  []sql.SelectColumn
}

// getUpsertVal extracts an *upsertVal semantic value.
func getUpsertVal(v interface{}) *upsertVal {
	if v == nil {
		return nil
	}
	if u, ok := v.(*upsertVal); ok {
		return u
	}
	return nil
}

// getWhereRet extracts a *whereRet semantic value.
func getWhereRet(v interface{}) *whereRet {
	if v == nil {
		return nil
	}
	if w, ok := v.(*whereRet); ok {
		return w
	}
	return nil
}

// foldReturning folds a slice of SELECT columns into a single sql.SelectColumn
// with a RowValue for multi-expression RETURNING, or nil when empty.
func foldReturning(cols []sql.SelectColumn) sql.SelectColumn {
	if len(cols) == 0 {
		return sql.SelectColumn{}
	}
	if len(cols) == 1 {
		return cols[0]
	}
	exprs := make([]sql.Expr, len(cols))
	for i, c := range cols {
		exprs[i] = c.Expr
	}
	return sql.SelectColumn{Expr: &sql.RowValue{Values: exprs}}
}

// getTableConstraints extracts a []sql.TableConstraint semantic value.
func getTableConstraints(v interface{}) []sql.TableConstraint {
	if v == nil {
		return nil
	}
	if list, ok := v.([]sql.TableConstraint); ok {
		return list
	}
	return nil
}

// getConstraintsCons coerces a single sql.TableConstraint into a one-element
// slice, for use by rule 376 (conslist ::= tcons).
func getConstraintsCons(v interface{}) []sql.TableConstraint {
	if tc, ok := v.(sql.TableConstraint); ok {
		if tc.Type == "" && tc.Name == "" {
			return ([]sql.TableConstraint)(nil)
		}
		return []sql.TableConstraint{tc}
	}
	return ([]sql.TableConstraint)(nil)
}

// getConsTConstraints coerces a value that may be either a single
// sql.TableConstraint or a []sql.TableConstraint into a slice.
func getConstraintSlice(v interface{}) []sql.TableConstraint {
	if list := getTableConstraints(v); list != nil {
		return list
	}
	return getConstraintsCons(v)
}

// getTableOptions extracts the *createTableArgs carry value produced by the
// table_option_set / table_option rules, returning a zero value if absent.
func getTableOptions(v interface{}) *createTableArgs {
	if opts, ok := v.(*createTableArgs); ok {
		return opts
	}
	return &createTableArgs{}
}

// indexColumnsFromSortlist converts a sortlist ([]sql.OrderByTerm) into the
// []sql.IndexedColumn list for a PRIMARY KEY / UNIQUE table constraint.
func indexColumnsFromSortlist(v interface{}) []sql.IndexedColumn {
	terms := getOrderByList(v)
	if terms == nil {
		return nil
	}
	out := make([]sql.IndexedColumn, 0, len(terms))
	for _, t := range terms {
		name, collate := indexedColumnName(t.Expr)
		out = append(out, sql.IndexedColumn{
			Name:    name,
			Collate: collate,
			Desc:    t.Desc,
		})
	}
	return out
}

// indexedColumnName extracts the column name (and optional COLLATE) from an
// expression used in a PRIMARY KEY / UNIQUE constraint column list.
func indexedColumnName(e sql.Expr) (string, string) {
	e = sql.UnwrapParenExpr(e)
	if bo, ok := e.(*sql.BinaryOp); ok && bo.Operator == "COLLATE" {
		if sl, ok := bo.Right.(*sql.StringLit); ok {
			n, _ := indexedColumnName(bo.Left)
			return n, sl.Value
		}
	}
	if ref, ok := e.(*sql.ColumnRef); ok {
		return ref.Name, ""
	}
	// SQLite sqlite3StringToId: a bare single-quoted string literal in an
	// index/constraint key becomes an identifier ("PRIMARY KEY('x' ASC)"
	// indexes column x). CREATE INDEX (rule 239) already applies this rule;
	// table-level PRIMARY KEY / UNIQUE constraints must do the same.
	if sl, ok := e.(*sql.StringLit); ok {
		return sl.Value, ""
	}
	return "", ""
}

// fkColumnsFromEidlist converts an eidlist (FOREIGN KEY column list) into
// []sql.IndexedColumn. Only the column names are meaningful for FK purposes.
func fkColumnsFromEidlist(v interface{}) []sql.IndexedColumn {
	names := getStringList(v)
	if names == nil {
		return nil
	}
	out := make([]sql.IndexedColumn, 0, len(names))
	for _, n := range names {
		out = append(out, sql.IndexedColumn{Name: n})
	}
	return out
}

// fixUpdateWhere re-parses an UPDATE statement's WHERE clause from the raw SQL
// text. The generated LALR grammar has a shift/reduce conflict in the UPDATE
// production: a function call followed by '=' in the WHERE (e.g.
// "WHERE abs(x)=248") is mis-parsed as a single column named "absx" (the
// '(' + args + ')' are absorbed). Re-parsing the WHERE as a SELECT-style
// expression (SELECT ... WHERE expr) yields the correct AST.
// findTopLevelKeyword finds a keyword at paren depth 0 (or returns -1).
// hasSavepointStatements reports whether the input contains a SAVEPOINT,
// RELEASE, or ROLLBACK TO statement at the top level.
