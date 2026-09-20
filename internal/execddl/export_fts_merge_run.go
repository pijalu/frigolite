// Package execddl: per-call state and phase helpers for MergeFTS (the FTS
// 'merge=N[,M]' special command, fts3_write.c sqlite3Fts3Incrmerge). Split
// from export_fts_merge_core.go; behavior unchanged.
package execddl

import (
	"container/heap"
	"fmt"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/sql"
)

// mergeFlow is one MergeFTS iteration's outcome: proceed, retry the level
// pick (the hinted level was already consumed), or stop the whole merge.
type mergeFlow int

const (
	mergeContinue mergeFlow = iota
	mergeRetry
	mergeStop
)

// ftsMergeRun carries one MergeFTS call's state (the IncrmergeWriter and
// pCsr locals of fts3_write.c sqlite3Fts3Incrmerge) across its phases; the
// fields mirror the original function's per-call and per-iteration locals
// one-for-one, reset where the original re-initialized them each iteration.
type ftsMergeRun struct {
	e         *DDLExecutor
	tableName string
	ftsTable  *fts.FTS3Table
	nodeSize  int
	nMerge    int
	nMin      int
	// Track the next %_segdir rowid so the merge's output/truncation writes
	// use EXPLICIT rowids — the implicit allocation's cached max can go stale
	// mid-transaction after a delete invalidates it, reusing a rowid and
	// duplicating the row (fts4merge4 2.2.x with the corrected hint: the L2
	// output's replacingOut delete + insert reused rowid 34).
	segdirNextRowID int64
	// nRem is the remaining leaf-page quota; mergeErr is the iteration's rc
	// (SQLite's `rc` in sqlite3Fts3Incrmerge): any reader/writer/chomp
	// failure aborts the WHOLE merge call before the output %_segdir row is
	// written (`while( rc==SQLITE_OK )`).
	nRem     int
	mergeErr error
	// The %_stat id=1 hint is a LIST of (level, nSeg) pairs (SQLite's
	// fts3IncrmergeHintPop/Push): each iteration POPS the first entry (and
	// restores it if the fresh FIND takes precedence), and chomp PUSHES
	// (level, remaining) to the END. A single row carries several pending
	// levels across the iterations of one call (fts4merge4 tx26: hint.n=4 =
	// the (1,2) and (2,2) pairs; the engine's old single-entry row dropped
	// the second pair when the first level was fully consumed).
	hintList []ftsHintEntry

	// Per-iteration state (reset at the head of each phase group, exactly
	// where the original loop body declared/re-initialized it).
	level, effMin  int
	fromHint       bool
	rows           []ftsSegdirRow
	maxSourceEnd   int
	nextLevel      int
	bIgnoreEmpty   bool
	outIdx         int
	replacingOut   bool
	nLeafEst       int
	mc             *fts.MergeCtx
	lastOutRow     ftsSegdirRow
	contStartBlock int
	contLeavesEnd  int
	contBounds     []string
	// The candidate root's own height and header child (fts3IncrmergeLoad's
	// nHeight / aRoot[0] and the root blob's first varints): they drive the
	// pending interior-node chain restore below.
	contRootHeight     int
	contRootFirstChild int
	contSize           int64
	contBare           bool // candidate had NO size suffix: keep end_block bare
	contLeaves         int
	markerID           int
	readers            []segReader
	h                  *mergeHeap
	writer             *fts.IncrLeafWriter
	// contReuseLeaf: the first flush of a continuation is the loaded
	// existing last leaf; it must OVERWRITE the existing last-leaf block
	// (SQLite's fts3IncrmergeLoad keeps aNodeWriter[0].iBlock = the
	// existing leaf's block, and the flush writes it in place). Only
	// subsequent NEW leaves allocate fresh block ids.
	contReuseLeaf    bool
	lastWrittenBlock int
	firstBlock       int
	allocFloor       int
	freshNext        int
	// contNext tracks the continuation's in-range allocation cursor
	// (SQLite's aNodeWriter[0].iBlock increments inside iStart..iEnd).
	contNext int64
	// SQLite nLeafData tracks bytes written for leaf nodes, including the
	// height byte in each serialized leaf block.
	outLeafData    int
	flushCount     int
	leavesEndBlock int
	rootBlob       []byte

	// Release/finalize hand-off (set by releaseWriter/finalizeOutputRow,
	// read by storeMergeHint/persistMergeCtx).
	truncated int
	outRowID  int64
	remaining int

	// Allocation-model switches (set in prepareAllocation).
	useMarker bool
	hierStart int
}

