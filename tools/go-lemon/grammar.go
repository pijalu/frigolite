// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// grammar.go — Grammar data structures and file parser for go-lemon.

package main

import (
	"fmt"
	"os"
	"strings"
)

// LemonGrammar represents a complete Lemon grammar.
type LemonGrammar struct {
	// Directives
	TokenPrefix string // %token_prefix
	TokenType   string // %token_type
	DefaultType string // %default_type
	ExtraArg    string // %extra_argument
	ExtraCtx    string // %extra_context
	Name        string // %name
	Include     string // %include sections

	// Symbols
	Symbols     []*Symbol
	SymbolIndex map[string]*Symbol // name -> symbol
	TermCount   int                // number of terminal symbols
	NonTermCount int               // number of non-terminal symbols

	// Rules
	Rules []*Rule

	// Start symbol
	StartSymbol *Symbol

	// Destructors
	Destructors      map[int]string // symbol index -> destructor code
	TokenDestructors string         // %token_destructor code

	// Parse failure/syntax error/accept code
	ParseFailureCode string // %parse_failure
	SyntaxErrorCode  string // %syntax_error
	ParseAcceptCode  string // %parse_accept
	StackOverflowCode string // %stack_overflow

	// Token type info
	TokenTypeMap map[string]string // symbol name -> type
	DefaultTypeStr string
}

// SymbolType indicates whether a symbol is terminal or non-terminal.
type SymbolType int

const (
	TermSymbol    SymbolType = iota // Terminal (token)
	NonTermSymbol                   // Non-terminal
	MultiTerminal                   // Multi-part terminal
)

// Symbol represents a grammar symbol (terminal or non-terminal).
type Symbol struct {
	Index       int         // index in the symbol table
	Name        string      // symbol name
	Type        SymbolType  // terminal or non-terminal
	Rule        *Rule       // for non-terminals: the rule that defines this symbol
	Prec        int         // precedence level
	Assoc       string      // associativity: "LEFT", "RIGHT", "NONE", ""
	UseCnt      int         // number of times used in RHS of rules
	Fallback    int         // fallback token (terminal), -1 if none
	Destructor  string      // destructor code (empty if none)
	TokenType   string      // type for this symbol's semantic value
}

// Rule represents a grammar rule.
type Rule struct {
	Index       int     // rule number
	Lhs         *Symbol // left-hand side symbol
	Rhs         []*Symbol // right-hand side symbols
	Prec        int     // precedence of this rule
	Assoc       string  // associativity of this rule
	Code        string  // action code (from { ... })
	Line        int     // source line number
	Filename    string  // source filename
}

// NewLemonGrammar creates a new empty grammar.
func NewLemonGrammar() *LemonGrammar {
	return &LemonGrammar{
		SymbolIndex:    make(map[string]*Symbol),
		Destructors:    make(map[int]string),
		TokenTypeMap:   make(map[string]string),
	}
}

// GetOrCreateSymbol returns an existing symbol or creates a new one.
func (g *LemonGrammar) GetOrCreateSymbol(name string, typ SymbolType) *Symbol {
	if s, ok := g.SymbolIndex[name]; ok {
		if typ != MultiTerminal && s.Type == MultiTerminal && typ == TermSymbol {
			// Don't downgrade from multi to terminal
		} else if s.Type == TermSymbol && typ == NonTermSymbol {
			// Non-terminal definition overrides terminal usage
			s.Type = NonTermSymbol
		}
		return s
	}
	s := &Symbol{
		Index:     len(g.Symbols),
		Name:      name,
		Type:      typ,
		Prec:      -1,
		Fallback:  -1,
	}
	g.Symbols = append(g.Symbols, s)
	g.SymbolIndex[name] = s
	if typ == TermSymbol {
		g.TermCount++
	}
	return s
}

