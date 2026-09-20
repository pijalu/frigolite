package fts

import (
	"errors"
	"fmt"
	"sort"
)

// SegmentBlockReader returns the %_segments block content for a block id, or
// an error if the block does not exist (a corrupt segment).
// ErrSegmentStructure marks a segment whose node structure (interior/leaf
// heights, block chain) is broken, as opposed to a block whose CONTENT is
// unreadable. A structural break defeats every term lookup, so all MATCH
// queries on the table fail; content damage only fails queries touching the
// damaged terms.
var ErrSegmentStructure = errors.New("corrupt segment structure")

type SegmentBlockReader func(blockID int) ([]byte, error)

// TermEntry is one term of a loaded segment: the term, the doc IDs it appears
// in, and the serialized size of appending its doclist (term header varints +
// term bytes + doclist length varint + doclist bytes). Used by the incremental
// merge to simulate SQLite's leaf-page flush accounting and to determine which
// doc IDs were merged from a partially-consumed segment.
type TermEntry struct {
	Term       string
	DocIDs     []int64
	AppendSize int
}

// TermEntries returns the index's (term, doc-ids, append-size) entries, sorted
// by term. The append size mirrors fts3IncrmergeAppend's nSpace: varint(prefix)
// + varint(suffix) + suffix + varint(doclist length) + doclist (the engine
// rebuilds full terms, so prefix=0).
func (idx *InvertedIndex) TermEntries() []TermEntry {
	terms := make([]string, 0, len(idx.index))
	for term := range idx.index {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	out := make([]TermEntry, 0, len(terms))
	for _, term := range terms {
		postings := idx.index[term]
		doclist := buildDoclist(postings)
		size := FTS3VarintLen(0) + FTS3VarintLen(uint64(len(term))) + len(term) + FTS3VarintLen(uint64(len(doclist))) + len(doclist)
		ids := make([]int64, 0, len(postings))
		var last int64
		for _, p := range postings {
			if p.DocID != last {
				ids = append(ids, p.DocID)
				last = p.DocID
			}
		}
		out = append(out, TermEntry{Term: term, DocIDs: ids, AppendSize: size})
	}
	return out
}

// maxSegmentHeight bounds the segment b-tree descent (see LoadSegment).
const maxSegmentHeight uint64 = 32

// LoadSegmentTermEntries loads one segment (root + %_segments blocks) into a
// fresh index and returns its term entries. A corrupt segment returns an
// error.
func LoadSegmentTermEntries(root []byte, leavesEndBlock int, readBlock SegmentBlockReader) ([]TermEntry, error) {
	tmp := NewInvertedIndex()
	if err := tmp.LoadSegment(root, leavesEndBlock, readBlock); err != nil {
		return nil, err
	}
	return tmp.TermEntries(), nil
}

// FTS3VarintLen returns the number of bytes needed to encode v as an FTS3
// varint (sqlite3Fts3VarintLen: 1 byte for values < 0x80, 2 for < 0x4000,
// etc.). FTS3 stores its varints little-endian with a byte count in the low 3
// bits of the first byte (sqlite3Fts3PutVarint), so the length encoding differs
// from SQLite's record varints for values that need more than one byte.
func FTS3VarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// PrefixCompressedAppendSize returns the number of bytes SQLite's
// fts3IncrmergeAppend charges to the output node when appending term zTerm with
// a doclist of nDoclist bytes after the previous output term prevTerm:
//
//	nSpace = varint(nPrefix) + varint(nSuffix) + nSuffix
//	       + varint(nDoclist) + nDoclist
//
// where nPrefix is the length of the shared prefix of prevTerm and zTerm, and
// nSuffix = len(zTerm) - nPrefix (fts3_write.c fts3IncrmergeAppend). SQLite's
// incremental-merge OUTPUT nodes are prefix-compressed, so consecutive terms
// share their prefix bytes; the engine's term-flush simulation must charge the
// same per-term space or the simulated leaf boundary drifts from the oracle's
// (fts4merge 5.11: the oracle's merge=1,6 packs all 12 source segments' terms
// into one output node while the engine's full-term sizing flushed a leaf 5
// terms early, leaving a level-0 segment behind).
func PrefixCompressedAppendSize(prevTerm, zTerm string, nDoclist int) int {
	nPrefix := 0
	if len(prevTerm) < len(zTerm) {
		nPrefix = len(prevTerm)
	} else {
		nPrefix = len(zTerm)
	}
	for nPrefix > 0 && prevTerm[nPrefix-1] != zTerm[nPrefix-1] {
		nPrefix--
	}
	nSuffix := len(zTerm) - nPrefix
	return FTS3VarintLen(uint64(nPrefix)) + FTS3VarintLen(uint64(nSuffix)) +
		nSuffix + FTS3VarintLen(uint64(nDoclist)) + nDoclist
}

// LoadSegment populates the in-memory index from one real FTS3 segment: the
// %_segdir root blob and, for an interior root, its leaf blocks from
// %_segments. This lets the engine query databases whose FTS index was written
// by real SQLite (fts3corrupt4 crash-dump and deserialize tests).
//
// The reader is deliberately permissive about doclist *content* (SQLite's
// fts3SegReaderNext validates the doclist framing and only requires the last
// byte to be 0 when the doclist is consumed); the term framing
// MergeFrom additively merges another index's loaded postings into idx: every
// posting of other is re-registered (addPosting's exact-triple dedupe keeps
// shared postings single), documents referenced by any posting become visible,
// and corrupt-term / aux-corrupt flags propagate. Delete-marker tombstones are
// NOT replayed — other is expected to be an already-merged load result.
//
// The segment loader uses this to union per-language loads without letting one
// language's tombstones cancel another language's postings (see
// FTS3Table.LoadSegmentsPerLanguage).
func (idx *InvertedIndex) MergeFrom(other *InvertedIndex) {
	for term, postings := range other.index {
		for _, p := range postings {
			idx.addPosting(term, p.DocID, p.Column, p.Position)
		}
	}
	for term := range other.corruptTerms {
		if idx.corruptTerms == nil {
			idx.corruptTerms = make(map[string]bool)
		}
		idx.corruptTerms[term] = true
	}
	if other.auxCorrupt {
		idx.auxCorrupt = true
	}
}

// (prefix/suffix/doclist length) must be structurally valid or the segment is
// reported corrupt ("database disk image is malformed").
func (idx *InvertedIndex) LoadSegment(root []byte, leavesEndBlock int, readBlock SegmentBlockReader) error {
	if len(root) == 0 {
		return nil // an empty segment has no terms
	}
	height, n := getFTS3Varint(root)
	if n == 0 {
		return fmt.Errorf("corrupt segment root")
	}
	// A node height beyond any writable segment b-tree (fts3's layered
	// writer adds one level per ~nLeafEst leaves; real trees stay <= 4) is a
	// crafted chain — SQLite's seek descends it and fails regardless of the
	// queried term (fts3corrupt7 3.x: the 40000-deep interior chain).
	if height > maxSegmentHeight {
		return ErrSegmentStructure
	}
	pos := n
	if height == 0 {
		lastTerm, err := idx.loadLeaf(root, pos, nil)
		if err != nil {
			return err
		}
		// A leaf root is block 0 of the segment. If the segdir claims the
		// segment has more leaf blocks (leaves_end_block > 0), SQLite's
		// fts3SegReaderNext (fts3_write.c) reads blocks 1..leaves_end_block
		// from %_segments after the root is exhausted. A missing block is
		// SQLITE_CORRUPT (fts3corrupt4 31.1: leaves_end_block=1 but %_segments
		// is empty). An unreadable block does NOT abort the load: SQLite
		// reads each term's doclist lazily, so terms in intact blocks stay
		// queryable and only queries touching the damaged range fail
		// (fts3defer2 1.x: after zeroblob()ing the large blocks, MATCH on
		// small-document terms still works). The first error is returned so
		// the caller records the segment as damaged.
		return idx.loadLeafChainBlocks(lastTerm, leavesEndBlock, readBlock)
	}
	// Interior node: descend the b-tree. Children sit at consecutive block
	// ids and may themselves be interior nodes of height-1 (a layered
	// output: SQLite's fts3SegReader descends layer by layer — fts3fuzz001
	// 220: a nodesize=24 merge writes a height-2 root over two layer-1
	// interior blocks), so the walk recurses until it reaches leaves.
	_, err := idx.loadInterior(root, height, pos, nil, readBlock)
	return err
}

// loadLeafChainBlocks reads a leaf-root segment's remaining leaf blocks
// 1..leavesEndBlock from %_segments, continuing the leaf chain's term
// stream. An unreadable or non-leaf block records the first error and
// resets the chain term but does not abort the load; the first error is
// returned so the caller records the segment as damaged.
func (idx *InvertedIndex) loadLeafChainBlocks(lastTerm []byte, leavesEndBlock int, readBlock SegmentBlockReader) error {
	var firstErr error
	for i := 1; i <= leavesEndBlock; i++ {
		loaded, err := idx.loadLeafChainBlock(lastTerm, i, readBlock)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			lastTerm = nil
			continue
		}
		lastTerm = loaded
	}
	return firstErr
}

