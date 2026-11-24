package fts

import (
	"sort"
	"strings"
)

// This file implements FTS4 deferred-token evaluation. SQLite marks an
// overflow-sized token in a multi-token AND/NEAR cluster as deferred, skips
// its segment doclist while selecting candidate rows, and re-tokenizes the
// current row's content to obtain that token's positions before deciding the
// final match (fts3.c fts3EvalStart / fts3EvalTestDeferred and fts3_write.c
// sqlite3Fts3CacheDeferredDoclists). Frigolite's in-memory index keeps the
// stored column values, so the same rescan is available without reading a
// segment.

// tokenPositionsFromContent returns a deferred token's positions in one stored
// document. col < 0 scans every indexed column.
func tokenPositionsFromContent(idx *InvertedIndex, docID int64, tok Tokenizer, term string, prefix, first bool, col int) []Posting {
	doc := idx.GetDoc(docID)
	if doc == nil || tok == nil {
		return nil
	}
	tok = tokenizerForDoc(tok, doc.LangID)
	limit := len(doc.Columns)
	if idx.nCol > 0 && idx.nCol < limit {
		limit = idx.nCol
	}
	var out []Posting
	for c := 0; c < limit; c++ {
		if col >= 0 && c != col || idx.skipColumns[c] {
			continue
		}
		out = append(out, contentColumnPostings(docID, c, ftsColumnString(doc.Columns[c]), tok, term, prefix, first)...)
	}
	return out
}

// tokenizerForDoc binds a language-aware tokenizer to the document's langid.
func tokenizerForDoc(tok Tokenizer, langid int64) Tokenizer {
	if la, ok := tok.(LangidAware); ok {
		return la.WithLangid(langid)
	}
	return tok
}

// contentColumnPostings re-tokenizes one column and keeps the tokens matching
// a deferred term. The column-first pin keeps only position zero.
func contentColumnPostings(docID int64, col int, text string, tok Tokenizer, term string, prefix, first bool) []Posting {
	var out []Posting
	for _, token := range tok.Tokenize(text) {
		if first && token.Position != 0 || !contentTokenMatches(token.Term, term, prefix) {
			continue
		}
		out = append(out, Posting{DocID: docID, Column: col, Position: token.Position})
	}
	return out
}

func contentTokenMatches(token, term string, prefix bool) bool {
	if prefix {
		return len(token) >= len(term) && token[:len(term)] == term
	}
	return token == term
}

// termPostingsForNode returns one term node's postings in a document. A
// deferred term uses the content rescan; every other term reads the index.
func termPostingsForNode(idx *InvertedIndex, docID int64, node QueryNode, col int) []Posting {
	switch n := node.(type) {
	case *TermNode:
		if n.Deferred {
			return tokenPositionsFromContent(idx, docID, n.Tokenizer, n.Term, false, n.First, col)
		}
		return filterPostings(idx.index[n.Term], docID, col, n.First)
	case *PrefixNode:
		if n.Deferred {
			return tokenPositionsFromContent(idx, docID, n.Tokenizer, n.Prefix, true, n.First, col)
		}
		var out []Posting
		for term, postings := range idx.index {
			if !prefixMatches(n.Prefix, term) {
				continue
			}
			out = append(out, filterPostings(postings, docID, col, n.First)...)
		}
		return out
	}
	return nil
}