// ParseGrammar reads a Lemon grammar file (.y format) and returns a LemonGrammar.
func ParseGrammar(filename string) (*LemonGrammar, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("reading grammar file: %w", err)
	}

	g := NewLemonGrammar()
	text := string(data)
	
	// Split into lines for processing
	lines := strings.Split(text, "\n")

	// Pre-process lines to merge multi-line action code with rule lines.
	// Action code in { ... } can span multiple lines after a rule line.
	processedLines := make([]string, 0, len(lines))
	actionDepth := 0
	actionLines := []string{}
	inAction := false
	
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		
		if inAction {
			// Accumulate lines until the matching } is found
			for _, ch := range trimmed {
				if ch == '{' {
					actionDepth++
				} else if ch == '}' {
					actionDepth--
				}
			}
			actionLines = append(actionLines, line)
			if actionDepth <= 0 {
				// Action code complete — merge with the rule line
				merged := strings.Join(actionLines, "\n")
				processedLines = append(processedLines, merged)
				actionLines = nil
				inAction = false
			}
			i++
			continue
		}
		
		// Check if this line starts a rule that continues with action code
		if strings.Contains(trimmed, "::=") {
			// Count braces on this line
			braceCount := 0
			for _, ch := range trimmed {
				if ch == '{' {
					braceCount++
				} else if ch == '}' {
					braceCount--
				}
			}
			if braceCount > 0 {
				// Has opening brace without closing brace — action code continues
				inAction = true
				actionDepth = braceCount
				actionLines = []string{line}
				i++
				continue
			}
			// No brace on this line — check if the next line starts with {
			if i+1 < len(lines) {
				nextTrimmed := strings.TrimSpace(lines[i+1])
				if strings.HasPrefix(nextTrimmed, "{") {
					inAction = true
					actionDepth = 0
					actionLines = []string{line}
					i++
					continue
				}
			}
		}
		
		processedLines = append(processedLines, line)
		i++
	}
	
	// Handle unfinished action code at end of file
	if inAction && len(actionLines) > 0 {
		merged := strings.Join(actionLines, "\n")
		processedLines = append(processedLines, merged)
	}
	
	// Replace lines with processed version
	lines = processedLines
	
	// State machine for parsing
	type ParseState int
	const (
		StateNormal   ParseState = iota
		StateInclude  // inside %include { ... }
	)
	
	state := StateNormal
	includeLines := []string{}
	includeDepth := 0
	
	for lineNo, line := range lines {
		trimmed := strings.TrimSpace(line)
		
		switch state {
		case StateInclude:
			if includeDepth == 0 && strings.HasPrefix(trimmed, "%") {
				// End of include section
				g.Include = strings.Join(includeLines, "\n")
				state = StateNormal
				// Re-process this line
				lineNo--
				continue
			}
			// Count braces
			for _, ch := range trimmed {
				if ch == '{' {
					includeDepth++
				} else if ch == '}' {
					includeDepth--
				}
			}
			if includeDepth > 0 {
				includeLines = append(includeLines, line)
			} else {
				// Include block is complete
				if len(includeLines) > 0 || len(trimmed) > 0 {
					includeLines = append(includeLines, line)
				}
				g.Include = strings.Join(includeLines, "\n")
				includeLines = nil
				state = StateNormal
			}
			continue
		}
		
		if trimmed == "" || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "//") {
			continue
		}

		// Handle % directives
		if strings.HasPrefix(trimmed, "%") {
			if err := g.parseDirective(trimmed, lineNo+1, filename); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
			}
			continue
		}

		// Handle grammar rules: nonterminal ::= RHS .
		if strings.Contains(trimmed, "::=") {
			if err := g.parseRule(trimmed, lineNo+1, filename); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
			}
			continue
		}
	}

	// Count non-terminals
	for _, s := range g.Symbols {
		if s.Type == NonTermSymbol {
			g.NonTermCount++
		}
	}

	return g, nil
}

