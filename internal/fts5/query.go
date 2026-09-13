package fts5

import (
	"strings"
)

// This file ports the fts5 MATCH query language (fts5_expr.c + fts5parse.y):
// the lexer (fts5ExprGetToken — double-quoted strings, barewords, the
// case-sensitive AND/OR/NOT keywords), the operator precedence (OR < AND <
// NOT < implicit AND < COLON) and the parse-time column-filter application
// (fts5ParseSetColset merges colsets down the tree; an empty merge becomes an
// EOF node that matches nothing). Evaluation resolves nodes to matching rowid
// sets against the inverted index.
//
// Grammar notes proven against SQLite:
//   - implicit AND joins cnearsets only (phrases, NEAR(...) calls and
//     "colset : nearset"); a parenthesized expression cannot join a chain
//     ("two (four)" parses as a NEAR call named "two" and fails).
//   - a word followed by "(" is always a NEAR call: non-"NEAR" words fail
//     with `fts5: syntax error near "<word>"` (case-sensitive).
//   - NOT's right operand absorbs implicit ANDs ("a NOT b c" = a NOT (b c)).
//   - a phrase with zero tokens (MATCH '""') is an EOF node (no error).

// qTerm is one token of a phrase; Prefix marks an abc* term.
type qTerm struct {
	term   string
	prefix bool
}

// phraseNode is one phrase: a chain of adjacent tokens (fts5ExprPhrase).
// First marks a ^phrase (the first token sits at token position 0 of a
// column); Colset is the effective column filter (nil = every column).
type phraseNode struct {
	terms  []qTerm
	first  bool
	colset []int
	// idx is the phrase's position in the query's phrase list (the aux API's
	// xInst phrase numbers).
	idx int
}

// size returns the phrase's token count (xPhraseSize).
func (p *phraseNode) size() int { return len(p.terms) }

// queryNode is a parsed MATCH expression.
type queryNode interface {
	// eval returns the matching rowids.
	eval(t *Table) (map[int64]bool, error)
}

// eofNode is FTS5_EOF: a node that matches no documents (a zero-token phrase
// or a column filter merged down to nothing).
type eofNode struct{}

func (eofNode) eval(*Table) (map[int64]bool, error) { return map[int64]bool{}, nil }

// stringNode is FTS5_STRING: a near cluster of one or more phrases
// (fts5ExprNearset). Window is the NEAR distance; with a single phrase it is
// inert (NEAR(a, 5) matches like a).
type stringNode struct {
	phrases []*phraseNode
	window  int
}

// andNode, orNode, notNode are the boolean combinators (n-ary AND/OR like
// fts5ExprAddChildren, binary NOT).
type andNode struct{ children []queryNode }
type orNode struct{ children []queryNode }
type notNode struct{ l, r queryNode }

// FTS5DefaultNearDist mirrors FTS5_DEFAULT_NEARDIST.
const FTS5DefaultNearDist = 10

// maxExprDepth is SQLITE_FTS5_MAX_EXPR_DEPTH: the tallest expression tree
// the parser accepts (fts5ParseNode's depth guard).
const maxExprDepth = 256

// ExprDepthError is fts5ParseNode's over-deep expression failure.
type ExprDepthError struct{}

func (e *ExprDepthError) Error() string {
	return "fts5 expression tree is too large (maximum depth 256)"
}

// exprHeight returns the node's distance to its deepest leaf
// (Fts5ExprNode.iHeight: the node is malloc-zeroed, so leaf STRING/EOF
// nodes are height 0 and each combinator adds one).
func exprHeight(n queryNode) int {
	switch x := n.(type) {
	case andNode:
		h := 0
		for _, c := range x.children {
			if ch := exprHeight(c); ch > h {
				h = ch
			}
		}
		return h + 1
	case orNode:
		h := 0
		for _, c := range x.children {
			if ch := exprHeight(c); ch > h {
				h = ch
			}
		}
		return h + 1
	case notNode:
		h := exprHeight(x.l)
		if r := exprHeight(x.r); r > h {
			h = r
		}
		return h + 1
	default:
		return 0
	}
}

// combineAND combines operands under one AND node, flattening same-type
// children (fts5ExprAddChildren) and enforcing the depth guard.
func (p *qParser) combineAND(children ...queryNode) (queryNode, error) {
	return combineAndNodes(children)
}

