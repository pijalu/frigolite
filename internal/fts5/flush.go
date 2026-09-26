package fts5

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"io"
	"strings"
)

// Segment flush, merge and optimize machinery — the mirror-model port of
// fts5_index.c's fts5FlushOneHash / fts5IndexMerge / fts5IndexAutomerge /
// fts5IndexCrisismerge / fts5IndexOptimizeStruct. The %_data leaf rows carry
// the Go-native blob payload (one row per segment at the segment's first-page
// rowid) instead of C's pgsz-chunked leaf pages; every SQL-visible counter
// (row multiplicity of seed/segment/tombstone rows, the structure record's
// segment/origin/tombstone accounting, automerge and crisismerge timing)
// follows C.

// segmentBlobRowid is the %_data rowid of a segment's (single) payload row.
func segmentBlobRowid(segid int64) int64 { return segmentRowid(segid, 1) }

// writeSegmentBlob persists one segment's document payload.
func (t *Table) writeSegmentBlob(seg *Segment, docs []blobDoc) error {
	var payload bytes.Buffer
	payload.WriteString("GF")
	if err := gobEncode(&payload, indexBlob{Docs: docs}); err != nil {
		return err
	}
	qData := qual(t.dbName, t.cfg.Name+"_data")
	_, err := t.db.ExecSQL(fmt.Sprintf("INSERT OR REPLACE INTO %s(id, block) VALUES(%d, X'%s');",
		qData, segmentBlobRowid(seg.Segid), hexEncode(payload.Bytes())))
	return err
}

// readSegmentBlob loads and decodes one segment's payload (nil when absent).
func (t *Table) readSegmentBlob(segid int64) ([]blobDoc, error) {
	qData := qual(t.dbName, t.cfg.Name+"_data")
	rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT block FROM %s WHERE id=%d", qData, segmentBlobRowid(segid)))
	if err != nil || len(rows) == 0 || rows[0][0] == nil {
		return nil, err
	}
	raw, ok := toBytes(rows[0][0])
	if !ok || len(raw) < 4 || !bytes.Equal(raw[:2], []byte("GF")) {
		return nil, nil
	}
	blob, err := gobDecode(bytes.NewReader(raw[2:]))
	if err != nil {
		return nil, nil
	}
	return blob.Docs, nil
}

// removeSegmentRows deletes a segment's payload row and its dlidx rows
// (fts5DataRemoveSegment: the leaf half deletes from %_data, the pIdxDeleter
// deletes from %_idx; tombstone pages are removed separately).
func (t *Table) removeSegmentRows(seg *Segment) error {
	qData := qual(t.dbName, t.cfg.Name+"_data")
	if _, err := t.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE id=%d", qData, segmentBlobRowid(seg.Segid))); err != nil {
		return err
	}
	qIdx := qual(t.dbName, t.cfg.Name+"_idx")
	_, err := t.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE segid=%d", qIdx, seg.Segid))
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return nil
	}
	return err
}

// writeDlidxRow emits a flushed segment's btree row into %_idx
// (fts5_index.c fts5WriteFlushBtree's pIdxWriter insert: one row per leaf —
// term the empty blob and pgno bFlag+(leaf<<1) for the mirror model's
// single-leaf segment blobs, bFlag 0 since no dlidx page flushes).
func (t *Table) writeDlidxRow(seg *Segment) error {
	qIdx := qual(t.dbName, t.cfg.Name+"_idx")
	_, err := t.db.ExecSQL(fmt.Sprintf("INSERT OR REPLACE INTO %s(segid, term, pgno) VALUES(%d, X'', 2)", qIdx, seg.Segid))
	if err != nil && strings.Contains(err.Error(), "no such table") {
		// A dropped %_idx is tolerated (dlidx is optional to C's readers —
		// fts5corrupt drops shadow tables and the table stays readable).
		return nil
	}
	return err
}

// segDocs materializes the blob payload of one segment from the in-memory
// index.
func (t *Table) segDocs(seg *Segment) []blobDoc {
	docs := make([]blobDoc, 0, len(seg.Rowids))
	for _, rowid := range seg.Rowids {
		if seg.Tombs[rowid] {
			continue
		}
		doc := t.ix.Doc(rowid)
		if doc == nil {
			continue
		}
		docs = append(docs, blobDoc{Rowid: rowid, Cols: detailCols(t.cfg.Detail, doc.cols)})
	}
	return docs
}