// loadLeafChainBlock loads one chained leaf block (id blockID): a missing or
// non-leaf block is a corrupt segment; a lower-level load failure propagates.
// An errored block resets the chain term (nil result).
func (idx *InvertedIndex) loadLeafChainBlock(lastTerm []byte, blockID int, readBlock SegmentBlockReader) ([]byte, error) {
	block, err := readBlock(blockID)
	if err != nil {
		return nil, err
	}
	bHeight, bn := getFTS3Varint(block)
	if bn == 0 || bHeight != 0 {
		return nil, fmt.Errorf("corrupt segment root")
	}
	term, err := idx.loadLeaf(block, bn, lastTerm)
	if err != nil {
		return nil, err
	}
	return term, nil
}

// loadInterior walks one interior node of a segment b-tree (height >= 1):
// the node blob is [height][firstChildBlock][boundary terms...]. There is
// one boundary term per child after the first (N children -> N-1
// boundaries), so nChildren = 1 + number of boundaries read. Children sit
// at consecutive block ids from firstBlock; the leaf chain is a continuous
// delta-encoded term stream, so lastTerm carries across the whole walk.
func (idx *InvertedIndex) loadInterior(blob []byte, height uint64, pos int, lastTerm []byte, readBlock SegmentBlockReader) ([]byte, error) {
	firstBlock, n := getFTS3Varint(blob[pos:])
	if n == 0 {
		return nil, fmt.Errorf("corrupt segment root")
	}
	pos += n
	// Parse the boundary terms to know the number of children.
	nChildren, _, err := countInteriorChildren(blob, pos)
	if err != nil {
		return nil, err
	}
	var firstErr error
	for i := 0; i < nChildren; i++ {
		blockID := int(firstBlock) + i
		block, err := readBlock(blockID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			lastTerm = nil
			continue
		}
		loaded, cerr := idx.loadInteriorChild(block, height, lastTerm, readBlock)
		if cerr != nil {
			if firstErr == nil {
				firstErr = cerr
			}
			lastTerm = nil
			continue
		}
		lastTerm = loaded
	}
	return lastTerm, firstErr
}

