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
	// %_segments; for an interior root the leaves are the consecutive
	// %_segments blocks [firstBlock, firstBlock+nLeaves).
	height     int
	firstBlock int
	nLeaves    int

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
	// Interior node: first block id, boundary terms, then the leaves are the
	// consecutive blocks starting at firstBlock.
	firstBlock, n := getFTS3Varint(root[pos:])
	if n == 0 {
		r.err = fmt.Errorf("corrupt segment root")
		return r
	}
	pos += n
	r.firstBlock = int(firstBlock)
	// The number of children = 1 + the number of boundary terms in the root.
	nChildren := 1
	if pos < len(root) {
		var prevTerm []byte
		first := true
		for pos < len(root) {
			if first {
				var nLen uint64
				nLen, n = getFTS3Varint(root[pos:])
				if n == 0 {
					r.err = fmt.Errorf("corrupt segment root")
					return r
				}
				pos += n
				if uint64(pos)+nLen > uint64(len(root)) {
					r.err = fmt.Errorf("corrupt segment root")
					return r
				}
				prevTerm = root[pos : pos+int(nLen)]
				pos += int(nLen)
				first = false
			} else {
				var nPrefix, nSuffix uint64
				nPrefix, n = getFTS3Varint(root[pos:])
				if n == 0 {
					r.err = fmt.Errorf("corrupt segment root")
					return r
				}
				pos += n
				nSuffix, n = getFTS3Varint(root[pos:])
				if n == 0 || nSuffix == 0 || uint64(nPrefix) > uint64(len(prevTerm)) || uint64(pos)+nSuffix > uint64(len(root)) {
					r.err = fmt.Errorf("corrupt segment root")
					return r
				}
				pos += n
				term := make([]byte, nPrefix)
				copy(term, prevTerm[:nPrefix])
				term = append(term, root[pos:pos+int(nSuffix)]...)
				prevTerm = term
				pos += int(nSuffix)
			}
			nChildren++
		}
	}
	r.nLeaves = nChildren
	// Load the first leaf lazily (the smallest terms live in the leftmost
	// leaf, so the merge reads blocks only as it advances past them).
	if err := r.loadLeafBlock(0); err != nil {
		r.err = err
	}
	r.nextLeafIdx = 1
	return r
}

