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
// Expression operators: arithmetic, comparison, logic, LIKE/GLOB
// family, BETWEEN, IN, IS, COLLATE, bit/shift, unary, and ->/->>.

package parse

import (
	sql "github.com/pijalu/frigolite/internal/sql"
	"strings"
)

// Rule 187: expr ::= expr COLLATE ID|STRING
func ruleExprExprCollateIdString(ruleNo int, p *Parser) interface{} {
	expr := getExpr(getRHS(p, ruleNo, 1))
	collation := getString(getRHS(p, ruleNo, 3))
	// COLLATE is an operator that wraps the expression
	return &sql.BinaryOp{
		Left:     expr,
		Operator: "COLLATE",
		Right:    &sql.StringLit{Value: collation},
	}

}

// Rule 197: expr ::= expr AND expr
func ruleExprExprAndExpr(ruleNo int, p *Parser) interface{} {
	return &sql.BinaryOp{
		Left:     getExpr(getRHS(p, ruleNo, 1)),
		Operator: "AND",
		Right:    getExpr(getRHS(p, ruleNo, 3)),
	}

}

// Rule 198: expr ::= expr OR expr
func ruleExprExprOrExpr(ruleNo int, p *Parser) interface{} {
	return &sql.BinaryOp{
		Left:     getExpr(getRHS(p, ruleNo, 1)),
		Operator: "OR",
		Right:    getExpr(getRHS(p, ruleNo, 3)),
	}

}

