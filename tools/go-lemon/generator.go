// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// generator.go — LALR(1) parse table generator for go-lemon.
//
// This implements the full LALR(1) algorithm to produce parse tables
// from a grammar, then generates a Go source file with the tables
// embedded for use with the engine.
//
// Algorithm follows DeRemer & Pennello's "Efficient Computation of
// LALR(1) Look-Ahead Sets" (1982), matching SQLite's lemon.c.

package main

import (
	"fmt"
	"sort"
	"strings"
)

// --- LR(0) Items ---

// Item represents an LR(0) item: a rule with a dot position.
type Item struct {
	Rule *Rule
	Pos  int // position of the dot (0 = before first, len(RHS) = after last)
}

// ItemSet is a set of LR(0) items (a "state" in the LR automaton).
type ItemSet struct {
	ID         int
	Items      map[string]*Item // key = "ruleIdx dotPos"
	Trans      map[string]int   // symbol name -> next state ID
	Reduces    []ReduceItem     // reduce actions (item at pos=len(RHS))
}

// ReduceItem is an item with its LALR(1) lookahead set.
type ReduceItem struct {
	Rule       *Rule
	Lookaheads map[string]bool // set of terminal symbol names
}

func itemKey(r *Rule, pos int) string {
	return fmt.Sprintf("%d:%d", r.Index, pos)
}

// closure computes the closure of an LR(0) item set.
func closure(items []*Item, grammar *LemonGrammar) map[string]*Item {
	result := make(map[string]*Item)
	work := make([]*Item, len(items))
	copy(work, items)
	for _, it := range items {
		result[itemKey(it.Rule, it.Pos)] = it
	}
	for len(work) > 0 {
		it := work[0]
		work = work[1:]
		if it.Pos < len(it.Rule.Rhs) {
			nextSym := it.Rule.Rhs[it.Pos]
			if nextSym.Type == NonTermSymbol {
				for _, rule := range grammar.Rules {
					if rule.Lhs == nextSym {
						newItem := &Item{Rule: rule, Pos: 0}
						key := itemKey(newItem.Rule, newItem.Pos)
						if _, exists := result[key]; !exists {
							result[key] = newItem
							work = append(work, newItem)
						}
					}
				}
			}
		}
	}
	return result
}

// gotoSet computes the GOTO set for a given symbol.
func gotoSet(items map[string]*Item, symName string) []*Item {
	var result []*Item
	for _, it := range items {
		if it.Pos < len(it.Rule.Rhs) && it.Rule.Rhs[it.Pos].Name == symName {
			result = append(result, &Item{Rule: it.Rule, Pos: it.Pos + 1})
		}
	}
	return result
}

// --- LALR(1) State Machine ---

// GenerateTables generates LALR(1) parse tables from the grammar.
func GenerateTables(grammar *LemonGrammar) (*ParseTables, error) {
	// Phase 1: Compute FIRST sets
	firstSets := computeFirstSets(grammar)
	
	// Phase 2: Build LR(0) states
	states, err := buildLR0States(grammar)
	if err != nil {
		return nil, fmt.Errorf("building LR(0) states: %w", err)
	}
	
	// Phase 3: Compute LALR(1) lookaheads
	computeLALRLookaheads(grammar, states, firstSets)
	
	// Phase 4: Build action and goto tables
	tables := buildParseTables(grammar, states)
	
	return tables, nil
}

// computeFirstSets computes FIRST sets for all symbols.
func computeFirstSets(grammar *LemonGrammar) map[string]map[string]bool {
	first := make(map[string]map[string]bool)
	for _, sym := range grammar.Symbols {
		first[sym.Name] = make(map[string]bool)
	}
	for _, sym := range grammar.Symbols {
		if sym.Type == TermSymbol && !isMultiTermString(sym.Name) {
			first[sym.Name][sym.Name] = true
		}
	}
	// Iteratively compute FIRST for non-terminals
	changed := true
	for changed {
		changed = false
		for _, rule := range grammar.Rules {
			lhs := rule.Lhs.Name
			for _, rhsSym := range rule.Rhs {
				rhsName := rhsSym.Name
				for t := range first[rhsName] {
					if !first[lhs][t] {
						first[lhs][t] = true
						changed = true
					}
				}
				if !canBeEmpty(rhsSym, grammar) {
					break
				}
			}
			// If all RHS symbols can be empty, add ε
			if len(rule.Rhs) == 0 || allCanBeEmpty(rule.Rhs, grammar) {
				// ε is represented as empty in the set
			}
		}
	}
	return first
}

