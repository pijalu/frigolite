package fts5

import (
	"fmt"
	"math"
	"sort"
)

// This file ports fts5_aux.c: the auxiliary functions bm25(), highlight() and
// snippet() evaluated against a prepared query (the Fts5Context stand-in) and
// one document. The per-row phrase-instance machinery mirrors fts5_main.c's
// fts5CacheInstArray (instances ordered by column, then token offset, then
// phrase) and fts5_expr.c's NEAR poslist trimming (an instance is reported
// only when it belongs to a NEAR window that matched, and a node that does
// not match contributes no instances).

// Inst is one phrase instance in a document (the aux API's xInst triple:
// phrase number, column, first-token offset).
type Inst struct {
	Phrase int
	Col    int
	Offset int
}

// AuxQuery is the prepared per-query state auxiliary functions evaluate
// against (fts5_aux.c's Fts5Context): the parsed expression, the query-order
// phrase list and cached per-phrase row hit counts for bm25's IDF values.
type AuxQuery struct {
	t       *Table
	root    queryNode
	phrases []*phraseNode
	hits    []int
	// special marks a special-query cursor ('*id'/'*reads'): every aux
	// function call on its rows fails like C's fts5ApiInvoke on an
	// FTS5_PLAN_SPECIAL cursor. specialValue records the cursor's value so
	// the error message carries the hidden column's value.
	special      bool
	specialValue int64
	// auxdata holds the statement-scoped integer auxdata slots the
	// test-support functions use (xSetAuxdataInt/xGetAuxdataInt parity).
	auxdata map[string]int64
}

// NewSpecialAux builds the aux context of a special-query cursor: aux
// function calls must fail with "no such cursor: <value>" where value is the
// special query's result (the cursor's hidden-column value).
func (t *Table) NewSpecialAux(value int64) *AuxQuery {
	return &AuxQuery{t: t, root: eofNode{}, special: true, specialValue: value}
}

// IsSpecial reports whether the context belongs to a special-query cursor.
func (aq *AuxQuery) IsSpecial() bool { return aq.special }

// SpecialValue returns the special query's value for the "no such cursor"
// message (fts5_misc.h: fts5ApiInvoke prints the hidden column's value).
func (aq *AuxQuery) SpecialValue() int64 { return aq.specialValue }

// globalCursorID is the process-wide fts5 cursor counter every vtab cursor
// receives at open (Fts5Global.iNextCursorId): '*id' reports it.
var globalCursorID int64

// NextCursorID allocates the next cursor id (fts5Filter's cursor
// registration). C numbers cursors process-wide in open order.
func (t *Table) NextCursorID() int64 {
	globalCursorID++
	return globalCursorID
}

// PrepareAux parses a MATCH query for auxiliary-function evaluation. col
// restricts the query to one user column (-1 for the whole table).
func (t *Table) PrepareAux(query string, col int) (*AuxQuery, error) {
	node, phrases, err := parseQueryAll(t, query)
	if err != nil {
		return nil, err
	}
	if col >= 0 {
		node = applyColset(node, []int{col})
	}
	return &AuxQuery{t: t, root: node, phrases: phrases, hits: make([]int, len(phrases))}, nil
}

// AuxConstraint is one MATCH constraint of a statement (the query text plus
// the user column it is restricted to, -1 for the whole table).
type AuxConstraint struct {
	Query string
	Col   int
}

