// SPDX-License-Identifier: GPL-3.0-or-later

package execdml

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// handleFTSCommand detects and runs the FTS special command inserted through
// the hidden table-name column (INSERT INTO t(t) VALUES('command')): the
// 'optimize', 'merge=N[,M]', 'nodesize=N', and 'integrity-check' commands.
// s is the hidden-column value; an empty value or one that matches no command
// is a normal document insert (special=false). An unrecognized NON-empty
// command string is SQL logic error (fts3.c fts3SpecialInsert initializes
// rc=SQLITE_ERROR and only sets it to OK for recognized commands; fts4merge5
// 1.5: 'maxpendinAB64' fails). Returns (special, result): special is true when
// the value was a command (the insert is a no-op), and a non-nil result
// carries a command error (e.g. "database disk image is malformed" for a merge
// over corrupt segments, fts3corrupt 6.10/8.3).
func (e *DMLExecutor) handleFTSCommand(tableName, s string) (bool, *Result) {
	// SQLite reads the hidden-column value through sqlite3_value_text, whose
	// result is NUL-terminated: an embedded NUL truncates the command text
	// (fts3corrupt4 24.7: t2.x holds "merge=1" followed by NUL bytes and
	// SQLite runs merge=1). Mirror the C-string semantics.
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	switch {
	case lower == "optimize":
		return e.handleFTSOptimize(tableName)
	case lower == "rebuild":
		return e.handleFTSRebuild(tableName)
	case lower == "integrity-check":
		return e.handleFTSIntegrityCheck(tableName)
	case strings.HasPrefix(lower, "merge="):
		return e.handleFTSMerge(tableName, s)
	case strings.HasPrefix(lower, "nodesize="):
		return e.handleFTSNodesize(tableName, s)
	case strings.HasPrefix(lower, "maxpending="):
		// maxpending=N sets the pending-terms hash size (fts3.c
		// fts3SpecialInsert under SQLITE_TEST); it does not add a document.
		return true, nil
	case strings.HasPrefix(lower, "test-no-incr-doclist="):
		// test-no-incr-doclist=0/1 toggles SQLite's bNoIncrDoclist debug
		// flag (fts3_write.c fts3SpecialInsert, SQLITE_TEST). It changes
		// only a performance optimization (incremental doclists), never
		// query results; the engine accepts and ignores it (fts4incr 2.x
		// runs each query under both settings expecting identical results).
		return true, nil
	case strings.HasPrefix(lower, "mergecount="):
		// mergecount=N sets SQLite's nMergeCount debug toggle (fts3_write.c
		// fts3SpecialInsert under SQLITE_TEST: 4..FTS3_MERGE_COUNT, even).
		// It adjusts the auto-merge segment threshold used during flushes;
		// accepting it without an engine effect keeps the test command a
		// no-op (results are unaffected by the merge threshold).
		return true, nil
	case strings.HasPrefix(lower, "automerge="):
		return e.handleFTSAutomerge(tableName, s)
	case s == "":
		return false, nil
	default:
		// An unrecognized non-empty hidden-column value is SQL logic error
		// (fts3.c fts3SpecialInsert's default rc=SQLITE_ERROR; fts4merge5
		// 1.5: 'maxpendinAB64' fails).
		return true, &Result{Error: fmt.Errorf("SQL logic error")}
	}
}

// handleFTSOptimize runs the 'optimize' special command: OPTIMIZE reads every
// segment and content row, and a corrupt one aborts it (fts3corrupt4 10.3/14.2:
// a crash-written content table fails the command with "database disk image is
// malformed"). A segment whose start_block/end_block metadata is inconsistent
// is still optimizable (fts3corrupt4 4.4: after UPDATE t1_segdir SET
// start_block=1, optimize succeeds).
func (e *DMLExecutor) handleFTSOptimize(tableName string) (bool, *Result) {
	if t, ok := e.ctx.FTSTables()[tableName]; ok && (t.LoadErr() != nil || t.HasCorruptContent()) {
		return true, &Result{Error: fmt.Errorf("database disk image is malformed")}
	}
	e.optimizeFTSShadow(tableName)
	return true, nil
}