// pickLevel resolves WHICH level this iteration merges. A %_stat id=1 hint
// from a previous merge (fts4merge 4.3: the hint continues consuming the same
// level even after its segment count drops below nMin) overrides the fresh
// FIND_MERGE_LEVEL search — but ONLY when the hinted level is at or below the
// level FIND_MERGE_LEVEL would pick RELATIVE to nMod (fts3_write.c
// sqlite3Fts3Incrmerge: iAbsLevel%nMod >= iHintAbsLevel%nMod). The POP is
// LIFO — from the END of the list, the entry chomp pushed LAST call — and it
// is UNCONDITIONAL: the popped entry is only restored (re-appended) when a
// strictly-lower found level takes precedence. Checking the hinted level's
// live count here diverged: stale heads accumulated at the FRONT (FIFO) and
// continuations never engaged, so merge=5,2 loops re-found fresh levels every
// call and drained orders of magnitude slower than SQLite (fts4opt 2.6).
func (r *ftsMergeRun) pickLevel() mergeFlow {
	foundLevel := r.e.ftSMergeLevel(r.tableName, r.nMin)
	r.level = -1
	r.effMin = r.nMin
	r.fromHint = false
	return r.applyHintPop(foundLevel)
}

// applyHintPop pops the LIFO hint entry, engages the continuation when the
// hint takes precedence (or restores it otherwise), then reads the candidate
// level's rows; see pickLevel.
func (r *ftsMergeRun) applyHintPop(foundLevel int) mergeFlow {
	nMod := 1024 * (1 + len(r.ftsTable.PrefixLengths()))
	if len(r.hintList) > 0 {
		popped := r.hintList[len(r.hintList)-1]
		r.hintList = r.hintList[:len(r.hintList)-1]
		hLevel, hSeg := popped.level, popped.nSeg
		if foundLevel < 0 || foundLevel%nMod >= hLevel%nMod {
			// nSeg = MIN(MAX(nMin, found), nHintSeg); SQLite then opens
			// the OLDEST nSeg segments and proceeds ONLY when exactly
			// nSeg exist (pCsr->nSegment==nSeg) — a stale hint pointing
			// at a drained level does NO work instead of merging a
			// lone segment upward (which cascaded outputs 1057→1058→…).
			rows2 := r.e.readFTSSegdirRows(r.tableName, hLevel)
			cap2 := nSegCap(r.nMin, foundCountAt(r.e, r.tableName, foundLevel), hSeg)
			if len(rows2) >= cap2 {
				r.level = hLevel
				r.effMin = cap2
				r.fromHint = true
			}
			// else: dead/short hint — entry stays consumed, this
			// iteration falls back below without touching the level.
		} else {
			// The fresh FIND picked a strictly-lower relative level; undo
			// the pop (SQLite restores hint.n).
			r.hintList = append(r.hintList, popped)
		}
	}
	if r.level < 0 {
		// No usable hint: use the lowest absolute level with at least
		// nMin segments.
		r.level = foundLevel
		if r.level < 0 {
			return mergeStop
		}
	}
	r.rows = r.e.readFTSSegdirRows(r.tableName, r.level)
	if len(r.rows) == 0 {
		r.e.clearFTSStatRow(r.tableName, 1)
		return mergeRetry
	}
	// SQLite's fts3IncrmergeLoad decides APPENDABILITY (for a hinted
	// continuation) purely by the zero-length marker block at end_block
	// (fts3IsAppendable: "blockid=? AND block IS NULL") plus the first-term
	// order check. A BARE integer end_block (no size suffix) does NOT block
	// continuation — fts3ReadEndBlockField parses it to (iEnd, nLeafData=0)
	// and bNoLeafData only gates promotion later. The engine previously
	// returned a silent no-op here, diverging from oracle (fts4growth x6:
	// merge25b extends leaves 744->769 on a bare end_block). The geometry
	// fallback in prepareOutput performs the marker and order checks.
	return mergeContinue
}