// combineOR is combineAND for OR nodes.
func (p *qParser) combineOR(children ...queryNode) (queryNode, error) {
	return combineOrNodes(children)
}

// combineAndNodes is the package-level AND combinator: same-type children
// flatten into one n-ary node (fts5ExprAddChildren) and the depth guard
// applies (sqlite3Fts5ParseNode's AND/OR branch).
func combineAndNodes(children []queryNode) (queryNode, error) {
	return combineCombinatorNodes(children, func(kids []queryNode) queryNode {
		return andNode{children: kids}
	}, func(c queryNode) ([]queryNode, bool) {
		n, ok := c.(andNode)
		return n.children, ok
	})
}

// combineOrNodes is combineAndNodes for OR nodes.
func combineOrNodes(children []queryNode) (queryNode, error) {
	return combineCombinatorNodes(children, func(kids []queryNode) queryNode {
		return orNode{children: kids}
	}, func(c queryNode) ([]queryNode, bool) {
		n, ok := c.(orNode)
		return n.children, ok
	})
}

// combineCombinatorNodes flattens same-type children and applies the depth
// check (sqlite3Fts5ParseNode's AND/OR branch via fts5ExprAddChildren).
func combineCombinatorNodes(children []queryNode, build func([]queryNode) queryNode, isSame func(queryNode) ([]queryNode, bool)) (queryNode, error) {
	var kids []queryNode
	for _, c := range children {
		if sub, ok := isSame(c); ok {
			kids = append(kids, sub...)
			continue
		}
		kids = append(kids, c)
	}
	h := 0
	for _, k := range kids {
		if ch := exprHeight(k); ch > h {
			h = ch
		}
	}
	if h+1 > maxExprDepth {
		return nil, &ExprDepthError{}
	}
	return build(kids), nil
}

// combineNOT combines a binary NOT with the depth guard.
func (p *qParser) combineNOT(l, r queryNode) (queryNode, error) {
	h := 1 + exprHeight(l)
	if hr := exprHeight(r) + 1; hr > h {
		h = hr
	}
	if h > maxExprDepth {
		return nil, &ExprDepthError{}
	}
	return notNode{l: l, r: r}, nil
}

// matchSyntaxError renders C's query syntax error (fts5parse.y %syntax_error).
func matchSyntaxError(near string) error {
	return &QuerySyntaxError{Near: near}
}

// QuerySyntaxError is the fts5 MATCH parse error ("fts5: syntax error near
// \"...\""); exported because the engine treats fts5 query errors as
// statement errors rather than no-match results.
type QuerySyntaxError struct{ Near string }

func (e *QuerySyntaxError) Error() string { return "fts5: syntax error near \"" + e.Near + "\"" }

// parseQuery parses a MATCH query against a table (sqlite3Fts5ExprNew).
func parseQuery(t *Table, query string) (queryNode, error) {
	node, _, err := parseQueryAll(t, query)
	return node, err
}

// parseQueryAll is parseQuery, additionally returning the query's phrase list
// in parse order (fts5Parse.apPhrase — including zero-token phrases, which
// the aux API counts).
func parseQueryAll(t *Table, query string) (queryNode, []*phraseNode, error) {
	p := &qParser{t: t}
	p.lex.init(query)
	p.next()
	node, err := p.parseOr()
	if err != nil {
		return nil, nil, err
	}
	if p.kind != tkEOF {
		return nil, nil, matchSyntaxError(p.tokenText())
	}
	if p.deferredColQueries != nil {
		return nil, nil, p.deferredColQueries
	}
	return node, p.phrases, nil
}

// --- lexer (fts5ExprGetToken) ---

// token kinds.
const (
	tkEOF = iota
	tkString
	tkLP
	tkRP
	tkColon
	tkComma
	tkPlus
	tkStar
	tkMinus
	tkCaret
	tkLBrace
	tkRBrace
	tkOr
	tkAnd
	tkNot
	tkErr
)

// qLexer is the fts5_expr.c lexer state.
type qLexer struct {
	src  string
	tok  string // current token text (raw, quotes included for strings)
	kind int
	// unterminated records an "unterminated string" lex failure.
	unterminated bool
}