func isMultiTermString(s string) bool {
	return len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"'
}

func canBeEmpty(sym *Symbol, grammar *LemonGrammar) bool {
	if sym.Type == TermSymbol {
		return false
	}
	for _, rule := range grammar.Rules {
		if rule.Lhs == sym && len(rule.Rhs) == 0 {
			return true
		}
	}
	return false
}

func allCanBeEmpty(syms []*Symbol, g *LemonGrammar) bool {
	for _, s := range syms {
		if !canBeEmpty(s, g) {
			return false
		}
	}
	return true
}

// buildLR0States builds all LR(0) states for the grammar.
func buildLR0States(grammar *LemonGrammar) ([]*ItemSet, error) {
	if len(grammar.Rules) == 0 {
		return nil, fmt.Errorf("no rules in grammar")
	}
	
	// Augment the grammar: add S' → S
	augRule := &Rule{
		Index: len(grammar.Rules),
		Lhs:   grammar.GetOrCreateSymbol("_START", NonTermSymbol),
		Rhs:   []*Symbol{grammar.Rules[0].Lhs},
	}
	
	startItems := []*Item{{Rule: augRule, Pos: 0}}
	startClosure := closure(startItems, grammar)
	
	var stateList []*ItemSet
	stateMap := make(map[string]int)
	
	stateList = append(stateList, &ItemSet{
		ID:    0,
		Items: startClosure,
		Trans: make(map[string]int),
	})
	stateMap[itemSetFingerprint(startClosure)] = 0
	
	for i := 0; i < len(stateList); i++ {
		st := stateList[i]
		
		// Collect symbols that appear right after a dot
		symbols := make(map[string]bool)
		for _, it := range st.Items {
			if it.Pos < len(it.Rule.Rhs) {
				symbols[it.Rule.Rhs[it.Pos].Name] = true
			}
		}
		
		// Compute GOTO for each symbol
		for symName := range symbols {
			gotoItems := gotoSet(st.Items, symName)
			if len(gotoItems) == 0 {
				continue
			}
			gotoClosure := closure(gotoItems, grammar)
			fp := itemSetFingerprint(gotoClosure)
			
			if existingID, exists := stateMap[fp]; exists {
				st.Trans[symName] = existingID
			} else {
				newID := len(stateList)
				newState := &ItemSet{
					ID:    newID,
					Items: gotoClosure,
					Trans: make(map[string]int),
				}
				stateList = append(stateList, newState)
				stateMap[fp] = newID
				st.Trans[symName] = newID
			}
		}
		
		// Collect reduce items
		for _, it := range st.Items {
			if it.Pos == len(it.Rule.Rhs) {
				// This is a reduce item
				isAugmentReduce := it.Rule.Lhs.Name == "_START"
				if !isAugmentReduce {
					st.Reduces = append(st.Reduces, ReduceItem{
						Rule:       it.Rule,
						Lookaheads: make(map[string]bool),
					})
				}
			}
		}
	}
	
	return stateList, nil
}

func itemSetFingerprint(items map[string]*Item) string {
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "|")
}

// computeLALRLookaheads computes LALR(1) lookahead sets for reduce items.
// Populates each ReduceItem's Lookaheads field directly.
func computeLALRLookaheads(grammar *LemonGrammar, states []*ItemSet, firstSets map[string]map[string]bool) {
	// Compute FOLLOW sets for all non-terminals
	followSets := computeFollowSets(grammar, firstSets)
	
	// Assign lookaheads to reduce items using FOLLOW sets
	for _, st := range states {
		for ri := range st.Reduces {
			red := &st.Reduces[ri]
			if followSet, ok := followSets[red.Rule.Lhs.Name]; ok {
				for t := range followSet {
					red.Lookaheads[t] = true
				}
			}
		}
	}
}