// PrepareAuxMulti parses several MATCH constraints into ONE combined aux
// context (C's xFilter merges every MATCH constraint on the table into a
// single query expression: the phrases concatenate in constraint order and
// the combined tree ANDs the constraint nodes with their per-constraint
// column filters applied).
func (t *Table) PrepareAuxMulti(constraints []AuxConstraint) (*AuxQuery, error) {
	if len(constraints) == 1 {
		return t.PrepareAux(constraints[0].Query, constraints[0].Col)
	}
	var kids []queryNode
	var phrases []*phraseNode
	for _, c := range constraints {
		node, phs, err := parseQueryAll(t, c.Query)
		if err != nil {
			return nil, err
		}
		if c.Col >= 0 {
			node = applyColset(node, []int{c.Col})
		}
		// The phrase idx values are per-parse; renumber to the combined
		// order so xInst phrase numbers match the merged query.
		for _, ph := range phs {
			ph.idx = len(phrases)
			phrases = append(phrases, ph)
		}
		kids = append(kids, node)
	}
	combined, err := combineAndNodes(kids)
	if err != nil {
		return nil, err
	}
	return &AuxQuery{t: t, root: combined, phrases: phrases, hits: make([]int, len(phrases))}, nil
}

// NewScanAux builds the aux context of a scan without a MATCH constraint: the
// aux functions see zero instances (C's full-scan cursor: bm25() = -0.0,
// highlight()/snippet() return the text unchanged).
func (t *Table) NewScanAux() *AuxQuery {
	return &AuxQuery{t: t, root: eofNode{}}
}

// PhraseCount returns the number of phrases in the query (xPhraseCount).
func (aq *AuxQuery) PhraseCount() int { return len(aq.phrases) }

// PhraseSize returns phrase i's token count, or 0 when i is out of range
// (xPhraseSize: the out-of-range form is defined to return 0).
func (aq *AuxQuery) PhraseSize(i int) int {
	if i < 0 || i >= len(aq.phrases) {
		return 0
	}
	return aq.phrases[i].size()
}

// RowInstances returns the document's phrase instances in xInst order
// (column, offset, phrase) with the NEAR/pruning semantics applied. A
// contentless table under a non-full detail mode yields no instances:
// fts5CsrPoslist returns an empty poslist when there is no content to
// re-derive per-instance positions from (fts5_main.c fts5CsrPoslist's
// contentless early return; fts5misc 25.0).
func (aq *AuxQuery) RowInstances(rowid int64) []Inst {
	if aq.t.cfg.Contentless() && !aq.t.cfg.DetailFull() {
		return nil
	}
	matched, insts := aq.evalNode(aq.root, rowid)
	if !matched {
		return nil
	}
	sort.Slice(insts, func(i, j int) bool {
		a, b := insts[i], insts[j]
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		if a.Offset != b.Offset {
			return a.Offset < b.Offset
		}
		return a.Phrase < b.Phrase
	})
	return insts
}

// evalNode evaluates one node for a document, returning whether the document
// matches and the phrase instances the node contributes (empty for a
// non-matching node, mirroring fts5ExprNodeZeroPoslist).
func (aq *AuxQuery) evalNode(n queryNode, rowid int64) (bool, []Inst) {
	switch x := n.(type) {
	case eofNode:
		return false, nil
	case stringNode:
		return aq.evalStringNode(x, rowid)
	case andNode:
		var all []Inst
		for _, c := range x.children {
			m, insts := aq.evalNode(c, rowid)
			if !m {
				return false, nil
			}
			all = append(all, insts...)
		}
		return true, all
	case orNode:
		var all []Inst
		any := false
		for _, c := range x.children {
			m, insts := aq.evalNode(c, rowid)
			if m {
				any = true
				all = append(all, insts...)
			}
		}
		return any, all
	case notNode:
		lm, linsts := aq.evalNode(x.l, rowid)
		if !lm {
			return false, nil
		}
		if rm, _ := aq.evalNode(x.r, rowid); rm {
			return false, nil
		}
		return true, linsts
	}
	return false, nil
}

// evalStringNode evaluates a near cluster for one document: single phrases
// report every instance; NEAR clusters report only the instances inside
// matching windows (fts5ExprNearIsMatch's trimmed poslists).
func (aq *AuxQuery) evalStringNode(n stringNode, rowid int64) (bool, []Inst) {
	doc := aq.t.ix.Doc(rowid)
	if doc == nil {
		return false, nil
	}
	if len(n.phrases) == 1 {
		insts := aq.t.phraseInstances(n.phrases[0], doc)
		return len(insts) > 0, insts
	}
	return aq.evalNearCluster(n, doc)
}

