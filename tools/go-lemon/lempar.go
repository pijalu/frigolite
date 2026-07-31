//go:build ignore

// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// lempar.go — Go parser engine runtime template.
//
// This file is the Go counterpart of SQLite's lempar.c: it contains the
// LALR(1) shift/reduce engine runtime with placeholders that the go-lemon
// generator fills in to produce a complete, self-contained Go parser file.
//
// Placeholders (each on its own line, replaced by the generator):
//
//	__LEMON_PACKAGE__        package name
//	__LEMON_TOKEN_CONSTANTS__ token code constants (TK_*)
//	__LEMON_PARSE_TABLES__    yyAction/yyLookahead/... + GetParseTables()
//	__LEMON_ACTION_CODE__     yyReduceAction() implementing grammar actions
//
// Because of the placeholders this file does NOT compile as-is; it is only
// consumed by go-lemon (see GenerateGoOutputFromTables in generator.go).

package __LEMON_PACKAGE__

import "fmt"

// ParserAction represents an action code from the parse tables.
// Encoding (matching lemon's convention):
//
//	  0 <= N <= YY_MAX_SHIFT          Shift N (goto state N)
//	  YY_MIN_SHIFTREDUCE <= N <= YY_MAX_SHIFTREDUCE  Shift+Reduce by rule N-YY_MIN_SHIFTREDUCE
//	  N == YY_ERROR_ACTION            Syntax error
//	  N == YY_ACCEPT_ACTION           Accept input
//	  N == YY_NO_ACTION               No action (unused slot)
//	  YY_MIN_REDUCE <= N <= YY_MAX_REDUCE  Reduce by rule N-YY_MIN_REDUCE
type ParserAction int32

// ParseTables holds all pre-generated parse tables for a grammar.
type ParseTables struct {
	Action    []ParserAction
	Lookahead []int
	ShiftOfst []int
	ReduceOfst []int
	Default   []ParserAction
	Goto      []map[int]ParserAction

	RuleInfoLhs  []int
	RuleInfoNRhs []int
	RuleName     []string
	TokenName    []string

	YYShiftCount     int
	YYReduceCount    int
	YYActTabCount    int
	YYNToken         int
	YYNState         int
	YYNRule          int
	YYMaxShift       ParserAction
	YYMinShiftReduce ParserAction
	YYMaxShiftReduce ParserAction
	YYMinReduce      ParserAction
	YYMaxReduce      ParserAction
	YYErrorAction    ParserAction
	YYAcceptAction   ParserAction
	YYNoAction       ParserAction
	YYNoCode         int
	YYWildcard       int
	YYFallback       []int

	Destructors []func(interface{})
}

// ParseState represents an entry on the parser stack.
type ParseState struct {
	StateNo ParserAction
	Major   int
	Minor   interface{}
}

// Parser is an LALR(1) parser instance.
type Parser struct {
	tables  *ParseTables
	stack   []ParseState
	pos     int
	trace   bool
	action  ActionFunc
	errCount int
	ExtraCtx interface{}
	// SemanticErr is set by action handlers to report grammar-accepted but
	// semantically rejected input (e.g. ORDER BY in a compound-select member).
	SemanticErr error
}

// NewParser creates a new parser instance with the given tables.
func NewParser(tables *ParseTables) *Parser {
	p := &Parser{
		tables: tables,
		stack:  make([]ParseState, 0, 100),
		pos:    -1,
	}
	p.push(ParseState{StateNo: 0, Major: 0})
	return p
}

// SetTrace enables or disables tracing output.
func (p *Parser) SetTrace(on bool) {
	p.trace = on
}

func (p *Parser) push(s ParseState) {
	p.stack = append(p.stack, s)
	p.pos++
}

func (p *Parser) pop() ParseState {
	if p.pos < 0 {
		return ParseState{}
	}
	s := p.stack[p.pos]
	p.stack = p.stack[:p.pos]
	p.pos--
	return s
}