// parseDirective handles a % directive line.
func (g *LemonGrammar) parseDirective(line string, lineNo int, filename string) error {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}

	directive := fields[0]
	args := fields[1:]

	switch directive {
	case "%include":
		// %include { code } or %include "file"
		content := strings.TrimSpace(strings.TrimPrefix(line, "%include"))
		content = strings.TrimSpace(content)
		if strings.HasPrefix(content, "{") {
			// Multi-line include - handled by state machine
			g.Include = content[1 : len(content)-1]
		} else if strings.HasPrefix(content, "\"") {
			// File include - read file
			fname := strings.Trim(content, "\"")
			data, err := os.ReadFile(fname)
			if err != nil {
				return fmt.Errorf("cannot include %s: %w", fname, err)
			}
			g.Include += string(data) + "\n"
		} else {
			g.Include += content + "\n"
		}

	case "%token_prefix":
		if len(args) > 0 {
			g.TokenPrefix = args[0]
		}

	case "%token_type":
		content := strings.TrimSpace(strings.TrimPrefix(line, "%token_type"))
		content = strings.TrimSpace(content)
		g.TokenType = content

	case "%default_type":
		content := strings.TrimSpace(strings.TrimPrefix(line, "%default_type"))
		content = strings.TrimSpace(content)
		g.DefaultTypeStr = content
		g.DefaultType = content

	case "%type":
		// %type nonterminal { type }
		if len(args) >= 2 {
			typeStr := ""
			for i, a := range args {
				if a == "{" {
					typeStr = strings.Join(args[i+1:], " ")
					typeStr = strings.TrimRight(typeStr, "}")
					typeStr = strings.TrimSpace(typeStr)
					break
				}
				if i > 0 {
					_ = args[i-1]
				}
			}
			symName := args[0]
			sym := g.GetOrCreateSymbol(symName, NonTermSymbol)
			sym.TokenType = typeStr
			g.TokenTypeMap[symName] = typeStr
		}

	case "%name":
		if len(args) > 0 {
			g.Name = args[0]
		}

	case "%extra_argument":
		content := strings.TrimSpace(strings.TrimPrefix(line, "%extra_argument"))
		g.ExtraArg = content

	case "%extra_context":
		content := strings.TrimSpace(strings.TrimPrefix(line, "%extra_context"))
		g.ExtraCtx = content

	case "%left":
		// %left SYMBOL ...
		prec := g.nextPrec()
		for _, symName := range args {
			symName = strings.TrimRight(symName, ".")
			sym := g.GetOrCreateSymbol(symName, TermSymbol)
			sym.Assoc = "LEFT"
			sym.Prec = prec
		}

	case "%right":
		prec := g.nextPrec()
		for _, symName := range args {
			symName = strings.TrimRight(symName, ".")
			sym := g.GetOrCreateSymbol(symName, TermSymbol)
			sym.Assoc = "RIGHT"
			sym.Prec = prec
		}

	case "%nonassoc":
		prec := g.nextPrec()
		for _, symName := range args {
			symName = strings.TrimRight(symName, ".")
			sym := g.GetOrCreateSymbol(symName, TermSymbol)
			sym.Assoc = "NONE"
			sym.Prec = prec
		}

	case "%fallback":
		// %fallback ID X Y Z
		if len(args) >= 2 {
			fallback := args[0]
			for _, symName := range args[1:] {
				sym := g.GetOrCreateSymbol(symName, TermSymbol)
				fallbackSym := g.GetOrCreateSymbol(fallback, TermSymbol)
				sym.Fallback = fallbackSym.Index
			}
		}

	case "%destructor":
		// %destructOR symbol { code }
		content := strings.TrimSpace(strings.TrimPrefix(line, "%destructor"))
		// Parse: {code} symbol1 symbol2 ...
		if strings.Contains(content, "{") {
			braceStart := strings.Index(content, "{")
			braceEnd := strings.LastIndex(content, "}")
			if braceEnd > braceStart {
				code := content[braceStart+1 : braceEnd]
				code = strings.TrimSpace(code)
				symList := strings.Fields(content[braceEnd+1:])
				if len(symList) == 0 {
					// Apply to all
					g.TokenDestructors = code
				} else {
					for _, symName := range symList {
						if sym, ok := g.SymbolIndex[symName]; ok {
							g.Destructors[sym.Index] = code
						}
					}
				}
			}
		}

	case "%token_destructor":
		content := strings.TrimSpace(strings.TrimPrefix(line, "%token_destructor"))
		if strings.HasPrefix(content, "{") {
			content = strings.TrimPrefix(content, "{")
			content = strings.TrimSuffix(content, "}")
			content = strings.TrimSpace(content)
			g.TokenDestructors = content
		}

	case "%syntax_error":
		content := strings.TrimSpace(strings.TrimPrefix(line, "%syntax_error"))
		if strings.HasPrefix(content, "{") {
			content = strings.TrimPrefix(content, "{")
			content = strings.TrimSuffix(content, "}")
			content = strings.TrimSpace(content)
			g.SyntaxErrorCode = content
		}

	case "%parse_failure":
		content := strings.TrimSpace(strings.TrimPrefix(line, "%parse_failure"))
		if strings.HasPrefix(content, "{") {
			content = strings.TrimPrefix(content, "{")
			content = strings.TrimSuffix(content, "}")
			content = strings.TrimSpace(content)
			g.ParseFailureCode = content
		}

	case "%parse_accept":
		content := strings.TrimSpace(strings.TrimPrefix(line, "%parse_accept"))
		if strings.HasPrefix(content, "{") {
			content = strings.TrimPrefix(content, "{")
			content = strings.TrimSuffix(content, "}")
			content = strings.TrimSpace(content)
			g.ParseAcceptCode = content
		}

	case "%stack_overflow":
		content := strings.TrimSpace(strings.TrimPrefix(line, "%stack_overflow"))
		if strings.HasPrefix(content, "{") {
			content = strings.TrimPrefix(content, "{")
			content = strings.TrimSuffix(content, "}")
			content = strings.TrimSpace(content)
			g.StackOverflowCode = content
		}

	case "%stack_size":
		// %stack_size N
		if len(args) > 0 {
			// Ignore for Go
		}

	case "%start_symbol":
		if len(args) > 0 {
			if sym, ok := g.SymbolIndex[args[0]]; ok {
				g.StartSymbol = sym
			}
		}

	case "%stack_size_limit":
		// Ignore for Go
	case "%realloc", "%free":
		// Ignore for Go
	}

	return nil
}

