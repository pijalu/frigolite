// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar.
//
// Code in this file is ORGANIZED BY GRAMMAR FUNCTION, one file per family;
// rule numbers are go-lemon table indices (see parser_ruleids.go).
//
// Expression primary forms: literals, column references, function
// calls, CAST, CASE, RAISE, EXISTS, subqueries, and expression lists.

package parse

import (
	"fmt"
	sql "github.com/pijalu/frigolite/internal/sql"
	"strings"
)

// Rule 179: expr ::= LP expr RP
func ruleExprLpExprRp(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 180: expr ::= ID|INDEXED|JOIN_KW (column reference)
func ruleExprIdIndexedJoinKwColumnReference(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		// Keep the Quoted flag on all double-quoted identifiers (including
		// the empty "") so resolution can apply SQLite's DQS rules: with
		// DQS enabled an unmatched double-quoted identifier becomes a
		// string literal; with DQS disabled it is a "no such column"
		// error hinting at single-quoted strings.
		return &sql.ColumnRef{Name: tok.Value, Quoted: tok.QuotedIdent}
	}
	if s, ok := getRHS(p, ruleNo, 1).(string); ok {
		return &sql.ColumnRef{Name: s}
	}
	return &sql.ColumnRef{Name: fmt.Sprintf("%v", getRHS(p, ruleNo, 1))}

}

// Rule 181: expr ::= nm DOT nm (schema.table)
func ruleExprNmDotNmSchemaTable(ruleNo int, p *Parser) interface{} {
	schema := getString(getRHS(p, ruleNo, 1))
	col := getString(getRHS(p, ruleNo, 3))
	return &sql.ColumnRef{Table: schema, Name: col}

}

// Rule 182: expr ::= nm DOT nm DOT nm (schema.table.column)
func ruleExprNmDotNmDotNmSchemaTableColumn(ruleNo int, p *Parser) interface{} {
	schema := getString(getRHS(p, ruleNo, 1))
	table := getString(getRHS(p, ruleNo, 3))
	col := getString(getRHS(p, ruleNo, 5))
	return &sql.ColumnRef{Table: schema + "." + table, Name: col}

}

// Rule 183: term ::= NULL|FLOAT|BLOB
func ruleTermNullFloatBlob(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		if strings.EqualFold(tok.Value, "NULL") {
			return &sql.NullLit{}
		}
		// Hex blob literal X'...' / x'...': decode the hex content so
		// the value keeps its blob type instead of becoming a number.
		if tok.Type == sql.TokenBlob {
			return decodeBlobToken(tok.Value)
		}
		return &sql.NumericLit{Value: tok.Value}
	}
	return &sql.NullLit{}

}

// Rule 184: term ::= STRING
func ruleTermString(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return &sql.StringLit{Value: tok.Value}
	}
	if s, ok := getRHS(p, ruleNo, 1).(string); ok {
		return &sql.StringLit{Value: s}
	}
	return &sql.StringLit{}

}

// Rule 185: term ::= INTEGER
func ruleTermInteger(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return &sql.NumericLit{Value: tok.Value}
	}
	if s, ok := getRHS(p, ruleNo, 1).(string); ok {
		return &sql.NumericLit{Value: s}
	}
	return &sql.NumericLit{}

}

// Rule 186: expr ::= VARIABLE
// A parameter placeholder (? or $name). Frigolite does not support bound
// parameters; it evaluates to NULL, but is kept distinct from a NULL
// literal so CREATE TABLE can reject it in non-constant DEFAULT
// expressions.
func ruleExprVariable(ruleNo int, p *Parser) interface{} {
	param := &sql.ParameterExpr{}
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		param.Name = tok.Value
	}
	return param

}

// Rule 188: expr ::= CAST LP expr AS typetoken RP
func ruleExprCastLpExprAsTypetokenRp(ruleNo int, p *Parser) interface{} {
	return &sql.CastExpr{
		Operand: getExpr(getRHS(p, ruleNo, 3)),
		AsType:  getString(getRHS(p, ruleNo, 5)),
	}

}

// Rule 189: expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist RP (function call)
func ruleExprFunc(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	distinct := getBool(getRHS(p, ruleNo, 3))
	args := getExprList(getRHS(p, ruleNo, 4))
	return &sql.FuncCall{
		Name:     name,
		Args:     args,
		Distinct: distinct,
	}

}