func (p *Parser) top() *ParseState {
	if p.pos < 0 {
		return nil
	}
	return &p.stack[p.pos]
}

func safeTokenName(t *ParseTables, code int) string {
	if t.TokenName != nil && code >= 0 && code < len(t.TokenName) {
		return t.TokenName[code]
	}
	return "?"
}

// yyFindShiftAction finds the action for a terminal lookahead token.
func (p *Parser) yyFindShiftAction(lookahead int, stateNo ParserAction) ParserAction {
	t := p.tables
	if stateNo > ParserAction(t.YYMaxShift) {
		return stateNo
	}

	i := t.ShiftOfst[stateNo]
	if i < 0 || i > t.YYActTabCount || i+lookahead >= len(t.Lookahead) {
		return t.Default[stateNo]
	}
	idx := i + lookahead
	if idx >= len(t.Lookahead) || t.Lookahead[idx] != lookahead {
		if t.YYFallback != nil && lookahead < len(t.YYFallback) && t.YYFallback[lookahead] != 0 {
			return p.yyFindShiftAction(t.YYFallback[lookahead], stateNo)
		}
		if t.YYWildcard >= 0 && lookahead > 0 {
			j := idx - lookahead + t.YYWildcard
			if j < len(t.Lookahead) && t.Lookahead[j] == t.YYWildcard {
				return t.Action[j]
			}
		}
		return t.Default[stateNo]
	}
	if idx < len(t.Action) {
		return t.Action[idx]
	}
	return t.Default[stateNo]
}

// yyFindReduceAction finds the action for a non-terminal lookahead (after reduce).
func (p *Parser) yyFindReduceAction(stateNo ParserAction, lookahead int) ParserAction {
	t := p.tables
	if t.Goto != nil && int(stateNo) < len(t.Goto) && t.Goto[stateNo] != nil {
		if act, ok := t.Goto[stateNo][lookahead]; ok {
			return act
		}
	}
	if int(stateNo) <= t.YYReduceCount {
		i := t.ReduceOfst[stateNo] + lookahead
		if i >= 0 && i < len(t.Lookahead) && i < len(t.Action) && t.Lookahead[i] == lookahead {
			return t.Action[i]
		}
	}
	return 0
}

// ActionFunc is the type for user-supplied reduce action functions.
type ActionFunc func(ruleNo int, parser *Parser, lookahead int, lookaheadToken interface{})

// OnReduce sets the callback for reduce actions.
func (p *Parser) OnReduce(fn ActionFunc) {
	p.action = fn
}

// yyReduce performs a reduce action and the shift that follows.
func (p *Parser) yyReduce(ruleNo int, lookahead int, lookaheadToken interface{}) ParserAction {
	t := p.tables
	top := p.pos

	if p.trace && ruleNo < len(t.RuleName) {
		nrhs := -t.RuleInfoNRhs[ruleNo]
		if nrhs > 0 && top+nrhs < len(p.stack) {
			fmt.Printf("Reduce %d [%s], pop back to state %d.\n",
				ruleNo, t.RuleName[ruleNo], p.stack[top-nrhs].StateNo)
		} else {
			fmt.Printf("Reduce %d [%s].\n", ruleNo, t.RuleName[ruleNo])
		}
	}

	if ruleNo >= len(t.RuleInfoLhs) {
		return t.YYErrorAction
	}
	gotoSym := t.RuleInfoLhs[ruleNo]
	nrhs := t.RuleInfoNRhs[ruleNo]
	size := -nrhs

	lhsSlot := top - size + 1

	if nrhs == 0 {
		p.stack = append(p.stack, ParseState{})
		p.pos = lhsSlot
	}

	if p.action != nil {
		p.action(ruleNo, p, lookahead, lookaheadToken)
	}

	lhsValue := p.stack[lhsSlot].Minor
	stateNo := p.stack[top+nrhs].StateNo
	act := p.yyFindReduceAction(stateNo, gotoSym)

	p.stack[lhsSlot] = ParseState{
		StateNo: act,
		Major:   gotoSym,
		Minor:   lhsValue,
	}
	p.pos = lhsSlot
	p.stack = p.stack[:lhsSlot+1]

	if p.trace {
		fmt.Printf("... then shift, go to state %d\n", act)
	}
	return act
}

