package fts

// IncrLeafWriter incrementally builds one FTS segment's leaf stream, matching
// SQLite's incremental-merge leaf writer (fts3_write.c fts3IncrmergeAppend):
// terms are appended to an in-memory leaf buffer; a leaf is FLUSHED (handed
// to the caller as a finished block) when adding the next term would exceed
// nodeSize AND the leaf quota is not exhausted (pLeaf->iBlock <
// pWriter->iStart + pWriter->nLeafEst). Unlike SerializeSegmentForTerms,
// the flush decision is the writer's own — a quota-blocked leaf may exceed
// nodeSize, exactly like SQLite's appendable segments — so the leaf count
// the merge is charged for (nWork) equals the blocks actually written.
type IncrLeafWriter struct {
	nodeSize  int
	nLeafEst  int
	leavesOut int // total materialized leaves (SQLite iBlock - iStart)

	hier          bool // layered interior output enabled
	hierStart     int  // segment iStart (interior layer id base)
	layers        [maxHierLayers]iLayer
	interiorQ     []iBlockOut // flushed interior nodes awaiting write
	pendingSep    string      // separator of the leaf just flushed
	hasPendingSep bool
	workOut       int // leaves written by Append during this call (SQLite nWork)

	records      []termRecord // current (unfinished) leaf
	buffer       int          // current leaf's byte size
	boundTerm    []string     // first term of each flushed leaf 1..n-1 (root boundaries)
	loadedLeaf   bool         // current leaf is continuation leaf being overwritten
	chargeHeight bool         // next empty leaf contributes its height byte
	leafData     int          // accumulated SQLite nLeafData
}

// NewIncrLeafWriter creates a writer. nLeafEst is the leaf quota bound
// (SQLite's writer nLeafEst); nodeSize the FTS node size (page size - 35).
// initialLeaves/initialBuffer resume a continuation: leaves already flushed
// and the current leaf's fill (SQLite's fts3IncrmergeLoad reloading the last
// leaf).
func NewIncrLeafWriter(nodeSize, nLeafEst, initialLeaves, initialBuffer int) *IncrLeafWriter {
	// The buffer starts at 1 to account for the height varint byte that
	// serializeLeafNode writes at the head of every leaf. SQLite's
	// fts3SegWriterAdd tracks nData including this byte, so the flush
	// threshold must match (fts3_write.c: "nData+nReq>p->nNodeSize").
	if initialBuffer == 0 {
		initialBuffer = 1
	}
	return &IncrLeafWriter{nodeSize: nodeSize, nLeafEst: nLeafEst, leavesOut: initialLeaves, buffer: initialBuffer, chargeHeight: initialLeaves == 0}

}

// leafSpace returns the bytes a term record occupies in the current leaf.
func (w *IncrLeafWriter) leafSpace(term string, nDoclist int) int {
	if len(w.records) == 0 {
		// First term of a leaf is stored un-prefixed (serializeLeafNode).
		return varintSize(uint64(len(term))) + len(term) + varintSize(uint64(nDoclist)) + nDoclist
	}
	prefix := commonPrefixLen(w.records[len(w.records)-1].term, term)
	suffix := len(term) - prefix
	return varintSize(uint64(prefix)) + varintSize(uint64(suffix)) + suffix +
		varintSize(uint64(nDoclist)) + nDoclist
}

// LoadLeaf loads an existing leaf block's (term, doclist) records into the
// writer's current leaf buffer, so a continuation appends to the END of the
// existing output's last leaf instead of starting a fresh one (SQLite's
// fts3IncrmergeLoad loads the candidate segment's last leaf into
// pWriter->aNodeWriter[0] — fts3_write.c). The buffer fill is restored so the
// first appended term flushes the existing leaf when it no longer fits,
// exactly matching SQLite's quota accounting (fts4merge 4.3: the continuation
// consumes ONE source segment per merge=1,16 call because the last leaf is
// already full, not two).
func (w *IncrLeafWriter) LoadLeaf(block []byte) {
	pos := 0
	height, n := getFTS3Varint(block[pos:])
	if n == 0 || height != 0 {
		return
	}
	pos += n
	w.records = nil
	w.buffer = 1 // height varint
	w.loadedLeaf = true
	var prevTerm []byte
	for pos < len(block) {
		var term string
		if len(w.records) == 0 {
			nLen, n := getFTS3Varint(block[pos:])
			if n == 0 || uint64(pos)+nLen > uint64(len(block)) {
				return
			}
			pos += n
			term = string(block[pos : pos+int(nLen)])
			pos += int(nLen)
		} else {
			nPrefix, n := getFTS3Varint(block[pos:])
			if n == 0 {
				return
			}
			pos += n
			nSuffix, n := getFTS3Varint(block[pos:])
			if n == 0 {
				return
			}
			pos += n
			if uint64(nPrefix) > uint64(len(prevTerm)) || uint64(pos)+nSuffix > uint64(len(block)) {
				return
			}
			t := make([]byte, nPrefix)
			copy(t, prevTerm[:nPrefix])
			t = append(t, block[pos:pos+int(nSuffix)]...)
			pos += int(nSuffix)
			term = string(t)
		}
		nDoclist, n := getFTS3Varint(block[pos:])
		if n == 0 || uint64(pos)+nDoclist > uint64(len(block)) {
			return
		}
		pos += n
		doclist := append([]byte(nil), block[pos:pos+int(nDoclist)]...)
		pos += int(nDoclist)
		w.records = append(w.records, termRecord{term: term, doclist: doclist})
		w.buffer += w.leafSpace(term, len(doclist))
		prevTerm = []byte(term)
	}
}