// lexMark captures the lexer state for lookahead backtracking.
type lexMark struct {
	src  string
	tok  string
	kind int
}

func (l *qLexer) init(s string) { l.src = s; l.kind = tkErr; l.tok = "" }

// mark captures the current lexer state.
func (l *qLexer) mark() lexMark { return lexMark{src: l.src, tok: l.tok, kind: l.kind} }

func (l *qLexer) restore(m lexMark) {
	l.src, l.tok, l.kind = m.src, m.tok, m.kind
}

func (l *qLexer) advance() {
	if l.kind == tkEOF {
		return
	}
	l.src = strings.TrimLeft(l.src, " \t\n\r")
	if l.src == "" {
		l.kind = tkEOF
		l.tok = ""
		return
	}
	switch l.src[0] {
	case '(':
		l.punct(1, tkLP)
	case ')':
		l.punct(1, tkRP)
	case '{':
		l.punct(1, tkLBrace)
	case '}':
		l.punct(1, tkRBrace)
	case ':':
		l.punct(1, tkColon)
	case ',':
		l.punct(1, tkComma)
	case '+':
		l.punct(1, tkPlus)
	case '*':
		l.punct(1, tkStar)
	case '-':
		l.punct(1, tkMinus)
	case '^':
		l.punct(1, tkCaret)
	case '"':
		l.lexString()
	default:
		if !isFts5Bareword(l.src[0]) {
			// An invalid first byte: the offender is that single byte
			// (fts5_expr.c "fts5: syntax error near \"%.1s\"").
			l.tok = l.src[:1]
			l.kind = tkErr
			return
		}
		i := 0
		for i < len(l.src) && isFts5Bareword(l.src[i]) {
			i++
		}
		l.tok = l.src[:i]
		l.src = l.src[i:]
		switch l.tok {
		case "OR":
			l.kind = tkOr
		case "AND":
			l.kind = tkAnd
		case "NOT":
			l.kind = tkNot
		default:
			l.kind = tkString
		}
	}
}

// punct consumes n bytes as one punctuation token.
func (l *qLexer) punct(n int, kind int) {
	l.tok = l.src[:n]
	l.src = l.src[n:]
	l.kind = kind
}

// lexString scans a double-quoted string with "" escapes (fts5ExprGetToken's
// quote branch). An unterminated string is a distinct parse error.
func (l *qLexer) lexString() {
	for i := 1; i < len(l.src); i++ {
		if l.src[i] == '"' {
			if i+1 < len(l.src) && l.src[i+1] == '"' {
				i++
				continue
			}
			l.tok = l.src[:i+1]
			l.src = l.src[i+1:]
			l.kind = tkString
			return
		}
	}
	l.tok = l.src
	l.kind = tkErr
	l.unterminated = true
}

// isFts5Bareword mirrors sqlite3Fts5IsBareword: digits, A-Z, a-z, '_', 0x1B
// and every byte >= 0x80.
func isFts5Bareword(b byte) bool {
	if b >= 0x80 {
		return true
	}
	switch {
	case b >= '0' && b <= '9', b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z':
		return true
	case b == '_', b == 0x1B:
		return true
	}
	return false
}