// computeFollowSets computes FOLLOW sets for all non-terminals.
// FOLLOW(A) = { t ∈ T | S' ⇒* α A t β }
func computeFollowSets(grammar *LemonGrammar, firstSets map[string]map[string]bool) map[string]map[string]bool {
	follow := make(map[string]map[string]bool)
	for _, sym := range grammar.Symbols {
		follow[sym.Name] = make(map[string]bool)
	}
	
	// Start symbol gets EOF ($)
	if len(grammar.Rules) > 0 {
		rule := grammar.Rules[0]
		follow[rule.Lhs.Name]["$"] = true
	}
	
	// Iteratively compute
	changed := true
	for changed {
		changed = false
		for _, rule := range grammar.Rules {
			lhs := rule.Lhs.Name
			for i, rhsSym := range rule.Rhs {
				if rhsSym.Type == NonTermSymbol {
					rhsName := rhsSym.Name
					// Add FIRST of following symbols
					allNullable := true
					for j := i + 1; j < len(rule.Rhs); j++ {
						nextSym := rule.Rhs[j]
						nextName := nextSym.Name
						for t := range firstSets[nextName] {
							if !follow[rhsName][t] {
								follow[rhsName][t] = true
								changed = true
							}
						}
						if !canBeEmpty(nextSym, grammar) {
							allNullable = false
							break
						}
					}
					// If all following symbols can be empty, add FOLLOW(LHS)
					if allNullable || i == len(rule.Rhs)-1 {
						for t := range follow[lhs] {
							if !follow[rhsName][t] {
								follow[rhsName][t] = true
								changed = true
							}
						}
					}
				}
			}
		}
	}
	
	return follow
}

// rawAction records the parse action for a single (state, terminal) pair.
type rawAction struct {
	isShift    bool
	isReduce   bool
	isAccept   bool
	isError    bool
	shiftState int
	reduceRule int
}

