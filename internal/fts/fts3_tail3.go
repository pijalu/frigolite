package fts

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/vtab"
)

func (t *FTS3Table) tokenStatsLocked() (int64, []int64) {
	ids := t.index.AllDocIDs()
	nCol := len(t.columnNames)
	totals := make([]int64, nCol)
	for _, docID := range ids {
		ds := t.index.DocSizeInfo(docID)
		if ds == nil {
			continue
		}
		for i := 0; i < nCol && i < len(ds.Counts); i++ {
			totals[i] += int64(ds.Counts[i])
		}
	}
	return int64(len(ids)), totals
}

// DocSize returns the per-column token counts and the total text bytes of one
// document (fts3.c fts3PendingTermsAdd fills aSz[iCol] with the token count of
// column iCol and aSz[nColumn] with the sum of the column byte lengths).
// Used to maintain the FTS4 %_docsize and %_stat shadow tables. Uses the
// cached size computed at insert.
func (t *FTS3Table) DocSize(docID int64) ([]int, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	nCol := len(t.columnNames)
	ds := t.index.DocSizeInfo(docID)
	if ds == nil {
		return make([]int, nCol), 0
	}
	counts := make([]int, nCol)
	copy(counts, ds.Counts)
	return counts, ds.TotalBytes
}

// MatchInfoX computes the 'x' format values for one phrase of a MATCH query:
// for each column, [local hits in the current row, global occurrences,
// global rows] (fts3_snippet.c fts3ExprLocalHitsCb / fts3ExprGlobalHitsCb).
// The current row is identified by docID. The phrase's scope (itself or its
// enclosing NEAR) bounds the global statistics.
func (t *FTS3Table) MatchInfoX(phrase, scope QueryNode, side int, gate QueryNode, docID int64) []uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	nCol := len(t.columnNames)
	out := make([]uint32, nCol*3)
	// Local hits come from the phrase's own position list for the current row
	// (sqlite3Fts3EvalPhrasePoslist). An AND sibling's truth does not erase
	// them: a row can be returned by another OR branch while this phrase still
	// has its own occurrence in the row. NEAR participation is applied by
	// phraseCountsFor through scope.
	local := phraseCountsFor(t.index, docID, phrase, scope, side, nCol)
	globalOcc, globalRows := t.globalPhraseStats(phrase, scope, side, nCol, docID, gate)
	for i := 0; i < nCol; i++ {
		out[i*3+0] = uint32(local[i])
		if globalOcc != nil {
			out[i*3+1] = uint32(globalOcc[i])
			out[i*3+2] = uint32(globalRows[i])
		}
	}
	return out
}

// globalPhraseStats accumulates the per-column global occurrence and global
// row counts of the phrase (fts3_snippet.c fts3ExprGlobalHitsCb): every row
// whose scope matches contributes its phrase counts, the current row first.
func (t *FTS3Table) globalPhraseStats(phrase, scope QueryNode, side, nCol int, docID int64, gate QueryNode) (globalOcc, globalRows []int) {
	kind, terms := t.phraseDeferral(phrase, gate)
	if kind == phraseDeferAll {
		return deferredNDocStats(t.index.AllDocIDs(), nCol)
	}
	phraseNode := phrase
	scopeNode := scope
	if kind == phraseDeferSome {
		// A phrase mixing deferred and loaded tokens is counted through
		// its loaded tokens only (fts3.c:5968).
		phraseNode = phraseTermsNode(terms, t.tokenizer)
		scopeNode = phraseNode
	}
	addRow := func(d int64) {
		c := phraseCountsFor(t.index, d, phraseNode, scopeNode, side, nCol)
		if globalOcc == nil {
			globalOcc = make([]int, nCol)
			globalRows = make([]int, nCol)
		}
		for i, v := range c {
			globalOcc[i] += v
			if v > 0 {
				globalRows[i]++
			}
		}
	}
	if ScopeMatchesDoc(t.index, docID, scopeNode) {
		addRow(docID)
	}
	for _, d := range t.index.AllDocIDs() {
		if d == docID {
			continue
		}
		if ScopeMatchesDoc(t.index, d, scopeNode) {
			addRow(d)
		}
	}
	return globalOcc, globalRows
}

// deferredNDocStats applies C's sqlite3Fts3EvalPhraseStats shortcut: an
// all-deferred phrase whose parent is not NEAR is assumed to occur once in
// every column of every document (fts3.c:5964).
func deferredNDocStats(docIDs []int64, nCol int) (globalOcc, globalRows []int) {
	nDoc := len(docIDs)
	globalOcc = make([]int, nCol)
	globalRows = make([]int, nCol)
	for i := 0; i < nCol; i++ {
		globalOcc[i] = nDoc
		globalRows[i] = nDoc
	}
	return globalOcc, globalRows
}

// phraseTerm is one token of a MATCH phrase (a prefix wildcard matches a
// family of index terms).
type phraseTerm struct {
	text     string
	prefix   bool
	deferred bool
}

type deferredPhraseKind int

const (
	phraseDeferNone deferredPhraseKind = iota
	phraseDeferSome
	phraseDeferAll
)

