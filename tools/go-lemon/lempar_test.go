// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// lempar_test.go — end-to-end validation of the lempar.go template.
//
// This test verifies that:
//   1. InstantiateLempar reads tools/go-lemon/lempar.go and fills in the
//      package, token constants, parse tables, and action code for a grammar.
//   2. The generated, self-contained parser compiles with `go build`.
//   3. The generated parser (driven through a small main program) parses the
//      same SQL statements from selectE that the engine's parser handles.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLemparTemplateInstantiation checks the template fills in correctly.
func TestLemparTemplateInstantiation(t *testing.T) {
	grammar, err := ParseGrammar("sql_select.y")
	if err != nil {
		t.Fatalf("ParseGrammar: %v", err)
	}
	tables, err := GenerateTables(grammar)
	if err != nil {
		t.Fatalf("GenerateTables: %v", err)
	}
	tokenCode := make(map[string]int)
	code := 1
	for _, sym := range grammar.Symbols {
		if sym.Type == TermSymbol {
			tokenCode[sym.Name] = code
			code++
		}
	}

	out, err := InstantiateLempar(tables, grammar, tokenCode, "sqlparse")
	if err != nil {
		t.Fatalf("InstantiateLempar: %v", err)
	}

	for _, marker := range []string{
		"package __LEMON_PACKAGE__",
		"// __LEMON_TOKEN_CONSTANTS__",
		"// __LEMON_PARSE_TABLES__",
		"// __LEMON_ACTION_CODE__",
		"//go:build ignore",
	} {
		if strings.Contains(out, marker) {
			t.Errorf("marker still present in generated output: %q", marker)
		}
	}
	for _, want := range []string{
		"package sqlparse",
		"TK_SELECT =",
		"func GetParseTables() *ParseTables",
		"func yyReduceAction",
		"func NewParser(tables *ParseTables) *Parser",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated output missing %q", want)
		}
	}
	t.Logf("template instantiated OK (%d bytes)", len(out))
}

// TestLemparTemplateCompiles writes the generated parser to a temp module and
// verifies it builds as a standalone package.
func TestLemparTemplateCompiles(t *testing.T) {
	grammar, err := ParseGrammar("sql_select.y")
	if err != nil {
		t.Fatalf("ParseGrammar: %v", err)
	}
	tables, err := GenerateTables(grammar)
	if err != nil {
		t.Fatalf("GenerateTables: %v", err)
	}
	tokenCode := make(map[string]int)
	code := 1
	for _, sym := range grammar.Symbols {
		if sym.Type == TermSymbol {
			tokenCode[sym.Name] = code
			code++
		}
	}

	out, err := InstantiateLempar(tables, grammar, tokenCode, "sqlparse")
	if err != nil {
		t.Fatalf("InstantiateLempar: %v", err)
	}

	dir := t.TempDir()
	modFile := "module sqlparse\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(modFile), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "parser.go"), []byte(out), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated parser does not compile: %v\n%s", err, out)
	}
}