var nextPrec int

func (g *LemonGrammar) nextPrec() int {
	nextPrec++
	return nextPrec
}

// parseRule parses a grammar rule line.
// Format: nonterminal ::= RHS1 RHS2 ... RHSn.
// Action code in { ... } at the end.
func (g *LemonGrammar) parseRule(line string, lineNo int, filename string) error {
	// Split by ::=
	parts := strings.SplitN(line, "::=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid rule syntax: %s", line)
	}

	lhsName := strings.TrimSpace(parts[0])
	rhsPart := parts[1]

	// Get or create LHS symbol
	lhs := g.GetOrCreateSymbol(lhsName, NonTermSymbol)

	// Parse RHS: symbols followed by optional action code
	rhsPart = strings.TrimSpace(rhsPart)

	// Extract action code from { ... }
	rhsStr := rhsPart
	actionCode := ""
	
	if braceIdx := strings.Index(rhsStr, "{"); braceIdx >= 0 {
		// Find matching closing brace
		depth := 0
		braceEnd := -1
		for i := braceIdx; i < len(rhsStr); i++ {
			if rhsStr[i] == '{' {
				depth++
			} else if rhsStr[i] == '}' {
				depth--
				if depth == 0 {
					braceEnd = i
					break
				}
			}
		}
		if braceEnd > braceIdx {
			actionCode = strings.TrimSpace(rhsStr[braceIdx+1 : braceEnd])
			rhsStr = strings.TrimSpace(rhsStr[:braceIdx])
		}
	}

	// Remove trailing dot
	rhsStr = strings.TrimRight(rhsStr, ".")

	// Parse RHS symbols
	rhsFields := strings.Fields(rhsStr)
	
	rule := &Rule{
		Index:    len(g.Rules),
		Lhs:      lhs,
		Rhs:      make([]*Symbol, 0),
		Prec:     -1,
		Code:     actionCode,
		Line:     lineNo,
		Filename: filename,
	}

	for _, field := range rhsFields {
		// Check for precedence marker [sym]
		if strings.HasPrefix(field, "[") && strings.HasSuffix(field, "]") {
			precName := field[1 : len(field)-1]
			if precSym, ok := g.SymbolIndex[precName]; ok {
				rule.Prec = precSym.Prec
				rule.Assoc = precSym.Assoc
			}
			continue
		}

		sym := g.GetOrCreateSymbol(field, TermSymbol)
		sym.UseCnt++
		rule.Rhs = append(rule.Rhs, sym)
	}

	g.Rules = append(g.Rules, rule)

	if lhs.Rule == nil {
		lhs.Rule = rule // First rule defines the symbol
	}

	return nil
}

// GenerateGoOutput generates a Go source file for the parser.
func (g *LemonGrammar) GenerateGoOutput(packageName string) (string, error) {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf(`// Code generated by go-lemon from %s. DO NOT EDIT.
package %s

import (
	"fmt"
)

// Token codes
const (
`, g.Name, packageName))

	// Generate token constants
	prefix := g.TokenPrefix
	tokenIdx := 1
	for _, sym := range g.Symbols {
		if sym.Type == TermSymbol && sym.Name != "" && !strings.HasPrefix(sym.Name, "'") {
			buf.WriteString(fmt.Sprintf("\t%s%s = %d\n", prefix, sym.Name, tokenIdx))
			tokenIdx++
		}
	}
	buf.WriteString(")\n\n")

	// Generate parse tables (placeholder)
	buf.WriteString(g.generateTables())

	return buf.String(), nil
}

func (g *LemonGrammar) generateTables() string {
	return `// TODO: generate parse tables
`
}

// CountSymbols categorizes all symbols.
func (g *LemonGrammar) CountSymbols() (terminals, nonterminals int) {
	for _, s := range g.Symbols {
		if s.Type == TermSymbol {
			terminals++
		} else if s.Type == NonTermSymbol {
			nonterminals++
		}
	}
	return
}

// PrintGrammarStats prints statistics about the grammar.
func (g *LemonGrammar) PrintGrammarStats() {
	terms, nonterms := g.CountSymbols()
	fmt.Printf("Grammar: %s\n", g.Name)
	fmt.Printf("  Symbols: %d total (%d terminals, %d non-terminals)\n",
		len(g.Symbols), terms, nonterms)
	fmt.Printf("  Rules: %d\n", len(g.Rules))
	fmt.Printf("  Start symbol: %s\n", func() string {
		if g.StartSymbol != nil {
			return g.StartSymbol.Name
		}
		return "(first rule)"
	}())
	fmt.Printf("  Token prefix: %s\n", g.TokenPrefix)
}

func init() {
	nextPrec = 0
}