// loadInteriorChild loads one child block of an interior node: height 0 is a
// leaf; height-1 is a lower interior layer descended recursively. Any other
// height means the segment b-tree chain is structurally broken; SQLite's
// seek descends it and fails regardless of the queried term (fts3corrupt7
// 3.x: a 40000-deep interior chain).
func (idx *InvertedIndex) loadInteriorChild(block []byte, height uint64, lastTerm []byte, readBlock SegmentBlockReader) ([]byte, error) {
	bHeight, bn := getFTS3Varint(block)
	if bn == 0 {
		return nil, fmt.Errorf("corrupt segment root")
	}
	if bHeight == 0 {
		return idx.loadLeaf(block, bn, lastTerm)
	}
	if bHeight != height-1 {
		return nil, ErrSegmentStructure
	}
	return idx.loadInterior(block, bHeight, bn, lastTerm, readBlock)
}

// countInteriorChildren parses an interior node's boundary terms (the first
// length-prefixed, the rest delta-encoded) and returns the child count: one
// boundary per child after the first. A framing violation is a corrupt
// segment root.
func countInteriorChildren(blob []byte, pos int) (nChildren, next int, err error) {
	nChildren = 1
	var prevTerm []byte
	first := true
	for pos < len(blob) {
		var term []byte
		var ok bool
		if first {
			term, pos, ok = readValidatedPrefixedTerm(blob, pos)
		} else {
			term, pos, ok = readValidatedLeafDeltaTerm(blob, pos, prevTerm)
		}
		if !ok {
			return 0, pos, fmt.Errorf("corrupt segment root")
		}
		prevTerm = term
		first = false
		nChildren++
	}
	return nChildren, pos, nil
}

