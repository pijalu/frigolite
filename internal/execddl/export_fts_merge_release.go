// Package execddl: the write/stream/release half of MergeFTS (the FTS
// "merge=N[,M]" special command, fts3_write.c sqlite3Fts3Incrmerge): output
// block persistence, the streaming k-way term merge, and the
// chomp/release/finalize sequence. Split from export_fts_merge_run.go;
// behavior unchanged.
package execddl

import (
	"container/heap"
	"fmt"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/sql"
)

// writeOutBlock persists one output block (leaf or interior node).
// ALL merge block writes use REPLACE semantics (fts3_write.c
// SQL_INSERT_SEGMENTS: "REPLACE INTO %_segments(blockid, block)") —
// a continuation re-writes its RESTORED pending interior node at its
// own slot, and a plain INSERT would fail on the duplicate rowid
// (silently swallowed, the stale bytes kept being served to readers
// and the chomp looped forever on the same block). A failed write
// aborts the merge (fts3WriteSegment's rc propagates through
// fts3IncrmergeAppend/fts3IncrmergePush).
func (r *ftsMergeRun) writeOutBlock(blk []byte) (int, error) {
	next, cached := r.ftsTable.NextBlockID()
	next, _ = r.nextOutputBlockID(next, cached)
	if r.firstBlock == 0 {
		r.firstBlock = next
	}
	ires := r.e.ctx.Exec(&sql.InsertStmt{
		Table:     r.tableName + "_segments",
		Columns:   []string{"blockid", "block"},
		IsReplace: true,
		Values: [][]sql.Expr{
			{
				&sql.NumericLit{Value: fmt.Sprintf("%d", next)},
				&sql.BlobLit{Value: blk},
			},
		},
	})
	if ires != nil && ires.Error != nil {
		return 0, ires.Error
	}
	r.ftsTable.SetNextBlockID(next + 1)
	r.outLeafData += len(blk)
	r.lastWrittenBlock = next
	return next, nil
}

// nextOutputBlockID decides WHERE the next output block lands: the
// continuation's in-place leaf, the markered continuation's sequential
// in-range cursor, the floor bump, or the fresh writer's pre-allocated
// range. See writeOutBlock for the REPLACE semantics.
func (r *ftsMergeRun) nextOutputBlockID(next int, cached bool) (int, bool) {
	reuseLeaf := r.contReuseLeaf
	if r.contReuseLeaf {
		next = r.contLeavesEnd
		r.contReuseLeaf = false
		cached = true
	} else if r.replacingOut && r.markerID > 0 && r.contLeavesEnd > 0 {
		// A markered continuation allocates SEQUENTIALLY inside its
		// pre-allocated range: the previous leaf id + 1, never the
		// max-based fallback (the NULL marker row sits above the
		// range and would drag every leaf up to markerID-1,
		// overwriting one block per flush).
		r.contNext++
		next = int(r.contNext)
		cached = true
	} else if !cached {
		// The cache was invalidated by a shadow write: recompute the max
		// (an uncached read returns 0 — writing block 0/1 would clobber
		// live blocks, fts4merge 1.2's "malformed").
		next = r.e.ftsNextBlockID(r.tableName)
	}
	if !reuseLeaf && next <= r.allocFloor && !(r.replacingOut && r.markerID > 0) {
		next = r.allocFloor + 1
		cached = true
	}
	if r.useMarker && !r.replacingOut && !r.contReuseLeaf {
		// Sequential allocation inside the writer's pre-allocated range
		// (iStart..iEnd): the cache/max-based fallback would jump ABOVE
		// the marker row once a shadow write invalidates the cache,
		// scattering leaves outside the root's contiguous range.
		next = r.freshNext
		r.freshNext++
		cached = true
	}
	return next, cached
}