// evalNearCluster evaluates a multi-phrase NEAR cluster: per column, only the
// instances inside matching windows are reported (fts5ExprNearIsMatch's
// trimmed poslists).
func (aq *AuxQuery) evalNearCluster(n stringNode, doc *docEntry) (bool, []Inst) {
	byCol := aq.phraseInstancesByColumn(n.phrases, doc)
	var out []Inst
	for _, col := range phraseCols(aq.t, n.phrases[0]) {
		out = append(out, aq.columnWindowInsts(n, byCol, col)...)
	}
	return len(out) > 0, out
}

// phraseInstancesByColumn buckets each phrase's instance offsets by column.
func (aq *AuxQuery) phraseInstancesByColumn(phrases []*phraseNode, doc *docEntry) []map[int][]int {
	byCol := make([]map[int][]int, len(phrases))
	for i, ph := range phrases {
		for _, in := range aq.t.phraseInstances(ph, doc) {
			if byCol[i] == nil {
				byCol[i] = make(map[int][]int)
			}
			byCol[i][in.Col] = append(byCol[i][in.Col], in.Offset)
		}
	}
	return byCol
}

// columnWindowInsts emits one column's kept instances (nearWindowCollect's
// trimmed poslists); a column where some phrase has no instance contributes
// nothing.
func (aq *AuxQuery) columnWindowInsts(n stringNode, byCol []map[int][]int, col int) []Inst {
	instances := make([][]int, len(n.phrases))
	for i := range n.phrases {
		instances[i] = byCol[i][col]
		if len(instances[i]) == 0 {
			return nil
		}
	}
	_, kept := nearWindowCollect(instances, phraseSizes(n.phrases), n.window)
	var out []Inst
	for i := range n.phrases {
		for _, pos := range kept[i] {
			out = append(out, Inst{Phrase: i, Col: col, Offset: pos})
		}
	}
	return out
}

// phraseInstances returns one phrase's instances in a document (colset
// respected, ^-anchoring enforced).
func (t *Table) phraseInstances(ph *phraseNode, doc *docEntry) []Inst {
	var out []Inst
	for _, col := range phraseCols(t, ph) {
		if col >= len(doc.cols) {
			continue
		}
		tokens := doc.cols[col]
		for p := 0; p+len(ph.terms) <= len(tokens); p++ {
			if ph.first && p != 0 {
				break
			}
			if phraseMatches(ph.terms, tokens, p) {
				out = append(out, Inst{Phrase: ph.idx, Col: col, Offset: p})
			}
		}
	}
	return out
}

// nearWindowScan is fts5ExprNearIsMatch's scan state: per-phrase cursors into
// the instance lists and the kept (trimmed) positions.
type nearWindowScan struct {
	instances [][]int
	sizes     []int
	window    int
	idx       []int
	kept      [][]int
	iMax      int
}

// nearWindowCollect ports fts5ExprNearIsMatch: it finds the matching windows
// and returns, per phrase, the instances that belong to at least one window
// (the trimmed output poslists).
func nearWindowCollect(instances [][]int, sizes []int, window int) (bool, [][]int) {
	w := &nearWindowScan{
		instances: instances,
		sizes:     sizes,
		window:    window,
		idx:       make([]int, len(instances)),
		kept:      make([][]int, len(instances)),
	}
	w.collect()
	return len(w.kept[0]) > 0, w.kept
}

// collect runs the advancing-anchors loop until a phrase's instances are
// exhausted or no cursor can advance past the recorded window
// (fts5ExprNearIsMatch).
func (w *nearWindowScan) collect() {
	w.iMax = w.instances[0][0]
	for w.step() {
	}
}

// step performs one loop iteration; false ends the scan (a phrase exhausted
// or, after a recorded window, no cursor left to advance).
func (w *nearWindowScan) step() bool {
	matched, ok := w.advanceCursors()
	if !ok {
		return false
	}
	if !matched {
		return true // a cursor moved the anchor past iMax: re-check the window
	}
	w.recordWindow()
	return w.advanceSmallest()
}

