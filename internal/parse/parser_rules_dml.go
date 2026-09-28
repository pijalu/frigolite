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
// DML commands: INSERT (incl. OR-conflict forms and DEFAULT VALUES),
// UPDATE, DELETE, upsert (ON CONFLICT), and RETURNING clauses.

package parse

import (
	"fmt"
	sql "github.com/pijalu/frigolite/internal/sql"
	"strings"
)

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

// Rule 155: where_opt_ret ::=
func ruleWhereOptRet(ruleNo int, p *Parser) interface{} {
	return &whereRet{}

}

// Rule 156: where_opt_ret ::= WHERE expr
func ruleWhereOptRetWhereExpr(ruleNo int, p *Parser) interface{} {
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
func ruleCmdUpdate(ruleNo int, p *Parser) interface{} {
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

// Rule 165: cmd ::= with insert_cmd INTO xfullname idlist_opt DEFAULT VALUES returning
func ruleCmdInsertDefaultValues(ruleNo int, p *Parser) interface{} {
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

// Rule 173: insert_cmd ::= OR resolvel
func ruleInsertCmdOrResolvel(ruleNo int, p *Parser) interface{} {
	// Return the orconf resolution type ("", "IGNORE", "REPLACE", ...).
	return getString(getRHS(p, ruleNo, 2))

}

// Rule 174: insert_cmd ::= REPLACE
func ruleInsertCmdReplace(ruleNo int, p *Parser) interface{} {
	return "REPLACE"

}

// Rule 385: returning ::= (empty)
func ruleReturningEmpty(ruleNo int, p *Parser) interface{} {
	return nil

}
