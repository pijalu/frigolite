// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar.
//
// Continuation of the per-rule grammar action handlers (split from
// parser_rules*.go to keep file sizes manageable).

package parse

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// Rule 212: expr ::= expr IS NOT DISTINCT FROM expr (6 RHS symbols)
func rule212(ruleNo int, p *Parser) interface{} {
	return &sql.IsNotDistinctFrom{
		Left:  getExpr(getRHS(p, ruleNo, 1)),
		Right: getExpr(getRHS(p, ruleNo, 6)),
	}

}

// Rule 213: expr ::= expr IS DISTINCT FROM expr (5 RHS symbols)
func rule213(ruleNo int, p *Parser) interface{} {
	return &sql.IsDistinctFrom{
		Left:  getExpr(getRHS(p, ruleNo, 1)),
		Right: getExpr(getRHS(p, ruleNo, 5)),
	}

}

// Rule 214: expr ::= NOT expr
func rule214(ruleNo int, p *Parser) interface{} {
	return &sql.UnaryOp{
		Operand:  getExpr(getRHS(p, ruleNo, 2)),
		Operator: "NOT",
	}

}

// Rule 215: expr ::= BITNOT expr
func rule215(ruleNo int, p *Parser) interface{} {
	return &sql.UnaryOp{
		Operand:  getExpr(getRHS(p, ruleNo, 2)),
		Operator: "~",
	}

}

