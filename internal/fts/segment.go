package fts

import (
	"fmt"
	"sort"
)

// Segment format constants from SQLite fts3.c.
const (
	leafMaxBytes = 2048 // LEAF_MAX: a leaf node never exceeds this (used when the page size is unavailable)
	posEnd       = 0    // marks end of a document's positions
	posColumn    = 1    // marks start of a new column's positions
)

// buildDoclist serializes a term's postings into the FTS3 doclist format
// (fts3_write.c fts3PendingListAppend): for each document, the delta-encoded
// docid, then the position list (column-0 positions delta-encoded as
// 2+delta, a new column as [1, colNum] followed by its positions, and a
// trailing 0). Postings must be sorted by (docid, column, position).
func buildDoclist(postings []Posting) []byte {
	var out []byte
	var lastDocID int64
	var lastCol, lastPos int
	wroteDoc := false
	forceNewEntry := false
	for _, p := range postings {
		if !wroteDoc || p.DocID != lastDocID || forceNewEntry {
			if wroteDoc {
				out = appendVarint(out, posEnd)
			}
			out = appendVarint(out, uint64(p.DocID-lastDocID))
			lastDocID = p.DocID
			wroteDoc = true
			lastCol = -1
			lastPos = 0
		}
		if p.Delete {
			// Position-less delete-marker entry: [docid][posEnd]. The next
			// posting for the SAME docid (a re-inserted REPLACE document)
			// opens a fresh entry with delta 0 (SQLite pending-flush doclists
			// carry both the deletion and the re-insertion of one docid).
			out = appendVarint(out, posEnd)
			forceNewEntry = true
			continue
		}
		forceNewEntry = false
		if p.Column > 0 && p.Column != lastCol {
			out = appendVarint(out, posColumn)
			out = appendVarint(out, uint64(p.Column))
			lastCol = p.Column
			lastPos = 0
		}
		if p.Column >= 0 {
			out = appendVarint(out, uint64(2+p.Position-lastPos))
			lastPos = p.Position
		}
	}
	if wroteDoc {
		out = appendVarint(out, posEnd)
	}
	return out
}

// TermRecord is one (term, doclist) pair in a leaf node.
type TermRecord struct {
	Term    string
	Doclist []byte
}

func (r TermRecord) Key() string { return r.Term }

// termRecord is one (term, doclist) pair in a leaf node.
type termRecord struct {
	term    string
	doclist []byte
}

// serializeLeafNode writes a leaf node (fts3.c: varint iHeight=0, then the
// first term un-prefixed, subsequent terms delta-encoded).
func serializeLeafNode(records []termRecord) []byte {
	var out []byte
	out = appendVarint(out, 0) // iHeight (leaf)
	for i, rec := range records {
		if i == 0 {
			out = appendVarint(out, uint64(len(rec.term)))
			out = append(out, rec.term...)
		} else {
			prev := records[i-1].term
			prefix := commonPrefixLen(prev, rec.term)
			suffix := rec.term[prefix:]
			out = appendVarint(out, uint64(prefix))
			out = appendVarint(out, uint64(len(suffix)))
			out = append(out, suffix...)
		}
		out = appendVarint(out, uint64(len(rec.doclist)))
		out = append(out, rec.doclist...)
	}
	return out
}

// serializeInteriorNode writes an interior node (fts3.c): varint iHeight (>0),
// varint iBlockid (first subtree block), then the boundary terms (first
// subtree's first term un-prefixed, then delta-encoded boundaries).
func serializeInteriorNode(height int, firstBlock int, boundaries []string) []byte {
	var out []byte
	out = appendVarint(out, uint64(height))
	out = appendVarint(out, uint64(firstBlock))
	for i, b := range boundaries {
		if i == 0 {
			out = appendVarint(out, uint64(len(b)))
			out = append(out, b...)
		} else {
			prev := boundaries[i-1]
			prefix := commonPrefixLen(prev, b)
			suffix := b[prefix:]
			out = appendVarint(out, uint64(prefix))
			out = appendVarint(out, uint64(len(suffix)))
			out = append(out, suffix...)
		}
	}
	return out
}