// yyShift performs a shift action.
func (p *Parser) yyShift(newState ParserAction, major int, minor interface{}) {
	t := p.tables
	if newState > t.YYMaxShift {
		newState += t.YYMinReduce - t.YYMinShiftReduce
	}
	p.push(ParseState{
		StateNo: newState,
		Major:   major,
		Minor:   minor,
	})
}

// ParseResult describes the outcome of parsing.
type ParseResult int

const (
	ParseAccept  ParseResult = iota
	ParseError
	ParseStackOverflow
)

// Parse processes one token through the parser.
// yymajor is the token code (0 = EOF); yyminor is the semantic value.
func (p *Parser) Parse(yymajor int, yyminor interface{}) ParseResult {
	t := p.tables
	if p.pos < 0 {
		return ParseError
	}

	yyact := p.stack[p.pos].StateNo

	if p.trace {
		traceName := "?"
		if t.TokenName != nil && yymajor < len(t.TokenName) {
			traceName = t.TokenName[yymajor]
		}
		if yyact < t.YYMinReduce {
			fmt.Printf("Input '%s' in state %d\n", traceName, yyact)
		} else {
			fmt.Printf("Input '%s' with pending reduce %d\n",
				traceName, yyact-t.YYMinReduce)
		}
	}

	for {
		if p.pos < 0 || p.pos >= len(p.stack) {
			return ParseError
		}
		yyact = p.yyFindShiftAction(yymajor, p.stack[p.pos].StateNo)

		if yyact == t.YYAcceptAction {
			p.pos = -1
			if p.trace {
				fmt.Printf("Accept!\n")
			}
			return ParseAccept
		}
		if yyact == t.YYErrorAction || yyact == t.YYNoAction {
			if p.trace {
				fmt.Printf("Syntax Error!\n")
			}
			return ParseError
		}

		if yyact >= t.YYMinReduce {
			ruleNo := int(yyact - t.YYMinReduce)
			if ruleNo >= t.YYNRule {
				return ParseError
			}
			yyact = p.yyReduce(ruleNo, yymajor, yyminor)
			continue
		} else {
			p.yyShift(yyact, yymajor, yyminor)
			return ParseAccept
		}
	}
}

// Finalize clears all parser state, calling destructors for remaining stack elements.
func (p *Parser) Finalize() {
	for p.pos >= 0 {
		top := &p.stack[p.pos]
		p.yyDestructor(top.Major, top.Minor)
		p.pop()
	}
}

func (p *Parser) yyDestructor(major int, minor interface{}) {
	if p.tables.Destructors != nil && major < len(p.tables.Destructors) && p.tables.Destructors[major] != nil {
		p.tables.Destructors[major](minor)
	}
}

// yyStackOverflow handles parser stack overflow.
func (p *Parser) yyStackOverflow() {
	for p.pos >= 0 {
		p.yyPopParserStack()
	}
}

func (p *Parser) yyPopParserStack() {
	if p.pos < 0 {
		return
	}
	top := &p.stack[p.pos]
	p.yyDestructor(top.Major, top.Minor)
	p.pop()
}

// Token constants are generated by go-lemon from the grammar.
// __LEMON_TOKEN_CONSTANTS__

// Parse tables are generated by go-lemon from the grammar.
// __LEMON_PARSE_TABLES__

// yyReduceAction executes the semantic action for a reduced rule.
// Register with parser via parser.OnReduce(yyReduceAction).
// __LEMON_ACTION_CODE__
