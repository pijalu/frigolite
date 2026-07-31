// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar. This replaces the hand-written
// recursive-descent parser in internal/sql/parser.go.

package parse

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// ParseSQL parses a SQL string using the go-lemon generated LALR(1) parser.
// Returns a list of statements compatible with Frigolite's AST types.
func ParseSQL(input string) ([]sql.Stmt, error) {
	// Append trailing semicolon if missing — the LALR grammar requires
	// SEMI as a statement terminator (ecmd ::= cmdx SEMI).
	input = strings.TrimRight(input, " \t\r\n")
	if input != "" && input[len(input)-1] != ';' {
		input += ";"
	}

	tables := GetParseTables()
	parser := NewParser(tables)

	tok := sql.NewTokenizer(input)
	var stmts []sql.Stmt
	var pendingStmt sql.Stmt

	// OnReduce callback: handle grammar rule reductions
	parser.OnReduce(func(ruleNo int, p *Parser, lookahead int, lookaheadToken interface{}) {
		t := p.tables
		top := p.pos
		nrhs := t.RuleInfoNRhs[ruleNo]
		size := -nrhs

		result := handleRule(ruleNo, p, lookahead, lookaheadToken)

		// Default: pass through first RHS value if handler returned nil
		// (Only for non-empty rules - empty rules have no RHS values)
		if result == nil && size > 0 {
			result = getRHS(p, ruleNo, 1)
		}

		// Collect completed statements when the statement-root rules fire
		if s, ok := result.(sql.Stmt); ok {
			pendingStmt = s
			// Collect at ecmd rules (after SEMI is consumed) to handle
			// multi-statement input. Rules 352 (ecmd ::= cmdx SEMI) and
			// 353 (ecmd ::= explain cmdx SEMI) complete a statement.
			if ruleNo == 352 || ruleNo == 353 {
				stmts = append(stmts, s)
			}
		}

		// Set the LHS value on the stack
		// For empty rules, LHS overwrites current top position
		lhsSlot := top
		if size > 0 {
			lhsSlot = top - size + 1
		}
		p.stack[lhsSlot].Minor = result
	})

	// Feed tokens until EOF
	for {
		tok := tok.Next()
		code := tokenCode(int(tok.Type), tok.Value)
		if code < 0 {
			parser.Finalize()
			return nil, fmt.Errorf("unexpected token: %s", tok.Value)
		}

		result := parser.Parse(code, tok)
		if result == ParseError {
			parser.Finalize()
			return nil, fmt.Errorf("syntax error near: %s", tok.Value)
		}
		if result == ParseAccept && code == 0 { // EOF
			break
		}
	}

	parser.Finalize()
	if parser.SemanticErr != nil {
		return nil, parser.SemanticErr
	}
	if len(stmts) == 0 && pendingStmt != nil {
		// No statements were collected via ecmd (no SEMI in input).
		// Use pendingStmt as a fallback.
		stmts = append(stmts, pendingStmt)
	}
	if len(stmts) == 0 {
		return nil, fmt.Errorf("no statements parsed")
	}
	return stmts, nil
}

// getRHS returns the Nth RHS symbol value (1-indexed) for the current rule.
// In C lemon convention: yymsp[-(N-n)] where N = RHS count.
// In Go stack: stack[pos - size + n] where size = -RuleInfoNRhs[ruleNo].
func getRHS(p *Parser, ruleNo, n int) interface{} {
	t := p.tables
	size := -t.RuleInfoNRhs[ruleNo]
	return p.stack[p.pos-size+n].Minor
}