// prepareOutput selects the output segment's identity and layout: the source
// ceiling, the bIgnoreEmpty verdict, the continuation load (hint or
// geometry), and the leaf-quota estimate.
func (r *ftsMergeRun) prepareOutput() {
	// The source segments' blocks are LIVE until this merge's chomp
	// truncates them. The output writes (and continuation's new leaves,
	// and the truncation's fresh blocks) must allocate ABOVE every source
	// segment's leaves_end_block — otherwise a source block and an output
	// leaf share an id and the truncation's old-range cleanup deletes the
	// fresh output leaf (fts4merge4 am=2 tx 17: the L1[1] continuation
	// wrote new leaves at 3420+, colliding with the L0 sources at 3421+;
	// the chomp deleted them, shrinking L1[1] and diverging the cascade).
	// SQLite avoids this by pre-allocating the writer's entire block
	// range (iStart..iEnd) above all existing blocks.
	r.maxSourceEnd = 0
	for _, row := range r.rows {
		if end := int(r.e.segdirRowLeavesEnd(row.leavesEndBlock)); end > r.maxSourceEnd {
			r.maxSourceEnd = end
		}
	}
	// The output lives at level+1. When the hint directed us here (a
	// continuation), APPEND to the existing output segment (the largest
	// idx at level+1); a fresh FIND_MERGE_LEVEL creates a new output.
	r.nextLevel = r.level + 1
	// SQLite's bIgnoreEmpty (fts3_write.c fts3SegmentMerge): when the
	// output lands ABOVE every other segment of this index, delete-
	// marker entries are DROPPED from the merge instead of preserved —
	// nothing older remains below the output, so applying the deletions
	// is safe and keeps outputs compact. Without this, propagated bare
	// entries inflate merged outputs (~1.5x) until regrowth-time
	// promotion aborts on size, stranding stale segments (fts4opt 2.7/
	// 2.8).
	promoBase := (r.level / 1024) * 1024
	iMaxLevel := -1
	for _, r2 := range r.e.readFTSSegdirRowsRange(r.tableName, promoBase, promoBase+1024) {
		if r2.level > iMaxLevel {
			iMaxLevel = r2.level
		}
	}
	r.bIgnoreEmpty = false && r.nextLevel > iMaxLevel
	r.outIdx = r.e.ftSSegmentIdx(r.tableName, r.nextLevel)
	r.replacingOut = false
	// A continuation appends to the existing output segment (SQLite's
	// fts3IncrmergeLoad): the existing leaves STAY at their block ids and
	// only NEW merged leaves are written after them. The engine previously
	// replayed the whole existing output through the writer (rewriting
	// every leaf at fresh ids), which both did O(segment) extra work and
	// made the continuation's output so large that the leaf quota stopped
	// mid-merge where SQLite's append finished (fts4merge4 tx22: engine
	// charged 506 leaves vs SQLite's 261 new ones).
	r.nLeafEst = 0
	r.mc = r.ftsTable.MergeCtxFor(r.nextLevel)
	r.lastOutRow = ftsSegdirRow{}
	// No-replay continuation state: existing leaves keep their ids.
	r.contStartBlock, r.contLeavesEnd = 0, 0
	r.contBounds = []string(nil)
	r.contRootHeight, r.contRootFirstChild = 0, 0
	r.contSize = 0
	r.contBare = false
	r.contLeaves = 0
	r.markerID = 0
	if r.fromHint && r.mc != nil {
		r.loadHintContinuation()
	}
	if !r.replacingOut {
		r.loadGeometryFallback()
	}
	if !r.replacingOut && r.mc != nil && r.fromHint {
		r.nLeafEst = 1 << 30
	}
	if !r.replacingOut && r.nLeafEst == 0 {
		r.estimateFreshLeafQuota()
	}
}

