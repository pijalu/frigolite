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
	// First marks the FTS4 "^term" column-first operator: the term must be
	// the FIRST token of a column (position 0).
	First bool
}

func (n *TermNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	postings := idx.index[n.Term]
	for _, p := range postings {
		if p.DocID == docID {
			// "^term": the term must be the column's first token.
			if n.First && p.Position != 0 {
				continue
			}
			return true
		}
	}
	return false
}

func (n *TermNode) String() string { return n.Term }

// PhraseNode matches a phrase (consecutive terms). Prefixes marks terms that
// are prefix wildcards ("lin* app*" — a phrase of prefix terms).
type PhraseNode struct {
	Terms    []string
	Prefixes []bool // parallel to Terms: true when the term is a prefix wildcard
	// First marks the "^"..." column-first operator: the phrase's first
	// token must be at position 0 of a column.
	First bool
	// FirstAt generalizes First for mid-phrase markers ('"K ^H"': token 1
	// would have to sit at position 0 while following K — impossible, so
	// the phrase never matches). -1 when no ^ marker applies.
	FirstAt int
}

func (n *PhraseNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	if n.FirstAt > 0 {
		// The marked token would have to sit at position 0 while preceding
		// tokens occupy earlier positions — impossible (fts3first '"K ^H"').
		return false
	}
	if n.First || n.FirstAt == 0 {
		return idx.phraseInDocFirst(docID, n.Terms, n.Prefixes)
	}
	return idx.phraseInDoc(docID, n.Terms, n.Prefixes)
}

func (n *PhraseNode) String() string {
	return "\"" + strings.Join(n.Terms, " ") + "\""
}

// PrefixNode matches terms with a given prefix.
type PrefixNode struct {
	Prefix string
	// First marks the "^pre*" column-first operator.
	First bool
}

func (n *PrefixNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	for term, postings := range idx.index {
		if !prefixMatches(n.Prefix, term) {
			continue
		}
		for _, p := range postings {
			if p.DocID == docID && (!n.First || p.Position == 0) {
				return true
			}
		}
	}
	return false
}

// prefixMatches reports whether term starts with the given prefix.
func prefixMatches(prefix, term string) bool {
	return len(term) >= len(prefix) && term[:len(prefix)] == prefix
}

func (n *PrefixNode) String() string { return n.Prefix + "*" }

// TailDropNode is a neutral AND operand produced when a '(' opens at the end
// of the query with no closing ')' (fts3_expr.c: the recursive fts3ExprParse
// consumes to end-of-input and returns SQLITE_DONE with no expression, so
// everything after the unclosed '(' is dropped — the query is the phrases
// before it). Matching everything makes `x AND (unclosed` equivalent to `x`
// (fts3snippet2.test 3.1's binary blob).
type TailDropNode struct{}

func (n *TailDropNode) MatchDoc(idx *InvertedIndex, docID int64) bool { return true }

func (n *TailDropNode) String() string { return "(..." }

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

// NearNode matches documents where the left and right phrases occur within
// Distance tokens of each other (SQLite's NEAR operator: `A NEAR/n B`). The
// default distance is 10 (SQLITE_FTS3_DEFAULT_NEAR_PARAM).
type NearNode struct {
	Left, Right QueryNode
	Distance    int
}

// MatchDoc implements QueryNode. SQLite's NEAR semantics (fts3.c
// fts3PoslistNearMerge / fts3EvalNearTrim): a phrase position is the offset of
// the phrase's LAST token, and positions in different columns never pair
// (fts3PoslistPhraseMerge only merges when iCol1==iCol2). A NEAR/n match
// requires a left position a and a right position b in the same column with
//
//	(b > a && b-a <= n+len(right))  (right occurs after left)
//	or
//	(a > b && a-b <= n+len(left))   (right occurs before left)
//
// The phrase-length terms account for the tokens inside each phrase: the gap
// between the phrase spans must be at most n tokens.
func (n *NearNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	leftPos, leftLen := n.phrasePositions(idx, docID, n.Left)
	if len(leftPos) == 0 {
		return false
	}
	rightPos, rightLen := n.phrasePositions(idx, docID, n.Right)
	if len(rightPos) == 0 {
		return false
	}
	for _, a := range leftPos {
		for _, b := range rightPos {
			if a.col != b.col {
				continue
			}
			if nearPairWithin(a, b, n.Distance, leftLen, rightLen) {
				return true
			}
		}
	}
	return false
}