// phraseDeferral classifies a phrase's tokens under C's deferred-token rule
// and returns the non-deferred terms for the partial case. Deferral is an
// AND/NEAR-cluster decision (fts3.c fts3EvalSelectDeferred): with no enclosing
// AND/NEAR there is nothing to defer against, and a NEAR parent keeps the
// gather-stats path because its global hits are trimmed by the NEAR distance.
func (t *FTS3Table) phraseDeferral(phrase, gate QueryNode) (deferredPhraseKind, []phraseTerm) {
	if _, ok := gate.(*NearNode); ok {
		return phraseDeferNone, nil
	}
	if !strings.EqualFold(t.moduleName, "fts4") || t.contentTable != "" {
		return phraseDeferNone, nil
	}
	terms := collectPhraseTerms(phrase)
	if len(terms) == 0 {
		return phraseDeferNone, nil
	}
	loaded := make([]phraseTerm, 0, len(terms))
	deferred := 0
	for _, term := range terms {
		if term.deferred {
			deferred++
		} else {
			loaded = append(loaded, term)
		}
	}
	if deferred == len(terms) {
		return phraseDeferAll, nil
	}
	if deferred > 0 {
		return phraseDeferSome, loaded
	}
	return phraseDeferNone, nil
}

// phraseTermsNode rebuilds the non-deferred tokens as a query node so the
// ordinary phrase-count path can count their potential occurrences. Those
// tokens are counted from content: their segment postings may have been lost
// with the same damaged block that made the partner token overflow-sized.
func phraseTermsNode(terms []phraseTerm, tok Tokenizer) QueryNode {
	if len(terms) == 1 {
		if terms[0].prefix {
			return &PrefixNode{Prefix: terms[0].text, Deferred: true, Tokenizer: tok}
		}
		return &TermNode{Term: terms[0].text, Deferred: true, Tokenizer: tok}
	}
	n := &PhraseNode{Terms: make([]string, 0, len(terms)), Prefixes: make([]bool, 0, len(terms)), Deferred: make([]bool, 0, len(terms)), Tokenizer: tok, FirstAt: -1}
	for _, term := range terms {
		n.Terms = append(n.Terms, term.text)
		n.Prefixes = append(n.Prefixes, term.prefix)
		n.Deferred = append(n.Deferred, true)
	}
	return n
}

// collectPhraseTerms unwraps column restrictions and returns the phrase's
// tokens in query order. A nil slice means an unrecognized node.
func collectPhraseTerms(node QueryNode) []phraseTerm {
	if cn, ok := node.(*ColumnNode); ok {
		return collectPhraseTerms(cn.Inner)
	}
	switch n := node.(type) {
	case *TermNode:
		return []phraseTerm{{text: n.Term, deferred: n.Deferred}}
	case *PrefixNode:
		return []phraseTerm{{text: n.Prefix, prefix: true, deferred: n.Deferred}}
	case *PhraseNode:
		out := make([]phraseTerm, 0, len(n.Terms))
		for i, term := range n.Terms {
			out = append(out, phraseTerm{
				text:     term,
				prefix:   i < len(n.Prefixes) && n.Prefixes[i],
				deferred: i < len(n.Deferred) && n.Deferred[i],
			})
		}
		return out
	}
	return nil
}

// termDoclistOverflows estimates the packed FTS3 doclist size of one token:
// a delta varint per docid, a column byte, and a position varint per posting.
const ftsDeferredDoclistBytes = 4096

func (t *FTS3Table) termDoclistOverflows(term phraseTerm) bool {
	key := contentDoclistKey{term: term.text, prefix: term.prefix}
	if overflow, ok := t.index.deferredDoclistOverflows[key]; ok {
		return overflow
	}
	overflow := t.estimateTermDoclistOverflows(term)
	if t.index.deferredDoclistOverflows == nil {
		t.index.deferredDoclistOverflows = make(map[contentDoclistKey]bool)
	}
	t.index.deferredDoclistOverflows[key] = overflow
	return overflow
}

// estimateTermDoclistOverflows computes the packed FTS3 doclist size of one
// token: a delta varint per docid, a column byte, and a position varint per
// posting.
func (t *FTS3Table) estimateTermDoclistOverflows(term phraseTerm) bool {
	size := 0
	for name, postings := range t.index.index {
		if term.prefix {
			if !strings.HasPrefix(name, term.text) {
				continue
			}
		} else if name != term.text {
			continue
		}
		size += packedDoclistBytes(postings)
		if size >= ftsDeferredDoclistBytes {
			return true
		}
	}
	// Postings may be missing or partial after a segment load failed; recover
	// the overflow decision from stored content. The estimate is cached on the
	// index so MATCH, which prepares the query once per row, rescans each term
	// at most once until the index changes.
	return termDoclistSizeFromContent(t.index, t.tokenizer, term) >= ftsDeferredDoclistBytes
}

func packedDoclistBytes(postings []Posting) int {
	size, prevDoc := 0, int64(0)
	for i, p := range postings {
		delta := p.DocID - prevDoc
		if i == 0 {
			delta = p.DocID
		}
		size += fts3VarintLen(uint64(delta)) + 1 + fts3VarintLen(uint64(p.Position+2))
		prevDoc = p.DocID
	}
	return size
}