// Append adds one merged term. It returns the finished previous leaf's bytes
// when the append flushed it (nil otherwise); the caller writes the block
// and counts one unit of work, mirroring fts3WriteSegment + pWriter->nWork.
func (w *IncrLeafWriter) Append(term string, doclist []byte) []byte {
	sz := w.leafSpace(term, len(doclist))
	if len(w.records) > 0 && w.buffer+sz > w.nodeSize && w.workOut < w.nLeafEst {
		flushed := serializeLeafNode(w.records)
		// The interior-root boundary is a TRUNCATED SEPARATOR: the new term
		// cut to its common prefix with the flushed leaf's last term, plus
		// one byte (fts3_write.c fts3NodeAddTerm(..., zTerm, nPrefix+1);
		// full terms diverge from SQLite's root blobs — fts4growth 2.x).
		cp := 0
		if n := len(w.records); n > 0 {
			cp = commonPrefixLen(w.records[n-1].term, term)
		}
		cut := cp + 1
		if cut > len(term) {
			cut = len(term)
		}
		wasLoaded := w.loadedLeaf
		w.records = nil
		w.buffer = 1 // height varint
		w.loadedLeaf = false
		if !wasLoaded {
			w.leavesOut++
		}
		w.chargeHeight = true
		w.workOut++

		w.boundTerm = append(w.boundTerm, term[:cut])
		// Layered mode: hold the separator until the caller reports the
		// flushed leaf's block id (NoteFlushedID) — the parent entry needs
		// the finished child to keep every layer contiguous.
		w.pendingSep = term[:cut]
		w.hasPendingSep = true
		// New leaf stores term in full; SQLite resets nSpace to height byte
		// plus full-term encoding after flushing previous leaf.
		w.appendCurrent(term, doclist, w.leafSpace(term, len(doclist)))
		return flushed
	}
	w.appendCurrent(term, doclist, sz)
	return nil
}

// appendCurrent stores the record in the (new or current) leaf.
func (w *IncrLeafWriter) appendCurrent(term string, doclist []byte, sz int) {
	// SQLite nLeafData excludes first leaf height marker, includes it after
	// each flush when new leaf starts.
	if len(w.records) == 0 && w.chargeHeight {
		w.leafData++
		w.chargeHeight = false
	}
	w.records = append(w.records, termRecord{term: term, doclist: doclist})
	w.buffer += sz
	w.leafData += sz
}

// LeavesFlushed reports total materialized leaves, including continuation
// leaves loaded before this call.
func (w *IncrLeafWriter) LeavesFlushed() int { return w.leavesOut }

// iBlockOut is one flushed INTERIOR node awaiting persistence: SQLite's
// incremental-merge writer materializes every b-tree level below the root
// as %_segments blocks (fts3IncrmergeRelease writes aNodeWriter[i].iBlock).
type iBlockOut struct {
	ID   int
	Data []byte
}

// maxHierLayers caps interior depth (FTS_MAX_APPENDABLE_HEIGHT).
const maxHierLayers = 16

// iLayer is one interior layer's pending state. Layer 1 sits directly above
// the leaves; layer L+1 indexes layer-L blocks. Children are consecutive
// block ids, so a node is fully described by its first child plus the
// separator terms between consecutive children.
type iLayer struct {
	started    bool
	firstChild int
	seps       []string
	bytes      int
	written    int
}

