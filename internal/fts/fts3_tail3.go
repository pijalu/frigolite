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
	// Local hits are only reported when the phrase's gate (the innermost
	// enclosing AND/NEAR, or the phrase itself when no gate) matches the
	// current row (fts3_snippet.c fts3ExprLocalHitsCb: the position list
	// is NULL for a row the phrase's evaluation did not reach).
	gateMatches := true
	if gate != nil {
		gateMatches = gate.MatchDoc(t.index, docID)
	}
	var local []int
	if gateMatches {
		local = phraseCountsFor(t.index, docID, phrase, scope, side, nCol)
	} else {
		local = make([]int, nCol)
	}
	var globalOcc []int
	var globalRows []int
	addRow := func(d int64) {
		c := phraseCountsFor(t.index, d, phrase, scope, side, nCol)
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
	if ScopeMatchesDoc(t.index, docID, scope) {
		addRow(docID)
	}
	for _, d := range t.index.AllDocIDs() {
		if d == docID {
			continue
		}
		if ScopeMatchesDoc(t.index, d, scope) {
			addRow(d)
		}
	}
	for i := 0; i < nCol; i++ {
		out[i*3+0] = uint32(local[i])
		if globalOcc != nil {
			out[i*3+1] = uint32(globalOcc[i])
			out[i*3+2] = uint32(globalRows[i])
		}
	}
	return out
}

// MatchInfoY computes the 'y' format values for one phrase: the per-column
// local hit counts for the current row (fts3_snippet.c fts3ExprLHits). The
// counts are zero when the phrase's gate (innermost enclosing AND/NEAR) does
// not match the current row.
func (t *FTS3Table) MatchInfoY(phrase, scope QueryNode, side int, gate QueryNode, docID int64) []uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	nCol := len(t.columnNames)
	out := make([]uint32, nCol)
	if gate != nil && !gate.MatchDoc(t.index, docID) {
		return out
	}
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
		// For a small doc set (a per-commit pending flush), re-tokenize the
		// wanted documents directly instead of scanning every posting in the
		// index (O(pending tokens) vs O(total postings)). This keeps per-row
		// FTS builds linear: without it, each flush scans the whole index, so
		// N flushes over a growing table are O(N^2) (fts4merge4 2.2.x inserts
		// 500 40KB documents in 100 transactions).
		if len(docIDs) <= 64 {
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
		} else {
			for term, postings := range t.index.index {
				for _, p := range postings {
					if want[p.DocID] {
						termPostings[term] = append(termPostings[term], p)
					}
				}
			}
		}
	} else {
		if iIndex-1 >= len(t.prefixLengths) {
			return serializeSegmentBlocks(nil, nodeSize)
		}
		prefixLen := t.prefixLengths[iIndex-1]
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
	// REPLACE markers first: they can introduce terms the re-inserted
	// documents no longer contain (the old document's term set), so the
	// record list must be collected afterwards.
	t.injectReplaceMarkersLocked(termPostings, iIndex)
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
	var prefixLen int
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
		for _, term := range terms {
			key := term
			if iIndex > 0 {
				if len(term) < prefixLen {
					continue
				}
				key = term[:prefixLen]
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
	if t.deleteMarkerTerms != nil {
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
	return termDocIDs
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
	t.nextBlockIDValid = false
	t.nextBlockID = 0
	t.mergeCtx = nil
	t.statDirty = true
}

// MatchDocIDs returns docids matching a MATCH query.
func (t *FTS3Table) MatchDocIDs(query string) ([]int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := ParseMatchQuery(query)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(t.moduleName, "fts4") {
		ClearFirstFlags(node)
	}

	node = tokenizeQueryNode(node, t.tokenizer)
	node = ResolveQuery(node, t.columnNames)
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

	node, err := ParseMatchQuery(query)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(t.moduleName, "fts4") {
		ClearFirstFlags(node)
	}

	node = tokenizeQueryNodeLangid(node, t.tokenizer, qLangid)
	node = ResolveQuery(node, t.columnNames)
	if columnName != "" {
		for i, name := range t.columnNames {
			if strings.EqualFold(name, columnName) {
				node = restrictQueryColumn(node, i)
				break
			}
		}
	}
	if queryReferencesCorruptTerm(node, t.index) {
		// The query reads a term whose segment doclist is corrupt: SQLite
		// fails the MATCH with "database disk image is malformed"
		// (fts3corrupt4 11.1/19.1).
		return false, fmt.Errorf("database disk image is malformed")
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
	switch n := node.(type) {
	case *TermNode:
		return &TermNode{Term: stem(n.Term), First: n.First}
	case *PrefixNode:
		return &PrefixNode{Prefix: stem(n.Prefix), First: n.First}
	case *PhraseNode:
		terms := make([]string, len(n.Terms))
		for i, t := range n.Terms {
			terms[i] = stem(t)
		}
		return &PhraseNode{Terms: terms, Prefixes: n.Prefixes, First: n.First, FirstAt: n.FirstAt}
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

func (v *FTS3VTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

func (v *FTS3VTab) Open() (vtab.Cursor, error) {
	return &FTS3Cursor{}, nil
}

// FTS3Cursor implements vtab.Cursor (stateless placeholder).
type FTS3Cursor struct{}