// Rule 216: expr ::= PLUS|MINUS expr (unary)
func rule216(ruleNo int, p *Parser) interface{} {
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

func rule220(ruleNo int, p *Parser) interface{} {
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
func rule221(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 222: in_op ::= NOT IN
func rule222(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 223: expr ::= expr in_op LP exprlist RP
func rule223(ruleNo int, p *Parser) interface{} {
	negated := getBool(getRHS(p, ruleNo, 2))
	return &sql.InList{
		Operand: getExpr(getRHS(p, ruleNo, 1)),
		List:    getExprList(getRHS(p, ruleNo, 4)),
		Negated: negated,
	}

}

// Rule 224: expr ::= LP select RP
func rule224(ruleNo int, p *Parser) interface{} {
	return &sql.Subquery{
		Select: getSelectStmt(getRHS(p, ruleNo, 2)),
	}

}

// Rule 225: expr ::= expr in_op LP select RP
func rule225(ruleNo int, p *Parser) interface{} {
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
func rule226(ruleNo int, p *Parser) interface{} {
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

// Rule 227: expr ::= EXISTS LP select RP
func rule227(ruleNo int, p *Parser) interface{} {
	return &sql.ExistsExpr{
		Select:  getSelectStmt(getRHS(p, ruleNo, 3)),
		Negated: false,
	}

}

func rule228(ruleNo int, p *Parser) interface{} {
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
func rule229(ruleNo int, p *Parser) interface{} {
	acc := getWhenClauses(getRHS(p, ruleNo, 1))
	whenExpr := getExpr(getRHS(p, ruleNo, 3))
	thenExpr := getExpr(getRHS(p, ruleNo, 5))
	return append(acc, sql.WhenClause{When: whenExpr, Then: thenExpr})

}

// Rule 230: case_exprlist ::= WHEN expr THEN expr
func rule230(ruleNo int, p *Parser) interface{} {
	whenExpr := getExpr(getRHS(p, ruleNo, 2))
	thenExpr := getExpr(getRHS(p, ruleNo, 4))
	return []sql.WhenClause{{When: whenExpr, Then: thenExpr}}

}

// Rule 231: case_else ::= ELSE expr
func rule231(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 232: case_else ::=
func rule232(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 233: case_operand ::=
func rule233(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 234: exprlist ::=
func rule234(ruleNo int, p *Parser) interface{} {
	return ([]sql.Expr)(nil)

}

// Rule 235: nexprlist ::= nexprlist COMMA expr
func rule235(ruleNo int, p *Parser) interface{} {
	acc := getExprList(getRHS(p, ruleNo, 1))
	return append(acc, getExpr(getRHS(p, ruleNo, 3)))

}

func rule236(ruleNo int, p *Parser) interface{} {
	return []sql.Expr{getExpr(getRHS(p, ruleNo, 1))}

}

// Rule 237: paren_exprlist ::=
func rule237(ruleNo int, p *Parser) interface{} {
	return ([]sql.Expr)(nil)

}

// Rule 238: paren_exprlist ::= LP exprlist RP
func rule238(ruleNo int, p *Parser) interface{} {
	return getExprList(getRHS(p, ruleNo, 2))

}

// Rule 239: cmd ::= createkw uniqueflag INDEX ifnotexists nm dbnm ON nm LP sortlist RP where_opt
func rule239(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 5))
	dbnm := getString(getRHS(p, ruleNo, 6))
	if dbnm != "" {
		// The full index name is "nm.dbnm" (e.g. CREATE INDEX aux.i2:
		// nm="aux", dbnm="i2"). The exec layer splits the schema prefix
		// back off.
		name = name + "." + dbnm
	}
	table := getString(getRHS(p, ruleNo, 8))
	sortlist := getOrderByList(getRHS(p, ruleNo, 10))
	// NULLS FIRST/LAST is only valid in ORDER BY, not in index key
	// definitions (SQLite: "unsupported use of NULLS FIRST/LAST").
	if err := rejectNullsInSortlist(sortlist); err != nil {
		p.SemanticErr = err
		return nil
	}
	where := getExpr(getRHS(p, ruleNo, 12))
	// uniqueflag is RHS[2]: empty or "UNIQUE".
	unique := strings.EqualFold(strings.TrimSpace(getString(getRHS(p, ruleNo, 2))), "UNIQUE")
	// The sortlist is []OrderByTerm; convert to []IndexColumn for the
	// engine's key population, while retaining the full term expressions
	// (Terms) for DDL validation and ALTER DROP COLUMN checks.
	// A plain identifier becomes a column reference. A numeric literal
	// is a 1-based column position (SQLite allows "CREATE INDEX ON
	// t1(1)" meaning "on the first column"); record it by its numeric
	// text so the engine can resolve it against the table columns.
	// A bare string literal index key is converted to an identifier
	// (SQLite sqlite3StringToId: CREATE INDEX t1('b') indexes column b).
	// Other expressions (e.g. "a+b") are not supported as index keys.
	var cols []sql.IndexColumn
	for _, term := range sortlist {
		switch ex := term.Expr.(type) {
		case *sql.ColumnRef:
			cols = append(cols, sql.IndexColumn{Name: ex.Name, Desc: term.Desc})
		case *sql.StringLit:
			cols = append(cols, sql.IndexColumn{Name: ex.Value, Desc: term.Desc})
		case *sql.NumericLit:
			cols = append(cols, sql.IndexColumn{Name: ex.Value, Desc: term.Desc})
		}
	}
	return &sql.CreateIndexStmt{
		Name:        name,
		Table:       table,
		Columns:     cols,
		Terms:       sortlist,
		Unique:      unique,
		Where:       where,
		IfNotExists: getBool(getRHS(p, ruleNo, 4)),
	}

}

// Rule 242: eidlist_opt ::=
func rule242(ruleNo int, p *Parser) interface{} {
	return ([]string)(nil)

}

// Rule 243: eidlist_opt ::= LP eidlist RP
func rule243(ruleNo int, p *Parser) interface{} {
	return getStringList(getRHS(p, ruleNo, 2))

}

// Rule 244: eidlist ::= eidlist COMMA nm collate sortorder
func rule244(ruleNo int, p *Parser) interface{} {
	acc := getStringList(getRHS(p, ruleNo, 1))
	name := getString(getRHS(p, ruleNo, 3))
	// SQLite rejects a COLLATE clause or ASC/DESC in an identifier list
	// (eidlist), used by CREATE VIEW / CTE column lists and FK reference
	// lists (parserAddExprIdListTerm raises "syntax error after column
	// name"). The FIRST offending column wins (sqlite3ErrorMsg is only
	// honored while zErrMsg is still NULL), so do not overwrite an
	// already-set error. Schema reload (SchemaMode) skips the check: SQLite
	// accepts such lists when re-parsing stored sqlite_schema text.
	if getString(getRHS(p, ruleNo, 4)) != "" || getString(getRHS(p, ruleNo, 5)) != "" {
		if !p.SchemaMode && p.SemanticErr == nil {
			p.SemanticErr = fmt.Errorf("syntax error after column name %q", name)
		}
		return acc
	}
	return append(acc, name)

}

// Rule 245: eidlist ::= nm collate sortorder
func rule245(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	if getString(getRHS(p, ruleNo, 2)) != "" || getString(getRHS(p, ruleNo, 3)) != "" {
		if !p.SchemaMode && p.SemanticErr == nil {
			p.SemanticErr = fmt.Errorf("syntax error after column name %q", name)
		}
		return []string{name}
	}
	return []string{name}

}

func rule248(ruleNo int, p *Parser) interface{} {
	ifExists := getBool(getRHS(p, ruleNo, 3))
	name := getString(getRHS(p, ruleNo, 4))
	return &sql.DropIndexStmt{Name: name, IfExists: ifExists}

}

// Rule 249: cmd ::= VACUUM into_opt
// VACUUM with an optional INTO <expr> clause (into_opt: empty, rule 252,
// or "INTO expr", rule 251). Only a string literal fills Into; any other
// expression node is carried in IntoExpr for the exec-side checks
// (vacuum.c: NULL → "non-text filename", column → resolved first).
func rule249(ruleNo int, p *Parser) interface{} {
	return vacuumStmtFromInto("", getRHS(p, ruleNo, 2))
}

// vacuumStmtFromInto builds a VacuumStmt from an INTO-clause RHS value: a
// string (or string-lit token) is the target filename; any other expression
// node is preserved for the exec-side error reporting.
func vacuumStmtFromInto(schema string, v interface{}) *sql.VacuumStmt {
	vs := &sql.VacuumStmt{Schema: schema}
	switch t := v.(type) {
	case nil:
	case string:
		vs.Into = t
	case *sql.StringLit:
		vs.Into = t.Value
	case sql.Token:
		vs.Into = t.Value
	default:
		if e, ok := v.(sql.Expr); ok {
			vs.IntoExpr = e
		}
	}
	return vs
}

// Rule 250: cmd ::= VACUUM nm vinto
// Schema-qualified VACUUM ("VACUUM main", "VACUUM aux INTO 'f'"): nm is the
// schema name, vinto the optional INTO target (empty when absent).
func rule250(ruleNo int, p *Parser) interface{} {
	return vacuumStmtFromInto(getString(getRHS(p, ruleNo, 2)), getRHS(p, ruleNo, 3))
}

// Rule 251: into_opt ::= INTO expr — the VACUUM INTO target. The raw value
// is returned (string for a literal, expression node otherwise).
func rule251(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 2)
}

// Rule 253: cmd ::= PRAGMA nm dbnm
// The nm token is the pragma name, dbnm the optional schema qualifier.
// When dbnm is present (PRAGMA main.foreign_key_check), nm is the schema
// and dbnm the pragma name (mirroring sqlite3Pragma's swap).
func rule253(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 2))
	schema := getString(getRHS(p, ruleNo, 3))
	if schema != "" {
		name, schema = schema, name
	}
	return &sql.PragmaStmt{
		Name:   name,
		Value:  "",
		Schema: schema,
	}

}

// Rule 254: cmd ::= PRAGMA nm dbnm = pragma_value
func rule254(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 2))
	value := getString(getRHS(p, ruleNo, 5))
	schema := getString(getRHS(p, ruleNo, 3))
	if schema != "" {
		name, schema = schema, name
	}
	return &sql.PragmaStmt{
		Name:   name,
		Value:  value,
		Schema: schema,
	}

}

// Rule 255: cmd ::= PRAGMA nm dbnm LP pragma_value RP
// Rule 257: cmd ::= PRAGMA nm dbnm LP minus_num RP
// rule255 also implements rule(s) [257] (identical action).
func rule255(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 2))
	value := getString(getRHS(p, ruleNo, 5))
	schema := getString(getRHS(p, ruleNo, 3))
	if schema != "" {
		name, schema = schema, name
	}
	return &sql.PragmaStmt{
		Name:   name,
		Value:  value,
		Schema: schema,
	}

}

// Rule 256: cmd ::= PRAGMA nm dbnm EQ minus_num
// (SQLite rule 1717: cmd ::= PRAGMA nm(X) dbnm(Z) EQ minus_num(Y))
// The minus_num value (e.g. -500) becomes the pragma Value.
func rule256(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 2))
	value := getString(getRHS(p, ruleNo, 5))
	schema := getString(getRHS(p, ruleNo, 3))
	if schema != "" {
		name, schema = schema, name
	}
	return &sql.PragmaStmt{
		Name:   name,
		Value:  value,
		Schema: schema,
	}

}