func fts3VarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// MatchInfoY computes the 'y' format values for one phrase: the per-column
// local hit counts for the current row (fts3_snippet.c fts3ExprLHits). Like
// 'x', the counts come from the phrase's own position list; an AND sibling's
// truth does not erase them.
func (t *FTS3Table) MatchInfoY(phrase, scope QueryNode, side int, gate QueryNode, docID int64) []uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	nCol := len(t.columnNames)
	out := make([]uint32, nCol)
	c := phraseCountsFor(t.index, docID, phrase, scope, side, nCol)
	for i := 0; i < nCol; i++ {
		out[i] = uint32(c[i])
	}
	return out
}

// MatchInfoDocLength returns the per-column token counts of a document for
// the matchinfo 'l' format (fts3_snippet.c FTS3_MATCHINFO_LENGTH).
func (t *FTS3Table) MatchInfoDocLength(docID int64) []uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	nCol := len(t.columnNames)
	c := DocTokenCounts(t.index.GetDoc(docID), t.tokenizer, nCol)
	out := make([]uint32, nCol)
	for i := 0; i < nCol; i++ {
		out[i] = uint32(c[i])
	}
	return out
}

// SegmentRoot builds the FTS3 segment root blob for the given document IDs
// (SQLite's fts3SegWriter produces one segment per insert batch; the root
// blob is stored in the %_segdir.root column so corruption tests can read and
// modify it). nodeSize is the segment node size (the FTS table's page size).
// The caller passes the doc IDs added in the batch; their postings are
// grouped by term and serialized into leaf/interior nodes.
func (t *FTS3Table) SegmentRoot(docIDs []int64, nodeSize int) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.segmentRootLocked(docIDs, nodeSize)
}

func (t *FTS3Table) segmentRootLocked(docIDs []int64, nodeSize int) []byte {
	root, _ := t.segmentBlocksLocked(docIDs, nodeSize)
	return root
}

// SegmentRootBlocks is SegmentRoot plus the leaf blocks for the %_segments
// table.
func (t *FTS3Table) SegmentRootBlocks(docIDs []int64, nodeSize int) ([]byte, []SegmentBlock) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.segmentBlocksLocked(docIDs, nodeSize)
}

func (t *FTS3Table) segmentBlocksLocked(docIDs []int64, nodeSize int) ([]byte, []SegmentBlock) {
	return t.segmentBlocksIndexLocked(docIDs, nodeSize, 0)
}

// segmentBlocksIndexLocked builds the segment for index iIndex (0 = main;
// iIndex >= 1 = prefix index). Prefix index terms are the first prefixLen
// bytes of each main term whose length is at least prefixLen (fts3_write.c
// fts3InsertTerms: a token is added to prefix index i only when
// nToken >= aIndex[i].nPrefix); the postings are the term's own postings.
func (t *FTS3Table) segmentBlocksIndexLocked(docIDs []int64, nodeSize int, iIndex int) ([]byte, []SegmentBlock) {
	want := make(map[int64]bool, len(docIDs))
	for _, id := range docIDs {
		want[id] = true
	}
	termPostings := make(map[string][]Posting)
	if iIndex <= 0 {
		t.collectMainIndexPostingsLocked(docIDs, want, termPostings)
	} else {
		if iIndex-1 >= len(t.prefixLengths) {
			return serializeSegmentBlocks(nil, nodeSize)
		}
		t.collectPrefixIndexPostingsLocked(want, t.prefixLengths[iIndex-1], termPostings)
	}
	// REPLACE markers first: they can introduce terms the re-inserted
	// documents no longer contain (the old document's term set), so the
	// record list must be collected afterwards.
	t.injectReplaceMarkersLocked(termPostings, iIndex)
	return serializeTermPostingsLocked(termPostings, nodeSize)
}

// collectMainIndexPostingsLocked gathers the wanted documents' postings for
// the main index, keyed by the full term. For a small doc set (a per-commit
// pending flush), it re-tokenizes the wanted documents directly instead of
// scanning every posting in the index (O(pending tokens) vs O(total
// postings)). This keeps per-row FTS builds linear: without it, each flush
// scans the whole index, so N flushes over a growing table are O(N^2)
// (fts4merge4 2.2.x inserts 500 40KB documents in 100 transactions).
func (t *FTS3Table) collectMainIndexPostingsLocked(docIDs []int64, want map[int64]bool, termPostings map[string][]Posting) {
	if len(docIDs) <= 64 {
		t.retokenizeDocPostingsLocked(docIDs, termPostings)
		return
	}
	for term, postings := range t.index.index {
		for _, p := range postings {
			if want[p.DocID] {
				termPostings[term] = append(termPostings[term], p)
			}
		}
	}
}

// retokenizeDocPostingsLocked re-tokenizes the wanted documents directly
// instead of scanning every posting in the index (O(pending tokens) vs
// O(total postings)). This keeps per-row FTS builds linear: without it, each
// flush scans the whole index, so N flushes over a growing table are O(N^2)
// (fts4merge4 2.2.x inserts 500 40KB documents in 100 transactions).
func (t *FTS3Table) retokenizeDocPostingsLocked(docIDs []int64, termPostings map[string][]Posting) {
	for _, docID := range docIDs {
		doc := t.index.GetDoc(docID)
		if doc == nil {
			continue
		}
		for colNum, v := range doc.Columns {
			if !t.ColumnIndexed(colNum) {
				continue
			}
			tokens := t.tokenizer.Tokenize(ftsColumnString(v))
			for _, tok := range tokens {
				termPostings[tok.Term] = append(termPostings[tok.Term], Posting{
					DocID:    docID,
					Column:   colNum,
					Position: tok.Position,
				})
			}
		}
	}
}