// handleRule implements the action code for each grammar rule.
// Returns the semantic value for the LHS symbol.
func handleRule(ruleNo int, p *Parser, lookahead int, lookaheadToken interface{}) interface{} {
	switch ruleNo {
	// Rule 0: explain ::= EXPLAIN
	case 0:
		return true

	// Rule 2: cmdx ::= cmd
	case 2:
		return getRHS(p, ruleNo, 1)

	// Rule 3: cmd ::= BEGIN transtype trans_opt
	case 3:
		return &sql.BeginStmt{}

	// Rule 4: transtype ::= (empty)
	case 4:
		return nil

	// Rule 8: cmd ::= COMMIT|END trans_opt
	case 8:
		return &sql.CommitStmt{}

	// Rule 9: cmd ::= ROLLBACK trans_opt
	case 9:
		return &sql.RollbackStmt{}

	// Rule 13: create_table ::= createkw temp TABLE ifnotexists nm dbnm
	case 13:
		name := getString(getRHS(p, ruleNo, 5))
		schema := getString(getRHS(p, ruleNo, 6)) // dbnm - optional schema
		if schema != "" {
			name = schema + "." + name
		}
		return &sql.CreateTableStmt{
			Name:        name,
			IfNotExists: getBool(getRHS(p, ruleNo, 4)),
			Columns:     nil, // will be filled by create_table_args
		}

	// Rule 14: createkw ::= CREATE
	case 14:
		return nil

	// Rule 15: ifnotexists ::=
	case 15:
		return false

	// Rule 16: ifnotexists ::= IF NOT EXISTS
	case 16:
		return true

	// Rule 17: temp ::= TEMP
	case 17:
		return true

	// Rule 18: temp ::=
	case 18:
		return false

	// Rule 19: create_table_args ::= LP columnlist conslist_opt RP table_option_set
	case 19:
		// This rule produces columns from a column definition list.
		// The create_table value isn't available here; rule 359 combines them.
		cols := getColumnList(getRHS(p, ruleNo, 2))
		if len(cols) > 0 {
			return cols
		}
		return nil

	// Rule 20: create_table_args ::= AS select
	case 20:
		sel := getSelectStmt(getRHS(p, ruleNo, 2))
		if sel != nil {
			// Wrap in CreateTableStmt with AS SELECT
			createStmt := &sql.CreateTableStmt{
				AsSelect: sel,
			}
			return createStmt
		}
		return nil

	// Rule 25: columnname ::= nm typetoken
	case 25:
		name := getString(getRHS(p, ruleNo, 1))
		typeName := getString(getRHS(p, ruleNo, 2))
		return sql.ColumnDef{Name: name, Type: typeName}

	// Rule 26: typetoken ::=
	case 26:
		return ""

	// Rule 79: cmd ::= DROP TABLE ifexists fullname
	case 79:
		ifExists := getBool(getRHS(p, ruleNo, 3))
		name := getString(getRHS(p, ruleNo, 4))
		return &sql.DropTableStmt{Name: name, IfExists: ifExists}

	// Rule 80: ifexists ::= IF EXISTS
	case 80:
		return true

	// Rule 81: ifexists ::=
	case 81:
		return false

	// Rule 82: cmd ::= DROP TABLE ifexists fullname (with dbnm handled in fullname)
	// (falls through to default pass-through if not matched)

	// Rule 83: cmd ::= DROP VIEW ifexists fullname
	case 83:
		ifExists := getBool(getRHS(p, ruleNo, 3))
		name := getString(getRHS(p, ruleNo, 4))
		return &sql.DropViewStmt{Name: name, IfExists: ifExists}

	// Rule 84: cmd ::= select
	case 84:
		return getSelectStmt(getRHS(p, ruleNo, 1))

	// Rule 85: select ::= WITH wqlist selectnowith
	case 85:
		return checkCompoundSelect(p, getSelectStmt(getRHS(p, ruleNo, 3)))

	// Rule 86: select ::= WITH RECURSIVE wqlist selectnowith
	case 86:
		return checkCompoundSelect(p, getSelectStmt(getRHS(p, ruleNo, 4)))

	// Rule 87: select ::= selectnowith
	case 87:
		return checkCompoundSelect(p, getSelectStmt(getRHS(p, ruleNo, 1)))

	// Rule 88: selectnowith ::= selectnowith multiselect_op oneselect
	case 88:
		left := getSelectStmt(getRHS(p, ruleNo, 1))
		right := getSelectStmt(getRHS(p, ruleNo, 3))
		if left == nil || right == nil {
			return left
		}
		// multiselect_op = getRHS(p, ruleNo, 2) - returns (SetOp, bool for ALL)
		op := getSetOp(getRHS(p, ruleNo, 2))
		// Walk to the end of the chain
		last := left
		for last.Union != nil {
			last = last.Union
		}
		last.Union = right
		last.SetOp = op
		return left

	// Rule 89: multiselect_op ::= UNION
	case 89:
		return setOpResult{Op: sql.SetUnion, All: false}

	// Rule 90: multiselect_op ::= UNION ALL
	case 90:
		return setOpResult{Op: sql.SetUnion, All: true}

	// Rule 91: multiselect_op ::= EXCEPT|INTERSECT
	case 91:
		// Distinguish EXCEPT vs INTERSECT from the RHS token value. The
		// lookahead at reduce time is the NEXT token (e.g. SELECT), not the
		// operator being reduced, so it cannot be used to tell them apart.
		op := sql.SetExcept // default
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok && strings.EqualFold(tok.Value, "INTERSECT") {
			op = sql.SetIntersect
		}
		return setOpResult{Op: op, All: false}

	// Rule 92: oneselect ::= SELECT distinct selcollist from where_opt groupby_opt having_opt orderby_opt limit_opt
	case 92:
		distinct := getBool(getRHS(p, ruleNo, 2))
		cols := getSelectColumns(getRHS(p, ruleNo, 3))
		from, joins := fromValue(getRHS(p, ruleNo, 4))
		where := getExpr(getRHS(p, ruleNo, 5))
		groupBy := getExprList(getRHS(p, ruleNo, 6))
		having := getExpr(getRHS(p, ruleNo, 7))
		orderBy := getOrderByList(getRHS(p, ruleNo, 8))
		limit := getExpr(getRHS(p, ruleNo, 9))

		return &sql.SelectStmt{
			Distinct: distinct,
			Columns:  cols,
			From:     from,
			Joins:    joins,
			Where:    where,
			GroupBy:  groupBy,
			Having:   having,
			OrderBy:  orderBy,
			Limit:    limit,
		}

	// Rule 93: oneselect ::= SELECT distinct selcollist from where_opt groupby_opt having_opt window_clause orderby_opt limit_opt
	case 93:
		// Same as 92 but with window_clause before orderby_opt
		distinct := getBool(getRHS(p, ruleNo, 2))
		cols := getSelectColumns(getRHS(p, ruleNo, 3))
		from, joins := fromValue(getRHS(p, ruleNo, 4))
		where := getExpr(getRHS(p, ruleNo, 5))
		groupBy := getExprList(getRHS(p, ruleNo, 6))
		having := getExpr(getRHS(p, ruleNo, 7))
		// Skip window_clause (8) for now
		orderBy := getOrderByList(getRHS(p, ruleNo, 9))
		limit := getExpr(getRHS(p, ruleNo, 10))

		return &sql.SelectStmt{
			Distinct: distinct,
			Columns:  cols,
			From:     from,
			Joins:    joins,
			Where:    where,
			GroupBy:  groupBy,
			Having:   having,
			OrderBy:  orderBy,
			Limit:    limit,
		}

	// Rule 94: values ::= VALUES LP nexprlist RP
	case 94:
		exprs := getExprList(getRHS(p, ruleNo, 3))
		cols := make([]sql.SelectColumn, len(exprs))
		for i, expr := range exprs {
			cols[i] = sql.SelectColumn{Expr: expr}
		}
		return &sql.SelectStmt{
			Columns: cols,
		}

	// Rule 95: oneselect ::= mvalues
	case 95:
		return getRHS(p, ruleNo, 1)

	// Rule 96: mvalues ::= values COMMA LP nexprlist RP
	case 96:
		first := getSelectStmt(getRHS(p, ruleNo, 1))
		secondExprs := getExprList(getRHS(p, ruleNo, 4))
		secondCols := make([]sql.SelectColumn, len(secondExprs))
		for i, expr := range secondExprs {
			secondCols[i] = sql.SelectColumn{Expr: expr}
		}
		second := &sql.SelectStmt{Columns: secondCols}
		if first != nil {
			first.Union = second
			first.SetOp = sql.SetUnion
			first.UnionAll = true
		}
		return first

	// Rule 97: mvalues ::= mvalues COMMA LP nexprlist RP
	case 97:
		acc := getSelectStmt(getRHS(p, ruleNo, 1))
		exprs := getExprList(getRHS(p, ruleNo, 4))
		cols := make([]sql.SelectColumn, len(exprs))
		for i, expr := range exprs {
			cols[i] = sql.SelectColumn{Expr: expr}
		}
		last := &sql.SelectStmt{Columns: cols}
		if acc != nil {
			cur := acc
			for cur.Union != nil {
				cur = cur.Union
			}
			cur.Union = last
			cur.SetOp = sql.SetUnion
			cur.UnionAll = true
		}
		return acc

	// Rule 98: distinct ::= DISTINCT
	case 98:
		return true

	// Rule 99: distinct ::= ALL
	case 99:
		return false

	// Rule 100: distinct ::=
	case 100:
		return false

	// Rule 102: selcollist ::= sclp scanpt expr scanpt as
	case 102:
		expr := getExpr(getRHS(p, ruleNo, 3))
		alias := getString(getRHS(p, ruleNo, 5))

		// Prepend the accumulated list from sclp (RHS 1). sclp holds the
		// columns collected before the COMMA (via rule 382).
		prev := getSelectColumns(getRHS(p, ruleNo, 1))
		return append(prev, sql.SelectColumn{Expr: expr, As: alias})

	// Rule 103: selcollist ::= sclp scanpt STAR
	case 103:
		prev := getSelectColumns(getRHS(p, ruleNo, 1))
		return append(prev, sql.SelectColumn{Expr: &sql.ColumnRef{Name: "*"}})

	// Rule 104: selcollist ::= sclp scanpt nm DOT STAR
	case 104:
		tbl := getString(getRHS(p, ruleNo, 3))
		prev := getSelectColumns(getRHS(p, ruleNo, 1))
		return append(prev, sql.SelectColumn{Expr: &sql.ColumnRef{Table: tbl, Name: "*"}})

	// Rule 105: as ::= AS nm
	case 105:
		return getString(getRHS(p, ruleNo, 2))

	// Rule 106: as ::=
	case 106:
		return ""

	// Rule 107: from ::=
	case 107:
		return sql.TableRef{}

	// Rule 108: from ::= FROM seltablist
	case 108:
		return getRHS(p, ruleNo, 2)

	// Rule 109: stl_prefix ::= seltablist joinop
	// Combine the accumulated seltablist with the join operator that follows.
	// The joinop (COMMA or JOIN) marks how the NEXT table will be joined.
	case 109:
		acc := getSeltablist(getRHS(p, ruleNo, 1))
		op := getJoinOp(getRHS(p, ruleNo, 2))
		acc.PendingOp = op
		return acc

	// Rule 110: stl_prefix ::=
	case 110:
		return &seltablistAcc{}

	// Rule 111: seltablist ::= stl_prefix nm dbnm as on_using
	case 111:
		return appendSeltablistTable(p, ruleNo, 2, 3, 4)

	// Rule 112: seltablist ::= stl_prefix nm dbnm as indexed_by on_using
	case 112:
		return appendSeltablistTable(p, ruleNo, 2, 3, 4)

	// Rule 114: seltablist ::= stl_prefix LP select RP as on_using
	case 114:
		acc := getSeltablist(getRHS(p, ruleNo, 1))
		sel := getSelectStmt(getRHS(p, ruleNo, 3))
		alias := getString(getRHS(p, ruleNo, 5))
		ref := sql.TableRef{Subquery: sel, As: alias}
		return acc.appendTable(ref)

	// Rule 115: seltablist ::= stl_prefix LP seltablist RP as on_using
	// Parenthesized table list: FROM (t1) or FROM (t1, t2).
	// The inner seltablist is a single table (or the first of a comma list);
	// for a bare parenthesized name we use that name as the table ref.
	case 115:
		acc := getSeltablist(getRHS(p, ruleNo, 1))
		inner := getSeltablist(getRHS(p, ruleNo, 3))
		alias := getString(getRHS(p, ruleNo, 5))
		ref := inner.firstTable()
		if alias != "" {
			ref.As = alias
		}
		// A parenthesized comma list (t1, t2) contributes its joins.
		acc = acc.appendTable(ref)
		for _, j := range inner.Joins {
			acc = acc.appendJoin(j)
		}
		return acc

	// Rule 116: dbnm ::=
	case 116:
		return ""

	// Rule 117: dbnm ::= DOT nm
	case 117:
		return getString(getRHS(p, ruleNo, 2))

	// Rule 118: fullname ::= nm
	case 118:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 119: fullname ::= nm DOT nm
	case 119:
		a := getString(getRHS(p, ruleNo, 1))
		b := getString(getRHS(p, ruleNo, 3))
		return a + "." + b

	// Rule 124: joinop ::= COMMA|JOIN
	case 124:
		return joinOp{Comma: true}

	// Rule 122: joinop ::= JOIN_KW JOIN
	case 122:
		return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1))}

	// Rule 123: joinop ::= JOIN_KW nm JOIN
	case 123:
		return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1))}

	// Rule 125: joinop ::= JOIN_KW OUTER JOIN
	case 125:
		return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1)), Outer: true}

	// Rule 126: joinop ::= JOIN_KW nm OUTER JOIN
	case 126:
		return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1)), Outer: true}

	// Rule 127: joinop ::= JOIN_KW nm OUTER JOIN
	case 127:
		return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1)), Outer: true}

	// Rule 128: joinop ::= JOIN_KW nm JOIN
	case 128:
		return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1))}

	// Rule 129: joinop ::= JOIN_KW nm nm JOIN
	case 129:
		return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1))}

	// Rule 130: on_using ::=
	case 130:
		return nil

	// Rule 134: orderby_opt ::=
	case 134:
		return ([]sql.OrderByTerm)(nil)

	// Rule 135: orderby_opt ::= ORDER BY sortlist
	case 135:
		return getOrderByList(getRHS(p, ruleNo, 3))

	// Rule 136: sortlist ::= sortlist COMMA expr sortorder nulls
	case 136:
		acc := getOrderByList(getRHS(p, ruleNo, 1))
		expr := getExpr(getRHS(p, ruleNo, 3))
		desc := getBool(getRHS(p, ruleNo, 4))
		return append(acc, sql.OrderByTerm{Expr: expr, Desc: desc})

	// Rule 137: sortlist ::= expr sortorder nulls
	case 137:
		expr := getExpr(getRHS(p, ruleNo, 1))
		desc := getBool(getRHS(p, ruleNo, 2))
		return []sql.OrderByTerm{{Expr: expr, Desc: desc}}

	// Rule 138: sortorder ::= ASC
	case 138:
		return false

	// Rule 139: sortorder ::= DESC
	case 139:
		return true

	// Rule 140: sortorder ::=
	case 140:
		return false

	// Rule 141: nulls ::= NULLS FIRST
	case 141:
		return nil

	// Rule 142: nulls ::= NULLS LAST
	case 142:
		return nil

	// Rule 143: nulls ::=
	case 143:
		return nil

	// Rule 144: groupby_opt ::=
	case 144:
		return ([]sql.Expr)(nil)

	// Rule 145: groupby_opt ::= GROUP BY nexprlist
	case 145:
		return getExprList(getRHS(p, ruleNo, 3))

	// Rule 146: having_opt ::=
	case 146:
		return nil

	// Rule 147: having_opt ::= HAVING expr
	case 147:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 148: limit_opt ::=
	case 148:
		return nil

	// Rule 149: limit_opt ::= LIMIT expr
	case 149:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 150: limit_opt ::= LIMIT expr OFFSET expr
	case 150:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 151: limit_opt ::= LIMIT expr COMMA expr
	case 151:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 152: cmd ::= with DELETE FROM xfullname indexed_opt where_opt_ret
	case 152:
		tbl := getString(getRHS(p, ruleNo, 4))
		where := getExpr(getRHS(p, ruleNo, 6))
		return &sql.DeleteStmt{
			Table: tbl,
			Where: where,
		}

	// Rule 159: cmd ::= with UPDATE orconf xfullname indexed_opt SET setlist from where_opt
	case 159:
		tbl := getString(getRHS(p, ruleNo, 4))
		setlist := getAssignments(getRHS(p, ruleNo, 7))
		where := getExpr(getRHS(p, ruleNo, 9))
		return &sql.UpdateStmt{
			Table:       tbl,
			Assignments: setlist,
			Where:       where,
		}

	// Rule 160: setlist ::= setlist COMMA nm EQ expr
	case 160:
		acc := getAssignments(getRHS(p, ruleNo, 1))
		col := getString(getRHS(p, ruleNo, 3))
		val := getExpr(getRHS(p, ruleNo, 5))
		return append(acc, sql.Assignment{Column: col, Value: val})

	// Rule 162: setlist ::= nm EQ expr
	case 162:
		col := getString(getRHS(p, ruleNo, 1))
		val := getExpr(getRHS(p, ruleNo, 3))
		return []sql.Assignment{{Column: col, Value: val}}

	// Rule 153: where_opt ::=
	case 153:
		return nil

	// Rule 154: where_opt ::= WHERE expr
	case 154:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 155: where_opt_ret ::=
	case 155:
		return nil

	// Rule 156: where_opt_ret ::= WHERE expr
	case 156:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 164: cmd ::= with insert_cmd INTO xfullname idlist_opt select upsert
	case 164:
		table := getString(getRHS(p, ruleNo, 4))
		columns := getStringList(getRHS(p, ruleNo, 5))
		sel := getSelectStmt(getRHS(p, ruleNo, 6))
		// Check if it's a VALUES insert or SELECT insert
		if sel != nil && len(sel.Columns) == 0 {
			// This is a VALUES insert - need to extract values from parser context
			// For now, we'll handle this differently
		}
		return &sql.InsertStmt{
			Table:   table,
			Columns: columns,
			Select:  sel,
		}

	// Rule 165: cmd ::= with insert_cmd INTO xfullname idlist_opt DEFAULT VALUES returning
	case 165:
		table := getString(getRHS(p, ruleNo, 4))
		columns := getStringList(getRHS(p, ruleNo, 5))
		return &sql.InsertStmt{
			Table:   table,
			Columns: columns,
		}

	// Rule 166: upsert ::=
	case 166:
		return nil

	// Rule 170: upsert ::= ON CONFLICT DO NOTHING returning
	case 170:
		return &sql.OnConflictClause{
			Action: sql.ConflictDoNothing,
		}

	// Rule 173: insert_cmd ::= INSERT orconf
	case 173:
		return "INSERT"

	// Rule 174: insert_cmd ::= REPLACE
	case 174:
		return "REPLACE"

	// Rule 175: idlist_opt ::=
	case 175:
		return ([]string)(nil)

	// Rule 176: idlist_opt ::= LP idlist RP
	case 176:
		return getStringList(getRHS(p, ruleNo, 2))

	// Rule 177: idlist ::= idlist COMMA nm
	case 177:
		acc := getStringList(getRHS(p, ruleNo, 1))
		return append(acc, getString(getRHS(p, ruleNo, 3)))

	// Rule 178: idlist ::= nm
	case 178:
		return []string{getString(getRHS(p, ruleNo, 1))}

	// Rule 179: expr ::= LP expr RP
	case 179:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 180: expr ::= ID|INDEXED|JOIN_KW (column reference)
	case 180:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			// SQLite DQS: an empty double-quoted identifier "" is a string
			// literal, not a column reference.
			if tok.QuotedIdent && tok.Value == "" {
				return &sql.StringLit{Value: ""}
			}
			return &sql.ColumnRef{Name: tok.Value}
		}
		if s, ok := getRHS(p, ruleNo, 1).(string); ok {
			return &sql.ColumnRef{Name: s}
		}
		return &sql.ColumnRef{Name: fmt.Sprintf("%v", getRHS(p, ruleNo, 1))}

	// Rule 181: expr ::= nm DOT nm (schema.table)
	case 181:
		schema := getString(getRHS(p, ruleNo, 1))
		col := getString(getRHS(p, ruleNo, 3))
		return &sql.ColumnRef{Table: schema, Name: col}

	// Rule 183: term ::= NULL|FLOAT|BLOB
	case 183:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			if strings.EqualFold(tok.Value, "NULL") {
				return &sql.NullLit{}
			}
			return &sql.NumericLit{Value: tok.Value}
		}
		return &sql.NullLit{}

	// Rule 184: term ::= STRING
	case 184:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return &sql.StringLit{Value: tok.Value}
		}
		if s, ok := getRHS(p, ruleNo, 1).(string); ok {
			return &sql.StringLit{Value: s}
		}
		return &sql.StringLit{}

	// Rule 185: term ::= INTEGER
	case 185:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return &sql.NumericLit{Value: tok.Value}
		}
		if s, ok := getRHS(p, ruleNo, 1).(string); ok {
			return &sql.NumericLit{Value: s}
		}
		return &sql.NumericLit{}

	// Rule 187: expr ::= expr COLLATE ID|STRING
	case 187:
		expr := getExpr(getRHS(p, ruleNo, 1))
		collation := getString(getRHS(p, ruleNo, 3))
		// COLLATE is an operator that wraps the expression
		return &sql.BinaryOp{
			Left:     expr,
			Operator: "COLLATE",
			Right:    &sql.StringLit{Value: collation},
		}

	// Rule 189: expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist RP (function call)
	case 189:
		name := getString(getRHS(p, ruleNo, 1))
		distinct := getBool(getRHS(p, ruleNo, 3))
		args := getExprList(getRHS(p, ruleNo, 4))
		return &sql.FuncCall{
			Name:     name,
			Args:     args,
			Distinct: distinct,
		}

	// Rule 191: expr ::= ID|INDEXED|JOIN_KW LP STAR RP (function(star))
	case 191:
		name := getString(getRHS(p, ruleNo, 1))
		return &sql.FuncCall{
			Name: name,
			Args: nil, // * is represented as nil args
		}

	// Rule 197: expr ::= expr AND expr
	case 197:
		return &sql.BinaryOp{
			Left:     getExpr(getRHS(p, ruleNo, 1)),
			Operator: "AND",
			Right:    getExpr(getRHS(p, ruleNo, 3)),
		}

	// Rule 198: expr ::= expr OR expr
	case 198:
		return &sql.BinaryOp{
			Left:     getExpr(getRHS(p, ruleNo, 1)),
			Operator: "OR",
			Right:    getExpr(getRHS(p, ruleNo, 3)),
		}

	// Rule 199: expr ::= expr LT|GT|GE|LE expr
	case 199:
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

	// Rule 200: expr ::= expr EQ|NE expr
	case 200:
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

	// Rule 201: expr ::= expr BITAND|BITOR|LSHIFT|RSHIFT expr
	case 201:
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

	// Rule 202: expr ::= expr PLUS|MINUS expr
	case 202:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		op := "+"
		if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok && tok.Value == "-" {
			op = "-"
		}
		return &sql.BinaryOp{Left: left, Operator: op, Right: right}

	// Rule 203: expr ::= expr STAR|SLASH|REM expr
	case 203:
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

	// Rule 204: expr ::= expr CONCAT expr
	case 204:
		return &sql.BinaryOp{
			Left:     getExpr(getRHS(p, ruleNo, 1)),
			Operator: "||",
			Right:    getExpr(getRHS(p, ruleNo, 3)),
		}

	// Rule 206: expr ::= expr likeop expr (LIKE/MATCH)
	case 206:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		op := "LIKE"
		if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok {
			u := strings.ToUpper(tok.Value)
			if u == "MATCH" {
				op = "MATCH"
			}
		}
		return &sql.BinaryOp{Left: left, Operator: op, Right: right}

	// Rule 207: expr ::= expr likeop expr ESCAPE expr
	case 207:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		escape := getExpr(getRHS(p, ruleNo, 5))
		return &sql.BinaryOp{
			Left:     left,
			Operator: "LIKE",
			Right:    right,
			Escape:   fmt.Sprintf("%v", escape),
		}

	// Rule 208: expr ::= expr ISNULL|NOTNULL
	case 208:
		operand := getExpr(getRHS(p, ruleNo, 1))
		if lookahead == TK_NOTNULL {
			return &sql.IsNotNull{Operand: operand}
		}
		return &sql.IsNull{Operand: operand}

	// Rule 210: expr ::= expr IS expr
	case 210:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		// IS TRUE / IS FALSE predicates.
		if name, ok := boolLitName(right); ok {
			if name == "TRUE" {
				return &sql.IsTrue{Operand: left}
			}
			return &sql.IsFalse{Operand: left}
		}
		return &sql.BinaryOp{Left: left, Operator: "IS", Right: right}

	// Rule 211: expr ::= expr IS NOT expr
	case 211:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 4))
		// IS NOT TRUE / IS NOT FALSE predicates.
		if name, ok := boolLitName(right); ok {
			if name == "TRUE" {
				return &sql.IsTrue{Operand: left, Negated: true}
			}
			return &sql.IsFalse{Operand: left, Negated: true}
		}
		return &sql.BinaryOp{Left: left, Operator: "IS NOT", Right: right}

	// Rule 212: expr ::= expr IS DISTINCT FROM expr
	case 212:
		return &sql.IsDistinctFrom{
			Left:  getExpr(getRHS(p, ruleNo, 1)),
			Right: getExpr(getRHS(p, ruleNo, 6)),
		}

	// Rule 213: expr ::= expr IS NOT DISTINCT FROM expr
	case 213:
		return &sql.IsNotDistinctFrom{
			Left:  getExpr(getRHS(p, ruleNo, 1)),
			Right: getExpr(getRHS(p, ruleNo, 7)),
		}

	// Rule 214: expr ::= NOT expr
	case 214:
		return &sql.UnaryOp{
			Operand:  getExpr(getRHS(p, ruleNo, 2)),
			Operator: "NOT",
		}

	// Rule 215: expr ::= BITNOT expr
	case 215:
		return &sql.UnaryOp{
			Operand:  getExpr(getRHS(p, ruleNo, 2)),
			Operator: "~",
		}

	// Rule 216: expr ::= PLUS|MINUS expr (unary)
	case 216:
		operand := getExpr(getRHS(p, ruleNo, 2))
		if lookahead == TK_MINUS {
			return &sql.UnaryOp{Operand: operand, Operator: "-"}
		}
		// Unary + is a no-op
		return operand

	// Rule 220: expr ::= expr between_op expr AND expr
	case 220:
		negated := (lookahead == TK_NOT)
		return &sql.Between{
			Operand: getExpr(getRHS(p, ruleNo, 1)),
			Low:     getExpr(getRHS(p, ruleNo, 3)),
			High:    getExpr(getRHS(p, ruleNo, 5)),
			Negated: negated,
		}

	// Rule 221: in_op ::= IN
	case 221:
		return false

	// Rule 222: in_op ::= NOT IN
	case 222:
		return true

	// Rule 223: expr ::= expr in_op LP exprlist RP
	case 223:
		negated := getBool(getRHS(p, ruleNo, 2))
		return &sql.InList{
			Operand: getExpr(getRHS(p, ruleNo, 1)),
			List:    getExprList(getRHS(p, ruleNo, 4)),
			Negated: negated,
		}

	// Rule 224: expr ::= LP select RP
	case 224:
		return &sql.Subquery{
			Select: getSelectStmt(getRHS(p, ruleNo, 2)),
		}

	// Rule 225: expr ::= expr in_op LP select RP
	case 225:
		negated := getBool(getRHS(p, ruleNo, 2))
		return &sql.InList{
			Operand: getExpr(getRHS(p, ruleNo, 1)),
			List:    []sql.Expr{&sql.Subquery{Select: getSelectStmt(getRHS(p, ruleNo, 4))}},
			Negated: negated,
		}

	// Rule 227: expr ::= EXISTS LP select RP
	case 227:
		return &sql.ExistsExpr{
			Select:  getSelectStmt(getRHS(p, ruleNo, 3)),
			Negated: false,
		}

	// Rule 228: expr ::= CASE case_operand case_exprlist case_else END
	case 228:
		operand := getExpr(getRHS(p, ruleNo, 2))
		whenList := getWhenClauses(getRHS(p, ruleNo, 3))
		elseExpr := getExpr(getRHS(p, ruleNo, 4))
		return &sql.CaseExpr{
			Operand: operand,
			Whens:   whenList,
			Else:    elseExpr,
		}

	// Rule 229: case_exprlist ::= case_exprlist WHEN expr THEN expr
	case 229:
		acc := getWhenClauses(getRHS(p, ruleNo, 1))
		whenExpr := getExpr(getRHS(p, ruleNo, 3))
		thenExpr := getExpr(getRHS(p, ruleNo, 5))
		return append(acc, sql.WhenClause{When: whenExpr, Then: thenExpr})

	// Rule 230: case_exprlist ::= WHEN expr THEN expr
	case 230:
		whenExpr := getExpr(getRHS(p, ruleNo, 2))
		thenExpr := getExpr(getRHS(p, ruleNo, 4))
		return []sql.WhenClause{{When: whenExpr, Then: thenExpr}}

	// Rule 231: case_else ::= ELSE expr
	case 231:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 232: case_else ::=
	case 232:
		return nil

	// Rule 233: case_operand ::=
	case 233:
		return nil

	// Rule 234: exprlist ::=
	case 234:
		return ([]sql.Expr)(nil)

	// Rule 235: nexprlist ::= nexprlist COMMA expr
	case 235:
		acc := getExprList(getRHS(p, ruleNo, 1))
		return append(acc, getExpr(getRHS(p, ruleNo, 3)))

	// Rule 236: nexprlist ::= expr
	case 236:
		return []sql.Expr{getExpr(getRHS(p, ruleNo, 1))}

	// Rule 237: paren_exprlist ::=
	case 237:
		return ([]sql.Expr)(nil)

	// Rule 238: paren_exprlist ::= LP exprlist RP
	case 238:
		return getExprList(getRHS(p, ruleNo, 2))

	// Rule 239: cmd ::= createkw uniqueflag INDEX ifnotexists nm dbnm ON nm LP sortlist RP where_opt
	case 239:
		name := getString(getRHS(p, ruleNo, 5))
		table := getString(getRHS(p, ruleNo, 8))
		sortlist := getOrderByList(getRHS(p, ruleNo, 10))
		where := getExpr(getRHS(p, ruleNo, 12))
		// The sortlist is []OrderByTerm; convert to []IndexColumn.
		var cols []sql.IndexColumn
		for _, term := range sortlist {
			if ref, ok := term.Expr.(*sql.ColumnRef); ok {
				cols = append(cols, sql.IndexColumn{Name: ref.Name, Desc: term.Desc})
			}
		}
		return &sql.CreateIndexStmt{
			Name:    name,
			Table:   table,
			Columns: cols,
			Where:   where,
		}

	// Rule 248: cmd ::= DROP INDEX ifexists fullname
	case 248:
		ifExists := getBool(getRHS(p, ruleNo, 3))
		name := getString(getRHS(p, ruleNo, 4))
		return &sql.DropIndexStmt{Name: name, IfExists: ifExists}

	// Rule 253: cmd ::= PRAGMA nm dbnm
	case 253:
		name := getString(getRHS(p, ruleNo, 2))
		return &sql.PragmaStmt{
			Name:  name,
			Value: "",
		}

	// Rule 254: cmd ::= PRAGMA nm dbnm = pragma_value
	case 254:
		name := getString(getRHS(p, ruleNo, 2))
		value := getString(getRHS(p, ruleNo, 5))
		return &sql.PragmaStmt{
			Name:  name,
			Value: value,
		}

	// Rule 260: cmd ::= createkw trigger_decl BEGIN trigger_cmd_list END
	case 260:
		return nil // CREATE TRIGGER

	// Rule 283: cmd ::= DROP TRIGGER ifexists fullname
	case 283:
		return nil

	// Rule 292: cmd ::= ALTER TABLE fullname RENAME TO nm
	case 292:
		return nil

	// Rule 302: cmd ::= create_vtab
	case 302:
		return nil

	// Rule 303: cmd ::= create_vtab LP vtabarglist RP
	case 303:
		return nil

	// Rule 304: create_vtab ::= createkw VIRTUAL TABLE ifnotexists nm dbnm USING nm
	case 304:
		name := getString(getRHS(p, ruleNo, 5))
		module := getString(getRHS(p, ruleNo, 8))
		return &sql.CreateVirtualTableStmt{
			Name:   name,
			Module: module,
		}

	// Rule 348: input ::= cmdlist
	case 348:
		return nil

	// Rule 349: cmdlist ::= cmdlist ecmd
	case 349:
		return nil

	// Rule 350: cmdlist ::= ecmd
	case 350:
		return nil

	// Rule 351: ecmd ::= SEMI
	case 351:
		return nil

	// Rule 352: ecmd ::= cmdx SEMI
	case 352:
		return getRHS(p, ruleNo, 1)

	// Rule 353: ecmd ::= explain cmdx SEMI (EXPLAIN)
	case 353:
		return &sql.ExplainStmt{
			Statement: getStmt(getRHS(p, ruleNo, 2)),
		}

	// Rule 354: trans_opt ::=
	case 354:
		return nil

	// Rule 355: trans_opt ::= TRANSACTION
	case 355:
		return nil

	// Rule 359: cmd ::= create_table create_table_args
	case 359:
		ct, _ := getRHS(p, ruleNo, 1).(*sql.CreateTableStmt)
		args := getRHS(p, ruleNo, 2)
		if ct != nil {
			// create_table_args can be columns ([]sql.ColumnDef) from rule 19
			// or a *CreateTableStmt with AsSelect from rule 20
			if cols, ok := args.([]sql.ColumnDef); ok {
				ct.Columns = cols
			} else if ct2, ok := args.(*sql.CreateTableStmt); ok && ct2 != nil {
				ct.Columns = ct2.Columns
				ct.AsSelect = ct2.AsSelect
			}
		}
		if ct != nil {
			return ct
		}
		return getRHS(p, ruleNo, 1)

	// Rule 360: table_option_set ::= table_option
	case 360:
		return nil

	// Rule 361: columnlist ::= columnlist COMMA columnname carglist
	case 361:
		acc := getColumnList(getRHS(p, ruleNo, 1))
		col := getColumnDef(getRHS(p, ruleNo, 3))
		return append(acc, col)

	// Rule 362: columnlist ::= columnname carglist
	case 362:
		col := getColumnDef(getRHS(p, ruleNo, 1))
		return []sql.ColumnDef{col}

	// Rule 363: nm ::= ID|INDEXED|JOIN_KW
	case 363:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return tok.Value
		}
		return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

	// Rule 364: nm ::= STRING
	case 364:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return tok.Value
		}
		return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

	// Rule 365: typetoken ::= typename
	case 365:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 366: typename ::= ID|STRING
	case 366:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return tok.Value
		}
		return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

	// Rule 369: carglist ::= carglist ccons
	case 369:
		return nil

	// Rule 370: carglist ::=
	case 370:
		return nil

	// Rule 380: selectnowith ::= oneselect (already handled, but keep for pass-through)
	case 380:
		return nil

	// Rule 381: oneselect ::= values
	case 381:
		return getRHS(p, ruleNo, 1)

	// Rule 383: as ::= ID|STRING
	case 383:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return tok.Value
		}
		return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

	// Rule 386: expr ::= term
	case 386:
		return getRHS(p, ruleNo, 1)

	// Rule 387: likeop ::= LIKE_KW|MATCH
	case 387:
		if lookahead == TK_MATCH {
			return "MATCH"
		}
		return "LIKE"

	// Rule 389: exprlist ::= nexprlist
	case 389:
		return getExprList(getRHS(p, ruleNo, 1))

	// Rule 395: plus_num ::= INTEGER|FLOAT
	case 395:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return &sql.NumericLit{Value: tok.Value}
		}
		return getRHS(p, ruleNo, 1)

	// Rule 409: with ::=
	case 409:
		return nil

	// Rule 410: windowdefn_list ::= windowdefn
	case 410:
		return nil

	default:
		// For unhandled rules, pass through the first RHS value only if the rule has RHS symbols
		if p.pos >= 1 {
			t := p.tables
			if ruleNo < len(t.RuleInfoNRhs) && t.RuleInfoNRhs[ruleNo] != 0 {
				return getRHS(p, ruleNo, 1)
			}
		}
		return nil
	}
}