// BeginHierarchy enables layered interior output: interior layer L blocks
// are allocated at iStart + L*nLeafEst (+ sequence), mirroring
// aNodeWriter[i].iBlock = pWriter->iStart + i*pWriter->nLeafEst
// (fts3_write.c fts3IncrmergeWriter). sep pushes flow through hierPush.
func (w *IncrLeafWriter) BeginHierarchy(iStart, nLeafEst int) {
	w.hier = true
	w.hierStart = iStart
	if nLeafEst > 0 {
		w.nLeafEst = nLeafEst
	}
}

// SeedHierarchySeps preloads layer 1 with an EXISTING segment's boundary
// separators (a continuation's stored root covers firstLeaf..firstLeaf+len-
// (seps)); further appends extend that node naturally. The byte accounting
// MUST include the node header — height byte + left-child varint — exactly
// like fts3IncrmergeLoad's restored pNode->block (header written via
// pBlk->a[0]=iLayer; pBlk->n = 1 + putVarint(child)); omitting it made the
// continuation node fit one extra separator before overflowing, shifting
// every subsequent split point (x6 blocks 2155/2156 were 990/750 instead of
// 986/754).
func (w *IncrLeafWriter) SeedHierarchySeps(firstChild int, seps []string) {
	w.hier = true
	n := &w.layers[1]
	n.started = true
	n.firstChild = firstChild
	n.bytes = 1 + varintSize(uint64(firstChild))
	for _, s := range seps {
		n.bytes += sepEntrySize(n.seps, s)
		n.seps = append(n.seps, s)
	}
}

// NoteFlushedID reports the block id the caller assigned to the leaf just
// flushed by Append/TakeLeaf, releasing the pending separator into the
// interior hierarchy. Ids arrive after the write because allocation belongs
// to the caller (reuse/overwrite rules for continuations).
func (w *IncrLeafWriter) NoteFlushedID(id int) {
	if !w.hasPendingSep {
		return
	}
	sep := w.pendingSep
	w.hasPendingSep = false
	if !w.hier {
		return
	}
	w.hierAdd(1, sep, id)
}

// hierAdd routes one separator into layer L (recursing upward when the
// pending node overflows nodeSize — fts3NodeAddTerm: the overflowing term
// is inserted into the PARENT and the new sibling starts empty).
func (w *IncrLeafWriter) hierAdd(L int, sep string, finishedChild int) {
	if L >= maxHierLayers {
		return
	}
	n := &w.layers[L]
	entry := sepEntrySize(n.seps, sep)
	if len(n.seps) > 0 && n.bytes+entry > w.nodeSize {
		// Flush this node WITHOUT the new separator, then let the separator
		// rise to the parent layer; the next node begins after the child
		// that produced the separator.
		blk := serializeInteriorNode(L, n.firstChild, n.seps)
		id := w.hierStart + L*w.nLeafEst + n.written
		n.written++
		w.interiorQ = append(w.interiorQ, iBlockOut{ID: id, Data: blk})
		w.hierAdd(L+1, sep, id)
		n.seps = n.seps[:0]
		n.bytes = 0
		n.firstChild = finishedChild + 1
		return
	}
	if len(n.seps) == 0 {
		n.started = true
		// This separator sits between the finished child and the NEXT one,
		// and opens THIS node's coverage: the node's first child IS the
		// finished child (children are consecutive).
		n.firstChild = finishedChild
		n.bytes = 1 + varintSize(uint64(n.firstChild))
	}
	n.bytes += entry
	n.seps = append(n.seps, sep)
}

// sepEntrySize sizes one boundary entry: the first entry of a node carries
// no prefix-length varint (serializeInteriorNode convention).
func sepEntrySize(seps []string, sep string) int {
	if len(seps) == 0 {
		return varintSize(uint64(len(sep))) + len(sep)
	}
	p := commonPrefixLen(seps[len(seps)-1], sep)
	suffix := len(sep) - p
	return varintSize(uint64(p)) + varintSize(uint64(suffix)) + suffix
}

// Finish finalizes the hierarchy after the last leaf: lower partial layers
// are emitted as blocks (their ids continue each layer's sequence) and the
// highest non-empty layer becomes the root blob. When no interior layer was
// ever used both returns are nil and the caller keeps its legacy root.
func (w *IncrLeafWriter) Finish() ([]byte, []iBlockOut) {
	if !w.hier {
		return nil, nil
	}
	top := 0
	for L := 1; L < maxHierLayers; L++ {
		if len(w.layers[L].seps) > 0 || w.layers[L].written > 0 {
			top = L
		}
	}
	if top == 0 {
		return nil, nil
	}
	extras := make([]iBlockOut, 0, 2)
	for L := 1; L < top; L++ {
		n := &w.layers[L]
		if len(n.seps) > 0 {
			id := w.hierStart + L*w.nLeafEst + n.written
			n.written++
			blk := serializeInteriorNode(L, n.firstChild, n.seps)
			extras = append(extras, iBlockOut{ID: id, Data: blk})
			n.seps = n.seps[:0]
		}
	}
	root := serializeInteriorNode(top, w.layers[top].firstChild, w.layers[top].seps)
	// Mid-append flushes queued by hierAdd must be persisted too — they are
	// earlier siblings of the same layers and their pre-allocated ids are
	// already recorded (fts3IncrmergeRelease writes every outstanding
	// aNodeWriter buffer, not just the final one).
	extras = append(w.interiorQ, extras...)
	return root, extras
}

