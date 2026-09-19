package fts5

// This file holds the MATCH query parser (query.go holds the node types and
// the lexer): the recursive-descent precedence chain, column sets, NEAR
// calls, phrases and the parse-time colset application (fts5_expr.c +
// fts5parse.y).

// qParser parses one MATCH query.
type qParser struct {
	lex  qLexer
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
	// depth counts open parenthesized expressions (fts5parse.y's lemon shift
	// stack: YYSTACKDEPTH=100 symbols, ~3 shifted per "( expr AND" nesting
	// level, so the parser overflows at 34 nested levels — oracle-verified).
	depth int
}

// maxParseNesting is the deepest parenthesized-expression nesting the fts5
// parser accepts before "fts5: parser stack overflow" (fts5parse.y
// %stack_overflow via the lemon yy_parse stack; oracle-verified: 32 nested
// levels parse, 33 raise the error).
const maxParseNesting = 32

// ParseStackOverflowError is fts5parse.y's stack-overflow failure
// (sqlite3Fts5ParseError "fts5: parser stack overflow").
type ParseStackOverflowError struct{}

func (e *ParseStackOverflowError) Error() string { return "fts5: parser stack overflow" }

// enterLP accounts one nesting level when an open paren begins a
// sub-expression; the caller decrements p.depth after the RP.
func (p *qParser) enterLP() error {
	p.depth++
	if p.depth > maxParseNesting {
		return &ParseStackOverflowError{}
	}
	return nil
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
		return p.parseParenExpr()
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
		node, err = p.parseImplicitAnd(node, next)
		if err != nil {
			return nil, err
		}
	}
	return node, nil
}

// parseParenExpr parses "( expr )" with the nesting-depth accounting
// (fts5parse.y LP expr RP).
func (p *qParser) parseParenExpr() (queryNode, error) {
	p.next()
	if err := p.enterLP(); err != nil {
		return nil, err
	}
	node, err := p.parseOr()
	p.depth--
	if err != nil {
		return nil, err
	}
	if p.kind != tkRP {
		return nil, p.syntaxError()
	}
	p.next()
	return node, nil
}

// parseImplicitAnd merges one cnearset into an implicit-AND chain
// (fts5_expr.c sqlite3Fts5ParseImplicitAnd): a zero-token (EOF) right operand
// is dropped; an EOF left operand (or last AND child) is replaced by the
// right operand. In both cases the EOF's phrases leave apPhrase; anything
// else becomes a regular AND node (with its depth guard).
func (p *qParser) parseImplicitAnd(left, right queryNode) (queryNode, error) {
	if re, ok := right.(eofNode); ok {
		p.dropEofPhrases(re)
		return left, nil
	}
	if lf, ok := left.(eofNode); ok {
		p.dropEofPhrases(lf)
		return right, nil
	}
	if and, ok := left.(andNode); ok {
		if lf, ok := and.children[len(and.children)-1].(eofNode); ok {
			p.dropEofPhrases(lf)
			and.children[len(and.children)-1] = right
			return left, nil
		}
	}
	return combineAndNodes([]queryNode{left, right})
}

// dropEofPhrases removes an EOF node's phrases from the parser's phrase list
// (the C parse's nPhrase--/memmove on the discarded FTS5_EOF node) and
// renumbers the surviving phrases' indices to their list positions.
func (p *qParser) dropEofPhrases(e eofNode) {
	if len(e.phrases) == 0 {
		return
	}
	drop := make(map[*phraseNode]bool, len(e.phrases))
	for _, ph := range e.phrases {
		drop[ph] = true
	}
	kept := p.phrases[:0:0]
	for _, ph := range p.phrases {
		if !drop[ph] {
			kept = append(kept, ph)
		}
	}
	p.phrases = kept
	for i, ph := range p.phrases {
		ph.idx = i
	}
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
		return p.parseColsetExpr(cols, allowExpr)
	}
	node, err := p.parseNearset()
	if err != nil {
		return nil, err
	}
	return applyColset(node, cols), nil
}

// parseColsetExpr parses the "( expr )" form of "colset : expr"
// (fts5parse.y colset COLON LP expr RP).
func (p *qParser) parseColsetExpr(cols []int, allowExpr bool) (queryNode, error) {
	if !allowExpr {
		return nil, p.syntaxError()
	}
	p.next()
	if err := p.enterLP(); err != nil {
		return nil, err
	}
	child, err := p.parseOr()
	p.depth--
	if err != nil {
		return nil, err
	}
	if p.kind != tkRP {
		return nil, p.syntaxError()
	}
	p.next()
	return applyColset(child, cols), nil
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
	cols, err := p.parseColsetList()
	if err != nil {
		return nil, err
	}
	if invert {
		return p.invertColset(cols), nil
	}
	return cols, nil
}

