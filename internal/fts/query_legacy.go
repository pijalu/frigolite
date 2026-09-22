package fts

import "fmt"

// This file ports the LEGACY fts3 MATCH query parser — fts3_expr.c
// fts3ExprParse with sqlite3_fts3_enable_parentheses==0 (the release-build
// default). It is a round-based loop: keyword / quoted-phrase / bare-token
// rounds wired together with implicit-AND insertion, the
// opPrecedence/insertBinaryOperator precedence climb, and the '-' implicit-NOT
// branch chain (ParseContext.pNotBranch).

// FTSQUERY_* mirror the fts3Int.h operator codes (FTSQUERY_NEAR=1 ..
// FTSQUERY_PHRASE=5). In parenthesis mode opPrecedence returns the code
// itself, so the numeric order (NEAR < NOT < AND < OR) IS the precedence.
const (
	ftsqueryNear   = 1
	ftsqueryNot    = 2
	ftsqueryAnd    = 3
	ftsqueryOr     = 4
	ftsqueryPhrase = 5
)

// exprNode is the parse-tree node shim used while building the expression —
// the Go analogue of fts3_expr.c's Fts3Expr with its pParent/pLeft/pRight
// links, which insertBinaryOperator climbs and rewrites. convertExpr lowers
// the finished tree to the parent-less QueryNode AST.
type exprNode struct {
	eType  int // one of the ftsquery* codes
	nNear  int // NEAR distance ("NEAR/n"; default 10)
	phrase QueryNode

	pLeft, pRight, pParent *exprNode
}

// opPrecedence ports fts3_expr.c opPrecedence: lower values bind tighter.
// Legacy mode (parentheses disabled): NEAR=1, OR=2, AND=3 — "the OR operator
// has a higher precedence than the AND operator". Parenthesis mode: the
// FTSQUERY_* code itself (NEAR < NOT < AND < OR).
func opPrecedence(p *exprNode) int {
	if fts3ParenthesesEnabled() {
		return p.eType
	}
	switch p.eType {
	case ftsqueryNear:
		return 1
	case ftsqueryOr:
		return 2
	}
	return 3
}

// insertBinaryOperator ports fts3_expr.c insertBinaryOperator: climb from the
// most recently inserted node through parents of <= precedence and splice the
// new operator node in above them.
func insertBinaryOperator(ppHead **exprNode, pPrev, pNew *exprNode) {
	pSplit := pPrev
	for pSplit.pParent != nil && opPrecedence(pSplit.pParent) <= opPrecedence(pNew) {
		pSplit = pSplit.pParent
	}
	if pSplit.pParent != nil {
		pSplit.pParent.pRight = pNew
		pNew.pParent = pSplit.pParent
	} else {
		*ppHead = pNew
	}
	pNew.pLeft = pSplit
	pSplit.pParent = pNew
}

// convertExpr lowers the exprNode tree to the QueryNode AST. A C NOT node is
// binary — NOT(X, Y) evaluates as "X AND NOT Y" (fts3eval.c
// FTSQUERY_NOT handling) — and is expressed here as AndNode{X, NotNode{Y}},
// which is exactly how the legacy '-' NOT chains (pNotBranch) evaluate.
func convertExpr(p *exprNode) QueryNode {
	if p == nil {
		return nil
	}
	switch p.eType {
	case ftsqueryPhrase:
		return p.phrase
	case ftsqueryAnd:
		return &AndNode{Left: convertExpr(p.pLeft), Right: convertExpr(p.pRight)}
	case ftsqueryOr:
		return &OrNode{Left: convertExpr(p.pLeft), Right: convertExpr(p.pRight)}
	case ftsqueryNear:
		return &NearNode{Left: convertExpr(p.pLeft), Right: convertExpr(p.pRight), Distance: p.nNear}
	case ftsqueryNot:
		return &AndNode{Left: convertExpr(p.pLeft), Right: &NotNode{Inner: convertExpr(p.pRight)}}
	}
	return nil
}

