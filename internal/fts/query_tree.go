package fts

import (
	"fmt"
	"strings"
)

// Query-tree utilities split out of query.go: first-flag clearing, matchinfo
// expression printing (the fts3_exprtest/test_fts3expr test harness), corrupt
// term queries, and negation analysis.

// ParseMatchQuery parses an FTS MATCH query string into a QueryNode tree.
// Syntax:
//
//	query     := expr
//	expr      := orExpr
//	orExpr    := andExpr (OR andExpr)*
//	andExpr   := term ( (AND | NOT | implicit) term)*
//	term      := column:term | phrase | prefix | word | -term
//	column    := word ':'
//	phrase    := '"' words '"'
//	prefix    := word '*'
//	word      := alphanumeric sequence
//
// ClearFirstFlags removes column-first ('^') pins from a parsed query tree.
// The '^' operator is an FTS4-only extension (fts3_expr.c is compiled with
// FTS4_MODULE); on FTS3 tables '^' is plain token text and must be ignored
// (fts3first.test 2.2.1 vs 2.2.2).
func ClearFirstFlags(node QueryNode) {
	switch n := node.(type) {
	case *TermNode:
		n.First = false
	case *PrefixNode:
		n.First = false
	case *PhraseNode:
		n.FirstAt = -1
	case *AndNode:
		ClearFirstFlags(n.Left)
		ClearFirstFlags(n.Right)
	case *OrNode:
		ClearFirstFlags(n.Left)
		ClearFirstFlags(n.Right)
	case *NotNode:
		ClearFirstFlags(n.Inner)
	case *NearNode:
		ClearFirstFlags(n.Left)
		ClearFirstFlags(n.Right)
	case *ColumnNode:
		ClearFirstFlags(n.Inner)
	case *ColumnRefNode:
		ClearFirstFlags(n.Inner)
	}
}

// allOperandsNegated reports whether the query tree's top-level implicit-AND
// chain consists solely of NOT nodes (no positive phrase), i.e. "-a -b".
func allOperandsNegated(n QueryNode) bool {
	switch t := n.(type) {
	case *AndNode:
		return allOperandsNegated(t.Left) && allOperandsNegated(t.Right)
	case *NotNode:
		return true
	default:
		return false
	}
}

// queryReferencesCorruptTerm reports whether the query reads a term whose
// segment doclist failed to load (corruptTerms). A MATCH that reads a corrupt
// term must fail with "database disk image is malformed" (fts3corrupt4
// 11.1/19.1); a query whose terms are all intact succeeds even when the same
// segment holds a corrupt term it does not read (13.1).
func queryReferencesCorruptTerm(node QueryNode, idx *InvertedIndex) bool {
	if idx == nil || len(idx.corruptTerms) == 0 {
		return false
	}
	switch n := node.(type) {
	case *TermNode:
		return idx.corruptTerms[n.Term]
	case *PrefixNode:
		return prefixTouchesCorruptTerm(n.Prefix, idx)
	case *PhraseNode:
		return phraseTouchesCorruptTerm(n.Terms, idx)
	case *AndNode:
		return corruptChildren(n.Left, n.Right, idx)
	case *NearNode:
		return corruptChildren(n.Left, n.Right, idx)
	case *OrNode:
		return corruptChildren(n.Left, n.Right, idx)
	case *NotNode:
		return queryReferencesCorruptTerm(n.Inner, idx)
	case *ColumnNode:
		return queryReferencesCorruptTerm(n.Inner, idx)
	case *ColumnRefNode:
		return queryReferencesCorruptTerm(n.Inner, idx)
	}
	return false
}

// corruptChildren reports whether either child of a binary node references a
// corrupt term.
func corruptChildren(l, r QueryNode, idx *InvertedIndex) bool {
	return queryReferencesCorruptTerm(l, idx) || queryReferencesCorruptTerm(r, idx)
}