// advanceCursors moves each cursor up to its window floor (iMax - size -
// window); ok=false when a phrase's instances are exhausted. matched reports
// whether every cursor already sits at or above its floor within the window.
func (w *nearWindowScan) advanceCursors() (matched, ok bool) {
	matched = true
	for i := range w.instances {
		iMin := w.iMax - w.sizes[i] - w.window
		for w.instances[i][w.idx[i]] < iMin {
			w.idx[i]++
			if w.idx[i] >= len(w.instances[i]) {
				return matched, false
			}
		}
		if p := w.instances[i][w.idx[i]]; p > w.iMax {
			w.iMax = p
			matched = false
		}
	}
	return matched, true
}

// recordWindow appends each phrase's current position to its kept list,
// deduplicated against the trailing entry (the trimmed poslist).
func (w *nearWindowScan) recordWindow() {
	for i := range w.instances {
		pos := w.instances[i][w.idx[i]]
		k := w.kept[i]
		if len(k) == 0 || k[len(k)-1] != pos {
			w.kept[i] = append(k, pos)
		}
	}
}

// advanceSmallest steps the cursor with the smallest lookahead past the
// recorded window; false when every cursor is at its last instance (no
// further window can match).
func (w *nearWindowScan) advanceSmallest() bool {
	const inf = math.MaxInt64
	best, bestAt := inf, -1
	for i := range w.instances {
		if la := w.lookahead(i); la < best {
			best, bestAt = la, i
		}
	}
	if bestAt < 0 {
		return false
	}
	w.idx[bestAt]++
	return true
}

// lookahead returns the next instance position of cursor i (the sentinel
// maximum at the end of the list).
func (w *nearWindowScan) lookahead(i int) int {
	if w.idx[i]+1 < len(w.instances[i]) {
		return w.instances[i][w.idx[i]+1]
	}
	return math.MaxInt64
}

// --- bm25 (fts5_aux.c fts5Bm25Function) ---

// bm25 constants (fts5_aux.c fts5Bm25Function).
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Bm25 evaluates the bm25() auxiliary function for one document with the
// given per-column weights (missing weights default to 1.0). The result is
// negated (smaller is more relevant) exactly like C.
func (aq *AuxQuery) Bm25(rowid int64, weights []float64) float64 {
	t := aq.t
	nRow := t.ix.NumDocs()
	var avgdl float64
	if nRow > 0 {
		avgdl = float64(t.totalTokens()) / float64(nRow)
	}
	aFreq := make([]float64, len(aq.phrases))
	for _, inst := range aq.RowInstances(rowid) {
		w := 1.0
		if inst.Col < len(weights) {
			w = weights[inst.Col]
		}
		aFreq[inst.Phrase] += w
	}
	D := float64(t.docTokenCount(rowid))
	if avgdl <= 0 {
		avgdl = 1 // unreachable for tables with matching rows; avoids NaN
	}
	score := 0.0
	for i := range aq.phrases {
		idf := aq.phraseIDF(i, nRow)
		score += idf * (aFreq[i] * (bm25K1 + 1.0)) / (aFreq[i] + bm25K1*(1.0-bm25B+bm25B*D/avgdl))
	}
	return -1.0 * score
}

// phraseIDF computes (and caches) phrase i's inverse document frequency:
// log((N - nHit + 0.5) / (nHit + 0.5)), clamped to a 1e-6 floor when
// non-positive (fts5Bm25GetData).
func (aq *AuxQuery) phraseIDF(i, nRow int) float64 {
	nHit := aq.hits[i]
	if nHit == 0 {
		nHit = aq.t.phraseRowHits(aq.phrases[i])
		aq.hits[i] = nHit
	}
	idf := math.Log((float64(nRow) - float64(nHit) + 0.5) / (float64(nHit) + 0.5))
	if idf <= 0.0 {
		idf = 1e-6
	}
	return idf
}