// --- Type-safe accessors for parser stack values ---

type setOpResult struct {
	Op  sql.SetOp
	All bool
}

func getString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if tok, ok := v.(sql.Token); ok {
		return tok.Value
	}
	return fmt.Sprintf("%v", v)
}

func getBool(v interface{}) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func getExpr(v interface{}) sql.Expr {
	if v == nil {
		return nil
	}
	if e, ok := v.(sql.Expr); ok {
		return e
	}
	return nil
}

func getStmt(v interface{}) sql.Stmt {
	if v == nil {
		return nil
	}
	if s, ok := v.(sql.Stmt); ok {
		return s
	}
	return nil
}

func getSelectStmt(v interface{}) *sql.SelectStmt {
	if v == nil {
		return nil
	}
	if s, ok := v.(*sql.SelectStmt); ok {
		return s
	}
	return nil
}

// checkCompoundSelect mirrors SQLite's parserDoubleLinkSelect semantic check:
// in a compound SELECT (UNION/INTERSECT/EXCEPT), ORDER BY and LIMIT may only
// appear on the final SELECT. If any earlier member has them, report the
// error "X clause should come after Y not before" (matching SQLite's message).
func checkCompoundSelect(p *Parser, sel *sql.SelectStmt) *sql.SelectStmt {
	if sel == nil || p.SemanticErr != nil {
		return sel
	}
	// Walk the chain built by rule 88: members are linked via Union.
	// The last member (Union == nil) may carry ORDER BY / LIMIT.
	members := []*sql.SelectStmt{}
	for cur := sel; cur != nil; cur = cur.Union {
		members = append(members, cur)
	}
	// members[len-1] is the final SELECT — allowed to have ORDER BY/LIMIT.
	// Any earlier member with ORDER BY or LIMIT is an error.
	for i := 0; i < len(members)-1; i++ {
		m := members[i]
		if len(m.OrderBy) > 0 {
			// The operator stored on this member links it to the next
			// member in the compound chain (set by rule 88), so it names
			// the operator that the ORDER BY should have come after.
			p.SemanticErr = fmt.Errorf("%s clause should come after %s not before", "ORDER BY", opNameOf(m.SetOp))
			return sel
		}
		if m.Limit != nil {
			p.SemanticErr = fmt.Errorf("%s clause should come after %s not before", "LIMIT", opNameOf(m.SetOp))
			return sel
		}
	}
	return sel
}