// collectPrefixIndexPostingsLocked gathers the wanted documents' postings
// under their prefix-truncated keys (fts3_write.c fts3InsertTerms: a token
// is added to prefix index i only when nToken >= aIndex[i].nPrefix).
func (t *FTS3Table) collectPrefixIndexPostingsLocked(want map[int64]bool, prefixLen int, termPostings map[string][]Posting) {
	for term, postings := range t.index.index {
		if len(term) < prefixLen {
			continue
		}
		prefix := term[:prefixLen]
		for _, p := range postings {
			if want[p.DocID] {
				termPostings[prefix] = append(termPostings[prefix], p)
			}
		}
	}
}

// serializeTermPostingsLocked sorts each term's postings and serializes the
// term records into the segment root + leaf blocks.
func serializeTermPostingsLocked(termPostings map[string][]Posting, nodeSize int) ([]byte, []SegmentBlock) {
	terms := make([]string, 0, len(termPostings))
	for term := range termPostings {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	records := make([]termRecord, 0, len(terms))
	for _, term := range terms {
		sortPostings(termPostings[term])
		records = append(records, termRecord{term: term, doclist: buildDoclist(termPostings[term])})
	}
	return serializeSegmentBlocks(records, nodeSize)
}

// SegmentRootBlocksIndex builds the segment root + leaf blocks for index
// iIndex (0 = main; iIndex >= 1 = prefix index), used by the %_segdir flush to
// write one row per index at level 1024*iIndex (fts3_write.c fts3SegWriter:
// "the block of iIndex starts at absolute level ((iLangid*(nPrefix+1)+iIndex)
// *1024)").
func (t *FTS3Table) SegmentRootBlocksIndex(docIDs []int64, nodeSize int, iIndex int) ([]byte, []SegmentBlock) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.segmentBlocksIndexLocked(docIDs, nodeSize, iIndex)
}

// SegmentRootBlocksForTerms builds a segment containing the postings of the
// given (term → docIDs) pairs. The incremental merge's chomp uses it to
// TRUNCATE a partially-consumed source segment to its unmerged terms
// (SQLite's fts3TruncateSegment removes the merged terms; rebuilding from doc
// IDs would re-include them because a document appears in many terms).
func (t *FTS3Table) SegmentRootBlocksForTerms(termDocs map[string][]int64, nodeSize int) ([]byte, []SegmentBlock) {
	t.mu.Lock()
	defer t.mu.Unlock()
	want := make(map[string]map[int64]bool, len(termDocs))
	for term, ids := range termDocs {
		m := make(map[int64]bool, len(ids))
		for _, id := range ids {
			m[id] = true
		}
		want[term] = m
	}
	terms := make([]string, 0, len(termDocs))
	for term := range termDocs {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	records := make([]termRecord, 0, len(terms))
	for _, term := range terms {
		var postings []Posting
		for _, p := range t.index.index[term] {
			if want[term][p.DocID] {
				postings = append(postings, p)
			}
		}
		sortPostings(postings)
		records = append(records, termRecord{term: term, doclist: buildDoclist(postings)})
	}
	return serializeSegmentBlocks(records, nodeSize)
}

// DeleteMarkerRoot builds the FTS3 delete-marker segment root for the given
// document IDs: one record per term the documents appear in, with a doclist
// of [docid][0] per document (no positions). SQLite's fts3DeleteTerms writes
// such a segment when a flushed document is deleted (fts4content 3.1.5: after
// DELETE FROM ft3 the segdir gains a delete-marker row). Loading it removes
// the documents' term postings. The TERMS come from the snapshot captured at
// Delete time (the in-memory index no longer has the document's postings).
func (t *FTS3Table) DeleteMarkerRoot(docIDs []int64, nodeSize int) ([]byte, []SegmentBlock) {
	t.mu.Lock()
	defer t.mu.Unlock()
	termDocIDs := t.deleteMarkerTermsLocked(docIDs)
	t.consumeDeleteMarkersLocked(docIDs)
	records := markerRecords(termDocIDs)
	if len(records) == 0 {
		return nil, nil
	}
	return serializeSegmentBlocks(records, nodeSize)
}

// DeleteMarkerRootIndex is DeleteMarkerRoot for a single FTS index:
// iIndex 0 is the main index; iIndex >= 1 is prefix index iIndex-1 whose
// records carry terms truncated to that index's prefix length (SQLite's
// fts3DeleteTerms feeds EVERY index's pending-terms hash — p->aIndex[i] — so
// a delete of a flushed document produces one marker segment per index,
// fts4opt 2.x: prefix indexes must gain level-1024*i marker rows too).
//
// Unlike DeleteMarkerRoot it does NOT consume the term snapshot: the flush
// builds all per-index markers first and then calls ConsumeDeleteMarkers once.
// Returns nil roots when no term maps into this index.
func (t *FTS3Table) DeleteMarkerRootIndex(docIDs []int64, nodeSize int, iIndex int) ([]byte, []SegmentBlock) {
	t.mu.Lock()
	defer t.mu.Unlock()
	termDocIDs := t.deleteMarkerTermsLocked(docIDs)
	if iIndex > 0 {
		if iIndex-1 >= len(t.prefixLengths) {
			return nil, nil
		}
		prefixLen := t.prefixLengths[iIndex-1]
		mapped := make(map[string]map[int64]bool)
		for term, ids := range termDocIDs {
			if len(term) < prefixLen {
				continue
			}
			prefix := term[:prefixLen]
			if mapped[prefix] == nil {
				mapped[prefix] = make(map[int64]bool)
			}
			for id := range ids {
				mapped[prefix][id] = true
			}
		}
		termDocIDs = mapped
	}
	records := markerRecords(termDocIDs)
	if len(records) == 0 {
		return nil, nil
	}
	return serializeSegmentBlocks(records, nodeSize)
}

// ConsumeDeleteMarkers discards the delete-term snapshots for the given docids
// after their per-index marker segments have been persisted.
func (t *FTS3Table) ConsumeDeleteMarkers(docIDs []int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.consumeDeleteMarkersLocked(docIDs)
}

// SetReplaceDocs marks docids deleted AND re-inserted within the current
// flush batch: their markers are merged into the pending-flush segment's term
// doclists rather than written as a separate delete-marker segment.
func (t *FTS3Table) SetReplaceDocs(docIDs []int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.replaceDocs = make(map[int64]bool, len(docIDs))
	for _, id := range docIDs {
		t.replaceDocs[id] = true
	}
}

// ClearReplaceDocs resets the replace-batch marker set after a flush.
func (t *FTS3Table) ClearReplaceDocs() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.replaceDocs = nil
}