// hashEntrySize is sizeof(Fts5HashEntry) on C's 64-bit builds: two pointers,
// five ints, two bytes and padding. It is part of C's nPendingData accounting
// for every NEW term entry, so the pending-hash overflow flush frequency
// (hashsize) matches C's.
const hashEntrySize = 48

// pendingAdd accounts one (term, rowid, position) write into the pending-hash
// byte estimate (sqlite3Fts5HashWrite's nIncr accumulation into pHash->pnByte
// → p->nPendingData).
func (t *Table) pendingAdd(term string, rowid int64) {
	if t.pendingTermState == nil {
		t.pendingTermState = make(map[string]*pendingTerm)
	}
	pt, ok := t.pendingTermState[term]
	if !ok {
		t.pendingTermState[term] = &pendingTerm{lastRowid: rowid}
		// New hash entry: entry header + key + varint(rowid) + poslist-size
		// byte + varint(position delta).
		t.pendingBytes += int64(hashEntrySize + len(term) + 1 + varintLen(uint64(rowid)) + 2)
		return
	}
	if rowid != pt.lastRowid {
		// New doclist entry: poslist-size byte + varint(rowid delta) +
		// varint(position delta).
		t.pendingBytes += int64(varintLen(uint64(rowid-pt.lastRowid)) + 2)
		pt.lastRowid = rowid
		return
	}
	// Same document, next position: one varint position delta.
	t.pendingBytes++
}

// pendingReset drops the pending state after a flush.
func (t *Table) pendingReset() {
	t.pendingRowids = nil
	t.pendingBytes = 0
	t.pendingTermState = nil
}

// varintLen returns the encoded length of a SQLite varint.
func varintLen(v uint64) int {
	for i := 1; i <= 8; i++ {
		if v < 1<<(uint(i)*7) {
			return i
		}
	}
	return 9
}

// AddPendingRow registers one inserted document in the pending state,
// flushing early when the pending estimate exceeds 'hashsize'
// (sqlite3Fts5IndexBeginWrite's overflow flush). cols is the document's
// token streams.
func (t *Table) AddPendingRow(rowid int64, cols [][]string) error {
	t.pendingRowids = append(t.pendingRowids, rowid)
	for c, tokens := range cols {
		if t.cfg.Unindexed != nil && c < len(t.cfg.Unindexed) && t.cfg.Unindexed[c] {
			continue
		}
		for _, tok := range tokens {
			t.pendingAdd(tok, rowid)
		}
	}
	if t.pendingBytes > t.cfg.HashSize {
		return t.FlushPending()
	}
	return nil
}

// FlushShadowIfDirty is the sync point (sqlite3Fts5IndexSync →
// fts5IndexFlush): pending documents flush as a new level-0 segment,
// tombstone-driven merges run, and the structure record is persisted.
func (t *Table) FlushShadowIfDirty() error {
	return t.FlushPending()
}

// FlushPending flushes the pending hash and any accumulated contentless
// deletes (fts5IndexFlush: nothing happens when both are empty).
func (t *Table) FlushPending() error {
	if len(t.pendingRowids) == 0 && t.nContentlessDelete == 0 && len(t.dirtySegments) == 0 {
		return nil
	}
	if len(t.pendingRowids) == 0 && t.nContentlessDelete == 0 {
		// Deleted rows only: rewrite the affected segments in place.
		for seg := range t.dirtySegments {
			if err := t.writeSegmentBlob(seg, t.segDocs(seg)); err != nil {
				return err
			}
		}
		t.dirtySegments = nil
		return t.structureWrite()
	}
	return t.flushOneHash()
}

