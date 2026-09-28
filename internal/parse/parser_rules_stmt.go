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
// Statement dispatch: input/cmdlist/ecmd/cmdx, EXPLAIN, and the
// transaction commands (BEGIN/COMMIT/ROLLBACK/SAVEPOINT).

package parse

import (
	sql "github.com/pijalu/frigolite/internal/sql"
	"strings"
)

// Rule 0: explain ::= EXPLAIN
func ruleExplainPlain(ruleNo int, p *Parser) interface{} {
	return false // plain EXPLAIN (opcode dump)

}

// Rule 1: explain ::= EXPLAIN QUERY PLAN
func ruleExplainExplainQueryPlan(ruleNo int, p *Parser) interface{} {
	return true // EXPLAIN QUERY PLAN (plan output)

}

// Rule 2: cmdx ::= cmd
func ruleCmdxCmd(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 3: cmd ::= BEGIN transtype trans_opt
func ruleCmdBeginTranstypeTransOpt(ruleNo int, p *Parser) interface{} {
	// transtype is RHS element 2 (BEGIN is 1). Its Minor is the DEFERRED /
	// IMMEDIATE / EXCLUSIVE token (fallback passthrough) or nil for the empty
	// rule (parse.y L179-187: sqlite3BeginTransaction(pParse, Y) with
	// Y = TK_DEFERRED/TK_IMMEDIATE/TK_EXCLUSIVE).
	typ := strings.ToUpper(getString(getRHS(p, ruleNo, 2)))
	return &sql.BeginStmt{Type: typ}

}

// Rule 4: transtype ::= (empty)
func ruleTranstypeEmpty(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 8: cmd ::= COMMIT|END trans_opt
func ruleCmdCommitEndTransOpt(ruleNo int, p *Parser) interface{} {
	return &sql.CommitStmt{}

}

// Rule 9: cmd ::= ROLLBACK trans_opt
func ruleCmdRollbackTransOpt(ruleNo int, p *Parser) interface{} {
	return &sql.RollbackStmt{}

}

// Rule 348: input ::= cmdlist
func ruleInputCmdlist(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 349: cmdlist ::= cmdlist ecmd
func ruleCmdlistCmdlistEcmd(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 350: cmdlist ::= ecmd
func ruleCmdlistEcmd(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 351: ecmd ::= SEMI
func ruleEcmdSemi(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 352: ecmd ::= cmdx SEMI
func ruleEcmdCmdxSemi(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 353: ecmd ::= explain cmdx SEMI (EXPLAIN)
func ruleEcmdExplainCmdxSemiExplain(ruleNo int, p *Parser) interface{} {
	queryPlan := false
	if b, ok := getRHS(p, ruleNo, 1).(bool); ok {
		queryPlan = b
	}
	return &sql.ExplainStmt{
		Statement: getStmt(getRHS(p, ruleNo, 2)),
		QueryPlan: queryPlan,
	}

}

// Rule 354: trans_opt ::=
func ruleTransOpt(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 355: trans_opt ::= TRANSACTION
func ruleTransOptTransaction(ruleNo int, p *Parser) interface{} {
	return nil

}
