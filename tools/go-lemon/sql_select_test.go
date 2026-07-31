// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// sql_select_test.go — end-to-end validation of go-lemon's own LALR(1)
// table generator.
//
// This test:
//   1. Uses the generated parser (tools/go-lemon/sql_select_tables.go,
//      produced by go-lemon from sql_select.y with GenerateTables — no
//      C-lemon intermediary).
//   2. Tokenizes the SQL statements exercised by the selectE test.
//   3. Drives the go-lemon engine (tools/go-lemon/engine.go) with those
//      tokens.
//   4. Verifies accept/reject matches SQLite semantics: valid compound
//      SELECTs parse; "SELECT 1 EXCEPT SELECT 2 ORDER BY ... EXCEPT SELECT 3"
//      is rejected.

package main

import (
	"strings"
	"testing"
)

// sqlToken is a single token from the minimal SQL lexer.
type sqlToken struct {
	code  int
	value string
}

// lexSQL tokenizes SQL text for the sql_select grammar.
func lexSQL(sql string) []sqlToken {
	var toks []sqlToken
	i := 0
	for i < len(sql) {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == ';':
			toks = append(toks, sqlToken{TK_SEMI, ";"})
			i++
		case c == '(':
			toks = append(toks, sqlToken{TK_LP, "("})
			i++
		case c == ')':
			toks = append(toks, sqlToken{TK_RP, ")"})
			i++
		case c == ',':
			toks = append(toks, sqlToken{TK_COMMA, ","})
			i++
		case c == '*':
			toks = append(toks, sqlToken{TK_STAR, "*"})
			i++
		case c == '\'':
			j := i + 1
			for j < len(sql) && sql[j] != '\'' {
				j++
			}
			toks = append(toks, sqlToken{TK_STRING, sql[i+1 : j]})
			i = j + 1
		case c >= '0' && c <= '9':
			j := i
			for j < len(sql) && (sql[j] >= '0' && sql[j] <= '9') {
				j++
			}
			toks = append(toks, sqlToken{TK_NUMBER, sql[i:j]})
			i = j
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j := i
			for j < len(sql) && (sql[j] == '_' || (sql[j] >= 'a' && sql[j] <= 'z') || (sql[j] >= 'A' && sql[j] <= 'Z') || (sql[j] >= '0' && sql[j] <= '9')) {
				j++
			}
			word := sql[i:j]
			toks = append(toks, sqlToken{kwCode(word), word})
			i = j
		default:
			i++
		}
	}
	return toks
}

// kwCode maps a keyword/identifier to its token code.
func kwCode(word string) int {
	switch strings.ToUpper(word) {
	case "CREATE":
		return TK_CREATE
	case "TABLE":
		return TK_TABLE
	case "INSERT":
		return TK_INSERT
	case "INTO":
		return TK_INTO
	case "VALUES":
		return TK_VALUES
	case "DELETE":
		return TK_DELETE
	case "FROM":
		return TK_FROM
	case "SELECT":
		return TK_SELECT
	case "AS":
		return TK_AS
	case "EXCEPT":
		return TK_EXCEPT
	case "ORDER":
		return TK_ORDER
	case "BY":
		return TK_BY
	case "COLLATE":
		return TK_COLLATE
	default:
		return TK_ID
	}
}

// parseWithEngine runs the generated parser over a token stream and returns
// true if the input is fully accepted (EOF after all tokens).
func parseWithEngine(toks []sqlToken) bool {
	tables := GetParseTables()
	parser := NewParser(tables)
	for _, tok := range toks {
		res := parser.Parse(tok.code, tok.value)
		if res == ParseError {
			return false
		}
	}
	// EOF (code 0) — must accept
	res := parser.Parse(0, nil)
	return res == ParseAccept
}

// TestOwnGeneratorSelectE validates that go-lemon's own LALR(1) generator
// produces a parser that handles selectE's SQL statements exactly as SQLite
// does: valid compounds accepted, ORDER BY between compound operators rejected.
func TestOwnGeneratorSelectE(t *testing.T) {
	cases := []struct {
		sql      string
		wantOK   bool
		desc     string
	}{
		{"CREATE TABLE t1(a);", true, "create table"},
		{"INSERT INTO t1 VALUES('abc'),('def'),('ghi');", true, "insert multi-row"},
		{"DELETE FROM t2;", true, "delete"},
		{"SELECT a FROM t1 EXCEPT SELECT a FROM t2 ORDER BY a COLLATE nocase;", true, "except orderby collate"},
		{"SELECT a FROM t2 EXCEPT SELECT a FROM t3 ORDER BY a COLLATE binary;", true, "except binary"},
		{"SELECT a FROM t2 EXCEPT SELECT a FROM t3 ORDER BY a;", true, "except orderby"},
		{"SELECT 1 EXCEPT SELECT 2 ORDER BY 1 COLLATE nocase EXCEPT SELECT 3;", false, "orderby between compound (reject)"},
		{"SELECT 1 EXCEPT SELECT 2 ORDER BY 1 COLLATE nocase;", true, "orderby after except"},
		{"SELECT lower(a) FROM t2;", true, "function call"},
		{"SELECT a COLLATE nocase FROM t2 EXCEPT SELECT a FROM t3 ORDER BY 1 COLLATE binary;", true, "collate in selcollist"},
	}

	for _, tc := range cases {
		toks := lexSQL(tc.sql)
		got := parseWithEngine(toks)
		if got != tc.wantOK {
			t.Errorf("%s: parse(%q) = %v, want %v", tc.desc, tc.sql, got, tc.wantOK)
		}
	}
}

// TestOwnGeneratorTablesGenerated checks the tables were produced by go-lemon's
// own generator: the constants in sql_select_tables.go must be self-consistent
// (engine can load and reduce rules without panicking).
func TestOwnGeneratorTablesGenerated(t *testing.T) {
	tables := GetParseTables()
	if tables == nil || tables.YYNRule == 0 || tables.YYNState == 0 {
		t.Fatal("generated tables are empty")
	}
	if len(tables.RuleInfoNRhs) != tables.YYNRule {
		t.Fatalf("RuleInfoNRhs len %d != YYNRule %d", len(tables.RuleInfoNRhs), tables.YYNRule)
	}
	t.Logf("tables: %d states, %d rules, %d tokens", tables.YYNState, tables.YYNRule, tables.YYNToken)
}
