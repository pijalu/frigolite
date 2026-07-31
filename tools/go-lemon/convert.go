// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// convert.go — Converts C lemon parse tables to Go format.
//
// Usage: go-lemon -convert parse.c > tables.go
//
// This reads the C output of the original lemon (parse.c) and
// extracts the LALR(1) parse tables into Go data structures
// that the go-lemon engine can use.

package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ConvertTables reads a parse.c file and extracts the parse tables
// into a Go-compatible format.
func ConvertTables(inputFile, outputFile string) error {
	data, err := os.ReadFile(inputFile)
	if err != nil {
		return fmt.Errorf("reading parse.c: %w", err)
	}

	content := string(data)
	tables := &ParseTables{}
	tokenNames := make(map[int]string)
	tokenCodes := make(map[string]int)

	// Extract parse tables
	extractActionTable(content, tables)
	extractLookaheadTable(content, tables)
	extractShiftOfst(content, tables)
	extractReduceOfst(content, tables)
	extractDefaultActions(content, tables)
	extractRuleInfo(content, tables)
	extractTokenNames(content, tokenNames, tokenCodes)
	extractFallback(content, tables)
	extractConstants(content, tables)

// Generate Go output
	goCode := "package main\n\n"
	goCode += generateTablesGo(tables)
	
	// Add token names for tracing
	goCode += "var yyTokenName = map[int]string{\n"
	for code := 0; code < len(tokenNames); code++ {
		if name, ok := tokenNames[code]; ok {
			goCode += fmt.Sprintf("\t%d: %q,\n", code, name)
		}
	}
	goCode += "}\n\n"

	// Add fallback if present
	if tables.YYFallback != nil {
		goCode += "var yyFallback = []int{\n"
		for i, fb := range tables.YYFallback {
			if i%10 == 0 {
				goCode += "\t"
			}
			goCode += fmt.Sprintf("%d, ", fb)
			if i%10 == 9 {
				goCode += "\n"
			}
		}
		goCode += "\n}\n\n"
	}

	// Add code to initialize the ParseTables
	goCode += `// GetParseTables returns the LALR(1) parse tables for the SQL grammar.
func GetParseTables() *ParseTables {
	return &ParseTables{
		Action:    yyAction,
		Lookahead: yyLookahead,
		ShiftOfst: yyShiftOfst,
		ReduceOfst: yyReduceOfst,
		Default:   yyDefault,
`
	// Add rule info
	goCode += "\t\tRuleInfoLhs: yyRuleInfoLhs,\n"
	goCode += "\t\tRuleInfoNRhs: yyRuleInfoNRhs,\n"
	goCode += "\t\tTokenName: nil, // set from yyTokenName map\n"
	
	// Add constants
	goCode += fmt.Sprintf("\t\tYYNToken: %d,\n", tables.YYNToken)
	goCode += fmt.Sprintf("\t\tYYNState: %d,\n", tables.YYNState)
	goCode += fmt.Sprintf("\t\tYYNRule: %d,\n", tables.YYNRule)
	goCode += fmt.Sprintf("\t\tYYMaxShift: %d,\n", tables.YYMaxShift)
	goCode += fmt.Sprintf("\t\tYYMinReduce: %d,\n", tables.YYMinReduce)
	goCode += fmt.Sprintf("\t\tYYMaxReduce: %d,\n", tables.YYMaxReduce)
	goCode += fmt.Sprintf("\t\tYYErrorAction: %d,\n", tables.YYErrorAction)
	goCode += fmt.Sprintf("\t\tYYAcceptAction: %d,\n", tables.YYAcceptAction)
	goCode += fmt.Sprintf("\t\tYYNoAction: %d,\n", tables.YYNoAction)
	goCode += "\t}\n}\n"

	if err := os.WriteFile(outputFile, []byte(goCode), 0644); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

func extractArray(content, varName string) ([]int, error) {
	// Find the array definition: static const TYPE varName[] = { ... };
	// Match from the first occurrence of varName to the closing };
	re := regexp.MustCompile(varName + `\[\] = \{([^}]*)\}`)
	match := re.FindStringSubmatch(content)
	if len(match) < 2 {
		return nil, fmt.Errorf("array %s not found", varName)
	}

	body := match[1]
	// Remove C-style comments /* ... */
	body = regexp.MustCompile(`/\*.*?\*/`).ReplaceAllString(body, " ")
	var result []int
	for _, token := range strings.Fields(body) {
		token = strings.TrimRight(token, ",")
		if token == "" {
			continue
		}
		val, err := strconv.Atoi(token)
		if err != nil {
			continue // skip non-numeric tokens
		}
		result = append(result, val)
	}
	return result, nil
}

func extractActionTable(content string, tables *ParseTables) {
	vals, err := extractArray(content, "yy_action")
	if err != nil {
		return
	}
	tables.Action = make([]ParserAction, len(vals))
	for i, v := range vals {
		tables.Action[i] = ParserAction(v)
	}
}

func extractLookaheadTable(content string, tables *ParseTables) {
	vals, err := extractArray(content, "yy_lookahead")
	if err != nil {
		return
	}
	tables.Lookahead = vals
}

func extractShiftOfst(content string, tables *ParseTables) {
	vals, err := extractArray(content, "yy_shift_ofst")
	if err != nil {
		return
	}
	tables.ShiftOfst = vals
}

func extractReduceOfst(content string, tables *ParseTables) {
	vals, err := extractArray(content, "yy_reduce_ofst")
	if err != nil {
		return
	}
	tables.ReduceOfst = vals
}

func extractDefaultActions(content string, tables *ParseTables) {
	vals, err := extractArray(content, "yy_default")
	if err != nil {
		return
	}
	tables.Default = make([]ParserAction, len(vals))
	for i, v := range vals {
		tables.Default[i] = ParserAction(v)
	}
}

func extractRuleInfo(content string, tables *ParseTables) {
	// Extract yyRuleInfoLhs[]
	vals, err := extractArray(content, "yyRuleInfoLhs")
	if err == nil {
		tables.RuleInfoLhs = vals
	}

	// Extract yyRuleInfoNRhs[]
	vals, err = extractArray(content, "yyRuleInfoNRhs")
	if err == nil {
		tables.RuleInfoNRhs = vals
	}
}

func extractTokenNames(content string, tokenNames map[int]string, tokenCodes map[string]int) {
	// Extract yyTokenName array
	re := regexp.MustCompile(`yyTokenName\[\] = \{([^}]*)\}`)
	match := re.FindStringSubmatch(content)
	if len(match) < 2 {
		return
	}
	
	body := match[1]
	scanner := bufio.NewScanner(strings.NewReader(body))
	idx := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "/*") || line == "" {
			continue
		}
		// Extract string between quotes
		if strings.Contains(line, "\"") {
			parts := strings.Split(line, "\"")
			if len(parts) >= 2 {
				name := parts[1]
				tokenNames[idx] = name
				if name != "" {
					tokenCodes[name] = idx
				}
			}
		}
		idx++
	}
}