// parseColsetList parses either form of the colset body: a brace-enclosed
// name list, or a single bareword/quoted name.
func (p *qParser) parseColsetList() ([]int, error) {
	if p.kind == tkLBrace {
		return p.parseBracedColsetList()
	}
	if p.kind != tkString {
		return nil, p.syntaxError()
	}
	idx, err := p.resolveColumn(dequoteFts5(p.tok))
	if err != nil {
		return nil, err
	}
	p.next()
	return []int{idx}, nil
}

// parseBracedColsetList parses '{' name ... '}' (fts5parse.y colsetlist).
// "{}" is a syntax error (the empty colsetlist cannot reduce).
func (p *qParser) parseBracedColsetList() ([]int, error) {
	p.next()
	var cols []int
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
		return nil, matchSyntaxError("}")
	}
	p.next()
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
	if p.kind == tkString && p.nextIsLP() {
		// A word followed by "(" is a NEAR-style call — even for non-NEAR
		// words, which fail at the closing reduce like C.
		return p.parseNearCall()
	}
	ph, err := p.parsePhrase()
	if err != nil {
		return nil, err
	}
	return p.newStringNode([]*phraseNode{ph}, FTS5DefaultNearDist)
}

// nextIsLP reports (with lookahead) whether the token after the current
// STRING is an open paren, leaving the lexer position unchanged.
func (p *qParser) nextIsLP() bool {
	m := p.lex.mark()
	p.next()
	isLP := p.kind == tkLP
	p.lex.restore(m)
	p.kind, p.tok = p.lex.kind, p.lex.tok
	return isLP
}

// parseNearCall parses NEAR(phrase phrase ...[, N]) (fts5parse.y: STRING LP
// nearphrases neardist_opt RP). The function word must be exactly "NEAR"
// (case-sensitive); the optional distance is a raw (unquoted) digit string.
func (p *qParser) parseNearCall() (queryNode, error) {
	word := p.tok
	p.next() // the word
	p.next() // the '('
	phrases, distRaw, hasDist, err := p.parseNearPhrases()
	if err != nil {
		return nil, err
	}
	if p.kind != tkRP {
		return nil, p.syntaxError()
	}
	p.next()
	if word != "NEAR" {
		return nil, matchSyntaxError(word)
	}
	window := FTS5DefaultNearDist
	if hasDist {
		n, ok := parseNearDistance(distRaw)
		if !ok {
			return nil, &NearDistanceError{Got: distRaw}
		}
		window = n
	}
	return p.newStringNode(phrases, window)
}

// parseNearPhrases consumes the NEAR call body up to (not including) the
// closing paren: one or more phrases, with the optional ", N" distance.
func (p *qParser) parseNearPhrases() ([]*phraseNode, string, bool, error) {
	var phrases []*phraseNode
	distRaw := ""
	hasDist := false
	for {
		ph, err := p.parsePhrase()
		if err != nil {
			return nil, "", false, err
		}
		phrases = appendNearPhrase(phrases, ph)
		if p.kind == tkComma {
			p.next()
			if p.kind != tkString {
				return nil, "", false, p.syntaxError()
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
	return phrases, distRaw, hasDist, nil
}

// appendNearPhrase applies sqlite3Fts5ParseNearset's incremental empty-phrase
// rule: a zero-token phrase added to a non-empty nearset is dropped, and a
// non-empty phrase replaces a trailing empty one (NEAR("" c) ≡ NEAR(c); a
// lone empty phrase keeps the EOF node).
func appendNearPhrase(phrases []*phraseNode, ph *phraseNode) []*phraseNode {
	if n := len(phrases); n > 0 {
		if len(ph.terms) == 0 {
			return phrases
		}
		if len(phrases[n-1].terms) == 0 {
			phrases[n-1] = ph
			return phrases
		}
	}
	return append(phrases, ph)
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
			// C's FTS5_EOF node keeps the nearset (apPhrase) attached.
			return eofNode{phrases: phrases}, nil
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
			// C's fts5ParseSetColset flips the node to FTS5_EOF, keeping
			// pNear (and apPhrase) attached.
			return eofNode{phrases: n.phrases}
		}
		for _, ph := range n.phrases {
			ph.colset = merged
		}
		return node
	case andNode:
		applyColsetToChildren(n.children, cols)
		return node
	case orNode:
		applyColsetToChildren(n.children, cols)
		return node
	case notNode:
		n.l = applyColset(n.l, cols)
		n.r = applyColset(n.r, cols)
		return node
	}
	return node
}

// applyColsetToChildren recurses the colset application into every child.
func applyColsetToChildren(children []queryNode, cols []int) {
	for i, c := range children {
		children[i] = applyColset(c, cols)
	}
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
