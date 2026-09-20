package fts5

import (
	"strings"
)

// This file evaluates a parsed MATCH expression (query.go's nodes) to
// matching rowid sets against the inverted index: phrase/near evaluation and
// the NEAR window check (fts5_expr.c's set-based evaluation).

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
		if rowidInAll(rowid, sets, smallest) {
			out[rowid] = true
		}
	}
	return out
}

// rowidInAll reports whether rowid is present in every set except skip.
func rowidInAll(rowid int64, sets []map[int64]bool, skip int) bool {
	for i, s := range sets {
		if i != skip && !s[rowid] {
			return false
		}
	}
	return true
}

// phraseCols returns the phrase's active columns.
func phraseCols(t *Table, ph *phraseNode) []int {
	return activeColIndexes(t, ph.colset)
}

// evalPhrase evaluates one phrase to its matching rowids.
func (t *Table) evalPhrase(ph *phraseNode) (map[int64]bool, error) {
	if len(ph.terms) == 0 {
		// A zero-token phrase (e.g. MATCH '"/"' under unicode61) matches
		// nothing; C's eval loop never reaches term iteration for it.
		return map[int64]bool{}, nil
	}
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
		cand = intersectSortedRowids(cand, m)
	}
	for _, rowid := range cand {
		if t.nearMatchesDoc(phrases, rowid, cols, window) {
			out[rowid] = true
		}
	}
	return out, nil
}

// intersectSortedRowids keeps the ascending rowids present in m.
func intersectSortedRowids(cand []int64, m map[int64]bool) []int64 {
	var next []int64
	for _, rowid := range cand {
		if m[rowid] {
			next = append(next, rowid)
		}
	}
	return next
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
	for _, rowid := range t.phraseCandidateRowids(terms, cols) {
		if t.docHasPhrase(rowid, terms, cols, first) {
			out[rowid] = true
		}
	}
	return out
}

// docHasPhrase reports whether the token chain occurs in one of the doc's
// active columns (positions step by one).
func (t *Table) docHasPhrase(rowid int64, terms []qTerm, cols []int, first bool) bool {
	doc := t.ix.Doc(rowid)
	if doc == nil {
		return false
	}
	for _, col := range activeColIndexes(t, cols) {
		if col >= len(doc.cols) {
			continue
		}
		if phraseStartsAt(terms, doc.cols[col], first) {
			return true
		}
	}
	return false
}

// phraseStartsAt reports whether the phrase occurs anywhere in tokens (its
// first token at some position, the chain stepping by one); a ^first phrase
// may only start at position 0.
func phraseStartsAt(terms []qTerm, tokens []string, first bool) bool {
	for i := 0; i+len(terms) <= len(tokens); i++ {
		if first && i != 0 {
			break
		}
		if phraseMatches(terms, tokens, i) {
			return true
		}
	}
	return false
}

// phraseCandidateRowids narrows to docs containing the first term (or all
// prefix expansions of it).
func (t *Table) phraseCandidateRowids(terms []qTerm, cols []int) []int64 {
	set := make(map[int64]bool)
	for _, col := range activeColIndexes(t, cols) {
		t.addFirstTermRowids(set, terms[0], col)
	}
	rowids := make([]int64, 0, len(set))
	for rowid := range set {
		rowids = append(rowids, rowid)
	}
	sortRowids(rowids)
	return rowids
}

// addFirstTermRowids adds the column's docs for the phrase's first term, or
// for every prefix expansion of it.
func (t *Table) addFirstTermRowids(set map[int64]bool, term qTerm, col int) {
	if !term.prefix {
		for rowid := range t.ix.termRowids(term.term, col) {
			set[rowid] = true
		}
		return
	}
	for _, term := range t.ix.prefixTerms(term.term) {
		for rowid := range t.ix.termRowids(term, col) {
			set[rowid] = true
		}
	}
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
		if t.columnNearMatch(phrases, doc.cols[col], window) {
			return true
		}
	}
	return false
}

// columnNearMatch checks one column: every phrase needs an instance and the
// instance set must fit the NEAR window (fts5ExprNearIsMatch).
func (t *Table) columnNearMatch(phrases []*phraseNode, tokens []string, window int) bool {
	instances := make([][]int, len(phrases))
	for i, ph := range phrases {
		pos := phraseInstancePositions(ph, tokens)
		if len(pos) == 0 {
			return false
		}
		instances[i] = pos
	}
	return nearWindowMatch(instances, phraseSizes(phrases), window)
}

// phraseSizes returns each phrase's token count.
func phraseSizes(phrases []*phraseNode) []int {
	sizes := make([]int, len(phrases))
	for i, ph := range phrases {
		sizes[i] = len(ph.terms)
	}
	return sizes
}

// phraseInstancePositions collects the positions of one phrase's instances in
// the column's token stream (the position of its first token); a ^phrase
// anchors at position 0 only.
func phraseInstancePositions(ph *phraseNode, tokens []string) []int {
	var pos []int
	for p := 0; p+len(ph.terms) <= len(tokens); p++ {
		if ph.first && p != 0 {
			break
		}
		if phraseMatches(ph.terms, tokens, p) {
			pos = append(pos, p)
		}
	}
	return pos
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
