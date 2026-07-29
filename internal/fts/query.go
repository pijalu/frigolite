package fts

import (
	"fmt"
	"strings"
	"unicode"
)

// QueryNode is a node in the MATCH query AST.
type QueryNode interface {
	// MatchDoc returns true if the document matches this node.
	MatchDoc(idx *InvertedIndex, docID int64) bool
	// String returns the query string for debugging.
	String() string
}

// TermNode matches a single term.
type TermNode struct {
	Term string
}

func (n *TermNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	postings := idx.index[n.Term]
	for _, p := range postings {
		if p.DocID == docID {
			return true
		}
	}
	return false
}

func (n *TermNode) String() string { return n.Term }

// PhraseNode matches a phrase (consecutive terms).
type PhraseNode struct {
	Terms []string
}

func (n *PhraseNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	return idx.phraseInDoc(docID, n.Terms)
}

func (n *PhraseNode) String() string {
	return "\"" + strings.Join(n.Terms, " ") + "\""
}

// PrefixNode matches terms with a given prefix.
type PrefixNode struct {
	Prefix string
}

func (n *PrefixNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	for term, postings := range idx.index {
		if len(term) >= len(n.Prefix) && term[:len(n.Prefix)] == n.Prefix {
			for _, p := range postings {
				if p.DocID == docID {
					return true
				}
			}
		}
	}
	return false
}

func (n *PrefixNode) String() string { return n.Prefix + "*" }

// AndNode matches documents matching both left and right.
type AndNode struct {
	Left, Right QueryNode
}

func (n *AndNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	return n.Left.MatchDoc(idx, docID) && n.Right.MatchDoc(idx, docID)
}

func (n *AndNode) String() string {
	return fmt.Sprintf("(%s AND %s)", n.Left, n.Right)
}

// OrNode matches documents matching either left or right.
type OrNode struct {
	Left, Right QueryNode
}

func (n *OrNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	return n.Left.MatchDoc(idx, docID) || n.Right.MatchDoc(idx, docID)
}

func (n *OrNode) String() string {
	return fmt.Sprintf("(%s OR %s)", n.Left, n.Right)
}

// NotNode matches documents NOT matching the inner node.
type NotNode struct {
	Inner QueryNode
}

func (n *NotNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	return !n.Inner.MatchDoc(idx, docID)
}

func (n *NotNode) String() string {
	return "-" + n.Inner.String()
}

// ColumnNode restricts matching to a specific column.
type ColumnNode struct {
	Column int
	Inner  QueryNode
}

func (n *ColumnNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	// Check if the inner node matches within the specified column
	switch inner := n.Inner.(type) {
	case *TermNode:
		postings := idx.index[inner.Term]
		for _, p := range postings {
			if p.DocID == docID && p.Column == n.Column {
				return true
			}
		}
		return false
	default:
		// Fall back to normal matching (may over-match for column filters)
		return n.Inner.MatchDoc(idx, docID)
	}
}

func (n *ColumnNode) String() string {
	return fmt.Sprintf("col%d:%s", n.Column, n.Inner)
}

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
func ParseMatchQuery(query string) (QueryNode, error) {
	p := &queryParser{
		input: strings.TrimSpace(query),
	}
	result, err := p.parse()
	if err != nil {
		return nil, fmt.Errorf("fts: failed to parse MATCH query %q: %w", query, err)
	}
	return result, nil
}

type queryParser struct {
	input string
	pos   int
}

func (p *queryParser) parse() (QueryNode, error) {
	return p.parseOr()
}

func (p *queryParser) parseOr() (QueryNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		if p.matchKeyword("OR") {
			right, err := p.parseAnd()
			if err != nil {
				return nil, err
			}
			left = &OrNode{Left: left, Right: right}
		} else {
			break
		}
	}
	return left, nil
}

func (p *queryParser) parseAnd() (QueryNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		if p.pos >= len(p.input) {
			break
		}
		if p.matchKeyword("AND") {
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left = &AndNode{Left: left, Right: right}
		} else if p.matchKeyword("NOT") {
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left = &AndNode{Left: left, Right: &NotNode{Inner: right}}
		} else if p.cur() == '-' {
			p.pos++
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left = &AndNode{Left: left, Right: &NotNode{Inner: right}}
		} else if p.cur() == '"' || isWordChar(p.cur()) {
			// Before treating as implicit AND, check if it's actually an OR keyword
			if p.peekKeyword("OR") {
				break
			}
			// Implicit AND
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left = &AndNode{Left: left, Right: right}
		} else {
			break
		}
	}
	return left, nil
}

func (p *queryParser) parseUnary() (QueryNode, error) {
	p.skipWS()
	if p.pos >= len(p.input) {
		return nil, fmt.Errorf("unexpected end of query")
	}
	if p.cur() == '-' {
		p.pos++
		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &NotNode{Inner: inner}, nil
	}
	return p.parsePrimary()
}