// loadLeaf parses a leaf node's terms (delta-encoded after the first) and adds
// their postings to the index. Each leaf node is SELF-CONTAINED: its first
// term is length-prefixed (SQLite writes the first term of a new leaf with
// nPrefix=0 and the full term — fts3_write.c fts3SegWriterFlush; the
// engine's serializeLeafNode does the same), and subsequent terms are
// delta-encoded relative to the leaf's own previous term. prevTerm is only
// used for the return value (the last term of the leaf chain, needed by the
// caller for the next leaf's boundary tracking — SQLite's nodeReader keeps
// the term across nodes, but the leaf format does not delta from it).
func (idx *InvertedIndex) loadLeaf(leaf []byte, pos int, prevTerm []byte) ([]byte, error) {
	if pos >= len(leaf) {
		return prevTerm, nil
	}
	var prev []byte
	first := true
	for pos < len(leaf) {
		term, next, hard, ok := readLeafChainTerm(leaf, pos, prev, first)
		if !ok {
			if hard {
				return prev, fmt.Errorf("corrupt segment root")
			}
			// A term whose framing overruns the node ends the readable
			// prefix of this segment. SQLite serves phrase queries via
			// segment b-tree term lookup (fts3SegReaderNew bLookup), so
			// the damaged term never surfaces unless queried; keep the
			// terms loaded so far (fts3corrupt7 1.1). The damaged term's
			// name is unknowable, so nothing is added to corruptTerms.
			return prev, nil
		}
		pos = next
		first = false
		doclist, dnext, okDoc := readLeafDoclistBytes(leaf, pos)
		if !okDoc {
			return prev, fmt.Errorf("corrupt segment root")
		}
		idx.recordLeafTerm(string(term), doclist)
		pos = dnext
		prev = term
	}
	return prev, nil
}

// readLeafDoclistBytes reads a leaf term's doclist length + bytes, comparing
// without addition: a near-2^64 doclist length would wrap the sum and pass
// the bounds check (fts3cov 17.x crafted root).
func readLeafDoclistBytes(leaf []byte, pos int) ([]byte, int, bool) {
	nDoclist, n := getFTS3Varint(leaf[pos:])
	if n == 0 {
		return nil, pos, false
	}
	pos += n
	if nDoclist > uint64(len(leaf)-pos) {
		return nil, pos, false
	}
	return leaf[pos : pos+int(nDoclist)], pos + int(nDoclist), true
}

// recordLeafTerm loads one term's doclist into the index, recording the term
// as corrupt (valid framing, damaged content) when the load fails. The
// recording keeps loading subsequent terms (their doclists are independent;
// fts3corrupt4 11.1 queries 'e*' which must fail, while 13.1's 'e*' on a
// segment with a corrupt unqueried term must succeed).
func (idx *InvertedIndex) recordLeafTerm(term string, doclist []byte) {
	if err := idx.loadDoclist(term, doclist); err != nil {
		if idx.corruptTerms == nil {
			idx.corruptTerms = make(map[string]bool)
		}
		idx.corruptTerms[term] = true
	}
}

// readLeafChainTerm reads one leaf term: the first is length-prefixed (a
// full term), subsequent terms delta-encode against prev. hard marks a fatal
// framing violation ("corrupt segment root"); a non-hard miss is the lenient
// delta overrun that ends the leaf's readable prefix.
func readLeafChainTerm(leaf []byte, pos int, prev []byte, first bool) (term []byte, next int, hard, ok bool) {
	if first {
		nLen, n := getFTS3Varint(leaf[pos:])
		if n == 0 {
			return nil, pos, true, false
		}
		pos += n
		if uint64(pos)+nLen > uint64(len(leaf)) {
			return nil, pos, true, false
		}
		term = leaf[pos : pos+int(nLen)]
		// A leaf's first term is length-prefixed (full term); subsequent
		// terms delta-encode against it.
		return append([]byte(nil), term...), pos + int(nLen), false, true
	}
	nPrefix, n := getFTS3Varint(leaf[pos:])
	if n == 0 {
		return nil, pos, true, false
	}
	pos += n
	nSuffix, n := getFTS3Varint(leaf[pos:])
	if n == 0 || uint64(nPrefix) > uint64(len(prev)) || nSuffix > uint64(len(leaf)) || uint64(pos)+nSuffix > uint64(len(leaf)) {
		return nil, pos, false, false
	}
	pos += n
	term = make([]byte, nPrefix)
	copy(term, prev[:nPrefix])
	term = append(term, leaf[pos:pos+int(nSuffix)]...)
	return term, pos + int(nSuffix), false, true
}

