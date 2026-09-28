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

// Rule 124: joinop ::= COMMA|JOIN
func ruleJoinopCommaJoin(ruleNo int, p *Parser) interface{} {
	// Rule 124: joinop ::= COMMA|JOIN — the multiterminal covers both a
	// comma join (FROM a, b) and a plain JOIN keyword (INNER JOIN).
	// Distinguish by the token value: "," is a comma join, "JOIN" is INNER.
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok && tok.Value == "," {
		return joinOp{Comma: true}
	}
	return joinOp{Kind: "INNER"}

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

func rule127(ruleNo int, p *Parser) interface{} {
	// joinop ::= JOIN_KW nm nm JOIN: all THREE keyword slots go to
	// sqlite3JoinType, whose error names every keyword as written
	// ("NATURAL AWK SED", join-1.2.3).
	return joinOpFromKeywords(p, getString(getRHS(p, ruleNo, 1)), getString(getRHS(p, ruleNo, 2)), getString(getRHS(p, ruleNo, 3)))

}

// Rule 128: joinop ::= JOIN_KW nm JOIN
func ruleJoinopJoinKwNmJoinN128(ruleNo int, p *Parser) interface{} {
	// Rule 128: on_using ::= ON expr — the ON condition for a JOIN.
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 129: on_using ::= USING LP idlist RP — the USING column list.
func ruleOnUsingUsingLpIdlistRpTheUsingColumnList(ruleNo int, p *Parser) interface{} {
	return getStringList(getRHS(p, ruleNo, 3))

}

// Rule 130: on_using ::=
func ruleOnUsing(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 131: on_using ::=
func ruleOnUsingN131(ruleNo int, p *Parser) interface{} {
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

func rule135(ruleNo int, p *Parser) interface{} {
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

func rule143(ruleNo int, p *Parser) interface{} {
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

func rule151(ruleNo int, p *Parser) interface{} {
	// SQLite's LIMIT expr, expr form: first expr is the OFFSET.
	return &limitClause{
		offset: getExpr(getRHS(p, ruleNo, 2)),
		limit:  getExpr(getRHS(p, ruleNo, 4)),
	}

}

// Rule 152: cmd ::= with DELETE FROM xfullname indexed_opt where_opt_ret
func ruleCmdWithDeleteFromXfullnameIndexedOptWhereOptRet(ruleNo int, p *Parser) interface{} {
	tbl := getString(getRHS(p, ruleNo, 4))
	wr := getWhereRet(getRHS(p, ruleNo, 6))
	stmt := &sql.DeleteStmt{Table: tbl, CTEs: getCTEDefs(getRHS(p, ruleNo, 1))}
	stmt.Alias = p.pendingDMLAlias
	p.pendingDMLAlias = ""
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
func ruleWhereOpt(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 154: where_opt ::= WHERE expr
func ruleWhereOptWhereExpr(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 155: where_opt_ret ::=
func ruleWhereOptRet(ruleNo int, p *Parser) interface{} {
	return &whereRet{}

}

func rule156(ruleNo int, p *Parser) interface{} {
	return &whereRet{where: getExpr(getRHS(p, ruleNo, 2))}

}

// Rule 157: where_opt_ret ::= RETURNING selcollist
func ruleWhereOptRetReturningSelcollist(ruleNo int, p *Parser) interface{} {
	return &whereRet{returning: getSelectColumns(getRHS(p, ruleNo, 2))}

}

// Rule 158: where_opt_ret ::= WHERE expr RETURNING selcollist
func ruleWhereOptRetWhereExprReturningSelcollist(ruleNo int, p *Parser) interface{} {
	return &whereRet{
		where:     getExpr(getRHS(p, ruleNo, 2)),
		returning: getSelectColumns(getRHS(p, ruleNo, 4)),
	}

}

// Rule 159: cmd ::= with UPDATE orconf xfullname indexed_opt SET setlist from where_opt_ret
func ruleCmdWithUpdateOrconfXfullnameIndexedOptSetSetlistFromWhereOptRet(ruleNo int, p *Parser) interface{} {
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
	stmt.Alias = p.pendingDMLAlias
	p.pendingDMLAlias = ""
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
func ruleSetlistSetlistCommaNmEqExpr(ruleNo int, p *Parser) interface{} {
	acc := getAssignments(getRHS(p, ruleNo, 1))
	col := getString(getRHS(p, ruleNo, 3))
	val := getExpr(getRHS(p, ruleNo, 5))
	return append(acc, sql.Assignment{Column: col, Value: val})

}

// Rule 162: setlist ::= nm EQ expr
func ruleSetlistNmEqExpr(ruleNo int, p *Parser) interface{} {
	col := getString(getRHS(p, ruleNo, 1))
	val := getExpr(getRHS(p, ruleNo, 3))
	return []sql.Assignment{{Column: col, Value: val}}

}

// Rule 164: cmd ::= with insert_cmd INTO xfullname idlist_opt select upsert
func ruleCmdWithInsertCmdIntoXfullnameIdlistOptSelectUpsert(ruleNo int, p *Parser) interface{} {
	table := getString(getRHS(p, ruleNo, 4))
	columns := getStringList(getRHS(p, ruleNo, 5))
	sel := getSelectStmt(getRHS(p, ruleNo, 6))
	// A VALUES insert (INSERT INTO t VALUES(...),(...)) parses as a SELECT
	// with no FROM. Convert it into s.Values tuples and clear s.Select so
	// the engine uses the VALUES path (insertRow); a real INSERT...SELECT
	// (even one without a FROM clause) keeps s.Select.
	var values [][]sql.Expr
	if sel != nil && sel.ValuesChain {
		badValues, badOp := checkValuesChainWidths(sel)
		if badValues {
			p.SemanticErr = fmt.Errorf("all VALUES must have the same number of terms")
			return nil
		}
		if badOp != "" {
			p.SemanticErr = fmt.Errorf("SELECTs to the left and right of %s do not have the same number of result columns", badOp)
			return nil
		}
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
	stmt.Alias = p.pendingDMLAlias
	p.pendingDMLAlias = ""
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

// Rule 165: cmd ::= with insert_cmd INTO xfullname idlist_opt DEFAULT VALUES returning
func ruleCmdWithInsertCmdIntoXfullnameIdlistOptDefaultValuesReturning(ruleNo int, p *Parser) interface{} {
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
	stmt.Alias = p.pendingDMLAlias
	p.pendingDMLAlias = ""
	// The returning nonterminal (RHS 8) is either nil (rule 166) or a
	// []sql.SelectColumn from `RETURNING selcollist` (rule 167).
	if cols, ok := getRHS(p, ruleNo, 8).([]sql.SelectColumn); ok && len(cols) > 0 {
		stmt.Returning = foldReturning(cols)
		stmt.HasReturning = true
	}
	return stmt

}

// Rule 166: upsert ::=
func ruleUpsert(ruleNo int, p *Parser) interface{} {
	return &upsertVal{}

}

// Rule 167: upsert ::= RETURNING selcollist
func ruleUpsertReturningSelcollist(ruleNo int, p *Parser) interface{} {
	return &upsertVal{returning: getSelectColumns(getRHS(p, ruleNo, 2))}

}

// Rule 168: upsert ::= ON CONFLICT LP sortlist RP where_opt
//
//	DO UPDATE SET setlist where_opt upsert
func ruleUpsertOnConflictLpSortlistRpWhereOpt(ruleNo int, p *Parser) interface{} {
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
func ruleUpsertOnConflictLpSortlistRpWhereOptDoNothingUpsert(ruleNo int, p *Parser) interface{} {
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
func ruleUpsertOnConflictDoNothingReturning(ruleNo int, p *Parser) interface{} {
	return &upsertVal{
		onConflict: &sql.OnConflictClause{
			Action: sql.ConflictDoNothing,
		},
		returning: getSelectColumns(getRHS(p, ruleNo, 5)),
	}

}

// Rule 171: upsert ::= ON CONFLICT DO UPDATE SET setlist where_opt returning
func ruleUpsertOnConflictDoUpdateSetSetlistWhereOptReturning(ruleNo int, p *Parser) interface{} {
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
func ruleReturningReturningSelcollist(ruleNo int, p *Parser) interface{} {
	return getSelectColumns(getRHS(p, ruleNo, 2))

}

func rule173(ruleNo int, p *Parser) interface{} {
	// Return the orconf resolution type ("", "IGNORE", "REPLACE", ...).
	return getString(getRHS(p, ruleNo, 2))

}

// Rule 174: insert_cmd ::= REPLACE
func ruleInsertCmdReplace(ruleNo int, p *Parser) interface{} {
	return "REPLACE"

}

// Rule 175: idlist_opt ::=
func ruleIdlistOpt(ruleNo int, p *Parser) interface{} {
	return ([]string)(nil)

}

func rule176(ruleNo int, p *Parser) interface{} {
	return getStringList(getRHS(p, ruleNo, 2))

}

// Rule 177: idlist ::= idlist COMMA nm
func ruleIdlistIdlistCommaNm(ruleNo int, p *Parser) interface{} {
	acc := getStringList(getRHS(p, ruleNo, 1))
	return append(acc, getString(getRHS(p, ruleNo, 3)))

}

// Rule 178: idlist ::= nm
func ruleIdlistNm(ruleNo int, p *Parser) interface{} {
	return []string{getString(getRHS(p, ruleNo, 1))}

}

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

func rule184(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return &sql.StringLit{Value: tok.Value}
	}
	if s, ok := getRHS(p, ruleNo, 1).(string); ok {
		return &sql.StringLit{Value: s}
	}
	return &sql.StringLit{}

}