// injectReplaceMarkersLocked adds position-less delete entries for the
// current flush batch's replaced documents into termPostings (mutated in
// place), keyed by the index's term form: full terms for the main index,
// prefix-truncated keys for prefix indexes. The OLD term set comes from the
// DELETE-time snapshot, so terms dropped by the re-insertion still get their
// marker record (SQLite's pending hash holds both operations per term).
// Caller holds t.mu.
func (t *FTS3Table) injectReplaceMarkersLocked(termPostings map[string][]Posting, iIndex int) {
	if len(t.replaceDocs) == 0 || len(t.deleteMarkerTerms) == 0 {
		return
	}
	prefixLen := 0
	if iIndex > 0 {
		if iIndex-1 >= len(t.prefixLengths) {
			return
		}
		prefixLen = t.prefixLengths[iIndex-1]
	}
	for id := range t.replaceDocs {
		terms, ok := t.deleteMarkerTerms[id]
		if !ok {
			continue
		}
		t.injectDocReplaceMarkersLocked(termPostings, id, terms, iIndex, prefixLen)
	}
}

// injectDocReplaceMarkersLocked adds one replaced document's position-less
// delete entries into termPostings, keyed by the index's term form.
func (t *FTS3Table) injectDocReplaceMarkersLocked(termPostings map[string][]Posting, id int64, terms []string, iIndex, prefixLen int) {
	for _, term := range terms {
		key, ok := bandTermKey(term, iIndex, prefixLen)
		if !ok {
			continue
		}
		// A term the re-inserted document STILL contains needs no
		// marker: SQLite's pending hash keys entries by (term, docid),
		// so the delete contributes only the bare docid and the insert
		// continues the SAME entry with its positions (one normal
		// entry). Only dropped terms get a bare-docid marker record.
		has := false
		for _, p := range termPostings[key] {
			if p.DocID == id {
				has = true
				break
			}
		}
		if has {
			continue
		}
		termPostings[key] = append([]Posting{{DocID: id, Column: -1, Delete: true}},
			termPostings[key]...)
	}
}

// bandTermKey returns a term's key in index band iIndex (prefix-truncated
// for prefix bands), reporting whether the term contributes to the band.
func bandTermKey(term string, iIndex, prefixLen int) (string, bool) {
	if iIndex <= 0 {
		return term, true
	}
	if len(term) < prefixLen {
		return "", false
	}
	return term[:prefixLen], true
}

// consumeDeleteMarkersLocked drops the snapshot entries for docIDs.
// Caller holds t.mu.
func (t *FTS3Table) consumeDeleteMarkersLocked(docIDs []int64) {
	if t.deleteMarkerTerms != nil {
		for _, id := range docIDs {
			delete(t.deleteMarkerTerms, id)
		}
	}
}

// deleteMarkerTermsLocked collects term → docid sets for the given documents:
// prefer the DELETE-time snapshot (the doc's postings were already removed
// from the index); fall back to scanning the index for postings of the docid
// (callers that build a marker while the doc is still indexed, e.g. OR REPLACE
// conflicts). Caller holds t.mu.
func (t *FTS3Table) deleteMarkerTermsLocked(docIDs []int64) map[string]map[int64]bool {
	want := make(map[int64]bool, len(docIDs))
	for _, id := range docIDs {
		want[id] = true
	}
	termDocIDs := make(map[string]map[int64]bool)
	t.collectSnapshotMarkerTerms(docIDs, termDocIDs)
	t.collectIndexedMarkerTerms(want, termDocIDs)
	return termDocIDs
}