// handleFTSRebuild runs the 'rebuild' special command: REBUILD drops and
// rebuilds the FTS index from %_content (fts3.c fts3RebuildMethod). A corrupt
// shadow btree or a corrupt freelist (the rebuild allocates new segments)
// fails it (fts3corrupt4 24.7: INSERT INTO t1(t1) SELECT 'rebuild' FROM ... on
// a corrupt DB).
func (e *DMLExecutor) handleFTSRebuild(tableName string) (bool, *Result) {
	if res := e.ctx.RebuildFTSIndex(tableName); res != nil {
		return true, res
	}
	if err := e.ctx.ValidateFreelistForGrowth(); err != nil {
		return true, &Result{Error: err}
	}
	return true, nil
}

// handleFTSIntegrityCheck runs the 'integrity-check' special command: validate
// all segment roots AND their referenced blocks, then verify the in-memory
// index against the content rows; a corrupt or drifted one fails the check
// (fts3.c sqlite3Fts3IntegrityCheck; fts4check/fts4intck1).
func (e *DMLExecutor) handleFTSIntegrityCheck(tableName string) (bool, *Result) {
	if res := e.ctx.RunFTSIntegrityCheck(tableName); res != nil {
		return true, res
	}
	return true, nil
}

// handleFTSMerge runs the merge=A[,B] special command with SQLite's
// fts3DoIncrmerge semantics (fts3_write.c): A = max leaf pages to write
// (fts3Getint — 0 for a non-numeric prefix), optional ,B = min segments on a
// level (default MergeCount/2 = 8). The command errors ("SQL logic error")
// when trailing garbage remains or B < 2 (fts4merge 2.x: merge=abc, merge=%%%,
// merge=,, merge=5,, merge=6,%, merge=6,six, merge=6,1 all fail; merge=1
// succeeds). A merge reads the source segments (roots AND blocks); a corrupt
// one aborts it (fts3corrupt 6.10/8.3). It also combines the level-0 segments
// into a level-1 segment whose leaf blocks go in %_segments (fts3corrupt4 2.1:
// after 12 single-leaf segments, merge=1,4 writes 3 blocks while keeping the
// segdir rows).
func (e *DMLExecutor) handleFTSMerge(tableName, s string) (bool, *Result) {
	rest := s[len("merge="):]
	nMerge, rest := ftsGetint(rest)
	nMin := 8
	// SQLite's fts3DoIncrmerge consumes ",B" only when a digit
	// follows the comma; a bare trailing comma ("5,") leaves it in z
	// and errors.
	if len(rest) > 1 && rest[0] == ',' {
		rest = rest[1:]
		nMin, rest = ftsGetint(rest)
	}
	if len(rest) != 0 || nMin < 2 {
		return true, &Result{Error: fmt.Errorf("SQL logic error")}
	}
	if res := e.ctx.ValidateFTSSegments(tableName, true); res != nil {
		return true, res
	}
	if err := e.ctx.MergeFTS(tableName, nMerge, nMin); err != nil {
		return true, &Result{Error: err}
	}
	return true, nil
}

// handleFTSNodesize runs the nodesize=N special command: it sets the segment
// node size (fts3.c fts3SegReader / the fts3 'nodesize' special command); it
// does not add a document.
func (e *DMLExecutor) handleFTSNodesize(tableName, s string) (bool, *Result) {
	if n, err := strconv.Atoi(strings.TrimSpace(s[len("nodesize="):])); err == nil {
		if t, ok := e.ctx.FTSTables()[tableName]; ok {
			t.SetNodeSize(n)
		}
	}
	return true, nil
}

// handleFTSAutomerge runs the automerge=X special command (fts3.c
// fts3DoAutoincrmerge: X==0 turns it off; 1 or > MergeCount map to 8; stored in
// the %_stat id=2 row). It does not add a document. SQLite's fts3SpecialInsert
// writes the %_stat row through the shadow btree, so a corrupt shadow table
// fails the command with "database disk image is malformed" (fts3corrupt4
// 24.7). The %_stat row makes the setting survive a close/reopen: a
// flushed-after-reopen connection whose setting is still unknown reads id=2
// back (fts3_write.c sqlite3Fts3PendingTermsFlush — fts4merge4 2.2 tn2=2).
func (e *DMLExecutor) handleFTSAutomerge(tableName, s string) (bool, *Result) {
	if res := e.ctx.ValidateFTSShadowRoots(tableName); res != nil {
		return true, res
	}
	v, _ := ftsGetint(s[len("automerge="):])
	if t, ok := e.ctx.FTSTables()[tableName]; ok {
		e.ctx.WriteFTSAutomergeStat(tableName, t.SetAutomerge(v))
	}
	return true, nil
}