// nearPairWithin reports whether one left occurrence and one right occurrence
// pair within the NEAR distance. fts3PoslistPhraseMerge pairs positions
// directionally: right occurrences within Distance+len(right), left
// occurrences within Distance+len(left) — including two occurrences at the
// SAME offset (identical phrases, fts3corrupt6 2.1 "(1 NEAR 1)").
func nearPairWithin(a, b nearPos, distance, leftLen, rightLen int) bool {
	if b.pos >= a.pos && b.pos-a.pos <= distance+rightLen {
		return true
	}
	return a.pos >= b.pos && a.pos-b.pos <= distance+leftLen
}

// nearPos is a phrase occurrence position: the last token offset plus the
// column it occurs in (NEAR only pairs positions in the same column).
type nearPos struct {
	pos int
	col int
}

// phrasePositions collects the last-token positions of every occurrence of a
// phrase-like node (term, prefix, phrase, or a column-restricted version of
// those) in a document, plus the phrase's token count. Non-phrase nodes (AND,
// OR, NOT, NEAR) have no single position list; SQLite restricts NEAR operands
// to phrases, so such operands contribute no positions (no match).
func (n *NearNode) phrasePositions(idx *InvertedIndex, docID int64, node QueryNode) ([]nearPos, int) {
	switch v := node.(type) {
	case *TermNode:
		return collectTermNearPositions(idx, docID, v.Term, -1), 1
	case *PrefixNode:
		return collectPrefixNearPositions(idx, docID, v.Prefix, -1), 1
	case *PhraseNode:
		return collectPhraseNearPositions(idx, docID, v.Terms, v.Prefixes, -1), len(v.Terms)
	case *ColumnNode:
		return n.columnPhrasePositions(idx, docID, v)
	case *NearNode:
		return n.nearPhrasePositions(idx, docID, v)
	}
	return nil, 0
}

// columnPhrasePositions returns the positions of a column-restricted
// phrase-like node (SQL `col MATCH ...` restricts the phrase to one column).
func (n *NearNode) columnPhrasePositions(idx *InvertedIndex, docID int64, v *ColumnNode) ([]nearPos, int) {
	switch inner := v.Inner.(type) {
	case *TermNode:
		return collectTermNearPositions(idx, docID, inner.Term, v.Column), 1
	case *PrefixNode:
		return collectPrefixNearPositions(idx, docID, inner.Prefix, v.Column), 1
	case *PhraseNode:
		return collectPhraseNearPositions(idx, docID, inner.Terms, inner.Prefixes, v.Column), len(inner.Terms)
	}
	return nil, 0
}

// nearPhrasePositions returns the position list of a chained NEAR expression.
// A chained NEAR (A NEAR B NEAR C) builds left-associatively: (A NEAR B) NEAR
// C. The inner NEAR's position list is its RIGHT operand's positions that are
// near its LEFT operand (fts3PoslistNearMerge writes the right list), and its
// token count is the right operand's. The outer NEAR then compares the inner's
// output positions against its own right operand.
func (n *NearNode) nearPhrasePositions(idx *InvertedIndex, docID int64, v *NearNode) ([]nearPos, int) {
	leftPos, leftLen := n.phrasePositions(idx, docID, v.Left)
	if len(leftPos) == 0 {
		return nil, 0
	}
	rightPos, rightLen := n.phrasePositions(idx, docID, v.Right)
	var out []nearPos
	for _, a := range leftPos {
		for _, b := range rightPos {
			if a.col == b.col && ((b.pos > a.pos && b.pos-a.pos <= v.Distance+rightLen) || (a.pos > b.pos && a.pos-b.pos <= v.Distance+leftLen)) {
				out = append(out, b)
			}
		}
	}
	if len(out) == 0 {
		return nil, 0
	}
	return out, rightLen
}

func (n *NearNode) String() string {
	return fmt.Sprintf("(%s NEAR/%d %s)", n.Left, n.Distance, n.Right)
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
	case *PhraseNode:
		return idx.phraseInDocColumn(docID, inner.Terms, inner.Prefixes, n.Column)
	case *PrefixNode:
		return idx.prefixInDocColumn(docID, inner.Prefix, n.Column)
	default:
		// Fall back to normal matching (may over-match for column filters)
		return n.Inner.MatchDoc(idx, docID)
	}
}

func (n *ColumnNode) String() string {
	return fmt.Sprintf("col%d:%s", n.Column, n.Inner)
}