// flushOneHash flushes pending documents into a new level-0 segment, then
// runs the automerge/crisismerge work (fts5FlushOneHash).
func (t *Table) flushOneHash() error {
	pgnoLast := int64(0)
	if len(t.pendingRowids) > 0 && t.pendingHasTokens() {
		seg := &Segment{
			Segid:     t.allocateSegid(),
			PgnoFirst: 1,
			PgnoLast:  1,
			Rowids:    append([]int64(nil), t.pendingRowids...),
			Tombs:     map[int64]bool{},
		}
		if t.structRec.V2 {
			seg.Origin1 = t.structRec.NOriginCntr
			seg.Origin2 = t.structRec.NOriginCntr
			seg.NEntry = int64(len(t.pendingRowids))
			t.structRec.NOriginCntr++
		}
		docs := make([]blobDoc, 0, len(seg.Rowids))
		for _, rowid := range seg.Rowids {
			doc := t.ix.Doc(rowid)
			if doc == nil {
				continue
			}
			docs = append(docs, blobDoc{Rowid: rowid, Cols: detailCols(t.cfg.Detail, doc.cols)})
		}
		if err := t.writeSegmentBlob(seg, docs); err != nil {
			return err
		}
		if err := t.writeDlidxRow(seg); err != nil {
			return err
		}
		for len(t.structRec.Levels) == 0 {
			t.structRec.Levels = append(t.structRec.Levels, nil)
		}
		t.structRec.Levels[0] = append(t.structRec.Levels[0], seg)
		pgnoLast = seg.PgnoLast
	}
	t.pendingReset()

	// fts5IndexAutomerge: deletes count toward the work quanta.
	nLeaf := pgnoLast + t.nContentlessDelete
	t.nContentlessDelete = 0
	t.automerge(nLeaf)
	t.crisismerge()
	return t.structureWrite()
}

// pendingHasTokens reports whether any pending document contributes indexed
// tokens (C's !sqlite3Fts5HashIsEmpty: an all-unindexed flush creates no
// segment).
func (t *Table) pendingHasTokens() bool {
	for _, rowid := range t.pendingRowids {
		if doc := t.ix.Doc(rowid); doc != nil {
			for c, tokens := range doc.cols {
				if t.cfg.Unindexed != nil && c < len(t.cfg.Unindexed) && t.cfg.Unindexed[c] {
					continue
				}
				if len(tokens) > 0 {
					return true
				}
			}
		}
	}
	return false
}

// automerge runs the incremental merge work after a flush
// (fts5IndexAutomerge): nWork quanta of 64 leaves each, nMin='automerge'.
func (t *Table) automerge(nLeaf int64) {
	if t.cfg.Automerge <= 0 || t.structRec == nil {
		return
	}
	nWrite := t.structRec.NWriteCounter
	nWork := (nWrite + nLeaf) / WorkUnit - nWrite / WorkUnit
	t.structRec.NWriteCounter += nLeaf
	nRem := WorkUnit * nWork * int64(len(t.structRec.Levels))
	t.mergeLoop(nRem, t.cfg.Automerge)
}

// crisismerge merges any level holding >= 'crisismerge' segments
// (fts5IndexCrisismerge).
func (t *Table) crisismerge() {
	nCrisis := t.cfg.CrisisMerge
	for len(t.structRec.Levels) > 0 && int64(len(t.structRec.Levels[0])) >= nCrisis {
		t.mergeLevel(0)
		t.promote(1)
	}
}

// mergeLoop runs incremental merges until the work budget is spent or no
// candidate remains (fts5IndexMerge).
func (t *Table) mergeLoop(nRem int64, nMin int64) {
	for nRem > 0 {
		iBest, nBest := -1, int64(0)
		for i, lvl := range t.structRec.Levels {
			if int64(len(lvl)) > nBest {
				nBest = int64(len(lvl))
				iBest = i
			}
		}
		if nBest < nMin {
			iBest = t.findDeleteMerge()
		}
		if iBest < 0 {
			return
		}
		t.mergeLevel(iBest)
		t.promote(iBest + 1)
		nRem--
		if nMin == 1 {
			nMin = 2
		}
	}
}