// runMergeStream consumes the source level's segments via a STREAMING k-way
// term merge: the merged term stream of ALL source segments is consumed in
// sorted order, writing the output until the leaf-page quota (nWork >= nRem)
// is met. Each group reader advances to its next (unmerged) term; a reader
// error mid-scan is SQLITE_ERROR from sqlite3Fts3SegReaderStep: it aborts
// the merge (the do-while's `while( rc==SQLITE_ROW )` exits), skipping chomp
// and release — swallowing it used to leave sources half-read and duplicated
// (level, idx) rows behind.
func (r *ftsMergeRun) runMergeStream() {
	// mergedDoclists holds the MERGED doclist per merged term (SQLite's
	// fts3IncrmergeAppend writes the merged term's doclist, NOT every
	// document's full postings — a truncated source segment only holds its
	// unmerged terms, so rebuilding from full documents would duplicate
	// the already-merged terms, and the live index may not cover flushed
	// segments at all).
	mergedDoclists := make(map[string][]byte)
	for r.h.Len() > 0 {
		e0 := heap.Pop(r.h).(mergeHeapEntry)
		term, group, totalDoclist, groupDoclists := r.collectTermGroup(e0)
		merged := r.mergeGroupDoclists(groupDoclists, totalDoclist)
		if len(merged) > 0 {
			mergedDoclists[term] = merged
		}
		if !r.appendMergedTerm(term, merged) {
			break
		}
		r.flushCount = r.writer.WorkDone()
		r.advanceGroup(group)
		if r.mergeErr != nil {
			break
		}
		if r.nMerge > 0 && r.flushCount >= r.nRem {
			// The term that triggered the flush was appended to the new
			// output node (SQLite fts3IncrmergeAppend flushes the old
			// block then adds the term), so it is merged; the group
			// readers are now positioned at their first unmerged terms.
			// The stop uses the CURRENT iteration's remaining quota nRem,
			// not the original nMerge (SQLite's do-while stops when
			// pWriter->nWork >= nRem).
			break
		}
	}
}

// mergeGroupDoclists merges the group's raw doclists into the output entry
// (SQLite's fts3IncrmergeAppend writes the merged term's doclist) and sizes
// the entry charge by the MERGED doclist (SQLite's nSpace uses
// pCsr->aDoclist after fts3DoclistMerge) — the raw per-segment doclist SUM
// undercounts because combining segments for the same term adds docid deltas
// (the automerge consumed ~1.5x the source per quota, shifting the level
// structure; fts4merge4 2.2.x).
func (r *ftsMergeRun) mergeGroupDoclists(groupDoclists [][]byte, totalDoclist int) []byte {
	var merged []byte
	if r.bIgnoreEmpty {
		merged = fts.MergeDoclistsApply(groupDoclists...)
	} else {
		merged = fts.MergeDoclists(groupDoclists...)
	}
	nDoclist := len(merged)
	if nDoclist == 0 {
		nDoclist = totalDoclist
	}
	_ = nDoclist
	return merged
}

// appendMergedTerm appends one merged term to the output writer, flushing
// the leaf when the writer does; returns false when the block write failed
// (the merge aborts, fts3IncrmergeAppend's rc).
func (r *ftsMergeRun) appendMergedTerm(term string, merged []byte) bool {
	if blk := r.writer.Append(term, merged); blk != nil {
		id, werr := r.writeOutBlock(blk)
		if werr != nil {
			r.mergeErr = werr
			return false
		}
		r.writer.NoteFlushedID(id)
	}
	return true
}

// collectTermGroup pops every reader whose current term equals the heap's
// next term into one merge group (SQLite's pCsr->aDoclist merges all source
// segments for the current term); the group's doclists and their total raw
// size come back alongside the term and its readers.
func (r *ftsMergeRun) collectTermGroup(e0 mergeHeapEntry) (string, []mergeHeapEntry, int, [][]byte) {
	term := e0.term
	group := []mergeHeapEntry{e0}
	totalDoclist := e0.size
	var groupDoclists [][]byte
	if len(e0.doclist) > 0 {
		groupDoclists = append(groupDoclists, e0.doclist)
	}
	for r.h.Len() > 0 && r.h.peekTerm() == term {
		e1 := heap.Pop(r.h).(mergeHeapEntry)
		group = append(group, e1)
		totalDoclist += e1.size
		if len(e1.doclist) > 0 {
			groupDoclists = append(groupDoclists, e1.doclist)
		}
	}
	return term, group, totalDoclist, groupDoclists
}

