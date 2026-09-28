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
// SELECT grammar: compound selects, one-select suffixes (DISTINCT,
// FROM/JOIN, WHERE, GROUP BY, HAVING, ORDER BY, LIMIT), VALUES rows,
// and WITH/CTE.

package parse

import (
	"fmt"
	sql "github.com/pijalu/frigolite/internal/sql"
	"strings"
)

// Rule 84: cmd ::= select
func ruleCmdSelect(ruleNo int, p *Parser) interface{} {
	return getSelectStmt(getRHS(p, ruleNo, 1))

}

// Rule 85: select ::= WITH wqlist selectnowith
func ruleSelectWithWqlistSelectnowith(ruleNo int, p *Parser) interface{} {
	sel := getSelectStmt(getRHS(p, ruleNo, 3))
	if sel != nil {
		sel.CTEs = getCTEDefs(getRHS(p, ruleNo, 2))
	}
	return checkCompoundSelect(p, sel)

}

// Rule 86: select ::= WITH RECURSIVE wqlist selectnowith
func ruleSelectWithRecursiveWqlistSelectnowith(ruleNo int, p *Parser) interface{} {
	sel := getSelectStmt(getRHS(p, ruleNo, 4))
	if sel != nil {
		sel.CTEs = getCTEDefs(getRHS(p, ruleNo, 3))
	}
	return checkCompoundSelect(p, sel)

}

// Rule 87: select ::= selectnowith
func ruleSelectSelectnowith(ruleNo int, p *Parser) interface{} {
	return checkCompoundSelect(p, getSelectStmt(getRHS(p, ruleNo, 1)))

}

// Rule 88: selectnowith ::= selectnowith multiselect_op oneselect
func ruleSelectnowithSelectnowithMultiselectOpOneselect(ruleNo int, p *Parser) interface{} {
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
	right.ExplicitSetOp = true
	left.AppendUnion(right, op, all)
	return left

}

// Rule 89: multiselect_op ::= UNION
func ruleMultiselectOpUnion(ruleNo int, p *Parser) interface{} {
	return setOpResult{Op: sql.SetUnion, All: false}

}

// Rule 90: multiselect_op ::= UNION ALL
func ruleMultiselectOpUnionAll(ruleNo int, p *Parser) interface{} {
	return setOpResult{Op: sql.SetUnion, All: true}

}