func ParseMatchQuery(query string) (QueryNode, error) {
	// SQLite parses the MATCH query with an implicit strlen length
	// (fts3.c fts3FilterMethod: sqlite3Fts3ExprParse(..., zQuery, -1, ...)), so
	// the query ends at the first NUL byte. A binary blob MATCH RHS may
	// contain embedded NULs that terminate the query (fts3snippet2.test 3.1:
	// the query's post-quote tail is dropped at a \x00 byte).
	if idx := strings.IndexByte(query, 0); idx >= 0 {
		query = query[:idx]
	}
	p := &queryParser{
		input: strings.TrimSpace(query),
	}
	result, err := p.parse()
	if err != nil {
		return nil, fmt.Errorf("malformed MATCH expression: [%s]", query)
	}
	// Mismatched parentheses are a syntax error (fts3_expr.c
	// fts3ExprParseUnbalanced: nNest > 0 → SQLITE_ERROR; fts3.c maps it to
	// "malformed MATCH expression: [query]" — fts3expr 2.1-2.7).
	if p.nest != 0 {
		return nil, fmt.Errorf("malformed MATCH expression: [%s]", query)
	}
	// An implicit-AND whose EVERY operand is negated ('-a -b') has no
	// positive phrase; the era's expression parser rejects it as malformed
	// (fts3ag 4.x), while a mix ('-a b') is valid (fts3aa-3.3 "-two one").
	if allOperandsNegated(result) {
		return nil, fmt.Errorf("malformed MATCH expression: [%s]", query)
	}
	return result, nil
}

type queryParser struct {
	input string
	pos   int
	// nest tracks open grouping parens (fts3_expr.c ParseContext.nNest).
	// After a successful parse any non-zero nest is a syntax error, and a
	// grouping ')' with nest==0 is one too (fts3ExprParseUnbalanced).
	nest int
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
	left, err := p.parseNear()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		if err := p.skipAndDelimiters(); err != nil {
			return nil, err
		}
		if p.pos >= len(p.input) {
			break
		}
		next, ok, err := p.parseAndOperand(left)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		left = next
	}
	return left, nil
}

// skipAndDelimiters advances past non-word separator bytes between
// implicit-AND operands, matching SQLite's tokenizer which skips all
// non-token bytes (fts3_expr.c getNextString). A '(' or ')' is only a
// grouping operator at a whitespace boundary: when it follows a
// non-whitespace separator (e.g. \x05 in a binary blob MATCH RHS) the
// tokenizer consumes it as a delimiter (fts3snippet2.test 3.1).
func (p *queryParser) skipAndDelimiters() error {
	for p.pos < len(p.input) && !isWordCharAt(p.input, p.pos) {
		c := p.cur()
		// A '(' or ')' is a grouping operator unless it directly follows a
		// hard delimiter byte (a non-whitespace, non-word, non-paren
		// separator like \x05 in a binary blob MATCH RHS), in which case
		// the tokenizer consumes it as a delimiter (fts3snippet2.test
		// 3.1's blob). After whitespace, a word, or another paren it is a
		// real operator.
		if c == '(' || c == ')' {
			stop, err := p.classifyParen(c)
			if err != nil {
				return err
			}
			if stop {
				break
			}
			continue
		}
		if c == ':' || c == '*' || c == '-' || c == '"' || c == '^' {
			break
		}
		p.pos++
	}
	return nil
}

// classifyParen handles a '(' or ')' inside the implicit-AND delimiter scan.
// Returns stop=true when it is a real grouping operator (the scan must stop);
// a paren directly following a hard delimiter byte is tokenizer content and
// is consumed. A grouping ')' with no matching '(' is a syntax error
// (fts3_expr.c fts3ExprParse: SQLITE_ERROR; fts3expr 2.1/2.7 "malformed MATCH
// expression").
func (p *queryParser) classifyParen(c byte) (bool, error) {
	if p.pos > 0 && isHardDelim(p.input[p.pos-1]) {
		p.pos++
		return false, nil
	}
	if c == ')' && p.nest == 0 {
		return false, fmt.Errorf("unexpected ')'")
	}
	return true, nil
}

