package parse

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// Rule 185: term ::= INTEGER
func rule185(ruleNo int, p *Parser) interface{} {
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
func rule186(ruleNo int, p *Parser) interface{} {
	param := &sql.ParameterExpr{}
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		param.Name = tok.Value
	}
	return param

}

// Rule 187: expr ::= expr COLLATE ID|STRING
func rule187(ruleNo int, p *Parser) interface{} {
	expr := getExpr(getRHS(p, ruleNo, 1))
	collation := getString(getRHS(p, ruleNo, 3))
	// COLLATE is an operator that wraps the expression
	return &sql.BinaryOp{
		Left:     expr,
		Operator: "COLLATE",
		Right:    &sql.StringLit{Value: collation},
	}

}

// Rule 188: expr ::= CAST LP expr AS typetoken RP
func rule188(ruleNo int, p *Parser) interface{} {
	return &sql.CastExpr{
		Operand: getExpr(getRHS(p, ruleNo, 3)),
		AsType:  getString(getRHS(p, ruleNo, 5)),
	}

}

// Rule 189: expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist RP (function call)
func rule189(ruleNo int, p *Parser) interface{} {
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
func rule190(ruleNo int, p *Parser) interface{} {
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
func rule191(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	return &sql.FuncCall{
		Name: name,
		Args: []sql.Expr{&sql.ColumnRef{Name: "*"}}, // COUNT(*) — star as a column ref
	}

}

func rule192(ruleNo int, p *Parser) interface{} {
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
func rule193(ruleNo int, p *Parser) interface{} {
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
func rule194(ruleNo int, p *Parser) interface{} {
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
func rule196(ruleNo int, p *Parser) interface{} {
	exprs := getExprList(getRHS(p, ruleNo, 2))
	last := getExpr(getRHS(p, ruleNo, 4))
	exprs = append(exprs, last)
	return &sql.RowValue{Values: exprs}

}

// Rule 197: expr ::= expr AND expr
func rule197(ruleNo int, p *Parser) interface{} {
	return &sql.BinaryOp{
		Left:     getExpr(getRHS(p, ruleNo, 1)),
		Operator: "AND",
		Right:    getExpr(getRHS(p, ruleNo, 3)),
	}

}

// Rule 198: expr ::= expr OR expr
func rule198(ruleNo int, p *Parser) interface{} {
	return &sql.BinaryOp{
		Left:     getExpr(getRHS(p, ruleNo, 1)),
		Operator: "OR",
		Right:    getExpr(getRHS(p, ruleNo, 3)),
	}

}

// Rule 199: expr ::= expr LT|GT|GE|LE expr
func rule199(ruleNo int, p *Parser) interface{} {
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
func rule200(ruleNo int, p *Parser) interface{} {
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

func rule201(ruleNo int, p *Parser) interface{} {
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

// Rule 217: expr ::= expr PTR expr — the SQLite '->' and '->>' JSON
// operators. The grammar uses a single PTR terminal for both (SQLite
// tokenize.c emits TK_PTR for either); the operator text distinguishes
// them: '->' yields the subvalue as JSON text, '->>' as a plain SQL value.
func rule217(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	op := "->"
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok && tok.Value == "->>" {
		op = "->>"
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

}

// Rule 202: expr ::= expr PLUS|MINUS expr
func rule202(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	op := "+"
	if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok && tok.Value == "-" {
		op = "-"
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

}

// Rule 203: expr ::= expr STAR|SLASH|REM expr
func rule203(ruleNo int, p *Parser) interface{} {
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
func rule204(ruleNo int, p *Parser) interface{} {
	return &sql.BinaryOp{
		Left:     getExpr(getRHS(p, ruleNo, 1)),
		Operator: "||",
		Right:    getExpr(getRHS(p, ruleNo, 3)),
	}

}

// Rule 205: likeop ::= NOT LIKE_KW|MATCH — the negated form of a
// LIKE/GLOB/REGEXP/MATCH operator ("a NOT LIKE 'x'"). Returns the
// negated operator name so rule 206 can build a NOT LIKE BinaryOp.
func rule205(ruleNo int, p *Parser) interface{} {
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

	// Rule 206: expr ::= expr likeop expr (LIKE/GLOB/REGEXP/MATCH)
}

func rule206(ruleNo int, p *Parser) interface{} {
	left := getExpr(getRHS(p, ruleNo, 1))
	right := getExpr(getRHS(p, ruleNo, 3))
	op := "LIKE"
	if s, ok := getRHS(p, ruleNo, 2).(string); ok && s != "" {
		op = s
	}
	return &sql.BinaryOp{Left: left, Operator: op, Right: right}

	// Rule 207: expr ::= expr likeop expr ESCAPE expr
}

func rule207(ruleNo int, p *Parser) interface{} {
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

func rule208(ruleNo int, p *Parser) interface{} {
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
func rule209(ruleNo int, p *Parser) interface{} {
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
func rule210(ruleNo int, p *Parser) interface{} {
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
func rule211(ruleNo int, p *Parser) interface{} {
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