// dequoteFts5 removes surrounding double quotes and unescapes "" pairs
// (sqlite3Fts5Dequote for the query lexer's only quote character).
func dequoteFts5(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// --- parser ---

// qParser parses one MATCH query.
type qParser struct {
	lex qLexer
	kind int
	tok  string
	t    *Table
	// unterminated records an "unterminated string" lex failure.
	unterminated bool
	// phrases lists every phrase in parse order (fts5Parse.apPhrase).
	phrases []*phraseNode
	// deferredColQueries records fts5ParseSetColset's detail=none rejection
	// for raising after a well-formed parse (see parseColsetPhrase).
	deferredColQueries error
}

func (p *qParser) next() {
	p.lex.advance()
	p.kind = p.lex.kind
	p.tok = p.lex.tok
	p.unterminated = p.lex.unterminated
}

// tokenText renders the current token for error messages: the raw token text,
// or "" at end of input (lemon reports the empty token at EOF).
func (p *qParser) tokenText() string {
	if p.kind == tkEOF {
		return ""
	}
	return p.tok
}

// syntaxError records the current-token syntax error.
func (p *qParser) syntaxError() error { return matchSyntaxError(p.tokenText()) }

// lexError returns the failure for a tkErr token: an unterminated string or
// the invalid-byte syntax error.
func (p *qParser) lexError() error {
	if p.unterminated {
		return errUnterminatedString()
	}
	return matchSyntaxError(p.tok)
}

// errUnterminatedString builds the lexer's unterminated-string error
// (fts5_expr.c: no "fts5:" prefix).
func errUnterminatedString() error { return &UnterminatedStringError{} }

// UnterminatedStringError is the MATCH lexer's unterminated "..." error.
type UnterminatedStringError struct{}

func (e *UnterminatedStringError) Error() string { return "unterminated string" }

// parseOr parses OR-level expressions (lowest precedence, left associative).
func (p *qParser) parseOr() (queryNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.kind == tkOr {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left, err = p.combineOR(left, right)
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

// parseAnd parses AND-level expressions (left associative).
func (p *qParser) parseAnd() (queryNode, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.kind == tkAnd {
		p.next()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left, err = p.combineAND(left, right)
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

// parseNot parses NOT-level expressions. The right operand is a full operand:
// an implicit-AND chain absorbs following cnearsets ("a NOT b c" is
// "a NOT (b AND c)", proven against SQLite).
func (p *qParser) parseNot() (queryNode, error) {
	left, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	for p.kind == tkNot {
		p.next()
		right, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		left, err = p.combineNOT(left, right)
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

// startsCnearset reports whether the current token can begin a cnearset (an
// implicit-AND chain member): a bareword/quoted phrase, a NEAR call, a colset
// or a caret phrase. LP cannot (a parenthesized expression is an expr, not a
// cnearset).
func (p *qParser) startsCnearset() bool {
	switch p.kind {
	case tkString, tkLBrace, tkMinus, tkCaret:
		return true
	}
	return false
}

// parseOperand parses one operand at the AND/NOT/OR levels: a parenthesized
// expression, or a cnearset chain (implicit AND).
func (p *qParser) parseOperand() (queryNode, error) {
	if p.kind == tkErr {
		return nil, p.lexError()
	}
	if p.kind == tkLP {
		p.next()
		node, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.kind != tkRP {
			return nil, p.syntaxError()
		}
		p.next()
		return node, nil
	}
	node, err := p.parseCnearset(true)
	if err != nil {
		return nil, err
	}
	for p.startsCnearset() {
		next, err := p.parseCnearset(true)
		if err != nil {
			return nil, err
		}
		node, err = p.combineAND(node, next)
		if err != nil {
			return nil, err
		}
	}
	return node, nil
}

// parseCnearset parses one cnearset: a nearset, or "colset : nearset". A
// bareword followed by ":" is a colset (its column resolves immediately);
// a bareword followed by "(" is a NEAR-style call.
func (p *qParser) parseCnearset(allowExpr bool) (queryNode, error) {
	if p.kind == tkLBrace || p.kind == tkMinus {
		return p.parseColsetPhrase(allowExpr)
	}
	if p.kind == tkString {
		m := p.lex.mark()
		p.next()
		isColon := p.kind == tkColon
		isLP := p.kind == tkLP
		p.lex.restore(m)
		p.kind, p.tok = p.lex.kind, p.lex.tok
		if isColon {
			return p.parseColsetPhrase(allowExpr)
		}
		if isLP {
			return p.parseNearset()
		}
	}
	return p.parseNearset()
}

// parseColsetPhrase parses "colset : expr" or "colset : nearset"
// (fts5parse.y: colset COLON LP expr RP | colset COLON nearset).
func (p *qParser) parseColsetPhrase(allowExpr bool) (queryNode, error) {
	cols, err := p.parseColset()
	if err != nil {
		return nil, err
	}
	if p.kind != tkColon {
		return nil, p.syntaxError()
	}
	if p.t.cfg.Detail == DetailNone {
		// The detail=none rejection is recorded and raised only when the
		// rest of the parse is well-formed: C's parser raises it at the
		// colset reduce, after a trailing-garbage syntax error has already
		// been reported (fts5detail 4.1 vs 4.2).
		if p.deferredColQueries == nil {
			p.deferredColQueries = &ColumnQueriesError{}
		}
	}
	p.next()
	if p.kind == tkLP {
		if !allowExpr {
			return nil, p.syntaxError()
		}
		p.next()
		child, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.kind != tkRP {
			return nil, p.syntaxError()
		}
		p.next()
		return applyColset(child, cols), nil
	}
	node, err := p.parseNearset()
	if err != nil {
		return nil, err
	}
	return applyColset(node, cols), nil
}

// parseColset parses a column set (fts5parse.y colset/colsetlist): a
// brace-enclosed space-separated list, either optionally inverted with a
// leading '-', or a single (possibly inverted) bareword/quoted name. Names
// resolve against the table columns immediately (C's parse-time
// "no such column" failure).
func (p *qParser) parseColset() ([]int, error) {
	invert := false
	if p.kind == tkMinus {
		invert = true
		p.next()
	}
	var cols []int
	if p.kind == tkLBrace {
		p.next()
		for p.kind != tkRBrace {
			if p.kind != tkString {
				return nil, p.syntaxError()
			}
			idx, err := p.resolveColumn(dequoteFts5(p.tok))
			if err != nil {
				return nil, err
			}
			cols = append(cols, idx)
			p.next()
		}
		if len(cols) == 0 {
			// "{}" is a syntax error (the empty colsetlist cannot reduce).
			return nil, matchSyntaxError("}")
		}
		p.next()
	} else if p.kind == tkString {
		idx, err := p.resolveColumn(dequoteFts5(p.tok))
		if err != nil {
			return nil, err
		}
		cols = []int{idx}
		p.next()
	} else {
		return nil, p.syntaxError()
	}
	if invert {
		return p.invertColset(cols), nil
	}
	return cols, nil
}

// resolveColumn resolves one column name (fts5ParseColset): case-insensitive
// against the declared columns; unknown names fail without the "fts5:"
// prefix.
func (p *qParser) resolveColumn(name string) (int, error) {
	if idx := p.t.ColumnIndex(name); idx >= 0 {
		return idx, nil
	}
	return -1, &NoSuchColumnError{Column: name}
}

// NoSuchColumnError is the colset/name resolution error ("no such column:
// x"); the engine surfaces it as a statement error.
type NoSuchColumnError struct{ Column string }

func (e *NoSuchColumnError) Error() string { return "no such column: " + e.Column }

// ColumnQueriesError is fts5ParseSetColset's detail=none rejection.
type ColumnQueriesError struct{}

func (e *ColumnQueriesError) Error() string {
	return "fts5: column queries are not supported (detail=none)"
}

// invertColset returns the ascending complement of cols over every column.
func (p *qParser) invertColset(cols []int) []int {
	excl := make(map[int]bool, len(cols))
	for _, c := range cols {
		excl[c] = true
	}
	var out []int
	for i := range p.t.cfg.Columns {
		if !excl[i] {
			out = append(out, i)
		}
	}
	return out
}

// parseNearset parses a nearset: a phrase, a caret phrase or a NEAR call
// (fts5parse.y nearset).
func (p *qParser) parseNearset() (queryNode, error) {
	if p.kind == tkCaret {
		p.next()
		ph, err := p.parsePhrase()
		if err != nil {
			return nil, err
		}
		ph.first = true
		return p.newStringNode([]*phraseNode{ph}, FTS5DefaultNearDist)
	}
	if p.kind == tkString {
		// A word followed by "(" is a NEAR-style call — even for non-NEAR
		// words, which fail at the closing reduce like C.
		m := p.lex.mark()
		p.next()
		isCall := p.kind == tkLP
		p.lex.restore(m)
		p.kind, p.tok = p.lex.kind, p.lex.tok
		if isCall {
			return p.parseNearCall()
		}
	}
	ph, err := p.parsePhrase()
	if err != nil {
		return nil, err
	}
	return p.newStringNode([]*phraseNode{ph}, FTS5DefaultNearDist)
}

// parseNearCall parses NEAR(phrase phrase ...[, N]) (fts5parse.y: STRING LP
// nearphrases neardist_opt RP). The function word must be exactly "NEAR"
// (case-sensitive); the optional distance is a raw (unquoted) digit string.
func (p *qParser) parseNearCall() (queryNode, error) {
	word := p.tok
	p.next() // the word
	p.next() // the '('
	var phrases []*phraseNode
	window := FTS5DefaultNearDist
	distRaw := ""
	hasDist := false
	for {
		ph, err := p.parsePhrase()
		if err != nil {
			return nil, err
		}
		phrases = append(phrases, ph)
		if p.kind == tkComma {
			p.next()
			if p.kind != tkString {
				return nil, p.syntaxError()
			}
			distRaw = p.tok
			hasDist = true
			p.next()
			break
		}
		if p.kind != tkString {
			break
		}
	}
	if p.kind != tkRP {
		return nil, p.syntaxError()
	}
	p.next()
	if word != "NEAR" {
		return nil, matchSyntaxError(word)
	}
	if hasDist {
		n, ok := parseNearDistance(distRaw)
		if !ok {
			return nil, &NearDistanceError{Got: distRaw}
		}
		window = n
	}
	return p.newStringNode(phrases, window)
}

// parseNearDistance parses fts5ParseSetDistance's digit string: every byte
// must be a digit; the value saturates at 214748363 (C's overflow guard).
func parseNearDistance(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		if n < 214748363 {
			n = n*10 + int(s[i]-'0')
		}
	}
	return n, true
}

// NearDistanceError is fts5ParseSetDistance's "expected integer" error.
type NearDistanceError struct{ Got string }

func (e *NearDistanceError) Error() string { return "expected integer, got \"" + e.Got + "\"" }

// parsePhrase parses one phrase: STRING tokens joined by '+' with per-term
// trailing stars (fts5parse.y phrase). Each word/string is tokenized with the
// table's tokenizer; a string yielding no tokens contributes nothing (an
// entirely empty phrase is a zero-term phrase).
func (p *qParser) parsePhrase() (*phraseNode, error) {
	ph := &phraseNode{idx: len(p.phrases)}
	for {
		if p.kind == tkErr {
			return nil, p.lexError()
		}
		if p.kind != tkString {
			return nil, p.syntaxError()
		}
		text := dequoteFts5(p.tok)
		p.next()
		prefix := false
		if p.kind == tkStar {
			prefix = true
			p.next()
		}
		for _, tok := range p.t.tok.Tokenize(text) {
			ph.terms = append(ph.terms, qTerm{term: tok.Term})
		}
		if prefix && len(ph.terms) > 0 {
			ph.terms[len(ph.terms)-1].prefix = true
		}
		if p.kind != tkPlus {
			break
		}
		p.next()
	}
	p.phrases = append(p.phrases, ph)
	return ph, nil
}

// newStringNode builds the STRING node for a near cluster, applying the
// zero-phrase EOF rule and the detail-mode restrictions
// (sqlite3Fts5ParseNode).
func (p *qParser) newStringNode(phrases []*phraseNode, window int) (queryNode, error) {
	for _, ph := range phrases {
		if len(ph.terms) == 0 {
			return eofNode{}, nil
		}
	}
	if p.t.cfg.Detail != DetailFull {
		if len(phrases) != 1 || len(phrases[0].terms) > 1 || phrases[0].first {
			kind := "NEAR"
			if len(phrases) == 1 {
				kind = "phrase"
			}
			return nil, &DetailUnsupportedError{Kind: kind}
		}
	}
	return stringNode{phrases: phrases, window: window}, nil
}

// DetailUnsupportedError is fts5ParseNode's detail!=full rejection.
type DetailUnsupportedError struct{ Kind string }

func (e *DetailUnsupportedError) Error() string {
	return "fts5: " + e.Kind + " queries are not supported (detail!=full)"
}

// applyColset applies a column filter to a parsed subtree (fts5ParseSetColset):
// STRING nodes merge the filter into their phrases' colsets; an empty merge
// becomes an EOF node. AND/OR/NOT subtrees recurse into every child.
func applyColset(node queryNode, cols []int) queryNode {
	switch n := node.(type) {
	case eofNode:
		return node
	case stringNode:
		// Every phrase of the node carries the same effective colset (they
		// are set together), so one intersection decides the merge.
		merged := intersectColsets(n.phrases[0].colset, cols)
		if len(merged) == 0 {
			return eofNode{}
		}
		for _, ph := range n.phrases {
			ph.colset = merged
		}
		return node
	case andNode:
		for i, c := range n.children {
			n.children[i] = applyColset(c, cols)
		}
		return node
	case orNode:
		for i, c := range n.children {
			n.children[i] = applyColset(c, cols)
		}
		return node
	case notNode:
		n.l = applyColset(n.l, cols)
		n.r = applyColset(n.r, cols)
		return node
	}
	return node
}

// intersectColsets intersects two ascending column sets (fts5MergeColset).
// A nil set means every column.
func intersectColsets(a, b []int) []int {
	if a == nil {
		return append([]int(nil), b...)
	}
	if b == nil {
		return append([]int(nil), a...)
	}
	var out []int
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

// --- evaluation (set based) ---

func (n stringNode) eval(t *Table) (map[int64]bool, error) {
	if len(n.phrases) == 1 {
		return t.evalPhrase(n.phrases[0])
	}
	return t.evalNear(n.phrases, n.window)
}

func (n andNode) eval(t *Table) (map[int64]bool, error) {
	sets := make([]map[int64]bool, len(n.children))
	for i, c := range n.children {
		s, err := c.eval(t)
		if err != nil {
			return nil, err
		}
		sets[i] = s
	}
	return intersectSets(sets), nil
}

func (n orNode) eval(t *Table) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, c := range n.children {
		s, err := c.eval(t)
		if err != nil {
			return nil, err
		}
		for rowid := range s {
			out[rowid] = true
		}
	}
	return out, nil
}

func (n notNode) eval(t *Table) (map[int64]bool, error) {
	l, err := n.l.eval(t)
	if err != nil {
		return nil, err
	}
	r, err := n.r.eval(t)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(l))
	for rowid := range l {
		if !r[rowid] {
			out[rowid] = true
		}
	}
	return out, nil
}

// intersectSets intersects rowid sets.
func intersectSets(sets []map[int64]bool) map[int64]bool {
	smallest := 0
	for i, s := range sets {
		if len(s) < len(sets[smallest]) {
			smallest = i
		}
	}
	out := make(map[int64]bool, len(sets[smallest]))
	for rowid := range sets[smallest] {
		in := true
		for i, s := range sets {
			if i != smallest && !s[rowid] {
				in = false
				break
			}
		}
		if in {
			out[rowid] = true
		}
	}
	return out
}

// phraseCols returns the phrase's active columns.
func phraseCols(t *Table, ph *phraseNode) []int {
	return activeColIndexes(t, ph.colset)
}

// evalPhrase evaluates one phrase to its matching rowids.
func (t *Table) evalPhrase(ph *phraseNode) (map[int64]bool, error) {
	cols := phraseCols(t, ph)
	if len(ph.terms) == 1 && !ph.terms[0].prefix && !ph.first {
		return t.termRowids(ph.terms[0].term, cols), nil
	}
	return t.matchPhrase(ph.terms, cols, ph.first), nil
}

// evalNear evaluates a NEAR cluster: every phrase must have an instance
// within the window in one column (fts5ExprNearIsMatch).
func (t *Table) evalNear(phrases []*phraseNode, window int) (map[int64]bool, error) {
	cols := phraseCols(t, phrases[0])
	out := make(map[int64]bool)
	var cand []int64
	for i, ph := range phrases {
		m, err := t.evalPhrase(ph)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			cand = t.SortedMatchRowids(m)
			continue
		}
		var next []int64
		for _, rowid := range cand {
			if m[rowid] {
				next = append(next, rowid)
			}
		}
		cand = next
	}
	for _, rowid := range cand {
		if t.nearMatchesDoc(phrases, rowid, cols, window) {
			out[rowid] = true
		}
	}
	return out, nil
}

// termRowids returns the docs containing one term in the active columns.
func (t *Table) termRowids(term string, cols []int) map[int64]bool {
	out := make(map[int64]bool)
	for _, col := range activeColIndexes(t, cols) {
		for rowid := range t.ix.termRowids(term, col) {
			out[rowid] = true
		}
	}
	return out
}

// matchPhrase finds docs where the token chain is adjacent (positions step by
// one) within one active column.
func (t *Table) matchPhrase(terms []qTerm, cols []int, first bool) map[int64]bool {
	out := make(map[int64]bool)
	candidates := t.phraseCandidateRowids(terms, cols)
	for _, rowid := range candidates {
		doc := t.ix.Doc(rowid)
		if doc == nil {
			continue
		}
		for _, col := range activeColIndexes(t, cols) {
			if col >= len(doc.cols) {
				continue
			}
			tokens := doc.cols[col]
			for i := 0; i+len(terms) <= len(tokens); i++ {
				if first && i != 0 {
					break
				}
				if phraseMatches(terms, tokens, i) {
					out[rowid] = true
					break
				}
			}
			if out[rowid] {
				break
			}
		}
	}
	return out
}

// phraseCandidateRowids narrows to docs containing the first term (or all
// prefix expansions of it).
func (t *Table) phraseCandidateRowids(terms []qTerm, cols []int) []int64 {
	set := make(map[int64]bool)
	for _, col := range activeColIndexes(t, cols) {
		if terms[0].prefix {
			for _, term := range t.ix.prefixTerms(terms[0].term) {
				for rowid := range t.ix.termRowids(term, col) {
					set[rowid] = true
				}
			}
			continue
		}
		for rowid := range t.ix.termRowids(terms[0].term, col) {
			set[rowid] = true
		}
	}
	rowids := make([]int64, 0, len(set))
	for rowid := range set {
		rowids = append(rowids, rowid)
	}
	sortRowids(rowids)
	return rowids
}

// phraseMatches checks one adjacency window.
func phraseMatches(terms []qTerm, tokens []string, start int) bool {
	for k, tm := range terms {
		tok := tokens[start+k]
		if tm.prefix {
			if !strings.HasPrefix(tok, tm.term) {
				return false
			}
			continue
		}
		if tok != tm.term {
			return false
		}
	}
	return true
}

// nearMatchesDoc checks one document: every phrase needs an instance and the
// instance set must fit C's NEAR window (fts5ExprNearIsMatch): anchoring on
// the running maximum position iMax, each phrase's instance at p (its FIRST
// token) must satisfy p >= iMax - nTerm_i - N and p <= iMax. Windows are per
// column (positions are column-major absolute in C, so a window never spans
// columns).
func (t *Table) nearMatchesDoc(phrases []*phraseNode, rowid int64, cols []int, window int) bool {
	doc := t.ix.Doc(rowid)
	if doc == nil {
		return false
	}
	for _, col := range cols {
		if col >= len(doc.cols) {
			continue
		}
		tokens := doc.cols[col]
		instances := make([][]int, len(phrases))
		empty := false
		for i, ph := range phrases {
			var pos []int
			for p := 0; p+len(ph.terms) <= len(tokens); p++ {
				if ph.first && p != 0 {
					break
				}
				if phraseMatches(ph.terms, tokens, p) {
					pos = append(pos, p)
				}
			}
			if len(pos) == 0 {
				empty = true
				break
			}
			instances[i] = pos
		}
		if empty {
			continue
		}
		sizes := make([]int, len(phrases))
		for i, ph := range phrases {
			sizes[i] = len(ph.terms)
		}
		if nearWindowMatch(instances, sizes, window) {
			return true
		}
	}
	return false
}

// nearWindowMatch ports fts5ExprNearIsMatch's advancing-anchors loop for one
// column's phrase instances (all non-empty, ascending). sizes[i] is phrase i's
// token count.
func nearWindowMatch(instances [][]int, sizes []int, window int) bool {
	idx := make([]int, len(instances))
	iMax := instances[0][0]
	for {
		bMatch := true
		for i := range instances {
			iMin := iMax - sizes[i] - window
			for instances[i][idx[i]] < iMin {
				idx[i]++
				if idx[i] >= len(instances[i]) {
					return false // phrase exhausted: no window in this column
				}
			}
			if p := instances[i][idx[i]]; p > iMax {
				iMax = p
				bMatch = false
			}
		}
		if bMatch {
			return true
		}
	}
}

// activeColIndexes resolves the active column set: the given columns, or
// every user column when empty.
func activeColIndexes(t *Table, cols []int) []int {
	if len(cols) == 0 {
		all := make([]int, len(t.cfg.Columns))
		for i := range all {
			all[i] = i
		}
		return all
	}
	return cols
}