// phraseRowHits counts the table rows matching one phrase alone (the
// xQueryPhrase count behind bm25's IDF).
func (t *Table) phraseRowHits(ph *phraseNode) int {
	set, err := t.evalPhrase(ph)
	if err != nil {
		return 0
	}
	return len(set)
}

// totalTokens returns the table's total token count (xColumnTotalSize(-1)).
func (t *Table) totalTokens() int64 {
	var out int64
	for _, n := range t.ix.nTokensPerCol {
		out += n
	}
	return out
}

// docTokenCount returns one document's total token count (xColumnSize(-1)).
func (t *Table) docTokenCount(rowid int64) int {
	doc := t.ix.Doc(rowid)
	if doc == nil {
		return 0
	}
	n := 0
	for _, toks := range doc.cols {
		n += len(toks)
	}
	return n
}

// columnTokenCount returns one document-column's token count
// (xColumnSize(iCol)).
func (t *Table) columnTokenCount(rowid int64, iCol int) int {
	doc := t.ix.Doc(rowid)
	if doc == nil || iCol < 0 || iCol >= len(doc.cols) {
		return 0
	}
	return len(doc.cols[iCol])
}

// columnText returns one document-column's text (xColumnText). ok=false means
// a NULL column (no stored value); a missing column index is SQLITE_RANGE.
func (t *Table) columnText(rowid int64, iCol int) (text string, ok bool, err error) {
	if iCol < 0 || iCol >= len(t.cfg.Columns) {
		return "", false, &ColumnRangeError{}
	}
	values, err := t.DocValues(rowid)
	if err != nil {
		return "", false, err
	}
	var v interface{}
	if iCol < len(values) {
		v = values[iCol]
	}
	if v == nil {
		return "", false, nil
	}
	switch x := v.(type) {
	case string:
		return x, true, nil
	case []byte:
		return string(x), true, nil
	default:
		return textValue(x), true, nil
	}
}

// textValue renders a stored value as text (sqlite3_value_text parity).
func textValue(v interface{}) string { return fmt.Sprintf("%v", v) }

// ColumnRangeError is SQLITE_RANGE ("column index out of range").
type ColumnRangeError struct{}

func (e *ColumnRangeError) Error() string { return "column index out of range" }

// --- highlight (fts5_aux.c fts5HighlightFunction) ---

// coInst is one coalesced phrase instance: the first and last token of a
// maximal run of overlapping instances (CInstIter).
type coInst struct{ start, end int }

// coalesceInstances merges a column's instances into maximal overlapping runs
// (fts5CInstIterNext's grouping, applied upfront: instances arrive in offset
// order).
func coalesceInstances(insts []Inst, aq *AuxQuery, iCol int) []coInst {
	var out []coInst
	for _, in := range insts {
		if in.Col != iCol {
			continue
		}
		end := in.Offset + aq.phrases[in.Phrase].size() - 1
		if n := len(out); n > 0 && in.Offset <= out[n-1].end {
			if end > out[n-1].end {
				out[n-1].end = end
			}
			continue
		}
		out = append(out, coInst{start: in.Offset, end: end})
	}
	return out
}

// highlightState is the HighlightContext of fts5_aux.c.
type highlightState struct {
	iRangeStart int
	iRangeEnd   int // -1: no range (plain highlight)
	zOpen       string
	zClose      string
	zIn         string
	iter        []coInst
	iterPos     int
	iStart      int // current coalesced instance start (-1 when exhausted)
	iEnd        int
	iPos        int
	iOff        int
	bOpen       bool
	zOut        []byte
}

// iterNext advances the coalesced-instance iterator (fts5CInstIterNext).
func (p *highlightState) iterNext() {
	p.iStart, p.iEnd = -1, -1
	if p.iterPos < len(p.iter) {
		p.iStart = p.iter[p.iterPos].start
		p.iEnd = p.iter[p.iterPos].end
		p.iterPos++
	}
}