// filterPostings keeps one document's postings in an optional column. The
// column-first operator keeps only position zero.
func filterPostings(postings []Posting, docID int64, col int, first bool) []Posting {
	var out []Posting
	for _, p := range postings {
		if p.DocID != docID || (col >= 0 && p.Column != col) {
			continue
		}
		if first && p.Position != 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

// termNearPositions returns a term node's NEAR positions: its own token
// offsets, in the optional restricted column.
func termNearPositions(idx *InvertedIndex, docID int64, node QueryNode, col int) []nearPos {
	out := make([]nearPos, 0)
	seen := make(map[nearPos]bool)
	for _, p := range termPostingsForNode(idx, docID, node, col) {
		np := nearPos{pos: p.Position, col: p.Column}
		if !seen[np] {
			seen[np] = true
			out = append(out, np)
		}
	}
	sortNearPositions(out)
	return out
}

// phraseTokenNode builds the term node for one token of a phrase, carrying
// that token's deferral mark into the shared position helpers. The first-token
// pin is applied by the phrase caller, not here.
func phraseTokenNode(phrase *PhraseNode, i int) QueryNode {
	prefix := i < len(phrase.Prefixes) && phrase.Prefixes[i]
	deferred := i < len(phrase.Deferred) && phrase.Deferred[i]
	if prefix {
		return &PrefixNode{Prefix: phrase.Terms[i], Deferred: deferred, Tokenizer: phrase.Tokenizer}
	}
	return &TermNode{Term: phrase.Terms[i], Deferred: deferred, Tokenizer: phrase.Tokenizer}
}

// phraseOccurrenceLastPositions returns the last-token positions of every
// occurrence of a phrase in one document. Per-token position sets let a phrase
// mix deferred tokens (content rescan) with ordinary tokens (postings).
// firstAt >= 0 pins the marked token to absolute position zero; firstAt > 0
// can never match because earlier phrase tokens would need negative offsets.
func phraseOccurrenceLastPositions(idx *InvertedIndex, docID int64, phrase *PhraseNode, col, firstAt int) []nearPos {
	if len(phrase.Terms) == 0 || firstAt > 0 {
		return nil
	}
	sets := phrasePositionSets(idx, docID, phrase, col, firstAt)
	out := phraseLastPositions(sets)
	sortNearPositions(out)
	return out
}

// phrasePositionSets collects each phrase token's distinct (column, position)
// occurrences. A firstAt pin keeps only absolute position zero for that token.
func phrasePositionSets(idx *InvertedIndex, docID int64, phrase *PhraseNode, col, firstAt int) []map[nearPos]bool {
	sets := make([]map[nearPos]bool, len(phrase.Terms))
	for i := range phrase.Terms {
		sets[i] = make(map[nearPos]bool)
		for _, np := range termNearPositions(idx, docID, phraseTokenNode(phrase, i), col) {
			if firstAt == i && np.pos != 0 {
				continue
			}
			sets[i][np] = true
		}
	}
	return sets
}

// phraseLastPositions returns the last-token position of every consecutive
// token chain in the position sets.
func phraseLastPositions(sets []map[nearPos]bool) []nearPos {
	var out []nearPos
	for start := range sets[0] {
		if phraseChainMatches(sets, start) {
			out = append(out, nearPos{pos: start.pos + len(sets) - 1, col: start.col})
		}
	}
	return out
}

func phraseChainMatches(sets []map[nearPos]bool, start nearPos) bool {
	for i := 1; i < len(sets); i++ {
		if !sets[i][nearPos{pos: start.pos + i, col: start.col}] {
			return false
		}
	}
	return true
}

func sortNearPositions(positions []nearPos) {
	sort.Slice(positions, func(i, j int) bool {
		if positions[i].col != positions[j].col {
			return positions[i].col < positions[j].col
		}
		return positions[i].pos < positions[j].pos
	})
}

// termDoclistSizeFromContent estimates the packed FTS3 doclist size of a
// token from the stored row text. It recovers the overflow decision after a
// segment block has been zeroblobbed and its postings are no longer loaded.
func termDoclistSizeFromContent(idx *InvertedIndex, tok Tokenizer, term phraseTerm) int {
	if tok == nil || len(idx.docs) == 0 {
		return 0
	}
	key := contentDoclistKey{term: term.text, prefix: term.prefix}
	if size, ok := idx.contentDoclistSizes[key]; ok {
		return size
	}
	size := estimateTermDoclistSizeFromContent(idx, tok, term)
	if idx.contentDoclistSizes == nil {
		idx.contentDoclistSizes = make(map[contentDoclistKey]int)
	}
	idx.contentDoclistSizes[key] = size
	return size
}

// estimateTermDoclistSizeFromContent scans the stored row text once for one
// token. Callers cache the result until the index changes.
func estimateTermDoclistSizeFromContent(idx *InvertedIndex, tok Tokenizer, term phraseTerm) int {
	ids := idx.AllDocIDs()
	size, prevDoc := 0, int64(0)
	for n, docID := range ids {
		positions, columns := deferredTokenPositions(idx.GetDoc(docID), tok, term, idx.skipColumns)
		if len(positions) == 0 {
			continue
		}
		delta := docID - prevDoc
		if n == 0 {
			delta = docID
		}
		size += fts3VarintLen(uint64(delta)) + 1 + len(columns)
		for _, pos := range positions {
			size += fts3VarintLen(uint64(pos + 2))
		}
		prevDoc = docID
		if size >= ftsDeferredDoclistBytes {
			return size
		}
	}
	return size
}

// deferredTokenPositions returns a token's positions and the columns that
// contribute them for one stored document.
func deferredTokenPositions(doc *Document, tok Tokenizer, term phraseTerm, skip map[int]bool) ([]int, []int) {
	if doc == nil || tok == nil {
		return nil, nil
	}
	tok = tokenizerForDoc(tok, doc.LangID)
	var positions, columns []int
	for c, value := range doc.Columns {
		if skip[c] {
			continue
		}
		for _, token := range tok.Tokenize(ftsColumnString(value)) {
			if !contentTokenMatches(token.Term, term.text, term.prefix) {
				continue
			}
			positions = append(positions, token.Position)
			columns = append(columns, c)
		}
	}
	return positions, columns
}

// markDeferredTokens applies the FTS4 deferral choice to a prepared query
// tree. Tokens are never deferred outside a multi-token AND/NEAR cluster and
// never for content=<table> tables (fts3.c fts3EvalSelectDeferred).
func (t *FTS3Table) markDeferredTokens(node QueryNode) {
	if t == nil || node == nil || t.tokenizer == nil {
		return
	}
	if !strings.EqualFold(t.moduleName, "fts4") || t.contentTable != "" {
		return
	}
	markDeferredNode(node, t)
}

func markDeferredNode(node QueryNode, t *FTS3Table) {
	switch n := node.(type) {
	case *OrNode:
		markDeferredNode(n.Left, t)
		markDeferredNode(n.Right, t)
	case *NotNode:
		markDeferredNode(n.Inner, t)
	case *AndNode:
		markDeferredCluster(n, t)
		markDeferredNode(n.Left, t)
		markDeferredNode(n.Right, t)
	case *NearNode:
		markDeferredCluster(n, t)
		markDeferredNode(n.Left, t)
		markDeferredNode(n.Right, t)
	case *ColumnNode:
		markDeferredCluster(n.Inner, t)
	default:
		markDeferredCluster(node, t)
	}
}

// markDeferredCluster marks the overflow-sized tokens of one cluster when the
// cluster has more than one token and at least one overflow-sized doclist.
func markDeferredCluster(node QueryNode, t *FTS3Table) {
	terms := collectClusterTerms(node)
	if len(terms) < 2 {
		return
	}
	for _, term := range terms {
		if t.termDoclistOverflows(term) {
			markDeferredNodeTerms(node, t)
			return
		}
	}
}

// collectClusterTerms gathers phrase tokens connected by AND/NEAR. OR and
// NOT boundaries separate clusters, as in fts3EvalSelectDeferred.
func collectClusterTerms(node QueryNode) []phraseTerm {
	switch n := node.(type) {
	case *TermNode:
		return []phraseTerm{{text: n.Term}}
	case *PrefixNode:
		return []phraseTerm{{text: n.Prefix, prefix: true}}
	case *PhraseNode:
		out := make([]phraseTerm, 0, len(n.Terms))
		for i, term := range n.Terms {
			out = append(out, phraseTerm{text: term, prefix: i < len(n.Prefixes) && n.Prefixes[i]})
		}
		return out
	case *ColumnNode:
		return collectClusterTerms(n.Inner)
	case *ColumnRefNode:
		return collectClusterTerms(n.Inner)
	case *AndNode:
		return append(collectClusterTerms(n.Left), collectClusterTerms(n.Right)...)
	case *NearNode:
		return append(collectClusterTerms(n.Left), collectClusterTerms(n.Right)...)
	}
	return nil
}

func markDeferredNodeTerms(node QueryNode, t *FTS3Table) {
	switch n := node.(type) {
	case *TermNode:
		markDeferredTerm(n, phraseTerm{text: n.Term}, t)
	case *PrefixNode:
		markDeferredTerm(n, phraseTerm{text: n.Prefix, prefix: true}, t)
	case *PhraseNode:
		markDeferredPhrase(n, t)
	case *ColumnNode:
		markDeferredNodeTerms(n.Inner, t)
	case *ColumnRefNode:
		markDeferredNodeTerms(n.Inner, t)
	case *AndNode:
		markDeferredNodeTerms(n.Left, t)
		markDeferredNodeTerms(n.Right, t)
	case *NearNode:
		markDeferredNodeTerms(n.Left, t)
		markDeferredNodeTerms(n.Right, t)
	}
}

func (n *TermNode) setDeferred(tok Tokenizer) {
	n.Deferred, n.Tokenizer = true, tok
}

func (n *PrefixNode) setDeferred(tok Tokenizer) {
	n.Deferred, n.Tokenizer = true, tok
}

func markDeferredTerm(node interface{ setDeferred(Tokenizer) }, term phraseTerm, t *FTS3Table) {
	if t.termDoclistOverflows(term) {
		node.setDeferred(t.tokenizer)
	}
}

func markDeferredPhrase(n *PhraseNode, t *FTS3Table) {
	if len(n.Deferred) != len(n.Terms) {
		n.Deferred = make([]bool, len(n.Terms))
	}
	n.Tokenizer = t.tokenizer
	for i, term := range n.Terms {
		if t.termDoclistOverflows(phraseTerm{text: term, prefix: i < len(n.Prefixes) && n.Prefixes[i]}) {
			n.Deferred[i] = true
		}
	}
}
