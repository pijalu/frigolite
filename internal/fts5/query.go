package fts5

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file ports the fts5 MATCH query language (fts5_expr.c + fts5parse.y):
// quoted phrases, bareword terms, prefix terms (abc*), initial-token caret
// (^abc), column filters (a : term, {a b} : term), implicit AND, the explicit
// AND/OR/NOT operators and NEAR(phrase phrase, N). Evaluation resolves nodes
// to matching rowid sets against the inverted index.

// qTerm is one token of a phrase; Prefix marks an abc* term.
type qTerm struct {
	term   string
	prefix bool
}

// queryNode is a parsed MATCH expression.
type queryNode interface {
	// eval returns the matching rowids restricted to the active columns.
	eval(t *Table, cols []int) (map[int64]bool, error)
}

// qParser parses one MATCH query string.
type qParser struct {
	src  string
	tok  string // current token text ("" at end)
	kind int    // current token kind
	bad  bool   // lexer hit an invalid position (tok holds the offender)
	t    *Table
}

// token kinds.
const (
	tkEOF = iota
	tkWord
	tkString
	tkLP
	tkRP
	tkColon
	tkCaret
	tkPlus
	tkStar
	tkLBrace
	tkRBrace
	tkComma
	tkMinus
)

// matchSyntaxError renders C's query syntax error (fts5_expr.c:2005).
func matchSyntaxError(near string) error {
	return fmt.Errorf("fts5: syntax error near \"%s\"", near)
}

// parseQuery parses a MATCH query against a table.
func parseQuery(t *Table, query string) (queryNode, error) {
	p := &qParser{src: query, t: t}
	p.next()
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.kind != tkEOF {
		return nil, matchSyntaxError(p.tok)
	}
	return node, nil
}

func (p *qParser) next() {
	p.src = strings.TrimLeft(p.src, " \t\n\r\v\f")
	if p.src == "" {
		p.kind = tkEOF
		p.tok = ""
		return
	}
	switch p.src[0] {
	case '(':
		p.advance(1, tkLP)
	case ')':
		p.advance(1, tkRP)
	case ':':
		p.advance(1, tkColon)
	case '^':
		p.advance(1, tkCaret)
	case '+':
		p.advance(1, tkPlus)
	case '*':
		p.advance(1, tkStar)
	case '{':
		p.advance(1, tkLBrace)
	case '}':
		p.advance(1, tkRBrace)
	case ',':
		p.advance(1, tkComma)
	case '"', '\'', '`', '[':
		closeQ := map[byte]byte{'"': '"', '\'': '\'', '`': '`', '[': ']'}[p.src[0]]
		q := p.src[0]
		var sb strings.Builder
		i := 1
		for i < len(p.src) {
			if p.src[i] == closeQ {
				if q != '[' && i+1 < len(p.src) && p.src[i+1] == q {
					sb.WriteByte(q)
					i += 2
					continue
				}
				p.tok = sb.String()
				p.src = p.src[i+1:]
				p.kind = tkString
				return
			}
			sb.WriteByte(p.src[i])
			i++
		}
		// Unterminated string: the whole tail is the offending token.
		p.kind = tkEOF
		p.tok = sb.String()
		p.bad = true
	default:
		i := 0
		for i < len(p.src) && isQueryWordByte(p.src[i]) {
			i++
		}
		if i == 0 {
			// An unknown punctuation byte: report it as the offending token.
			p.tok = p.src[:1]
			p.kind = tkEOF
			p.bad = true
			return
		}
		p.tok = p.src[:i]
		p.src = p.src[i:]
		p.kind = tkWord
	}
}

// advance consumes n bytes as one punctuation token.
func (p *qParser) advance(n int, kind int) {
	p.tok = p.src[:n]
	p.src = p.src[n:]
	p.kind = kind
}

// isQueryWordByte reports whether b continues a bareword (fts5_expr.c's
// lexer: everything except whitespace and the reserved punctuation).
func isQueryWordByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f',
		'(', ')', '{', '}', ':', ',', '+', '*', '^', '"', '\'', '`', '[':
		return false
	}
	return true
}

// errAtEnd reports a syntax error when the lexer hit an invalid position.
func (p *qParser) errAtEnd() error {
	if p.bad {
		return matchSyntaxError(p.tok)
	}
	return nil
}

// parseOr parses OR-level expressions (lowest precedence).
func (p *qParser) parseOr() (queryNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.kind == tkWord && strings.EqualFold(p.tok, "OR") {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = orNode{left, right}
	}
	return left, nil
}