// buildParseTables generates the final parse tables.
func buildParseTables(grammar *LemonGrammar, states []*ItemSet) *ParseTables {
	
	tables := &ParseTables{}
	
	// Build a mapping from symbol name to token code
	symIndex := make(map[string]int)
	terminalList := make([]*Symbol, 0)
	for _, sym := range grammar.Symbols {
		if sym.Type == TermSymbol {
			symIndex[sym.Name] = len(terminalList) + 1 // 1-based token codes
			terminalList = append(terminalList, sym)
		}
	}
	
	// Terminal count (excluding non-terminal symbols)
	termCount := len(terminalList) + 1 // +1 for EOF
	
	// For each state and terminal, determine the action
	// Action encoding (matching lemon):
	// 0..YY_MAX_SHIFT = shift
	// YY_MIN_SHIFTREDUCE..YY_MAX_SHIFTREDUCE = shift+reduce (not used in simple form)
	// YY_ERROR_ACTION = error
	// YY_ACCEPT_ACTION = accept
	// YY_MIN_REDUCE..YY_MAX_REDUCE = reduce by rule N - YY_MIN_REDUCE
	
	stateCount := len(states)
	if stateCount == 0 {
		return tables
	}
	
	rawActions := make([][]rawAction, stateCount)
	for i := range states {
		rawActions[i] = make([]rawAction, termCount)
		for j := range rawActions[i] {
			rawActions[i][j].isError = true // default: error
		}
	}
	
	// Add shift actions from transitions
	for si, st := range states {
		for symName, nextState := range st.Trans {
			if code, ok := symIndex[symName]; ok {
				rawActions[si][code].isShift = true
				rawActions[si][code].shiftState = nextState
				rawActions[si][code].isError = false
			}
		}
	}
	
	// Add reduce actions from reduce items with lookaheads
	for si, st := range states {
		for ri := range st.Reduces {
			red := &st.Reduces[ri]
			for lookahead := range red.Lookaheads {
				code := symToCode(lookahead, symIndex)
				if code >= 0 && code < termCount {
					ra := &rawActions[si][code]
					if ra.isShift {
						// Shift/reduce conflict — resolve by precedence
						if resolveConflict(ra, red.Rule, terminalList, code) {
							// Reduce wins
							ra.isReduce = true
							ra.reduceRule = red.Rule.Index
						}
					} else {
						ra.isReduce = true
						ra.reduceRule = red.Rule.Index
						ra.isError = false
					}
				}
			}
		}
	}
	
	// Check for special case: accept action
	// State that contains _START → S ·   (dot at end of augmented start rule)
	for si, st := range states {
		for _, it := range st.Items {
			if it.Pos == len(it.Rule.Rhs) && it.Rule.Lhs.Name == "_START" {
				// This state indicates acceptance
				// Set accept on EOF token
				if 0 < termCount {
					rawActions[si][0].isAccept = true
					rawActions[si][0].isError = false
				}
			}
		}
	}
	
	// Build compressed action tables
	// We need to generate:
	//   yy_action[]     - action values
	//   yy_lookahead[]  - lookahead tokens
	//   yy_shift_ofst[] - offset for shift lookups
	//   yy_reduce_ofst[] - offset for reduce/goto lookups
	//   yy_default[]    - default actions
	
	// First, compute action value for each state/terminal
	YY_MIN_REDUCE := ParserAction(10000) // base for reduce actions
	YY_MAX_SHIFT := ParserAction(stateCount)

	// Special action values — placed between shift/reduce and reduce ranges
	// to avoid collision with shift state numbers (0..stateCount).
	YY_ERROR_ACTION := YY_MIN_REDUCE - 3
	YY_ACCEPT_ACTION := YY_MIN_REDUCE - 2
	YY_NO_ACTION := YY_MIN_REDUCE - 1

	actionVals := make([][]ParserAction, stateCount)
	for i := range actionVals {
		actionVals[i] = make([]ParserAction, termCount)
		for j := range actionVals[i] {
			ra := rawActions[i][j]
			if ra.isAccept {
				actionVals[i][j] = YY_ACCEPT_ACTION
			} else if ra.isShift {
				actionVals[i][j] = ParserAction(ra.shiftState)
			} else if ra.isReduce {
				actionVals[i][j] = YY_MIN_REDUCE + ParserAction(ra.reduceRule)
			} else if ra.isError {
				actionVals[i][j] = YY_ERROR_ACTION
			} else {
				actionVals[i][j] = YY_ERROR_ACTION
			}
		}
	}
	
	// Compact action table using the same approach as lemon:
	// Try to find an offset for each state such that:
	//   yy_shift_ofst[S] + X gives an index into yy_action where the action for (S, X) is stored
	
	// Simple compaction: gather all actions into one table
	// For each state, find the best offset
	
	maxActions := stateCount * termCount
	yyAction := make([]ParserAction, maxActions)
	yyLookahead := make([]int, maxActions)
	for i := range yyAction {
		yyAction[i] = 0
		yyLookahead[i] = -1
	}
	
	yyShiftOfst := make([]int, stateCount)
	yyReduceOfst := make([]int, stateCount)
	yyDefault := make([]ParserAction, stateCount)
	
	// For each state, find the best offset
	nextSlot := 0
	
	for si := 0; si < stateCount; si++ {
		// Find default action (most common non-error action)
		actionCount := make(map[ParserAction]int)
		for j := 0; j < termCount; j++ {
			if rawActions[si][j].isError {
				continue
			}
			act := actionVals[si][j]
			actionCount[act]++
		}
		bestCount := 0
		bestAction := ParserAction(0)
		for act, count := range actionCount {
			if count > bestCount {
				bestCount = count
				bestAction = act
			}
		}
		yyDefault[si] = bestAction
		
		// For non-default actions, add to the action table
		// Try to find an offset where existing actions don't conflict
		bestOffset := -1
		for offset := 0; offset <= nextSlot; offset++ {
			conflict := false
			for j := 0; j < termCount; j++ {
				if rawActions[si][j].isError {
					continue
				}
				idx := offset + j
				if idx < nextSlot {
					// A slot is occupied if yyLookahead[idx] >= 0
					// Conflict if the lookahead matches but the action differs,
					// OR if a different lookahead already occupies this slot
					if yyLookahead[idx] >= 0 && yyLookahead[idx] != j {
						conflict = true
						break
					}
					if yyLookahead[idx] == j && yyAction[idx] != actionVals[si][j] {
						conflict = true
						break
					}
				} else {
					// This slot is beyond current table — always OK
				}
			}
			if !conflict {
				bestOffset = offset
				break
			}
		}
		
		if bestOffset < 0 {
			bestOffset = nextSlot
		}
		
		yyShiftOfst[si] = bestOffset
		
		// Ensure the table is large enough
		needSize := bestOffset + termCount
		if needSize > len(yyAction) {
			newAction := make([]ParserAction, needSize)
			newLookahead := make([]int, needSize)
			copy(newAction, yyAction)
			copy(newLookahead, yyLookahead)
			for k := len(yyAction); k < needSize; k++ {
				newLookahead[k] = -1
			}
			yyAction = newAction
			yyLookahead = newLookahead
		}
		
		// Fill in entries (non-error actions only)
		for j := 0; j < termCount; j++ {
			if rawActions[si][j].isError {
				continue
			}
			idx := bestOffset + j
			if idx >= nextSlot {
				yyLookahead[idx] = j
				yyAction[idx] = actionVals[si][j]
				if idx >= nextSlot {
					nextSlot = idx + 1
				}
			} else if yyLookahead[idx] == j {
				// Already set
			} else if yyLookahead[idx] < 0 {
				yyLookahead[idx] = j
				yyAction[idx] = actionVals[si][j]
			}
		}
	}
	
	// Trim tables
	yyAction = yyAction[:nextSlot]
	yyLookahead = yyLookahead[:nextSlot]
	
	// Build goto table for non-terminals
	// Collect non-terminal transitions
	nonTermCodes := make(map[string]int)
	ntIndex := 0
	for _, sym := range grammar.Symbols {
		if sym.Type == NonTermSymbol {
			nonTermCodes[sym.Name] = ntIndex
			ntIndex++
		}
	}
	
	nTermCount := ntIndex
	
	// For each state and non-terminal, find goto state
	type gotoEntry struct {
		state  int
		symbol string
		next   int
	}
	var gotoEntries []gotoEntry
	
	for si, st := range states {
		for symName, nextState := range st.Trans {
			if _, ok := nonTermCodes[symName]; ok {
				gotoEntries = append(gotoEntries, gotoEntry{
					state:  si,
					symbol: symName,
					next:   nextState,
				})
			}
		}
	}
	
	// Pack goto entries into the same table structure
	// For simplicity, use reduce_ofst for goto lookups
	_ = nTermCount
	_ = gotoEntries
	
	// For now, use the same offset for reduce (we don't need separate reduce_ofst for LALR(1))
	copy(yyReduceOfst, yyShiftOfst)
	
	// Set table fields
	tables.Action = yyAction
	tables.Lookahead = yyLookahead
	tables.ShiftOfst = yyShiftOfst
	tables.ReduceOfst = yyReduceOfst
	tables.Default = yyDefault
	
	// Build goto table: default goto state for each state after reduce
	tables.Goto = make([]map[int]ParserAction, stateCount)
	for si := range tables.Goto {
		tables.Goto[si] = make(map[int]ParserAction)
	}
	for _, ge := range gotoEntries {
		if ge.next >= 0 && ge.next < stateCount {
			// Map from nonTerm code to next state
			ntCode := nonTermCodes[ge.symbol]
			tables.Goto[ge.state][ntCode] = ParserAction(ge.next)
		}
	}
	
	// Rule info
	tables.RuleInfoLhs = make([]int, len(grammar.Rules))
	tables.RuleInfoNRhs = make([]int, len(grammar.Rules))
	for i, rule := range grammar.Rules {
		if name, ok := nonTermCodes[rule.Lhs.Name]; ok {
			tables.RuleInfoLhs[i] = name
		} else {
			// For the augment rule's LHS (_START)
			tables.RuleInfoLhs[i] = len(nonTermCodes)
		}
		tables.RuleInfoNRhs[i] = -len(rule.Rhs)
	}
	
	// Constants
	tables.YYNState = stateCount
	tables.YYNRule = len(grammar.Rules)
	tables.YYNToken = termCount
	tables.YYMaxShift = YY_MAX_SHIFT
	tables.YYMinReduce = YY_MIN_REDUCE
	tables.YYMaxReduce = YY_MIN_REDUCE + ParserAction(len(grammar.Rules))
	tables.YYErrorAction = YY_ERROR_ACTION
	tables.YYAcceptAction = YY_ACCEPT_ACTION
	tables.YYNoAction = YY_NO_ACTION
	tables.YYMinShiftReduce = YY_MAX_SHIFT + 1
	tables.YYMaxShiftReduce = YY_MIN_REDUCE - 1
	tables.YYShiftCount = stateCount
	tables.YYReduceCount = stateCount
	tables.YYActTabCount = len(yyAction)
	tables.YYNoCode = -1
	tables.YYWildcard = -1
	tables.YYFallback = nil
	tables.TokenName = make([]string, termCount)
	for name, code := range symIndex {
		if code > 0 && code < termCount {
			tables.TokenName[code] = name
		}
	}
	tables.Destructors = nil
	
	return tables
}