// findDeleteMerge finds the level with the highest tombstone percentage at or
// above 'deletemerge' (fts5IndexFindDeleteMerge); -1 when none qualifies.
func (t *Table) findDeleteMerge() int {
	if !t.structRec.V2 || t.cfg.DeleteMerge <= 0 {
		return -1
	}
	iRet, nBest := -1, 0
	for ii, lvl := range t.structRec.Levels {
		var nEntry, nTomb int64
		for _, seg := range lvl {
			nEntry += seg.NEntry
			nTomb += seg.NEntryTombstone
		}
		if nEntry > 0 {
			nPercent := int((nTomb * 100) / nEntry)
			if nPercent >= int(t.cfg.DeleteMerge) && nPercent > nBest {
				iRet, nBest = ii, nPercent
			}
		}
	}
	return iRet
}

// mergeLevel merges every segment of level iLvl into one new segment at
// level iLvl+1 (fts5IndexMergeLevel's atomic mirror). The output drops
// tombstoned documents; an empty output writes no blob and no segment.
func (t *Table) mergeLevel(iLvl int) {
	inputs := t.structRec.Levels[iLvl]
	if len(inputs) == 0 {
		return
	}
	for len(t.structRec.Levels) <= iLvl+1 {
		t.structRec.Levels = append(t.structRec.Levels, nil)
	}

	out := &Segment{
		Segid:     t.allocateSegid(),
		PgnoFirst: 1,
		Tombs:     map[int64]bool{},
	}
	mergeOutBounds(inputs, out)
	// Output doclist: every input rowid that is not tombstoned in its own
	// segment (tombstones are resolved by the merge, like C's bDel skip).
	out.Rowids = mergeSurvivingRowids(inputs)
	sortRowids(out.Rowids)

	// Persist the output (or drop it when annihilated), remove the inputs.
	if len(out.Rowids) > 0 {
		out.PgnoLast = 1
		if err := t.writeSegmentBlob(out, t.segDocs(out)); err != nil {
			return
		}
		t.structRec.Levels[iLvl+1] = append(t.structRec.Levels[iLvl+1], out)
	}
	for _, in := range inputs {
		_ = t.removeSegmentRows(in)
		_ = t.removeTombstoneRows(in)
	}
	t.structRec.Levels[iLvl] = nil
}

// mergeOutBounds accumulates the merged segment's entry count and origin
// range: nEntry is the live-entry sum; the origins span from the oldest
// input's origin1 to the newest input's origin2 (fts5Merge's bounds).
func mergeOutBounds(inputs []*Segment, out *Segment) {
	for i, in := range inputs {
		out.NEntry += in.NEntry - in.NEntryTombstone
		if i == 0 {
			out.Origin1 = in.Origin1
		}
		if i == len(inputs)-1 {
			out.Origin2 = in.Origin2
		}
	}
}

// mergeSurvivingRowids collects the merged segment's rowid list: every input
// rowid not tombstoned in any input segment, each source contributing its
// own duplicates only once.
func mergeSurvivingRowids(inputs []*Segment) []int64 {
	tomb := map[int64]bool{}
	for _, in := range inputs {
		for rowid := range in.Tombs {
			tomb[rowid] = true
		}
	}
	seen := map[int64]bool{}
	out := []int64{}
	for _, in := range inputs {
		for _, rowid := range in.Rowids {
			if tomb[rowid] || seen[rowid] {
				continue
			}
			seen[rowid] = true
			out = append(out, rowid)
		}
	}
	return out
}

// promote applies fts5StructurePromote's level bookkeeping after a merge
// wrote its output at iLvl: segments smaller than the output move down from
// higher levels (scenario b); scenario (a) only applies below the flush
// level and never fires for mirror-model single-leaf segments at level 0.
func (t *Table) promote(iLvl int) {
	if iLvl <= 0 || iLvl >= len(t.structRec.Levels) {
		return
	}
	lvl := t.structRec.Levels[iLvl]
	if len(lvl) == 0 {
		return
	}
	szSeg := lvl[len(lvl)-1].size()
	for il := iLvl + 1; il < len(t.structRec.Levels); il++ {
		higher := t.structRec.Levels[il]
		for len(higher) > 0 {
			cand := higher[len(higher)-1]
			if cand.size() > szSeg {
				break
			}
			t.structRec.Levels[iLvl] = append(t.structRec.Levels[iLvl], cand)
			t.structRec.Levels[il] = higher[:len(higher)-1]
			higher = t.structRec.Levels[il]
			sortSegsByAge(t.structRec.Levels[iLvl])
		}
	}
}