// collectSnapshotMarkerTerms copies the DELETE-time term snapshots of docIDs
// into termDocIDs.
func (t *FTS3Table) collectSnapshotMarkerTerms(docIDs []int64, termDocIDs map[string]map[int64]bool) {
	if t.deleteMarkerTerms == nil {
		return
	}
	for _, id := range docIDs {
		if terms, ok := t.deleteMarkerTerms[id]; ok {
			for _, term := range terms {
				if termDocIDs[term] == nil {
					termDocIDs[term] = make(map[int64]bool)
				}
				termDocIDs[term][id] = true
			}
		}
	}
}

// collectIndexedMarkerTerms scans the index for postings of the wanted
// docids (marker sources for documents still indexed).
func (t *FTS3Table) collectIndexedMarkerTerms(want map[int64]bool, termDocIDs map[string]map[int64]bool) {
	for term, postings := range t.index.index {
		for _, p := range postings {
			if want[p.DocID] {
				if termDocIDs[term] == nil {
					termDocIDs[term] = make(map[int64]bool)
				}
				termDocIDs[term][p.DocID] = true
			}
		}
	}
}

// markerRecords converts a term → docid set into sorted segment records with
// position-less delete doclists ([delta-docid][posEnd]).
func markerRecords(termDocIDs map[string]map[int64]bool) []termRecord {
	terms := make([]string, 0, len(termDocIDs))
	for term := range termDocIDs {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	records := make([]termRecord, 0, len(terms))
	for _, term := range terms {
		var doclist []byte
		var lastDocID int64
		wrote := false
		ids := make([]int64, 0, len(termDocIDs[term]))
		for id := range termDocIDs[term] {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			if wrote && id == lastDocID {
				continue
			}
			doclist = appendVarint(doclist, uint64(id-lastDocID))
			doclist = appendVarint(doclist, posEnd)
			lastDocID = id
			wrote = true
		}
		records = append(records, termRecord{term: term, doclist: doclist})
	}
	return records
}

// RecordPending marks a doc ID as inserted since the last segment flush.
func (t *FTS3Table) RecordPending(docID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingDocIDs = append(t.pendingDocIDs, docID)
}

// PendingFlush returns the doc IDs accumulated since the last flush (one
// segment batch) and clears the pending list. Returns nil when nothing is
// pending.
func (t *FTS3Table) PendingFlush() []int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pendingDocIDs) == 0 {
		return nil
	}
	ids := append([]int64(nil), t.pendingDocIDs...)
	t.pendingDocIDs = nil
	return ids
}

// DeletedFlush returns the doc IDs deleted since the last flush that need a
// delete-marker segment, and clears the list. Returns nil when none. The
// delete-marker TERM snapshots are consumed by DeleteMarkerRoot (called by
// the flush after this), not here — this method only reports which docids
// need markers.
func (t *FTS3Table) DeletedFlush() []int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.deletedDocIDs) == 0 {
		return nil
	}
	ids := append([]int64(nil), t.deletedDocIDs...)
	t.deletedDocIDs = nil
	return ids
}

// DeletedSnapshot returns a COPY of the doc IDs deleted since the last flush
// WITHOUT clearing the list (unlike DeletedFlush). Used by the integrity
// check, which must see — but not consume — unflushed deletions (an UPDATE's
// Delete+reinsert inside an open transaction leaves the old postings in the
// persisted segments until COMMIT; fts4langid 6.1 integrity-checks mid-
// transaction and must ignore those stale postings).
func (t *FTS3Table) DeletedSnapshot() []int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.deletedDocIDs) == 0 {
		return nil
	}
	return append([]int64(nil), t.deletedDocIDs...)
}

// Snapshot returns a deep copy of the FTS table's in-memory index for
// transaction/savepoint rollback (see InvertedIndex.Snapshot).
func (t *FTS3Table) Snapshot() *InvertedIndex {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.index.Snapshot()
}

// PendingSnapshot returns a copy of the pending-docid list so a rollback can
// restore it (the pending segments must match the rolled-back index).
func (t *FTS3Table) PendingSnapshot() []int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]int64(nil), t.pendingDocIDs...)
}

// DeleteMarkerTermsSnapshot returns a deep copy of the delete-marker term
// snapshot map so a rollback can restore it (a failed statement's Delete must
// not leave marker terms behind).
func (t *FTS3Table) DeleteMarkerTermsSnapshot() map[int64][]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.deleteMarkerTerms == nil {
		return nil
	}
	out := make(map[int64][]string, len(t.deleteMarkerTerms))
	for id, terms := range t.deleteMarkerTerms {
		out[id] = append([]string(nil), terms...)
	}
	return out
}

// RestoreDeleteMarkerTerms replaces the delete-marker term snapshot with a
// copy taken earlier (see DeleteMarkerTermsSnapshot).
func (t *FTS3Table) RestoreDeleteMarkerTerms(m map[int64][]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deleteMarkerTerms = m
}

// RestorePending replaces the pending-docid list with a snapshot taken
// earlier (see PendingSnapshot).
func (t *FTS3Table) RestorePending(ids []int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingDocIDs = ids
}