// opNameOf returns the SQL keyword for a compound-set operator.
func opNameOf(op sql.SetOp) string {
	switch op {
	case sql.SetExcept:
		return "EXCEPT"
	case sql.SetIntersect:
		return "INTERSECT"
	case sql.SetUnion:
		return "UNION"
	default:
		return "UNION"
	}
}

func getSelectColumns(v interface{}) []sql.SelectColumn {
	if v == nil {
		return nil
	}
	if cols, ok := v.([]sql.SelectColumn); ok {
		return cols
	}
	return nil
}

func getColumnList(v interface{}) []sql.ColumnDef {
	if v == nil {
		return nil
	}
	if cols, ok := v.([]sql.ColumnDef); ok {
		return cols
	}
	return nil
}

func getColumnDef(v interface{}) sql.ColumnDef {
	if v == nil {
		return sql.ColumnDef{}
	}
	if c, ok := v.(sql.ColumnDef); ok {
		return c
	}
	return sql.ColumnDef{}
}

func getTableRef(v interface{}) sql.TableRef {
	if v == nil {
		return sql.TableRef{}
	}
	if t, ok := v.(sql.TableRef); ok {
		return t
	}
	return sql.TableRef{}
}

// seltablistAcc accumulates the FROM clause during seltablist reductions.
// It carries the first table and the list of joins (comma or explicit).
type seltablistAcc struct {
	First     sql.TableRef
	HasFirst  bool
	Joins     []sql.JoinClause
	PendingOp joinOp // join operator waiting for the next table
}