// parseAnd parses AND-level expressions (explicit AND or implicit
// concatenation).
func (p *qParser) parseAnd() (queryNode, error) {
	if err := p.errAtEnd(); err != nil {
		return nil, err
	}
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for {
		if p.kind == tkWord && strings.EqualFold(p.tok, "AND") {
			p.next()
		} else if p.startsOperand() {
			// Implicit AND: "a b" means a AND b.
		} else {
			break
		}
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = andNode{left, right}
	}
	return left, nil
}

// startsOperand reports whether the current token can begin an operand
// (implicit-AND lookahead). NOT/EOF/closing punctuation end the expression.
func (p *qParser) startsOperand() bool {
	switch p.kind {
	case tkLP, tkString, tkCaret, tkLBrace, tkMinus:
		return true
	case tkWord:
		return !strings.EqualFold(p.tok, "OR") && !strings.EqualFold(p.tok, "NOT")
	}
	return false
}

// parseNot parses NOT-level expressions (left NOT right).
func (p *qParser) parseNot() (queryNode, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.kind == tkWord && strings.EqualFold(p.tok, "NOT") {
		p.next()
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		left = notNode{left, right}
	}
	return left, nil
}

// parsePrimary parses one operand: a parenthesized expression, a column
// filter, a phrase set or NEAR.
func (p *qParser) parsePrimary() (queryNode, error) {
	if err := p.errAtEnd(); err != nil {
		return nil, err
	}
	switch p.kind {
	case tkLP:
		p.next()
		node, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.kind != tkRP {
			return nil, matchSyntaxError(p.tok)
		}
		p.next()
		return node, nil
	case tkWord:
		if strings.EqualFold(p.tok, "NEAR") {
			// NEAR is a keyword only when followed by '('; a bareword
			// "near" is an ordinary term (fts5parse.y).
			rest := &qParser{src: p.src, t: p.t}
			rest.next()
			if rest.kind == tkLP {
				return p.parseNear()
			}
		}
		return p.parsePhraseSet()
	case tkString, tkCaret:
		return p.parsePhraseSet()
	case tkLBrace:
		return p.parseColsetPhrase()
	}
	return nil, matchSyntaxError(p.tok)
}

// parseColsetPhrase parses "{a b} : expr" (fts5parse.y colset COLON expr).
func (p *qParser) parseColsetPhrase() (queryNode, error) {
	cols, err := p.parseColset()
	if err != nil {
		return nil, err
	}
	if p.kind != tkColon {
		return nil, matchSyntaxError(p.tok)
	}
	p.next()
	var child queryNode
	if p.kind == tkLP {
		p.next()
		child, err = p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.kind != tkRP {
			return nil, matchSyntaxError(p.tok)
		}
		p.next()
	} else {
		child, err = p.parsePrimary()
		if err != nil {
			return nil, err
		}
	}
	return colNode{cols: cols, child: child}, nil
}

// parseColset parses the column list inside braces.
func (p *qParser) parseColset() ([]int, error) {
	p.next() // consume '{'
	var cols []int
	accept := func(name string) error {
		idx := p.t.ColumnIndex(name)
		if idx < 0 {
			return fmt.Errorf("fts5: no such column: %s", name)
		}
		cols = append(cols, idx)
		return nil
	}
	for {
		if p.kind == tkRBrace {
			p.next()
			break
		}
		if p.kind != tkWord && p.kind != tkString {
			return nil, matchSyntaxError(p.tok)
		}
		if err := accept(p.tok); err != nil {
			return nil, err
		}
		p.next()
		if p.kind == tkComma {
			p.next()
		}
	}
	if len(cols) == 0 {
		return nil, matchSyntaxError("}")
	}
	return cols, nil
}

// parsePhraseSet parses a phrase with optional + continuations and a trailing
// prefix star, or a NEAR call, or a column filter "a : ...".
func (p *qParser) parsePhraseSet() (queryNode, error) {
	// A bareword column filter ("a : term" / "a : (expr)") is detected before
	// phrase parsing: the LHS must be a single plain bareword.
	if p.kind == tkWord && !strings.EqualFold(p.tok, "NEAR") {
		rest := &qParser{src: p.src, t: p.t}
		rest.next()
		if rest.kind == tkColon {
			return p.parseColFilter()
		}
	}
	first, err := p.parsePhrase()
	if err != nil {
		return nil, err
	}
	terms := first.terms
	for p.kind == tkPlus {
		p.next()
		next, err := p.parsePhrase()
		if err != nil {
			return nil, err
		}
		terms = append(terms, next.terms...)
	}
	return phraseNode{terms: terms, initial: first.initial}, nil
}