// ParseSegmentRootBounds decodes an interior segment root's structure: its
// height, the first leaf block id, and the boundary terms (the first term of
// each leaf after the first). A leaf root (height 0) has no first-block or
// boundaries. Used by the incremental-merge continuation to rebuild the root
// over the EXISTING leaves plus the newly appended ones without rewriting the
// existing leaves (SQLite's fts3IncrmergeLoad keeps them in place).
func ParseSegmentRootBounds(root []byte) (height, firstBlock int, boundaries []string) {
	pos := 0
	h, n := getFTS3Varint(root[pos:])
	if n == 0 {
		return 0, 0, nil
	}
	pos += n
	height = int(h)
	if height == 0 || pos >= len(root) {
		return height, 0, nil
	}
	fb, n := getFTS3Varint(root[pos:])
	if n == 0 {
		return height, 0, nil
	}
	pos += n
	firstBlock = int(fb)
	var prevTerm []byte
	first := true
	for pos < len(root) {
		var nLen, nPrefix, nSuffix uint64
		var n int
		if first {
			nLen, n = getFTS3Varint(root[pos:])
			if n == 0 || uint64(pos)+nLen > uint64(len(root)) {
				break
			}
			pos += n
			prevTerm = root[pos : pos+int(nLen)]
			pos += int(nLen)
		} else {
			nPrefix, n = getFTS3Varint(root[pos:])
			if n == 0 {
				break
			}
			pos += n
			nSuffix, n = getFTS3Varint(root[pos:])
			if n == 0 || uint64(pos)+nSuffix > uint64(len(root)) {
				break
			}
			pos += n
			term := make([]byte, 0, int(nPrefix)+int(nSuffix))
			term = append(term, prevTerm[:int(nPrefix)]...)
			term = append(term, root[pos:pos+int(nSuffix)]...)
			prevTerm = term
			pos += int(nSuffix)
		}
		boundaries = append(boundaries, string(prevTerm))
		first = false
	}
	return height, firstBlock, boundaries
}

// BuildInteriorRoot builds an interior segment root over leaf blocks whose
// first block id is firstBlock and whose boundaries (the first term of each
// leaf after the first) are the given terms. Used by the incremental-merge
// continuation to extend an existing output's root with the newly appended
// leaves' boundaries (SQLite's fts3IncrmergePush).
func BuildInteriorRoot(firstBlock int, boundaries []string) []byte {
	return serializeInteriorNode(1, firstBlock, boundaries)
}

// RewriteSegmentRootFirstBlock returns a copy of an interior segment root with
// its first-block varint replaced by newFirstBlock. The engine's segment
// writer assigns leaf blocks internal IDs 1..N (serializeSegmentBlocks), but
// the %_segments shadow rows are stored at the table's global block counter
// (nextFTSBlockID), so a non-first segment's root must be patched to point at
// the actual stored block IDs (fts3.c fts3WriteSegdir stores the writer's real
// block ids in the node). A leaf root (height 0) has no first-block field.
func RewriteSegmentRootFirstBlock(root []byte, newFirstBlock int) []byte {
	if len(root) == 0 {
		return root
	}
	_, n := getFTS3Varint(root)
	if n == 0 {
		return root
	}
	height := root[:n]
	rest := root[n:]
	_, fbN := getFTS3Varint(rest)
	if fbN == 0 {
		return root
	}
	boundaries := rest[fbN:]
	out := append([]byte(nil), height...)
	out = putFTS3Varint(out, uint64(newFirstBlock))
	out = append(out, boundaries...)
	return out
}

