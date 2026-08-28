package parse

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// Rule 83: cmd ::= DROP VIEW ifexists fullname
func rule83(ruleNo int, p *Parser) interface{} {
	ifExists := getBool(getRHS(p, ruleNo, 3))
	name := getString(getRHS(p, ruleNo, 4))
	return &sql.DropViewStmt{Name: name, IfExists: ifExists}

}

// Rule 84: cmd ::= select
func rule84(ruleNo int, p *Parser) interface{} {
	return getSelectStmt(getRHS(p, ruleNo, 1))

}

func rule85(ruleNo int, p *Parser) interface{} {
	sel := getSelectStmt(getRHS(p, ruleNo, 3))
	if sel != nil {
		sel.CTEs = getCTEDefs(getRHS(p, ruleNo, 2))
	}
	return checkCompoundSelect(p, sel)

}

// Rule 86: select ::= WITH RECURSIVE wqlist selectnowith
func rule86(ruleNo int, p *Parser) interface{} {
	sel := getSelectStmt(getRHS(p, ruleNo, 4))
	if sel != nil {
		sel.CTEs = getCTEDefs(getRHS(p, ruleNo, 3))
	}
	return checkCompoundSelect(p, sel)

}

// Rule 87: select ::= selectnowith
func rule87(ruleNo int, p *Parser) interface{} {
	return checkCompoundSelect(p, getSelectStmt(getRHS(p, ruleNo, 1)))

}

// Rule 88: selectnowith ::= selectnowith multiselect_op oneselect
func rule88(ruleNo int, p *Parser) interface{} {
	left := getSelectStmt(getRHS(p, ruleNo, 1))
	right := getSelectStmt(getRHS(p, ruleNo, 3))
	if left == nil || right == nil {
		return left
	}
	// multiselect_op = getRHS(p, ruleNo, 2) - returns (SetOp, bool for ALL)
	op := getSetOp(getRHS(p, ruleNo, 2))
	all := false
	if sr, ok := getRHS(p, ruleNo, 2).(setOpResult); ok {
		all = sr.All
	}
	left.AppendUnion(right, op, all)
	return left

}

// Rule 89: multiselect_op ::= UNION
func rule89(ruleNo int, p *Parser) interface{} {
	return setOpResult{Op: sql.SetUnion, All: false}

}

// Rule 90: multiselect_op ::= UNION ALL
func rule90(ruleNo int, p *Parser) interface{} {
	return setOpResult{Op: sql.SetUnion, All: true}

}

// Rule 91: multiselect_op ::= EXCEPT|INTERSECT
func rule91(ruleNo int, p *Parser) interface{} {
	// Distinguish EXCEPT vs INTERSECT from the RHS token value. The
	// lookahead at reduce time is the NEXT token (e.g. SELECT), not the
	// operator being reduced, so it cannot be used to tell them apart.
	op := sql.SetExcept // default
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok && strings.EqualFold(tok.Value, "INTERSECT") {
		op = sql.SetIntersect
	}
	return setOpResult{Op: op, All: false}

}

// Rule 92: oneselect ::= SELECT distinct selcollist from where_opt groupby_opt having_opt orderby_opt limit_opt
func rule92(ruleNo int, p *Parser) interface{} {
	distinct := getBool(getRHS(p, ruleNo, 2))
	cols := getSelectColumns(getRHS(p, ruleNo, 3))
	from, joins := fromValue(getRHS(p, ruleNo, 4))
	where := getExpr(getRHS(p, ruleNo, 5))
	groupBy := getExprList(getRHS(p, ruleNo, 6))
	having := getExpr(getRHS(p, ruleNo, 7))
	orderBy := getOrderByList(getRHS(p, ruleNo, 8))
	lc := getLimitClause(getRHS(p, ruleNo, 9))

	return &sql.SelectStmt{
		Distinct: distinct,
		Columns:  cols,
		From:     from,
		Joins:    joins,
		Where:    where,
		GroupBy:  groupBy,
		Having:   having,
		OrderBy:  orderBy,
		Limit:    lc.limit,
		Offset:   lc.offset,
	}

}