// prefixTouchesCorruptTerm reports whether any corrupt term starts with the
// given prefix (a prefix query reads every matching term's doclist).
func prefixTouchesCorruptTerm(prefix string, idx *InvertedIndex) bool {
	for term := range idx.corruptTerms {
		if len(term) >= len(prefix) && term[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// phraseTouchesCorruptTerm reports whether any token of the phrase is a
// corrupt term.
func phraseTouchesCorruptTerm(terms []string, idx *InvertedIndex) bool {
	for _, t := range terms {
		if idx.corruptTerms[t] {
			return true
		}
	}
	return false
}

// printPhraseTokens renders one phrase node's "PHRASE <col> <i> <term>"
// token pairs ("+" suffix marks prefix tokens).
func printPhraseTokens(t *PhraseNode) string {
	out := "PHRASE 3"
	for i, term := range t.Terms {
		suf := ""
		if i < len(t.Prefixes) && t.Prefixes[i] {
			suf = "+"
		}
		out += fmt.Sprintf(" %d %s%s", i, term, suf)
	}
	return out
}

// ExprPrint renders a parsed MATCH expression in SQLite's test-harness
// format (fts3_expr.c fts3ExprPrint, exercised through the fts3_exprtest /
// test_fts3expr SQL test functions): binary operators print as
// "OP {left} {right}" (NOT included), NEAR carries its distance ("/n"),
// and a phrase prints as "PHRASE <col> <i> <term>" pairs with a "+"
// suffix marking prefix tokens. The default column index is 3 (the
// fts3expr.test harness parses with columns a,b,c).
func ExprPrint(n QueryNode) string {
	switch t := n.(type) {
	case *TermNode:
		return "PHRASE 3 0 " + t.Term
	case *PrefixNode:
		return "PHRASE 3 0 " + t.Prefix + "+"
	case *PhraseNode:
		return printPhraseTokens(t)
	case *AndNode:
		if not, ok := t.Right.(*NotNode); ok {
			// 'a NOT b' parses as AND(a, NOT b); SQLite's tree is the
			// binary NOT(a, b).
			return "NOT {" + ExprPrint(t.Left) + "} {" + ExprPrint(not.Inner) + "}"
		}
		return "AND {" + ExprPrint(t.Left) + "} {" + ExprPrint(t.Right) + "}"
	case *OrNode:
		return "OR {" + ExprPrint(t.Left) + "} {" + ExprPrint(t.Right) + "}"
	case *NearNode:
		return fmt.Sprintf("NEAR/%d {%s} {%s}", t.Distance, ExprPrint(t.Left), ExprPrint(t.Right))
	case *NotNode:
		return "NOT {} {" + ExprPrint(t.Inner) + "}"
	default:
		return ""
	}
}

// ResolveQuery resolves column references in a query node and returns
// both the resolved node and the set of column indices referenced.
func ResolveQuery(node QueryNode, colNames []string) QueryNode {
	colIndex := make(map[string]int)
	for i, name := range colNames {
		colIndex[strings.ToLower(name)] = i
	}
	// Also allow table name to refer to all columns (no restriction)
	return ResolveColumnRef(node, colIndex)
}

// ResolveColumnRef converts a ColumnRefNode to a ColumnNode using the column
// index mapping.
func ResolveColumnRef(node QueryNode, colIndex map[string]int) QueryNode {
	switch n := node.(type) {
	case *ColumnRefNode:
		colIdx := 0
		if idx, ok := colIndex[n.ColumnName]; ok {
			colIdx = idx
		}
		return &ColumnNode{
			Column: colIdx,
			Inner:  ResolveColumnRef(n.Inner, colIndex),
		}
	case *AndNode:
		return &AndNode{
			Left:  ResolveColumnRef(n.Left, colIndex),
			Right: ResolveColumnRef(n.Right, colIndex),
		}
	case *NearNode:
		return &NearNode{
			Left:     ResolveColumnRef(n.Left, colIndex),
			Right:    ResolveColumnRef(n.Right, colIndex),
			Distance: n.Distance,
		}
	case *OrNode:
		return &OrNode{
			Left:  ResolveColumnRef(n.Left, colIndex),
			Right: ResolveColumnRef(n.Right, colIndex),
		}
	case *NotNode:
		return &NotNode{Inner: ResolveColumnRef(n.Inner, colIndex)}
	default:
		return node
	}
}

// CollectTerms collects all unique terms from a query node.
func CollectTerms(node QueryNode) []string {
	seen := make(map[string]bool)
	var terms []string
	collectTerms(node, &seen, &terms)
	return terms
}

func collectTerms(node QueryNode, seen *map[string]bool, terms *[]string) {
	switch n := node.(type) {
	case *TermNode:
		addTerm(n.Term, seen, terms)
	case *PrefixNode:
		addTerm(n.Prefix, seen, terms)
	case *PhraseNode:
		for _, t := range n.Terms {
			addTerm(t, seen, terms)
		}
	case *AndNode:
		collectTerms(n.Left, seen, terms)
		collectTerms(n.Right, seen, terms)
	case *NearNode:
		collectTerms(n.Left, seen, terms)
		collectTerms(n.Right, seen, terms)
	case *OrNode:
		collectTerms(n.Left, seen, terms)
		collectTerms(n.Right, seen, terms)
	case *NotNode:
		collectTerms(n.Inner, seen, terms)
	case *ColumnNode:
		collectTerms(n.Inner, seen, terms)
	case *ColumnRefNode:
		collectTerms(n.Inner, seen, terms)
	}
}

// addTerm appends a term to the collected list if not already seen.
func addTerm(term string, seen *map[string]bool, terms *[]string) {
	if !(*seen)[term] {
		(*seen)[term] = true
		*terms = append(*terms, term)
	}
}

// parseNearDistance consumes the NEAR keyword (already verified at p.pos) and
// an optional explicit /n distance. It returns the distance (default 10) and
// whether the NEAR operator is valid here: the keyword must be followed by
// whitespace, '/', or end of input (a term like "nearby" is not the operator),
// and an explicit NEAR/n must have at least one digit.
func (p *queryParser) parseNearDistance() (int, bool) {
	next := p.pos + 4
	if next < len(p.input) && p.input[next] == '/' {
		d, ok := p.parseNearNumber(next)
		return d, ok
	}
	if next < len(p.input) && (p.input[next] == ' ' || p.input[next] == '\t' || p.input[next] == '\n') {
		p.pos = next
		return 10, true
	}
	if next == len(p.input) {
		p.pos = next
		return 10, true
	}
	return 0, false
}

// parseNearNumber consumes an explicit NEAR/n distance ('/' already at pos).
func (p *queryParser) parseNearNumber(slash int) (int, bool) {
	numStart := slash + 1
	numEnd := numStart
	for numEnd < len(p.input) && p.input[numEnd] >= '0' && p.input[numEnd] <= '9' {
		numEnd++
	}
	if numEnd == numStart {
		return 0, false
	}
	val := 0
	for i := numStart; i < numEnd; i++ {
		val = val*10 + int(p.input[i]-'0')
		if val >= 1000000000 {
			val = 1000000000
		}
	}
	p.pos = numEnd
	return val, true
}
