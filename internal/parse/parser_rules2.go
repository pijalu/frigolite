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
	right.ExplicitSetOp = true
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
	return acc.appendTableWithOn(p, sql.TableRef{Name: tbl, As: alias, Args: args, IsTabFunc: true}, on, using)

}

// Rule 114: seltablist ::= stl_prefix LP select RP as on_using
func rule114(ruleNo int, p *Parser) interface{} {
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

// Rule 122: xfullname ::= nm AS nm — table alias. The value is the
// TABLE NAME (the alias is consumed into pendingDMLAlias so the DML
// statement rule can set stmt.Alias); the join-op productions are
// separate (rules 124+).
func rule122(ruleNo int, p *Parser) interface{} {
	p.pendingDMLAlias = getString(getRHS(p, ruleNo, 3))
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 123: xfullname ::= nm DOT nm AS nm
func rule123(ruleNo int, p *Parser) interface{} {
	// Rule 123: xfullname ::= nm DOT nm AS nm — schema-qualified target with
	// an alias ("INSERT INTO main.t1 AS t2(a,b)"). The alias is consumed
	// (SQLite keeps it in SrcList->a[0].zAlias); the value is the
	// "schema.table" name. (This was mis-handled as a joinop production,
	// leaking a zero joinOp into the table-name slot — "%v" printed
	// "{ false false}" and every schema-qualified aliased DML target failed
	// with "no such table".)
	p.pendingDMLAlias = getString(getRHS(p, ruleNo, 5))
	return getString(getRHS(p, ruleNo, 1)) + "." + getString(getRHS(p, ruleNo, 3))
}