// Restore replaces the FTS table's in-memory index with a snapshot taken
// earlier (see InvertedIndex.Restore).
func (t *FTS3Table) Restore(s *InvertedIndex) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index.Restore(s)
	t.statDirty = true
}

// Clear resets the FTS table's in-memory index and pending/delete lists,
// dropping every document and posting (SQLite's fts3DeleteAll for a
// DELETE FROM <fts> with no WHERE clause). The segdir-idx, %_segments block
// and merge-writer caches are also invalidated — the shadow tables were
// emptied, so a stale next-block-id or next-idx would make the next flush
// collide with (or reference) freed rows ("database disk image is malformed"
// on the automerge's following insert, fts4merge4 2.2.1.2).
func (t *FTS3Table) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index = NewInvertedIndex()
	t.reapplySkipColumnsLocked()
	t.pendingDocIDs = nil
	t.deletedDocIDs = nil
	t.deleteMarkerTerms = nil
	t.segdirIdxValid = false
	t.segdirNextIdx = nil
	if t.nextBlockID > t.blockIDHighWater {
		t.blockIDHighWater = t.nextBlockID
	}
	t.nextBlockIDValid = false
	t.nextBlockID = 0
	t.mergeCtx = nil
	t.statDirty = true
}

// prepareQueryLocked parses, tokenizes, resolves and marks a MATCH query.
// The caller must hold t.mu. The returned tree is shared by row filtering and
// every auxiliary function for one query so deferral decisions agree.
func (t *FTS3Table) prepareQueryLocked(query, columnName string, langid int64) (QueryNode, error) {
	node, err := ParseMatchQuery(query)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(t.moduleName, "fts4") {
		ClearFirstFlags(node)
	}
	node = tokenizeQueryNodeLangid(node, t.tokenizer, langid)
	node = ResolveQuery(node, t.columnNames)
	if columnName != "" {
		for i, name := range t.columnNames {
			if strings.EqualFold(name, columnName) {
				node = restrictQueryColumn(node, i)
				break
			}
		}
	}
	t.markDeferredTokens(node)
	return node, nil
}

// MatchPhrases returns the phrase leaves of a MATCH query prepared with the
// same tokenizer, column restriction and deferral marks as row filtering.
func (t *FTS3Table) MatchPhrases(query, columnName string) []MatchPhrase {
	t.mu.Lock()
	defer t.mu.Unlock()
	node, err := t.prepareQueryLocked(query, columnName, 0)
	if err != nil {
		return nil
	}
	return ExtractPhrases(node)
}

// MatchDocIDs returns docids matching a MATCH query.
func (t *FTS3Table) MatchDocIDs(query string) ([]int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := t.prepareQueryLocked(query, "", 0)
	if err != nil {
		return nil, err
	}
	allIDs := t.index.AllDocIDs()
	var matched []int64
	for _, docID := range allIDs {
		if node.MatchDoc(t.index, docID) {
			matched = append(matched, docID)
		}
	}
	return matched, nil
}

// MatchQuery checks if a specific document matches the given MATCH query.
func (t *FTS3Table) MatchQuery(docID int64, query string) (bool, error) {
	return t.MatchQueryColumn(docID, query, "")
}

// MatchQueryColumn checks if a specific document matches the given MATCH
// query, optionally restricted to one column (SQL `col MATCH 'q'`). The column
// restriction applies only to query terms WITHOUT their own column prefix
// (fts3_expr.c: an explicit `col:term` overrides the SQL left-side column).
// langid is the query's FTS4 languageid (the langid= constraint value, 0 by
// default): a language-aware tokenizer parses the query at that language
// (fts3.c fts3ExprParse receives the cursor's iLangid — fts4langid 4.1.3:
// 'Quick' at langid 1 keeps its case).
func (t *FTS3Table) MatchQueryColumn(docID int64, query, columnName string, langid ...int64) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	qLangid := int64(0)
	if len(langid) > 0 {
		qLangid = langid[0]
	}

	node, err := t.prepareQueryLocked(query, columnName, qLangid)
	if err != nil {
		return false, err
	}
	if queryReferencesCorruptTerm(node, t.index) {
		// The query reads a term whose segment doclist is corrupt: SQLite
		// fails the MATCH with "database disk image is malformed"
		// (fts3corrupt4 11.1/19.1).
		return false, fmt.Errorf("database disk image is malformed [T31]")
	}
	return node.MatchDoc(t.index, docID), nil
}

// tokenizeQueryNode applies the table's tokenizer to every term in a parsed
// MATCH query, so query words are stemmed/lowercased exactly like the indexed
// document terms (SQLite: the query is tokenized with the table's tokenizer).
func tokenizeQueryNode(node QueryNode, tok Tokenizer) QueryNode {
	return TokenizeQueryNode(node, tok)
}

// tokenizeQueryNodeLangid tokenizes the query with a language-bound tokenizer
// when the table's tokenizer is language-aware (fts4langid 4.x); the default
// language id 0 keeps the plain behavior.
func tokenizeQueryNodeLangid(node QueryNode, tok Tokenizer, langid int64) QueryNode {
	if la, ok := tok.(LangidAware); ok {
		tok = la.WithLangid(langid)
	}
	return TokenizeQueryNode(node, tok)
}