func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// Segment nodes, doclists, %_segdir root blobs and the %_stat hint blob all
// use FTS3's own varint encoding (fts3.c sqlite3Fts3PutVarint/
// sqlite3Fts3GetVarintU): LITTLE-ENDIAN base-128 — each byte carries the
// NEXT 7 low-order bits of the value, with the high bit set on every byte
// except the last. This is deliberately NOT the record-format varint of
// util.GetVarint (which is big-endian); mixing the two codecs corrupts any
// blob that crosses them.
func putFTS3Varint(buf []byte, v uint64) []byte {
	var tmp [10]byte
	n := 0
	for {
		tmp[n] = byte(v & 0x7f)
		v >>= 7
		n++
		if v == 0 {
			break
		}
	}
	for i := 0; i < n-1; i++ {
		tmp[i] |= 0x80
	}
	return append(buf, tmp[:n]...)
}

// getFTS3Varint decodes an FTS3 varint starting at buf[0], returning the
// value and the number of bytes consumed (0 on truncation). Ported from
// fts3.c sqlite3Fts3GetVarintU: little-endian base-128, each byte's low 7
// bits accumulate at increasing shift; a clear high bit terminates.
func getFTS3Varint(buf []byte) (uint64, int) {
	var v uint64
	for n := 0; n < len(buf); n++ {
		b := buf[n]
		v |= uint64(b&0x7f) << (7 * uint(n))
		if b < 0x80 {
			return v, n + 1
		}
		if n == 9 {
			break // 10 bytes without terminator: corrupt
		}
	}
	return 0, 0
}

// GetFTS3Varint is the exported form of getFTS3Varint (fts3.c
// sqlite3Fts3GetVarintBounded): it decodes an FTS3 varint starting at buf[0]
// and returns the value plus the number of bytes consumed. A truncated or
// overflowing varint returns (0, 0), which the caller treats as corruption
// ("database disk image is malformed").
func GetFTS3Varint(buf []byte) (uint64, int) {
	return getFTS3Varint(buf)
}

// appendVarint writes v in the FTS3 varint encoding.
func appendVarint(buf []byte, v uint64) []byte {
	return putFTS3Varint(buf, v)
}

// AppendFTS3Varint appends v in the FTS3 varint encoding (fts3.c
// sqlite3Fts3PutVarint). Exported for the docsize/stat shadow-table writers.
func AppendFTS3Varint(buf []byte, v uint64) []byte {
	return putFTS3Varint(buf, v)
}

// segmentLeaf is one leaf block of a segment.
type segmentLeaf struct {
	blockID   int
	records   []termRecord
	firstTerm string
	lastTerm  string
}

// SegmentBlock is one leaf block of a segment, stored in the %_segments
// shadow table (blockid, block).
type SegmentBlock struct {
	BlockID int
	Block   []byte
}