// Rule 259: minus_num ::= MINUS number
// SQLite's minus_num(A) ::= MINUS number(X). {A = X;} — the semantic
// value is the NUMBER token, not the minus. PRAGMA ...(-51) uses this
// rule so the pragma value must be "-51".
func rule259(ruleNo int, p *Parser) interface{} {
	number := getRHS(p, ruleNo, 2)
	if tok, ok := number.(sql.Token); ok {
		return "-" + tok.Value
	}
	if s := getString(number); s != "" {
		return "-" + s
	}
	return "-"

}

// Rule 260: cmd ::= createkw trigger_decl BEGIN trigger_cmd_list END
func rule260(ruleNo int, p *Parser) interface{} {
	decl, _ := getRHS(p, ruleNo, 2).(*triggerDeclInfo)
	if decl == nil {
		return nil
	}
	stmts := getStmtList(getRHS(p, ruleNo, 4))
	return &sql.CreateTriggerStmt{
		Name:        decl.name,
		Table:       decl.table,
		Event:       decl.event,
		Time:        decl.time,
		When:        decl.when,
		Statements:  stmts,
		IfNotExists: decl.ifNotExist,
	}

}

func rule261(ruleNo int, p *Parser) interface{} {
	nm := getString(getRHS(p, ruleNo, 4))
	dbnm := getString(getRHS(p, ruleNo, 5))
	trigName := nm
	if dbnm != "" {
		// "CREATE TRIGGER main.r300": nm is the schema, dbnm the name.
		trigName = nm + "." + dbnm
	}
	return &triggerDeclInfo{
		name:       trigName,
		schema:     dbnm,
		time:       getString(getRHS(p, ruleNo, 6)),
		event:      getString(getRHS(p, ruleNo, 7)),
		table:      getString(getRHS(p, ruleNo, 9)),
		when:       getExpr(getRHS(p, ruleNo, 11)),
		ifNotExist: getBool(getRHS(p, ruleNo, 3)),
	}

}