// append appends text to the output buffer (fts5HighlightAppend).
func (p *highlightState) append(s string) { p.zOut = append(p.zOut, s...) }

// token processes one token (fts5HighlightCb). startOff/endOff are the token's
// byte offsets in the column text.
func (p *highlightState) token(startOff, endOff int) {
	iPos := p.iPos
	p.iPos++
	if !p.rangeFilter(iPos, startOff) {
		return
	}
	p.closeBefore(startOff, iPos)
	p.openAt(iPos, startOff)
	p.closeAt(iPos, endOff)
	p.atRangeEnd(iPos, endOff)
}

// rangeFilter applies the [iRangeStart, iRangeEnd] token window: it reports
// whether the token is emitted, updating iOff at the window start
// (fts5HighlightCb's range branch).
func (p *highlightState) rangeFilter(iPos, startOff int) bool {
	if p.iRangeEnd < 0 {
		return true
	}
	if iPos < p.iRangeStart || iPos > p.iRangeEnd {
		return false
	}
	if p.iRangeStart != 0 && iPos == p.iRangeStart {
		p.iOff = startOff
	}
	return true
}

// closeBefore closes an open highlight run that ended before this token.
func (p *highlightState) closeBefore(startOff, iPos int) {
	if p.bOpen && (iPos <= p.iStart || p.iStart < 0) && startOff > p.iOff {
		p.append(p.zClose)
		p.bOpen = false
	}
}

// openAt opens the highlight at a coalesced instance start.
func (p *highlightState) openAt(iPos, startOff int) {
	if iPos == p.iStart && !p.bOpen {
		p.append(p.zIn[p.iOff:startOff])
		p.append(p.zOpen)
		p.iOff = startOff
		p.bOpen = true
	}
}

// closeAt closes the coalesced instance at its end token.
func (p *highlightState) closeAt(iPos, endOff int) {
	if iPos == p.iEnd {
		if !p.bOpen {
			p.append(p.zOpen)
			p.bOpen = true
		}
		p.append(p.zIn[p.iOff:endOff])
		p.iOff = endOff
		p.iterNext()
	}
}

// atRangeEnd flushes the trailing text at the range's last token
// (fts5HighlightCb's range-end branch).
func (p *highlightState) atRangeEnd(iPos, endOff int) {
	if iPos != p.iRangeEnd {
		return
	}
	if p.bOpen {
		if p.iStart >= 0 && iPos >= p.iStart {
			p.append(p.zIn[p.iOff:endOff])
			p.iOff = endOff
		}
		p.append(p.zClose)
		p.bOpen = false
	}
	p.append(p.zIn[p.iOff:endOff])
	p.iOff = endOff
}

// Highlight evaluates highlight(t1, iCol, zOpen, zClose) for one document
// (fts5HighlightFunction). A NULL column yields an SQL NULL (nil); an
// out-of-range column index yields the empty string.
func (aq *AuxQuery) Highlight(rowid int64, iCol int, zOpen, zClose string) (interface{}, error) {
	t := aq.t
	text, ok, err := t.columnText(rowid, iCol)
	if err != nil {
		switch err.(type) {
		case *ColumnRangeError:
			return "", nil
		default:
			return nil, err
		}
	}
	if !ok {
		return nil, nil
	}
	p := &highlightState{
		iRangeStart: 0,
		iRangeEnd:   -1,
		zOpen:       zOpen,
		zClose:      zClose,
		zIn:         text,
		iStart:      -1,
		iEnd:        -1,
	}
	p.iter = coalesceInstances(aq.RowInstances(rowid), aq, iCol)
	p.iterNext()
	for _, tok := range t.tokenizeFor(text) {
		p.token(tok.Start, tok.End)
	}
	if p.bOpen {
		p.append(p.zClose)
	}
	p.append(p.zIn[p.iOff:])
	return string(p.zOut), nil
}

// --- snippet (fts5_aux.c fts5SnippetFunction) ---