// Rule 190: expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist ORDER BY sortlist RP
// (function call with internal ORDER BY, e.g. group_concat(x ORDER BY y))
func ruleExprFuncOrderBy(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	distinct := getBool(getRHS(p, ruleNo, 3))
	args := getExprList(getRHS(p, ruleNo, 4))
	orderBy := getOrderByList(getRHS(p, ruleNo, 6))
	return &sql.FuncCall{
		Name:     name,
		Args:     args,
		Distinct: distinct,
		OrderBy:  orderBy,
	}

}

// Rule 191: expr ::= ID|INDEXED|JOIN_KW LP STAR RP (function(star))
func ruleExprFuncStar(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	return &sql.FuncCall{
		Name: name,
		Args: []sql.Expr{&sql.ColumnRef{Name: "*"}}, // COUNT(*) — star as a column ref
	}

}

// Rule 192: expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist RP filter_over
func ruleExprFuncFilterOver(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	distinct := getBool(getRHS(p, ruleNo, 3))
	args := getExprList(getRHS(p, ruleNo, 4))
	wf := getWindowFilter(getRHS(p, ruleNo, 6))
	var over *sql.WindowDef
	var filter sql.Expr
	if wf != nil {
		over = wf.over
		filter = wf.filter
	}
	return &sql.FuncCall{
		Name:     name,
		Args:     args,
		Distinct: distinct,
		Filter:   filter,
		Over:     over,
	}

}

// Rule 193: expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist ORDER BY sortlist RP filter_over
func ruleExprFuncOrderByFilterOver(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	distinct := getBool(getRHS(p, ruleNo, 3))
	args := getExprList(getRHS(p, ruleNo, 4))
	orderBy := getOrderByList(getRHS(p, ruleNo, 6))
	wf := getWindowFilter(getRHS(p, ruleNo, 9))
	var over *sql.WindowDef
	var filter sql.Expr
	if wf != nil {
		over = wf.over
		filter = wf.filter
	}
	return &sql.FuncCall{
		Name:     name,
		Args:     args,
		Distinct: distinct,
		OrderBy:  orderBy,
		Filter:   filter,
		Over:     over,
	}

}

// Rule 194: expr ::= ID|INDEXED|JOIN_KW LP STAR RP filter_over (window function)
func ruleExprFuncStarFilterOver(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	wf := getWindowFilter(getRHS(p, ruleNo, 5))
	var over *sql.WindowDef
	var filter sql.Expr
	if wf != nil {
		over = wf.over
		filter = wf.filter
	}
	return &sql.FuncCall{
		Name:   name,
		Args:   []sql.Expr{&sql.ColumnRef{Name: "*"}}, // COUNT(*) — star as a column ref
		Filter: filter,
		Over:   over,
	}

}

// Rule 196: expr ::= LP exprlist COMMA expr RP (row value / vector)
// A parenthesized list of two or more expressions is a row value used
// in comparisons like (a, b) = ('x', 'y'). The grammar splits the list
// as (exprlist, expr) with exprlist holding all but the last element.
func ruleExprLpExprlistCommaExprRpRowValueVector(ruleNo int, p *Parser) interface{} {
	exprs := getExprList(getRHS(p, ruleNo, 2))
	last := getExpr(getRHS(p, ruleNo, 4))
	exprs = append(exprs, last)
	return &sql.RowValue{Values: exprs}

}

// Rule 224: expr ::= LP select RP
func ruleExprLpSelectRp(ruleNo int, p *Parser) interface{} {
	return &sql.Subquery{
		Select: getSelectStmt(getRHS(p, ruleNo, 2)),
	}

}

// Rule 227: expr ::= EXISTS LP select RP
func ruleExprExistsLpSelectRp(ruleNo int, p *Parser) interface{} {
	return &sql.ExistsExpr{
		Select:  getSelectStmt(getRHS(p, ruleNo, 3)),
		Negated: false,
	}

}