// legacyParser carries the lexer state across fts3ExprParse rounds.
type legacyParser struct {
	input string
	isNot bool // ParseContext.isNot: the next phrase had a unary "-" attached
}

// legacyParseState carries fts3ExprParse's locals across rounds: the tree
// head (pRet), the most recently inserted node (pPrev), the '-' NOT branch
// chain (pNotBranch), and the isRequirePhrase flag.
type legacyParseState struct {
	p         *legacyParser
	pRet      *exprNode
	pPrev     *exprNode
	pNotBr    *exprNode
	require   bool
	err       error
	syntaxErr error
}

// parseLegacyMatchQuery is the port of fts3_expr.c fts3ExprParse (legacy
// syntax, sqlite3_fts3_enable_parentheses==0): a single loop of getNextNode
// rounds — keyword, quoted phrase, or bare token — wired together with
// implicit-AND insertion, precedence-climbing operator insertion and the
// '-' implicit-NOT branch chain.
func parseLegacyMatchQuery(query string) (QueryNode, error) {
	st := &legacyParseState{
		p:         &legacyParser{input: query},
		require:   true,
		syntaxErr: fmt.Errorf("malformed MATCH expression: [%s]", query),
	}
	pos := 0
	for st.err == nil {
		node, consumed, done, nerr := st.p.legacyNextNode(pos)
		if nerr != nil {
			st.err = nerr
			break
		}
		if done {
			break
		}
		pos += consumed
		if node == nil {
			continue
		}
		st.attach(node)
	}
	root, err := st.finish()
	if err != nil {
		return nil, err
	}
	if root == nil {
		// No tokens at all — the FTS cursor sits at EOF and matches nothing
		// (MATCH '' semantics).
		return &emptyQueryNode{}, nil
	}
	return convertExpr(root), nil
}

// attach folds one non-nil round node into the tree (the body of C's
// fts3ExprParse loop): a '-phrase' extends the NOT branch chain, everything
// else attaches as phrase or binary operator.
func (st *legacyParseState) attach(node *exprNode) {
	if node.eType == ftsqueryPhrase && st.p.isNot {
		st.extendNotBranch(node)
		node = st.pPrev
	} else {
		st.attachPhraseOrOperator(node)
	}
	st.pPrev = node
}

// extendNotBranch handles the legacy "-token" round: create an implicit NOT
// operator and chain it in front of the previous NOT branch (fts3ExprParse's
// pNotBranch handling). The main tree continues from pPrev — the negated
// phrase does not participate in it.
func (st *legacyParseState) extendNotBranch(node *exprNode) {
	pNot := &exprNode{eType: ftsqueryNot, pRight: node}
	node.pParent = pNot
	if st.pNotBr != nil {
		pNot.pLeft = st.pNotBr
		st.pNotBr.pParent = pNot
	}
	st.pNotBr = pNot
}

// keywordBoundary mirrors getNextToken's cNext test: a keyword must be
// followed by whitespace, a quote, a parenthesis or end of input, else the
// word is a term such as "ORacle".
func keywordBoundary(cNext byte) bool {
	return isFTS3Space(cNext) || cNext == '"' || cNext == '(' || cNext == ')' || cNext == 0
}

// nearOperandViolation reports fts3ExprParse's NEAR operand error: a NEAR
// whose right operand is not a plain phrase, or any bracketed/compound
// operand following a NEAR ("(x) NEAR y" / "x NEAR (y)").
func nearOperandViolation(pPrev *exprNode, eType int, isPhrase bool) bool {
	if pPrev == nil {
		return false
	}
	if eType == ftsqueryNear && !isPhrase && pPrev.eType != ftsqueryPhrase {
		return true
	}
	return eType != ftsqueryPhrase && isPhrase && pPrev.eType == ftsqueryNear
}