// advanceGroup moves each group reader to its next (unmerged) term. A
// reader error mid-scan is SQLITE_ERROR from sqlite3Fts3SegReaderStep: it
// aborts the merge (the do-while's `while( rc==SQLITE_ROW )` exits), skipping
// chomp and release — swallowing it used to leave sources half-read and
// duplicated (level, idx) rows behind.
func (r *ftsMergeRun) advanceGroup(group []mergeHeapEntry) {
	for _, g := range group {
		if nterm, nids, ndl, nsize, ok := g.reader.Next(); ok {
			heap.Push(r.h, mergeHeapEntry{term: nterm, docIDs: nids, doclist: ndl, size: nsize, reader: g.reader, seq: g.seq})
		} else if g.reader.Err() != nil {
			r.mergeErr = g.reader.Err()
		}
	}
}

// chompSources truncates each source segment to its unmerged terms (SQLite's
// fts3IncrmergeChomp / fts3TruncateSegment) — chompFTSMerge deletes
// fully-consumed segments and truncates the rest IN PLACE (trimmed
// blocks keep their ids, so no fresh allocation is needed). C's order
// is append → chomp → RELEASE: a chomp error must abort BEFORE the
// output %_segdir row (and before the release flushes) exist —
// swallowing it left sources un-truncated and duplicated (level,idx)
// rows (17 rows at level=2 idx=0).
func (r *ftsMergeRun) chompSources() bool {
	_, truncated, chompErr := r.e.chompFTSMerge(r.tableName, r.level, r.readers)
	r.truncated = truncated
	return chompErr == nil
}

// releaseWriter writes the outstanding final leaf, then the layered interior
// nodes, then computes the segdir layout over the writer's real state
// (fts3IncrmergeRelease).
func (r *ftsMergeRun) releaseWriter() bool {
	// Release (fts3IncrmergeRelease): write the outstanding final leaf.
	if blk := r.writer.TakeLeaf(); blk != nil {
		id, werr := r.writeOutBlock(blk)
		if werr != nil {
			return false
		}
		r.writer.NoteFlushedID(id)
		// Release flush is outside fts3IncrmergeAppend and therefore does
		// not contribute to nWork or this call's quota.
	}
	r.leavesEndBlock = 0
	if r.lastWrittenBlock > 0 {
		// The real last written block (a continuation's first flush
		// OVERWRITES the existing last leaf, so the block ids are NOT a
		// simple contiguous range from firstBlock).
		r.leavesEndBlock = r.lastWrittenBlock
	} else if r.firstBlock > 0 {
		r.leavesEndBlock = r.firstBlock + r.writer.LeavesFlushed() - 1
	}
	// Finalize the layered hierarchy: interior layers below the root are
	// persisted as %_segments blocks at their base slots (iStart +
	// L*nLeafEst + seq for fresh layers; the LOADED slot for restored
	// ones); the highest non-empty layer becomes the root blob
	// (fts3IncrmergeRelease). For segments small enough to need a single
	// interior node this reproduces the legacy flat root byte-for-byte.
	rootBlob, interiorBlocks := r.writer.Finish()
	for _, ib := range interiorBlocks {
		// REPLACE semantics (fts3IncrmergeRelease → fts3WriteSegment): a
		// restored pending node is re-written at its own block id.
		ires := r.e.ctx.Exec(&sql.InsertStmt{
			Table:     r.tableName + "_segments",
			Columns:   []string{"blockid", "block"},
			IsReplace: true,
			Values: [][]sql.Expr{
				{
					&sql.NumericLit{Value: fmt.Sprintf("%d", ib.ID)},
					&sql.BlobLit{Value: ib.Data},
				},
			},
		})
		if ires != nil && ires.Error != nil {
			return false
		}
	}
	if rootBlob == nil {
		if r.replacingOut && len(r.contBounds) == 0 {
			rootBlob = fts.RootBlobBytes(r.lastOutRow.root)
		} else {
			rootBlob = r.writer.BuildRoot(r.firstBlock)
		}
	}
	r.rootBlob = rootBlob
	return true
}