// estimateFreshLeafQuota sizes a fresh merge's leaf quota
// (SQL_MAX_LEAF_NODE_ESTIMATE): nLeafEst = 2*total(1 + leaves_end_block -
// start_block) over the source segments.
func (r *ftsMergeRun) estimateFreshLeafQuota() {
	for _, row := range r.rows {
		sb := int64(0)
		if s, ok := row.startBlock.(int64); ok {
			sb = s
		}
		le := r.e.segdirRowLeavesEnd(row.leavesEndBlock)
		r.nLeafEst += 2 * int(1+le-sb)
	}
	if r.nLeafEst < 2 {
		r.nLeafEst = 2
	}
}

// loadHintContinuation appends to the EXISTING output segment recorded in the
// MergeCtx (SQLite's fts3IncrmergeLoad): the existing leaves keep their block
// ids and only new merged leaves are written after them.
func (r *ftsMergeRun) loadHintContinuation() {
	outRows := r.e.readFTSSegdirRows(r.tableName, r.nextLevel)
	if len(outRows) <= 0 {
		return
	}
	last := outRows[len(outRows)-1]
	if last.rowidKnown && last.rowid != r.mc.OutRowID {
		r.mc = nil
		return
	}
	r.lastOutRow = last
	r.outIdx = last.idx
	r.replacingOut = true
	r.nLeafEst = r.mc.NLeafEst
	r.contStartBlock = segdirRowStart(last)
	r.contLeavesEnd = int(r.e.segdirRowLeavesEnd(last.leavesEndBlock))
	r.contSize = segdirRowSize(last.endBlock)
	// bNoLeafData propagation (fts3IncrmergeLoad): a candidate
	// loaded WITHOUT a size suffix keeps the bare integer
	// end_block on EVERY subsequent rewrite of the row —
	// SQLite rebinds int64 iEndBlock while bNoLeafData is set,
	// even after further partial or completing merges
	// (fts4growth 7.5: the completed output stays "23694",
	// never gains a size suffix).
	r.contBare = r.contSize == 0
	r.contLeaves = r.mc.IBlock
	if h, fb, bounds := fts.ParseSegmentRootBounds(fts.RootBlobBytes(last.root)); h > 0 && r.contStartBlock > 0 {
		r.contBounds = bounds
		r.contRootHeight = h
		// contStartBlock stays the ROW's start_block — the first
		// LEAF id (SQLite's pWriter->iStart = iStart from
		// %_segdir; fts3IncrmergeLoad never re-derives it). The
		// root's first child is only the chain-seed's header
		// pointer — for a height>=2 root it is an INTERIOR
		// block id, and writing it into start_block made
		// leaves_end < start (the next merge then read the
		// segment as empty and dropped its content).
		if fb > 0 {
			r.contRootFirstChild = fb
		}
	} else {
		// A single-leaf output (root IS the leaf, no %_segments
		// rows) cannot be appended in place. Fall back to the
		// rebuild path — but KEEP the existing output row as a
		// merge SOURCE: its terms were already merged (and its
		// sources chomped away), so dropping it here would lose
		// every term it holds (fts4opt 1.x: merge #2 rebuilt
		// without it and integrity-check reported missing-term
		// runs covering the previous call's entire output).
		r.rows = append(r.rows, last)
		if end := int(r.e.segdirRowLeavesEnd(last.leavesEndBlock)); end > r.maxSourceEnd {
			r.maxSourceEnd = end
		}
		r.replacingOut = false
		r.mc = nil
	}
	// The append-order check (first merged term > existing last term)
	// runs after the reader heap is primed (checkAppendOrder).
}

