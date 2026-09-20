package fts

import (
	"fmt"
	"sort"
)

// SegmentStreamReader lazily iterates the (term, docIDs) entries of one FTS3
// segment in sorted order, reading %_segments leaf blocks only as the
// iteration reaches them (SQLite's Fts3SegReader / fts3SegReaderNext). The
// incremental merge uses one reader per source segment so its cost is bounded
// by the terms it consumes (the leaf-page quota), not by the whole segment
// size — without this, the automerge re-read every source segment's content
// each flush, O(n^2) over many flushes (fts4merge4 2.2.x: 100 transactions of
// five 40KB documents).
type SegmentStreamReader struct {
	readBlock SegmentBlockReader

	// Parsed root: the segment's leaf chain. For a root-only (height 0)
	// segment the root blob is leaf 0 and blocks 1..nLeaves-1 come from
	// %_segments; for a height-1 interior root the leaves are the
	// consecutive %_segments blocks [firstBlock, firstBlock+nLeaves). For a
	// TALLER root the children are interior layers, so the leaf block ids
	// are enumerated once up front by walking the tree (leafIDs) — SQLite's
	// fts3SegReader descends the same layers when seeking (fts3_write.c);
	// a leaf-only walker misreads every interior child as a leaf and fails
	// with "corrupt segment root", which silently aborted every merge into
	// levels whose outputs had split (the fts4merge4 2.2 plateau).
	height     int
	firstBlock int
	nLeaves    int
	leafIDs    []int // leaf block ids in order (height >= 2 roots only)

	// Current leaf buffer and parse position.
	leaf      []byte
	pos       int
	leafFirst bool // the next term in this leaf is its first (full) term
	prevTerm  []byte

	// nextLeafIdx is the index of the next %_segments leaf to load (0 = the
	// first leaf). For a height-0 segment leaf 0 is the root blob.
	nextLeafIdx int

	// The most recently yielded term (the reader's position). After a merge
	// stops at its quota, the reader is positioned at the first UNMERGED term;
	// the truncation keeps terms from this position onward (SQLite's
	// fts3IncrmergeChomp / fts3TruncateSegment, which removes keys smaller
	// than the reader's current zTerm).
	term        string
	docIDs      []int64
	doclist     []byte
	doclistSize int

	atEOF bool
	err   error
}

// NewSegmentStreamReader parses a segment's root blob and returns a reader over
// its terms. The leaves are read lazily via readBlock; a structurally corrupt
// segment surfaces on the first Next.
func NewSegmentStreamReader(root []byte, leavesEndBlock int, readBlock SegmentBlockReader) *SegmentStreamReader {
	r := &SegmentStreamReader{
		readBlock: readBlock,
		nLeaves:   1,
	}
	if len(root) == 0 {
		r.atEOF = true
		return r
	}
	height, n := getFTS3Varint(root)
	if n == 0 {
		r.err = fmt.Errorf("corrupt segment root")
		return r
	}
	r.height = int(height)
	pos := n
	if r.height == 0 {
		// The root is a leaf; additional leaves (blocks 1..leavesEndBlock)
		// follow in %_segments (SQLite's fts3SegReaderNext reads them after
		// the root is exhausted).
		r.leaf = root
		r.pos = pos
		r.nLeaves = leavesEndBlock + 1
		r.leafFirst = true
		r.nextLeafIdx = 1
		return r
	}
	// Interior node: first block id, boundary terms, then the leaves. For a
	// height-1 root the children ARE the leaves (consecutive blocks from
	// firstBlock); a taller root's children are interior layers, so the leaf
	// block ids are collected by descending the tree (SQLite's
	// fts3SegReader walks the same interior nodes on demand).
	firstBlock, n := getFTS3Varint(root[pos:])
	if n == 0 {
		r.err = fmt.Errorf("corrupt segment root")
		return r
	}
	pos += n
	r.firstBlock = int(firstBlock)
	if r.height > 1 {
		ids, err := collectLeafIDs(root, r.height, readBlock)
		if err != nil {
			r.err = err
			return r
		}
		r.leafIDs = ids
		r.nLeaves = len(ids)
	} else {
		// The number of children = 1 + the number of boundary terms in the root.
		nChildren, _, err := countInteriorChildren(root, pos)
		if err != nil {
			r.err = err
			return r
		}
		r.nLeaves = nChildren
	}
	// Load the first leaf lazily (the smallest terms live in the leftmost
	// leaf, so the merge reads blocks only as it advances past them).
	if err := r.loadLeafBlock(0); err != nil {
		r.err = err
	}
	r.nextLeafIdx = 1
	return r
}

