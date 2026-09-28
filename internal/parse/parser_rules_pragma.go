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
// PRAGMA command forms.

package parse

import (
	sql "github.com/pijalu/frigolite/internal/sql"
)

// Rule 253: cmd ::= PRAGMA nm dbnm
// The nm token is the pragma name, dbnm the optional schema qualifier.
// When dbnm is present (PRAGMA main.foreign_key_check), nm is the schema
// and dbnm the pragma name (mirroring sqlite3Pragma's swap).
func ruleCmdPragmaNmDbnm(ruleNo int, p *Parser) interface{} {
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
func ruleCmdPragmaNmDbnmPragmaValue(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 2))
	value := getString(getRHS(p, ruleNo, 5))
	schema := getString(getRHS(p, ruleNo, 3))
	if schema != "" {
		name, schema = schema, name
	}
	return &sql.PragmaStmt{
		Name:     name,
		Value:    value,
		Schema:   schema,
		HasValue: true,
	}

}

// Rule 255: cmd ::= PRAGMA nm dbnm LP pragma_value RP
// Rule 257: cmd ::= PRAGMA nm dbnm LP minus_num RP
// rule255 also implements rule(s) [257] (identical action).
func ruleCmdPragmaNmDbnmLpPragmaValueRp(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 2))
	value := getString(getRHS(p, ruleNo, 5))
	schema := getString(getRHS(p, ruleNo, 3))
	if schema != "" {
		name, schema = schema, name
	}
	return &sql.PragmaStmt{
		Name:     name,
		Value:    value,
		Schema:   schema,
		HasValue: true,
	}

}

// Rule 256: cmd ::= PRAGMA nm dbnm EQ minus_num
// (SQLite rule 1717: cmd ::= PRAGMA nm(X) dbnm(Z) EQ minus_num(Y))
// The minus_num value (e.g. -500) becomes the pragma Value.
func ruleCmdPragmaNmDbnmEqMinusNum(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 2))
	value := getString(getRHS(p, ruleNo, 5))
	schema := getString(getRHS(p, ruleNo, 3))
	if schema != "" {
		name, schema = schema, name
	}
	return &sql.PragmaStmt{
		Name:     name,
		Value:    value,
		Schema:   schema,
		HasValue: true,
	}

}