// loadGeometryFallback falls back to the candidate output row's own GEOMETRY
// when there is no usable MergeCtx and its end_block has NO size suffix
// (user-stripped, fts4growth 7.3) — SQLite's fts3IncrmergeLoad re-derives
// writer state from %_segdir and the zero-length marker row alone, so a merge
// can keep appending inside the pre-allocated range even after the size
// accounting is gone (oracle s.db: 7.5 merges continue the stripped segment;
// start_block/leaves preserved).
func (r *ftsMergeRun) loadGeometryFallback() {
	cand := r.e.readFTSSegdirRows(r.tableName, r.nextLevel)
	if len(cand) <= 0 {
		return
	}
	c := cand[len(cand)-1]
	start := segdirRowStart(c)
	le := int(r.e.segdirRowLeavesEnd(c.leavesEndBlock))
	endFirst := int(segdirEndBlockFirst(c.endBlock))
	if start <= 0 || le <= 0 || endFirst <= le || segdirRowSize(c.endBlock) != 0 {
		return
	}
	// The marker row is a ZERO-LENGTH/NULL block.
	blk, res := r.e.readFTSBlock(r.tableName, endFirst)
	if res != nil || len(blk) != 0 {
		return
	}
	r.lastOutRow = c
	r.outIdx = c.idx
	r.replacingOut = true
	r.contStartBlock = start
	r.contLeavesEnd = le
	r.contSize = segdirRowSize(c.endBlock)
	r.contLeaves = r.contLeavesEnd - r.contStartBlock + 1
	r.markerID = endFirst
	r.contBare = true
	// SQLite derives the pre-allocated range as
	// iEnd = iStart-1 + nLeafEst*FTS_MAX_APPENDABLE_HEIGHT
	// (HEIGHT=16); invert it for the writer's flush quota.
	r.nLeafEst = (endFirst - r.contStartBlock + 1) / 16
	if h, fb, bounds := fts.ParseSegmentRootBounds(fts.RootBlobBytes(c.root)); h > 0 {
		r.contBounds = bounds
		r.contRootHeight = h
		// Same as above: the row's start_block (first leaf)
		// stays; the root's first child only seeds the chain.
		if fb > 0 {
			r.contRootFirstChild = fb
		}
	} else {
		r.replacingOut = false
		r.mc = nil
	}
}

// buildReaders opens a lazy streaming reader per loaded source segment (see
// MergeFTS for the k-way merge strategy); returns false when no segment
// yields a reader (the merge stops).
func (r *ftsMergeRun) buildReaders() bool {
	// SQLite's SQL_FIND_MERGE_LEVEL returns the count of segments at the
	// lowest level with at least nMin segments; sqlite3Fts3Incrmerge then
	// CAPS nSeg to the hint's remaining count when the hint is used
	// (nSeg = MIN(MAX(nMin, found), nHintSeg)), merging only that many of
	// the OLDEST segments — the flush's fresh segment is left behind when
	// the hint still tracks the earlier ones (fts4merge4 2.2.x: the
	// am=2 hint (0,2) at tx 19 merges the 2 old small L0 segments and
	// keeps the new flush at L0).
	loadCount := len(r.rows)
	if r.fromHint && r.effMin < loadCount {
		loadCount = r.effMin
	}
	r.readers = make([]segReader, 0, loadCount)
	for _, row := range r.rows[:loadCount] {
		// Each source segment is streamed by a lazy reader
		// (fts.SegmentStreamReader, the engine's Fts3SegReader
		// equivalent) that reads %_segments leaf blocks only as the merge
		// consumes terms, so the automerge's cost is bounded by the
		// leaf-page quota, not the level size.
		sr := fts.NewSegmentStreamReader(fts.RootBlobBytes(row.root), int(r.e.segdirRowLeavesEnd(row.leavesEndBlock)), func(blockID int) ([]byte, error) {
			blk, res := r.e.readFTSBlock(r.tableName, blockID)
			if res != nil {
				return nil, fmt.Errorf("corrupt segment root")
			}
			return blk, nil
		})
		r.readers = append(r.readers, segReader{row: row, reader: sr})
	}
	return len(r.readers) > 0
}