func rule93(ruleNo int, p *Parser) interface{} {
	// Same as 92 but with window_clause before orderby_opt
	distinct := getBool(getRHS(p, ruleNo, 2))
	cols := getSelectColumns(getRHS(p, ruleNo, 3))
	from, joins := fromValue(getRHS(p, ruleNo, 4))
	where := getExpr(getRHS(p, ruleNo, 5))
	groupBy := getExprList(getRHS(p, ruleNo, 6))
	having := getExpr(getRHS(p, ruleNo, 7))
	windows := getWindowDefList(getRHS(p, ruleNo, 8))
	orderBy := getOrderByList(getRHS(p, ruleNo, 9))
	lc := getLimitClause(getRHS(p, ruleNo, 10))

	return &sql.SelectStmt{
		Distinct: distinct,
		Columns:  cols,
		From:     from,
		Joins:    joins,
		Where:    where,
		GroupBy:  groupBy,
		Having:   having,
		Windows:  windows,
		OrderBy:  orderBy,
		Limit:    lc.limit,
		Offset:   lc.offset,
	}

}

// Rule 94: values ::= VALUES LP nexprlist RP
func rule94(ruleNo int, p *Parser) interface{} {
	exprs := getExprList(getRHS(p, ruleNo, 3))
	cols := make([]sql.SelectColumn, len(exprs))
	for i, expr := range exprs {
		cols[i] = sql.SelectColumn{Expr: expr}
	}
	return &sql.SelectStmt{
		Columns: cols,
	}

}

// Rule 95: oneselect ::= mvalues
func rule95(ruleNo int, p *Parser) interface{} {
	sel := getSelectStmt(getRHS(p, ruleNo, 1))
	if sel != nil {
		sel.ValuesChain = true
	}
	return sel

}

// Rule 96: mvalues ::= values COMMA LP nexprlist RP
func rule96(ruleNo int, p *Parser) interface{} {
	first := getSelectStmt(getRHS(p, ruleNo, 1))
	secondExprs := getExprList(getRHS(p, ruleNo, 4))
	secondCols := make([]sql.SelectColumn, len(secondExprs))
	for i, expr := range secondExprs {
		secondCols[i] = sql.SelectColumn{Expr: expr}
	}
	second := &sql.SelectStmt{Columns: secondCols}
	if first != nil {
		if len(first.Columns) != len(secondExprs) {
			p.SemanticErr = fmt.Errorf("all VALUES must have the same number of terms")
		}
		first.AppendUnion(second, sql.SetUnion, true)
	}
	return first

}

// Rule 97: mvalues ::= mvalues COMMA LP nexprlist RP
func rule97(ruleNo int, p *Parser) interface{} {
	acc := getSelectStmt(getRHS(p, ruleNo, 1))
	exprs := getExprList(getRHS(p, ruleNo, 4))
	cols := make([]sql.SelectColumn, len(exprs))
	for i, expr := range exprs {
		cols[i] = sql.SelectColumn{Expr: expr}
	}
	last := &sql.SelectStmt{Columns: cols}
	if acc != nil {
		if len(acc.Columns) != len(exprs) {
			p.SemanticErr = fmt.Errorf("all VALUES must have the same number of terms")
		}
		acc.AppendUnion(last, sql.SetUnion, true)
	}
	return acc

}

// Rule 98: distinct ::= DISTINCT
func rule98(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 99: distinct ::= ALL
func rule99(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 100: distinct ::=
func rule100(ruleNo int, p *Parser) interface{} {
	return false

}

func rule102(ruleNo int, p *Parser) interface{} {
	expr := getExpr(getRHS(p, ruleNo, 3))
	alias := getString(getRHS(p, ruleNo, 5))

	// Prepend the accumulated list from sclp (RHS 1). sclp holds the
	// columns collected before the COMMA (via rule 382).
	prev := getSelectColumns(getRHS(p, ruleNo, 1))
	return append(prev, sql.SelectColumn{Expr: expr, As: alias})

}

// Rule 103: selcollist ::= sclp scanpt STAR
func rule103(ruleNo int, p *Parser) interface{} {
	prev := getSelectColumns(getRHS(p, ruleNo, 1))
	return append(prev, sql.SelectColumn{Expr: &sql.ColumnRef{Name: "*"}})

}

// Rule 104: selcollist ::= sclp scanpt nm DOT STAR
func rule104(ruleNo int, p *Parser) interface{} {
	tbl := getString(getRHS(p, ruleNo, 3))
	prev := getSelectColumns(getRHS(p, ruleNo, 1))
	return append(prev, sql.SelectColumn{Expr: &sql.ColumnRef{Table: tbl, Name: "*"}})

}

// Rule 105: as ::= AS nm
func rule105(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 2))

}