func (p *queryParser) parsePrimary() (QueryNode, error) {
	p.skipWS()
	if p.pos >= len(p.input) {
		return nil, fmt.Errorf("unexpected end of query")
	}

	// Phrase: "word word ..."
	if p.cur() == '"' {
		return p.parsePhrase()
	}

	// Word
	word := p.parseWord()
	if word == "" {
		return nil, fmt.Errorf("expected word at position %d", p.pos)
	}

	// Check for column prefix: "word:"
	if p.pos < len(p.input) && p.cur() == ':' {
		// This is a column prefix - parse the value after the colon
		p.pos++ // skip ':'
		p.skipWS()
		// Column name is the word before the colon
		colName := strings.ToLower(word)

		// Parse the inner expression after the colon
		var inner QueryNode
		var err error
		if p.pos < len(p.input) && p.cur() == '"' {
			inner, err = p.parsePhrase()
		} else {
			innerWord := p.parseWord()
			if innerWord == "" {
				return nil, fmt.Errorf("expected word after column prefix %s", colName)
			}
			inner = &TermNode{Term: strings.ToLower(innerWord)}

			// Check for prefix wildcard
			if p.pos < len(p.input) && p.cur() == '*' {
				p.pos++
				inner = &PrefixNode{Prefix: strings.ToLower(innerWord)}
			}
		}
		if err != nil {
			return nil, err
		}

		// Note: We can't resolve column index here because we don't have the table schema.
		// The ColumnIndex is resolved later. For now, store the column name.
		return &ColumnRefNode{ColumnName: colName, Inner: inner}, nil
	}

	// Check for prefix wildcard: "word*"
	if p.pos < len(p.input) && p.cur() == '*' {
		p.pos++
		return &PrefixNode{Prefix: strings.ToLower(word)}, nil
	}

	return &TermNode{Term: strings.ToLower(word)}, nil
}

func (p *queryParser) parsePhrase() (QueryNode, error) {
	if p.cur() != '"' {
		return nil, fmt.Errorf("expected '\"' at position %d", p.pos)
	}
	p.pos++ // skip opening "
	var terms []string
	for {
		p.skipWS()
		if p.pos >= len(p.input) {
			return nil, fmt.Errorf("unterminated string literal")
		}
		if p.cur() == '"' {
			p.pos++ // skip closing "
			break
		}
		word := p.parseWord()
		if word == "" {
			return nil, fmt.Errorf("expected word in phrase at position %d", p.pos)
		}
		terms = append(terms, strings.ToLower(word))
	}

	if len(terms) == 0 {
		// Empty phrase matches nothing
		return &TermNode{Term: ""}, nil
	}
	if len(terms) == 1 {
		// Single term in quotes is the same as the term
		return &TermNode{Term: terms[0]}, nil
	}
	return &PhraseNode{Terms: terms}, nil
}

func (p *queryParser) parseWord() string {
	p.skipWS()
	start := p.pos
	for p.pos < len(p.input) && isWordChar(p.cur()) {
		p.pos++
	}
	return p.input[start:p.pos]
}

func (p *queryParser) matchKeyword(keyword string) bool {
	p.skipWS()
	if p.pos+len(keyword) > len(p.input) {
		return false
	}
	upper := strings.ToUpper(p.input[p.pos : p.pos+len(keyword)])
	if upper != keyword {
		return false
	}
	// Check that the keyword is followed by whitespace or end of input
	next := p.pos + len(keyword)
	if next < len(p.input) && isWordChar(p.input[next]) {
		return false
	}
	p.pos = next
	return true
}

// peekKeyword checks if the next characters form a keyword without consuming them.
func (p *queryParser) peekKeyword(keyword string) bool {
	// Save position, check, restore
	saved := p.pos
	defer func() { p.pos = saved }()
	return p.matchKeyword(keyword)
}

func (p *queryParser) skipWS() {
	for p.pos < len(p.input) && (p.input[p.pos] == ' ' || p.input[p.pos] == '\t' || p.input[p.pos] == '\n') {
		p.pos++
	}
}

func (p *queryParser) cur() byte {
	if p.pos < len(p.input) {
		return p.input[p.pos]
	}
	return 0
}

func isWordChar(c byte) bool {
	return unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '_'
}

// ColumnRefNode is a temporary node that references a column by name.
// This is resolved to ColumnNode with the correct column index during
// query planning.
type ColumnRefNode struct {
	ColumnName string
	Inner      QueryNode
}

func (n *ColumnRefNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	// Without column resolution, match on any column
	return n.Inner.MatchDoc(idx, docID)
}

func (n *ColumnRefNode) String() string {
	return n.ColumnName + ":" + n.Inner.String()
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
		if !(*seen)[n.Term] {
			(*seen)[n.Term] = true
			*terms = append(*terms, n.Term)
		}
	case *PrefixNode:
		if !(*seen)[n.Prefix] {
			(*seen)[n.Prefix] = true
			*terms = append(*terms, n.Prefix)
		}
	case *PhraseNode:
		for _, t := range n.Terms {
			if !(*seen)[t] {
				(*seen)[t] = true
				*terms = append(*terms, t)
			}
		}
	case *AndNode:
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