// Rule 228: expr ::= CASE case_operand case_exprlist case_else END
func ruleExprCase(ruleNo int, p *Parser) interface{} {
	operand := getExpr(getRHS(p, ruleNo, 2))
	whenList := getWhenClauses(getRHS(p, ruleNo, 3))
	elseExpr := getExpr(getRHS(p, ruleNo, 4))
	return &sql.CaseExpr{
		Operand: operand,
		Whens:   whenList,
		Else:    elseExpr,
	}

}

// Rule 229: case_exprlist ::= case_exprlist WHEN expr THEN expr
func ruleCaseExprlistCaseExprlistWhenExprThenExpr(ruleNo int, p *Parser) interface{} {
	acc := getWhenClauses(getRHS(p, ruleNo, 1))
	whenExpr := getExpr(getRHS(p, ruleNo, 3))
	thenExpr := getExpr(getRHS(p, ruleNo, 5))
	return append(acc, sql.WhenClause{When: whenExpr, Then: thenExpr})

}

// Rule 230: case_exprlist ::= WHEN expr THEN expr
func ruleCaseExprlistWhenExprThenExpr(ruleNo int, p *Parser) interface{} {
	whenExpr := getExpr(getRHS(p, ruleNo, 2))
	thenExpr := getExpr(getRHS(p, ruleNo, 4))
	return []sql.WhenClause{{When: whenExpr, Then: thenExpr}}

}

// Rule 231: case_else ::= ELSE expr
func ruleCaseElseElseExpr(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 232: case_else ::=
func ruleCaseElse(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 233: case_operand ::=
func ruleCaseOperand(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 234: exprlist ::=
func ruleExprlist(ruleNo int, p *Parser) interface{} {
	return ([]sql.Expr)(nil)

}

// Rule 235: nexprlist ::= nexprlist COMMA expr
func ruleNexprlistNexprlistCommaExpr(ruleNo int, p *Parser) interface{} {
	acc := getExprList(getRHS(p, ruleNo, 1))
	return append(acc, getExpr(getRHS(p, ruleNo, 3)))

}

// Rule 236: nexprlist ::= expr
func ruleNexprlistExpr(ruleNo int, p *Parser) interface{} {
	return []sql.Expr{getExpr(getRHS(p, ruleNo, 1))}

}

// Rule 237: paren_exprlist ::=
func ruleParenExprlist(ruleNo int, p *Parser) interface{} {
	return ([]sql.Expr)(nil)

}

// Rule 238: paren_exprlist ::= LP exprlist RP
func ruleParenExprlistLpExprlistRp(ruleNo int, p *Parser) interface{} {
	return getExprList(getRHS(p, ruleNo, 2))

}

// Rule 278: expr ::= RAISE LP IGNORE RP
func ruleExprRaiseLpIgnoreRp(ruleNo int, p *Parser) interface{} {
	return &sql.RaiseExpr{Kind: "IGNORE"}

}

// Rule 279: expr ::= RAISE LP raisetype COMMA expr RP
func ruleExprRaise(ruleNo int, p *Parser) interface{} {
	return &sql.RaiseExpr{
		Kind:    getString(getRHS(p, ruleNo, 3)),
		Message: getExpr(getRHS(p, ruleNo, 5)),
	}

}

// Rule 280: raisetype ::= ROLLBACK
// Rules 280-282: raisetype ::= ROLLBACK | ABORT | FAIL
func ruleRaisetypeRollback(ruleNo int, p *Parser) interface{} {
	return "ROLLBACK"
}

// Rule 281: raisetype ::= ABORT
func ruleRaisetypeAbort(ruleNo int, p *Parser) interface{} {
	return "ABORT"
}

// Rule 282: raisetype ::= FAIL
func ruleRaisetypeFail(ruleNo int, p *Parser) interface{} {
	return "FAIL"

}

// Rule 386: expr ::= term
func ruleExprTerm(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 389: exprlist ::= expr
func ruleExprlistExpr(ruleNo int, p *Parser) interface{} {
	return getExprList(getRHS(p, ruleNo, 1))

}

// isCurrentTimeKeyword reports whether name is CURRENT_TIME, CURRENT_DATE, or
// CURRENT_TIMESTAMP (case-insensitive).
func isCurrentTimeKeyword(name string) bool {
	switch strings.ToUpper(name) {
	case "CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP":
		return true
	}
	return false
}
