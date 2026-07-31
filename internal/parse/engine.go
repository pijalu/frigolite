// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) parser engine adapted from go-lemon.
// It provides the shift/reduce infrastructure used by the generated SQL parser.
package parse

import "fmt"

// ParserAction represents an action code from the parse tables.
type ParserAction int32

// ParseTables holds all pre-generated parse tables for a grammar.
type ParseTables struct {
	Action     []ParserAction
	Lookahead  []int
	ShiftOfst  []int
	ReduceOfst []int
	Default    []ParserAction
	Goto       []map[int]ParserAction

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

// ParseState represents the parser's current stack entry.
type ParseState struct {
	StateNo ParserAction
	Major   int
	Minor   interface{}
}

// Parser is an LALR(1) parser instance.
type Parser struct {
	tables *ParseTables
	stack  []ParseState
	pos    int
	trace  bool
	action ActionFunc
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
	// After reductions, p.pos may be less than len(p.stack)-1 because
	// empty-rule reductions push elements that are later "popped" by
	// non-empty rule reductions (which only adjust p.pos without
	// trimming the underlying slice). So we overwrite if space exists,
	// otherwise append.
	next := p.pos + 1
	if next < len(p.stack) {
		p.stack[next] = s
	} else {
		p.stack = append(p.stack, s)
	}
	p.pos = next
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
			fallback := t.YYFallback[lookahead]
			if p.trace {
				fmt.Printf("FALLBACK %s => %s\n", t.TokenName[lookahead], t.TokenName[fallback])
			}
			return p.yyFindShiftAction(fallback, stateNo)
		}
		if t.YYWildcard >= 0 && lookahead > 0 {
			j := idx - lookahead + t.YYWildcard
			if j < len(t.Lookahead) && t.Lookahead[j] == t.YYWildcard {
				if p.trace {
					fmt.Printf("WILDCARD %s => %s\n", t.TokenName[lookahead], t.TokenName[t.YYWildcard])
				}
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

// yyDestructor calls the destructor for a symbol value.
func (p *Parser) yyDestructor(major int, minor interface{}) {
	if p.tables.Destructors != nil && major < len(p.tables.Destructors) && p.tables.Destructors[major] != nil {
		p.tables.Destructors[major](minor)
	}
}

// yyPopParserStack pops one element from the stack, calling its destructor.
func (p *Parser) yyPopParserStack() {
	if p.pos < 0 {
		return
	}
	top := &p.stack[p.pos]
	if p.trace {
		fmt.Printf("Popping %s\n", p.tables.TokenName[top.Major])
	}
	p.yyDestructor(top.Major, top.Minor)
	p.pop()
}

// yyStackOverflow handles parser stack overflow.
func (p *Parser) yyStackOverflow() {
	if p.trace {
		fmt.Printf("Stack Overflow!\n")
	}
	for p.pos >= 0 {
		p.yyPopParserStack()
	}
}

// yyShift performs a shift action.
// For shift+reduce actions (newState in YYMinShiftReduce..YYMaxShiftReduce),
// the adjusted state (newState + YYMinReduce - YYMinShiftReduce) is >= YYMinReduce,
// which causes the next Parse() call to immediately reduce.
func (p *Parser) yyShift(newState ParserAction, major int, minor interface{}) {
	t := p.tables
	if newState > ParserAction(t.YYMaxShift) {
		newState += ParserAction(t.YYMinReduce - t.YYMinShiftReduce)
	}
	p.push(ParseState{
		StateNo: newState,
		Major:   major,
		Minor:   minor,
	})
	if p.trace {
		if int(newState) < t.YYNState {
			fmt.Printf("Shift '%s', go to state %d\n", t.TokenName[major], newState)
		} else {
			fmt.Printf("Shift '%s', pending reduce %d\n", t.TokenName[major], newState-t.YYMinReduce)
		}
	}
}

// ActionFunc is the type for user-supplied reduce action functions.
type ActionFunc func(ruleNo int, parser *Parser, lookahead int, lookaheadToken interface{})

// yyReduce performs a reduce action and the shift that follows.
func (p *Parser) yyReduce(ruleNo int, lookahead int, lookaheadToken interface{}) ParserAction {
	t := p.tables
	top := p.pos

	if ruleNo >= len(t.RuleInfoLhs) {
		return t.YYErrorAction
	}

	gotoSym := t.RuleInfoLhs[ruleNo]
	nrhs := t.RuleInfoNRhs[ruleNo]
	size := -nrhs

	// For empty RHS, the LHS goes to a new slot above current top
	if nrhs == 0 {
		// Save the state from the current top before pushing
		prevStateNo := p.stack[top].StateNo
		// Grow the stack by one element (LHS slot)
		p.push(ParseState{}) // p.push increments p.pos
		top = p.pos
		lhsSlot := top

		if p.action != nil {
			p.action(ruleNo, p, lookahead, lookaheadToken)
		}
		lhsValue := p.stack[lhsSlot].Minor
		stateNo := prevStateNo
		act := p.yyFindReduceAction(stateNo, gotoSym)
		p.stack[lhsSlot] = ParseState{
			StateNo: act,
			Major:   gotoSym,
			Minor:   lhsValue,
		}
		p.pos = lhsSlot
		return act
	}

	lhsSlot := top - size + 1

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

	return act
}

// OnReduce sets the callback for reduce actions.
func (p *Parser) OnReduce(fn ActionFunc) {
	p.action = fn
}

// ParseResult describes the outcome of parsing.
type ParseResult int

const (
	ParseAccept         ParseResult = iota
	ParseError
	ParseStackOverflow
)

// Parse processes one token through the parser.
func (p *Parser) Parse(yymajor int, yyminor interface{}) ParseResult {
	t := p.tables
	if p.pos < 0 {
		return ParseError
	}

	yyact := p.stack[p.pos].StateNo

	for {
		if p.pos < 0 || p.pos >= len(p.stack) {
			return ParseError
		}
		yyact = p.yyFindShiftAction(yymajor, p.stack[p.pos].StateNo)

		if yyact >= ParserAction(t.YYMinReduce) {
			// Reduce by rule (yyact - YYMinReduce)
			ruleNo := int(yyact - ParserAction(t.YYMinReduce))
			if ruleNo >= t.YYNRule {
				return ParseError
			}
			yyact = p.yyReduce(ruleNo, yymajor, yyminor)
			// yyact is now a state number (not an action).
			// Loop back to find the next action for this state + lookahead.
			continue
		} else if yyact <= ParserAction(t.YYMaxShiftReduce) {
			// Shift (includes both normal shift and shift+reduce).
			// For shift+reduce, yyShift adjusts the state to encode
			// the pending reduce; the next Parse() call will reduce.
			p.yyShift(yyact, yymajor, yyminor)
			return ParseAccept
		} else if yyact == ParserAction(t.YYAcceptAction) {
			// Accept
			p.pos--
			return ParseAccept
		} else {
			// Error or No Action
			return ParseError
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