// parseNear parses a NEAR chain: phrase (NEAR[/n] phrase)*. NEAR binds more
// tightly than NOT/AND/OR (fts3_expr.c opPrecedence: NEAR=1 is the tightest
// binary operator). An explicit distance `NEAR/n` (e.g. NEAR/6) sets the token
// window; a bare NEAR uses SQLite's default of 10.
func (p *queryParser) parseNear() (QueryNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		if p.pos+4 > len(p.input) {
			break
		}
		upper := strings.ToUpper(p.input[p.pos : p.pos+4])
		if upper != "NEAR" {
			break
		}
		distance, ok := p.parseNearDistance()
		if !ok {
			break
		}
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		// NEAR binds phrases only: an AND/OR (or parenthesized group) operand
		// is a syntax error in the era's grammar (fts3expr 3.x "(hello OR
		// world) NEAR one" → malformed MATCH expression).
		if !isNearOperand(left) || !isNearOperand(right) {
			return nil, fmt.Errorf("NEAR operands must be phrases")
		}
		left = &NearNode{Left: left, Right: right, Distance: distance}
	}
	return left, nil
}

// isNearOperand reports whether node is a valid NEAR operand: a phrase-like
// leaf or another NEAR (fts3_expr.c: NEAR may not combine AND/OR subtrees).
func isNearOperand(n QueryNode) bool {
	switch n.(type) {
	case *TermNode, *PrefixNode, *PhraseNode, *NearNode, *ColumnNode, *ColumnRefNode:
		return true
	default:
		return false
	}
}

// parseAndOperand parses one AND / NOT / implicit-AND operand and combines
// it with left. Returns (combined, true) when an operand was consumed,
// (nil, false) when the loop should stop (end of expression or an OR).
func (p *queryParser) parseAndOperand(left QueryNode) (QueryNode, bool, error) {
	if p.matchKeyword("AND") {
		return p.combineUnary(left, false)
	}
	if p.matchKeyword("NOT") {
		return p.combineUnary(left, true)
	}
	if p.cur() == '-' {
		p.pos++
		return p.combineUnary(left, true)
	}
	if p.cur() == '^' {
		// Implicit AND with a column-first operand ("V ^-E" = V AND
		// NOT(^E), fts3first.test).
		p.pos++
		p.skipWS()
		inner, negated, err := p.parseCaretOperand()
		if err != nil {
			return nil, false, err
		}
		markFirst(inner)
		if negated {
			inner = &NotNode{Inner: inner}
		}
		return &AndNode{Left: left, Right: inner}, true, nil
	}
	if p.cur() == '"' || isWordCharAt(p.input, p.pos) || p.cur() == '(' {
		// Before treating as implicit AND, check if it's actually an OR keyword
		if p.peekKeyword("OR") {
			return nil, false, nil
		}
		// Implicit AND
		right, err := p.parseUnary()
		if err != nil {
			return nil, false, err
		}
		return &AndNode{Left: left, Right: right}, true, nil
	}
	return nil, false, nil
}

// combineUnary parses the right operand of an AND/NOT and combines it with
// left. negated selects NOT semantics ("a NOT b", "-a b").
func (p *queryParser) combineUnary(left QueryNode, negated bool) (QueryNode, bool, error) {
	right, err := p.parseUnary()
	if err != nil {
		return nil, false, err
	}
	if negated {
		right = &NotNode{Inner: right}
	}
	return &AndNode{Left: left, Right: right}, true, nil
}

// parseCaretOperand parses the operand after a consumed '^': an optional '-'
// marks NOT composition ("V ^-E" = V AND NOT(^E), fts3first.test). Returns
// the operand node and whether it is negated.
func (p *queryParser) parseCaretOperand() (QueryNode, bool, error) {
	negated := false
	if p.cur() == '-' {
		p.pos++
		p.skipWS()
		negated = true
	}
	node, err := p.parseUnary()
	return node, negated, err
}

// markFirst marks the top phrase-like node of the tree as column-first.
func markFirst(n QueryNode) {
	switch t := n.(type) {
	case *TermNode:
		t.First = true
	case *PrefixNode:
		t.First = true
	case *PhraseNode:
		t.First = true
	}
}