// snippetScore scores the token window starting at iPos in column iCol: the
// first occurrence of each query phrase scores 1000, later ones 1
// (fts5SnippetScore). When wantAdj is set the adjusted window start is
// returned, clamped to the column.
func (aq *AuxQuery) snippetScore(insts []Inst, nDocsize, iCol, iPos, nToken int, aSeen []bool, wantAdj bool) (int, int) {
	nScore := 0
	iFirst := -1
	iLast := 0
	iEnd := iPos + nToken
	for _, in := range insts {
		if in.Col != iCol || in.Offset < iPos || in.Offset >= iEnd {
			continue
		}
		if aSeen[in.Phrase] {
			nScore++
		} else {
			nScore += 1000
		}
		aSeen[in.Phrase] = true
		if iFirst < 0 {
			iFirst = in.Offset
		}
		iLast = in.Offset + aq.phrases[in.Phrase].size()
	}
	if !wantAdj {
		return nScore, 0
	}
	iAdj := iFirst - (nToken-(iLast-iFirst))/2
	if iAdj+nToken > nDocsize {
		iAdj = nDocsize - nToken
	}
	if iAdj < 0 {
		iAdj = 0
	}
	return nScore, iAdj
}

// sentenceStarts finds the first-token-of-a-sentence positions of a column's
// text (fts5SentenceFinderCb): token 0, and every token whose preceding
// non-whitespace byte is '.' or ':'.
func sentenceStarts(text string, toks []Token) []int {
	var out []int
	for iPos, tok := range toks {
		if iPos == 0 {
			out = append(out, 0)
			continue
		}
		i := tok.Start - 1
		c := byte(0)
		for ; i >= 0; i-- {
			c = text[i]
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				break
			}
		}
		if i != tok.Start-1 && (c == '.' || c == ':') {
			out = append(out, iPos)
		}
	}
	return out
}

// snippetCandidate is one scored candidate window (fts5SnippetFunction's
// iBestCol/iBestStart/nColSize tracking).
type snippetCandidate struct {
	col, start, size, score int
}

// bestSnippetWindow scans every candidate column and instance offset for the
// highest-scoring token window; ties keep the first candidate in scan order
// (column ascending, instance offset ascending, offset window before
// sentence window — the C loop's update order).
func (aq *AuxQuery) bestSnippetWindow(insts []Inst, rowid int64, iCol, nToken, nCol int) snippetCandidate {
	best := snippetCandidate{}
	if iCol >= 0 {
		best.col = iCol
	}
	for i := 0; i < nCol; i++ {
		if iCol >= 0 && iCol != i {
			continue
		}
		for _, c := range aq.scoreColumnWindows(insts, rowid, i, nToken) {
			if c.score > best.score {
				best = c
			}
		}
	}
	return best
}

// scoreColumnWindows scores column i's candidate windows: every instance's
// offset window, plus each instance's preceding sentence-start window with
// its bonus (fts5SnippetFunction's inner loops).
func (aq *AuxQuery) scoreColumnWindows(insts []Inst, rowid int64, i, nToken int) []snippetCandidate {
	nDocsize := aq.t.columnTokenCount(rowid, i)
	firsts := aq.columnSentenceStarts(rowid, i)
	var cands []snippetCandidate
	for _, in := range insts {
		if in.Col != i {
			continue
		}
		cands = append(cands, aq.instanceCandidates(insts, i, in.Offset, nToken, nDocsize, firsts)...)
	}
	return cands
}

// instanceCandidates scores one instance's offset window and, when sentence
// starts exist and the column is longer than the window, the sentence window
// before the instance (fts5SnippetFunction's per-instance block).
func (aq *AuxQuery) instanceCandidates(insts []Inst, i, io, nToken, nDocsize int, firsts []int) []snippetCandidate {
	nScore, iAdj := aq.scoreWindow(insts, nDocsize, i, io, nToken, true)
	cands := []snippetCandidate{{col: i, start: iAdj, size: nDocsize, score: nScore}}
	if len(firsts) == 0 || nDocsize <= nToken {
		return cands
	}
	if start, ok := precedingSentenceStart(firsts, io); ok {
		cands = append(cands, aq.sentenceCandidate(insts, i, start, nToken, nDocsize))
	}
	return cands
}