// Rule 106: as ::=
func rule106(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 107: from ::=
func rule107(ruleNo int, p *Parser) interface{} {
	return sql.TableRef{}

}

// Rule 108: from ::= FROM seltablist
func rule108(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 2)

}

// Rule 109: stl_prefix ::= seltablist joinop
// Combine the accumulated seltablist with the join operator that follows.
// The joinop (COMMA or JOIN) marks how the NEXT table will be joined.
func rule109(ruleNo int, p *Parser) interface{} {
	acc := getSeltablist(getRHS(p, ruleNo, 1))
	op := getJoinOp(getRHS(p, ruleNo, 2))
	acc.PendingOp = op
	return acc

}

func rule110(ruleNo int, p *Parser) interface{} {
	return &seltablistAcc{}

}

// Rule 111: seltablist ::= stl_prefix nm dbnm as on_using
func rule111(ruleNo int, p *Parser) interface{} {
	return appendSeltablistTable(p, ruleNo, 2, 3, 4, 0, 5)

}

// Rule 112: seltablist ::= stl_prefix nm dbnm as indexed_by on_using
func rule112(ruleNo int, p *Parser) interface{} {
	return appendSeltablistTable(p, ruleNo, 2, 3, 4, 5, 6)

}

// Rule 113: seltablist ::= stl_prefix nm dbnm LP exprlist RP as on_using
// Table-valued function in FROM: pragma_table_info('t1').
func rule113(ruleNo int, p *Parser) interface{} {
	acc := getSeltablist(getRHS(p, ruleNo, 1))
	tbl := getString(getRHS(p, ruleNo, 2))
	schema := getString(getRHS(p, ruleNo, 3))
	args := getExprList(getRHS(p, ruleNo, 5))
	alias := getString(getRHS(p, ruleNo, 7))
	on, using := getOnUsing(getRHS(p, ruleNo, 8))
	if schema != "" {
		tbl = tbl + "." + schema
	}
	return acc.appendTableWithOn(sql.TableRef{Name: tbl, As: alias, Args: args, IsTabFunc: true}, on, using)

}

// Rule 114: seltablist ::= stl_prefix LP select RP as on_using
func rule114(ruleNo int, p *Parser) interface{} {
	acc := getSeltablist(getRHS(p, ruleNo, 1))
	sel := getSelectStmt(getRHS(p, ruleNo, 3))
	alias := getString(getRHS(p, ruleNo, 5))
	on, using := getOnUsing(getRHS(p, ruleNo, 6))
	ref := sql.TableRef{Subquery: sel, As: alias}
	return acc.appendTableWithOn(ref, on, using)

}