// serializeSegmentBlocks builds the segment root blob AND the leaf blocks
// (for the %_segments table). SQLite stores multi-block segments as leaf
// blocks in %_segments with the root (top node) in %_segdir.root.
// SerializeSegmentForTerms serializes a segment from (term → raw doclist)
// pairs, packing records into leaf nodes (SQLite's fts3SegWriter). This is the
// incremental merge's output writer: the merged term's doclist (from the
// source segments' doclists, via MergeDoclists) is written verbatim — exactly
// what fts3IncrmergeAppend does — without consulting the live in-memory index
// (whose postings may not cover flushed segments).
func SerializeSegmentForTerms(termDoclists map[string][]byte, nodeSize int) ([]byte, []SegmentBlock) {
	terms := make([]string, 0, len(termDoclists))
	for term := range termDoclists {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	records := make([]termRecord, 0, len(terms))
	for _, term := range terms {
		records = append(records, termRecord{term: term, doclist: termDoclists[term]})
	}
	return serializeSegmentBlocks(records, nodeSize)
}

func serializeSegmentBlocks(records []termRecord, nodeSize int) ([]byte, []SegmentBlock) {
	if nodeSize <= 0 {
		nodeSize = leafMaxBytes
	}
	var leaves []segmentLeaf
	var cur segmentLeaf
	cur.blockID = 1
	for _, rec := range records {
		prefix := 0
		if len(cur.records) > 0 {
			prefix = commonPrefixLen(cur.lastTerm, rec.term)
		}
		suffix := len(rec.term) - prefix
		nSpace := varintSize(uint64(prefix)) + varintSize(uint64(suffix)) + suffix +
			varintSize(uint64(len(rec.doclist))) + len(rec.doclist)
		if len(cur.records) > 0 && leafSize(cur)+nSpace > nodeSize {
			leaves = append(leaves, cur)
			cur = segmentLeaf{blockID: cur.blockID + 1, firstTerm: rec.term, lastTerm: rec.term}
		}
		if cur.firstTerm == "" {
			cur.firstTerm = rec.term
		}
		cur.lastTerm = rec.term
		cur.records = append(cur.records, rec)
	}
	if len(cur.records) > 0 {
		leaves = append(leaves, cur)
	}

	if len(leaves) == 0 {
		return serializeLeafNode(nil), nil
	}
	if len(leaves) == 1 {
		// A single-leaf segment: the root is the leaf, no %_segments rows.
		return serializeLeafNode(leaves[0].records), nil
	}

	// Multiple leaves: store each leaf in %_segments and build an interior
	// root referencing them.
	blocks := make([]SegmentBlock, 0, len(leaves))
	for _, l := range leaves {
		blocks = append(blocks, SegmentBlock{BlockID: l.blockID, Block: serializeLeafNode(l.records)})
	}
	// Boundary terms are TRUNCATED SEPARATORS (fts3_write.c
	// fts3SegWriterAddBlock → fts3NodeAddTerm(..., zTerm, nPrefix+1)): the
	// new leaf's first term cut to the common prefix it shares with the
	// PREVIOUS leaf's last term, plus one byte. Full first-terms make the
	// root blob diverge from SQLite's byte-for-byte (fts4growth 2.x).
	boundaries := make([]string, 0, len(leaves)-1)
	for i := 1; i < len(leaves); i++ {
		prev := leaves[i-1].lastTerm
		cp := commonPrefixLen(prev, leaves[i].firstTerm)
		cut := cp + 1
		if cut > len(leaves[i].firstTerm) {
			cut = len(leaves[i].firstTerm)
		}
		boundaries = append(boundaries, leaves[i].firstTerm[:cut])
	}
	root := serializeInteriorNode(1, leaves[0].blockID, boundaries)
	return root, blocks
}

// leafSize estimates the serialized size of a leaf's current records.
func leafSize(l segmentLeaf) int {
	if len(l.records) == 0 {
		return 0
	}
	n := 1 // iHeight varint
	for i, rec := range l.records {
		if i == 0 {
			n += varintSize(uint64(len(rec.term))) + len(rec.term)
		} else {
			p := commonPrefixLen(l.records[i-1].term, rec.term)
			sfx := len(rec.term) - p
			n += varintSize(uint64(p)) + varintSize(uint64(sfx)) + sfx
		}
		n += varintSize(uint64(len(rec.doclist))) + len(rec.doclist)
	}
	return n
}

func varintSize(v uint64) int {
	if v == 0 {
		return 1
	}
	n := 0
	for v > 0 {
		n++
		v >>= 7
	}
	return n
}

// ValidateSegmentRoot parses a segment root blob and reports whether it is
// structurally valid (fts3.c leaf/interior node format). The corruption tests
// (fts3corrupt*) modify the %_segdir.root column and expect FTS operations to
// fail with "database disk image is malformed" when the structure is broken.
func ValidateSegmentRoot(root []byte) error {
	if len(root) == 0 {
		return nil
	}
	pos := 0
	height, n := getFTS3Varint(root[pos:])
	if n == 0 {
		return fmt.Errorf("corrupt segment root")
	}
	pos += n
	if height == 0 {
		return validateLeafNode(root, pos)
	}
	return validateInteriorNode(root, pos)
}

// validateDoclist checks a doclist the way SQLite's fts3SegReaderNext does
// (fts3_write.c fts3SegReaderNext): a non-empty doclist. SQLite validates the
// final POS_END byte only when the doclist is actually consumed; a segment
// whose framing is valid but whose doclist content is damaged is accepted
// until a query reads that term (fts3corrupt4 13.1: a crash-written segment
// has one corrupt term doclist, but queries on other terms succeed). The
// engine mirrors that by checking only the framing here; the segment loader
// surfaces doclist corruption when it reads a term.
func validateDoclist(doclist []byte) error {
	if len(doclist) == 0 {
		return fmt.Errorf("corrupt doclist")
	}
	return nil
}

// validateLeafNode parses a leaf node: the first term (length + bytes), then
// repeated delta-encoded terms, each followed by a doclist length + bytes. An
// empty leaf (just the height byte) is valid.
func validateLeafNode(root []byte, pos int) error {
	if pos >= len(root) {
		return nil
	}
	// Validate only the entry region (the first two terms). SQLite reads a
	// segment lazily: corruption in a later term only fails queries that read
	// it (fts3corrupt4 24.x). The corruption tests corrupt the entry.
	var prevTerm []byte
	first := true
	for termIdx := 0; termIdx < 2 && pos < len(root); termIdx++ {
		var nLen uint64
		var n int
		if first {
			nLen, n = getFTS3Varint(root[pos:])
			if n == 0 {
				return fmt.Errorf("corrupt segment root")
			}
			pos += n
			if uint64(pos)+nLen > uint64(len(root)) {
				return fmt.Errorf("corrupt segment root")
			}
			prevTerm = root[pos : pos+int(nLen)]
			pos += int(nLen)
		} else {
			var nPrefix, nSuffix uint64
			nPrefix, n = getFTS3Varint(root[pos:])
			if n == 0 {
				return fmt.Errorf("corrupt segment root")
			}
			pos += n
			nSuffix, n = getFTS3Varint(root[pos:])
			if n == 0 {
				return fmt.Errorf("corrupt segment root")
			}
			pos += n
			if nSuffix == 0 || uint64(nPrefix) > uint64(len(prevTerm)) || nSuffix > uint64(len(root)) || uint64(pos)+nSuffix > uint64(len(root)) {
				return fmt.Errorf("corrupt segment root")
			}
			// Reconstruct the full term (prefix from the previous term + the
			// suffix) so a later delta can compare against it.
			term := make([]byte, nPrefix)
			copy(term, prevTerm[:nPrefix])
			term = append(term, root[pos:pos+int(nSuffix)]...)
			pos += int(nSuffix)
			prevTerm = term
		}
		// Doclist length + bytes.
		var nDoclist uint64
		nDoclist, n = getFTS3Varint(root[pos:])
		if n == 0 {
			return fmt.Errorf("corrupt segment root")
		}
		pos += n
		// Absolute bound first: a near-2^64 doclist length would wrap the
		// uint64 sum and pass the relative check (fts3cov 17.x crafted root).
		if nDoclist > uint64(len(root)) || uint64(pos)+nDoclist > uint64(len(root)) {
			return fmt.Errorf("corrupt segment root")
		}
		if verr := validateDoclist(root[pos : pos+int(nDoclist)]); verr != nil {
			return fmt.Errorf("corrupt segment root")
		}
		pos += int(nDoclist)
		first = false
	}
	return nil
}

// validateInteriorNode parses an interior node: height, first block id, then
// the boundary terms (first un-prefixed, then delta-encoded). An interior
// node with no boundary terms (just height + block) is valid.
func validateInteriorNode(root []byte, pos int) error {
	// First subtree block id.
	_, n := getFTS3Varint(root[pos:])
	if n == 0 {
		return fmt.Errorf("corrupt segment root")
	}
	pos += n
	if pos >= len(root) {
		return nil
	}
	// Boundary terms: the first is length-prefixed, the rest delta-encoded.
	var prevTerm []byte
	first := true
	for pos < len(root) {
		var nLen uint64
		var n int
		if first {
			nLen, n = getFTS3Varint(root[pos:])
			if n == 0 {
				return fmt.Errorf("corrupt segment root")
			}
			pos += n
			if uint64(pos)+nLen > uint64(len(root)) {
				return fmt.Errorf("corrupt segment root")
			}
			prevTerm = root[pos : pos+int(nLen)]
			pos += int(nLen)
		} else {
			var nPrefix, nSuffix uint64
			nPrefix, n = getFTS3Varint(root[pos:])
			if n == 0 {
				return fmt.Errorf("corrupt segment root")
			}
			pos += n
			nSuffix, n = getFTS3Varint(root[pos:])
			if n == 0 {
				return fmt.Errorf("corrupt segment root")
			}
			pos += n
			if uint64(nPrefix) > uint64(len(prevTerm)) || uint64(pos)+nSuffix > uint64(len(root)) {
				return fmt.Errorf("corrupt segment root")
			}
			term := make([]byte, nPrefix)
			copy(term, prevTerm[:nPrefix])
			term = append(term, root[pos:pos+int(nSuffix)]...)
			pos += int(nSuffix)
			prevTerm = term
		}
		first = false
	}
	return nil
}

// LeafTermRange returns the first and last term of a leaf-node block (for
// validating that a segment's blocks are in sorted order; fts3corrupt 8.3
// copies block 1 into block 2, breaking the order).
func LeafTermRange(block []byte) (string, string) {
	if len(block) == 0 {
		return "", ""
	}
	pos := 0
	height, n := getFTS3Varint(block[pos:])
	if n == 0 {
		return "", ""
	}
	pos += n
	if height != 0 || pos >= len(block) {
		return "", ""
	}
	// First term: length-prefixed.
	termLen, n := getFTS3Varint(block[pos:])
	if n == 0 || uint64(pos)+termLen > uint64(len(block)) {
		return "", ""
	}
	pos += n
	first := string(block[pos : pos+int(termLen)])
	pos += int(termLen)
	last := first
	// Iterate the remaining delta-encoded terms.
	var prev []byte = []byte(first)
	for pos < len(block) {
		// Doclist length + content.
		nDoclist, n := getFTS3Varint(block[pos:])
		if n == 0 {
			break
		}
		pos += n
		if uint64(pos)+nDoclist > uint64(len(block)) {
			break
		}
		pos += int(nDoclist)
		if pos >= len(block) {
			break
		}
		// Delta-encoded term.
		nPrefix, n := getFTS3Varint(block[pos:])
		if n == 0 {
			break
		}
		pos += n
		nSuffix, n := getFTS3Varint(block[pos:])
		if n == 0 {
			break
		}
		pos += n
		if uint64(nPrefix) > uint64(len(prev)) || uint64(pos)+nSuffix > uint64(len(block)) {
			break
		}
		term := make([]byte, nPrefix)
		copy(term, prev[:nPrefix])
		term = append(term, block[pos:pos+int(nSuffix)]...)
		pos += int(nSuffix)
		last = string(term)
		prev = term
	}
	return first, last
}

// sortPostings sorts postings by (docid, column, position) so buildDoclist
// can encode them in SQLite's order.
func sortPostings(postings []Posting) {
	sort.SliceStable(postings, func(i, j int) bool {
		if postings[i].DocID != postings[j].DocID {
			return postings[i].DocID < postings[j].DocID
		}
		if postings[i].Column != postings[j].Column {
			return postings[i].Column < postings[j].Column
		}
		return postings[i].Position < postings[j].Position
	})
}

// ParseLeafRecords decodes a leaf node's full entry list (fts3 leaf format:
// varint iHeight==0, then per-entry [nTerm|nPrefix nSuffix] term + varint
// nDoclist + doclist). Used by the incremental-merge chomp to trim a leaf's
// merged leading entries in place (fts3TruncateNode).
func ParseLeafRecords(block []byte) ([]TermRecord, error) {
	return parseLeafRecs(block)
}

func parseLeafRecs(block []byte) ([]TermRecord, error) {
	pos := 0
	h, n := getFTS3Varint(block[pos:])
	if n == 0 || h != 0 {
		return nil, fmt.Errorf("not a leaf node")
	}
	pos += n
	var out []TermRecord
	var prev []byte
	first := true
	for pos < len(block) {
		var term []byte
		if first {
			nLen, n2 := getFTS3Varint(block[pos:])
			if nLen > uint64(len(block)) || n2 == 0 || pos+n2+int(nLen) > len(block) {
				return nil, fmt.Errorf("corrupt leaf")
			}
			pos += n2
			term = block[pos : pos+int(nLen)]
			pos += int(nLen)
			prev = term
			first = false
		} else {
			nPrefix, n2 := getFTS3Varint(block[pos:])
			if n2 == 0 {
				return nil, fmt.Errorf("corrupt leaf")
			}
			pos += n2
			nSuffix, n3 := getFTS3Varint(block[pos:])
			if nSuffix > uint64(len(block)) || nPrefix > uint64(len(prev)) || n3 == 0 || pos+n3+int(nSuffix) > len(block) {
				return nil, fmt.Errorf("corrupt leaf")
			}
			pos += n3
			term = make([]byte, 0, int(nPrefix)+int(nSuffix))
			term = append(term, prev[:nPrefix]...)
			term = append(term, block[pos:pos+int(nSuffix)]...)
			pos += int(nSuffix)
			prev = term
		}
		nDoc, n4 := getFTS3Varint(block[pos:])
		if nDoc > uint64(len(block)) || n4 == 0 || pos+n4+int(nDoc) > len(block) {
			return nil, fmt.Errorf("corrupt leaf")
		}
		pos += n4
		dl := block[pos : pos+int(nDoc)]
		pos += int(nDoc)
		out = append(out, TermRecord{Term: string(term), Doclist: dl})
	}
	return out, nil
}

// SerializeLeafNode encodes leaf entries (fts3 leaf format) — exported for
// the merge chomp's in-place leaf trimming (fts3TruncateNode).
func SerializeLeafNode(records []TermRecord) []byte {
	recs := make([]termRecord, len(records))
	for i, r := range records {
		recs[i] = termRecord{term: r.Term, doclist: r.Doclist}
	}
	return serializeLeafNode(recs)
}

// ParseLeafRecordsAsTerms is a typed wrapper returning exported records.
func ParseLeafRecordsAsTerms(block []byte) ([]TermRecord, error) { return ParseLeafRecords(block) }

// BuildSegmentBlocks serializes sorted (term, doclist) records into a
// segment root blob plus its %_segments leaf blocks (the same layout
// serializeSegmentBlocks produces for flush-built segments). Used by the
// crisis merge, which merges streamed source doclists instead of rebuilding
// from the live in-memory index — a rebuild drops delete-marker tombstones
// while OLDER segments at higher levels still hold the deleted documents'
// postings, resurrecting them on reload (fts4opt 2.x integrity-check).
func BuildSegmentBlocks(terms []string, getDoclist func(string) []byte, nodeSize int) ([]byte, []SegmentBlock) {
	if len(terms) == 0 {
		return nil, nil
	}
	records := make([]termRecord, 0, len(terms))
	for _, term := range terms {
		records = append(records, termRecord{term: term, doclist: getDoclist(term)})
	}
	return serializeSegmentBlocks(records, nodeSize)
}