// loadDoclist parses an FTS3 doclist (delta-encoded docids, then position
// lists with 1=new-column, 0=end-of-doc) and adds each hit to the index.
func (idx *InvertedIndex) loadDoclist(term string, doclist []byte) error {
	if len(doclist) == 0 {
		// A zero-length doclist is a term with no postings — the era's
		// reader accepts it and simply yields no rows (fts3corrupt7 1.1).
		return nil
	}
	// SQLite requires the final byte of a doclist to be 0x00 (an end-of-doc
	// marker); a non-zero tail means the doclist is corrupt
	// (fts3_write.c fts3SegReaderNext: "the final byte of the doclist is 0x00.
	// If either of these statements is untrue, then the data structure is
	// corrupt"). fts3corrupt4 31.1: the hand-crafted root's 'm' term has a
	// doclist ending in a non-zero byte; real SQLite rejects the MATCH.
	if doclist[len(doclist)-1] != 0 {
		return fmt.Errorf("corrupt segment root")
	}
	sc := &doclistScanner{idx: idx, term: term, needDocID: true}
	return sc.scan(doclist)
}

// doclistScanner carries the in-flight state of a doclist position scan
// (delta docids, then 1=new-column / 0=end / 2+delta position varints).
type doclistScanner struct {
	idx  *InvertedIndex
	term string
	// docID is the document id of the entry being scanned.
	docID int64
	// needDocID tracks whether the next varint is a docid delta (the first
	// varint, and every varint after an end-of-doc 0).
	needDocID        bool
	lastPos, lastCol int
	// hasPosition tracks whether the current docid has any position posted.
	// A docid with an empty or column-only position list (fts3corrupt4 15.x:
	// doclist "07 01 00" = docid 7, new column 0, no positions) still matches
	// the term in SQLite (the offset list is skipped, only the docid is read).
	hasPosition bool
	// sawColumn tracks whether a new-column marker (varint 1) was seen for the
	// current docid. A doclist entry whose docid is followed IMMEDIATELY by an
	// end-of-doc 0 (no column marker, no positions) is a DELETE marker: the
	// document is removed from the term's postings (fts3.c fts3DeleteTerms
	// writes [docid][0] doclists; fts4content 3.1.5 after DELETE FROM ft3).
	sawColumn bool
	// docEnded tracks whether the current docid's postings were already
	// flushed by an explicit end-of-doc marker (varint 0). A well-formed
	// doclist's final byte is 0, so the trailing flush after the loop must
	// NOT re-interpret the already-flushed doc as a delete marker.
	docEnded bool
}

// scan walks the doclist body.
func (sc *doclistScanner) scan(doclist []byte) error {
	pos := 0
	for pos < len(doclist) {
		v, n := getFTS3Varint(doclist[pos:])
		if n == 0 {
			return sc.truncatedTail()
		}
		pos += n
		next, err := sc.step(v, doclist, pos)
		if err != nil {
			return err
		}
		pos = next
	}
	if sc.docID != 0 {
		sc.flushDoc()
	}
	return nil
}

// step consumes one doclist varint v (at pos, for the new-column marker's
// column argument) and returns the position after it.
func (sc *doclistScanner) step(v uint64, doclist []byte, pos int) (int, error) {
	if sc.needDocID {
		if v == 0 && sc.docID != 0 {
			// A zero docid delta repeats the previous docid. Era SQLite
			// processed it without a corruption check (fts3corrupt7 1.1
			// crafted doclists rely on the tolerance); flush the pending
			// posting and keep scanning.
			sc.flushDoc()
			return pos, nil
		}
		if sc.docID != 0 {
			sc.flushDoc()
		}
		sc.docID += int64(v)
		sc.needDocID = false
		sc.docEnded = false
		sc.lastCol = 0
		sc.lastPos = 0
		return pos, nil
	}
	switch {
	case v == 0:
		// End of this document's positions: the next varint is a docid.
		sc.flushDoc()
		sc.needDocID = true
	case v == 1:
		// New column: the next varint is the column number.
		next, err := sc.newColumn(doclist, pos)
		if err != nil {
			return 0, err
		}
		pos = next
	default:
		// A position: v = 2 + pos - lastPos.
		sc.lastPos = int(v) - 2 + sc.lastPos
		sc.idx.addPosting(sc.term, sc.docID, sc.lastCol, sc.lastPos)
		sc.hasPosition = true
	}
	return pos, nil
}