// ftsOptimizeState carries the shared state of one optimizeFTSShadow run.
type ftsOptimizeState struct {
	tableName      string
	existingLevels map[int64]bool
	maxLevel       int64
	nodeSize       int
	nextBlock      int
}

// ftsLangGroup is one (languageid, docids) merge group of an optimize run.
type ftsLangGroup struct {
	langid int64
	ids    []int64
}

// optimizeFTSShadow merges an FTS table's segment-directory rows into one,
// mirroring SQLite's OPTIMIZE command (fts3.c fts3DoOptimize →
// fts3SegmentMerge merges every segment of the index into one). The merged
// segment is written as a single level-0 row whose root covers all documents
// (fts3SegWriterFlush over the whole table).
func (e *DMLExecutor) optimizeFTSShadow(tableName string) {
	segdir := tableName + "_segdir"
	// SQLite's optimize merges each (langid, index) group into ONE segment
	// whose level is the numerically GREATEST level present in that group
	// (fts3_write.c fts3SegmentMerge, iLevel==FTS3_SEGCURSOR_ALL:
	// "iNewLevel = iMaxLevel"), and it is a NO-OP when a single non-pending
	// segment already covers the table — the existing row (including a
	// user-modified end_block) is left untouched (fts4growth 5.x: the
	// optimized segment keeps its level and end_block across later steps).
	// One row = one segment; SQLite's no-op check is nSegment==1.
	existingLevels, maxLevel, nSegments := e.optimizeCollectSegdirLevels(segdir)
	pending := 0
	if t, ok := e.ctx.FTSTables()[tableName]; ok && t != nil {
		pending = len(t.PendingSnapshot())
	}
	if nSegments == 1 && pending == 0 {
		// Single non-pending segment: SQLITE_DONE without rewriting.
		if t, ok := e.ctx.FTSTables()[tableName]; ok && t != nil {
			t.PendingFlush()
		}
		return
	}
	// Delete all segdir rows; one merged row per language is written below.
	_ = e.ctx.Exec(&sql.DeleteStmt{Table: segdir})
	// Every pre-optimize %_segments block belonged to a source segment that
	// OPTIMIZE deletes (fts3DeleteSegment per source): purge them so only the
	// merged output's own leaf blocks remain (fts4growth 5.x parity).
	_ = e.ctx.Exec(&sql.DeleteStmt{Table: tableName + "_segments"})
	e.optimizeWriteMergedGroups(tableName, existingLevels, maxLevel)
}

// optimizeWriteMergedGroups writes one merged level-0 segment per language
// group over the purged segdir, then refreshes the segment caches.
func (e *DMLExecutor) optimizeWriteMergedGroups(tableName string, existingLevels map[int64]bool, maxLevel int64) {
	var ftsTable *fts.FTS3Table
	if t, ok := e.ctx.FTSTables()[tableName]; ok {
		ftsTable = t
	}
	if ftsTable == nil {
		return
	}
	nodeSize := ftsTable.NodeSize()
	if nodeSize <= 0 {
		nodeSize = int(e.ctx.Pager().PageSize())
	}
	ids := ftsTable.AllRowsMap()

	groups := optimizeLangGroups(ftsTable, ids)
	st := &ftsOptimizeState{
		tableName:      tableName,
		existingLevels: existingLevels,
		maxLevel:       maxLevel,
		nodeSize:       nodeSize,
		nextBlock:      e.ctx.NextFTSBlockID(tableName),
	}
	for _, g := range groups {
		e.optimizeMergeGroup(st, ftsTable, g)
	}
	e.optimizeRefreshSegmentCache(tableName, st)
}

// optimizeRefreshSegmentCache refreshes the FTS segment caches after an
// optimize. The optimize deleted every %_segdir row and replaced them with
// one; the segdir-idx cache is stale and must be rescanned next time. The
// %_segments block counter is advanced past the new blocks.
func (e *DMLExecutor) optimizeRefreshSegmentCache(tableName string, st *ftsOptimizeState) {
	if t, ok := e.ctx.FTSTables()[tableName]; ok && t != nil {
		t.SetNextBlockID(st.nextBlock)
		t.InvalidateSegmentCache()
		// The merged segment already contains every pending document
		// (fts3DoOptimize flushes pending terms before merging); consuming
		// the pending list prevents a duplicate segment at the next flush
		// and lets optimize() report "Index already optimal" afterwards
		// (fts3f 1.3).
		t.PendingFlush()
	}
}