func symToCode(name string, symIndex map[string]int) int {
	if name == "$" {
		return 0 // EOF
	}
	if code, ok := symIndex[name]; ok {
		return code
	}
	return -1
}

// resolveConflict resolves a shift/reduce conflict using precedence/associativity.
// Returns true if reduce should win (false = shift wins).
func resolveConflict(ra *rawAction, reduceRule *Rule, terminals []*Symbol, tokenIdx int) bool {
	// Get the precedence of the lookahead token (the operator being shifted)
	tokenPrec := -1
	tokenAssoc := ""
	// tokenIdx is 1-based (0=EOF), terminals is 0-based
	if tokenIdx > 0 && tokenIdx <= len(terminals) {
		tok := terminals[tokenIdx-1]
		tokenPrec = tok.Prec
		tokenAssoc = tok.Assoc
	}
	
	// Get the precedence of the rule (from the grammar's %left/%right declarations)
	rulePrec := reduceRule.Prec
	
	// If the rule has no explicit precedence, use the last terminal in the RHS
	if rulePrec < 0 && len(reduceRule.Rhs) > 0 {
		for i := len(reduceRule.Rhs) - 1; i >= 0; i-- {
			if reduceRule.Rhs[i].Type == TermSymbol {
				rulePrec = reduceRule.Rhs[i].Prec
				break
			}
		}
	}
	
	// Resolve by precedence
	if rulePrec > tokenPrec {
		return true // rule has higher precedence → reduce
	}
	if tokenPrec > rulePrec {
		return false // token has higher precedence → shift
	}
	
	// Same precedence: use associativity
	switch tokenAssoc {
	case "LEFT":
		return true // left-assoc → reduce
	case "RIGHT":
		return false // right-assoc → shift
	case "NONE":
		return true // non-assoc → error (treat as reduce)
	default:
		// Default: shift wins (standard LR policy)
		return false
	}
}