// primeHeap seeds the k-way merge heap with each reader's first term. The
// merge pulls terms in sorted order, groups the readers that share a term,
// and simulates the output node buffer (fts3IncrmergeAppend).
func (r *ftsMergeRun) primeHeap() bool {
	r.h = &mergeHeap{}
	for i, sr := range r.readers {
		term, ids, doclist, size, ok := sr.reader.Next()
		if !ok {
			if sr.reader.Err() != nil {
				return false
			}
			continue // empty segment
		}
		heap.Push(r.h, mergeHeapEntry{term: term, docIDs: ids, doclist: doclist, size: size, reader: sr.reader, seq: i})
	}
	return true
}

// checkAppendOrder verifies a continuation's first merged term is > the
// existing output's last term (SQLite's fts3IncrmergeLoad bAppendable=0
// rejects the append otherwise; the output would break sorted order). On
// mismatch the iteration falls back to a fresh output.
func (r *ftsMergeRun) checkAppendOrder() {
	if r.replacingOut && r.h.Len() > 0 {
		first := r.h.peekTerm()
		lt := r.e.segdirRowLastTerm(r.tableName, r.lastOutRow)
		if lt != "" && first <= lt {
			r.replacingOut = false
			r.mc = nil
		}
	}
}

// setupWriter builds the incremental leaf writer and, for a continuation,
// resumes the existing output's LAST leaf (SQLite's fts3IncrmergeLoad loads
// the candidate segment's last leaf into pWriter->aNodeWriter[0]): the buffer
// is already full, so the first appended term flushes it and the quota is
// charged exactly as SQLite's does (fts4merge 4.3: one source segment per
// merge=1,16 call, not two). A leaf that fails to parse is a corrupt segment
// — SQLite's read/parse failure sets rc and aborts the merge (fix 3), it does
// not silently skip the leaf.
func (r *ftsMergeRun) setupWriter() bool {
	// The output is written by the REAL incremental leaf writer
	// (fts.IncrLeafWriter, fts3IncrmergeAppend): each flushed leaf becomes
	// a %_segments block immediately, so the work quota charged
	// (nWork = leaves flushed this call) equals the blocks actually
	// written. A continuation (SQLite's fts3IncrmergeLoad) keeps the
	// EXISTING leaves at their block ids and appends only the NEW merged
	// leaves after them; the existing leaves are NOT replayed/rewritten
	// (that made the output so large the quota stopped mid-merge where
	// SQLite's append finished — fts4merge4 tx22).
	r.writer = fts.NewIncrLeafWriter(r.nodeSize, r.nLeafEst, 0, 0)
	r.contReuseLeaf = false
	r.lastWrittenBlock = 0
	if r.replacingOut {
		r.writer = fts.NewIncrLeafWriter(r.nodeSize, r.nLeafEst, r.contLeaves, 0)
		if !r.resumeContinuationLeaf() {
			return false
		}
	}
	r.firstBlock = 0
	if r.replacingOut {
		r.firstBlock = r.contStartBlock // existing leaves keep their ids
	}
	return true
}

// resumeContinuationLeaf loads a continuation's existing last leaf into the
// writer (fts3IncrmergeLoad's aNodeWriter[0] restore); see setupWriter.
func (r *ftsMergeRun) resumeContinuationLeaf() bool {
	if r.contLeavesEnd <= 0 {
		return true
	}
	lastLeaf, res := r.e.readFTSBlock(r.tableName, r.contLeavesEnd)
	if res == nil && lastLeaf != nil {
		if !r.writer.LoadLeaf(lastLeaf) {
			return false
		}
		// nLeafData is cumulative across partial calls. Loading the
		// last leaf restores its fill, but not this accounting value.
		if r.contSize < 0 {
			r.contSize = -r.contSize
		}
		r.writer.SeedLeafData(int(r.contSize))
		r.contReuseLeaf = true
		r.writer.SetLeafNextID(r.contLeavesEnd)
		return true
	}
	return false
}