// WorkDone reports leaves written by Append during this call. The final
// release flush is intentionally excluded from SQLite's nWork quota.
func (w *IncrLeafWriter) WorkDone() int { return w.workOut }

// BoundTerms returns the first term of every flushed leaf after the first
// (the interior-node boundaries a continuation's rebuilt root appends to the
// existing output's boundaries).
func (w *IncrLeafWriter) BoundTerms() []string { return append([]string(nil), w.boundTerm...) }

// BufferFill reports the current leaf's fill (persisted as the continuation
// state; SQLite's nearly-empty last leaf).
func (w *IncrLeafWriter) BufferFill() int { return w.buffer }

// SeedLeafData sets the accumulated leaf-data counter (SQLite's fts3IncrmergeLoad
// reads the prior end_block size into pWriter->nLeafData before appending).
func (w *IncrLeafWriter) SeedLeafData(n int) { w.leafData = n }

// LeafData returns the accumulated per-term entry byte count (SQLite's
// pWriter->nLeafData — the end_block size suffix), NOT raw block bytes.
func (w *IncrLeafWriter) LeafData() int { return w.leafData }

// TakeLeaf returns the current leaf's serialized bytes and resets it, for
// the final release flush (fts3IncrmergeRelease writes the outstanding
// buffer). Returns nil when the leaf is empty.
func (w *IncrLeafWriter) TakeLeaf() []byte {
	if len(w.records) == 0 {
		return nil
	}
	blk := serializeLeafNode(w.records)
	w.records = nil
	w.buffer = 0
	// The released final leaf is a MATERIALIZED block (the caller writes it
	// under its own id), so it must join the leaf count — otherwise a
	// continuation that added exactly one new leaf reports leavesOut==1 and
	// BuildRoot emits a single-leaf synthetic root while TWO blocks exist on
	// disk; the last leaf then has NO boundary entry and every reader skips
	// its terms (fts4opt 2.x: prefix-band term "b" vanished). A still-loaded
	// leaf is the exception: it rewrites the EXISTING last-leaf block in
	// place, which the initial-leaves count already covers.
	if !w.loadedLeaf {
		w.leavesOut++
	}
	w.loadedLeaf = false
	return blk
}

// LeafCount reports the total leaves of the finished segment (flushed plus
// the final one when includeFinal).
func (w *IncrLeafWriter) LeafCount(includeFinal bool) int {
	n := w.leavesOut
	if includeFinal && len(w.records) > 0 {
		n++
	}
	return n
}

// BuildRoot assembles the segment's root blob over the leaf blocks written
// so far. firstBlockID is the block id of the FIRST leaf (assigned by the
// caller when it wrote the blocks); the count includes the final leaf. A
// single-leaf segment stores the leaf itself as the root (no %_segments
// rows); multiple leaves get an interior node whose boundaries are the
// first terms the writer recorded when starting each new leaf.
func (w *IncrLeafWriter) BuildRoot(firstBlockID int) []byte {
	n := w.leavesOut
	if len(w.records) > 0 {
		n++
	}
	if n == 0 {
		return serializeLeafNode(nil)
	}
	if n == 1 {
		if firstBlockID > 0 {
			// The single leaf is stored in %_segments (the merge wrote it at
			// firstBlockID), so the root cannot be the leaf itself: released
			// FTS versions cannot handle a height-0 root with start_block!=0
			// (the reader would treat leaves_end_block as extra %_segments
			// leaves after the root and read OTHER segments' blocks — the
			// fts4merge 1.3 "rewind" that made a truncated segment keep all
			// its terms). Emit SQLite's synthetic interior root (height 1,
			// firstBlockID, no boundaries) exactly like fts3IncrmergeRelease's
			// iRoot==0 case (fts3_write.c).
			return serializeInteriorNode(1, firstBlockID, nil)
		}
		return serializeLeafNode(w.records)
	}
	return serializeInteriorNode(1, firstBlockID, w.boundTerm)
}