// Rule 115: seltablist ::= stl_prefix LP seltablist RP as on_using
// Parenthesized table list: FROM (t1) or FROM (t1, t2).
// A parenthesized comma list is flattened into the outer query (SQLite
// treats (t1, t2) as a group). A parenthesized JOIN group — FROM
// (t1 JOIN t2 ON ...) — is a derived table (subquery): its joins must
// stay inside the parens so an outer join sees the group as one unit
// (e.g. FROM t2 LEFT JOIN (dual JOIN t1 ON true) ON b=c).
func rule115(ruleNo int, p *Parser) interface{} {
	acc := getSeltablist(getRHS(p, ruleNo, 1))
	inner := getSeltablist(getRHS(p, ruleNo, 3))
	alias := getString(getRHS(p, ruleNo, 5))
	on, using := getOnUsing(getRHS(p, ruleNo, 6))
	// A parenthesized JOIN group (explicit JOIN keywords, not a comma list)
	// is always kept as a derived table when it is a JOIN operand: the
	// outer ON/USING applies to the group as a unit and may reference the
	// group's inner tables (e.g. FROM t1 INNER JOIN (t2 CROSS JOIN t0) ON
	// (t0.c0<t0.c1)), which flattening would break. Only parenthesized
	// comma lists are flattened (SQLite treats (t1, t2) as a group).
	if inner.hasExplicitJoins() && acc.HasFirst {
		// A parenthesized JOIN group must stay a derived table when the
		// outer join is OUTER, OR when the group itself contains an OUTER
		// join: flattening a group with an inner FULL JOIN would let
		// later joins leak into it (e.g. FROM t4 INNER JOIN (t5 FULL JOIN
		// t6 USING(id)) USING(id) must keep the FULL JOIN scoped).
		sub := &sql.SelectStmt{
			From:  inner.First,
			Joins: inner.Joins,
			Columns: []sql.SelectColumn{
				{Expr: &sql.ColumnRef{Name: "*"}},
			},
		}
		ref := sql.TableRef{Subquery: sub, As: alias}
		return acc.appendTableWithOn(ref, on, using)
	}
	ref := inner.firstTable()
	if alias != "" {
		ref.As = alias
	}
	// A parenthesized comma list (t1, t2) contributes its joins. The
	// trailing ON/USING of the parenthesized group binds to the first
	// table contributed by the group (SQLite: FROM t1 JOIN (t2 JOIN t3
	// USING(a)) USING(a) applies the outer USING to the group's first
	// table t2).
	acc = acc.appendTableWithOn(ref, on, using)
	for _, j := range inner.Joins {
		acc = acc.appendJoin(j)
	}
	return acc

}

// Rule 116: dbnm ::=
func rule116(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 117: dbnm ::= DOT nm
func rule117(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 2))

}

func rule118(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 119: fullname ::= nm DOT nm
func rule119(ruleNo int, p *Parser) interface{} {
	a := getString(getRHS(p, ruleNo, 1))
	b := getString(getRHS(p, ruleNo, 3))
	return a + "." + b

}

// Rule 121: xfullname ::= nm DOT nm (schema-qualified table name used by
// INSERT/UPDATE/DELETE, e.g. "temp.t2"). Produces "schema.table".
func rule121(ruleNo int, p *Parser) interface{} {
	a := getString(getRHS(p, ruleNo, 1))
	b := getString(getRHS(p, ruleNo, 3))
	return a + "." + b

}

// Rule 122: fullname ::= nm AS nm — table alias. The value is the
// TABLE NAME (the alias is consumed); the join-op productions are
// separate (rules 124+).
func rule122(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 123: joinop ::= JOIN_KW nm JOIN
func rule123(ruleNo int, p *Parser) interface{} {
	return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1))}

}

// Rule 124: joinop ::= COMMA|JOIN
func rule124(ruleNo int, p *Parser) interface{} {
	// Rule 124: joinop ::= COMMA|JOIN — the multiterminal covers both a
	// comma join (FROM a, b) and a plain JOIN keyword (INNER JOIN).
	// Distinguish by the token value: "," is a comma join, "JOIN" is INNER.
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok && tok.Value == "," {
		return joinOp{Comma: true}
	}
	return joinOp{Kind: "INNER"}

}

// Rule 125: joinop ::= JOIN_KW JOIN
func rule125(ruleNo int, p *Parser) interface{} {
	return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1)), Outer: true}

}

// Rule 126: joinop ::= JOIN_KW nm JOIN
// "NATURAL LEFT JOIN" has JOIN_KW=NATURAL and nm=LEFT; the nm join type
// must be preserved so exec can NULL-fill the correct side (SQLite's
// sqlite3JoinType ORs JT_NATURAL with the JOIN_KW/nm flags).
func rule126(ruleNo int, p *Parser) interface{} {
	kw := joinKind(getRHS(p, ruleNo, 1))
	nm := joinKind(getRHS(p, ruleNo, 2))
	return joinOp{Kind: combineNaturalJoin(kw, nm), Outer: true}

}

func rule127(ruleNo int, p *Parser) interface{} {
	kw := joinKind(getRHS(p, ruleNo, 1))
	nm := joinKind(getRHS(p, ruleNo, 2))
	return joinOp{Kind: combineNaturalJoin(kw, nm), Outer: true}

}