// joinOp describes a join operator between two tables.
type joinOp struct {
	Kind  string // "LEFT", "RIGHT", "INNER", "CROSS", "NATURAL", ""
	Outer bool   // "LEFT OUTER JOIN" etc.
	Comma bool   // comma join (FROM a, b)
}

// appendTable adds a table to the accumulator. If a join operator is pending,
// the new table becomes a JoinClause; otherwise it becomes the first table.
func (a *seltablistAcc) appendTable(ref sql.TableRef) *seltablistAcc {
	if !a.HasFirst {
		a.First = ref
		a.HasFirst = true
		return a
	}
	jc := sql.JoinClause{Table: ref, CommaJoin: a.PendingOp.Comma}
	switch a.PendingOp.Kind {
	case "LEFT":
		jc.JoinType = "LEFT"
	case "RIGHT":
		jc.JoinType = "RIGHT"
	case "FULL":
		jc.JoinType = "FULL"
	case "INNER":
		jc.JoinType = "INNER"
	case "CROSS":
		jc.JoinType = "CROSS"
	case "NATURAL":
		jc.JoinType = "NATURAL"
	default:
		jc.JoinType = "CROSS"
	}
	a.Joins = append(a.Joins, jc)
	a.PendingOp = joinOp{}
	return a
}