// attachPhraseOrOperator attaches a phrase (as an operator operand, with an
// implicit AND inserted for juxtaposition) or a binary operator (via the
// precedence climb).
func (st *legacyParseState) attachPhraseOrOperator(node *exprNode) {
	eType := node.eType
	isPhrase := eType == ftsqueryPhrase || node.pLeft != nil

	// A binary operator where a phrase (or bracketed expression) is
	// required is a syntax error.
	if !isPhrase && st.require {
		st.err = st.syntaxErr
		return
	}

	// Juxtaposed phrases: insert an implicit AND.
	if isPhrase && !st.require {
		pAnd := &exprNode{eType: ftsqueryAnd}
		insertBinaryOperator(&st.pRet, st.pPrev, pAnd)
		st.pPrev = pAnd
	}

	// NEAR operands must be phrases: "(x) NEAR y" / "x NEAR (y)" are errors
	// (fts3ExprParse's NEAR operand test).
	if nearOperandViolation(st.pPrev, eType, isPhrase) {
		st.err = st.syntaxErr
		return
	}

	if isPhrase {
		if st.pRet != nil {
			st.pPrev.pRight = node
			node.pParent = st.pPrev
		} else {
			st.pRet = node
		}
	} else {
		insertBinaryOperator(&st.pRet, st.pPrev, node)
	}
	st.require = !isPhrase
}

// finish runs fts3ExprParse's endgame: a trailing operator is a syntax
// error, and the main tree attaches as the leftmost leaf of the '-' NOT
// chain ("-a -b c" becomes NOT(b, NOT(a, c)) — i.e. (c AND NOT a) AND NOT b).
func (st *legacyParseState) finish() (*exprNode, error) {
	if st.err == nil && st.pRet != nil && st.require {
		st.err = st.syntaxErr
	}
	if st.err == nil && st.pNotBr != nil {
		if st.pRet == nil {
			st.err = st.syntaxErr
		} else {
			pIter := st.pNotBr
			for pIter.pLeft != nil {
				pIter = pIter.pLeft
			}
			pIter.pLeft = st.pRet
			st.pRet.pParent = pIter
			st.pRet = st.pNotBr
		}
	}
	if st.err != nil {
		return nil, st.err
	}
	return st.pRet, nil
}

// scanWordAt returns the run of token characters at pos (empty when none).
func scanWordAt(input string, pos int) string {
	e := pos
	for e < len(input) && isWordCharAt(input, e) {
		e++
	}
	return input[pos:e]
}

// fts3ReadInt ports sqlite3Fts3ReadInt (fts3.c): digits only; on overflow
// past 0x7FFFFFFF it returns (-1 bytes consumed, caller keeps its default) —
// the C returns -1 without writing the output.
func fts3ReadInt(s string, i int) (int, int) {
	start := i
	val := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		val = val*10 + int(s[i]-'0')
		if val > 0x7FFFFFFF {
			return 0, -1
		}
		i++
	}
	if i == start {
		return 0, 0
	}
	return val, i - start
}