// TestLemparTemplateParsesSelectE drives the generated self-contained parser
// over selectE's SQL statements via a small driver main program and checks the
// accept/reject results.
func TestLemparTemplateParsesSelectE(t *testing.T) {
	grammar, err := ParseGrammar("sql_select.y")
	if err != nil {
		t.Fatalf("ParseGrammar: %v", err)
	}
	tables, err := GenerateTables(grammar)
	if err != nil {
		t.Fatalf("GenerateTables: %v", err)
	}
	tokenCode := make(map[string]int)
	code := 1
	for _, sym := range grammar.Symbols {
		if sym.Type == TermSymbol {
			tokenCode[sym.Name] = code
			code++
		}
	}

	out, err := InstantiateLempar(tables, grammar, tokenCode, "main")
	if err != nil {
		t.Fatalf("InstantiateLempar: %v", err)
	}

	// Driver program: tokenize each SQL case and print "OK" or "REJECT".
	driver := `package main

import (
	"fmt"
	"os"
	"strings"
)

type sqlToken struct {
	code  int
	value string
}

func kwCode(word string) int {
	switch strings.ToUpper(word) {
	case "CREATE": return TK_CREATE
	case "TABLE": return TK_TABLE
	case "INSERT": return TK_INSERT
	case "INTO": return TK_INTO
	case "VALUES": return TK_VALUES
	case "DELETE": return TK_DELETE
	case "FROM": return TK_FROM
	case "SELECT": return TK_SELECT
	case "AS": return TK_AS
	case "EXCEPT": return TK_EXCEPT
	case "ORDER": return TK_ORDER
	case "BY": return TK_BY
	case "COLLATE": return TK_COLLATE
	default: return TK_ID
	}
}

func lexSQL(sql string) []sqlToken {
	var toks []sqlToken
	i := 0
	for i < len(sql) {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == ';':
			toks = append(toks, sqlToken{TK_SEMI, ";"}); i++
		case c == '(':
			toks = append(toks, sqlToken{TK_LP, "("}); i++
		case c == ')':
			toks = append(toks, sqlToken{TK_RP, ")"}); i++
		case c == ',':
			toks = append(toks, sqlToken{TK_COMMA, ","}); i++
		case c == '*':
			toks = append(toks, sqlToken{TK_STAR, "*"}); i++
		case c == '\'':
			j := i + 1
			for j < len(sql) && sql[j] != '\'' { j++ }
			toks = append(toks, sqlToken{TK_STRING, sql[i+1 : j]}); i = j + 1
		case c >= '0' && c <= '9':
			j := i
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' { j++ }
			toks = append(toks, sqlToken{TK_NUMBER, sql[i:j]}); i = j
		default:
			j := i
			for j < len(sql) && (sql[j] == '_' || sql[j] >= 'a' && sql[j] <= 'z' || sql[j] >= 'A' && sql[j] <= 'Z' || sql[j] >= '0' && sql[j] <= '9') { j++ }
			word := sql[i:j]
			toks = append(toks, sqlToken{kwCode(word), word}); i = j
		}
	}
	return toks
}

func parseOK(sql string) bool {
	tables := GetParseTables()
	parser := NewParser(tables)
	for _, tok := range lexSQL(sql) {
		if parser.Parse(tok.code, tok.value) == ParseError {
			return false
		}
	}
	return parser.Parse(0, nil) == ParseAccept
}

func main() {
	cases := []struct {
		sql    string
		wantOK bool
	}{
		{"CREATE TABLE t1(a);", true},
		{"INSERT INTO t1 VALUES('abc'),('def'),('ghi');", true},
		{"DELETE FROM t2;", true},
		{"SELECT a FROM t1 EXCEPT SELECT a FROM t2 ORDER BY a COLLATE nocase;", true},
		{"SELECT a FROM t2 EXCEPT SELECT a FROM t3 ORDER BY a COLLATE binary;", true},
		{"SELECT a FROM t2 EXCEPT SELECT a FROM t3 ORDER BY a;", true},
		{"SELECT 1 EXCEPT SELECT 2 ORDER BY 1 COLLATE nocase EXCEPT SELECT 3;", false},
		{"SELECT 1 EXCEPT SELECT 2 ORDER BY 1 COLLATE nocase;", true},
		{"SELECT lower(a) FROM t2;", true},
		{"SELECT a COLLATE nocase FROM t2 EXCEPT SELECT a FROM t3 ORDER BY 1 COLLATE binary;", true},
	}
	for i, tc := range cases {
		got := parseOK(tc.sql)
		if got != tc.wantOK {
			fmt.Printf("case %d: parse(%q) = %v, want %v\n", i, tc.sql, got, tc.wantOK)
			os.Exit(1)
		}
	}
	fmt.Println("ALL_OK")
}
`

	dir := t.TempDir()
	modFile := "module sqlparse\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(modFile), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "parser.go"), []byte(out), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(driver), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driver failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "ALL_OK") {
		t.Fatalf("driver did not report ALL_OK:\n%s", output)
	}
	t.Logf("generated lempar parser parsed selectE SQL correctly")
}