// sortSegsByAge orders segments oldest-first (by segid — segids are
// allocated monotonically, matching C's insertion order).
func sortSegsByAge(segs []*Segment) {
	for i := 1; i < len(segs); i++ {
		for j := i; j > 0 && segs[j].Segid < segs[j-1].Segid; j-- {
			segs[j], segs[j-1] = segs[j-1], segs[j]
		}
	}
}

// optimizeCommand implements the 'optimize' special insert
// (sqlite3Fts5IndexOptimize): every segment consolidates into one segment at
// a new final level, tombstoned documents annihilating.
func (t *Table) optimizeCommand() error {
	if err := t.FlushPending(); err != nil {
		return err
	}
	pNew := t.optimizeStruct()
	if pNew == nil {
		return nil
	}
	t.structRec = pNew
	// Merge the first non-empty level until it drains (sqlite3Fts5IndexOptimize's
	// for/while: the inputs all sit on one level; one pass annihilates them).
	lvl := 0
	for lvl < len(t.structRec.Levels) && len(t.structRec.Levels[lvl]) == 0 {
		lvl++
	}
	for lvl < len(t.structRec.Levels) && len(t.structRec.Levels[lvl]) > 0 {
		t.mergeLevel(lvl)
	}
	return t.structureWrite()
}

// optimizeStruct builds the optimize target structure
// (fts5IndexOptimizeStruct): nil when there is no work; otherwise every
// segment (oldest first) collected onto the final level, tombstone state
// carried over.
func (t *Table) optimizeStruct() *StructRec {
	sr := t.structRec
	nSeg := sr.totalSegments()
	if nSeg == 0 {
		return nil
	}
	for _, lvl := range sr.Levels {
		nThis := int64(len(lvl))
		if nThis > 0 && nThis == int64(nSeg) {
			if nSeg == 1 && lvl[0].NPgTombstone == 0 {
				return nil // nothing to optimize
			}
			// Single-level structure: optimize rebuilds it one level down.
			out := &StructRec{V2: sr.V2, NWriteCounter: sr.NWriteCounter, NOriginCntr: sr.NOriginCntr}
			out.Levels = append(out.Levels, nil, nil)
			out.Levels[1] = append(out.Levels[1], lvl...)
			return out
		}
	}
	// Multi-level: collect every segment onto a new final level, oldest
	// (lowest level, then lowest segid) first.
	out := &StructRec{V2: sr.V2, NWriteCounter: sr.NWriteCounter, NOriginCntr: sr.NOriginCntr}
	var all []*Segment
	for _, lvl := range sr.Levels {
		all = append(all, lvl...)
	}
	sortSegsByAge(all)
	out.Levels = append(out.Levels, nil, nil)
	out.Levels[1] = all
	return out
}

// mergeCommand implements the 'merge' special insert (sqlite3Fts5IndexMerge):
// nMerge>=0 runs nRem=nMerge work units at nMin='usermerge'; a negative
// nMerge optimizes the structure instead.
func (t *Table) mergeCommand(nMerge int64) error {
	if err := t.FlushPending(); err != nil {
		return err
	}
	if nMerge < 0 {
		if pNew := t.optimizeStruct(); pNew != nil {
			t.structRec = pNew
			lvl := 0
			for lvl < len(t.structRec.Levels) && len(t.structRec.Levels[lvl]) == 0 {
				lvl++
			}
			for lvl < len(t.structRec.Levels) && len(t.structRec.Levels[lvl]) > 0 {
				t.mergeLevel(lvl)
			}
		}
		return t.structureWrite()
	}
	t.mergeLoop(nMerge, t.cfg.Usermerge)
	return t.structureWrite()
}

// gobEncode serializes v into w (the segment payload envelope).
func gobEncode(w *bytes.Buffer, v interface{}) error {
	return gob.NewEncoder(w).Encode(v)
}

// gobDecode restores a blob payload.
func gobDecode(r io.Reader) (indexBlob, error) {
	var blob indexBlob
	err := gob.NewDecoder(r).Decode(&blob)
	return blob, err
}
