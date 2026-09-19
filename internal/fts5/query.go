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
// sets against the inverted index (query_eval.go); parsing lives in
// query_parse.go.
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
// or a column filter merged down to nothing). It retains the phrases the
// parse registered for it — C's EOF node keeps pNear->apPhrase, and the
// implicit-AND merge removes exactly those phrases from pParse->apPhrase when
// it discards the node (fts5_expr.c sqlite3Fts5ParseImplicitAnd).
type eofNode struct {
	phrases []*phraseNode
}

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
		return 1 + maxChildHeight(x.children)
	case orNode:
		return 1 + maxChildHeight(x.children)
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

// maxChildHeight returns the tallest child's height (0 with no children).
func maxChildHeight(children []queryNode) int {
	h := 0
	for _, c := range children {
		if ch := exprHeight(c); ch > h {
			h = ch
		}
	}
	return h
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
	h := maxChildHeight(kids)
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

// punctKinds maps single-byte punctuation to its token kind (fts5_expr.c's
// punctuation switch in fts5ExprGetToken).
var punctKinds = map[byte]int{
	'(': tkLP,
	')': tkRP,
	'{': tkLBrace,
	'}': tkRBrace,
	':': tkColon,
	',': tkComma,
	'+': tkPlus,
	'*': tkStar,
	'-': tkMinus,
	'^': tkCaret,
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
	if kind, ok := punctKinds[l.src[0]]; ok {
		l.punct(1, kind)
		return
	}
	if l.src[0] == '"' {
		l.lexString()
		return
	}
	l.lexBareword()
}

// lexBareword scans a bareword or keyword token; an invalid first byte makes
// that single byte the token (fts5_expr.c "fts5: syntax error near \"%.1s\"").
func (l *qLexer) lexBareword() {
	if !isFts5Bareword(l.src[0]) {
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

// isFts5Bareword mirrors sqlite3Fts5IsBareword: digits, A-Z, a-z, '_', 0x1A
// (the unicode substitute character) and every byte >= 0x80.
func isFts5Bareword(b byte) bool {
	if b >= 0x80 {
		return true
	}
	switch {
	case b >= '0' && b <= '9', b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z':
		return true
	case b == '_', b == 0x1A:
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