// truncatedTail handles a truncated varint (a continuation byte with no stop
// byte). If it occurs before any docid was read (the very first varint, or
// after an end-of-doc when a new docid is expected), the doclist is corrupt
// and the term cannot be read — SQLite's fts3SegReaderFirstDocid fails on
// the unreadable docid. A truncated tail AFTER valid docids is tolerated
// lazily: the docids already seen still match, and the corrupt tail only
// breaks a query that needs the positions (fts3corrupt4 27.4).
func (sc *doclistScanner) truncatedTail() error {
	if sc.docID == 0 {
		return fmt.Errorf("corrupt segment root")
	}
	sc.flushDoc()
	return nil
}

// newColumn consumes a new-column marker's column number. A column beyond
// the table's column count is tolerated by ordinary MATCH reads (the
// postings are skipped) but fails fts4aux, whose term merge validates every
// column against nColumn (fts3corrupt7 4.x/4.4 vs 8.1).
func (sc *doclistScanner) newColumn(doclist []byte, pos int) (int, error) {
	col, n := getFTS3Varint(doclist[pos:])
	if n == 0 {
		return 0, fmt.Errorf("corrupt segment root")
	}
	if sc.idx.nCol > 0 && col >= uint64(sc.idx.nCol) {
		sc.idx.auxCorrupt = true
	}
	pos += n
	sc.lastCol = int(col)
	sc.lastPos = 0
	sc.sawColumn = true
	return pos, nil
}

// flushDoc posts the pending document's entry.
func (sc *doclistScanner) flushDoc() {
	if sc.hasPosition {
		// Normal posting(s).
	} else if sc.sawColumn {
		// Column-only entry (docid + column marker, no positions): a
		// regular posting at column 0 (fts3corrupt4 15.x).
		sc.idx.addPosting(sc.term, sc.docID, 0, 0)
	} else if !sc.docEnded {
		// Delete marker: remove the document from this term and from the
		// index entirely (fts3.c fts3DeleteTerms writes [docid][0]
		// doclists). Only applies when the docid was NOT already ended by
		// an explicit end-of-doc marker.
		sc.idx.deleteDocFromTerm(sc.term, sc.docID)
	}
	sc.hasPosition = false
	sc.sawColumn = false
	sc.docEnded = true
}

// addPosting records one (term, docid, column, position) hit in the index,
// skipping a hit already present (the same segment data may be loaded from
// both %_content and %_segments for an FTS4 table).
func (idx *InvertedIndex) addPosting(term string, docID int64, col, pos int) {
	for _, p := range idx.index[term] {
		if p.DocID == docID && p.Column == col && p.Position == pos {
			return
		}
	}
	idx.index[term] = append(idx.index[term], Posting{
		DocID:    docID,
		Column:   col,
		Position: pos,
	})
	// Ensure the document is registered (even with no content columns) so
	// docid lookups and rowid scans see it. The engine's SELECT reads FTS
	// rows through the in-memory store; for a real-SQLite FTS3 table there is
	// no content to recover, so columns are empty strings.
	if _, ok := idx.docs[docID]; !ok {
		idx.docs[docID] = &Document{DocID: docID, Columns: nil}
		if docID >= idx.nextID {
			idx.nextID = docID + 1
		}
	}
}

// deleteDocFromTerm removes a document from one term's postings (a segment
// delete marker, fts3.c fts3DeleteTerms). When the document has no remaining
// postings anywhere in the index it is removed from the document map too, so
// rowid scans and MATCH don't see a deleted document.
func (idx *InvertedIndex) deleteDocFromTerm(term string, docID int64) {
	postings := idx.index[term]
	filtered := postings[:0]
	for _, p := range postings {
		if p.DocID != docID {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		delete(idx.index, term)
	} else {
		idx.index[term] = filtered
	}
	// Check whether the document has any other postings.
	for _, postings := range idx.index {
		for _, p := range postings {
			if p.DocID == docID {
				return // still present in another term
			}
		}
	}
	delete(idx.docs, docID)
	delete(idx.docSizes, docID)
}