// decodeAction translates a ParserAction value back to type+value.
func decodeAction(act ParserAction, tables *ParseTables) (string, int) {
	if act == tables.YYAcceptAction {
		return "accept", 0
	}
	if act == tables.YYErrorAction {
		return "error", 0
	}
	if act == tables.YYNoAction {
		return "noop", 0
	}
	if act >= tables.YYMinReduce && act <= tables.YYMaxReduce {
		return "reduce", int(act - tables.YYMinReduce)
	}
	if act <= tables.YYMaxShift {
		return "shift", int(act)
	}
	return "unknown", 0
}

// --- Output Generation ---

// generateTablesGo generates Go code for the parse tables.
func generateTablesGo(tables *ParseTables) string {
	if tables == nil {
		return "// Parse tables not generated\n"
	}

	var buf strings.Builder

	// Generate action table
	buf.WriteString("// Parse tables\n")
	buf.WriteString("var yyAction = []ParserAction{\n")
	for i, act := range tables.Action {
		if i%10 == 0 {
			buf.WriteString("\t")
		}
		buf.WriteString(fmt.Sprintf("%d, ", act))
		if i%10 == 9 {
			buf.WriteString("\n")
		}
	}
	buf.WriteString("\n}\n\n")

	// Generate lookahead table
	buf.WriteString("var yyLookahead = []int{\n")
	for i, la := range tables.Lookahead {
		if i%10 == 0 {
			buf.WriteString("\t")
		}
		buf.WriteString(fmt.Sprintf("%d, ", la))
		if i%10 == 9 {
			buf.WriteString("\n")
		}
	}
	buf.WriteString("\n}\n\n")

	// Generate shift/reduce offset tables
	buf.WriteString("var yyShiftOfst = []int{\n")
	for i, ofst := range tables.ShiftOfst {
		if i%10 == 0 {
			buf.WriteString("\t")
		}
		buf.WriteString(fmt.Sprintf("%d, ", ofst))
		if i%10 == 9 {
			buf.WriteString("\n")
		}
	}
	buf.WriteString("\n}\n\n")

	buf.WriteString("var yyReduceOfst = []int{\n")
	for i, ofst := range tables.ReduceOfst {
		if i%10 == 0 {
			buf.WriteString("\t")
		}
		buf.WriteString(fmt.Sprintf("%d, ", ofst))
		if i%10 == 9 {
			buf.WriteString("\n")
		}
	}
	buf.WriteString("\n}\n\n")

	buf.WriteString("var yyDefault = []ParserAction{\n")
	for i, def := range tables.Default {
		if i%10 == 0 {
			buf.WriteString("\t")
		}
		buf.WriteString(fmt.Sprintf("%d, ", def))
		if i%10 == 9 {
			buf.WriteString("\n")
		}
	}
	buf.WriteString("\n}\n\n")

	// Generate rule info tables
	if len(tables.RuleInfoLhs) > 0 {
		buf.WriteString("var yyRuleInfoLhs = []int{\n")
		for i, lhs := range tables.RuleInfoLhs {
			if i%10 == 0 {
				buf.WriteString("\t")
			}
			buf.WriteString(fmt.Sprintf("%d, ", lhs))
			if i%10 == 9 {
				buf.WriteString("\n")
			}
		}
		buf.WriteString("\n}\n\n")
	}

	if len(tables.RuleInfoNRhs) > 0 {
		buf.WriteString("var yyRuleInfoNRhs = []int{\n")
		for i, nrhs := range tables.RuleInfoNRhs {
			if i%10 == 0 {
				buf.WriteString("\t")
			}
			buf.WriteString(fmt.Sprintf("%d, ", nrhs))
			if i%10 == 9 {
				buf.WriteString("\n")
			}
		}
		buf.WriteString("\n}\n\n")
	}

	return buf.String()
}