func extractFallback(content string, tables *ParseTables) {
	// Extract yyFallback array if present
	vals, err := extractArray(content, "yyFallback")
	if err != nil {
		return
	}
	tables.YYFallback = vals
}

func extractConstants(content string, tables *ParseTables) {
	extractConstant := func(name string) int {
		re := regexp.MustCompile(`#define\s+` + name + `\s+(\d+)`)
		match := re.FindStringSubmatch(content)
		if len(match) >= 2 {
			val, _ := strconv.Atoi(match[1])
			return val
		}
		return 0
	}

	tables.YYNToken = extractConstant("YYNTOKEN")
	tables.YYNState = extractConstant("YYNSTATE")
	tables.YYNRule = extractConstant("YYNRULE")
	tables.YYMaxShift = ParserAction(extractConstant("YY_MAX_SHIFT"))
	tables.YYMinReduce = ParserAction(extractConstant("YY_MIN_REDUCE"))
	tables.YYMaxReduce = ParserAction(extractConstant("YY_MAX_REDUCE"))
	tables.YYErrorAction = ParserAction(extractConstant("YY_ERROR_ACTION"))
	tables.YYAcceptAction = ParserAction(extractConstant("YY_ACCEPT_ACTION"))
	tables.YYNoAction = ParserAction(extractConstant("YY_NO_ACTION"))
	
	// Compute derived constants
	tables.YYShiftCount = int(tables.YYMaxShift)
	tables.YYReduceCount = int(tables.YYMinReduce) // rough
	tables.YYMinShiftReduce = ParserAction(tables.YYMaxShift + 1)
	tables.YYMaxShiftReduce = ParserAction(tables.YYMinReduce - 1)
	tables.YYActTabCount = len(tables.Action)
	tables.YYNoCode = -1
	tables.YYWildcard = extractConstant("YYWILDCARD")
	if tables.YYWildcard == 0 {
		tables.YYWildcard = -1 // no wildcard
	}
}