func (p *queryParser) parseUnary() (QueryNode, error) {
	p.skipWS()
	if p.pos >= len(p.input) {
		return nil, fmt.Errorf("unexpected end of query")
	}
	// The FTS4 column-first operator: "^term" / '^"a b"' — the term or
	// phrase must be the first token(s) of a column (fts3first.test).
	// "^-term" composes with NOT: NOT(^term) ("V ^-E" family).
	if p.cur() == '^' {
		p.pos++
		p.skipWS()
		node, negated, err := p.parseCaretOperand()
		if err != nil {
			return nil, err
		}
		markFirst(node)
		if negated {
			return &NotNode{Inner: node}, nil
		}
		return node, nil
	}
	if p.cur() == '-' {
		p.pos++
		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &NotNode{Inner: inner}, nil
	}
	if p.cur() == '(' {
		return p.parseParenGroup()
	}
	return p.parsePrimary()
}

// parseParenGroup parses a parenthesized ( expr ) group. A '(' that directly
// follows a hard delimiter byte (a non-word, non-space separator like the
// \x05 in fts3snippet2 3.1's binary blob MATCH RHS) is tokenizer content,
// not a grouping operator — skip it like the other delimiter bytes.
func (p *queryParser) parseParenGroup() (QueryNode, error) {
	if p.pos > 0 && isHardDelim(p.input[p.pos-1]) {
		p.pos++
		return p.parseUnary()
	}
	// Parenthesized expression: ( expr ) — AND/OR precedence group.
	p.pos++
	p.nest++
	inner, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos >= len(p.input) {
		// An unclosed '(' at the end of the query: SQLite's recursive
		// fts3ExprParse consumes to end-of-input; the leftover open paren
		// leaves ParseContext.nNest > 0 and fts3ExprParseUnbalanced then
		// reports the mismatch as an error. The parser returns a neutral
		// TailDropNode here and the nest check in ParseMatchQuery turns
		// it into "malformed MATCH expression" (fts3expr 2.2-2.7).
		return &TailDropNode{}, nil
	}
	if p.cur() != ')' {
		return nil, fmt.Errorf("missing ')' in query at position %d", p.pos)
	}
	p.pos++
	p.nest--
	return inner, nil
}

func (p *queryParser) parsePrimary() (QueryNode, error) {
	p.skipWS()
	if p.pos >= len(p.input) {
		return nil, fmt.Errorf("unexpected end of query")
	}

	// FTS4 column-first operator: "^term", "^pre*", '^"phrase"' (fts3first).
	// "^-term" composes with NOT: NOT(^term) — fts3first's "V ^-E" family.
	first, negateFirst := p.parseCaretPrefix()

	// Phrase: "word word ..." or 'word word ...'
	if p.cur() == '"' || p.cur() == '\'' {
		return p.parseFirstMarked(first, negateFirst)
	}

	// Skip unrecognized characters (e.g. '+', control bytes) like SQLite's
	// query tokenizer does (fts3_expr.c getNextString uses the table's
	// tokenizer, which skips non-token bytes; fts3matchinfo2 passes a binary
	// blob as the MATCH RHS). Only word chars, ':', '*', '"', '(', ')', and
	// '-' are meaningful to the hand-written parser.
	if !isWordCharAt(p.input, p.pos) && p.cur() != ':' && p.cur() != '*' && p.cur() != '-' && p.cur() != '(' && p.cur() != ')' {
		p.pos++
		return p.parsePrimary()
	}

	if err := p.checkOperatorKeyword(); err != nil {
		return nil, err
	}

	// Word
	word := p.parseWord()
	if word == "" {
		return nil, fmt.Errorf("expected word at position %d", p.pos)
	}
	return p.parseWordTail(word, first, negateFirst)
}

// parseWordTail completes a bare word operand: a column prefix ("word:"),
// a prefix wildcard ("word*"), or a plain term.
func (p *queryParser) parseWordTail(word string, first, negateFirst bool) (QueryNode, error) {
	// Check for column prefix: "word:"
	if p.pos < len(p.input) && p.cur() == ':' {
		return p.parseColumnPrefix(word)
	}
	// Check for prefix wildcard: "word*"
	if p.pos < len(p.input) && p.cur() == '*' {
		p.pos++
		return wrapFirst(&PrefixNode{Prefix: word, First: first}, negateFirst), nil
	}
	return wrapFirst(&TermNode{Term: word, First: first}, negateFirst), nil
}

// parseFirstMarked parses a quoted phrase and applies the column-first
// markers from a preceding '^'/'^-'.
func (p *queryParser) parseFirstMarked(first, negateFirst bool) (QueryNode, error) {
	node, err := p.parsePhrase()
	if err != nil {
		return nil, err
	}
	switch tn := node.(type) {
	case *TermNode:
		tn.First = first
	case *PrefixNode:
		pn := tn
		pn.First = first
	case *PhraseNode:
		pn := tn
		pn.First = first
	}
	return wrapFirst(node, negateFirst), nil
}