// finalizeOutputRow persists SQLite's cumulative nLeafData, the %_segdir row
// (with the sign flip for partial merges), and the output row's identity.
func (r *ftsMergeRun) finalizeOutputRow() {
	// Persist SQLite's cumulative nLeafData, not serialized block bytes.
	// SQLite's nLeafData includes one height byte per newly materialized
	// leaf; continuation's pre-existing leaves are already in contSize.
	r.outLeafData = r.writer.LeafData()
	if r.replacingOut && r.outLeafData < int(r.contSize) {
		r.outLeafData = int(r.contSize)
	}
	// Defer the %_segdir row write until AFTER chomp: SQLite negates the
	// output's nLeafData when the merge was PARTIAL (fts3IncrmergeRelease
	// runs after fts3IncrmergeChomp, and `if(nSeg!=0) nLeafData *= -1` —
	// the negative size suffix marks a partial-merge output, and
	// fts3PromoteSegments aborts on any nSize<=0 candidate). The engine
	// writes the row after chomp below with the correct sign.
	outRowID := r.segdirNextRowID
	// A continuation's end_block size is the ACCUMULATED leaf data of every
	// append (SQLite's pWriter->nLeafData persists across calls); the
	// promotion 3/2 rule uses it, so fold the existing output's size in.
	// Continuation writer already starts with prior nLeafData, restored from
	// end_block before appending. Do not add prior size a second time.
	outSize := r.outLeafData
	_ = outSize
	// Write the %_segdir row now that the chomp's result is known: negate
	// the size suffix when the merge was partial (SQLite's nLeafData *= -1
	// when nSeg!=0 — promotion aborts on any negative candidate).
	remaining := r.truncated
	rowSize := outSize
	if remaining > 0 {
		rowSize = -outSize
	}
	if r.replacingOut {
		// The continuation REWRITES the output row: SQLite deletes the
		// old %_segdir row and inserts a fresh one (fts3IncrmergeRelease
		// → fts3DeleteSegdir + insert), so its rowid moves ABOVE every
		// surviving segment row. An in-place UPDATE keeps the old rowid
		// and reverses the natural (rowid) order of segdir rows, which
		// unordered SELECTs expose (fts4growth 6.4/6.5). The leaves keep
		// their ids; only the ROW identity is new.
		r.e.deleteFTSSegdirIdx(r.tableName, r.nextLevel, r.outIdx)
		outRowID = r.e.ftsSegdirNextRowID(r.tableName)
		// Re-sync the explicit-rowid cursor after the cont-rewrite's
		// fresh scan (see syncSegdirRowID — a stale cursor collides with
		// the surviving row and REPLACES it).
		r.segdirNextRowID = syncSegdirRowID(r.segdirNextRowID, outRowID)
		if r.contBare {
			// bNoLeafData: the loaded candidate carried no size suffix,
			// so the rewritten row keeps a BARE integer end_block
			// (fts3WriteSegdir binds int64 when nLeafData==0).
			rowSize = 0
		}
		r.e.writeFTSShadowRowAtRange(r.tableName, r.nextLevel, r.outIdx, outRowID, r.firstBlock, r.leavesEndBlock, rowSize, r.rootBlob, r.markerID)
		if r.lastOutRow.rowidKnown {
			r.lastOutRow.rowid = outRowID
		}
	} else {
		r.e.writeFTSShadowRowAtRange(r.tableName, r.nextLevel, r.outIdx, outRowID, r.firstBlock, r.leavesEndBlock, rowSize, r.rootBlob, r.markerID)
		r.segdirNextRowID++
	}
	r.outRowID = outRowID
	r.remaining = remaining
}