// Rule 270: trigger_cmd_list ::= trigger_cmd_list trigger_cmd SEMI
func rule270(ruleNo int, p *Parser) interface{} {
	list := getStmtList(getRHS(p, ruleNo, 1))
	stmt := getStmt(getRHS(p, ruleNo, 2))
	if stmt != nil {
		list = append(list, stmt)
	}
	return list

}

// Rule 271: trigger_cmd_list ::= trigger_cmd SEMI
func rule271(ruleNo int, p *Parser) interface{} {
	stmt := getStmt(getRHS(p, ruleNo, 1))
	if stmt == nil {
		return []sql.Stmt(nil)
	}
	return []sql.Stmt{stmt}

}

// Rule 274: trigger_cmd ::= UPDATE orconf nm indexed_opt SET setlist from where_opt
func rule274(ruleNo int, p *Parser) interface{} {
	fromInfo := getFromInfo(getRHS(p, ruleNo, 7))
	stmt := &sql.UpdateStmt{
		Table:       getString(getRHS(p, ruleNo, 3)),
		Assignments: getAssignments(getRHS(p, ruleNo, 6)),
		Where:       getExpr(getRHS(p, ruleNo, 8)),
	}
	if io := getString(getRHS(p, ruleNo, 4)); io != "" {
		stmt.IndexedBy = io
	}
	if fromInfo != nil {
		stmt.From = fromInfo.first
		stmt.FromJoins = fromInfo.joins
	}
	return stmt

	// Rule 275: trigger_cmd ::= with insert_cmd INTO nm idlist_opt select upsert
}

func rule275(ruleNo int, p *Parser) interface{} {
	cmd := getString(getRHS(p, ruleNo, 2))
	table := getString(getRHS(p, ruleNo, 4))
	columns := getStringList(getRHS(p, ruleNo, 5))
	sel := getSelectStmt(getRHS(p, ruleNo, 6))
	var values [][]sql.Expr
	if sel != nil && sel.ValuesChain {
		values = valuesFromSelect(sel)
		sel = nil
	}
	stmt := &sql.InsertStmt{
		Table:      table,
		Columns:    columns,
		Values:     values,
		Select:     sel,
		IsReplace:  strings.EqualFold(cmd, "REPLACE"),
		OrIgnore:   strings.EqualFold(cmd, "IGNORE"),
		OrFail:     strings.EqualFold(cmd, "FAIL"),
		OrConflict: strings.ToUpper(cmd),
	}
	// The upsert nonterminal (RHS 7) carries an ON CONFLICT clause.
	if uv := getUpsertVal(getRHS(p, ruleNo, 7)); uv != nil {
		stmt.OnConflict = uv.onConflict
		if len(uv.returning) > 0 {
			stmt.HasReturning = true
			stmt.Returning = foldReturning(uv.returning)
		}
	}
	return stmt

}

// Rule 276: trigger_cmd ::= DELETE FROM xfullname tridxby where_opt scanpt
func rule276(ruleNo int, p *Parser) interface{} {
	stmt := &sql.DeleteStmt{
		Table: getString(getRHS(p, ruleNo, 3)),
		Where: getExpr(getRHS(p, ruleNo, 5)),
	}
	if io := getString(getRHS(p, ruleNo, 4)); io != "" {
		stmt.IndexedBy = io
	}
	return stmt

}

// Rule 277: trigger_cmd ::= scanpt select scanpt
// A bare SELECT as a trigger body. scanpt markers are empty (nil).
func rule277(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 2)

}

// Rule 278: expr ::= RAISE LP IGNORE RP
func rule278(ruleNo int, p *Parser) interface{} {
	return &sql.RaiseExpr{Kind: "IGNORE"}

}

func rule279(ruleNo int, p *Parser) interface{} {
	return &sql.RaiseExpr{
		Kind:    getString(getRHS(p, ruleNo, 3)),
		Message: getExpr(getRHS(p, ruleNo, 5)),
	}

}

// Rules 280-282: raisetype ::= ROLLBACK | ABORT | FAIL
func rule280(ruleNo int, p *Parser) interface{} {
	return "ROLLBACK"
}