// wrapFirst wraps node in a NOT when negateFirst is set ("^-term").
func wrapFirst(node QueryNode, negateFirst bool) QueryNode {
	if negateFirst {
		return &NotNode{Inner: node}
	}
	return node
}

// parseColumnPrefix parses a column-scoped term "col:value", where value may
// be a phrase, a plain word, or a prefix wildcard.
func (p *queryParser) parseColumnPrefix(word string) (QueryNode, error) {
	p.pos++ // skip ':'
	p.skipWS()
	// Column name is the word before the colon
	colName := asciiLowerBytes(word)

	// Parse the inner expression after the colon
	if p.pos < len(p.input) && p.cur() == '"' {
		inner, err := p.parsePhrase()
		if err != nil {
			return nil, err
		}
		return &ColumnRefNode{ColumnName: colName, Inner: inner}, nil
	}

	innerWord := p.parseWord()
	if innerWord == "" {
		return nil, fmt.Errorf("expected word after column prefix %s", colName)
	}
	var inner QueryNode = &TermNode{Term: innerWord}

	// Check for prefix wildcard
	if p.pos < len(p.input) && p.cur() == '*' {
		p.pos++
		inner = &PrefixNode{Prefix: innerWord}
	}

	// Note: We can't resolve column index here because we don't have the table schema.
	// The ColumnIndex is resolved later. For now, store the column name.
	return &ColumnRefNode{ColumnName: colName, Inner: inner}, nil
}

func (p *queryParser) parsePhrase() (QueryNode, error) {
	// Phrases may be quoted with either double quotes or single quotes
	// (fts3corrupt5 4.x: MATCH '"""b:one"""' strips to a single-quoted FTS
	// phrase).
	quote := p.cur()
	if quote != '"' && quote != '\'' {
		return nil, fmt.Errorf("expected quote at position %d", p.pos)
	}
	p.pos++ // skip opening quote
	var terms []string
	var prefixes []bool
	firstAt := -1 // token index marked with ^ (must sit at position 0)
	p.skipWS()
	if p.cur() == '^' {
		// '^' right after the opening quote marks token 0.
		p.pos++
		p.skipWS()
		firstAt = 0
	}
	for {
		p.skipWS()
		// A '^' before a phrase token marks that token as the column's
		// first (fts3first '"K ^H"'). Checked BEFORE the delimiter skip
		// loop, which would otherwise consume the '^'.
		if p.cur() == '^' {
			p.pos++
			p.skipWS()
			firstAt = len(terms)
		}
		done, err := p.scanPhraseToken(quote, &terms, &prefixes)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
	}

	if len(terms) == 0 {
		// Empty phrase matches nothing
		return &TermNode{Term: ""}, nil
	}
	if firstAt >= 0 {
		// A ^ marker makes this a column-first phrase even for a single
		// token ('"^K"' is not the same as 'K').
		return &PhraseNode{Terms: terms, Prefixes: prefixes, FirstAt: firstAt}, nil
	}
	if len(terms) == 1 {
		// Single term in quotes is the same as the term (or a prefix)
		if prefixes[0] {
			return &PrefixNode{Prefix: terms[0]}, nil
		}
		return &TermNode{Term: terms[0]}, nil
	}
	return &PhraseNode{Terms: terms, Prefixes: prefixes, FirstAt: -1}, nil
}

// scanPhraseToken scans one token inside a quoted phrase (or the closing
// quote). Skips non-word separator bytes — the tokenizer skips all non-token
// bytes within a quoted string (fts3_expr.c getNextString), so a '(' or ')'
// inside a quoted phrase is phrase content, not a grouping operator
// (fts3snippet2.test 3.1's binary query has a quoted phrase containing a '(').
// Appends the term/prefix to terms/prefixes; returns done=true when the
// closing quote was consumed.
func (p *queryParser) scanPhraseToken(quote byte, terms *[]string, prefixes *[]bool) (bool, error) {
	// Skip non-word separator bytes (punctuation, parens, control chars)
	// between phrase tokens.
	for p.pos < len(p.input) && !isWordCharAt(p.input, p.pos) && p.cur() != quote {
		p.pos++
	}
	if p.pos >= len(p.input) {
		return false, fmt.Errorf("unterminated string literal")
	}
	if p.cur() == quote {
		p.pos++ // skip closing quote
		return true, nil
	}
	word := p.parseWord()
	if word == "" {
		return false, fmt.Errorf("expected word in phrase at position %d", p.pos)
	}
	isPrefix := false
	if p.pos < len(p.input) && p.cur() == '*' {
		p.pos++
		isPrefix = true
	}
	*terms = append(*terms, asciiLowerBytes(word))
	*prefixes = append(*prefixes, isPrefix)
	return false, nil
}