// parseColFilter parses "<colname> : expr" (the LHS bareword is consumed).
func (p *qParser) parseColFilter() (queryNode, error) {
	name := p.tok
	p.next() // column name
	p.next() // colon
	idx := p.t.ColumnIndex(name)
	if idx < 0 {
		return nil, fmt.Errorf("fts5: no such column: %s", name)
	}
	var child queryNode
	var err error
	if p.kind == tkLP {
		p.next()
		child, err = p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.kind != tkRP {
			return nil, matchSyntaxError(p.tok)
		}
		p.next()
	} else {
		child, err = p.parsePrimary()
		if err != nil {
			return nil, err
		}
	}
	return colNode{cols: []int{idx}, child: child}, nil
}

// parsePhrase parses one phrase: an optional caret, a bareword or string,
// and a trailing star. A bareword "NEAR" not followed by '(' is an ordinary
// term. A quoted string is TOKENIZED by the table's tokenizer into one or
// more adjacent terms (fts5_expr.c: a quoted phrase's tokens must appear
// consecutively), so '"one two"' is the two-term phrase [one, two].
func (p *qParser) parsePhrase() (*phraseNode, error) {
	initial := false
	if p.kind == tkCaret {
		initial = true
		p.next()
	}
	var terms []qTerm
	prefix := false
	switch p.kind {
	case tkWord:
		// A bareword goes through the table's tokenizer too: case folding
		// (MATCH 'HELLO' matches 'hello') and the porter stemmer apply to
		// query terms exactly as to indexed text (fts5_expr.c tokenizes the
		// query with the table's tokenizer). A bareword that splits into
		// several tokens becomes a phrase.
		for _, tok := range p.t.tok.Tokenize(p.tok) {
			terms = append(terms, qTerm{term: tok.Term})
		}
		p.next()
	case tkString:
		for _, tok := range p.t.tok.Tokenize(p.tok) {
			terms = append(terms, qTerm{term: tok.Term})
		}
		p.next()
	default:
		return nil, matchSyntaxError(p.tok)
	}
	if len(terms) == 0 {
		return nil, matchSyntaxError("")
	}
	if p.kind == tkStar {
		prefix = true
		p.next()
	}
	if prefix {
		terms[len(terms)-1].prefix = true
	}
	return &phraseNode{terms: terms, initial: initial}, nil
}

// parseNearArgs parses the NEAR argument list (NEAR already consumed).
func (p *qParser) parseNear() (queryNode, error) {
	p.next() // consume NEAR
	if p.kind != tkLP {
		return nil, matchSyntaxError(p.tok)
	}
	p.next()
	var phrases []*phraseNode
	window := 10 // fts5's default NEAR window
	for p.kind != tkRP {
		ph, err := p.parsePhrase()
		if err != nil {
			return nil, err
		}
		phrases = append(phrases, ph)
		if p.kind == tkPlus {
			p.next()
			continue
		}
		if p.kind == tkComma {
			p.next()
			if p.kind != tkWord {
				return nil, matchSyntaxError(p.tok)
			}
			n, err := strconv.Atoi(p.tok)
			if err != nil || n <= 0 {
				return nil, matchSyntaxError(p.tok)
			}
			window = n
			p.next()
			break
		}
		if p.kind != tkRP && p.kind != tkWord && p.kind != tkString && p.kind != tkCaret {
			return nil, matchSyntaxError(p.tok)
		}
	}
	if p.kind != tkRP {
		return nil, matchSyntaxError(p.tok)
	}
	p.next()
	if len(phrases) < 2 {
		return nil, matchSyntaxError("NEAR")
	}
	return nearNode{phrases: phrases, window: window}, nil
}

// colNode restricts a child expression to a column set.
type colNode struct {
	cols  []int
	child queryNode
}

func (n colNode) eval(t *Table, cols []int) (map[int64]bool, error) {
	if t.cfg.Detail == DetailNone {
		return nil, fmt.Errorf("fts5: column queries are not supported (detail=none)")
	}
	return n.child.eval(t, n.cols)
}

// andNode, orNode, notNode are the boolean combinators.
type andNode struct{ l, r queryNode }
type orNode struct{ l, r queryNode }
type notNode struct{ l, r queryNode }

func (n andNode) eval(t *Table, cols []int) (map[int64]bool, error) {
	l, err := n.l.eval(t, cols)
	if err != nil {
		return nil, err
	}
	r, err := n.r.eval(t, cols)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(l))
	for rowid := range l {
		if r[rowid] {
			out[rowid] = true
		}
	}
	return out, nil
}