// Rule 128: joinop ::= JOIN_KW nm JOIN
func rule128(ruleNo int, p *Parser) interface{} {
	// Rule 128: on_using ::= ON expr — the ON condition for a JOIN.
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 129: on_using ::= USING LP idlist RP — the USING column list.
func rule129(ruleNo int, p *Parser) interface{} {
	return getStringList(getRHS(p, ruleNo, 3))

}

// Rule 130: on_using ::=
func rule130(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 131: on_using ::=
func rule131(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 132: indexed_by ::= INDEXED BY nm
// Returns the index name. Consumers currently ignore indexed_by.
func rule132(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 3))

}

// Rule 133: indexed_by ::= NOT INDEXED
// Marks the table reference as NOT INDEXED (no index hints).
// Consumers currently ignore indexed_by; this returns a non-nil marker
// so the rule does not fall through to a nil passthrough.
func rule133(ruleNo int, p *Parser) interface{} {
	return "NOT INDEXED"

}

// Rule 134: orderby_opt ::=
func rule134(ruleNo int, p *Parser) interface{} {
	return ([]sql.OrderByTerm)(nil)

}

func rule135(ruleNo int, p *Parser) interface{} {
	return getOrderByList(getRHS(p, ruleNo, 3))

}

// Rule 136: sortlist ::= sortlist COMMA expr sortorder nulls
func rule136(ruleNo int, p *Parser) interface{} {
	acc := getOrderByList(getRHS(p, ruleNo, 1))
	expr := getExpr(getRHS(p, ruleNo, 3))
	desc := getRHS(p, ruleNo, 4) == "DESC"
	nf, nl := getNullsOrder(getRHS(p, ruleNo, 5))
	return append(acc, sql.OrderByTerm{Expr: expr, Desc: desc, NullsFirst: nf, NullsLast: nl})

}

// Rule 137: sortlist ::= expr sortorder nulls
func rule137(ruleNo int, p *Parser) interface{} {
	expr := getExpr(getRHS(p, ruleNo, 1))
	desc := getRHS(p, ruleNo, 2) == "DESC"
	nf, nl := getNullsOrder(getRHS(p, ruleNo, 3))
	return []sql.OrderByTerm{{Expr: expr, Desc: desc, NullsFirst: nf, NullsLast: nl}}

}

// Rule 138: sortorder ::= ASC
// The sortorder value is a string: "ASC", "DESC", or "" (absent).
// Consumers compare against "DESC" for descending order; eidlist rules
// treat ANY explicit sortorder (ASC or DESC) as an error, matching SQLite's
// SQLITE_SO_UNDEFINED distinction.
func rule138(ruleNo int, p *Parser) interface{} {
	return "ASC"

}

// Rule 139: sortorder ::= DESC
func rule139(ruleNo int, p *Parser) interface{} {
	return "DESC"

}

// Rule 140: sortorder ::=
func rule140(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 141: nulls ::= NULLS FIRST
func rule141(ruleNo int, p *Parser) interface{} {
	return nullsOrder{first: true}

}

// Rule 142: nulls ::= NULLS LAST
func rule142(ruleNo int, p *Parser) interface{} {
	return nullsOrder{last: true}

}

func rule143(ruleNo int, p *Parser) interface{} {
	return nullsOrder{}

}

// Rule 144: groupby_opt ::=
func rule144(ruleNo int, p *Parser) interface{} {
	return ([]sql.Expr)(nil)

}

// Rule 145: groupby_opt ::= GROUP BY nexprlist
func rule145(ruleNo int, p *Parser) interface{} {
	return getExprList(getRHS(p, ruleNo, 3))

}

// Rule 146: having_opt ::=
func rule146(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 147: having_opt ::= HAVING expr
func rule147(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 148: limit_opt ::=
func rule148(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 149: limit_opt ::= LIMIT expr
func rule149(ruleNo int, p *Parser) interface{} {
	return &limitClause{limit: getExpr(getRHS(p, ruleNo, 2))}

}

// Rule 150: limit_opt ::= LIMIT expr OFFSET expr
func rule150(ruleNo int, p *Parser) interface{} {
	return &limitClause{
		limit:  getExpr(getRHS(p, ruleNo, 2)),
		offset: getExpr(getRHS(p, ruleNo, 4)),
	}

}

func rule151(ruleNo int, p *Parser) interface{} {
	// SQLite's LIMIT expr, expr form: first expr is the OFFSET.
	return &limitClause{
		offset: getExpr(getRHS(p, ruleNo, 2)),
		limit:  getExpr(getRHS(p, ruleNo, 4)),
	}

}

// Rule 152: cmd ::= with DELETE FROM xfullname indexed_opt where_opt_ret
func rule152(ruleNo int, p *Parser) interface{} {
	tbl := getString(getRHS(p, ruleNo, 4))
	wr := getWhereRet(getRHS(p, ruleNo, 6))
	stmt := &sql.DeleteStmt{Table: tbl, CTEs: getCTEDefs(getRHS(p, ruleNo, 1))}
	if io := getString(getRHS(p, ruleNo, 5)); io != "" {
		stmt.IndexedBy = io
	}
	if wr != nil {
		stmt.Where = wr.where
		if len(wr.returning) > 0 {
			stmt.Returning = foldReturning(wr.returning)
			stmt.HasReturning = true
		}
	}
	return stmt

}

// Rule 153: where_opt ::=
func rule153(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 154: where_opt ::= WHERE expr
func rule154(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 155: where_opt_ret ::=
func rule155(ruleNo int, p *Parser) interface{} {
	return &whereRet{}

}

func rule156(ruleNo int, p *Parser) interface{} {
	return &whereRet{where: getExpr(getRHS(p, ruleNo, 2))}

}

// Rule 157: where_opt_ret ::= RETURNING selcollist
func rule157(ruleNo int, p *Parser) interface{} {
	return &whereRet{returning: getSelectColumns(getRHS(p, ruleNo, 2))}

}

// Rule 158: where_opt_ret ::= WHERE expr RETURNING selcollist
func rule158(ruleNo int, p *Parser) interface{} {
	return &whereRet{
		where:     getExpr(getRHS(p, ruleNo, 2)),
		returning: getSelectColumns(getRHS(p, ruleNo, 4)),
	}

}

// Rule 159: cmd ::= with UPDATE orconf xfullname indexed_opt SET setlist from where_opt_ret
func rule159(ruleNo int, p *Parser) interface{} {
	tbl := getString(getRHS(p, ruleNo, 4))
	setlist := getAssignments(getRHS(p, ruleNo, 7))
	fromInfo := getFromInfo(getRHS(p, ruleNo, 8))
	wr := getWhereRet(getRHS(p, ruleNo, 9))
	stmt := &sql.UpdateStmt{
		Table:       tbl,
		OnConflict:  getString(getRHS(p, ruleNo, 3)),
		Assignments: setlist,
		CTEs:        getCTEDefs(getRHS(p, ruleNo, 1)),
	}
	if io := getString(getRHS(p, ruleNo, 5)); io != "" {
		stmt.IndexedBy = io
	}
	if fromInfo != nil {
		stmt.From = fromInfo.first
		stmt.FromJoins = fromInfo.joins
	}
	if wr != nil {
		stmt.Where = wr.where
		if len(wr.returning) > 0 {
			stmt.Returning = foldReturning(wr.returning)
			stmt.HasReturning = true
		}
	}
	return stmt

}

// Rule 160: setlist ::= setlist COMMA nm EQ expr
func rule160(ruleNo int, p *Parser) interface{} {
	acc := getAssignments(getRHS(p, ruleNo, 1))
	col := getString(getRHS(p, ruleNo, 3))
	val := getExpr(getRHS(p, ruleNo, 5))
	return append(acc, sql.Assignment{Column: col, Value: val})

}

// Rule 162: setlist ::= nm EQ expr
func rule162(ruleNo int, p *Parser) interface{} {
	col := getString(getRHS(p, ruleNo, 1))
	val := getExpr(getRHS(p, ruleNo, 3))
	return []sql.Assignment{{Column: col, Value: val}}

}

// Rule 164: cmd ::= with insert_cmd INTO xfullname idlist_opt select upsert
func rule164(ruleNo int, p *Parser) interface{} {
	table := getString(getRHS(p, ruleNo, 4))
	columns := getStringList(getRHS(p, ruleNo, 5))
	sel := getSelectStmt(getRHS(p, ruleNo, 6))
	// A VALUES insert (INSERT INTO t VALUES(...),(...)) parses as a SELECT
	// with no FROM. Convert it into s.Values tuples and clear s.Select so
	// the engine uses the VALUES path (insertRow); a real INSERT...SELECT
	// (even one without a FROM clause) keeps s.Select.
	var values [][]sql.Expr
	if sel != nil && sel.ValuesChain {
		values = valuesFromSelect(sel)
		sel = nil
	}
	// insert_cmd is "INSERT" or "REPLACE" (rules 173/174); the orconf
	// resolution type ("IGNORE", "REPLACE", ...) arrives in that string.
	cmd := getString(getRHS(p, ruleNo, 2))
	stmt := &sql.InsertStmt{
		Table:      table,
		Columns:    columns,
		Values:     values,
		Select:     sel,
		IsReplace:  strings.EqualFold(cmd, "REPLACE"),
		OrIgnore:   strings.EqualFold(cmd, "IGNORE"),
		OrFail:     strings.EqualFold(cmd, "FAIL"),
		OrConflict: strings.ToUpper(cmd),
		CTEs:       getCTEDefs(getRHS(p, ruleNo, 1)),
	}
	// The upsert nonterminal (RHS 7) carries an ON CONFLICT clause and/or
	// a RETURNING projection.
	if uv := getUpsertVal(getRHS(p, ruleNo, 7)); uv != nil {
		stmt.OnConflict = uv.onConflict
		if len(uv.returning) > 0 {
			stmt.Returning = foldReturning(uv.returning)
			stmt.HasReturning = true
		}
	}

	return stmt

}

// Rule 165: cmd ::= with insert_cmd INTO xfullname idlist_opt DEFAULT VALUES returning
func rule165(ruleNo int, p *Parser) interface{} {
	table := getString(getRHS(p, ruleNo, 4))
	columns := getStringList(getRHS(p, ruleNo, 5))
	cmd := getString(getRHS(p, ruleNo, 2))
	stmt := &sql.InsertStmt{
		Table:      table,
		Columns:    columns,
		IsReplace:  strings.EqualFold(cmd, "REPLACE"),
		OrIgnore:   strings.EqualFold(cmd, "IGNORE"),
		OrFail:     strings.EqualFold(cmd, "FAIL"),
		OrConflict: strings.ToUpper(cmd),
	}
	// The returning nonterminal (RHS 8) is either nil (rule 166) or a
	// []sql.SelectColumn from `RETURNING selcollist` (rule 167).
	if cols, ok := getRHS(p, ruleNo, 8).([]sql.SelectColumn); ok && len(cols) > 0 {
		stmt.Returning = foldReturning(cols)
		stmt.HasReturning = true
	}
	return stmt

}

// Rule 166: upsert ::=
func rule166(ruleNo int, p *Parser) interface{} {
	return &upsertVal{}

}

// Rule 167: upsert ::= RETURNING selcollist
func rule167(ruleNo int, p *Parser) interface{} {
	return &upsertVal{returning: getSelectColumns(getRHS(p, ruleNo, 2))}

}

// Rule 168: upsert ::= ON CONFLICT LP sortlist RP where_opt
//
//	DO UPDATE SET setlist where_opt upsert
func rule168(ruleNo int, p *Parser) interface{} {
	target := getOrderByList(getRHS(p, ruleNo, 4))
	// NULLS FIRST/LAST is not supported in an ON CONFLICT target.
	if err := rejectNullsInSortlist(target); err != nil {
		p.SemanticErr = err
		return nil
	}
	names, exprs := conflictTargetColumns(target)
	oc := &sql.OnConflictClause{
		Action:         sql.ConflictDoUpdate,
		ConflictColumn: names,
		TargetExpr:     exprs,
		// RHS 6 is the conflict-target WHERE (partial-index predicate);
		// RHS 11 is the DO UPDATE WHERE (update condition).
		TargetWhere: getExpr(getRHS(p, ruleNo, 6)),
		Where:       getExpr(getRHS(p, ruleNo, 11)),
		Assignments: getAssignments(getRHS(p, ruleNo, 10)),
	}
	// A chained ON CONFLICT clause: SQLite walks the clauses in statement
	// order and uses the first whose conflict target matches the conflict
	// actually encountered, so the new clause becomes the head of the chain
	// with the rest hanging off Next.
	uv := &upsertVal{onConflict: oc}
	if chained, ok := getRHS(p, ruleNo, 12).(*upsertVal); ok && chained != nil {
		oc.Next = chained.onConflict
		uv.returning = chained.returning
	}
	return uv

}

// Rule 169: upsert ::= ON CONFLICT LP sortlist RP where_opt DO NOTHING upsert
func rule169(ruleNo int, p *Parser) interface{} {
	target := getOrderByList(getRHS(p, ruleNo, 4))
	names, exprs := conflictTargetColumns(target)
	oc := &sql.OnConflictClause{
		Action:         sql.ConflictDoNothing,
		ConflictColumn: names,
		TargetExpr:     exprs,
		TargetWhere:    getExpr(getRHS(p, ruleNo, 6)),
	}
	uv := &upsertVal{onConflict: oc}
	if chained, ok := getRHS(p, ruleNo, 9).(*upsertVal); ok && chained != nil {
		oc.Next = chained.onConflict
		uv.returning = chained.returning
	}
	return uv

}

// Rule 170: upsert ::= ON CONFLICT DO NOTHING returning
func rule170(ruleNo int, p *Parser) interface{} {
	return &upsertVal{
		onConflict: &sql.OnConflictClause{
			Action: sql.ConflictDoNothing,
		},
		returning: getSelectColumns(getRHS(p, ruleNo, 5)),
	}

}

// Rule 171: upsert ::= ON CONFLICT DO UPDATE SET setlist where_opt returning
func rule171(ruleNo int, p *Parser) interface{} {
	return &upsertVal{
		onConflict: &sql.OnConflictClause{
			Action:      sql.ConflictDoUpdate,
			Where:       getExpr(getRHS(p, ruleNo, 7)),
			Assignments: getAssignments(getRHS(p, ruleNo, 6)),
		},
		returning: getSelectColumns(getRHS(p, ruleNo, 8)),
	}

}

// Rule 172: returning ::= RETURNING selcollist
func rule172(ruleNo int, p *Parser) interface{} {
	return getSelectColumns(getRHS(p, ruleNo, 2))

}

func rule173(ruleNo int, p *Parser) interface{} {
	// Return the orconf resolution type ("", "IGNORE", "REPLACE", ...).
	return getString(getRHS(p, ruleNo, 2))

}

// Rule 174: insert_cmd ::= REPLACE
func rule174(ruleNo int, p *Parser) interface{} {
	return "REPLACE"

}

// Rule 175: idlist_opt ::=
func rule175(ruleNo int, p *Parser) interface{} {
	return ([]string)(nil)

}

func rule176(ruleNo int, p *Parser) interface{} {
	return getStringList(getRHS(p, ruleNo, 2))

}

// Rule 177: idlist ::= idlist COMMA nm
func rule177(ruleNo int, p *Parser) interface{} {
	acc := getStringList(getRHS(p, ruleNo, 1))
	return append(acc, getString(getRHS(p, ruleNo, 3)))

}

// Rule 178: idlist ::= nm
func rule178(ruleNo int, p *Parser) interface{} {
	return []string{getString(getRHS(p, ruleNo, 1))}

}

// Rule 179: expr ::= LP expr RP
func rule179(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 180: expr ::= ID|INDEXED|JOIN_KW (column reference)
func rule180(ruleNo int, p *Parser) interface{} {
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
func rule181(ruleNo int, p *Parser) interface{} {
	schema := getString(getRHS(p, ruleNo, 1))
	col := getString(getRHS(p, ruleNo, 3))
	return &sql.ColumnRef{Table: schema, Name: col}

}

// Rule 182: expr ::= nm DOT nm DOT nm (schema.table.column)
func rule182(ruleNo int, p *Parser) interface{} {
	schema := getString(getRHS(p, ruleNo, 1))
	table := getString(getRHS(p, ruleNo, 3))
	col := getString(getRHS(p, ruleNo, 5))
	return &sql.ColumnRef{Table: schema + "." + table, Name: col}

}

// Rule 183: term ::= NULL|FLOAT|BLOB
func rule183(ruleNo int, p *Parser) interface{} {
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

func rule184(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return &sql.StringLit{Value: tok.Value}
	}
	if s, ok := getRHS(p, ruleNo, 1).(string); ok {
		return &sql.StringLit{Value: s}
	}
	return &sql.StringLit{}

}