// appendJoin appends a pre-built JoinClause (used by parenthesized lists).
func (a *seltablistAcc) appendJoin(j sql.JoinClause) *seltablistAcc {
	a.Joins = append(a.Joins, j)
	return a
}

// firstTable returns the first table ref, or an empty one.
func (a *seltablistAcc) firstTable() sql.TableRef {
	if a == nil || !a.HasFirst {
		return sql.TableRef{}
	}
	return a.First
}

// appendSeltablistTable handles seltablist ::= stl_prefix nm dbnm as ... rules.
// posName/posSchema/posAlias are the 1-based RHS positions of the table name,
// schema (dbnm), and alias.
func appendSeltablistTable(p *Parser, ruleNo, posName, posSchema, posAlias int) *seltablistAcc {
	acc := getSeltablist(getRHS(p, ruleNo, 1))
	tbl := getString(getRHS(p, ruleNo, posName))
	schema := getString(getRHS(p, ruleNo, posSchema))
	alias := getString(getRHS(p, ruleNo, posAlias))
	if schema != "" {
		tbl = schema + "." + tbl
	}
	return acc.appendTable(sql.TableRef{Name: tbl, As: alias})
}

// getSeltablist extracts a seltablistAcc from a stack value, creating an empty
// one if the value is a plain TableRef (backward compat for rules that return
// TableRef directly).
func getSeltablist(v interface{}) *seltablistAcc {
	switch t := v.(type) {
	case *seltablistAcc:
		return t
	case sql.TableRef:
		return &seltablistAcc{First: t, HasFirst: true}
	default:
		return &seltablistAcc{}
	}
}