func (p *queryParser) parseWord() string {
	p.skipWS()
	start := p.pos
	for p.pos < len(p.input) && isWordCharAt(p.input, p.pos) {
		p.pos++
	}
	return p.input[start:p.pos]
}

func (p *queryParser) matchKeyword(keyword string) bool {
	p.skipWS()
	if p.pos+len(keyword) > len(p.input) {
		return false
	}
	// FTS3 MATCH keywords are CASE-SENSITIVE uppercase (fts3_expr.c
	// getNextToken: memcmp against "AND"/"OR"/"NOT"/"NEAR" — fts4incr 2.1
	// matches the plain term 'and').
	if p.input[p.pos:p.pos+len(keyword)] != keyword {
		return false
	}
	// Check that the keyword is followed by whitespace or end of input
	next := p.pos + len(keyword)
	if next < len(p.input) && isWordCharAt(p.input, next) {
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

// isWordCharAt reports whether the byte at input[pos] begins a token
// character for the simple tokenizer: an ASCII letter/digit/underscore or any
// byte >= 0x80 (fts3_tokenizer1.c simpleDelim: `c<0x80 && delim[c]` — high
// bytes are never delimiters, so they are token chars; fts3snippet2.test 3.1's
// binary query blob has high-byte tokens).
func isWordCharAt(input string, pos int) bool {
	if pos >= len(input) {
		return false
	}
	c := input[pos]
	if c < 0x80 {
		return unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '_'
	}
	// Bytes >= 0x80 are token characters (fts3_tokenizer1.c simpleDelim:
	// `c<0x80 && delim[c]` — high bytes are never delimiters, so they are
	// part of a token; fts3snippet2.test 3.1's binary doc/query).
	return true
}

// isASCIIWordChar reports whether b is an ASCII word character (letter, digit,
// or underscore).
func isASCIIWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// isHardDelim reports whether b is a hard delimiter byte: a non-whitespace,
// non-word, non-paren separator (e.g. \x05, '.'). The simple tokenizer treats
// such bytes as token separators (fts3_tokenizer1.c simpleDelim), and a '(' or
// ')' that directly follows one is consumed as part of the delimiter run
// rather than being a grouping operator.
func isHardDelim(b byte) bool {
	if b >= 0x80 {
		return false
	}
	return !isSpaceByte(b) && !isASCIIWordChar(b) && b != '(' && b != ')' && b != ':' && b != '*' && b != '-'
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

// parseCaretPrefix consumes an optional leading '^' (FTS4 column-first
// operator) and an optional following '-' (NOT composition). Returns
// (first, negateFirst).
func (p *queryParser) parseCaretPrefix() (bool, bool) {
	if p.cur() != '^' {
		return false, false
	}
	p.pos++
	p.skipWS()
	if p.cur() == '-' {
		p.pos++
		p.skipWS()
		return true, true
	}
	return true, false
}

// checkOperatorKeyword rejects a boolean operator keyword at operand
// position: it has no left operand, and the era's grammar treats it as a
// syntax error (fts3expr 3.x "OR hello world", "one (OR hello world) two").
func (p *queryParser) checkOperatorKeyword() error {
	for _, kw := range []string{"OR", "AND", "NOT"} {
		if len(p.input)-p.pos >= len(kw) {
			// Case-sensitive: lowercase 'and'/'or' are plain terms
			// (fts3_expr.c memcmp; fts4incr 2.1).
			if p.input[p.pos:p.pos+len(kw)] == kw {
				after := p.pos + len(kw)
				// Keyword boundary: only ASCII bytes can delimit a keyword;
				// UTF-8 continuation bytes (>=0x80) are token content
				// (fts3_expr.c getNextToken: "OR\xd7" is a plain term).
				if after >= len(p.input) || (p.input[after] < 0x80 && !isASCIIWordChar(p.input[after])) {
					return fmt.Errorf("unexpected %s", kw)
				}
			}
		}
	}
	return nil
}