// TokenizeQueryNode applies the table's tokenizer to every term in a parsed
// MATCH query, so query words are stemmed/lowercased exactly like the indexed
// document terms (SQLite: the query is tokenized with the table's tokenizer).
// Exported for the matchinfo phrase extraction (ftsMatchPhrases).
func TokenizeQueryNode(node QueryNode, tok Tokenizer) QueryNode {
	if tok == nil {
		return node
	}
	stem := func(term string) string {
		tokens := tok.Tokenize(term)
		if len(tokens) == 0 {
			return strings.ToLower(term)
		}
		return tokens[0].Term
	}
	if leaf := tokenizeQueryLeaf(node, stem); leaf != nil {
		return leaf
	}
	switch n := node.(type) {
	case *AndNode:
		return &AndNode{Left: tokenizeQueryNode(n.Left, tok), Right: tokenizeQueryNode(n.Right, tok)}
	case *NearNode:
		return &NearNode{Left: tokenizeQueryNode(n.Left, tok), Right: tokenizeQueryNode(n.Right, tok), Distance: n.Distance}
	case *OrNode:
		return &OrNode{Left: tokenizeQueryNode(n.Left, tok), Right: tokenizeQueryNode(n.Right, tok)}
	case *NotNode:
		return &NotNode{Inner: tokenizeQueryNode(n.Inner, tok)}
	case *ColumnRefNode:
		return &ColumnRefNode{ColumnName: n.ColumnName, Inner: tokenizeQueryNode(n.Inner, tok)}
	case *ColumnNode:
		return &ColumnNode{Column: n.Column, Inner: tokenizeQueryNode(n.Inner, tok)}
	default:
		return node
	}
}

// tokenizeQueryLeaf rewrites a leaf query node (term, prefix or phrase) into
// its tokenized form; nil when node is not a leaf the tokenizer rewrites.
func tokenizeQueryLeaf(node QueryNode, stem func(string) string) QueryNode {
	switch n := node.(type) {
	case *TermNode:
		return &TermNode{Term: stem(n.Term), First: n.First, Deferred: n.Deferred, Tokenizer: n.Tokenizer}
	case *PrefixNode:
		return &PrefixNode{Prefix: stem(n.Prefix), First: n.First, Deferred: n.Deferred, Tokenizer: n.Tokenizer}
	case *PhraseNode:
		terms := make([]string, len(n.Terms))
		for i, t := range n.Terms {
			terms[i] = stem(t)
		}
		return &PhraseNode{Terms: terms, Prefixes: n.Prefixes, Deferred: n.Deferred, Tokenizer: n.Tokenizer, First: n.First, FirstAt: n.FirstAt}
	}
	return nil
}

// RestrictQueryColumn wraps every query node that is NOT already column-scoped
// in a ColumnNode for colIdx — the exported form used by execddl's
// matchinfo/offsets phrase extraction, applying the same restriction
// MatchQueryColumn applies for matching (fts3.c fts3FilterMethod parses the
// query with the MATCH left-hand column as iDefaultCol).
func RestrictQueryColumn(node QueryNode, colIdx int) QueryNode {
	return restrictQueryColumn(node, colIdx)
}

// restrictQueryColumn wraps every query node that is NOT already column-scoped
// (a ColumnNode from an explicit col: prefix) in a ColumnNode for colIdx. This
// mirrors SQLite: `body MATCH 'title:linux driver'` restricts only the
// unprefixed `driver` term to the body column, while `title:linux` keeps its
// own title restriction.
func restrictQueryColumn(node QueryNode, colIdx int) QueryNode {
	switch n := node.(type) {
	case *ColumnNode:
		// Explicit col: prefix — keep its own column restriction.
		return n
	case *TermNode, *PhraseNode, *PrefixNode:
		return &ColumnNode{Column: colIdx, Inner: node}
	case *AndNode:
		return &AndNode{Left: restrictQueryColumn(n.Left, colIdx), Right: restrictQueryColumn(n.Right, colIdx)}
	case *NearNode:
		return &NearNode{Left: restrictQueryColumn(n.Left, colIdx), Right: restrictQueryColumn(n.Right, colIdx), Distance: n.Distance}
	case *OrNode:
		return &OrNode{Left: restrictQueryColumn(n.Left, colIdx), Right: restrictQueryColumn(n.Right, colIdx)}
	case *NotNode:
		return &NotNode{Inner: restrictQueryColumn(n.Inner, colIdx)}
	default:
		return node
	}
}

// --- vtab interface implementation ---

// FTS3VTab implements vtab.VirtualTable for FTS3/4.
type FTS3VTab struct {
	module *FTS3Module
}

// SchemaDeclared implements vtab.SchemaDeclaredMarker: fts3's constructor
// always calls sqlite3_declare_vtab, even for an empty-column declaration
// (CREATE VIRTUAL TABLE t4 USING fts3(), vtab1-22.x).
func (v *FTS3VTab) SchemaDeclared() bool { return true }

func (v *FTS3VTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

func (v *FTS3VTab) Open() (vtab.Cursor, error) {
	return &FTS3Cursor{}, nil
}

// FTS3Cursor implements vtab.Cursor (stateless placeholder).
type FTS3Cursor struct{}