// isFTS3Space ports fts3isspace.
func isFTS3Space(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

// parsePhraseFrom parses a quoted phrase whose opening quote sits at
// quotePos-1 in the input — quotePos is the byte after the opening quote
// (the legacy path's wrapper around the shared phrase lexer).
func (p *legacyParser) parsePhraseFrom(quotePos int) (QueryNode, error) {
	shared := &queryParser{input: p.input, pos: quotePos - 1}
	return shared.parsePhrase()
}

// legacyNextNode ports getNextNode for one round starting at start. It
// returns the next expression node (nil for a tokenless round), the number
// of bytes consumed, done=true at end of input, and a syntax error for an
// unterminated quote.
func (p *legacyParser) legacyNextNode(start int) (node *exprNode, consumed int, done bool, err error) {
	p.isNot = false
	zIn, nIn := p.skipLegacySpace(start)
	if nIn == 0 {
		return nil, 0, true, nil
	}
	if node, consumed, matched := p.keywordRound(start, zIn, nIn); matched {
		return node, consumed, false, nil
	}
	if p.input[zIn] == '"' {
		return p.quotedPhraseRound(zIn, start)
	}
	return p.tokenRound(start, zIn)
}

// skipLegacySpace skips whitespace before the keyword/quote/token checks
// (fts3isspace) and returns the new position and remaining length.
func (p *legacyParser) skipLegacySpace(start int) (zIn, nIn int) {
	zIn = start
	nIn = len(p.input) - start
	for nIn > 0 && isFTS3Space(p.input[zIn]) {
		zIn++
		nIn--
	}
	return zIn, nIn
}

// legacyKeyword mirrors fts3_expr.c's aKeyword table: OR and NEAR are always
// keywords; AND and NOT only exist in parenthesis mode (parenOnly).
type legacyKeyword struct {
	text      string
	code      int
	parenOnly bool
}

var legacyKeywords = []legacyKeyword{
	{"OR", ftsqueryOr, false},
	{"AND", ftsqueryAnd, true},
	{"NOT", ftsqueryNot, true},
	{"NEAR", ftsqueryNear, false},
}

// keywordRound matches a boolean keyword at zIn. The following byte must be
// whitespace, a quote, a parenthesis or end of input, else the word is a
// term ("ORacle"). The consumed count spans the round start through the
// keyword end (the skipped whitespace included, as (zInput - z) + nKey in
// the C). Returns matched=false to fall through to the token rounds.
func (p *legacyParser) keywordRound(start, zIn, nIn int) (node *exprNode, consumed int, matched bool) {
	for _, kw := range legacyKeywords {
		if kw.parenOnly && !fts3ParenthesesEnabled() {
			continue
		}
		if nIn < len(kw.text) || p.input[zIn:zIn+len(kw.text)] != kw.text {
			continue
		}
		nKey, nNear := len(kw.text), 10 // SQLITE_FTS3_DEFAULT_NEAR_PARAM
		if kw.code == ftsqueryNear {
			adv := p.nearDistance(zIn, &nKey, &nNear)
			if !adv {
				continue
			}
		}
		if keywordBoundary(p.byteAt(zIn + nKey)) {
			return &exprNode{eType: kw.code, nNear: nNear}, (zIn - start) + nKey, true
		}
	}
	return nil, 0, false
}

// nearDistance parses an optional "/n" suffix after NEAR (zIn points at
// 'N'). It updates nKey (keyword length incl. the suffix) and nNear, and
// reports whether the keyword still stands: on varint overflow the suffix
// is not consumed and the bare-token round takes "NEAR/..." as a term.
func (p *legacyParser) nearDistance(zIn int, nKey *int, nNear *int) bool {
	if zIn+4 >= len(p.input) || p.input[zIn+4] != '/' || zIn+5 >= len(p.input) || p.input[zIn+5] < '0' || p.input[zIn+5] > '9' {
		return true
	}
	d, adv := fts3ReadInt(p.input, zIn+5)
	if adv < 0 {
		// sqlite3Fts3ReadInt overflow: the distance is not consumed and
		// nNear keeps the default; the keyword boundary test below fails,
		// leaving "NEAR/<huge>" to the bare-token round.
		return false
	}
	*nNear = d
	*nKey += 1 + adv
	return true
}

// byteAt returns input[i] or 0 past the end (the C reads the NUL).
func (p *legacyParser) byteAt(i int) byte {
	if i < len(p.input) {
		return p.input[i]
	}
	return 0
}

// quotedPhraseRound parses a quoted phrase whose opening quote is at zIn
// (no escaping exists; the closing quote must be present).
func (p *legacyParser) quotedPhraseRound(zIn, start int) (node *exprNode, consumed int, done bool, err error) {
	ii := 1
	for ii < len(p.input)-zIn && p.input[zIn+ii] != '"' {
		ii++
	}
	consumed = (zIn - start) + ii + 1
	if zIn+ii >= len(p.input) {
		return nil, 0, false, fmt.Errorf("unterminated string literal")
	}
	phrase, perr := p.parsePhraseFrom(zIn + 1)
	if perr != nil {
		return nil, 0, false, perr
	}
	return &exprNode{eType: ftsqueryPhrase, phrase: phrase}, consumed, false, nil
}

// tokenRound parses a bare token (getNextToken): the tokenizer sees the
// buffer from the ROUND start (not the whitespace-skipped position), so the
// byte directly before the token decides the '-' (legacy) and '^' (FTS4)
// qualifiers. A tokenless round consumes the remainder up to a barred '"'.
func (p *legacyParser) tokenRound(start, zIn int) (node *exprNode, consumed int, done bool, err error) {
	colLen := p.columnPrefixLen(zIn, start)
	buf := p.input[start+colLen:]
	i := 0
	for i < len(buf) && !isWordCharAt(buf, i) {
		// findBarredChar: a '"' stops a tokenless round so the next round
		// can parse the quoted phrase (legacy mode bars only '"').
		if buf[i] == '"' {
			return nil, colLen + i, false, nil
		}
		i++
	}
	if i >= len(buf) {
		// No token in the remainder: consume it entirely (getNextToken's
		// SQLITE_DONE branch, *pnConsumed = n).
		return nil, len(p.input) - start, false, nil
	}
	wordStart, _, tokEnd, isPrefix, first := scanToken(buf, i, p)
	word := buf[wordStart:tokEnd]
	if isPrefix {
		tokEnd++
	}
	var phrase QueryNode
	if isPrefix {
		phrase = &PrefixNode{Prefix: word, First: first}
	} else {
		phrase = &TermNode{Term: word, First: first}
	}
	if cl := p.columnNameAt(zIn); cl != "" || colLen > 0 {
		if cl == "" {
			cl = asciiLowerBytes(scanWordAt(p.input, zIn))
		}
		phrase = &ColumnRefNode{ColumnName: cl, Inner: phrase}
	}
	return &exprNode{eType: ftsqueryPhrase, phrase: phrase}, colLen + tokEnd, false, nil
}

// scanToken finds the token starting at i in buf, applies the '-'/'^'
// qualifier walk (which only moves the qualifier scan point — the token
// text never includes those bytes; the C token text comes from the
// tokenizer while iStart just walks back) and the trailing '*' prefix flag.
// scanToken needs the parser only for the parentheses mode gate on '-'.
func scanToken(buf string, i int, p *legacyParser) (wordStart, tokStart, tokEnd int, isPrefix, first bool) {
	wordStart = i
	tokStart = i
	tokEnd = i
	for tokEnd < len(buf) && isWordCharAt(buf, tokEnd) {
		tokEnd++
	}
	for tokStart > 0 {
		if !fts3ParenthesesEnabled() && buf[tokStart-1] == '-' {
			p.isNot = true
			tokStart--
		} else if buf[tokStart-1] == '^' {
			first = true
			tokStart--
		} else {
			break
		}
	}
	isPrefix = tokEnd < len(buf) && buf[tokEnd] == '*'
	return wordStart, tokStart, tokEnd, isPrefix, first
}

// columnPrefixLen reports the length of a "colname:" prefix at zIn (0 when
// absent): a table column name immediately followed by ':' scopes the token
// (getNextNode's azCol loop). Parsed syntactically here; the schema resolves
// the index later (ColumnRefNode).
func (p *legacyParser) columnPrefixLen(zIn, start int) int {
	if w := scanWordAt(p.input, zIn); w != "" && zIn+len(w) < len(p.input) && p.input[zIn+len(w)] == ':' {
		return (zIn - start) + len(w) + 1
	}
	return 0
}

// columnNameAt returns the lower-cased column name when a "word:" prefix
// sits at zIn.
func (p *legacyParser) columnNameAt(zIn int) string {
	if w := scanWordAt(p.input, zIn); w != "" && zIn+len(w) < len(p.input) && p.input[zIn+len(w)] == ':' {
		return asciiLowerBytes(w)
	}
	return ""
}