func (n orNode) eval(t *Table, cols []int) (map[int64]bool, error) {
	l, err := n.l.eval(t, cols)
	if err != nil {
		return nil, err
	}
	r, err := n.r.eval(t, cols)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(l)+len(r))
	for rowid := range l {
		out[rowid] = true
	}
	for rowid := range r {
		out[rowid] = true
	}
	return out, nil
}

func (n notNode) eval(t *Table, cols []int) (map[int64]bool, error) {
	l, err := n.l.eval(t, cols)
	if err != nil {
		return nil, err
	}
	r, err := n.r.eval(t, cols)
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

// phraseNode is a phrase: one or more adjacent tokens (a single term when
// len(terms) == 1).
type phraseNode struct {
	terms   []qTerm
	initial bool // ^abc: the first token sits at position 0
}

func (n phraseNode) eval(t *Table, cols []int) (map[int64]bool, error) {
	if err := t.requireDetailForPhrase(n.terms, n.initial); err != nil {
		return nil, err
	}
	if len(n.terms) == 1 && !n.terms[0].prefix && !n.initial {
		return t.termRowids(n.terms[0].term, cols), nil
	}
	return t.matchPhrase(n.terms, cols, n.initial), nil
}

// requireDetailForPhrase rejects phrase features the detail= mode cannot
// serve (fts5_expr.c:2239/2424).
func (t *Table) requireDetailForPhrase(terms []qTerm, initial bool) error {
	if len(terms) > 1 && t.cfg.Detail != DetailFull {
		return fmt.Errorf("fts5: phrase queries are not supported (detail!=full)")
	}
	if initial && t.cfg.Detail != DetailFull {
		return fmt.Errorf("fts5: phrase queries are not supported (detail!=full)")
	}
	return nil
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
func (t *Table) matchPhrase(terms []qTerm, cols []int, initial bool) map[int64]bool {
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
				if initial && i != 0 {
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
	sort.Slice(rowids, func(i, j int) bool { return rowids[i] < rowids[j] })
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

// nearNode is NEAR(phrase phrase ...[, N]): every phrase must have an
// instance within a window of `window` tokens inside one active column.
type nearNode struct {
	phrases []*phraseNode
	window  int
}

func (n nearNode) eval(t *Table, cols []int) (map[int64]bool, error) {
	if t.cfg.Detail == DetailNone {
		return nil, fmt.Errorf("fts5: NEAR queries are not supported (detail=none)")
	}
	if t.cfg.Detail != DetailFull {
		return nil, fmt.Errorf("fts5: NEAR queries are not supported (detail!=full)")
	}
	out := make(map[int64]bool)
	// Candidate docs: intersection over phrases.
	var cand []int64
	for i, ph := range n.phrases {
		m, err := ph.eval(t, cols)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			for rowid := range m {
				cand = append(cand, rowid)
			}
			sort.Slice(cand, func(a, b int) bool { return cand[a] < cand[b] })
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
		if t.nearMatchesDoc(n, rowid, cols) {
			out[rowid] = true
		}
	}
	return out, nil
}

// nearMatchesDoc checks one document: every phrase needs an instance and the
// instance set must fit C's NEAR window (fts5_expr.c fts5ExprNearIsMatch):
// anchoring on the running maximum position iMax, each phrase's instance at
// p (its FIRST token) must satisfy p >= iMax - nTerm_i - N and p <= iMax.
// Windows are per column.
func (t *Table) nearMatchesDoc(n nearNode, rowid int64, cols []int) bool {
	doc := t.ix.Doc(rowid)
	if doc == nil {
		return false
	}
	for _, col := range activeColIndexes(t, cols) {
		if col >= len(doc.cols) {
			continue
		}
		tokens := doc.cols[col]
		instances := make([][]int, len(n.phrases))
		empty := false
		for i, ph := range n.phrases {
			var pos []int
			for p := 0; p+len(ph.terms) <= len(tokens); p++ {
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
		if nearWindowMatch(n.phrases, instances, n.window) {
			return true
		}
	}
	return false
}

// nearWindowMatch ports fts5ExprNearIsMatch's advancing-anchors loop for one
// column's phrase instances (all non-empty, ascending).
func nearWindowMatch(phrases []*phraseNode, instances [][]int, window int) bool {
	idx := make([]int, len(instances))
	iMax := instances[0][0]
	for {
		bMatch := true
		for i := range instances {
			iMin := iMax - len(phrases[i].terms) - window
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
