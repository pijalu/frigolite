package fts

import (
	"fmt"
	"sort"
	"strings"
)

// This file implements the FTS3 offsets() and snippet() auxiliary functions
// (fts3_snippet.c sqlite3Fts3Offsets / sqlite3Fts3Snippet). Both operate on
// the current row of an FTS SELECT with a MATCH constraint: offsets() reports
// the byte span of each query-token occurrence, snippet() extracts a short
// text fragment around the matches.

// Offsets returns the offsets() result string for a document: for each
// column, for each query-token occurrence, "col termIndex start len " where
// termIndex is the token's index across all query phrases (left-to-right,
// token-by-token) and start/len are byte offsets in the column text
// (fts3_snippet.c sqlite3Fts3Offsets). It returns an error when the column
// text is shorter than the index (fts3_snippet.c: the tokenizer reaches DONE
// with query positions remaining - FTS_CORRUPT_VTAB). When override is
// non-nil (an FTS4 content=<table> table's content-row values), it supplies
// the column texts instead of the in-memory document's (SQLite reads the
// content table for offsets()/snippet(); fts4content 2.4/2.5).
func (t *FTS3Table) Offsets(docID int64, phrases []MatchPhrase, override ...[]interface{}) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	doc := t.index.GetDoc(docID)
	if doc == nil {
		return "", nil
	}
	var b strings.Builder
	nCol := len(t.columnNames)
	for col := 0; col < nCol; col++ {
		var colText string
		var ok bool
		// lenient marks a content=<table> override: the external content row
		// may legitimately be shorter than the index (the content was
		// updated/deleted after indexing); SQLite then yields empty offsets
		// instead of a corruption error (fts4content 2.5.4).
		lenient := false
		if len(override) > 0 && override[0] != nil && col < len(override[0]) {
			colText, ok = override[0][col].(string)
			lenient = true
		} else {
			colText, ok = doc.Columns[col].(string)
		}
		if !ok {
			continue
		}
		if err := t.offsetsColumn(&b, docID, phrases, col, colText, lenient); err != nil {
			return "", err
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// offsetsColumn appends one column's offset entries ("col termIndex start
// len ") to b. It returns a corruption error when the column text is shorter
// than the index (fts3_snippet.c: the tokenizer reaches DONE with query
// positions remaining).
func (t *FTS3Table) offsetsColumn(b *strings.Builder, docID int64, phrases []MatchPhrase, col int, colText string, lenient bool) error {
	// Per-phrase occurrence positions in this column (last-token positions
	// for phrases, token positions for single terms). A phrase inside a
	// NEAR reports only the occurrences that PARTICIPATE in the NEAR match
	// (fts3.c fts3EvalNearTrim): e.g. for 'a OR (b NEAR/1 c)', a trailing
	// 'c' with no adjacent 'b' yields no offset entry.
	positions := make([][]int, len(phrases))
	any := false
	for pi, mp := range phrases {
		pos := phrasePositionsInCol(t.index, docID, mp, col)
		if near, okN := mp.Scope.(*NearNode); okN {
			pos = nearParticipatingPositions(t.index, docID, near, mp.Side, col)
		}
		positions[pi] = pos
		if len(pos) > 0 {
			any = true
		}
	}
	if !any {
		return nil
	}
	tokens := tokenizeOffsets(t.tokenizer, colText)
	// A position beyond the tokenized content means the %_content row is
	// shorter than the index — corrupt (fts3_snippet.c: tokenizer DONE with
	// query positions remaining). For a content=<table> table (lenient) the
	// external content row may legitimately be shorter (updated after
	// indexing); SQLite yields empty offsets for the row (fts4content
	// 2.5.4).
	for _, posList := range positions {
		for _, p := range posList {
			if p >= len(tokens) {
				if lenient {
					return nil
				}
				return fmt.Errorf("database disk image is malformed")
			}
		}
	}
	terms := offsetsQueryTerms(phrases)
	// occPtr[ti] = how many occurrences of term ti's phrase have been
	// consumed.
	occPtr := make([]int, len(terms))
	for tokIdx, tok := range tokens {
		for ti := range terms {
			qt := terms[ti]
			if occPtr[ti] >= len(positions[qt.phraseIdx]) {
				continue
			}
			lastPos := positions[qt.phraseIdx][occPtr[ti]]
			// The k-th token of a phrase sits at lastPos - (len-1-k).
			termPos := lastPos - (qt.phraseLen - 1 - qt.tokenOff)
			if termPos == tokIdx {
				fmt.Fprintf(b, "%d %d %d %d ", col, ti, tok.Start, tok.End-tok.Start)
				occPtr[ti]++
			}
		}
	}
	return nil
}

// offsetsQueryTerm is one query token in the offsets() term list: it
// references its phrase's occurrence positions in the column and its offset
// within the phrase (SQLite's TermOffset.iOff: the token position of a
// phrase's k-th token is the phrase's last-token position minus (nToken-1-k)).
type offsetsQueryTerm struct {
	phraseIdx int
	phraseLen int
	tokenOff  int // token index within the phrase (0 = first)
}

// offsetsQueryTerms builds the query term list: one entry per token, in
// phrase order.
func offsetsQueryTerms(phrases []MatchPhrase) []offsetsQueryTerm {
	var terms []offsetsQueryTerm
	for pi, mp := range phrases {
		n := phraseTokenCount(mp.Node)
		for k := 0; k < n; k++ {
			terms = append(terms, offsetsQueryTerm{phraseIdx: pi, phraseLen: n, tokenOff: k})
		}
	}
	return terms
}

// phraseTokenCount returns the number of tokens in a phrase-like node.
func phraseTokenCount(node QueryNode) int {
	switch n := node.(type) {
	case *TermNode, *PrefixNode:
		return 1
	case *PhraseNode:
		return len(n.Terms)
	case *ColumnNode:
		return phraseTokenCount(n.Inner)
	case *ColumnRefNode:
		return phraseTokenCount(n.Inner)
	}
	return 1
}

// phrasePositionsInCol returns the occurrence positions of a phrase in a
// document column (the phrase's last-token positions for phrases, the token
// positions for terms/prefixes).
func phrasePositionsInCol(idx *InvertedIndex, docID int64, mp MatchPhrase, col int) []int {
	inner := mp.Node
	if cn, ok := inner.(*ColumnNode); ok {
		if cn.Column != col {
			return nil
		}
		inner = cn.Inner
	}
	switch n := inner.(type) {
	case *TermNode:
		pos := collectPostingPositions(idx.index[n.Term], docID, col)
		// fts3_snippet.c: a column-first (^term) phrase only matches at
		// position 0, so snippets must highlight only that occurrence.
		if n.First {
			pos = filterFirstPositions(pos, 0)
		}
		return pos
	case *PrefixNode:
		pos := collectPrefixPositions(idx, n.Prefix, docID, col)
		if n.First {
			pos = filterFirstPositions(pos, 0)
		}
		return pos
	case *PhraseNode:
		pos := collectPhraseNearPositions(idx, docID, n.Terms, n.Prefixes, col)
		out := make([]int, len(pos))
		for i, np := range pos {
			out[i] = np.pos
		}
		// ^ inside a quoted phrase pins the marked token to position 0;
		// FirstAt<0 means no pin (fts3first.test semantics).
		if n.FirstAt >= 0 {
			out = filterFirstPositions(out, n.FirstAt)
		}
		return out
	}
	return nil
}

// filterFirstPositions keeps only occurrences whose marked token sits at the
// given absolute position (the column-start pin introduced by '^').
func filterFirstPositions(pos []int, at int) []int {
	var out []int
	for _, p := range pos {
		if p == at {
			out = append(out, p)
		}
	}
	return out
}

// collectPostingPositions returns the positions of a doc's postings in a
// column.
func collectPostingPositions(postings []Posting, docID int64, col int) []int {
	var out []int
	for _, p := range postings {
		if p.DocID == docID && p.Column == col {
			out = append(out, p.Position)
		}
	}
	return out
}

// collectPrefixPositions returns the positions of a doc's postings whose term
// starts with the prefix, in a column.
func collectPrefixPositions(idx *InvertedIndex, prefix string, docID int64, col int) []int {
	var out []int
	for term, postings := range idx.index {
		if len(term) >= len(prefix) && term[:len(prefix)] == prefix {
			out = append(out, collectPostingPositions(postings, docID, col)...)
		}
	}
	return out
}

// tokenizeOffsets tokenizes text with the table's tokenizer, returning byte
// spans.
func tokenizeOffsets(tok Tokenizer, text string) []OffsetToken {
	if t, ok := tok.(interface {
		TokenizeOffsets(string) []OffsetToken
	}); ok {
		return t.TokenizeOffsets(text)
	}
	// Fallback: use plain Tokenize and synthesize spans (used for the
	// simple tokenizer's default path).
	return (&SimpleTokenizer{}).TokenizeOffsets(text)
}

// Snippet returns the snippet() result string for a document
// (fts3_snippet.c sqlite3Fts3Snippet): up to four fragments of up to nToken
// total tokens, each fragment containing phrase matches and fragments joined by
// zEllipsis. Matched tokens are wrapped in zStart/zEnd. When iCol >= 0 only
// that column is considered.
func (t *FTS3Table) Snippet(docID int64, phrases []MatchPhrase, zStart, zEnd, zEllipsis string, iCol, nToken int, override ...[]interface{}) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	doc := t.index.GetDoc(docID)
	if doc == nil || len(phrases) == 0 {
		return ""
	}
	nToken = clampSnippetTokenCount(nToken)
	if nToken == 0 {
		// nToken=0 yields an empty snippet (fts3_snippet.c: nFToken=0, no
		// fragment can cover any phrase — fts3snippet.test 4.1).
		return ""
	}

	// Find up to four fragments covering the query phrases (fts3_snippet.c
	// sqlite3Fts3Snippet: nSnippet=1..4, each iteration finds the best
	// fragment(s) of nFToken tokens; the fragment array is reused, so a later
	// nSnippet overwrites the earlier fragments. mSeen/mCovered are LOCAL to
	// each nSnippet iteration (the C resets them at line 1481-1482).
	var fragments []snippetFragment
	var finalNFToken int
	nSnippet := 0
	for {
		nSnippet++
		fragments = fragments[:0]
		nFToken := (nToken + nSnippet - 1) / nSnippet
		finalNFToken = nFToken
		var mSeen uint64    // phrases seen across all columns/fragments (this iteration)
		var mCovered uint64 // phrases covered by chosen fragments (this iteration)
		for iSnip := 0; iSnip < nSnippet; iSnip++ {
			iBestScore := -1
			var best snippetFragment
			for col := 0; col < len(t.columnNames); col++ {
				if iCol >= 0 && col != iCol {
					continue
				}
				frag, score, seen, ok := t.bestSnippetFragment(docID, col, phrases, nFToken, mCovered)
				if !ok {
					continue
				}
				mSeen |= seen
				if score > iBestScore {
					best = frag
					iBestScore = score
				}
			}
			fragments = append(fragments, best)
			mCovered |= best.covered
		}
		if mSeen == mCovered || nSnippet == 4 {
			break
		}
	}

	// Render the fragments (fts3_snippet.c fts3SnippetText). Each fragment is
	// finalNFToken tokens long (the per-fragment count of the last nSnippet
	// iteration).
	var b strings.Builder
	for i, frag := range fragments {
		isLast := i == len(fragments)-1
		var cols []interface{}
		if len(override) > 0 && override[0] != nil {
			cols = override[0]
		}
		t.snippetText(&b, doc, frag, i, isLast, finalNFToken, zStart, zEnd, zEllipsis, cols)
	}
	return b.String()
}

// snippetFragment is one selected snippet fragment (fts3_snippet.c
// SnippetFragment): the column, the first token position, the highlight bitmask
// (bits = token offsets within the fragment), and the bitmask of phrases
// covered.
type snippetFragment struct {
	col     int
	iPos    int
	hlmask  uint64
	covered uint64
}

// bestSnippetFragment finds the best nFToken-token fragment in one column
// (fts3_snippet.c fts3BestSnippet): the fragment with the highest score, where
// each phrase occurrence in the fragment scores +1 (+1000 for the first
// occurrence of a phrase not already covered). seen is the bitmask of phrases
// that occur in the column; ok is false when no phrase occurs there.
func (t *FTS3Table) bestSnippetFragment(docID int64, col int, phrases []MatchPhrase, nFToken int, covered uint64) (snippetFragment, int, uint64, bool) {
	positions := make([][]int, len(phrases))
	var seen uint64
	any := false
	for pi, mp := range phrases {
		positions[pi] = phrasePositionsInCol(t.index, docID, mp, col)
		sort.Ints(positions[pi])
		if len(positions[pi]) > 0 {
			seen |= uint64(1) << uint(pi%64)
			any = true
		}
	}
	if !any {
		return snippetFragment{}, -1, 0, false
	}
	iter := &snippetIter{
		phrases:  make([]*snippetPhrasePos, len(phrases)),
		nSnippet: nFToken,
		iCurrent: -1,
	}
	for pi := range phrases {
		iter.phrases[pi] = &snippetPhrasePos{
			positions: positions[pi],
			nToken:    phraseTokenCount(phrases[pi].Node),
		}
	}
	best := snippetFragment{col: col}
	iBestScore := -1
	for !iter.nextCandidate() {
		iPos, score, mCover, mHigh := iter.details(covered)
		if score > iBestScore {
			best = snippetFragment{col: col, iPos: iPos, hlmask: mHigh, covered: mCover}
			iBestScore = score
		}
	}
	return best, iBestScore, seen, true
}

// snippetPhrasePos is one phrase's position list and its head/tail iterators
// (fts3_snippet.c SnippetPhrase).
type snippetPhrasePos struct {
	positions  []int // sorted ascending
	head, tail int   // next index for the head/tail iterators
	nToken     int   // tokens in the phrase
}

// snippetIter iterates candidate snippet fragments (fts3_snippet.c SnippetIter).
type snippetIter struct {
	phrases  []*snippetPhrasePos
	nSnippet int
	iCurrent int // current fragment start token; -1 = uninitialized
}

// advanceIdx returns the first index i >= idx with positions[i] >= iNext
// (fts3_snippet.c fts3SnippetAdvance).
func advanceIdx(pos []int, idx, iNext int) int {
	for idx < len(pos) && pos[idx] < iNext {
		idx++
	}
	return idx
}

// nextCandidate advances the iterator to the next candidate fragment start.
// Returns true when no more candidates exist (fts3_snippet.c
// fts3SnippetNextCandidate).
func (it *snippetIter) nextCandidate() bool {
	if it.iCurrent < 0 {
		// First candidate always starts at offset 0; advance each phrase's
		// head to the first offset >= nSnippet for the next iteration.
		it.iCurrent = 0
		for _, p := range it.phrases {
			p.head = advanceIdx(p.positions, p.head, it.nSnippet)
		}
		return false
	}
	iEnd := int(^uint(0) >> 1)
	for _, p := range it.phrases {
		if p.head < len(p.positions) && p.positions[p.head] < iEnd {
			iEnd = p.positions[p.head]
		}
	}
	if iEnd == int(^uint(0)>>1) {
		return true
	}
	iStart := iEnd - it.nSnippet + 1
	it.iCurrent = iStart
	for _, p := range it.phrases {
		p.head = advanceIdx(p.positions, p.head, iEnd+1)
		p.tail = advanceIdx(p.positions, p.tail, iStart)
	}
	return false
}

// details scores the current candidate fragment (fts3_snippet.c
// fts3SnippetDetails): +1 per phrase occurrence in the fragment, +1000 for the
// first occurrence of a phrase not already covered. Returns the fragment start,
// score, covered-phrase bitmask, and highlight bitmask.
func (it *snippetIter) details(covered uint64) (int, int, uint64, uint64) {
	iStart := it.iCurrent
	iScore := 0
	var mCover, mHighlight uint64
	for i, p := range it.phrases {
		mPhrase := uint64(1) << uint(i%64)
		ti := p.tail
		for ti < len(p.positions) {
			pos := p.positions[ti]
			if pos < iStart {
				ti++
				continue
			}
			if pos >= iStart+it.nSnippet {
				break
			}
			mPos := uint64(1) << uint(pos-iStart)
			if (mCover|covered)&mPhrase != 0 {
				iScore++
			} else {
				iScore += 1000
			}
			mCover |= mPhrase
			for j := 0; j < p.nToken && j < it.nSnippet; j++ {
				if pos-iStart-j >= 0 {
					mHighlight |= mPos >> uint(j)
				}
			}
			ti++
		}
		// NOTE: p.tail is intentionally NOT advanced here — the C's
		// fts3SnippetDetails iterates a local copy (pCsr) and only
		// fts3SnippetNextCandidate advances the shared pTail. Advancing it
		// here would skip positions between candidates, under-scoring every
		// fragment after the first (fts3snippet2.test 3.1).
	}
	return iStart, iScore, mCover, mHighlight
}

// snippetText renders one fragment into b (fts3_snippet.c fts3SnippetText):
// the fragment's tokens from frag.iPos, highlighted per frag.hlmask, with a
// leading ellipsis when the fragment does not start at the document beginning
// (or a non-first fragment), and a trailing ellipsis for the last fragment when
// the document continues.
func (t *FTS3Table) snippetText(b *strings.Builder, doc *Document, frag snippetFragment, iFragment int, isLast bool, nToken int, zStart, zEnd, zEllipsis string, override []interface{}) {
	colText, ok := ftsDocColumnString(doc, frag.col)
	if override != nil && frag.col < len(override) {
		if s, sok := override[frag.col].(string); sok {
			colText, ok = s, true
		} else if override[frag.col] == nil {
			// A content=<table> row whose content was deleted after indexing:
			// SQLite renders an empty snippet (fts4content 2.4.2).
			return
		}
	}
	if !ok {
		return
	}
	tokens := tokenizeOffsets(t.tokenizer, colText)
	nSnippet := nToken
	iPos, hlmask := frag.iPos, frag.hlmask
	iPos, hlmask = snippetShift(tokens, nSnippet, iPos, hlmask)

	iBegin := 0
	if iPos < len(tokens) {
		iBegin = tokens[iPos].Start
	}
	if iPos > 0 || iFragment > 0 {
		b.WriteString(zEllipsis)
	} else if iBegin > 0 {
		b.WriteString(colText[:iBegin])
	}

	iEnd := 0
	for ti := iPos; ti < len(tokens); ti++ {
		if ti >= iPos+nSnippet {
			if isLast {
				b.WriteString(zEllipsis)
			}
			break
		}
		tok := tokens[ti]
		isHighlight := ti-iPos < 64 && (hlmask&(uint64(1)<<uint(ti-iPos))) != 0
		if ti > iPos {
			b.WriteString(colText[iEnd:tok.Start])
		}
		if isHighlight {
			b.WriteString(zStart)
		}
		b.WriteString(colText[tok.Start:tok.End])
		if isHighlight {
			b.WriteString(zEnd)
		}
		iEnd = tok.End
	}
	// If the fragment's last token is also the column's last token, append
	// the punctuation between it and the document end (fts3_snippet.c
	// fts3SnippetText: the tokenizer reaches SQLITE_DONE and appends
	// &zDoc[iEnd]).
	if iEnd > 0 && iEnd < len(colText) && iPos+nSnippet >= len(tokens) {
		b.WriteString(colText[iEnd:])
	}
}

// snippetShift shifts a fragment right to center its highlighted tokens
// (fts3_snippet.c fts3SnippetShift). Returns the adjusted start position and
// highlight mask.
func snippetShift(tokens []OffsetToken, nSnippet, iPos int, hlmask uint64) (int, uint64) {
	if hlmask == 0 {
		return iPos, hlmask
	}
	nLeft := 0
	for nLeft < 64 && (hlmask&(uint64(1)<<uint(nLeft))) == 0 {
		nLeft++
	}
	nRight := 0
	for nRight < 64 && nSnippet-1-nRight >= 0 && (hlmask&(uint64(1)<<uint(nSnippet-1-nRight))) == 0 {
		nRight++
	}
	nDesired := (nLeft - nRight) / 2
	if nDesired > 0 {
		// Count tokens available to the right of iPos.
		iCurrent := 0
		for iCurrent < nSnippet+nDesired && iPos+iCurrent < len(tokens) {
			iCurrent++
		}
		nShift := iCurrent - nSnippet
		if iPos+iCurrent >= len(tokens) {
			nShift++ // reached the end of the column (SQLITE_DONE)
		}
		if nShift > 0 {
			iPos += nShift
			hlmask >>= uint(nShift)
		}
	}
	return iPos, hlmask
}

// clampSnippetTokenCount bounds the snippet window to SQLite's +/-64 token
// limit (fts3_snippet.c sqlite3Fts3Snippet clamps nToken to 64). Zero stays
// zero (the caller returns an empty snippet for it).
func clampSnippetTokenCount(nToken int) int {
	if nToken > 64 {
		nToken = 64
	}
	if nToken < -64 {
		nToken = -64
	}
	if nToken < 0 {
		nToken = -nToken
	}
	return nToken
}

// ftsDocColumnString renders a stored FTS column value as the string the
// tokenizer would see: a string as-is, a []byte blob as its raw bytes, and an
// integer/float via its canonical text form (ftsColumnString's %v). ok is
// false for NULL (nil) and other non-tokenizable values. Like ftsColumnString,
// the result is truncated at the first NUL byte (SQLite's tokenizer is invoked
// with strlen semantics).
func ftsDocColumnString(doc *Document, col int) (string, bool) {
	var s string
	switch v := doc.Columns[col].(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	case int64:
		s = fmt.Sprintf("%d", v)
	case int:
		s = fmt.Sprintf("%d", v)
	case float64:
		s = fmt.Sprintf("%v", v)
	default:
		return "", false
	}
	if idx := strings.IndexByte(s, 0); idx >= 0 {
		s = s[:idx]
	}
	return s, true
}

// nearParticipatingPositions returns, for one column, the positions of a
// NEAR operand phrase that participate in the NEAR match for docID — the
// offsets() analogue of fts3EvalNearTrim's doclist trimming. side is 0 for
// the NEAR's left operand, 1 for the right.
func nearParticipatingPositions(idx *InvertedIndex, docID int64, near *NearNode, side int, col int) []int {
	leftPos, leftLen := near.phrasePositions(idx, docID, near.Left)
	rightPos, rightLen := near.phrasePositions(idx, docID, near.Right)
	if len(leftPos) == 0 || len(rightPos) == 0 {
		return nil
	}
	hit := markNearPairings(leftPos, rightPos, near.Distance, leftLen, rightLen)
	mine := leftPos
	off := 0
	if side == 1 {
		mine = rightPos
		off = len(leftPos)
	}
	var out []int
	for i, p := range mine {
		if hit[off+i] && p.col == col {
			out = append(out, p.pos)
		}
	}
	return out
}