// Rule 199: expr ::= expr LT|GT|GE|LE expr
func ruleExprExprLtGtGeLeExpr(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	// Read the operator from the RHS token value (the lookahead at reduce
	// time is the NEXT token, not the operator being reduced).
	op := "<"
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok {
		switch strings.ToUpper(tok.Value) {
		case ">":
			op = ">"
		case ">=":
			op = ">="
		case "<=":
			op = "<="
		}
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

}

// Rule 200: expr ::= expr EQ|NE expr
func ruleExprExprEqNeExpr(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	// Read the operator from the RHS token value (lookahead is the NEXT
	// token, so it cannot distinguish = from != / <>).
	op := "="
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok {
		if tok.Value == "!=" || tok.Value == "<>" {
			op = "<>"
		}
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

}

// Rule 201: expr ::= expr BITOP expr (& | << >>)
func ruleExprBitop(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	// Read the operator from the RHS token value (lookahead is the NEXT
	// token at reduce time).
	op := "&"
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok {
		switch tok.Value {
		case "|":
			op = "|"
		case "<<":
			op = "<<"
		case ">>":
			op = ">>"
		}
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

}

// Rule 202: expr ::= expr PLUS|MINUS expr
func ruleExprExprPlusMinusExpr(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	op := "+"
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok && tok.Value == "-" {
		op = "-"
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

}

// Rule 203: expr ::= expr STAR|SLASH|REM expr
func ruleExprExprStarSlashRemExpr(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	op := "*"
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok {
		switch tok.Value {
		case "/":
			op = "/"
		case "%":
			op = "%"
		}
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

}

// Rule 204: expr ::= expr CONCAT expr
func ruleExprExprConcatExpr(ruleNo int, p *Parser) interface{} {
	return &sql.BinaryOp{
		Left:     getExpr(getRHS(p, ruleNo, 1)),
		Operator: "||",
		Right:    getExpr(getRHS(p, ruleNo, 3)),
	}

}

// Rule 205: likeop ::= NOT LIKE_KW|MATCH — the negated form of a
// LIKE/GLOB/REGEXP/MATCH operator ("a NOT LIKE 'x'"). Returns the
// negated operator name so rule 206 can build a NOT LIKE BinaryOp.
func ruleLikeopNot(ruleNo int, p *Parser) interface{} {
	op := "NOT LIKE"
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok {
		switch strings.ToUpper(tok.Value) {
		case "MATCH":
			op = "NOT MATCH"
		case "GLOB":
			op = "NOT GLOB"
		case "REGEXP":
			op = "NOT REGEXP"
		}
	}
	return op

}

// Rule 206: expr ::= expr likeop expr (LIKE/GLOB/REGEXP/MATCH)
func ruleExprLikeop(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	op := "LIKE"
	if s, ok := getRHS(p, ruleNo, 2).(string); ok && s != "" {
		op = s
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

}

// Rule 207: expr ::= expr likeop expr ESCAPE expr
func ruleExprLikeopEscape(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	escape := getExpr(getRHS(p, ruleNo, 5))
	// likeop may be the negated form ("NOT LIKE" etc. from rule 205) — the
	// operator name must survive the ESCAPE attachment, or `x NOT LIKE y
	// ESCAPE z` silently evaluates as a positive LIKE.
	op := "LIKE"
	if s, ok := getRHS(p, ruleNo, 2).(string); ok && s != "" {
		op = s
	}
	return &sql.BinaryOp{
		Left:      left,
		Operator:  op,
		Right:     right,
		Escape:    getString(escape),
		HasEscape: true,
	}

}

// Rule 208: expr ::= expr ISNULL|NOTNULL
func ruleExprIsnullNotnull(ruleNo int, p *Parser) interface{} {
	operand := getExpr(getRHS(p, ruleNo, 1))
	// Read the operator from the RHS token value (lookahead at reduce
	// time is the NEXT token, not the ISNULL/NOTNULL keyword).
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok && tok.Value != "ISNULL" {
		return &sql.IsNotNull{Operand: operand}
	}
	return &sql.IsNull{Operand: operand}

}

// Rule 209: expr ::= expr NOT likeop expr (NOT LIKE / NOT GLOB /
// NOT REGEXP / NOT MATCH) or expr ::= expr NOT NULL (the postfix
// NOT NULL operator, equivalent to IS NOT NULL). The NOT negates the
// likeop result; a trailing NULL keyword instead makes it IsNotNull.
func ruleExprExprNotLikeopExprNotLikeNotGlob(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	if tok, ok := getRHS(p, ruleNo, 3).(sql.Token); ok && strings.EqualFold(tok.Value, "NULL") {
		return &sql.IsNotNull{Operand: left}
	}
	op := "NOT LIKE"
	if s, ok := getRHS(p, ruleNo, 2).(string); ok && s != "" {
		switch s {
		case "LIKE":
			op = "NOT LIKE"
		case "GLOB":
			op = "NOT GLOB"
		case "REGEXP":
			op = "NOT REGEXP"
		case "MATCH":
			op = "NOT MATCH"
		}
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

}

// Rule 210: expr ::= expr IS expr
func ruleExprExprIsExpr(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	// IS TRUE / IS FALSE predicates. The right side may be wrapped in a
	// COLLATE operator (e.g. `x IS TRUE COLLATE NOCASE`), which SQLite
	// parses as the IS TRUE predicate with a no-op collation on the
	// result; unwrap it so the predicate is still recognized.
	boolExpr := right
	if bo, ok := boolExpr.(*sql.BinaryOp); ok && bo.Operator == "COLLATE" {
		boolExpr = bo.Left
	}
	if name, ok := boolLitName(boolExpr); ok {
		if name == "TRUE" {
			return &sql.IsTrue{Operand: left}
		}
		return &sql.IsFalse{Operand: left}
	}
	return &sql.BinaryOp{Left: left, Operator: "IS", Right: right}

}

// Rule 211: expr ::= expr IS NOT expr
func ruleExprExprIsNotExpr(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 4))
	// IS NOT TRUE / IS NOT FALSE predicates (unwrap a COLLATE wrapper on
	// the right side, mirroring rule 210).
	boolExpr := right
	if bo, ok := boolExpr.(*sql.BinaryOp); ok && bo.Operator == "COLLATE" {
		boolExpr = bo.Left
	}
	if name, ok := boolLitName(boolExpr); ok {
		if name == "TRUE" {
			return &sql.IsTrue{Operand: left, Negated: true}
		}
		return &sql.IsFalse{Operand: left, Negated: true}
	}
	return &sql.BinaryOp{Left: left, Operator: "IS NOT", Right: right}

}

// Rule 212: expr ::= expr IS NOT DISTINCT FROM expr (6 RHS symbols)
func ruleExprExprIsNotDistinctFromExpr6RhsSymbols(ruleNo int, p *Parser) interface{} {
	return &sql.IsNotDistinctFrom{
		Left:  getExpr(getRHS(p, ruleNo, 1)),
		Right: getExpr(getRHS(p, ruleNo, 6)),
	}

}

// Rule 213: expr ::= expr IS DISTINCT FROM expr (5 RHS symbols)
func ruleExprExprIsDistinctFromExpr5RhsSymbols(ruleNo int, p *Parser) interface{} {
	return &sql.IsDistinctFrom{
		Left:  getExpr(getRHS(p, ruleNo, 1)),
		Right: getExpr(getRHS(p, ruleNo, 5)),
	}

}

// Rule 214: expr ::= NOT expr
func ruleExprNotExpr(ruleNo int, p *Parser) interface{} {
	return &sql.UnaryOp{
		Operand:  getExpr(getRHS(p, ruleNo, 2)),
		Operator: "NOT",
	}

}

// Rule 215: expr ::= BITNOT expr
func ruleExprBitnotExpr(ruleNo int, p *Parser) interface{} {
	return &sql.UnaryOp{
		Operand:  getExpr(getRHS(p, ruleNo, 2)),
		Operator: "~",
	}

}

// Rule 216: expr ::= PLUS|MINUS expr (unary)
func ruleExprPlusMinusExprUnary(ruleNo int, p *Parser) interface{} {
	operand := getExpr(getRHS(p, ruleNo, 2))
	// Read the operator from the RHS token value (lookahead is the NEXT
	// token at reduce time, so it cannot distinguish + from -).
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok && tok.Value == "-" {
		// SQLite special case: -9223372036854775808 is the minimum int64.
		// The positive literal 9223372036854775808 does not fit in int64
		// (it is 2^63), so SQLite folds the unary minus into the literal
		// to produce math.MinInt64 as an INTEGER (not a REAL).
		if nl, ok := operand.(*sql.NumericLit); ok && nl.Value == "9223372036854775808" {
			return &sql.NumericLit{Value: "-9223372036854775808"}
		}
		// Leading zeros do not change the magnitude (SQLite numerals are
		// decimal, never octal): -00000009223372036854775808 folds too
		// (expr-8.41 typeof is integer).
		if nl, ok := operand.(*sql.NumericLit); ok && strings.TrimLeft(nl.Value, "0") == "9223372036854775808" {
			return &sql.NumericLit{Value: "-9223372036854775808"}
		}
		// SQLite folds the sign into hex literals too, so the "hex
		// literal too big" error message carries the minus sign
		// (e.g. "-0x08000000000000000").
		if nl, ok := operand.(*sql.NumericLit); ok && isHexLiteral(nl.Value) {
			return &sql.NumericLit{Value: "-" + nl.Value}
		}
		return &sql.UnaryOp{Operand: operand, Operator: "-"}
	}
	// Unary + is a no-op at parse level (SQLite semantics: +expr is
	// equivalent to expr but the result has NO affinity).
	return &sql.UnaryOp{Operand: operand, Operator: "+"}

}

// Rule 217: expr ::= expr PTR expr — the SQLite '->' and '->>' JSON
// operators. The grammar uses a single PTR terminal for both (SQLite
// tokenize.c emits TK_PTR for either); the operator text distinguishes
// them: '->' yields the subvalue as JSON text, '->>' as a plain SQL value.
func ruleExprPtr(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	op := "->"
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok && tok.Value == "->>" {
		op = "->>"
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

}

// Rule 220: expr ::= expr between_op expr AND expr
func ruleExprBetween(ruleNo int, p *Parser) interface{} {
	// between_op is the raw keyword token: BETWEEN (or NOT for
	// "NOT BETWEEN"). SQLite's grammar reduces between_op to an
	// int flag; here the token itself sits on the stack.
	negated := false
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok && strings.EqualFold(tok.Value, "NOT") {
		negated = true
	}
	return &sql.Between{
		Operand: getExpr(getRHS(p, ruleNo, 1)),
		Low:     getExpr(getRHS(p, ruleNo, 3)),
		High:    getExpr(getRHS(p, ruleNo, 5)),
		Negated: negated,
	}

}

// Rule 221: in_op ::= IN
func ruleInOpIn(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 222: in_op ::= NOT IN
func ruleInOpNotIn(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 223: expr ::= expr in_op LP exprlist RP
func ruleExprExprInOpLpExprlistRp(ruleNo int, p *Parser) interface{} {
	negated := getBool(getRHS(p, ruleNo, 2))
	return &sql.InList{
		Operand: getExpr(getRHS(p, ruleNo, 1)),
		List:    getExprList(getRHS(p, ruleNo, 4)),
		Negated: negated,
	}

}

// Rule 225: expr ::= expr in_op LP select RP
func ruleExprExprInOpLpSelectRp(ruleNo int, p *Parser) interface{} {
	negated := getBool(getRHS(p, ruleNo, 2))
	return &sql.InList{
		Operand: getExpr(getRHS(p, ruleNo, 1)),
		List:    []sql.Expr{&sql.Subquery{Select: getSelectStmt(getRHS(p, ruleNo, 4))}},
		Negated: negated,
	}

}

// Rule 226: expr ::= expr in_op nm dbnm paren_exprlist
// SQLite extension: `expr IN table-name` is equivalent to
// `expr IN (SELECT * FROM table-name)`. The optional paren_exprlist is
// the argument list of a table-valued function in the FROM clause.
func ruleExprExprInOpNmDbnmParenExprlist(ruleNo int, p *Parser) interface{} {
	negated := getBool(getRHS(p, ruleNo, 2))
	tbl := getString(getRHS(p, ruleNo, 3))
	schema := getString(getRHS(p, ruleNo, 4))
	if schema != "" {
		tbl = tbl + "." + schema
	}
	args := getExprList(getRHS(p, ruleNo, 5))
	// paren_exprlist may be empty: "x IN t" and "x IN t()" are the same
	// rowid-lookup form (no table-function call), while "x IN tvf(a,b)"
	// is a genuine table-valued function reference.
	sub := &sql.Subquery{Select: &sql.SelectStmt{
		Columns: []sql.SelectColumn{{Expr: &sql.ColumnRef{Name: "*"}}},
		From:    sql.TableRef{Name: tbl, Args: args, IsTabFunc: len(args) > 0},
	}}
	return &sql.InList{
		Operand: getExpr(getRHS(p, ruleNo, 1)),
		List:    []sql.Expr{sub},
		Negated: negated,
	}

}

// Rule 387: likeop ::= LIKE|GLOB|MATCH|REGEXP (single keyword form)
func ruleLikeopKeywords(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		switch strings.ToUpper(tok.Value) {
		case "MATCH":
			return "MATCH"
		case "GLOB":
			return "GLOB"
		case "REGEXP":
			return "REGEXP"
		}
	}
	return "LIKE"

}