// precedingSentenceStart returns the sentence start at or before io (the
// fts5SentenceFinder probe); ok=false when it is not strictly before io.
func precedingSentenceStart(firsts []int, io int) (int, bool) {
	jj := 0
	for jj < len(firsts)-1 && firsts[jj+1] <= io {
		jj++
	}
	if firsts[jj] >= io {
		return 0, false
	}
	return firsts[jj], true
}

// sentenceCandidate scores one sentence-start window with its bonus (120 for
// a column-initial window, 100 otherwise).
func (aq *AuxQuery) sentenceCandidate(insts []Inst, i, start, nToken, nDocsize int) snippetCandidate {
	nScore, _ := aq.scoreWindow(insts, nDocsize, i, start, nToken, false)
	if start == 0 {
		nScore += 120
	} else {
		nScore += 100
	}
	return snippetCandidate{col: i, start: start, size: nDocsize, score: nScore}
}

// scoreWindow wraps snippetScore with a fresh seen mask (each candidate
// scores independently).
func (aq *AuxQuery) scoreWindow(insts []Inst, nDocsize, iCol, iPos, nToken int, wantAdj bool) (int, int) {
	return aq.snippetScore(insts, nDocsize, iCol, iPos, nToken, make([]bool, len(aq.phrases)), wantAdj)
}

// columnSentenceStarts returns column i's sentence starts (empty when the
// column has no stored text).
func (aq *AuxQuery) columnSentenceStarts(rowid int64, i int) []int {
	t := aq.t
	text, ok, err := t.columnText(rowid, i)
	if err != nil || !ok {
		return nil
	}
	return sentenceStarts(text, t.tokenizeFor(text))
}

// Snippet evaluates snippet(t1, iCol, zOpen, zClose, zEllips, nToken) for one
// document (fts5SnippetFunction): the highest-scoring token window is
// selected (per-instance scores plus sentence-start bonuses) and rendered
// through the highlight walk in range mode.
func (aq *AuxQuery) Snippet(rowid int64, iCol int, zOpen, zClose, zEllips string, nToken int) (interface{}, error) {
	t := aq.t
	nCol := len(t.cfg.Columns)
	insts := aq.RowInstances(rowid)
	best := aq.bestSnippetWindow(insts, rowid, iCol, nToken, nCol)
	if best.col < 0 || best.col >= nCol {
		// xColumnText(iBestCol) reports SQLITE_RANGE for an out-of-range best
		// column; the function fails (fts5SnippetFunction's rc path).
		return nil, &ColumnRangeError{}
	}
	iBestStart := best.start
	nColSize := best.size
	if nColSize == 0 {
		nColSize = t.columnTokenCount(rowid, best.col)
	}
	text, ok, err := t.columnText(rowid, best.col)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	p := &highlightState{
		iRangeStart: iBestStart,
		iRangeEnd:   iBestStart + nToken - 1,
		zOpen:       zOpen,
		zClose:      zClose,
		zIn:         text,
		iStart:      -1,
		iEnd:        -1,
	}
	p.iter = coalesceInstances(insts, aq, best.col)
	p.iterNext()
	if iBestStart > 0 {
		p.append(zEllips)
	}
	for p.iStart >= 0 && p.iStart < iBestStart {
		p.iterNext()
	}
	for _, tok := range t.tokenizeFor(text) {
		p.token(tok.Start, tok.End)
	}
	if p.bOpen {
		p.append(p.zClose)
	}
	if p.iRangeEnd >= nColSize-1 {
		p.append(p.zIn[p.iOff:])
	} else {
		p.append(zEllips)
	}
	return string(p.zOut), nil
}