// collectLeafIDs enumerates one interior subtree's leaf block ids in order
// (the %_segments block ids of every leaf under the node). The node blob is
// [height][firstChildBlock][boundary terms...]; children sit at consecutive
// block ids and a child's own height byte says leaf (0) or interior layer
// (recursed). Structural breaks surface as "corrupt segment root", the same
// error SQLite's descent produces (fts3SegReaderNext).
func collectLeafIDs(node []byte, height int, readBlock SegmentBlockReader) ([]int, error) {
	pos := 0
	h, n := getFTS3Varint(node)
	if n == 0 || int(h) != height {
		return nil, fmt.Errorf("corrupt segment root")
	}
	pos += n
	firstBlock, n := getFTS3Varint(node[pos:])
	if n == 0 {
		return nil, fmt.Errorf("corrupt segment root")
	}
	pos += n
	nChildren, _, err := countInteriorChildren(node, pos)
	if err != nil {
		return nil, err
	}
	var out []int
	for i := 0; i < nChildren; i++ {
		blockID := int(firstBlock) + i
		block, err := readBlock(blockID)
		if err != nil {
			return nil, err
		}
		bHeight, bn := getFTS3Varint(block)
		if bn == 0 {
			return nil, fmt.Errorf("corrupt segment root")
		}
		if bHeight == 0 {
			out = append(out, blockID)
			continue
		}
		sub, err := collectLeafIDs(block, int(bHeight), readBlock)
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return out, nil
}

// loadLeafBlock loads leaf block index idx (0 = the first leaf) into r.leaf
// and resets r.pos past the leaf's height varint. For a height-0 segment leaf
// 0 is the root blob; for a height-1 interior root the leaves are
// %_segments blocks firstBlock+idx; for taller roots the leaf ids come from
// the pre-enumerated leafIDs (the children are interior layers).
func (r *SegmentStreamReader) loadLeafBlock(idx int) error {
	var block []byte
	if r.height == 0 && idx == 0 {
		block = r.leaf // the root blob was set in NewSegmentStreamReader
	} else {
		blockID := r.firstBlock + idx
		if r.height == 0 {
			blockID = idx
		}
		if r.leafIDs != nil {
			if idx >= len(r.leafIDs) {
				return fmt.Errorf("corrupt segment root")
			}
			blockID = r.leafIDs[idx]
		}
		var err error
		block, err = r.readBlock(blockID)
		if err != nil {
			return err
		}
	}
	bHeight, bn := getFTS3Varint(block)
	if bn == 0 || bHeight != 0 {
		return fmt.Errorf("corrupt segment root")
	}
	r.leaf = block
	r.pos = bn
	r.leafFirst = true
	return nil
}

// Next advances the reader to the next term and returns it, its docIDs, the
// raw doclist bytes (SQLite's encoding, with positions) and the doclist byte
// length. ok is false at EOF. The returned slices are freshly allocated and
// valid until the next call.
func (r *SegmentStreamReader) Next() (term string, docIDs []int64, doclist []byte, doclistSize int, ok bool) {
	if r.err != nil || r.atEOF {
		return "", nil, nil, 0, false
	}
	for {
		if r.pos >= len(r.leaf) {
			if !r.advanceLeaf() {
				r.atEOF = true
				return "", nil, nil, 0, false
			}
		}
		termBytes, okTerm := r.nextLeafTerm()
		if !okTerm {
			r.err = fmt.Errorf("corrupt segment root")
			return "", nil, nil, 0, false
		}
		r.prevTerm = append(r.prevTerm[:0], termBytes...)
		doclist, nDoclist, okDoc := r.nextLeafDoclist()
		if !okDoc {
			r.err = fmt.Errorf("corrupt segment root")
			return "", nil, nil, 0, false
		}
		ids, err := doclistDocIDs(doclist)
		if err != nil {
			r.err = err
			return "", nil, nil, 0, false
		}
		r.term = string(termBytes)
		r.docIDs = ids
		r.doclist = doclist
		r.doclistSize = int(nDoclist)
		//lint:ignore SA4004 return intentionally ends loop after one yielded term
		return r.term, r.docIDs, doclist, r.doclistSize, true
	}
}

// nextLeafTerm reads the leaf's next term (the first is length-prefixed,
// later ones delta-encode against r.prevTerm), updating r.pos/r.leafFirst.
// ok is false on a framing violation.
func (r *SegmentStreamReader) nextLeafTerm() (termBytes []byte, ok bool) {
	var n int
	if r.leafFirst {
		var nLen uint64
		nLen, n = getFTS3Varint(r.leaf[r.pos:])
		if n == 0 {
			return nil, false
		}
		r.pos += n
		if uint64(r.pos)+nLen > uint64(len(r.leaf)) {
			return nil, false
		}
		termBytes = r.leaf[r.pos : r.pos+int(nLen)]
		r.pos += int(nLen)
		r.leafFirst = false
		return termBytes, true
	}
	var nPrefix, nSuffix uint64
	nPrefix, n = getFTS3Varint(r.leaf[r.pos:])
	if n == 0 {
		return nil, false
	}
	r.pos += n
	nSuffix, n = getFTS3Varint(r.leaf[r.pos:])
	if n == 0 || nSuffix == 0 || uint64(nPrefix) > uint64(len(r.prevTerm)) || uint64(r.pos)+nSuffix > uint64(len(r.leaf)) {
		return nil, false
	}
	r.pos += n
	termBytes = make([]byte, nPrefix)
	copy(termBytes, r.prevTerm[:nPrefix])
	termBytes = append(termBytes, r.leaf[r.pos:r.pos+int(nSuffix)]...)
	r.pos += int(nSuffix)
	return termBytes, true
}

// nextLeafDoclist reads the current term's doclist length + bytes (copied),
// updating r.pos. ok is false on a framing violation.
func (r *SegmentStreamReader) nextLeafDoclist() (doclist []byte, nDoclist uint64, ok bool) {
	nDoclist, n := getFTS3Varint(r.leaf[r.pos:])
	if n == 0 {
		return nil, 0, false
	}
	r.pos += n
	if uint64(r.pos)+nDoclist > uint64(len(r.leaf)) {
		return nil, 0, false
	}
	doclist = append([]byte(nil), r.leaf[r.pos:r.pos+int(nDoclist)]...)
	r.pos += int(nDoclist)
	return doclist, nDoclist, true
}

// advanceLeaf loads the next leaf block, or returns false at EOF.
func (r *SegmentStreamReader) advanceLeaf() bool {
	if r.nextLeafIdx >= r.nLeaves {
		return false
	}
	if err := r.loadLeafBlock(r.nextLeafIdx); err != nil {
		r.err = err
		return false
	}
	r.nextLeafIdx++
	return true
}

// AtEOF reports whether the reader has consumed every term of the segment.
func (r *SegmentStreamReader) AtEOF() bool { return r.atEOF || r.err != nil }

// Err returns the first error encountered while reading the segment.
func (r *SegmentStreamReader) Err() error { return r.err }

// Current returns the reader's current (most recently yielded) term, its
// docIDs and its raw doclist, without advancing. After an incremental merge
// stops at its quota, this is the first UNMERGED term of the segment — the
// truncation keeps terms from this position onward (SQLite's
// fts3IncrmergeChomp / fts3TruncateSegment remove keys smaller than the
// reader's current zTerm). The returned slices are valid until the next call
// to Next.
func (r *SegmentStreamReader) Current() (term string, docIDs []int64, doclist []byte) {
	return r.term, r.docIDs, r.doclist
}

// doclistDocIDs parses an FTS3 doclist and returns the docids of the documents
// that match the term (skipping position lists). A doclist entry that is a
// delete marker ([docid][0] with no column/positions) is excluded, mirroring
// SQLite's fts3DeleteTerms semantics.
func doclistDocIDs(doclist []byte) ([]int64, error) {
	if len(doclist) == 0 {
		return nil, fmt.Errorf("corrupt segment root")
	}
	if doclist[len(doclist)-1] != 0 {
		return nil, fmt.Errorf("corrupt segment root")
	}
	s := &doclistIDScanner{}
	return s.scan(doclist)
}

// doclistIDScanner carries the state of a doclist id-collection scan (the
// delete-marker-aware docids of fts3DeleteTerms semantics).
type doclistIDScanner struct {
	docID     int64
	needDocID bool
	// hasPosition/sawColumn/docEnded flag how the current docid's entry is
	// encoded; a docid with NONE of them is a delete marker and is skipped.
	hasPosition, sawColumn, docEnded bool
	ids                              []int64
}

// scan walks the doclist body, collecting the surviving docids.
func (s *doclistIDScanner) scan(doclist []byte) ([]int64, error) {
	for pos := 0; pos < len(doclist); {
		v, n := getFTS3Varint(doclist[pos:])
		if n == 0 {
			// A truncated varint before any docid is corrupt; after valid
			// docids it lazily ends the scan (fts3corrupt4 27.4).
			if s.docID == 0 {
				return nil, fmt.Errorf("corrupt segment root")
			}
			s.flushDoc()
			return s.ids, nil
		}
		pos += n
		next, err := s.step(v, doclist, pos)
		if err != nil {
			return nil, err
		}
		pos = next
	}
	if s.docID != 0 {
		s.flushDoc()
	}
	return s.ids, nil
}

// step consumes one doclist varint v (at pos, for a new-column marker's
// column argument) and returns the position after it.
func (s *doclistIDScanner) step(v uint64, doclist []byte, pos int) (int, error) {
	if s.needDocID {
		if v == 0 && s.docID != 0 {
			// A zero docid delta would repeat the previous docid; the id
			// collector rejects it (fts3corrupt7 1.1 crafted doclists).
			return 0, fmt.Errorf("corrupt segment root")
		}
		if s.docID != 0 {
			s.flushDoc()
		}
		s.docID += int64(v)
		s.needDocID = false
		s.docEnded = false
		return pos, nil
	}
	switch {
	case v == 0:
		// End of this document's positions: the next varint is a docid.
		s.flushDoc()
		s.needDocID = true
	case v == 1:
		// New column: the next varint is the column number (skipped).
		_, cn := getFTS3Varint(doclist[pos:])
		if cn == 0 {
			return 0, fmt.Errorf("corrupt segment root")
		}
		pos += cn
		s.sawColumn = true
	default:
		// A position; skip it.
		s.hasPosition = true
	}
	return pos, nil
}

// flushDoc records the pending docid unless its entry was a delete marker.
func (s *doclistIDScanner) flushDoc() {
	if s.hasPosition || s.sawColumn || s.docEnded {
		s.ids = append(s.ids, s.docID)
	}
	s.hasPosition = false
	s.sawColumn = false
	s.docEnded = true
}

// MergeDoclistsApply merges several FTS3 doclists for the SAME term with
// SQLite's FTS3_SEGMENT_IGNORE_EMPTY semantics (fts3_write.c: when a merge
// output lands above every existing segment of its index, fully-deleted
// documents are DROPPED from the output instead of preserved as bare-docid
// tombstones -- nothing older remains below the output, so applying the
// deletions is safe and keeps merge outputs compact).
func MergeDoclistsApply(doclists ...[]byte) []byte {
	out := MergeDoclists(doclists...)
	if out == nil {
		return nil
	}
	// Walk the merged doclist keeping only POSITIONED documents, then
	// RE-ENCODE docid deltas: dropping a bare entry invalidates every
	// following delta (they are relative to the preceding docid), so the
	// result must be rebuilt from absolute docids.
	keep := keepPositionedDocs(out)
	var res []byte
	var prev int64
	for _, k := range keep {
		res = appendVarint(res, uint64(k.id-prev))
		prev = k.id
		res = append(res, k.body...)
	}
	return res
}

// keptDoc is one merged document retained by keepPositionedDocs: its
// absolute docid and raw position body (including the trailing terminator).
type keptDoc struct {
	id   int64
	body []byte
}

// keepPositionedDocs walks a merged doclist keeping only POSITIONED
// documents; bare delete tombstones are dropped.
func keepPositionedDocs(out []byte) []keptDoc {
	var keep []keptDoc
	pos := 0
	var last int64
	for pos < len(out) {
		v, n := GetFTS3Varint(out[pos:])
		if n == 0 {
			break
		}
		id := last + int64(v)
		last = id
		pos += n
		bodyStart := pos
		end, sawPos := doclistBodyEnd(out, pos)
		pos = end
		if sawPos {
			keep = append(keep, keptDoc{id: id, body: out[bodyStart:pos]})
		}
	}
	return keep
}

// doclistBodyEnd scans one docid's position body (new-column markers and
// positions up to the end-of-doc 0 or the buffer end), reporting whether any
// position/column entry was seen and the position after the body.
func doclistBodyEnd(out []byte, pos int) (int, bool) {
	sawPos := false
	for pos < len(out) {
		v2, n2 := GetFTS3Varint(out[pos:])
		if n2 == 0 {
			break
		}
		if v2 == 0 {
			pos += n2
			break
		}
		if v2 == 1 {
			_, cn := GetFTS3Varint(out[pos:])
			pos += cn
			sawPos = true
			continue
		}
		pos += n2
		sawPos = true
	}
	return pos, sawPos
}

// MergeDoclists merges several FTS3 doclists for the SAME term into one,
// combining position lists for common docids and concatenating distinct
// docids in ascending order (SQLite's fts3DoclistMerge; the incremental
// merge's output for a term is the union of the source segments' postings).
// A delete-marker entry ([docid][0]) removes the docid from the merged result.
// mergePosEntry is one (column, position) hit of a merged document.
type mergePosEntry struct {
	col int
	pos int
}

// mergeDocEntry accumulates one document's postings across the merged
// doclists.
type mergeDocEntry struct {
	docID     int64
	positions []mergePosEntry
	deleted   bool
	// hasMarker records that some input doclist carried an explicit
	// delete entry ([docid][0]) for this document. SQLite's merge writes
	// such documents back as EMPTY position lists (the tombstone
	// survives: fts3_write.c sqlite3Fts3SegReaderStep writes the docid
	// with nList==0 unless FTS3_SEGMENT_IGNORE_EMPTY is set), because
	// OLDER segments at other levels may still hold real postings for
	// the docid — dropping the tombstone here would let a later segment
	// reload resurrect them (integrity-check extra-term failures).
	hasMarker bool
}

func MergeDoclists(doclists ...[]byte) []byte {
	if len(doclists) == 0 {
		return nil
	}
	if len(doclists) == 1 {
		return append([]byte(nil), doclists[0]...)
	}
	entries := map[int64]*mergeDocEntry{}
	var order []int64
	for _, dl := range doclists {
		if err := mergeDoclistHits(entries, &order, dl); err != nil {
			return nil
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	return encodeMergedDoclists(entries, order)
}

// mergeDoclistHits folds one source doclist into the merge maps: postings
// accumulate per (docid, col, pos); a delete marker clears the positions and
// marks the tombstone.
func mergeDoclistHits(entries map[int64]*mergeDocEntry, order *[]int64, dl []byte) error {
	return parseDoclistHits(dl, func(docID int64, col, pos int, isDelete bool) {
		e, ok := entries[docID]
		if !ok {
			e = &mergeDocEntry{docID: docID}
			entries[docID] = e
			*order = append(*order, docID)
		}
		if isDelete {
			e.deleted = true
			e.positions = nil
			e.hasMarker = true
			return
		}
		if e.deleted {
			e.deleted = false
		}
		// Multi-generation segments legitimately carry IDENTICAL
		// postings for the same (docid, col, pos) — an n-way merge of
		// sorted lists yields each hit once (fts3SegReaderStep merges
		// duplicate docids across readers into one position list).
		for _, ex := range e.positions {
			if ex.col == col && ex.pos == pos {
				return
			}
		}
		e.positions = append(e.positions, mergePosEntry{col: col, pos: pos})
	})
}

// encodeMergedDoclists serializes the merged entries in ascending docid
// order with delta-encoded docids.
func encodeMergedDoclists(entries map[int64]*mergeDocEntry, order []int64) []byte {
	var out []byte
	var lastDocID int64
	for _, docID := range order {
		e := entries[docID]
		if len(e.positions) == 0 && !e.hasMarker {
			continue
		}
		out = appendMergedDocEntry(out, docID, e, &lastDocID)
	}
	return out
}

// appendMergedDocEntry encodes one merged document: a tombstone (no
// positions, hasMarker) writes the bare docid with an empty position list;
// otherwise the combined position list follows the docid delta.
func appendMergedDocEntry(out []byte, docID int64, e *mergeDocEntry, lastDocID *int64) []byte {
	out = appendVarint(out, uint64(docID-*lastDocID))
	*lastDocID = docID
	if len(e.positions) == 0 {
		// Tombstone: bare docid, empty position list.
		return appendVarint(out, posEnd)
	}
	lastCol, lastPos := -1, 0
	for _, pe := range e.positions {
		if pe.col > 0 && pe.col != lastCol {
			out = appendVarint(out, posColumn)
			out = appendVarint(out, uint64(pe.col))
			lastCol = pe.col
			lastPos = 0
		}
		out = appendVarint(out, uint64(2+pe.pos-lastPos))
		lastPos = pe.pos
	}
	return appendVarint(out, posEnd)
}

// parseDoclistHits iterates an FTS3 doclist, invoking fn for each (docid,
// column, position) hit; isDelete is true for a delete-marker entry.
func parseDoclistHits(doclist []byte, fn func(docID int64, col, pos int, isDelete bool)) error {
	s := &doclistHitScanner{fn: fn, needDocID: true}
	for pos := 0; pos < len(doclist); {
		v, n := getFTS3Varint(doclist[pos:])
		if n == 0 {
			// A truncated varint lazily ends the scan without error.
			return nil
		}
		pos += n
		next, stop := s.step(v, doclist, pos)
		if stop {
			return nil
		}
		pos = next
	}
	if s.docID != 0 {
		s.flush()
	}
	return nil
}

// doclistHitScanner carries the state of a doclist hit iteration.
type doclistHitScanner struct {
	fn        func(docID int64, col, pos int, isDelete bool)
	docID     int64
	needDocID bool
	// lastCol/lastPos decode positions relative to the previous hit.
	lastCol, lastPos int
	// hasPosition/sawColumn/docEnded flag how the current docid's entry is
	// encoded; a docid with NONE of them is a delete marker.
	hasPosition, sawColumn, docEnded bool
}

// flush emits the pending document's entry when it is a delete marker and
// resets the per-document flags.
func (s *doclistHitScanner) flush() {
	if !s.hasPosition && !s.sawColumn && !s.docEnded {
		s.fn(s.docID, 0, 0, true)
	}
	s.hasPosition = false
	s.sawColumn = false
	s.docEnded = true
}

// step consumes one doclist varint v (at pos, for a new-column marker's
// column argument) and returns the position after it; stop is true when the
// scan must end without error (a truncated column varint).
func (s *doclistHitScanner) step(v uint64, doclist []byte, pos int) (int, bool) {
	if s.needDocID {
		if s.docID != 0 {
			s.flush()
		}
		s.docID += int64(v)
		s.needDocID = false
		s.docEnded = false
		s.lastCol, s.lastPos = 0, 0
		return pos, false
	}
	switch {
	case v == 0:
		// End of this document's positions: the next varint is a docid.
		s.flush()
		s.needDocID = true
	case v == 1:
		// New column: the next varint is the column number.
		col, cn := getFTS3Varint(doclist[pos:])
		if cn == 0 {
			return pos, true
		}
		pos += cn
		s.lastCol = int(col)
		s.lastPos = 0
		s.sawColumn = true
	default:
		// A position: v = 2 + pos - lastPos.
		s.lastPos = int(v) - 2 + s.lastPos
		s.fn(s.docID, s.lastCol, s.lastPos, false)
		s.hasPosition = true
	}
	return pos, false
}