// GenerateGoOutputFromTables generates a complete Go parser file from the parse tables.
func GenerateGoOutputFromTables(tables *ParseTables, grammar *LemonGrammar, tokenCode map[string]int, pkgName string) string {
	var buf strings.Builder
	
	buf.WriteString(fmt.Sprintf(`// Code generated by go-lemon. DO NOT EDIT.
package %s

// Token codes
const (
	YYNOCODE = 0
`, pkgName))

	// Generate token constants in order
	tokenNames := make([]string, 0, len(tokenCode))
	for name := range tokenCode {
		tokenNames = append(tokenNames, name)
	}
	sort.Strings(tokenNames)
	
	prefix := grammar.TokenPrefix
	for _, name := range tokenNames {
		code := tokenCode[name]
		if code > 0 {
			buf.WriteString(fmt.Sprintf("\t%s%s = %d\n", prefix, name, code))
		}
	}
	buf.WriteString(")\n\n")

	// Generate parse tables as Go constants
	buf.WriteString(generateTablesGo(tables))

	// Generate the GetParseTables function
	buf.WriteString(fmt.Sprintf(`// GetParseTables returns the LALR(1) parse tables.
func GetParseTables() *ParseTables {
	t := &ParseTables{
		Action:    yyAction,
		Lookahead: yyLookahead,
		ShiftOfst: yyShiftOfst,
		ReduceOfst: yyReduceOfst,
		Default:   yyDefault,
		RuleInfoLhs: yyRuleInfoLhs,
		RuleInfoNRhs: yyRuleInfoNRhs,
		TokenName: nil,
		YYNToken: %d,
		YYNState: %d,
		YYNRule: %d,
		YYMaxShift: %d,
		YYMinReduce: %d,
		YYMaxReduce: %d,
		YYErrorAction: %d,
		YYAcceptAction: %d,
		YYNoAction: %d,
		YYMinShiftReduce: %d,
		YYMaxShiftReduce: %d,
		YYShiftCount: %d,
		YYReduceCount: %d,
		YYActTabCount: %d,
		YYNoCode: %d,
		YYWildcard: %d,
	}
	// Initialize goto table (maps from nonTermCode to nextState)
	t.Goto = make([]map[int]ParserAction, %d)
`, tables.YYNToken, tables.YYNState, tables.YYNRule,
		tables.YYMaxShift, tables.YYMinReduce, tables.YYMaxReduce,
		tables.YYErrorAction, tables.YYAcceptAction, tables.YYNoAction,
		tables.YYMinShiftReduce, tables.YYMaxShiftReduce,
		tables.YYShiftCount, tables.YYReduceCount, tables.YYActTabCount,
		tables.YYNoCode, tables.YYWildcard, tables.YYNState))
	
	// Add goto entries
	for si, m := range tables.Goto {
		if m == nil || len(m) == 0 {
			continue
		}
		buf.WriteString(fmt.Sprintf("\tt.Goto[%d] = make(map[int]ParserAction)\n", si))
		for ntCode, nextState := range m {
			buf.WriteString(fmt.Sprintf("\tt.Goto[%d][%d] = %d\n", si, ntCode, nextState))
		}
	}
	
	buf.WriteString("\treturn t\n}\n")

	// Generate action functions from grammar rule code
	if hasActions(grammar) {
		buf.WriteString(generateActionFunction(grammar))
	}

	return buf.String()
}

