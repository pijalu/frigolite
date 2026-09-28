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
// Trigger grammar: trigger declaration header, trigger_cmd_list, and
// the per-statement trigger_cmd forms (UPDATE/INSERT/DELETE/SELECT
// inside a trigger body).

package parse

import (
	sql "github.com/pijalu/frigolite/internal/sql"
	"strings"
)

// Rule 260: cmd ::= createkw trigger_decl BEGIN trigger_cmd_list END
func ruleCmdCreatekwTriggerDeclBeginTriggerCmdListEnd(ruleNo int, p *Parser) interface{} {
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

// Rule 261: trigger_decl ::= TRIGGER ifnotexists nm dbnm trigger_time trigger_event ON nm ... when_clause (11 RHS symbols)
func ruleTriggerDecl(ruleNo int, p *Parser) interface{} {
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
func ruleTriggerCmdListTriggerCmdListTriggerCmdSemi(ruleNo int, p *Parser) interface{} {
	list := getStmtList(getRHS(p, ruleNo, 1))
	stmt := getStmt(getRHS(p, ruleNo, 2))
	if stmt != nil {
		list = append(list, stmt)
	}
	return list

}

// Rule 271: trigger_cmd_list ::= trigger_cmd SEMI
func ruleTriggerCmdListTriggerCmdSemi(ruleNo int, p *Parser) interface{} {
	stmt := getStmt(getRHS(p, ruleNo, 1))
	if stmt == nil {
		return []sql.Stmt(nil)
	}
	return []sql.Stmt{stmt}

}

// Rule 274: trigger_cmd ::= UPDATE orconf nm indexed_opt SET setlist from where_opt
func ruleTriggerCmdUpdate(ruleNo int, p *Parser) interface{} {
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

}

// Rule 275: trigger_cmd ::= with insert_cmd INTO nm idlist_opt select upsert (INSERT inside a trigger body)
func ruleTriggerCmdInsert(ruleNo int, p *Parser) interface{} {
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
func ruleTriggerCmdDeleteFromXfullnameTridxbyWhereOptScanpt(ruleNo int, p *Parser) interface{} {
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
func ruleTriggerCmdScanptSelectScanpt(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 2)

}
