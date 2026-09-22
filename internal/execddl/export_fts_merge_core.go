// Package execddl: MergeFTS, the FTS 'merge=N[,M]' special command (fts3_write.c
// fts3DoIncrmerge / sqlite3Fts3Incrmerge). Split from export_fts_merge.go;
// behavior unchanged.
package execddl

// MergeFTS implements the FTS 'merge=N[,M]' special command (fts3_write.c
// partially-consumed segment is kept (its remaining doc IDs stay in the row).
// The %_stat id=1 hint records (level, remaining count) so the next merge
// continues the same output segment (fts4merge 4.3: 16→14→13→... consumes one
// or two level-0 segments per merge=1,16 call into a single level-1 output).
//
// The per-call/per-iteration state lives in ftsMergeRun (export_fts_merge_run.go);
// each phase below is one of its methods, in the original execution order.
// MergeFTS implements the FTS 'merge=N[,M]' special command (fts3_write.c
// sqlite3Fts3Incrmerge). A non-nil error is a corruption detected while
// reading the source/output segdir rows ("database disk image is malformed"),
// which the 'merge=' special insert surfaces to the statement (fts3corrupt
// 6.10). The flush-time automerge caller ignores it, like SQLite's xSync.
func (e *DDLExecutor) MergeFTS(tableName string, nMerge, nMin int) error {
	ftsTable, ok := e.ctx.FTSTables()[tableName]
	if !ok || ftsTable == nil {
		return nil
	}
	if nMin < 2 {
		nMin = 2
	}
	nodeSize := e.ftsNodeSize(ftsTable)
	r := &ftsMergeRun{
		e:               e,
		tableName:       tableName,
		ftsTable:        ftsTable,
		nodeSize:        nodeSize,
		nMerge:          nMerge,
		nMin:            nMin,
		segdirNextRowID: e.ftsSegdirNextRowID(tableName),
		nRem:            nMerge,
		effMin:          nMin,
	}
	if blob := e.readFTSStatRow(tableName, 1); blob != nil {
		r.hintList = ftsParseHintList(blob)
	}
	for _, h := range r.hintList {
		println("  hint level:", h.level, "nseg:", h.nSeg)
	}
	// Loop over levels: consume a level, then move up (SQLite's outer while
	// loop in sqlite3Fts3Incrmerge). nMerge is the leaf-page quota; the quota
	// (nRem) decreases by (1 + leaf pages written) each iteration and the
	// loop exits when it is exhausted.
	for r.nRem > 0 {
		if r.iterate() == mergeStop {
			return r.mergeErr
		}
		// mergeRetry loops again: the hinted level was consumed and the
		// hint cleared, so the next iteration re-runs FIND_MERGE_LEVEL.
	}
	return r.mergeErr
}

// iterate runs ONE MergeFTS level-consumption iteration: pick the level,
// prepare the output, stream the merge, then chomp/release/finalize (SQLite's
// sqlite3Fts3Incrmerge loop body). Returns mergeRetry when the hinted level
// was already consumed (the loop re-picks), mergeStop on any abort.
func (r *ftsMergeRun) iterate() mergeFlow {
	if flow := r.pickLevel(); flow != mergeContinue {
		return flow
	}
	r.prepareOutput()
	if !r.buildReaders() || !r.primeHeap() {
		return mergeStop
	}
	// The append-order verdict can still flip replacingOut off (falling back
	// to a fresh output), so it must run BEFORE the writer is created.
	r.checkAppendOrder()
	if !r.setupWriter() {
		return mergeStop
	}
	if !r.prepareAllocation() || !r.seedHierarchy() {
		return mergeStop
	}
	r.runMergeStream()
	if r.mergeErr != nil {
		// rc != SQLITE_OK: no chomp, no release, no %_segdir row, no
		// hint store (fts3_write.c sqlite3Fts3Incrmerge gates every
		// step on rc and `while(rc==SQLITE_OK)` exits the loop). The
		// output blocks written so far are unreachable garbage without
		// a segdir row, exactly like SQLite's unwritten buffers.
		return mergeStop
	}
	if !r.chompSources() || !r.releaseWriter() {
		return mergeStop
	}
	r.finalizeOutputRow()
	r.storeMergeHint()
	r.persistMergeCtx()
	// SQLite's sqlite3Fts3Incrmerge subtracts (1 + nWork) from the leaf
	// quota after each merge iteration.
	r.nRem -= 1 + r.flushCount
	// If the level was fully consumed, continue at the next level; else
	// the quota stopped us and the loop's next FIND_MERGE_LEVEL may pick
	// the same level again (the kept rows still count). SQLite's outer
	// loop re-runs FIND_MERGE_LEVEL, so loop again.
	return mergeContinue
}

// ftSMergeLevel returns the lowest absolute level whose %_segdir has at least
// nMin segments, or -1 when no such level exists (fts3_write.c
// SQL_FIND_MERGE_LEVEL: the level with the smallest relative level containing
// at least nMin segments).
// nSegCap mirrors SQLite's nSeg = MIN(MAX(nMin, nSegFound), nHintSeg)
// (fts3_write.c sqlite3Fts3Incrmerge): a hint continuation loads at most
// nHintSeg segments, but never fewer than nMin when the level has more.
