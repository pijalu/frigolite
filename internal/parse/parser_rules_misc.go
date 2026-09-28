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
// Shared name/list/literal helpers: nm, dbnm, fullname, xfullname,
// idlist, and numeric literal tokens.

package parse

import (
	"fmt"
	sql "github.com/pijalu/frigolite/internal/sql"
)

// Rule 116: dbnm ::=
func ruleDbnm(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 117: dbnm ::= DOT nm
func ruleDbnmDotNm(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 2))

}

// Rule 118: fullname ::= nm
func ruleFullnameNm(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 119: fullname ::= nm DOT nm
func ruleFullnameNmDotNm(ruleNo int, p *Parser) interface{} {
	a := getString(getRHS(p, ruleNo, 1))
	b := getString(getRHS(p, ruleNo, 3))
	return a + "." + b

}

// Rule 121: xfullname ::= nm DOT nm (schema-qualified table name used by
// INSERT/UPDATE/DELETE, e.g. "temp.t2"). Produces "schema.table".
func ruleXfullnameNmDotNmSchemaQualifiedTableNameUsedBy(ruleNo int, p *Parser) interface{} {
	a := getString(getRHS(p, ruleNo, 1))
	b := getString(getRHS(p, ruleNo, 3))
	return a + "." + b

}

// Rule 122: xfullname ::= nm AS nm — table alias. The value is the
// TABLE NAME (the alias is consumed into pendingDMLAlias so the DML
// statement rule can set stmt.Alias); the join-op productions are
// separate (rules 124+).
func ruleXfullnameNmAsNm(ruleNo int, p *Parser) interface{} {
	p.pendingDMLAlias = getString(getRHS(p, ruleNo, 3))
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 123: xfullname ::= nm DOT nm AS nm
func ruleXfullnameNmDotNmAsNm(ruleNo int, p *Parser) interface{} {
	// an alias ("INSERT INTO main.t1 AS t2(a,b)"). The alias is consumed
	// (SQLite keeps it in SrcList->a[0].zAlias); the value is the
	// "schema.table" name. (This was mis-handled as a joinop production,
	// leaking a zero joinOp into the table-name slot — "%v" printed
	// "{ false false}" and every schema-qualified aliased DML target failed
	// with "no such table".)
	p.pendingDMLAlias = getString(getRHS(p, ruleNo, 5))
	return getString(getRHS(p, ruleNo, 1)) + "." + getString(getRHS(p, ruleNo, 3))
}

// Rule 175: idlist_opt ::=
func ruleIdlistOpt(ruleNo int, p *Parser) interface{} {
	return ([]string)(nil)

}

// Rule 176: idlist_opt ::= LP idlist RP
func ruleIdlistOptLpIdlistRp(ruleNo int, p *Parser) interface{} {
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

// Rule 259: minus_num ::= MINUS number
// SQLite's minus_num(A) ::= MINUS number(X). {A = X;} — the semantic
// value is the NUMBER token, not the minus. PRAGMA ...(-51) uses this
// rule so the pragma value must be "-51".
func ruleMinusNumMinusNumber(ruleNo int, p *Parser) interface{} {
	number := getRHS(p, ruleNo, 2)
	if tok, ok := number.(sql.Token); ok {
		return "-" + tok.Value
	}
	if s := getString(number); s != "" {
		return "-" + s
	}
	return "-"

}

// Rule 363: nm ::= ID|INDEXED|JOIN_KW
func ruleNmIdIndexedJoinKw(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return tok.Value
	}
	return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

}

// Rule 364: nm ::= STRING
func ruleNmString(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return tok.Value
	}
	return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

}

// Rule 395: plus_num ::= INTEGER|FLOAT
func rulePlusNumIntegerFloat(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return &sql.NumericLit{Value: tok.Value}
	}
	return getRHS(p, ruleNo, 1)

}