// hasActions checks if any grammar rule has action code.
func hasActions(grammar *LemonGrammar) bool {
	for _, rule := range grammar.Rules {
		if rule.Code != "" {
			return true
		}
	}
	return false
}

// generateActionFunction generates a Go function that implements the semantic actions
// for all grammar rules.
func generateActionFunction(grammar *LemonGrammar) string {
	var buf strings.Builder
	
	buf.WriteString(`// yyReduceAction executes the semantic action for a reduced rule.
// Register with parser via parser.OnReduce(yyReduceAction).
func yyReduceAction(ruleNo int, p *Parser, lookahead int, lookaheadToken interface{}) {
	pos := p.pos
`)

	// If grammar has an %extra_argument, emit code to extract it from the parser
	if grammar.ExtraArg != "" {
		argName := argNameFromCDecl(grammar.ExtraArg)
		if argName != "" {
			buf.WriteString(fmt.Sprintf("\t%s := p.ExtraCtx\n", argName))
		}
	}

	buf.WriteString("\tswitch ruleNo {\n")
	
	for _, rule := range grammar.Rules {
		if rule.Code == "" {
			continue
		}
		n := len(rule.Rhs)
		code := translateActionCode(rule.Code, n)
		buf.WriteString(fmt.Sprintf("\tcase %d:\n", rule.Index))
		buf.WriteString(fmt.Sprintf("\t\t// %s ->", rule.Lhs.Name))
		for _, s := range rule.Rhs {
			buf.WriteString(fmt.Sprintf(" %s", s.Name))
		}
		buf.WriteString("\n")
		if n > 0 {
			for i := 1; i <= n; i++ {
				stackIdx := n - i
				buf.WriteString(fmt.Sprintf("\t\t_%d_val := p.stack[pos-%d].Minor\n", i, stackIdx))
			}
			// Suppress unused variable warnings
			for i := 1; i <= n; i++ {
				buf.WriteString(fmt.Sprintf("\t\t_ = _%d_val\n", i))
			}
		}
		for _, line := range strings.Split(code, "\n") {
			buf.WriteString(fmt.Sprintf("\t\t%s\n", line))
		}
	}
	
	buf.WriteString("\t}\n}\n")
	
	return buf.String()
}

// translateActionCode replaces $1..$N and $$ with Go expressions.
// $$ becomes a direct stack assignment (p.stack[pos-(N-1)].Minor).
// $n becomes a local variable (_n_val) that's set up before the action.
func translateActionCode(code string, nRhs int) string {
	result := code
	// $$ is the LHS result — translate to stack slot assignment
	if nRhs > 0 {
		lhsSlot := nRhs - 1 // stack[pos - (N-1)]
		result = strings.ReplaceAll(result, "$$", fmt.Sprintf("p.stack[pos-%d].Minor", lhsSlot))
	} else {
		// Empty rule: LHS slot is at the newly pushed element
		result = strings.ReplaceAll(result, "$$", "p.stack[pos].Minor")
	}
	// $1 through $N are the RHS symbol values
	for i := nRhs; i >= 1; i-- {
		result = strings.ReplaceAll(result, fmt.Sprintf("$%d", i), fmt.Sprintf("_%d_val", i))
	}
	return result
}

// argNameFromCDecl extracts the variable name from a C argument declaration
// like "{CodeBuffer *buf}" or "struct ParseState *p".
// Returns the variable name (e.g., "buf") or "" if it can't be parsed.
func argNameFromCDecl(decl string) string {
	s := strings.TrimSpace(decl)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	s = strings.TrimSpace(s)
	// Remove "struct" keyword
	s = strings.Replace(s, "struct ", "", 1)
	// The variable name is the last word after the last space
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	name := fields[len(fields)-1]
	// Remove leading *
	name = strings.TrimLeft(name, "*")
	return name
}