// storeMergeHint writes the %_stat id=1 hint: (absLevel, remaining count at
// the level) so the next merge continues this output (fts4merge 4.3:
// X'000E'... X'0006'; fts4merge 1.3 after merge #2: X'010F' = (1, 15)). When
// the level is fully consumed the hint is an EMPTY blob (fts4merge 1.3 after
// merge #3: id=1 exists with value X”).
func (r *ftsMergeRun) storeMergeHint() {
	// SQLite's nSeg after chomp counts only the segments the merge
	// LOADED (pCsr->nSegment): fully-consumed ones are deleted, the rest
	// truncated; segments at the level NOT part of this merge (e.g. the
	// flush's fresh segment, or the tail the hint capped away) are NOT
	// counted. len(rows)-deleted over-counted them, pushing a hint that
	// made the next iteration re-merge the level (fts4merge4 tx18:
	// oracle goes 0:1 1:2 2:1, the engine looped L0 again producing
	// overlapping L1 outputs).
	if r.remaining > 0 {
		// PUSH (level, remaining) to the END of the hint list (SQLite's
		// fts3IncrmergeHintPush); bDirtyHint is set when the hint was used
		// OR a push happened, so store the current list.
		r.hintList = append(r.hintList, ftsHintEntry{level: r.level, nSeg: r.remaining})
		r.storeHint()
	} else if r.fromHint {
		// The hint directed this merge and the level is now fully
		// consumed: the popped entry is gone; store the REMAINING list
		// (other entries survive — the engine's old single-row hint
		// dropped them, fts4merge4 tx26).
		r.storeHint()
	}
	// else: a fresh FIND_MERGE_LEVEL merge fully consumed its level
	// without touching the hint — SQLite's bDirtyHint stays 0 and the
	// PREVIOUS hint row is preserved (fts3_write.c sqlite3Fts3Incrmerge:
	// fts3IncrmergeHintStore only runs when bDirtyHint is set; fts4merge
	// 5.11: the merge=1,6 drains L0 while the L1 hint X'010E' remains).
	// No write here keeps the existing row.
}

// storeHint persists the hint list (empty list → empty %_stat row).
func (r *ftsMergeRun) storeHint() {
	if len(r.hintList) == 0 {
		r.e.writeFTSStatRow(r.tableName, 1, nil)
	} else {
		r.e.writeFTSStatRow(r.tableName, 1, ftsEncodeHintList(r.hintList))
	}
}

// persistMergeCtx invalidates the segment cache, then persists the writer
// state for the next continuation — nLeafEst (unchanged), iBlock (leaves
// written so far), buffer (the ending leaf-buffer fill — SQLite's
// nearly-empty last leaf after a quota stop, which the next merge resumes
// from so it consumes a full page; fts4merge 1.3 merge #3 drains L1).
// Dropped when the level is fully consumed.
func (r *ftsMergeRun) persistMergeCtx() {
	r.ftsTable.InvalidateSegmentCacheKeepMergeCtx()
	if r.remaining > 0 {
		mcOut := r.outRowID
		if r.replacingOut {
			mcOut = r.lastOutRow.rowid
		}
		r.ftsTable.SetMergeCtx(r.nextLevel, &fts.MergeCtx{
			NLeafEst: r.nLeafEst,
			IBlock:   r.writer.LeavesFlushed(),
			Buffer:   r.writer.BufferFill(),
			OutRowID: mcOut,
			MarkerID: r.markerID,
		})
	} else {
		r.ftsTable.ClearMergeCtx(r.nextLevel)
		// The merge fully consumed its level: promote every segment at
		// higher levels of this index that is smaller than 3/2 of the new
		// output's leaf data down to the output level (SQLite
		// fts3PromoteSegments, fts3_write.c:5076 — the oracle's sparse level
		// structures come from this collapse, fts4merge4 2.2 am=2: 4 1 6 1).
		// A segment with no size in end_block (0 or unparsable) blocks the
		// whole promotion, exactly like fts3ReadEndBlockField nSize<=0.
		r.e.promoteFTSSegments(r.tableName, r.ftsTable, r.nextLevel, r.outLeafData)
	}
}