// prepareAllocation computes the block-allocation floor, the marker
// (pre-allocated range) model, and the writer's layered hierarchy base.
func (r *ftsMergeRun) prepareAllocation() bool {
	// The block-allocation floor: every fresh output leaf and every
	// truncation block must land above (a) the still-live source blocks
	// (maxSourceEnd) AND (b) the continuation output's existing leaves
	// (contLeavesEnd — the output may extend beyond the sources).
	r.allocFloor = r.maxSourceEnd
	if r.contLeavesEnd > r.allocFloor {
		r.allocFloor = r.contLeavesEnd
	}
	// SQLite's incrmerge writer PRE-ALLOCATES the output's block range
	// (fts3IncrmergeWriter: iEnd = iStart-1 + nLeafEst*HEIGHT, HEIGHT=16)
	// and writes a NULL marker %_segments row at iEnd so a later merge can
	// detect the appendable segment. The base must be above EVERY live
	// block (other levels' segments stay live during this merge).
	// SCOPE: currently enabled only for languageid=<col> tables
	// (fts4langid asserts the marker row and (iEnd,size) end_block).
	// Real SQLite pre-allocates for ALL tables, but its writer reserves a
	// PER-LAYER range (aNodeWriter[i].iBlock = iStart + i*nLeafEst) that
	// this engine's allocator does not mirror yet; enabling it globally
	// diverges block ids across multi-merge sequences (fts4merge 1.3,
	// fts4growth 1.x). Porting the exact reservation is the remaining
	// step for full segment-internals parity.
	r.useMarker = true // markers now model SQLite unconditionally (oracle-verified at page_size 1024)
	if r.useMarker {
		if nb := r.e.ftsNextBlockID(r.tableName) - 1; nb > r.allocFloor {
			r.allocFloor = nb
		}
	}
	// markerID is the segment's iEnd: the end_block first component for
	// fresh outputs (fts4langid 5.4 "256 65") and the allocation ceiling
	// for continuations.
	//
	// SCOPE: the marker/pre-allocation model is enabled only for
	// languageid=<col> tables, whose tests assert the marker row and the
	// (iEnd,size) end_block form (fts4langid 5.4.x.5). Non-langid tables
	// keep the engine's historical no-marker layout: their large-scale
	// merge scenarios (fts4growth 2.x+) diverge from the oracle in source
	// counting long before markers matter, and switching them over
	// regressed those suites.
	if r.mc != nil && r.replacingOut {
		r.markerID = r.mc.MarkerID
	}
	r.freshNext = 0
	if !r.replacingOut && r.useMarker {
		r.markerID = r.allocFloor + r.nLeafEst*16
		// The marker goes in with REPLACE semantics (fts3WriteSegment:
		// "REPLACE INTO %_segments"), so a marker left behind by an
		// aborted earlier merge cannot duplicate the rowid. A failed
		// write aborts the merge before any source is touched
		// (fts3IncrmergeWriter returns rc).
		mres := r.e.ctx.Exec(&sql.InsertStmt{
			Table:     r.tableName + "_segments",
			Columns:   []string{"blockid", "block"},
			IsReplace: true,
			Values: [][]sql.Expr{
				{
					&sql.NumericLit{Value: fmt.Sprintf("%d", r.markerID)},
					&sql.NullLit{},
				},
			},
		})
		if mres != nil && mres.Error != nil {
			return false
		}
		// Leaf allocation runs sequentially from just above the floor and
		// never reaches the marker within one quota (nRem < nLeafEst*16);
		// pin the cache there so cache invalidations cannot jump past it.
		r.freshNext = r.allocFloor + 1
		r.ftsTable.SetNextBlockID(r.freshNext)
	}
	// Enable layered interior output (SQLite aNodeWriter): interior
	// layer L allocates blocks from its base slot iStart + L*nLeafEst.
	// Continuations restore the WHOLE pending interior-node chain
	// (fts3IncrmergeLoad): the root blob goes into layer nHeight, and
	// each layer below it is the root-to-leaf chain of LAST children,
	// loaded from %_segments so the restored nodes are re-written at
	// their own slots.
	r.hierStart = r.freshNext
	if r.replacingOut {
		r.hierStart = r.contStartBlock
	}
	r.writer.BeginHierarchy(r.hierStart, r.nLeafEst)
	r.contNext = int64(r.contLeavesEnd)
	r.outLeafData = 0
	r.flushCount = 0
	return true
}