// Rule 91: multiselect_op ::= EXCEPT|INTERSECT
func ruleMultiselectOpExceptIntersect(ruleNo int, p *Parser) interface{} {
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
func ruleOneselectCore(ruleNo int, p *Parser) interface{} {
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

// Rule 93: oneselect ::= SELECT distinct selcollist from where_opt groupby_opt having_opt window_clause orderby_opt limit_opt
func ruleOneselectCoreWindow(ruleNo int, p *Parser) interface{} {
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
func ruleValuesValuesLpNexprlistRp(ruleNo int, p *Parser) interface{} {
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
func ruleOneselectMvalues(ruleNo int, p *Parser) interface{} {
	sel := getSelectStmt(getRHS(p, ruleNo, 1))
	if sel != nil {
		sel.ValuesChain = true
	}
	return sel

}

// Rule 96: mvalues ::= values COMMA LP nexprlist RP
func ruleMvaluesValuesCommaLpNexprlistRp(ruleNo int, p *Parser) interface{} {
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
func ruleMvaluesMvaluesCommaLpNexprlistRp(ruleNo int, p *Parser) interface{} {
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
func ruleDistinctDistinct(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 99: distinct ::= ALL
func ruleDistinctAll(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 100: distinct ::=
func ruleDistinct(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 102: selcollist ::= sclp COMMA scanpt expr as
func ruleSelcollistSclpCommaScanptExprAs(ruleNo int, p *Parser) interface{} {
	expr := getExpr(getRHS(p, ruleNo, 3))
	alias := getString(getRHS(p, ruleNo, 5))

	// Prepend the accumulated list from sclp (RHS 1). sclp holds the
	// columns collected before the COMMA (via rule 382).
	prev := getSelectColumns(getRHS(p, ruleNo, 1))
	return append(prev, sql.SelectColumn{Expr: expr, As: alias})

}

// Rule 103: selcollist ::= sclp scanpt STAR
func ruleSelcollistSclpScanptStar(ruleNo int, p *Parser) interface{} {
	prev := getSelectColumns(getRHS(p, ruleNo, 1))
	return append(prev, sql.SelectColumn{Expr: &sql.ColumnRef{Name: "*"}})

}

// Rule 104: selcollist ::= sclp scanpt nm DOT STAR
func ruleSelcollistSclpScanptNmDotStar(ruleNo int, p *Parser) interface{} {
	tbl := getString(getRHS(p, ruleNo, 3))
	prev := getSelectColumns(getRHS(p, ruleNo, 1))
	return append(prev, sql.SelectColumn{Expr: &sql.ColumnRef{Table: tbl, Name: "*"}})

}

// Rule 105: as ::= AS nm
func ruleAsAsNm(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 2))

}

// Rule 106: as ::=
func ruleAs(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 107: from ::=
func ruleFrom(ruleNo int, p *Parser) interface{} {
	return sql.TableRef{}

}

// Rule 108: from ::= FROM seltablist
func ruleFromFromSeltablist(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 2)

}

// Rule 109: stl_prefix ::= seltablist joinop
// Combine the accumulated seltablist with the join operator that follows.
// The joinop (COMMA or JOIN) marks how the NEXT table will be joined.
func ruleStlPrefixSeltablistJoinop(ruleNo int, p *Parser) interface{} {
	acc := getSeltablist(getRHS(p, ruleNo, 1))
	op := getJoinOp(getRHS(p, ruleNo, 2))
	acc.PendingOp = op
	return acc

}

// Rule 110: stl_prefix ::= (empty)
func ruleStlPrefixEmpty(ruleNo int, p *Parser) interface{} {
	return &seltablistAcc{}

}

// Rule 111: seltablist ::= stl_prefix nm dbnm as on_using
func ruleSeltablistStlPrefixNmDbnmAsOnUsing(ruleNo int, p *Parser) interface{} {
	return appendSeltablistTable(p, ruleNo, 2, 3, 4, 0, 5)

}

// Rule 112: seltablist ::= stl_prefix nm dbnm as indexed_by on_using
func ruleSeltablistStlPrefixNmDbnmAsIndexedByOnUsing(ruleNo int, p *Parser) interface{} {
	return appendSeltablistTable(p, ruleNo, 2, 3, 4, 5, 6)

}

// Rule 113: seltablist ::= stl_prefix nm dbnm LP exprlist RP as on_using
// Table-valued function in FROM: pragma_table_info('t1').
func ruleSeltablistStlPrefixNmDbnmLpExprlistRpAsOnUsing(ruleNo int, p *Parser) interface{} {
	acc := getSeltablist(getRHS(p, ruleNo, 1))
	tbl := getString(getRHS(p, ruleNo, 2))
	schema := getString(getRHS(p, ruleNo, 3))
	args := getExprList(getRHS(p, ruleNo, 5))
	alias := getString(getRHS(p, ruleNo, 7))
	on, using := getOnUsing(getRHS(p, ruleNo, 8))
	if schema != "" {
		tbl = tbl + "." + schema
	}
	return acc.appendTableWithOn(p, sql.TableRef{Name: tbl, As: alias, Args: args, IsTabFunc: true}, on, using)

}

// Rule 114: seltablist ::= stl_prefix LP select RP as on_using
func ruleSeltablistStlPrefixLpSelectRpAsOnUsing(ruleNo int, p *Parser) interface{} {
	acc := getSeltablist(getRHS(p, ruleNo, 1))
	sel := getSelectStmt(getRHS(p, ruleNo, 3))
	alias := getString(getRHS(p, ruleNo, 5))
	on, using := getOnUsing(getRHS(p, ruleNo, 6))
	ref := sql.TableRef{Subquery: sel, As: alias}
	return acc.appendTableWithOn(p, ref, on, using)

}

// Rule 115: seltablist ::= stl_prefix LP seltablist RP as on_using
// Parenthesized table list: FROM (t1) or FROM (t1, t2).
// A parenthesized comma list is flattened into the outer query (SQLite
// treats (t1, t2) as a group). A parenthesized JOIN group — FROM
// (t1 JOIN t2 ON ...) — is a derived table (subquery): its joins must
// stay inside the parens so an outer join sees the group as one unit
// (e.g. FROM t2 LEFT JOIN (dual JOIN t1 ON true) ON b=c).
func ruleSeltablistStlPrefixLpSeltablistRpAsOnUsing(ruleNo int, p *Parser) interface{} {
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
		return acc.appendTableWithOn(p, ref, on, using)
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
	acc = acc.appendTableWithOn(p, ref, on, using)
	for _, j := range inner.Joins {
		acc = acc.appendJoin(j)
	}
	return acc

}

// Rule 124: joinop ::= COMMA|JOIN
func ruleJoinopCommaJoin(ruleNo int, p *Parser) interface{} {
	// comma join (FROM a, b) and a plain JOIN keyword (INNER JOIN).
	// Distinguish by the token value: "," is a comma join, "JOIN" is INNER.
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok && tok.Value == "," {
		return joinOp{Comma: true}
	}
	return joinOp{Kind: "INNER"}

}

// Rule 125: joinop ::= JOIN_KW JOIN
func ruleJoinopJoinKwJoin(ruleNo int, p *Parser) interface{} {
	return joinOpFromKeywords(p, getString(getRHS(p, ruleNo, 1)))

}

// Rule 126: joinop ::= JOIN_KW nm JOIN
// "NATURAL LEFT JOIN" has JOIN_KW=NATURAL and nm=LEFT; the nm join type
// must be preserved so exec can NULL-fill the correct side (SQLite's
// sqlite3JoinType ORs JT_NATURAL with the JOIN_KW/nm flags).
func ruleJoinopJoinKwNmJoin(ruleNo int, p *Parser) interface{} {
	return joinOpFromKeywords(p, getString(getRHS(p, ruleNo, 1)), getString(getRHS(p, ruleNo, 2)))

}

// Rule 127: joinop ::= JOIN_KW nm nm JOIN
func ruleJoinopJoinKwNmNmJoin(ruleNo int, p *Parser) interface{} {
	// joinop ::= JOIN_KW nm nm JOIN: all THREE keyword slots go to
	// sqlite3JoinType, whose error names every keyword as written
	// ("NATURAL AWK SED", join-1.2.3).
	return joinOpFromKeywords(p, getString(getRHS(p, ruleNo, 1)), getString(getRHS(p, ruleNo, 2)), getString(getRHS(p, ruleNo, 3)))

}

// Rule 128: on_using ::= ON expr
func ruleOnUsingOnExpr(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 129: on_using ::= USING LP idlist RP — the USING column list.
func ruleOnUsingUsing(ruleNo int, p *Parser) interface{} {
	return getStringList(getRHS(p, ruleNo, 3))

}

// Rule 130: on_using ::=
func ruleOnUsing(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 131: scanpt ::= (empty) — zero-width scan-position marker
func ruleScanpt(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 132: indexed_by ::= INDEXED BY nm
// Returns the index name. Consumers currently ignore indexed_by.
func ruleIndexedByIndexedByNm(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 3))

}

// Rule 133: indexed_by ::= NOT INDEXED
// Marks the table reference as NOT INDEXED (no index hints).
// Consumers currently ignore indexed_by; this returns a non-nil marker
// so the rule does not fall through to a nil passthrough.
func ruleIndexedByNotIndexed(ruleNo int, p *Parser) interface{} {
	return "NOT INDEXED"

}

// Rule 134: orderby_opt ::=
func ruleOrderbyOpt(ruleNo int, p *Parser) interface{} {
	return ([]sql.OrderByTerm)(nil)

}

// Rule 135: orderby_opt ::= ORDER BY sortlist
func ruleOrderbyOptOrderBySortlist(ruleNo int, p *Parser) interface{} {
	return getOrderByList(getRHS(p, ruleNo, 3))

}

// Rule 136: sortlist ::= sortlist COMMA expr sortorder nulls
func ruleSortlistSortlistCommaExprSortorderNulls(ruleNo int, p *Parser) interface{} {
	acc := getOrderByList(getRHS(p, ruleNo, 1))
	expr := getExpr(getRHS(p, ruleNo, 3))
	desc := getRHS(p, ruleNo, 4) == "DESC"
	nf, nl := getNullsOrder(getRHS(p, ruleNo, 5))
	return append(acc, sql.OrderByTerm{Expr: expr, Desc: desc, NullsFirst: nf, NullsLast: nl})

}

// Rule 137: sortlist ::= expr sortorder nulls
func ruleSortlistExprSortorderNulls(ruleNo int, p *Parser) interface{} {
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
func ruleSortorderAsc(ruleNo int, p *Parser) interface{} {
	return "ASC"

}

// Rule 139: sortorder ::= DESC
func ruleSortorderDesc(ruleNo int, p *Parser) interface{} {
	return "DESC"

}

// Rule 140: sortorder ::=
func ruleSortorder(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 141: nulls ::= NULLS FIRST
func ruleNullsNullsFirst(ruleNo int, p *Parser) interface{} {
	return nullsOrder{first: true}

}

// Rule 142: nulls ::= NULLS LAST
func ruleNullsNullsLast(ruleNo int, p *Parser) interface{} {
	return nullsOrder{last: true}

}

// Rule 143: nulls ::= (empty)
func ruleNullsEmpty(ruleNo int, p *Parser) interface{} {
	return nullsOrder{}

}

// Rule 144: groupby_opt ::=
func ruleGroupbyOpt(ruleNo int, p *Parser) interface{} {
	return ([]sql.Expr)(nil)

}

// Rule 145: groupby_opt ::= GROUP BY nexprlist
func ruleGroupbyOptGroupByNexprlist(ruleNo int, p *Parser) interface{} {
	return getExprList(getRHS(p, ruleNo, 3))

}

// Rule 146: having_opt ::=
func ruleHavingOpt(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 147: having_opt ::= HAVING expr
func ruleHavingOptHavingExpr(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 148: limit_opt ::=
func ruleLimitOpt(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 149: limit_opt ::= LIMIT expr
func ruleLimitOptLimitExpr(ruleNo int, p *Parser) interface{} {
	return &limitClause{limit: getExpr(getRHS(p, ruleNo, 2))}

}

// Rule 150: limit_opt ::= LIMIT expr OFFSET expr
func ruleLimitOptLimitExprOffsetExpr(ruleNo int, p *Parser) interface{} {
	return &limitClause{
		limit:  getExpr(getRHS(p, ruleNo, 2)),
		offset: getExpr(getRHS(p, ruleNo, 4)),
	}

}

// Rule 151: limit_opt ::= LIMIT expr COMMA expr
func ruleLimitOptLimitExprCommaExpr(ruleNo int, p *Parser) interface{} {
	// SQLite's LIMIT expr, expr form: first expr is the OFFSET.
	return &limitClause{
		offset: getExpr(getRHS(p, ruleNo, 2)),
		limit:  getExpr(getRHS(p, ruleNo, 4)),
	}

}

// Rule 153: where_opt ::=
func ruleWhereOpt(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 154: where_opt ::= WHERE expr
func ruleWhereOptWhereExpr(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 309: with ::= WITH wqlist
// The wqlist value is []sql.CTEDef; propagate it as the with value so
// INSERT (rule 164) can attach the CTEs.
func ruleWithWithWqlist(ruleNo int, p *Parser) interface{} {
	return getCTEDefs(getRHS(p, ruleNo, 2))

}

// Rule 310: with ::= WITH RECURSIVE wqlist
// Mark every CTE as recursive (WITH RECURSIVE applies to the whole list).
func ruleWithWithRecursiveWqlist(ruleNo int, p *Parser) interface{} {
	defs := getCTEDefs(getRHS(p, ruleNo, 3))
	for i := range defs {
		defs[i].Recursive = true
	}
	return defs

}

// Rule 311: wqas ::= AS
// The materialization hint (MATERIALIZED / NOT MATERIALIZED) is not
// modeled; pass through a marker value.
func ruleWqasAs(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 314: wqitem ::= withnm eidlist_opt wqas LP select RP
func ruleWqitemWithnmEidlistOptWqasLpSelectRp(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	cols := getStringList(getRHS(p, ruleNo, 2))
	sel := getSelectStmt(getRHS(p, ruleNo, 5))
	return sql.CTEDef{Name: name, Columns: cols, Select: sel}

}

// Rule 315: withnm ::= nm
func ruleWithnmNm(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 316: wqlist ::= wqitem
func ruleWqlistWqitem(ruleNo int, p *Parser) interface{} {
	if d, ok := getRHS(p, ruleNo, 1).(sql.CTEDef); ok {
		return []sql.CTEDef{d}
	}
	return nil

}

// Rule 317: wqlist ::= wqlist COMMA wqitem
func ruleWqlistListCommaWqitem(ruleNo int, p *Parser) interface{} {
	defs := getCTEDefs(getRHS(p, ruleNo, 1))
	if d, ok := getRHS(p, ruleNo, 3).(sql.CTEDef); ok {
		return append(defs, d)
	}
	return defs

}

// Rule 380: selectnowith ::= oneselect (already handled, but keep for pass-through)
func ruleSelectnowithOneselect(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 381: oneselect ::= values
func ruleOneselectValues(ruleNo int, p *Parser) interface{} {
	sel := getSelectStmt(getRHS(p, ruleNo, 1))
	if sel != nil {
		sel.ValuesChain = true
	}
	return sel

}

// Rule 383: as ::= ID|STRING
func ruleAsIdString(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return tok.Value
	}
	return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

}

// Rule 409: with ::=
func ruleWith(ruleNo int, p *Parser) interface{} {
	return nil

}

// joinOpFromKeywords merges the joinop's raw keyword texts into the joinOp,
// porting select.c sqlite3JoinType's error contract ("unknown join type",
// vtab6-3.7: INNER OUTER / LEFT BOGUS). On an invalid combination the parse
// carries the error via SemanticErr and the op degrades to INNER.
func joinOpFromKeywords(p *Parser, kws ...string) joinOp {
	kind, err := combineJoinKeywords(kws...)
	if err != nil {
		p.SemanticErr = err
		return joinOp{Kind: "INNER"}
	}
	return joinOp{Kind: kind, Outer: true}
}

// valuesChainArmWidth counts the result columns of one VALUES/SELECT arm;
// star reports a "*" column (which cannot be counted statically).
func valuesChainArmWidth(cur *sql.SelectStmt) (n int, star bool) {
	for _, col := range cur.Columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			return 0, true
		}
		n++
	}
	return n, false
}

// checkValuesChainWidths implements sqlite3SelectWrongNumTermsError during
// compound generation: a VALUES chain whose arms have differing widths names
// the set op (select4-11.16: "INSERT INTO t2(rowid) VALUES(2) UNION SELECT 3,4"
// reports the UNION, not the INSERT column-count check). Arms with a star
// cannot be counted statically and abort the check.
func checkValuesChainWidths(sel *sql.SelectStmt) (badValues bool, badOp string) {
	width := -1
	for cur := sel; cur != nil && badOp == ""; cur = cur.Union {
		n, star := valuesChainArmWidth(cur)
		if star {
			break
		}
		if width >= 0 && n != width {
			if !cur.ExplicitSetOp {
				// Comma-linked VALUES row: the SF_Values branch of
				// sqlite3SelectWrongNumTermsError (values-2.1.x).
				badValues = true
			} else {
				badOp = opNameOf(cur)
			}
			break
		}
		width = n
	}
	return badValues, badOp
}