// getJoinOp extracts a joinOp from a stack value.
func getJoinOp(v interface{}) joinOp {
	if op, ok := v.(joinOp); ok {
		return op
	}
	return joinOp{}
}

// joinKind maps a JOIN_KW token value to a join type keyword.
func joinKind(v interface{}) string {
	s := getString(v)
	switch strings.ToUpper(s) {
	case "LEFT":
		return "LEFT"
	case "RIGHT":
		return "RIGHT"
	case "FULL":
		return "FULL"
	case "INNER":
		return "INNER"
	case "CROSS":
		return "CROSS"
	case "NATURAL":
		return "NATURAL"
	default:
		return ""
	}
}

// fromValue extracts the From TableRef and Joins list from a `from` nonterminal
// value. The value is either a TableRef (old path) or a seltablistAcc.
func fromValue(v interface{}) (sql.TableRef, []sql.JoinClause) {
	if acc, ok := v.(*seltablistAcc); ok {
		return acc.First, acc.Joins
	}
	if t, ok := v.(sql.TableRef); ok {
		return t, nil
	}
	return sql.TableRef{}, nil
}

func getExprList(v interface{}) []sql.Expr {
	if v == nil {
		return nil
	}
	if list, ok := v.([]sql.Expr); ok {
		return list
	}
	return nil
}

func getOrderByList(v interface{}) []sql.OrderByTerm {
	if v == nil {
		return nil
	}
	if list, ok := v.([]sql.OrderByTerm); ok {
		return list
	}
	return nil
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

func getCreateTable(v interface{}) *sql.CreateTableStmt {
	if v == nil {
		return nil
	}
	if ct, ok := v.(*sql.CreateTableStmt); ok {
		return ct
	}
	return nil
}

func getCreateTableArgs(v interface{}) interface{} {
	return v
}