// seedHierarchy restores a continuation's pending interior-node chain
// (fts3IncrmergeLoad): the root blob goes into layer nHeight, and each layer
// below it is the root-to-leaf chain of LAST children, loaded from
// %_segments so the restored nodes are re-written at their own slots.
func (r *ftsMergeRun) seedHierarchy() bool {
	if !(r.replacingOut && r.contRootHeight > 0 && r.contStartBlock > 0) {
		return true
	}
	// The last child of a node whose header child is fb and which
	// carries n boundary entries is fb+n (children are consecutive).
	lastLeaf := r.contRootFirstChild + len(r.contBounds)
	switch {
	case r.contRootHeight == 1:
		// Height-1 root: the pending layer-1 node IS the root blob;
		// its slot is the layer-1 base (free — a height-1 root means
		// layer 1 never overflowed in any earlier call).
		r.writer.SeedHierarchyLayer(1, r.contRootFirstChild, r.contBounds, r.hierStart+r.nLeafEst)
	case r.contRootHeight < fts.MaxHierLayers:
		r.writer.SeedHierarchyLayer(r.contRootHeight, r.contRootFirstChild, r.contBounds, r.hierStart+r.contRootHeight*r.nLeafEst)
		// Walk the last-child chain down (fts3IncrmergeLoad: for
		// i=nHeight..1, aNodeWriter[i-1].iBlock = reader.iChild of
		// layer i's restored node, and that block is LOADED into
		// layer i-1's buffer). Seeding ONLY layer 1 with the root's
		// boundaries was correct only for height-1 roots; for
		// height>=2 it pointed layer 1 at an interior-layer slot
		// with the wrong entries — the self-referential node that
		// wedged later merges (fts4merge4 2.2.3.x plateau).
		var chainOK bool
		lastLeaf, chainOK = r.restoreChildChain(lastLeaf)
		if !chainOK {
			return false
		}
	default:
		// Root taller than FTS_MAX_APPENDABLE_HEIGHT: corrupt
		// (fts3IncrmergeLoad returns FTS_CORRUPT_VTAB).
		return false
	}
	if lastLeaf != r.contLeavesEnd {
		return false
	}
	return true
}

// restoreChildChain walks the restored root's last-child chain down the
// hierarchy layers, seeding each layer from its %_segments block; returns the
// chain's leaf-most block id and whether every hop parsed (fts3IncrmergeLoad).
func (r *ftsMergeRun) restoreChildChain(lastLeaf int) (int, bool) {
	for L := r.contRootHeight - 1; L >= 1; L-- {
		blk, res := r.e.readFTSBlock(r.tableName, lastLeaf)
		if res != nil || blk == nil {
			return lastLeaf, false
		}
		hh, fb2, b2 := fts.ParseSegmentRootBounds(blk)
		if hh != L || fb2 <= 0 {
			return lastLeaf, false
		}
		r.writer.SeedHierarchyLayer(L, fb2, b2, lastLeaf)
		lastLeaf = fb2 + len(b2)
	}
	return lastLeaf, true
}