// loadLeafBlock loads leaf block index idx (0 = the first leaf) into r.leaf
// and resets r.pos past the leaf's height varint. For a height-0 segment leaf
// 0 is the root blob; otherwise the leaves are %_segments blocks
// firstBlock+idx.
func (r *SegmentStreamReader) loadLeafBlock(idx int) error {
	var block []byte
	if r.height == 0 && idx == 0 {
		block = r.leaf // the root blob was set in NewSegmentStreamReader
	} else {
		blockID := r.firstBlock + idx
		if r.height == 0 {
			blockID = idx
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
		var termBytes []byte
		var n int
		if r.leafFirst {
			var nLen uint64
			nLen, n = getFTS3Varint(r.leaf[r.pos:])
			if n == 0 {
				r.err = fmt.Errorf("corrupt segment root")
				return "", nil, nil, 0, false
			}
			r.pos += n
			if uint64(r.pos)+nLen > uint64(len(r.leaf)) {
				r.err = fmt.Errorf("corrupt segment root")
				return "", nil, nil, 0, false
			}
			termBytes = r.leaf[r.pos : r.pos+int(nLen)]
			r.pos += int(nLen)
			r.leafFirst = false
		} else {
			var nPrefix, nSuffix uint64
			nPrefix, n = getFTS3Varint(r.leaf[r.pos:])
			if n == 0 {
				r.err = fmt.Errorf("corrupt segment root")
				return "", nil, nil, 0, false
			}
			r.pos += n
			nSuffix, n = getFTS3Varint(r.leaf[r.pos:])
			if n == 0 || nSuffix == 0 || uint64(nPrefix) > uint64(len(r.prevTerm)) || uint64(r.pos)+nSuffix > uint64(len(r.leaf)) {
				r.err = fmt.Errorf("corrupt segment root")
				return "", nil, nil, 0, false
			}
			r.pos += n
			termBytes = make([]byte, nPrefix)
			copy(termBytes, r.prevTerm[:nPrefix])
			termBytes = append(termBytes, r.leaf[r.pos:r.pos+int(nSuffix)]...)
			r.pos += int(nSuffix)
		}
		r.prevTerm = append(r.prevTerm[:0], termBytes...)
		var nDoclist uint64
		nDoclist, n = getFTS3Varint(r.leaf[r.pos:])
		if n == 0 {
			r.err = fmt.Errorf("corrupt segment root")
			return "", nil, nil, 0, false
		}
		r.pos += n
		if uint64(r.pos)+nDoclist > uint64(len(r.leaf)) {
			r.err = fmt.Errorf("corrupt segment root")
			return "", nil, nil, 0, false
		}
		doclist = append([]byte(nil), r.leaf[r.pos:r.pos+int(nDoclist)]...)
		r.pos += int(nDoclist)
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
	pos := 0
	var docID int64
	needDocID := true
	hasPosition := false
	sawColumn := false
	docEnded := false
	var ids []int64
	flushDoc := func() {
		if hasPosition || sawColumn || docEnded {
			ids = append(ids, docID)
		}
		hasPosition = false
		sawColumn = false
		docEnded = true
	}
	for pos < len(doclist) {
		v, n := getFTS3Varint(doclist[pos:])
		if n == 0 {
			if docID == 0 {
				return nil, fmt.Errorf("corrupt segment root")
			}
			if docID != 0 {
				flushDoc()
			}
			return ids, nil
		}
		pos += n
		if needDocID {
			if v == 0 && docID != 0 {
				return nil, fmt.Errorf("corrupt segment root")
			}
			if docID != 0 {
				flushDoc()
			}
			docID += int64(v)
			needDocID = false
			docEnded = false
			continue
		}
		if v == 0 {
			flushDoc()
			needDocID = true
			continue
		}
		if v == 1 {
			_, cn := getFTS3Varint(doclist[pos:])
			if cn == 0 {
				return nil, fmt.Errorf("corrupt segment root")
			}
			pos += cn
			sawColumn = true
			continue
		}
		// A position; skip it.
		hasPosition = true
	}
	if docID != 0 {
		flushDoc()
	}
	return ids, nil
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
	type kept struct {
		id   int64
		body []byte // raw position bytes INCLUDING the trailing terminator
	}
	var keep []kept
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
		if sawPos {
			keep = append(keep, kept{id: id, body: out[bodyStart:pos]})
		}
	}
	var res []byte
	var prev int64
	for _, k := range keep {
		res = appendVarint(res, uint64(k.id-prev))
		prev = k.id
		res = append(res, k.body...)
	}
	return res
}

// MergeDoclists merges several FTS3 doclists for the SAME term into one,
// combining position lists for common docids and concatenating distinct
// docids in ascending order (SQLite's fts3DoclistMerge; the incremental
// merge's output for a term is the union of the source segments' postings).
// A delete-marker entry ([docid][0]) removes the docid from the merged result.
func MergeDoclists(doclists ...[]byte) []byte {
	if len(doclists) == 0 {
		return nil
	}
	if len(doclists) == 1 {
		return append([]byte(nil), doclists[0]...)
	}
	type posEntry struct {
		col int
		pos int
	}
	type docEntry struct {
		docID     int64
		positions []posEntry
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
	entries := map[int64]*docEntry{}
	var order []int64
	for _, dl := range doclists {
		if err := parseDoclistHits(dl, func(docID int64, col, pos int, isDelete bool) {
			e, ok := entries[docID]
			if !ok {
				e = &docEntry{docID: docID}
				entries[docID] = e
				order = append(order, docID)
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
			dup := false
			for _, ex := range e.positions {
				if ex.col == col && ex.pos == pos {
					dup = true
					break
				}
			}
			if !dup {
				e.positions = append(e.positions, posEntry{col: col, pos: pos})
			}
		}); err != nil {
			return nil
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	var out []byte
	var lastDocID int64
	for _, docID := range order {
		e := entries[docID]
		if len(e.positions) == 0 {
			if !e.hasMarker {
				continue
			}
			// Tombstone: bare docid, empty position list.
			out = appendVarint(out, uint64(docID-lastDocID))
			lastDocID = docID
			out = appendVarint(out, posEnd)
			continue
		}
		out = appendVarint(out, uint64(docID-lastDocID))
		lastDocID = docID
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
		out = appendVarint(out, posEnd)
	}
	return out
}

// parseDoclistHits iterates an FTS3 doclist, invoking fn for each (docid,
// column, position) hit; isDelete is true for a delete-marker entry.
func parseDoclistHits(doclist []byte, fn func(docID int64, col, pos int, isDelete bool)) error {
	pos := 0
	var docID int64
	needDocID := true
	lastCol, lastPos := 0, 0
	hasPosition := false
	sawColumn := false
	docEnded := false
	flush := func() {
		if !hasPosition && !sawColumn && !docEnded {
			fn(docID, 0, 0, true)
		}
		hasPosition = false
		sawColumn = false
		docEnded = true
	}
	for pos < len(doclist) {
		v, n := getFTS3Varint(doclist[pos:])
		if n == 0 {
			return nil
		}
		pos += n
		if needDocID {
			if docID != 0 {
				flush()
			}
			docID += int64(v)
			needDocID = false
			docEnded = false
			lastCol, lastPos = 0, 0
			continue
		}
		if v == 0 {
			flush()
			needDocID = true
			continue
		}
		if v == 1 {
			col, cn := getFTS3Varint(doclist[pos:])
			if cn == 0 {
				return nil
			}
			pos += cn
			lastCol = int(col)
			lastPos = 0
			sawColumn = true
			continue
		}
		lastPos = int(v) - 2 + lastPos
		fn(docID, lastCol, lastPos, false)
		hasPosition = true
	}
	if docID != 0 {
		flush()
	}
	return nil
}