// optimizeCollectSegdirLevels reads the %_segdir level rows before a merge,
// returning the distinct levels, the greatest level, and the segment count.
func (e *DMLExecutor) optimizeCollectSegdirLevels(segdir string) (map[int64]bool, int64, int) {
	levelsRes := e.ctx.Exec(&sql.SelectStmt{
		Columns: []sql.SelectColumn{{Expr: &sql.ColumnRef{Name: "level"}, As: "level"}},
		From:    sql.TableRef{Name: segdir},
	})
	existingLevels := map[int64]bool{}
	maxLevel := int64(-1)
	nSegments := 0
	if levelsRes.Error == nil {
		for _, row := range levelsRes.Rows {
			if lv, ok := util.UnwrapColumnValue(row[0]).(int64); ok {
				existingLevels[lv] = true
				if lv > maxLevel {
					maxLevel = lv
				}
			}
			// One row = one segment; SQLite's no-op check is nSegment==1.
			nSegments++
		}
	}
	return existingLevels, maxLevel, nSegments
}

// optimizeLangGroups partitions the table's documents into per-language merge
// groups. A languageid=<col> table merges PER LANGUAGE: SQLite's segment merge
// works within one (iLangid, iIndex) group, so after 'optimize' the %_segdir
// holds one row per distinct language, at the language's base absolute level
// ((iLangid*nIndex+iIndex)*FTS3_SEGDIR_MAXLEVEL = iLangid*1024 with no prefix
// indexes — fts3_write.c getAbsoluteLevel; fts4langid 2.2: 9 languages → 9
// segdir rows after optimize).
func optimizeLangGroups(ftsTable *fts.FTS3Table, ids []int64) []ftsLangGroup {
	var groups []ftsLangGroup
	if ftsTable.LangIDColName() != "" {
		byLang := map[int64][]int64{}
		for _, id := range ids {
			l := ftsTable.DocLangID(id)
			byLang[l] = append(byLang[l], id)
		}
		for l := range byLang {
			groups = append(groups, ftsLangGroup{l, byLang[l]})
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i].langid < groups[j].langid })
	} else {
		groups = append(groups, ftsLangGroup{0, ids})
	}
	return groups
}

// optimizeMergeGroup merges one language group into a single segment, writing
// its segdir row and leaf blocks (advancing the run's next block id).
func (e *DMLExecutor) optimizeMergeGroup(st *ftsOptimizeState, ftsTable *fts.FTS3Table, g ftsLangGroup) {
	if len(g.ids) == 0 {
		// An empty group (no documents) contributes no segment: SQLite's
		// optimize merges existing segments, and a table with none stays
		// with zero %_segdir rows (fts4opt 3.2: CREATE + 'optimize' on an
		// empty table leaves count(*) FROM fts_segdir at 0).
		return
	}
	rootBlob, blocks := ftsTable.SegmentRootBlocks(g.ids, st.nodeSize)
	level := optimizeGroupLevel(g.langid, st.maxLevel, st.existingLevels)
	e.ctx.WriteFTSShadowRow(st.tableName, level, 0, blocks, rootBlob)
	for _, blk := range blocks {
		_ = e.ctx.Exec(&sql.InsertStmt{
			Table:   st.tableName + "_segments",
			Columns: []string{"blockid", "block"},
			Values: [][]sql.Expr{
				{
					&sql.NumericLit{Value: fmt.Sprintf("%d", st.nextBlock)},
					&sql.BlobLit{Value: blk.Block},
				},
			},
		})
		st.nextBlock++
	}
}

// optimizeGroupLevel picks the merged segment's level for a language group.
func optimizeGroupLevel(langid, maxLevel int64, existingLevels map[int64]bool) int {
	level := int(langid * 1024)
	if langid == 0 {
		// Main-index group: the output takes the greatest level present
		// before the merge (fts3_write.c iNewLevel = iMaxLevel).
		if maxLevel >= 0 {
			level = int(maxLevel)
		}
	} else {
		// Prefix/language groups keep their absolute base; use the
		// greatest RELATIVE level seen inside this group's range.
		base := langid * 1024
		for lv := range existingLevels {
			if lv >= base && lv < base+1024 && lv-base > int64(level)-base {
				level = int(lv)
			}
		}
	}
	return level
}
